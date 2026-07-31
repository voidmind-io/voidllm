package mcp

import "strings"

// Version identifies an MCP specification revision. Every Version is a
// YYYY-MM-DD date string, matching how the Model Context Protocol Working
// Group names revisions (dates, not SemVer).
type Version string

// Known MCP specification revisions. VoidLLM understands exactly these four;
// see SupportedVersions.
const (
	// V20250326 introduced Streamable HTTP and deprecated HTTP+SSE. It is the
	// oldest revision VoidLLM understands and the default assumed for
	// requests that carry neither an MCP-Protocol-Version header nor an
	// initialize handshake (see negotiate.go).
	V20250326 Version = "2025-03-26"
	// V20250618 added the MCP-Protocol-Version header, elicitation, and the
	// OAuth resource server model.
	V20250618 Version = "2025-06-18"
	// V20251125 added experimental tasks, URL-mode elicitation, and MCP Apps.
	V20251125 Version = "2025-11-25"
	// V20260728 is the stateless-core revision: it removes protocol sessions
	// and the initialize handshake in favor of per-request metadata, and adds
	// server/discover, MRTR, subscriptions/listen, standard request headers,
	// and CacheableResult.
	V20260728 Version = "2026-07-28"
)

// Era classifies a Version by how it conveys protocol version, identity and
// capabilities: EraLegacy uses an initialize handshake, EraModern carries them
// as per-request metadata.
type Era uint8

const (
	// EraLegacy identifies revisions that negotiate via an initialize
	// handshake and (optionally, from 2025-06-18 onward) an
	// MCP-Protocol-Version header established once per connection.
	EraLegacy Era = iota
	// EraModern identifies revisions that carry protocol version, identity,
	// and capabilities as per-request metadata, with no handshake and no
	// server-side session state.
	EraModern
)

// Era returns the protocol era v belongs to. V20260728 and any later
// revision (by chronological comparison, see Compare) are EraModern; every
// other revision is EraLegacy.
func (v Version) Era() Era {
	if v.Compare(V20260728) >= 0 {
		return EraModern
	}
	return EraLegacy
}

// Valid reports whether v is one of the four revisions VoidLLM understands.
// A Version that merely looks like a date but names an unrecognized revision
// is not valid — callers must not assume forward compatibility with
// specification revisions VoidLLM was not built against.
func (v Version) Valid() bool {
	switch v {
	case V20250326, V20250618, V20251125, V20260728:
		return true
	default:
		return false
	}
}

// Compare orders two versions chronologically. Because every Version is a
// YYYY-MM-DD date string, lexicographic byte comparison is equivalent to
// chronological comparison — no date parsing is needed. It returns a negative
// number if v sorts before other, zero if v equals other, and a positive
// number if v sorts after other.
func (v Version) Compare(other Version) int {
	return strings.Compare(string(v), string(other))
}

// SupportedVersions returns every MCP revision VoidLLM understands, newest
// first. This is the "supported" list surfaced in server/discover results and
// in UnsupportedProtocolVersion error data.
func SupportedVersions() []Version {
	return []Version{V20260728, V20251125, V20250618, V20250326}
}
