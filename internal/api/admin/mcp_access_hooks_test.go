package admin_test

// This file covers Handler.AfterMCPAccessRefresh and
// Handler.AfterMCPBlocklistChange (handler.go, mcp_access.go, mcp_servers.go)
// — the hooks internal/app wires to a Code Mode *mcp.Server's own
// NotifyToolsListChanged so a subscriptions/listen subscriber learns its own
// view of tools/list may have changed. These tests verify only the HOOK
// INVOCATION contract (called with the right org/server ID, at the right
// point, and never when nil) at the admin HTTP layer; the actual
// notification delivery and tenant scoping those hooks feed into is covered
// by internal/app's mcp_notify_scoping_test.go.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
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
	"github.com/voidmind-io/voidllm/internal/proxy"
)

// hookRecorder collects every value a hook under test was invoked with, safe
// for concurrent use even though every call site in this file is currently
// synchronous with the triggering HTTP request.
type hookRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *hookRecorder) record(v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, v)
}

func (r *hookRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// scopeRecorder collects every admin.MCPAccessRefreshScope a hook under test
// was invoked with — the typed counterpart of hookRecorder for
// AfterMCPAccessRefresh, whose argument is now a scope struct rather than a
// bare org ID string (item 3: SetKeyMCPAccess/SetTeamMCPAccess/
// SetOrgMCPAccess each resolve their own precise scope, not always OrgID).
type scopeRecorder struct {
	mu    sync.Mutex
	calls []admin.MCPAccessRefreshScope
}

func (r *scopeRecorder) record(v admin.MCPAccessRefreshScope) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, v)
}

func (r *scopeRecorder) snapshot() []admin.MCPAccessRefreshScope {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]admin.MCPAccessRefreshScope(nil), r.calls...)
}

func (r *scopeRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

// setupMCPAccessHooksApp builds a Fiber app wired with AfterMCPAccessRefresh
// and AfterMCPBlocklistChange recording into the returned recorders
// (accessCalls, blocklistCalls respectively), and an EncryptionKey so MCP
// server creation works.
func setupMCPAccessHooksApp(t *testing.T, dsn string) (app *fiber.App, database *db.DB, keyCache *cache.Cache[string, auth.KeyInfo], accessCalls *scopeRecorder, blocklistCalls *hookRecorder) {
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

	keyCache = cache.New[string, auth.KeyInfo]()
	accessCalls = &scopeRecorder{}
	blocklistCalls = &hookRecorder{}

	handler := &admin.Handler{
		DB:            database,
		HMACSecret:    testHMACSecret,
		EncryptionKey: testEncryptionKey,
		KeyCache:      keyCache,
		License:       license.NewHolder(license.Verify("", true)),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		// refreshMCPAccessCache (handler.go) is a no-op — and so never calls
		// AfterMCPAccessRefresh — when MCPAccessCache is nil; every test in
		// this file needs a real one so the hook under test actually fires.
		MCPAccessCache: proxy.NewMCPAccessCache(),
		AfterMCPAccessRefresh: func(scope admin.MCPAccessRefreshScope) {
			accessCalls.record(scope)
		},
		AfterMCPBlocklistChange: func(serverID string) {
			blocklistCalls.record(serverID)
		},
	}

	app = fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)
	return app, database, keyCache, accessCalls, blocklistCalls
}

// ---- AfterMCPAccessRefresh: SetOrgMCPAccess ----------------------------------

func TestAfterMCPAccessRefresh_SetOrgMCPAccess_CalledWithOrgID(t *testing.T) {
	t.Parallel()

	app, database, keyCache, accessCalls, _ := setupMCPAccessHooksApp(t,
		"file:TestAfterMCPAccessRefresh_SetOrgMCPAccess?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Hooks Org", "hooks-org-set-org")
	server := mustCreateGlobalMCPServer(t, database, "Hooks Server", "hooks-server-org")
	key := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	resp := mcpServerRequest(t, app, http.MethodPut, orgMCPAccessURL(org.ID), key,
		map[string]any{"servers": []string{server.ID}})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	want := admin.MCPAccessRefreshScope{OrgID: org.ID}
	if got := accessCalls.snapshot(); len(got) != 1 || got[0] != want {
		t.Errorf("AfterMCPAccessRefresh calls = %+v, want exactly one call with %+v", got, want)
	}
}

// ---- AfterMCPAccessRefresh: SetTeamMCPAccess scopes to that team only ------

func TestAfterMCPAccessRefresh_SetTeamMCPAccess_ScopedToTeamOnly(t *testing.T) {
	t.Parallel()

	app, database, keyCache, accessCalls, _ := setupMCPAccessHooksApp(t,
		"file:TestAfterMCPAccessRefresh_SetTeamMCPAccess?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Hooks Org Team", "hooks-org-set-team")
	server := mustCreateGlobalMCPServer(t, database, "Hooks Server Team", "hooks-server-team")
	team := mustCreateTeam(t, database, org.ID, "Hooks Team", "hooks-team")
	key := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	// The org must already allow the server before a team-level allowlist
	// naming it is accepted (requireSubsetOfOrgMCPServers).
	orgResp := mcpServerRequest(t, app, http.MethodPut, orgMCPAccessURL(org.ID), key,
		map[string]any{"servers": []string{server.ID}})
	orgResp.Body.Close()
	accessCalls.reset()

	resp := mcpServerRequest(t, app, http.MethodPut, teamMCPAccessURL(org.ID, team.ID), key,
		map[string]any{"servers": []string{server.ID}})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	// SetTeamMCPAccess resolves a TeamID-only scope — its own blast radius is
	// known precisely to be that one team's subscribers, not the whole org
	// (item 3) — even though requireSubsetOfOrgMCPServers also reads the
	// org's own allowlist as part of validation.
	want := admin.MCPAccessRefreshScope{TeamID: team.ID}
	if got := accessCalls.snapshot(); len(got) != 1 || got[0] != want {
		t.Errorf("AfterMCPAccessRefresh calls = %+v, want exactly one call with %+v", got, want)
	}
}

// ---- AfterMCPAccessRefresh: SetKeyMCPAccess scopes to that key only --------

func TestAfterMCPAccessRefresh_SetKeyMCPAccess_ScopedToKeyOnly(t *testing.T) {
	t.Parallel()

	app, database, keyCache, accessCalls, _ := setupMCPAccessHooksApp(t,
		"file:TestAfterMCPAccessRefresh_SetKeyMCPAccess?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Hooks Org Key", "hooks-org-set-key")
	server := mustCreateGlobalMCPServer(t, database, "Hooks Server Key", "hooks-server-key")
	adminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)
	user := mustCreateUser(t, database, "hooks-key-user@example.com", "Hooks Key User")
	targetKey := mustCreateAPIKeyInDB(t, database, org.ID, user.ID)

	orgResp := mcpServerRequest(t, app, http.MethodPut, orgMCPAccessURL(org.ID), adminKey,
		map[string]any{"servers": []string{server.ID}})
	orgResp.Body.Close()
	accessCalls.reset()

	resp := mcpServerRequest(t, app, http.MethodPut, keyMCPAccessURL(org.ID, targetKey.ID), adminKey,
		map[string]any{"servers": []string{server.ID}})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	want := admin.MCPAccessRefreshScope{KeyID: targetKey.ID}
	if got := accessCalls.snapshot(); len(got) != 1 || got[0] != want {
		t.Errorf("AfterMCPAccessRefresh calls = %+v, want exactly one call with %+v", got, want)
	}
}

// ---- AfterMCPBlocklistChange: Add/Remove -------------------------------------

func TestAfterMCPBlocklistChange_AddMCPServerBlocklist_CalledWithServerID(t *testing.T) {
	t.Parallel()

	app, database, keyCache, _, blocklistCalls := setupMCPAccessHooksApp(t,
		"file:TestAfterMCPBlocklistChange_Add?mode=memory&cache=private")
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-hooks-blocklist-add")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Hooks Blocklist Add Server",
		Alias:    "hooks-bl-add-server",
		URL:      "https://hooks-bl-add.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	resp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/mcp-servers/"+s.ID+"/blocklist", key,
		map[string]any{"tool_name": "exec-shell", "reason": "hooks test"})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, raw)
	}

	if got := blocklistCalls.snapshot(); len(got) != 1 || got[0] != s.ID {
		t.Errorf("AfterMCPBlocklistChange calls = %v, want exactly one call with server ID %q", got, s.ID)
	}
}

func TestAfterMCPBlocklistChange_RemoveMCPServerBlocklist_CalledWithServerID(t *testing.T) {
	t.Parallel()

	app, database, keyCache, _, blocklistCalls := setupMCPAccessHooksApp(t,
		"file:TestAfterMCPBlocklistChange_Remove?mode=memory&cache=private")
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-hooks-blocklist-remove")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Hooks Blocklist Remove Server",
		Alias:    "hooks-bl-remove-server",
		URL:      "https://hooks-bl-remove.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	addResp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/mcp-servers/"+s.ID+"/blocklist", key,
		map[string]any{"tool_name": "exec-shell", "reason": "hooks test"})
	addResp.Body.Close()
	blocklistCalls.reset()

	// RemoveMCPServerBlocklist identifies the entry by the tool_name query
	// parameter, not a path segment (mcp_servers.go).
	resp := mcpServerRequest(t, app, http.MethodDelete,
		"/api/v1/mcp-servers/"+s.ID+"/blocklist?tool_name=exec-shell", key, nil)
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 204; body: %s", resp.StatusCode, raw)
	}

	if got := blocklistCalls.snapshot(); len(got) != 1 || got[0] != s.ID {
		t.Errorf("AfterMCPBlocklistChange calls = %v, want exactly one call with server ID %q", got, s.ID)
	}
}

// reset clears previously recorded calls, for a test that needs to isolate a
// SECOND mutation's own hook call from a first, unrelated setup mutation.
func (r *hookRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

// ---- nil hooks are a perfectly ordinary configuration ------------------------

// TestMCPAccessHooks_NilHooks_NoPanic verifies both fields' own doc: nil is a
// perfectly ordinary configuration and every call site nil-checks before
// invoking. setupMCPServersTestApp (mcp_servers_test.go) and
// setupMCPAccessHooksApp's own zero-value handler fields already exercise
// this in every OTHER test in this package; this test pins it down
// explicitly for both mutation families in one place.
func TestMCPAccessHooks_NilHooks_NoPanic(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupMCPServersTestApp(t, "file:TestMCPAccessHooks_NilHooks_NoPanic?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Nil Hooks Org", "nil-hooks-org")
	server := mustCreateGlobalMCPServer(t, database, "Nil Hooks Server", "nil-hooks-server")
	key := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	resp := mcpServerRequest(t, app, http.MethodPut, orgMCPAccessURL(org.ID), key,
		map[string]any{"servers": []string{server.ID}})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	adminKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-nil-hooks-blocklist")
	blResp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/mcp-servers/"+server.ID+"/blocklist", adminKey,
		map[string]any{"tool_name": "some-tool", "reason": "nil hooks test"})
	defer blResp.Body.Close()
	if blResp.StatusCode != fiber.StatusCreated {
		raw, _ := io.ReadAll(blResp.Body)
		t.Fatalf("blocklist add status = %d, want 201; body: %s", blResp.StatusCode, raw)
	}
}
