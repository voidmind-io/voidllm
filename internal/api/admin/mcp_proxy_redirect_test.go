package admin_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// This file covers HandleMCPProxy's own doc above its 3xx branch (review
// finding C5): mcp.NewHTTPTransport's CheckRedirect already refuses to
// follow a redirect itself (http.ErrUseLastResponse), so a 3xx response
// reaches HandleMCPProxy as an ordinary — if unusual — upstream response.
// Handing a 3xx straight to the caller of an MCP gateway would leak an
// upstream implementation detail (a Location header) that a JSON-RPC-only
// caller has no way to usefully act on, so HandleMCPProxy treats it exactly
// like any other upstream/transport problem: close the body, record a
// failure, answer 502.

// TestMCPProxy_UpstreamRedirect_BecomesBadGateway is table-driven across the
// two ordinary redirect statuses (301 and 302) and verifies each becomes a
// 502 at the caller, with the response body closed (no half-open stream) and
// a JSON-RPC error body, not the raw redirect.
func TestMCPProxy_UpstreamRedirect_BecomesBadGateway(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{name: "301 Moved Permanently", status: http.StatusMovedPermanently},
		{name: "302 Found", status: http.StatusFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Location", "https://attacker.example.com/mcp")
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(upstream.Close)

			alias := "redirect-" + sanitizeTestName(tc.name)
			dsn := "file:TestMCPProxy_UpstreamRedirect_" + sanitizeTestName(tc.name) + "?mode=memory&cache=private"
			app, database, keyCache := setupMCPProxyApp(t, dsn)
			org := mustCreateTestOrg(t, database, alias)
			memberKey := addMCPTestKey(t, keyCache, org.ID)

			s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusBadGateway {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 502 (a redirect must never reach the caller as-is); body: %s", resp.StatusCode, raw)
			}
			if got := resp.Header.Get("Location"); got != "" {
				t.Errorf("Location = %q, want absent — the upstream's redirect target must never leak to the caller", got)
			}

			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body: %v — a redirect turned into an error must still be a COMPLETE response body, "+
					"not a half-open stream", err)
			}
			if len(raw) == 0 {
				t.Error("body is empty, want a JSON-RPC error body")
			}
		})
	}
}
