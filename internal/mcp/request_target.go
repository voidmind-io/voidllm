package mcp

import "github.com/voidmind-io/voidllm/internal/jsonx"

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

// envelopeTargetName extracts the tool name or resource URI a single
// JSON-RPC request names — Envelope.Name's contract ("tool name or resource
// URI the request targets") — from params, the request's raw params.RawMessage,
// according to TargetParamKey's mapping for method. Returns "" both when
// method carries no such field (TargetParamKey's ok == false, e.g.
// tools/list or server/discover) and when params does not actually contain
// the named field or it is not a JSON string.
//
// Both server dialects (dialect_2026.go, dialect_legacy.go) call this from
// their own Decode, in place of each formerly hand-rolling its own
// "tools/call"-only switch, so Envelope.Name is populated identically in
// both eras for every method TargetParamKey names — not only tools/call —
// without duplicating the method-to-field mapping a second time per dialect
// (see TargetParamKey's own doc for why that single mapping must not drift).
func envelopeTargetName(method string, params jsonx.RawMessage) string {
	key, ok := TargetParamKey(method)
	if !ok {
		return ""
	}
	var fields map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(params, &fields); err != nil {
		return ""
	}
	var name string
	_ = jsonx.Unmarshal(fields[key], &name)
	return name
}
