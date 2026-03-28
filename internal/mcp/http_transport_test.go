package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// newTransport is a convenience constructor that defaults to a 5-second timeout
// with private addresses allowed (test servers run on loopback).
func newTransport(endpoint, authType, authHeader, authToken string) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, authType, authHeader, authToken, 5*time.Second, true)
}

// ---- Call -------------------------------------------------------------------

func TestHTTPTransport_Call_Success(t *testing.T) {
	t.Parallel()

	want := `{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, want)
	}))
	t.Cleanup(srv.Close)

	tr := newTransport(srv.URL, "none", "", "")
	got, _, err := tr.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}
	if string(got) != want {
		t.Errorf("Call() = %q, want %q", got, want)
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

	tr := newTransport(srv.URL, "bearer", "", token)
	_, _, err := tr.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "")
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

	tr := newTransport(srv.URL, "header", headerName, token)
	_, _, err := tr.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "")
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

	tr := newTransport(srv.URL, "none", "", "")
	_, _, err := tr.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "")
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

	tr := mcp.NewHTTPTransport(srv.URL, "none", "", "", 50*time.Millisecond, true)
	_, _, err := tr.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "")
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

	tr := newTransport(srv.URL, "none", "", "")
	_, _, err := tr.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "")
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

	tr := newTransport(srv.URL, "none", "", "")
	resp, _, err := tr.Call(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/ping"}`), "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil for 202", err)
	}
	if resp != nil {
		t.Errorf("Call() response = %q, want nil for 202 Accepted", resp)
	}
}

func TestHTTPTransport_Call_BodyLimit(t *testing.T) {
	t.Parallel()

	// Serve exactly 10 MiB + 1 byte. The transport must not crash — it should
	// silently truncate to the limit and still return a non-nil body.
	const limit = 10 << 20 // 10 MiB
	oversized := bytes.Repeat([]byte("x"), limit+1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(oversized)
	}))
	t.Cleanup(srv.Close)

	tr := newTransport(srv.URL, "none", "", "")
	got, _, err := tr.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "")
	// Must not error — body is merely truncated.
	if err != nil {
		t.Fatalf("Call() error = %v, want nil (body should be truncated not error)", err)
	}
	if len(got) > limit {
		t.Errorf("body len = %d, want at most %d (10 MiB limit)", len(got), limit)
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

	tr := newTransport(srv.URL, "none", "", "")
	tools, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
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

	tr := newTransport(srv.URL, "none", "", "")
	tools, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want error for JSON-RPC error response")
	}
	if tools != nil {
		t.Errorf("ListTools() tools = %v, want nil on error", tools)
	}
	if !strings.Contains(err.Error(), "method not found") {
		t.Errorf("error = %q, want it to contain the RPC error message", err.Error())
	}
}

// ---- Call session ID handling -----------------------------------------------

func TestCall_SessionID_Forwarded(t *testing.T) {
	t.Parallel()

	const wantSessionID = "session-abc-123"
	var gotHeader string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newTransport(srv.URL, "none", "", "")
	_, _, err := tr.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), wantSessionID)
	if err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}
	if gotHeader != wantSessionID {
		t.Errorf("Mcp-Session-Id request header = %q, want %q", gotHeader, wantSessionID)
	}
}

func TestCall_SessionID_Returned(t *testing.T) {
	t.Parallel()

	const responseSessionID = "server-assigned-session-xyz"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", responseSessionID)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newTransport(srv.URL, "none", "", "")
	_, gotSession, err := tr.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}
	if gotSession != responseSessionID {
		t.Errorf("returned session ID = %q, want %q", gotSession, responseSessionID)
	}
}

func TestCall_SessionExpired_404(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.NotFound(w, nil)
	}))
	t.Cleanup(srv.Close)

	tr := newTransport(srv.URL, "none", "", "")
	_, _, err := tr.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "stale-session")
	if err == nil {
		t.Fatal("Call() error = nil, want ErrSessionExpired")
	}
	if !strings.Contains(err.Error(), mcp.ErrSessionExpired.Error()) {
		t.Errorf("error = %q, want it to wrap ErrSessionExpired", err.Error())
	}
}

func TestCall_NoSessionID_NoHeader(t *testing.T) {
	t.Parallel()

	var gotHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Record what was sent; an absent header returns "".
		gotHeader = r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newTransport(srv.URL, "none", "", "")
	_, _, err := tr.Call(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}
	if gotHeader != "" {
		t.Errorf("Mcp-Session-Id header = %q, want empty (should not be sent when session is empty)", gotHeader)
	}
}

// ---- ListTools session flow -------------------------------------------------

// TestListTools_AutoInitialize verifies that ListTools performs an initialize
// call before tools/list and gracefully handles the session lifecycle.
func TestListTools_AutoInitialize(t *testing.T) {
	t.Parallel()

	var callOrder []string

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

		switch rpc.Method {
		case "initialize":
			callOrder = append(callOrder, "initialize")
			fmt.Fprint(w, initResp)
		case "notifications/initialized":
			callOrder = append(callOrder, "notifications/initialized")
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			callOrder = append(callOrder, "tools/list")
			fmt.Fprint(w, toolsResp)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newTransport(srv.URL, "none", "", "")
	tools, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
	if len(tools) != 1 {
		t.Fatalf("ListTools() count = %d, want 1", len(tools))
	}
	if tools[0].Name != "mytool" {
		t.Errorf("tools[0].Name = %q, want %q", tools[0].Name, "mytool")
	}

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

// TestListTools_InitializeReturnsSession verifies that the session ID obtained
// from the initialize response is forwarded to the tools/list request.
func TestListTools_InitializeReturnsSession(t *testing.T) {
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
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newTransport(srv.URL, "none", "", "")
	_, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
	if sessionOnToolsList != assignedSession {
		t.Errorf("tools/list Mcp-Session-Id = %q, want %q (session from initialize)", sessionOnToolsList, assignedSession)
	}
}
