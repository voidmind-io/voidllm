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
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

const mcpURL = "/api/v1/mcp/voidllm"

// mcpTestDeps returns a minimal valid VoidLLMDeps for handler-level tests.
func mcpTestDeps() mcp.VoidLLMDeps {
	return mcp.VoidLLMDeps{
		ListModels: func(_ context.Context) ([]map[string]any, error) {
			return []map[string]any{
				{"name": "gpt-4o", "provider": "openai", "type": "chat"},
				{"name": "claude-3", "provider": "anthropic", "type": "chat"},
			}, nil
		},
		ListAvailableModels: func(_ context.Context) ([]map[string]any, error) {
			return []map[string]any{
				{"name": "gpt-4o", "type": "chat"},
				{"name": "claude-3", "type": "chat"},
			}, nil
		},
		GetAllHealth: func() []map[string]any {
			return []map[string]any{
				{"name": "gpt-4o", "status": "healthy", "latency_ms": float64(10)},
			}
		},
		GetHealth: func(key string) (map[string]any, bool) {
			if key == "gpt-4o" {
				return map[string]any{"name": "gpt-4o", "status": "healthy"}, true
			}
			return nil, false
		},
		GetUsage: func(_ context.Context, from, to, groupBy, orgID, keyID string) (any, error) {
			return map[string]any{"rows": []any{}}, nil
		},
		ListKeys: func(_ context.Context, orgID, role string) ([]map[string]any, error) {
			return []map[string]any{
				{"id": "k1", "name": "test-key", "org_id": orgID},
			}, nil
		},
		CreateKey: func(_ context.Context, orgID, userID, name string, expiresIn time.Duration) (map[string]any, error) {
			return map[string]any{"id": "new-k", "key": "vl_uk_plaintext", "name": name}, nil
		},
		ListDeployments: func(_ context.Context, modelID string) ([]map[string]any, error) {
			return []map[string]any{
				{"id": "dep-1", "model_id": modelID, "name": "primary"},
			}, nil
		},
	}
}

// setupTestAppWithMCP creates a Fiber app with the MCP route registered and
// an authenticated key in the cache. Returns the app, the key cache, and the
// plaintext key for use in Authorization headers.
func setupTestAppWithMCP(t *testing.T, dsn string) (*fiber.App, *cache.Cache[string, auth.KeyInfo], string) {
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

	mcpServer := mcp.NewServer("voidllm", "test")
	mcp.RegisterVoidLLMTools(mcpServer, mcpTestDeps())

	handler := &admin.Handler{
		DB:         database,
		HMACSecret: testHMACSecret,
		KeyCache:   keyCache,
		License:    license.NewHolder(license.Verify("", true)),
		Log:        noopLogger(t),
		MCPServer:  mcpServer,
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	// Default test key with member role.
	key := addTestKey(t, keyCache, auth.RoleMember, "org-mcp-test")

	return app, keyCache, key
}

// mcpPost sends a POST to /api/v1/mcp/voidllm with the given raw body and
// Authorization header. Returns the response.
func mcpPost(t *testing.T, app *fiber.App, key, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("POST", mcpURL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}

// decodeMCPResponse reads and decodes the response body as mcp.Response.
func decodeMCPResponse(t *testing.T, body io.ReadCloser) mcp.Response {
	t.Helper()
	defer body.Close()
	var resp mcp.Response
	if err := json.NewDecoder(body).Decode(&resp); err != nil {
		t.Fatalf("decode MCP response: %v", err)
	}
	return resp
}

// mcpRequest builds a JSON-RPC request string with the given method and params.
func mcpRequest(id int, method string, params any) string {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		req["params"] = params
	}
	b, _ := json.Marshal(req)
	return string(b)
}

// mcpNotification builds a JSON-RPC notification string (no id field).
func mcpNotification(method string) string {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
	})
	return string(b)
}

// ---- POST /api/v1/mcp/voidllm — initialize ----------------------------------

func TestMCPHandler_Initialize(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_Initialize?mode=memory&cache=private")

	resp := mcpPost(t, app, key, mcpRequest(1, "initialize", nil))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error != nil {
		t.Fatalf("unexpected error: %+v", mcpResp.Error)
	}

	b, _ := json.Marshal(mcpResp.Result)
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}

	pv, _ := result["protocolVersion"].(string)
	if pv == "" {
		t.Errorf("protocolVersion missing or empty")
	}
	info, _ := result["serverInfo"].(map[string]any)
	if info == nil || info["name"] == nil {
		t.Errorf("serverInfo missing or incomplete: %v", result["serverInfo"])
	}
}

// ---- tools/list -------------------------------------------------------------

func TestMCPHandler_ToolsList(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_ToolsList?mode=memory&cache=private")

	resp := mcpPost(t, app, key, mcpRequest(2, "tools/list", nil))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error != nil {
		t.Fatalf("unexpected error: %+v", mcpResp.Error)
	}

	b, _ := json.Marshal(mcpResp.Result)
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}

	tools, _ := result["tools"].([]any)
	if len(tools) == 0 {
		t.Errorf("expected tools array to be non-empty")
	}
}

// ---- tools/call list_models -------------------------------------------------

func TestMCPHandler_ToolsCall_ListModels(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_ToolsCall_ListModels?mode=memory&cache=private")

	params := map[string]any{
		"name":      "list_models",
		"arguments": map[string]any{},
	}
	resp := mcpPost(t, app, key, mcpRequest(3, "tools/call", params))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", mcpResp.Error)
	}

	b, _ := json.Marshal(mcpResp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if tr.IsError {
		t.Fatalf("tool returned error: %s", tr.Content[0].Text)
	}
	if len(tr.Content) == 0 {
		t.Fatal("empty content in tool result")
	}
	if !strings.Contains(tr.Content[0].Text, "gpt-4o") {
		t.Errorf("expected gpt-4o in result\ngot: %s", tr.Content[0].Text)
	}
}

// ---- notification → 202 Accepted --------------------------------------------

func TestMCPHandler_Notification_Returns202(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_Notification?mode=memory&cache=private")

	resp := mcpPost(t, app, key, mcpNotification("notifications/initialized"))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 202; body: %s", resp.StatusCode, raw)
	}
}

// ---- empty body → parse error -----------------------------------------------

func TestMCPHandler_EmptyBody(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_EmptyBody?mode=memory&cache=private")

	req := httptest.NewRequest("POST", mcpURL, strings.NewReader(""))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 with error body; body: %s", resp.StatusCode, raw)
	}

	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error == nil {
		t.Fatal("expected error in response for empty body, got nil")
	}
	if mcpResp.Error.Code != mcp.CodeParseError {
		t.Errorf("Error.Code = %d, want CodeParseError (%d)", mcpResp.Error.Code, mcp.CodeParseError)
	}
}

// ---- invalid JSON → parse error ---------------------------------------------

func TestMCPHandler_InvalidJSON(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_InvalidJSON?mode=memory&cache=private")

	resp := mcpPost(t, app, key, `{not valid json`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 with error body; body: %s", resp.StatusCode, raw)
	}

	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error == nil {
		t.Fatal("expected error in response for invalid JSON, got nil")
	}
	if mcpResp.Error.Code != mcp.CodeParseError {
		t.Errorf("Error.Code = %d, want CodeParseError (%d)", mcpResp.Error.Code, mcp.CodeParseError)
	}
}

// ---- no auth → 401 ----------------------------------------------------------

func TestMCPHandler_NoAuth(t *testing.T) {
	t.Parallel()

	app, _, _ := setupTestAppWithMCP(t, "file:TestMCPHandler_NoAuth?mode=memory&cache=private")

	req := httptest.NewRequest("POST", mcpURL,
		strings.NewReader(mcpRequest(1, "initialize", nil)))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no Authorization header.

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 401; body: %s", resp.StatusCode, raw)
	}
}

// ---- Content-Type header ----------------------------------------------------

func TestMCPHandler_ContentType(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_ContentType?mode=memory&cache=private")

	resp := mcpPost(t, app, key, mcpRequest(1, "ping", nil))
	defer resp.Body.Close()

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", ct)
	}
}

// ---- KeyIdentity injection --------------------------------------------------

func TestMCPHandler_KeyIdentityInjected(t *testing.T) {
	t.Parallel()

	dsn := "file:TestMCPHandler_KeyIdentityInjected?mode=memory&cache=private"

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

	const wantOrgID = "org-identity-test"
	const wantKeyID = "key-identity-id"
	const wantRole = "org_admin"

	var capturedOrgID, capturedRole string

	deps := mcpTestDeps()
	deps.ListKeys = func(_ context.Context, orgID, role string) ([]map[string]any, error) {
		capturedOrgID = orgID
		capturedRole = role
		return []map[string]any{}, nil
	}

	mcpServer := mcp.NewServer("voidllm", "test")
	mcp.RegisterVoidLLMTools(mcpServer, deps)

	handler := &admin.Handler{
		DB:         database,
		HMACSecret: testHMACSecret,
		KeyCache:   keyCache,
		License:    license.NewHolder(license.Verify("", true)),
		Log:        noopLogger(t),
		MCPServer:  mcpServer,
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	// Register a key with specific org and key ID.
	key := addTestKeyWithIDAndOrg(t, keyCache, wantRole, wantOrgID, wantKeyID)

	params := map[string]any{
		"name":      "list_keys",
		"arguments": map[string]any{},
	}
	resp := mcpPost(t, app, key, mcpRequest(1, "tools/call", params))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	if capturedOrgID != wantOrgID {
		t.Errorf("orgID passed to ListKeys = %q, want %q", capturedOrgID, wantOrgID)
	}
	if capturedRole != wantRole {
		t.Errorf("role passed to ListKeys = %q, want %q", capturedRole, wantRole)
	}
}

// ---- list_models RBAC -------------------------------------------------------

// TestMCPHandler_ListModels_MemberRBAC verifies that a member-role caller sees
// only name and type in list_models output — no strategy, deployment_count, or
// provider fields. The member path uses ListAvailableModels.
func TestMCPHandler_ListModels_MemberRBAC(t *testing.T) {
	t.Parallel()

	dsn := "file:TestMCPHandler_ListModels_MemberRBAC?mode=memory&cache=private"
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

	deps := mcpTestDeps()
	// ListAvailableModels returns name+type only — no strategy or deployment_count.
	deps.ListAvailableModels = func(_ context.Context) ([]map[string]any, error) {
		return []map[string]any{
			{"name": "gpt-4o", "type": "chat"},
			{"name": "claude-3", "type": "chat"},
		}, nil
	}
	// ListModels would expose admin-only fields — must NOT be called for member.
	deps.ListModels = func(_ context.Context) ([]map[string]any, error) {
		return []map[string]any{
			{
				"name":             "gpt-4o",
				"provider":         "openai",
				"type":             "chat",
				"strategy":         "round-robin",
				"deployment_count": float64(3),
			},
		}, nil
	}

	mcpServer := mcp.NewServer("voidllm", "test")
	mcp.RegisterVoidLLMTools(mcpServer, deps)

	handler := &admin.Handler{
		DB:         database,
		HMACSecret: testHMACSecret,
		KeyCache:   keyCache,
		License:    license.NewHolder(license.Verify("", true)),
		Log:        noopLogger(t),
		MCPServer:  mcpServer,
	}
	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	memberKey := addTestKey(t, keyCache, auth.RoleMember, "org-rbac-member")

	params := map[string]any{
		"name":      "list_models",
		"arguments": map[string]any{},
	}
	resp := mcpPost(t, app, memberKey, mcpRequest(1, "tools/call", params))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", mcpResp.Error)
	}

	b, _ := json.Marshal(mcpResp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if tr.IsError {
		t.Fatalf("tool returned error: %s", tr.Content[0].Text)
	}
	text := tr.Content[0].Text
	if strings.Contains(text, "strategy") {
		t.Errorf("strategy must not appear in member output\ngot: %s", text)
	}
	if strings.Contains(text, "deployment_count") {
		t.Errorf("deployment_count must not appear in member output\ngot: %s", text)
	}
	if strings.Contains(text, "openai") {
		t.Errorf("provider info must not appear in member output\ngot: %s", text)
	}
	// Model names should still be visible.
	if !strings.Contains(text, "gpt-4o") {
		t.Errorf("expected gpt-4o in member output\ngot: %s", text)
	}
}

// TestMCPHandler_ListModels_AdminRBAC verifies that a system_admin caller
// receives full model metadata including strategy and deployment_count.
func TestMCPHandler_ListModels_AdminRBAC(t *testing.T) {
	t.Parallel()

	dsn := "file:TestMCPHandler_ListModels_AdminRBAC?mode=memory&cache=private"
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

	deps := mcpTestDeps()
	deps.ListModels = func(_ context.Context) ([]map[string]any, error) {
		return []map[string]any{
			{
				"name":             "gpt-4o",
				"provider":         "openai",
				"type":             "chat",
				"strategy":         "round-robin",
				"deployment_count": float64(2),
			},
		}, nil
	}
	deps.GetAllHealth = func() []map[string]any {
		return []map[string]any{
			{"name": "gpt-4o", "status": "healthy", "latency_ms": float64(10)},
		}
	}

	mcpServer := mcp.NewServer("voidllm", "test")
	mcp.RegisterVoidLLMTools(mcpServer, deps)

	handler := &admin.Handler{
		DB:         database,
		HMACSecret: testHMACSecret,
		KeyCache:   keyCache,
		License:    license.NewHolder(license.Verify("", true)),
		Log:        noopLogger(t),
		MCPServer:  mcpServer,
	}
	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	adminKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-rbac-admin")

	params := map[string]any{
		"name":      "list_models",
		"arguments": map[string]any{},
	}
	resp := mcpPost(t, app, adminKey, mcpRequest(1, "tools/call", params))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", mcpResp.Error)
	}

	b, _ := json.Marshal(mcpResp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if tr.IsError {
		t.Fatalf("tool returned error: %s", tr.Content[0].Text)
	}
	text := tr.Content[0].Text
	if !strings.Contains(text, "strategy") {
		t.Errorf("expected strategy field in admin output\ngot: %s", text)
	}
	if !strings.Contains(text, "round-robin") {
		t.Errorf("expected strategy=round-robin in admin output\ngot: %s", text)
	}
	if !strings.Contains(text, "deployment_count") {
		t.Errorf("expected deployment_count field in admin output\ngot: %s", text)
	}
	if !strings.Contains(text, "openai") {
		t.Errorf("expected provider=openai in admin output\ngot: %s", text)
	}
}

// ---- error sanitization -----------------------------------------------------

// TestMCPHandler_ErrorSanitized verifies that when a tool dep returns an error
// containing internal details, the handler returns a generic "internal error"
// and does not leak the raw error message to the caller.
func TestMCPHandler_ErrorSanitized(t *testing.T) {
	t.Parallel()

	dsn := "file:TestMCPHandler_ErrorSanitized?mode=memory&cache=private"
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

	deps := mcpTestDeps()
	// Inject a dep error containing internal connection details.
	deps.ListKeys = func(_ context.Context, _, _ string) ([]map[string]any, error) {
		return nil, fmt.Errorf("postgres://admin:secret@internal-host:5432/voidllm: connection refused")
	}

	mcpServer := mcp.NewServer("voidllm", "test")
	mcp.RegisterVoidLLMTools(mcpServer, deps)

	handler := &admin.Handler{
		DB:         database,
		HMACSecret: testHMACSecret,
		KeyCache:   keyCache,
		License:    license.NewHolder(license.Verify("", true)),
		Log:        noopLogger(t),
		MCPServer:  mcpServer,
	}
	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	memberKey := addTestKey(t, keyCache, auth.RoleMember, "org-sanitize-test")

	params := map[string]any{
		"name":      "list_keys",
		"arguments": map[string]any{},
	}
	resp := mcpPost(t, app, memberKey, mcpRequest(1, "tools/call", params))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", mcpResp.Error)
	}

	b, _ := json.Marshal(mcpResp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if !tr.IsError {
		t.Errorf("IsError = false; expected dep error to surface as tool-level error")
	}
	if len(tr.Content) == 0 {
		t.Fatal("Content is empty")
	}
	text := tr.Content[0].Text
	if !strings.Contains(text, "internal error") {
		t.Errorf("expected %q to contain %q", text, "internal error")
	}
	if strings.Contains(text, "postgres://") {
		t.Errorf("raw connection string must not be leaked to caller: %q", text)
	}
	if strings.Contains(text, "secret") {
		t.Errorf("credentials must not be leaked to caller: %q", text)
	}
}

// ---- MCP route not registered when MCPServer is nil -------------------------

func TestMCPHandler_RouteNotRegisteredWhenNil(t *testing.T) {
	t.Parallel()

	// setupTestApp (from orgs_test.go) creates a Handler with MCPServer=nil.
	app, _, keyCache := setupTestApp(t, "file:TestMCPHandler_NilRoute?mode=memory&cache=private")
	key := addTestKey(t, keyCache, auth.RoleMember, "org-nil-mcp")

	req := httptest.NewRequest("POST", mcpURL,
		strings.NewReader(mcpRequest(1, "initialize", nil)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	// Route is not registered — Fiber returns 404.
	if resp.StatusCode != fiber.StatusNotFound {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404 (route not registered); body: %s", resp.StatusCode, raw)
	}
}

// ---- Table-driven: various methods ------------------------------------------

func TestMCPHandler_Methods(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantStatus int
		checkResp  func(t *testing.T, resp mcp.Response)
	}{
		{
			name:       "initialize returns protocol version",
			body:       mcpRequest(1, "initialize", nil),
			wantStatus: fiber.StatusOK,
			checkResp: func(t *testing.T, resp mcp.Response) {
				t.Helper()
				if resp.Error != nil {
					t.Fatalf("unexpected error: %+v", resp.Error)
				}
				b, _ := json.Marshal(resp.Result)
				var m map[string]any
				_ = json.Unmarshal(b, &m)
				if m["protocolVersion"] == nil {
					t.Errorf("protocolVersion missing")
				}
			},
		},
		{
			name:       "ping returns empty object",
			body:       mcpRequest(2, "ping", nil),
			wantStatus: fiber.StatusOK,
			checkResp: func(t *testing.T, resp mcp.Response) {
				t.Helper()
				if resp.Error != nil {
					t.Fatalf("unexpected error: %+v", resp.Error)
				}
				b, _ := json.Marshal(resp.Result)
				if string(b) != "{}" {
					t.Errorf("ping result = %s, want {}", b)
				}
			},
		},
		{
			name:       "unknown method returns -32601",
			body:       mcpRequest(3, "no/such/method", nil),
			wantStatus: fiber.StatusOK,
			checkResp: func(t *testing.T, resp mcp.Response) {
				t.Helper()
				if resp.Error == nil {
					t.Fatal("expected error, got nil")
				}
				if resp.Error.Code != mcp.CodeMethodNotFound {
					t.Errorf("Error.Code = %d, want %d", resp.Error.Code, mcp.CodeMethodNotFound)
				}
			},
		},
		{
			name:       "wrong jsonrpc version returns -32600",
			body:       `{"jsonrpc":"1.0","id":4,"method":"ping"}`,
			wantStatus: fiber.StatusOK,
			checkResp: func(t *testing.T, resp mcp.Response) {
				t.Helper()
				if resp.Error == nil {
					t.Fatal("expected error, got nil")
				}
				if resp.Error.Code != mcp.CodeInvalidRequest {
					t.Errorf("Error.Code = %d, want %d", resp.Error.Code, mcp.CodeInvalidRequest)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestMCPHandler_Methods_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, _, key := setupTestAppWithMCP(t, dsn)

			resp := mcpPost(t, app, key, tc.body)
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, raw)
			}

			if tc.checkResp != nil {
				mcpResp := decodeMCPResponse(t, resp.Body)
				tc.checkResp(t, mcpResp)
			}
		})
	}
}

// ---- Helpers ----------------------------------------------------------------

// noopLogger returns a slog.Logger that discards all output.
func noopLogger(_ *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// addTestKeyWithIDAndOrg generates a test key with a specific key ID and org.
// The key ID is embedded directly into the KeyInfo stored in the cache.
func addTestKeyWithIDAndOrg(t *testing.T, keyCache *cache.Cache[string, auth.KeyInfo], role, orgID, keyID string) string {
	t.Helper()

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}

	hash := keygen.Hash(plaintext, testHMACSecret)
	keyCache.Set(hash, auth.KeyInfo{
		ID:      keyID,
		KeyType: keygen.KeyTypeUser,
		Role:    role,
		OrgID:   orgID,
		Name:    "identity-test key",
	})

	return plaintext
}
