package mcp_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---- GenerateToolTypeDefs ---------------------------------------------------

func TestGenerateToolTypeDefs_Empty(t *testing.T) {
	t.Parallel()

	got := mcp.GenerateToolTypeDefs(map[string][]mcp.Tool{})
	if got != "" {
		t.Errorf("GenerateToolTypeDefs(empty) = %q, want empty string", got)
	}
}

func TestGenerateToolTypeDefs_NilMap(t *testing.T) {
	t.Parallel()

	got := mcp.GenerateToolTypeDefs(nil)
	if got != "" {
		t.Errorf("GenerateToolTypeDefs(nil) = %q, want empty string", got)
	}
}

func TestGenerateToolTypeDefs_SingleServerSingleTool(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		tool        mcp.Tool
		wantContain []string
	}{
		{
			name: "string param",
			tool: mcp.Tool{
				Name:        "search",
				Description: "Search the web.",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]mcp.Property{
						"query": {Type: "string"},
					},
					Required: []string{"query"},
				},
			},
			wantContain: []string{
				"declare namespace tools.aws",
				"function search",
				"query: string",
				"Promise<any>",
				"/** Search the web. */",
			},
		},
		{
			name: "number param",
			tool: mcp.Tool{
				Name:        "paginate",
				Description: "Paginate results.",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]mcp.Property{
						"limit": {Type: "number"},
					},
					Required: []string{"limit"},
				},
			},
			wantContain: []string{
				"limit: number",
			},
		},
		{
			name: "integer param",
			tool: mcp.Tool{
				Name:        "offset_tool",
				Description: "Offset results.",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]mcp.Property{
						"offset": {Type: "integer"},
					},
					Required: []string{"offset"},
				},
			},
			wantContain: []string{
				"offset: number",
			},
		},
		{
			name: "boolean param",
			tool: mcp.Tool{
				Name:        "toggle",
				Description: "Toggle a flag.",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]mcp.Property{
						"enabled": {Type: "boolean"},
					},
					Required: []string{"enabled"},
				},
			},
			wantContain: []string{
				"enabled: boolean",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := mcp.GenerateToolTypeDefs(map[string][]mcp.Tool{
				"aws": {tc.tool},
			})
			for _, want := range tc.wantContain {
				if !strings.Contains(got, want) {
					t.Errorf("output does not contain %q\nfull output:\n%s", want, got)
				}
			}
		})
	}
}

func TestGenerateToolTypeDefs_RequiredVsOptional(t *testing.T) {
	t.Parallel()

	tool := mcp.Tool{
		Name: "mixed_params",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]mcp.Property{
				"required_field": {Type: "string"},
				"optional_field": {Type: "string"},
			},
			Required: []string{"required_field"},
		},
	}

	got := mcp.GenerateToolTypeDefs(map[string][]mcp.Tool{
		"srv": {tool},
	})

	// required_field has no ?; optional_field has ?
	if !strings.Contains(got, "required_field: string") {
		t.Errorf("required param should not have '?', got:\n%s", got)
	}
	if strings.Contains(got, "required_field?: string") {
		t.Errorf("required param incorrectly annotated as optional, got:\n%s", got)
	}
	if !strings.Contains(got, "optional_field?: string") {
		t.Errorf("optional param should have '?', got:\n%s", got)
	}
}

func TestGenerateToolTypeDefs_ArrayAndObjectParams(t *testing.T) {
	t.Parallel()

	tool := mcp.Tool{
		Name: "complex_tool",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]mcp.Property{
				"tags":     {Type: "array"},
				"metadata": {Type: "object"},
			},
		},
	}

	got := mcp.GenerateToolTypeDefs(map[string][]mcp.Tool{
		"srv": {tool},
	})

	if !strings.Contains(got, "tags?: any[]") {
		t.Errorf("array param should map to 'any[]', got:\n%s", got)
	}
	if !strings.Contains(got, "metadata?: Record<string, any>") {
		t.Errorf("object param should map to 'Record<string, any>', got:\n%s", got)
	}
}

func TestGenerateToolTypeDefs_UnknownTypeMapToAny(t *testing.T) {
	t.Parallel()

	tool := mcp.Tool{
		Name: "weird_tool",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]mcp.Property{
				"val": {Type: ""},
			},
		},
	}

	got := mcp.GenerateToolTypeDefs(map[string][]mcp.Tool{
		"srv": {tool},
	})

	if !strings.Contains(got, "val?: any") {
		t.Errorf("property with no type should map to 'any', got:\n%s", got)
	}
}

func TestGenerateToolTypeDefs_EmptyDescription(t *testing.T) {
	t.Parallel()

	tool := mcp.Tool{
		Name:        "no_desc",
		Description: "",
		InputSchema: mcp.InputSchema{Type: "object"},
	}

	got := mcp.GenerateToolTypeDefs(map[string][]mcp.Tool{
		"srv": {tool},
	})

	// No JSDoc comment should be emitted.
	if strings.Contains(got, "/**") {
		t.Errorf("empty description should produce no JSDoc comment, got:\n%s", got)
	}
	if !strings.Contains(got, "function no_desc") {
		t.Errorf("function declaration missing, got:\n%s", got)
	}
}

func TestGenerateToolTypeDefs_LongDescriptionTruncated(t *testing.T) {
	t.Parallel()

	// A description with two sentences — only the first should appear.
	tool := mcp.Tool{
		Name:        "verbose",
		Description: "First sentence. Second sentence with extra details.",
		InputSchema: mcp.InputSchema{Type: "object"},
	}

	got := mcp.GenerateToolTypeDefs(map[string][]mcp.Tool{
		"srv": {tool},
	})

	if !strings.Contains(got, "First sentence.") {
		t.Errorf("first sentence should appear in output, got:\n%s", got)
	}
	if strings.Contains(got, "Second sentence") {
		t.Errorf("second sentence should be truncated from output, got:\n%s", got)
	}
}

func TestGenerateToolTypeDefs_LongDescriptionNewlineTruncated(t *testing.T) {
	t.Parallel()

	tool := mcp.Tool{
		Name:        "multiline",
		Description: "First line\nSecond line with more info.",
		InputSchema: mcp.InputSchema{Type: "object"},
	}

	got := mcp.GenerateToolTypeDefs(map[string][]mcp.Tool{
		"srv": {tool},
	})

	if !strings.Contains(got, "First line") {
		t.Errorf("first line should appear in output, got:\n%s", got)
	}
	if strings.Contains(got, "Second line") {
		t.Errorf("second line should be truncated from output, got:\n%s", got)
	}
}

func TestGenerateToolTypeDefs_MultipleServersSortedAlphabetically(t *testing.T) {
	t.Parallel()

	tools := map[string][]mcp.Tool{
		"zebra": {{Name: "tool_z", InputSchema: mcp.InputSchema{Type: "object"}}},
		"alpha": {{Name: "tool_a", InputSchema: mcp.InputSchema{Type: "object"}}},
		"mango": {{Name: "tool_m", InputSchema: mcp.InputSchema{Type: "object"}}},
	}

	got := mcp.GenerateToolTypeDefs(tools)

	posAlpha := strings.Index(got, "tools.alpha")
	posMango := strings.Index(got, "tools.mango")
	posZebra := strings.Index(got, "tools.zebra")

	if posAlpha < 0 || posMango < 0 || posZebra < 0 {
		t.Fatalf("one or more namespace declarations missing:\n%s", got)
	}
	if !(posAlpha < posMango && posMango < posZebra) {
		t.Errorf("namespaces not in alphabetical order: alpha@%d mango@%d zebra@%d\n%s",
			posAlpha, posMango, posZebra, got)
	}
}

func TestGenerateToolTypeDefs_ToolNamesWithHyphensAndDots(t *testing.T) {
	t.Parallel()

	tool := mcp.Tool{
		Name:        "get-user.profile",
		Description: "Fetch user profile.",
		InputSchema: mcp.InputSchema{Type: "object"},
	}

	got := mcp.GenerateToolTypeDefs(map[string][]mcp.Tool{
		"my-api.v1": {tool},
	})

	// Aliases with special chars use bracket notation comment + sanitized namespace.
	if !strings.Contains(got, `tools["my-api.v1"]`) {
		t.Errorf("server alias should appear in bracket notation comment, got:\n%s", got)
	}
	if !strings.Contains(got, "tools_my_api_v1") {
		t.Errorf("sanitized namespace should appear, got:\n%s", got)
	}
	// Tool names with special chars are emitted as bracket-notation comments
	// instead of function declarations (prevents TS injection).
	if !strings.Contains(got, `tools["<alias>"]["get-user.profile"]`) {
		t.Errorf("tool name should appear in bracket notation comment, got:\n%s", got)
	}
	// Should NOT contain a function declaration for the invalid identifier.
	if strings.Contains(got, "function get-user.profile") {
		t.Errorf("should not emit function declaration for invalid identifier, got:\n%s", got)
	}
}

func TestGenerateToolTypeDefs_NoPropertiesProducesEmptyArgs(t *testing.T) {
	t.Parallel()

	tool := mcp.Tool{
		Name:        "no_args",
		InputSchema: mcp.InputSchema{Type: "object"},
	}

	got := mcp.GenerateToolTypeDefs(map[string][]mcp.Tool{
		"srv": {tool},
	})

	// args block should be inline with no newlines between braces.
	if !strings.Contains(got, "function no_args(args: {})") {
		t.Errorf("no-property tool should have empty inline args block, got:\n%s", got)
	}
}

func TestGenerateToolTypeDefs_ServerWithNoToolsOmitted(t *testing.T) {
	t.Parallel()

	// A server entry with an empty tools slice should produce no output.
	tools := map[string][]mcp.Tool{
		"real_server":  {{Name: "do_something", InputSchema: mcp.InputSchema{Type: "object"}}},
		"empty_server": {},
	}

	got := mcp.GenerateToolTypeDefs(tools)

	if strings.Contains(got, "tools.empty_server") {
		t.Errorf("server with no tools should be omitted, got:\n%s", got)
	}
	if !strings.Contains(got, "tools.real_server") {
		t.Errorf("server with tools should appear, got:\n%s", got)
	}
}

func TestGenerateToolTypeDefs_ToolsSortedWithinNamespace(t *testing.T) {
	t.Parallel()

	tools := map[string][]mcp.Tool{
		"srv": {
			{Name: "zzz_last", InputSchema: mcp.InputSchema{Type: "object"}},
			{Name: "aaa_first", InputSchema: mcp.InputSchema{Type: "object"}},
			{Name: "mmm_middle", InputSchema: mcp.InputSchema{Type: "object"}},
		},
	}

	got := mcp.GenerateToolTypeDefs(tools)

	posFirst := strings.Index(got, "aaa_first")
	posMiddle := strings.Index(got, "mmm_middle")
	posLast := strings.Index(got, "zzz_last")

	if posFirst < 0 || posMiddle < 0 || posLast < 0 {
		t.Fatalf("one or more tool declarations missing:\n%s", got)
	}
	if !(posFirst < posMiddle && posMiddle < posLast) {
		t.Errorf("tools not sorted alphabetically within namespace: first@%d middle@%d last@%d\n%s",
			posFirst, posMiddle, posLast, got)
	}
}

func TestGenerateToolTypeDefs_DeterministicOutput(t *testing.T) {
	t.Parallel()

	// Running GenerateToolTypeDefs twice on the same input must yield identical
	// results — no map iteration order non-determinism.
	tools := map[string][]mcp.Tool{
		"beta":  {{Name: "tool_b", InputSchema: mcp.InputSchema{Type: "object"}}},
		"alpha": {{Name: "tool_a", InputSchema: mcp.InputSchema{Type: "object"}}},
	}

	first := mcp.GenerateToolTypeDefs(tools)
	second := mcp.GenerateToolTypeDefs(tools)

	if first != second {
		t.Errorf("non-deterministic output:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestGenerateToolTypeDefs_OutputStructure(t *testing.T) {
	t.Parallel()

	// Full structural check: one server, one tool with required + optional params.
	tool := mcp.Tool{
		Name:        "find_user",
		Description: "Find a user by ID.",
		InputSchema: mcp.InputSchema{
			Type: "object",
			Properties: map[string]mcp.Property{
				"id":    {Type: "string"},
				"limit": {Type: "number"},
			},
			Required: []string{"id"},
		},
	}

	got := mcp.GenerateToolTypeDefs(map[string][]mcp.Tool{
		"users": {tool},
	})

	wantParts := []string{
		"declare namespace tools.users {",
		"/** Find a user by ID. */",
		"function find_user(args: {",
		"id: string;",
		"limit?: number;",
		"}): Promise<any>;",
		"}",
	}
	for _, part := range wantParts {
		if !strings.Contains(got, part) {
			t.Errorf("output missing expected fragment %q\nfull output:\n%s", part, got)
		}
	}
}

// ---- SetOnToolsList / OnToolsListHook ---------------------------------------

// TestServer_SetOnToolsList_BasicModification verifies that a hook registered
// via SetOnToolsList is called during tools/list and that its return value
// replaces the tool list in the response.
func TestServer_SetOnToolsList_BasicModification(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "original_tool",
		Description: "Original description.",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("ok"), nil
	})

	// Hook appends a modified description note.
	s.SetOnToolsList(func(tools []mcp.Tool) []mcp.Tool {
		for i := range tools {
			tools[i].Description = tools[i].Description + " [modified by hook]"
		}
		return tools
	})

	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	assertNoError(t, resp)

	m := resultMap(t, resp)
	toolsRaw, _ := m["tools"].([]any)
	if len(toolsRaw) != 1 {
		t.Fatalf("tools count = %d, want 1", len(toolsRaw))
	}
	tool, _ := toolsRaw[0].(map[string]any)
	desc, _ := tool["description"].(string)
	if !strings.Contains(desc, "[modified by hook]") {
		t.Errorf("tool description = %q, want it to contain '[modified by hook]'", desc)
	}
}

// TestServer_SetOnToolsList_FilterTools verifies that a hook can remove tools
// from the list returned to the caller.
func TestServer_SetOnToolsList_FilterTools(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{Name: "public_tool", InputSchema: mcp.InputSchema{Type: "object"}},
		func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			return mcp.TextResult("ok"), nil
		})
	s.RegisterTool(mcp.Tool{Name: "hidden_tool", InputSchema: mcp.InputSchema{Type: "object"}},
		func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			return mcp.TextResult("ok"), nil
		})

	// Hook filters out any tool whose name contains "hidden".
	s.SetOnToolsList(func(tools []mcp.Tool) []mcp.Tool {
		visible := tools[:0]
		for _, t := range tools {
			if !strings.Contains(t.Name, "hidden") {
				visible = append(visible, t)
			}
		}
		return visible
	})

	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	assertNoError(t, resp)

	m := resultMap(t, resp)
	toolsRaw, _ := m["tools"].([]any)
	if len(toolsRaw) != 1 {
		t.Fatalf("tools count = %d, want 1 (hidden_tool should be filtered)", len(toolsRaw))
	}
	tool, _ := toolsRaw[0].(map[string]any)
	if tool["name"] != "public_tool" {
		t.Errorf("visible tool name = %q, want %q", tool["name"], "public_tool")
	}
}

// TestServer_SetOnToolsList_HookReceivesCopy verifies that modifications to the
// slice passed to the hook do not mutate the server's internal tool registry.
// Subsequent tools/list calls (with no hook) must see the original descriptions.
func TestServer_SetOnToolsList_HookReceivesCopy(t *testing.T) {
	t.Parallel()

	const originalDesc = "Original description."

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "my_tool",
		Description: originalDesc,
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("ok"), nil
	})

	// Hook mutates the slice it receives (tests that this is a copy).
	s.SetOnToolsList(func(tools []mcp.Tool) []mcp.Tool {
		for i := range tools {
			tools[i].Description = "mutated"
		}
		return tools
	})

	// First call — hook mutates the copy.
	resp1 := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	assertNoError(t, resp1)

	// Remove the hook and call again — original description must be intact.
	s.SetOnToolsList(nil)

	resp2 := callRaw(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	assertNoError(t, resp2)

	m2 := resultMap(t, resp2)
	toolsRaw2, _ := m2["tools"].([]any)
	if len(toolsRaw2) != 1 {
		t.Fatalf("tools count = %d, want 1", len(toolsRaw2))
	}
	tool2, _ := toolsRaw2[0].(map[string]any)
	desc2, _ := tool2["description"].(string)
	if desc2 != originalDesc {
		t.Errorf("description after nil hook = %q, want %q (hook mutated internal state)", desc2, originalDesc)
	}
}

// TestServer_SetOnToolsList_NilHookClearsHook verifies that passing nil to
// SetOnToolsList removes any previously registered hook.
func TestServer_SetOnToolsList_NilHookClearsHook(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{Name: "a_tool", InputSchema: mcp.InputSchema{Type: "object"}},
		func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			return mcp.TextResult("ok"), nil
		})

	var hookCalled bool
	s.SetOnToolsList(func(tools []mcp.Tool) []mcp.Tool {
		hookCalled = true
		return tools
	})

	// Clear the hook.
	s.SetOnToolsList(nil)

	// Call tools/list — hook must NOT be invoked.
	_ = callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if hookCalled {
		t.Error("hook was called after being cleared with nil")
	}
}

// TestServer_SetOnToolsList_HookCanReturnEmpty verifies that a hook may return
// an empty slice, producing a tools/list response with no tools.
func TestServer_SetOnToolsList_HookCanReturnEmpty(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{Name: "some_tool", InputSchema: mcp.InputSchema{Type: "object"}},
		func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			return mcp.TextResult("ok"), nil
		})

	s.SetOnToolsList(func(_ []mcp.Tool) []mcp.Tool {
		return []mcp.Tool{}
	})

	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	assertNoError(t, resp)

	m := resultMap(t, resp)
	toolsRaw, _ := m["tools"].([]any)
	if len(toolsRaw) != 0 {
		t.Errorf("tools count = %d, want 0 (hook returned empty slice)", len(toolsRaw))
	}
}

// TestServer_SetOnToolsList_ConcurrentAccess verifies that concurrent calls to
// SetOnToolsList and Handle (tools/list) do not produce data races.
func TestServer_SetOnToolsList_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{Name: "concurrent_tool", InputSchema: mcp.InputSchema{Type: "object"}},
		func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			return mcp.TextResult("ok"), nil
		})

	var wg sync.WaitGroup
	const goroutines = 20

	// Goroutines that keep calling tools/list.
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_ = s.Handle(context.Background(),
					[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
			}
		}()
	}

	// Goroutines that keep swapping the hook.
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 10 {
				if i%2 == 0 {
					s.SetOnToolsList(func(tools []mcp.Tool) []mcp.Tool { return tools })
				} else {
					s.SetOnToolsList(nil)
				}
			}
		}()
	}

	wg.Wait()
}

// TestServer_SetOnToolsList_HookInvocationCount verifies the hook is called
// exactly once per tools/list request and not during tools/call.
func TestServer_SetOnToolsList_HookInvocationCount(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{Name: "probe", InputSchema: mcp.InputSchema{Type: "object"}},
		func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			return mcp.TextResult("ok"), nil
		})

	var callCount int
	s.SetOnToolsList(func(tools []mcp.Tool) []mcp.Tool {
		callCount++
		return tools
	})

	// Two tools/list calls → two hook invocations.
	_ = callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	_ = callRaw(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)

	if callCount != 2 {
		t.Errorf("hook invocation count = %d, want 2", callCount)
	}

	// A tools/call must NOT trigger the hook.
	_ = callRaw(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"probe","arguments":{}}}`)
	if callCount != 2 {
		t.Errorf("hook invocation count = %d after tools/call, want still 2", callCount)
	}
}
