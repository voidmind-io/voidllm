package proxy

import (
	"encoding/json"
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
				var msgs []anthropicMessage
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
				var contentStr string
				if err := json.Unmarshal(msgs[0].Content, &contentStr); err != nil {
					t.Fatalf("unmarshal remaining message content: %v", err)
				}
				if msgs[0].Role != "user" || contentStr != "Hi" {
					t.Errorf("remaining message role = %q content = %q, want {user Hi}", msgs[0].Role, contentStr)
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
				var msgs []anthropicMessage
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
		wantContent    string
		wantFinish     string
		wantPrompt     int
		wantCompletion int
		wantTotal      int
		wantErr        bool
	}{
		{
			name:           "basic response maps fields to OpenAI format",
			inputJSON:      `{"id":"msg_01abc","type":"message","model":"claude-3-5-sonnet-20240620","content":[{"type":"text","text":"Hello there"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`,
			wantID:         "msg_01abc",
			wantObject:     "chat.completion",
			wantContent:    "Hello there",
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
			wantContent: "Hello world",
		},
		{
			name:        "non-text content blocks are ignored",
			inputJSON:   `{"id":"msg_08","content":[{"type":"tool_use","text":""},{"type":"text","text":"answer"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
			wantContent: "answer",
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

			var resp openAIResponse
			if err := json.Unmarshal(out, &resp); err != nil {
				t.Fatalf("output is not valid OpenAI response JSON: %v", err)
			}

			if resp.Object != "chat.completion" {
				t.Errorf("object = %q, want %q", resp.Object, "chat.completion")
			}
			if tc.wantID != "" && resp.ID != tc.wantID {
				t.Errorf("id = %q, want %q", resp.ID, tc.wantID)
			}
			if len(resp.Choices) != 1 {
				t.Fatalf("len(choices) = %d, want 1", len(resp.Choices))
			}
			ch := resp.Choices[0]
			if tc.wantContent != "" && ch.Message.Content != tc.wantContent {
				t.Errorf("choices[0].message.content = %q, want %q", ch.Message.Content, tc.wantContent)
			}
			if tc.wantFinish != "" && ch.FinishReason != tc.wantFinish {
				t.Errorf("choices[0].finish_reason = %q, want %q", ch.FinishReason, tc.wantFinish)
			}
			if tc.wantPrompt != 0 && resp.Usage.PromptTokens != tc.wantPrompt {
				t.Errorf("usage.prompt_tokens = %d, want %d", resp.Usage.PromptTokens, tc.wantPrompt)
			}
			if tc.wantCompletion != 0 && resp.Usage.CompletionTokens != tc.wantCompletion {
				t.Errorf("usage.completion_tokens = %d, want %d", resp.Usage.CompletionTokens, tc.wantCompletion)
			}
			if tc.wantTotal != 0 && resp.Usage.TotalTokens != tc.wantTotal {
				t.Errorf("usage.total_tokens = %d, want %d", resp.Usage.TotalTokens, tc.wantTotal)
			}
		})
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
			name:    "data content_block_start is dropped",
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
			name:    "content_block_delta with non-text_delta type is dropped",
			line:    `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{}"}}`,
			wantNil: true,
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
			tc.checkFn(t, out)
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
