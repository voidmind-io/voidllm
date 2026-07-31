package mcp_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// newTestServer returns a fresh Server with the given name and version.
func newTestServer(name, version string) *mcp.Server {
	return mcp.NewServer(name, version)
}

// callRaw sends raw JSON to the server with no transport headers (the legacy
// path: no MCP-Protocol-Version header) and returns the parsed Response.
func callRaw(t *testing.T, s *mcp.Server, raw string) *mcp.Response {
	t.Helper()
	return callRawHdr(t, s, context.Background(), raw, mcp.MapHeader{})
}

// callRawCtx sends raw JSON to the server with a given context and no
// transport headers.
func callRawCtx(t *testing.T, s *mcp.Server, ctx context.Context, raw string) *mcp.Response {
	t.Helper()
	return callRawHdr(t, s, ctx, raw, mcp.MapHeader{})
}

// callRawHdr sends raw JSON to the server with the given context and
// transport headers, and returns the parsed Response.
func callRawHdr(t *testing.T, s *mcp.Server, ctx context.Context, raw string, hdr mcp.Header) *mcp.Response {
	t.Helper()
	result := s.Handle(ctx, []byte(raw), hdr)
	if result.Body == nil {
		return nil
	}
	var resp mcp.Response
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		t.Fatalf("unmarshal server response: %v\nraw: %s", err, result.Body)
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
		InputSchema: mcp.ObjectSchema(nil),
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("hello"), nil
	})
	s.RegisterTool(mcp.Tool{
		Name:        "farewell",
		Description: "Says goodbye.",
		InputSchema: mcp.ObjectSchema(nil),
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
		InputSchema: mcp.ObjectSchema(nil),
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
		InputSchema: mcp.ObjectSchema(nil),
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
		InputSchema: mcp.ObjectSchema(nil),
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
		InputSchema: mcp.ObjectSchema(nil),
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
		[]byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`), mcp.MapHeader{})

	if result.Body != nil {
		t.Errorf("notifications/initialized returned non-nil: %s", result.Body)
	}
	if result.Hint != mcp.HintNotification {
		t.Errorf("Hint = %v, want HintNotification", result.Hint)
	}
}

// TestServer_NotificationsInitialized_WithID_ReturnsValidJSONRPC is the exact
// reproduction for FIX A: a client that mistakenly attaches an id to the
// notifications/initialized notification must still get back a
// JSON-RPC-2.0-valid response — one that carries exactly one of "result" or
// "error", never neither. Before FIX A, dispatchLegacy returned (nil, nil)
// for this method, and because the request carried an id (IsNotification is
// false), Handle went on to call EncodeResult with a nil *Result: the
// legacy dialect's encoder rendered that as {"jsonrpc":"2.0","id":7} — no
// "result", no "error" — which is not a valid JSON-RPC 2.0 response.
func TestServer_NotificationsInitialized_WithID_ReturnsValidJSONRPC(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	result := s.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":7,"method":"notifications/initialized"}`), mcp.MapHeader{})

	if result.Hint == mcp.HintNotification {
		t.Fatal("Hint = HintNotification, but the request carried an id and therefore is NOT a " +
			"notification per JSON-RPC 2.0 — it must receive a response")
	}
	if result.Body == nil {
		t.Fatal("Body = nil, want a JSON-RPC-valid response body")
	}

	var raw map[string]any
	if err := json.Unmarshal(result.Body, &raw); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody: %s", err, result.Body)
	}

	_, hasResult := raw["result"]
	_, hasError := raw["error"]
	if hasResult == hasError {
		t.Fatalf("response must carry exactly one of \"result\"/\"error\", got result=%v error=%v: %s",
			hasResult, hasError, result.Body)
	}

	var resp mcp.Response
	if err := json.Unmarshal(result.Body, &resp); err != nil {
		t.Fatalf("unmarshal response: %v\nbody: %s", err, result.Body)
	}
	if string(resp.ID) != "7" {
		t.Errorf("ID = %q, want %q", string(resp.ID), "7")
	}
}

func TestServer_Notification_NullID(t *testing.T) {
	t.Parallel()

	// Any method with null ID (notification) returns nil.
	s := newTestServer("voidllm", "0.1.0")
	result := s.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":null,"method":"tools/list"}`), mcp.MapHeader{})

	if result.Body != nil {
		t.Errorf("null-ID request returned non-nil: %s", result.Body)
	}
}

func TestServer_Notification_AbsentID_UnknownMethod(t *testing.T) {
	t.Parallel()

	// An unknown method sent as a notification (no ID) should also return nil.
	s := newTestServer("voidllm", "0.1.0")
	result := s.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","method":"custom/event"}`), mcp.MapHeader{})

	if result.Body != nil {
		t.Errorf("notification with unknown method returned non-nil: %s", result.Body)
	}
}

// ---- Concurrent access ------------------------------------------------------

func TestServer_ConcurrentHandle(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "counter",
		InputSchema: mcp.ObjectSchema(nil),
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
			result := s.Handle(context.Background(), []byte(raw), mcp.MapHeader{})
			if result.Body == nil {
				errs <- fmt.Sprintf("goroutine %d: got nil response", id)
				return
			}
			var resp mcp.Response
			if err := json.Unmarshal(result.Body, &resp); err != nil {
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
		InputSchema: mcp.ObjectSchema(nil),
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
				InputSchema: mcp.ObjectSchema(nil),
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
		InputSchema: mcp.ObjectSchema(nil),
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
		InputSchema: mcp.ObjectSchema(nil),
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
		InputSchema: mcp.ObjectSchema(nil),
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		called++
		return mcp.TextResult("executed"), nil
	})

	// A notification has no "id" field at all.
	result := s.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","method":"tools/call","params":{"name":"side_effect_tool","arguments":{}}}`), mcp.MapHeader{})

	if result.Body != nil {
		t.Errorf("expected nil response for notification, got: %s", result.Body)
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
		InputSchema: mcp.ObjectSchema(nil),
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("ok"), nil
	})

	// Explicit null ID is treated as a notification by IsNotification().
	result := s.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":null,"method":"tools/call","params":{"name":"null_id_tool","arguments":{}}}`), mcp.MapHeader{})

	if result.Body != nil {
		t.Errorf("expected nil response for null-ID request, got: %s", result.Body)
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

// ---- server/discover (modern-era, SEP-2575 MUST) -----------------------------

// TestServer_Discover verifies server/discover reports every revision VoidLLM
// understands, its capabilities, human-readable instructions, and
// self-identifies under _meta["io.modelcontextprotocol/serverInfo"] — the
// modern-era replacement for the legacy initialize handshake.
func TestServer_Discover(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.4.0")
	resp := callRawHdr(t, s, context.Background(),
		modernRequestBody(1, "server/discover", nil, nil), modernHeader())

	assertNoError(t, resp)
	m := resultMap(t, resp)

	versionsRaw, ok := m["supportedVersions"].([]any)
	if !ok {
		t.Fatalf("supportedVersions type = %T, want []any", m["supportedVersions"])
	}
	got := make(map[string]bool, len(versionsRaw))
	for _, v := range versionsRaw {
		got[v.(string)] = true
	}
	for _, want := range []string{"2025-03-26", "2025-06-18", "2025-11-25", "2026-07-28"} {
		if !got[want] {
			t.Errorf("supportedVersions missing %q; got %v", want, versionsRaw)
		}
	}
	if len(versionsRaw) != 4 {
		t.Errorf("supportedVersions len = %d, want 4", len(versionsRaw))
	}

	caps, ok := m["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities type = %T, want map[string]any", m["capabilities"])
	}
	if _, ok := caps["tools"]; !ok {
		t.Errorf("capabilities.tools missing")
	}

	instructions, _ := m["instructions"].(string)
	if instructions == "" {
		t.Errorf("instructions missing or empty")
	}

	meta, ok := m["_meta"].(map[string]any)
	if !ok {
		t.Fatalf("_meta type = %T, want map[string]any", m["_meta"])
	}
	serverInfo, ok := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if !ok {
		t.Fatalf("_meta[serverInfo] type = %T, want map[string]any", meta["io.modelcontextprotocol/serverInfo"])
	}
	if serverInfo["name"] != "voidllm" || serverInfo["version"] != "0.4.0" {
		t.Errorf("serverInfo = %v, want {name:voidllm version:0.4.0}", serverInfo)
	}
}

// ---- tools/list ordering (SHOULD, minor changes: deterministic order) --------

// TestServer_ToolsList_DeterministicallySortedByName registers tools in
// deliberately unsorted order and verifies tools/list returns them sorted by
// name, so client-side caching and LLM prompt caches can rely on stable
// ordering (docs/mcp-v2.md §8 minor changes).
func TestServer_ToolsList_DeterministicallySortedByName(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	// Registered deliberately out of alphabetical order.
	for _, name := range []string{"zebra_tool", "alpha_tool", "mango_tool", "beta_tool"} {
		s.RegisterTool(mcp.Tool{Name: name, InputSchema: mcp.ObjectSchema(nil)},
			func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
				return mcp.TextResult("ok"), nil
			})
	}

	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	assertNoError(t, resp)

	m := resultMap(t, resp)
	toolsRaw, _ := m["tools"].([]any)
	if len(toolsRaw) != 4 {
		t.Fatalf("tools count = %d, want 4", len(toolsRaw))
	}

	want := []string{"alpha_tool", "beta_tool", "mango_tool", "zebra_tool"}
	for i, w := range want {
		tool, _ := toolsRaw[i].(map[string]any)
		if got := tool["name"]; got != w {
			t.Errorf("tools[%d].name = %v, want %q (registration order was NOT alphabetical: "+
				"zebra, alpha, mango, beta)", i, got, w)
		}
	}
}

// ---- Era-specific methods are rejected in the wrong era -----------------------

// TestServer_EraMismatch_MethodNotFound verifies that a legacy-only method
// sent under the modern era, and a modern-only method sent under the legacy
// era, both resolve to CodeMethodNotFound rather than being accidentally
// dispatched — the era-specific vocabularies are disjoint (docs/mcp-v2.md
// §3.1-3.3).
func TestServer_EraMismatch_MethodNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		hdr  mcp.Header
	}{
		{
			// Modern-era requests now require params._meta's two MUST fields
			// (docs/mcp-v2.md §3.2) to reach dispatch at all — modernRequestBody
			// supplies them so this case exercises era-mismatch dispatch, not
			// the separate missing-required-fields rejection.
			name: "initialize under the modern era is not found",
			body: modernRequestBody(1, "initialize", nil, nil),
			hdr:  modernHeader(),
		},
		{
			name: "notifications/initialized under the modern era is not found",
			body: modernRequestBody(1, "notifications/initialized", nil, nil),
			hdr:  modernHeader(),
		},
		{
			name: "ping under the modern era is not found",
			body: modernRequestBody(1, "ping", nil, nil),
			hdr:  modernHeader(),
		},
		{
			name: "server/discover under the legacy era is not found",
			body: `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`,
			hdr:  mcp.MapHeader{},
		},
		{
			name: "subscriptions/listen under the legacy era is not found",
			body: `{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen"}`,
			hdr:  mcp.MapHeader{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestServer("voidllm", "0.1.0")
			resp := callRawHdr(t, s, context.Background(), tc.body, tc.hdr)
			assertErrorCode(t, resp, mcp.CodeMethodNotFound)
		})
	}
}

// TestServer_SubscriptionsListen_NotYetSupportedInPhase1 documents and locks
// in the CURRENT, INTENTIONAL Phase 1 behavior: subscriptions/listen resolves
// to CodeMethodNotFound under the modern era. This is NOT a bug — VoidLLM's
// Handle returns a single, immediately-final []byte per call, and a
// spec-faithful subscriptions/listen requires a long-lived response stream
// that delivers notifications/subscriptions/acknowledged first and then stays
// open. That streaming passthrough is explicitly out of scope until Phase 4
// (see the Phase 1 report and dispatchModern's doc comment in server.go). If
// this test starts failing because subscriptions/listen now succeeds, update
// this test — don't treat the failure as a regression to revert.
func TestServer_SubscriptionsListen_NotYetSupportedInPhase1(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	resp := callRawHdr(t, s, context.Background(),
		modernRequestBody(1, "subscriptions/listen", map[string]any{"toolsListChanged": true}, nil),
		modernHeader())

	assertErrorCode(t, resp, mcp.CodeMethodNotFound)
	if resp.Error != nil && !strings.Contains(resp.Error.Message, "not yet supported") {
		t.Errorf("error message = %q, want it to explain streaming is not yet supported (Phase 4)", resp.Error.Message)
	}
}

// ---- hintForError / StatusHint mapping (FIX 2, MCP Streamable HTTP §4.5/§4.6) ---

// TestHintForError exhaustively verifies the JSON-RPC error code → StatusHint
// mapping HTTP-aware callers (internal/api/admin/mcp_handler.go) rely on to
// choose a status code, for every code/era combination hintForError branches
// on. The single highest-priority case here is the legacy CodeMethodNotFound
// regression: HTTP callers MUST keep responding 200 for it, never 404 — a 404
// on an endpoint a legacy client already knows exists would be misread as
// "wrong URL" and misdirect the client to the deprecated HTTP+SSE transport
// fallback (docs/mcp-v2.md §4.6).
func TestHintForError(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code int
		era  mcp.Era
		want mcp.StatusHint
	}{
		{"UnsupportedProtocolVersion (-32022), legacy era", mcp.CodeUnsupportedProtocolVersion, mcp.EraLegacy, mcp.HintBadRequest},
		{"UnsupportedProtocolVersion (-32022), modern era", mcp.CodeUnsupportedProtocolVersion, mcp.EraModern, mcp.HintBadRequest},
		{"HeaderMismatch (-32020), legacy era", mcp.CodeHeaderMismatch, mcp.EraLegacy, mcp.HintBadRequest},
		{"HeaderMismatch (-32020), modern era", mcp.CodeHeaderMismatch, mcp.EraModern, mcp.HintBadRequest},
		{"MissingRequiredClientCapability (-32021), legacy era", mcp.CodeMissingRequiredClientCapability, mcp.EraLegacy, mcp.HintBadRequest},
		{"MissingRequiredClientCapability (-32021), modern era", mcp.CodeMissingRequiredClientCapability, mcp.EraModern, mcp.HintBadRequest},
		{"MethodNotFound (-32601), modern era → HintMethodNotFound (HTTP 404)", mcp.CodeMethodNotFound, mcp.EraModern, mcp.HintMethodNotFound},
		{
			name: "REGRESSION (highest priority): MethodNotFound (-32601), legacy era → HintOK (HTTP 200), NEVER HintMethodNotFound/404",
			code: mcp.CodeMethodNotFound, era: mcp.EraLegacy, want: mcp.HintOK,
		},
		{"InvalidParams (-32602) is an ordinary protocol error, legacy era → HintOK", mcp.CodeInvalidParams, mcp.EraLegacy, mcp.HintOK},
		{"InvalidParams (-32602) is an ordinary protocol error, modern era → HintOK", mcp.CodeInvalidParams, mcp.EraModern, mcp.HintOK},
		{"InvalidRequest (-32600), legacy era → HintOK", mcp.CodeInvalidRequest, mcp.EraLegacy, mcp.HintOK},
		{"ParseError (-32700), modern era → HintOK", mcp.CodeParseError, mcp.EraModern, mcp.HintOK},
		{"InternalError (-32603), legacy era → HintOK", mcp.CodeInternalError, mcp.EraLegacy, mcp.HintOK},
		{"InternalError (-32603), modern era → HintOK", mcp.CodeInternalError, mcp.EraModern, mcp.HintOK},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := mcp.HintForError(tc.code, tc.era)
			if got != tc.want {
				t.Errorf("hintForError(%d, %v) = %v, want %v", tc.code, tc.era, got, tc.want)
			}
		})
	}
}

// ---- Era leak regression: dispatch is keyed on Negotiate's dialect, never --
// ---- on the body-derived Envelope.Version (docs/mcp-v2.md §4.6) -----------

// TestServer_EraLeak_LegacyInitializeWithModernBodyProtocolVersion_StaysLegacy
// is the single most important regression test of the whole dual-era
// rework. A legacy client sends a bare "initialize" handshake with NO
// MCP-Protocol-Version header — but its params.protocolVersion happens to
// name the modern 2026-07-28 revision (a dual-era-capable legacy client that
// already knows about the new revision, or simply a coincidence). Before
// dispatch was keyed on the dialect Negotiate resolved, a body-derived
// Envelope.Version could leak the modern era into dispatch: dispatchModern
// does not know "initialize" at all, so this would have incorrectly resolved
// to CodeMethodNotFound (-32601). The fix is that legacyDialect.Decode only
// ever refines Envelope.Version WITHIN the era Negotiate already chose (see
// the Era guard in dialect_legacy.go's Decode) — so this request must still
// receive a normal, valid legacy initialize response.
func TestServer_EraLeak_LegacyInitializeWithModernBodyProtocolVersion_StaysLegacy(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	resp := callRaw(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2026-07-28"}}`)

	assertNoError(t, resp)

	m := resultMap(t, resp)
	pv, _ := m["protocolVersion"].(string)
	if pv != string(mcp.V20250326) {
		t.Errorf("protocolVersion = %q, want %q — a body-claimed modern version must NOT leak the era, "+
			"since no MCP-Protocol-Version header was sent", pv, mcp.V20250326)
	}
	// A leaked-era response would carry the modern wire shape (resultType,
	// _meta.serverInfo) instead of the legacy shape (bare protocolVersion at
	// the top level) — assert the legacy shape explicitly.
	if _, ok := m["resultType"]; ok {
		t.Errorf("legacy result must not carry \"resultType\" (a modern-era field), got: %v", m)
	}
}

// TestServer_WithinEraRefinement_LegacyBodyCanRefineVersion verifies the
// companion, positive half of the era-leak guard: a legacy initialize body
// MAY still refine the negotiated version to a more specific one, as long as
// it stays within the same era (V20250326 → V20251125 here, both EraLegacy).
// Losing this would be an over-correction of the era-leak fix.
func TestServer_WithinEraRefinement_LegacyBodyCanRefineVersion(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	resp := callRaw(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)

	assertNoError(t, resp)

	m := resultMap(t, resp)
	pv, _ := m["protocolVersion"].(string)
	if pv != string(mcp.V20251125) {
		t.Errorf("protocolVersion = %q, want %q — a same-era body value must still be able to refine "+
			"the negotiated version", pv, mcp.V20251125)
	}
}

// ---- Tools() / OnToolsListHook: no aliasing with server-internal state (FIX 4) --

// TestServer_Tools_MutatingReturnedSchemaDoesNotAffectServerState verifies
// that mutating the InputSchema bytes of a Tool returned by Server.Tools()
// in place can never corrupt the Server's own registered schema — Tools()
// must return a byte-for-byte deep copy, not an aliased view.
func TestServer_Tools_MutatingReturnedSchemaDoesNotAffectServerState(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "t1",
		InputSchema: mcp.ObjectSchema(nil),
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("ok"), nil
	})

	got := s.Tools()
	if len(got) != 1 {
		t.Fatalf("Tools() len = %d, want 1", len(got))
	}
	// Corrupt the returned schema bytes in place.
	for i := range got[0].InputSchema {
		got[0].InputSchema[i] = 'X'
	}

	resp := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	assertNoError(t, resp)

	m := resultMap(t, resp)
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools/list tools count = %d, want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	schema, _ := tool["inputSchema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("server-internal schema was corrupted by mutating Tools()'s returned bytes: tool = %v", tool)
	}
}

// TestServer_OnToolsListHook_MutatingHookDoesNotCorruptServerState verifies
// the same aliasing guarantee for the tools slice handed to an
// OnToolsListHook: a hook that mutates the InputSchema bytes it was given in
// place — a legitimate, if unusual, thing for a hook to do to its own working
// copy — must never corrupt the Server's own registered schema for
// subsequent, unrelated calls.
func TestServer_OnToolsListHook_MutatingHookDoesNotCorruptServerState(t *testing.T) {
	t.Parallel()

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{
		Name:        "t1",
		InputSchema: mcp.ObjectSchema(nil),
	}, func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("ok"), nil
	})

	var hookRan bool
	s.SetOnToolsList(func(tools []mcp.Tool) []mcp.Tool {
		hookRan = true
		for i := range tools {
			// Mutate letters only, leaving JSON punctuation (quotes, braces,
			// colons) intact, so the mutated schema is still syntactically
			// valid JSON — an encoding failure from corrupting the JSON
			// syntax itself is a different scenario, covered separately by
			// TestServer_EncodingFailure_ToolsList_ReturnsInternalErrorNotNotification.
			for j := range tools[i].InputSchema {
				if b := tools[i].InputSchema[j]; b >= 'a' && b <= 'z' {
					tools[i].InputSchema[j] = 'x'
				}
			}
		}
		return tools
	})

	// First call: the hook corrupts ITS OWN copy in place, which is reflected
	// in this call's response — that part is the hook's own choice and not
	// under test here.
	resp1 := callRaw(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	assertNoError(t, resp1)
	if !hookRan {
		t.Fatal("OnToolsListHook was not invoked")
	}

	// Remove the hook and verify the Server's internally registered schema
	// for t1 is still the original, valid ObjectSchema — proving the hook's
	// in-place mutation never reached Server.tools.
	s.SetOnToolsList(nil)
	resp2 := callRaw(t, s, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	assertNoError(t, resp2)

	m := resultMap(t, resp2)
	tools, _ := m["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools/list tools count = %d, want 1", len(tools))
	}
	tool, _ := tools[0].(map[string]any)
	schema, _ := tool["inputSchema"].(map[string]any)
	if schema["type"] != "object" {
		t.Errorf("server-internal schema was corrupted by the hook's in-place mutation: tool = %v", tool)
	}
}

// ---- Encoding failure must never surface as a notification / 202 (FIX 3) --

// brokenSchemaTool returns a Tool whose InputSchema is deliberately invalid
// JSON, so that any dialect's EncodeResult attempting to marshal it fails.
// JSONSchema.MarshalJSON returns its bytes verbatim (see protocol.go), so
// encoding/json's own compaction step rejects them as malformed JSON at
// marshal time — this is the only realistic way to make a Server.Handle
// response fail to encode, since ToolResult's own fields (plain strings/bool)
// can never fail to marshal.
func brokenSchemaTool(name string) mcp.Tool {
	return mcp.Tool{Name: name, InputSchema: mcp.JSONSchema("not-valid-json")}
}

// TestServer_EncodingFailure_ToolsList_ReturnsInternalErrorNotNotification
// verifies FIX 3: when a dialect's EncodeResult fails to marshal a response,
// Handle must fall back to encodeFallback and report HintOK with a valid
// CodeInternalError body — NEVER HintNotification (which HTTP callers map to
// 202 Accepted with an empty body, silently discarding the failure). This is
// checked for both eras, since dialect2026's payloadAsMap takes a different
// code path than legacyDialect's direct marshal.
func TestServer_EncodingFailure_ToolsList_ReturnsInternalErrorNotNotification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		hdr  mcp.Header
	}{
		{"legacy era", mcp.MapHeader{}},
		{"modern era", modernHeader()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			s := newTestServer("voidllm", "0.1.0")
			s.RegisterTool(brokenSchemaTool("broken"),
				func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
					return mcp.TextResult("unreachable"), nil
				})

			// modernRequestBody's params._meta is harmless under the legacy
			// dialect (its Decode never inspects params content for
			// tools/list) and required under the modern one (docs/mcp-v2.md
			// §3.2), so the same body exercises both era subtests here.
			result := s.Handle(context.Background(),
				[]byte(modernRequestBody(1, "tools/list", nil, nil)), tc.hdr)

			if result.Hint == mcp.HintNotification {
				t.Fatal("Hint = HintNotification: an encoding failure must never be reported as a " +
					"notification — HTTP callers map this to 202 Accepted with no body, silently " +
					"discarding the failure")
			}
			if result.Hint != mcp.HintOK {
				t.Errorf("Hint = %v, want HintOK (encoding failures are still reported via a normal "+
					"JSON-RPC error body)", result.Hint)
			}
			if result.Body == nil {
				t.Fatal("Body = nil, want a valid JSON-RPC error response")
			}
			if !json.Valid(result.Body) {
				t.Fatalf("Body is not valid JSON: %s", result.Body)
			}

			var resp mcp.Response
			if err := json.Unmarshal(result.Body, &resp); err != nil {
				t.Fatalf("unmarshal response: %v\nraw: %s", err, result.Body)
			}
			if resp.Error == nil {
				t.Fatalf("Error is nil, want CodeInternalError; raw: %s", result.Body)
			}
			if resp.Error.Code != mcp.CodeInternalError {
				t.Errorf("Error.Code = %d, want %d (CodeInternalError)", resp.Error.Code, mcp.CodeInternalError)
			}
		})
	}
}

// TestServer_EncodingFailure_LoggedWithoutBodyContent verifies that the
// encoding-failure log line (logEncodeFailure) carries only the error cause
// and the JSON-RPC method name — never request or schema content — per
// VoidLLM's zero-knowledge logging rule. This test is deliberately NOT
// t.Parallel(): it swaps the process-global slog default logger for the
// duration of the call and must not race with other tests' log output.
func TestServer_EncodingFailure_LoggedWithoutBodyContent(t *testing.T) {
	const corruptSchemaMarker = "not-valid-json"

	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	s := newTestServer("voidllm", "0.1.0")
	s.RegisterTool(mcp.Tool{Name: "broken", InputSchema: mcp.JSONSchema(corruptSchemaMarker)},
		func(_ context.Context, _ json.RawMessage) (*mcp.ToolResult, error) {
			return mcp.TextResult("unreachable"), nil
		})

	result := s.Handle(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), mcp.MapHeader{})
	if result.Hint != mcp.HintOK {
		t.Fatalf("Hint = %v, want HintOK", result.Hint)
	}

	logged := buf.String()
	if !strings.Contains(logged, "mcp: failed to encode response") {
		t.Errorf("log output missing the expected encode-failure message; got: %s", logged)
	}
	if !strings.Contains(logged, "method=tools/list") {
		t.Errorf("log output missing method=tools/list; got: %s", logged)
	}
	if strings.Contains(logged, corruptSchemaMarker) {
		t.Errorf("log output leaked the corrupt schema/body content %q; got: %s", corruptSchemaMarker, logged)
	}
}
