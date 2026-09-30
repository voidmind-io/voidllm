package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

// This file covers copyMCPResponseHeaders' RFC 7230 §6.1 handling
// (mcp_headers.go, docs/mcp-v2.md review finding C): "for each
// connection-option in this field, remove any header field(s) from the
// message with the same name as the connection-option". An upstream naming
// an otherwise-allowlisted header in its own Connection field value is
// declaring THAT header connection-specific for this one response, which
// mcpConnectionOptions parses and copyMCPResponseHeaders then honors —
// exactly like the fixed, well-known hop-by-hop set (mcpHopByHopHeaders), but
// dynamic and upstream-controlled per response.

// TestMCPProxy_ConnectionHeader_DynamicallyHidesNamedHeader is table-driven
// coverage for every shape RFC 7230 §6.1 requires copyMCPResponseHeaders to
// handle: a single connection-option, one naming an X-RateLimit-* header
// (matched by prefix, not the fixed allowlist), multiple Connection
// field-lines, a comma-separated list within one field-line, and
// case-insensitive matching of both the option value and the target header's
// own name.
func TestMCPProxy_ConnectionHeader_DynamicallyHidesNamedHeader(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		setHeaders  func(w http.ResponseWriter)
		wantAbsent  []string            // canonical header names that must NOT reach the caller
		wantPresent map[string][]string // canonical header name -> exact wanted values, for headers that must still arrive
	}{
		{
			// The scenario named explicitly in the task: the upstream
			// self-declares its own Cache-Control as connection-specific for
			// this one response, even though Cache-Control is normally
			// allowlisted (CacheableResult hints, docs/mcp-v2.md §5).
			name: "Connection: Cache-Control hides the upstream's own otherwise-allowlisted Cache-Control",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("Connection", "Cache-Control")
				w.Header().Set("Cache-Control", "public, max-age=31536000")
			},
			wantAbsent: []string{"Cache-Control"},
		},
		{
			// X-RateLimit-* is matched by PREFIX, not the fixed
			// allowedMCPResponseHeaders slice — this proves
			// mcpConnectionOptions' filtering applies to that dynamic prefix
			// match too, not only the fixed allowlist entries.
			name: "Connection: X-RateLimit-Limit hides that one X-RateLimit-* header, others still pass",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("Connection", "X-RateLimit-Limit")
				w.Header().Set("X-RateLimit-Limit", "100")
				w.Header().Set("X-RateLimit-Remaining", "42")
			},
			wantAbsent:  []string{"X-Ratelimit-Limit"},
			wantPresent: map[string][]string{"X-Ratelimit-Remaining": {"42"}},
		},
		{
			// Multiple Connection field-lines AND a comma-separated list
			// within one of them, together: net/http.Header can carry more
			// than one Connection field-line for the same response, and RFC
			// 7230 §6.1 itself allows several connection-options
			// comma-separated in a single field-line.
			name: "multiple Connection field-lines, one of them a comma-separated list, hide multiple headers",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Add("Connection", "Cache-Control, X-RateLimit-Reset")
				w.Header().Add("Connection", "Content-Language")
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("X-RateLimit-Reset", "60")
				w.Header().Set("Content-Language", "en-US")
				w.Header().Set("Retry-After", "30") // untouched control header
			},
			wantAbsent:  []string{"Cache-Control", "X-Ratelimit-Reset", "Content-Language"},
			wantPresent: map[string][]string{"Retry-After": {"30"}},
		},
		{
			// Case-insensitivity of the connection-option's OWN casing in the
			// Connection header value.
			name: "lowercase connection-option value still matches the canonical header name",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("Connection", "cache-control")
				w.Header().Set("Cache-Control", "no-store")
			},
			wantAbsent: []string{"Cache-Control"},
		},
		{
			name: "fully upper-cased connection-option value still matches",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("Connection", "CACHE-CONTROL")
				w.Header().Set("Cache-Control", "no-store")
			},
			wantAbsent: []string{"Cache-Control"},
		},
		{
			// Whitespace around commas in the connection-option list must not
			// prevent a match.
			name: "whitespace around comma-separated connection-options",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("Connection", "  Cache-Control ,  Content-Language  ")
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Content-Language", "en-US")
			},
			wantAbsent: []string{"Cache-Control", "Content-Language"},
		},
		{
			// The negative control: naming a header in Connection that the
			// upstream never actually set has no effect on anything else —
			// other allowlisted headers are unaffected.
			name: "naming an unrelated header in Connection does not hide unrelated allowlisted headers",
			setHeaders: func(w http.ResponseWriter) {
				w.Header().Set("Connection", "X-Something-Never-Set")
				w.Header().Set("Cache-Control", "public, max-age=60")
				w.Header().Set("Retry-After", "5")
			},
			wantPresent: map[string][]string{
				"Cache-Control": {"public, max-age=60"},
				"Retry-After":   {"5"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				tc.setHeaders(w)
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
			}))
			t.Cleanup(upstream.Close)

			alias := "conn-hbh-" + sanitizeTestName(tc.name)
			dsn := "file:TestMCPProxy_ConnectionHeader_" + sanitizeTestName(tc.name) + "?mode=memory&cache=private"
			app, database, keyCache := setupMCPProxyApp(t, dsn)
			org := mustCreateTestOrg(t, database, alias)
			memberKey := addMCPTestKey(t, keyCache, org.ID)

			s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
			}

			for _, name := range tc.wantAbsent {
				if got := resp.Header.Values(name); len(got) != 0 {
					t.Errorf("header %q = %v, want absent entirely (named as a connection-option by the "+
						"upstream's own Connection header, RFC 7230 §6.1)", name, got)
				}
			}
			for name, want := range tc.wantPresent {
				got := resp.Header.Values(name)
				if len(got) != len(want) {
					t.Errorf("header %q = %v, want %v", name, got, want)
					continue
				}
				for i := range want {
					if got[i] != want[i] {
						t.Errorf("header %q = %v, want %v", name, got, want)
						break
					}
				}
			}

			// Connection itself is always hop-by-hop and must never reach the
			// caller, regardless of which connection-options it names.
			if got := resp.Header.Values("Connection"); len(got) != 0 {
				t.Errorf("Connection header = %v, want absent entirely (always hop-by-hop)", got)
			}
		})
	}
}
