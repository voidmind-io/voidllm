package admin_test

// Tests for internal/api/admin/mcp_origin.go's mcpOriginMiddleware — the
// mandatory Origin validation MCP Streamable HTTP requires on every MCP
// endpoint (docs/mcp-v2.md §4.1, FIX 11). mcpOriginMiddleware itself is
// unexported, so these tests exercise it black-box through the four routes
// RegisterRoutes applies it to: POST/GET /api/v1/mcp (Code Mode) and
// POST/GET /api/v1/mcp/:alias (the "voidllm" built-in server here).

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// setupOriginTestApp creates a Fiber app with BOTH MCP route groups
// registered — the Code Mode server (POST/GET /api/v1/mcp) and the built-in
// "voidllm" management server (POST/GET /api/v1/mcp/:alias) — so the origin
// middleware table test below can exercise all four MCP routes. Neither
// server needs real tool dependencies wired: the middleware runs before any
// tool is ever dispatched.
func setupOriginTestApp(t *testing.T, dsn string, allowedOrigins []string) (*fiber.App, string) {
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

	mcpServer := mcp.NewServer("voidllm", "test")
	mcp.RegisterVoidLLMTools(mcpServer, mcpTestDeps())
	codeModeServer := mcp.NewServer("codemode", "test")

	handler := &admin.Handler{
		DB:                database,
		HMACSecret:        testHMACSecret,
		KeyCache:          keyCache,
		License:           license.NewHolder(license.Verify("", true)),
		Log:               noopLogger(t),
		MCPServer:         mcpServer,
		CodeModeServer:    codeModeServer,
		MCPAllowedOrigins: allowedOrigins,
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	key := addTestKey(t, keyCache, auth.RoleMember, "org-mcp-origin-test")

	return app, key
}

// originMCPRoute describes one of the four MCP routes the origin middleware
// is applied to.
type originMCPRoute struct {
	name   string
	method string
	path   string
	body   string // only used for POST
}

// originTestRoutes is the full set of MCP routes mcpOriginMiddleware guards.
func originTestRoutes() []originMCPRoute {
	return []originMCPRoute{
		{"POST /api/v1/mcp (Code Mode)", http.MethodPost, "/api/v1/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{"GET /api/v1/mcp (Code Mode SSE)", http.MethodGet, "/api/v1/mcp", ""},
		{"POST /api/v1/mcp/:alias (voidllm)", http.MethodPost, "/api/v1/mcp/voidllm", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`},
		{"GET /api/v1/mcp/:alias (voidllm SSE)", http.MethodGet, "/api/v1/mcp/voidllm", ""},
	}
}

// TestMCPOriginMiddleware is table-driven across all four MCP routes and the
// five origin scenarios docs/mcp-v2.md §4.1 (FIX 11) distinguishes. The
// "no Origin header" case is the single most important regression here: it
// is the shape of every non-browser caller (CLIs, SDKs, service-to-service
// integrations, and every existing VoidLLM integration that predates this
// middleware) and must always pass through unconditionally, regardless of
// the allowlist configuration.
func TestMCPOriginMiddleware(t *testing.T) {
	t.Parallel()

	type scenario struct {
		name           string
		allowedOrigins []string
		originHeader   string // empty means "do not set the Origin header at all"
		wantAllowed    bool
	}

	scenarios := []scenario{
		{
			name:           "REGRESSION (highest priority): no Origin header passes through unconditionally, allowlist irrelevant",
			allowedOrigins: []string{"https://allowed.example.com"},
			originHeader:   "",
			wantAllowed:    true,
		},
		{
			name:           "Origin set, allowlist configured, exact match passes",
			allowedOrigins: []string{"https://allowed.example.com"},
			originHeader:   "https://allowed.example.com",
			wantAllowed:    true,
		},
		{
			name:           "Origin set, allowlist configured, no match is rejected",
			allowedOrigins: []string{"https://allowed.example.com"},
			originHeader:   "https://evil.example.com",
			wantAllowed:    false,
		},
		{
			// httptest.NewRequest defaults the request Host to "example.com"
			// for a relative target URL.
			name:           "Origin set, allowlist empty, Origin host equals request host passes",
			allowedOrigins: nil,
			originHeader:   "https://example.com",
			wantAllowed:    true,
		},
		{
			name:           "Origin set, allowlist empty, foreign host is rejected",
			allowedOrigins: nil,
			originHeader:   "https://evil.example.com",
			wantAllowed:    false,
		},
		{
			// RFC 6454: an Origin's scheme and host are case-insensitive
			// (docs/mcp-v2.md, FIX 4). The allowlist entry is lowercase (as
			// config.Validate normalizes it), the request sends the fully
			// uppercase equivalent.
			name:           "allowlist lowercase, request Origin fully uppercase: still matches (RFC 6454 case-insensitivity)",
			allowedOrigins: []string{"https://allowed.example.com"},
			originHeader:   "HTTPS://ALLOWED.EXAMPLE.COM",
			wantAllowed:    true,
		},
		{
			// The reverse direction: an operator who entered the allowlist
			// with unusual casing (before config normalization runs, or via
			// a Handler constructed directly in a test/library context that
			// bypasses config.Validate) must still match a normally-cased
			// request Origin.
			name:           "allowlist uppercase, request Origin lowercase: still matches (RFC 6454 case-insensitivity)",
			allowedOrigins: []string{"HTTPS://ALLOWED.EXAMPLE.COM"},
			originHeader:   "https://allowed.example.com",
			wantAllowed:    true,
		},
		{
			name:           "allowlist and request Origin both mixed-case: still matches (RFC 6454 case-insensitivity)",
			allowedOrigins: []string{"https://Allowed.Example.Com"},
			originHeader:   "HTTPS://alloweD.example.COM",
			wantAllowed:    true,
		},
		{
			// The case-insensitivity relaxation must not go further than the
			// scheme/host it applies to: a genuinely different origin, sent
			// with unusual casing, must still be rejected — case-folding is
			// not a general "ignore differences" mechanism.
			name:           "case-insensitivity does not paper over a genuinely different origin",
			allowedOrigins: []string{"https://allowed.example.com"},
			originHeader:   "HTTPS://EVIL.EXAMPLE.COM",
			wantAllowed:    false,
		},
		{
			// Sandboxed iframes and data: URIs send the literal Origin value
			// "null" (RFC 6454 §7). url.Parse("null") yields an empty Host,
			// which can never equal a real request Host, so this is already
			// rejected correctly — this case pins that down as a regression
			// test, since it is security-relevant (an attacker cannot use a
			// sandboxed/opaque origin to bypass the DNS-rebinding check).
			name:           "Origin: null (sandboxed iframe / data: URI) is rejected, allowlist empty",
			allowedOrigins: nil,
			originHeader:   "null",
			wantAllowed:    false,
		},
		{
			name:           "Origin: null (sandboxed iframe / data: URI) is rejected, allowlist configured",
			allowedOrigins: []string{"https://allowed.example.com"},
			originHeader:   "null",
			wantAllowed:    false,
		},
	}

	for _, route := range originTestRoutes() {
		for _, sc := range scenarios {
			t.Run(route.name+"/"+sc.name, func(t *testing.T) {
				t.Parallel()

				dsn := "file:TestMCPOriginMiddleware_" +
					sanitizeDSNName(route.name+"_"+sc.name) + "?mode=memory&cache=private"
				app, key := setupOriginTestApp(t, dsn, sc.allowedOrigins)

				var body *strings.Reader
				if route.body != "" {
					body = strings.NewReader(route.body)
				} else {
					body = strings.NewReader("")
				}
				req := httptest.NewRequest(route.method, route.path, body)
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+key)
				if sc.originHeader != "" {
					req.Header.Set("Origin", sc.originHeader)
				}

				// A short timeout keeps the passing GET (SSE) cases fast: we
				// only need the initial status code, not the full stream —
				// app.Test blocks for the full configured timeout on a
				// streaming response regardless of how quickly data was
				// flushed.
				timeout := testTimeout
				if !sc.wantAllowed {
					timeout = 2 * time.Second
				} else if route.method == http.MethodGet {
					timeout = 200 * time.Millisecond
				}

				resp, err := app.Test(req, fiber.TestConfig{Timeout: timeout})
				if err != nil {
					t.Fatalf("app.Test: %v", err)
				}
				defer resp.Body.Close()

				if sc.wantAllowed {
					if resp.StatusCode == fiber.StatusForbidden {
						t.Errorf("status = 403, want request to pass through the origin middleware "+
							"(route=%s scenario=%s)", route.name, sc.name)
					}
				} else {
					if resp.StatusCode != fiber.StatusForbidden {
						t.Errorf("status = %d, want 403 (route=%s scenario=%s)",
							resp.StatusCode, route.name, sc.name)
					}
				}
			})
		}
	}
}

// TestMCPOriginMiddleware_RejectionBody verifies that a 403 origin rejection
// carries a JSON-RPC error object with a null ID: the request is rejected
// before its body is ever parsed, so no JSON-RPC ID is known to echo back.
func TestMCPOriginMiddleware_RejectionBody(t *testing.T) {
	t.Parallel()

	app, key := setupOriginTestApp(t, "file:TestMCPOriginMiddleware_RejectionBody?mode=memory&cache=private",
		[]string{"https://allowed.example.com"})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/voidllm",
		strings.NewReader(`{"jsonrpc":"2.0","id":42,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Origin", "https://evil.example.com")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	mcpResp := decodeMCPErrorBody(t, resp.Body)
	if mcpResp.Error == nil {
		t.Fatal("expected a JSON-RPC error object in the 403 body, got nil")
	}
	// The request carried id:42, but the rejection must NOT echo it back —
	// the body was never parsed.
	if string(mcpResp.ID) != "" && string(mcpResp.ID) != "null" {
		t.Errorf("ID = %q, want null/empty (request body is never parsed before an origin rejection)",
			string(mcpResp.ID))
	}
}

// TestSanitizeOriginForLog is table-driven across the control-character and
// Unicode-line-break categories sanitizeOriginForLog must strip before an
// attacker-controlled Origin header reaches the log, plus a harmless origin
// left untouched so the function is confirmed not to over-filter.
//
// U+2028 (LINE SEPARATOR) and U+2029 (PARAGRAPH SEPARATOR) are the
// regression this test exists to pin down: unicode.IsControl reports false
// for both — they are category Zl/Zp, not Cc — so a filter built only on
// IsControl lets them through even though many log viewers and SIEM
// pipelines render them as a line break, which is exactly the log-injection
// vector this function guards against.
func TestSanitizeOriginForLog(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "harmless origin is left unchanged",
			input: "https://example.com:8443",
			want:  "https://example.com:8443",
		},
		{
			name:  "CR is stripped (regression)",
			input: "https://evil.example.com\rSet-Cookie: x=1",
			want:  "https://evil.example.comSet-Cookie: x=1",
		},
		{
			name:  "LF is stripped (regression)",
			input: "https://evil.example.com\nSet-Cookie: x=1",
			want:  "https://evil.example.comSet-Cookie: x=1",
		},
		{
			name:  "U+2028 LINE SEPARATOR is stripped even though unicode.IsControl reports false for it",
			input: "https://evil.example.com forged log line",
			want:  "https://evil.example.comforged log line",
		},
		{
			name:  "U+2029 PARAGRAPH SEPARATOR is stripped even though unicode.IsControl reports false for it",
			input: "https://evil.example.com forged log line",
			want:  "https://evil.example.comforged log line",
		},
		{
			name:  "ESC (ANSI escape lead-in) is stripped",
			input: "https://evil.example.com\x1b[31mred text\x1b[0m",
			want:  "https://evil.example.com[31mred text[0m",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := admin.SanitizeOriginForLog(tc.input)
			if got != tc.want {
				t.Errorf("SanitizeOriginForLog(%q) = %q, want %q", tc.input, got, tc.want)
			}
			if strings.ContainsAny(got, "\r\n  \x1b") {
				t.Errorf("SanitizeOriginForLog(%q) = %q still contains a character that must be filtered",
					tc.input, got)
			}
		})
	}
}

// TestSanitizeOriginForLog_TruncationIsRuneAware verifies that truncating an
// oversized Origin value to admin.MaxLoggedOriginLen runes never splits a
// multi-byte UTF-8 sequence: the function ranges over the string (which
// iterates runes) and counts accepted runes, not bytes, so the result must
// always be valid UTF-8 and never exceed MaxLoggedOriginLen runes.
func TestSanitizeOriginForLog_TruncationIsRuneAware(t *testing.T) {
	t.Parallel()

	// "世" is a 3-byte UTF-8 sequence. Well more than MaxLoggedOriginLen of
	// them guarantees truncation kicks in; a naive byte-slice truncation to
	// 256 bytes would land mid-sequence and corrupt the last rune.
	input := strings.Repeat("世", admin.MaxLoggedOriginLen*3)

	got := admin.SanitizeOriginForLog(input)

	if !utf8.ValidString(got) {
		t.Fatalf("SanitizeOriginForLog result is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > admin.MaxLoggedOriginLen {
		t.Errorf("SanitizeOriginForLog result has %d runes, want <= %d", n, admin.MaxLoggedOriginLen)
	}
}

// sanitizeDSNName replaces characters that are invalid in SQLite URI
// filenames and Go subtest names with underscores.
func sanitizeDSNName(s string) string {
	r := strings.NewReplacer("/", "_", " ", "_", "#", "_", ":", "_", "(", "_", ")", "_")
	return r.Replace(s)
}
