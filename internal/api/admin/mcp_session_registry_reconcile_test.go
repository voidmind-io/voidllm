package admin_test

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

// This file is the full-stack counterpart of internal/mcp's
// TestSessionRegistry_Reconcile_* unit tests: it drives Handler.
// refreshMCPCaches's real production trigger — an MCP server mutation
// through the actual admin API (delete, deactivate) — and asserts directly
// on the resulting *mcp.SessionRegistry state, rather than calling Reconcile
// by hand.

// reconcileTestApp bundles together everything a test in this file needs
// direct access to: the app for driving both the MCP proxy route (to prime a
// real session) and the admin mcp-servers CRUD routes (to trigger
// refreshMCPCaches), plus the database and the *admin.Handler itself so
// assertions can read Handler.MCPSessionRegistry directly instead of
// inferring its state indirectly through more proxied requests.
type reconcileTestApp struct {
	app     *fiber.App
	db      *db.DB
	keys    *cache.Cache[string, auth.KeyInfo]
	handler *admin.Handler
	cache   *proxy.MCPTransportCache
}

// setupReconcileTestApp builds a Fiber app wired exactly like production
// (internal/app): MCPSessionRegistry and MCPTransportCache both set, so a
// proxied request primes a real session that survives across requests, and
// every mcp-servers admin mutation runs the real refreshMCPCaches ->
// Reconcile path afterward.
func setupReconcileTestApp(t *testing.T, dsn string) *reconcileTestApp {
	t.Helper()

	ctx := context.Background()
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()
	transportCache := proxy.NewMCPTransportCache(testEncryptionKey, true, 5*time.Second, 5*time.Second, noopLogger(t))
	t.Cleanup(transportCache.Close)

	handler := &admin.Handler{
		DB:                  database,
		HMACSecret:          testHMACSecret,
		EncryptionKey:       testEncryptionKey,
		KeyCache:            keyCache,
		License:             license.NewHolder(license.Verify("", true)),
		Log:                 noopLogger(t),
		MCPServer:           mcp.NewServer("voidllm", "test"),
		MCPCallTimeout:      5 * time.Second,
		MCPAllowPrivateURLs: true,
		MCPTransportCache:   transportCache,
		MCPSessionRegistry:  mcp.NewSessionRegistry(),
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return &reconcileTestApp{app: app, db: database, keys: keyCache, handler: handler, cache: transportCache}
}

// primeLegacySession registers an external, pinned-legacy MCP server under
// alias, grants org access, primes the transport cache, and drives one
// proxied "ping" through it — mirroring exactly what HandleMCPProxy does in
// production: mint a session, mirror it back to the caller, and record it
// into Handler.MCPSessionRegistry under mcp.NewClientSessionScope(org.ID,
// keyID). Returns the server's ID, the minted session ID, and the exact
// SessionScope the registry recorded it under, so the caller can assert on
// Handler.MCPSessionRegistry directly.
func primeLegacySession(t *testing.T, rt *reconcileTestApp, alias, upstreamURL, orgID, keyID, key string) (serverID, sessionID string, scope mcp.SessionScope) {
	t.Helper()

	serverID = createExternalMCPServerPinned(t, rt.db, alias, upstreamURL, "2025-03-26")
	if err := rt.db.SetOrgMCPAccess(context.Background(), orgID, []string{serverID}); err != nil {
		t.Fatalf("SetOrgMCPAccess(%s): %v", alias, err)
	}
	loadMCPTransportCache(t, rt.db, rt.cache)

	resp := proxyPost(t, rt.app, alias, key, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	sessionID = resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if sessionID == "" {
		t.Fatalf("priming %s: upstream did not mint a session, test setup is broken", alias)
	}

	scope = mcp.NewClientSessionScope(orgID, keyID)
	if !rt.handler.MCPSessionRegistry.Known(serverID, scope, sessionID) {
		t.Fatalf("priming %s: session not known in MCPSessionRegistry right after priming, test setup is broken", alias)
	}
	return serverID, sessionID, scope
}

// mintingUpstream returns an httptest handler that mints a fresh
// Mcp-Session-Id whenever it receives a request with no inbound one, and
// answers every request with an ordinary JSON-RPC result otherwise —
// exactly the minimal legacy upstream shape every test in this file needs.
func mintingUpstream(prefix string) http.Handler {
	var mu sync.Mutex
	var mintCount int
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inbound := r.Header.Get("Mcp-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		if inbound == "" {
			mu.Lock()
			mintCount++
			session := fmt.Sprintf("%s-session-%d", prefix, mintCount)
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", session)
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	})
}

// TestMCPServerDelete_PrunesSessionRegistry_FullStack is the full-stack
// regression test for admin.Handler.refreshMCPCaches's Reconcile call on the
// DELETE path: deleting an MCP server through the real admin API must leave
// no trace of it in Handler.MCPSessionRegistry — a session that was known a
// moment before the delete must be unknown immediately afterward.
func TestMCPServerDelete_PrunesSessionRegistry_FullStack(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(mintingUpstream("delete-prune"))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPServerDelete_PrunesSessionRegistry?mode=memory&cache=private"
	rt := setupReconcileTestApp(t, dsn)

	org := mustCreateTestOrg(t, rt.db, "delete-prune-org")
	const keyID = "delete-prune-key"
	key := addMCPTestKeyWithID(t, rt.keys, org.ID, keyID)

	const alias = "delete-prune-server"
	serverID, sessionID, scope := primeLegacySession(t, rt, alias, upstream.URL, org.ID, keyID, key)

	adminKey := addTestKey(t, rt.keys, auth.RoleSystemAdmin, "delete-prune-admin-org")
	respDel := mcpServerRequest(t, rt.app, http.MethodDelete, "/api/v1/mcp-servers/"+serverID, adminKey, nil)
	respDel.Body.Close()
	if respDel.StatusCode != fiber.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", respDel.StatusCode)
	}

	if rt.handler.MCPSessionRegistry.Known(serverID, scope, sessionID) {
		t.Error("session still known in MCPSessionRegistry after the server was deleted via the admin API, want the registry entry pruned")
	}
}

// TestMCPServerDelete_PrunesOnlyTheDeletedServer_SiblingServerUnaffected
// extends the prune test above: deleting ONE server must not disturb another,
// still-active server's own SessionRegistry entries — refreshMCPCaches's
// Reconcile call passes the FULL current active set, and this proves it does
// not overshoot and prune everything.
func TestMCPServerDelete_PrunesOnlyTheDeletedServer_SiblingServerUnaffected(t *testing.T) {
	t.Parallel()

	upstreamDoomed := httptest.NewServer(mintingUpstream("doomed"))
	t.Cleanup(upstreamDoomed.Close)
	upstreamSurvivor := httptest.NewServer(mintingUpstream("survivor"))
	t.Cleanup(upstreamSurvivor.Close)

	dsn := "file:TestMCPServerDelete_PrunesOnlyDeleted?mode=memory&cache=private"
	rt := setupReconcileTestApp(t, dsn)

	org := mustCreateTestOrg(t, rt.db, "prune-only-org")
	const keyID = "prune-only-key"
	key := addMCPTestKeyWithID(t, rt.keys, org.ID, keyID)

	doomedID, doomedSid, doomedScope := primeLegacySession(t, rt, "doomed-server", upstreamDoomed.URL, org.ID, keyID, key)
	survivorID, survivorSid, survivorScope := primeLegacySession(t, rt, "survivor-server", upstreamSurvivor.URL, org.ID, keyID, key)

	adminKey := addTestKey(t, rt.keys, auth.RoleSystemAdmin, "prune-only-admin-org")
	respDel := mcpServerRequest(t, rt.app, http.MethodDelete, "/api/v1/mcp-servers/"+doomedID, adminKey, nil)
	respDel.Body.Close()
	if respDel.StatusCode != fiber.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", respDel.StatusCode)
	}

	if rt.handler.MCPSessionRegistry.Known(doomedID, doomedScope, doomedSid) {
		t.Error("the DELETED server's session is still known, want it pruned")
	}
	if !rt.handler.MCPSessionRegistry.Known(survivorID, survivorScope, survivorSid) {
		t.Error("the STILL-ACTIVE sibling server's session was lost when a different server was deleted, want it unaffected")
	}
}

// TestMCPServerDeactivate_PrunesSessionRegistry_FullStack verifies that
// DEACTIVATING a server (not deleting it) prunes it from Handler.
// MCPSessionRegistry exactly the same way deletion does: refreshMCPCaches
// reloads from LoadAllActiveMCPServers, which a deactivated server no longer
// appears in, so Reconcile prunes it regardless of whether it was deleted or
// merely switched off.
func TestMCPServerDeactivate_PrunesSessionRegistry_FullStack(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(mintingUpstream("deactivate-prune"))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPServerDeactivate_PrunesSessionRegistry?mode=memory&cache=private"
	rt := setupReconcileTestApp(t, dsn)

	org := mustCreateTestOrg(t, rt.db, "deactivate-prune-org")
	const keyID = "deactivate-prune-key"
	key := addMCPTestKeyWithID(t, rt.keys, org.ID, keyID)

	const alias = "deactivate-prune-server"
	serverID, sessionID, scope := primeLegacySession(t, rt, alias, upstream.URL, org.ID, keyID, key)

	adminKey := addTestKey(t, rt.keys, auth.RoleSystemAdmin, "deactivate-prune-admin-org")
	respDeactivate := mcpServerRequest(t, rt.app, http.MethodPatch, "/api/v1/mcp-servers/"+serverID+"/deactivate", adminKey, nil)
	respDeactivate.Body.Close()
	if respDeactivate.StatusCode != fiber.StatusOK {
		t.Fatalf("deactivate status = %d, want 200", respDeactivate.StatusCode)
	}

	if rt.handler.MCPSessionRegistry.Known(serverID, scope, sessionID) {
		t.Error("session still known in MCPSessionRegistry after the server was deactivated (not deleted) via the admin API, want it pruned")
	}
}
