package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// fakeRoundTripper is a minimal, in-memory mcp.RoundTripper for unit-testing
// a ClientDialect's Warmup/Prepare wiring without any real HTTP server. Each
// call is dispatched by JSON-RPC method to a caller-supplied responder;
// requests and headers seen are recorded for assertions.
type fakeRoundTripper struct {
	// responders maps a JSON-RPC method name to the response it should
	// produce. A method with no responder gets a bare empty success.
	responders map[string]func(raw []byte, hdr mcp.MapHeader) (*mcp.RoundTripResult, error)

	calls []fakeRTCall
}

type fakeRTCall struct {
	method string
	raw    []byte
	hdr    mcp.MapHeader
}

func (f *fakeRoundTripper) RoundTrip(_ context.Context, raw []byte, hdr mcp.MapHeader) (*mcp.RoundTripResult, error) {
	var probe struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(raw, &probe)
	f.calls = append(f.calls, fakeRTCall{method: probe.Method, raw: raw, hdr: hdr})

	if r, ok := f.responders[probe.Method]; ok {
		return r(raw, hdr)
	}
	return &mcp.RoundTripResult{
		Status: http.StatusOK,
		Body:   []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`),
		Header: mcp.MapHeader{},
	}, nil
}

// TestLegacyClientDialect_Warmup_SendsInitializeThenNotificationsInitialized
// verifies the exact handshake order and that the session ID the upstream
// returns on initialize (via Mcp-Session-Id) is recorded into UpstreamState.
func TestLegacyClientDialect_Warmup_SendsInitializeThenNotificationsInitialized(t *testing.T) {
	t.Parallel()

	const session = "warmup-session-1"

	rt := &fakeRoundTripper{
		responders: map[string]func([]byte, mcp.MapHeader) (*mcp.RoundTripResult, error){
			"initialize": func(_ []byte, _ mcp.MapHeader) (*mcp.RoundTripResult, error) {
				return &mcp.RoundTripResult{
					Status: http.StatusOK,
					Body:   []byte(`{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26"}}`),
					Header: mcp.MapHeader{"Mcp-Session-Id": session},
				}, nil
			},
		},
	}

	d := mcp.NewLegacyClientDialect(mcp.V20250326, mcp.ClientInfo{Name: "voidllm-test", Version: "1.0"})
	st := &mcp.UpstreamState{}

	if err := d.Warmup(context.Background(), rt, st); err != nil {
		t.Fatalf("Warmup() error = %v, want nil", err)
	}

	if len(rt.calls) != 2 {
		t.Fatalf("calls = %v, want exactly 2 (initialize, notifications/initialized)", rt.calls)
	}
	if rt.calls[0].method != "initialize" {
		t.Errorf("calls[0].method = %q, want %q", rt.calls[0].method, "initialize")
	}
	if rt.calls[1].method != "notifications/initialized" {
		t.Errorf("calls[1].method = %q, want %q", rt.calls[1].method, "notifications/initialized")
	}
	if got := st.SessionID(); got != session {
		t.Errorf("SessionID() = %q, want %q", got, session)
	}
	// notifications/initialized must carry the session it just learned.
	if got := rt.calls[1].hdr.Get("Mcp-Session-Id"); got != session {
		t.Errorf("notifications/initialized Mcp-Session-Id = %q, want %q", got, session)
	}
}

// TestLegacyClientDialect_Warmup_NoSessionReturned_Tolerated verifies that a
// server which never hands out a session ID at all does not fail Warmup —
// some legacy servers behave statelessly in practice even though the era
// formally has a session concept.
func TestLegacyClientDialect_Warmup_NoSessionReturned_Tolerated(t *testing.T) {
	t.Parallel()

	rt := &fakeRoundTripper{} // default responder: no Mcp-Session-Id header

	d := mcp.NewLegacyClientDialect(mcp.V20250326, mcp.ClientInfo{})
	st := &mcp.UpstreamState{}

	if err := d.Warmup(context.Background(), rt, st); err != nil {
		t.Fatalf("Warmup() error = %v, want nil", err)
	}
	if got := st.SessionID(); got != "" {
		t.Errorf("SessionID() = %q, want empty", got)
	}
}

// TestLegacyClientDialect_Warmup_NotificationFailure_DoesNotFailWarmup
// verifies the notifications/initialized send is fire-and-forget: even if
// the RoundTripper reports an error for it, Warmup still succeeds, since the
// session was already established by the preceding initialize.
func TestLegacyClientDialect_Warmup_NotificationFailure_DoesNotFailWarmup(t *testing.T) {
	t.Parallel()

	rt := &fakeRoundTripper{
		responders: map[string]func([]byte, mcp.MapHeader) (*mcp.RoundTripResult, error){
			"initialize": func(_ []byte, _ mcp.MapHeader) (*mcp.RoundTripResult, error) {
				return &mcp.RoundTripResult{
					Status: http.StatusOK,
					Body:   []byte(`{"jsonrpc":"2.0","id":0,"result":{}}`),
					Header: mcp.MapHeader{"Mcp-Session-Id": "s1"},
				}, nil
			},
			"notifications/initialized": func(_ []byte, _ mcp.MapHeader) (*mcp.RoundTripResult, error) {
				return nil, errors.New("upstream dropped the connection")
			},
		},
	}

	d := mcp.NewLegacyClientDialect(mcp.V20250326, mcp.ClientInfo{})
	st := &mcp.UpstreamState{}

	if err := d.Warmup(context.Background(), rt, st); err != nil {
		t.Fatalf("Warmup() error = %v, want nil (notifications/initialized failure is advisory-only)", err)
	}
	if got := st.SessionID(); got != "s1" {
		t.Errorf("SessionID() = %q, want %q", got, "s1")
	}
}

// TestLegacyClientDialect_Warmup_InitializeFails_ReturnsError verifies that
// a genuine transport-level failure on initialize itself DOES fail Warmup.
func TestLegacyClientDialect_Warmup_InitializeFails_ReturnsError(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("connection refused")
	rt := &fakeRoundTripper{
		responders: map[string]func([]byte, mcp.MapHeader) (*mcp.RoundTripResult, error){
			"initialize": func(_ []byte, _ mcp.MapHeader) (*mcp.RoundTripResult, error) {
				return nil, wantErr
			},
		},
	}

	d := mcp.NewLegacyClientDialect(mcp.V20250326, mcp.ClientInfo{})
	err := d.Warmup(context.Background(), rt, &mcp.UpstreamState{})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Warmup() error = %v, want it to wrap %v", err, wantErr)
	}
	// notifications/initialized must never be attempted if initialize itself
	// never got far enough to matter.
	if len(rt.calls) != 1 {
		t.Errorf("calls = %v, want exactly 1 (initialize only)", rt.calls)
	}
}

// upstreamErrorSentinel is a conspicuous marker planted in an upstream's
// free-form JSON-RPC error message so tests can assert it never leaks into a
// Go error string (docs/mcp-v2.md §11.2/§11.5). If this literal ever shows up
// in a t.Error/t.Fatal failure about a *different* assertion, that is itself
// a sign the leak happened.
const upstreamErrorSentinel = "TOTALLY-CONFIDENTIAL-UPSTREAM-DIAGNOSTIC-XYZZY-42"

// TestLegacyClientDialect_Warmup_InitializeStatusAndBodyHandling is the
// regression test for the fixed Warmup bug: previously the HTTP status of
// initialize's response was discarded by the RoundTripper, so ANY response —
// including a 500, a 404, or an HTTP-200-wrapped JSON-RPC error object — was
// treated as a successful handshake. An upstream that answered initialize
// with HTTP 500 left the transport permanently wedged, because
// invalidateBinding never had a failure to react to.
//
// The subtlest row is the HTTP-200-with-a-JSON-RPC-error-body case: per
// JSON-RPC convention a legacy server reports initialize failure this way,
// answering 200 at the transport level while carrying "error" in the body —
// exactly what a naive status-only check would misclassify as success.
//
// A second, later-closed gap lives in the same table: a 2xx response whose
// body carries neither "result" nor "error" at all — empty, unparsable, or
// well-formed JSON missing both fields, including "result": null, which
// jsonRPCInitializeOutcome treats as absent — used to look identical to a
// stateless success (no session header either way), so Warmup never noticed
// the handshake had not actually produced anything. See
// jsonRPCInitializeOutcome and its doc for the exact classification.
//
// Every error case also asserts the upstream's free-form error message never
// appears in the resulting Go error (docs/mcp-v2.md §11.2/§11.5): only the
// HTTP status and the numeric JSON-RPC code may be named.
func TestLegacyClientDialect_Warmup_InitializeStatusAndBodyHandling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		body        string
		header      mcp.MapHeader
		wantErr     bool
		wantSession string
	}{
		{
			name:    "HTTP 500 fails Warmup",
			status:  http.StatusInternalServerError,
			body:    `{"jsonrpc":"2.0","id":"warmup-initialize","error":{"code":-32000,"message":"` + upstreamErrorSentinel + `"}}`,
			wantErr: true,
		},
		{
			name:    "HTTP 404 fails Warmup",
			status:  http.StatusNotFound,
			body:    `` + upstreamErrorSentinel,
			wantErr: true,
		},
		{
			name:    "HTTP 400 fails Warmup",
			status:  http.StatusBadRequest,
			body:    `{"jsonrpc":"2.0","id":"warmup-initialize","error":{"code":-32602,"message":"` + upstreamErrorSentinel + `"}}`,
			wantErr: true,
		},
		{
			name:    "HTTP 200 wrapping a JSON-RPC error body fails Warmup — a legacy server reports initialize failure exactly this way, per JSON-RPC convention",
			status:  http.StatusOK,
			body:    `{"jsonrpc":"2.0","id":"warmup-initialize","error":{"code":-32000,"message":"` + upstreamErrorSentinel + `"}}`,
			wantErr: true,
		},
		{
			name:        "HTTP 200 with a valid result and Mcp-Session-Id succeeds and records the session",
			status:      http.StatusOK,
			body:        `{"jsonrpc":"2.0","id":"warmup-initialize","result":{"protocolVersion":"2025-03-26"}}`,
			header:      mcp.MapHeader{"Mcp-Session-Id": "session-abc"},
			wantSession: "session-abc",
		},
		{
			name:   "HTTP 200 with a valid result but NO Mcp-Session-Id still succeeds — legitimate sessionless legacy servers exist and this must not be treated as failure",
			status: http.StatusOK,
			body:   `{"jsonrpc":"2.0","id":"warmup-initialize","result":{"protocolVersion":"2025-03-26"}}`,
		},
		{
			name:    "HTTP 200 with an EMPTY body fails Warmup — a 2xx status alone is not proof the handshake produced a usable result",
			status:  http.StatusOK,
			body:    ``,
			wantErr: true,
		},
		{
			name:    "HTTP 200 with an unparsable body fails Warmup",
			status:  http.StatusOK,
			body:    `not JSON at all ` + upstreamErrorSentinel,
			wantErr: true,
		},
		{
			name:    "HTTP 200 with well-formed JSON carrying neither result nor error fails Warmup",
			status:  http.StatusOK,
			body:    `{"jsonrpc":"2.0","id":"warmup-initialize","protocolVersion":"2025-03-26"}`,
			wantErr: true,
		},
		{
			name:    `HTTP 200 with "result": null fails Warmup — a null result is not a usable result`,
			status:  http.StatusOK,
			body:    `{"jsonrpc":"2.0","id":"warmup-initialize","result":null}`,
			wantErr: true,
		},
		{
			name:    "HTTP 204 No Content fails Warmup",
			status:  http.StatusNoContent,
			body:    ``,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			hdr := tc.header
			if hdr == nil {
				hdr = mcp.MapHeader{}
			}
			rt := &fakeRoundTripper{
				responders: map[string]func([]byte, mcp.MapHeader) (*mcp.RoundTripResult, error){
					"initialize": func(_ []byte, _ mcp.MapHeader) (*mcp.RoundTripResult, error) {
						return &mcp.RoundTripResult{Status: tc.status, Body: []byte(tc.body), Header: hdr}, nil
					},
				},
			}

			d := mcp.NewLegacyClientDialect(mcp.V20250326, mcp.ClientInfo{})
			st := &mcp.UpstreamState{}
			err := d.Warmup(context.Background(), rt, st)

			if tc.wantErr {
				if err == nil {
					t.Fatal("Warmup() error = nil, want an error")
				}
				if strings.Contains(err.Error(), upstreamErrorSentinel) {
					t.Errorf("Warmup() error = %q, must never embed the upstream's free-form error text", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("Warmup() error = %v, want nil", err)
			}
			if got := st.SessionID(); got != tc.wantSession {
				t.Errorf("SessionID() = %q, want %q", got, tc.wantSession)
			}
		})
	}
}

// ---- Prepare ------------------------------------------------------------------

// TestLegacyClientDialect_Prepare_PassesBodyUnchanged verifies legacy
// requests carry no per-request metadata: Prepare must return raw byte-for-
// byte identical to its input.
func TestLegacyClientDialect_Prepare_PassesBodyUnchanged(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	d := mcp.NewLegacyClientDialect(mcp.V20250326, mcp.ClientInfo{})

	prepared, _, err := d.Prepare(&mcp.CallRequest{Raw: raw}, &mcp.UpstreamState{})
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
	if string(prepared) != string(raw) {
		t.Errorf("Prepare() body = %s, want it unchanged: %s", prepared, raw)
	}
}

// TestLegacyClientDialect_Prepare_HeadersTable verifies Prepare sets
// Mcp-Session-Id exactly when st holds a session, and never injects any
// params._meta into the body, across all three legacy revisions.
//
// MCP-Protocol-Version is handled separately, by
// TestLegacyClientDialect_Prepare_ProtocolVersionHeaderByRevision: the
// header did not exist before V20250618 (docs/mcp-v2.md revision history),
// so whether Prepare sets it is itself a function of which legacy version
// this dialect negotiated — it must NOT be asserted here as a single,
// version-independent "never set" rule. Mcp-Method and Mcp-Name, by
// contrast, genuinely are never set by ANY legacy revision — those two are
// exclusively modern-era headers (dialect_2026_client.go) — so they remain
// asserted here across the board.
func TestLegacyClientDialect_Prepare_HeadersTable(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		version    mcp.Version
		session    string
		wantHeader bool
	}{
		{name: "no session known yet: no Mcp-Session-Id header at all", version: mcp.V20250326, session: "", wantHeader: false},
		{name: "session known: Mcp-Session-Id header set to it", version: mcp.V20250326, session: "abc-123", wantHeader: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			st := &mcp.UpstreamState{}
			if tc.session != "" {
				st.SetSessionID(tc.session)
			}

			d := mcp.NewLegacyClientDialect(tc.version, mcp.ClientInfo{})
			_, hdr, err := d.Prepare(&mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`)}, st)
			if err != nil {
				t.Fatalf("Prepare() error = %v, want nil", err)
			}

			got, ok := hdr["Mcp-Session-Id"]
			if ok != tc.wantHeader {
				t.Fatalf("Mcp-Session-Id present = %v, want %v (headers: %v)", ok, tc.wantHeader, hdr)
			}
			if tc.wantHeader && got != tc.session {
				t.Errorf("Mcp-Session-Id = %q, want %q", got, tc.session)
			}

			for _, modernHeader := range []string{mcp.HeaderMethod, mcp.HeaderName} {
				if _, present := hdr[modernHeader]; present {
					t.Errorf("legacy Prepare() must never set the modern header %q, got headers: %v", modernHeader, hdr)
				}
			}
		})
	}
}

// TestLegacyClientDialect_Prepare_ProtocolVersionHeaderByRevision is the
// differentiated replacement for what TestLegacyClientDialect_Prepare_HeadersTable
// used to assert as a single, version-independent rule ("legacy never sets
// MCP-Protocol-Version"): that was only ever true for V20250326, the
// revision that predates the header's existence. From V20250618 onward the
// header is a MUST on every request past initialize (docs/mcp-v2.md §4.2),
// and legacyClientDialect.Prepare sets it (FIX 2) — omitting it
// unconditionally, the prior behavior, broke both V20250618 and V20251125
// upstreams that validate it against the body (docs/mcp-v2.md §4.5).
func TestLegacyClientDialect_Prepare_ProtocolVersionHeaderByRevision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		version    mcp.Version
		wantHeader bool
	}{
		{
			name:       "V20250326 predates the header entirely: never set",
			version:    mcp.V20250326,
			wantHeader: false,
		},
		{
			name:       "V20250618 introduced the header: set to exactly this version",
			version:    mcp.V20250618,
			wantHeader: true,
		},
		{
			name:       "V20251125 still requires the header: set to exactly this version",
			version:    mcp.V20251125,
			wantHeader: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := mcp.NewLegacyClientDialect(tc.version, mcp.ClientInfo{})
			_, hdr, err := d.Prepare(&mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo"}}`)}, &mcp.UpstreamState{})
			if err != nil {
				t.Fatalf("Prepare() error = %v, want nil", err)
			}

			got, ok := hdr[mcp.HeaderProtocolVersion]
			if ok != tc.wantHeader {
				t.Fatalf("%s present = %v, want %v (headers: %v)", mcp.HeaderProtocolVersion, ok, tc.wantHeader, hdr)
			}
			if tc.wantHeader && got != string(tc.version) {
				t.Errorf("%s = %q, want %q", mcp.HeaderProtocolVersion, got, string(tc.version))
			}
		})
	}
}

// TestLegacyClientDialect_Prepare_NeverInjectsMeta verifies no params._meta
// key is ever added to a legacy request body, even when the incoming raw
// body already has a params object (proving Prepare truly leaves it alone
// rather than round-tripping it through a decode/re-encode step that could
// silently add one).
func TestLegacyClientDialect_Prepare_NeverInjectsMeta(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"x":1}}}`)
	d := mcp.NewLegacyClientDialect(mcp.V20250326, mcp.ClientInfo{})

	prepared, _, err := d.Prepare(&mcp.CallRequest{Raw: raw}, &mcp.UpstreamState{})
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}
	if strings.Contains(string(prepared), "_meta") {
		t.Errorf("Prepare() body = %s, must never contain _meta", prepared)
	}
}

// ---- HeaderParams: deliberately ignored by every legacy era ---------------

// TestLegacyClientDialect_Prepare_IgnoresHeaderParams is the era-boundary
// test for CallRequest.HeaderParams' own documented contract: "which era
// understands Mcp-Param-* at all is a property of the resolved
// ClientDialect" — CallMCPTool sets HeaderParams unconditionally, regardless
// of era, and dialect2026Client.Prepare is the ONLY implementation that
// renders it. This drives the exact same CallRequest — carrying a resolvable
// binding — through the legacy dialect instead, and asserts two things
// together: no Mcp-Param-* header appears in hdr at all, AND the prepared
// body is byte-identical to req.Raw. Rot the moment someone hoists the
// x-mcp-header mirroring logic up into a shared helper both dialects call
// (as the doc on legacyClientDialect.Prepare warns against) — the body
// check alone would not catch a change that only added headers, and the
// header check alone would not catch a change that also started mutating
// the body.
func TestLegacyClientDialect_Prepare_IgnoresHeaderParams(t *testing.T) {
	t.Parallel()

	raw := []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"lookup","arguments":{"region":"us-west1"}}}`)
	param := mcp.HeaderParam{Name: "Region", Path: []string{"region"}, Kind: mcp.HeaderParamKindString}

	d := mcp.NewLegacyClientDialect(mcp.V20250326, mcp.ClientInfo{})
	prepared, hdr, err := d.Prepare(&mcp.CallRequest{Raw: raw, HeaderParams: []mcp.HeaderParam{param}}, &mcp.UpstreamState{})
	if err != nil {
		t.Fatalf("Prepare() error = %v, want nil", err)
	}

	if string(prepared) != string(raw) {
		t.Errorf("Prepare() body = %s, want byte-identical to req.Raw: %s", prepared, raw)
	}
	for k := range hdr {
		if strings.HasPrefix(k, mcp.HeaderParamPrefix) {
			t.Errorf("hdr contains %q = %q, want no Mcp-Param-* header from any legacy dialect", k, hdr[k])
		}
	}
}

// ---- Parse ----------------------------------------------------------------

func TestLegacyClientDialect_Parse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		raw         string
		wantErrCode int // 0 means no error expected
	}{
		{
			name: "well-formed success response passes through with its payload",
			raw:  `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`,
		},
		{
			name: "well-formed error response is NOT intercepted by Parse — it is ordinary content",
			raw:  `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`,
		},
		{
			name:        "malformed JSON is a parse error",
			raw:         `{not valid json`,
			wantErrCode: mcp.CodeParseError,
		},
	}

	d := mcp.NewLegacyClientDialect(mcp.V20250326, mcp.ClientInfo{})

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			result, err := d.Parse([]byte(tc.raw))
			if tc.wantErrCode != 0 {
				if err == nil {
					t.Fatalf("Parse() error = nil, want code %d", tc.wantErrCode)
				}
				if err.Code != tc.wantErrCode {
					t.Errorf("Parse() error code = %d, want %d", err.Code, tc.wantErrCode)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse() unexpected error: %+v", err)
			}
			if result == nil {
				t.Fatal("Parse() result = nil, want non-nil")
			}
		})
	}
}
