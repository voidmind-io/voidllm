package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/voidmind-io/voidllm/internal/metrics"
)

// This file covers sendLegacySSEWrapped's own usage-event/metrics recording
// (mcp_proxy.go): "success" is recorded only once the write to the caller —
// c.SendString — has actually returned, not right after the upstream body was
// read, mirroring the fix review finding A1 already made to the genuine
// pass-through path (mcp_proxy_usage_streaming_test.go). Every test here
// wires a real usage.MCPLogger backed by a real in-memory SQLite database, per
// this package's testing philosophy, and calls logger.Stop() to force a
// synchronous flush before querying mcp_tool_calls directly.

// TestMCPProxy_LegacySSEWrap_UsageEvent_DurationIncludesUpstreamRead is the
// legacy-wrap counterpart of
// TestMCPProxy_UsageEvent_RealStreamDuration_NotHeaderArrivalTime: an
// upstream that pauses for a measurable amount of time before ever writing
// its application/json body must produce a usage event whose DurationMS
// reflects that pause, not a near-zero "as soon as something happened"
// duration — sendLegacySSEWrapped's own read of result.Body is where that
// time is actually spent, and start is set before Forward is even called
// (HandleMCPProxy), so the full round trip, read included, must show up.
func TestMCPProxy_LegacySSEWrap_UsageEvent_DurationIncludesUpstreamRead(t *testing.T) {
	t.Parallel()

	const pause = 300 * time.Millisecond
	const upstreamBody = `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		time.Sleep(pause)
		fmt.Fprint(w, upstreamBody)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_LegacySSEWrap_UsageEvent_Duration?mode=memory&cache=private"
	app, database, keyCache, logger := newMCPProxyAppWithRealUsageLogger(t, dsn, 0)
	org := mustCreateTestOrg(t, database, "legacy-sse-usage-duration")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "legacy-sse-usage-duration-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := legacyWrapPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
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

	logger.Stop()

	got := latestMCPToolCallByAlias(t, database, alias)
	if got.status != "success" {
		t.Errorf("status = %q, want %q", got.status, "success")
	}
	if got.durationMS == nil {
		t.Fatal("duration_ms is nil, want a measured duration")
	}
	wantAtLeast := int(pause.Milliseconds()) / 2
	if *got.durationMS < wantAtLeast {
		t.Errorf("duration_ms = %d, want at least ~%d (the real elapsed time including the upstream's %v "+
			"pause before it ever wrote a byte)", *got.durationMS, wantAtLeast, pause)
	}
}

// TestMCPProxy_LegacySSEWrap_UsageEvent_ByteLimitExceeded_IsErrorOutcome_CallType
// verifies that exceeding settings.mcp.stream_max_bytes on the legacy-wrap
// path is recorded as an error outcome — never "success" — and, unlike the
// genuine pass-through path's mid-stream byte-limit failure (recorded under
// error_type="stream", since bytes had already started reaching the caller),
// is recorded under error_type="call": sendLegacySSEWrapped's own read
// happens entirely BEFORE anything is ever written to the caller, so a
// failure discovered there is a call-level failure by definition, not a
// stream-level one.
func TestMCPProxy_LegacySSEWrap_UsageEvent_ByteLimitExceeded_IsErrorOutcome_CallType(t *testing.T) {
	t.Parallel()

	largePayload := make([]byte, 8192)
	for i := range largePayload {
		largePayload[i] = 'z'
	}
	upstreamBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"padding":%q}}`, largePayload)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, upstreamBody)
	}))
	t.Cleanup(upstream.Close)

	const maxBytes = 100 // far smaller than upstreamBody

	dsn := "file:TestMCPProxy_LegacySSEWrap_UsageEvent_ByteLimitExceeded?mode=memory&cache=private"
	app, database, keyCache, logger := newMCPProxyAppWithRealUsageLogger(t, dsn, maxBytes)
	org := mustCreateTestOrg(t, database, "legacy-sse-usage-bytelimit")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "legacy-sse-usage-bytelimit-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	before := testutil.ToFloat64(metrics.MCPTransportErrorsTotal.WithLabelValues(alias, "call"))

	resp := legacyWrapPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
	if resp.StatusCode != fiber.StatusBadGateway {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want 502 (the configured ceiling must reject the oversized body); body: %s", resp.StatusCode, raw)
	}
	resp.Body.Close()

	logger.Stop()

	row := latestMCPToolCallByAlias(t, database, alias)
	if row.status == "success" {
		t.Errorf("status = %q, want anything other than %q when the byte limit was exceeded", row.status, "success")
	}

	after := testutil.ToFloat64(metrics.MCPTransportErrorsTotal.WithLabelValues(alias, "call"))
	if after != before+1 {
		t.Errorf("MCPTransportErrorsTotal{server=%q,error_type=\"call\"} = %v, want %v (before=%v) — a "+
			"byte-limit failure discovered before any byte reached the caller must be classified \"call\", "+
			"not \"stream\"", alias, after, before+1, before)
	}
}
