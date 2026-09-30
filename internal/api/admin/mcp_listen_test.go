package admin_test

// End-to-end HTTP tests for server-side MCP subscriptions/listen
// (mcp_handler.go's handleMCPListenStream) driven through the real admin
// Fiber app — both the built-in management server (h.MCPServer) and a
// Code-Mode-style server (h.CodeModeServer) with SetToolsListChangedSource
// and an allow-all AccessChecker installed, mirroring internal/app's own
// wiring shape without needing a full Application or a real database-backed
// Code Mode service.
//
// Two request styles are used, deliberately:
//
//   - "ack-only" tests (mcpListenAckOnly) drive a SHORT h.MCPListenMaxDuration
//     through fiber's app.Test harness: the handler naturally completes (ack,
//     then the max-duration timer fires a graceful complete) well inside
//     app.Test's own timeout, so the full buffered transcript is available to
//     inspect after one ordinary, non-concurrent call — no real socket needed.
//   - tests that inject an event from OUTSIDE the request that opened the
//     stream (NotifyToolsListChanged, a client disconnect, CloseSubscriptions)
//     need a REAL TCP connection (startMCPListenListener): fiber's app.Test
//     harness only returns a *http.Response once the whole request/response
//     cycle has already finished, so there is no way to read a partial,
//     still-open stream through it — see startTunnelListener's identical doc
//     in internal/app/playground_tunnel_test.go, the precedent this mirrors.
//     Reading the ack off the real socket before triggering the external event
//     is what proves the subscriber is already registered, with no sleep.

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
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

// setupMCPListenApp builds a Fiber app with both the management server
// (h.MCPServer, never given a tools/list_changed source — mirrors the
// built-in voidllm server) and a Code-Mode-style server (h.CodeModeServer,
// SetToolsListChangedSource(true), an allow-all AccessChecker) registered,
// and h.MCPListenMaxDuration set to maxDuration.
func setupMCPListenApp(t *testing.T, dsn string, maxDuration time.Duration) (app *fiber.App, mgmtServer, codeModeServer *mcp.Server, keyCache *cache.Cache[string, auth.KeyInfo]) {
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

	mgmtServer = mcp.NewServer("voidllm", "test")
	codeModeServer = mcp.NewServer("code-mode", "test")
	codeModeServer.SetToolsListChangedSource(true)
	codeModeServer.SetAccessChecker(func(mcp.KeyIdentity, string) bool { return true })

	handler := &admin.Handler{
		DB:                   database,
		HMACSecret:           testHMACSecret,
		KeyCache:             keyCache,
		License:              license.NewHolder(license.Verify("", true)),
		Log:                  noopLogger(t),
		MCPServer:            mgmtServer,
		CodeModeServer:       codeModeServer,
		MCPListenMaxDuration: maxDuration,
	}

	app = fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)
	return app, mgmtServer, codeModeServer, keyCache
}

// subscriptionsListenBody builds a subscriptions/listen JSON-RPC request body
// with the two MUST _meta fields (docs/mcp-v2.md §3.2). notifications is
// marshaled verbatim as params.notifications when non-nil — including a
// deliberately malformed value (e.g. a bare string), for the malformed-input
// tests below.
func subscriptionsListenBody(id int, notifications any) string {
	params := map[string]any{
		"_meta": map[string]any{
			"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
			"io.modelcontextprotocol/clientCapabilities": map[string]any{},
		},
	}
	if notifications != nil {
		params["notifications"] = notifications
	}
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": "subscriptions/listen", "params": params}
	b, err := json.Marshal(req)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// mcpListenRequest builds an *http.Request for a subscriptions/listen call:
// modern-era headers (MCP-Protocol-Version, Mcp-Method — required by
// validateMCPHeaders' §4.5 cross-check; subscriptions/listen names no
// params.name/uri, so Mcp-Name is never required — see mcp.TargetParamKey)
// plus Authorization.
func mcpListenRequest(path, key, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "subscriptions/listen")
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}

// sseEvent is one parsed "event: message\ndata: <json>\n\n" block.
type sseEvent struct {
	raw  string
	body map[string]any
}

// parseSSEEvents splits a buffered SSE transcript (possibly several
// concatenated events, exactly what an ack-only request's full response body
// contains) into individual events, each decoded as JSON. formatSSEMessage
// (mcp_handler.go) always renders a compact, single-line JSON body, so a
// plain "data: " prefix strip per line, rejoined, recovers the exact bytes.
func parseSSEEvents(t *testing.T, raw []byte) []sseEvent {
	t.Helper()
	var events []sseEvent
	for _, block := range strings.Split(string(raw), "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var dataLines []string
		for _, line := range strings.Split(block, "\n") {
			if after, ok := strings.CutPrefix(line, "data: "); ok {
				dataLines = append(dataLines, after)
			}
		}
		if len(dataLines) == 0 {
			continue
		}
		data := strings.Join(dataLines, "\n")
		var decoded map[string]any
		if err := json.Unmarshal([]byte(data), &decoded); err != nil {
			t.Fatalf("event data is not valid JSON: %v; raw: %s", err, data)
		}
		events = append(events, sseEvent{raw: data, body: decoded})
	}
	return events
}

// mcpListenAckOnly drives a subscriptions/listen request that is expected to
// complete entirely on its own — the handler's h.MCPListenMaxDuration must be
// short enough that this happens well within reqTimeout — and returns every
// SSE event the full response contained, in order (ordinarily the ack
// followed by the graceful-end complete message).
func mcpListenAckOnly(t *testing.T, app *fiber.App, path, key string, notifications any, id int) []sseEvent {
	t.Helper()
	req := mcpListenRequest(path, key, subscriptionsListenBody(id, notifications))
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream prefix", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}
	if xab := resp.Header.Get("X-Accel-Buffering"); xab != "no" {
		t.Errorf("X-Accel-Buffering = %q, want %q", xab, "no")
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return parseSSEEvents(t, raw)
}

// ---- response headers + exact ack JSON ---------------------------------------

func TestMCPListenHTTP_HeadersAndAck_ManagementServer(t *testing.T) {
	t.Parallel()

	app, _, _, keyCache := setupMCPListenApp(t, "file:TestMCPListenHTTP_Ack_Mgmt?mode=memory&cache=private", 100*time.Millisecond)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-ack-mgmt")

	events := mcpListenAckOnly(t, app, mcpURL, key, map[string]any{"toolsListChanged": true}, 42)
	if len(events) == 0 {
		t.Fatal("no SSE events in response")
	}
	ack := events[0].body

	if ack["method"] != "notifications/subscriptions/acknowledged" {
		t.Errorf("ack method = %v, want notifications/subscriptions/acknowledged", ack["method"])
	}
	params, _ := ack["params"].(map[string]any)
	if params == nil {
		t.Fatalf("ack params missing: %+v", ack)
	}
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil || meta["io.modelcontextprotocol/subscriptionId"] != float64(42) {
		t.Errorf("ack _meta.subscriptionId = %v, want 42 (this request's own id)", meta)
	}
	notifications, _ := params["notifications"].(map[string]any)
	// The management server never enables a tools/list_changed source
	// (setupMCPListenApp's own doc) — the honored set must be empty, even
	// though toolsListChanged was requested.
	if len(notifications) != 0 {
		t.Errorf("ack notifications = %v, want empty (management server honors nothing)", notifications)
	}
}

func TestMCPListenHTTP_HeadersAndAck_CodeModeServer(t *testing.T) {
	t.Parallel()

	app, _, _, keyCache := setupMCPListenApp(t, "file:TestMCPListenHTTP_Ack_CodeMode?mode=memory&cache=private", 100*time.Millisecond)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-ack-cm")

	events := mcpListenAckOnly(t, app, "/api/v1/mcp", key, map[string]any{"toolsListChanged": true}, 7)
	if len(events) == 0 {
		t.Fatal("no SSE events in response")
	}
	ack := events[0].body
	params, _ := ack["params"].(map[string]any)
	notifications, _ := params["notifications"].(map[string]any)
	if notifications["toolsListChanged"] != true {
		t.Errorf("ack notifications.toolsListChanged = %v, want true (Code Mode server has a listChanged source)", notifications["toolsListChanged"])
	}
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil || meta["io.modelcontextprotocol/subscriptionId"] != float64(7) {
		t.Errorf("ack _meta.subscriptionId = %v, want 7", meta)
	}
}

// ---- graceful end via max duration -------------------------------------------

func TestMCPListenHTTP_MaxDuration_GracefulEndThenClose(t *testing.T) {
	t.Parallel()

	app, _, _, keyCache := setupMCPListenApp(t, "file:TestMCPListenHTTP_MaxDurationEnd?mode=memory&cache=private", 100*time.Millisecond)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-maxdur")

	events := mcpListenAckOnly(t, app, mcpURL, key, map[string]any{"toolsListChanged": true}, 1)
	if len(events) != 2 {
		t.Fatalf("got %d SSE events, want exactly 2 (ack, then graceful complete); events: %+v", len(events), events)
	}

	complete := events[1].body
	if complete["id"] != float64(1) {
		t.Errorf("complete id = %v, want 1 (echoing the original request id)", complete["id"])
	}
	result, _ := complete["result"].(map[string]any)
	if result == nil || result["resultType"] != "complete" {
		t.Errorf("complete result = %+v, want resultType: complete", result)
	}
	meta, _ := result["_meta"].(map[string]any)
	if meta == nil || meta["io.modelcontextprotocol/subscriptionId"] != float64(1) {
		t.Errorf("complete _meta.subscriptionId = %v, want 1", meta)
	}
}

// ---- keep-alive: bounded negative check only ---------------------------------

// TestMCPListenHTTP_KeepAlive_NoPrematureComment is a WEAK, one-directional
// check: mcpListenKeepAliveInterval (mcp_handler.go) is a hardcoded 30s
// constant with no test hook to shrink it, so this suite cannot positively
// prove the ": ping" comment actually fires without either waiting out the
// real 30s or adding one (out of scope for a test-only change — see the
// final report's testability-gap note). What CAN be proven without a sleep
// anywhere near that long: a stream that ends (via a short
// h.MCPListenMaxDuration) well before 30s elapses contains no keep-alive
// comment line at all — i.e. the two SSE events already asserted by
// TestMCPListenHTTP_MaxDuration_GracefulEndThenClose are the ONLY lines in
// the transcript, with nothing extraneous interleaved.
func TestMCPListenHTTP_KeepAlive_NoPrematureComment(t *testing.T) {
	t.Parallel()

	app, _, _, keyCache := setupMCPListenApp(t, "file:TestMCPListenHTTP_KeepAliveNegative?mode=memory&cache=private", 100*time.Millisecond)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-keepalive")

	req := mcpListenRequest(mcpURL, key, subscriptionsListenBody(1, map[string]any{"toolsListChanged": true}))
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 3 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(raw), ": ping") {
		t.Errorf("transcript contains a keep-alive comment well before mcpListenKeepAliveInterval could have elapsed: %q", raw)
	}
}

// ---- malformed input never opens a stream ------------------------------------

func TestMCPListenHTTP_MalformedNotifications_400NoStream(t *testing.T) {
	t.Parallel()

	app, _, _, keyCache := setupMCPListenApp(t, "file:TestMCPListenHTTP_Malformed?mode=memory&cache=private", time.Hour)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-malformed")

	req := mcpListenRequest(mcpURL, key, subscriptionsListenBody(1, "not-an-object"))
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want an ordinary JSON error response, not SSE", ct)
	}
	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error == nil || mcpResp.Error.Code != mcp.CodeInvalidParams {
		t.Errorf("Error = %+v, want CodeInvalidParams", mcpResp.Error)
	}
}

// ---- legacy era: MethodNotFound, never opens a stream ------------------------

func TestMCPListenHTTP_LegacyEra_MethodNotFound(t *testing.T) {
	t.Parallel()

	app, _, _, keyCache := setupMCPListenApp(t, "file:TestMCPListenHTTP_Legacy?mode=memory&cache=private", time.Hour)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-legacy")

	// No MCP-Protocol-Version / Mcp-Method headers at all — a genuinely
	// legacy request.
	resp := mcpPost(t, app, key, mcpRequest(1, "subscriptions/listen", map[string]any{}))
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (legacy CodeMethodNotFound keeps HTTP 200); body: %s", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q, want application/json (never a stream)", ct)
	}
	mcpResp := decodeMCPResponse(t, resp.Body)
	if mcpResp.Error == nil || mcpResp.Error.Code != mcp.CodeMethodNotFound {
		t.Errorf("Error = %+v, want CodeMethodNotFound", mcpResp.Error)
	}
}

// ---- discover: tools.listChanged only on the Code Mode server ---------------

func TestMCPListenHTTP_Discover_ToolsListChangedOnlyOnCodeModeServer(t *testing.T) {
	t.Parallel()

	app, _, _, keyCache := setupMCPListenApp(t, "file:TestMCPListenHTTP_Discover?mode=memory&cache=private", time.Hour)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-discover")

	tests := []struct {
		name        string
		path        string
		wantChanged bool
	}{
		{name: "management server", path: mcpURL, wantChanged: false},
		{name: "code mode server", path: "/api/v1/mcp", wantChanged: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path,
				strings.NewReader(subscriptionsListenDiscoverBody()))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("MCP-Protocol-Version", "2026-07-28")
			req.Header.Set("Mcp-Method", "server/discover")
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
			mcpResp := decodeMCPResponse(t, resp.Body)
			if mcpResp.Error != nil {
				t.Fatalf("unexpected error: %+v", mcpResp.Error)
			}
			b, _ := json.Marshal(mcpResp.Result)
			var result map[string]any
			if err := json.Unmarshal(b, &result); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			caps, _ := result["capabilities"].(map[string]any)
			tools, _ := caps["tools"].(map[string]any)
			_, present := tools["listChanged"]
			if present != tc.wantChanged {
				t.Errorf("capabilities.tools.listChanged present = %v, want %v", present, tc.wantChanged)
			}
		})
	}
}

// subscriptionsListenDiscoverBody builds a server/discover request body with
// the same MUST _meta fields subscriptionsListenBody uses.
func subscriptionsListenDiscoverBody() string {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "server/discover",
		"params": map[string]any{
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	}
	b, _ := json.Marshal(req)
	return string(b)
}

// ---- handler-level: MCPListenMaxDuration <= 0 refuses and unregisters -------

func TestMCPListenHTTP_MaxDurationZero_Refuses503AndUnregisters(t *testing.T) {
	t.Parallel()

	app, _, _, keyCache := setupMCPListenApp(t, "file:TestMCPListenHTTP_MaxDurationZero?mode=memory&cache=private", 0)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-zero-maxdur")

	// maxListenStreamsPerKey is 4 — five consecutive requests, each one
	// registering-then-immediately-unregistering per handleMCPListenStream's
	// own doc, must ALL return 503 (never 429): if even one failed to
	// unregister, the per-key slot would eventually run out and a later
	// request in this loop would fail with 429 instead, proving a leak.
	for i := 0; i < 5; i++ {
		req := mcpListenRequest(mcpURL, key, subscriptionsListenBody(i+1, map[string]any{"toolsListChanged": true}))
		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test (iteration %d): %v", i, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != fiber.StatusServiceUnavailable {
			t.Fatalf("iteration %d: status = %d, want 503; body: %s", i, resp.StatusCode, raw)
		}
		var mcpResp mcp.Response
		if err := json.Unmarshal(raw, &mcpResp); err != nil {
			t.Fatalf("iteration %d: decode body: %v; raw: %s", i, err, raw)
		}
		if mcpResp.Error == nil || mcpResp.Error.Code != mcp.CodeInternalError {
			t.Errorf("iteration %d: Error = %+v, want CodeInternalError (the static "+
				"'not available on this deployment' response)", i, mcpResp.Error)
		}
	}
}

// ---- real-socket tests: external events injected mid-stream -----------------

// startMCPListenListener binds app to a real, ephemeral TCP listener and
// waits until it is actually serving requests (not merely bound) — see this
// file's own top-of-file doc for why the real-socket tests below need this
// instead of app.Test. Mirrors startTunnelListener
// (internal/app/playground_tunnel_test.go) exactly, adapted to a bare
// *fiber.App instead of a full *Application.
func startMCPListenListener(t *testing.T, app *fiber.App) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	listenErr := make(chan error, 1)
	go func() {
		listenErr <- app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
	}()

	baseURL := "http://" + ln.Addr().String()
	probeClient := &http.Client{Timeout: 200 * time.Millisecond}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-listenErr:
			t.Fatalf("app.Listener exited before becoming ready: %v", err)
		default:
		}
		req, _ := http.NewRequest(http.MethodPost, baseURL+mcpURL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		req.Header.Set("Content-Type", "application/json")
		resp, probeErr := probeClient.Do(req)
		if probeErr == nil {
			_ = resp.Body.Close()
			return baseURL
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for the MCP listen test listener to become ready at %s", baseURL)
	return ""
}

// openMCPListenStream opens a real, long-lived subscriptions/listen
// connection against baseURL+path and reads exactly one SSE event (expected:
// the ack) off the wire before returning, proving the subscriber is already
// registered server-side by the time this call returns — the synchronization
// point every real-socket test in this file relies on instead of a sleep.
func openMCPListenStream(t *testing.T, client *http.Client, baseURL, path, key string, id int) (resp *http.Response, reader *bufio.Reader, ack sseEvent) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+path,
		strings.NewReader(subscriptionsListenBody(id, map[string]any{"toolsListChanged": true})))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "subscriptions/listen")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err = client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	reader = bufio.NewReader(resp.Body)
	ack = readOneMCPListenEvent(t, reader)
	if ack.body["method"] != "notifications/subscriptions/acknowledged" {
		t.Fatalf("first event method = %v, want the acknowledgement", ack.body["method"])
	}
	return resp, reader, ack
}

// readOneMCPListenEvent reads exactly one "event: message\ndata: ...\n\n"
// block from r, blocking until it arrives (bounded only by the surrounding
// *http.Client's own Timeout — every caller in this file sets one).
func readOneMCPListenEvent(t *testing.T, r *bufio.Reader) sseEvent {
	t.Helper()
	var lines []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE event: %v (partial: %v)", err, lines)
		}
		trimmed := strings.TrimRight(line, "\n")
		if trimmed == "" {
			break // blank line terminates one SSE block
		}
		lines = append(lines, trimmed)
	}
	var dataLines []string
	for _, line := range lines {
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			dataLines = append(dataLines, after)
		}
	}
	data := strings.Join(dataLines, "\n")
	var decoded map[string]any
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		t.Fatalf("event data is not valid JSON: %v; raw: %s", err, data)
	}
	return sseEvent{raw: data, body: decoded}
}

// TestMCPListenHTTP_NotifyToolsListChanged_DeliversEvent proves the full
// path a real client observes: after the ack, a direct
// codeModeServer.NotifyToolsListChanged call (standing in for
// ToolCache.SetOnChange's own wiring — internal/app's own test coverage
// already proves that half) delivers a notifications/tools/list_changed
// event carrying this stream's own subscriptionId.
func TestMCPListenHTTP_NotifyToolsListChanged_DeliversEvent(t *testing.T) {
	t.Parallel()

	app, _, codeModeServer, keyCache := setupMCPListenApp(t,
		"file:TestMCPListenHTTP_NotifyDelivers?mode=memory&cache=private", 5*time.Second)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-notify")
	baseURL := startMCPListenListener(t, app)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, reader, ack := openMCPListenStream(t, client, baseURL, "/api/v1/mcp", key, 9)
	defer resp.Body.Close()
	subID := ack.body["params"].(map[string]any)["_meta"].(map[string]any)["io.modelcontextprotocol/subscriptionId"]

	codeModeServer.NotifyToolsListChanged(mcp.NotifyScope{ServerID: "any-server"})

	event := readOneMCPListenEvent(t, reader)
	if event.body["method"] != "notifications/tools/list_changed" {
		t.Errorf("event method = %v, want notifications/tools/list_changed", event.body["method"])
	}
	meta, _ := event.body["params"].(map[string]any)["_meta"].(map[string]any)
	if meta == nil || meta["io.modelcontextprotocol/subscriptionId"] != subID {
		t.Errorf("event subscriptionId = %v, want %v (this stream's own)", meta, subID)
	}
}

// TestMCPListenHTTP_PerKeyLimit_FifthStreamRejectedBeforeOpening verifies
// maxListenStreamsPerKey end-to-end: 4 real, concurrently open streams for
// one key, then a 5th ordinary (non-streaming) request that must be refused
// with 429 before ever opening a connection.
func TestMCPListenHTTP_PerKeyLimit_FifthStreamRejectedBeforeOpening(t *testing.T) {
	t.Parallel()

	app, _, _, keyCache := setupMCPListenApp(t,
		"file:TestMCPListenHTTP_PerKeyLimit?mode=memory&cache=private", 5*time.Second)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-perkey")
	baseURL := startMCPListenListener(t, app)

	client := &http.Client{Timeout: 6 * time.Second}
	var streams []*http.Response
	t.Cleanup(func() {
		for _, r := range streams {
			r.Body.Close()
		}
	})
	for i := 0; i < 4; i++ {
		resp, _, _ := openMCPListenStream(t, client, baseURL, mcpURL, key, i+1)
		streams = append(streams, resp)
	}

	fifthReq, err := http.NewRequest(http.MethodPost, baseURL+mcpURL,
		strings.NewReader(subscriptionsListenBody(5, map[string]any{"toolsListChanged": true})))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	fifthReq.Header.Set("Content-Type", "application/json")
	fifthReq.Header.Set("MCP-Protocol-Version", "2026-07-28")
	fifthReq.Header.Set("Mcp-Method", "subscriptions/listen")
	fifthReq.Header.Set("Authorization", "Bearer "+key)

	fifthResp, err := client.Do(fifthReq)
	if err != nil {
		t.Fatalf("5th request: %v", err)
	}
	defer fifthResp.Body.Close()
	raw, _ := io.ReadAll(fifthResp.Body)
	if fifthResp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("5th request status = %d, want 429; body: %s", fifthResp.StatusCode, raw)
	}
	var mcpResp mcp.Response
	if err := json.Unmarshal(raw, &mcpResp); err != nil {
		t.Fatalf("decode 5th response: %v; raw: %s", err, raw)
	}
	if mcpResp.Error == nil || mcpResp.Error.Code != mcp.CodeTooManyListenStreams {
		t.Errorf("Error = %+v, want CodeTooManyListenStreams", mcpResp.Error)
	}
}

// TestMCPListenHTTP_ClientDisconnect_SlotFreed verifies that closing a
// client's connection eventually frees its subscriberRegistry slot: with 4
// of 4 per-key slots held, closing ONE connection and then forcing a write
// attempt on it (via NotifyToolsListChanged, which every still-open stream
// for this key honors) lets its handler observe the broken connection and
// unregister — proven by a 5th listen request eventually succeeding. Polled
// with a bounded retry loop (not a fixed sleep): the disconnect's own cleanup
// happens on the server's request-handling goroutine, asynchronously
// relative to this test's own NotifyToolsListChanged call.
func TestMCPListenHTTP_ClientDisconnect_SlotFreed(t *testing.T) {
	t.Parallel()

	app, _, codeModeServer, keyCache := setupMCPListenApp(t,
		"file:TestMCPListenHTTP_Disconnect?mode=memory&cache=private", 5*time.Second)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-disconnect")
	baseURL := startMCPListenListener(t, app)

	client := &http.Client{Timeout: 6 * time.Second}
	var streams []*http.Response
	for i := 0; i < 4; i++ {
		resp, _, _ := openMCPListenStream(t, client, baseURL, "/api/v1/mcp", key, i+1)
		streams = append(streams, resp)
	}
	t.Cleanup(func() {
		for _, r := range streams {
			r.Body.Close()
		}
	})

	// Confirm the 5th is indeed blocked before disconnecting anything.
	blockedReq, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/mcp",
		strings.NewReader(subscriptionsListenBody(5, map[string]any{"toolsListChanged": true})))
	blockedReq.Header.Set("Content-Type", "application/json")
	blockedReq.Header.Set("MCP-Protocol-Version", "2026-07-28")
	blockedReq.Header.Set("Mcp-Method", "subscriptions/listen")
	blockedReq.Header.Set("Authorization", "Bearer "+key)
	blockedResp, err := client.Do(blockedReq)
	if err != nil {
		t.Fatalf("blocked probe request: %v", err)
	}
	blockedRaw, _ := io.ReadAll(blockedResp.Body)
	blockedResp.Body.Close()
	if blockedResp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("pre-disconnect probe status = %d, want 429; body: %s", blockedResp.StatusCode, blockedRaw)
	}

	// Disconnect one of the 4 open streams.
	streams[0].Body.Close()

	deadline := time.Now().Add(5 * time.Second)
	var lastStatus int
	var lastBody []byte
	for time.Now().Before(deadline) {
		// Force a write attempt on every still-registered subscriber for this
		// key, including the now-disconnected one: its next write will fail,
		// triggering its own handler's defer Unregister.
		codeModeServer.NotifyToolsListChanged(mcp.NotifyScope{ServerID: "any-server"})

		req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/mcp",
			strings.NewReader(subscriptionsListenBody(6, map[string]any{"toolsListChanged": true})))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "subscriptions/listen")
		req.Header.Set("Authorization", "Bearer "+key)

		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("retry request: %v", err)
		}
		lastStatus = resp.StatusCode
		if resp.StatusCode == http.StatusOK {
			// Succeeded — drain the ack so we don't leak yet another open
			// stream past this test, then stop polling.
			readOneMCPListenEvent(t, bufio.NewReader(resp.Body))
			resp.Body.Close()
			return
		}
		lastBody, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("a 5th listen request never succeeded within the bound after disconnecting one of 4 streams "+
		"— last status %d, body: %s", lastStatus, lastBody)
}

// TestMCPListenHTTP_CloseSubscriptions_GracefulEndThenFutureListen503
// verifies Server.CloseSubscriptions' full HTTP-visible contract: an
// already-open real stream receives the graceful-end complete message (and
// the connection then ends), and every subsequent subscriptions/listen
// request is refused with 503, permanently.
func TestMCPListenHTTP_CloseSubscriptions_GracefulEndThenFutureListen503(t *testing.T) {
	t.Parallel()

	app, _, codeModeServer, keyCache := setupMCPListenApp(t,
		"file:TestMCPListenHTTP_CloseSubs?mode=memory&cache=private", 5*time.Second)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-closesubs")
	baseURL := startMCPListenListener(t, app)

	client := &http.Client{Timeout: 6 * time.Second}
	resp, reader, _ := openMCPListenStream(t, client, baseURL, "/api/v1/mcp", key, 1)
	defer resp.Body.Close()

	codeModeServer.CloseSubscriptions()

	complete := readOneMCPListenEvent(t, reader)
	result, _ := complete.body["result"].(map[string]any)
	if result == nil || result["resultType"] != "complete" {
		t.Fatalf("event after CloseSubscriptions = %+v, want the graceful-end complete message", complete.body)
	}

	newReq, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/mcp",
		strings.NewReader(subscriptionsListenBody(2, map[string]any{"toolsListChanged": true})))
	newReq.Header.Set("Content-Type", "application/json")
	newReq.Header.Set("MCP-Protocol-Version", "2026-07-28")
	newReq.Header.Set("Mcp-Method", "subscriptions/listen")
	newReq.Header.Set("Authorization", "Bearer "+key)

	newResp, err := client.Do(newReq)
	if err != nil {
		t.Fatalf("post-close request: %v", err)
	}
	defer newResp.Body.Close()
	raw, _ := io.ReadAll(newResp.Body)
	if newResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("post-close status = %d, want 503; body: %s", newResp.StatusCode, raw)
	}
	var mcpResp mcp.Response
	if err := json.Unmarshal(raw, &mcpResp); err != nil {
		t.Fatalf("decode post-close response: %v; raw: %s", err, raw)
	}
	if mcpResp.Error == nil || mcpResp.Error.Code != mcp.CodeSubscriptionsClosed {
		t.Errorf("Error = %+v, want CodeSubscriptionsClosed", mcpResp.Error)
	}
}

// TestMCPListenHTTP_SlotsReleasedAfterStreamsEnd verifies that once all
// maxListenStreamsPerKey streams for a key end on their own (a short
// h.MCPListenMaxDuration here, standing in for any other natural end), a
// subsequent listen request for that same key succeeds immediately — no
// leaked slots.
func TestMCPListenHTTP_SlotsReleasedAfterStreamsEnd(t *testing.T) {
	t.Parallel()

	app, _, _, keyCache := setupMCPListenApp(t,
		"file:TestMCPListenHTTP_SlotsReleased?mode=memory&cache=private", 150*time.Millisecond)
	key := addTestKey(t, keyCache, auth.RoleMember, "org-listen-slots-released")

	// Fill, then drain, all 4 per-key slots via the ack-only helper: each
	// call's own h.MCPListenMaxDuration (150ms) elapses and the handler
	// returns before app.Test's own 3s timeout, so by the time each call
	// below returns its slot has already been released.
	for i := 0; i < 4; i++ {
		events := mcpListenAckOnly(t, app, mcpURL, key, map[string]any{"toolsListChanged": true}, i+1)
		if len(events) != 2 {
			t.Fatalf("iteration %d: got %d events, want 2 (ack + complete)", i, len(events))
		}
	}

	// A 5th request now must succeed immediately — every earlier slot was
	// already released by the time its own request returned above.
	events := mcpListenAckOnly(t, app, mcpURL, key, map[string]any{"toolsListChanged": true}, 5)
	if len(events) != 2 || events[0].body["method"] != "notifications/subscriptions/acknowledged" {
		t.Fatalf("5th request after all 4 slots were released = %+v, want a successful ack+complete pair", events)
	}
}
