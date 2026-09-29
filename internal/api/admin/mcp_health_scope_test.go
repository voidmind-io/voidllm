package admin_test

// Tests covering the caller-scoped filtering ListMCPServerHealth applies on
// top of health.MCPHealthChecker.GetAllHealth(): a system_admin sees every
// probed server, while an org member sees only the org-scoped and global
// servers visible to their own organization (team-scoped servers and other
// orgs' servers are excluded), matching db.ListMCPServersByOrg's visibility
// rule.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/health"
	"github.com/voidmind-io/voidllm/internal/license"
)

// openMCPHealthScopeDB opens an in-memory SQLite DB and runs migrations,
// without yet building the health checker or the Fiber app — tests create
// their MCP servers first so the checker can be seeded with real server IDs.
func openMCPHealthScopeDB(t *testing.T, dsn string) (*db.DB, *cache.Cache[string, auth.KeyInfo]) {
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

	return database, cache.New[string, auth.KeyInfo]()
}

// buildMCPHealthApp builds an MCPHealthChecker seeded with a "healthy" result
// for each of serverIDs, then wires it and database/keyCache into a Fiber app.
// Every target uses Source: "builtin" purely so the checker records a healthy
// result synchronously on Start() without making a real network call — this
// is unrelated to the db.MCPServer.Source field on the servers themselves,
// which are created separately by the caller with whatever scope the test needs.
func buildMCPHealthApp(t *testing.T, database *db.DB, keyCache *cache.Cache[string, auth.KeyInfo], serverIDs []string) *fiber.App {
	t.Helper()

	targets := make([]health.MCPServerTarget, len(serverIDs))
	for i, id := range serverIDs {
		targets[i] = health.MCPServerTarget{
			ID:     id,
			Name:   "target-" + id,
			Alias:  "alias-" + id,
			Source: "builtin", // bypass real network probing; see doc comment above
		}
	}
	checker := health.NewMCPHealthChecker(
		func() []health.MCPServerTarget { return targets },
		24*time.Hour, // long enough that the background ticker never fires during the test
		true,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		nil,
	)
	stop := checker.Start()
	t.Cleanup(stop)

	handler := &admin.Handler{
		DB:               database,
		HMACSecret:       testHMACSecret,
		EncryptionKey:    testEncryptionKey,
		KeyCache:         keyCache,
		License:          license.NewHolder(license.Verify("", true)),
		Log:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		MCPHealthChecker: checker,
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)
	return app
}

// mcpHealthServerIDs decodes a ListMCPServerHealth response body into the set
// of server_id values present.
func mcpHealthServerIDs(t *testing.T, body io.ReadCloser) map[string]bool {
	t.Helper()
	list := decodeMCPServerList(t, body)
	ids := make(map[string]bool, len(list))
	for _, entry := range list {
		id, ok := entry["server_id"].(string)
		if !ok {
			t.Fatalf("health entry missing server_id: %v", entry)
		}
		ids[id] = true
	}
	return ids
}

// TestListMCPServerHealth_ScopedByCallerOrg builds one org-scoped server per
// org, one team-scoped server, and one global server, then asserts that a
// system_admin sees all four while an org member sees only their own
// org-scoped server plus the global one.
func TestListMCPServerHealth_ScopedByCallerOrg(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListMCPServerHealth_ScopedByCallerOrg?mode=memory&cache=private"
	database, keyCache := openMCPHealthScopeDB(t, dsn)

	orgA := mustCreateOrg(t, database, "Health Org A", "health-org-a")
	orgB := mustCreateOrg(t, database, "Health Org B", "health-org-b")
	teamA1 := mustCreateTeam(t, database, orgA.ID, "Health Team A1", "health-team-a1")

	globalSv := mustCreateGlobalMCPServer(t, database, "Global", "health-global")
	orgASv := mustCreateOrgScopedMCPServer(t, database, orgA.ID, "Org A", "health-org-a-srv")
	orgBSv := mustCreateOrgScopedMCPServer(t, database, orgB.ID, "Org B", "health-org-b-srv")
	teamSv, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Team A1",
		Alias:    "health-team-a1-srv",
		URL:      "https://example.com",
		AuthType: "none",
		OrgID:    &orgA.ID,
		TeamID:   &teamA1.ID,
	})
	if err != nil {
		t.Fatalf("create team-scoped server: %v", err)
	}

	app := buildMCPHealthApp(t, database, keyCache, []string{globalSv.ID, orgASv.ID, orgBSv.ID, teamSv.ID})

	sysAdminKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")
	orgAMemberKey := addTestKey(t, keyCache, auth.RoleMember, orgA.ID)

	t.Run("system admin sees all servers", func(t *testing.T) {
		resp := mcpServerRequest(t, app, http.MethodGet, "/api/v1/mcp-servers/health", sysAdminKey, nil)
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		ids := mcpHealthServerIDs(t, resp.Body)
		for _, want := range []string{globalSv.ID, orgASv.ID, orgBSv.ID, teamSv.ID} {
			if !ids[want] {
				t.Errorf("system_admin missing server_id %q in response", want)
			}
		}
	})

	t.Run("org A member sees org-scoped and global only", func(t *testing.T) {
		resp := mcpServerRequest(t, app, http.MethodGet, "/api/v1/mcp-servers/health", orgAMemberKey, nil)
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		ids := mcpHealthServerIDs(t, resp.Body)
		if !ids[globalSv.ID] {
			t.Error("org A member missing global server_id in response")
		}
		if !ids[orgASv.ID] {
			t.Error("org A member missing own org-scoped server_id in response")
		}
		if ids[orgBSv.ID] {
			t.Error("org A member must not see org B's server_id")
		}
		if ids[teamSv.ID] {
			t.Error("org A member must not see the team-scoped server_id (team-scoped servers are excluded from org-level health)")
		}
	})
}

// TestListMCPServerHealth_MissingAuth verifies that a request with no
// Authorization header is rejected before reaching the handler's own ki==nil
// defense-in-depth check.
func TestListMCPServerHealth_MissingAuth(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListMCPServerHealth_MissingAuth?mode=memory&cache=private"
	database, keyCache := openMCPHealthScopeDB(t, dsn)
	app := buildMCPHealthApp(t, database, keyCache, nil)

	resp := mcpServerRequest(t, app, http.MethodGet, "/api/v1/mcp-servers/health", "", nil)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// TestListMCPServerHealth_DBErrorReturns500WithoutPartialResults verifies that
// when the visibility lookup (ListMCPServersByOrg) fails for a non-system-admin
// caller, the handler returns 500 rather than falling back to the unfiltered
// (or a partially filtered) result set. The DB is closed before the request to
// force every subsequent query to fail deterministically.
func TestListMCPServerHealth_DBErrorReturns500WithoutPartialResults(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListMCPServerHealth_DBError?mode=memory&cache=private"
	database, keyCache := openMCPHealthScopeDB(t, dsn)

	org := mustCreateOrg(t, database, "DB Error Org", "db-error-org")
	memberKey := addTestKey(t, keyCache, auth.RoleMember, org.ID)

	sv := mustCreateGlobalMCPServer(t, database, "DB Error Global", "db-error-global")
	app := buildMCPHealthApp(t, database, keyCache, []string{sv.ID})

	// Sanity check: the endpoint works before the DB is closed.
	okResp := mcpServerRequest(t, app, http.MethodGet, "/api/v1/mcp-servers/health", memberKey, nil)
	okResp.Body.Close()
	if okResp.StatusCode != fiber.StatusOK {
		t.Fatalf("pre-close status = %d, want 200", okResp.StatusCode)
	}

	if err := database.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	resp := mcpServerRequest(t, app, http.MethodGet, "/api/v1/mcp-servers/health", memberKey, nil)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	// The error envelope must not contain a "server_id" field — i.e. no
	// partial/unfiltered health array leaked through on the failure path.
	body := decodeMCPServerResponse(t, resp.Body)
	if _, ok := body["server_id"]; ok {
		t.Errorf("500 response unexpectedly contains health data: %v", body)
	}
}
