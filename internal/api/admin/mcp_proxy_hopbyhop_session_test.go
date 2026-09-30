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

// This file covers docs/mcp-v2.md Fund 2: an upstream response that names
// Mcp-Session-Id as one of its own Connection field's connection-options
// (RFC 7230 §6.1: "Connection: Mcp-Session-Id") is declaring that header
// connection-specific for this one response. copyMCPResponseHeaders
// (mcp_headers.go) already refuses to mirror such a header back to the
// caller — that half was already correct before this fix. The other half,
// and the actual point of this fix, is that h.MCPSessionRegistry.Record must
// ALSO be skipped for it: a session the caller was never told about must
// never be remembered as "known" on the caller's behalf either, or it would
// sit in the registry unreachable by any legitimate follow-up request (the
// caller has nothing to send back) while still being available to leak to
// anyone who happens to guess or otherwise learn the same value.
//
// A test that only checks the response mirror would still be green even if
// the recording half of the fix were reverted — that is exactly the
// distinction this file's table drives out via each case's second half: the
// upstream's own record of what it actually saw on a follow-up request
// carrying that exact session ID.

// TestMCPProxy_HopByHopMarkedSession_NeitherMirroredNorRecorded is
// table-driven over whether the upstream marks its own Mcp-Session-Id
// response header as connection-specific via "Connection: Mcp-Session-Id":
//
//   - marked: the client must never see the session mirrored back, AND a
//     follow-up request from the same caller presenting that exact session
//     ID must be dropped before it ever reaches the upstream (unrecognized,
//     exactly like a fabricated guess).
//   - not marked (the counter-proof): the session IS mirrored back to the
//     client, AND a follow-up request presenting it is recognized and
//     relayed to the upstream unchanged.
func TestMCPProxy_HopByHopMarkedSession_NeitherMirroredNorRecorded(t *testing.T) {
	t.Parallel()

	const mintedSession = "hopbyhop-fund2-session-1"

	tests := []struct {
		name         string
		markHopByHop bool
	}{
		{
			name:         "Connection: Mcp-Session-Id hides the session - never mirrored, never recorded",
			markHopByHop: true,
		},
		{
			name:         "no Connection marking - mirrored to the client and recorded (counter-proof)",
			markHopByHop: false,
		},
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

				w.Header().Set("Content-Type", "application/json")
				if inbound == "" {
					w.Header().Set("Mcp-Session-Id", mintedSession)
					if tc.markHopByHop {
						w.Header().Set("Connection", "Mcp-Session-Id")
					}
				}
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			}))
			t.Cleanup(upstream.Close)

			slug := sanitizeTestName(tc.name)
			dsn := "file:TestMCPProxy_HopByHopMarkedSession_" + slug + "?mode=memory&cache=private"
			app, database, keyCache, transportCache := setupMCPProxyAppWithTransportCache(t, dsn)
			org := mustCreateTestOrg(t, database, "hopbyhop-fund2-"+slug)
			key := addMCPTestKey(t, keyCache, org.ID)

			alias := "hopbyhop-fund2-server-" + slug
			serverID := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2025-03-26")
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{serverID}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}
			loadMCPTransportCache(t, database, transportCache)

			const body = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

			// Request 1: no inbound session — the upstream mints one and,
			// depending on the case, marks it hop-by-hop.
			resp1 := proxyPost(t, app, alias, key, body)
			if resp1.StatusCode != fiber.StatusOK {
				raw, _ := io.ReadAll(resp1.Body)
				resp1.Body.Close()
				t.Fatalf("first request: status = %d, want 200; body: %s", resp1.StatusCode, raw)
			}
			mirrored := resp1.Header.Get("Mcp-Session-Id")
			resp1.Body.Close()

			if tc.markHopByHop {
				if mirrored != "" {
					t.Errorf("client received Mcp-Session-Id = %q, want empty — a hop-by-hop-marked session must "+
						"never be mirrored back to the caller", mirrored)
				}
			} else if mirrored != mintedSession {
				t.Fatalf("client received Mcp-Session-Id = %q, want %q — counter-proof setup is broken", mirrored, mintedSession)
			}

			// Request 2: the caller presents the upstream's own, real session
			// ID. Whether it reaches the upstream depends entirely on whether
			// request 1's response caused it to be RECORDED — never on
			// whether the client happened to learn it (a hop-by-hop-marked
			// session must be forgotten by the registry even though this
			// test, unlike a real attacker, already knows its exact value).
			resp2 := proxyPostWithHeaders(t, app, alias, key, body, map[string]string{"Mcp-Session-Id": mintedSession})
			if resp2.StatusCode != fiber.StatusOK {
				raw, _ := io.ReadAll(resp2.Body)
				resp2.Body.Close()
				t.Fatalf("second request: status = %d, want 200 (dropped, not rejected); body: %s", resp2.StatusCode, raw)
			}
			resp2.Body.Close()

			mu.Lock()
			defer mu.Unlock()
			if len(seenInbound) != 2 {
				t.Fatalf("upstream saw %d requests, want 2", len(seenInbound))
			}
			if tc.markHopByHop {
				if seenInbound[1] != "" {
					t.Errorf("upstream saw Mcp-Session-Id = %q on the follow-up, want empty — a hop-by-hop-marked "+
						"session must never be recorded as known, so a caller presenting it is treated exactly "+
						"like a fabricated guess", seenInbound[1])
				}
			} else if seenInbound[1] != mintedSession {
				t.Errorf("upstream saw Mcp-Session-Id = %q on the follow-up, want %q — a normally-mirrored "+
					"session must be recorded and relayed on a caller's follow-up request", seenInbound[1], mintedSession)
			}
		})
	}
}
