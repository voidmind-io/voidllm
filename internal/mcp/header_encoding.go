package mcp

import (
	"encoding/base64"
	"strings"
)

// HeaderMethod and HeaderName are the MCP Streamable HTTP standard request
// headers (SEP-2243) that mirror a modern-era request's JSON-RPC method and
// target name, so intermediaries — load balancers, gateways, observability
// tooling — can route and inspect without parsing the body (MCP Streamable
// HTTP §4.2, docs/mcp-v2.md §4.2). HeaderProtocolVersion (negotiate.go) is
// the third such header.
const (
	HeaderMethod = "Mcp-Method"
	HeaderName   = "Mcp-Name"
)

// HeaderSentinelPrefix and HeaderSentinelSuffix delimit the MCP Streamable
// HTTP encoding for header values that are not visible ASCII (MCP Streamable
// HTTP §4.4, docs/mcp-v2.md §4.4). Both are case-sensitive and must appear
// exactly as written on the wire.
const (
	HeaderSentinelPrefix = "=?base64?"
	HeaderSentinelSuffix = "?="
)

// looksLikeHeaderSentinel reports whether v already carries the base64
// sentinel wrapper. A value that happens to look like the sentinel format —
// even if its own unencoded content would otherwise be visible ASCII — MUST
// itself be encoded, since otherwise it would be indistinguishable on the
// wire from a genuinely encoded value (docs/mcp-v2.md §4.4).
func looksLikeHeaderSentinel(v string) bool {
	return strings.HasPrefix(v, HeaderSentinelPrefix) && strings.HasSuffix(v, HeaderSentinelSuffix)
}

// isVisibleASCIIHeaderValue reports whether v can be sent as an MCP standard
// request header value without base64-sentinel encoding: every byte is
// printable ASCII (0x20-0x7E) and v carries no leading or trailing
// whitespace, which HTTP header field parsing would otherwise silently trim
// (RFC 9110 §5.1), silently changing the value the receiver observes.
func isVisibleASCIIHeaderValue(v string) bool {
	if v == "" {
		return true
	}
	if strings.TrimSpace(v) != v {
		return false
	}
	for i := 0; i < len(v); i++ {
		if v[i] < 0x20 || v[i] > 0x7E {
			return false
		}
	}
	return true
}

// HeaderParamPrefix is the prefix of the Mcp-Param-{Name} header family: a
// server may annotate an individual tool-input-schema property with
// "x-mcp-header", and a client that calls the tool then mirrors that
// property's value onto an outbound header named HeaderParamPrefix plus the
// annotated name (MCP Streamable HTTP §4.3). An intermediary that does not
// itself recognize a given Mcp-Param-{Name} header MUST still forward it,
// uninterpreted, rather than drop it (§4.3) — see collectMCPParamHeaders in
// internal/api/admin/mcp_proxy.go for the caller that applies this
// constant's validation on VoidLLM's inbound side of that rule.
const HeaderParamPrefix = "Mcp-Param-"

// MaxParamHeaders, MaxParamHeaderNameLength, and MaxParamHeaderValueLength
// bound how many Mcp-Param-{Name} headers a single request forwards, and how
// large each one's name and value may be, before ValidParamHeaderName or
// ValidParamHeaderValue starts rejecting them. The spec places no ceiling on
// any of the three itself, which — for an intermediary obligated to forward
// every Mcp-Param-* header it does not recognize (see HeaderParamPrefix) —
// would otherwise let a caller turn "forward what I don't understand" into
// an unbounded amount of per-request header-parsing and outbound-request
// work. All three are deliberately generous for the traffic §4.3 actually
// describes (a handful of scalar — integer, string, boolean, never number —
// tool-input-schema properties a server chose to mirror onto headers) while
// keeping a single request's worst case a small, fixed amount of work,
// mirroring MaxSessionIDLength's reasoning in session_registry.go:
//
//   - MaxParamHeaders = 16. §4.3 requires the annotation to be
//     case-insensitively unique per tool schema, so the realistic count is
//     "how many parameters does one tool take", which is small in every
//     real schema this project has seen; 16 comfortably covers a tool with
//     an unusually large number of header-mirrored parameters while still
//     bounding the number of outbound header-map entries and
//     sort/dedup-loop iterations a single malicious request can force.
//   - MaxParamHeaderNameLength = 64, applied to the suffix after
//     HeaderParamPrefix, not the full header name. This is the same 64-byte
//     ceiling mcpRequestMeta.ToolName already truncates a JSON-RPC tool
//     name to in mcp_proxy.go — a JSON Schema property key, which is what
//     an x-mcp-header annotation's name always is, has no legitimate reason
//     to be longer than a tool name already assumed to be.
//   - MaxParamHeaderValueLength = 1024, measured on the value as it appears
//     on the wire — i.e. after EncodeHeaderValue's base64 sentinel wrapping,
//     when that applies — because those are the actual bytes a caller sent
//     and this process must hold in memory, regardless of how long the
//     decoded value underneath happens to be. 1024 comfortably covers any
//     realistic scalar tool argument mirrored this way (a region code, a
//     UUID, a filesystem path, a short token) including the roughly 33%
//     size increase base64 adds for a non-ASCII value, while bounding a
//     single header's contribution to a request's memory footprint to a
//     small constant regardless of what an attacker-controlled or
//     misconfigured upstream schema asks a client to send.
const (
	MaxParamHeaders           = 16
	MaxParamHeaderNameLength  = 64
	MaxParamHeaderValueLength = 1024
)

// IsHTTPToken reports whether s is a valid HTTP token as RFC 9110 §5.1
// defines the "token" production: one or more tchar octets, where tchar is
// any US-ASCII letter or digit, or one of
// "!#$%&'*+-.^_`|~". The empty string is not a token — the production
// requires at least one tchar.
func IsHTTPToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isHTTPTChar(s[i]) {
			return false
		}
	}
	return true
}

// isHTTPTChar reports whether b is a tchar octet under RFC 9110 §5.1's
// token production (see IsHTTPToken's doc).
func isHTTPTChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '!', '#', '$', '%', '&', '\'', '*', '+', '-', '.', '^', '_', '`', '|', '~':
		return true
	}
	return false
}

// ValidParamHeaderName reports whether headerName is a validly formed
// Mcp-Param-{Name} header name: HeaderParamPrefix, matched
// case-insensitively (HTTP header names are themselves case-insensitive,
// and this prefix is no exception), followed by a non-empty suffix that is
// itself a valid HTTP token (IsHTTPToken) of at most
// MaxParamHeaderNameLength bytes. A headerName that does not even start
// with the prefix — the overwhelming majority of headers on an ordinary
// request — is rejected without allocating.
func ValidParamHeaderName(headerName string) bool {
	if len(headerName) <= len(HeaderParamPrefix) {
		return false
	}
	if !strings.EqualFold(headerName[:len(HeaderParamPrefix)], HeaderParamPrefix) {
		return false
	}
	suffix := headerName[len(HeaderParamPrefix):]
	return len(suffix) <= MaxParamHeaderNameLength && IsHTTPToken(suffix)
}

// ValidParamHeaderValue reports whether v is an acceptable Mcp-Param-{Name}
// header value as received on the wire: non-empty, at most
// MaxParamHeaderValueLength bytes (see that constant's doc for why the
// bound applies to the wire form), and visible ASCII with no leading or
// trailing whitespace (isVisibleASCIIHeaderValue) — the same shape
// EncodeHeaderValue guarantees its own output has, whether or not the
// sentinel wrapping was actually applied. Unlike isVisibleASCIIHeaderValue
// alone, the empty string is rejected here: an Mcp-Param-{Name} header with
// no value is not a value worth mirroring, and §4.3's own constraints on
// the underlying annotation already require the value be non-empty.
func ValidParamHeaderValue(v string) bool {
	return v != "" && len(v) <= MaxParamHeaderValueLength && isVisibleASCIIHeaderValue(v)
}

// EncodeHeaderValue renders v as an MCP standard request header value,
// applying the base64 sentinel encoding (docs/mcp-v2.md §4.4) whenever v is
// not visible ASCII or already looks like the sentinel format itself. This
// is the encoding-side counterpart of DecodeHeaderValue, which
// internal/api/admin's inbound header validation uses to reverse it — both
// must stay in lockstep, which is why they live together in this package
// rather than being duplicated per side.
func EncodeHeaderValue(v string) string {
	if !looksLikeHeaderSentinel(v) && isVisibleASCIIHeaderValue(v) {
		return v
	}
	return HeaderSentinelPrefix + base64.StdEncoding.EncodeToString([]byte(v)) + HeaderSentinelSuffix
}

// DecodeHeaderValue reverses EncodeHeaderValue. Values without the sentinel
// wrapper are returned unchanged. ok is false if v carries the sentinel
// wrapper but is not valid base64.
func DecodeHeaderValue(v string) (decoded string, ok bool) {
	if !looksLikeHeaderSentinel(v) {
		return v, true
	}
	inner := v[len(HeaderSentinelPrefix) : len(v)-len(HeaderSentinelSuffix)]
	raw, err := base64.StdEncoding.DecodeString(inner)
	if err != nil {
		return "", false
	}
	return string(raw), true
}
