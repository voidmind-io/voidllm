package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"log/slog"
	"time"
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
	app, database, keyCache := setupMCPServersTestApp(t, dsn)
	key := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-test-conn")

	// Insert directly into the DB to bypass SSRF URL validation — the test
	// server is a localhost httptest server which is intentionally blocked by
	// the admin API but valid in the test environment.
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

func TestMCPServers_RBAC_NonSystemAdmin(t *testing.T) {
	t.Parallel()

	roles := []string{auth.RoleMember, auth.RoleTeamAdmin, auth.RoleOrgAdmin}

	for _, role := range roles {
		role := role
		t.Run("role="+role, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestMCPServers_RBAC_%s?mode=memory&cache=private", role)
			app, _, keyCache := setupMCPServersTestApp(t, dsn)
			key := addTestKey(t, keyCache, role, "org-rbac")

			endpoints := []struct {
				method string
				url    string
				body   any
			}{
				{http.MethodPost, "/api/v1/mcp-servers",
					map[string]any{"name": "X", "alias": "x", "url": "https://x.example.com", "auth_type": "none"}},
				{http.MethodGet, "/api/v1/mcp-servers", nil},
				{http.MethodGet, "/api/v1/mcp-servers/some-id", nil},
				{http.MethodPatch, "/api/v1/mcp-servers/some-id", map[string]any{"name": "Y"}},
				{http.MethodDelete, "/api/v1/mcp-servers/some-id", nil},
				{http.MethodPost, "/api/v1/mcp-servers/some-id/test", nil},
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
