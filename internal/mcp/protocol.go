// Package mcp implements the Model Context Protocol (MCP) over JSON-RPC 2.0.
// It has no dependency on VoidLLM internals and can be used standalone.
package mcp

import (
	"errors"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// JSON-RPC 2.0 standard error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// MCP-specific error codes introduced by the 2026-07-28 revision. The range
// -32000 to -32019 remains implementation-defined (existing SDK usage is
// grandfathered); -32020 to -32099 is reserved by the specification itself,
// so VoidLLM must not allocate codes in that range for anything other than
// the meanings the spec assigns them.
const (
	// CodeHeaderMismatch indicates a modern-era request's Mcp-Method or
	// Mcp-Name header does not match the JSON-RPC body, or a required header
	// is missing. See MCP Streamable HTTP §4.5.
	CodeHeaderMismatch = -32020
	// CodeMissingRequiredClientCapability indicates the server would need to
	// use a capability (e.g. an extension) the client did not declare.
	CodeMissingRequiredClientCapability = -32021
	// CodeUnsupportedProtocolVersion indicates the requested protocol
	// version is not one VoidLLM understands. The accompanying Error.Data
	// carries {"supported": [...], "requested": "..."}.
	CodeUnsupportedProtocolVersion = -32022

	// CodeTooManyParamHeaders indicates the caller sent more valid,
	// non-duplicate Mcp-Param-{Name} headers (MCP 2026-07-28 §4.3) than this
	// intermediary can forward in full — see MaxParamHeaders and
	// collectMCPParamHeaders (internal/api/admin/mcp_proxy.go) for the check
	// that returns this code, and its own doc for why the whole request is
	// rejected rather than a subset of the headers silently dropped.
	//
	// Allocated from the -32000..-32019 range this const block's own doc
	// reserves for implementation-defined codes, not from -32020..-32099:
	// §4.3 obligates an intermediary to forward every Mcp-Param-{Name}
	// header it does not recognize, but sets no ceiling of its own on how
	// many there may be, so a local ceiling being exceeded has no meaning
	// the specification itself assigns a code for. It is also deliberately
	// not CodeHeaderMismatch (-32020): that code is reserved for a header
	// disagreeing with the JSON-RPC body per §4.5, and this condition is not
	// a disagreement between the two — the body is never even inspected for
	// this check, only the inbound header count.
	CodeTooManyParamHeaders = -32001
)

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      jsonx.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  jsonx.RawMessage `json:"params,omitempty"`
}

// IsNotification returns true if the request has no ID (JSON-RPC notification).
func (r *Request) IsNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      jsonx.RawMessage `json:"id"`
	Result  any              `json:"result,omitempty"`
	Error   *Error           `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	// Data carries arbitrary, error-code-specific detail — for example
	// {"supported": [...], "requested": "..."} on
	// CodeUnsupportedProtocolVersion, or MissingCapabilityData on
	// CodeMissingRequiredClientCapability. Its shape varies per error code, so
	// a concrete type is not expressible; omitted from the wire when nil.
	Data any `json:"data,omitempty"`
	// Hint carries an explicit HTTP status intent for an error whose code
	// alone does not determine it — see hintForError, which normally derives
	// this from Code and era, and Server.Handle, which prefers this field
	// over that derivation whenever it is set. Never serialized: it is a
	// signal between internal/mcp's own layers, not part of the JSON-RPC wire
	// format. The zero value, HintOK, means "no override — derive from Code
	// via hintForError", which is also HintOK's ordinary meaning for the
	// error codes hintForError does not special-case; the two are
	// indistinguishable, and that is fine, since deriving HintOK for those
	// codes is exactly what an explicit HintOK would also mean.
	Hint StatusHint `json:"-"`
}

// Tool describes an MCP tool with its name, description, and input schema.
type Tool struct {
	Name        string     `json:"name"`
	Description string     `json:"description,omitempty"`
	InputSchema JSONSchema `json:"inputSchema"`
}

// clone returns a deep copy of t. InputSchema is a []byte-backed JSONSchema,
// so a plain field-for-field copy of Tool (what Go's `=` or a slice `copy`
// does) only copies the header — the copy and the original still alias the
// same backing array. clone copies the InputSchema bytes too, so a caller
// that mutates the returned Tool's InputSchema in place — or a Server that
// hands tools to an OnToolsListHook running concurrently with other
// Handle calls — can never observe or corrupt the Server's own registered
// schema. Used everywhere a Tool value crosses the Server's API boundary
// (Tools, handleToolsList).
func (t Tool) clone() Tool {
	if len(t.InputSchema) == 0 {
		return t
	}
	schema := make(JSONSchema, len(t.InputSchema))
	copy(schema, t.InputSchema)
	t.InputSchema = schema
	return t
}

// errNilJSONSchemaReceiver guards JSONSchema.UnmarshalJSON against a nil
// pointer receiver, mirroring encoding/json.RawMessage's own guard.
var errNilJSONSchemaReceiver = errors.New("mcp: JSONSchema: UnmarshalJSON on nil pointer")

// JSONSchema is a JSON Schema 2020-12 document carried verbatim. It is a raw
// message rather than a struct because the 2020-12 vocabulary is open-ended:
// SEP-2106 permits any keyword, so no Go struct can represent it without
// silently dropping keywords on round-trip.
type JSONSchema jsonx.RawMessage

// MarshalJSON returns the raw JSON bytes s contains, or the JSON null literal
// if s is empty. Without this method, a named []byte type such as JSONSchema
// would marshal as a base64 string instead of the JSON document it holds —
// this is the most common pitfall of the RawMessage-alias pattern.
func (s JSONSchema) MarshalJSON() ([]byte, error) {
	if len(s) == 0 {
		return []byte("null"), nil
	}
	return s, nil
}

// UnmarshalJSON sets *s to a copy of data. It implements json.Unmarshaler so
// JSONSchema round-trips as a raw JSON value instead of base64.
func (s *JSONSchema) UnmarshalJSON(data []byte) error {
	if s == nil {
		return errNilJSONSchemaReceiver
	}
	*s = append((*s)[0:0], data...)
	return nil
}

// SchemaProp describes a single property within a JSONSchema built by
// ObjectSchema. It covers the subset of JSON Schema 2020-12 keywords used by
// VoidLLM's built-in tool schemas (internal/mcp/voidllm.go) — enough to avoid
// hand-written JSON there, not a general-purpose schema builder. Schemas
// needing keywords beyond these (oneOf, $ref, pattern, ...) should be
// constructed as raw JSON and converted to JSONSchema directly instead of
// extending this type.
type SchemaProp struct {
	// Type is the JSON Schema "type" keyword, e.g. "string", "array", "object".
	Type string
	// Description is the JSON Schema "description" keyword.
	Description string
	// Enum is the JSON Schema "enum" keyword. Omitted when empty.
	Enum []string
	// Items is the JSON Schema "items" keyword for Type == "array". Omitted
	// when nil.
	Items *SchemaProp
	// Properties is the JSON Schema "properties" keyword for Type ==
	// "object", describing a nested object property. Omitted when empty.
	Properties map[string]SchemaProp
}

// render converts p to the map[string]any shape ObjectSchema marshals,
// omitting zero-value keywords the same way the JSON `omitempty` tag would.
func (p SchemaProp) render() map[string]any {
	m := make(map[string]any, 5)
	if p.Type != "" {
		m["type"] = p.Type
	}
	if p.Description != "" {
		m["description"] = p.Description
	}
	if len(p.Enum) > 0 {
		m["enum"] = p.Enum
	}
	if p.Items != nil {
		m["items"] = p.Items.render()
	}
	if len(p.Properties) > 0 {
		nested := make(map[string]any, len(p.Properties))
		for name, prop := range p.Properties {
			nested[name] = prop.render()
		}
		m["properties"] = nested
	}
	return m
}

// ObjectSchema builds a JSON Schema 2020-12 object schema — type: "object"
// with the given properties and required fields — from typed property
// descriptions, so RegisterTool call sites need no hand-written JSON. props
// may be nil or empty to describe an object schema with no declared
// properties (e.g. a tool that takes no arguments).
func ObjectSchema(props map[string]SchemaProp, required ...string) JSONSchema {
	obj := map[string]any{"type": "object"}
	if len(props) > 0 {
		rendered := make(map[string]any, len(props))
		for name, p := range props {
			rendered[name] = p.render()
		}
		obj["properties"] = rendered
	}
	if len(required) > 0 {
		obj["required"] = required
	}

	out, err := jsonx.Marshal(obj)
	if err != nil {
		// obj is built entirely from string, []string, and map[string]any
		// literals produced by render(), which cannot fail to marshal; this
		// branch is unreachable defensive fallback, not a real error path.
		return JSONSchema(`{"type":"object"}`)
	}
	return JSONSchema(out)
}

// ToolResult is the result of a tool call.
type ToolResult struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// Content is a single content block in a tool result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// TextResult creates a successful ToolResult with a single text block.
func TextResult(text string) *ToolResult {
	return &ToolResult{
		Content: []Content{{Type: "text", Text: text}},
	}
}

// ErrorResult creates a ToolResult that indicates a tool-level error.
// Tool errors are NOT JSON-RPC protocol errors — they go in the result.
func ErrorResult(msg string) *ToolResult {
	return &ToolResult{
		Content: []Content{{Type: "text", Text: msg}},
		IsError: true,
	}
}

// NewResponse creates a success response.
func NewResponse(id jsonx.RawMessage, result any) Response {
	return Response{JSONRPC: "2.0", ID: id, Result: result}
}

// NewErrorResponse creates an error response.
func NewErrorResponse(id jsonx.RawMessage, code int, message string) Response {
	return Response{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: message}}
}
