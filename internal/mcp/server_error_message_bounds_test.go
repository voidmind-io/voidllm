package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers docs/mcp-v2.md review round Fund 7: server.go's
// "method not found: %s" (dispatchLegacy, dispatchModern) and
// "unknown tool: %s" (handleToolsCall) messages used to embed the
// caller-supplied method or tool name verbatim and unbounded — a large
// request body could inflate the resulting JSON-RPC error object without
// limit, and any raw control characters in the name would reach whatever
// consumes err.Error() unescaped. Both now reuse truncateForError
// (header_params.go) — the same bound ToolHeaderParams' own error text
// already applies to an x-mcp-header annotation name — combined with %q
// instead of %s, so a name over the bound is cut off and any control
// characters are Go-syntax-escaped rather than passed through raw.

// TestServer_ToolsCall_UnknownToolName_MessageIsBounded verifies the
// "unknown tool" error message for a tool name far longer than
// MaxHeaderParamNameInError is truncated, never embeds the tool name in
// full, and escapes control characters via %q.
func TestServer_ToolsCall_UnknownToolName_MessageIsBounded(t *testing.T) {
	t.Parallel()

	s := mcp.NewServer("voidllm", "0.1.0")

	longName := strings.Repeat("a", mcp.MaxHeaderParamNameInError+1000)
	body := modernRequestBody(1, "tools/call",
		map[string]any{"name": longName, "arguments": map[string]any{}}, nil)

	result := s.Handle(context.Background(), []byte(body), modernHeader())

	var resp mcp.Response
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, result.Body)
	}
	if resp.Error == nil {
		t.Fatal("expected a JSON-RPC error, got nil")
	}
	if resp.Error.Code != mcp.CodeInvalidParams {
		t.Errorf("Error.Code = %d, want %d (CodeInvalidParams)", resp.Error.Code, mcp.CodeInvalidParams)
	}
	if strings.Contains(resp.Error.Message, longName) {
		t.Errorf("Error.Message embeds the full %d-byte tool name unbounded, want it truncated", len(longName))
	}
	if len(resp.Error.Message) > mcp.MaxHeaderParamNameInError+64 {
		// +64 for the "unknown tool: " prefix, surrounding quotes, and the
		// "...(truncated)" marker truncateForError appends — a generous
		// margin, not a precise byte count, since this test only needs to
		// prove the message is BOUNDED, not pin its exact length.
		t.Errorf("Error.Message length = %d, want it bounded near MaxHeaderParamNameInError (%d); got: %s",
			len(resp.Error.Message), mcp.MaxHeaderParamNameInError, resp.Error.Message)
	}
}

// TestServer_Dispatch_UnknownMethod_MessageIsBounded is
// TestServer_ToolsCall_UnknownToolName_MessageIsBounded's counterpart for an
// unrecognized JSON-RPC method name, exercised through both
// dispatchLegacy's and dispatchModern's identical default case.
func TestServer_Dispatch_UnknownMethod_MessageIsBounded(t *testing.T) {
	t.Parallel()

	longMethod := strings.Repeat("m", mcp.MaxHeaderParamNameInError+1000)

	tests := []struct {
		name string
		hdr  mcp.Header
		body string
	}{
		{
			name: "legacy era",
			hdr:  mcp.MapHeader{},
			body: `{"jsonrpc":"2.0","id":1,"method":"` + longMethod + `"}`,
		},
		{
			name: "modern era",
			hdr:  modernHeader(),
			body: modernRequestBody(1, longMethod, nil, nil),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := mcp.NewServer("voidllm", "0.1.0")
			result := s.Handle(context.Background(), []byte(tc.body), tc.hdr)

			var resp mcp.Response
			if err := json.Unmarshal(result.Body, &resp); err != nil {
				t.Fatalf("unmarshal response: %v\nraw: %s", err, result.Body)
			}
			if resp.Error == nil {
				t.Fatal("expected a JSON-RPC error, got nil")
			}
			if resp.Error.Code != mcp.CodeMethodNotFound {
				t.Errorf("Error.Code = %d, want %d (CodeMethodNotFound)", resp.Error.Code, mcp.CodeMethodNotFound)
			}
			if strings.Contains(resp.Error.Message, longMethod) {
				t.Errorf("Error.Message embeds the full %d-byte method name unbounded, want it truncated", len(longMethod))
			}
			if len(resp.Error.Message) > mcp.MaxHeaderParamNameInError+64 {
				t.Errorf("Error.Message length = %d, want it bounded near MaxHeaderParamNameInError (%d); got: %s",
					len(resp.Error.Message), mcp.MaxHeaderParamNameInError, resp.Error.Message)
			}
		})
	}
}

// TestServer_Dispatch_UnknownMethod_ControlCharactersEscaped verifies a
// control character embedded in the method name reaches the JSON-RPC error
// message only in its Go-syntax-escaped form (via %q), never raw — raw
// control characters (e.g. a newline) in a value later logged via
// err.Error() would otherwise let a caller inject fake extra log lines
// (log injection) into a text-format log.
func TestServer_Dispatch_UnknownMethod_ControlCharactersEscaped(t *testing.T) {
	t.Parallel()

	const methodWithNewline = "evil\nmethod"

	s := mcp.NewServer("voidllm", "0.1.0")
	// Built via json.Marshal, not string concatenation: a literal newline
	// inside a JSON string is illegal on the wire, so this lets the encoder
	// produce the correct \n escape — exactly what a real client sending
	// this method name would transmit.
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": methodWithNewline}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	result := s.Handle(context.Background(), raw, mcp.MapHeader{})

	var resp mcp.Response
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nraw: %s", err, result.Body)
	}
	if resp.Error == nil {
		t.Fatal("expected a JSON-RPC error, got nil")
	}
	if strings.Contains(resp.Error.Message, "\n") {
		t.Errorf("Error.Message contains a raw newline, want it escaped: %q", resp.Error.Message)
	}
	if !strings.Contains(resp.Error.Message, `\n`) {
		t.Errorf("Error.Message = %q, want the escaped form of the newline (\\n) to be present", resp.Error.Message)
	}
}
