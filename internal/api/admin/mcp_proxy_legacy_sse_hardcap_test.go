package admin_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// This file covers legacySSEWrapReadCap (mcp_proxy.go): the fixed 10 MiB hard
// ceiling sendLegacySSEWrapped enforces on its upstream body read regardless
// of settings.mcp.stream_max_bytes, including when that setting is explicitly
// 0 ("unbounded" everywhere else on this proxy —
// TestMCPProxy_LegacySSEWrap_StreamMaxBytes_Zero_MeansUnbounded in
// mcp_proxy_legacy_sse_bytelimit_test.go covers that ordinary case, at a body
// size far below this cap). Before this fix, stream_max_bytes=0 turned
// sendLegacySSEWrapped's io.ReadAll into a genuinely unbounded read of
// whatever an upstream — or an attacker controlling one — chose to send
// (docs/mcp-v2.md, Punkt C).

// oversizedLegacyMCPServer starts an httptest.Server that answers any request
// with a single application/json body of exactly n bytes: an opening
// `{"jsonrpc":"2.0","id":1,"result":{"padding":"` prefix, n-worth of 'x'
// padding, and a closing `"}}` suffix. The body is not valid enough to matter
// here — sendLegacySSEWrapped's byte-count enforcement runs before anything
// ever tries to parse it as JSON-RPC.
func oversizedLegacyMCPServer(t *testing.T, totalBytes int) *httptest.Server {
	t.Helper()
	const prefix = `{"jsonrpc":"2.0","id":1,"result":{"padding":"`
	const suffix = `"}}`
	padding := totalBytes - len(prefix) - len(suffix)
	if padding < 0 {
		t.Fatalf("oversizedLegacyMCPServer: totalBytes %d too small for prefix/suffix overhead", totalBytes)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, prefix)
		_, _ = io.Copy(w, io.LimitReader(strings.NewReader(strings.Repeat("x", padding+1)), int64(padding)))
		_, _ = io.WriteString(w, suffix)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestMCPProxy_LegacySSEWrap_HardCap_AppliesEvenWhenStreamMaxBytesIsZero is
// the direct regression test for Punkt C: with stream_max_bytes explicitly 0,
// an upstream body that exceeds legacySSEWrapReadCap (10 MiB) must still be
// rejected — the hard cap applies unconditionally on this branch, unlike the
// genuine streaming pass-through path, where 0 truly means unbounded.
func TestMCPProxy_LegacySSEWrap_HardCap_AppliesEvenWhenStreamMaxBytesIsZero(t *testing.T) {
	t.Parallel()

	const oversized = (10 << 20) + 1024 // just over the 10 MiB hard cap

	upstream := oversizedLegacyMCPServer(t, oversized)

	dsn := "file:TestMCPProxy_LegacySSEWrap_HardCap_AppliesEvenWhenStreamMaxBytesIsZero?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyAppWithMaxBytes(t, dsn, 0)
	org := mustCreateTestOrg(t, database, "legacy-sse-hardcap-zero")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "legacy-sse-hardcap-zero-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := legacyWrapPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadGateway {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 502 (legacySSEWrapReadCap must reject a body over 10 MiB even when "+
			"stream_max_bytes=0); body: %s", resp.StatusCode, raw)
	}
}

// TestMCPProxy_LegacySSEWrap_HardCap_ConfiguredLimitBelowCapStillWins verifies
// that legacySSEWrapReadCap is a ceiling on top of h.MCPStreamMaxBytes, never
// a floor that raises a stricter configured value: with stream_max_bytes set
// well below the 10 MiB hard cap, that smaller configured value is still the
// one that rejects an oversized body — the hard cap never has to be reached
// for the smaller, explicit ceiling to already apply.
func TestMCPProxy_LegacySSEWrap_HardCap_ConfiguredLimitBelowCapStillWins(t *testing.T) {
	t.Parallel()

	const maxBytes = 1024 // far below the 10 MiB hard cap

	padding := strings.Repeat("x", 2048) // 2 KiB — under the hard cap, over maxBytes
	upstreamBody := `{"jsonrpc":"2.0","id":1,"result":{"padding":"` + padding + `"}}`

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_LegacySSEWrap_HardCap_ConfiguredLimitBelowCapStillWins?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyAppWithMaxBytes(t, dsn, maxBytes)
	org := mustCreateTestOrg(t, database, "legacy-sse-hardcap-below")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "legacy-sse-hardcap-below-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := legacyWrapPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadGateway {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 502 (the configured 1 KiB ceiling, well below the 10 MiB hard cap, "+
			"must still reject a 2 KiB body); body: %s", resp.StatusCode, raw)
	}
}
