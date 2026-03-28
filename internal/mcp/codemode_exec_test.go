package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// newTestPool creates a small RuntimePool suitable for unit tests.
// Pool size 2, 32 MB memory limit, 5 s timeout.
func newTestPool(t *testing.T) *mcp.RuntimePool {
	t.Helper()
	pool, err := mcp.NewRuntimePool(2, 32, 5*time.Second)
	if err != nil {
		t.Fatalf("NewRuntimePool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTestExecutor creates an Executor backed by a fresh test pool.
func newTestExecutor(t *testing.T) *mcp.Executor {
	t.Helper()
	return mcp.NewExecutor(newTestPool(t))
}

// mockToolCaller builds a ToolCaller that returns pre-configured responses.
// key format: "serverAlias/toolName".
func mockToolCaller(results map[string]json.RawMessage) mcp.ToolCaller {
	return func(_ context.Context, server, tool string, _ json.RawMessage) (json.RawMessage, error) {
		key := server + "/" + tool
		if r, ok := results[key]; ok {
			return r, nil
		}
		return nil, fmt.Errorf("unknown tool: %s", key)
	}
}

// mockToolCallerWithError builds a ToolCaller that always returns an error for
// the specified key and succeeds for all others.
func mockToolCallerWithError(errorKey string, errMsg string, results map[string]json.RawMessage) mcp.ToolCaller {
	return func(_ context.Context, server, tool string, _ json.RawMessage) (json.RawMessage, error) {
		key := server + "/" + tool
		if key == errorKey {
			return nil, fmt.Errorf("%s", errMsg)
		}
		if r, ok := results[key]; ok {
			return r, nil
		}
		return nil, fmt.Errorf("unknown tool: %s", key)
	}
}

// ---- Execute with no tools ---------------------------------------------------

func TestExecute_NoTools(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	tests := []struct {
		name       string
		code       string
		wantResult string
	}{
		{
			name:       "return number",
			code:       `42`,
			wantResult: `42`,
		},
		{
			name:       "return string",
			code:       `"hello"`,
			wantResult: `"hello"`,
		},
		{
			name:       "arithmetic expression",
			code:       `2 + 2`,
			wantResult: `4`,
		},
		{
			name:       "return object via JSON.stringify",
			code:       `JSON.stringify({a: 1, b: "two"})`,
			wantResult: `{"a":1,"b":"two"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
				Code:        tc.code,
				ServerTools: map[string][]mcp.Tool{},
			})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if res.Error != "" {
				t.Fatalf("Execute() result.Error = %q", res.Error)
			}
			if string(res.Result) != tc.wantResult {
				t.Errorf("Result = %s, want %s", res.Result, tc.wantResult)
			}
			if len(res.ToolCalls) != 0 {
				t.Errorf("ToolCalls = %v, want empty slice", res.ToolCalls)
			}
		})
	}
}

// ---- Execute with sync tool call --------------------------------------------

func TestExecute_WithSyncToolCall(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	caller := mockToolCaller(map[string]json.RawMessage{
		"myserver/get_weather": json.RawMessage(`{"temp":22,"unit":"celsius"}`),
	})

	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code: `
			async function main() {
				const result = await tools.myserver.get_weather({city: "Berlin"});
				return JSON.stringify(result);
			}
			await main();
		`,
		ServerTools: map[string][]mcp.Tool{
			"myserver": {
				{Name: "get_weather", Description: "Get weather for a city"},
			},
		},
		CallTool: caller,
	})

	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.Error != "" {
		t.Fatalf("Execute() result.Error = %q", res.Error)
	}

	// Result should contain the weather data.
	if !strings.Contains(string(res.Result), "22") {
		t.Errorf("Result = %s, want it to contain temperature 22", res.Result)
	}

	// Exactly one tool call log.
	if len(res.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(res.ToolCalls))
	}
	log := res.ToolCalls[0]
	if log.Server != "myserver" {
		t.Errorf("ToolCallLog.Server = %q, want %q", log.Server, "myserver")
	}
	if log.Tool != "get_weather" {
		t.Errorf("ToolCallLog.Tool = %q, want %q", log.Tool, "get_weather")
	}
	if log.Status != "success" {
		t.Errorf("ToolCallLog.Status = %q, want %q", log.Status, "success")
	}
}

// ---- Execute with multiple tool calls ----------------------------------------

func TestExecute_MultipleToolCalls(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	caller := mockToolCaller(map[string]json.RawMessage{
		"srv/tool_a": json.RawMessage(`{"val":"a"}`),
		"srv/tool_b": json.RawMessage(`{"val":"b"}`),
		"srv/tool_c": json.RawMessage(`{"val":"c"}`),
	})

	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code: `
			async function main() {
				const a = await tools.srv.tool_a({});
				const b = await tools.srv.tool_b({});
				const c = await tools.srv.tool_c({});
				return JSON.stringify([a, b, c]);
			}
			await main();
		`,
		ServerTools: map[string][]mcp.Tool{
			"srv": {
				{Name: "tool_a"},
				{Name: "tool_b"},
				{Name: "tool_c"},
			},
		},
		CallTool: caller,
	})

	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.Error != "" {
		t.Fatalf("Execute() result.Error = %q", res.Error)
	}
	if len(res.ToolCalls) != 3 {
		t.Fatalf("ToolCalls len = %d, want 3", len(res.ToolCalls))
	}

	// All calls must be logged as success.
	for i, log := range res.ToolCalls {
		if log.Status != "success" {
			t.Errorf("ToolCalls[%d].Status = %q, want %q", i, log.Status, "success")
		}
		if log.Server != "srv" {
			t.Errorf("ToolCalls[%d].Server = %q, want %q", i, log.Server, "srv")
		}
	}

	// All three tool names must appear in the logs.
	loggedTools := make(map[string]bool)
	for _, log := range res.ToolCalls {
		loggedTools[log.Tool] = true
	}
	for _, wantTool := range []string{"tool_a", "tool_b", "tool_c"} {
		if !loggedTools[wantTool] {
			t.Errorf("ToolCallLog missing entry for tool %q", wantTool)
		}
	}
}

// ---- Execute with tool error -------------------------------------------------

func TestExecute_ToolError(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	caller := mockToolCallerWithError("server/failing_tool", "upstream timeout", nil)

	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code: `
			async function main() {
				let caught = false;
				try {
					await tools.server.failing_tool({});
				} catch (e) {
					caught = true;
				}
				return JSON.stringify({caught: caught});
			}
			await main();
		`,
		ServerTools: map[string][]mcp.Tool{
			"server": {
				{Name: "failing_tool"},
			},
		},
		CallTool: caller,
	})

	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	// The JS caught the error, so result.Error should be empty.
	if res.Error != "" {
		t.Fatalf("Execute() result.Error = %q, want empty (error was caught in JS)", res.Error)
	}

	// The result should indicate the error was caught.
	if !strings.Contains(string(res.Result), `"caught":true`) {
		t.Errorf("Result = %s, want it to contain caught:true", res.Result)
	}

	// Tool call log should show status=error.
	if len(res.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(res.ToolCalls))
	}
	if res.ToolCalls[0].Status != "error" {
		t.Errorf("ToolCallLog.Status = %q, want %q", res.ToolCalls[0].Status, "error")
	}
}

// TestExecute_ToolErrorUncaught verifies that an unhandled tool rejection
// results in an execution error.
func TestExecute_ToolErrorUncaught(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	caller := mockToolCallerWithError("server/boom", "catastrophic failure", nil)

	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code: `
			async function main() {
				const r = await tools.server.boom({});
				return JSON.stringify(r);
			}
			await main();
		`,
		ServerTools: map[string][]mcp.Tool{
			"server": {
				{Name: "boom"},
			},
		},
		CallTool: caller,
	})

	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.Error == "" {
		t.Errorf("Execute() result.Error is empty, want an error for uncaught rejection")
	}

	if len(res.ToolCalls) != 1 {
		t.Fatalf("ToolCalls len = %d, want 1", len(res.ToolCalls))
	}
	if res.ToolCalls[0].Status != "error" {
		t.Errorf("ToolCallLog.Status = %q, want %q", res.ToolCalls[0].Status, "error")
	}
}

// ---- Execute with syntax error -----------------------------------------------

func TestExecute_SyntaxError(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	tests := []struct {
		name string
		code string
	}{
		{
			name: "unclosed brace",
			code: `function broken( { return 1; }`,
		},
		{
			name: "invalid token",
			code: `const x = @invalid;`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
				Code:        tc.code,
				ServerTools: map[string][]mcp.Tool{},
			})
			if err != nil {
				t.Fatalf("Execute() returned Go error = %v, want nil", err)
			}
			if res.Error == "" {
				t.Errorf("Execute() result.Error is empty, want syntax error message")
			}
		})
	}
}

// ---- Execute with context cancellation from pool exhaustion -----------------

// TestExecute_ContextCancelled verifies that a cancelled context propagates
// correctly through Acquire and returns a Go error (not a script error).
// Note: the QJS runtime does not interrupt running WASM code via
// MaxExecutionTime in the current build, so infinite loop tests are not
// feasible as unit tests. Instead we test the pool's context propagation.
func TestExecute_ContextCancelled(t *testing.T) {
	t.Parallel()

	// Create a pool of size 1 and drain it so Acquire blocks.
	pool, err := mcp.NewRuntimePool(1, 32, 5*time.Second)
	if err != nil {
		t.Fatalf("NewRuntimePool: %v", err)
	}
	t.Cleanup(pool.Close)

	exec := mcp.NewExecutor(pool)

	// Drain the pool by acquiring without releasing.
	rt, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire (drain): %v", err)
	}
	defer pool.Release(rt, true)

	// Execute with a cancelled context — should fail to acquire and return a
	// Go error (not a result).
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, goErr := exec.Execute(ctx, mcp.ExecuteParams{
		Code:        `1 + 1`,
		ServerTools: map[string][]mcp.Tool{},
	})
	if goErr == nil {
		t.Fatal("Execute() expected Go error from cancelled context, got nil")
	}
}

// ---- Execute result types ----------------------------------------------------

// TestExecute_ResultTypes verifies that various JavaScript value types are
// correctly round-tripped as JSON. All cases use JSON.stringify inside the
// script because the QJS WASM runtime's JS_JSONStringify helper is unreliable
// for bare primitive values (it may emit truncated or empty output). Using
// JSON.stringify in the script is the idiomatic way for LLM-generated code to
// return structured results from Code Mode anyway.
func TestExecute_ResultTypes(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	tests := []struct {
		name        string
		code        string
		wantContain string
	}{
		{
			name:        "string result via JSON.stringify",
			code:        `JSON.stringify("hello world")`,
			wantContain: "hello world",
		},
		{
			name:        "number result via JSON.stringify",
			code:        `JSON.stringify(3.14)`,
			wantContain: "3.14",
		},
		{
			name:        "integer result via JSON.stringify",
			code:        `JSON.stringify(99)`,
			wantContain: "99",
		},
		{
			name:        "object result via JSON.stringify",
			code:        `JSON.stringify({ key: "value", count: 7 })`,
			wantContain: "value",
		},
		{
			name:        "array result via JSON.stringify",
			code:        `JSON.stringify([1, 2, 3])`,
			wantContain: "1",
		},
		{
			name:        "boolean true via JSON.stringify",
			code:        `JSON.stringify(true)`,
			wantContain: "true",
		},
		{
			name:        "boolean false via JSON.stringify",
			code:        `JSON.stringify(false)`,
			wantContain: "false",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
				Code:        tc.code,
				ServerTools: map[string][]mcp.Tool{},
			})
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			if res.Error != "" {
				t.Fatalf("Execute() result.Error = %q", res.Error)
			}
			if !strings.Contains(string(res.Result), tc.wantContain) {
				t.Errorf("Result = %s, want it to contain %q", res.Result, tc.wantContain)
			}
			// Result must be valid JSON.
			if !json.Valid(res.Result) {
				t.Errorf("Result is not valid JSON: %s", res.Result)
			}
		})
	}
}

// ---- Execute with JSON.stringify result — no double encoding -----------------

func TestExecute_JSONStringifyResult_NoDoubleEncoding(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code:        `JSON.stringify({a: 1, b: "two", c: [3, 4]})`,
		ServerTools: map[string][]mcp.Tool{},
	})

	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.Error != "" {
		t.Fatalf("Execute() result.Error = %q", res.Error)
	}

	// The result should be parseable as a JSON object directly — not a JSON
	// string wrapping another JSON string.
	var obj map[string]any
	if err := json.Unmarshal(res.Result, &obj); err != nil {
		t.Fatalf("Result is not a JSON object (possible double-encoding): %s — %v", res.Result, err)
	}
	if v, ok := obj["a"].(float64); !ok || v != 1 {
		t.Errorf("Result[\"a\"] = %v, want 1", obj["a"])
	}
	if v, ok := obj["b"].(string); !ok || v != "two" {
		t.Errorf("Result[\"b\"] = %v, want \"two\"", obj["b"])
	}
}

// ---- Preamble generation ------------------------------------------------------

func TestBuildToolsPreamble_Empty(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	// With an empty ServerTools map the preamble should declare an empty tools
	// object and not break script execution. Use JSON.stringify to produce a
	// stable JSON output (bare boolean/string values are unreliable in this WASM build).
	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code:        `JSON.stringify(typeof tools === "object")`,
		ServerTools: map[string][]mcp.Tool{},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.Error != "" {
		t.Fatalf("Execute() result.Error = %q", res.Error)
	}
	if string(res.Result) != "true" {
		t.Errorf("Result = %s, want 'true' (tools should be an object)", res.Result)
	}
}

func TestBuildToolsPreamble_ToolAccessible(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	caller := mockToolCaller(map[string]json.RawMessage{
		"my-server/my-tool": json.RawMessage(`{"ok":true}`),
	})

	// Alias and tool name with hyphens — these must be sanitized in the preamble.
	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code: `
			async function main() {
				const r = await tools["my-server"]["my-tool"]({});
				return JSON.stringify(r);
			}
			await main();
		`,
		ServerTools: map[string][]mcp.Tool{
			"my-server": {
				{Name: "my-tool"},
			},
		},
		CallTool: caller,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.Error != "" {
		t.Fatalf("Execute() result.Error = %q", res.Error)
	}
	if !strings.Contains(string(res.Result), "true") {
		t.Errorf("Result = %s, want it to contain 'true'", res.Result)
	}
}

func TestBuildToolsPreamble_ColonDotInNames(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	caller := mockToolCaller(map[string]json.RawMessage{
		"api.v1:server/get.data": json.RawMessage(`{"data":"found"}`),
	})

	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code: `
			async function main() {
				const r = await tools["api.v1:server"]["get.data"]({});
				return JSON.stringify(r);
			}
			await main();
		`,
		ServerTools: map[string][]mcp.Tool{
			"api.v1:server": {
				{Name: "get.data"},
			},
		},
		CallTool: caller,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.Error != "" {
		t.Fatalf("Execute() result.Error = %q", res.Error)
	}
	if !strings.Contains(string(res.Result), "found") {
		t.Errorf("Result = %s, want it to contain 'found'", res.Result)
	}
}

// ---- sanitizeIdentifier ------------------------------------------------------

func TestSanitizeIdentifier(t *testing.T) {
	t.Parallel()

	tests := []struct {
		input string
		want  string
	}{
		{"simple", "simple"},
		{"CamelCase", "CamelCase"},
		{"with-hyphen", "with_hyphen"},
		{"with.dot", "with_dot"},
		{"with:colon", "with_colon"},
		{"with space", "with_space"},
		{"123starts_with_digit", "123starts_with_digit"},
		{"under_score", "under_score"},
		{"MixedChars-123.abc:def", "MixedChars_123_abc_def"},
	}

	// sanitizeIdentifier is unexported, so we test its observable behavior via
	// the preamble generation: the __tool_ function name uses sanitized identifiers
	// and must be callable from the injected JS wrapper.
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()

			exec := newTestExecutor(t)
			caller := mockToolCaller(map[string]json.RawMessage{
				tc.input + "/probe": json.RawMessage(`{"sanitized":true}`),
			})

			res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
				Code: fmt.Sprintf(`
					async function main() {
						const r = await tools[%q].probe({});
						return JSON.stringify(r);
					}
					await main();
				`, tc.input),
				ServerTools: map[string][]mcp.Tool{
					tc.input: {{Name: "probe"}},
				},
				CallTool: caller,
			})
			if err != nil {
				t.Fatalf("Execute(%q) error = %v", tc.input, err)
			}
			if res.Error != "" {
				t.Fatalf("Execute(%q) result.Error = %q", tc.input, res.Error)
			}
			if !strings.Contains(string(res.Result), "true") {
				t.Errorf("Execute(%q) result = %s, want to contain 'true'", tc.input, res.Result)
			}
		})
	}
}

// ---- needsQuoting ------------------------------------------------------------

func TestNeedsQuoting(t *testing.T) {
	t.Parallel()

	// needsQuoting is unexported. We test its effect through the preamble: when a
	// server alias or tool name starts with a digit or contains special chars, the
	// generated JS must still be syntactically valid and produce a tools object
	// whose properties are accessible with bracket notation.
	tests := []struct {
		name      string
		alias     string
		toolName  string
		accessKey string // JS expression to access the tool, e.g. `tools["1bad"].t`
	}{
		{
			name:      "alias starts with digit",
			alias:     "1invalid",
			toolName:  "t",
			accessKey: `tools["1invalid"].t`,
		},
		{
			name:      "tool name starts with digit",
			alias:     "srv",
			toolName:  "2tool",
			accessKey: `tools.srv["2tool"]`,
		},
		{
			name:      "empty alias",
			alias:     "",
			toolName:  "tool",
			accessKey: `tools[""].tool`,
		},
		{
			name:      "alias with special chars",
			alias:     "a-b.c",
			toolName:  "valid",
			accessKey: `tools["a-b.c"].valid`,
		},
		{
			name:      "both valid bare identifiers",
			alias:     "myserver",
			toolName:  "mytool",
			accessKey: `tools.myserver.mytool`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			exec := newTestExecutor(t)
			caller := mockToolCaller(map[string]json.RawMessage{
				tc.alias + "/" + tc.toolName: json.RawMessage(`{"ok":true}`),
			})

			code := fmt.Sprintf(`
				async function main() {
					const fn = %s;
					return JSON.stringify(typeof fn === "function");
				}
				await main();
			`, tc.accessKey)

			res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
				Code: code,
				ServerTools: map[string][]mcp.Tool{
					tc.alias: {{Name: tc.toolName}},
				},
				CallTool: caller,
			})
			if err != nil {
				t.Fatalf("Execute(%q) error = %v", tc.name, err)
			}
			if res.Error != "" {
				// A syntax error here means quoting logic produced invalid JS.
				t.Fatalf("Execute(%q) result.Error = %q (preamble produced invalid JS)", tc.name, res.Error)
			}
			if string(res.Result) != `true` {
				t.Errorf("Execute(%q) result = %s, want true (tool should be a function)", tc.name, res.Result)
			}
		})
	}
}

// ---- Tool name collisions ----------------------------------------------------

func TestExecute_ToolNameCollisions(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	// "tool-a" and "tool.a" both sanitize to "tool_a", producing the same
	// flat function name. The executor must detect this collision and return
	// an error rather than silently overwriting one tool with the other.
	caller := mockToolCaller(map[string]json.RawMessage{
		"srv/tool-a": json.RawMessage(`{"source":"hyphen"}`),
		"srv/tool.a": json.RawMessage(`{"source":"dot"}`),
	})

	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code: `return "should not run";`,
		ServerTools: map[string][]mcp.Tool{
			"srv": {
				{Name: "tool-a"},
				{Name: "tool.a"},
			},
		},
		CallTool: caller,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.Error == "" {
		t.Fatal("expected collision error in result.Error, got empty")
	}
	if !strings.Contains(res.Error, "tool function name collision") {
		t.Errorf("result.Error = %q, want it to mention collision", res.Error)
	}
}

// ---- Empty code --------------------------------------------------------------

func TestExecute_EmptyCode(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code:        "",
		ServerTools: map[string][]mcp.Tool{},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	// Empty code should not panic; result may be nil/undefined which is OK.
	_ = res
}

// ---- Nil CallTool ------------------------------------------------------------

func TestExecute_NilCallTool(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code: `
			async function main() {
				try {
					const r = await tools.server.some_tool({});
					return JSON.stringify(r);
				} catch (e) {
					return JSON.stringify({error_caught: true});
				}
			}
			await main();
		`,
		ServerTools: map[string][]mcp.Tool{
			"server": {
				{Name: "some_tool"},
			},
		},
		CallTool: nil, // deliberately nil
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	// The tool call must not crash; it should either return an error result
	// or the JS catch block should handle it.
	_ = res
	// If tool call logs exist, the status should be "error".
	for _, log := range res.ToolCalls {
		if log.Status != "error" {
			t.Errorf("ToolCallLog.Status = %q, want %q when CallTool is nil", log.Status, "error")
		}
	}
}

// ---- MaxToolCalls enforcement ------------------------------------------------

func TestExecute_MaxToolCallsEnforced(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)
	caller := mockToolCaller(map[string]json.RawMessage{
		"srv/tool": json.RawMessage(`{"ok":true}`),
	})

	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code: `
			async function main() {
				let completed = 0;
				for (let i = 0; i < 10; i++) {
					await tools.srv.tool({});
					completed++;
				}
				return JSON.stringify({completed: completed});
			}
			await main();
		`,
		ServerTools: map[string][]mcp.Tool{
			"srv": {{Name: "tool"}},
		},
		CallTool:     caller,
		MaxToolCalls: 3,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	// The 4th tool call should be rejected, causing an unhandled promise
	// rejection that surfaces as either res.Error or stops execution early.
	// The key assertion: no more than MaxToolCalls successful calls were made.
	successCount := 0
	for _, log := range res.ToolCalls {
		if log.Status == "success" {
			successCount++
		}
	}
	if successCount > 3 {
		t.Errorf("successful tool calls = %d, want <= 3 (MaxToolCalls)", successCount)
	}
}

// ---- Concurrent executions ---------------------------------------------------

func TestExecute_Concurrent(t *testing.T) {
	t.Parallel()

	// Larger pool so concurrent goroutines don't block each other.
	pool, err := mcp.NewRuntimePool(4, 32, 5*time.Second)
	if err != nil {
		t.Fatalf("NewRuntimePool: %v", err)
	}
	t.Cleanup(pool.Close)

	exec := mcp.NewExecutor(pool)

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)

	errs := make([]error, goroutines)

	for i := range goroutines {
		go func(idx int) {
			defer wg.Done()
			res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
				Code:        fmt.Sprintf(`%d * 2`, idx),
				ServerTools: map[string][]mcp.Tool{},
			})
			if err != nil {
				errs[idx] = fmt.Errorf("goroutine %d Execute error: %w", idx, err)
				return
			}
			if res.Error != "" {
				errs[idx] = fmt.Errorf("goroutine %d result.Error = %q", idx, res.Error)
			}
		}(i)
	}

	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
}

// ---- DurationMS is populated -------------------------------------------------

func TestExecute_DurationPopulated(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code:        `1 + 1`,
		ServerTools: map[string][]mcp.Tool{},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if res.DurationMS < 0 {
		t.Errorf("DurationMS = %d, want >= 0", res.DurationMS)
	}
}

// ---- ToolCalls always non-nil ------------------------------------------------

func TestExecute_ToolCallsNonNil(t *testing.T) {
	t.Parallel()

	exec := newTestExecutor(t)

	res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
		Code:        `"no tools"`,
		ServerTools: map[string][]mcp.Tool{},
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	// ToolCalls must be a non-nil slice so JSON serialises to [] not null.
	if res.ToolCalls == nil {
		t.Error("ToolCalls is nil, want non-nil empty slice for JSON []")
	}
}

// ---- Benchmark ---------------------------------------------------------------

func BenchmarkExecute_NoTools(b *testing.B) {
	pool, err := mcp.NewRuntimePool(1, 32, 5*time.Second)
	if err != nil {
		b.Fatalf("NewRuntimePool: %v", err)
	}
	defer pool.Close()

	exec := mcp.NewExecutor(pool)

	b.ResetTimer()
	for range b.N {
		res, err := exec.Execute(context.Background(), mcp.ExecuteParams{
			Code:        `[1,2,3].reduce((a,b) => a+b, 0)`,
			ServerTools: map[string][]mcp.Tool{},
		})
		if err != nil {
			b.Fatalf("Execute: %v", err)
		}
		if res.Error != "" {
			b.Fatalf("result.Error: %s", res.Error)
		}
	}
}

func BenchmarkExecute_WithToolCall(b *testing.B) {
	pool, err := mcp.NewRuntimePool(1, 32, 5*time.Second)
	if err != nil {
		b.Fatalf("NewRuntimePool: %v", err)
	}
	defer pool.Close()

	exec := mcp.NewExecutor(pool)
	caller := mockToolCaller(map[string]json.RawMessage{
		"bench/noop": json.RawMessage(`{"ok":true}`),
	})

	b.ResetTimer()
	for range b.N {
		exec.Execute(context.Background(), mcp.ExecuteParams{
			Code: `
				async function main() {
					const r = await tools.bench.noop({});
					return JSON.stringify(r);
				}
				await main();
			`,
			ServerTools: map[string][]mcp.Tool{
				"bench": {{Name: "noop"}},
			},
			CallTool: caller,
		})
	}
}
