package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// newTestServer returns a fresh Server with the given name and version.
func newTestServer(name, version string) *mcp.Server {
	return mcp.NewServer(name, version)
}

// callRaw sends raw JSON to the server and returns the parsed Response.
func callRaw(t *testing.T, s *mcp.Server, raw string) *mcp.Response {
	t.Helper()
	result := s.Handle(context.Background(), []byte(raw))
	if result == nil {
		return nil
	}
	var resp mcp.Response
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal server response: %v\nraw: %s", err, result)
	}
	return &resp
}

// callRawCtx sends raw JSON to the server with a given context.
func callRawCtx(t *testing.T, s *mcp.Server, ctx context.Context, raw string) *mcp.Response {
	t.Helper()
	result := s.Handle(ctx, []byte(raw))
	if result == nil {
		return nil
	}
	var resp mcp.Response
	if err := json.Unmarshal(result, &resp); err != nil {
		t.Fatalf("unmarshal server response: %v\nraw: %s", err, result)
	}
	return &resp
}

// assertNoError asserts resp has no error field set.
func assertNoError(t *testing.T, resp *mcp.Response) {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected error in response: code=%d msg=%q", resp.Error.Code, resp.Error.Message)
	}
}

// assertErrorCode asserts resp.Error is non-nil with the given code.
func assertErrorCode(t *testing.T, resp *mcp.Response, wantCode int) {
	t.Helper()
	if resp.Error == nil {
		t.Fatalf("expected error code %d but Error is nil; Result=%+v", wantCode, resp.Result)
	}
	if resp.Error.Code != wantCode {
		t.Errorf("Error.Code = %d, want %d (msg=%q)", resp.Error.Code, wantCode, resp.Error.Message)
	}
}

// resultMap decodes resp.Result into map[string]any and returns it.
func resultMap(t *testing.T, resp *mcp.Response) map[string]any {
	t.Helper()
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatalf("re-marshal result: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode result map: %v", err)
	}
	return m
}

// ---- Initialize -------------------------------------------------------------

func TestServer_Initialize(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize"}`)

	assertNoError(t, resp)

	m := resultMap(t, resp)

	pv, _ := m["protocolVersion"].(string)
	if pv == "" {
		t.Errorf("protocolVersion missing or empty")
	}

	caps, _ := m["capabilities"].(map[string]any)
	if caps == nil {
		t.Errorf("capabilities missing")
	}
	if _, ok := caps["tools"]; !ok {
		t.Errorf("capabilities.tools missing")
	}

	info, _ := m["serverInfo"].(map[string]any)
	if info == nil {
		t.Fatalf("serverInfo missing")
	}
	if info["name"] != "voidllm" {
		t.Errorf("serverInfo.name = %q, want %q", info["name"], "voidllm")
	}
	if info["version"] != "0.1.0" {
		t.Errorf("serverInfo.version = %q, want %q", info["version"], "0.1.0")
	}
}

func TestServer_Initialize_WithClientInfo(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.2.0")
	// clientInfo is accepted but not stored or reflected — should not crash.
	resp := callRaw(t, s,
		`{"jsonrpc":"2.0","id":"init-1","method":"initialize","params":{"clientInfo":{"name":"cursor","version":"1.0"}}}`)

	assertNoError(t, resp)

	m := resultMap(t, resp)
	if m["protocolVersion"] == nil {
		t.Errorf("protocolVersion missing from response")
	}
}

// ---- Ping -------------------------------------------------------------------

func TestServer_Ping(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":2,"method":"ping"}`)

	assertNoError(t, resp)

	// Result must be an empty object {}.
	b, _ := json.Marshal(resp.Result)
	if string(b) != "{}" {
		t.Errorf("ping result = %s, want {}", b)
	}
}

// ---- Tools/list -------------------------------------------------------------

func TestServer_ToolsList(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "greet",
		Description: "Greets the user.",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("hello"), nil
	})
	s.RegisterTool(mcp.Tool{
		Name:        "farewell",
		Description: "Says goodbye.",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("bye"), nil
	})

	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	assertNoError(t, resp)

	m := resultMap(t, resp)
	tools, _ := m["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools count = %d, want 2", len(tools))
	}

	// Verify tool names are present.
	names := make(map[string]bool)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true
	}
	for _, want := range []string{"greet", "farewell"} {
		if !names[want] {
			t.Errorf("tool %q not found in list", want)
		}
	}
}

func TestServer_ToolsList_Empty(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	assertNoError(t, resp)

	m := resultMap(t, resp)
	tools, ok := m["tools"]
	if !ok {
		t.Fatalf("tools key missing from result")
	}
	// tools should be an empty array ([]any{}) not nil after JSON round-trip.
	toolSlice, _ := tools.([]any)
	if len(toolSlice) != 0 {
		t.Errorf("tools = %v, want empty array", toolSlice)
	}
}

// ---- Tools/call -------------------------------------------------------------

func TestServer_ToolsCall_Success(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "echo",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("echoed"), nil
	})

	resp := callRaw(t, s,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"echo","arguments":{}}}`)
	assertNoError(t, resp)

	// Re-marshal result as ToolResult.
	b, _ := json.Marshal(resp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if tr.IsError {
		t.Errorf("IsError = true, want false")
	}
	if len(tr.Content) == 0 || tr.Content[0].Text != "echoed" {
		t.Errorf("Content = %+v, want text=echoed", tr.Content)
	}
}

func TestServer_ToolsCall_WithArguments(t *testing.T) {
	t.Parallel()

	type input struct {
		Message string `json:"message"`
	}

	var receivedMsg string
	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "parrot",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
		var in input
		if err := json.Unmarshal(args, &in); err != nil {
			return mcp.ErrorResult("bad args"), nil
		}
		receivedMsg = in.Message
		return mcp.TextResult(in.Message), nil
	})

	resp := callRaw(t, s,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"parrot","arguments":{"message":"hello from test"}}}`)
	assertNoError(t, resp)

	if receivedMsg != "hello from test" {
		t.Errorf("handler received message = %q, want %q", receivedMsg, "hello from test")
	}
}

func TestServer_ToolsCall_UnknownTool(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	resp := callRaw(t, s,
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"does_not_exist","arguments":{}}}`)

	assertErrorCode(t, resp, mcp.CodeInvalidParams)
}

func TestServer_ToolsCall_HandlerReturnsGoError(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "failing_tool",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return nil, errors.New("internal failure")
	})

	resp := callRaw(t, s,
		`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"failing_tool","arguments":{}}}`)

	// Go error → protocol-level success but with isError=true in ToolResult.
	assertNoError(t, resp)

	b, _ := json.Marshal(resp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if !tr.IsError {
		t.Errorf("IsError = false, want true")
	}
	if len(tr.Content) == 0 || tr.Content[0].Text == "" {
		t.Errorf("expected non-empty error message in Content, got %+v", tr.Content)
	}
}

func TestServer_ToolsCall_HandlerReturnsErrorResult(t *testing.T) {
	t.Parallel()

	const errMsg = "model_id parameter is required"

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "strict_tool",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.ErrorResult(errMsg), nil
	})

	resp := callRaw(t, s,
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":{"name":"strict_tool","arguments":{}}}`)
	assertNoError(t, resp)

	b, _ := json.Marshal(resp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if !tr.IsError {
		t.Errorf("IsError = false, want true")
	}
	if tr.Content[0].Text != errMsg {
		t.Errorf("Content[0].Text = %q, want %q", tr.Content[0].Text, errMsg)
	}
}

// ---- Error paths ------------------------------------------------------------

func TestServer_MethodNotFound(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":10,"method":"unknown/method"}`)

	assertErrorCode(t, resp, mcp.CodeMethodNotFound)
}

func TestServer_ParseError(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	resp := callRaw(t, s, `{invalid json`)

	assertErrorCode(t, resp, mcp.CodeParseError)
}

func TestServer_InvalidRequest_WrongVersion(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	resp := callRaw(t, s, `{"jsonrpc":"1.0","id":11,"method":"ping"}`)

	assertErrorCode(t, resp, mcp.CodeInvalidRequest)
}

// ---- Notifications ----------------------------------------------------------

func TestServer_Notification_NotificationsInitialized(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	result := s.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))

	if result != nil {
		t.Errorf("notifications/initialized returned non-nil: %s", result)
	}
}

func TestServer_Notification_NullID(t *testing.T) {
	t.Parallel()

	// Any method with null ID (notification) returns nil.
	s := newTestServer("voidllm", "0.1.0")
	result := s.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`))

	if result != nil {
		t.Errorf("null-ID request returned non-nil: %s", result)
	}
}

func TestServer_Notification_AbsentID_UnknownMethod(t *testing.T) {
	t.Parallel()

	// An unknown method sent as a notification (no ID) should also return nil.
	s := newTestServer("voidllm", "0.1.0")
	result := s.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","method":"custom/event"}`))

	if result != nil {
		t.Errorf("notification with unknown method returned non-nil: %s", result)
	}
}

// ---- Concurrent access ------------------------------------------------------

func TestServer_ConcurrentHandle(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "counter",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("ok"), nil
	})

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)

	errs := make(chan string, goroutines)

	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			raw := fmt.Sprintf(
				`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":"counter","arguments":{}}}`,
				id)
			result := s.Handle(context.Background(), []byte(raw))
			if result == nil {
				errs <- fmt.Sprintf("goroutine %d: got nil response", id)
				return
			}
			var resp mcp.Response
			if err := json.Unmarshal(result, &resp); err != nil {
				errs <- fmt.Sprintf("goroutine %d: unmarshal error: %v", id, err)
				return
			}
			if resp.Error != nil {
				errs <- fmt.Sprintf("goroutine %d: protocol error: %+v", id, resp.Error)
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for msg := range errs {
		t.Error(msg)
	}
}

// ---- RegisterTool appearance in tools/list ----------------------------------

func TestServer_RegisterTool_AppearsInList(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")

	// Verify empty list first.
	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	assertNoError(t, resp)
	m := resultMap(t, resp)
	tools, _ := m["tools"].([]any)
	if len(tools) != 0 {
		t.Fatalf("expected empty tools list before registration, got %d", len(tools))
	}

	// Register a tool.
	s.RegisterTool(mcp.Tool{
		Name:        "new_tool",
		Description: "A newly registered tool.",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("ok"), nil
	})

	// Verify it appears now.
	resp = callRaw(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	assertNoError(t, resp)
	m = resultMap(t, resp)
	tools, _ = m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool after registration, got %d", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	if tool["name"] != "new_tool" {
		t.Errorf("tool name = %q, want %q", tool["name"], "new_tool")
	}
	if tool["description"] != "A newly registered tool." {
		t.Errorf("tool description = %q, want %q", tool["description"], "A newly registered tool.")
	}
}

// ---- Edge cases: empty / null arguments -------------------------------------

func TestServer_ToolsCall_EmptyArguments(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params string
	}{
		{
			name:   "null arguments",
			params: `{"name":"safe_tool","arguments":null}`,
		},
		{
			name:   "empty object arguments",
			params: `{"name":"safe_tool","arguments":{}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestServer("voidllm", "0.1.0")
			s.RegisterTool(mcp.Tool{
				Name:        "safe_tool",
				InputSchema: mcp.InputSchema{Type: "object"},
			}, func(_ context.Context, args json.RawMessage) (*mcp.ToolResult, error) {
				// Tool must not crash on nil/empty args.
				return mcp.TextResult("safe"), nil
			})

			raw := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":%s}`,
				tc.params)
			resp := callRaw(t, s, raw)
			assertNoError(t, resp)
		})
	}
}

func TestServer_ToolsCall_MissingParams(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	// No params field at all — handleToolsCall should return an error.
	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`)

	assertErrorCode(t, resp, mcp.CodeInvalidParams)
}

func TestServer_ToolsCall_MissingName(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "named_tool",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("ok"), nil
	})

	// params without a name field — should resolve to unknown tool "".
	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"arguments":{}}}`)

	assertErrorCode(t, resp, mcp.CodeInvalidParams)
}

// ---- Error sanitization -----------------------------------------------------

// TestServer_ToolsCall_ErrorSanitized verifies that when a tool handler returns
// a Go error containing internal details (e.g. a database URL), the server
// returns a generic "internal error" message and does NOT forward the raw error
// to the caller. This tests Fix 3: error sanitization.
func TestServer_ToolsCall_ErrorSanitized(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "leaky_tool",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return nil, fmt.Errorf("database connection failed: postgres://user:pass@host/db")
	})

	resp := callRaw(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"leaky_tool","arguments":{}}}`)
	assertNoError(t, resp)

	b, _ := json.Marshal(resp.Result)
	var tr mcp.ToolResult
	if err := json.Unmarshal(b, &tr); err != nil {
		t.Fatalf("decode ToolResult: %v", err)
	}
	if !tr.IsError {
		t.Errorf("IsError = false, want true")
	}
	if len(tr.Content) == 0 {
		t.Fatal("Content is empty")
	}
	text := tr.Content[0].Text
	if !strings.Contains(text, "internal error") {
		t.Errorf("expected %q to contain %q", text, "internal error")
	}
	// Raw error details must not be exposed to the caller.
	if strings.Contains(text, "postgres://") {
		t.Errorf("response leaked internal error details: %q", text)
	}
	if strings.Contains(text, "database connection failed") {
		t.Errorf("response leaked internal error message: %q", text)
	}
}

// TestServer_ToolsCall_NotificationExecutes verifies that a tools/call sent as
// a notification (no ID field in the JSON) still invokes the tool handler but
// returns nil rather than a response. This tests Fix 6: spec-compliant
// notification behavior.
func TestServer_ToolsCall_NotificationExecutes(t *testing.T) {
	t.Parallel()

	var called int32
	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "side_effect_tool",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		called++
		return mcp.TextResult("executed"), nil
	})

	// A notification has no "id" field at all.
	result := s.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"side_effect_tool","arguments":{}}}`))

	if result != nil {
		t.Errorf("expected nil response for notification, got: %s", result)
	}
	if called != 1 {
		t.Errorf("tool handler called %d times, want 1", called)
	}
}

// TestServer_ToolsCall_NotificationNoResponse verifies that a tools/call with
// an explicit null ID (also a notification per the MCP/JSON-RPC spec) returns
// nil rather than a response body.
func TestServer_ToolsCall_NotificationNoResponse(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "null_id_tool",
		InputSchema: mcp.InputSchema{Type: "object"},
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("ok"), nil
	})

	// Explicit null ID is treated as a notification by IsNotification().
	result := s.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"null_id_tool","arguments":{}}}`))

	if result != nil {
		t.Errorf("expected nil response for null-ID request, got: %s", result)
	}
}

// ---- ID is echoed correctly -------------------------------------------------

func TestServer_ResponseID_EchoesRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		raw    string
		wantID string
	}{
		{
			name:   "numeric id",
			raw:    `{"jsonrpc":"2.0","id":42,"method":"ping"}`,
			wantID: "42",
		},
		{
			name:   "string id",
			raw:    `{"jsonrpc":"2.0","id":"my-req","method":"ping"}`,
			wantID: `"my-req"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestServer("voidllm", "0.1.0")
			resp := callRaw(t, s, tc.raw)
			if string(resp.ID) != tc.wantID {
				t.Errorf("response ID = %q, want %q", string(resp.ID), tc.wantID)
			}
		})
	}
}
