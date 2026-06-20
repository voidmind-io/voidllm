package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- TransformRequest -------------------------------------------------------

func TestAnthropicTransformRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// input is valid OpenAI-format JSON fed to TransformRequest.
		input string
		// checkFn receives the parsed output document and asserts expectations.
		checkFn func(t *testing.T, doc map[string]json.RawMessage)
		wantErr bool
	}{
		{
			name:  "system message extracted to top-level field and removed from messages",
			input: `{"model":"claude-3","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"Hi"}]}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()

				// "system" top-level field must be present.
				raw, ok := doc["system"]
				if !ok {
					t.Fatal("output missing top-level 'system' field")
				}
				var system string
				if err := json.Unmarshal(raw, &system); err != nil {
					t.Fatalf("unmarshal system: %v", err)
				}
				if system != "You are helpful." {
					t.Errorf("system = %q, want %q", system, "You are helpful.")
				}

				// "messages" must not contain the system entry.
				var msgs []anthropicOutboundMessage
				if err := json.Unmarshal(doc["messages"], &msgs); err != nil {
					t.Fatalf("unmarshal messages: %v", err)
				}
				for _, m := range msgs {
					if m.Role == "system" {
						t.Error("messages still contains a system-role entry")
					}
				}
				if len(msgs) != 1 {
					t.Errorf("len(messages) = %d, want 1", len(msgs))
				}
				if msgs[0].Role != "user" {
					t.Errorf("remaining message role = %q, want %q", msgs[0].Role, "user")
				}
				if len(msgs[0].Content) != 1 || msgs[0].Content[0].Text != "Hi" {
					t.Errorf("remaining message content = %v, want text block 'Hi'", msgs[0].Content)
				}
			},
		},
		{
			name:  "multiple system messages joined with newline",
			input: `{"model":"claude-3","messages":[{"role":"system","content":"Part one."},{"role":"system","content":"Part two."},{"role":"user","content":"Hello"}]}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				raw, ok := doc["system"]
				if !ok {
					t.Fatal("output missing top-level 'system' field")
				}
				var system string
				if err := json.Unmarshal(raw, &system); err != nil {
					t.Fatalf("unmarshal system: %v", err)
				}
				if system != "Part one.\nPart two." {
					t.Errorf("system = %q, want %q", system, "Part one.\nPart two.")
				}
			},
		},
		{
			name:  "no system message produces no system field",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}]}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				if _, ok := doc["system"]; ok {
					t.Error("unexpected 'system' field in output when no system message was present")
				}
			},
		},
		{
			name:  "missing max_tokens defaults to 4096",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}]}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				raw, ok := doc["max_tokens"]
				if !ok {
					t.Fatal("output missing 'max_tokens' field")
				}
				var maxTok int
				if err := json.Unmarshal(raw, &maxTok); err != nil {
					t.Fatalf("unmarshal max_tokens: %v", err)
				}
				if maxTok != 4096 {
					t.Errorf("max_tokens = %d, want 4096", maxTok)
				}
			},
		},
		{
			name:  "present max_tokens is preserved",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"max_tokens":1024}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				raw, ok := doc["max_tokens"]
				if !ok {
					t.Fatal("output missing 'max_tokens' field")
				}
				var maxTok int
				if err := json.Unmarshal(raw, &maxTok); err != nil {
					t.Fatalf("unmarshal max_tokens: %v", err)
				}
				if maxTok != 1024 {
					t.Errorf("max_tokens = %d, want 1024", maxTok)
				}
			},
		},
		{
			name:  "OpenAI-only fields are stripped",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"n":2,"frequency_penalty":0.5,"presence_penalty":0.3,"logprobs":true,"top_logprobs":5,"logit_bias":{}}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				for _, field := range []string{"n", "frequency_penalty", "presence_penalty", "logprobs", "top_logprobs", "logit_bias"} {
					if _, ok := doc[field]; ok {
						t.Errorf("output still contains OpenAI-only field %q", field)
					}
				}
			},
		},
		{
			name:  "non-system messages preserved unchanged",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hello"},{"role":"assistant","content":"Hi there"},{"role":"user","content":"Bye"}]}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				var msgs []anthropicOutboundMessage
				if err := json.Unmarshal(doc["messages"], &msgs); err != nil {
					t.Fatalf("unmarshal messages: %v", err)
				}
				if len(msgs) != 3 {
					t.Fatalf("len(messages) = %d, want 3", len(msgs))
				}
				wantRoles := []string{"user", "assistant", "user"}
				for i, m := range msgs {
					if m.Role != wantRoles[i] {
						t.Errorf("messages[%d].Role = %q, want %q", i, m.Role, wantRoles[i])
					}
				}
			},
		},
		{
			name:  "stream field preserved",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"stream":true}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				raw, ok := doc["stream"]
				if !ok {
					t.Fatal("output missing 'stream' field")
				}
				var stream bool
				if err := json.Unmarshal(raw, &stream); err != nil {
					t.Fatalf("unmarshal stream: %v", err)
				}
				if !stream {
					t.Error("stream = false, want true")
				}
			},
		},
		{
			name:    "invalid JSON returns error",
			input:   `not-json`,
			wantErr: true,
		},
		// ---- Tool definition translation ----------------------------------------
		{
			name:  "tools array translated from OpenAI to Anthropic format",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"What is the weather?"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"Get current weather","parameters":{"type":"object","properties":{"location":{"type":"string"}},"required":["location"]}}}]}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				rawTools, ok := doc["tools"]
				if !ok {
					t.Fatal("output missing 'tools' field")
				}
				var tools []anthropicToolDefinition
				if err := json.Unmarshal(rawTools, &tools); err != nil {
					t.Fatalf("unmarshal tools: %v", err)
				}
				if len(tools) != 1 {
					t.Fatalf("len(tools) = %d, want 1", len(tools))
				}
				if tools[0].Name != "get_weather" {
					t.Errorf("tools[0].Name = %q, want %q", tools[0].Name, "get_weather")
				}
				if tools[0].Description != "Get current weather" {
					t.Errorf("tools[0].Description = %q, want %q", tools[0].Description, "Get current weather")
				}
				// input_schema must be the parameters object.
				var schema map[string]json.RawMessage
				if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
					t.Fatalf("unmarshal input_schema: %v", err)
				}
				if _, ok := schema["properties"]; !ok {
					t.Error("input_schema missing 'properties' key")
				}
			},
		},
		{
			name:  "tool_choice auto translated to Anthropic auto object",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],"tool_choice":"auto"}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				raw, ok := doc["tool_choice"]
				if !ok {
					t.Fatal("output missing 'tool_choice' field")
				}
				var tc anthropicToolChoice
				if err := json.Unmarshal(raw, &tc); err != nil {
					t.Fatalf("unmarshal tool_choice: %v", err)
				}
				if tc.Type != "auto" {
					t.Errorf("tool_choice.type = %q, want %q", tc.Type, "auto")
				}
			},
		},
		{
			name:  "tool_choice required translated to Anthropic any",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],"tool_choice":"required"}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				raw, ok := doc["tool_choice"]
				if !ok {
					t.Fatal("output missing 'tool_choice' field")
				}
				var tc anthropicToolChoice
				if err := json.Unmarshal(raw, &tc); err != nil {
					t.Fatalf("unmarshal tool_choice: %v", err)
				}
				if tc.Type != "any" {
					t.Errorf("tool_choice.type = %q, want %q", tc.Type, "any")
				}
			},
		},
		{
			name:  "tool_choice none removes tools and tool_choice",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}],"tool_choice":"none"}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				if _, ok := doc["tool_choice"]; ok {
					t.Error("output still has 'tool_choice' after none")
				}
				if _, ok := doc["tools"]; ok {
					t.Error("output still has 'tools' after none")
				}
			},
		},
		{
			name:  "tool_choice object with function name translated to Anthropic tool type",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"tools":[{"type":"function","function":{"name":"get_weather","parameters":{}}}],"tool_choice":{"type":"function","function":{"name":"get_weather"}}}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				raw, ok := doc["tool_choice"]
				if !ok {
					t.Fatal("output missing 'tool_choice' field")
				}
				var tc anthropicToolChoice
				if err := json.Unmarshal(raw, &tc); err != nil {
					t.Fatalf("unmarshal tool_choice: %v", err)
				}
				if tc.Type != "tool" {
					t.Errorf("tool_choice.type = %q, want %q", tc.Type, "tool")
				}
				if tc.Name != "get_weather" {
					t.Errorf("tool_choice.name = %q, want %q", tc.Name, "get_weather")
				}
			},
		},
		// ---- Message history translation ----------------------------------------
		{
			name: "assistant message with tool_calls translated to Anthropic tool_use blocks",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"What is the weather?"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"London\"}"}}]}` +
				`]}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				var msgs []anthropicOutboundMessage
				if err := json.Unmarshal(doc["messages"], &msgs); err != nil {
					t.Fatalf("unmarshal messages: %v", err)
				}
				if len(msgs) != 2 {
					t.Fatalf("len(messages) = %d, want 2", len(msgs))
				}
				assistantMsg := msgs[1]
				if assistantMsg.Role != "assistant" {
					t.Errorf("role = %q, want %q", assistantMsg.Role, "assistant")
				}
				if len(assistantMsg.Content) != 1 {
					t.Fatalf("len(content) = %d, want 1 (tool_use block)", len(assistantMsg.Content))
				}
				block := assistantMsg.Content[0]
				if block.Type != "tool_use" {
					t.Errorf("content[0].type = %q, want %q", block.Type, "tool_use")
				}
				if block.ID != "call_abc" {
					t.Errorf("content[0].id = %q, want %q", block.ID, "call_abc")
				}
				if block.Name != "get_weather" {
					t.Errorf("content[0].name = %q, want %q", block.Name, "get_weather")
				}
				// Input must be the parsed JSON object, not the string.
				var input map[string]json.RawMessage
				if err := json.Unmarshal(block.Input, &input); err != nil {
					t.Fatalf("unmarshal input: %v", err)
				}
				var loc string
				if err := json.Unmarshal(input["location"], &loc); err != nil {
					t.Fatalf("unmarshal location: %v", err)
				}
				if loc != "London" {
					t.Errorf("input.location = %q, want %q", loc, "London")
				}
			},
		},
		{
			name: "assistant message with content and tool_calls includes text block first",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"Go"},` +
				`{"role":"assistant","content":"I will call a tool.","tool_calls":[{"id":"call_1","type":"function","function":{"name":"do_thing","arguments":"{}"}}]}` +
				`]}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				var msgs []anthropicOutboundMessage
				if err := json.Unmarshal(doc["messages"], &msgs); err != nil {
					t.Fatalf("unmarshal messages: %v", err)
				}
				if len(msgs) != 2 {
					t.Fatalf("len(messages) = %d, want 2", len(msgs))
				}
				content := msgs[1].Content
				if len(content) != 2 {
					t.Fatalf("len(content) = %d, want 2 (text + tool_use)", len(content))
				}
				if content[0].Type != "text" || content[0].Text != "I will call a tool." {
					t.Errorf("content[0] = {%s, %s}, want text block with text", content[0].Type, content[0].Text)
				}
				if content[1].Type != "tool_use" {
					t.Errorf("content[1].type = %q, want %q", content[1].Type, "tool_use")
				}
			},
		},
		{
			name: "role tool messages translated to Anthropic tool_result user turn",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"What is the weather?"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"get_weather","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"call_abc","content":"It is sunny, 22C"}` +
				`]}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				var msgs []anthropicOutboundMessage
				if err := json.Unmarshal(doc["messages"], &msgs); err != nil {
					t.Fatalf("unmarshal messages: %v", err)
				}
				// Expected: user, assistant(tool_use), user(tool_result)
				if len(msgs) != 3 {
					t.Fatalf("len(messages) = %d, want 3", len(msgs))
				}
				toolResultMsg := msgs[2]
				if toolResultMsg.Role != "user" {
					t.Errorf("tool result message role = %q, want %q", toolResultMsg.Role, "user")
				}
				if len(toolResultMsg.Content) != 1 {
					t.Fatalf("len(content) = %d, want 1 (tool_result block)", len(toolResultMsg.Content))
				}
				block := toolResultMsg.Content[0]
				if block.Type != "tool_result" {
					t.Errorf("content[0].type = %q, want %q", block.Type, "tool_result")
				}
				if block.ToolUseID != "call_abc" {
					t.Errorf("tool_use_id = %q, want %q", block.ToolUseID, "call_abc")
				}
				var gotContent string
				if err := json.Unmarshal(block.Content, &gotContent); err != nil {
					t.Fatalf("unmarshal tool_result content: %v", err)
				}
				if gotContent != "It is sunny, 22C" {
					t.Errorf("content = %q, want %q", gotContent, "It is sunny, 22C")
				}
			},
		},
		{
			name: "consecutive tool messages merged into single Anthropic user turn",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"Hi"},` +
				`{"role":"assistant","content":null,"tool_calls":[` +
				`{"id":"call_1","type":"function","function":{"name":"fn1","arguments":"{}"}},` +
				`{"id":"call_2","type":"function","function":{"name":"fn2","arguments":"{}"}}` +
				`]},` +
				`{"role":"tool","tool_call_id":"call_1","content":"result one"},` +
				`{"role":"tool","tool_call_id":"call_2","content":"result two"}` +
				`]}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				var msgs []anthropicOutboundMessage
				if err := json.Unmarshal(doc["messages"], &msgs); err != nil {
					t.Fatalf("unmarshal messages: %v", err)
				}
				// Expected: user, assistant(2 tool_use), user(2 tool_result)
				if len(msgs) != 3 {
					t.Fatalf("len(messages) = %d, want 3 (user + assistant + merged tool_results)", len(msgs))
				}
				toolResultMsg := msgs[2]
				if toolResultMsg.Role != "user" {
					t.Errorf("merged message role = %q, want %q", toolResultMsg.Role, "user")
				}
				if len(toolResultMsg.Content) != 2 {
					t.Fatalf("len(content) = %d, want 2 merged tool_result blocks", len(toolResultMsg.Content))
				}
				for i, block := range toolResultMsg.Content {
					if block.Type != "tool_result" {
						t.Errorf("content[%d].type = %q, want %q", i, block.Type, "tool_result")
					}
				}
				if toolResultMsg.Content[0].ToolUseID != "call_1" {
					t.Errorf("content[0].tool_use_id = %q, want %q", toolResultMsg.Content[0].ToolUseID, "call_1")
				}
				if toolResultMsg.Content[1].ToolUseID != "call_2" {
					t.Errorf("content[1].tool_use_id = %q, want %q", toolResultMsg.Content[1].ToolUseID, "call_2")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			out, err := a.TransformRequest([]byte(tc.input), Model{})

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("TransformRequest() error = %v", err)
			}

			var doc map[string]json.RawMessage
			if err := json.Unmarshal(out, &doc); err != nil {
				t.Fatalf("output is not valid JSON: %v", err)
			}

			tc.checkFn(t, doc)
		})
	}
}

// ---- TransformResponse ------------------------------------------------------

func TestAnthropicTransformResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// inputJSON is a raw Anthropic Messages API response body.
		inputJSON      string
		wantID         string
		wantObject     string
		wantContent    *string // nil means do not assert; pointer to "" checks null/empty
		wantToolCalls  int     // expected len(choices[0].message.tool_calls)
		wantFinish     string
		wantPrompt     int
		wantCompletion int
		wantTotal      int
		wantErr        bool
	}{
		{
			// FIX 2: id is synthesized by the proxy (chatcmpl-<timestamp>), not
			// forwarded from upstream. The upstream id "msg_01abc" must not appear
			// in the response; the synthesized id must start with "chatcmpl-".
			name:           "basic response maps fields to OpenAI format with synthesized id",
			inputJSON:      `{"id":"msg_01abc","type":"message","model":"claude-3-5-sonnet-20240620","content":[{"type":"text","text":"Hello there"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`,
			wantObject:     "chat.completion",
			wantContent:    strPtr("Hello there"),
			wantFinish:     "stop",
			wantPrompt:     10,
			wantCompletion: 5,
			wantTotal:      15,
		},
		{
			name:       "stop_reason end_turn maps to finish_reason stop",
			inputJSON:  `{"id":"msg_02","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			wantFinish: "stop",
		},
		{
			name:       "stop_reason max_tokens maps to finish_reason length",
			inputJSON:  `{"id":"msg_03","content":[{"type":"text","text":"truncated"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1}}`,
			wantFinish: "length",
		},
		{
			name:       "stop_reason stop_sequence maps to finish_reason stop",
			inputJSON:  `{"id":"msg_04","content":[{"type":"text","text":"ended"}],"stop_reason":"stop_sequence","usage":{"input_tokens":1,"output_tokens":1}}`,
			wantFinish: "stop",
		},
		{
			name:       "null stop_reason maps to finish_reason stop",
			inputJSON:  `{"id":"msg_05","content":[{"type":"text","text":"hi"}],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":1}}`,
			wantFinish: "stop",
		},
		{
			name:           "usage input_tokens and output_tokens mapped correctly",
			inputJSON:      `{"id":"msg_06","content":[{"type":"text","text":"x"}],"stop_reason":"end_turn","usage":{"input_tokens":42,"output_tokens":13}}`,
			wantPrompt:     42,
			wantCompletion: 13,
			wantTotal:      55,
		},
		{
			name:        "multiple text content blocks joined into single content string",
			inputJSON:   `{"id":"msg_07","content":[{"type":"text","text":"Hello"},{"type":"text","text":" world"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":2}}`,
			wantContent: strPtr("Hello world"),
		},
		{
			name:          "tool_use block translates to OpenAI tool_calls with null content",
			inputJSON:     `{"id":"msg_08","content":[{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{"location":"London","unit":"celsius"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":20}}`,
			wantToolCalls: 1,
			wantFinish:    "tool_calls",
		},
		{
			name:          "mixed text and tool_use: content present plus tool_calls",
			inputJSON:     `{"id":"msg_09","content":[{"type":"text","text":"Let me look that up."},{"type":"tool_use","id":"toolu_02","name":"search","input":{"query":"golang"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":10}}`,
			wantContent:   strPtr("Let me look that up."),
			wantToolCalls: 1,
			wantFinish:    "tool_calls",
		},
		{
			name:          "multiple tool_use blocks translate to multiple tool_calls",
			inputJSON:     `{"id":"msg_10","content":[{"type":"tool_use","id":"toolu_03","name":"fn_a","input":{"x":1}},{"type":"tool_use","id":"toolu_04","name":"fn_b","input":{"y":2}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":10}}`,
			wantToolCalls: 2,
			wantFinish:    "tool_calls",
		},
		{
			name:      "invalid JSON returns error",
			inputJSON: "not-json",
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			out, err := a.TransformResponse([]byte(tc.inputJSON))

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("TransformResponse() error = %v", err)
			}

			// Parse into a generic map to allow flexible assertions.
			var rawResp map[string]json.RawMessage
			if err := json.Unmarshal(out, &rawResp); err != nil {
				t.Fatalf("output is not valid JSON: %v", err)
			}

			// Parse the object and id fields.
			if rawObj, ok := rawResp["object"]; ok {
				var obj string
				_ = json.Unmarshal(rawObj, &obj)
				if obj != "chat.completion" {
					t.Errorf("object = %q, want %q", obj, "chat.completion")
				}
			}
			if tc.wantID != "" {
				var id string
				_ = json.Unmarshal(rawResp["id"], &id)
				if id != tc.wantID {
					t.Errorf("id = %q, want %q", id, tc.wantID)
				}
			}

			// Parse choices.
			var choices []json.RawMessage
			if err := json.Unmarshal(rawResp["choices"], &choices); err != nil {
				t.Fatalf("unmarshal choices: %v", err)
			}
			if len(choices) != 1 {
				t.Fatalf("len(choices) = %d, want 1", len(choices))
			}
			var choice struct {
				Index        int             `json:"index"`
				FinishReason string          `json:"finish_reason"`
				Message      json.RawMessage `json:"message"`
			}
			if err := json.Unmarshal(choices[0], &choice); err != nil {
				t.Fatalf("unmarshal choice: %v", err)
			}

			if tc.wantFinish != "" && choice.FinishReason != tc.wantFinish {
				t.Errorf("finish_reason = %q, want %q", choice.FinishReason, tc.wantFinish)
			}

			var msg struct {
				Role      string          `json:"role"`
				Content   json.RawMessage `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			}
			if err := json.Unmarshal(choice.Message, &msg); err != nil {
				t.Fatalf("unmarshal message: %v", err)
			}

			if tc.wantContent != nil {
				// Content may be null (JSON null) or a string.
				if string(msg.Content) == "null" {
					t.Errorf("message.content = null, want %q", *tc.wantContent)
				} else {
					var content string
					if err := json.Unmarshal(msg.Content, &content); err != nil {
						t.Fatalf("unmarshal message.content: %v", err)
					}
					if content != *tc.wantContent {
						t.Errorf("message.content = %q, want %q", content, *tc.wantContent)
					}
				}
			}

			if tc.wantToolCalls > 0 {
				if msg.ToolCalls == nil || string(msg.ToolCalls) == "null" {
					t.Fatalf("message.tool_calls is null/absent, want %d entries", tc.wantToolCalls)
				}
				var toolCalls []openAIToolCall
				if err := json.Unmarshal(msg.ToolCalls, &toolCalls); err != nil {
					t.Fatalf("unmarshal tool_calls: %v", err)
				}
				if len(toolCalls) != tc.wantToolCalls {
					t.Errorf("len(tool_calls) = %d, want %d", len(toolCalls), tc.wantToolCalls)
				}
				for i, tc2 := range toolCalls {
					if tc2.Type != "function" {
						t.Errorf("tool_calls[%d].type = %q, want %q", i, tc2.Type, "function")
					}
					if tc2.ID == "" {
						t.Errorf("tool_calls[%d].id is empty", i)
					}
					if tc2.Function.Name == "" {
						t.Errorf("tool_calls[%d].function.name is empty", i)
					}
					// Arguments must be a valid JSON string.
					if !json.Valid([]byte(tc2.Function.Arguments)) {
						t.Errorf("tool_calls[%d].function.arguments is not valid JSON: %q", i, tc2.Function.Arguments)
					}
				}
			}

			// Usage assertions.
			if tc.wantPrompt != 0 || tc.wantCompletion != 0 || tc.wantTotal != 0 {
				var usage openAIUsage
				if err := json.Unmarshal(rawResp["usage"], &usage); err != nil {
					t.Fatalf("unmarshal usage: %v", err)
				}
				if tc.wantPrompt != 0 && usage.PromptTokens != tc.wantPrompt {
					t.Errorf("usage.prompt_tokens = %d, want %d", usage.PromptTokens, tc.wantPrompt)
				}
				if tc.wantCompletion != 0 && usage.CompletionTokens != tc.wantCompletion {
					t.Errorf("usage.completion_tokens = %d, want %d", usage.CompletionTokens, tc.wantCompletion)
				}
				if tc.wantTotal != 0 && usage.TotalTokens != tc.wantTotal {
					t.Errorf("usage.total_tokens = %d, want %d", usage.TotalTokens, tc.wantTotal)
				}
			}
		})
	}
}

// strPtr is a helper to obtain a pointer to a string literal.
func strPtr(s string) *string { return &s }

// unmarshalDoc parses JSON bytes into a map[string]json.RawMessage.
func unmarshalDoc(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal doc: %v\ndata: %s", err, data)
	}
	return doc
}

// unmarshalMessages parses the "messages" field from a transformed request doc
// into a slice of anthropicOutboundMessage.
func unmarshalMessages(t *testing.T, doc map[string]json.RawMessage) []anthropicOutboundMessage {
	t.Helper()
	var msgs []anthropicOutboundMessage
	if err := json.Unmarshal(doc["messages"], &msgs); err != nil {
		t.Fatalf("unmarshal messages: %v", err)
	}
	return msgs
}

// transformRequest is a thin wrapper that calls TransformRequest and fatals on error.
func transformRequest(t *testing.T, input string) map[string]json.RawMessage {
	t.Helper()
	a := &AnthropicAdapter{}
	out, err := a.TransformRequest([]byte(input), Model{})
	if err != nil {
		t.Fatalf("TransformRequest(%q): %v", input, err)
	}
	return unmarshalDoc(t, out)
}

// runStream feeds each event line through a fresh AnthropicAdapter and returns
// the non-nil outputs and the adapter itself for further inspection.
func runStream(t *testing.T, a *AnthropicAdapter, events []string) [][]byte {
	t.Helper()
	var out [][]byte
	for _, ev := range events {
		if b := a.TransformStreamLine([]byte(ev)); b != nil {
			out = append(out, b)
		}
	}
	return out
}

// ── REQUEST: tool definitions ──────────────────────────────────────────────────

// TestAnthropicTransformRequest_ToolDefinition verifies that OpenAI-style tool
// definitions are translated field-by-field to Anthropic's input_schema shape,
// and that a tool missing parameters gets an empty-object schema.
func TestAnthropicTransformRequest_ToolDefinition(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		input       string
		wantTools   []anthropicToolDefinition // expected translated tools
		checkSchema func(t *testing.T, tools []anthropicToolDefinition)
	}{
		{
			name: "full parameters object becomes input_schema",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"weather","description":"Get weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}`,
			checkSchema: func(t *testing.T, tools []anthropicToolDefinition) {
				t.Helper()
				if len(tools) != 1 {
					t.Fatalf("len(tools) = %d, want 1", len(tools))
				}
				tool := tools[0]
				if tool.Name != "weather" {
					t.Errorf("name = %q, want weather", tool.Name)
				}
				if tool.Description != "Get weather" {
					t.Errorf("description = %q, want 'Get weather'", tool.Description)
				}
				// input_schema must be the parameters object verbatim.
				var schema map[string]json.RawMessage
				if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
					t.Fatalf("unmarshal input_schema: %v", err)
				}
				if _, ok := schema["properties"]; !ok {
					t.Error("input_schema missing 'properties'")
				}
				if _, ok := schema["required"]; !ok {
					t.Error("input_schema missing 'required'")
				}
				var typeVal string
				_ = json.Unmarshal(schema["type"], &typeVal)
				if typeVal != "object" {
					t.Errorf("input_schema.type = %q, want object", typeVal)
				}
			},
		},
		{
			name: "tool without parameters gets empty-object schema",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"noop","description":"Does nothing"}}]}`,
			checkSchema: func(t *testing.T, tools []anthropicToolDefinition) {
				t.Helper()
				if len(tools) != 1 {
					t.Fatalf("len(tools) = %d, want 1", len(tools))
				}
				// input_schema must be the fallback empty-object schema.
				var schema map[string]json.RawMessage
				if err := json.Unmarshal(tools[0].InputSchema, &schema); err != nil {
					t.Fatalf("unmarshal input_schema: %v", err)
				}
				var typeVal string
				_ = json.Unmarshal(schema["type"], &typeVal)
				if typeVal != "object" {
					t.Errorf("fallback schema type = %q, want object", typeVal)
				}
				if _, ok := schema["properties"]; !ok {
					t.Error("fallback schema missing 'properties'")
				}
			},
		},
		{
			name: "multiple tools translated in order",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[` +
				`{"type":"function","function":{"name":"fn_a","description":"A","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}}` +
				`,{"type":"function","function":{"name":"fn_b","description":"B","parameters":{"type":"object","properties":{"y":{"type":"number"}}}}}` +
				`]}`,
			checkSchema: func(t *testing.T, tools []anthropicToolDefinition) {
				t.Helper()
				if len(tools) != 2 {
					t.Fatalf("len(tools) = %d, want 2", len(tools))
				}
				if tools[0].Name != "fn_a" {
					t.Errorf("tools[0].name = %q, want fn_a", tools[0].Name)
				}
				if tools[1].Name != "fn_b" {
					t.Errorf("tools[1].name = %q, want fn_b", tools[1].Name)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			doc := transformRequest(t, tc.input)
			rawTools, ok := doc["tools"]
			if !ok {
				t.Fatal("output missing 'tools' field")
			}
			var tools []anthropicToolDefinition
			if err := json.Unmarshal(rawTools, &tools); err != nil {
				t.Fatalf("unmarshal tools: %v", err)
			}
			tc.checkSchema(t, tools)
		})
	}
}

// ── REQUEST: tool_choice translation ─────────────────────────────────────────

// TestAnthropicTransformRequest_ToolChoice covers all tool_choice variants
// including the named-function object form.
func TestAnthropicTransformRequest_ToolChoice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		toolChoiceRaw string // raw JSON value to embed as tool_choice
		wantType      string // expected Anthropic type
		wantName      string // expected Anthropic name (empty = not present)
		wantRemoved   bool   // tools and tool_choice should both be gone
	}{
		{
			name:          "string auto becomes type:auto",
			toolChoiceRaw: `"auto"`,
			wantType:      "auto",
		},
		{
			name:          "string required becomes type:any",
			toolChoiceRaw: `"required"`,
			wantType:      "any",
		},
		{
			name:          "object function with name becomes type:tool with name",
			toolChoiceRaw: `{"type":"function","function":{"name":"lookup"}}`,
			wantType:      "tool",
			wantName:      "lookup",
		},
		{
			name:          "string none removes both tools and tool_choice",
			toolChoiceRaw: `"none"`,
			wantRemoved:   true,
		},
	}

	baseTools := `[{"type":"function","function":{"name":"lookup","parameters":{}}}]`

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			input := fmt.Sprintf(
				`{"model":"claude-3","messages":[{"role":"user","content":"q"}],"tools":%s,"tool_choice":%s}`,
				baseTools, tc.toolChoiceRaw,
			)
			doc := transformRequest(t, input)

			if tc.wantRemoved {
				if _, ok := doc["tool_choice"]; ok {
					t.Error("tool_choice still present, expected removed for 'none'")
				}
				if _, ok := doc["tools"]; ok {
					t.Error("tools still present, expected removed for 'none'")
				}
				return
			}

			raw, ok := doc["tool_choice"]
			if !ok {
				t.Fatal("tool_choice missing from output")
			}
			var tc2 anthropicToolChoice
			if err := json.Unmarshal(raw, &tc2); err != nil {
				t.Fatalf("unmarshal tool_choice: %v", err)
			}
			if tc2.Type != tc.wantType {
				t.Errorf("tool_choice.type = %q, want %q", tc2.Type, tc.wantType)
			}
			if tc.wantName != "" && tc2.Name != tc.wantName {
				t.Errorf("tool_choice.name = %q, want %q", tc2.Name, tc.wantName)
			}
		})
	}
}

// ── REQUEST: assistant message with tool_calls ────────────────────────────────

// TestAnthropicTransformRequest_AssistantToolCalls covers the conversion of an
// OpenAI assistant message carrying tool_calls into Anthropic tool_use blocks.
// It covers: null content, non-empty text content, and multiple tool_calls.
func TestAnthropicTransformRequest_AssistantToolCalls(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		wantBlocks    int    // number of content blocks in the assistant message
		wantFirstType string // type of blocks[0]
		wantLastType  string // type of last block (tool_use)
		wantText      string // if non-empty, blocks[0].text must equal this
		wantToolID    string
		wantToolName  string
		wantInputKey  string // key expected in the input object
		wantInputVal  string // string value for wantInputKey
	}{
		{
			name: "null content produces single tool_use block",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"call it"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"id1","type":"function","function":{"name":"fn","arguments":"{\"k\":\"v\"}"}}]}` +
				`]}`,
			wantBlocks:    1,
			wantFirstType: "tool_use",
			wantLastType:  "tool_use",
			wantToolID:    "id1",
			wantToolName:  "fn",
			wantInputKey:  "k",
			wantInputVal:  "v",
		},
		{
			name: "non-empty content yields text block then tool_use block",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"go"},` +
				`{"role":"assistant","content":"Sure, calling tool.","tool_calls":[{"id":"id2","type":"function","function":{"name":"do","arguments":"{}"}}]}` +
				`]}`,
			wantBlocks:    2,
			wantFirstType: "text",
			wantLastType:  "tool_use",
			wantText:      "Sure, calling tool.",
			wantToolID:    "id2",
			wantToolName:  "do",
		},
		{
			name: "multiple tool_calls yield multiple tool_use blocks in order",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"go"},` +
				`{"role":"assistant","content":null,"tool_calls":[` +
				`{"id":"tc1","type":"function","function":{"name":"fn1","arguments":"{\"a\":1}"}},` +
				`{"id":"tc2","type":"function","function":{"name":"fn2","arguments":"{\"b\":2}"}},` +
				`{"id":"tc3","type":"function","function":{"name":"fn3","arguments":"{\"c\":3}"}}` +
				`]}` +
				`]}`,
			wantBlocks:    3,
			wantFirstType: "tool_use",
			wantLastType:  "tool_use",
			wantToolID:    "tc1",
			wantToolName:  "fn1",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			doc := transformRequest(t, tc.input)
			msgs := unmarshalMessages(t, doc)

			// Find the assistant message.
			var assistantMsg *anthropicOutboundMessage
			for i := range msgs {
				if msgs[i].Role == "assistant" {
					assistantMsg = &msgs[i]
					break
				}
			}
			if assistantMsg == nil {
				t.Fatal("no assistant message in output")
			}

			if len(assistantMsg.Content) != tc.wantBlocks {
				t.Fatalf("len(content) = %d, want %d; blocks: %+v", len(assistantMsg.Content), tc.wantBlocks, assistantMsg.Content)
			}

			if assistantMsg.Content[0].Type != tc.wantFirstType {
				t.Errorf("content[0].type = %q, want %q", assistantMsg.Content[0].Type, tc.wantFirstType)
			}
			last := assistantMsg.Content[len(assistantMsg.Content)-1]
			if last.Type != tc.wantLastType {
				t.Errorf("last block type = %q, want %q", last.Type, tc.wantLastType)
			}

			if tc.wantText != "" {
				if assistantMsg.Content[0].Text != tc.wantText {
					t.Errorf("content[0].text = %q, want %q", assistantMsg.Content[0].Text, tc.wantText)
				}
			}

			// Validate the first tool_use block.
			toolBlock := assistantMsg.Content[len(assistantMsg.Content)-tc.wantBlocks+tc.wantBlocks-1]
			if tc.wantFirstType == "tool_use" {
				toolBlock = assistantMsg.Content[0]
			} else {
				toolBlock = assistantMsg.Content[1]
			}
			if toolBlock.ID != tc.wantToolID {
				t.Errorf("tool_use id = %q, want %q", toolBlock.ID, tc.wantToolID)
			}
			if toolBlock.Name != tc.wantToolName {
				t.Errorf("tool_use name = %q, want %q", toolBlock.Name, tc.wantToolName)
			}

			// If a key/value was specified, verify input is a parsed object.
			if tc.wantInputKey != "" {
				var input map[string]json.RawMessage
				if err := json.Unmarshal(toolBlock.Input, &input); err != nil {
					t.Fatalf("unmarshal tool_use input: %v", err)
				}
				rawVal, ok := input[tc.wantInputKey]
				if !ok {
					t.Fatalf("input missing key %q", tc.wantInputKey)
				}
				var strVal string
				if err := json.Unmarshal(rawVal, &strVal); err != nil {
					// Maybe it's a number.
					if tc.wantInputVal != "" {
						t.Errorf("input[%q] = %s, could not unmarshal as string: %v", tc.wantInputKey, rawVal, err)
					}
				} else if tc.wantInputVal != "" && strVal != tc.wantInputVal {
					t.Errorf("input[%q] = %q, want %q", tc.wantInputKey, strVal, tc.wantInputVal)
				}
			}
		})
	}
}

// ── REQUEST: role:tool messages ───────────────────────────────────────────────

// TestAnthropicTransformRequest_ToolResultMessages covers single and consecutive
// tool-result messages being merged into Anthropic tool_result content blocks.
func TestAnthropicTransformRequest_ToolResultMessages(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		input            string
		wantUserMsgCount int // how many user-role messages in output
		wantResultBlocks int // how many tool_result blocks in the last user turn
		wantIDs          []string
		wantContents     []string
	}{
		{
			name: "single tool message becomes one tool_result block",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"q"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"fn","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"c1","content":"result here"}` +
				`]}`,
			wantUserMsgCount: 1, // original user turn + merged tool_result = the merged one is the 3rd msg
			wantResultBlocks: 1,
			wantIDs:          []string{"c1"},
			wantContents:     []string{"result here"},
		},
		{
			name: "two consecutive tool messages merge into one user turn",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"q"},` +
				`{"role":"assistant","content":null,"tool_calls":[` +
				`{"id":"c1","type":"function","function":{"name":"fn1","arguments":"{}"}},` +
				`{"id":"c2","type":"function","function":{"name":"fn2","arguments":"{}"}}` +
				`]},` +
				`{"role":"tool","tool_call_id":"c1","content":"first result"},` +
				`{"role":"tool","tool_call_id":"c2","content":"second result"}` +
				`]}`,
			wantUserMsgCount: 1,
			wantResultBlocks: 2,
			wantIDs:          []string{"c1", "c2"},
			wantContents:     []string{"first result", "second result"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			doc := transformRequest(t, tc.input)
			msgs := unmarshalMessages(t, doc)

			// The last message should be the merged user turn with tool_result blocks.
			last := msgs[len(msgs)-1]
			if last.Role != "user" {
				t.Errorf("last message role = %q, want user", last.Role)
			}
			if len(last.Content) != tc.wantResultBlocks {
				t.Fatalf("last user turn content blocks = %d, want %d; blocks: %+v",
					len(last.Content), tc.wantResultBlocks, last.Content)
			}

			for i, block := range last.Content {
				if block.Type != "tool_result" {
					t.Errorf("content[%d].type = %q, want tool_result", i, block.Type)
				}
				if i < len(tc.wantIDs) && block.ToolUseID != tc.wantIDs[i] {
					t.Errorf("content[%d].tool_use_id = %q, want %q", i, block.ToolUseID, tc.wantIDs[i])
				}
				if i < len(tc.wantContents) {
					var gotContent string
					if err := json.Unmarshal(block.Content, &gotContent); err != nil {
						t.Fatalf("content[%d]: unmarshal tool_result content: %v", i, err)
					}
					if gotContent != tc.wantContents[i] {
						t.Errorf("content[%d].content = %q, want %q", i, gotContent, tc.wantContents[i])
					}
				}
			}
		})
	}
}

// ── REQUEST: role:tool with array-of-parts content (FIX 1) ──────────────────

// TestAnthropicTransformRequest_ToolResultArrayContent verifies that a role:tool
// message whose content is an array of {type,text} parts is translated to an
// Anthropic tool_result block whose content is an ARRAY of Anthropic text blocks,
// one per OpenAI text part — with no concatenation (FIX 1 zero-knowledge
// preservation). For a plain string, content is a JSON string (unchanged).
func TestAnthropicTransformRequest_ToolResultArrayContent(t *testing.T) {
	t.Parallel()

	t.Run("plain string content emits JSON string in tool_result.content", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"user","content":"q"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c4","type":"function","function":{"name":"fn","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"c4","content":"plain string result"}` +
			`]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)
		last := msgs[len(msgs)-1]
		if last.Role != "user" {
			t.Fatalf("last role = %q, want user", last.Role)
		}
		if len(last.Content) != 1 {
			t.Fatalf("len(content) = %d, want 1", len(last.Content))
		}
		block := last.Content[0]
		if block.Type != "tool_result" {
			t.Errorf("type = %q, want tool_result", block.Type)
		}
		// For a plain string, Content is a JSON string.
		var got string
		if err := json.Unmarshal(block.Content, &got); err != nil {
			t.Fatalf("unmarshal content as string: %v", err)
		}
		if got != "plain string result" {
			t.Errorf("content string = %q, want %q", got, "plain string result")
		}
	})

	t.Run("array with single text part emits array of one text block", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"user","content":"q"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"fn","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"c1","content":[{"type":"text","text":"tool output here"}]}` +
			`]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)
		last := msgs[len(msgs)-1]
		block := last.Content[0]
		if block.Type != "tool_result" {
			t.Errorf("type = %q, want tool_result", block.Type)
		}
		// Content must be an ARRAY, not a string.
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(block.Content, &blocks); err != nil {
			t.Fatalf("unmarshal content as array: %v (raw: %s)", err, block.Content)
		}
		if len(blocks) != 1 {
			t.Fatalf("len(content array) = %d, want 1", len(blocks))
		}
		if blocks[0].Type != "text" {
			t.Errorf("blocks[0].type = %q, want text", blocks[0].Type)
		}
		if blocks[0].Text != "tool output here" {
			t.Errorf("blocks[0].text = %q, want %q", blocks[0].Text, "tool output here")
		}
	})

	// FIX 1 critical: multiple text parts must remain SEPARATE blocks — never joined.
	// Joining would reconstruct PII that was deliberately split across parts
	// (e.g. "alice@" + "example.com" → "alice@example.com").
	t.Run("array with multiple text parts emits separate text blocks — parts never joined", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"user","content":"q"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c2","type":"function","function":{"name":"fn","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"c2","content":[{"type":"text","text":"alice@"},{"type":"text","text":"example.com"}]}` +
			`]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)
		last := msgs[len(msgs)-1]
		block := last.Content[0]
		if block.Type != "tool_result" {
			t.Errorf("type = %q, want tool_result", block.Type)
		}
		// Must be an array with exactly two blocks — not the joined string.
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(block.Content, &blocks); err != nil {
			t.Fatalf("unmarshal content as array: %v (raw: %s)", err, block.Content)
		}
		if len(blocks) != 2 {
			t.Fatalf("len(content array) = %d, want 2 (parts must remain separate)", len(blocks))
		}
		if blocks[0].Text != "alice@" {
			t.Errorf("blocks[0].text = %q, want %q", blocks[0].Text, "alice@")
		}
		if blocks[1].Text != "example.com" {
			t.Errorf("blocks[1].text = %q, want %q", blocks[1].Text, "example.com")
		}
		// The joined form must never appear as any single value.
		rawContent := string(block.Content)
		if strings.Contains(rawContent, "alice@example.com") {
			t.Errorf("SECURITY: joined PII string %q appears in tool_result content; parts must stay separate; raw: %s",
				"alice@example.com", rawContent)
		}
	})

	t.Run("array with only non-text parts emits empty array", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[` +
			`{"role":"user","content":"q"},` +
			`{"role":"assistant","content":null,"tool_calls":[{"id":"c3","type":"function","function":{"name":"fn","arguments":"{}"}}]},` +
			`{"role":"tool","tool_call_id":"c3","content":[{"type":"image_url","text":""}]}` +
			`]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)
		last := msgs[len(msgs)-1]
		block := last.Content[0]
		if block.Type != "tool_result" {
			t.Errorf("type = %q, want tool_result", block.Type)
		}
		// Must be literally "[]", not "null" — a nil Go slice marshals to null,
		// but we want an explicit empty array for consistency with Gemini adapter.
		if string(block.Content) == "null" {
			t.Fatalf("tool_result.content is null; want [] (use make([]T, 0) not var []T)")
		}
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(block.Content, &blocks); err != nil {
			t.Fatalf("unmarshal content as array: %v (raw: %s)", err, block.Content)
		}
		if len(blocks) != 0 {
			t.Errorf("len(content array) = %d, want 0 (non-text parts skipped)", len(blocks))
		}
	})
}

// ── REQUEST: parseArgumentsToObject edge cases (FIX 3) ───────────────────────

// TestAnthropicTransformRequest_ArgumentsEdgeCases verifies the argument
// parsing rules: empty/whitespace → {}, valid object → used as-is, valid
// non-object JSON → error, invalid JSON → error.
func TestAnthropicTransformRequest_ArgumentsEdgeCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		input     string
		wantErr   bool
		wantInput string // expected Anthropic input JSON string when no error
	}{
		{
			name: "empty arguments string becomes empty object",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"q"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"tc1","type":"function","function":{"name":"fn","arguments":""}}]}` +
				`]}`,
			wantInput: `{}`,
		},
		{
			name: "whitespace-only arguments becomes empty object",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"q"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"tc2","type":"function","function":{"name":"fn","arguments":"   "}}]}` +
				`]}`,
			wantInput: `{}`,
		},
		{
			name: "valid object arguments passed through",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"q"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"tc3","type":"function","function":{"name":"fn","arguments":"{\"k\":\"v\"}"}}]}` +
				`]}`,
			wantInput: `{"k":"v"}`,
		},
		{
			name: "arguments is a JSON number (not object) returns error",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"q"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"tc4","type":"function","function":{"name":"fn","arguments":"42"}}]}` +
				`]}`,
			wantErr: true,
		},
		{
			name: "arguments is a JSON array (not object) returns error",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"q"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"tc5","type":"function","function":{"name":"fn","arguments":"[1,2]"}}]}` +
				`]}`,
			wantErr: true,
		},
		{
			name: "arguments is invalid JSON returns error",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"q"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"tc6","type":"function","function":{"name":"fn","arguments":"not-json"}}]}` +
				`]}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			out, err := a.TransformRequest([]byte(tc.input), Model{})

			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("TransformRequest() unexpected error: %v", err)
			}

			doc := unmarshalDoc(t, out)
			msgs := unmarshalMessages(t, doc)

			// Find the assistant message's tool_use block.
			var assistantMsg *anthropicOutboundMessage
			for i := range msgs {
				if msgs[i].Role == "assistant" {
					assistantMsg = &msgs[i]
					break
				}
			}
			if assistantMsg == nil {
				t.Fatal("no assistant message in output")
			}
			if len(assistantMsg.Content) == 0 {
				t.Fatal("assistant message has no content blocks")
			}
			block := assistantMsg.Content[len(assistantMsg.Content)-1]
			if block.Type != "tool_use" {
				t.Fatalf("last block type = %q, want tool_use", block.Type)
			}
			if string(block.Input) != tc.wantInput {
				t.Errorf("tool_use.input = %s, want %s", block.Input, tc.wantInput)
			}
		})
	}
}

// ── REQUEST: full round-trip conversation ────────────────────────────────────

// TestAnthropicTransformRequest_FullRound verifies that a complete multi-turn
// conversation (system + user + assistant with tool_calls + tool result + user)
// is translated correctly: system extracted to top-level, messages in the right
// order with correct roles and blocks, and tool_use_id matches tool_call_id.
func TestAnthropicTransformRequest_FullRound(t *testing.T) {
	t.Parallel()

	input := `{"model":"claude-3","messages":[` +
		`{"role":"system","content":"You are a helpful assistant."},` +
		`{"role":"user","content":"What is the weather in Paris?"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"call_paris","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Paris\"}"}}]},` +
		`{"role":"tool","tool_call_id":"call_paris","content":"It is 18C and sunny."},` +
		`{"role":"user","content":"Thanks!"}` +
		`]}`

	doc := transformRequest(t, input)

	// System extracted.
	rawSystem, ok := doc["system"]
	if !ok {
		t.Fatal("missing top-level 'system' field")
	}
	var systemText string
	if err := json.Unmarshal(rawSystem, &systemText); err != nil {
		t.Fatalf("unmarshal system: %v", err)
	}
	if systemText != "You are a helpful assistant." {
		t.Errorf("system = %q, want exact string", systemText)
	}

	msgs := unmarshalMessages(t, doc)

	// Expected: user, assistant(tool_use), user(tool_result), user
	// The system message is removed; original user + tool_result user + final user = 4 messages total.
	if len(msgs) != 4 {
		t.Fatalf("len(messages) = %d, want 4; messages: %+v", len(msgs), msgs)
	}

	// msgs[0]: user
	if msgs[0].Role != "user" {
		t.Errorf("msgs[0].role = %q, want user", msgs[0].Role)
	}

	// msgs[1]: assistant with tool_use block
	if msgs[1].Role != "assistant" {
		t.Errorf("msgs[1].role = %q, want assistant", msgs[1].Role)
	}
	if len(msgs[1].Content) != 1 || msgs[1].Content[0].Type != "tool_use" {
		t.Fatalf("msgs[1] content: %+v; want single tool_use block", msgs[1].Content)
	}
	toolUseBlock := msgs[1].Content[0]
	if toolUseBlock.ID != "call_paris" {
		t.Errorf("tool_use.id = %q, want call_paris", toolUseBlock.ID)
	}
	if toolUseBlock.Name != "get_weather" {
		t.Errorf("tool_use.name = %q, want get_weather", toolUseBlock.Name)
	}
	var toolInput map[string]json.RawMessage
	if err := json.Unmarshal(toolUseBlock.Input, &toolInput); err != nil {
		t.Fatalf("unmarshal tool_use input: %v", err)
	}
	var city string
	if err := json.Unmarshal(toolInput["city"], &city); err != nil {
		t.Fatalf("unmarshal city: %v", err)
	}
	if city != "Paris" {
		t.Errorf("input.city = %q, want Paris", city)
	}

	// msgs[2]: user with tool_result — tool_use_id must match the tool_call_id.
	if msgs[2].Role != "user" {
		t.Errorf("msgs[2].role = %q, want user", msgs[2].Role)
	}
	if len(msgs[2].Content) != 1 || msgs[2].Content[0].Type != "tool_result" {
		t.Fatalf("msgs[2] content: %+v; want single tool_result block", msgs[2].Content)
	}
	toolResultBlock := msgs[2].Content[0]
	if toolResultBlock.ToolUseID != "call_paris" {
		t.Errorf("tool_result.tool_use_id = %q, want call_paris (must match tool_call_id)", toolResultBlock.ToolUseID)
	}
	var gotToolResultContent string
	if err := json.Unmarshal(toolResultBlock.Content, &gotToolResultContent); err != nil {
		t.Fatalf("unmarshal tool_result.content: %v", err)
	}
	if gotToolResultContent != "It is 18C and sunny." {
		t.Errorf("tool_result.content = %q, want 'It is 18C and sunny.'", gotToolResultContent)
	}

	// msgs[3]: final user message.
	if msgs[3].Role != "user" {
		t.Errorf("msgs[3].role = %q, want user", msgs[3].Role)
	}
}

// ── REQUEST: no-regression plain text ────────────────────────────────────────

// TestAnthropicTransformRequest_PlainTextNoRegression ensures that plain
// text-only requests (no tools) are still transformed correctly: system
// extraction, max_tokens default, and OpenAI-only field stripping all work.
func TestAnthropicTransformRequest_PlainTextNoRegression(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		checkFn func(t *testing.T, doc map[string]json.RawMessage)
	}{
		{
			name:  "system extraction still works without tools",
			input: `{"model":"claude-3","messages":[{"role":"system","content":"Be concise."},{"role":"user","content":"Hi"}]}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				if _, ok := doc["system"]; !ok {
					t.Error("system field missing")
				}
				msgs := unmarshalMessages(t, doc)
				for _, m := range msgs {
					if m.Role == "system" {
						t.Error("system role still in messages array")
					}
				}
			},
		},
		{
			name:  "max_tokens defaults to 4096 when absent",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}]}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				var maxTok int
				if err := json.Unmarshal(doc["max_tokens"], &maxTok); err != nil {
					t.Fatalf("unmarshal max_tokens: %v", err)
				}
				if maxTok != 4096 {
					t.Errorf("max_tokens = %d, want 4096", maxTok)
				}
			},
		},
		{
			name:  "openAIOnlyFields stripped even without tools",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"n":2,"response_format":{"type":"json_object"},"seed":42,"user":"alice","store":true}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				for _, field := range []string{"n", "response_format", "seed", "user", "store"} {
					if _, ok := doc[field]; ok {
						t.Errorf("OpenAI-only field %q still present", field)
					}
				}
			},
		},
		{
			name:  "max_completion_tokens alias promoted to max_tokens",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"Hi"}],"max_completion_tokens":512}`,
			checkFn: func(t *testing.T, doc map[string]json.RawMessage) {
				t.Helper()
				var maxTok int
				if err := json.Unmarshal(doc["max_tokens"], &maxTok); err != nil {
					t.Fatalf("unmarshal max_tokens: %v", err)
				}
				if maxTok != 512 {
					t.Errorf("max_tokens = %d, want 512 (from max_completion_tokens)", maxTok)
				}
				if _, ok := doc["max_completion_tokens"]; ok {
					t.Error("max_completion_tokens still present after promotion")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			doc := transformRequest(t, tc.input)
			tc.checkFn(t, doc)
		})
	}
}

// ── RESPONSE: tool_use blocks ─────────────────────────────────────────────────

// TestAnthropicTransformResponse_ToolUse covers detailed assertions on
// tool_use → tool_calls translation: content null, arguments re-serialization,
// multiple blocks, and mixed text+tool.
func TestAnthropicTransformResponse_ToolUse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		inputJSON       string
		wantContent     *string  // nil = content must be JSON null
		wantNullContent bool     // true = content field must be null
		wantToolIDs     []string // expected tool_calls IDs in order
		wantToolNames   []string
		wantArgKeys     []string // first key expected in each tool_call arguments JSON
		wantFinish      string
	}{
		{
			name: "tool_use only: content null, tool_calls present",
			inputJSON: `{"id":"msg_tc1","type":"message","model":"claude-3","content":[` +
				`{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{"location":"London","unit":"celsius"}}` +
				`],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":20}}`,
			wantNullContent: true,
			wantToolIDs:     []string{"toolu_01"},
			wantToolNames:   []string{"get_weather"},
			wantArgKeys:     []string{"location"},
			wantFinish:      "tool_calls",
		},
		{
			name: "mixed text and tool_use: content non-null, tool_calls present",
			inputJSON: `{"id":"msg_tc2","type":"message","model":"claude-3","content":[` +
				`{"type":"text","text":"Let me look that up."},` +
				`{"type":"tool_use","id":"toolu_02","name":"search","input":{"query":"golang testing"}}` +
				`],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":15}}`,
			wantContent:   strPtr("Let me look that up."),
			wantToolIDs:   []string{"toolu_02"},
			wantToolNames: []string{"search"},
			wantArgKeys:   []string{"query"},
			wantFinish:    "tool_calls",
		},
		{
			name: "multiple tool_use blocks preserve order and ids",
			inputJSON: `{"id":"msg_tc3","type":"message","model":"claude-3","content":[` +
				`{"type":"tool_use","id":"toolu_A","name":"fn_a","input":{"x":1}},` +
				`{"type":"tool_use","id":"toolu_B","name":"fn_b","input":{"y":2}},` +
				`{"type":"tool_use","id":"toolu_C","name":"fn_c","input":{"z":3}}` +
				`],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":10}}`,
			wantNullContent: true,
			wantToolIDs:     []string{"toolu_A", "toolu_B", "toolu_C"},
			wantToolNames:   []string{"fn_a", "fn_b", "fn_c"},
			wantArgKeys:     []string{"x", "y", "z"},
			wantFinish:      "tool_calls",
		},
		{
			name: "text-only response: content non-null, no tool_calls, finish_reason stop",
			inputJSON: `{"id":"msg_text","type":"message","model":"claude-3","content":[` +
				`{"type":"text","text":"Just a normal reply."}` +
				`],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":5}}`,
			wantContent: strPtr("Just a normal reply."),
			wantFinish:  "stop",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			out, err := a.TransformResponse([]byte(tc.inputJSON))
			if err != nil {
				t.Fatalf("TransformResponse: %v", err)
			}

			var resp openAIResponse
			if err := json.Unmarshal(out, &resp); err != nil {
				t.Fatalf("unmarshal response: %v", err)
			}
			if len(resp.Choices) != 1 {
				t.Fatalf("len(choices) = %d, want 1", len(resp.Choices))
			}
			choice := resp.Choices[0]
			msg := choice.Message

			// Finish reason.
			if tc.wantFinish != "" && choice.FinishReason != tc.wantFinish {
				t.Errorf("finish_reason = %q, want %q", choice.FinishReason, tc.wantFinish)
			}

			// Content assertion.
			if tc.wantNullContent {
				if msg.Content != nil {
					t.Errorf("content = %q, want null (nil pointer)", *msg.Content)
				}
			} else if tc.wantContent != nil {
				if msg.Content == nil {
					t.Fatalf("content is null, want %q", *tc.wantContent)
				}
				if *msg.Content != *tc.wantContent {
					t.Errorf("content = %q, want %q", *msg.Content, *tc.wantContent)
				}
			}

			// Tool calls assertions.
			if len(tc.wantToolIDs) == 0 {
				if len(msg.ToolCalls) != 0 {
					t.Errorf("tool_calls = %v, want empty/absent", msg.ToolCalls)
				}
				return
			}

			if len(msg.ToolCalls) != len(tc.wantToolIDs) {
				t.Fatalf("len(tool_calls) = %d, want %d", len(msg.ToolCalls), len(tc.wantToolIDs))
			}

			for i, tc2 := range msg.ToolCalls {
				if tc2.Type != "function" {
					t.Errorf("tool_calls[%d].type = %q, want function", i, tc2.Type)
				}
				if tc2.ID != tc.wantToolIDs[i] {
					t.Errorf("tool_calls[%d].id = %q, want %q", i, tc2.ID, tc.wantToolIDs[i])
				}
				if tc2.Function.Name != tc.wantToolNames[i] {
					t.Errorf("tool_calls[%d].function.name = %q, want %q", i, tc2.Function.Name, tc.wantToolNames[i])
				}
				// Arguments must be valid JSON.
				if !json.Valid([]byte(tc2.Function.Arguments)) {
					t.Errorf("tool_calls[%d].function.arguments is not valid JSON: %q", i, tc2.Function.Arguments)
				}
				// Check that the expected key is present in the arguments object.
				if i < len(tc.wantArgKeys) && tc.wantArgKeys[i] != "" {
					var args map[string]json.RawMessage
					if err := json.Unmarshal([]byte(tc2.Function.Arguments), &args); err != nil {
						t.Fatalf("tool_calls[%d] arguments unmarshal: %v", i, err)
					}
					if _, ok := args[tc.wantArgKeys[i]]; !ok {
						t.Errorf("tool_calls[%d].arguments missing key %q; arguments: %s", i, tc.wantArgKeys[i], tc2.Function.Arguments)
					}
				}
			}
		})
	}
}

// ── STREAMING: content_block_start tool_use header delta ─────────────────────

// TestAnthropicStream_ToolUseHeaderDelta verifies that a content_block_start
// with type=="tool_use" produces a Stage-0c-conformant header delta containing
// index, id, type:"function", function.name, and function.arguments:"".
func TestAnthropicStream_ToolUseHeaderDelta(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}
	// Prime the adapter with a message_start to set the ID.
	_ = a.TransformStreamLine([]byte(`data: {"type":"message_start","message":{"id":"msg_hdr","usage":{"input_tokens":5}}}`))

	line := `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_hdr","name":"get_data","input":{}}}`
	out := a.TransformStreamLine([]byte(line))
	if out == nil {
		t.Fatal("content_block_start(tool_use) returned nil, want header delta")
	}

	chunk := parseChunk(t, out)
	if chunk.ID != "msg_hdr" {
		t.Errorf("chunk.id = %q, want msg_hdr", chunk.ID)
	}
	if chunk.Object != "chat.completion.chunk" {
		t.Errorf("chunk.object = %q, want chat.completion.chunk", chunk.Object)
	}
	if len(chunk.Choices) != 1 {
		t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
	}
	delta := chunk.Choices[0].Delta
	if len(delta.ToolCalls) != 1 {
		t.Fatalf("len(delta.tool_calls) = %d, want 1", len(delta.ToolCalls))
	}
	tc := delta.ToolCalls[0]

	// Stage-0c conformance: index, id, type, name, and empty arguments must all be present.
	if tc.Index == nil || *tc.Index != 0 {
		t.Errorf("index = %v, want 0", tc.Index)
	}
	if tc.ID != "toolu_hdr" {
		t.Errorf("id = %q, want toolu_hdr", tc.ID)
	}
	if tc.Type != "function" {
		t.Errorf("type = %q, want function", tc.Type)
	}
	if tc.Function.Name != "get_data" {
		t.Errorf("function.name = %q, want get_data", tc.Function.Name)
	}
	if tc.Function.Arguments != "" {
		t.Errorf("function.arguments = %q, want empty string on header delta", tc.Function.Arguments)
	}
}

// TestAnthropicStream_ToolUseHeaderDelta_WireFormat verifies that the JSON wire
// representation of the header delta includes "arguments":"" explicitly — not
// omitted — so that OpenAI SDK clients that check for the key presence on the
// first tool_call delta are satisfied (FIX 1).
func TestAnthropicStream_ToolUseHeaderDelta_WireFormat(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}
	_ = a.TransformStreamLine([]byte(`data: {"type":"message_start","message":{"id":"msg_wire","usage":{"input_tokens":2}}}`))

	line := `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_wire","name":"my_fn","input":{}}}`
	out := a.TransformStreamLine([]byte(line))
	if out == nil {
		t.Fatal("content_block_start(tool_use) returned nil, want header delta")
	}

	// Strip the "data: " prefix to get the raw JSON.
	const prefix = "data: "
	if !strings.HasPrefix(string(out), prefix) {
		t.Fatalf("output does not start with %q: %s", prefix, out)
	}
	rawJSON := string(out[len(prefix):])

	// The JSON must contain "arguments":"" (key present, value empty string).
	// This is the wire-format assertion: omitempty would have dropped the key.
	if !strings.Contains(rawJSON, `"arguments":""`) {
		t.Errorf("header delta JSON missing explicit \"arguments\":\"\"; got: %s", rawJSON)
	}

	// The name must be present on the header delta.
	if !strings.Contains(rawJSON, `"name":"my_fn"`) {
		t.Errorf("header delta JSON missing \"name\":\"my_fn\"; got: %s", rawJSON)
	}
}

// TestAnthropicStream_InputJsonDeltaFragment_WireFormat verifies that the JSON
// wire representation of an argument-fragment delta omits "name" (omitempty
// keeps absent names absent) but includes "arguments" with the fragment value.
func TestAnthropicStream_InputJsonDeltaFragment_WireFormat(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}
	_ = a.TransformStreamLine([]byte(`data: {"type":"message_start","message":{"id":"msg_fwire","usage":{"input_tokens":2}}}`))
	_ = a.TransformStreamLine([]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_fwire","name":"do_thing","input":{}}}`))

	fragLine := `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`
	out := a.TransformStreamLine([]byte(fragLine))
	if out == nil {
		t.Fatal("input_json_delta returned nil, want fragment delta")
	}

	const prefix = "data: "
	rawJSON := string(out[len(prefix):])

	// Fragment delta must include arguments with the partial JSON value.
	if !strings.Contains(rawJSON, `"arguments":"{\"x\":1}"`) {
		t.Errorf("fragment delta JSON missing arguments fragment; got: %s", rawJSON)
	}

	// Fragment delta must NOT include name (omitempty removes it when empty).
	if strings.Contains(rawJSON, `"name"`) {
		t.Errorf("fragment delta JSON unexpectedly contains \"name\" field; got: %s", rawJSON)
	}
}

// TestAnthropicStream_InputJsonDeltaFragment verifies that an input_json_delta
// event produces an arguments-fragment delta with only index and
// function.arguments — no id, type, or name (Stage-0c requirement).
func TestAnthropicStream_InputJsonDeltaFragment(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}
	// Register block 0 → tool-call 0 by sending a content_block_start first.
	_ = a.TransformStreamLine([]byte(`data: {"type":"message_start","message":{"id":"msg_frag","usage":{"input_tokens":3}}}`))
	_ = a.TransformStreamLine([]byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_frag","name":"fn","input":{}}}`))

	fragLine := `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`
	out := a.TransformStreamLine([]byte(fragLine))
	if out == nil {
		t.Fatal("input_json_delta returned nil, want fragment delta")
	}

	chunk := parseChunk(t, out)
	if len(chunk.Choices) != 1 || len(chunk.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("unexpected chunk structure: %+v", chunk)
	}
	tc := chunk.Choices[0].Delta.ToolCalls[0]

	// Index must be present.
	if tc.Index == nil || *tc.Index != 0 {
		t.Errorf("index = %v, want 0", tc.Index)
	}
	// Arguments fragment must carry the partial JSON.
	if tc.Function.Arguments != `{"x":1}` {
		t.Errorf("function.arguments = %q, want {\"x\":1}", tc.Function.Arguments)
	}
	// Stage-0c: no id/type/name on argument fragments.
	if tc.ID != "" {
		t.Errorf("id = %q on fragment delta, want empty", tc.ID)
	}
	if tc.Type != "" {
		t.Errorf("type = %q on fragment delta, want empty", tc.Type)
	}
	if tc.Function.Name != "" {
		t.Errorf("function.name = %q on fragment delta, want empty", tc.Function.Name)
	}
}

// ── STREAMING: content-block-index → tool-call-index mapping ─────────────────

// TestAnthropicStream_TextBlockThenToolUse_IndexMapping verifies that when a
// text block (content index 0) precedes a tool_use block (content index 1), the
// tool_use gets tool-call index 0, not 1. Text blocks are invisible to the
// tool-call counter.
func TestAnthropicStream_TextBlockThenToolUse_IndexMapping(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}
	events := []string{
		`data: {"type":"message_start","message":{"id":"msg_idx","usage":{"input_tokens":3}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Thinking..."}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_idx","name":"calculate","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"n\":42}"}}`,
	}

	type result struct {
		out  []byte
		drop bool
	}
	var results []result
	for _, ev := range events {
		out := a.TransformStreamLine([]byte(ev))
		results = append(results, result{out: out, drop: out == nil})
	}

	// [0] message_start → role chunk (not nil).
	if results[0].drop {
		t.Error("message_start: got nil")
	}

	// [1] text content_block_start → nil (dropped).
	if !results[1].drop {
		t.Errorf("text content_block_start: got %q, want nil", results[1].out)
	}

	// [2] text_delta → text content chunk.
	if results[2].drop {
		t.Error("text_delta: got nil, want text chunk")
	}
	textChunk := parseChunk(t, results[2].out)
	if textChunk.Choices[0].Delta.Content != "Thinking..." {
		t.Errorf("text delta content = %q, want Thinking...", textChunk.Choices[0].Delta.Content)
	}

	// [3] content_block_stop → nil.
	if !results[3].drop {
		t.Errorf("content_block_stop: got %q, want nil", results[3].out)
	}

	// [4] tool_use content_block_start at content-block index 1 →
	// must get tool-call index 0 (first and only tool call in stream).
	if results[4].drop {
		t.Fatal("tool_use content_block_start: got nil, want header delta")
	}
	hdrChunk := parseChunk(t, results[4].out)
	hdrTC := hdrChunk.Choices[0].Delta.ToolCalls[0]
	if hdrTC.Index == nil || *hdrTC.Index != 0 {
		t.Errorf("tool_use at content index 1 got tool-call index %v, want 0", hdrTC.Index)
	}
	if hdrTC.ID != "toolu_idx" {
		t.Errorf("header delta id = %q, want toolu_idx", hdrTC.ID)
	}

	// [5] input_json_delta for content-block 1 → tool-call index 0.
	if results[5].drop {
		t.Fatal("input_json_delta: got nil, want fragment")
	}
	fragChunk := parseChunk(t, results[5].out)
	fragTC := fragChunk.Choices[0].Delta.ToolCalls[0]
	if fragTC.Index == nil || *fragTC.Index != 0 {
		t.Errorf("input_json_delta for content index 1 got tool-call index %v, want 0", fragTC.Index)
	}

	// Validate the mapping table.
	if a.blockToToolCall[1] != 0 {
		t.Errorf("blockToToolCall[1] = %d, want 0", a.blockToToolCall[1])
	}
}

// TestAnthropicStream_TwoToolUseBlocks_IndexMapping verifies that when two
// tool_use content blocks appear (indices 1 and 2, because index 0 is text),
// they get tool-call indices 0 and 1 respectively, and input_json_delta events
// for each route to the correct tool-call slot.
func TestAnthropicStream_TwoToolUseBlocks_IndexMapping(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}
	events := []string{
		`data: {"type":"message_start","message":{"id":"msg_two","usage":{"input_tokens":5}}}`,
		// text block at index 0 — not a tool call.
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_stop","index":0}`,
		// first tool_use at content index 1 → tool-call index 0.
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_first","name":"fn_first","input":{}}}`,
		// second tool_use at content index 2 → tool-call index 1.
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_second","name":"fn_second","input":{}}}`,
		// arguments for the second tool call (content index 2, tool-call index 1).
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"b\":2}"}}`,
		// arguments for the first tool call (content index 1, tool-call index 0).
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
	}

	var chunks []openAIChunk
	for _, ev := range events {
		out := a.TransformStreamLine([]byte(ev))
		if out != nil && !strings.HasPrefix(string(out), "data: [DONE]") {
			chunks = append(chunks, parseChunk(t, out))
		}
	}

	// Expected: role_chunk, header_first(idx=0), header_second(idx=1), frag_second(idx=1), frag_first(idx=0)
	if len(chunks) != 5 {
		t.Fatalf("len(chunks) = %d, want 5; chunks: %+v", len(chunks), chunks)
	}

	// header_first: tool-call index 0, id=toolu_first.
	hdrFirst := chunks[1].Choices[0].Delta.ToolCalls[0]
	if hdrFirst.Index == nil || *hdrFirst.Index != 0 {
		t.Errorf("hdrFirst.index = %v, want 0", hdrFirst.Index)
	}
	if hdrFirst.ID != "toolu_first" {
		t.Errorf("hdrFirst.id = %q, want toolu_first", hdrFirst.ID)
	}

	// header_second: tool-call index 1, id=toolu_second.
	hdrSecond := chunks[2].Choices[0].Delta.ToolCalls[0]
	if hdrSecond.Index == nil || *hdrSecond.Index != 1 {
		t.Errorf("hdrSecond.index = %v, want 1", hdrSecond.Index)
	}
	if hdrSecond.ID != "toolu_second" {
		t.Errorf("hdrSecond.id = %q, want toolu_second", hdrSecond.ID)
	}

	// frag for content index 2 → tool-call index 1.
	fragSecond := chunks[3].Choices[0].Delta.ToolCalls[0]
	if fragSecond.Index == nil || *fragSecond.Index != 1 {
		t.Errorf("fragSecond.index = %v, want 1", fragSecond.Index)
	}
	if fragSecond.Function.Arguments != `{"b":2}` {
		t.Errorf("fragSecond.arguments = %q, want {\"b\":2}", fragSecond.Function.Arguments)
	}

	// frag for content index 1 → tool-call index 0.
	fragFirst := chunks[4].Choices[0].Delta.ToolCalls[0]
	if fragFirst.Index == nil || *fragFirst.Index != 0 {
		t.Errorf("fragFirst.index = %v, want 0", fragFirst.Index)
	}
	if fragFirst.Function.Arguments != `{"a":1}` {
		t.Errorf("fragFirst.arguments = %q, want {\"a\":1}", fragFirst.Function.Arguments)
	}

	// Verify blockToToolCall mapping table.
	if a.blockToToolCall[1] != 0 {
		t.Errorf("blockToToolCall[1] = %d, want 0", a.blockToToolCall[1])
	}
	if a.blockToToolCall[2] != 1 {
		t.Errorf("blockToToolCall[2] = %d, want 1", a.blockToToolCall[2])
	}
}

// ── STREAMING: terminal events ────────────────────────────────────────────────

// TestAnthropicStream_TerminalEvents covers the stop-reason and dropped events.
func TestAnthropicStream_TerminalEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		line       string
		wantNil    bool
		wantExact  string
		wantFinish string // expected finish_reason in parsed chunk
	}{
		{
			name:       "message_delta stop_reason tool_use → finish_reason tool_calls",
			line:       `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":15}}`,
			wantFinish: "tool_calls",
		},
		{
			name:      "message_stop → data: [DONE]",
			line:      `data: {"type":"message_stop"}`,
			wantExact: "data: [DONE]",
		},
		{
			name:    "ping → nil (dropped)",
			line:    `data: {"type":"ping"}`,
			wantNil: true,
		},
		{
			name:    "content_block_stop → nil (dropped)",
			line:    `data: {"type":"content_block_stop","index":0}`,
			wantNil: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			// Set a known message ID so parseChunk can be used.
			_ = a.TransformStreamLine([]byte(`data: {"type":"message_start","message":{"id":"msg_term","usage":{"input_tokens":5}}}`))

			out := a.TransformStreamLine([]byte(tc.line))

			if tc.wantNil {
				if out != nil {
					t.Errorf("got %q, want nil", out)
				}
				return
			}
			if tc.wantExact != "" {
				if string(out) != tc.wantExact {
					t.Errorf("got %q, want %q", out, tc.wantExact)
				}
				return
			}
			if out == nil {
				t.Fatal("got nil, want non-nil chunk")
			}
			chunk := parseChunk(t, out)
			if len(chunk.Choices) != 1 {
				t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
			}
			ch := chunk.Choices[0]
			if ch.FinishReason == nil {
				t.Fatal("finish_reason is nil")
			}
			if *ch.FinishReason != tc.wantFinish {
				t.Errorf("finish_reason = %q, want %q", *ch.FinishReason, tc.wantFinish)
			}
		})
	}
}

// ── STREAMING: end-to-end Stage-0c conformance with PII engine ───────────────

// TestAnthropicStream_Stage0c_EndToEnd_ViaProxy is an end-to-end integration
// test that feeds a full Anthropic tool_use stream (with input_json_delta
// fragments that contain a PII pseudonym split across chunks) through the
// complete proxy pipeline — adapter + PII Stage-0c restorer — and asserts:
//
//  1. The stream completes cleanly with [DONE].
//  2. The client receives the restored original email in the tool_calls arguments.
//  3. No raw pseudonym appears in any emitted SSE line.
//  4. The tool_calls header delta (id, type, name, empty arguments) is received.
//
// The upstream emits a native Anthropic tool_use stream; the adapter translates
// it to OpenAI-shaped tool_calls deltas that the StreamRestorer processes.
func TestAnthropicStream_Stage0c_EndToEnd_ViaProxy(t *testing.T) {
	t.Parallel()

	// Build the PII engine and derive the pseudonym for piiTestEmail.
	engine := newTestPIIEngine(t)
	sampleFilter := engine.NewFilter("")
	sampleBody := []byte(chatBody("anthropic-test", "lookup "+piiTestEmail))
	anonBody, err := sampleFilter.AnonymizeJSON(sampleBody)
	if err != nil {
		t.Fatalf("pre-compute AnonymizeJSON: %v", err)
	}
	pseudo := piiPseudonymPattern.FindString(string(anonBody))
	if pseudo == "" {
		t.Fatal("could not derive pseudonym for piiTestEmail")
	}

	// Split the pseudonym across two input_json_delta events.
	mid := len(pseudo) / 2
	part1 := pseudo[:mid]
	part2 := pseudo[mid:]

	// Build the Anthropic-format stream. The tool arguments contain the pseudonym
	// split so that the Stage-0c restorer must buffer and restore across deltas.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		// Emit a complete Anthropic tool_use stream with pseudonym split across
		// two input_json_delta events. The leading `{"email":"` and trailing `"}`
		// are ASCII so they do not cross pseudonym boundaries.
		events := []string{
			`event: message_start`,
			fmt.Sprintf(`data: {"type":"message_start","message":{"id":"msg_0c","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet","stop_reason":null,"usage":{"input_tokens":12,"output_tokens":0}}}`),
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_0c","name":"lookup_user","input":{}}}`,
			``,
			// First input_json_delta: open brace + key + first half of pseudonym.
			`event: content_block_delta`,
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"email\":\"%s"}}`, part1),
			``,
			// Second input_json_delta: second half of pseudonym + closing characters.
			`event: content_block_delta`,
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"%s\"}"}}`, part2),
			``,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":10}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}
		for _, line := range events {
			fmt.Fprintln(w, line)
		}
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryAnthropic(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	baseURL := startTestServer(t, h)

	streamBody := fmt.Sprintf(
		`{"model":"anthropic-test","messages":[{"role":"user","content":"lookup %s"}],"stream":true}`,
		piiTestEmail,
	)
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: testTimeout.Timeout}
	streamResp, err := client.Do(httpReq)
	if err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	defer streamResp.Body.Close()

	if streamResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("status = %d, want 200; body: %s", streamResp.StatusCode, body)
	}

	fullBody, _ := io.ReadAll(streamResp.Body)
	fullStr := string(fullBody)

	// 1. Stream must complete with [DONE].
	if !strings.Contains(fullStr, "[DONE]") {
		t.Errorf("Stage-0c: [DONE] absent — stream did not complete cleanly\noutput: %s", fullStr)
	}

	// 2. Restored email must appear in the output (restorer replaced pseudonym).
	if !strings.Contains(fullStr, piiTestEmail) {
		t.Errorf("Stage-0c: restored email %q absent from tool_calls arguments\noutput: %s", piiTestEmail, fullStr)
	}

	// 3. No raw pseudonym visible (either part).
	if strings.Contains(fullStr, pseudo) {
		t.Errorf("SECURITY: raw pseudonym %q visible in Stage-0c output\noutput: %s", pseudo, fullStr)
	}

	// 4. tool_calls header delta must be present (id, type, name).
	if !strings.Contains(fullStr, `"tool_calls"`) {
		t.Errorf("Stage-0c: tool_calls key absent from output\noutput: %s", fullStr)
	}
	if !strings.Contains(fullStr, `"toolu_0c"`) {
		t.Errorf("Stage-0c: tool_calls id toolu_0c absent from output\noutput: %s", fullStr)
	}
	if !strings.Contains(fullStr, `"lookup_user"`) {
		t.Errorf("Stage-0c: tool name lookup_user absent from output\noutput: %s", fullStr)
	}
}

// ---- TransformStreamLine ----------------------------------------------------

// parseChunk parses a "data: {JSON}" SSE line into an openAIChunk.
func parseChunk(t *testing.T, line []byte) openAIChunk {
	t.Helper()
	const prefix = "data: "
	if !strings.HasPrefix(string(line), prefix) {
		t.Fatalf("line %q does not start with %q", line, prefix)
	}
	var chunk openAIChunk
	if err := json.Unmarshal(line[len(prefix):], &chunk); err != nil {
		t.Fatalf("parse chunk JSON: %v\nline: %s", err, line)
	}
	return chunk
}

func TestAnthropicTransformStreamLine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// line is the raw SSE line bytes sent by Anthropic's server.
		line string
		// wantNil means the adapter should drop this line.
		wantNil bool
		// wantExact, if non-empty, is the exact byte slice expected (for simple cases).
		wantExact string
		// checkFn performs structured assertions on the returned line.
		checkFn func(t *testing.T, out []byte)
	}{
		{
			name:    "event line is dropped",
			line:    "event: content_block_delta",
			wantNil: true,
		},
		{
			name:    "event message_start line is dropped",
			line:    "event: message_start",
			wantNil: true,
		},
		{
			name:      "blank line passes through unchanged",
			line:      "",
			wantExact: "",
		},
		{
			name:    "data ping is dropped",
			line:    `data: {"type":"ping"}`,
			wantNil: true,
		},
		{
			name:    "data text content_block_start is dropped",
			line:    `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			wantNil: true,
		},
		{
			name:    "data content_block_stop is dropped",
			line:    `data: {"type":"content_block_stop","index":0}`,
			wantNil: true,
		},
		{
			name:      "data message_stop becomes data: [DONE]",
			line:      `data: {"type":"message_stop"}`,
			wantExact: "data: [DONE]",
		},
		{
			name: "content_block_delta with text produces OpenAI chunk with content",
			line: `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) != 1 {
					t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
				}
				if chunk.Choices[0].Delta.Content != "Hello" {
					t.Errorf("delta.content = %q, want %q", chunk.Choices[0].Delta.Content, "Hello")
				}
				if chunk.Object != "chat.completion.chunk" {
					t.Errorf("object = %q, want %q", chunk.Object, "chat.completion.chunk")
				}
			},
		},
		{
			name: "message_start produces OpenAI chunk with role assistant",
			line: `data: {"type":"message_start","message":{"id":"msg_123","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet-20240620","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) != 1 {
					t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
				}
				if chunk.Choices[0].Delta.Role != "assistant" {
					t.Errorf("delta.role = %q, want %q", chunk.Choices[0].Delta.Role, "assistant")
				}
				// The message ID from the event should be stored and used.
				if chunk.ID != "msg_123" {
					t.Errorf("id = %q, want %q", chunk.ID, "msg_123")
				}
			},
		},
		{
			name: "message_start without message id uses fallback id",
			line: `data: {"type":"message_start","message":{}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if chunk.ID == "" {
					t.Error("chunk id is empty, want non-empty fallback id")
				}
			},
		},
		{
			name: "message_delta with stop_reason end_turn produces finish_reason stop",
			line: `data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":42}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) != 1 {
					t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
				}
				ch := chunk.Choices[0]
				if ch.FinishReason == nil {
					t.Fatal("finish_reason is nil, want non-nil")
				}
				if *ch.FinishReason != "stop" {
					t.Errorf("finish_reason = %q, want %q", *ch.FinishReason, "stop")
				}
			},
		},
		{
			name: "message_delta with stop_reason max_tokens produces finish_reason length",
			line: `data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":100}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) != 1 {
					t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
				}
				ch := chunk.Choices[0]
				if ch.FinishReason == nil {
					t.Fatal("finish_reason is nil, want non-nil")
				}
				if *ch.FinishReason != "length" {
					t.Errorf("finish_reason = %q, want %q", *ch.FinishReason, "length")
				}
			},
		},
		// ---- Tool streaming tests -----------------------------------------------
		{
			name: "content_block_start tool_use emits header delta with id type name and empty arguments",
			line: `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) != 1 {
					t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
				}
				delta := chunk.Choices[0].Delta
				if len(delta.ToolCalls) != 1 {
					t.Fatalf("len(delta.tool_calls) = %d, want 1", len(delta.ToolCalls))
				}
				tc := delta.ToolCalls[0]
				if tc.Index == nil || *tc.Index != 0 {
					t.Errorf("tool_calls[0].index = %v, want 0", tc.Index)
				}
				if tc.ID != "toolu_01" {
					t.Errorf("tool_calls[0].id = %q, want %q", tc.ID, "toolu_01")
				}
				if tc.Type != "function" {
					t.Errorf("tool_calls[0].type = %q, want %q", tc.Type, "function")
				}
				if tc.Function.Name != "get_weather" {
					t.Errorf("tool_calls[0].function.name = %q, want %q", tc.Function.Name, "get_weather")
				}
				if tc.Function.Arguments != "" {
					t.Errorf("tool_calls[0].function.arguments = %q, want empty string on header delta", tc.Function.Arguments)
				}
			},
		},
		{
			name: "input_json_delta emits arguments fragment with correct tool-call index",
			line: `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"loc"}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				// This test requires the adapter to have already seen a content_block_start
				// for index 0. We prime the adapter inline.
				// NOTE: This test is intended to be called after the adapter has seen the
				// content_block_start; the standalone test uses a fresh adapter so it will
				// drop the delta (no known mapping). The intent is verified via the
				// integration flow test below.
				// Here we verify that a nil is returned when no block is registered.
				// That behavior is tested in TestAnthropicStreamToolCallFlow.
			},
			wantNil: true, // fresh adapter has no block→toolcall mapping
		},
		{
			name: "message_delta with stop_reason tool_use produces finish_reason tool_calls",
			line: `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":30}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) != 1 {
					t.Fatalf("len(choices) = %d, want 1", len(chunk.Choices))
				}
				ch := chunk.Choices[0]
				if ch.FinishReason == nil {
					t.Fatal("finish_reason is nil, want non-nil")
				}
				if *ch.FinishReason != "tool_calls" {
					t.Errorf("finish_reason = %q, want %q", *ch.FinishReason, "tool_calls")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			out := a.TransformStreamLine([]byte(tc.line))

			if tc.wantNil {
				if out != nil {
					t.Errorf("TransformStreamLine() = %q, want nil", out)
				}
				return
			}

			if tc.wantExact != "" || tc.line == "" {
				// Exact string comparison (includes blank-line passthrough).
				if string(out) != tc.wantExact {
					t.Errorf("TransformStreamLine() = %q, want %q", out, tc.wantExact)
				}
				return
			}

			if out == nil {
				t.Fatal("TransformStreamLine() = nil, want non-nil")
			}
			if tc.checkFn != nil {
				tc.checkFn(t, out)
			}
		})
	}
}

// TestAnthropicTransformStreamLine_IDPropagation verifies that the message ID
// captured from message_start is reused in subsequent content_block_delta chunks
// produced by the same adapter instance.
func TestAnthropicTransformStreamLine_IDPropagation(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}

	startLine := []byte(`data: {"type":"message_start","message":{"id":"msg_propagate","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet-20240620"}}`)
	_ = a.TransformStreamLine(startLine)

	deltaLine := []byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}`)
	out := a.TransformStreamLine(deltaLine)
	if out == nil {
		t.Fatal("TransformStreamLine(content_block_delta) = nil, want non-nil")
	}

	chunk := parseChunk(t, out)
	if chunk.ID != "msg_propagate" {
		t.Errorf("chunk.ID = %q, want %q", chunk.ID, "msg_propagate")
	}
}

// TestAnthropicStreamToolCallFlow verifies the full streaming tool-call sequence
// from content_block_start through input_json_delta through message_delta,
// producing conformant OpenAI tool_calls deltas that the PII Stage 0c restorer can consume.
//
// Expected delta sequence (Stage 0c conformant):
//  1. message_start → role:"assistant" chunk
//  2. content_block_start(tool_use) → header delta: index=0, id, type:"function", name, arguments:""
//  3. content_block_delta(input_json_delta) → arguments fragment: index=0, function.arguments:<partial>
//  4. content_block_stop → nil (dropped)
//  5. message_delta(tool_use) → finish_reason:"tool_calls"
//  6. message_stop → "data: [DONE]"
func TestAnthropicStreamToolCallFlow(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}

	events := []string{
		`data: {"type":"message_start","message":{"id":"msg_tool_stream","usage":{"input_tokens":15}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"loc"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"ation\":\"London\"}"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":20}}`,
		`data: {"type":"message_stop"}`,
	}

	type result struct {
		line []byte
		drop bool
	}
	var results []result
	for _, ev := range events {
		out := a.TransformStreamLine([]byte(ev))
		results = append(results, result{line: out, drop: out == nil})
	}

	// 1. message_start → role chunk.
	if results[0].drop {
		t.Fatal("message_start produced nil, want role chunk")
	}
	roleChunk := parseChunk(t, results[0].line)
	if len(roleChunk.Choices) != 1 || roleChunk.Choices[0].Delta.Role != "assistant" {
		t.Errorf("message_start chunk: delta.role = %q, want %q", roleChunk.Choices[0].Delta.Role, "assistant")
	}
	if roleChunk.ID != "msg_tool_stream" {
		t.Errorf("message_start chunk: id = %q, want %q", roleChunk.ID, "msg_tool_stream")
	}

	// 2. content_block_start(tool_use) → header delta.
	if results[1].drop {
		t.Fatal("content_block_start(tool_use) produced nil, want header delta")
	}
	headerChunk := parseChunk(t, results[1].line)
	if len(headerChunk.Choices) != 1 {
		t.Fatalf("header chunk: len(choices) = %d, want 1", len(headerChunk.Choices))
	}
	headerDelta := headerChunk.Choices[0].Delta
	if len(headerDelta.ToolCalls) != 1 {
		t.Fatalf("header chunk: len(tool_calls) = %d, want 1", len(headerDelta.ToolCalls))
	}
	headerTC := headerDelta.ToolCalls[0]
	if headerTC.Index == nil || *headerTC.Index != 0 {
		t.Errorf("header delta: tool_calls[0].index = %v, want 0", headerTC.Index)
	}
	if headerTC.ID != "toolu_01" {
		t.Errorf("header delta: id = %q, want %q", headerTC.ID, "toolu_01")
	}
	if headerTC.Type != "function" {
		t.Errorf("header delta: type = %q, want %q", headerTC.Type, "function")
	}
	if headerTC.Function.Name != "get_weather" {
		t.Errorf("header delta: function.name = %q, want %q", headerTC.Function.Name, "get_weather")
	}
	if headerTC.Function.Arguments != "" {
		t.Errorf("header delta: function.arguments = %q, want empty string", headerTC.Function.Arguments)
	}

	// 3a. First input_json_delta → arguments fragment.
	if results[2].drop {
		t.Fatal("first input_json_delta produced nil, want arguments fragment")
	}
	frag1 := parseChunk(t, results[2].line)
	if len(frag1.Choices) != 1 || len(frag1.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("frag1: unexpected structure: %+v", frag1)
	}
	frag1TC := frag1.Choices[0].Delta.ToolCalls[0]
	if frag1TC.Index == nil || *frag1TC.Index != 0 {
		t.Errorf("frag1: index = %v, want 0", frag1TC.Index)
	}
	if frag1TC.ID != "" {
		t.Errorf("frag1: id = %q, want empty (arguments fragment)", frag1TC.ID)
	}
	if frag1TC.Type != "" {
		t.Errorf("frag1: type = %q, want empty (arguments fragment)", frag1TC.Type)
	}
	if frag1TC.Function.Name != "" {
		t.Errorf("frag1: function.name = %q, want empty (arguments fragment)", frag1TC.Function.Name)
	}
	if frag1TC.Function.Arguments != `{"loc` {
		t.Errorf("frag1: function.arguments = %q, want %q", frag1TC.Function.Arguments, `{"loc`)
	}

	// 3b. Second input_json_delta → second arguments fragment.
	if results[3].drop {
		t.Fatal("second input_json_delta produced nil, want arguments fragment")
	}
	frag2 := parseChunk(t, results[3].line)
	if len(frag2.Choices) != 1 || len(frag2.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("frag2: unexpected structure")
	}
	frag2TC := frag2.Choices[0].Delta.ToolCalls[0]
	if frag2TC.Index == nil || *frag2TC.Index != 0 {
		t.Errorf("frag2: index = %v, want 0", frag2TC.Index)
	}
	if frag2TC.Function.Arguments != `ation":"London"}` {
		t.Errorf("frag2: function.arguments = %q, want %q", frag2TC.Function.Arguments, `ation":"London"}`)
	}

	// 4. content_block_stop → nil.
	if !results[4].drop {
		t.Errorf("content_block_stop: got %q, want nil", results[4].line)
	}

	// 5. message_delta(tool_use) → finish_reason:"tool_calls".
	if results[5].drop {
		t.Fatal("message_delta produced nil, want finish chunk")
	}
	finishChunk := parseChunk(t, results[5].line)
	if len(finishChunk.Choices) != 1 {
		t.Fatalf("finish chunk: len(choices) = %d, want 1", len(finishChunk.Choices))
	}
	if finishChunk.Choices[0].FinishReason == nil || *finishChunk.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish chunk: finish_reason = %v, want tool_calls", finishChunk.Choices[0].FinishReason)
	}

	// 6. message_stop → [DONE].
	if string(results[6].line) != "data: [DONE]" {
		t.Errorf("message_stop: got %q, want %q", results[6].line, "data: [DONE]")
	}

	// Verify content-block-index → tool-call-index mapping is correct.
	if a.blockToToolCall[0] != 0 {
		t.Errorf("blockToToolCall[0] = %d, want 0", a.blockToToolCall[0])
	}
}

// TestAnthropicStreamMultipleToolCalls verifies that multiple consecutive tool_use
// content blocks each receive distinct tool-call indices (0, 1, 2…) and that
// input_json_delta events route to the correct tool-call slot.
func TestAnthropicStreamMultipleToolCalls(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}

	// Start two tool_use blocks.
	events := []string{
		`data: {"type":"message_start","message":{"id":"msg_multi","usage":{"input_tokens":5}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_A","name":"fn_a","input":{}}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_B","name":"fn_b","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"y\":2}"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`,
	}

	var chunks []openAIChunk
	for _, ev := range events {
		out := a.TransformStreamLine([]byte(ev))
		if out != nil && !strings.HasPrefix(string(out), "data: [DONE]") {
			chunks = append(chunks, parseChunk(t, out))
		}
	}

	// chunks: [role, header_A(idx=0), header_B(idx=1), frag_B(idx=1), frag_A(idx=0)]
	if len(chunks) != 5 {
		t.Fatalf("len(chunks) = %d, want 5", len(chunks))
	}

	// header_A: block index 0 → tool-call index 0.
	headerA := chunks[1].Choices[0].Delta.ToolCalls[0]
	if headerA.Index == nil || *headerA.Index != 0 {
		t.Errorf("headerA.index = %v, want 0", headerA.Index)
	}
	if headerA.ID != "toolu_A" {
		t.Errorf("headerA.id = %q, want toolu_A", headerA.ID)
	}

	// header_B: block index 1 → tool-call index 1.
	headerB := chunks[2].Choices[0].Delta.ToolCalls[0]
	if headerB.Index == nil || *headerB.Index != 1 {
		t.Errorf("headerB.index = %v, want 1", headerB.Index)
	}
	if headerB.ID != "toolu_B" {
		t.Errorf("headerB.id = %q, want toolu_B", headerB.ID)
	}

	// frag_B routes to tool-call index 1.
	fragB := chunks[3].Choices[0].Delta.ToolCalls[0]
	if fragB.Index == nil || *fragB.Index != 1 {
		t.Errorf("fragB.index = %v, want 1", fragB.Index)
	}

	// frag_A routes to tool-call index 0.
	fragA := chunks[4].Choices[0].Delta.ToolCalls[0]
	if fragA.Index == nil || *fragA.Index != 0 {
		t.Errorf("fragA.index = %v, want 0", fragA.Index)
	}
}

// TestAnthropicStreamTextAndToolMixed verifies that a stream with a text block
// followed by a tool_use block produces text deltas then tool header/argument
// deltas, with the tool-call index starting at 0 (not at the content-block index).
func TestAnthropicStreamTextAndToolMixed(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}

	events := []string{
		`data: {"type":"message_start","message":{"id":"msg_mix","usage":{"input_tokens":5}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I will call"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_99","name":"lookup","input":{}}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"q\":\"go\"}"}}`,
	}

	type lineResult struct {
		out  []byte
		drop bool
	}
	var results []lineResult
	for _, ev := range events {
		out := a.TransformStreamLine([]byte(ev))
		results = append(results, lineResult{out: out, drop: out == nil})
	}

	// message_start → role chunk (not nil).
	if results[0].drop {
		t.Error("message_start: got nil, want role chunk")
	}
	// text content_block_start → nil.
	if !results[1].drop {
		t.Errorf("text content_block_start: got %q, want nil", results[1].out)
	}
	// text_delta → text chunk.
	if results[2].drop {
		t.Error("text_delta: got nil, want text chunk")
	}
	textChunk := parseChunk(t, results[2].out)
	if textChunk.Choices[0].Delta.Content != "I will call" {
		t.Errorf("text delta content = %q, want %q", textChunk.Choices[0].Delta.Content, "I will call")
	}
	// content_block_stop → nil.
	if !results[3].drop {
		t.Errorf("content_block_stop: got %q, want nil", results[3].out)
	}
	// tool_use content_block_start → header delta; tool-call index must be 0
	// even though this is content-block index 1.
	if results[4].drop {
		t.Fatal("tool_use content_block_start: got nil, want header delta")
	}
	headerChunk := parseChunk(t, results[4].out)
	headerTC := headerChunk.Choices[0].Delta.ToolCalls[0]
	if headerTC.Index == nil || *headerTC.Index != 0 {
		t.Errorf("header delta: tool-call index = %v, want 0 (first tool call in stream)", headerTC.Index)
	}
	if headerTC.ID != "toolu_99" {
		t.Errorf("header delta: id = %q, want toolu_99", headerTC.ID)
	}
	// input_json_delta for content-block index 1 → arguments fragment for tool-call 0.
	if results[5].drop {
		t.Fatal("input_json_delta: got nil, want arguments fragment")
	}
	fragChunk := parseChunk(t, results[5].out)
	fragTC := fragChunk.Choices[0].Delta.ToolCalls[0]
	if fragTC.Index == nil || *fragTC.Index != 0 {
		t.Errorf("frag delta: tool-call index = %v, want 0", fragTC.Index)
	}
}

// ---- TransformURL -----------------------------------------------------------

func TestAnthropicTransformURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		baseURL      string
		upstreamPath string
		wantURL      string
	}{
		{
			name:         "chat/completions maps to /v1/messages",
			baseURL:      "https://api.anthropic.com",
			upstreamPath: "chat/completions",
			wantURL:      "https://api.anthropic.com/v1/messages",
		},
		{
			name:         "embeddings is forwarded as-is",
			baseURL:      "https://api.anthropic.com",
			upstreamPath: "embeddings",
			wantURL:      "https://api.anthropic.com/embeddings",
		},
		{
			name:         "trailing slash on base URL does not produce double slash",
			baseURL:      "https://api.anthropic.com/",
			upstreamPath: "chat/completions",
			wantURL:      "https://api.anthropic.com/v1/messages",
		},
		{
			name:         "trailing slash on base with non-chat path",
			baseURL:      "https://api.anthropic.com/",
			upstreamPath: "embeddings",
			wantURL:      "https://api.anthropic.com/embeddings",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			got := a.TransformURL(tc.baseURL, tc.upstreamPath, Model{})

			if got != tc.wantURL {
				t.Errorf("TransformURL(%q, %q) = %q, want %q", tc.baseURL, tc.upstreamPath, got, tc.wantURL)
			}

			// Guard against double slashes in the result (common trailing-slash bug).
			if strings.Contains(got, "//") && !strings.HasPrefix(got, "https://") {
				// Allow the protocol scheme's "//"; check after stripping it.
				noScheme := strings.SplitN(got, "://", 2)
				if len(noScheme) == 2 && strings.Contains(noScheme[1], "//") {
					t.Errorf("TransformURL result %q contains double slash in path", got)
				}
			}
		})
	}
}

// ---- SetHeaders -------------------------------------------------------------

func TestAnthropicSetHeaders(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		model        Model
		initialAuth  string // Authorization header value set before calling SetHeaders
		wantXAPIKey  string // expected x-api-key value ("" means header must be absent)
		wantAuthGone bool   // Authorization header must be absent
	}{
		{
			name:         "Authorization removed and x-api-key set",
			model:        Model{APIKey: "ant-key-abc"},
			initialAuth:  "Bearer vl_uk_somekey",
			wantXAPIKey:  "ant-key-abc",
			wantAuthGone: true,
		},
		{
			name:         "empty APIKey produces no x-api-key header",
			model:        Model{APIKey: ""},
			initialAuth:  "Bearer vl_uk_somekey",
			wantXAPIKey:  "",
			wantAuthGone: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", nil)
			if tc.initialAuth != "" {
				req.Header.Set("Authorization", tc.initialAuth)
			}

			a := &AnthropicAdapter{}
			a.SetHeaders(req, tc.model)

			if tc.wantAuthGone {
				if got := req.Header.Get("Authorization"); got != "" {
					t.Errorf("Authorization header = %q, want absent (empty)", got)
				}
			}

			if tc.wantXAPIKey != "" {
				if got := req.Header.Get("x-api-key"); got != tc.wantXAPIKey {
					t.Errorf("x-api-key = %q, want %q", got, tc.wantXAPIKey)
				}
			} else {
				if got := req.Header.Get("x-api-key"); got != "" {
					t.Errorf("x-api-key = %q, want absent (empty)", got)
				}
			}

			if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
				t.Errorf("anthropic-version = %q, want %q", got, "2023-06-01")
			}

			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q, want %q", got, "application/json")
			}
		})
	}
}

// ── FIX 2: buildTextMessage with structured content ───────────────────────────

// TestAnthropicBuildTextMessage_ArrayContent verifies that user/assistant
// messages whose content is an array of parts are translated to separate
// Anthropic text blocks (FIX 2), not serialized as literal JSON text, and that
// unknown part types cause a fail-closed error.
func TestAnthropicBuildTextMessage_ArrayContent(t *testing.T) {
	t.Parallel()

	t.Run("plain string content emits single text block (byte-identical)", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[{"role":"user","content":"hello world"}]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)
		if len(msgs) != 1 {
			t.Fatalf("len(msgs) = %d, want 1", len(msgs))
		}
		if len(msgs[0].Content) != 1 {
			t.Fatalf("len(content) = %d, want 1", len(msgs[0].Content))
		}
		if msgs[0].Content[0].Type != "text" {
			t.Errorf("block type = %q, want text", msgs[0].Content[0].Type)
		}
		if msgs[0].Content[0].Text != "hello world" {
			t.Errorf("block text = %q, want hello world", msgs[0].Content[0].Text)
		}
	})

	t.Run("array content with two text parts emits two separate text blocks", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[{"role":"user","content":[{"type":"text","text":"part one"},{"type":"text","text":"part two"}]}]}`
		doc := transformRequest(t, input)
		msgs := unmarshalMessages(t, doc)
		if len(msgs) != 1 {
			t.Fatalf("len(msgs) = %d, want 1", len(msgs))
		}
		if len(msgs[0].Content) != 2 {
			t.Fatalf("len(content) = %d, want 2 separate blocks", len(msgs[0].Content))
		}
		if msgs[0].Content[0].Type != "text" || msgs[0].Content[0].Text != "part one" {
			t.Errorf("block[0] = {%s, %q}, want {text, part one}", msgs[0].Content[0].Type, msgs[0].Content[0].Text)
		}
		if msgs[0].Content[1].Type != "text" || msgs[0].Content[1].Text != "part two" {
			t.Errorf("block[1] = {%s, %q}, want {text, part two}", msgs[0].Content[1].Type, msgs[0].Content[1].Text)
		}
		// The parts must NOT appear as literal JSON array text.
		if msgs[0].Content[0].Text == `[{"type":"text","text":"part one"},{"type":"text","text":"part two"}]` {
			t.Error("content was serialized to literal JSON text; must be separate blocks")
		}
	})

	t.Run("unknown part type returns error (fail-closed)", func(t *testing.T) {
		t.Parallel()
		input := `{"model":"claude-3","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://example.com/img.png"}}]}]}`
		a := &AnthropicAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for unsupported content part type, got nil")
		}
	})
}

// ── FIX 3: no caller content in error strings ──────────────────────────────────

// TestAnthropicErrorStrings verifies that error strings produced by
// TransformRequest do not contain caller-supplied function names or other
// caller-derived values that could carry PII.
func TestAnthropicErrorStrings(t *testing.T) {
	t.Parallel()

	// Invalid arguments string — old code included the function name via %q.
	input := `{"model":"claude-3","messages":[` +
		`{"role":"user","content":"q"},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"tc1","type":"function","function":{"name":"sensitiveFunc","arguments":"not-json"}}]}` +
		`]}`
	a := &AnthropicAdapter{}
	_, err := a.TransformRequest([]byte(input), Model{})
	if err == nil {
		t.Fatal("expected error for invalid arguments, got nil")
	}
	errStr := err.Error()
	// The function name "sensitiveFunc" must NOT appear in the error message.
	if strings.Contains(errStr, "sensitiveFunc") {
		t.Errorf("error string leaks function name %q; error: %s", "sensitiveFunc", errStr)
	}
	// Error must be lowercase, content-free.
	if strings.Contains(errStr, `"sensitiveFunc"`) {
		t.Errorf("error string contains quoted function name; error: %s", errStr)
	}
}

// ── FIX 4: streaming state cap ────────────────────────────────────────────────

// TestAnthropicStream_ToolBlockCap verifies that streaming content_block_start
// events for more than maxAnthropicToolBlocks distinct tool_use blocks are
// silently dropped — the adapter does not grow the map beyond the cap.
func TestAnthropicStream_ToolBlockCap(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}
	// Prime with message_start.
	_ = a.TransformStreamLine([]byte(`data: {"type":"message_start","message":{"id":"msg_cap","usage":{"input_tokens":1}}}`))

	accepted := 0
	dropped := 0
	for i := 0; i < maxAnthropicToolBlocks+10; i++ {
		line := fmt.Sprintf(
			`data: {"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"toolu_%d","name":"fn_%d","input":{}}}`,
			i, i, i,
		)
		out := a.TransformStreamLine([]byte(line))
		if out != nil {
			accepted++
		} else {
			dropped++
		}
	}

	if accepted != maxAnthropicToolBlocks {
		t.Errorf("accepted = %d, want %d (cap)", accepted, maxAnthropicToolBlocks)
	}
	if dropped != 10 {
		t.Errorf("dropped = %d, want 10 (excess beyond cap)", dropped)
	}
	// The map must not exceed the cap.
	if len(a.blockToToolCall) > maxAnthropicToolBlocks {
		t.Errorf("blockToToolCall len = %d, exceeds cap %d", len(a.blockToToolCall), maxAnthropicToolBlocks)
	}
}

// TestAnthropicStream_NegativeIndex verifies that a content_block_start with a
// negative index is silently dropped (FIX 4 defense against oversized indices).
func TestAnthropicStream_NegativeIndex(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}
	_ = a.TransformStreamLine([]byte(`data: {"type":"message_start","message":{"id":"msg_neg","usage":{"input_tokens":1}}}`))

	line := `data: {"type":"content_block_start","index":-1,"content_block":{"type":"tool_use","id":"toolu_neg","name":"fn","input":{}}}`
	out := a.TransformStreamLine([]byte(line))
	if out != nil {
		t.Errorf("negative index: got %q, want nil (drop)", out)
	}
	if a.toolCallCounter != 0 {
		t.Errorf("toolCallCounter = %d, want 0 (negative index must not increment counter)", a.toolCallCounter)
	}
}

// ── FIX 5: charset validation for tool ids and names ──────────────────────────

// TestAnthropicFix5_ToolIDCharset verifies that tool_call_id and function name
// values containing invalid characters (e.g. "@") cause TransformRequest to
// return an error (fail-closed), while valid values are accepted.
func TestAnthropicFix5_ToolIDCharset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name: "valid tool_call_id accepted",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"q"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"fn","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"call_abc","content":"result"}` +
				`]}`,
			wantErr: false,
		},
		{
			name: "tool_call_id with @ rejected",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"q"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"alice@example.com","type":"function","function":{"name":"fn","arguments":"{}"}}]},` +
				`{"role":"tool","tool_call_id":"alice@example.com","content":"result"}` +
				`]}`,
			wantErr: true,
		},
		{
			name: "function name with @ rejected",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":"q"},` +
				`{"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"alice@example.com","arguments":"{}"}}]}` +
				`]}`,
			wantErr: true,
		},
		{
			name: "tool definition name with @ rejected",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"alice@example.com","parameters":{}}}]}`,
			wantErr: true,
		},
		{
			name: "valid tool definition name accepted",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"get_weather","parameters":{}}}]}`,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &AnthropicAdapter{}
			_, err := a.TransformRequest([]byte(tc.input), Model{})
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestAnthropicFix5_ResponseToolIDCharset verifies that TransformResponse
// rejects tool_use blocks in the Anthropic response whose id or name contains
// invalid characters (fail-closed on the response path).
func TestAnthropicFix5_ResponseToolIDCharset(t *testing.T) {
	t.Parallel()

	t.Run("valid response tool_use id and name accepted", func(t *testing.T) {
		t.Parallel()
		input := `{"id":"msg_ok","type":"message","model":"claude-3","content":[` +
			`{"type":"tool_use","id":"toolu_abc","name":"get_weather","input":{"city":"London"}}` +
			`],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":5}}`
		a := &AnthropicAdapter{}
		_, err := a.TransformResponse([]byte(input))
		if err != nil {
			t.Fatalf("unexpected error for valid id/name: %v", err)
		}
	})

	t.Run("response tool_use id with @ rejected", func(t *testing.T) {
		t.Parallel()
		input := `{"id":"msg_bad","type":"message","model":"claude-3","content":[` +
			`{"type":"tool_use","id":"alice@example.com","name":"fn","input":{}}` +
			`],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":5}}`
		a := &AnthropicAdapter{}
		_, err := a.TransformResponse([]byte(input))
		if err == nil {
			t.Fatal("expected error for tool_use id with @, got nil")
		}
	})

	t.Run("response tool_use name with @ rejected", func(t *testing.T) {
		t.Parallel()
		input := `{"id":"msg_bad2","type":"message","model":"claude-3","content":[` +
			`{"type":"tool_use","id":"toolu_ok","name":"alice@example.com","input":{}}` +
			`],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":5}}`
		a := &AnthropicAdapter{}
		_, err := a.TransformResponse([]byte(input))
		if err == nil {
			t.Fatal("expected error for tool_use name with @, got nil")
		}
	})
}

// ── FIX 6: tool and tool_choice validation ────────────────────────────────────

// TestAnthropicFix6_ToolValidation verifies that TransformRequest fails closed
// on invalid tool definitions: non-function type, empty name, and parameters
// that are not a JSON object.
func TestAnthropicFix6_ToolValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name: "tool type web_search rejected",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"web_search","function":{"name":"search","parameters":{}}}]}`,
			wantErr: true,
		},
		{
			name: "tool with empty function name rejected",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"","parameters":{}}}]}`,
			wantErr: true,
		},
		{
			name: "tool parameters null rejected",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"fn","parameters":null}}]}`,
			wantErr: true,
		},
		{
			name: "tool parameters array rejected",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"fn","parameters":[1,2,3]}}]}`,
			wantErr: true,
		},
		{
			name: "tool parameters scalar rejected",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"fn","parameters":42}}]}`,
			wantErr: true,
		},
		{
			name: "tool with no parameters gets empty-object schema (ok)",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"fn"}}]}`,
			wantErr: false,
		},
		{
			name: "valid function tool accepted",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{}}}}]}`,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &AnthropicAdapter{}
			_, err := a.TransformRequest([]byte(tc.input), Model{})
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// TestAnthropicFix6_ToolChoiceValidation verifies that TransformRequest fails
// closed on unknown tool_choice values: unknown strings, unknown object shapes,
// and named choices that reference undeclared tools.
func TestAnthropicFix6_ToolChoiceValidation(t *testing.T) {
	t.Parallel()

	baseTools := `[{"type":"function","function":{"name":"get_weather","parameters":{}}}]`

	tests := []struct {
		name          string
		toolChoiceRaw string
		wantErr       bool
	}{
		{
			name:          "unknown string value rejected",
			toolChoiceRaw: `"magic"`,
			wantErr:       true,
		},
		{
			name:          "unknown object type rejected",
			toolChoiceRaw: `{"type":"web_search"}`,
			wantErr:       true,
		},
		{
			name:          "named choice for undeclared tool rejected",
			toolChoiceRaw: `{"type":"function","function":{"name":"undeclared_tool"}}`,
			wantErr:       true,
		},
		{
			name:          "named choice for declared tool accepted",
			toolChoiceRaw: `{"type":"function","function":{"name":"get_weather"}}`,
			wantErr:       false,
		},
		{
			name:          "auto string accepted",
			toolChoiceRaw: `"auto"`,
			wantErr:       false,
		},
		{
			name:          "required string accepted",
			toolChoiceRaw: `"required"`,
			wantErr:       false,
		},
		{
			name:          "none string accepted (tools removed)",
			toolChoiceRaw: `"none"`,
			wantErr:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			input := fmt.Sprintf(
				`{"model":"claude-3","messages":[{"role":"user","content":"q"}],"tools":%s,"tool_choice":%s}`,
				baseTools, tc.toolChoiceRaw,
			)
			a := &AnthropicAdapter{}
			_, err := a.TransformRequest([]byte(input), Model{})
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// ── FIX 7: non-streaming response tool_use input validation ───────────────────

// TestAnthropicFix7_ResponseInputValidation verifies that TransformResponse
// fails closed when an Anthropic tool_use block's input is present but is not
// a JSON object (array or scalar).
func TestAnthropicFix7_ResponseInputValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		inputJSON string
		wantErr   bool
	}{
		{
			name: "object input accepted",
			inputJSON: `{"id":"msg_ok","type":"message","model":"claude-3","content":[` +
				`{"type":"tool_use","id":"toolu_ok","name":"fn","input":{"key":"val"}}` +
				`],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`,
			wantErr: false,
		},
		{
			name: "empty input accepted as empty object",
			inputJSON: `{"id":"msg_empty","type":"message","model":"claude-3","content":[` +
				`{"type":"tool_use","id":"toolu_empty","name":"fn","input":{}}` +
				`],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`,
			wantErr: false,
		},
		{
			name: "array input rejected",
			inputJSON: `{"id":"msg_arr","type":"message","model":"claude-3","content":[` +
				`{"type":"tool_use","id":"toolu_arr","name":"fn","input":[1,2,3]}` +
				`],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`,
			wantErr: true,
		},
		{
			name: "scalar input rejected",
			inputJSON: `{"id":"msg_scalar","type":"message","model":"claude-3","content":[` +
				`{"type":"tool_use","id":"toolu_scalar","name":"fn","input":42}` +
				`],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`,
			wantErr: true,
		},
		{
			name: "string input rejected",
			inputJSON: `{"id":"msg_str","type":"message","model":"claude-3","content":[` +
				`{"type":"tool_use","id":"toolu_str","name":"fn","input":"hello"}` +
				`],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":1}}`,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a := &AnthropicAdapter{}
			_, err := a.TransformResponse([]byte(tc.inputJSON))
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// ── FIX 8: duplicate content-block index ──────────────────────────────────────

// TestAnthropicStream_DuplicateContentBlockIndex verifies that a duplicate
// content_block_start for an already-seen content-block index is silently
// dropped (returns nil) rather than overwriting the existing mapping.
// The first registration remains intact; subsequent deltas continue to route
// correctly to the original tool call.
func TestAnthropicStream_DuplicateContentBlockIndex(t *testing.T) {
	t.Parallel()

	a := &AnthropicAdapter{}
	_ = a.TransformStreamLine([]byte(`data: {"type":"message_start","message":{"id":"msg_dup","usage":{"input_tokens":1}}}`))

	// Register content-block index 0 → tool-call index 0.
	first := a.TransformStreamLine([]byte(
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_first","name":"fn_first","input":{}}}`,
	))
	if first == nil {
		t.Fatal("first content_block_start: got nil, want header delta")
	}

	// Duplicate registration for the same index — must be dropped.
	dup := a.TransformStreamLine([]byte(
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_dup","name":"fn_dup","input":{}}}`,
	))
	if dup != nil {
		t.Errorf("duplicate content_block_start: got %q, want nil (must be dropped)", dup)
	}

	// toolCallCounter must still be 1 (the duplicate must not increment it).
	if a.toolCallCounter != 1 {
		t.Errorf("toolCallCounter = %d after duplicate, want 1", a.toolCallCounter)
	}

	// The mapping for index 0 must still point to the first tool call (0).
	if a.blockToToolCall[0] != 0 {
		t.Errorf("blockToToolCall[0] = %d after duplicate, want 0 (original mapping preserved)", a.blockToToolCall[0])
	}

	// Subsequent input_json_delta for index 0 must still route to tool-call 0.
	frag := a.TransformStreamLine([]byte(
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":1}"}}`,
	))
	if frag == nil {
		t.Fatal("input_json_delta after duplicate: got nil, want fragment delta")
	}
	chunk := parseChunk(t, frag)
	if len(chunk.Choices) != 1 || len(chunk.Choices[0].Delta.ToolCalls) != 1 {
		t.Fatalf("fragment chunk structure unexpected: %+v", chunk)
	}
	tc := chunk.Choices[0].Delta.ToolCalls[0]
	if tc.Index == nil || *tc.Index != 0 {
		t.Errorf("fragment tool-call index = %v, want 0", tc.Index)
	}
}

// ── FIX 1 (streaming): charset validation on content_block_start tool_use ───

// TestAnthropicStream_ToolUseCharsetValidation verifies that a streaming
// content_block_start event for a tool_use block whose id or name contains
// invalid characters is silently dropped (returns nil), matching the fail-closed
// behavior of the non-streaming TransformResponse path.
func TestAnthropicStream_ToolUseCharsetValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    string
		wantNil bool
	}{
		{
			name:    "invalid id with @ is dropped",
			line:    `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"bad@id","name":"good_name","input":{}}}`,
			wantNil: true,
		},
		{
			name:    "invalid name with space is dropped",
			line:    `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_ok","name":"bad name","input":{}}}`,
			wantNil: true,
		},
		{
			name:    "invalid id with @ in name position is dropped",
			line:    `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_ok","name":"a@b","input":{}}}`,
			wantNil: true,
		},
		{
			name:    "valid id and name produces header delta",
			line:    `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_valid","name":"get_weather","input":{}}}`,
			wantNil: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			// Prime with message_start.
			_ = a.TransformStreamLine([]byte(`data: {"type":"message_start","message":{"id":"msg_cs","usage":{"input_tokens":1}}}`))

			out := a.TransformStreamLine([]byte(tc.line))
			if tc.wantNil {
				if out != nil {
					t.Errorf("got %q, want nil (block must be dropped for invalid charset)", out)
				}
				// Verify the tool-call counter was not incremented.
				if a.toolCallCounter != 0 {
					t.Errorf("toolCallCounter = %d after dropped block, want 0", a.toolCallCounter)
				}
			} else {
				if out == nil {
					t.Fatal("got nil, want header delta for valid id/name")
				}
			}
		})
	}
}

// ── FIX 2: unconditional charset validation in translateToolChoice ────────────

// TestAnthropicTranslateToolChoice_InvalidNameNoTools verifies that
// translateToolChoice rejects a named function tool_choice unconditionally when
// the name is invalid OR when no tools are declared (declaredNames is empty).
// FIX 4: a named tool_choice must reference a declared tool — if there are no
// declared tools, or the name is not among them, the result is fail-closed.
func TestAnthropicTranslateToolChoice_InvalidNameNoTools(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		toolChoiceRaw string
		wantErr       bool
	}{
		{
			name:          "invalid function name with @ rejected even with no tools declared",
			toolChoiceRaw: `{"type":"function","function":{"name":"a@b"}}`,
			wantErr:       true,
		},
		{
			name:          "invalid function name with space rejected even with no tools declared",
			toolChoiceRaw: `{"type":"function","function":{"name":"bad name"}}`,
			wantErr:       true,
		},
		{
			// FIX 4: a valid name but no declared tools is also fail-closed.
			// A named tool_choice with no tools array is a protocol error.
			name:          "valid function name with no declared tools is rejected fail-closed",
			toolChoiceRaw: `{"type":"function","function":{"name":"valid_fn"}}`,
			wantErr:       true,
		},
	}

	// Use no declared tools (empty tools list) so we hit the fail-closed
	// unconditional check.
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// Build a request with NO tools array but with tool_choice (unusual but
			// valid to construct to test the isolated translateToolChoice behavior).
			raw := json.RawMessage(tc.toolChoiceRaw)
			_, err := translateToolChoice(raw, nil)
			if tc.wantErr && err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

// ── FIX 3: no caller content in error strings (extended) ─────────────────────

// TestAnthropicErrorStrings_NoCaller verifies that all new validation error
// messages added by FIX 1, FIX 2, and FIX 3 do not contain caller-supplied
// values such as type discriminators, function names, or other input strings.
func TestAnthropicErrorStrings_NoCaller(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		wantErrSubstr string // substring that must NOT appear in the error
	}{
		{
			name: "unsupported tool type error contains no caller value",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"web_search_XYZ","function":{"name":"fn","parameters":{}}}]}`,
			wantErrSubstr: "web_search_XYZ",
		},
		{
			name: "unknown tool_choice string value not in error",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"fn","parameters":{}}}],` +
				`"tool_choice":"magic_value_XYZ"}`,
			wantErrSubstr: "magic_value_XYZ",
		},
		{
			name: "unknown tool_choice object type not in error",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"fn","parameters":{}}}],` +
				`"tool_choice":{"type":"web_search_XYZ"}}`,
			wantErrSubstr: "web_search_XYZ",
		},
		{
			name: "undeclared tool name not in error",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tools":[{"type":"function","function":{"name":"fn","parameters":{}}}],` +
				`"tool_choice":{"type":"function","function":{"name":"undeclared_secret_XYZ"}}}`,
			wantErrSubstr: "undeclared_secret_XYZ",
		},
		{
			name: "unsupported content part type not in error",
			input: `{"model":"claude-3","messages":[` +
				`{"role":"user","content":[{"type":"image_url_XYZ","image_url":{"url":"http://example.com/img.png"}}]}` +
				`]}`,
			wantErrSubstr: "image_url_XYZ",
		},
		{
			name: "invalid tool_choice function name not in error (FIX 2)",
			input: `{"model":"claude-3","messages":[{"role":"user","content":"q"}],` +
				`"tool_choice":{"type":"function","function":{"name":"secret@name_XYZ"}}}`,
			wantErrSubstr: "secret@name_XYZ",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			_, err := a.TransformRequest([]byte(tc.input), Model{})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if strings.Contains(err.Error(), tc.wantErrSubstr) {
				t.Errorf("error string leaks caller value %q; full error: %s", tc.wantErrSubstr, err.Error())
			}
		})
	}
}

// ── FIX 4: content:null treated as empty, not error ──────────────────────────

// TestAnthropicBuildTextMessage_NullContent verifies that user and assistant
// messages with JSON null content are accepted (not rejected) and produce a
// message with a single empty text block — matching the behavior of nil content.
// This is required by PR #136 which established null as a legitimate value.
func TestAnthropicBuildTextMessage_NullContent(t *testing.T) {
	t.Parallel()

	roles := []string{"user", "assistant"}
	for _, role := range roles {
		role := role
		t.Run("role "+role+" with null content produces empty text block", func(t *testing.T) {
			t.Parallel()

			input := fmt.Sprintf(
				`{"model":"claude-3","messages":[{"role":"%s","content":null}]}`,
				role,
			)
			a := &AnthropicAdapter{}
			out, err := a.TransformRequest([]byte(input), Model{})
			if err != nil {
				t.Fatalf("TransformRequest returned error for content:null; role=%s: %v", role, err)
			}

			doc := unmarshalDoc(t, out)
			msgs := unmarshalMessages(t, doc)
			if len(msgs) != 1 {
				t.Fatalf("len(messages) = %d, want 1", len(msgs))
			}
			msg := msgs[0]
			if msg.Role != role {
				t.Errorf("role = %q, want %q", msg.Role, role)
			}
			// Must have a single text block with empty text.
			if len(msg.Content) != 1 {
				t.Fatalf("len(content) = %d, want 1 (empty text block)", len(msg.Content))
			}
			if msg.Content[0].Type != "text" {
				t.Errorf("content[0].type = %q, want text", msg.Content[0].Type)
			}
			if msg.Content[0].Text != "" {
				t.Errorf("content[0].text = %q, want empty string", msg.Content[0].Text)
			}
		})
	}

	t.Run("number content still returns error (not null)", func(t *testing.T) {
		t.Parallel()

		input := `{"model":"claude-3","messages":[{"role":"user","content":42}]}`
		a := &AnthropicAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for numeric content, got nil")
		}
	})
}

// ── FIX 1 (parity security): pseudonym-shaped tool_use id/name rejected ──────

// TestAnthropicTransformResponse_PseudonymToolUse verifies that FIX 1 closes
// the non-streaming PII leak in the Anthropic adapter's TransformResponse.
// If a (malicious or compromised) upstream returns a tool_use id or name that
// contains or matches the canonical PII pseudonym shape, TransformResponse must
// return a content-free error rather than forwarding the value (where
// filter.Restore would substitute real PII into the response body).
func TestAnthropicTransformResponse_PseudonymToolUse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		inputJSON string
	}{
		{
			name: "tool_use id matching canonical pseudonym shape is rejected",
			// PII_EM_<24hex> is the shape of an email pseudonym (2-char type abbrev = EM).
			inputJSON: `{"id":"msg_01","type":"message","model":"claude-3","content":[{"type":"tool_use","id":"PII_EM_aabbccddeeff00112233445566","name":"get_weather","input":{"location":"NYC"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`,
		},
		{
			name:      "tool_use name matching canonical pseudonym shape is rejected",
			inputJSON: `{"id":"msg_02","type":"message","model":"claude-3","content":[{"type":"tool_use","id":"toolu_01","name":"PII_PN_aabbccddeeff00112233445566","input":{"q":"x"}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`,
		},
		{
			name:      "tool_use id containing PII_ substring is rejected",
			inputJSON: `{"id":"msg_03","type":"message","model":"claude-3","content":[{"type":"tool_use","id":"toolu_PII_something","name":"fn","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`,
		},
		{
			name:      "tool_use name containing PII_ substring is rejected",
			inputJSON: `{"id":"msg_04","type":"message","model":"claude-3","content":[{"type":"tool_use","id":"toolu_01","name":"fn_PII_suffix","input":{}}],"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			out, err := a.TransformResponse([]byte(tc.inputJSON))
			if err == nil {
				t.Fatalf("TransformResponse() = %s, want error for pseudonym-shaped tool_use field", out)
			}
			// Error must be content-free: must not echo back the pseudonym value.
			errStr := err.Error()
			if strings.Contains(errStr, "PII_EM_") || strings.Contains(errStr, "aabbcc") ||
				strings.Contains(errStr, "PII_PN_") || strings.Contains(errStr, "PII_something") ||
				strings.Contains(errStr, "PII_suffix") {
				t.Errorf("error message leaks pseudonym content: %s", errStr)
			}
		})
	}
}

// TestAnthropicTransformResponse_LegitToolUse confirms that a legitimate tool_use
// block (no PII_ prefix in id or name) is not rejected by the pseudonym check.
func TestAnthropicTransformResponse_LegitToolUse(t *testing.T) {
	t.Parallel()

	input := `{"id":"msg_01","type":"message","model":"claude-3","content":[{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{"location":"London"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`
	a := &AnthropicAdapter{}
	out, err := a.TransformResponse([]byte(input))
	if err != nil {
		t.Fatalf("TransformResponse() error = %v for legitimate tool_use id/name", err)
	}
	if out == nil {
		t.Fatal("TransformResponse() = nil, want non-nil for legitimate tool_use")
	}
}

// ── FIX 2: synthesized id/model — upstream values not forwarded ───────────────

// TestAnthropicTransformResponse_SynthesizedIDModel verifies that
// TransformResponse synthesizes the response id and model locally, rather than
// forwarding the upstream Anthropic id (which could carry a PII pseudonym that
// filter.Restore would expand into real PII in a structural client field).
func TestAnthropicTransformResponse_SynthesizedIDModel(t *testing.T) {
	t.Parallel()

	t.Run("upstream id is not forwarded to client", func(t *testing.T) {
		t.Parallel()
		upstream := `{"id":"msg_UPSTREAM_ID","type":"message","model":"claude-3-opus","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":5,"output_tokens":3}}`
		a := &AnthropicAdapter{}
		out, err := a.TransformResponse([]byte(upstream))
		if err != nil {
			t.Fatalf("TransformResponse() error = %v", err)
		}
		var resp openAIResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// The upstream id must not appear verbatim in the response.
		if resp.ID == "msg_UPSTREAM_ID" {
			t.Error("upstream id was forwarded verbatim to client; must be synthesized")
		}
		// Synthesized id must start with "chatcmpl-".
		if !strings.HasPrefix(resp.ID, "chatcmpl-") {
			t.Errorf("synthesized id = %q, want chatcmpl- prefix", resp.ID)
		}
	})

	t.Run("upstream model is not forwarded when modelName captured from request", func(t *testing.T) {
		t.Parallel()
		upstream := `{"id":"msg_01","type":"message","model":"claude-upstream-model","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		a := &AnthropicAdapter{}
		// Simulate TransformRequest having set modelName.
		a.modelName = "claude-3-sonnet-client-requested"
		out, err := a.TransformResponse([]byte(upstream))
		if err != nil {
			t.Fatalf("TransformResponse() error = %v", err)
		}
		var resp openAIResponse
		if err := json.Unmarshal(out, &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		// Response model must be the client-requested name, not the upstream model.
		if resp.Model == "claude-upstream-model" {
			t.Error("upstream model name was forwarded verbatim; must use client-requested model")
		}
		if resp.Model != "claude-3-sonnet-client-requested" {
			t.Errorf("model = %q, want claude-3-sonnet-client-requested", resp.Model)
		}
	})

	t.Run("TransformRequest captures model name for response synthesis", func(t *testing.T) {
		t.Parallel()
		a := &AnthropicAdapter{}
		input := `{"model":"my-claude-model","messages":[{"role":"user","content":"hi"}]}`
		_, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("TransformRequest() error = %v", err)
		}
		if a.modelName != "my-claude-model" {
			t.Errorf("modelName = %q after TransformRequest, want my-claude-model", a.modelName)
		}
	})

	t.Run("pseudonym in upstream id does not leak to client", func(t *testing.T) {
		t.Parallel()
		// A malicious upstream echoes a PII pseudonym into the id field.
		// filter.Restore would substitute the real PII if we forwarded it.
		// The synthesized id must not carry the pseudonym.
		upstream := `{"id":"PII_EM_aabbccddeeff00112233445566","type":"message","model":"claude-3","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`
		a := &AnthropicAdapter{}
		out, err := a.TransformResponse([]byte(upstream))
		if err != nil {
			t.Fatalf("TransformResponse() error = %v", err)
		}
		outStr := string(out)
		// The pseudonym must not appear in the output at all.
		if strings.Contains(outStr, "PII_EM_aabbccddeeff00112233445566") {
			t.Errorf("SECURITY: upstream pseudonym-shaped id leaked into response: %s", outStr)
		}
	})
}

// ── FIX 4: named tool_choice with no declared tools ───────────────────────────

// TestAnthropicTransformRequest_Fix4_NamedToolChoiceNoTools verifies that a
// named tool_choice without any declared tools fails closed unconditionally.
func TestAnthropicTransformRequest_Fix4_NamedToolChoiceNoTools(t *testing.T) {
	t.Parallel()

	t.Run("named tool_choice with no tools array returns error", func(t *testing.T) {
		t.Parallel()
		// No tools declared, but tool_choice names a function.
		input := `{"model":"claude-3","messages":[{"role":"user","content":"q"}],"tool_choice":{"type":"function","function":{"name":"get_data"}}}`
		a := &AnthropicAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for named tool_choice with no tools declared, got nil")
		}
	})

	t.Run("named tool_choice with tools but name not in list returns error", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model":"claude-3",
			"messages":[{"role":"user","content":"q"}],
			"tools":[{"type":"function","function":{"name":"fn_a","parameters":{}}}],
			"tool_choice":{"type":"function","function":{"name":"fn_missing"}}
		}`
		a := &AnthropicAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err == nil {
			t.Fatal("expected error for named tool_choice referencing undeclared function, got nil")
		}
	})

	t.Run("named tool_choice with matching declared tool succeeds", func(t *testing.T) {
		t.Parallel()
		input := `{
			"model":"claude-3",
			"messages":[{"role":"user","content":"q"}],
			"tools":[{"type":"function","function":{"name":"get_data","parameters":{}}}],
			"tool_choice":{"type":"function","function":{"name":"get_data"}}
		}`
		a := &AnthropicAdapter{}
		_, err := a.TransformRequest([]byte(input), Model{})
		if err != nil {
			t.Fatalf("unexpected error for valid named tool_choice: %v", err)
		}
	})
}

// ── FIX 5: tool block cap at 64 ───────────────────────────────────────────────

// TestAnthropicToolBlockCap verifies that maxAnthropicToolBlocks is 64, aligned
// with the PII Stage 0c StreamRestorer's maxToolCallsPerChoice=64.
func TestAnthropicToolBlockCap(t *testing.T) {
	t.Parallel()

	if maxAnthropicToolBlocks != 64 {
		t.Errorf("maxAnthropicToolBlocks = %d, want 64 (must match Stage 0c maxToolCallsPerChoice)", maxAnthropicToolBlocks)
	}
}
