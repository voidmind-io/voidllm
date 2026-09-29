package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/voidmind-io/voidllm/internal/metrics"
)

// This file covers recordMCPForwardOutcome's own documented exception
// (mcp_proxy.go): a proxied subscriptions/listen call still streams the
// upstream's response through to the caller, still increments
// MCPToolCallsTotal, and still logs a usage.MCPToolCallEvent — exactly like
// every other proxied method — but never contributes an observation to the
// MCPToolCallDurationSeconds histogram, since that histogram's buckets are
// meaningless for a call whose "duration" is really "however long the real
// client chose to keep the stream open" rather than a bounded round trip.
//
// Every test here wires a real usage.MCPLogger backed by a real in-memory
// SQLite database (this package's testing philosophy — no mocks for
// storage), per newMCPProxyAppWithRealUsageLogger (mcp_proxy_usage_streaming_test.go,
// same package), and calls logger.Stop() to force a synchronous flush before
// querying mcp_tool_calls directly.

// mcpToolCallDurationSampleCount returns the number of samples so far
// recorded in metrics.MCPToolCallDurationSeconds for the (alias, method)
// label pair, read directly off the *prometheus.Histogram's own wire-format
// snapshot (dto.Metric) rather than via testutil.ToFloat64 — which panics for
// a histogram, since a histogram exposes no single float value the way a
// counter or gauge does.
func mcpToolCallDurationSampleCount(t *testing.T, alias, method string) uint64 {
	t.Helper()
	obs := metrics.MCPToolCallDurationSeconds.WithLabelValues(alias, method)
	hist, ok := obs.(prometheus.Histogram)
	if !ok {
		t.Fatalf("MCPToolCallDurationSeconds.WithLabelValues(%q, %q) did not return a prometheus.Histogram", alias, method)
	}
	var m dto.Metric
	if err := hist.Write(&m); err != nil {
		t.Fatalf("write histogram metric: %v", err)
	}
	return m.GetHistogram().GetSampleCount()
}

// TestMCPProxy_SubscriptionsListen_StreamsAndRecordsUsage_ButNoDurationSample
// is the direct regression test: a subscriptions/listen call proxied through
// /api/v1/mcp/:alias must still stream the upstream's bytes through
// byte-for-byte, still increment MCPToolCallsTotal under status "success",
// and still produce exactly one mcp_tool_calls row — but the
// MCPToolCallDurationSeconds histogram's sample count for this exact
// (alias, "subscriptions/listen") label pair must be unchanged from before
// the call.
func TestMCPProxy_SubscriptionsListen_StreamsAndRecordsUsage_ButNoDurationSample(t *testing.T) {
	t.Parallel()

	const ackEvent = "event: message\n" +
		`data: {"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"voidllm-listen"},"notifications":{"toolsListChanged":true}}}` +
		"\n\n"
	const gracefulEndEvent = "event: message\n" +
		`data: {"jsonrpc":"2.0","id":"voidllm-listen","result":{}}` +
		"\n\n"
	wantBody := ackEvent + gracefulEndEvent

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, wantBody)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_SubscriptionsListen_NoDurationSample?mode=memory&cache=private"
	app, database, keyCache, logger := newMCPProxyAppWithRealUsageLogger(t, dsn, 0)
	org := mustCreateTestOrg(t, database, "listen-metrics")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "listen-metrics-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	const method = "subscriptions/listen"
	beforeCounter := testutil.ToFloat64(metrics.MCPToolCallsTotal.WithLabelValues(alias, method, "success"))
	beforeSamples := mcpToolCallDurationSampleCount(t, alias, method)

	resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"notifications":{"toolsListChanged":true}}}`)
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}
	gotBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(gotBody) != wantBody {
		t.Errorf("body = %q, want the upstream's bytes streamed through unchanged: %q", gotBody, wantBody)
	}

	logger.Stop() // force a synchronous flush before querying mcp_tool_calls

	afterCounter := testutil.ToFloat64(metrics.MCPToolCallsTotal.WithLabelValues(alias, method, "success"))
	if afterCounter != beforeCounter+1 {
		t.Errorf("MCPToolCallsTotal{%s,%s,success} = %v, want %v (before + 1)", alias, method, afterCounter, beforeCounter+1)
	}

	afterSamples := mcpToolCallDurationSampleCount(t, alias, method)
	if afterSamples != beforeSamples {
		t.Errorf("MCPToolCallDurationSeconds sample count for {%s,%s} = %d, want unchanged at %d (subscriptions/listen must never contribute a duration sample)", alias, method, afterSamples, beforeSamples)
	}

	got := latestMCPToolCallByAlias(t, database, alias)
	if got.status != "success" {
		t.Errorf("mcp_tool_calls.status = %q, want %q", got.status, "success")
	}
	if got.durationMS == nil {
		t.Error("mcp_tool_calls.duration_ms is nil, want a recorded duration (the usage event's own duration tracking is unaffected by the histogram skip)")
	}
}
