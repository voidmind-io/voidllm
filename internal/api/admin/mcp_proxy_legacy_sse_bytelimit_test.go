package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
)

// This file covers sendLegacySSEWrapped's own settings.mcp.stream_max_bytes
// enforcement (mcp_proxy.go). Before this fix, sendLegacySSEWrapped read the
// upstream's application/json body via a hard-coded, unconfigurable 10 MiB
// io.ReadAll ceiling — an operator who set stream_max_bytes lower (1 KiB,
// say) had that ceiling silently bypassed on exactly this branch
// (docs/mcp-v2.md, review finding A1). This is the legacy-caller counterpart
// of mcp_proxy_bytelimit_test.go, which covers the SAME setting on the
// genuine transparent pass-through path (flushingWriter/io.Copy).
//
// A "legacy caller" here means Accept: text/event-stream ONLY (never also
// application/json) — see acceptsOnlySSE's doc — which is the one Accept
// shape that routes a JSON upstream response through sendLegacySSEWrapped at
// all (mcp_proxy.go's own branch above the call site).

// legacyWrapPost sends a POST to /api/v1/mcp/:alias with an Accept header
// that names text/event-stream EXCLUSIVELY, the one shape that routes an
// upstream application/json response through sendLegacySSEWrapped instead of
// the genuine pass-through path.
func legacyWrapPost(t *testing.T, app *fiber.App, alias, key, body string) *http.Response {
	t.Helper()
	return proxyPostWithHeaders(t, app, alias, key, body, map[string]string{"Accept": "text/event-stream"})
}

// TestMCPProxy_LegacySSEWrap_StreamMaxBytes_Exceeded verifies that a
// configured stream_max_bytes ceiling — not the old hard-coded 10 MiB
// constant — is what sendLegacySSEWrapped enforces: a small ceiling (1 KiB)
// against a 2 KiB upstream JSON body must fail the call, not silently let
// the oversized body through because it is still comfortably under 10 MiB.
func TestMCPProxy_LegacySSEWrap_StreamMaxBytes_Exceeded(t *testing.T) {
	t.Parallel()

	const maxBytes = 1024 // 1 KiB — far below the old hard-coded 10 MiB ceiling

	// A 2 KiB JSON body: comfortably under the old 10 MiB constant (so the
	// pre-fix implementation would have let it through untouched), but well
	// over the 1 KiB ceiling configured below.
	padding := strings.Repeat("x", 2048)
	upstreamBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"padding":%q}}`, padding)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, upstreamBody)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_LegacySSEWrap_StreamMaxBytes_Exceeded?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyAppWithMaxBytes(t, dsn, maxBytes)
	org := mustCreateTestOrg(t, database, "legacy-sse-bytelimit-exceeded")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "legacy-sse-bytelimit-exceeded-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := legacyWrapPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadGateway {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 502 (the 1 KiB ceiling must reject a 2 KiB upstream body); body: %s", resp.StatusCode, raw)
	}
}

// TestMCPProxy_LegacySSEWrap_StreamMaxBytes_Zero_MeansUnbounded verifies the
// explicit-zero case: a 2 KiB upstream body — the SAME size the ceiling test
// above rejects at 1 KiB — must be wrapped and delivered in full when
// MCPStreamMaxBytes is explicitly 0. "Unbounded" here means "not bounded by
// h.MCPStreamMaxBytes", exactly as it does everywhere else on this proxy
// (config.MCPConfig.StreamMaxBytes' doc) — it does not mean sendLegacySSEWrapped
// itself imposes no ceiling at all: that function additionally enforces its
// own fixed legacySSEWrapReadCap (10 MiB) unconditionally, precisely BECAUSE
// this branch cannot honor a genuinely unbounded read the way the streaming
// pass-through path can (see legacySSEWrapReadCap's doc, docs/mcp-v2.md
// Punkt C). This test's 2 KiB body stays far under that fixed cap, so it is
// not exercised here — TestMCPProxy_LegacySSEWrap_HardCap_AppliesEvenWhenZero
// (mcp_proxy_legacy_sse_hardcap_test.go) covers that boundary directly.
func TestMCPProxy_LegacySSEWrap_StreamMaxBytes_Zero_MeansUnbounded(t *testing.T) {
	t.Parallel()

	padding := strings.Repeat("y", 2048)
	upstreamBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"result":{"padding":%q}}`, padding)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, upstreamBody)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_LegacySSEWrap_StreamMaxBytes_Zero?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyAppWithMaxBytes(t, dsn, 0)
	org := mustCreateTestOrg(t, database, "legacy-sse-bytelimit-zero")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "legacy-sse-bytelimit-zero-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := legacyWrapPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v — MCPStreamMaxBytes=0 must never truncate", err)
	}

	want := admin.FormatSSEMessage([]byte(upstreamBody))
	if string(raw) != want {
		t.Errorf("body = %q, want the full upstream body wrapped as a single SSE message: %q", raw, want)
	}
}

// TestMCPProxy_LegacySSEWrap_BelowLimit_WrappedNormally is the "well under
// the ceiling" control case: a small upstream body under a generous ceiling
// must be wrapped and delivered exactly as sendLegacySSEWrapped/
// formatSSEMessage always did, with nothing about the ceiling enforcement
// itself observable in the successful path.
func TestMCPProxy_LegacySSEWrap_BelowLimit_WrappedNormally(t *testing.T) {
	t.Parallel()

	const compactBody = `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`
	const maxBytes = 65536 // far larger than compactBody

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, compactBody)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_LegacySSEWrap_BelowLimit?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyAppWithMaxBytes(t, dsn, maxBytes)
	org := mustCreateTestOrg(t, database, "legacy-sse-below-limit")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "legacy-sse-below-limit-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := legacyWrapPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	want := admin.FormatSSEMessage([]byte(compactBody))
	if string(raw) != want {
		t.Errorf("body = %q, want %q", raw, want)
	}
}
