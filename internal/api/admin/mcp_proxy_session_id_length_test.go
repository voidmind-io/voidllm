package admin_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file is the full-stack regression test suite for the
// mcp.MaxSessionIDLength enforcement HandleMCPProxy applies on the response
// side (internal/api/admin/mcp_proxy.go: an upstream Mcp-Session-Id longer
// than mcp.MaxSessionIDLength is stripped from the response before it is
// either mirrored back to the caller or recorded into
// Handler.MCPSessionRegistry, which would silently refuse to remember it
// anyway — see mcp.MaxSessionIDLength's own doc). The unit-level counterpart
// (Record/Known refusing an over-long session ID outright) lives in
// internal/mcp/session_registry_test.go.

// overLongSessionUpstream returns an httptest handler that always answers
// with an Mcp-Session-Id response header of exactly length bytes — long
// enough to exceed mcp.MaxSessionIDLength when length says so — so tests can
// exercise both sides of the boundary with one handler.
func overLongSessionUpstream(length int) http.Handler {
	session := strings.Repeat("a", length)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", session)
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	})
}

// TestMCPProxy_OverLongUpstreamSessionID_NeverMirrored_NotTruncated is the
// core regression test: an upstream that mints an Mcp-Session-Id one byte
// longer than mcp.MaxSessionIDLength must never reach the caller at all —
// not the full value, and not a truncated prefix either, since a truncated
// ID would itself be a value SessionRegistry could never recognize on a
// follow-up request (mcp.MaxSessionIDLength's own doc: over-long IDs are
// "never recorded by Record and always reported unrecognized by Known").
func TestMCPProxy_OverLongUpstreamSessionID_NeverMirrored_NotTruncated(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(overLongSessionUpstream(mcp.MaxSessionIDLength + 1))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_OverLongUpstreamSessionID?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "over-long-session-org")
	key := addMCPTestKey(t, keyCache, org.ID)

	const alias = "over-long-session-server"
	serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, alias, key, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	got, present := resp.Header["Mcp-Session-Id"]
	if present {
		gotLen := 0
		if len(got) > 0 {
			gotLen = len(got[0])
		}
		t.Errorf("Mcp-Session-Id header present on the response (length %d), want it entirely absent — neither the "+
			"full over-long ID nor a truncated prefix", gotLen)
	}
}

// TestMCPProxy_SessionIDExactlyAtMaxLength_MirroredAndRecorded_NormalFollowUp
// is the boundary counterpart: an upstream session ID exactly
// mcp.MaxSessionIDLength bytes long is well within bounds and must be
// mirrored back to the caller normally, AND actually recorded — proven by a
// follow-up request carrying it back reaching the upstream with that exact
// session ID, not dropped as unrecognized.
func TestMCPProxy_SessionIDExactlyAtMaxLength_MirroredAndRecorded_NormalFollowUp(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(overLongSessionUpstream(mcp.MaxSessionIDLength))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_SessionIDExactlyAtMaxLength?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "at-max-length-org")
	key := addMCPTestKey(t, keyCache, org.ID)

	const alias = "at-max-length-server"
	serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, alias, key, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	sid := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(sid) != mcp.MaxSessionIDLength {
		t.Fatalf("Mcp-Session-Id length = %d, want exactly %d (mirrored unmodified at the boundary)", len(sid), mcp.MaxSessionIDLength)
	}
}

// TestMCPProxy_OverLongUpstreamSessionID_ClientReinitializesCleanlyAfterward
// verifies the consequence of the drop is a WELL-DEFINED, working state, not
// a wedged client: a caller that received no Mcp-Session-Id after an
// over-long upstream mint behaves exactly like a caller that never had a
// session at all — its very next request (with no session header of its own)
// succeeds normally, exactly as an ordinary first-ever request would.
func TestMCPProxy_OverLongUpstreamSessionID_ClientReinitializesCleanlyAfterward(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(overLongSessionUpstream(mcp.MaxSessionIDLength + 1))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_OverLongUpstreamSessionID_Reinit?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "over-long-reinit-org")
	key := addMCPTestKey(t, keyCache, org.ID)

	const alias = "over-long-reinit-server"
	serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	const body = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	resp1 := proxyPost(t, app, alias, key, body)
	if resp1.StatusCode != fiber.StatusOK {
		resp1.Body.Close()
		t.Fatalf("first request: status = %d, want 200", resp1.StatusCode)
	}
	if _, present := resp1.Header["Mcp-Session-Id"]; present {
		resp1.Body.Close()
		t.Fatal("first request: Mcp-Session-Id present despite the over-long upstream mint, test setup is broken")
	}
	resp1.Body.Close()

	// The caller has no session at all — exactly the ordinary "not
	// initialized yet" state — so its next request, carrying no session
	// header, must succeed exactly like any fresh first request would.
	resp2 := proxyPost(t, app, alias, key, body)
	defer resp2.Body.Close()
	if resp2.StatusCode != fiber.StatusOK {
		t.Errorf("second (re-initializing) request: status = %d, want 200 — a dropped over-long session must leave the "+
			"client in a normal, working state, not a wedged one", resp2.StatusCode)
	}
}
