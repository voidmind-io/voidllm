package mcp

// TargetParamKey returns the params field a modern-era method's Mcp-Name
// header mirrors, and whether the method carries one at all (MCP Streamable
// HTTP §4.2, docs/mcp-v2.md §4.2): "tools/call" and "prompts/get" mirror
// params.name, while "resources/read" mirrors params.uri instead. Every
// other method carries no Mcp-Name header.
//
// This mapping is protocol semantics, not a client-dialect or HTTP-transport
// concern, so it lives here rather than being duplicated in
// dialect_2026_client.go (which builds the outbound header from it) and
// internal/api/admin/mcp_handler.go (which validates the inbound header
// against it) — both need the identical mapping and must never drift apart.
// A switch, not a map, keeps this allocation-free and its result immutable.
func TargetParamKey(method string) (key string, ok bool) {
	switch method {
	case "tools/call", "prompts/get":
		return "name", true
	case "resources/read":
		return "uri", true
	default:
		return "", false
	}
}
