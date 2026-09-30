package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// TestServer_ToolsCall_RequireExtensions is the table-driven enforcement test
// for RequireExtensions / CodeMissingRequiredClientCapability (-32021), per
// the spec quoted verbatim in docs/mcp-v2.md §3.2:
//
//	A server MUST NOT rely on capabilities the client has not declared. If
//	processing a request requires a capability the client did not include
//	in io.modelcontextprotocol/clientCapabilities, the server MUST return a
//	MissingRequiredClientCapabilityError (-32021) whose
//	data.requiredCapabilities lists the missing capabilities. On HTTP, the
//	response status MUST be 400 Bad Request.
func TestServer_ToolsCall_RequireExtensions(t *testing.T) {
	t.Parallel()

	const taskExt = "io.modelcontextprotocol/tasks"
	const uiExt = "io.modelcontextprotocol/ui"

	tests := []struct {
		name               string
		requires           []string
		declaredExtensions map[string]any // nil = client declares no extensions.extensions at all
		wantHandlerCalled  bool
		wantErr            bool
		wantRequiredCaps   []string // exact, ordered contents of data.requiredCapabilities
	}{
		{
			name:              "a tool requiring an undeclared extension is rejected with -32021, and the handler never runs",
			requires:          []string{taskExt},
			wantHandlerCalled: false,
			wantErr:           true,
			wantRequiredCaps:  []string{taskExt},
		},
		{
			name:               "declaring the required extension lets the handler run",
			requires:           []string{taskExt},
			declaredExtensions: map[string]any{taskExt: map[string]any{}},
			wantHandlerCalled:  true,
			wantErr:            false,
		},
		{
			name:              "multiple missing extensions are reported deterministically sorted",
			requires:          []string{uiExt, taskExt}, // registered out of alphabetical order
			wantHandlerCalled: false,
			wantErr:           true,
			wantRequiredCaps:  []string{taskExt, uiExt}, // "tasks" < "ui"
		},
		{
			name:              "a tool with no requirements at all is unaffected by undeclared capabilities",
			requires:          nil,
			wantHandlerCalled: true,
			wantErr:           false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var handlerCalled bool
			s := mcp.NewServer("voidllm", "0.1.0")

			var opts []mcp.ToolOption
			if len(tc.requires) > 0 {
				opts = append(opts, mcp.RequireExtensions(tc.requires...))
			}
			s.RegisterTool(mcp.Tool{Name: "guarded_tool", InputSchema: mcp.ObjectSchema(nil)},
				func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
					handlerCalled = true
					return mcp.TextResult("ok"), nil
				}, opts...)

			metaOverrides := map[string]any{}
			if tc.declaredExtensions != nil {
				metaOverrides["io.modelcontextprotocol/clientCapabilities"] = map[string]any{
					"extensions": tc.declaredExtensions,
				}
			}
			body := modernRequestBody(1, "tools/call",
				map[string]any{"name": "guarded_tool", "arguments": map[string]any{}}, metaOverrides)

			result := s.Handle(context.Background(), []byte(body),
				mcp.MapHeader{mcp.HeaderProtocolVersion: string(mcp.V20260728)})

			var resp mcp.Response
			if err := json.Unmarshal(result.Body, &resp); err != nil {
				t.Fatalf("unmarshal response: %v\nraw: %s", err, result.Body)
			}

			if handlerCalled != tc.wantHandlerCalled {
				t.Errorf("handler called = %v, want %v", handlerCalled, tc.wantHandlerCalled)
			}

			if !tc.wantErr {
				if resp.Error != nil {
					t.Fatalf("unexpected error: %+v", resp.Error)
				}
				return
			}

			if resp.Error == nil {
				t.Fatal("expected a JSON-RPC error, got nil")
			}
			if resp.Error.Code != mcp.CodeMissingRequiredClientCapability {
				t.Errorf("Error.Code = %d, want %d (CodeMissingRequiredClientCapability)",
					resp.Error.Code, mcp.CodeMissingRequiredClientCapability)
			}
			if result.Hint != mcp.HintBadRequest {
				t.Errorf("Hint = %v, want HintBadRequest (docs/mcp-v2.md §3.2 requires HTTP 400 for -32021)", result.Hint)
			}

			dataMap, ok := resp.Error.Data.(map[string]any)
			if !ok {
				t.Fatalf("Error.Data type = %T, want map[string]any", resp.Error.Data)
			}
			rawCaps, ok := dataMap["requiredCapabilities"].([]any)
			if !ok {
				t.Fatalf("Error.Data[requiredCapabilities] type = %T, want []any", dataMap["requiredCapabilities"])
			}
			gotCaps := make([]string, len(rawCaps))
			for i, v := range rawCaps {
				gotCaps[i], _ = v.(string)
			}
			if len(gotCaps) != len(tc.wantRequiredCaps) {
				t.Fatalf("requiredCapabilities = %v, want %v", gotCaps, tc.wantRequiredCaps)
			}
			for i, want := range tc.wantRequiredCaps {
				if gotCaps[i] != want {
					t.Errorf("requiredCapabilities[%d] = %q, want %q (must be sorted deterministically)", i, gotCaps[i], want)
				}
			}
		})
	}
}
