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

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
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

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
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
	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
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
	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
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
	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
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

// ---- SSE POST tests ---------------------------------------------------------

// mcpPostSSE sends a POST to /api/v1/mcp/voidllm with Accept: text/event-stream
// and returns the response.
func mcpPostSSE(t *testing.T, app *fiber.App, key, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("POST", mcpURL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test (SSE): %v", err)
	}
	return resp
}

// extractSSEData reads the response body and returns the value of the first
// "data: " line in the SSE stream. It also validates that the stream starts
// with "event: message\n".
func extractSSEData(t *testing.T, body io.ReadCloser) string {
	t.Helper()
	defer body.Close()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read SSE body: %v", err)
	}
	text := string(raw)
	if !strings.HasPrefix(text, "event: message\n") {
		t.Errorf("SSE body does not start with 'event: message\\n'; got: %q", text)
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "data: ") {
			return strings.TrimPrefix(line, "data: ")
		}
	}
	t.Fatalf("no 'data: ' line found in SSE body: %q", text)
	return ""
}

// TestMCPHandler_SSE_PostWithAcceptSSE verifies that a POST carrying
// Accept: text/event-stream wraps the JSON-RPC response in SSE format.
func TestMCPHandler_SSE_PostWithAcceptSSE(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_SSE_PostWithAcceptSSE?mode=memory&cache=private")

	resp := mcpPostSSE(t, app, key, mcpRequest(1, "initialize", nil))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream prefix", ct)
	}

	dataLine := extractSSEData(t, resp.Body)

	var mcpResp mcp.Response
	if err := json.Unmarshal([]byte(dataLine), &mcpResp); err != nil {
		t.Fatalf("data line is not valid JSON: %v; raw: %q", err, dataLine)
	}
	if mcpResp.Error != nil {
		t.Fatalf("unexpected MCP error: %+v", mcpResp.Error)
	}

	b, _ := json.Marshal(mcpResp.Result)
	var result map[string]any
	if err := json.Unmarshal(b, &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	pv, _ := result["protocolVersion"].(string)
	if pv == "" {
		t.Errorf("protocolVersion missing or empty in SSE data payload")
	}
}

// TestMCPHandler_SSE_PostWithAcceptJSON verifies that a POST carrying an
// explicit Accept: application/json header returns a raw JSON body without SSE
// wrapping, preserving existing non-SSE behaviour.
func TestMCPHandler_SSE_PostWithAcceptJSON(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_SSE_PostWithAcceptJSON?mode=memory&cache=private")

	req := httptest.NewRequest("POST", mcpURL, strings.NewReader(mcpRequest(1, "initialize", nil)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json prefix", ct)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	text := string(raw)
	if strings.Contains(text, "event: message") {
		t.Errorf("response must not contain SSE framing; got: %q", text)
	}
	if strings.HasPrefix(text, "data: ") {
		t.Errorf("response must not start with SSE data line; got: %q", text)
	}

	// Body should be valid JSON-RPC.
	var mcpResp mcp.Response
	if err := json.Unmarshal(raw, &mcpResp); err != nil {
		t.Fatalf("body is not valid JSON-RPC: %v; raw: %q", err, text)
	}
	if mcpResp.Error != nil {
		t.Fatalf("unexpected MCP error: %+v", mcpResp.Error)
	}
}

// TestMCPHandler_SSE_PostToolsCallSSE verifies that a tools/call request with
// Accept: text/event-stream returns the tool result wrapped in SSE format.
func TestMCPHandler_SSE_PostToolsCallSSE(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_SSE_PostToolsCallSSE?mode=memory&cache=private")

	params := map[string]any{
		"name":      "list_models",
		"arguments": map[string]any{},
	}
	resp := mcpPostSSE(t, app, key, mcpRequest(2, "tools/call", params))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream prefix", ct)
	}

	dataLine := extractSSEData(t, resp.Body)

	var mcpResp mcp.Response
	if err := json.Unmarshal([]byte(dataLine), &mcpResp); err != nil {
		t.Fatalf("data line is not valid JSON: %v; raw: %q", err, dataLine)
	}
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
		t.Errorf("expected gpt-4o in SSE tool result; got: %s", tr.Content[0].Text)
	}
}

// TestMCPHandler_SSE_NotificationStill202 verifies that a JSON-RPC
// notification (no id) with Accept: text/event-stream still returns 202
// Accepted. Notifications produce no response body so SSE wrapping does not
// apply.
func TestMCPHandler_SSE_NotificationStill202(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_SSE_NotificationStill202?mode=memory&cache=private")

	resp := mcpPostSSE(t, app, key, mcpNotification("notifications/initialized"))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 202; body: %s", resp.StatusCode, raw)
	}
}

// ---- SSE GET tests ----------------------------------------------------------

// TestMCPHandler_SSE_GetOpensStream verifies that GET /api/v1/mcp/voidllm
// returns a text/event-stream response containing the initial endpoint event.
func TestMCPHandler_SSE_GetOpensStream(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_SSE_GetOpensStream?mode=memory&cache=private")

	req := httptest.NewRequest("GET", mcpURL, nil)
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test (GET): %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream prefix", ct)
	}

	// io.ReadAll on a streaming response may return io.ErrUnexpectedEOF when
	// the test framework closes the connection after the initial event is
	// written. Treat partial data with that error as a successful read.
	raw, err := io.ReadAll(resp.Body)
	if err != nil && !strings.Contains(err.Error(), "unexpected EOF") {
		t.Fatalf("read SSE body: %v", err)
	}
	text := string(raw)
	const wantPrefix = "event: endpoint\ndata: /api/v1/mcp/voidllm"
	if !strings.HasPrefix(text, wantPrefix) {
		t.Errorf("SSE body does not start with %q; got: %q", wantPrefix, text)
	}
}

// TestMCPHandler_SSE_GetRequiresAuth verifies that GET /api/v1/mcp/voidllm
// without an Authorization header returns 401.
func TestMCPHandler_SSE_GetRequiresAuth(t *testing.T) {
	t.Parallel()

	app, _, _ := setupTestAppWithMCP(t, "file:TestMCPHandler_SSE_GetRequiresAuth?mode=memory&cache=private")

	req := httptest.NewRequest("GET", mcpURL, nil)
	// Deliberately no Authorization header.

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test (GET no auth): %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 401; body: %s", resp.StatusCode, raw)
	}
}

// TestMCPHandler_SSE_GetHeaders verifies that GET /api/v1/mcp/voidllm sets the
// required SSE proxy headers: Cache-Control: no-cache and X-Accel-Buffering: no.
func TestMCPHandler_SSE_GetHeaders(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_SSE_GetHeaders?mode=memory&cache=private")

	req := httptest.NewRequest("GET", mcpURL, nil)
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test (GET headers): %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}
	if xab := resp.Header.Get("X-Accel-Buffering"); xab != "no" {
		t.Errorf("X-Accel-Buffering = %q, want %q", xab, "no")
	}
}

// ---- Modern-era header validation (MCP Streamable HTTP §4.5) ---------------
//
// These tests cover internal/api/admin/mcp_handler.go's validateMCPHeaders:
// for a request carrying a modern MCP-Protocol-Version, the standard request
// headers (Mcp-Method, Mcp-Name) must be present and match the JSON-RPC body,
// or the request is rejected with HTTP 400 and JSON-RPC CodeHeaderMismatch
// (-32020), per docs/mcp-v2.md §4.5.

// modernToolCallRequest builds a modern-era tools/call JSON-RPC body with a
// params._meta block carrying the MUST/SHOULD identity fields.
func modernToolCallRequest(id int, toolName string) string {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": map[string]any{},
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	}
	b, _ := json.Marshal(req)
	return string(b)
}

// mcpPostModern sends a POST with the given headers set on top of the modern
// MCP-Protocol-Version header, and returns the response.
func mcpPostModern(t *testing.T, app *fiber.App, key, body string, extraHeaders map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest("POST", mcpURL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	return resp
}

// decodeMCPErrorBody decodes a non-2xx JSON-RPC error response body. It does
// NOT close body: every call site already holds its own
// `defer resp.Body.Close()` for the surrounding test function (frequently
// established before this helper is ever called, so other assertions in the
// same test can still read resp.Body's headers/status afterward) — closing
// it here too would close the same body twice. Closing is therefore the
// caller's responsibility alone, exactly once, per this repo's "every test
// closes its response bodies exactly once" convention.
func decodeMCPErrorBody(t *testing.T, body io.ReadCloser) mcp.Response {
	t.Helper()
	var resp mcp.Response
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode error body: %v; raw: %s", err, raw)
	}
	return resp
}

// ---- Era leak: dispatch is keyed on the negotiated dialect, never on body --
// ---- content (FIX 5, docs/mcp-v2.md §4.5, §4.6) -----------------------------

// modernMetaRequest builds a JSON-RPC request whose params._meta carries
// exactly the given protocolVersion (and an empty clientCapabilities object,
// so requests are otherwise well-formed).
func modernMetaRequest(id int, method, protocolVersion string) string {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    protocolVersion,
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	}
	b, _ := json.Marshal(req)
	return string(b)
}

// TestMCPHandler_ModernHeaderMismatch_ProtocolVersionVsBody verifies the era
// leak this whole revision guards against: a request whose MCP-Protocol-Version
// header names the modern revision but whose body's
// params._meta["io.modelcontextprotocol/protocolVersion"] names a legacy one
// must NOT be silently served in either era — it is rejected outright with
// HTTP 400 and JSON-RPC CodeHeaderMismatch (-32020), so that an intermediary
// routing on the header and VoidLLM executing the body can never disagree
// about which era served the request (docs/mcp-v2.md §4.5).
func TestMCPHandler_ModernHeaderMismatch_ProtocolVersionVsBody(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_ModernHeaderMismatch_ProtocolVersionVsBody?mode=memory&cache=private")

	// mcpPostModern always sets the header to "2026-07-28"; the body claims
	// the legacy "2025-03-26" instead.
	body := modernMetaRequest(1, "tools/list", "2025-03-26")
	resp := mcpPostModern(t, app, key, body, map[string]string{"Mcp-Method": "tools/list"})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPErrorBody(t, resp.Body)
	if mcpResp.Error == nil {
		t.Fatal("expected JSON-RPC error, got nil")
	}
	if mcpResp.Error.Code != mcp.CodeHeaderMismatch {
		t.Errorf("Error.Code = %d, want %d (CodeHeaderMismatch)", mcpResp.Error.Code, mcp.CodeHeaderMismatch)
	}
}

// TestMCPHandler_ModernHeaderMatchesBody_Accepted verifies the positive
// counterpart: header and body naming the SAME modern protocol version is not
// a mismatch and the request proceeds normally.
func TestMCPHandler_ModernHeaderMatchesBody_Accepted(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_ModernHeaderMatchesBody_Accepted?mode=memory&cache=private")

	body := modernMetaRequest(1, "tools/list", "2026-07-28")
	resp := mcpPostModern(t, app, key, body, map[string]string{"Mcp-Method": "tools/list"})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", mcpResp.Error)
	}
}

// TestMCPHandler_ModernBodyClaimsVersion_MissingHeader verifies the other
// direction of the same interop bug (docs/mcp-v2.md §4.5): a body whose
// params._meta claims a modern protocol version, but with NO
// MCP-Protocol-Version header at all, must not be silently served as the
// legacy default (Negotiate's rule 4) — it is rejected with HTTP 400 and
// CodeHeaderMismatch, since MCP-Protocol-Version is a MUST header for every
// modern request.
func TestMCPHandler_ModernBodyClaimsVersion_MissingHeader(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_ModernBodyClaimsVersion_MissingHeader?mode=memory&cache=private")

	body := modernMetaRequest(1, "tools/list", "2026-07-28")
	req := httptest.NewRequest("POST", mcpURL, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	// Deliberately no MCP-Protocol-Version header.

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPErrorBody(t, resp.Body)
	if mcpResp.Error == nil {
		t.Fatal("expected JSON-RPC error, got nil")
	}
	if mcpResp.Error.Code != mcp.CodeHeaderMismatch {
		t.Errorf("Error.Code = %d, want %d (CodeHeaderMismatch)", mcpResp.Error.Code, mcp.CodeHeaderMismatch)
	}
}

// TestMCPHandler_LegacyInitializeWithModernBodyProtocolVersion_StaysLegacy is
// the HTTP-level counterpart of
// internal/mcp/server_test.go's
// TestServer_EraLeak_LegacyInitializeWithModernBodyProtocolVersion_StaysLegacy
// — the single most important regression test of the whole dual-era rework.
// A legacy "initialize" handshake with NO MCP-Protocol-Version header, but
// whose params.protocolVersion (a top-level legacy field, NOT
// params._meta["io.modelcontextprotocol/protocolVersion"]) happens to name
// the modern 2026-07-28 revision, must still be served a normal, valid
// legacy initialize response — never CodeMethodNotFound. Since the
// top-level params.protocolVersion field is not the modern _meta location
// validateMCPHeaders inspects, this request is not even flagged as a header
// mismatch: it must sail straight through to Server.Handle, which itself
// must never let the body value switch dispatch to the modern era.
func TestMCPHandler_LegacyInitializeWithModernBodyProtocolVersion_StaysLegacy(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_LegacyInitializeWithModernBodyProtocolVersion_StaysLegacy?mode=memory&cache=private")

	body := mcpRequest(1, "initialize", map[string]any{"protocolVersion": "2026-07-28"})
	resp := mcpPost(t, app, key, body)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error != nil {
		t.Fatalf("unexpected protocol error (a leaked era would report initialize as CodeMethodNotFound "+
			"since the modern vocabulary has no \"initialize\" method): %+v", mcpResp.Error)
	}

	b, _ := json.Marshal(mcpResp.Result)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if m["protocolVersion"] != "2025-03-26" {
		t.Errorf("protocolVersion = %v, want %q — a body-claimed modern version must NOT leak the era",
			m["protocolVersion"], "2025-03-26")
	}
	if _, ok := m["resultType"]; ok {
		t.Errorf("legacy result must not carry \"resultType\" (a modern-era field), got: %v", m)
	}
}

// TestMCPHandler_ModernHeaderMismatch_McpNameVsBody verifies that a modern
// request whose Mcp-Name header names a different tool than params.name is
// rejected with HTTP 400 and JSON-RPC CodeHeaderMismatch (-32020).
func TestMCPHandler_ModernHeaderMismatch_McpNameVsBody(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_ModernHeaderMismatch_McpNameVsBody?mode=memory&cache=private")

	body := modernToolCallRequest(1, "list_models")
	resp := mcpPostModern(t, app, key, body, map[string]string{
		"Mcp-Method": "tools/call",
		"Mcp-Name":   "get_model_health", // deliberately does not match params.name
	})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPErrorBody(t, resp.Body)
	if mcpResp.Error == nil {
		t.Fatal("expected JSON-RPC error, got nil")
	}
	if mcpResp.Error.Code != mcp.CodeHeaderMismatch {
		t.Errorf("Error.Code = %d, want %d (CodeHeaderMismatch)", mcpResp.Error.Code, mcp.CodeHeaderMismatch)
	}
}

// TestMCPHandler_ModernMissingRequiredHeader verifies that a modern request
// missing a header the spec requires (Mcp-Method for every request; Mcp-Name
// additionally for tools/call) is rejected with 400 + CodeHeaderMismatch.
func TestMCPHandler_ModernMissingRequiredHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		body    string
		headers map[string]string
	}{
		{
			name:    "missing Mcp-Method entirely",
			body:    modernToolCallRequest(1, "list_models"),
			headers: map[string]string{"Mcp-Name": "list_models"},
		},
		{
			name: "tools/call missing Mcp-Name",
			body: modernToolCallRequest(2, "list_models"),
			headers: map[string]string{
				"Mcp-Method": "tools/call",
				// Mcp-Name deliberately omitted.
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestMCPHandler_ModernMissingRequiredHeader_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, _, key := setupTestAppWithMCP(t, dsn)

			resp := mcpPostModern(t, app, key, tc.body, tc.headers)
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusBadRequest {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, raw)
			}
			mcpResp := decodeMCPErrorBody(t, resp.Body)
			if mcpResp.Error == nil {
				t.Fatal("expected JSON-RPC error, got nil")
			}
			if mcpResp.Error.Code != mcp.CodeHeaderMismatch {
				t.Errorf("Error.Code = %d, want %d (CodeHeaderMismatch)", mcpResp.Error.Code, mcp.CodeHeaderMismatch)
			}
		})
	}
}

// TestMCPHandler_ModernBase64SentinelHeaderDecoded verifies that a header
// value wrapped in the =?base64?...?= sentinel (docs/mcp-v2.md §4.4) is
// decoded before comparison against the body, so a request is accepted when
// the decoded value matches even though the raw header bytes do not look like
// the tool name.
func TestMCPHandler_ModernBase64SentinelHeaderDecoded(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_ModernBase64SentinelHeaderDecoded?mode=memory&cache=private")

	body := modernToolCallRequest(1, "list_models")
	// base64("list_models") = bGlzdF9tb2RlbHM=
	resp := mcpPostModern(t, app, key, body, map[string]string{
		"Mcp-Method": "tools/call",
		"Mcp-Name":   "=?base64?bGlzdF9tb2RlbHM=?=",
	})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (sentinel should decode to a matching value); body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", mcpResp.Error)
	}
}

// TestMCPHandler_ModernBase64SentinelHeaderDecoded_MismatchStillRejected
// verifies that base64-sentinel decoding does not weaken the comparison: a
// sentinel-encoded value that decodes to something OTHER than the body's tool
// name is still a header mismatch.
func TestMCPHandler_ModernBase64SentinelHeaderDecoded_MismatchStillRejected(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_ModernBase64SentinelHeaderDecoded_MismatchStillRejected?mode=memory&cache=private")

	body := modernToolCallRequest(1, "list_models")
	// base64("get_model_health") decodes to a DIFFERENT tool name.
	resp := mcpPostModern(t, app, key, body, map[string]string{
		"Mcp-Method": "tools/call",
		"Mcp-Name":   "=?base64?Z2V0X21vZGVsX2hlYWx0aA==?=",
	})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPErrorBody(t, resp.Body)
	if mcpResp.Error == nil || mcpResp.Error.Code != mcp.CodeHeaderMismatch {
		t.Errorf("Error = %+v, want CodeHeaderMismatch (%d)", mcpResp.Error, mcp.CodeHeaderMismatch)
	}
}

// TestMCPHandler_ModernValidHeaders_Accepted verifies the positive case: a
// modern request with all required headers present and matching the body
// succeeds exactly like the equivalent legacy request would.
func TestMCPHandler_ModernValidHeaders_Accepted(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_ModernValidHeaders_Accepted?mode=memory&cache=private")

	body := modernToolCallRequest(1, "list_models")
	resp := mcpPostModern(t, app, key, body, map[string]string{
		"Mcp-Method": "tools/call",
		"Mcp-Name":   "list_models",
	})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error != nil {
		t.Fatalf("unexpected protocol error: %+v", mcpResp.Error)
	}

	// Modern-era results carry resultType: "complete" (docs/mcp-v2.md §3.8) —
	// confirms the request actually took the modern dialect, not a legacy
	// fallback.
	b, _ := json.Marshal(mcpResp.Result)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if m["resultType"] != "complete" {
		t.Errorf("resultType = %v, want %q", m["resultType"], "complete")
	}
}

// TestMCPHandler_GET_ModernHeader_405 verifies that GET /api/v1/mcp/voidllm
// carrying a modern MCP-Protocol-Version is rejected with 405 Method Not
// Allowed: the 2026-07-28 revision has no GET-based transport.
func TestMCPHandler_GET_ModernHeader_405(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_GET_ModernHeader_405?mode=memory&cache=private")

	req := httptest.NewRequest("GET", mcpURL, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusMethodNotAllowed {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 405; body: %s", resp.StatusCode, raw)
	}
}

// TestMCPHandler_GET_LegacyHeader_StillSSE verifies that GET carrying an
// explicit LEGACY MCP-Protocol-Version (not merely an absent header) is still
// served as SSE — only a modern header triggers 405. This is the important
// regression guard alongside TestMCPHandler_SSE_GetOpensStream (which covers
// the fully headerless legacy case): legacy SSE-only clients must keep
// working exactly as before this revision, in every legacy shape they might
// send.
func TestMCPHandler_GET_LegacyHeader_StillSSE(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_GET_LegacyHeader_StillSSE?mode=memory&cache=private")

	req := httptest.NewRequest("GET", mcpURL, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("MCP-Protocol-Version", "2025-06-18")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream prefix", ct)
	}
}

// ---- HTTP status code mapping (FIX 2, docs/mcp-v2.md §4.5, §4.6) -----------

// TestMCPHandler_UnsupportedProtocolVersion_Returns400 verifies that an
// unrecognized MCP-Protocol-Version header (CodeUnsupportedProtocolVersion,
// -32022) is rejected with HTTP 400 — so a dual-era client probing this
// server recognizes it as modern from the status code and body, and retries
// with one of the announced supported versions instead of falling back to a
// legacy handshake (docs/mcp-v2.md §4.6).
func TestMCPHandler_UnsupportedProtocolVersion_Returns400(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_UnsupportedProtocolVersion_Returns400?mode=memory&cache=private")

	req := httptest.NewRequest("POST", mcpURL, strings.NewReader(mcpRequest(1, "tools/list", nil)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("MCP-Protocol-Version", "1900-01-01")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPErrorBody(t, resp.Body)
	if mcpResp.Error == nil {
		t.Fatal("expected JSON-RPC error, got nil")
	}
	if mcpResp.Error.Code != mcp.CodeUnsupportedProtocolVersion {
		t.Errorf("Error.Code = %d, want %d (CodeUnsupportedProtocolVersion)", mcpResp.Error.Code, mcp.CodeUnsupportedProtocolVersion)
	}
}

// TestMCPHandler_ModernUnknownMethod_Returns404 verifies that an unknown
// method under the MODERN era is rejected with HTTP 404 (in addition to
// JSON-RPC CodeMethodNotFound in the body): the JSON-RPC body is what lets a
// dual-era client distinguish this from the 404 a legacy-only server would
// return for not hosting the modern endpoint at all (docs/mcp-v2.md §4.6).
func TestMCPHandler_ModernUnknownMethod_Returns404(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_ModernUnknownMethod_Returns404?mode=memory&cache=private")

	body := modernMetaRequest(1, "no/such/method", "2026-07-28")
	resp := mcpPostModern(t, app, key, body, map[string]string{"Mcp-Method": "no/such/method"})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404; body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPErrorBody(t, resp.Body)
	if mcpResp.Error == nil {
		t.Fatal("expected JSON-RPC error, got nil")
	}
	if mcpResp.Error.Code != mcp.CodeMethodNotFound {
		t.Errorf("Error.Code = %d, want %d (CodeMethodNotFound)", mcpResp.Error.Code, mcp.CodeMethodNotFound)
	}
}

// TestMCPHandler_LegacyMethodNotFound_MustStayHTTP200_NotHTTP404 is the
// highest-priority regression test for FIX 2's status code mapping: an
// unknown method under the LEGACY era (no MCP-Protocol-Version header, or one
// naming a pre-2026-07-28 revision) MUST keep responding HTTP 200 with the
// JSON-RPC error in the body — NEVER HTTP 404. A legacy client that already
// knows this endpoint exists would read a 404 as "wrong URL" and fall back to
// the deprecated HTTP+SSE transport, breaking every legacy integration that
// predates the modern era's own distinct 404 behavior (docs/mcp-v2.md §4.6,
// see HintMethodNotFound's doc in internal/mcp/server.go).
func TestMCPHandler_LegacyMethodNotFound_MustStayHTTP200_NotHTTP404(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_LegacyMethodNotFound_MustStayHTTP200_NotHTTP404?mode=memory&cache=private")

	resp := mcpPost(t, app, key, mcpRequest(1, "no/such/method", nil))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (NOT 404 — see test doc comment); body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error == nil {
		t.Fatal("expected JSON-RPC error, got nil")
	}
	if mcpResp.Error.Code != mcp.CodeMethodNotFound {
		t.Errorf("Error.Code = %d, want %d (CodeMethodNotFound)", mcpResp.Error.Code, mcp.CodeMethodNotFound)
	}
}

// ---- Strict required-field validation (docs/mcp-v2.md §3.2) ----------------
//
// dialect2026.Decode rejects any modern-era request whose params._meta omits
// io.modelcontextprotocol/protocolVersion or io.modelcontextprotocol/clientCapabilities
// with CodeInvalidParams (-32602) and an explicit HTTP-400 hint — see
// internal/mcp's TestServer_Handle_Modern_RequiredMetaFieldValidation for the
// exhaustive table of accepted/rejected _meta shapes at the Server.Handle
// level. The tests below cover the property that table cannot: the HTTP
// status code distinction this handler is responsible for producing, and the
// legacy compatibility guarantee that a request with no _meta at all must
// never even reach this stricter validation in the first place.

// TestMCPHandler_Modern_MissingRequiredMetaFields_Returns400 verifies that a
// modern-era request whose body carries no params._meta at all — so it is
// missing BOTH MUST fields — is rejected with HTTP 400, not HTTP 200: this is
// the HTTP-status half of docs/mcp-v2.md §3.2's verbatim requirement, and the
// exact distinction Error.Hint (dialect_2026.go) exists to carry, since an
// ordinary CodeInvalidParams otherwise keeps the HTTP 200 JSON-RPC-error
// convention (see TestMCPHandler_Modern_OrdinaryInvalidParams_StaysHTTP200,
// its direct counterpart).
func TestMCPHandler_Modern_MissingRequiredMetaFields_Returns400(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_Modern_MissingRequiredMetaFields_Returns400?mode=memory&cache=private")

	// No params._meta at all — validateMCPHeaders itself does not require one
	// (it only cross-checks headers against whatever _meta IS present), so
	// this body sails past header validation and reaches Server.Handle, where
	// dialect2026.Decode is the layer that must reject it.
	body := mcpRequest(1, "tools/list", nil)
	resp := mcpPostModern(t, app, key, body, map[string]string{"Mcp-Method": "tools/list"})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPErrorBody(t, resp.Body)
	if mcpResp.Error == nil {
		t.Fatal("expected JSON-RPC error, got nil")
	}
	if mcpResp.Error.Code != mcp.CodeInvalidParams {
		t.Errorf("Error.Code = %d, want %d (CodeInvalidParams)", mcpResp.Error.Code, mcp.CodeInvalidParams)
	}
}

// TestMCPHandler_Modern_OrdinaryInvalidParams_StaysHTTP200 verifies the other
// half of the same distinction: an ORDINARY CodeInvalidParams — one that has
// nothing to do with a missing MUST _meta field, here a tools/call naming a
// tool that does not exist — must keep the usual HTTP 200 JSON-RPC-error
// convention, exactly as it would under the legacy era. Both this test and
// TestMCPHandler_Modern_MissingRequiredMetaFields_Returns400 share the same
// JSON-RPC error code (-32602); only Error.Hint, set explicitly by
// dialect2026.Decode for the missing-required-field case, tells them apart.
func TestMCPHandler_Modern_OrdinaryInvalidParams_StaysHTTP200(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_Modern_OrdinaryInvalidParams_StaysHTTP200?mode=memory&cache=private")

	const unknownTool = "definitely_not_a_real_tool"
	body := modernToolCallRequest(1, unknownTool)
	resp := mcpPostModern(t, app, key, body, map[string]string{
		"Mcp-Method": "tools/call",
		"Mcp-Name":   unknownTool,
	})
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (an ordinary CodeInvalidParams unrelated to a missing _meta "+
			"field must NOT be reported as HTTP 400); body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error == nil {
		t.Fatal("expected JSON-RPC error, got nil")
	}
	if mcpResp.Error.Code != mcp.CodeInvalidParams {
		t.Errorf("Error.Code = %d, want %d (CodeInvalidParams)", mcpResp.Error.Code, mcp.CodeInvalidParams)
	}
}

// TestMCPHandler_LegacyRequestWithNoMeta_CompatibilityGuarantee is a
// regression/compatibility guarantee test, named explicitly as such: a
// legacy request — no MCP-Protocol-Version header, no params._meta at all —
// must keep working completely unchanged by the 2026-07-28 strict
// required-field validation added elsewhere in this file. Negotiate's rule 4
// (negotiate.go) defaults such a request to V20250326 (EraLegacy), so it
// never reaches dialect2026.Decode — and therefore never trips the
// missing-_meta rejection — regardless of how strict that dialect has
// become. If this test ever starts failing, the legacy fallback path itself
// has regressed, not merely the modern-era validation this file is mostly
// about.
func TestMCPHandler_LegacyRequestWithNoMeta_CompatibilityGuarantee(t *testing.T) {
	t.Parallel()

	app, _, key := setupTestAppWithMCP(t, "file:TestMCPHandler_LegacyRequestWithNoMeta_CompatibilityGuarantee?mode=memory&cache=private")

	// mcpPost sets no MCP-Protocol-Version header at all, and mcpRequest emits
	// no params._meta — the plainest possible legacy shape.
	resp := mcpPost(t, app, key, mcpRequest(1, "tools/list", nil))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (legacy compatibility guarantee); body: %s", resp.StatusCode, raw)
	}
	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error != nil {
		t.Fatalf("unexpected protocol error (a legacy request with no _meta at all must never be rejected "+
			"by the modern dialect's required-field validation): %+v", mcpResp.Error)
	}
}

// ---- Compatibility matrix (docs/mcp-v2.md §4.7, server role only) -----------
//
// Phase 1 gives VoidLLM only the server role of the matrix: a legacy client
// and a modern client must both be able to reach our dual-era server, each
// getting back the wire format their own era expects. The client-role rows
// (VoidLLM as an outbound MCP client probing an upstream server) are Phase 2
// (probe.go) and out of scope here.

// TestMCPHandler_CompatibilityMatrix_ServerRole verifies both matrix rows that
// concern VoidLLM as the server: "Legacy client, our (dual-era) server" and
// "Modern client, our (dual-era) server" both succeed, each in its own era's
// wire format.
func TestMCPHandler_CompatibilityMatrix_ServerRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		buildReq  func(key string) *http.Request
		checkWire func(t *testing.T, m map[string]any)
	}{
		{
			name: "legacy client (initialize handshake, no MCP-Protocol-Version header) reaches our dual-era server",
			buildReq: func(key string) *http.Request {
				req := httptest.NewRequest("POST", mcpURL, strings.NewReader(mcpRequest(1, "initialize", nil)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+key)
				return req
			},
			checkWire: func(t *testing.T, m map[string]any) {
				t.Helper()
				// Legacy wire format: protocolVersion/capabilities/serverInfo at
				// the top level, no resultType/ttlMs/cacheScope wrapper.
				if m["protocolVersion"] == nil {
					t.Errorf("legacy result missing protocolVersion: %v", m)
				}
				if _, ok := m["resultType"]; ok {
					t.Errorf("legacy result must not carry resultType: %v", m)
				}
			},
		},
		{
			name: "modern client (per-request _meta, MCP-Protocol-Version header) reaches our dual-era server",
			buildReq: func(key string) *http.Request {
				params := map[string]any{
					"_meta": map[string]any{
						"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
						"io.modelcontextprotocol/clientCapabilities": map[string]any{},
					},
				}
				req := httptest.NewRequest("POST", mcpURL, strings.NewReader(mcpRequest(1, "tools/list", params)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+key)
				req.Header.Set("MCP-Protocol-Version", "2026-07-28")
				req.Header.Set("Mcp-Method", "tools/list")
				return req
			},
			checkWire: func(t *testing.T, m map[string]any) {
				t.Helper()
				// Modern wire format: resultType/cacheScope/_meta.serverInfo
				// present, no bare protocolVersion field.
				if m["resultType"] != "complete" {
					t.Errorf("modern result resultType = %v, want \"complete\"", m["resultType"])
				}
				if m["cacheScope"] != "private" {
					t.Errorf("modern tools/list cacheScope = %v, want \"private\"", m["cacheScope"])
				}
				meta, _ := m["_meta"].(map[string]any)
				if meta == nil || meta["io.modelcontextprotocol/serverInfo"] == nil {
					t.Errorf("modern result missing _meta.serverInfo: %v", m)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestMCPHandler_CompatibilityMatrix_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, _, key := setupTestAppWithMCP(t, dsn)

			resp, err := app.Test(tc.buildReq(key), fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
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
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			tc.checkWire(t, m)
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
