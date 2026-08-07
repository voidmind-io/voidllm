package admin_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/internal/metrics"
	"github.com/voidmind-io/voidllm/internal/usage"
)

// This file covers review finding A1 (mcp_proxy.go's own doc above the
// SendStreamWriter closure): before the streaming rewrite, the call-level
// metrics and the usage.MCPToolCallEvent were recorded eagerly, right after
// Forward returned response HEADERS — long before io.Copy, and therefore the
// actual outcome of the stream, was known. A stream that ran for minutes and
// then broke was recorded as an instant "success", 2ms duration. Recording
// now happens from inside the SendStreamWriter closure, after io.Copy
// returns, so it sees the real elapsed duration and the real outcome.
//
// Every test here wires a real usage.MCPLogger backed by a real in-memory
// SQLite database (per this package's testing philosophy — no mocks for
// storage) and calls logger.Stop() after the HTTP round trip completes to
// force a synchronous flush before querying the mcp_tool_calls table
// directly, instead of waiting out MCPLogger's own 5s/100-event batching.

// mcpToolCallRow is the subset of a mcp_tool_calls row these tests assert on.
type mcpToolCallRow struct {
	durationMS *int
	status     string
}

// latestMCPToolCallByAlias queries the single most recent mcp_tool_calls row
// for serverAlias, failing the test if none exists.
func latestMCPToolCallByAlias(t *testing.T, database *db.DB, serverAlias string) mcpToolCallRow {
	t.Helper()
	row := database.SQL().QueryRowContext(context.Background(),
		"SELECT duration_ms, status FROM mcp_tool_calls WHERE server_alias = ? ORDER BY created_at DESC, id DESC LIMIT 1",
		serverAlias)
	var got mcpToolCallRow
	if err := row.Scan(&got.durationMS, &got.status); err != nil {
		t.Fatalf("query mcp_tool_calls for alias %q: %v", serverAlias, err)
	}
	return got
}

// newMCPProxyAppWithRealUsageLogger builds a Fiber app wired exactly like
// setupMCPProxyApp, except Handler.MCPLogger is a real usage.MCPLogger backed
// by database instead of nil, and Handler.MCPStreamMaxBytes is maxBytes (0
// leaves it at the Handler zero value — unbounded, matching every other test
// in this package's default). MCPLogger must be set before
// admin.RegisterRoutes mounts the handlers that close over it, so there is no
// way to attach it to an already-built app — hence this dedicated
// constructor rather than reusing setupMCPProxyApp. The returned
// *usage.MCPLogger must be Stop()ed by the caller once the request(s) under
// test have completed, to force a synchronous flush before querying
// mcp_tool_calls.
func newMCPProxyAppWithRealUsageLogger(t *testing.T, dsn string, maxBytes int64) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo], *usage.MCPLogger) {
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
	logger := usage.NewMCPLogger(database, 10, noopLogger(t))

	handler := &admin.Handler{
		DB:                  database,
		HMACSecret:          testHMACSecret,
		EncryptionKey:       testEncryptionKey,
		KeyCache:            keyCache,
		License:             license.NewHolder(license.Verify("", true)),
		Log:                 noopLogger(t),
		MCPServer:           mcp.NewServer("voidllm", "test"),
		MCPCallTimeout:      5 * time.Second,
		MCPLogger:           logger,
		MCPStreamMaxBytes:   maxBytes,
		MCPAllowPrivateURLs: true,
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache, logger
}

// ---- The main case: real elapsed duration, not header-arrival time --------

// TestMCPProxy_UsageEvent_RealStreamDuration_NotHeaderArrivalTime is the
// direct regression test for review finding A1: an upstream that sends
// response headers almost immediately, then pauses for a measurable amount
// of time before writing any body bytes, then ends cleanly, must produce a
// usage event whose DurationMS reflects the FULL round trip — including the
// pause — not just the near-zero time it took Forward to receive headers.
func TestMCPProxy_UsageEvent_RealStreamDuration_NotHeaderArrivalTime(t *testing.T) {
	t.Parallel()

	const pause = 300 * time.Millisecond
	const event = "event: message\n" +
		`data: {"jsonrpc":"2.0","id":1,"result":{"tools":[]}}` +
		"\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(pause)
		fmt.Fprint(w, event)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_UsageEvent_RealStreamDuration?mode=memory&cache=private"
	app, database, keyCache, logger := newMCPProxyAppWithRealUsageLogger(t, dsn, 0)
	org := mustCreateTestOrg(t, database, "usage-real-duration")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "usage-real-duration-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		resp.Body.Close()
		t.Fatalf("read body: %v", err)
	}
	resp.Body.Close()

	logger.Stop() // force a synchronous flush before querying

	got := latestMCPToolCallByAlias(t, database, alias)
	if got.status != "success" {
		t.Errorf("status = %q, want %q", got.status, "success")
	}
	if got.durationMS == nil {
		t.Fatal("duration_ms is nil, want a measured duration")
	}
	// The real bug this test guards against recorded ~0-2ms (the time to
	// receive headers) regardless of how long the stream actually ran. A
	// generous fraction of pause, well below it, is enough to distinguish
	// "the real elapsed time" from "header-arrival time".
	wantAtLeast := int(pause.Milliseconds()) / 2
	if *got.durationMS < wantAtLeast {
		t.Errorf("duration_ms = %d, want at least ~%d (the real elapsed time including the upstream's %v pause, "+
			"not just the time to receive headers)", *got.durationMS, wantAtLeast, pause)
	}
}

// ---- Mid-stream abort: not success, transport error metric incremented ----

// TestMCPProxy_UsageEvent_MidStreamAbort_NotSuccess_ErrorMetricIncremented
// verifies that an upstream connection dropping mid-stream (after headers,
// before the stream would otherwise have ended cleanly) is recorded as
// something other than "success", and increments MCPTransportErrorsTotal
// under the "stream" error_type — as opposed to "call", used for a failure
// discovered before any response reached the caller.
func TestMCPProxy_UsageEvent_MidStreamAbort_NotSuccess_ErrorMetricIncremented(t *testing.T) {
	t.Parallel()

	const firstChunk = "event: message\n" +
		`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}` +
		"\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, firstChunk)
		w.(http.Flusher).Flush()

		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, hijackErr := hj.Hijack()
		if hijackErr != nil {
			return
		}
		conn.Close()
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_UsageEvent_MidStreamAbort?mode=memory&cache=private"
	app, database, keyCache, logger := newMCPProxyAppWithRealUsageLogger(t, dsn, 0)
	org := mustCreateTestOrg(t, database, "usage-mid-abort")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "usage-mid-abort-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	before := testutil.ToFloat64(metrics.MCPTransportErrorsTotal.WithLabelValues(alias, "stream"))

	resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{}}`)
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	_, _ = io.ReadAll(resp.Body) // read until the connection drop; an error here is expected and ignored
	resp.Body.Close()

	logger.Stop()

	got := latestMCPToolCallByAlias(t, database, alias)
	if got.status == "success" {
		t.Errorf("status = %q, want anything other than %q for a stream that broke mid-flight", got.status, "success")
	}

	after := testutil.ToFloat64(metrics.MCPTransportErrorsTotal.WithLabelValues(alias, "stream"))
	if after != before+1 {
		t.Errorf("MCPTransportErrorsTotal{server=%q,error_type=\"stream\"} = %v, want %v (before=%v)", alias, after, before+1, before)
	}
}

// ---- Byte limit exceeded: also an error outcome, not success --------------

// TestMCPProxy_UsageEvent_ByteLimitExceeded_IsErrorOutcome verifies that the
// configured settings.mcp.stream_max_bytes ceiling being exceeded is recorded
// the same way a mid-stream abort is: not "success", and counted under
// MCPTransportErrorsTotal's "stream" error_type — flushingWriter's
// errStreamMaxBytesExceeded takes exactly the same path through the
// SendStreamWriter closure's copyErr branch as any other transport-level
// stream error.
func TestMCPProxy_UsageEvent_ByteLimitExceeded_IsErrorOutcome(t *testing.T) {
	t.Parallel()

	// The upstream sends far more than the small ceiling configured below.
	largePayload := make([]byte, 8192)
	for i := range largePayload {
		largePayload[i] = 'x'
	}
	upstreamBody := "event: message\ndata: " + string(largePayload) + "\n\n"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, upstreamBody)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)

	const maxBytes = 100 // far smaller than upstreamBody

	dsn := "file:TestMCPProxy_UsageEvent_ByteLimitExceeded?mode=memory&cache=private"
	app, database, keyCache, logger := newMCPProxyAppWithRealUsageLogger(t, dsn, maxBytes)
	org := mustCreateTestOrg(t, database, "usage-bytelimit")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "usage-bytelimit-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	before := testutil.ToFloat64(metrics.MCPTransportErrorsTotal.WithLabelValues(alias, "stream"))

	resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if len(got) > maxBytes {
		t.Errorf("client received %d bytes, want at most %d (the configured ceiling)", len(got), maxBytes)
	}

	logger.Stop()

	row := latestMCPToolCallByAlias(t, database, alias)
	if row.status == "success" {
		t.Errorf("status = %q, want anything other than %q when the byte limit was exceeded", row.status, "success")
	}

	after := testutil.ToFloat64(metrics.MCPTransportErrorsTotal.WithLabelValues(alias, "stream"))
	if after != before+1 {
		t.Errorf("MCPTransportErrorsTotal{server=%q,error_type=\"stream\"} = %v, want %v (before=%v)", alias, after, before+1, before)
	}
}

// ---- The two paths that already write immediately are still correct ------

// TestMCPProxy_UsageEvent_NotificationPath_RecordsSuccessImmediately verifies
// the 202 Accepted (notification) path still records its usage event
// immediately — it never enters the SendStreamWriter closure at all, since
// there is no body to stream (see mcp_proxy.go's own comment on this branch).
func TestMCPProxy_UsageEvent_NotificationPath_RecordsSuccessImmediately(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_UsageEvent_NotificationPath?mode=memory&cache=private"
	app, database, keyCache, logger := newMCPProxyAppWithRealUsageLogger(t, dsn, 0)
	org := mustCreateTestOrg(t, database, "usage-notification")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "usage-notification-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusAccepted {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 202; body: %s", resp.StatusCode, raw)
	}

	logger.Stop()

	got := latestMCPToolCallByAlias(t, database, alias)
	if got.status != "success" {
		t.Errorf("status = %q, want %q", got.status, "success")
	}
}

// TestMCPProxy_UsageEvent_ErrorBeforeHeaders_RecordsFailureImmediately
// verifies the pre-first-byte failure path (Forward itself returns a Go
// error — connection refused here) still records its usage event
// immediately: there is no stream to defer to, since Forward never even
// returned a *ForwardResult.
func TestMCPProxy_UsageEvent_ErrorBeforeHeaders_RecordsFailureImmediately(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachableURL := upstream.URL
	upstream.Close() // closed before any request reaches it: connection refused

	dsn := "file:TestMCPProxy_UsageEvent_ErrorBeforeHeaders?mode=memory&cache=private"
	app, database, keyCache, logger := newMCPProxyAppWithRealUsageLogger(t, dsn, 0)
	org := mustCreateTestOrg(t, database, "usage-error-before-headers")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "usage-error-before-headers-server"
	s := createExternalMCPServerPinned(t, database, alias, unreachableURL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadGateway {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 502; body: %s", resp.StatusCode, raw)
	}

	logger.Stop()

	got := latestMCPToolCallByAlias(t, database, alias)
	if got.status == "success" {
		t.Errorf("status = %q, want anything other than %q for an unreachable upstream", got.status, "success")
	}
}
