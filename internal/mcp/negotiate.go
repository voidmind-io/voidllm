package mcp

import "strings"

// HeaderProtocolVersion is the name of the MCP Streamable HTTP header that
// carries the caller's requested protocol version, mirroring
// params._meta["io.modelcontextprotocol/protocolVersion"] in modern-era
// requests (MCP Streamable HTTP §4.2). Callers outside internal/mcp — the
// admin HTTP handler in particular — use this constant instead of
// hardcoding the header name.
const HeaderProtocolVersion = "MCP-Protocol-Version"

// ProtocolVersionHeaderValue returns hdr's HeaderProtocolVersion value with
// leading and trailing whitespace removed via strings.TrimSpace, or "" if hdr
// is nil or carries no such header. This is the single point where that
// header is ever read: Negotiate uses it directly below, and
// internal/api/admin's header/body cross-validation (validateMCPHeaders,
// handleMCPSSE) uses it too, so both sides of that cross-validation agree on
// the same trimmed value Negotiate itself negotiates on. Before this helper
// existed, validateMCPHeaders and handleMCPSSE read hdr.Get(HeaderProtocolVersion)
// untrimmed while Negotiate trimmed it: a header value padded with
// whitespace then negotiated as modern by Negotiate but read as "not modern"
// by the untrimmed comparison, skipping the Mcp-Method/Mcp-Name checks
// validateMCPHeaders exists to enforce (docs/mcp-v2.md, FIX 1). Every reader
// of this header MUST go through this function instead of calling hdr.Get
// directly, so a future caller cannot reopen the same split-brain gap.
func ProtocolVersionHeaderValue(hdr Header) string {
	if hdr == nil {
		return ""
	}
	return strings.TrimSpace(hdr.Get(HeaderProtocolVersion))
}

// Negotiate determines which protocol Version should handle an inbound
// request, given its transport headers and raw JSON-RPC body. This is the
// single point where the protocol version is resolved from the wire —
// everything downstream (dispatch, access control, usage logging) operates
// on the era-neutral Envelope and never re-inspects headers or version
// strings.
//
// It implements the era-detection outcomes MCP Streamable HTTP §4.6 and
// docs/mcp-v2.md §4.6 describe as five numbered rules, but not as five
// sequential, independent checks — the numbered form is the right way to
// read the SPEC, not this function's own branch structure, which is exactly
// two branches:
//
//   - HeaderProtocolVersion is present (any non-empty value once trimmed via
//     ProtocolVersionHeaderValue): its validity is checked FIRST — an
//     unrecognized version (spec rule 5) returns an
//     UnsupportedProtocolVersion error carrying every version VoidLLM
//     supports and the version that was requested, before a valid one is
//     ever considered. A valid header value is returned directly regardless
//     of which era it names — spec rules 1 (EraModern) and 3 (EraLegacy)
//     are one and the same branch here, since Version.Valid alone, not era,
//     is what this function checks; era-specific behavior belongs to
//     Version.Era and each dialect's own Decode, not to Negotiate.
//   - HeaderProtocolVersion is absent: V20250326 always, regardless of
//     whether the body's JSON-RPC method is "initialize" — spec rules 2 and
//     4 are one and the same branch here too, since raw is not inspected in
//     this branch at all (a legacy dialect's own Decode step refines this
//     default from the initialize body's own protocolVersion field when
//     present, and surfaces a parse error for any body Negotiate could not
//     have made sense of anyway).
func Negotiate(raw []byte, hdr Header) (Version, *Error) {
	headerVal := ProtocolVersionHeaderValue(hdr)

	if headerVal != "" {
		v := Version(headerVal)
		if !v.Valid() {
			return "", &Error{
				Code:    CodeUnsupportedProtocolVersion,
				Message: "unsupported protocol version",
				Data: map[string]any{
					"supported": SupportedVersions(),
					"requested": headerVal,
				},
			}
		}
		return v, nil
	}

	// No header: rules 2 and 4 both resolve to V20250326 here, whether the
	// body's method is "initialize" (rule 2, a fresh legacy handshake with no
	// version known yet) or something else entirely (rule 4, a headerless
	// legacy client the spec explicitly permits defaulting to the oldest
	// revision). raw is not otherwise inspected: a legacy dialect's Decode
	// step refines this default from the initialize body's own
	// protocolVersion field when present, and surfaces a parse error for any
	// body Negotiate could not have made sense of anyway.
	return V20250326, nil
}
