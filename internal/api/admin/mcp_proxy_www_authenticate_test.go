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

// This file covers filterWWWAuthenticateBearer/filterWWWAuthenticateChallenges
// (mcp_headers.go, docs/mcp-v2.md review finding F): Bearer is the only
// auth-scheme MCP authorization actually uses, so copyMCPResponseHeaders
// mirrors ONLY the Bearer challenge(s) in an upstream's WWW-Authenticate
// response header, dropping every other scheme (Basic, Digest, Negotiate,
// ...) outright. Mirroring a Basic challenge verbatim would make a
// browser-based caller sitting in front of this proxy show its own native
// HTTP Basic credential dialog for a realm the caller's user has no way to
// know is not VoidLLM's own.

// TestMCPProxy_WWWAuthenticate_OnlyBearerChallengesSurvive is table-driven
// coverage for every shape filterWWWAuthenticateChallenges documents it must
// handle: a lone Bearer challenge passes through, a lone non-Bearer challenge
// is dropped entirely (the header does not reach the caller at all), a
// mixed multi-challenge value keeps only the Bearer part, a comma INSIDE a
// quoted parameter is not mistaken for a challenge boundary, and the scheme
// token itself is matched case-insensitively.
func TestMCPProxy_WWWAuthenticate_OnlyBearerChallengesSurvive(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		upstreamValue  string
		wantHeaderSeen bool   // false means the header must not reach the caller at all
		wantValue      string // checked only when wantHeaderSeen is true
	}{
		{
			name:           "a lone Bearer challenge passes through unchanged",
			upstreamValue:  `Bearer realm="mcp"`,
			wantHeaderSeen: true,
			wantValue:      `Bearer realm="mcp"`,
		},
		{
			// A browser-based caller in front of this proxy would otherwise
			// show its own native HTTP Basic credential dialog for a realm
			// the caller's user cannot tell apart from VoidLLM's own.
			name:           "a lone Basic challenge is dropped entirely, header absent",
			upstreamValue:  `Basic realm="evil"`,
			wantHeaderSeen: false,
		},
		{
			name:           "a lone Negotiate challenge is dropped entirely, header absent",
			upstreamValue:  `Negotiate`,
			wantHeaderSeen: false,
		},
		{
			name:           "a lone Digest challenge is dropped entirely, header absent",
			upstreamValue:  `Digest realm="mcp", nonce="abc123"`,
			wantHeaderSeen: false,
		},
		{
			// RFC 7235 §4.1 allows more than one challenge, comma-separated,
			// in a single header value: only the Bearer segment must survive.
			name:           "multiple challenges in one header: only the Bearer challenge survives",
			upstreamValue:  `Basic realm="evil", Bearer realm="mcp", Negotiate`,
			wantHeaderSeen: true,
			wantValue:      `Bearer realm="mcp"`,
		},
		{
			name:           "Bearer challenge with several of its own auth-params, all preserved",
			upstreamValue:  `Bearer realm="mcp", error="invalid_token", error_description="the token expired"`,
			wantHeaderSeen: true,
			wantValue:      `Bearer realm="mcp", error="invalid_token", error_description="the token expired"`,
		},
		{
			// The comma inside the quoted realm value must NOT be mistaken
			// for a challenge boundary — the whole quoted string, comma
			// included, belongs to the Bearer challenge's own realm param.
			name:           "a comma INSIDE a quoted parameter is not a challenge boundary",
			upstreamValue:  `Bearer realm="a,b"`,
			wantHeaderSeen: true,
			wantValue:      `Bearer realm="a,b"`,
		},
		{
			// The same, but with an attacker-shaped payload trying to smuggle
			// a second, non-Bearer-looking segment inside a quoted value.
			name:           "a comma inside a quoted parameter cannot smuggle a fake extra challenge",
			upstreamValue:  `Bearer realm="mcp, Basic realm=fake"`,
			wantHeaderSeen: true,
			wantValue:      `Bearer realm="mcp, Basic realm=fake"`,
		},
		{
			name:           "lowercase bearer scheme token is still recognized (case-insensitive)",
			upstreamValue:  `bearer realm="mcp"`,
			wantHeaderSeen: true,
			wantValue:      `bearer realm="mcp"`,
		},
		{
			name:           "fully upper-cased BEARER scheme token is still recognized",
			upstreamValue:  `BEARER realm="mcp"`,
			wantHeaderSeen: true,
			wantValue:      `BEARER realm="mcp"`,
		},
		{
			name:           "mixed-case scheme among multiple challenges: only the (mixed-case) Bearer one survives",
			upstreamValue:  `Basic realm="evil", BeArEr realm="mcp"`,
			wantHeaderSeen: true,
			wantValue:      `BeArEr realm="mcp"`,
		},
		{
			name:           "Bearer with a bare token68 credential form (no auth-params) passes through",
			upstreamValue:  `Bearer`,
			wantHeaderSeen: true,
			wantValue:      `Bearer`,
		},
		{
			// RFC 7230 §3.2.6 quoted-pair: a backslash inside a
			// quoted-string escapes exactly the character after it. Before
			// splitOutsideQuotes handled this, an escaped quote toggled
			// inQuotes exactly like a real one, so this comma — still
			// inside the SAME quoted realm value per RFC — was misread as
			// a challenge boundary, silently truncating the value at that
			// point instead of preserving it whole.
			name:           "an escaped quote inside a quoted parameter does not end the quoted string early",
			upstreamValue:  `Bearer realm="say \"hi, trap"`,
			wantHeaderSeen: true,
			wantValue:      `Bearer realm="say \"hi, trap"`,
		},
		{
			// Two escaped quotes bracketing a word, followed by the TRUE
			// closing quote and then a second, real challenge. Verifies
			// the fix closes the quoted-string at the right place — not
			// too early (which would wrongly split the realm value) and
			// not too late (which would wrongly swallow the Basic
			// challenge that follows into the Bearer one's own value).
			name:           "escaped quotes around a phrase, followed by a real second challenge after the true closing quote",
			upstreamValue:  `Bearer realm="say \"hi\"", Basic realm="evil"`,
			wantHeaderSeen: true,
			wantValue:      `Bearer realm="say \"hi\""`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("WWW-Authenticate", tc.upstreamValue)
				w.WriteHeader(http.StatusUnauthorized)
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"unauthorized"}}`)
			}))
			t.Cleanup(upstream.Close)

			alias := "wwwauth-" + sanitizeTestName(tc.name)
			dsn := "file:TestMCPProxy_WWWAuthenticate_" + sanitizeTestName(tc.name) + "?mode=memory&cache=private"
			app, database, keyCache := setupMCPProxyApp(t, dsn)
			org := mustCreateTestOrg(t, database, alias)
			memberKey := addMCPTestKey(t, keyCache, org.ID)

			s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
			if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
				t.Fatalf("SetOrgMCPAccess: %v", err)
			}

			resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusUnauthorized {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 401; body: %s", resp.StatusCode, raw)
			}

			got := resp.Header.Values("Www-Authenticate")
			if !tc.wantHeaderSeen {
				if len(got) != 0 {
					t.Errorf("WWW-Authenticate = %v, want absent entirely (no Bearer challenge in %q)", got, tc.upstreamValue)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("WWW-Authenticate values = %#v, want exactly one value", got)
			}
			if got[0] != tc.wantValue {
				t.Errorf("WWW-Authenticate = %q, want %q", got[0], tc.wantValue)
			}
		})
	}
}
