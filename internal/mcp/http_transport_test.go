package mcp_test

import (
	"bytes"
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

// testStreamIdleTimeout is the streamIdleTimeout passed to every
// mcp.NewHTTPTransport constructed across this package's tests that do not
// specifically exercise Forward's idle-timeout behavior (see
// forward_streaming_test.go for those): it only needs to be nonzero and
// generous enough to never fire during a fast, local httptest exchange.
const testStreamIdleTimeout = 5 * time.Second

// newLegacyTransport builds an HTTPTransport pinned to V20250326 (legacy
// era), so every test using it exercises the initialize/notifications
// handshake deterministically instead of racing HTTPTransport's own
// auto-probe (which is covered exhaustively in probe_test.go). A 5-second
// timeout with private addresses allowed (test servers run on loopback).
func newLegacyTransport(endpoint, authType, authHeader, authToken string) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, authType, authHeader, authToken, 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20250326, testStreamIdleTimeout)
}

// newModernTransport builds an HTTPTransport pinned to V20260728 (modern
// era), whose Warmup performs no I/O — so every Call it makes is exactly one
// HTTP request, which is what the plain transport-plumbing tests below (auth
// headers, timeouts, HTTP error propagation, body limits) want to isolate.
func newModernTransport(endpoint, authType, authHeader, authToken string) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, authType, authHeader, authToken, 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)
}

// ---- Call: transport plumbing (pinned modern era — one request per Call) ---

func TestHTTPTransport_Call_Success(t *testing.T) {
	t.Parallel()

	want := `{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, want)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	got, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}
	if string(got.Body) != want {
		t.Errorf("Call() = %q, want %q", got.Body, want)
	}
}

func TestHTTPTransport_Call_BearerAuth(t *testing.T) {
	t.Parallel()

	const token = "super-secret-token"
	var gotHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "bearer", "", token)
	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}

	want := "Bearer " + token
	if gotHeader != want {
		t.Errorf("Authorization header = %q, want %q", gotHeader, want)
	}
}

func TestHTTPTransport_Call_HeaderAuth(t *testing.T) {
	t.Parallel()

	const headerName = "X-API-Key"
	const token = "my-api-key-value"
	var gotHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get(headerName)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "header", headerName, token)
	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}

	if gotHeader != token {
		t.Errorf("%s header = %q, want %q", headerName, gotHeader, token)
	}
}

func TestHTTPTransport_Call_NoAuth(t *testing.T) {
	t.Parallel()

	var gotAuthHeader, gotAPIKeyHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		gotAPIKeyHeader = r.Header.Get("X-API-Key")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}

	if gotAuthHeader != "" {
		t.Errorf("Authorization header = %q, want empty", gotAuthHeader)
	}
	if gotAPIKeyHeader != "" {
		t.Errorf("X-API-Key header = %q, want empty", gotAPIKeyHeader)
	}
}

func TestHTTPTransport_Call_Timeout(t *testing.T) {
	t.Parallel()

	// srvClosed is closed when the server is being shut down so the handler
	// can unblock promptly and let httptest.Server.Close() drain cleanly.
	srvClosed := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// Block until either the server is closing or a generous backstop
		// timer fires. 200 ms is well past the 50 ms transport timeout.
		select {
		case <-srvClosed:
		case <-time.After(200 * time.Millisecond):
		}
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	t.Cleanup(func() {
		close(srvClosed)
		srv.Close()
	})

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 50*time.Millisecond, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)
	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err == nil {
		t.Fatal("Call() error = nil, want timeout error")
	}
	if !strings.Contains(err.Error(), "transport:") {
		t.Errorf("error = %q, want it to contain %q", err.Error(), "transport:")
	}
}

func TestHTTPTransport_Call_HTTPError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err == nil {
		t.Fatal("Call() error = nil, want error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("error = %q, want it to mention HTTP 500", err.Error())
	}
}

func TestHTTPTransport_Call_Notification_202(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	resp, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","method":"notifications/ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil for 202", err)
	}
	if resp.Body != nil {
		t.Errorf("Call() response = %q, want nil for 202 Accepted", resp.Body)
	}
}

// TestHTTPTransport_Call_BodyLimit uses a legacy-pinned transport: the
// modern dialect's doCall feeds every non-empty 200 body through Parse (to
// detect MRTR's input_required), which would itself fail on the garbage,
// non-JSON payload this test serves. The legacy dialect never parses a
// response body at all — Call is purely a byte-limited pass-through for it —
// which is the actual property under test here (truncation, not
// interpretation).
//
// The oversized garbage payload is served only for the real "ping" request
// under test. "initialize" gets a small, well-formed JSON-RPC result of its
// own — a real legacy upstream answers the handshake sanely even if some
// OTHER response it later sends is oversized or malformed — since
// legacyClientDialect.Warmup now rejects a 2xx initialize response that is
// not a well-formed JSON-RPC result (see jsonRPCInitializeOutcome), and this
// test's actual target is the body-limit truncation on Call, not the
// handshake.
func TestHTTPTransport_Call_BodyLimit(t *testing.T) {
	t.Parallel()

	// Serve exactly 10 MiB + 1 byte on the real request. The transport must
	// not crash — it should silently truncate to the limit and still return a
	// non-nil body.
	const limit = 10 << 20 // 10 MiB
	oversized := bytes.Repeat([]byte("x"), limit+1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "initialize":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(oversized)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")
	got, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	// Must not error — body is merely truncated.
	if err != nil {
		t.Fatalf("Call() error = %v, want nil (body should be truncated not error)", err)
	}
	if len(got.Body) > limit {
		t.Errorf("body len = %d, want at most %d (10 MiB limit)", len(got.Body), limit)
	}
}

// TestHTTPTransport_Call_Modern_MalformedBody_IsAParseError verifies that,
// unlike the legacy era (TestHTTPTransport_Call_BodyLimit), a modern-era 200
// response whose body is not valid JSON-RPC is surfaced as an error: doCall
// feeds every non-empty modern-era body through Parse to detect MRTR's
// input_required, and a body Parse cannot even understand must not be
// forwarded to the caller as if it were a real result.
func TestHTTPTransport_Call_Modern_MalformedBody_IsAParseError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `this is not JSON at all`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err == nil {
		t.Fatal("Call() error = nil, want an error for an unparsable modern-era response body")
	}
	if !strings.Contains(err.Error(), "parse error") {
		t.Errorf("error = %q, want it to mention the parse failure", err.Error())
	}
}

// TestHTTPTransport_Call_Legacy_InitialWarmupFails verifies that a failure
// in the very FIRST Warmup (not a reinit after expiry — see
// TestCall_Legacy_SessionExpired_ReinitFails_ErrorsWithoutLooping) is
// reported as a "warmup:"-prefixed error and the real request is never
// attempted at all.
func TestHTTPTransport_Call_Legacy_InitialWarmupFails(t *testing.T) {
	t.Parallel()

	var pingCount int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		switch rpc.Method {
		case "initialize":
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("test server ResponseWriter does not support hijacking")
				return
			}
			conn, _, err := hj.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			conn.Close()
		case "ping":
			mu.Lock()
			pingCount++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")
	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err == nil {
		t.Fatal("Call() error = nil, want an error when the initial Warmup fails")
	}
	if !strings.Contains(err.Error(), "warmup:") {
		t.Errorf("error = %q, want it prefixed with the warmup step", err.Error())
	}

	mu.Lock()
	defer mu.Unlock()
	if pingCount != 0 {
		t.Errorf("ping count = %d, want 0 — the real request must never be attempted when Warmup itself fails", pingCount)
	}
}

// TestHTTPTransport_Call_Legacy_WarmupFailure_ScopeNotPermanentlyWarm
// verifies ensureWarm's failure contract: a scope whose very first Warmup
// fails must NOT be left marked warm (ss.warmed stays false), because
// ensureWarm only sets it after Warmup returns nil. If a failed Warmup were
// ever mistaken for a completed one, every subsequent Call in that
// SessionScope would skip Warmup entirely and go straight to a real request
// with no session ever established — even after the upstream recovered. This
// test drives that recovery end to end: the first Call's initialize answers
// with a 2xx-but-body-less response (the newly closed "silent" failure case,
// see jsonRPCInitializeOutcome), then the SECOND Call against the same scope,
// once the upstream starts answering initialize properly, must both retry
// Warmup and succeed.
func TestHTTPTransport_Call_Legacy_WarmupFailure_ScopeNotPermanentlyWarm(t *testing.T) {
	t.Parallel()

	var initializeCount int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		switch rpc.Method {
		case "initialize":
			mu.Lock()
			initializeCount++
			n := initializeCount
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if n == 1 {
				// The silent failure case: 2xx status, no body at all.
				return
			}
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26"}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "ping":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")

	// Call A: the upstream's initialize answers 2xx with an empty body — a
	// silently broken handshake, not a loud transport error.
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err == nil {
		t.Fatal("Call A error = nil, want an error for a silently failed Warmup")
	}

	// Call B: the same SessionScope, against an upstream that now answers
	// initialize properly. If the scope had been wrongly left "warmed" by
	// Call A's failure, this would skip Warmup and never establish a session
	// — succeeding only by accident, or failing outright. It must instead
	// retry Warmup and succeed.
	got, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call B error = %v, want nil — a failed Warmup must not permanently mark the scope warm", err)
	}
	if !strings.Contains(string(got.Body), `"status":"ok"`) {
		t.Errorf("Call B = %q, want the successful response", got.Body)
	}

	mu.Lock()
	defer mu.Unlock()
	if initializeCount != 2 {
		t.Errorf("initialize count = %d, want 2 (Call A's failed attempt, Call B's successful retry)", initializeCount)
	}
}

// TestHTTPTransport_Close_ReleasesIdleConnections verifies Close is safe to
// call and does not itself error, regardless of whether any request was ever
// made.
func TestHTTPTransport_Close_ReleasesIdleConnections(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}
	if err := tr.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

// ---- ListTools --------------------------------------------------------------

func TestHTTPTransport_ListTools_Success(t *testing.T) {
	t.Parallel()

	respBody := `{
		"jsonrpc": "2.0",
		"id": 1,
		"result": {
			"tools": [
				{"name": "search",   "description": "Search the web",   "inputSchema": {"type": "object"}},
				{"name": "weather",  "description": "Get the weather",  "inputSchema": {"type": "object"}},
				{"name": "calendar", "description": "Manage calendar",  "inputSchema": {"type": "object"}}
			]
		}
	}`

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, respBody)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
	tools := listing.Tools
	if len(tools) != 3 {
		t.Fatalf("ListTools() count = %d, want 3", len(tools))
	}

	wantNames := []string{"search", "weather", "calendar"}
	for i, tool := range tools {
		if tool.Name != wantNames[i] {
			t.Errorf("tools[%d].Name = %q, want %q", i, tool.Name, wantNames[i])
		}
	}

	// Verify the outgoing request was a valid JSON-RPC tools/list.
	var req struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("decode outgoing request: %v", err)
	}
	if req.Method != "tools/list" {
		t.Errorf("outgoing method = %q, want %q", req.Method, "tools/list")
	}
}

func TestHTTPTransport_ListTools_RPCError(t *testing.T) {
	t.Parallel()

	respBody := `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, respBody)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want error for JSON-RPC error response")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil on error", listing)
	}
	if !strings.Contains(err.Error(), "code -32601") {
		t.Errorf("error = %q, want it to contain the RPC error code", err.Error())
	}
}

// TestListTools_Modern_ExactlyOneRequest verifies the stateless-core promise
// (docs/mcp-v2.md §2, §11.2): under the modern era, ListTools sends exactly
// one request — tools/list — and never initialize or
// notifications/initialized, which the legacy era required on every fresh
// connection.
func TestListTools_Modern_ExactlyOneRequest(t *testing.T) {
	t.Parallel()

	var methodsSeen []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		mu.Lock()
		methodsSeen = append(methodsSeen, rpc.Method)
		mu.Unlock()

		if rpc.Method != "tools/list" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"echo","description":"","inputSchema":{"type":"object"}}]}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
	tools := listing.Tools
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools() = %+v, want [{echo ...}]", tools)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(methodsSeen) != 1 || methodsSeen[0] != "tools/list" {
		t.Errorf("methods seen = %v, want exactly [\"tools/list\"] (no initialize, no notifications/initialized)", methodsSeen)
	}
}

// ---- Call session ID handling (legacy era; modern is covered separately) ---

// TestCall_Legacy_SessionEstablishedByWarmupAndForwarded is the internal
// counterpart of the pre-Phase-2 tests that passed a session ID explicitly
// into Call: the session is no longer a caller-supplied parameter, but the
// underlying property — a session minted by initialize is remembered and
// sent back on every subsequent request — still holds, now entirely inside
// HTTPTransport/UpstreamState. It also pins down that Warmup runs exactly
// once per HTTPTransport: a second Call must reuse the same session without
// initializing again.
func TestCall_Legacy_SessionEstablishedByWarmupAndForwarded(t *testing.T) {
	t.Parallel()

	const assignedSession = "server-assigned-session-xyz"

	var initializeCount int
	var sessionsSeenOnPing []string
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		w.Header().Set("Content-Type", "application/json")

		switch rpc.Method {
		case "initialize":
			mu.Lock()
			initializeCount++
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", assignedSession)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "ping":
			mu.Lock()
			sessionsSeenOnPing = append(sessionsSeenOnPing, r.Header.Get("Mcp-Session-Id"))
			mu.Unlock()
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")

	for i := 0; i < 2; i++ {
		if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
			t.Fatalf("Call() #%d error = %v, want nil", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if initializeCount != 1 {
		t.Errorf("initialize count = %d, want 1 (Warmup must run exactly once per HTTPTransport)", initializeCount)
	}
	if len(sessionsSeenOnPing) != 2 {
		t.Fatalf("ping count = %d, want 2", len(sessionsSeenOnPing))
	}
	for i, got := range sessionsSeenOnPing {
		if got != assignedSession {
			t.Errorf("ping #%d Mcp-Session-Id = %q, want %q (session from initialize forwarded internally)", i, got, assignedSession)
		}
	}
}

// TestCall_Legacy_NoSessionID_NoHeader verifies that a legacy server which
// answers initialize with no Mcp-Session-Id at all is tolerated: subsequent
// requests simply carry no session header, rather than the transport
// inventing one or failing.
func TestCall_Legacy_NoSessionID_NoHeader(t *testing.T) {
	t.Parallel()

	var gotHeader string
	var sawHeaderKey bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "initialize":
			// Deliberately no Mcp-Session-Id header.
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			_, sawHeaderKey = r.Header["Mcp-Session-Id"]
			gotHeader = r.Header.Get("Mcp-Session-Id")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")
	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}
	if sawHeaderKey {
		t.Errorf("Mcp-Session-Id header present = %q, want the header entirely absent when no session was ever established", gotHeader)
	}
}

// TestCall_Legacy_SessionExpired_ReinitsExactlyOnceAndRetries verifies
// docs/mcp-v2.md's session-expiry contract (reinitAndRetry in
// http_transport.go): an HTTP 404 on a legacy request causes the transport
// to transparently re-initialize exactly once and retry the original
// request exactly once more — no loop, even if the retry also fails.
func TestCall_Legacy_SessionExpired_ReinitsExactlyOnceAndRetries(t *testing.T) {
	t.Parallel()

	const staleSession = "stale-session-1"
	const freshSession = "fresh-session-2"

	var initializeCount int
	var pingAttempts []string // session ID seen on each "ping" attempt, in order
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		w.Header().Set("Content-Type", "application/json")

		switch rpc.Method {
		case "initialize":
			mu.Lock()
			initializeCount++
			n := initializeCount
			mu.Unlock()
			session := staleSession
			if n > 1 {
				session = freshSession
			}
			w.Header().Set("Mcp-Session-Id", session)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "ping":
			session := r.Header.Get("Mcp-Session-Id")
			mu.Lock()
			pingAttempts = append(pingAttempts, session)
			mu.Unlock()
			if session == staleSession {
				http.NotFound(w, nil) // simulate an expired session
				return
			}
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")
	got, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil (transparent reinit+retry should have succeeded)", err)
	}
	if !strings.Contains(string(got.Body), `"status":"ok"`) {
		t.Errorf("Call() = %q, want the successful retry's body", got.Body)
	}

	mu.Lock()
	defer mu.Unlock()
	if initializeCount != 2 {
		t.Errorf("initialize count = %d, want 2 (one Warmup, one reinit after expiry)", initializeCount)
	}
	wantAttempts := []string{staleSession, freshSession}
	if len(pingAttempts) != len(wantAttempts) {
		t.Fatalf("ping attempts = %v, want exactly %v (no retry loop)", pingAttempts, wantAttempts)
	}
	for i, want := range wantAttempts {
		if pingAttempts[i] != want {
			t.Errorf("ping attempt #%d session = %q, want %q", i, pingAttempts[i], want)
		}
	}
}

// TestCall_Legacy_SessionExpired_ReinitFails_ErrorsWithoutLooping verifies
// that when the re-initialize itself cannot establish a usable session (the
// upstream now refuses initialize outright), Call surfaces an error instead
// of looping, and does not retry the original request a second time.
func TestCall_Legacy_SessionExpired_ReinitFails_ErrorsWithoutLooping(t *testing.T) {
	t.Parallel()

	var initializeCount int
	var pingCount int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		switch rpc.Method {
		case "initialize":
			mu.Lock()
			initializeCount++
			n := initializeCount
			mu.Unlock()
			if n > 1 {
				// The reinit attempt itself fails at the TRANSPORT level (not
				// merely a non-2xx status, which legacyClientDialect.Warmup
				// tolerates — see TestCall_Legacy_NoSessionID_NoHeader):
				// hijack and close the connection out from under the client
				// so client.Do itself returns a Go error, forcing warmErr
				// non-nil in reinitAndRetry.
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Errorf("test server ResponseWriter does not support hijacking")
					return
				}
				conn, _, err := hj.Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				conn.Close()
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "stale-session")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "ping":
			mu.Lock()
			pingCount++
			mu.Unlock()
			http.NotFound(w, nil) // always looks expired
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")
	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err == nil {
		t.Fatal("Call() error = nil, want an error when reinit itself fails")
	}
	if !strings.Contains(err.Error(), "re-initialize after session expiry") {
		t.Errorf("error = %q, want it to name the failed reinit step", err.Error())
	}

	mu.Lock()
	defer mu.Unlock()
	if initializeCount != 2 {
		t.Errorf("initialize count = %d, want 2 (Warmup, then exactly one failed reinit attempt)", initializeCount)
	}
	if pingCount != 1 {
		t.Errorf("ping count = %d, want 1 — reinitAndRetry must not attempt doCall again once warmErr != nil, and Call must not loop", pingCount)
	}
}

// TestCall_Legacy_SessionExpired_TwiceInARow_DoesNotLeakSentinel covers the
// OTHER way reinitAndRetry can fail: not Warmup itself erroring (see
// TestCall_Legacy_SessionExpired_ReinitFails_ErrorsWithoutLooping), but
// Warmup succeeding — a fresh session really is established — and the
// retried request STILL comes back as an expired session. doCall returns the
// bare ErrSessionExpired sentinel in that case (see its doc), and Call's own
// doc is explicit that this must never reach Call's caller: "errors.Is(err,
// ErrSessionExpired) can no longer match outside this function, while still
// leaving a diagnosable message." Both halves of that contract are asserted
// here — the sentinel itself must not be identifiable via errors.Is, but the
// resulting error must still name the failed step.
func TestCall_Legacy_SessionExpired_TwiceInARow_DoesNotLeakSentinel(t *testing.T) {
	t.Parallel()

	var initializeCount int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		switch rpc.Method {
		case "initialize":
			mu.Lock()
			initializeCount++
			n := initializeCount
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			// A genuinely fresh session every time — reinitAndRetry's own
			// Warmup call succeeds cleanly, it is the immediately following
			// retry that will still be told the session is expired.
			w.Header().Set("Mcp-Session-Id", fmt.Sprintf("session-%d", n))
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "ping":
			// Every ping, regardless of which session it carries, looks
			// expired to this upstream — simulating an upstream so broken
			// that even a freshly re-established session is immediately
			// rejected.
			http.NotFound(w, nil)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")
	_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err == nil {
		t.Fatal("Call() error = nil, want an error when even the freshly re-established session is rejected")
	}
	if errors.Is(err, mcp.ErrSessionExpired) {
		t.Errorf("errors.Is(err, mcp.ErrSessionExpired) = true, want false — ErrSessionExpired must never escape Call to its own caller")
	}
	if !strings.Contains(err.Error(), "session could not be re-established after retry") {
		t.Errorf("error = %q, want it to name the failed retry step so the failure stays diagnosable", err.Error())
	}

	mu.Lock()
	defer mu.Unlock()
	if initializeCount != 2 {
		t.Errorf("initialize count = %d, want 2 (the original Warmup, then exactly one reinit — no looping)", initializeCount)
	}
}

// TestCall_Legacy_RecoversAfterReinitFailure_ViaInvalidateBinding is the
// self-healing counterpart to the test above: once reinitAndRetry's own
// Warmup fails outright, invalidateBinding discards the cached era binding
// (see its own doc), so it must not merely fail cleanly — the NEXT,
// independent Call must re-resolve the binding from scratch and, if the
// upstream has since recovered, succeed. Without invalidateBinding a
// transport would stay wedged against a binding that already proved broken
// for the rest of the process lifetime, even after the upstream came back.
func TestCall_Legacy_RecoversAfterReinitFailure_ViaInvalidateBinding(t *testing.T) {
	t.Parallel()

	var initializeCount, pingCount int
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		switch rpc.Method {
		case "initialize":
			mu.Lock()
			initializeCount++
			n := initializeCount
			mu.Unlock()
			if n == 2 {
				// Simulate a transient upstream outage during the reinit
				// attempt: hijack and close the connection so client.Do
				// itself returns a transport error, forcing Warmup to fail —
				// not merely a non-2xx status.
				hj, ok := w.(http.Hijacker)
				if !ok {
					t.Errorf("test server ResponseWriter does not support hijacking")
					return
				}
				conn, _, err := hj.Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				conn.Close()
				return
			}
			session := "session-1"
			if n == 3 {
				session = "session-3" // the upstream has recovered
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", session)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "ping":
			mu.Lock()
			pingCount++
			n := pingCount
			mu.Unlock()
			if n == 2 {
				http.NotFound(w, nil) // simulate the session expiring
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")

	// Call A: establishes the binding and warms the session normally.
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("Call A error = %v, want nil", err)
	}

	// Call B: the upstream reports the session expired, and the reinit
	// itself fails outright. Call must surface an error, not loop.
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err == nil {
		t.Fatal("Call B error = nil, want an error when reinit fails")
	}

	// Call C: the upstream has recovered. If invalidateBinding did its job,
	// this Call re-resolves the binding and re-warms from scratch, and
	// succeeds — rather than staying permanently wedged against the binding
	// that just proved broken.
	got, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call C error = %v, want nil — the transport must recover after invalidateBinding, not stay wedged", err)
	}
	if !strings.Contains(string(got.Body), `"status":"ok"`) {
		t.Errorf("Call C = %q, want the successful response", got.Body)
	}

	mu.Lock()
	defer mu.Unlock()
	if initializeCount != 3 {
		t.Errorf("initialize count = %d, want 3 (Call A warmup, Call B failed reinit, Call C fresh warmup after invalidateBinding)", initializeCount)
	}
}

// TestHTTPTransport_InvalidateBinding_ResetsPostURL verifies invalidateBinding's
// documented state-reset contract: after a modern-era resolution sets
// postURL to the base endpoint, InvalidateBinding must clear it back to
// empty — leaving a stale POST target behind could misroute the next
// resolveBinding's requests if that next resolution lands on a different
// era.
func TestHTTPTransport_InvalidateBinding_ResetsPostURL(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}
	if got := tr.PostURL(); got != srv.URL {
		t.Fatalf("PostURL() = %q, want %q after a modern-era resolution", got, srv.URL)
	}

	tr.InvalidateBinding()

	if got := tr.PostURL(); got != "" {
		t.Errorf("PostURL() = %q after InvalidateBinding(), want empty — a stale POST target must not survive binding invalidation", got)
	}
}

// TestHTTPTransport_PostURL_ReresolvesAfterInvalidateBinding is the
// round-trip counterpart of TestHTTPTransport_InvalidateBinding_ResetsPostURL:
// it is not enough for InvalidateBinding to clear PostURL back to empty — the
// NEXT Call must re-probe the era from scratch and land on a freshly resolved
// *eraBinding whose postURL is correct again, proving postURL genuinely lives
// on the binding (set once, at construction, inside resolveBinding — see
// eraBinding's doc) rather than on some transport-level field that
// InvalidateBinding merely forgot to keep in sync.
func TestHTTPTransport_PostURL_ReresolvesAfterInvalidateBinding(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")

	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("Call() #1 error = %v, want nil", err)
	}
	if got := tr.PostURL(); got != srv.URL {
		t.Fatalf("PostURL() after Call #1 = %q, want %q", got, srv.URL)
	}

	tr.InvalidateBinding()
	if got := tr.PostURL(); got != "" {
		t.Fatalf("PostURL() after InvalidateBinding() = %q, want empty", got)
	}

	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("Call() #2 error = %v, want nil", err)
	}
	if got := tr.PostURL(); got != srv.URL {
		t.Errorf("PostURL() after re-resolution = %q, want %q — the next Call must re-probe and land on a correct POST target again", got, srv.URL)
	}
}

// TestHTTPTransport_Call_ConcurrentWithInvalidateBinding_NoRace is a
// regression test for the fixed postURL data race (docs/mcp-v2.md): postURL
// used to live directly on *HTTPTransport, written by invalidateBinding under
// eraMu but read by rawPost with NO lock at all — a classic unsynchronized
// read/write race the Go race detector flags immediately under any
// concurrent load. It is fixed by moving postURL onto *eraBinding, set
// exactly once at construction inside resolveBinding and never mutated
// afterward (see eraBinding's doc): every Call captures one *eraBinding
// locally at the top and reads postURL only from that local, immutable
// value for its own entire duration, so a concurrent invalidateBinding —
// which only ever replaces HTTPTransport.binding under eraMu for the NEXT
// resolveBinding call — can no longer race with anything an in-flight Call
// is doing.
//
// Reproducing the OLD bug here does not require re-implementing the old
// field layout: this test simply drives many concurrent Calls (which read
// whatever the transport considers its current POST target) against a
// transport whose binding is repeatedly invalidated from another goroutine
// at the same time. Under the old design that read happened via
// HTTPTransport.postURL directly in rawPost/postTarget with no mutex, while
// invalidateBinding's write held eraMu — the two are unsynchronized by
// definition, so `go test -race` would have flagged it on essentially the
// first iteration. Under the current design the same access pattern is race
// -free, because there is no longer a mutable shared field for the two
// goroutines to race on at all — which is exactly the property this test
// pins down.
func TestHTTPTransport_Call_ConcurrentWithInvalidateBinding_NoRace(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")

	const callers = 16
	const invalidators = 4
	const iterations = 200

	var wg sync.WaitGroup
	wg.Add(callers + invalidators)

	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// A transport-level or Warmup error is an acceptable outcome
				// here (the binding might be invalidated mid-flight) — the
				// property under test is the absence of a data race, not
				// that every single Call succeeds.
				_, _ = tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
			}
		}()
	}
	for i := 0; i < invalidators; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				tr.InvalidateBinding()
			}
		}()
	}

	wg.Wait()

	// Sanity check: the transport must still be fully usable afterward.
	got, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() after the concurrent storm error = %v, want nil", err)
	}
	if len(got.Body) == 0 {
		t.Error("Call() after the concurrent storm returned an empty body, want the server's response")
	}
}

// ---- Modern era: no session concept exists at all ---------------------------

// TestCall_Modern_NoSessionHeaderNoInitialize verifies the modern era's
// defining property (docs/mcp-v2.md §2, §3.2): across repeated Calls to the
// same upstream, no Mcp-Session-Id header is EVER sent and "initialize" is
// NEVER called — there is no session to establish or carry.
func TestCall_Modern_NoSessionHeaderNoInitialize(t *testing.T) {
	t.Parallel()

	var methodsSeen []string
	var sawSessionHeader bool
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Mcp-Session-Id"]; ok {
			mu.Lock()
			sawSessionHeader = true
			mu.Unlock()
		}

		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		mu.Lock()
		methodsSeen = append(methodsSeen, rpc.Method)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	for i := 0; i < 3; i++ {
		if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
			t.Fatalf("Call() #%d error = %v, want nil", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if sawSessionHeader {
		t.Error("Mcp-Session-Id header was sent, want it never to appear under the modern era")
	}
	for _, m := range methodsSeen {
		if m == "initialize" {
			t.Errorf("methods seen = %v, want no initialize under the modern era", methodsSeen)
			break
		}
	}
	if len(methodsSeen) != 3 {
		t.Errorf("methods seen = %v, want exactly 3 pings (one per Call, no handshake overhead)", methodsSeen)
	}
}

// ---- ListTools legacy handshake (kept for parity with the modern-era test)--

// TestListTools_Legacy_AutoInitialize verifies that ListTools performs an
// initialize handshake before tools/list under the legacy era, and forwards
// the session initialize returns.
func TestListTools_Legacy_AutoInitialize(t *testing.T) {
	t.Parallel()

	var callOrder []string
	var mu sync.Mutex

	toolsResp := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"mytool","description":"desc","inputSchema":{"type":"object"}}]}}`
	initResp := `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "test-session-id")

		mu.Lock()
		callOrder = append(callOrder, rpc.Method)
		mu.Unlock()

		switch rpc.Method {
		case "initialize":
			fmt.Fprint(w, initResp)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			fmt.Fprint(w, toolsResp)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
	tools := listing.Tools
	if len(tools) != 1 {
		t.Fatalf("ListTools() count = %d, want 1", len(tools))
	}
	if tools[0].Name != "mytool" {
		t.Errorf("tools[0].Name = %q, want %q", tools[0].Name, "mytool")
	}

	mu.Lock()
	defer mu.Unlock()
	// initialize must come before tools/list.
	if len(callOrder) < 2 {
		t.Fatalf("expected at least 2 calls (initialize, tools/list), got %v", callOrder)
	}
	if callOrder[0] != "initialize" {
		t.Errorf("first call = %q, want %q", callOrder[0], "initialize")
	}
	last := callOrder[len(callOrder)-1]
	if last != "tools/list" {
		t.Errorf("last call = %q, want %q", last, "tools/list")
	}
}

// ---- probeEra + ListTools integration (SSE fallback) ------------------------
//
// The exhaustive era-classification matrix lives in probe_test.go
// (TestProbeEra_ClassificationTable), exercised directly against
// HTTPTransport.ProbeEra. The tests below instead prove that the public,
// end-user-facing surface (ListTools, on an unpinned/auto transport) really
// does route through that classification — in particular that a server
// which never speaks Streamable HTTP at all (404/405 on every probe) comes
// back as ErrSSENotSupported, not a generic error.

func TestProbeEra_ThroughListTools_StreamableHTTP(t *testing.T) {
	t.Parallel()

	initResp := `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`
	toolsResp := `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"ping","description":"Ping tool","inputSchema":{"type":"object"}}]}}`

	var getCount int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			getCount++
		}

		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "initialize":
			fmt.Fprint(w, initResp)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			fmt.Fprint(w, toolsResp)
		default:
			// Includes "server/discover" — this fake upstream is a plain
			// legacy server that has never heard of it.
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newProbeTransport(srv.URL)
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
	tools := listing.Tools
	if len(tools) != 1 || tools[0].Name != "ping" {
		t.Errorf("ListTools() = %v, want [{ping ...}]", tools)
	}
	// No GET request should have been made — Streamable HTTP uses POST only.
	if getCount != 0 {
		t.Errorf("GET request count = %d, want 0 (Streamable HTTP should not do SSE discovery)", getCount)
	}
}

func TestProbeEra_ThroughListTools_SSEFallback_404_ReturnsError(t *testing.T) {
	t.Parallel()

	// Server returns 404 on every POST — indicates the deprecated SSE
	// transport (or nothing at all) rather than Streamable HTTP.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	tr := newProbeTransport(srv.URL)
	_, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want ErrSSENotSupported")
	}
	if !strings.Contains(err.Error(), "SSE transport") {
		t.Errorf("ListTools() error = %q, want it to mention SSE transport", err.Error())
	}
}

func TestProbeEra_ThroughListTools_SSEFallback_405_ReturnsError(t *testing.T) {
	t.Parallel()

	// Server returns 405 on every POST — also indicates SSE transport.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	t.Cleanup(srv.Close)

	tr := newProbeTransport(srv.URL)
	_, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want ErrSSENotSupported")
	}
	if !strings.Contains(err.Error(), "SSE transport") {
		t.Errorf("ListTools() error = %q, want it to mention SSE transport", err.Error())
	}
}

func TestProbeEra_ThroughListTools_WithAuth_ReturnsNotSupported(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransport(srv.URL, "bearer", "", "secret-token", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, "", testStreamIdleTimeout)
	_, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want ErrSSENotSupported")
	}
	if !strings.Contains(err.Error(), "SSE transport") {
		t.Errorf("ListTools() error = %q, want it to mention SSE transport", err.Error())
	}
}

// TestProbeEra_ThroughListTools_InitializeReturnsSession verifies that the
// session ID obtained from the initialize response during auto-probe's
// legacy fallback is forwarded to the tools/list request.
func TestProbeEra_ThroughListTools_InitializeReturnsSession(t *testing.T) {
	t.Parallel()

	const assignedSession = "init-returned-session-99"

	// sessionOnToolsList captures the Mcp-Session-Id header seen during the
	// tools/list request.
	var sessionOnToolsList string

	initResp := `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`
	toolsResp := `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		w.Header().Set("Content-Type", "application/json")

		switch rpc.Method {
		case "initialize":
			// Return the session ID only on initialize.
			w.Header().Set("Mcp-Session-Id", assignedSession)
			fmt.Fprint(w, initResp)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			// Record what session ID the client sent.
			sessionOnToolsList = r.Header.Get("Mcp-Session-Id")
			fmt.Fprint(w, toolsResp)
		default:
			// "server/discover" — a plain legacy server.
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newProbeTransport(srv.URL)
	_, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
	if sessionOnToolsList != assignedSession {
		t.Errorf("tools/list Mcp-Session-Id = %q, want %q (session from initialize)", sessionOnToolsList, assignedSession)
	}
}
