package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
)

// protocolVersion is the MCP specification version this server implements.
const protocolVersion = "2025-03-26"

// ToolHandler is a function that handles a tool call.
// It receives the request context and the raw JSON arguments from the caller.
// Return a ToolResult on success, or an error for unexpected failures.
// Tool-level errors (e.g. invalid input) should be returned as ErrorResult,
// not as a Go error.
type ToolHandler func(ctx context.Context, args json.RawMessage) (*ToolResult, error)

// OnToolsListHook is an optional callback invoked inside tools/list before the
// tool list is returned to the caller. It receives a copy of the registered
// tools and may return a modified slice. The hook must not retain references to
// the slice after it returns.
type OnToolsListHook func(tools []Tool) []Tool

// Server is an MCP server that handles JSON-RPC 2.0 requests.
// It is safe for concurrent use.
type Server struct {
	name        string
	version     string
	mu          sync.RWMutex
	tools       []Tool
	handlers    map[string]ToolHandler
	onToolsList OnToolsListHook
}

// NewServer creates a new MCP server with the given name and version.
func NewServer(name, version string) *Server {
	return &Server{
		name:     name,
		version:  version,
		handlers: make(map[string]ToolHandler),
	}
}

// RegisterTool adds a tool and its handler to the server.
// It is not safe to call concurrently with Handle — register all tools
// before starting to handle requests.
func (s *Server) RegisterTool(tool Tool, handler ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = append(s.tools, tool)
	s.handlers[tool.Name] = handler
}

// Tools returns a copy of the registered tool schemas. The returned slice is
// safe to use after the call; mutations do not affect the server's internal
// state. It is safe to call concurrently with Handle and RegisterTool.
func (s *Server) Tools() []Tool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Tool, len(s.tools))
	copy(out, s.tools)
	return out
}

// SetOnToolsList registers a hook that is called inside tools/list before the
// tool list is returned. The hook receives a shallow copy of the registered
// tools and may return a modified slice. Setting a nil hook clears any
// previously registered hook. It is safe to call concurrently with Handle.
func (s *Server) SetOnToolsList(hook OnToolsListHook) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onToolsList = hook
}

// Handle processes a raw JSON-RPC 2.0 request and returns the JSON-encoded
// response bytes. For notifications (requests with no ID), it returns nil.
func (s *Server) Handle(ctx context.Context, raw []byte) []byte {
	var req Request
	if err := json.Unmarshal(raw, &req); err != nil {
		resp := NewErrorResponse(nil, CodeParseError, "parse error")
		out, _ := json.Marshal(resp)
		return out
	}

	if req.JSONRPC != "2.0" {
		resp := NewErrorResponse(req.ID, CodeInvalidRequest, "jsonrpc must be \"2.0\"")
		out, _ := json.Marshal(resp)
		return out
	}

	var result any
	var respErr *Error

	switch req.Method {
	case "initialize":
		result = s.handleInitialize(req.Params)
	case "notifications/initialized":
		// Notification — no response required by the MCP spec.
		return nil
	case "ping":
		result = map[string]any{}
	case "tools/list":
		result = s.handleToolsList()
	case "tools/call":
		result, respErr = s.handleToolsCall(ctx, req.Params)
	default:
		respErr = &Error{Code: CodeMethodNotFound, Message: fmt.Sprintf("method not found: %s", req.Method)}
	}

	// Notifications (no ID) receive no response regardless of method.
	if req.IsNotification() {
		return nil
	}

	var resp Response
	if respErr != nil {
		resp = Response{JSONRPC: "2.0", ID: req.ID, Error: respErr}
	} else {
		resp = Response{JSONRPC: "2.0", ID: req.ID, Result: result}
	}

	out, _ := json.Marshal(resp)
	return out
}

// handleInitialize returns the server's capabilities and identity.
func (s *Server) handleInitialize(_ json.RawMessage) any {
	return map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    s.name,
			"version": s.version,
		},
	}
}

// handleToolsList returns the list of registered tools. If an OnToolsListHook
// has been set via SetOnToolsList, the hook is invoked with a copy of the tool
// list and its return value is used as the response payload.
func (s *Server) handleToolsList() any {
	s.mu.RLock()
	tools := make([]Tool, len(s.tools))
	copy(tools, s.tools)
	hook := s.onToolsList
	s.mu.RUnlock()

	if hook != nil {
		tools = hook(tools)
	}
	return map[string]any{
		"tools": tools,
	}
}

// handleToolsCall dispatches a tools/call request to the registered handler.
// Unexpected handler errors are converted to tool-level error results rather
// than JSON-RPC protocol errors, keeping protocol integrity intact.
func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *Error) {
	var call struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, &Error{Code: CodeInvalidParams, Message: "invalid params: expected {name, arguments}"}
	}

	s.mu.RLock()
	handler, ok := s.handlers[call.Name]
	s.mu.RUnlock()

	if !ok {
		return nil, &Error{Code: CodeInvalidParams, Message: fmt.Sprintf("unknown tool: %s", call.Name)}
	}

	result, err := handler(ctx, call.Arguments)
	if err != nil {
		// Unexpected handler error → tool-level error result, NOT a protocol error.
		// Internal error details are not forwarded to the caller.
		return ErrorResult("internal error"), nil
	}

	return result, nil
}
