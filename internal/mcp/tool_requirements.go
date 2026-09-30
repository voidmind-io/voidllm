package mcp

import (
	"sort"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// registeredTool pairs a tool's handler with server-side registration
// options — the client extensions it requires (see RequireExtensions) and
// its validated x-mcp-header bindings (see headerParams below) — so
// Server.handleToolsCall can enforce both before the handler ever runs. This
// is deliberately separate from Tool, which is the wire-visible schema a
// client sees: mixing execution policy into that struct would leak
// server-internal enforcement rules into what crosses the wire.
type registeredTool struct {
	handler  ToolHandler
	requires []string
	// headerParams holds tool's validated x-mcp-header bindings (MCP
	// 2026-07-28 §4.3), computed exactly once at RegisterTool time via
	// ToolHeaderParams rather than on every tools/call — the same
	// once-at-registration approach the outbound path already uses (see
	// ToolCache.HeaderParams' own doc for why re-deriving bindings per
	// request would be wasteful on a hot path). Empty when tool's schema
	// carries no x-mcp-header annotation at all — the common case for every
	// tool registered today (docs/mcp-v2.md review round, Fund 6).
	// handleToolsCall uses this to validate an inbound Mcp-Param-{Name}
	// header against the request body per §4.5, the built-in server's own
	// counterpart to the outbound mirroring dialect2026Client.Prepare
	// performs.
	headerParams []HeaderParam
}

// ToolOption configures a registeredTool at RegisterTool time.
type ToolOption func(*registeredTool)

// RequireExtensions declares the client extension identifiers (docs/mcp-v2.md
// §6, "{vendor-prefix}/{extension-name}") a tool needs the calling client to
// have declared in
// params._meta["io.modelcontextprotocol/clientCapabilities"].extensions
// before its handler runs. A tools/call for a tool with unmet requirements is
// rejected with CodeMissingRequiredClientCapability (-32021), whose Data is a
// MissingCapabilityData naming every missing identifier, per the spec quoted
// verbatim in docs/mcp-v2.md §3.2:
//
//	A server MUST NOT rely on capabilities the client has not declared. If
//	processing a request requires a capability the client did not include
//	in io.modelcontextprotocol/clientCapabilities, the server MUST return a
//	MissingRequiredClientCapabilityError (-32021) whose
//	data.requiredCapabilities lists the missing capabilities. On HTTP, the
//	response status MUST be 400 Bad Request.
//
// A legacy-era request never carries clientCapabilities at all — its
// Envelope.ClientCaps is always empty — so it is treated as declaring no
// extensions and fails any requirement. VoidLLM registers no tool with
// requirements yet (this is groundwork for future extensions such as Tasks
// and MRTR); passing no ids is a no-op.
func RequireExtensions(ids ...string) ToolOption {
	return func(rt *registeredTool) {
		rt.requires = append(rt.requires, ids...)
	}
}

// MissingCapabilityData is the Data payload of a
// CodeMissingRequiredClientCapability error: the extension identifiers a
// tool requires that the calling client did not declare.
type MissingCapabilityData struct {
	// RequiredCapabilities lists the missing extension identifiers, sorted
	// for a deterministic wire representation.
	RequiredCapabilities []string `json:"requiredCapabilities"`
}

// missingExtensions returns the subset of requires the caller's declared
// clientCapabilities.extensions does not contain, sorted for a deterministic
// result. Returns nil (no error) when requires is empty. Malformed or absent
// capabilities are treated as declaring no extensions at all, so every
// requirement is reported missing in that case.
func missingExtensions(requires []string, caps Capabilities) []string {
	if len(requires) == 0 {
		return nil
	}

	declared := declaredExtensions(caps)
	missing := make([]string, 0, len(requires))
	for _, id := range requires {
		if !declared[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	return missing
}

// declaredExtensions parses caps's "extensions" object into a set of
// declared identifiers. An empty, absent, or malformed capabilities value
// yields an empty set — the caller declared no extensions, per the same
// leniency Capabilities affords the rest of its (intentionally open-ended)
// shape.
func declaredExtensions(caps Capabilities) map[string]bool {
	if len(caps) == 0 {
		return nil
	}
	var parsed struct {
		Extensions map[string]jsonx.RawMessage `json:"extensions"`
	}
	if err := jsonx.Unmarshal(caps, &parsed); err != nil {
		return nil
	}
	out := make(map[string]bool, len(parsed.Extensions))
	for id := range parsed.Extensions {
		out[id] = true
	}
	return out
}
