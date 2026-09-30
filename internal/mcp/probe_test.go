package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// newProbeTransport builds an HTTPTransport pointed at srv with no protocol
// version pin, suitable for exercising ProbeEra (mcp.HTTPTransport.ProbeEra,
// export_test.go) or the full auto-probe path through Call/ListTools.
func newProbeTransport(endpoint string) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, "", testStreamIdleTimeout)
}

// defaultLegacyInitializeBody is the canned successful legacy initialize
// response used by probeEraHandler whenever a test case does not override
// it — every table row that expects the fallback to probeLegacy needs
// *something* sane behind "initialize", even though the row's assertion is
// about the discover step, not this fallback body.
const defaultLegacyInitializeBody = `{"jsonrpc":"2.0","id":"probe-initialize","result":{"protocolVersion":"2025-03-26","capabilities":{}}}`

// probeEraHandler returns an httptest handler that answers "server/discover"
// per discoverStatus/discoverBody and "initialize" per initStatus/initBody
// (defaulting to a canned legacy success when initStatus is 0), and 404 for
// anything else. seen, if non-nil, records every JSON-RPC method observed,
// in order, for tests that need to assert which probes actually fired.
func probeEraHandler(discoverStatus int, discoverBody string, initStatus int, initBody string, seen *[]string, mu *sync.Mutex) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var probe struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &probe)

		if seen != nil {
			mu.Lock()
			*seen = append(*seen, probe.Method)
			mu.Unlock()
		}

		switch probe.Method {
		case "server/discover":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(discoverStatus)
			if discoverBody != "" {
				fmt.Fprint(w, discoverBody)
			}
		case "initialize":
			status := initStatus
			respBody := initBody
			if status == 0 {
				status = http.StatusOK
				if respBody == "" {
					respBody = defaultLegacyInitializeBody
				}
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if respBody != "" {
				fmt.Fprint(w, respBody)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// TestProbeEra_ClassificationTable pins down every branch of probeEra's
// era-classification algorithm (docs/mcp-v2.md §4.6) against a fake upstream.
//
// The governing principle, per probeEra's own doc: IN DOUBT, LEGACY. A wrong
// "legacy" guess costs one redundant initialize handshake; a wrong "modern"
// guess produces a binding the upstream can never actually service, breaking
// the connection outright. Every ambiguous row in this table therefore
// expects EraLegacy, never EraModern.
func TestProbeEra_ClassificationTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string

		discoverStatus int
		discoverBody   string
		// initStatus/initBody override the canned legacy-success fallback for
		// rows that exercise probeLegacy too (the 404/405-on-both-probes rows).
		initStatus int
		initBody   string

		wantErr     error
		wantVersion mcp.Version // checked only when non-empty
	}{
		{
			name:           "200 result with a single known supportedVersions entry classifies modern at that version",
			discoverStatus: http.StatusOK,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","result":{"supportedVersions":["2026-07-28"]}}`,
			wantVersion:    mcp.V20260728,
		},
		{
			name:           "200 result with multiple versions including an unknown one classifies modern at the highest KNOWN version",
			discoverStatus: http.StatusOK,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","result":{"supportedVersions":["2099-01-01","2026-07-28","2025-11-25"]}}`,
			wantVersion:    mcp.V20260728,
		},
		{
			name:           "200 result with exclusively unknown versions falls through to legacy",
			discoverStatus: http.StatusOK,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","result":{"supportedVersions":["2099-01-01","1999-01-01"]}}`,
			wantVersion:    mcp.V20250326,
		},
		{
			// Regression test for the fixed probeEra bug: a legacy server
			// answers an unrecognized "server/discover" method the way
			// JSON-RPC dictates — HTTP 200 with a -32601 error body, not a
			// non-200 status. The original probeEra classified any HTTP 200
			// as modern without inspecting the body, so this exact response
			// was misclassified as modern and the connection became
			// unusable. It MUST classify as legacy.
			name:           "REGRESSION: legacy server's HTTP 200 + JSON-RPC -32601 body classifies legacy, not modern",
			discoverStatus: http.StatusOK,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","error":{"code":-32601,"message":"method not found: server/discover"}}`,
			wantVersion:    mcp.V20250326,
		},
		{
			name:           "200 with a JSON-RPC error of any other code also falls through to legacy",
			discoverStatus: http.StatusOK,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","error":{"code":-32000,"message":"server-defined error"}}`,
			wantVersion:    mcp.V20250326,
		},
		{
			name:           "200 with an unparsable body falls through to legacy",
			discoverStatus: http.StatusOK,
			discoverBody:   `this is not JSON at all {{{`,
			wantVersion:    mcp.V20250326,
		},
		{
			name:           "200 result present but missing supportedVersions falls through to legacy",
			discoverStatus: http.StatusOK,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","result":{"capabilities":{}}}`,
			wantVersion:    mcp.V20250326,
		},
		{
			name:           "400 with -32022 UnsupportedProtocolVersion and data.supported classifies modern at the highest known supported version",
			discoverStatus: http.StatusBadRequest,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","error":{"code":-32022,"message":"unsupported protocol version","data":{"supported":["2026-07-28","2025-11-25"],"requested":"1900-01-01"}}}`,
			wantVersion:    mcp.V20260728,
		},
		{
			name:           "400 with -32022 whose data.supported names only legacy revisions still resolves without falling back to probeLegacy",
			discoverStatus: http.StatusBadRequest,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","error":{"code":-32022,"message":"unsupported protocol version","data":{"supported":["2025-11-25"],"requested":"1900-01-01"}}}`,
			wantVersion:    mcp.V20251125,
		},
		{
			name:           "400 with -32022 but no data field at all falls back to V20260728 directly (the code alone already proves modern-awareness)",
			discoverStatus: http.StatusBadRequest,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","error":{"code":-32022,"message":"unsupported protocol version"}}`,
			wantVersion:    mcp.V20260728,
		},
		{
			// Regression test: data.supported present but naming NOTHING this
			// VoidLLM build recognizes is a genuine version mismatch, not
			// something to paper over with the V20260728 default — see
			// highestSupportedVersion's doc and
			// TestHighestSupportedVersion_UnknownOnly_NamesBothVersionLists
			// for the message-content assertion.
			name:           "400 with -32022 whose data.supported names ONLY unrecognized versions reports ErrUnsupportedProtocolVersion, not a silent default",
			discoverStatus: http.StatusBadRequest,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","error":{"code":-32022,"message":"unsupported protocol version","data":{"supported":["2099-01-01","1999-01-01"],"requested":"1900-01-01"}}}`,
			wantErr:        mcp.ErrUnsupportedProtocolVersion,
		},
		{
			name:           "400 with -32020 HeaderMismatch classifies modern at V20260728 directly",
			discoverStatus: http.StatusBadRequest,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","error":{"code":-32020,"message":"header mismatch"}}`,
			wantVersion:    mcp.V20260728,
		},
		{
			// Regression test: matching the numeric code alone is not enough
			// to prove modernity — a legacy server or unrelated gateway could
			// reuse -32020 for its own purposes. A body is only accepted as a
			// modern error when it is ALSO a well-formed JSON-RPC 2.0
			// response (see modernVersionFromErrorBody's doc).
			name:           "400 with -32020 but a MISSING jsonrpc 2.0 field is NOT recognized as a modern error and falls through to legacy",
			discoverStatus: http.StatusBadRequest,
			discoverBody:   `{"id":"probe-discover","error":{"code":-32020,"message":"header mismatch"}}`,
			wantVersion:    mcp.V20250326,
		},
		{
			name:           "400 with -32020 and a WRONG jsonrpc version string is NOT recognized as a modern error and falls through to legacy",
			discoverStatus: http.StatusBadRequest,
			discoverBody:   `{"jsonrpc":"1.0","id":"probe-discover","error":{"code":-32020,"message":"header mismatch"}}`,
			wantVersion:    mcp.V20250326,
		},
		{
			name:           "400 with -32021 MissingRequiredClientCapability classifies modern at V20260728 directly",
			discoverStatus: http.StatusBadRequest,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","error":{"code":-32021,"message":"missing capability"}}`,
			wantVersion:    mcp.V20260728,
		},
		{
			name:           "400 with no recognizable modern error body falls through to legacy",
			discoverStatus: http.StatusBadRequest,
			discoverBody:   `{"jsonrpc":"2.0","id":"probe-discover","error":{"code":-32602,"message":"invalid params"}}`,
			wantVersion:    mcp.V20250326,
		},
		{
			name:           "400 with an empty body falls through to legacy",
			discoverStatus: http.StatusBadRequest,
			discoverBody:   ``,
			wantVersion:    mcp.V20250326,
		},
		{
			name:           "404 on both server/discover and the legacy initialize fallback reports ErrSSENotSupported",
			discoverStatus: http.StatusNotFound,
			initStatus:     http.StatusNotFound,
			wantErr:        mcp.ErrSSENotSupported,
		},
		{
			name:           "405 on both server/discover and the legacy initialize fallback reports ErrSSENotSupported",
			discoverStatus: http.StatusMethodNotAllowed,
			initStatus:     http.StatusMethodNotAllowed,
			wantErr:        mcp.ErrSSENotSupported,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(probeEraHandler(tc.discoverStatus, tc.discoverBody, tc.initStatus, tc.initBody, nil, nil))
			t.Cleanup(srv.Close)

			tr := newProbeTransport(srv.URL)
			got, err := tr.ProbeEra(context.Background())

			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("ProbeEra() error = %v, want it to wrap %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ProbeEra() unexpected error: %v", err)
			}
			if got != tc.wantVersion {
				t.Errorf("ProbeEra() version = %q, want %q", got, tc.wantVersion)
			}
			if got.Era() != tc.wantVersion.Era() {
				t.Errorf("ProbeEra() era = %v, want %v (version %q)", got.Era(), tc.wantVersion.Era(), got)
			}
		})
	}
}

// TestProbeEra_DiscoverUnexpectedStatus_ReturnsGenericError verifies that an
// HTTP status on server/discover outside the set probeEra specifically
// handles (200/202, 400, 404/405) is reported as a plain error rather than
// silently falling back to legacy — an upstream answering 500 is broken, not
// merely unaware of server/discover.
func TestProbeEra_DiscoverUnexpectedStatus_ReturnsGenericError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	tr := newProbeTransport(srv.URL)
	_, err := tr.ProbeEra(context.Background())
	if err == nil {
		t.Fatal("ProbeEra() error = nil, want an error for HTTP 500 on server/discover")
	}
	if errors.Is(err, mcp.ErrSSENotSupported) {
		t.Errorf("error = %v, want a generic probe error, not ErrSSENotSupported (500 is not 404/405)", err)
	}
}

// TestProbeEra_LegacyFallbackUnexpectedStatus_ReturnsGenericError is the
// same check for probeLegacy's own fallthrough default: once server/discover
// has already fallen through to the legacy probe, an initialize response
// carrying a status probeLegacy does not specifically handle must also
// surface as a plain error.
func TestProbeEra_LegacyFallbackUnexpectedStatus_ReturnsGenericError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(probeEraHandler(
		http.StatusBadRequest, "", // discover: no recognizable modern error body -> falls through
		http.StatusInternalServerError, "upstream unavailable",
		nil, nil,
	))
	t.Cleanup(srv.Close)

	tr := newProbeTransport(srv.URL)
	_, err := tr.ProbeEra(context.Background())
	if err == nil {
		t.Fatal("ProbeEra() error = nil, want an error for HTTP 500 on the legacy initialize fallback")
	}
	if errors.Is(err, mcp.ErrSSENotSupported) {
		t.Errorf("error = %v, want a generic probe error, not ErrSSENotSupported (500 is not 404/405)", err)
	}
}

// TestHighestSupportedVersion_UnknownOnly_NamesBothVersionLists used to be
// named the same and asserted the opposite of what it asserts now: that the
// resulting error message contained the upstream's own data.supported
// strings verbatim. That was the privacy violation this test now guards
// against (docs/mcp-v2.md §11.5, review finding E): data.supported is an
// array of arbitrary, upstream-controlled strings — a malicious or
// misconfigured upstream can put anything in it — and this error's message
// is logged via err.Error() by every caller of ProbeEra. Echoing
// upstream-supplied free-form content into a log line is exactly what
// VoidLLM's zero-knowledge logging rule forbids.
//
// The fixed message instead names only the COUNT of versions the upstream
// listed (so an operator can tell "the upstream listed some versions we
// don't know" from "the upstream listed none at all") and VoidLLM's own
// supported-version list (so they can see what this build understands) —
// never the upstream's own strings, and never a silent default to
// V20260728 the way an absent data.supported does.
func TestHighestSupportedVersion_UnknownOnly_NamesBothVersionLists(t *testing.T) {
	t.Parallel()

	// A sentinel that would be unmistakable if it leaked into the error
	// message — distinct from an ordinary-looking date string like
	// "2099-01-01" so a false negative (the assertion below coincidentally
	// matching something else in the message) is not possible.
	const sentinelUpstreamVersion = "2099-01-01-attacker-controlled-marker"

	srv := httptest.NewServer(probeEraHandler(
		http.StatusBadRequest,
		fmt.Sprintf(`{"jsonrpc":"2.0","id":"probe-discover","error":{"code":-32022,"message":"unsupported protocol version","data":{"supported":[%q,"1999-01-01"],"requested":"1900-01-01"}}}`, sentinelUpstreamVersion),
		0, "", nil, nil,
	))
	t.Cleanup(srv.Close)

	tr := newProbeTransport(srv.URL)
	_, err := tr.ProbeEra(context.Background())
	if !errors.Is(err, mcp.ErrUnsupportedProtocolVersion) {
		t.Fatalf("ProbeEra() error = %v, want it to wrap ErrUnsupportedProtocolVersion", err)
	}

	// The core privacy assertion: the upstream's own data.supported strings
	// must never appear in the message, however distinctive they are.
	if strings.Contains(err.Error(), sentinelUpstreamVersion) {
		t.Errorf("error = %q, must NOT contain the upstream-controlled data.supported string %q "+
			"(zero-knowledge logging, docs/mcp-v2.md §11.5) — that array is arbitrary, "+
			"attacker-controlled text, not verified protocol version data", err.Error(), sentinelUpstreamVersion)
	}
	if strings.Contains(err.Error(), "1999-01-01") {
		t.Errorf("error = %q, must NOT contain the upstream-controlled data.supported string %q either",
			err.Error(), "1999-01-01")
	}

	// The message must still be useful: it names the COUNT of upstream
	// entries (2, here) instead of their content.
	if !strings.Contains(err.Error(), "2 supported version") {
		t.Errorf("error = %q, want it to mention the COUNT of upstream-listed versions (2), not the "+
			"versions themselves", err.Error())
	}

	for _, v := range mcp.SupportedVersions() {
		want := string(v)
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to mention voidllm's own supported version %q", err.Error(), want)
		}
	}
}

// TestHTTPTransport_InvalidPinnedVersion_StillProbes verifies resolveBinding's
// own defensiveness independent of ResolvePinnedVersion's normalization: an
// HTTPTransport constructed directly (bypassing ResolvePinnedVersion) with a
// pinnedVersion string that is not one of the four recognized revisions must
// still probe rather than silently misbehave on a Version it cannot
// classify.
func TestHTTPTransport_InvalidPinnedVersion_StillProbes(t *testing.T) {
	t.Parallel()

	var seen []string
	var mu sync.Mutex

	srv := httptest.NewServer(realCallHandler(
		http.StatusNotFound, "",
		http.StatusOK, "",
		&seen, &mu,
	))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.Version("not-a-real-version"), testStreamIdleTimeout)

	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || seen[0] != "server/discover" {
		t.Errorf("methods seen = %v, want the first to be server/discover (an invalid pin must not skip probing)", seen)
	}
}

// realCallHandler wraps probeEraHandler's probe responses with a generic
// success handler for every OTHER JSON-RPC method (e.g. "ping" or
// "notifications/initialized"), so the ProtocolVersion-override tests below
// can exercise a full Call — probe (if any) + Warmup + the real request —
// rather than only the probe step itself.
func realCallHandler(discoverStatus int, discoverBody string, initStatus int, initBody string, seen *[]string, mu *sync.Mutex) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		if seen != nil {
			mu.Lock()
			*seen = append(*seen, rpc.Method)
			mu.Unlock()
		}

		switch rpc.Method {
		case "server/discover":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(discoverStatus)
			if discoverBody != "" {
				fmt.Fprint(w, discoverBody)
			}
		case "initialize":
			status := initStatus
			if status == 0 {
				status = http.StatusOK
			}
			respBody := initBody
			if respBody == "" && status == http.StatusOK {
				// A 2xx initialize response with no body is not a realistic
				// upstream — legacyClientDialect.Warmup now requires a
				// well-formed JSON-RPC "result" to accept the handshake (see
				// jsonRPCInitializeOutcome). Every caller of realCallHandler
				// that leaves initBody empty wants a generically successful
				// legacy handshake, not this now-rejected empty-body case.
				respBody = defaultLegacyInitializeBody
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if respBody != "" {
				fmt.Fprint(w, respBody)
			}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			// Any other real method (e.g. "ping") succeeds unconditionally —
			// this handler exists to exercise the full Call path (probe, if
			// any, plus Warmup, plus the real request), not to test error
			// handling on the real request itself.
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		}
	})
}

// TestResolvePinnedVersion covers every case ResolvePinnedVersion documents:
// "auto"/"" mean probe (empty Version out), a recognized value pins that
// exact revision, and an unrecognized value degrades to "auto" rather than
// failing outright.
func TestResolvePinnedVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
		want mcp.Version
	}{
		{name: "empty string means auto", raw: "", want: ""},
		{name: `"auto" means auto`, raw: "auto", want: ""},
		{name: "a known legacy version pins exactly that version", raw: "2025-03-26", want: mcp.V20250326},
		{name: "a known modern version pins exactly that version", raw: "2026-07-28", want: mcp.V20260728},
		{name: "an unrecognized value degrades to auto instead of failing", raw: "not-a-real-version", want: ""},
		{name: "a forward-versioned value not yet built degrades to auto", raw: "2099-01-01", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := mcp.ResolvePinnedVersion(tc.raw)
			if got != tc.want {
				t.Errorf("ResolvePinnedVersion(%q) = %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// ---- ProtocolVersion override: auto probes, a pin skips probing entirely --

// TestHTTPTransport_ProtocolVersionAuto_Probes verifies that ResolvePinnedVersion("auto")
// (the empty Version) makes the transport actually probe the upstream via
// server/discover before ever attempting a real request.
func TestHTTPTransport_ProtocolVersionAuto_Probes(t *testing.T) {
	t.Parallel()

	var seen []string
	var mu sync.Mutex

	srv := httptest.NewServer(realCallHandler(
		http.StatusNotFound, "", // legacy server: server/discover is an unknown route
		http.StatusOK, "", // canned legacy initialize success
		&seen, &mu,
	))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.ResolvePinnedVersion("auto"), testStreamIdleTimeout)

	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(seen) == 0 || seen[0] != "server/discover" {
		t.Errorf("methods seen = %v, want the first to be server/discover (auto must probe)", seen)
	}
}

// TestHTTPTransport_ProtocolVersionPinned_SkipsProbe verifies the opposite:
// a pinned mcp_servers.protocol_version override must never call
// server/discover, even against an upstream that would honestly report
// itself as EraModern if asked — proving the pin, not a probe outcome, is
// what decided the era.
func TestHTTPTransport_ProtocolVersionPinned_SkipsProbe(t *testing.T) {
	t.Parallel()

	var seen []string
	var mu sync.Mutex

	// This upstream would classify as modern (V20260728) if actually probed:
	// server/discover succeeds with a genuine modern result.
	srv := httptest.NewServer(realCallHandler(
		http.StatusOK, `{"jsonrpc":"2.0","id":"probe-discover","result":{"supportedVersions":["2026-07-28"]}}`,
		http.StatusOK, "",
		&seen, &mu,
	))
	t.Cleanup(srv.Close)

	pinned := mcp.ResolvePinnedVersion(string(mcp.V20250326))
	if pinned != mcp.V20250326 {
		t.Fatalf("ResolvePinnedVersion setup: got %q, want %q", pinned, mcp.V20250326)
	}

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, pinned, testStreamIdleTimeout)

	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, m := range seen {
		if m == "server/discover" {
			t.Fatalf("methods seen = %v, want no server/discover at all — the pin must skip probing entirely", seen)
		}
	}
	// The pin resolved to a legacy version, so Warmup's own initialize
	// handshake must still have happened — this is legacy dialect wiring,
	// not the absence of any request at all.
	var sawInitialize bool
	for _, m := range seen {
		if m == "initialize" {
			sawInitialize = true
		}
	}
	if !sawInitialize {
		t.Errorf("methods seen = %v, want an initialize handshake (legacy pin)", seen)
	}
}

// ---- Probe-failure caching TTL (bindingProbeErrorTTL) ----------------------

// TestHTTPTransport_ProbeFailureCache_HealsAfterTTL verifies the full
// contract bindingProbeErrorTTL documents: a probe failure is cached and
// reused — the upstream is not re-probed on every call — for up to
// bindingProbeErrorTTL, and once that window has elapsed the very next call
// gets a fresh probe attempt, so a transient outage self-heals without an
// operator having to invalidate anything by hand.
//
// This test controls time via HTTPTransport.SetBindingErrAt
// (export_test.go) instead of a real 5-second sleep: bindingProbeErrorTTL
// (http_transport.go) is compared against time.Since(bindingErrAt) inside
// resolveBinding, which has no injectable clock of its own — SetBindingErrAt
// is a minimal, test-only hook added specifically so this property could be
// verified deterministically and quickly rather than either sleeping in the
// test suite or leaving the TTL's boundary behavior unverified. If this
// hook did not exist, this test could only be written with a real sleep past
// the TTL, at the cost of a slow, five-second-plus test.
func TestHTTPTransport_ProbeFailureCache_HealsAfterTTL(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var unhealthy = true
	var totalRequests int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		totalRequests++
		isUnhealthy := unhealthy
		mu.Unlock()

		if isUnhealthy {
			// An unexpected status on server/discover — probeEra's own
			// "default:" branch — is a genuine, cacheable probe failure,
			// distinct from a 404/405 that merely falls through to the
			// legacy probe (see probeEra's doc).
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "server/discover":
			// This upstream is legacy: server/discover is an unknown route.
			w.WriteHeader(http.StatusNotFound)
		case "initialize":
			fmt.Fprint(w, defaultLegacyInitializeBody)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"ok":true}}`)
		}
	}))
	t.Cleanup(srv.Close)

	// pinnedVersion is deliberately empty ("auto"): resolveBinding's probe-
	// failure caching only ever applies to the auto-detect branch — a pinned
	// transport never calls probeEra at all and so can never populate
	// bindingErr in the first place.
	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, "", testStreamIdleTimeout)

	// First Call: the upstream is unhealthy, so probeEra's server/discover
	// attempt itself fails with a generic (non-404/405) error. That failure
	// gets cached.
	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err == nil {
		t.Fatal("Call() #1 error = nil, want an error while the upstream is unhealthy")
	}

	mu.Lock()
	hitsAfterFirstFailure := totalRequests
	mu.Unlock()
	if hitsAfterFirstFailure != 1 {
		t.Fatalf("upstream received %d requests after Call #1, want exactly 1 (the failed server/discover attempt)", hitsAfterFirstFailure)
	}

	// Second Call, immediately after and still well within
	// bindingProbeErrorTTL: must reuse the cached failure WITHOUT hitting the
	// upstream again.
	_, err = tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err == nil {
		t.Fatal("Call() #2 error = nil, want the cached probe failure to still apply within the TTL window")
	}

	mu.Lock()
	hitsWithinTTL := totalRequests
	mu.Unlock()
	if hitsWithinTTL != hitsAfterFirstFailure {
		t.Errorf("upstream received %d requests after Call #2, want still %d — a cached probe failure within "+
			"the TTL window must not re-probe the upstream", hitsWithinTTL, hitsAfterFirstFailure)
	}

	// Simulate the TTL window having elapsed, and let the upstream recover —
	// exactly the "transient outage self-heals" scenario bindingProbeErrorTTL
	// exists for.
	tr.SetBindingErrAt(time.Now().Add(-2 * mcp.BindingProbeErrorTTL))
	mu.Lock()
	unhealthy = false
	mu.Unlock()

	got, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() #3 error = %v, want nil — once the TTL has elapsed the next call must re-probe and succeed", err)
	}
	if !strings.Contains(string(got.Body), `"ok":true`) {
		t.Errorf("Call() #3 response = %s, want it to contain the real ping result", got.Body)
	}

	mu.Lock()
	hitsAfterHealing := totalRequests
	mu.Unlock()
	if hitsAfterHealing <= hitsWithinTTL {
		t.Errorf("upstream received %d requests after Call #3, want more than %d — the elapsed TTL must trigger "+
			"a fresh probe attempt, not reuse the stale cached failure", hitsAfterHealing, hitsWithinTTL)
	}
}

// ---- Context errors must never poison resolveBinding's shared cache -------
//
// docs/mcp-v2.md, FIX 2: resolveBinding used to cache EVERY probeEra failure
// for bindingProbeErrorTTL, including context.Canceled and
// context.DeadlineExceeded — which describe the ONE caller's own context, not
// the upstream's reachability. A cancelled or timed-out first call against a
// fresh HTTPTransport (a client disconnecting early, a short per-request
// deadline) wedged that same context error onto every OTHER caller sharing
// this upstream — including different organizations with their own, perfectly
// healthy contexts — for the full TTL window, even though the upstream itself
// was never actually probed to completion. The tests below assert the fix's
// core, observable property: the very next call, with a good context, must
// succeed immediately — NOT wait out bindingProbeErrorTTL — because nothing
// was ever published to the shared resolution in the first place.

// TestHTTPTransport_ResolveBinding_ContextCanceled_DoesNotPoisonCache is the
// centerpiece of FIX 2: a first Call made with an already-cancelled context
// must fail for the caller, but must leave the shared binding resolution
// completely untouched, so an immediately following Call with a healthy
// context against the same HTTPTransport succeeds without any wait.
func TestHTTPTransport_ResolveBinding_ContextCanceled_DoesNotPoisonCache(t *testing.T) {
	t.Parallel()

	var seen []string
	var mu sync.Mutex

	// A perfectly healthy legacy upstream — nothing here is actually broken;
	// the ONLY thing that will fail is the first call's own context.
	srv := httptest.NewServer(realCallHandler(
		http.StatusNotFound, "", // legacy: server/discover is an unknown route
		http.StatusOK, "", // canned legacy initialize success
		&seen, &mu,
	))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, "", testStreamIdleTimeout)

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the call, so probeEra's rawPost fails immediately

	_, err := tr.Call(canceledCtx, &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err == nil {
		t.Fatal("Call() with an already-cancelled context: error = nil, want a context.Canceled error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Call() error = %v, want it to wrap context.Canceled", err)
	}

	// The core assertion: nothing was published to the shared resolution.
	if gen := tr.CurrentBindingGeneration(); gen != nil {
		t.Errorf("CurrentBindingGeneration() = %v, want nil — a context-caused probe failure must never "+
			"publish a cached resolution", gen)
	}

	// The second call, immediately after and with a healthy context, must
	// succeed right away — no bindingProbeErrorTTL wait, because the first
	// call's context.Canceled was never cached as a genuine probe failure.
	got, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() immediately after a cancelled-context failure, with a healthy context, error = %v, want nil "+
			"— a caller-specific context error must never poison the shared binding cache", err)
	}
	if strings.Contains(string(got.Body), `"error"`) {
		t.Errorf("Call() response = %s, want the real ping result with no error", got.Body)
	}
}

// TestHTTPTransport_ResolveBinding_ContextDeadlineExceeded_DoesNotPoisonCache
// is the same property for the other context error resolveBinding must never
// cache: a deadline that has already elapsed before the call even starts.
func TestHTTPTransport_ResolveBinding_ContextDeadlineExceeded_DoesNotPoisonCache(t *testing.T) {
	t.Parallel()

	var seen []string
	var mu sync.Mutex

	srv := httptest.NewServer(realCallHandler(
		http.StatusNotFound, "",
		http.StatusOK, "",
		&seen, &mu,
	))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, "", testStreamIdleTimeout)

	expiredCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
	defer cancel()

	_, err := tr.Call(expiredCtx, &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err == nil {
		t.Fatal("Call() with an already-expired deadline: error = nil, want a context.DeadlineExceeded error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call() error = %v, want it to wrap context.DeadlineExceeded", err)
	}

	if gen := tr.CurrentBindingGeneration(); gen != nil {
		t.Errorf("CurrentBindingGeneration() = %v, want nil — a context-caused probe failure must never "+
			"publish a cached resolution", gen)
	}

	got, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() immediately after an expired-deadline failure, with a healthy context, error = %v, want nil "+
			"— a caller-specific context error must never poison the shared binding cache", err)
	}
	if strings.Contains(string(got.Body), `"error"`) {
		t.Errorf("Call() response = %s, want the real ping result with no error", got.Body)
	}
}

// TestHTTPTransport_ResolveBinding_GenuineFailureStillCached is the
// counterpart the fix must NOT weaken: an ordinary, non-context probe failure
// (an upstream answering HTTP 500) is still cached and reused for
// bindingProbeErrorTTL — this is the same property
// TestHTTPTransport_ProbeFailureCache_HealsAfterTTL already verifies in full;
// this test isolates just the "still cached, upstream not hit again
// immediately" half of it as a direct counterpoint sitting next to the two
// context tests above, so the distinction between "caller's own context" and
// "genuine upstream failure" is pinned down in one place.
func TestHTTPTransport_ResolveBinding_GenuineFailureStillCached(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var totalRequests int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		totalRequests++
		mu.Unlock()
		// An unexpected status on server/discover is a genuine, cacheable
		// probe failure (probeEra's own "default:" branch) — never a context
		// error, so it must be cached exactly as before this fix.
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, "", testStreamIdleTimeout)

	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err == nil {
		t.Fatal("Call() #1 error = nil, want an error while the upstream answers 500")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call() #1 error = %v, want a genuine upstream failure, not a context error", err)
	}

	mu.Lock()
	hitsAfterFirstFailure := totalRequests
	mu.Unlock()

	// A second call, still within bindingProbeErrorTTL, must reuse the cached
	// failure without hitting the upstream again — unlike the context-error
	// tests above, THIS failure is a property of the upstream and must stay
	// cached.
	_, err = tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err == nil {
		t.Fatal("Call() #2 error = nil, want the cached genuine probe failure to still apply within the TTL window")
	}

	mu.Lock()
	hitsWithinTTL := totalRequests
	mu.Unlock()
	if hitsWithinTTL != hitsAfterFirstFailure {
		t.Errorf("upstream received %d requests after Call #2, want still %d — a genuine cached probe failure "+
			"within the TTL window must not re-probe the upstream", hitsWithinTTL, hitsAfterFirstFailure)
	}
}
