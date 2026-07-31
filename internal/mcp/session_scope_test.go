package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file is the regression suite for docs/mcp-v2.md §11.3 Befund 1: a
// global legacy MCP server (org_id IS NULL) is reachable by many
// organizations, and without per-org isolation every one of them would share
// a single Mcp-Session-Id and could observe upstream state left behind by
// another org. HTTPTransport.Call's SessionScope parameter exists
// specifically to prevent that. Every test here fails loudly — via
// initialize call counts and distinct session IDs observed at a fake
// upstream — if that isolation ever regresses.

// sessionScopeHandler returns an httptest handler that assigns a fresh,
// numbered Mcp-Session-Id on every "initialize" it receives, always accepts
// "ping" unconditionally, and records the JSON-RPC method sequence plus every
// session ID it ever minted. mu guards both slices.
func sessionScopeHandler(seenMethods, assignedSessions *[]string, mu *sync.Mutex) http.Handler {
	var initializeCount int
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		mu.Lock()
		*seenMethods = append(*seenMethods, rpc.Method)
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "initialize":
			mu.Lock()
			initializeCount++
			session := fmt.Sprintf("session-%d", initializeCount)
			*assignedSessions = append(*assignedSessions, session)
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", session)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "ping":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
}

// TestCall_Legacy_SessionScope_DifferentOrgsGetIsolatedSessions is the
// primary regression test against the tenant-isolation regression named in
// docs/mcp-v2.md §11.3 Befund 1: two calls scoped to two different
// organizations sharing one global legacy server must each get their own
// initialize handshake and their own, distinct Mcp-Session-Id — never one
// shared session.
func TestCall_Legacy_SessionScope_DifferentOrgsGetIsolatedSessions(t *testing.T) {
	t.Parallel()

	var seenMethods, assignedSessions []string
	var mu sync.Mutex

	srv := httptest.NewServer(sessionScopeHandler(&seenMethods, &assignedSessions, &mu))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")

	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, mcp.SessionScope("org-A")); err != nil {
		t.Fatalf("Call(org-A) error = %v, want nil", err)
	}
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, mcp.SessionScope("org-B")); err != nil {
		t.Fatalf("Call(org-B) error = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(assignedSessions) != 2 {
		t.Fatalf("initialize count = %d, want 2 — a global legacy server MUST NOT share one session across organizations (docs/mcp-v2.md §11.3 Befund 1)", len(assignedSessions))
	}
	if assignedSessions[0] == assignedSessions[1] {
		t.Errorf("both orgs were assigned the same session %q, want two distinct sessions", assignedSessions[0])
	}
}

// TestCall_Legacy_SessionScope_SameScopeSharesOneSession is the positive
// control for the isolation test above: two calls made with the SAME scope
// must reuse a single session, exactly like the pre-Phase-2, unscoped
// behavior — isolation must apply ACROSS scopes, not fragment a single org's
// own session into one per Call.
func TestCall_Legacy_SessionScope_SameScopeSharesOneSession(t *testing.T) {
	t.Parallel()

	var seenMethods, assignedSessions []string
	var mu sync.Mutex

	srv := httptest.NewServer(sessionScopeHandler(&seenMethods, &assignedSessions, &mu))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")

	for i := 0; i < 3; i++ {
		if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, mcp.SessionScope("org-A")); err != nil {
			t.Fatalf("Call #%d error = %v, want nil", i, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if len(assignedSessions) != 1 {
		t.Errorf("initialize count = %d, want exactly 1 — repeated calls in the same scope must reuse one session, not mint a fresh one each time", len(assignedSessions))
	}
}

// TestCall_Legacy_SessionScope_EmptyScopeIsolatedFromTenantScope verifies
// that the empty SessionScope — reserved for system-internal calls such as
// ListTools and health probes — is itself isolated from any real
// organization's scope, exactly like two distinct organizations would be.
func TestCall_Legacy_SessionScope_EmptyScopeIsolatedFromTenantScope(t *testing.T) {
	t.Parallel()

	var seenMethods, assignedSessions []string
	var mu sync.Mutex

	srv := httptest.NewServer(sessionScopeHandler(&seenMethods, &assignedSessions, &mu))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")

	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("Call(empty scope) error = %v, want nil", err)
	}
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, mcp.SessionScope("org-A")); err != nil {
		t.Fatalf("Call(org-A) error = %v, want nil", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(assignedSessions) != 2 {
		t.Fatalf("initialize count = %d, want 2 — the system-internal empty scope must not share a session with any real org's scope", len(assignedSessions))
	}
	if assignedSessions[0] == assignedSessions[1] {
		t.Errorf("empty scope and org-A were assigned the same session %q, want two distinct sessions", assignedSessions[0])
	}
}

// TestCall_Modern_SessionScope_IsMeaningless verifies that under the modern
// era SessionScope has no observable effect at all: no initialize is ever
// sent and no Mcp-Session-Id header ever appears, regardless of how many
// distinct scopes are used — the 2026-07-28 revision's stateless core has no
// session concept for SessionScope to isolate (docs/mcp-v2.md §2, §3.1).
func TestCall_Modern_SessionScope_IsMeaningless(t *testing.T) {
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

	scopes := []mcp.SessionScope{"", "org-A", "org-B", "org-A"}
	for _, scope := range scopes {
		if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, scope); err != nil {
			t.Fatalf("Call(scope=%q) error = %v, want nil", scope, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if sawSessionHeader {
		t.Error("Mcp-Session-Id header was sent, want it never to appear under the modern era regardless of scope")
	}
	for _, m := range methodsSeen {
		if m == "initialize" {
			t.Errorf("methods seen = %v, want no initialize under the modern era, for any scope", methodsSeen)
			break
		}
	}
	if len(methodsSeen) != len(scopes) {
		t.Errorf("methods seen = %v, want exactly %d pings (one per Call, no per-scope handshake overhead)", methodsSeen, len(scopes))
	}
}
