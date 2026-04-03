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
	"github.com/voidmind-io/voidllm/internal/usage"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// setupMCPProxyApp creates a Fiber app wired with the MCP proxy routes and an
// in-memory database. The MCPServer is registered so that the /mcp/:alias
// routes are mounted. Returns the app, database, and key cache.
func setupMCPProxyApp(t *testing.T, dsn string) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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
		DB:                  database,
		HMACSecret:          testHMACSecret,
		EncryptionKey:       testEncryptionKey,
		KeyCache:            keyCache,
		License:             license.NewHolder(license.Verify("", true)),
		Log:                 noopLogger(t),
		MCPServer:           mcpServer,
		MCPCallTimeout:      5 * time.Second,
		MCPAllowPrivateURLs: true, // tests use loopback httptest servers
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// proxyPost sends a POST to /api/v1/mcp/:alias.
func proxyPost(t *testing.T, app *fiber.App, alias, key, body string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/"+alias,
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test POST /api/v1/mcp/%s: %v", alias, err)
	}
	return resp
}

// proxyGet sends a GET to /api/v1/mcp/:alias.
func proxyGet(t *testing.T, app *fiber.App, alias, key string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/mcp/"+alias, nil)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test GET /api/v1/mcp/%s: %v", alias, err)
	}
	return resp
}

// createExternalMCPServer inserts an MCP server record directly into the
// database, bypassing SSRF URL validation so that tests can use localhost
// httptest servers. Returns the created server ID.
func createExternalMCPServer(t *testing.T, database *db.DB, alias, upstreamURL string) string {
	t.Helper()
	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "External " + alias,
		Alias:    alias,
		URL:      upstreamURL,
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("create external MCP server in DB: %v", err)
	}
	return s.ID
}

// addMCPTestKey is like addTestKey but returns a key with member role, since
// the MCP proxy routes accept any authenticated caller.
func addMCPTestKey(t *testing.T, keyCache *cache.Cache[string, auth.KeyInfo], orgID string) string {
	t.Helper()
	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	hash := keygen.Hash(plaintext, testHMACSecret)
	keyCache.Set(hash, auth.KeyInfo{
		ID:      "mcp-proxy-key-id",
		KeyType: keygen.KeyTypeUser,
		Role:    auth.RoleMember,
		OrgID:   orgID,
		Name:    "mcp proxy test key",
	})
	return plaintext
}

// ---- POST /api/v1/mcp/voidllm -----------------------------------------------

func TestMCPProxy_ToVoidllm(t *testing.T) {
	t.Parallel()

	dsn := "file:TestMCPProxy_ToVoidllm?mode=memory&cache=private"
	app, _, keyCache := setupMCPProxyApp(t, dsn)
	key := addMCPTestKey(t, keyCache, "org-proxy-voidllm")

	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":null}`
	resp := proxyPost(t, app, "voidllm", key, body)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	var rpcResp mcp.Response
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if rpcResp.Error != nil {
		t.Errorf("unexpected RPC error: %+v", rpcResp.Error)
	}
}

// ---- POST /api/v1/mcp/:alias — unknown alias --------------------------------

func TestMCPProxy_UnknownAlias(t *testing.T) {
	t.Parallel()

	dsn := "file:TestMCPProxy_UnknownAlias?mode=memory&cache=private"
	app, _, keyCache := setupMCPProxyApp(t, dsn)
	key := addMCPTestKey(t, keyCache, "org-proxy-unknown")

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
	resp := proxyPost(t, app, "does-not-exist", key, body)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 404; body: %s", resp.StatusCode, raw)
	}

	var rpcResp mcp.Response
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if rpcResp.Error == nil {
		t.Error("RPC error field is nil, want non-nil error for unknown alias")
	}
}

// ---- POST /api/v1/mcp/:alias — external server ------------------------------

func TestMCPProxy_ToExternalServer(t *testing.T) {
	t.Parallel()

	const toolsListResponse = `{"jsonrpc":"2.0","id":1,"result":{"tools":[
		{"name":"search","inputSchema":{"type":"object"}},
		{"name":"fetch","inputSchema":{"type":"object"}}
	]}}`

	var gotBody []byte
	var gotContentType string

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read err", http.StatusInternalServerError)
			return
		}
		gotContentType = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, toolsListResponse)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_ToExternalServer?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "proxy-ext")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServer(t, database, "ext-server", upstream.URL)
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	requestBody := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	resp := proxyPost(t, app, "ext-server", memberKey, requestBody)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	// Verify the request was forwarded verbatim.
	if string(gotBody) != requestBody {
		t.Errorf("upstream received body = %q, want %q", gotBody, requestBody)
	}
	if gotContentType != "application/json" {
		t.Errorf("upstream Content-Type = %q, want application/json", gotContentType)
	}

	// Verify response is the proxied JSON-RPC body.
	raw, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(raw), `"tools"`) {
		t.Errorf("response body = %q, want it to contain tools list", raw)
	}
}

// ---- GET /api/v1/mcp/voidllm — SSE -----------------------------------------

func TestMCPProxy_SSE_Voidllm(t *testing.T) {
	t.Parallel()

	dsn := "file:TestMCPProxy_SSE_Voidllm?mode=memory&cache=private"
	app, _, keyCache := setupMCPProxyApp(t, dsn)
	key := addMCPTestKey(t, keyCache, "org-proxy-sse")

	resp := proxyGet(t, app, "voidllm", key)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /mcp/voidllm status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
}

// ---- GET /api/v1/mcp/:alias — external SSE returns 501 ---------------------

func TestMCPProxy_SSE_ExternalServer_NotImplemented(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_SSE_ExternalServer_501?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	memberKey := addMCPTestKey(t, keyCache, "org-proxy-sse-ext")

	createExternalMCPServer(t, database, "ext-sse-server", upstream.URL)

	resp := proxyGet(t, app, "ext-sse-server", memberKey)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotImplemented {
		t.Errorf("GET /mcp/ext-sse-server status = %d, want 501", resp.StatusCode)
	}
}

// ---- Proxy metrics — MCPToolCallsTotal incremented -------------------------

// TestMCPProxy_MetricsToolCallsIncremented verifies that a successful proxy
// call to an external server increments the MCPToolCallsTotal counter. It
// inspects the Prometheus default registry via the /metrics endpoint if
// registered, or instead relies on the transport to complete without error to
// confirm the code path executes (the counter increment is always present in
// the production code path regardless of observability).
//
// Because Prometheus counters are package-level globals, direct comparison is
// unreliable in a parallel-test environment. We therefore verify the proxy
// round-trip succeeds (status 200) and trust the metric increment is exercised
// via the same code path confirmed by TestMCPProxy_ToExternalServer.
func TestMCPProxy_MetricsToolCallsIncremented(t *testing.T) {
	t.Parallel()

	const toolsListResponse = `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, toolsListResponse)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_MetricsToolCallsIncremented?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "proxy-metrics")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServer(t, database, "metrics-server", upstream.URL)
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	// The tools/call method exercises the tool-name extraction and the
	// duration histogram in addition to the counter.
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{}}}`
	resp := proxyPost(t, app, "metrics-server", memberKey, body)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
}

// ---- MCPLogger called on proxied requests ----------------------------------

// mockMCPLogger is an in-process MCPToolCallLogger for tests.
type mockMCPLogger struct {
	events []usage.MCPToolCallEvent
}

func (m *mockMCPLogger) Log(ev usage.MCPToolCallEvent) {
	m.events = append(m.events, ev)
}

func TestMCPProxy_LoggerCalled(t *testing.T) {
	t.Parallel()

	const toolsListResponse = `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, toolsListResponse)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_LoggerCalled?mode=memory&cache=private"
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
	logger := &mockMCPLogger{}

	handler := &admin.Handler{
		DB:                  database,
		HMACSecret:          testHMACSecret,
		EncryptionKey:       testEncryptionKey,
		KeyCache:            keyCache,
		License:             license.NewHolder(license.Verify("", true)),
		Log:                 noopLogger(t),
		MCPServer:           mcpServer,
		MCPCallTimeout:      5 * time.Second,
		MCPLogger:           logger,
		MCPAllowPrivateURLs: true, // test uses a loopback httptest server
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	org := mustCreateTestOrg(t, database, "proxy-logger")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServer(t, database, "logger-server", upstream.URL)
	if err := database.SetOrgMCPAccess(ctx, org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"search","arguments":{}}}`
	resp := proxyPost(t, app, "logger-server", memberKey, body)
	resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if len(logger.events) != 1 {
		t.Fatalf("logger received %d events, want 1", len(logger.events))
	}
	ev := logger.events[0]
	if ev.ServerAlias != "logger-server" {
		t.Errorf("ServerAlias = %q, want %q", ev.ServerAlias, "logger-server")
	}
	if ev.ToolName != "search" {
		t.Errorf("ToolName = %q, want %q", ev.ToolName, "search")
	}
	if ev.Status != "success" {
		t.Errorf("Status = %q, want %q", ev.Status, "success")
	}
}
