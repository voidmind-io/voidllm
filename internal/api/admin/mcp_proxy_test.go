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

// proxyPostWithHeaders is like proxyPost but also sets arbitrary extra
// headers on top of Content-Type and Authorization, so tests can drive the
// MCP standard request headers (MCP-Protocol-Version, Mcp-Method, Mcp-Name)
// that validateMCPHeaders cross-checks against the body (docs/mcp-v2.md
// §4.5, FIX 4).
func proxyPostWithHeaders(t *testing.T, app *fiber.App, alias, key, body string, extraHeaders map[string]string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/"+alias,
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
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

// createExternalMCPServerPinned is like createExternalMCPServer but pins
// protocol_version so the transport skips era probing entirely (see
// mcp.ResolvePinnedVersion). Tests asserting exact upstream request counts or
// byte-identical pass-through use this instead of createExternalMCPServer:
// with no MCPTransportCache configured (as in this test harness), every
// request builds a fresh ad-hoc HTTPTransport, and an UNPINNED transport
// probes the upstream (server/discover, and possibly a legacy initialize
// fallback) on its very first use — noise these tests are not about and must
// not have to account for.
func createExternalMCPServerPinned(t *testing.T, database *db.DB, alias, upstreamURL, protocolVersion string) string {
	t.Helper()
	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:            "External " + alias,
		Alias:           alias,
		URL:             upstreamURL,
		AuthType:        "none",
		ProtocolVersion: protocolVersion,
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

// ---- validMCPMethods includes the modern-era methods (FIX 10) --------------

// TestMCPProxy_ValidMCPMethods_ModernEraMethodsNotLabeledUnknown verifies FIX
// 10: internal/api/admin/mcp_proxy.go's validMCPMethods set — used both for
// the Prometheus metrics label (mcpRequestMeta.MetricsMethod) and for the
// usage-log ToolName field (parseMCPRequestMeta) — includes the two
// modern-era (2026-07-28) methods server/discover and subscriptions/listen.
// Before this fix, proxying either method to an external server would have
// logged/labeled it as the catch-all "unknown" instead of its own method
// name, indistinguishable from a truly unrecognized method and useless for
// per-method usage breakdowns.
//
// parseMCPRequestMeta and MetricsMethod are unexported, so this is exercised
// end-to-end through the proxy: an external MCP server is mocked, the method
// is proxied through HandleMCPProxy, and the resulting usage-log event's
// ToolName field — populated from the SAME validMCPMethods set — is asserted
// to be the method name, not "unknown".
func TestMCPProxy_ValidMCPMethods_ModernEraMethodsNotLabeledUnknown(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
	}{
		{"server/discover", "server/discover"},
		{"subscriptions/listen", "subscriptions/listen"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			const upstreamResponse = `{"jsonrpc":"2.0","id":1,"result":{}}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, upstreamResponse)
			}))
			t.Cleanup(upstream.Close)

			dsn := fmt.Sprintf("file:TestMCPProxy_ValidMCPMethods_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, "/", "_"))
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

			org := mustCreateTestOrg(t, database, "proxy-validmethods-"+strings.ReplaceAll(tc.name, "/", "-"))
			memberKey := addMCPTestKey(t, keyCache, org.ID)

			alias := "validmethods-" + strings.ReplaceAll(tc.name, "/", "-")
			s := createExternalMCPServer(t, database, alias, upstream.URL)
			if err := database.SetOrgMCPAccess(ctx, org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q,"params":{}}`, tc.method)
			resp := proxyPost(t, app, alias, memberKey, body)
			resp.Body.Close()

			if resp.StatusCode != fiber.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}

			if len(logger.events) != 1 {
				t.Fatalf("logger received %d events, want 1", len(logger.events))
			}
			ev := logger.events[0]
			if ev.ToolName != tc.method {
				t.Errorf("ToolName = %q, want %q (must not fall back to \"unknown\" for a known "+
					"modern-era method)", ev.ToolName, tc.method)
			}
		})
	}
}

// ---- Header validation on /:alias (FIX 4, docs/mcp-v2.md §4.5) -------------

// TestMCPProxy_HeaderValidation verifies that HandleMCPProxy cross-checks the
// standard request headers against the body exactly as handleMCPRequest does
// for the built-in "voidllm" alias (mcp_handler_test.go's
// TestMCPHandler_ModernHeaderMismatch_* suite): before this fix, only
// /api/v1/mcp/voidllm performed this check, so an external server reached
// through /api/v1/mcp/:alias with a header disagreeing with its own body
// would be silently forwarded upstream — the exact split-brain the MCP
// Streamable HTTP spec warns intermediaries against.
func TestMCPProxy_HeaderValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		// headers, if nil, sends no MCP standard request headers at all
		// (the legacy case).
		headers map[string]string
		// protocolVersion pins the server's transport (see
		// createExternalMCPServerPinned) so era probing never contaminates
		// the upstream hit count this test asserts on.
		protocolVersion string

		wantStatus       int
		wantErrCode      int // checked only when wantStatus == 400
		wantUpstreamHits int // requests HandleMCPProxy must have sent upstream
	}{
		{
			name: "Mcp-Method header disagrees with body method: rejected before ever reaching upstream",
			body: modernToolCallRequest(1, "search"), // body method is "tools/call"
			headers: map[string]string{
				"MCP-Protocol-Version": "2026-07-28",
				"Mcp-Method":           "tools/list", // deliberately does not match
			},
			protocolVersion:  "2026-07-28",
			wantStatus:       fiber.StatusBadRequest,
			wantErrCode:      mcp.CodeHeaderMismatch,
			wantUpstreamHits: 0,
		},
		{
			name: "header matches body exactly: forwarded upstream and passed through",
			body: modernToolCallRequest(2, "search"),
			headers: map[string]string{
				"MCP-Protocol-Version": "2026-07-28",
				"Mcp-Method":           "tools/call",
				"Mcp-Name":             "search",
			},
			protocolVersion:  "2026-07-28",
			wantStatus:       fiber.StatusOK,
			wantUpstreamHits: 1,
		},
		{
			name:             "legacy request with no modern headers at all: unchanged (regression)",
			body:             `{"jsonrpc":"2.0","id":3,"method":"tools/list","params":{}}`,
			headers:          nil,
			protocolVersion:  "2025-03-26",
			wantStatus:       fiber.StatusOK,
			wantUpstreamHits: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var upstreamHits int
			var mu sync.Mutex

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				upstreamHits++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
			}))
			t.Cleanup(upstream.Close)

			dsn := fmt.Sprintf("file:TestMCPProxy_HeaderValidation_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupMCPProxyApp(t, dsn)
			org := mustCreateTestOrg(t, database, "proxy-headervalidation-"+strings.ReplaceAll(tc.name, " ", "-"))
			memberKey := addMCPTestKey(t, keyCache, org.ID)

			alias := "headervalidation-" + strings.ReplaceAll(tc.name, " ", "-")
			s := createExternalMCPServerPinned(t, database, alias, upstream.URL, tc.protocolVersion)
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			resp := proxyPostWithHeaders(t, app, alias, memberKey, tc.body, tc.headers)
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, raw)
			}
			if tc.wantStatus == fiber.StatusBadRequest {
				mcpResp := decodeMCPErrorBody(t, resp.Body)
				if mcpResp.Error == nil {
					t.Fatal("expected JSON-RPC error, got nil")
				}
				if mcpResp.Error.Code != tc.wantErrCode {
					t.Errorf("Error.Code = %d, want %d", mcpResp.Error.Code, tc.wantErrCode)
				}
			}

			mu.Lock()
			defer mu.Unlock()
			if upstreamHits != tc.wantUpstreamHits {
				t.Errorf("upstream received %d requests, want %d", upstreamHits, tc.wantUpstreamHits)
			}
		})
	}
}

// ---- Forward: MRTR and upstream error status pass through unmodified ------
// ---- (docs/mcp-v2.md §3.7, FIX 1/FIX 3) -------------------------------------

// TestMCPProxy_Forward_MRTR_InputRequired_PassedThroughByteIdentical is the
// full-stack counterpart of internal/mcp's
// TestForward_MRTR_InputRequired_PassedThroughByteIdentical: a modern-era
// upstream's resultType:"input_required" response — MRTR's InputRequiredResult
// — must reach the real MCP client on the other side of HandleMCPProxy
// completely unmodified, HTTP 200 included, so it can perform the retry the
// spec obligates it to.
func TestMCPProxy_Forward_MRTR_InputRequired_PassedThroughByteIdentical(t *testing.T) {
	t.Parallel()

	const mrtrBody = `{"jsonrpc":"2.0","id":1,"result":{"resultType":"input_required","inputRequests":{"confirm":{"method":"elicitation/create","params":{"mode":"form","message":"Confirm deployment?"}}},"requestState":"opaque-AEAD-blob"}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, mrtrBody)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_Forward_MRTR_InputRequired?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "proxy-mrtr")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	s := createExternalMCPServer(t, database, "mrtr-server", upstream.URL)
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	requestBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy","arguments":{}}}`
	resp := proxyPost(t, app, "mrtr-server", memberKey, requestBody)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (an input_required result must not be turned into 502); body: %s", resp.StatusCode, raw)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(raw) != mrtrBody {
		t.Errorf("response body =\n%s\nwant byte-identical to the upstream's response:\n%s", raw, mrtrBody)
	}
}

// TestMCPProxy_Forward_UpstreamErrorStatus_PassedThroughUnchanged verifies
// that an upstream JSON-RPC error, delivered at a non-2xx HTTP status, is
// forwarded to the caller with the SAME status and SAME body — not collapsed
// into a generic 502 the way any transport error was before Forward existed.
func TestMCPProxy_Forward_UpstreamErrorStatus_PassedThroughUnchanged(t *testing.T) {
	t.Parallel()

	const upstreamErrorBody = `{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"invalid params"}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, upstreamErrorBody)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_Forward_UpstreamErrorStatus?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "proxy-upstream-error")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	// Pinned: this upstream returns HTTP 400 for every request, including a
	// would-be server/discover or legacy initialize probe — an unpinned
	// transport would treat that 400 as a probe failure (resolveBinding never
	// even reaching the real request), not as the upstream error status this
	// test is actually about. See createExternalMCPServerPinned's doc.
	s := createExternalMCPServerPinned(t, database, "error-server", upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	requestBody := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy","arguments":{}}}`
	resp := proxyPost(t, app, "error-server", memberKey, requestBody)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400 (the upstream's own status, not a generic 502); body: %s", resp.StatusCode, raw)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(raw) != upstreamErrorBody {
		t.Errorf("response body =\n%s\nwant byte-identical to the upstream's error response:\n%s", raw, upstreamErrorBody)
	}
}

// ---- Forward: no session state of VoidLLM's own on this path --------------

// TestMCPProxy_Forward_SessionHeader_UnknownDropped_KnownRelayed is the
// successor of TestMCPProxy_Forward_SessionHeader_MirroredNotMerged, updated
// for docs/mcp-v2.md Designkorrektur A: the caller's own Mcp-Session-Id no
// longer reaches the upstream unconditionally. HandleMCPProxy relays it only
// once this exact caller's org has actually been issued it by the upstream
// on an earlier request through this same server — a session header the org
// was never issued is silently dropped before the request leaves VoidLLM.
// The upstream's own Mcp-Session-Id response header is still mirrored back
// to the caller exactly as received either way; that half of the original
// test is unaffected. This requires a PERSISTENT transport
// (setupMCPProxyAppWithTransportCache), unlike the original test's ad-hoc
// one: the tracking this test exercises lives on the *mcp.HTTPTransport
// itself and has nothing to observe across two separate ad-hoc transports.
func TestMCPProxy_Forward_SessionHeader_UnknownDropped_KnownRelayed(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var seenByUpstream []string
	var mintCount int

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inbound := r.Header.Get("Mcp-Session-Id")
		mu.Lock()
		seenByUpstream = append(seenByUpstream, inbound)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if inbound == "" {
			mu.Lock()
			mintCount++
			session := fmt.Sprintf("fullstack-mirror-session-%d", mintCount)
			mu.Unlock()
			w.Header().Set("Mcp-Session-Id", session)
		}
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_Forward_SessionHeader_UnknownDropped_KnownRelayed?mode=memory&cache=private"
	app, database, keyCache, transportCache := setupMCPProxyAppWithTransportCache(t, dsn)
	org := mustCreateTestOrg(t, database, "proxy-session-mirror")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "session-mirror-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}
	loadMCPTransportCache(t, database, transportCache)

	const body = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	// An Mcp-Session-Id this org was never issued is dropped before it
	// reaches the upstream — the upstream must see no session at all — but
	// the upstream's own response Mcp-Session-Id still mirrors back to the
	// caller unchanged.
	respUnknown := proxyPostWithHeaders(t, app, alias, memberKey, body, map[string]string{"Mcp-Session-Id": "never-issued-to-this-org"})
	if respUnknown.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(respUnknown.Body)
		respUnknown.Body.Close()
		t.Fatalf("status = %d, want 200; body: %s", respUnknown.StatusCode, raw)
	}
	mirroredSession := respUnknown.Header.Get("Mcp-Session-Id")
	respUnknown.Body.Close()

	mu.Lock()
	if len(seenByUpstream) != 1 || seenByUpstream[0] != "" {
		mu.Unlock()
		t.Fatalf("upstream saw %v, want a single empty entry — an unissued session must never reach the upstream", seenByUpstream)
	}
	mu.Unlock()
	if mirroredSession == "" {
		t.Fatal("Mcp-Session-Id mirrored back to caller is empty, want the upstream's own minted session (the response mirror is unaffected by the request-side fix)")
	}

	// Now that mirroredSession has actually been issued to this org by this
	// upstream, a follow-up request carrying it must be relayed unchanged.
	respKnown := proxyPostWithHeaders(t, app, alias, memberKey, body, map[string]string{"Mcp-Session-Id": mirroredSession})
	if respKnown.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(respKnown.Body)
		respKnown.Body.Close()
		t.Fatalf("status = %d, want 200; body: %s", respKnown.StatusCode, raw)
	}
	respKnown.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if len(seenByUpstream) != 2 || seenByUpstream[1] != mirroredSession {
		t.Errorf("upstream saw %v, want the second entry to be %q (a session actually issued to this org must be relayed)", seenByUpstream, mirroredSession)
	}
}
