package admin_test

// Tests for Handler.CallMCPTool — the programmatic MCP tool call path.
//
// CallMCPTool is not reached via HTTP routes; it is called directly by other
// services (e.g. code mode). Each test constructs a *Handler with a real
// in-memory SQLite database, registers an upstream httptest.Server as the MCP
// server record, and calls CallMCPTool with a synthetic auth.KeyInfo.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

// newCallMCPToolHandler creates a Handler backed by a fresh in-memory SQLite
// database. It returns the handler and the database so callers can seed data.
// The DSN must be unique per test to isolate in-memory SQLite databases.
func newCallMCPToolHandler(t *testing.T, dsn string, logger admin.MCPToolCallLogger) (*admin.Handler, *db.DB) {
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

	h := &admin.Handler{
		DB:                  database,
		HMACSecret:          testHMACSecret,
		EncryptionKey:       testEncryptionKey,
		KeyCache:            keyCache,
		License:             license.NewHolder(license.Verify("", true)),
		Log:                 noopLogger(t),
		MCPServer:           mcpServer,
		MCPCallTimeout:      5 * time.Second,
		MCPLogger:           logger,
		MCPAllowPrivateURLs: true, // tests use loopback httptest servers
	}

	return h, database
}

// newTestKeyInfo returns a minimal auth.KeyInfo suitable for CallMCPTool tests.
func newTestKeyInfo(orgID, keyID string) *auth.KeyInfo {
	plaintext, _ := keygen.Generate(keygen.KeyTypeUser)
	if keyID == "" {
		keyID = "call-mcp-key-" + plaintext[:8]
	}
	return &auth.KeyInfo{
		ID:      keyID,
		KeyType: keygen.KeyTypeUser,
		Role:    auth.RoleMember,
		OrgID:   orgID,
		Name:    "call mcp test key",
	}
}

// mustCreateTestOrg creates an org in the database for tests that need FK-valid
// org IDs (e.g. SetOrgMCPAccess references organizations.id).
func mustCreateTestOrg(t *testing.T, database *db.DB, slug string) *db.Org {
	t.Helper()
	org, err := database.CreateOrg(context.Background(), db.CreateOrgParams{
		Name: "Test Org " + slug,
		Slug: slug,
	})
	if err != nil {
		t.Fatalf("CreateOrg %q: %v", slug, err)
	}
	return org
}

// createActiveMCPServer inserts an active MCP server into the database and
// returns its record. The URL points to the given upstream httptest server.
func createActiveMCPServer(t *testing.T, database *db.DB, alias, upstreamURL string) *db.MCPServer {
	t.Helper()
	s, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Test " + alias,
		Alias:    alias,
		URL:      upstreamURL,
		AuthType: "none",
	})
	if err != nil {
		t.Fatalf("CreateMCPServer %q: %v", alias, err)
	}
	return s
}

// upstreamToolCallServer returns an httptest.Server that responds to any
// JSON-RPC request with the provided response body.
func upstreamToolCallServer(t *testing.T, responseBody string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, responseBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---- TestCallMCPTool_Success -------------------------------------------------

func TestCallMCPTool_Success(t *testing.T) {
	t.Parallel()

	const toolResponse = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"42"}]}}`

	upstream := upstreamToolCallServer(t, toolResponse)
	dsn := "file:TestCallMCPTool_Success?mode=memory&cache=private"
	handler, database := newCallMCPToolHandler(t, dsn, nil)

	s := createActiveMCPServer(t, database, "calc-server", upstream.URL)
	org := mustCreateTestOrg(t, database, "call-success")
	ki := newTestKeyInfo(org.ID, "")

	// Grant org access to the global server so access control passes.
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s.ID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	args := json.RawMessage(`{"x":21,"y":21}`)
	result, err := handler.CallMCPTool(context.Background(), ki, "calc-server", "add", args, false, "")
	if err != nil {
		t.Fatalf("CallMCPTool() error = %v, want nil", err)
	}
	if result == nil {
		t.Fatal("CallMCPTool() result = nil, want non-nil")
	}
	if !strings.Contains(string(result), "42") {
		t.Errorf("result = %q, want it to contain %q", result, "42")
	}
}

// ---- TestCallMCPTool_ServerNotFound -----------------------------------------

func TestCallMCPTool_ServerNotFound(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCallMCPTool_ServerNotFound?mode=memory&cache=private"
	handler, _ := newCallMCPToolHandler(t, dsn, nil)
	ki := newTestKeyInfo("org-call-notfound", "")

	_, err := handler.CallMCPTool(context.Background(), ki, "does-not-exist", "noop", json.RawMessage(`{}`), false, "")
	if err == nil {
		t.Fatal("CallMCPTool() error = nil, want error for unknown server alias")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error = %q, want it to mention the alias", err)
	}
}

// ---- TestCallMCPTool_ServerInactive -----------------------------------------

// TestCallMCPTool_ServerInactive verifies that calling an inactive server
// returns an error. GetMCPServerByAliasScoped filters out inactive servers at
// the DB level (WHERE is_active = 1), so the caller sees an "unknown server"
// error rather than a "disabled" message — either way, the call must fail.
func TestCallMCPTool_ServerInactive(t *testing.T) {
	t.Parallel()

	upstream := upstreamToolCallServer(t, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	dsn := "file:TestCallMCPTool_ServerInactive?mode=memory&cache=private"
	handler, database := newCallMCPToolHandler(t, dsn, nil)

	s := createActiveMCPServer(t, database, "inactive-server", upstream.URL)

	// Deactivate the server — GetMCPServerByAliasScoped will now return ErrNotFound.
	falseVal := false
	if _, err := database.UpdateMCPServer(context.Background(), s.ID, db.UpdateMCPServerParams{IsActive: &falseVal}); err != nil {
		t.Fatalf("UpdateMCPServer(deactivate): %v", err)
	}

	ki := newTestKeyInfo("org-call-inactive", "")
	_, err := handler.CallMCPTool(context.Background(), ki, "inactive-server", "noop", json.RawMessage(`{}`), false, "")
	if err == nil {
		t.Fatal("CallMCPTool() error = nil, want error for inactive server")
	}
	// The DB filters inactive servers at query time, so the error surfaces as
	// "unknown MCP server" (ErrNotFound path in CallMCPTool).
	if !strings.Contains(err.Error(), "inactive-server") {
		t.Errorf("error = %q, want it to mention the server alias", err)
	}
}

// ---- TestCallMCPTool_AccessDenied -------------------------------------------

func TestCallMCPTool_AccessDenied(t *testing.T) {
	t.Parallel()

	upstream := upstreamToolCallServer(t, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	dsn := "file:TestCallMCPTool_AccessDenied?mode=memory&cache=private"
	handler, database := newCallMCPToolHandler(t, dsn, nil)

	// Global server (no org_id) — requires explicit org_mcp_access entry.
	// CheckMCPAccess with an empty allowlist returns true (passes through).
	// To get access DENIED we must set a non-empty org allowlist that excludes
	// our target server ID (a different server ID in the list).
	s := createActiveMCPServer(t, database, "global-server", upstream.URL)

	// Create a second dummy server whose ID we will use as the only allowed entry,
	// which excludes our target server.
	other := createActiveMCPServer(t, database, "other-server-acl", upstream.URL)

	// Create a real org so SetOrgMCPAccess satisfies the FK constraint.
	org := mustCreateTestOrg(t, database, "call-denied")
	ki := newTestKeyInfo(org.ID, "key-denied-id")
	// Set org access to the OTHER server only — target is excluded.
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{other.ID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	_, err := handler.CallMCPTool(context.Background(), ki, s.Alias, "noop", json.RawMessage(`{}`), false, "")
	if err == nil {
		t.Fatal("CallMCPTool() error = nil, want access denied error")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error = %q, want it to mention access denied", err)
	}
}

// ---- TestCallMCPTool_CodeModeFlag -------------------------------------------

// mockCodeModeMCPLogger captures MCPToolCallEvent values for assertion.
type mockCodeModeMCPLogger struct {
	events []usage.MCPToolCallEvent
}

func (m *mockCodeModeMCPLogger) Log(ev usage.MCPToolCallEvent) {
	m.events = append(m.events, ev)
}

func TestCallMCPTool_CodeModeFlag(t *testing.T) {
	t.Parallel()

	const toolResponse = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`

	upstream := upstreamToolCallServer(t, toolResponse)
	dsn := "file:TestCallMCPTool_CodeModeFlag?mode=memory&cache=private"
	logger := &mockCodeModeMCPLogger{}
	handler, database := newCallMCPToolHandler(t, dsn, logger)

	s := createActiveMCPServer(t, database, "codemode-server", upstream.URL)
	org := mustCreateTestOrg(t, database, "call-codemode")
	ki := newTestKeyInfo(org.ID, "")

	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s.ID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	_, err := handler.CallMCPTool(context.Background(), ki, "codemode-server", "run", json.RawMessage(`{}`), true, "")
	if err != nil {
		t.Fatalf("CallMCPTool() error = %v, want nil", err)
	}

	if len(logger.events) != 1 {
		t.Fatalf("logger.events len = %d, want 1", len(logger.events))
	}
	ev := logger.events[0]
	if !ev.CodeMode {
		t.Errorf("event.CodeMode = false, want true when codeMode=true")
	}
}

// ---- TestCallMCPTool_ExecutionID --------------------------------------------

func TestCallMCPTool_ExecutionID(t *testing.T) {
	t.Parallel()

	const toolResponse = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"done"}]}}`

	upstream := upstreamToolCallServer(t, toolResponse)
	dsn := "file:TestCallMCPTool_ExecutionID?mode=memory&cache=private"
	logger := &mockCodeModeMCPLogger{}
	handler, database := newCallMCPToolHandler(t, dsn, logger)

	s := createActiveMCPServer(t, database, "execid-server", upstream.URL)
	org := mustCreateTestOrg(t, database, "call-execid")
	ki := newTestKeyInfo(org.ID, "")

	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s.ID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	const wantExecID = "01932e45-6789-7abc-def0-123456789abc"
	_, err := handler.CallMCPTool(context.Background(), ki, "execid-server", "run", json.RawMessage(`{}`), true, wantExecID)
	if err != nil {
		t.Fatalf("CallMCPTool() error = %v, want nil", err)
	}

	if len(logger.events) != 1 {
		t.Fatalf("logger.events len = %d, want 1", len(logger.events))
	}
	ev := logger.events[0]
	if ev.CodeModeExecutionID != wantExecID {
		t.Errorf("event.CodeModeExecutionID = %q, want %q", ev.CodeModeExecutionID, wantExecID)
	}
}

// ---- TestCallMCPTool_NilLogger ----------------------------------------------

// TestCallMCPTool_NilLogger verifies that CallMCPTool does not panic when
// MCPLogger is nil (the common production path without usage logging wired).
func TestCallMCPTool_NilLogger(t *testing.T) {
	t.Parallel()

	const toolResponse = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"ok"}]}}`

	upstream := upstreamToolCallServer(t, toolResponse)
	dsn := "file:TestCallMCPTool_NilLogger?mode=memory&cache=private"
	// Pass nil logger explicitly.
	handler, database := newCallMCPToolHandler(t, dsn, nil)

	s := createActiveMCPServer(t, database, "nil-logger-server", upstream.URL)
	org := mustCreateTestOrg(t, database, "call-nil-logger")
	ki := newTestKeyInfo(org.ID, "")

	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s.ID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	_, err := handler.CallMCPTool(context.Background(), ki, "nil-logger-server", "run", json.RawMessage(`{}`), false, "")
	if err != nil {
		t.Fatalf("CallMCPTool() error = %v, want nil (nil logger must not panic)", err)
	}
}

// ---- TestCallMCPTool_BuiltinServer ------------------------------------------

// TestCallMCPTool_BuiltinServer verifies that CallMCPTool with alias "voidllm"
// dispatches the tool call in-process via the built-in MCP server rather than
// making an HTTP request. The response must be a valid JSON-RPC result.
func TestCallMCPTool_BuiltinServer(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCallMCPTool_BuiltinServer?mode=memory&cache=private"
	handler, _ := newCallMCPToolHandler(t, dsn, nil)
	ki := newTestKeyInfo("org-builtin", "key-builtin")

	// list_models is a real registered tool that requires no arguments and
	// returns a JSON array. Using it proves the full in-process dispatch path.
	result, err := handler.CallMCPTool(context.Background(), ki, "voidllm", "list_models", json.RawMessage(`{}`), false, "")
	if err != nil {
		t.Fatalf("CallMCPTool(voidllm/list_models) error = %v, want nil", err)
	}
	if result == nil {
		t.Fatal("CallMCPTool(voidllm/list_models) result = nil, want non-nil")
	}
	// The response must be valid JSON.
	var parsed map[string]any
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v — raw: %s", err, result)
	}
	// A successful JSON-RPC response must carry a "result" key, not "error".
	if _, hasErr := parsed["error"]; hasErr {
		t.Errorf("response contains \"error\" key, want successful result: %s", result)
	}
	if _, hasResult := parsed["result"]; !hasResult {
		t.Errorf("response missing \"result\" key: %s", result)
	}
}

// ---- TestCallMCPTool_LoggerFields -------------------------------------------

// TestCallMCPTool_LoggerFields verifies that the event emitted to MCPLogger
// carries the correct ServerAlias and ToolName values.
func TestCallMCPTool_LoggerFields(t *testing.T) {
	t.Parallel()

	const toolResponse = `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"pong"}]}}`

	upstream := upstreamToolCallServer(t, toolResponse)
	dsn := "file:TestCallMCPTool_LoggerFields?mode=memory&cache=private"
	logger := &mockCodeModeMCPLogger{}
	handler, database := newCallMCPToolHandler(t, dsn, logger)

	s := createActiveMCPServer(t, database, "ping-server", upstream.URL)
	org := mustCreateTestOrg(t, database, "call-fields")
	ki := newTestKeyInfo(org.ID, "")

	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s.ID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	_, err := handler.CallMCPTool(context.Background(), ki, "ping-server", "ping", json.RawMessage(`{}`), false, "")
	if err != nil {
		t.Fatalf("CallMCPTool() error = %v, want nil", err)
	}

	if len(logger.events) != 1 {
		t.Fatalf("logger.events len = %d, want 1", len(logger.events))
	}
	ev := logger.events[0]
	if ev.ServerAlias != "ping-server" {
		t.Errorf("event.ServerAlias = %q, want %q", ev.ServerAlias, "ping-server")
	}
	if ev.ToolName != "ping" {
		t.Errorf("event.ToolName = %q, want %q", ev.ToolName, "ping")
	}
	if ev.Status != "success" {
		t.Errorf("event.Status = %q, want %q", ev.Status, "success")
	}
	if ev.OrgID != ki.OrgID {
		t.Errorf("event.OrgID = %q, want %q", ev.OrgID, ki.OrgID)
	}
	if ev.KeyID != ki.ID {
		t.Errorf("event.KeyID = %q, want %q", ev.KeyID, ki.ID)
	}
}
