package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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

// setupMCPServersTestApp builds a Fiber app with the admin routes registered,
// an in-memory SQLite database, and an encryption key so that auth-token
// encryption in CreateMCPServer / UpdateMCPServer works correctly.
func setupMCPServersTestApp(t *testing.T, dsn string) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()

	handler := &admin.Handler{
		DB:            database,
		HMACSecret:    testHMACSecret,
		EncryptionKey: testEncryptionKey,
		KeyCache:      keyCache,
		License:       license.NewHolder(license.Verify("", true)),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// setupMCPServersTestAppAllowPrivate is identical to setupMCPServersTestApp but
// sets MCPAllowPrivateURLs = true on the handler. Use this only in tests that
// connect to loopback httptest servers and need the SSRF dialer disabled.
func setupMCPServersTestAppAllowPrivate(t *testing.T, dsn string) (*db.DB, *cache.Cache[string, auth.KeyInfo], *fiber.App) {
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

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()

	handler := &admin.Handler{
		DB:                  database,
		HMACSecret:          testHMACSecret,
		EncryptionKey:       testEncryptionKey,
		KeyCache:            keyCache,
		License:             license.NewHolder(license.Verify("", true)),
		Log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		MCPAllowPrivateURLs: true,
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return database, keyCache, app
}

// mcpServerRequest sends an HTTP request to the MCP servers API.
func mcpServerRequest(t *testing.T, app *fiber.App, method, url, key string, body any) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bodyJSON(t, body)
	}
	req := httptest.NewRequest(method, url, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}

// decodeMCPServerResponse decodes the response body into a map.
func decodeMCPServerResponse(t *testing.T, body io.ReadCloser) map[string]any {
	t.Helper()
	defer body.Close()
	var m map[string]any
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
	return m
}

// decodeMCPServerList decodes the response body into a []map[string]any.
func decodeMCPServerList(t *testing.T, body io.ReadCloser) []map[string]any {
	t.Helper()
	defer body.Close()
	var list []map[string]any
	if err := json.NewDecoder(body).Decode(&list); err != nil {
		t.Fatalf("decode response list: %v", err)
	}
	return list
}

// createMCPServerViaAPI creates an MCP server through the API and returns the
// decoded response. Fails the test if the status is not 201.
func createMCPServerViaAPI(t *testing.T, app *fiber.App, key string, body any) map[string]any {
	t.Helper()
	resp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/mcp-servers", key, body)
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("CreateMCPServer status = %d, want 201; body: %s", resp.StatusCode, raw)
	}
	var m map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("decode created server: %v", err)
	}
	return m
}

// ---- POST /api/v1/mcp-servers -----------------------------------------------

func TestCreateMCPServer_API(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateMCPServer_API?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-test")

	body := map[string]any{
		"name":       "GitHub MCP",
		"alias":      "github",
		"url":        "https://mcp.github.example.com/v1",
		"auth_type":  "bearer",
		"auth_token": "plaintext-secret-token",
	}

	resp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/mcp-servers", key, body)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, raw)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	if got["id"] == "" || got["id"] == nil {
		t.Error("id field missing or empty")
	}
	if got["alias"] != "github" {
		t.Errorf("alias = %v, want %q", got["alias"], "github")
	}
	if got["auth_type"] != "bearer" {
		t.Errorf("auth_type = %v, want %q", got["auth_type"], "bearer")
	}
	if got["is_active"] != true {
		t.Errorf("is_active = %v, want true", got["is_active"])
	}
	// Global server must have scope = "global" and no org_id or team_id.
	if got["scope"] != "global" {
		t.Errorf("scope = %v, want %q", got["scope"], "global")
	}
	if got["org_id"] != nil {
		t.Errorf("org_id = %v, want nil for global server", got["org_id"])
	}
}

func TestCreateMCPServer_API_Validation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       map[string]any
		wantStatus int
	}{
		{
			name:       "missing name returns 400",
			body:       map[string]any{"alias": "good", "url": "https://example.com", "auth_type": "none"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "missing alias returns 400",
			body:       map[string]any{"name": "Test", "url": "https://example.com", "auth_type": "none"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "missing url returns 400",
			body:       map[string]any{"name": "Test", "alias": "test", "auth_type": "none"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "url without scheme returns 400",
			body:       map[string]any{"name": "Test", "alias": "test", "url": "mcp.example.com", "auth_type": "none"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "reserved alias voidllm returns 400",
			body:       map[string]any{"name": "Test", "alias": "voidllm", "url": "https://example.com", "auth_type": "none"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "invalid auth_type returns 400",
			body:       map[string]any{"name": "Test", "alias": "valid", "url": "https://example.com", "auth_type": "oauth"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "header auth type without auth_header returns 400",
			body:       map[string]any{"name": "Test", "alias": "hdr-test", "url": "https://example.com", "auth_type": "header"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "alias with uppercase returns 400",
			body:       map[string]any{"name": "Test", "alias": "MyServer", "url": "https://example.com", "auth_type": "none"},
			wantStatus: fiber.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateMCPServer_API_Val_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, _, keyCache := setupMCPServersTestApp(t, dsn)
			key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-val")

			resp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/mcp-servers", key, tc.body)
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				raw, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, raw)
			}
		})
	}
}

func TestCreateMCPServer_API_AuthTokenNotInResponse(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateMCPServer_AuthTokenNotInResponse?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-sec")

	body := map[string]any{
		"name":       "Secure Server",
		"alias":      "secure",
		"url":        "https://secure.example.com",
		"auth_type":  "bearer",
		"auth_token": "super-secret-value",
	}

	resp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/mcp-servers", key, body)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, raw)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	// auth_token and auth_token_enc must never appear in any API response.
	if _, ok := got["auth_token"]; ok {
		t.Error("auth_token present in response, want it excluded")
	}
	if _, ok := got["auth_token_enc"]; ok {
		t.Error("auth_token_enc present in response, want it excluded")
	}
}

// ---- POST /api/v1/orgs/:org_id/mcp-servers (org-scoped) --------------------

func TestCreateOrgMCPServer_API(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateOrgMCPServer_API?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	org := mustCreateOrg(t, database, "Org ABC", "org-abc")
	key := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	body := map[string]any{
		"name":      "Org Tool",
		"alias":     "org-tool",
		"url":       "https://org-tool.example.com",
		"auth_type": "none",
	}

	resp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/orgs/"+org.ID+"/mcp-servers", key, body)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, raw)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	if got["scope"] != "org" {
		t.Errorf("scope = %v, want %q", got["scope"], "org")
	}
	if got["org_id"] != org.ID {
		t.Errorf("org_id = %v, want %q", got["org_id"], org.ID)
	}
	if got["team_id"] != nil {
		t.Errorf("team_id = %v, want nil", got["team_id"])
	}
}

func TestCreateOrgMCPServer_API_RequiresOrgAdmin(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateOrgMCPServer_RBAC?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-xyz")

	body := map[string]any{
		"name":      "Test",
		"alias":     "test-rbac",
		"url":       "https://test-rbac.example.com",
		"auth_type": "none",
	}

	resp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/orgs/org-xyz/mcp-servers", key, body)
	resp.Body.Close()

	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("status = %d, want 403 for member creating org-scoped server", resp.StatusCode)
	}
}

// ---- GET /api/v1/orgs/:org_id/mcp-servers (org-scoped list) ----------------

func TestListOrgMCPServers_API(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListOrgMCPServers_API?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)

	org := mustCreateOrg(t, database, "List Org", "list-org-mcp")
	otherOrg := mustCreateOrg(t, database, "Other Org", "other-org-mcp")
	memberKey := addTestKey(t, keyCache, auth.RoleMember, org.ID)

	// Create a global server and an org-scoped server; both should appear.
	_, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Global",
		Alias:    "global-list",
		URL:      "https://global.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create global server: %v", err)
	}

	_, err = database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Org Scoped",
		Alias:    "org-scoped-list",
		URL:      "https://org-scoped.example.com",
		AuthType: "none",
		OrgID:    &org.ID,
	})
	if err != nil {
		t.Fatalf("create org-scoped server: %v", err)
	}

	_, err = database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Other Org Server",
		Alias:    "other-org-list",
		URL:      "https://other-org.example.com",
		AuthType: "none",
		OrgID:    &otherOrg.ID,
	})
	if err != nil {
		t.Fatalf("create other-org server: %v", err)
	}

	resp := mcpServerRequest(t, app, http.MethodGet, "/api/v1/orgs/"+org.ID+"/mcp-servers", memberKey, nil)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	list := decodeMCPServerList(t, resp.Body)

	aliases := make(map[string]bool, len(list))
	for _, s := range list {
		aliases[s["alias"].(string)] = true
	}

	if !aliases["global-list"] {
		t.Error("ListOrgMCPServers missing global server")
	}
	if !aliases["org-scoped-list"] {
		t.Error("ListOrgMCPServers missing org-scoped server")
	}
	if aliases["other-org-list"] {
		t.Error("ListOrgMCPServers returned server from different org")
	}
}

// ---- GET /api/v1/mcp-servers -----------------------------------------------

func TestListMCPServers_API(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListMCPServers_API?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-list")

	for _, alias := range []string{"alpha-list", "beta-list"} {
		createMCPServerViaAPI(t, app, key, map[string]any{
			"name":      "Server " + alias,
			"alias":     alias,
			"url":       "https://" + alias + ".example.com",
			"auth_type": "none",
		})
	}

	resp := mcpServerRequest(t, app, http.MethodGet, "/api/v1/mcp-servers", key, nil)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	list := decodeMCPServerList(t, resp.Body)
	if len(list) < 2 {
		t.Errorf("list length = %d, want >= 2", len(list))
	}
	for _, s := range list {
		if _, ok := s["auth_token"]; ok {
			t.Error("auth_token present in list response, want it excluded")
		}
		// Global list must only contain global servers.
		if s["scope"] != "global" {
			t.Errorf("ListMCPServers returned non-global server: scope = %v", s["scope"])
		}
	}
}

// ---- GET /api/v1/mcp-servers/:server_id ------------------------------------

func TestGetMCPServer_API(t *testing.T) {
	t.Parallel()

	dsn := "file:TestGetMCPServer_API?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-get")

	created := createMCPServerViaAPI(t, app, key, map[string]any{
		"name":      "Get Test Server",
		"alias":     "get-test",
		"url":       "https://get-test.example.com",
		"auth_type": "none",
	})
	serverID := created["id"].(string)

	t.Run("found", func(t *testing.T) {
		t.Parallel()
		resp := mcpServerRequest(t, app, http.MethodGet,
			"/api/v1/mcp-servers/"+serverID, key, nil)
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
		}
		got := decodeMCPServerResponse(t, resp.Body)
		if got["id"] != serverID {
			t.Errorf("id = %v, want %q", got["id"], serverID)
		}
	})

	t.Run("not found", func(t *testing.T) {
		t.Parallel()
		resp := mcpServerRequest(t, app, http.MethodGet,
			"/api/v1/mcp-servers/00000000-0000-0000-0000-000000000000", key, nil)
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusNotFound {
			t.Errorf("status = %d, want 404", resp.StatusCode)
		}
	})
}

// Member role gets 404 (server not found in their scope) not 403.
func TestGetMCPServer_API_MemberGets404ForNonExistent(t *testing.T) {
	t.Parallel()

	dsn := "file:TestGetMCPServer_API_Member?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	memberKey := addTestKey(t, keyCache, auth.RoleMember, "org-member-get")

	resp := mcpServerRequest(t, app, http.MethodGet,
		"/api/v1/mcp-servers/00000000-0000-0000-0000-000000000000", memberKey, nil)
	resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404 for member reading non-existent server", resp.StatusCode)
	}
}

// Member role gets 403 when trying to access a global server (scope: system_admin only).
func TestGetMCPServer_API_MemberForbiddenForGlobalServer(t *testing.T) {
	t.Parallel()

	dsn := "file:TestGetMCPServer_API_MemberForbidden?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	memberKey := addTestKey(t, keyCache, auth.RoleMember, "org-member-forbidden")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Global Server",
		Alias:    "global-server",
		URL:      "https://global-server.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create global server: %v", err)
	}

	resp := mcpServerRequest(t, app, http.MethodGet,
		"/api/v1/mcp-servers/"+s.ID, memberKey, nil)
	resp.Body.Close()

	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("status = %d, want 403 for member reading global server", resp.StatusCode)
	}
}

// ---- PATCH /api/v1/mcp-servers/:server_id ----------------------------------

func TestUpdateMCPServer_API(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateMCPServer_API?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-update")

	created := createMCPServerViaAPI(t, app, key, map[string]any{
		"name":      "Original Name",
		"alias":     "original-alias",
		"url":       "https://original.example.com",
		"auth_type": "none",
	})
	serverID := created["id"].(string)

	t.Run("partial update changes name and url", func(t *testing.T) {
		t.Parallel()
		patch := map[string]any{
			"name": "Updated Name",
			"url":  "https://updated.example.com",
		}
		resp := mcpServerRequest(t, app, http.MethodPatch,
			"/api/v1/mcp-servers/"+serverID, key, patch)
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
		}
		got := decodeMCPServerResponse(t, resp.Body)
		if got["name"] != "Updated Name" {
			t.Errorf("name = %v, want %q", got["name"], "Updated Name")
		}
		if got["url"] != "https://updated.example.com" {
			t.Errorf("url = %v, want %q", got["url"], "https://updated.example.com")
		}
	})

	t.Run("invalid alias returns 400", func(t *testing.T) {
		t.Parallel()
		patch := map[string]any{"alias": "INVALID_UPPER"}
		resp := mcpServerRequest(t, app, http.MethodPatch,
			"/api/v1/mcp-servers/"+serverID, key, patch)
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})

	t.Run("reserved alias voidllm returns 400", func(t *testing.T) {
		t.Parallel()
		patch := map[string]any{"alias": "voidllm"}
		resp := mcpServerRequest(t, app, http.MethodPatch,
			"/api/v1/mcp-servers/"+serverID, key, patch)
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusBadRequest {
			t.Errorf("status = %d, want 400", resp.StatusCode)
		}
	})
}

// Org admin can update an org-scoped server in their own org.
func TestUpdateMCPServer_API_OrgAdminCanUpdateOrgScoped(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateMCPServer_OrgAdmin?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)

	org := mustCreateOrg(t, database, "Update Org", "update-org-mcp")
	orgAdminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Org Server",
		Alias:    "org-update-server",
		URL:      "https://org-update.example.com",
		AuthType: "none",
		OrgID:    &org.ID,
	})
	if err != nil {
		t.Fatalf("create org-scoped server: %v", err)
	}

	resp := mcpServerRequest(t, app, http.MethodPatch,
		"/api/v1/mcp-servers/"+s.ID, orgAdminKey,
		map[string]any{"name": "Updated Org Server"})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
}

// Org admin gets 403 when trying to update a global server.
func TestUpdateMCPServer_API_OrgAdminForbiddenForGlobal(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateMCPServer_OrgAdminForbidden?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	orgAdminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "org-forbidden")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Global Server",
		Alias:    "global-forbidden",
		URL:      "https://global-forbidden.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create global server: %v", err)
	}

	resp := mcpServerRequest(t, app, http.MethodPatch,
		"/api/v1/mcp-servers/"+s.ID, orgAdminKey,
		map[string]any{"name": "Should Fail"})
	resp.Body.Close()

	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("status = %d, want 403 for org admin patching global server", resp.StatusCode)
	}
}

// ---- DELETE /api/v1/mcp-servers/:server_id ---------------------------------

func TestDeleteMCPServer_API(t *testing.T) {
	t.Parallel()

	dsn := "file:TestDeleteMCPServer_API?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-delete")

	created := createMCPServerViaAPI(t, app, key, map[string]any{
		"name":      "Delete Me",
		"alias":     "delete-me",
		"url":       "https://delete-me.example.com",
		"auth_type": "none",
	})
	serverID := created["id"].(string)

	// First delete returns 204.
	resp := mcpServerRequest(t, app, http.MethodDelete,
		"/api/v1/mcp-servers/"+serverID, key, nil)
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}

	// Second request for the same server returns 404.
	resp2 := mcpServerRequest(t, app, http.MethodGet,
		"/api/v1/mcp-servers/"+serverID, key, nil)
	resp2.Body.Close()
	if resp2.StatusCode != fiber.StatusNotFound {
		t.Errorf("get after delete status = %d, want 404", resp2.StatusCode)
	}
}

// ---- POST /api/v1/mcp-servers/:server_id/test ------------------------------

func TestTestMCPServerConnection_API(t *testing.T) {
	t.Parallel()

	// Upstream mock: responds with a valid tools/list JSON-RPC response.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[
			{"name":"search","inputSchema":{"type":"object"}},
			{"name":"fetch","inputSchema":{"type":"object"}}
		]}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestTestMCPServerConnection_API?mode=memory&cache=private"
	// Build the app with MCPAllowPrivateURLs=true so the SSRF-safe dialer
	// permits connections to the loopback httptest server. This mirrors a
	// self-hosted deployment where private URLs are explicitly allowed.
	database, keyCache, app := setupMCPServersTestAppAllowPrivate(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-test-conn")

	// Insert directly into the DB to bypass SSRF URL validation — the test
	// server is a localhost httptest server which is only reachable because
	// MCPAllowPrivateURLs is set to true for this test.
	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Test Upstream",
		Alias:    "test-upstream",
		URL:      upstream.URL,
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create test MCP server: %v", err)
	}
	serverID := s.ID

	resp := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/"+serverID+"/test", key, nil)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	if got["success"] != true {
		t.Errorf("success = %v, want true", got["success"])
	}
	if got["tools"].(float64) != 2 {
		t.Errorf("tools = %v, want 2", got["tools"])
	}
}

func TestTestMCPServerConnection_API_NotFound(t *testing.T) {
	t.Parallel()

	dsn := "file:TestTestMCPServerConnection_NotFound?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-test-notfound")

	resp := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/00000000-0000-0000-0000-000000000000/test", key, nil)
	resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// ---- RBAC -------------------------------------------------------------------

// TestMCPServers_RBAC_GlobalWrite verifies that only system_admin can create
// or list global (unscoped) MCP servers.
func TestMCPServers_RBAC_GlobalWrite(t *testing.T) {
	t.Parallel()

	roles := []string{auth.RoleMember, auth.RoleTeamAdmin, auth.RoleOrgAdmin}

	for _, role := range roles {
		role := role
		t.Run("role="+role, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestMCPServers_RBAC_GlobalWrite_%s?mode=memory&cache=private", role)
			app, _, keyCache := setupMCPServersTestApp(t, dsn)
			key := addTestKey(t, keyCache, role, "org-rbac")

			// POST and GET on the global endpoint require system_admin.
			endpoints := []struct {
				method string
				url    string
				body   any
			}{
				{http.MethodPost, "/api/v1/mcp-servers",
					map[string]any{"name": "X", "alias": "x", "url": "https://x.example.com", "auth_type": "none"}},
				{http.MethodGet, "/api/v1/mcp-servers", nil},
			}

			for _, ep := range endpoints {
				resp := mcpServerRequest(t, app, ep.method, ep.url, key, ep.body)
				resp.Body.Close()
				if resp.StatusCode != fiber.StatusForbidden {
					t.Errorf("%s %s with role %q: status = %d, want 403",
						ep.method, ep.url, role, resp.StatusCode)
				}
			}
		})
	}
}

// TestMCPServers_RBAC_SharedRoutes verifies that members can reach the shared
// PATCH/DELETE/test routes but receive 404 (not found) rather than 403 when
// the server does not exist (because the scope check comes after the DB lookup).
func TestMCPServers_RBAC_SharedRoutes_NotFound(t *testing.T) {
	t.Parallel()

	dsn := "file:TestMCPServers_RBAC_SharedRoutes?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	memberKey := addTestKey(t, keyCache, auth.RoleMember, "org-shared-rbac")

	// All shared routes return 404 for a non-existent server_id, not 403.
	endpoints := []struct {
		method string
		url    string
		body   any
	}{
		{http.MethodGet, "/api/v1/mcp-servers/some-id", nil},
		{http.MethodPatch, "/api/v1/mcp-servers/some-id", map[string]any{"name": "Y"}},
		{http.MethodDelete, "/api/v1/mcp-servers/some-id", nil},
		{http.MethodPost, "/api/v1/mcp-servers/some-id/test", nil},
	}

	for _, ep := range endpoints {
		resp := mcpServerRequest(t, app, ep.method, ep.url, memberKey, ep.body)
		resp.Body.Close()
		if resp.StatusCode != fiber.StatusNotFound {
			t.Errorf("%s %s with member role: status = %d, want 404",
				ep.method, ep.url, resp.StatusCode)
		}
	}
}

// ---- Source field -----------------------------------------------------------

func TestCreateMCPServer_SourceIsAPI(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateMCPServer_SourceIsAPI?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-source-api")

	body := map[string]any{
		"name":      "Source Test Server",
		"alias":     "source-test",
		"url":       "https://source-test.example.com",
		"auth_type": "none",
	}

	got := createMCPServerViaAPI(t, app, key, body)

	if got["source"] != "api" {
		t.Errorf("source = %v, want %q (API-created servers must have source=api)", got["source"], "api")
	}
}

func TestCreateMCPServer_CannotSetSourceYAML(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateMCPServer_CannotSetSourceYAML?mode=memory&cache=private"
	app, _, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-source-yaml")

	// Include source="yaml" in the request body — the handler must ignore it.
	body := map[string]any{
		"name":      "YAML Override Test",
		"alias":     "yaml-override",
		"url":       "https://yaml-override.example.com",
		"auth_type": "none",
		"source":    "yaml",
	}

	got := createMCPServerViaAPI(t, app, key, body)

	// Regardless of what the caller sent, source must always be "api".
	if got["source"] != "api" {
		t.Errorf("source = %v, want %q (client cannot set source=yaml via API)", got["source"], "api")
	}
}

// setupMCPServersTestAppWithToolCache builds a Fiber app identical to
// setupMCPServersTestAppAllowPrivate but with a ToolCache wired onto the
// handler. The ToolCache uses a static fetcher that always returns the
// provided tools for any alias, so tests can exercise refresh-tools and
// list-tools handlers without a real upstream MCP server.
func setupMCPServersTestAppWithToolCache(t *testing.T, dsn string, staticTools []mcp.Tool) (*db.DB, *cache.Cache[string, auth.KeyInfo], *fiber.App) {
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

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()

	fetcher := mcp.ToolFetcher(func(_ context.Context, _ string) ([]mcp.Tool, error) {
		return staticTools, nil
	})
	toolCache := mcp.NewToolCache(fetcher, time.Hour)

	handler := &admin.Handler{
		DB:                  database,
		HMACSecret:          testHMACSecret,
		EncryptionKey:       testEncryptionKey,
		KeyCache:            keyCache,
		License:             license.NewHolder(license.Verify("", true)),
		Log:                 slog.New(slog.NewTextHandler(io.Discard, nil)),
		MCPAllowPrivateURLs: true,
		ToolCache:           toolCache,
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return database, keyCache, app
}

// ---- GET /api/v1/mcp-servers/:server_id/blocklist ---------------------------

func TestListMCPServerBlocklist_API(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListMCPServerBlocklist_API?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-blocklist-list")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Blocklist List Server",
		Alias:    "bl-list-server",
		URL:      "https://bl-list.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	for _, toolName := range []string{"dangerous-tool", "secret-exfil"} {
		_, err := database.CreateToolBlocklistEntry(context.Background(), s.ID, toolName, "test reason", "")
		if err != nil {
			t.Fatalf("create blocklist entry %q: %v", toolName, err)
		}
	}

	resp := mcpServerRequest(t, app, http.MethodGet,
		"/api/v1/mcp-servers/"+s.ID+"/blocklist", key, nil)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode blocklist response: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("blocklist length = %d, want 2", len(list))
	}

	names := make(map[string]bool, len(list))
	for _, entry := range list {
		name, ok := entry["tool_name"].(string)
		if !ok {
			t.Errorf("blocklist entry missing tool_name field: %v", entry)
			continue
		}
		names[name] = true
	}
	for _, want := range []string{"dangerous-tool", "secret-exfil"} {
		if !names[want] {
			t.Errorf("blocklist missing expected tool %q", want)
		}
	}
}

func TestListMCPServerBlocklist_Empty(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListMCPServerBlocklist_Empty?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-blocklist-empty")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Empty Blocklist Server",
		Alias:    "bl-empty-server",
		URL:      "https://bl-empty.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	resp := mcpServerRequest(t, app, http.MethodGet,
		"/api/v1/mcp-servers/"+s.ID+"/blocklist", key, nil)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode blocklist response: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("blocklist length = %d, want 0 (empty array)", len(list))
	}
}

// ---- POST /api/v1/mcp-servers/:server_id/blocklist --------------------------

func TestAddMCPServerBlocklist_API(t *testing.T) {
	t.Parallel()

	dsn := "file:TestAddMCPServerBlocklist_API?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-blocklist-add")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Add Blocklist Server",
		Alias:    "bl-add-server",
		URL:      "https://bl-add.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	body := map[string]any{
		"tool_name": "exec-shell",
		"reason":    "arbitrary command execution risk",
	}

	resp := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/"+s.ID+"/blocklist", key, body)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, raw)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	if got["id"] == nil || got["id"] == "" {
		t.Error("id field missing or empty in blocklist entry response")
	}
	if got["tool_name"] != "exec-shell" {
		t.Errorf("tool_name = %v, want %q", got["tool_name"], "exec-shell")
	}
	if got["reason"] != "arbitrary command execution risk" {
		t.Errorf("reason = %v, want %q", got["reason"], "arbitrary command execution risk")
	}
	if got["server_id"] != s.ID {
		t.Errorf("server_id = %v, want %q", got["server_id"], s.ID)
	}
}

func TestAddMCPServerBlocklist_Duplicate(t *testing.T) {
	t.Parallel()

	dsn := "file:TestAddMCPServerBlocklist_Duplicate?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-blocklist-dup")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Dup Blocklist Server",
		Alias:    "bl-dup-server",
		URL:      "https://bl-dup.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	body := map[string]any{"tool_name": "some-tool", "reason": "first add"}

	// First add — must succeed.
	resp := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/"+s.ID+"/blocklist", key, body)
	resp.Body.Close()
	if resp.StatusCode != fiber.StatusCreated {
		t.Fatalf("first add status = %d, want 201", resp.StatusCode)
	}

	// Second add of the same tool — must return 409.
	resp2 := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/"+s.ID+"/blocklist", key, body)
	defer resp2.Body.Close()

	if resp2.StatusCode != fiber.StatusConflict {
		raw, _ := io.ReadAll(resp2.Body)
		t.Errorf("second add status = %d, want 409; body: %s", resp2.StatusCode, raw)
	}
}

func TestAddMCPServerBlocklist_EmptyToolName(t *testing.T) {
	t.Parallel()

	dsn := "file:TestAddMCPServerBlocklist_EmptyToolName?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-blocklist-empty-name")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Empty Name Server",
		Alias:    "bl-empty-name-server",
		URL:      "https://bl-empty-name.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	body := map[string]any{"tool_name": "", "reason": "empty"}

	resp := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/"+s.ID+"/blocklist", key, body)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, raw)
	}
}

func TestAddMCPServerBlocklist_ToolNameTooLong(t *testing.T) {
	t.Parallel()

	dsn := "file:TestAddMCPServerBlocklist_ToolNameTooLong?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-blocklist-too-long")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Too Long Server",
		Alias:    "bl-too-long-server",
		URL:      "https://bl-too-long.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	// 257 characters — one over the 256-character limit.
	body := map[string]any{"tool_name": strings.Repeat("x", 257), "reason": "too long"}

	resp := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/"+s.ID+"/blocklist", key, body)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, raw)
	}
}

// ---- DELETE /api/v1/mcp-servers/:server_id/blocklist ------------------------

func TestRemoveMCPServerBlocklist_API(t *testing.T) {
	t.Parallel()

	dsn := "file:TestRemoveMCPServerBlocklist_API?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-blocklist-remove")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Remove Blocklist Server",
		Alias:    "bl-remove-server",
		URL:      "https://bl-remove.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	// Add the entry via the API first.
	addBody := map[string]any{"tool_name": "to-remove", "reason": "test"}
	addResp := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/"+s.ID+"/blocklist", key, addBody)
	addResp.Body.Close()
	if addResp.StatusCode != fiber.StatusCreated {
		t.Fatalf("add blocklist status = %d, want 201", addResp.StatusCode)
	}

	// Remove via DELETE with query parameter.
	delResp := mcpServerRequest(t, app, http.MethodDelete,
		"/api/v1/mcp-servers/"+s.ID+"/blocklist?tool_name=to-remove", key, nil)
	defer delResp.Body.Close()

	if delResp.StatusCode != fiber.StatusNoContent {
		raw, _ := io.ReadAll(delResp.Body)
		t.Errorf("delete status = %d, want 204; body: %s", delResp.StatusCode, raw)
	}

	// Confirm the entry is gone by listing.
	listResp := mcpServerRequest(t, app, http.MethodGet,
		"/api/v1/mcp-servers/"+s.ID+"/blocklist", key, nil)
	defer listResp.Body.Close()

	var list []map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&list); err != nil {
		t.Fatalf("decode blocklist after remove: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("blocklist length after remove = %d, want 0", len(list))
	}
}

func TestRemoveMCPServerBlocklist_NotFound(t *testing.T) {
	t.Parallel()

	dsn := "file:TestRemoveMCPServerBlocklist_NotFound?mode=memory&cache=private"
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-blocklist-notfound")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "NotFound Blocklist Server",
		Alias:    "bl-notfound-server",
		URL:      "https://bl-notfound.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	resp := mcpServerRequest(t, app, http.MethodDelete,
		"/api/v1/mcp-servers/"+s.ID+"/blocklist?tool_name=does-not-exist", key, nil)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404; body: %s", resp.StatusCode, raw)
	}
}

// ---- POST /api/v1/mcp-servers/:server_id/refresh-tools ----------------------

func TestHandleRefreshMCPServerTools_API(t *testing.T) {
	t.Parallel()

	staticTools := []mcp.Tool{
		{Name: "search", Description: "Search the web"},
		{Name: "fetch", Description: "Fetch a URL"},
		{Name: "write-file", Description: "Write a file"},
	}

	dsn := "file:TestHandleRefreshMCPServerTools_API?mode=memory&cache=private"
	database, keyCache, app := setupMCPServersTestAppWithToolCache(t, dsn, staticTools)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-refresh-tools")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Refresh Tools Server",
		Alias:    "refresh-tools-server",
		URL:      "https://refresh-tools.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	resp := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/"+s.ID+"/refresh-tools", key, nil)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	toolCount, ok := got["tool_count"].(float64)
	if !ok {
		t.Fatalf("tool_count field missing or wrong type: %v", got["tool_count"])
	}
	if int(toolCount) != len(staticTools) {
		t.Errorf("tool_count = %d, want %d", int(toolCount), len(staticTools))
	}
}

func TestHandleRefreshMCPServerTools_CodeModeDisabled(t *testing.T) {
	t.Parallel()

	dsn := "file:TestHandleRefreshMCPServerTools_CodeModeDisabled?mode=memory&cache=private"
	// Use the standard setup — no ToolCache wired in.
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-refresh-disabled")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Disabled Code Mode Server",
		Alias:    "disabled-cm-server",
		URL:      "https://disabled-cm.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	resp := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/"+s.ID+"/refresh-tools", key, nil)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 503; body: %s", resp.StatusCode, raw)
	}
}

func TestHandleRefreshMCPServerTools_Cooldown(t *testing.T) {
	t.Parallel()

	staticTools := []mcp.Tool{{Name: "cooldown-tool"}}

	dsn := "file:TestHandleRefreshMCPServerTools_Cooldown?mode=memory&cache=private"
	database, keyCache, app := setupMCPServersTestAppWithToolCache(t, dsn, staticTools)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-refresh-cooldown")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Cooldown Server",
		Alias:    "cooldown-server",
		URL:      "https://cooldown.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	// First refresh — must succeed.
	resp1 := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/"+s.ID+"/refresh-tools", key, nil)
	resp1.Body.Close()
	if resp1.StatusCode != fiber.StatusOK {
		t.Fatalf("first refresh status = %d, want 200", resp1.StatusCode)
	}

	// Immediate second refresh — must return 429 because the cooldown (60s) has
	// not elapsed since the first refresh completed.
	resp2 := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/"+s.ID+"/refresh-tools", key, nil)
	defer resp2.Body.Close()

	if resp2.StatusCode != fiber.StatusTooManyRequests {
		raw, _ := io.ReadAll(resp2.Body)
		t.Errorf("second refresh status = %d, want 429; body: %s", resp2.StatusCode, raw)
	}
}

// ---- GET /api/v1/mcp-servers/:server_id/tools -------------------------------

func TestHandleListMCPServerTools_API(t *testing.T) {
	t.Parallel()

	staticTools := []mcp.Tool{
		{Name: "allowed-tool", Description: "An allowed tool"},
		{Name: "blocked-tool", Description: "A blocked tool"},
	}

	dsn := "file:TestHandleListMCPServerTools_API?mode=memory&cache=private"
	database, keyCache, app := setupMCPServersTestAppWithToolCache(t, dsn, staticTools)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-list-tools")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "List Tools Server",
		Alias:    "list-tools-server",
		URL:      "https://list-tools.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	// Block one of the tools directly in the DB.
	_, err = database.CreateToolBlocklistEntry(context.Background(), s.ID, "blocked-tool", "test block", "")
	if err != nil {
		t.Fatalf("create blocklist entry: %v", err)
	}

	// Trigger a refresh so the ToolCache has entries for this alias.
	refreshResp := mcpServerRequest(t, app, http.MethodPost,
		"/api/v1/mcp-servers/"+s.ID+"/refresh-tools", key, nil)
	refreshResp.Body.Close()
	if refreshResp.StatusCode != fiber.StatusOK {
		t.Fatalf("refresh status = %d, want 200", refreshResp.StatusCode)
	}

	// Now list tools.
	resp := mcpServerRequest(t, app, http.MethodGet,
		"/api/v1/mcp-servers/"+s.ID+"/tools", key, nil)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	var list []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode tools response: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("tools length = %d, want 2", len(list))
	}

	toolsByName := make(map[string]map[string]any, len(list))
	for _, tool := range list {
		name, ok := tool["name"].(string)
		if !ok {
			t.Errorf("tool entry missing name field: %v", tool)
			continue
		}
		toolsByName[name] = tool
	}

	if allowed, ok := toolsByName["allowed-tool"]; ok {
		if allowed["blocked"] != false {
			t.Errorf("allowed-tool blocked = %v, want false", allowed["blocked"])
		}
	} else {
		t.Error("allowed-tool missing from tools list")
	}

	if blocked, ok := toolsByName["blocked-tool"]; ok {
		if blocked["blocked"] != true {
			t.Errorf("blocked-tool blocked = %v, want true", blocked["blocked"])
		}
	} else {
		t.Error("blocked-tool missing from tools list")
	}
}

func TestHandleListMCPServerTools_CodeModeDisabled(t *testing.T) {
	t.Parallel()

	dsn := "file:TestHandleListMCPServerTools_CodeModeDisabled?mode=memory&cache=private"
	// Use the standard setup — no ToolCache wired in.
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-list-tools-disabled")

	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Disabled CM List Tools Server",
		Alias:    "disabled-cm-list-server",
		URL:      "https://disabled-cm-list.example.com",
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create MCP server: %v", err)
	}

	resp := mcpServerRequest(t, app, http.MethodGet,
		"/api/v1/mcp-servers/"+s.ID+"/tools", key, nil)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusServiceUnavailable {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 503; body: %s", resp.StatusCode, raw)
	}
}
