package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// This file covers a gap left open alongside the hop-by-hop fix in
// mcp_proxy_hopbyhop_session_test.go: that fix taught HandleMCPProxy to skip
// recording a session the response mirror itself withholds from the caller,
// but the check it added ran unconditionally, before ANY branch on
// result.Status — including the two branches that return before
// h.mirrorMCPResponseHeaders (nee copyMCPResponseHeaders) ever runs at all:
// 202 Accepted (a notification acknowledgment, no body) and a 3xx from the
// upstream (which this gateway refuses to follow and turns into a 502). On
// both of those paths NO header is ever mirrored to the caller — not just a
// hop-by-hop-marked one — yet the pre-fix code still recorded whatever
// Mcp-Session-Id the upstream response carried, straight off
// result.Header, before reaching either early return. A test that only
// checks the response mirror on these two paths would stay green even if
// this half of the fix were reverted, exactly as the hopbyhop file's own
// doc warns for its own case: the actual proof is a follow-up request
// presenting that exact session ID, and whether the upstream ends up seeing
// it relayed.
func TestMCPProxy_EarlyReturnPaths_SessionNeitherMirroredNorRecorded(t *testing.T) {
	t.Parallel()

	const mintedSession = "early-return-session-1"

	tests := []struct {
		name           string
		upstreamStatus int
	}{
		{name: "202 Accepted - notification acknowledgment carries no body", upstreamStatus: fiber.StatusAccepted},
		{name: "302 Found - a redirect this gateway refuses to follow", upstreamStatus: fiber.StatusFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			var seenInbound []string

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				inbound := r.Header.Get("Mcp-Session-Id")
				mu.Lock()
				seenInbound = append(seenInbound, inbound)
				mu.Unlock()

				// The upstream mints a session on every request in this test —
				// including the follow-up — so that a relayed follow-up would be
				// indistinguishable from a rejected one by status code alone;
				// only seenInbound (what the UPSTREAM received) can tell the two
				// apart, which is the whole point (see this file's own doc).
				w.Header().Set("Mcp-Session-Id", mintedSession)
				if tc.upstreamStatus == fiber.StatusFound {
					w.Header().Set("Location", "https://example.invalid/elsewhere")
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstreamStatus)
				if tc.upstreamStatus != fiber.StatusAccepted {
					fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
				}
			}))
			t.Cleanup(upstream.Close)

			slug := sanitizeTestName(tc.name)
			dsn := "file:TestMCPProxy_EarlyReturnPaths_" + slug + "?mode=memory&cache=private"
			app, database, keyCache, transportCache := setupMCPProxyAppWithTransportCache(t, dsn)
			org := mustCreateTestOrg(t, database, "early-return-"+slug)
			key := addMCPTestKey(t, keyCache, org.ID)

			alias := "early-return-server-" + slug
			serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{serverID}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}
			loadMCPTransportCache(t, database, transportCache)

			const body = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

			// Request 1: no inbound session — the upstream mints one on an
			// early-return response the caller never actually gets a header
			// mirror for.
			resp1 := proxyPost(t, app, alias, key, body)
			mirrored := resp1.Header.Get("Mcp-Session-Id")
			raw, _ := io.ReadAll(resp1.Body)
			resp1.Body.Close()

			if mirrored != "" {
				t.Errorf("client received Mcp-Session-Id = %q, want empty — HandleMCPProxy never mirrors any "+
					"header on this response path; body: %s", mirrored, raw)
			}

			// Request 2: the caller presents the upstream's own, real session
			// ID. It must reach the upstream empty (dropped as unrecognized),
			// exactly like a fabricated guess — recording it despite never
			// having mirrored it to the caller is the bug this test exists to
			// catch.
			resp2 := proxyPostWithHeaders(t, app, alias, key, body, map[string]string{"Mcp-Session-Id": mintedSession})
			io.Copy(io.Discard, resp2.Body) //nolint:errcheck // draining is enough; status/body content is not under test here
			resp2.Body.Close()

			mu.Lock()
			defer mu.Unlock()
			if len(seenInbound) != 2 {
				t.Fatalf("upstream saw %d requests, want 2", len(seenInbound))
			}
			if seenInbound[1] != "" {
				t.Errorf("upstream saw Mcp-Session-Id = %q on the follow-up, want empty — a session never mirrored "+
					"to the caller must never be recorded as known either", seenInbound[1])
			}
		})
	}
}
