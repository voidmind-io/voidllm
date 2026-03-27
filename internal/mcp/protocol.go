// Package mcp implements the Model Context Protocol (MCP) over JSON-RPC 2.0.
// It has no dependency on VoidLLM internals and can be used standalone.
package mcp

import "encoding/json"

// JSON-RPC 2.0 standard error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Request is a JSON-RPC 2.0 request.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification returns true if the request has no ID (JSON-RPC notification).
func (r *Request) IsNotification() bool {
	return len(r.ID) == 0 || string(r.ID) == "null"
}

// Response is a JSON-RPC 2.0 response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC 2.0 error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Tool describes an MCP tool with its name, description, and input schema.
type Tool struct {
	Name        string      `json:"name"`
	Description string      `json:"description,omitempty"`
	InputSchema InputSchema `json:"inputSchema"`
}

// InputSchema defines the JSON Schema for tool input parameters.
type InputSchema struct {
	Type       string              `json:"type"`
	Properties map[string]Property `json:"properties,omitempty"`
	Required   []string            `json:"required,omitempty"`
}

// Property describes a single property in a tool's input schema.
type Property struct {
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
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
func NewResponse(id json.RawMessage, result any) Response {
	return Response{JSONRPC: "2.0", ID: id, Result: result}
}

// NewErrorResponse creates an error response.
func NewErrorResponse(id json.RawMessage, code int, message string) Response {
	return Response{JSONRPC: "2.0", ID: id, Error: &Error{Code: code, Message: message}}
}
