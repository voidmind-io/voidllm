package admin_test

// Tests for Handler.NotifyMCPServerScopeChange's own wiring contract (item
// 1's "every mutation that changes what tools/list renders must notify both
// sides" requirement): CreateMCPServer, ActivateMCPServer, DeleteMCPServer,
// DeactivateMCPServer, and a scope/alias/CodeModeEnabled-affecting
// UpdateMCPServer each call the hook — wired exactly as internal/app wires
// it in production, firing mcp.NotifyScope{Server: before} (when there is a
// "before") and mcp.NotifyScope{ServerID: serverID} (when there is a live
// "after") independently — and each notification reaches only the
// subscriber whose own identity could actually see the server under that
// scope, never a subscriber in an unrelated organization.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strconv"
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
)

// mustCreateOrgMCPServer creates an active, org-scoped, code-mode-enabled MCP
// server directly via the DB layer. Fails the test on error.
func mustCreateOrgMCPServer(t *testing.T, database *db.DB, orgID, name, alias string) *db.MCPServer {
	t.Helper()
	enabled := true
	server, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:            name,
		Alias:           alias,
		URL:             "https://example.com/" + alias,
		AuthType:        "none",
		OrgID:           &orgID,
		CodeModeEnabled: &enabled,
	})
	if err != nil {
		t.Fatalf("mustCreateOrgMCPServer(%q, %q): %v", name, alias, err)
	}
	return server
}

// mustCreateOrgMCPServerCodeModeDisabled is mustCreateOrgMCPServer's own
// CodeModeEnabled=false counterpart, for the "enable" (false->true) case
// item 1 explicitly requires coverage for.
func mustCreateOrgMCPServerCodeModeDisabled(t *testing.T, database *db.DB, orgID, name, alias string) *db.MCPServer {
	t.Helper()
	disabled := false
	server, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:            name,
		Alias:           alias,
		URL:             "https://example.com/" + alias,
		AuthType:        "none",
		OrgID:           &orgID,
		CodeModeEnabled: &disabled,
	})
	if err != nil {
		t.Fatalf("mustCreateOrgMCPServerCodeModeDisabled(%q, %q): %v", name, alias, err)
	}
	return server
}

// notifiedServerScopeFromRow builds an mcp.NotifiedServerScope from sv's own
// current field values — the same field-for-field mapping
// internal/api/admin/mcp_servers.go's own (unexported, so unreachable from
// this black-box _test package) mcpServerScopeSnapshot performs — so
// scopeNotifyAccessChecker's live-serverID branch can apply the identical
// access rules its snapshot branch does.
func notifiedServerScopeFromRow(sv *db.MCPServer) mcp.NotifiedServerScope {
	return mcp.NotifiedServerScope{
		ID:              sv.ID,
		Alias:           sv.Alias,
		OrgID:           sv.OrgID,
		TeamID:          sv.TeamID,
		Source:          sv.Source,
		CodeModeEnabled: sv.CodeModeEnabled,
		Active:          sv.IsActive,
	}
}

// scopeNotifyAccessChecker returns a small, self-contained mcp.AccessChecker
// standing in for internal/app's own codeModeAccessChecker (which this
// package cannot import — admin is a lower layer than app), covering exactly
// the rules this file's own fixtures exercise: an org- or team-scoped
// server is visible to a matching org (and, if team-scoped, matching team)
// identity, provided it is both Active and CodeModeEnabled — item 2's own
// requirement that a NotifiedServerScope snapshot is judged by real access
// rules, not a stub that ignores it. Both the snapshot branch (before a
// mutation) and the live-serverID branch (resolved via a real GetMCPServer
// read against database — a test-only shortcut; production ACCESS checkers
// must never hit the database, see AccessChecker's own doc) apply the exact
// same decision, via notifiedServerScopeFromRow, so this file's tests can
// observe BOTH halves of item 1's dual notify.
func scopeNotifyAccessChecker(database *db.DB) mcp.AccessChecker {
	return func(id mcp.KeyIdentity, serverID string, snapshot *mcp.NotifiedServerScope) bool {
		row := snapshot
		if row == nil {
			if serverID == "" {
				return false
			}
			sv, err := database.GetMCPServer(context.Background(), serverID)
			if err != nil {
				return false
			}
			resolved := notifiedServerScopeFromRow(sv)
			row = &resolved
		}
		if !row.Active || !row.CodeModeEnabled {
			return false
		}
		if row.TeamID != nil {
			return row.OrgID != nil && id.OrgID == *row.OrgID && id.TeamID == *row.TeamID
		}
		if row.OrgID != nil {
			return id.OrgID == *row.OrgID
		}
		return row.Source == "builtin"
	}
}

// setupMCPServerScopeNotifyApp builds a Fiber app whose Handler.
// NotifyMCPServerScopeChange is wired exactly as internal/app.New wires it
// in production — firing codeModeServer.NotifyToolsListChanged twice, once
// Server-scoped against before (when non-nil) and once ServerID-scoped
// against serverID (when non-empty) — so this test exercises the real
// integration, not merely the hook's own invocation arguments.
func setupMCPServerScopeNotifyApp(t *testing.T, dsn string) (app *fiber.App, database *db.DB, keyCache *cache.Cache[string, auth.KeyInfo], codeModeServer *mcp.Server) {
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
	codeModeServer = mcp.NewServer("code-mode", "test")
	codeModeServer.SetToolsListChangedSource(true)
	codeModeServer.SetAccessChecker(scopeNotifyAccessChecker(database))

	handler := &admin.Handler{
		DB:         database,
		HMACSecret: testHMACSecret,
		KeyCache:   keyCache,
		License:    license.NewHolder(license.Verify("", true)),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		NotifyMCPServerScopeChange: func(before *mcp.NotifiedServerScope, serverID string) {
			if before != nil {
				codeModeServer.NotifyToolsListChanged(mcp.NotifyScope{Server: before})
			}
			if serverID != "" {
				codeModeServer.NotifyToolsListChanged(mcp.NotifyScope{ServerID: serverID})
			}
		},
	}

	app = fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)
	return app, database, keyCache, codeModeServer
}

// registerScopeNotifySubscriber registers a subscriptions/listen subscriber
// on codeModeServer for identity, requiring toolsListChanged to be honored.
func registerScopeNotifySubscriber(t *testing.T, codeModeServer *mcp.Server, id mcp.KeyIdentity, reqID int) *mcp.Subscriber {
	t.Helper()
	ctx := mcp.WithKeyIdentity(context.Background(), id)
	body := `{"jsonrpc":"2.0","id":` + strconv.Itoa(reqID) + `,"method":"subscriptions/listen","params":{` +
		`"notifications":{"toolsListChanged":true},` +
		`"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28","io.modelcontextprotocol/clientCapabilities":{}}}}`
	result := codeModeServer.Handle(ctx, []byte(body), mcp.MapHeader{mcp.HeaderProtocolVersion: "2026-07-28"})
	if result.Listen == nil {
		t.Fatalf("registration for %+v failed: %s", id, result.Body)
	}
	if !result.Listen.Sub.Honored().ToolsListChanged {
		t.Fatalf("registration for %+v did not honor toolsListChanged — fixture bug", id)
	}
	t.Cleanup(result.Listen.Sub.Unregister)
	return result.Listen.Sub
}

func assertScopeNotifyEventPending(t *testing.T, sub *mcp.Subscriber) {
	t.Helper()
	select {
	case <-sub.Events():
	default:
		t.Error("no pending event, want one")
	}
}

func assertScopeNotifyNoEvent(t *testing.T, sub *mcp.Subscriber) {
	t.Helper()
	select {
	case body := <-sub.Events():
		t.Errorf("unexpected pending event: %s", body)
	default:
	}
}

func TestNotifyMCPServerScopeChange_DeleteMCPServer_NotifiesOnlyThatOrg(t *testing.T) {
	t.Parallel()

	app, database, keyCache, codeModeServer := setupMCPServerScopeNotifyApp(t,
		"file:TestNotifyMCPServerScopeChange_Delete?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Scope Notify Org", "scope-notify-org-delete")
	server := mustCreateOrgMCPServer(t, database, org.ID, "Scope Notify Server", "scope-notify-server-delete")
	adminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	sameOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: org.ID, KeyID: "key-same-org"}, 1)
	otherOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-unrelated", KeyID: "key-other-org"}, 2)

	resp := mcpServerRequest(t, app, http.MethodDelete, "/api/v1/mcp-servers/"+server.ID, adminKey, nil)
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusNoContent {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 204; body: %s", resp.StatusCode, raw)
	}

	assertScopeNotifyEventPending(t, sameOrg)
	assertScopeNotifyNoEvent(t, otherOrg)
}

func TestNotifyMCPServerScopeChange_DeactivateMCPServer_NotifiesOnlyThatOrg(t *testing.T) {
	t.Parallel()

	app, database, keyCache, codeModeServer := setupMCPServerScopeNotifyApp(t,
		"file:TestNotifyMCPServerScopeChange_Deactivate?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Scope Notify Org Deact", "scope-notify-org-deact")
	server := mustCreateOrgMCPServer(t, database, org.ID, "Scope Notify Server Deact", "scope-notify-server-deact")
	adminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	sameOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: org.ID, KeyID: "key-same-org-deact"}, 1)
	otherOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-unrelated-deact", KeyID: "key-other-org-deact"}, 2)

	resp := mcpServerRequest(t, app, http.MethodPatch, "/api/v1/mcp-servers/"+server.ID+"/deactivate", adminKey, nil)
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	assertScopeNotifyEventPending(t, sameOrg)
	assertScopeNotifyNoEvent(t, otherOrg)
}

func TestNotifyMCPServerScopeChange_UpdateMCPServer_CodeModeDisabled_NotifiesOldScope(t *testing.T) {
	t.Parallel()

	app, database, keyCache, codeModeServer := setupMCPServerScopeNotifyApp(t,
		"file:TestNotifyMCPServerScopeChange_Update?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Scope Notify Org Update", "scope-notify-org-update")
	server := mustCreateOrgMCPServer(t, database, org.ID, "Scope Notify Server Update", "scope-notify-server-update")
	adminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	sameOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: org.ID, KeyID: "key-same-org-update"}, 1)
	otherOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-unrelated-update", KeyID: "key-other-org-update"}, 2)

	// Turning OFF code_mode_enabled is a scope-affecting update: this server
	// was visible under its OLD scope (CodeModeEnabled true) and no longer
	// is — mcpVisibilityScopeChanged detects this and fires the hook with
	// the pre-mutation (CodeModeEnabled: true) snapshot.
	resp := mcpServerRequest(t, app, http.MethodPatch, "/api/v1/mcp-servers/"+server.ID, adminKey,
		map[string]any{"code_mode_enabled": false})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	assertScopeNotifyEventPending(t, sameOrg)
	assertScopeNotifyNoEvent(t, otherOrg)
}

func TestNotifyMCPServerScopeChange_UpdateMCPServer_NoScopeChange_NotCalled(t *testing.T) {
	t.Parallel()

	app, database, keyCache, codeModeServer := setupMCPServerScopeNotifyApp(t,
		"file:TestNotifyMCPServerScopeChange_NoChange?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Scope Notify Org NoChange", "scope-notify-org-nochange")
	server := mustCreateOrgMCPServer(t, database, org.ID, "Scope Notify Server NoChange", "scope-notify-server-nochange")
	adminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	sameOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: org.ID, KeyID: "key-same-org-nochange"}, 1)

	// A rename does not change OrgID, TeamID, or CodeModeEnabled — the hook
	// must not fire at all (mcpVisibilityScopeChanged reports false).
	resp := mcpServerRequest(t, app, http.MethodPatch, "/api/v1/mcp-servers/"+server.ID, adminKey,
		map[string]any{"name": "Renamed Scope Notify Server"})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	assertScopeNotifyNoEvent(t, sameOrg)
}

// TestNotifyMCPServerScopeChange_UpdateMCPServer_AliasRename_NotifiesOnlyThatOrg
// verifies item 1's explicit "alias rename" requirement: mcpVisibilityScopeChanged
// now includes Alias, so a rename alone (no OrgID/TeamID/CodeModeEnabled
// change) still fires the hook — unlike a NAME rename
// (TestNotifyMCPServerScopeChange_UpdateMCPServer_NoScopeChange_NotCalled),
// which must not.
func TestNotifyMCPServerScopeChange_UpdateMCPServer_AliasRename_NotifiesOnlyThatOrg(t *testing.T) {
	t.Parallel()

	app, database, keyCache, codeModeServer := setupMCPServerScopeNotifyApp(t,
		"file:TestNotifyMCPServerScopeChange_AliasRename?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Scope Notify Org Rename", "scope-notify-org-rename")
	server := mustCreateOrgMCPServer(t, database, org.ID, "Scope Notify Server Rename", "scope-notify-server-rename")
	adminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	sameOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: org.ID, KeyID: "key-same-org-rename"}, 1)
	otherOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-unrelated-rename", KeyID: "key-other-org-rename"}, 2)

	resp := mcpServerRequest(t, app, http.MethodPatch, "/api/v1/mcp-servers/"+server.ID, adminKey,
		map[string]any{"alias": "scope-notify-server-renamed"})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	assertScopeNotifyEventPending(t, sameOrg)
	assertScopeNotifyNoEvent(t, otherOrg)
}

// TestNotifyMCPServerScopeChange_UpdateMCPServer_CodeModeEnable_NotifiesOnlyThatOrg
// verifies item 1's explicit "CodeModeEnabled false->true" requirement: the
// server was NOT visible before (CodeModeEnabled false, so
// scopeNotifyAccessChecker's snapshot branch denies sameOrg too), but IS
// visible after — the live-serverID half of NotifyMCPServerScopeChange
// (resolved against the cache AFTER refreshMCPCaches ran) is what reaches
// sameOrg here, proving the "after" half fires independently of the
// "before" half.
func TestNotifyMCPServerScopeChange_UpdateMCPServer_CodeModeEnable_NotifiesOnlyThatOrg(t *testing.T) {
	t.Parallel()

	app, database, keyCache, codeModeServer := setupMCPServerScopeNotifyApp(t,
		"file:TestNotifyMCPServerScopeChange_CodeModeEnable?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Scope Notify Org Enable", "scope-notify-org-enable")
	server := mustCreateOrgMCPServerCodeModeDisabled(t, database, org.ID, "Scope Notify Server Enable", "scope-notify-server-enable")
	adminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	sameOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: org.ID, KeyID: "key-same-org-enable"}, 1)
	otherOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-unrelated-enable", KeyID: "key-other-org-enable"}, 2)

	resp := mcpServerRequest(t, app, http.MethodPatch, "/api/v1/mcp-servers/"+server.ID, adminKey,
		map[string]any{"code_mode_enabled": true})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	assertScopeNotifyEventPending(t, sameOrg)
	assertScopeNotifyNoEvent(t, otherOrg)
}

// TestNotifyMCPServerScopeChange_ActivateMCPServer_NotifiesOnlyThatOrg
// verifies item 1's explicit "activate" requirement: a server created
// inactive (never visible to anyone — accessibleServers' own DB queries all
// filter WHERE is_active = 1) becomes visible the moment it is activated,
// and NotifyMCPServerScopeChange's live-serverID half — not merely the
// best-effort, asynchronous RefreshServer goroutine — is what tells a
// subscriber about it.
func TestNotifyMCPServerScopeChange_ActivateMCPServer_NotifiesOnlyThatOrg(t *testing.T) {
	t.Parallel()

	app, database, keyCache, codeModeServer := setupMCPServerScopeNotifyApp(t,
		"file:TestNotifyMCPServerScopeChange_Activate?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Scope Notify Org Activate", "scope-notify-org-activate")
	server := mustCreateOrgMCPServer(t, database, org.ID, "Scope Notify Server Activate", "scope-notify-server-activate")
	inactive := false
	if _, err := database.UpdateMCPServer(context.Background(), server.ID, db.UpdateMCPServerParams{IsActive: &inactive}); err != nil {
		t.Fatalf("deactivate fixture server: %v", err)
	}
	adminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	sameOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: org.ID, KeyID: "key-same-org-activate"}, 1)
	otherOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-unrelated-activate", KeyID: "key-other-org-activate"}, 2)

	resp := mcpServerRequest(t, app, http.MethodPatch, "/api/v1/mcp-servers/"+server.ID+"/activate", adminKey, nil)
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	assertScopeNotifyEventPending(t, sameOrg)
	assertScopeNotifyNoEvent(t, otherOrg)
}

// TestNotifyMCPServerScopeChange_CreateOrgMCPServer_NotifiesOnlyThatOrg
// verifies item 1's explicit "create" requirement: there is no "before" for
// a brand-new server, so only the live-serverID half applies — a subscriber
// already registered against the SAME org, before the server even existed,
// still learns about it the moment CreateOrgMCPServer's own
// NotifyMCPServerScopeChange(nil, s.ID) call resolves against the
// just-refreshed cache.
func TestNotifyMCPServerScopeChange_CreateOrgMCPServer_NotifiesOnlyThatOrg(t *testing.T) {
	t.Parallel()

	app, database, keyCache, codeModeServer := setupMCPServerScopeNotifyApp(t,
		"file:TestNotifyMCPServerScopeChange_Create?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Scope Notify Org Create", "scope-notify-org-create")
	adminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	sameOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: org.ID, KeyID: "key-same-org-create"}, 1)
	otherOrg := registerScopeNotifySubscriber(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-unrelated-create", KeyID: "key-other-org-create"}, 2)

	resp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/orgs/"+org.ID+"/mcp-servers", adminKey,
		map[string]any{
			"name":  "Scope Notify Server Create",
			"alias": "scope-notify-server-create",
			"url":   "https://example.com/scope-notify-server-create",
		})
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, raw)
	}

	assertScopeNotifyEventPending(t, sameOrg)
	assertScopeNotifyNoEvent(t, otherOrg)
}
