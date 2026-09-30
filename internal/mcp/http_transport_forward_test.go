package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers HTTPTransport.Forward — the transparent-intermediary path
// HandleMCPProxy uses, as opposed to Call — which VoidLLM uses as its own MCP
// client (ListTools, CallMCPTool). See Forward's doc in http_transport.go for
// the full contract; these tests pin down the three properties that doc
// promises: exactly one handshake on the wire for a proxied legacy client, no
// interpretation of the upstream's response (MRTR shapes and error statuses
// pass through byte-identical), and no session state of VoidLLM's own.

// forwardMethodLogHandler returns an httptest handler that records the
// JSON-RPC "method" of every request it receives, in order, and answers
// "initialize" with a well-formed legacy success (assigning a fresh,
// numbered Mcp-Session-Id each time) and "server/discover" with the given
// status/body — everything else gets a generic success. mu guards seen.
func forwardMethodLogHandler(discoverStatus int, discoverBody string, seen *[]string, mu *sync.Mutex) http.Handler {
	var initCount int
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		mu.Lock()
		*seen = append(*seen, rpc.Method)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "server/discover":
			w.WriteHeader(discoverStatus)
			if discoverBody != "" {
				fmt.Fprint(w, discoverBody)
			}
		case "initialize":
			mu.Lock()
			initCount++
			session := fmt.Sprintf("orphaned-probe-session-%d", initCount)
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", session)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		}
	})
}

// clientInitializeRequest is the JSON-RPC body a real legacy MCP client sends
// as its own opening handshake — this is the payload a legacy client proxied
// through /api/v1/mcp/:alias actually POSTs, exactly as its own SDK would
// construct it, unrelated to any handshake VoidLLM itself might separately
// perform.
const clientInitializeRequest = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"legacy-client","version":"1.0"}}}`

// TestForward_Legacy_Pinned_SendsExactlyOneHandshake is the regression test
// for the original bug (docs/mcp-v2.md, FIX 1/FIX 3): a legacy client sends
// its own "initialize" to /api/v1/mcp/:alias, and HandleMCPProxy relays it
// via Forward. Forward performs no warmup of its own, so the upstream must
// see EXACTLY the client's single "initialize" — never a second, VoidLLM-
// initiated one alongside it.
func TestForward_Legacy_Pinned_SendsExactlyOneHandshake(t *testing.T) {
	t.Parallel()

	var seen []string
	var mu sync.Mutex

	srv := httptest.NewServer(forwardMethodLogHandler(http.StatusNotFound, "", &seen, &mu))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20250326, testStreamIdleTimeout)

	res, err := tr.Forward(context.Background(), []byte(clientInitializeRequest), mcp.MapHeader{})
	if err != nil {
		t.Fatalf("Forward() error = %v, want nil", err)
	}
	res.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 1 || seen[0] != "initialize" {
		t.Fatalf("methods seen by upstream = %v, want exactly [\"initialize\"] — Forward must never add its "+
			"own handshake around the caller's own (docs/mcp-v2.md, FIX 1/FIX 3)", seen)
	}
}

// TestCall_Legacy_Pinned_StillWrapsClientPayloadInItsOwnWarmup is the direct
// contrast to TestForward_Legacy_Pinned_SendsExactlyOneHandshake: Call is
// VoidLLM acting as its OWN MCP client (ListTools, CallMCPTool), so its own
// warmup handshake around whatever payload the caller passes in is correct,
// intentional behavior for that role — this is the exact sequence
// [initialize, notifications/initialized, initialize] that was the
// ORIGINAL, pre-split bug when it leaked onto the Forward/proxy path. Here,
// on Call, it is not a bug: it is what distinguishes Call's role from
// Forward's.
func TestCall_Legacy_Pinned_StillWrapsClientPayloadInItsOwnWarmup(t *testing.T) {
	t.Parallel()

	var seen []string
	var mu sync.Mutex

	srv := httptest.NewServer(forwardMethodLogHandler(http.StatusNotFound, "", &seen, &mu))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20250326, testStreamIdleTimeout)

	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(clientInitializeRequest)}, "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{"initialize", "notifications/initialized", "initialize"}
	if len(seen) != len(want) {
		t.Fatalf("methods seen by upstream = %v, want %v", seen, want)
	}
	for i, m := range want {
		if seen[i] != m {
			t.Errorf("methods seen by upstream = %v, want %v", seen, want)
			break
		}
	}
}

// TestForward_Auto_FirstContactStillProbesOnceAndOrphansASession documents,
// deliberately and by name, the current, INTENTIONAL state of the "auto"
// (no pinned protocol_version) case on the Forward path: era resolution is a
// property of the upstream SERVER, cached for the lifetime of this
// HTTPTransport (resolveBinding, docs/mcp-v2.md §4.7) — not of any one
// request — so on the very FIRST call through a fresh transport, the probe
// itself still runs once, ahead of the caller's own request, even on the
// Forward path where no further warmup follows it.
//
// For a legacy client proxied through /api/v1/mcp/:alias with no pinned
// protocol_version, that first contact costs a "server/discover" probe (this
// upstream is legacy, so it falls through) PLUS the probe's own "initialize"
// (probeLegacy) — which itself may mint an upstream session, per the legacy
// era's semantics — immediately followed by the client's own "initialize",
// forwarded unmodified. The probe's own session (asserted below by its
// distinctive "orphaned-probe-session-1" value) is never referenced again by
// anything: Forward holds no session state of its own (see Forward's doc),
// so that upstream-side session is orphaned from the moment it is minted.
//
// This is intentionally documented as a red-if-changed test, not a
// currently-desired property: if the era probe is later changed to be
// per-request-cost-free, or Forward is changed to skip probing on an
// unpinned transport, this test must fail rather than silently keep passing
// against a different reality. Every SUBSEQUENT call through the same
// HTTPTransport reuses the cached binding and pays none of this cost (see
// resolveBinding's doc) — only first contact does.
func TestForward_Auto_FirstContactStillProbesOnceAndOrphansASession(t *testing.T) {
	t.Parallel()

	var seen []string
	var mu sync.Mutex

	// discoverStatus 404: this upstream is a legacy server that does not
	// implement server/discover at all, forcing probeEra to fall through to
	// probeLegacy exactly as a real legacy-only upstream would.
	srv := httptest.NewServer(forwardMethodLogHandler(http.StatusNotFound, "", &seen, &mu))
	t.Cleanup(srv.Close)

	// pinnedVersion is deliberately the empty Version ("auto"): no pin, so
	// resolveBinding must probe on first use.
	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, "", testStreamIdleTimeout)

	res, err := tr.Forward(context.Background(), []byte(clientInitializeRequest), mcp.MapHeader{})
	if err != nil {
		t.Fatalf("Forward() error = %v, want nil", err)
	}
	defer res.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	want := []string{"server/discover", "initialize", "initialize"}
	if len(seen) != len(want) {
		t.Fatalf("methods seen by upstream = %v, want %v — first contact on an unpinned transport still "+
			"probes once, even on the Forward path (see this test's doc)", seen, want)
	}
	for i, m := range want {
		if seen[i] != m {
			t.Fatalf("methods seen by upstream = %v, want %v", seen, want)
		}
	}

	// The response the CALLER actually receives is the upstream's answer to
	// the client's own "initialize" — the third request above — which this
	// fake upstream answers identically to the probe's own initialize,
	// including minting its own, second, "orphaned-probe-session-2" — never
	// referenced by anything after this point, exactly as the doc above
	// describes for the probe's session. This assertion exists only to make
	// that orphaning concrete rather than an assertion-free narrative.
	if got := res.Header.Get("Mcp-Session-Id"); got != "orphaned-probe-session-2" {
		t.Errorf("Mcp-Session-Id on the response actually returned to the caller = %q, want %q "+
			"(the second initialize call, i.e. the client's own, forwarded unmodified)", got, "orphaned-probe-session-2")
	}
}

// ---- MRTR and error status pass-through ------------------------------------

// mrtrInputRequiredBody is a realistic InputRequiredResult (docs/mcp-v2.md
// §3.7): a modern-era upstream responding to tools/call with a request for
// more input, carrying both inputRequests and an opaque requestState the
// spec requires Forward's caller — the real MCP client on the other side of
// the proxy — to reflect back byte-for-byte, unparsed, on retry.
const mrtrInputRequiredBody = `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","inputRequests":{"github_login":{"method":"elicitation/create","params":{"mode":"form","message":"Please provide your GitHub username","requestedSchema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}}},"requestState":"AEAD-protected-opaque-blob=="}}`

// TestForward_MRTR_InputRequired_PassedThroughByteIdentical verifies the
// core Forward contract for MRTR (docs/mcp-v2.md §3.7): Call's doCall feeds
// every modern-era 200 body through the dialect's Parse to detect
// resultType:"input_required" and turns it into a Go error, because Call's
// own callers (ListTools, CallMCPTool) cannot act on that shape — but
// Forward's caller is the real MCP client on the other side of the proxy,
// which the spec obligates to retry with inputResponses. Forward must
// therefore hand that shape through completely unexamined: not merely
// "without erroring", but byte-for-byte identical to what the upstream sent,
// since requestState must never be altered.
func TestForward_MRTR_InputRequired_PassedThroughByteIdentical(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, mrtrInputRequiredBody)
	}))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)

	res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`), mcp.MapHeader{})
	if err != nil {
		t.Fatalf("Forward() error = %v, want nil — an input_required result is not a transport error on this path", err)
	}
	if res.Status != http.StatusOK {
		t.Errorf("Status = %d, want %d", res.Status, http.StatusOK)
	}
	defer res.Body.Close()
	gotBody, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read res.Body: %v", err)
	}
	if !bytes.Equal(gotBody, []byte(mrtrInputRequiredBody)) {
		t.Errorf("Body = %s,\nwant byte-identical to the upstream's response:\n%s", gotBody, mrtrInputRequiredBody)
	}
}

// TestForward_UpstreamErrorStatus_PassedThroughUnchanged verifies the second
// half of the "no interpretation" contract: a non-2xx/202 upstream response
// — even a well-formed JSON-RPC error body — must reach the caller with the
// SAME status code and the SAME body, not collapsed into a generic
// transport error (which the pre-Forward implementation did, turning every
// non-2xx into a Go error HandleMCPProxy then mapped to a flat 502).
func TestForward_UpstreamErrorStatus_PassedThroughUnchanged(t *testing.T) {
	t.Parallel()

	const upstreamErrorBody = `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid params: missing required field \"region\""}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, upstreamErrorBody)
	}))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)

	res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`), mcp.MapHeader{})
	if err != nil {
		t.Fatalf("Forward() error = %v, want nil — an upstream HTTP error status is not itself a transport "+
			"error on this path; it must reach the caller as-is", err)
	}
	if res.Status != http.StatusBadRequest {
		t.Errorf("Status = %d, want %d", res.Status, http.StatusBadRequest)
	}
	defer res.Body.Close()
	gotBody, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read res.Body: %v", err)
	}
	if !bytes.Equal(gotBody, []byte(upstreamErrorBody)) {
		t.Errorf("Body = %s,\nwant byte-identical to the upstream's error response:\n%s", gotBody, upstreamErrorBody)
	}
}

// ---- Forward is session-blind: it neither checks nor tracks any session, ---
// ---- for EITHER era (docs/mcp-v2.md, "Designkorrektur A" reverted) ---------
//
// An intermediate revision of the session-isolation fix had Forward itself
// check an inbound Mcp-Session-Id against a per-SessionScope record of what
// the upstream had actually issued (the two tests this section used to
// contain: UnknownDiscardedBeforeUpstream_KnownRelayed and
// ConsecutiveCalls_KnownSessionsNeverMixed_UnknownNeverRelayed). That gate
// was wrong on two independent counts, both documented on Forward's own doc
// in http_transport.go:
//
//  1. It was reachable only for the legacy era (the modern era never had a
//     scopedState to check against in the first place) — but a dual-era
//     upstream that answers server/discover as MODERN, and therefore
//     resolves to an EraModern binding, may still accept headerless legacy
//     traffic at the very same endpoint. A caller doing exactly that sailed
//     through completely unexamined, because the check never even ran
//     against a binding that merely LOOKED modern to whichever caller
//     happened to probe it first. See
//     TestMCPProxy_DualEraUpstream_ModernBindingStillLeaksLegacySessions_
//     WithoutHeaderGatedCheck in internal/api/admin for the full-stack
//     regression test this now protects against.
//  2. The store it checked against lived on *eraBinding, which does not
//     outlive a single ad-hoc transport built for a cache-miss request
//     (buildAdHocTransport) — so a caller's own, entirely legitimate session
//     could be forgotten before its own follow-up request ever arrived.
//
// Both problems are fixed by moving the entire mechanism OUT of Forward and
// UP to HandleMCPProxy (internal/api/admin/mcp_proxy.go), which checks and
// records via a *mcp.SessionRegistry it holds independently of any one
// HTTPTransport (see that type's own doc), and which gates the check on
// whether the caller's request carries an Mcp-Session-Id header AT ALL,
// never on what era anything resolved to. Forward itself, correspondingly,
// lost its scope parameter entirely (it is once again `Forward(ctx, raw,
// hdr)`, exactly as before the intermediate revision) and is once more a
// pure relay: whatever Mcp-Session-Id hdr carries reaches the upstream
// unconditionally, and whatever the upstream responds with is mirrored back
// unconditionally — regardless of era, regardless of whether Forward has
// ever seen that value before, because it never tracks anything at all. The
// tests below pin exactly that: the property the OLD (pre-Designkorrektur)
// test suite once asserted only for the modern era
// (TestForward_Modern_SessionHeaderNeverCheckedRegardlessOfScope, deleted
// from forward_session_binding_test.go along with the rest of that file's
// now-relocated content) now holds for BOTH eras — that asymmetry was
// exactly the shape of the bug.

// TestForward_NeverChecksOrTracksAnySession_BothErasRelayUnconditionally is
// the direct, deliberate inversion of the deleted
// TestForward_Modern_SessionHeaderNeverCheckedRegardlessOfScope: that test's
// name and body asserted the era-gated check as INTENDED behavior for
// legacy, checking only that modern was exempt from it. Since the whole
// mechanism has moved out of Forward, the correct assertion is the opposite
// of what "regardless of scope" implied — Forward must relay an
// Mcp-Session-Id it has never seen before unconditionally on the LEGACY era
// too, exactly as it already does on modern, and must never allocate any
// per-scope bookkeeping of its own (ScopedStateCount stays 0) for either.
func TestForward_NeverChecksOrTracksAnySession_BothErasRelayUnconditionally(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		pinned mcp.Version
	}{
		{
			name:   "legacy era",
			pinned: mcp.V20250326,
		},
		{
			name:   "modern era",
			pinned: mcp.V20260728,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			var gotHeaders []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				gotHeaders = append(gotHeaders, r.Header.Get("Mcp-Session-Id"))
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete"}}`)
			}))
			t.Cleanup(srv.Close)

			tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
				"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, tc.pinned, testStreamIdleTimeout)

			const neverIssued = "never-issued-to-any-scope-under-any-era"
			const rounds = 3

			for i := 0; i < rounds; i++ {
				res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`),
					mcp.MapHeader{"Mcp-Session-Id": neverIssued})
				if err != nil {
					t.Fatalf("round %d: Forward() error = %v, want nil", i, err)
				}
				res.Body.Close()

				if got := tr.ScopedStateCount(); got != 0 {
					t.Errorf("after round %d: ScopedStateCount() = %d, want 0 — Forward must never allocate any "+
						"per-scope bookkeeping of its own, for either era", i, got)
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if len(gotHeaders) != rounds {
				t.Fatalf("upstream saw %d requests, want %d", len(gotHeaders), rounds)
			}
			for i, got := range gotHeaders {
				if got != neverIssued {
					t.Errorf("round %d: Mcp-Session-Id seen by upstream = %q, want %q — Forward relays it "+
						"unconditionally, checking nothing, for BOTH eras", i, got, neverIssued)
				}
			}
		})
	}
}

// TestForward_ResponseSessionHeader_AlwaysMirroredBackUnconditionally proves
// the response-side half of the same property: whatever Mcp-Session-Id the
// upstream answers with — including one that contradicts what the caller
// sent, which a session-tracking Forward would have had an opinion about —
// is mirrored back into ForwardResult.Header completely unexamined. Forward
// does not compare it against anything, does not record it anywhere (that is
// HandleMCPProxy's job now, via SessionRegistry.Record), and does not treat
// it as more or less trustworthy than any other response header.
func TestForward_ResponseSessionHeader_AlwaysMirroredBackUnconditionally(t *testing.T) {
	t.Parallel()

	const inboundSession = "caller-supplied-session"
	const upstreamAnswer = "upstream-rotated-to-a-completely-different-session"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", upstreamAnswer)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20250326, testStreamIdleTimeout)

	res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`),
		mcp.MapHeader{"Mcp-Session-Id": inboundSession})
	if err != nil {
		t.Fatalf("Forward() error = %v, want nil", err)
	}
	defer res.Body.Close()

	if got := res.Header.Get("Mcp-Session-Id"); got != upstreamAnswer {
		t.Errorf("ForwardResult.Header[Mcp-Session-Id] = %q, want %q — Forward mirrors back exactly what the "+
			"upstream answered with, unconditionally, without comparing it to what the caller sent", got, upstreamAnswer)
	}
}

// ---- Auth headers on the Forward path --------------------------------------

// TestForward_Auth_HeaderReachesUpstream is table-driven coverage for
// Forward's own auth-header branch (http_transport.go, the switch on
// t.authType right after the Accept header is set) — a straight duplicate of
// the buffered rawPost path's auth handling, but unexercised anywhere else in
// this file, which otherwise only ever constructs "none"-auth transports.
func TestForward_Auth_HeaderReachesUpstream(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		authType      string
		authHeader    string
		authToken     string
		wantHeaderKey string
		wantHeaderVal string
	}{
		{
			name:          "bearer",
			authType:      "bearer",
			authToken:     "secret-bearer-token",
			wantHeaderKey: "Authorization",
			wantHeaderVal: "Bearer secret-bearer-token",
		},
		{
			name:          "header",
			authType:      "header",
			authHeader:    "X-API-Key",
			authToken:     "secret-api-key",
			wantHeaderKey: "X-Api-Key",
			wantHeaderVal: "secret-api-key",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var gotVal string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotVal = r.Header.Get(tc.wantHeaderKey)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			}))
			t.Cleanup(srv.Close)

			tr := mcp.NewHTTPTransport(srv.URL, tc.authType, tc.authHeader, tc.authToken, 5*time.Second, true,
				"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)

			res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), mcp.MapHeader{})
			if err != nil {
				t.Fatalf("Forward() error = %v, want nil", err)
			}
			res.Body.Close()

			if gotVal != tc.wantHeaderVal {
				t.Errorf("upstream received %s = %q, want %q", tc.wantHeaderKey, gotVal, tc.wantHeaderVal)
			}
		})
	}
}

// TestForward_AuthHeaderVsMCPHeaderCollision_AuthHeaderProtected verifies
// the corrected collision rule documented above rawPost's and Forward's own
// authentication switch statements (docs/mcp-v2.md, review finding C4):
// authentication is still applied to the outbound request BEFORE hdr is
// merged in, but the merge itself (applyExtraHeaders) now refuses to let ANY
// entry in hdr — including one of the four MCP standard request headers,
// not only a Mcp-Param-{Name} one — overwrite whichever header just carried
// this transport's own credential. This inverts what this test used to
// assert (see git history): letting hdr unconditionally win, the previous
// rule, was safe only as long as hdr could never carry attacker-influenced
// content; since dialect2026Client.Prepare started mirroring x-mcp-header
// annotations from an UPSTREAM's own tool schema onto hdr on the Call path,
// that assumption no longer holds, so the guard now protects the
// credential-carrying header unconditionally instead of special-casing
// which family of header collided with it.
//
// Server registration (isReservedMCPHeader, internal/api/admin/mcp_servers.go)
// already refuses to let an admin configure auth_header as one of the
// reserved MCP header names in the first place — see
// TestCreateMCPServer_API_RejectsReservedAuthHeader /
// TestUpdateMCPServer_API_RejectsReservedAuthHeader in internal/api/admin —
// but this test exercises HTTPTransport directly, bypassing that
// registration-time guard entirely, to prove the merge-time protection holds
// even for an HTTPTransport constructed with an already-misconfigured
// auth_header (e.g. a server registered before that validation existed, or a
// future caller of NewHTTPTransport that skips mcp_servers.go).
func TestForward_AuthHeaderVsMCPHeaderCollision_AuthHeaderProtected(t *testing.T) {
	t.Parallel()

	const authValue = "attacker-controlled-auth-value"
	const callerMCPMethodValue = "tools/call"

	var gotHeaderValues []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaderValues = r.Header.Values(mcp.HeaderMethod)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	// authType "header" with authHeader deliberately set to mcp.HeaderMethod
	// ("Mcp-Method") itself — the exact misconfiguration
	// isReservedMCPHeader/reservedMCPProtocolHeaders exists to reject at
	// registration time, constructed here directly to test Forward's own
	// ordering in isolation.
	tr := mcp.NewHTTPTransport(srv.URL, "header", mcp.HeaderMethod, authValue, 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)

	// hdr carries the caller's own already-validated Mcp-Method — exactly
	// what forwardHeaders (mcp_proxy.go) builds from an inbound request.
	hdr := mcp.MapHeader{mcp.HeaderMethod: callerMCPMethodValue}

	res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call"}`), hdr)
	if err != nil {
		t.Fatalf("Forward() error = %v, want nil", err)
	}
	res.Body.Close()

	if len(gotHeaderValues) != 1 {
		t.Fatalf("upstream received %d values for %s, want exactly 1: %v", len(gotHeaderValues), mcp.HeaderMethod, gotHeaderValues)
	}
	if gotHeaderValues[0] != authValue {
		t.Errorf("upstream received %s = %q, want %q (the credential-carrying header must never be "+
			"overwritten by an hdr entry, even one of the caller's own MCP standard headers)",
			mcp.HeaderMethod, gotHeaderValues[0], authValue)
	}
}

// TestCall_UpstreamMirroredParamHeader_NeverOverwritesAuthHeader is the
// Call-path regression test for the actual scenario that made the previous
// "hdr always wins" rule unsafe: dialect2026Client.Prepare mirrors an
// x-mcp-header annotation the UPSTREAM's own tool schema declared onto hdr
// (MCP 2026-07-28 §4.3) — content this transport did not choose and cannot
// vet — before rawPost ever sees it. This drives that real path end to end
// (Call -> doCall -> dialect2026Client.Prepare -> rawPost), with
// req.HeaderParams carrying a binding whose rendered header name,
// Mcp-Param-Token, is made to collide with this server's own configured
// auth_header (authType "header", possible for a server registered before
// isReservedMCPHeader forbade it — see the Forward-path sibling test's own
// doc for the full argument). The header the upstream actually receives
// must carry the transport's own credential, never the tool-argument value
// Prepare mirrored.
func TestCall_UpstreamMirroredParamHeader_NeverOverwritesAuthHeader(t *testing.T) {
	t.Parallel()

	const authValue = "the-real-credential"
	const mirroredArgValue = "attacker-chosen-tool-argument"
	paramHeaderName := mcp.HeaderParamPrefix + "Token"

	var gotHeaderValues []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaderValues = r.Header.Values(paramHeaderName)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete"}}`)
	}))
	t.Cleanup(srv.Close)

	// A server registered before isReservedMCPHeader forbade it, with
	// auth_header pointed at exactly the Mcp-Param-Token name a
	// dialect2026Client.Prepare-rendered x-mcp-header binding would use.
	tr := mcp.NewHTTPTransport(srv.URL, "header", paramHeaderName, authValue, 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)

	req := &mcp.CallRequest{
		Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"execute_sql","arguments":{"token":"` + mirroredArgValue + `"}}}`),
		HeaderParams: []mcp.HeaderParam{
			{Name: "Token", Path: []string{"token"}, Kind: mcp.HeaderParamKindString},
		},
	}
	if _, err := tr.Call(context.Background(), req, ""); err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}

	if len(gotHeaderValues) != 1 {
		t.Fatalf("upstream received %d values for %s, want exactly 1: %v", len(gotHeaderValues), paramHeaderName, gotHeaderValues)
	}
	if gotHeaderValues[0] != authValue {
		t.Errorf("upstream received %s = %q, want %q (an upstream-mirrored Mcp-Param-* header must never "+
			"overwrite this transport's own configured credential)", paramHeaderName, gotHeaderValues[0], authValue)
	}
}

// ---- Transport-level failure: before the first byte ------------------------

// TestForward_TransportError_ConnectionRefused_ReturnsErrorBeforeAnyBody
// covers Forward's t.streamClient.Do error branch: a connection failure that
// happens before the upstream ever answers must surface as a plain Go error,
// with no *ForwardResult returned at all — the caller (HandleMCPProxy) can
// then answer with an ordinary error response instead of a half-open stream
// (see Forward's doc, and TestMCPProxy_UpstreamUnreachable_ReturnsOrdinaryErrorResponse_NoHalfOpenStream
// in internal/api/admin for the full-stack counterpart).
func TestForward_TransportError_ConnectionRefused_ReturnsErrorBeforeAnyBody(t *testing.T) {
	t.Parallel()

	// Reserve and immediately release an ephemeral port: reliably nothing is
	// listening there.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve ephemeral port: %v", err)
	}
	unreachable := "http://" + ln.Addr().String() + "/mcp"
	if err := ln.Close(); err != nil {
		t.Fatalf("release ephemeral port: %v", err)
	}

	tr := mcp.NewHTTPTransport(unreachable, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)

	res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), mcp.MapHeader{})
	if err == nil {
		res.Body.Close()
		t.Fatal("Forward() error = nil, want a transport error for an unreachable upstream")
	}
	if res != nil {
		t.Errorf("Forward() result = %+v, want nil alongside a non-nil error", res)
	}
}
