package admin

import (
	"log/slog"
	"net/url"
	"strings"
	"unicode"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// maxLoggedOriginLen caps the sanitized Origin header value recorded by
// rejectMCPOrigin. The header is attacker-controlled and unbounded in
// length; capping it keeps a single malicious request from inflating log
// volume.
const maxLoggedOriginLen = 256

// mcpOriginMiddleware enforces the MCP Streamable HTTP spec's mandatory
// Origin validation (docs/mcp-v2.md §4.1) on every MCP endpoint, guarding
// against DNS-rebinding attacks from a browser-based client. RegisterRoutes
// constructs one instance from handler.MCPAllowedOrigins and applies it to
// every MCP route (POST/GET on /mcp and /mcp/:alias). log receives a Warn
// entry for every rejected request, so a 403 here is diagnosable from server
// logs alone — see rejectMCPOrigin.
//
// Non-browser callers — CLIs, SDKs, service-to-service integrations — never
// send an Origin header at all, and such requests always pass through
// unchanged: rejecting them here would break every existing integration
// that predates this check, none of which are browsers subject to the
// same-origin policy this check exists to backstop.
//
// When an Origin header IS present, this is an explicit-allow check — the
// same principle VoidLLM applies to model access (an empty allowlist grants
// nothing):
//
//   - If allowed is non-empty, the Origin must match one of its entries — an
//     explicit, operator-configured allowlist — compared case-insensitively
//     via originEqualFold, since RFC 6454 defines an origin's scheme and host
//     as case-insensitive: "HTTPS://app.example.com" and
//     "https://app.example.com" name the same origin, so an allowlist entered
//     with different casing than a browser happens to send must still match
//     (docs/mcp-v2.md, FIX 4). A path component remains categorically
//     forbidden in an Origin either way — see originEqualFold — this only
//     changes how the scheme/host/port comparison itself is performed, not
//     what shape is accepted. The request's Host header plays no role at all
//     in this branch.
//   - If allowed is empty (the default), only the built-in localhost origins
//     isDefaultAllowedOrigin recognizes — http/https on localhost, 127.0.0.1,
//     or [::1], each with or without a port — are accepted.
//
// This does NOT compare the Origin against the request's own Host, which is
// the mistake this middleware used to make (originHostMatches, removed): in
// the real DNS-rebinding attack, the attacker controls a DNS record — say
// evil.example.com — that first resolves to their own server (to serve the
// malicious page) and then, once the browser has loaded it, is rebound to
// resolve to 127.0.0.1 (to reach a service the browser page then talks to as
// if it were same-origin). The browser's Origin header always reads
// "https://evil.example.com" — it is fixed by the page's own URL, not by
// whatever the DNS record resolves to at request time — and its Host header
// is generated from that exact same URL, so the two headers agree by
// construction on every single request. An attacker who controls the DNS
// record controls both headers identically; comparing one attacker-supplied
// value against another attacker-supplied value can never detect anything,
// which is exactly why the MCP Streamable HTTP conformance suite's
// dns-rebinding-protection scenario fails against a same-origin check like
// that. Restricting the default to a fixed, non-attacker-influenced set of
// hostnames is what actually closes the gap: no DNS record an attacker
// controls can ever make isDefaultAllowedOrigin see "localhost", "127.0.0.1",
// or "[::1]" for a page the attacker's own server actually served.
func mcpOriginMiddleware(allowed []string, log *slog.Logger) fiber.Handler {
	return func(c fiber.Ctx) error {
		origin := c.Get(fiber.HeaderOrigin)
		if origin == "" {
			return c.Next()
		}

		if len(allowed) > 0 {
			for _, a := range allowed {
				if originEqualFold(a, origin) {
					return c.Next()
				}
			}
			return rejectMCPOrigin(c, log, origin)
		}

		if isDefaultAllowedOrigin(origin) {
			return c.Next()
		}
		return rejectMCPOrigin(c, log, origin)
	}
}

// originEqualFold reports whether a and origin name the same Origin per RFC
// 6454: scheme and host (hostname plus port) are both case-insensitive by
// definition, so "HTTPS://Example.com" and "https://example.com" name the
// same origin. An Origin carries no path component at all (config validation
// enforces this for the allowlist side, and a browser-generated Origin
// header never carries one either), so a plain case-insensitive comparison
// of the whole value — with no separate path handling — is exactly the RFC
// 6454 comparison; a value that differs by anything other than casing still
// correctly does not match.
func originEqualFold(a, origin string) bool {
	return strings.EqualFold(a, origin)
}

// defaultAllowedOriginHosts is the built-in localhost allowlist
// isDefaultAllowedOrigin accepts when settings.mcp.allowed_origins is not
// configured. It is exactly the "Valid localhost values" the MCP Streamable
// HTTP DNS-rebinding conformance scenario expects: localhost, 127.0.0.1, and
// [::1] (IPv6 loopback), compared against url.URL.Hostname() — which already
// strips the brackets an IPv6 host carries in a URL — so entries are bare
// hostnames, never bracketed.
var defaultAllowedOriginHosts = map[string]struct{}{
	"localhost": {},
	"127.0.0.1": {},
	"::1":       {},
}

// isDefaultAllowedOrigin reports whether origin — a full Origin header
// value, e.g. "https://localhost:5173" — is one of the built-in localhost
// origins mcpOriginMiddleware accepts by default, when
// settings.mcp.allowed_origins is not configured: scheme http or https,
// host localhost, 127.0.0.1, or [::1] (see defaultAllowedOriginHosts), with
// or without an explicit port. The scheme and host comparison is
// case-insensitive, matching RFC 6454. A port, if present, is not otherwise
// inspected — any port on a genuinely local origin is accepted, since a port
// number carries no rebinding risk by itself.
//
// Deliberately absent: any comparison against the request's own Host
// header. See mcpOriginMiddleware's doc for why that comparison — the prior
// behavior here — cannot detect DNS rebinding at all: an attacker who
// controls the DNS record also controls both headers, so they always agree.
func isDefaultAllowedOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return false
	}
	_, ok := defaultAllowedOriginHosts[strings.ToLower(u.Hostname())]
	return ok
}

// rejectMCPOrigin sends the HTTP 403 response for a request whose Origin
// header fails validation, and logs the rejection at Warn level via log so a
// 403 here is diagnosable from server logs alone instead of a bare,
// unexplained status code. The body is a JSON-RPC error object with a null
// ID: the request is rejected before its body is parsed, so no JSON-RPC ID
// is known to echo back.
//
// origin is the raw, attacker-controlled Origin header value. It is
// sanitized via sanitizeOriginForLog — control characters stripped, length
// capped — before it reaches the log, to prevent log injection. This is a
// deliberate, narrow exception to VoidLLM's rule against logging
// caller-supplied values: an Origin header carries only a scheme/host/port a
// browser sets automatically, never prompt, response, tool-argument, or
// _meta content.
func rejectMCPOrigin(c fiber.Ctx, log *slog.Logger, origin string) error {
	log.LogAttrs(c.Context(), slog.LevelWarn, "mcp: rejected request with disallowed Origin",
		slog.String("origin", sanitizeOriginForLog(origin)),
		slog.String("path", c.Path()),
	)
	return c.Status(fiber.StatusForbidden).JSON(
		mcp.NewErrorResponse(nil, mcp.CodeInvalidRequest, "origin not allowed"))
}

// sanitizeOriginForLog prepares a raw, attacker-controlled Origin header
// value for safe logging: control characters — notably CR and LF, which
// could otherwise forge additional log lines or fields in a line-oriented or
// naively-parsed log sink — are stripped, and the result is truncated to
// maxLoggedOriginLen runes so a single oversized header cannot inflate log
// volume. Truncation is rune-aware (range over the string, not a byte slice
// operation) so it never splits a multi-byte UTF-8 sequence.
//
// U+2028 (LINE SEPARATOR) and U+2029 (PARAGRAPH SEPARATOR) are stripped
// alongside control characters even though unicode.IsControl reports false
// for both — they fall in Unicode categories Zl and Zp, not Cc. Many log
// viewers and SIEM pipelines still render them as a line break, so leaving
// them in place would reopen the same log-line-forging risk this function
// exists to close.
func sanitizeOriginForLog(origin string) string {
	var b strings.Builder
	b.Grow(len(origin))
	n := 0
	for _, r := range origin {
		if n >= maxLoggedOriginLen {
			break
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			continue
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}
