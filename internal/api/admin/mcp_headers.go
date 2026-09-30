package admin

import (
	"net/http"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// mcpHopByHopHeaders mirrors internal/proxy/headers.go's hopByHopHeaders
// (RFC 7230) for the MCP transparent-proxy response path (HandleMCPProxy).
// It is duplicated here rather than imported: internal/api/admin depending
// on internal/proxy for this would be a dependency in the wrong direction —
// HandleMCPProxy is a standalone transparent intermediary in its own right
// (see mcp_proxy.go's forwardHeaders and mcp.HTTPTransport.Forward's doc),
// not layered on top of the LLM proxy package. Keep this set in sync with
// its sibling by hand.
var mcpHopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"TE":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// allowedMCPResponseHeaders is the allowlist of upstream MCP server response
// headers mirrored back to the caller on the transparent proxy path
// (HandleMCPProxy). Every other header — including Server, X-Powered-By, and
// any header in mcpHopByHopHeaders — is dropped. X-RateLimit-* is matched
// separately, by prefix, in copyMCPResponseHeaders below, mirroring
// internal/proxy/headers.go's allowedResponseHeaders pattern.
var allowedMCPResponseHeaders = []string{
	"Content-Type", // the upstream, not VoidLLM, decides the response format

	// Content-Encoding is deliberately NOT included, unlike when this entry
	// was first added (review finding C2). mcp.HTTPTransport.Forward never
	// sets its own Accept-Encoding, which leaves Go's http.Transport free to
	// negotiate "gzip" on its own and transparently decompress a
	// gzip-encoded response, stripping Content-Encoding from it before
	// Forward ever sees it (net/http.Transport's DisableCompression doc).
	// Forward itself now rejects any OTHER Content-Encoding outright — br,
	// zstd, deflate, or anything else this build never asked for — as an
	// unsolicited encoding, closing the body and failing the call before a
	// *ForwardResult, and therefore this header, ever reaches
	// copyMCPResponseHeaders (see Forward's doc, docs/mcp-v2.md review
	// finding B). Under that contract Content-Encoding can never legitimately
	// be present here again: mirroring it would either forward a stale
	// gzip label onto a body Go already decompressed, or — if Forward's own
	// check above were ever removed — reopen the decompression-bomb risk
	// that fix closed, since settings.mcp.stream_max_bytes counts wire
	// bytes, not the bytes an encoded body expands to on the caller's side.
	//
	// Content-Language is representation metadata (RFC 9110 §8,
	// "Representation Data and Metadata") that, like Content-Type, is
	// inseparable from how the body that follows must be interpreted.
	// Content-Location is deliberately NOT included: it names a URI, not
	// body-decoding metadata, and could leak an upstream's internal
	// addressing.
	"Content-Language",

	// "Mcp-Session-Id" — transport.Forward mints and re-initializes no
	// session of its own on this path (see mcp.HTTPTransport.Forward's doc):
	// any legacy session belongs to the caller on the other side of this
	// proxy, and the value the upstream returns is mirrored back exactly as
	// received. HandleMCPProxy does now track, per (organization, API key)
	// SessionScope via h.MCPSessionRegistry, which values the upstream has
	// actually issued — purely to decide whether to relay an INBOUND
	// Mcp-Session-Id on the request side — but that tracking has no bearing
	// on this response mirror: what the upstream sends back here is exactly
	// what reaches the caller, unconditionally. This is the ONLY place that
	// mirrors it — mcp_proxy.go deliberately does not also c.Set it, since
	// that would duplicate this single source of truth and send the header
	// twice.
	mcp.HeaderSessionID,

	// WWW-Authenticate — otherwise a 401 cannot be answered by the caller.
	// Only its Bearer challenge(s) are ever actually mirrored, never the
	// whole header verbatim — see copyMCPResponseHeaders' filtering of this
	// entry and filterWWWAuthenticateBearer's doc (docs/mcp-v2.md, review
	// finding F).
	"WWW-Authenticate",
	"Retry-After",   // otherwise a 429 is not actionable by the caller
	"Cache-Control", // CacheableResult's Cache-Control-analogous hints (docs/mcp-v2.md §5)

	// X-Accel-Buffering is deliberately NOT allowlisted from the upstream
	// response — see copyMCPResponseHeaders' doc and mcp_proxy.go's own
	// c.Set of it. VoidLLM decides that value itself, once, for the response
	// IT sends to the caller; mirroring the upstream's own copy of the same
	// header name here would let it silently override VoidLLM's decision
	// when both happen to be set (docs/mcp-v2.md, review finding C1).
	//
	// X-Request-ID is deliberately NOT allowlisted either: RequestIDMiddleware
	// (internal/apierror/requestid.go) already sets it exactly once for
	// every VoidLLM response, including this one, and that ID — not the
	// upstream's own, unrelated value under the same header name — is the
	// one that correlates across VoidLLM's own logs and metrics for this
	// request. Mirroring the upstream's copy here would send the header
	// twice with two different values (docs/mcp-v2.md, review finding C3) —
	// the same duplication already fixed for Mcp-Session-Id (FIX 1/FIX 3).
}

// copyMCPResponseHeaders mirrors the allowlisted headers from an upstream MCP
// server response (header) onto the outbound Fiber response. It preserves
// multi-valued headers — every value present under a given header name is
// forwarded, not just the last one — since the allowlist must be able to
// carry things like multiple X-RateLimit-* lines through unchanged. It never
// mirrors a hop-by-hop header (mcpHopByHopHeaders) or any header outside
// allowedMCPResponseHeaders and the X-RateLimit-* prefix.
//
// Before either loop runs, header's own Connection field value is parsed for
// the additional, per-response hop-by-hop header names it names — RFC 7230
// §6.1 requires exactly this of "a proxy or gateway": "for each
// connection-option in this field, remove any header field(s) from the
// message with the same name as the connection-option." mcpHopByHopHeaders
// alone only covers the fixed, well-known set; an upstream naming an
// otherwise-allowlisted header (say, Connection: Cache-Control alongside its
// own Cache-Control: public, max-age=...) is declaring THAT header
// connection-specific for this one response, and copyMCPResponseHeaders
// honors that declaration the same way it honors the fixed set (docs/mcp-v2.md,
// review finding C).
func copyMCPResponseHeaders(c fiber.Ctx, header http.Header) {
	connectionOptions := mcpConnectionOptions(header)

	for _, name := range allowedMCPResponseHeaders {
		if connectionOptions[name] {
			continue
		}
		values := header.Values(name)
		if name == "WWW-Authenticate" {
			values = filterWWWAuthenticateBearer(values)
		}
		for _, v := range values {
			c.Response().Header.Add(name, v)
		}
	}

	for key, values := range header {
		if mcpHopByHopHeaders[key] || connectionOptions[key] {
			continue
		}
		// net/http canonicalizes header names (textproto.CanonicalMIMEHeaderKey),
		// which capitalizes the letter after each hyphen — "X-Ratelimit-Limit",
		// not "X-RateLimit-Limit". Both prefixes are checked, exactly as
		// internal/proxy/headers.go's copyResponseHeaders does, since which
		// form a given upstream's headers arrive in depends on how that
		// upstream itself set them before net/http re-canonicalized on receipt.
		if strings.HasPrefix(key, "X-Ratelimit") || strings.HasPrefix(key, "X-RateLimit") {
			for _, v := range values {
				c.Response().Header.Add(key, v)
			}
		}
	}
}

// mcpConnectionOptions parses every value header itself carries under the
// Connection header field name into the set of header names that field
// names as connection-specific for this one response (RFC 7230 §6.1). A
// field can list more than one connection-option, comma-separated
// (Connection: Cache-Control, X-Upstream-Debug), and net/http.Header can
// itself carry more than one Connection field-line for the same response —
// both are handled by iterating header.Values("Connection") and splitting
// each on commas. Every name is canonicalized via http.CanonicalHeaderKey so
// it compares equal to both allowedMCPResponseHeaders' own entries and the
// canonical keys net/http already normalized header's own map to.
func mcpConnectionOptions(header http.Header) map[string]bool {
	options := map[string]bool{}
	for _, line := range header.Values("Connection") {
		for _, tok := range strings.Split(line, ",") {
			tok = strings.TrimSpace(tok)
			if tok == "" {
				continue
			}
			options[http.CanonicalHeaderKey(tok)] = true
		}
	}
	return options
}

// filterWWWAuthenticateBearer returns only the Bearer challenge(s) present in
// values, dropping every other auth-scheme challenge (Basic, Digest,
// Negotiate, ...) an upstream's WWW-Authenticate response header may carry.
//
// Bearer is the only scheme MCP authorization actually uses (docs/mcp-v2.md
// §7); mirroring anything else verbatim would let a malicious or
// misconfigured upstream MCP server answer a request with, say,
// `WWW-Authenticate: Basic realm="evil"`. A browser-based caller sitting in
// front of a caller of this proxy would then show its own native HTTP Basic
// credential dialog — for a realm the CALLER'S user has no way to know is not
// this proxy's own — and whatever the user enters goes straight to that
// dialog's origin on the caller's very next retry, not to VoidLLM
// (docs/mcp-v2.md, review finding F).
func filterWWWAuthenticateBearer(values []string) []string {
	kept := make([]string, 0, len(values))
	for _, v := range values {
		if filtered := filterWWWAuthenticateChallenges(v); filtered != "" {
			kept = append(kept, filtered)
		}
	}
	return kept
}

// filterWWWAuthenticateChallenges drops every auth-scheme challenge in a
// single WWW-Authenticate header value other than Bearer, returning the
// empty string if none remain. RFC 7235 §4.1 allows more than one challenge
// in a single header value, comma-separated at the same syntactic level as
// each challenge's own comma-separated auth-params, which makes a naive
// top-level comma split ambiguous. This resolves that ambiguity with the
// same rule every challenge in the wild follows: a segment that opens a NEW
// challenge is a bare auth-scheme token — optionally followed by more
// tokens or a token68 — never a "name=value" pair, while a segment
// continuing the CURRENT challenge's own parameters always has "=" as part
// of its very first token. splitOutsideQuotes keeps a comma inside a quoted
// realm or error_description from being mistaken for a challenge boundary.
func filterWWWAuthenticateChallenges(value string) string {
	var kept []string
	keepingBearer := false
	for _, seg := range splitOutsideQuotes(value, ',') {
		trimmed := strings.TrimSpace(seg)
		if trimmed == "" {
			continue
		}
		firstToken := trimmed
		if idx := strings.IndexAny(trimmed, " \t"); idx >= 0 {
			firstToken = trimmed[:idx]
		}
		if !strings.Contains(firstToken, "=") {
			// A bare token with no "=" opens a new challenge.
			keepingBearer = strings.EqualFold(firstToken, "Bearer")
		}
		if keepingBearer {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, ", ")
}

// splitOutsideQuotes splits s on sep, treating any sep byte that falls
// between a pair of unescaped double quotes as literal content rather than a
// separator. Used by filterWWWAuthenticateChallenges so that a comma inside
// a quoted auth-param value (realm="a, evil, realm") never causes that
// param to be mistaken for the start of a new challenge.
//
// RFC 7230 §3.2.6 defines quoted-string as DQUOTE *( qdtext / quoted-pair )
// DQUOTE, where quoted-pair is a backslash followed by exactly one octet —
// meaning a backslash inside a quoted-string escapes the character right
// after it, which must be read as literal content no matter what it is,
// including a literal '"' (realm="say \"hi\"") or, in principle, sep itself.
// Without this, a '\' before a '"' was treated as two independent,
// unrelated bytes: the '\' fell through to the default case as ordinary
// content, but the following '"' still flipped inQuotes on its own — so the
// SECOND quote of an escaped pair like \"hi\" was read as closing the
// quoted-string early, and a comma appearing later in the same value (still
// inside the real, RFC-correct quoted-string) was then split on as if it
// were a challenge boundary.
func splitOutsideQuotes(s string, sep byte) []string {
	var parts []string
	var b strings.Builder
	inQuotes := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch == '\\' && inQuotes && i+1 < len(s):
			// Consume the backslash and the escaped byte together so neither
			// the loop's own '"' case nor its sep case ever inspects the
			// escaped byte on its own.
			b.WriteByte(ch)
			i++
			b.WriteByte(s[i])
		case ch == '"':
			inQuotes = !inQuotes
			b.WriteByte(ch)
		case ch == sep && !inQuotes:
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteByte(ch)
		}
	}
	parts = append(parts, b.String())
	return parts
}
