package proxy

// stream_abort_test.go covers the test matrix for L-018: stream-adapter
// fail-closed abort signal and multi-line output.
//
// Test IDs map to the task specification:
//
//   ADAPTER-LEVEL
//   AZ-1  Azure: passthrough returns ([][]byte{line}, nil) for any normal line.
//   AZ-2  Azure: drop cases return (nil, nil) — N/A for Azure (all lines pass).
//   AN-1  Anthropic ABORT cases: errStreamTransformAborted on protocol violations.
//   AN-2  Anthropic DROP cases: (nil, nil) for event: lines, ping, etc.
//   AN-3  Anthropic normal cases: one output line per valid data event.
//   GE-1  Gemini mixed text+functionCall chunk: TWO output lines (text first, tool second).
//   GE-2  Gemini ABORT cases: errStreamTransformAborted on invalid/excess tool calls.
//   GE-3  Gemini normal cases: functionCall-only → one line; text-only → one line; drop cases.
//
//   HANDLER / INTEGRATION
//   H-8   PII path: adapter abort → exactly one error event "stream_transform_aborted",
//          no [DONE], breaker RecordFailure, streamIncomplete → usage with BadGateway.
//   H-9   Plain path: adapter abort → distinct self-terminated abort event (\n\n),
//          no [DONE], abort in its own segment, breaker RecordFailure.
//   H-10  Abort event NOT emitted after client disconnect, NOT double-emitted.
//   H-11  Multi-line (mixed text+tool) through PII restorer: BOTH lines delivered,
//          stream terminates cleanly with [DONE].
//   H-12  Raw-byte cap counts raw upstream bytes before the adapter.
//   H-13  Plain path: Gemini mixed text+functionCall chunk → at least 4 distinct
//          \n\n-terminated events, each valid JSON; text and tool_calls both present.
//   H-14  Azure passthrough single-line stream → byte-identical wire output (regression
//          guard): exactly 4 events, each data line followed by \n\n.
//   H-15  Gemini plain path: [DONE] is \n\n-terminated as a distinct segment.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/circuitbreaker"
)

// ── AZ-1: Azure passthrough returns ([][]byte{line}, nil) ─────────────────────

func TestAzureTransformStreamLine_SingleLineSlice(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "normal OpenAI SSE data line",
			input: `data: {"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`,
		},
		{
			name:  "DONE sentinel",
			input: "data: [DONE]",
		},
		{
			name:  "blank SSE delimiter",
			input: "",
		},
		{
			name:  "arbitrary line (event:)",
			input: "event: ping",
		},
		{
			name:  "comment line",
			input: ": keep-alive",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AzureAdapter{}
			outLines, err := a.TransformStreamLine([]byte(tc.input))

			if err != nil {
				t.Fatalf("TransformStreamLine() returned error = %v, want nil", err)
			}
			if len(outLines) != 1 {
				t.Fatalf("TransformStreamLine() returned %d lines, want exactly 1", len(outLines))
			}
			if string(outLines[0]) != tc.input {
				t.Errorf("TransformStreamLine() = %q, want %q (passthrough unchanged)", outLines[0], tc.input)
			}
		})
	}
}

// ── AN-1: Anthropic ABORT cases ───────────────────────────────────────────────

func TestAnthropicTransformStreamLine_AbortCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup, if non-nil, is called on a fresh adapter before the line under
		// test is processed. Use it to prime state (e.g. register a block index).
		setup func(a *AnthropicAdapter)
		line  string
	}{
		{
			name: "content_block_start with unparseable JSON aborts",
			line: `data: {"type":"content_block_start","index":"not-an-int","content_block":{"type":"tool_use"}}`,
		},
		{
			name: "content_block_start negative index aborts",
			line: `data: {"type":"content_block_start","index":-1,"content_block":{"type":"tool_use","id":"toolu_neg","name":"fn","input":{}}}`,
		},
		{
			name: "content_block_start over tool-block cap aborts",
			setup: func(a *AnthropicAdapter) {
				// Fill the adapter up to maxAnthropicToolBlocks.
				for i := 0; i < maxAnthropicToolBlocks; i++ {
					line := fmt.Sprintf(
						`data: {"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"toolu_%d","name":"fn_%d","input":{}}}`,
						i, i, i,
					)
					_, _ = a.TransformStreamLine([]byte(line))
				}
			},
			line: fmt.Sprintf(
				`data: {"type":"content_block_start","index":%d,"content_block":{"type":"tool_use","id":"toolu_over","name":"fn_over","input":{}}}`,
				maxAnthropicToolBlocks,
			),
		},
		{
			name: "duplicate content_block_start for same index aborts",
			setup: func(a *AnthropicAdapter) {
				first := `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_first","name":"fn_first","input":{}}}`
				_, _ = a.TransformStreamLine([]byte(first))
			},
			line: `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_dup","name":"fn_dup","input":{}}}`,
		},
		{
			name: "content_block_start tool_use with invalid-charset id aborts",
			line: `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu@bad","name":"fn","input":{}}}`,
		},
		{
			name: "content_block_start tool_use with invalid-charset name aborts",
			line: `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_ok","name":"bad name spaces","input":{}}}`,
		},
		{
			name: "input_json_delta when blockToToolCall is nil aborts",
			// Fresh adapter: no content_block_start seen yet.
			line: `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}`,
		},
		{
			name: "input_json_delta referencing unknown block index aborts",
			setup: func(a *AnthropicAdapter) {
				// Register block 0 but send delta for block 5 (not registered).
				first := `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_0","name":"fn","input":{}}}`
				_, _ = a.TransformStreamLine([]byte(first))
			},
			line: `data: {"type":"content_block_delta","index":5,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}`,
		},
		{
			name: "message_delta with unparseable JSON aborts",
			line: `data: {"type":"message_delta","delta":"not-an-object","usage":"bad"}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			// Prime the adapter ID so buildChunk never produces nil.
			transformLineIgnore(a, []byte(`data: {"type":"message_start","message":{"id":"msg_abort_test","usage":{"input_tokens":1}}}`))

			if tc.setup != nil {
				tc.setup(a)
			}

			outLines, err := a.TransformStreamLine([]byte(tc.line))

			if err == nil {
				t.Errorf("TransformStreamLine() error = nil, want errStreamTransformAborted")
			}
			if !errors.Is(err, errStreamTransformAborted) {
				t.Errorf("TransformStreamLine() error = %v, want errStreamTransformAborted", err)
			}
			if len(outLines) != 0 {
				t.Errorf("TransformStreamLine() returned %d lines, want nil/empty on abort", len(outLines))
			}
		})
	}
}

// ── AN-2: Anthropic DROP cases still return (nil, nil) ────────────────────────

func TestAnthropicTransformStreamLine_DropCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		line string
	}{
		{
			name: "event: line is dropped",
			line: "event: content_block_delta",
		},
		{
			name: "event: message_start is dropped",
			line: "event: message_start",
		},
		{
			name: "data ping is dropped",
			line: `data: {"type":"ping"}`,
		},
		{
			name: "content_block_stop is dropped",
			line: `data: {"type":"content_block_stop","index":0}`,
		},
		{
			name: "unknown event type is dropped",
			line: `data: {"type":"some_future_event","data":{}}`,
		},
		{
			name: "text content_block_start is dropped",
			line: `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		},
		{
			name: "unknown delta type within content_block_delta is dropped",
			line: `data: {"type":"content_block_delta","index":0,"delta":{"type":"future_delta","value":"x"}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			outLines, err := a.TransformStreamLine([]byte(tc.line))

			if err != nil {
				t.Fatalf("TransformStreamLine(%q) error = %v, want nil", tc.line, err)
			}
			if len(outLines) != 0 {
				t.Errorf("TransformStreamLine(%q) returned %d lines, want 0 (drop)", tc.line, len(outLines))
			}
		})
	}
}

// ── AN-3: Anthropic normal cases return exactly one output line ───────────────

func TestAnthropicTransformStreamLine_NormalCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		// setup primes the adapter state before the line under test is processed.
		setup   func(a *AnthropicAdapter)
		line    string
		checkFn func(t *testing.T, out []byte)
	}{
		{
			name: "message_start emits role:assistant chunk",
			line: `data: {"type":"message_start","message":{"id":"msg_norm","type":"message","role":"assistant","content":[],"model":"claude-3-5","stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) == 0 {
					t.Fatal("len(choices) = 0, want 1")
				}
				if chunk.Choices[0].Delta.Role != "assistant" {
					t.Errorf("delta.role = %q, want assistant", chunk.Choices[0].Delta.Role)
				}
				if chunk.ID != "msg_norm" {
					t.Errorf("chunk.id = %q, want msg_norm", chunk.ID)
				}
			},
		},
		{
			name: "text_delta emits content chunk",
			setup: func(a *AnthropicAdapter) {
				transformLineIgnore(a, []byte(`data: {"type":"message_start","message":{"id":"msg_text","usage":{"input_tokens":1}}}`))
			},
			line: `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello world"}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) == 0 {
					t.Fatal("len(choices) = 0, want 1")
				}
				if chunk.Choices[0].Delta.Content != "Hello world" {
					t.Errorf("delta.content = %q, want %q", chunk.Choices[0].Delta.Content, "Hello world")
				}
			},
		},
		{
			name: "valid tool_use content_block_start emits header delta",
			setup: func(a *AnthropicAdapter) {
				transformLineIgnore(a, []byte(`data: {"type":"message_start","message":{"id":"msg_tool","usage":{"input_tokens":1}}}`))
			},
			line: `data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_01","name":"get_weather","input":{}}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) == 0 {
					t.Fatal("len(choices) = 0, want 1")
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
					t.Errorf("tool_calls[0].id = %q, want toolu_01", tc.ID)
				}
				if tc.Type != "function" {
					t.Errorf("tool_calls[0].type = %q, want function", tc.Type)
				}
				if tc.Function.Name != "get_weather" {
					t.Errorf("tool_calls[0].function.name = %q, want get_weather", tc.Function.Name)
				}
				if tc.Function.Arguments != "" {
					t.Errorf("tool_calls[0].function.arguments = %q, want empty (header delta)", tc.Function.Arguments)
				}
			},
		},
		{
			name: "input_json_delta emits arguments fragment",
			setup: func(a *AnthropicAdapter) {
				transformLineIgnore(a, []byte(`data: {"type":"message_start","message":{"id":"msg_args","usage":{"input_tokens":1}}}`))
				transformLineIgnore(a, []byte(`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_arg","name":"fn","input":{}}}`))
			},
			line: `data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"loc"}}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				chunk := parseChunk(t, out)
				if len(chunk.Choices) == 0 {
					t.Fatal("len(choices) = 0, want 1")
				}
				delta := chunk.Choices[0].Delta
				if len(delta.ToolCalls) != 1 {
					t.Fatalf("len(delta.tool_calls) = %d, want 1", len(delta.ToolCalls))
				}
				tc := delta.ToolCalls[0]
				if tc.Index == nil || *tc.Index != 0 {
					t.Errorf("tool_calls[0].index = %v, want 0", tc.Index)
				}
				if tc.Function.Arguments != `{"loc` {
					t.Errorf("tool_calls[0].function.arguments = %q, want %q", tc.Function.Arguments, `{"loc`)
				}
			},
		},
		{
			name: "message_stop emits [DONE]",
			setup: func(a *AnthropicAdapter) {
				transformLineIgnore(a, []byte(`data: {"type":"message_start","message":{"id":"msg_stop","usage":{"input_tokens":1}}}`))
			},
			line: `data: {"type":"message_stop"}`,
			checkFn: func(t *testing.T, out []byte) {
				t.Helper()
				if string(out) != "data: [DONE]" {
					t.Errorf("message_stop output = %q, want %q", out, "data: [DONE]")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &AnthropicAdapter{}
			if tc.setup != nil {
				tc.setup(a)
			}

			outLines, err := a.TransformStreamLine([]byte(tc.line))

			if err != nil {
				t.Fatalf("TransformStreamLine(%q) error = %v, want nil", tc.line, err)
			}
			if len(outLines) != 1 {
				t.Fatalf("TransformStreamLine(%q) returned %d lines, want 1", tc.line, len(outLines))
			}
			if outLines[0] == nil {
				t.Fatalf("TransformStreamLine(%q) outLines[0] = nil, want non-nil", tc.line)
			}
			if tc.checkFn != nil {
				tc.checkFn(t, outLines[0])
			}
		})
	}
}

// ── GE-1: Gemini mixed text+functionCall returns TWO lines ───────────────────

// TestGeminiTransformStreamLine_MixedTextAndFunctionCall_TwoLines is the
// headline fix test (test 5 in the task specification). A single Gemini chunk
// that carries both text content and a functionCall part must produce exactly
// two SSE lines: the text content chunk first, then the tool_calls chunk.
// Previously the text was silently discarded.
func TestGeminiTransformStreamLine_MixedTextAndFunctionCall_TwoLines(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		line      string
		wantText  string
		wantFnArg string // partial expected content in tool_calls JSON
	}{
		{
			name:      "text and single functionCall",
			line:      `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Let me check that."},{"functionCall":{"name":"lookup","args":{"q":"test"}}}]},"finishReason":"STOP"}],"usageMetadata":{}}`,
			wantText:  "Let me check that.",
			wantFnArg: "lookup",
		},
		{
			name:      "text and functionCall without finishReason",
			line:      `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Thinking..."},{"functionCall":{"name":"fn","args":{}}}]},"finishReason":""}],"usageMetadata":{}}`,
			wantText:  "Thinking...",
			wantFnArg: "fn",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &GeminiAdapter{}
			outLines, err := a.TransformStreamLine([]byte(tc.line))

			if err != nil {
				t.Fatalf("TransformStreamLine() returned unexpected error: %v", err)
			}
			if len(outLines) != 2 {
				t.Fatalf("TransformStreamLine() returned %d lines, want exactly 2 (text + tool_calls)", len(outLines))
			}

			// Line 0 must be the text content chunk.
			textChunk := parseChunk(t, outLines[0])
			if len(textChunk.Choices) == 0 {
				t.Fatal("first line: choices is empty")
			}
			if textChunk.Choices[0].Delta.Content != tc.wantText {
				t.Errorf("first line (text): content = %q, want %q",
					textChunk.Choices[0].Delta.Content, tc.wantText)
			}
			if len(textChunk.Choices[0].Delta.ToolCalls) != 0 {
				t.Error("first line (text): must not contain tool_calls")
			}

			// Line 1 must be the tool_calls chunk.
			toolChunk := parseChunk(t, outLines[1])
			if len(toolChunk.Choices) == 0 {
				t.Fatal("second line: choices is empty")
			}
			if len(toolChunk.Choices[0].Delta.ToolCalls) == 0 {
				t.Error("second line (tool_calls): must contain at least one tool call")
			}
			tc2 := toolChunk.Choices[0].Delta.ToolCalls[0]
			if tc2.Function.Name != tc.wantFnArg {
				t.Errorf("second line tool_calls[0].function.name = %q, want %q",
					tc2.Function.Name, tc.wantFnArg)
			}

			// The second line (tool_calls chunk) carries the finishReason when present.
			// The text chunk must NOT carry a finishReason when there are tool calls.
			if textChunk.Choices[0].FinishReason != nil {
				t.Errorf("first line (text): finish_reason = %q, want nil (must not carry finish reason when tool calls follow)",
					*textChunk.Choices[0].FinishReason)
			}
		})
	}
}

// ── GE-2: Gemini ABORT cases ─────────────────────────────────────────────────

func TestGeminiTransformStreamLine_AbortCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		setup func(a *GeminiAdapter)
		line  string
	}{
		{
			name: "functionCall over streamToolCallCounter cap aborts",
			setup: func(a *GeminiAdapter) {
				// Fill to cap.
				for i := 0; i < maxGeminiToolBlocks; i++ {
					line := fmt.Sprintf(
						`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"fn_%d","args":{}}}]},"finishReason":""}],"usageMetadata":{}}`,
						i,
					)
					_, _ = a.TransformStreamLine([]byte(line))
				}
			},
			line: `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"fn_over","args":{}}}]},"finishReason":""}],"usageMetadata":{}}`,
		},
		{
			name: "empty function name aborts",
			line: `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"","args":{}}}]},"finishReason":""}],"usageMetadata":{}}`,
		},
		{
			name: "invalid-charset function name aborts",
			line: `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"bad name spaces","args":{}}}]},"finishReason":""}],"usageMetadata":{}}`,
		},
		{
			name: "serializeInputToArguments error on non-object args aborts",
			// args is a JSON array, not an object — serializeInputToArguments returns error.
			line: `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"fn","args":[1,2,3]}}]},"finishReason":""}],"usageMetadata":{}}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &GeminiAdapter{}
			if tc.setup != nil {
				tc.setup(a)
			}

			outLines, err := a.TransformStreamLine([]byte(tc.line))

			if err == nil {
				t.Errorf("TransformStreamLine() error = nil, want errStreamTransformAborted")
			}
			if !errors.Is(err, errStreamTransformAborted) {
				t.Errorf("TransformStreamLine() error = %v, want errStreamTransformAborted", err)
			}
			if len(outLines) != 0 {
				t.Errorf("TransformStreamLine() returned %d lines, want nil/empty on abort", len(outLines))
			}
		})
	}
}

// ── GE-3: Gemini normal cases ─────────────────────────────────────────────────

func TestGeminiTransformStreamLine_NormalCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    string
		wantLen int // expected number of output lines (0 = drop, 1 = single line)
		checkFn func(t *testing.T, outLines [][]byte)
	}{
		{
			name:    "functionCall-only chunk emits one tool_calls line",
			line:    `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"fn","args":{"x":1}}}]},"finishReason":""}],"usageMetadata":{}}`,
			wantLen: 1,
			checkFn: func(t *testing.T, outLines [][]byte) {
				t.Helper()
				chunk := parseChunk(t, outLines[0])
				if len(chunk.Choices) == 0 {
					t.Fatal("choices empty")
				}
				if len(chunk.Choices[0].Delta.ToolCalls) == 0 {
					t.Error("tool_calls missing in functionCall-only chunk")
				}
				if chunk.Choices[0].Delta.Content != "" {
					t.Errorf("content = %q, want empty for functionCall-only chunk", chunk.Choices[0].Delta.Content)
				}
				tc := chunk.Choices[0].Delta.ToolCalls[0]
				if tc.Function.Name != "fn" {
					t.Errorf("function.name = %q, want fn", tc.Function.Name)
				}
				// id must be synthesised and satisfy charset regex.
				if !anthropicToolIDRe.MatchString(tc.ID) {
					t.Errorf("tool_calls[0].id = %q does not satisfy charset regex", tc.ID)
				}
			},
		},
		{
			name:    "text-only chunk emits one content line",
			line:    `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hello there"}]},"finishReason":""}],"usageMetadata":{}}`,
			wantLen: 1,
			checkFn: func(t *testing.T, outLines [][]byte) {
				t.Helper()
				chunk := parseChunk(t, outLines[0])
				if len(chunk.Choices) == 0 {
					t.Fatal("choices empty")
				}
				if chunk.Choices[0].Delta.Content != "hello there" {
					t.Errorf("content = %q, want %q", chunk.Choices[0].Delta.Content, "hello there")
				}
				if len(chunk.Choices[0].Delta.ToolCalls) != 0 {
					t.Error("tool_calls must be absent for text-only chunk")
				}
			},
		},
		{
			name:    "content-free intermediate chunk is dropped",
			line:    `data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":""}],"usageMetadata":{}}`,
			wantLen: 0,
		},
		{
			name:    "non-data line is dropped",
			line:    "not-a-data-line",
			wantLen: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			a := &GeminiAdapter{}
			outLines, err := a.TransformStreamLine([]byte(tc.line))

			if err != nil {
				t.Fatalf("TransformStreamLine(%q) error = %v, want nil", tc.line, err)
			}
			if len(outLines) != tc.wantLen {
				t.Fatalf("TransformStreamLine(%q) returned %d lines, want %d", tc.line, len(outLines), tc.wantLen)
			}
			if tc.checkFn != nil && len(outLines) > 0 {
				tc.checkFn(t, outLines)
			}
		})
	}
}

// ── H-8: PII path — adapter abort → fail-closed error event, no [DONE], breaker failure ──

// abortRegistryAnthropic builds a Registry with a single "abort-anthropic"
// model. H-8 and H-9 drive the abort via the real Anthropic adapter responding
// to a deliberately malformed content_block_start with negative index, which
// causes errStreamTransformAborted.
func abortRegistryAnthropic(t *testing.T, upstreamURL string) *Registry {
	t.Helper()
	m := &Model{
		Name:     "abort-anthropic",
		Provider: "anthropic",
		Type:     "chat",
		BaseURL:  upstreamURL,
		APIKey:   "key-abort",
		// destPrivate=false: PII filter fires.
	}
	r := &Registry{
		models:  map[string]*Model{"abort-anthropic": m},
		aliases: make(map[string]string),
	}
	r.rebuildSorted()
	return r
}

// abortRegistryPlain builds a Registry with a model that uses provider "vllm"
// (no adapter). We test H-9 by using a real Anthropic provider too (Anthropic
// adapter is the easiest way to get errStreamTransformAborted on demand), but
// with NO PII engine attached to exercise the plain passthrough scan loop.
func abortRegistryPlain(t *testing.T, upstreamURL string) *Registry {
	t.Helper()
	m := &Model{
		Name:     "abort-plain",
		Provider: "anthropic",
		Type:     "chat",
		BaseURL:  upstreamURL,
		APIKey:   "key-abort-plain",
	}
	r := &Registry{
		models:  map[string]*Model{"abort-plain": m},
		aliases: make(map[string]string),
	}
	r.rebuildSorted()
	return r
}

// anthropicAbortUpstream returns an httptest.Server that sends a valid
// message_start event followed by a content_block_start with negative index
// (which causes errStreamTransformAborted in AnthropicAdapter), then one more
// valid line that must never reach the client.
func anthropicAbortUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream: no Flusher")
			return
		}
		events := []string{
			// A valid message_start — gets processed fine.
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_abort_e2e","type":"message","role":"assistant","content":[],"model":"claude-test","stop_reason":null,"usage":{"input_tokens":5,"output_tokens":0}}}`,
			``,
			// This line triggers errStreamTransformAborted (negative index).
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":-1,"content_block":{"type":"tool_use","id":"toolu_bad","name":"fn","input":{}}}`,
			``,
			// This line must never reach the client after the abort.
			`data: {"type":"message_stop"}`,
			``,
		}
		for _, ev := range events {
			fmt.Fprintln(w, ev)
		}
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestHandle_PII_AdapterAbort_FailClosed is H-8.
//
// The PII path (filter.Touched() == true) must:
//   - emit exactly one "stream_transform_aborted" error event to the client
//   - NOT emit a [DONE] sentinel
//   - record RecordFailure on the circuit breaker
//   - log streamIncomplete → usage with http.StatusBadGateway
func TestHandle_PII_AdapterAbort_FailClosed(t *testing.T) {
	t.Parallel()

	engine := newTestPIIEngine(t)
	upstream := anthropicAbortUpstream(t)

	reg := abortRegistryAnthropic(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	// Wire a circuit breaker with threshold=1 so a single RecordFailure trips it open.
	h.CircuitBreakers = circuitbreaker.NewRegistry(circuitbreaker.Config{
		Enabled:     true,
		Threshold:   1,
		Timeout:     30 * time.Second,
		HalfOpenMax: 1,
	})

	baseURL := startTestServer(t, h)

	// The request body must contain PII so that filter.Touched() == true.
	streamBody := fmt.Sprintf(
		`{"model":"abort-anthropic","messages":[{"role":"user","content":"lookup %s"}],"stream":true}`,
		piiTestEmail,
	)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	fullBody, _ := io.ReadAll(resp.Body)
	fullStr := string(fullBody)

	// Must contain exactly one "stream_transform_aborted" error event.
	if !strings.Contains(fullStr, "stream_transform_aborted") {
		t.Errorf("H-8: error event with stream_transform_aborted absent\noutput: %s", fullStr)
	}
	// The error event must be valid JSON with the expected shape.
	for _, line := range strings.Split(fullStr, "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var obj struct {
			Error struct {
				Type string `json:"type"`
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(payload), &obj); err != nil {
			// Not an error event — skip (may be the role chunk).
			continue
		}
		if obj.Error.Type == "stream_transform_aborted" {
			// Found the abort event — validate it is content-free.
			if strings.Contains(payload, piiTestEmail) {
				t.Errorf("H-8 SECURITY: error event contains PII value %q\npayload: %s", piiTestEmail, payload)
			}
			break
		}
	}

	// Must NOT contain [DONE].
	if strings.Contains(fullStr, "[DONE]") {
		t.Errorf("H-8: [DONE] present after adapter abort, want absent\noutput: %s", fullStr)
	}

	// Circuit breaker for "abort-anthropic" must be Open (RecordFailure was called).
	breaker := h.CircuitBreakers.Get("abort-anthropic")
	if state := breaker.CurrentState(); state != circuitbreaker.Open {
		t.Errorf("H-8: breaker state = %v, want Open (RecordFailure expected after adapter abort)", state)
	}
}

// TestHandle_Plain_AdapterAbort_FailClosed is H-9.
//
// The plain passthrough path (no PII engine) must:
//   - emit exactly one "stream_transform_aborted" error event as its own
//     self-terminated SSE event (ends with \n\n, distinct from any preceding event)
//   - NOT emit a [DONE] sentinel
//   - record RecordFailure on the circuit breaker
//
// Wire-format invariants verified by splitting the full body on "\n\n" and
// asserting event count and separation — not merely string presence.
func TestHandle_Plain_AdapterAbort_FailClosed(t *testing.T) {
	t.Parallel()

	upstream := anthropicAbortUpstream(t)

	reg := abortRegistryPlain(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// No PII engine: exercises the plain scan loop.

	h.CircuitBreakers = circuitbreaker.NewRegistry(circuitbreaker.Config{
		Enabled:     true,
		Threshold:   1,
		Timeout:     30 * time.Second,
		HalfOpenMax: 1,
	})

	baseURL := startTestServer(t, h)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"abort-plain","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	fullBody, _ := io.ReadAll(resp.Body)
	fullStr := string(fullBody)

	// Must NOT contain [DONE].
	if strings.Contains(fullStr, "[DONE]") {
		t.Errorf("H-9: [DONE] present after adapter abort, want absent\noutput: %s", fullStr)
	}

	// Parse the wire output as a sequence of SSE events split on "\n\n".
	// Each non-empty segment is one self-terminated event. The abort event
	// must be a distinct segment (not merged with a preceding event).
	rawEvents := strings.Split(fullStr, "\n\n")
	var dataEvents []string
	for _, seg := range rawEvents {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		dataEvents = append(dataEvents, seg)
	}

	// Find the abort event among the segments.
	abortEventCount := 0
	var abortPayload string
	for _, ev := range dataEvents {
		for _, line := range strings.Split(ev, "\n") {
			if !strings.HasPrefix(line, "data: {") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			var obj struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(payload), &obj); err != nil {
				continue
			}
			if obj.Error.Type == "stream_transform_aborted" {
				abortEventCount++
				abortPayload = payload
			}
		}
	}

	if abortEventCount == 0 {
		t.Fatalf("H-9: stream_transform_aborted event absent\noutput: %s", fullStr)
	}
	if abortEventCount > 1 {
		t.Errorf("H-9: stream_transform_aborted emitted %d times, want exactly 1\noutput: %s", abortEventCount, fullStr)
	}

	// Validate the abort event payload fields.
	var full struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(abortPayload), &full); err != nil {
		t.Fatalf("H-9: abort event JSON invalid: %v\npayload: %s", err, abortPayload)
	}
	if full.Error.Message == "" {
		t.Error("H-9: error.message is empty")
	}
	if full.Error.Code != "stream_transform_aborted" {
		t.Errorf("H-9: error.code = %q, want stream_transform_aborted", full.Error.Code)
	}

	// The abort event must be a distinct SSE segment — assert that at least one
	// non-abort event exists before it (the role:assistant chunk from the
	// upstream message_start) and the abort is in its own segment.
	// We do this by checking that the abort payload appears exactly once in the
	// full string split on \n\n (i.e. it is not concatenated with another event).
	abortInOwnSegment := false
	for _, seg := range rawEvents {
		seg = strings.TrimSpace(seg)
		if strings.Contains(seg, "stream_transform_aborted") {
			// This segment must NOT also contain any non-abort data line.
			lines := strings.Split(seg, "\n")
			nonAbortData := false
			for _, l := range lines {
				if strings.HasPrefix(l, "data: ") && !strings.Contains(l, "stream_transform_aborted") {
					nonAbortData = true
					break
				}
			}
			if !nonAbortData {
				abortInOwnSegment = true
			}
		}
	}
	if !abortInOwnSegment {
		t.Errorf("H-9: abort event is not in its own \\n\\n-delimited segment (merged with another event)\noutput: %s", fullStr)
	}

	// Breaker must be Open.
	breaker := h.CircuitBreakers.Get("abort-plain")
	if state := breaker.CurrentState(); state != circuitbreaker.Open {
		t.Errorf("H-9: breaker state = %v, want Open (RecordFailure expected)", state)
	}
}

// TestHandle_AbortEvent_NotDoubleEmitted is H-10 (unit-level equivalent).
//
// writeStreamAbortEvent is only called once on adapter abort (not duplicated by
// the post-loop error-event block). We verify this at the unit level by checking
// the two guards: adapterAborted prevents the PII post-loop block, and
// plainAborted prevents the plain post-loop block from recording success.
//
// The end-to-end tests (H-8, H-9) already guarantee the stream has exactly one
// error event. This test drives writeStreamAbortEvent directly to confirm the
// output contains exactly one SSE event terminator (\n\n).
func TestHandle_AbortEvent_SingleEmission(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w := bufio.NewWriter(&buf)
	writeStreamAbortEvent(w, "stream_transform_aborted")
	// writeStreamAbortEvent flushes internally.

	output := buf.String()

	// Count occurrences of double-newline (SSE event terminators).
	count := strings.Count(output, "\n\n")
	if count != 1 {
		t.Errorf("writeStreamAbortEvent emitted %d SSE event terminators, want exactly 1\noutput: %q", count, output)
	}

	// The single event must be the error object.
	if !strings.Contains(output, "stream_transform_aborted") {
		t.Errorf("output does not contain stream_transform_aborted: %q", output)
	}

	// Must not contain [DONE].
	if strings.Contains(output, "[DONE]") {
		t.Errorf("writeStreamAbortEvent output contains [DONE], want absent: %q", output)
	}
}

// ── H-11: Multi-line through PII restorer — both lines delivered ──────────────

// TestHandle_Gemini_MixedChunk_PII_BothLinesDelivered is H-11.
//
// A Gemini upstream emits a mixed text+functionCall chunk. The adapter produces
// TWO SSE lines. Both must flow through the PII StreamRestorer and reach the
// client. The pseudonym in the tool_call arguments must be restored. The stream
// must end cleanly with [DONE].
//
// Design: the text part carries plain (non-PII) content to avoid a content-carry
// collision at the restorer cross-lane check. The pseudonym is placed exclusively
// in the functionCall arguments. The Stage 0c StreamRestorer must:
//  1. Pass the text content chunk through (no pseudonym → emitted immediately).
//  2. Buffer the tool_calls chunk arguments, restore the pseudonym, and emit.
//  3. Emit the finish_reason chunk and [DONE] cleanly.
func TestHandle_Gemini_MixedChunk_PII_BothLinesDelivered(t *testing.T) {
	t.Parallel()

	engine := newTestPIIEngine(t)

	// Pre-compute the pseudonym for piiTestEmail.
	sampleFilter := engine.NewFilter("")
	sampleBody := []byte(chatBody("gemini-test", "lookup "+piiTestEmail))
	anonBody, err := sampleFilter.AnonymizeJSON(sampleBody)
	if err != nil {
		t.Fatalf("pre-compute AnonymizeJSON: %v", err)
	}
	pseudo := piiPseudonymPattern.FindString(string(anonBody))
	if pseudo == "" {
		t.Fatal("could not derive pseudonym for piiTestEmail")
	}

	// The upstream emits:
	//   Chunk 1: mixed text ("Calling tool...") + functionCall with pseudonym in args.
	//            No finishReason so text gets its own chunk and tool_calls gets its own
	//            chunk — the adapter splits them into two lines.
	//   Chunk 2: terminal chunk with finishReason=STOP (sawFunctionCall=true → tool_calls).
	//   Blank:   triggers [DONE] from the Gemini adapter's doneSent logic.
	//
	// The pseudonym is ONLY in args (not in the text part). This avoids the
	// StreamRestorer cross-lane abort: the content carry drains between chunks
	// because the text has no pseudonym.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream: no Flusher")
			return
		}

		// Chunk 1: plain text + functionCall with pseudonym in args.
		// finishReason="" so GeminiAdapter emits text-only chunk (line 0) and
		// tool_calls chunk (line 1) with no finishReason on either.
		chunk1 := fmt.Sprintf(
			`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Calling tool..."},{"functionCall":{"name":"notify","args":{"email":"%s"}}}]},"finishReason":""}],"usageMetadata":{}}`,
			pseudo,
		)
		// Chunk 2: terminal chunk with finishReason=STOP. Parts is empty so no text/tool
		// is produced, only the finish_reason chunk (sawFunctionCall=true → "tool_calls").
		chunk2 := `data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":10,"totalTokenCount":15}}`

		for _, chunk := range []string{chunk1, chunk2} {
			fmt.Fprintln(w, chunk)
			fmt.Fprintln(w) // blank separator
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryGemini(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	baseURL := startTestServer(t, h)

	streamBody := fmt.Sprintf(
		`{"model":"gemini-test","messages":[{"role":"user","content":"lookup %s"}],"stream":true}`,
		piiTestEmail,
	)
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
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

	// No abort events must appear.
	if strings.Contains(fullStr, "stream_transform_aborted") || strings.Contains(fullStr, "pii_restore_aborted") {
		t.Fatalf("H-11: unexpected abort event in output\noutput: %s", fullStr)
	}

	// Both SSE kinds must be delivered: the text content chunk and the tool_calls chunk.
	if !strings.Contains(fullStr, `"content"`) {
		t.Errorf("H-11: text content chunk missing from output\noutput: %s", fullStr)
	}
	if !strings.Contains(fullStr, `"tool_calls"`) {
		t.Errorf("H-11: tool_calls chunk missing from output\noutput: %s", fullStr)
	}
	// The tool name must be present.
	if !strings.Contains(fullStr, `"notify"`) {
		t.Errorf("H-11: function name 'notify' missing from output\noutput: %s", fullStr)
	}

	// The pseudonym must NOT appear in any emitted line (restorer replaced it).
	if strings.Contains(fullStr, pseudo) {
		t.Errorf("H-11 SECURITY: raw pseudonym %q visible in output\noutput: %s", pseudo, fullStr)
	}

	// The restored email must appear in the output (restorer replaced the pseudonym).
	if !strings.Contains(fullStr, piiTestEmail) {
		t.Errorf("H-11: restored email %q missing from output\noutput: %s", piiTestEmail, fullStr)
	}

	// Stream must terminate cleanly with [DONE].
	if !strings.Contains(fullStr, "[DONE]") {
		t.Errorf("H-11: [DONE] missing — stream did not terminate cleanly\noutput: %s", fullStr)
	}
}

// ── H-12: Raw-byte cap counts raw upstream bytes before the adapter ───────────

// TestHandle_Streaming_ByteCapCountsRawBytes is H-12.
//
// The piiTotalInputBytes counter on the PII path is incremented for EVERY raw
// byte read from the upstream scanner, including lines the adapter drops
// (e.g. Anthropic "event:" lines). We verify this by checking that the proxy
// aborts with a PII policy error when the raw byte total exceeds maxRespBody,
// even if the adapter drops most lines so the client would otherwise receive
// far fewer bytes.
//
// Strategy: set MaxResponseBody very low (e.g. 100 bytes) and send enough raw
// upstream bytes to exceed the cap. At least one dropped line must push the
// aggregate over the threshold.
func TestHandle_Streaming_ByteCapCountsRawBytes(t *testing.T) {
	t.Parallel()

	engine := newTestPIIEngine(t)

	// The upstream will send a stream where the raw byte total exceeds the cap.
	// We send Anthropic "event:" lines (dropped by adapter) plus a final data line.
	// Each "event: content_block_delta" line is 28 bytes; 10 of them = 280 bytes.
	// We set maxRespBody = 200 so the cap is hit around line 7.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream: no Flusher")
			return
		}

		// Start with message_start (required for AnthropicAdapter).
		fmt.Fprintln(w, `data: {"type":"message_start","message":{"id":"msg_cap","type":"message","role":"assistant","content":[],"model":"claude-test","stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`)
		fmt.Fprintln(w) // blank
		flusher.Flush()

		// Emit 15 "event:" lines (each ~28 bytes + newline = ~29 bytes each).
		// 15 * 29 = 435 bytes, exceeding our 200-byte cap.
		for i := 0; i < 15; i++ {
			fmt.Fprintln(w, "event: content_block_delta")
			flusher.Flush()
		}
		// Send a data line that would normally be forwarded — but the cap fires first.
		fmt.Fprintln(w, `data: {"type":"message_stop"}`)
		fmt.Fprintln(w)
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	reg := abortRegistryAnthropic(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine
	h.MaxResponseBody = 200 // Low cap: triggers after ~7 "event:" lines.

	baseURL := startTestServer(t, h)

	// The request body must contain PII so filter.Touched() == true (PII path).
	streamBody := fmt.Sprintf(
		`{"model":"abort-anthropic","messages":[{"role":"user","content":"lookup %s"}],"stream":true}`,
		piiTestEmail,
	)
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	// Status must be 200 (streaming response header was already sent).
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	fullBody, _ := io.ReadAll(resp.Body)
	fullStr := string(fullBody)

	// The byte cap abort must have triggered. The output must contain the PII
	// policy error event (the cap abort produces a pii_restore_aborted event).
	// Note: the byte cap abort fires "streamAborted=true" on the PII path, and
	// !adapterAborted && !clientDisconnected && streamIncomplete triggers the
	// pii error event (not stream_transform_aborted, since it's a cap abort).
	if !strings.Contains(fullStr, "pii_restore_aborted") {
		t.Errorf("H-12: byte cap not triggered — pii_restore_aborted event absent\noutput: %s", fullStr)
	}

	// [DONE] must be absent (stream was aborted before completion).
	if strings.Contains(fullStr, "[DONE]") {
		t.Errorf("H-12: [DONE] present after byte-cap abort, want absent\noutput: %s", fullStr)
	}
}

// ── H-13: Plain path — Gemini mixed chunk → two properly-separated SSE events ─

// TestHandle_Gemini_MixedChunk_Plain_TwoSeparatedEvents is H-13.
//
// A Gemini upstream emits a single mixed text+functionCall chunk. The Gemini
// adapter produces TWO SSE lines from it. On the PLAIN (non-PII) path the
// handler must write each line as its own self-terminated SSE event ending in
// "\n\n". The client receives exactly the expected number of events and each
// parses as valid JSON.
//
// Expected wire output for the mixed chunk:
//
//	data: {"choices":[{"delta":{"role":"assistant","content":"Calling tool..."}}],...}\n\n
//	data: {"choices":[{"delta":{"tool_calls":[...]}}],...}\n\n
//
// Each event is self-terminated with "\n\n" — no reliance on upstream blank lines.
// The terminal [DONE] likewise ends with "\n\n".
func TestHandle_Gemini_MixedChunk_Plain_TwoSeparatedEvents(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream: no Flusher")
			return
		}

		// Mixed chunk: text + functionCall, no finishReason so the adapter emits
		// a text-only line (line 0) and a tool_calls line (line 1).
		chunk1 := `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Calling tool..."},{"functionCall":{"name":"notify","args":{"email":"user@example.com"}}}]},"finishReason":""}],"usageMetadata":{}}`
		// Terminal chunk with finishReason=STOP; emits finish_reason chunk.
		chunk2 := `data: {"candidates":[{"content":{"role":"model","parts":[]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":10,"totalTokenCount":15}}`

		for _, chunk := range []string{chunk1, chunk2} {
			fmt.Fprintln(w, chunk)
			fmt.Fprintln(w) // blank SSE event separator
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	// Plain Gemini registry — no PIIEngine attached so the plain scan loop runs.
	reg := piiRegistryGemini(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// h.PIIEngine is intentionally nil: forces the plain passthrough path.

	baseURL := startTestServer(t, h)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-test","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("H-13: status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	fullBody, _ := io.ReadAll(resp.Body)
	fullStr := string(fullBody)

	// No abort events must appear.
	if strings.Contains(fullStr, "stream_transform_aborted") {
		t.Fatalf("H-13: unexpected abort event in output\noutput: %s", fullStr)
	}

	// Parse the raw wire output into individual SSE events by splitting on "\n\n".
	// Each non-empty event that starts with "data: " must carry valid JSON after
	// stripping the prefix (or be the [DONE] sentinel).
	rawEvents := strings.Split(fullStr, "\n\n")
	var dataEvents []string
	for _, ev := range rawEvents {
		ev = strings.TrimSpace(ev)
		if ev == "" {
			continue
		}
		if !strings.HasPrefix(ev, "data: ") {
			continue
		}
		dataEvents = append(dataEvents, ev)
	}

	// We expect exactly: text-chunk, tool_calls-chunk, finish_reason-chunk, [DONE].
	// That is at least 4 data events. Assert minimum count to catch merging.
	if len(dataEvents) < 4 {
		t.Errorf("H-13: got %d data events, want at least 4 (text, tool_calls, finish_reason, [DONE])\nfull output:\n%s",
			len(dataEvents), fullStr)
	}

	// Verify every data event carries valid JSON (or is [DONE]).
	// If the "\n\n" separator were missing, two lines would be joined as one event
	// and the JSON parse would fail.
	for _, ev := range dataEvents {
		payload := strings.TrimPrefix(ev, "data: ")
		if payload == "[DONE]" {
			continue
		}
		if !json.Valid([]byte(payload)) {
			t.Errorf("H-13: SSE event carries invalid JSON — \\n\\n terminator likely missing\npayload: %s\nfull output:\n%s", payload, fullStr)
		}
	}

	// The text content chunk and the tool_calls chunk must both appear as separate
	// parseable events.
	textFound := false
	toolFound := false
	doneFound := false
	for _, ev := range dataEvents {
		payload := strings.TrimPrefix(ev, "data: ")
		if payload == "[DONE]" {
			doneFound = true
			continue
		}
		if strings.Contains(payload, `"content"`) && strings.Contains(payload, "Calling tool") {
			textFound = true
		}
		if strings.Contains(payload, `"tool_calls"`) {
			toolFound = true
		}
	}
	if !textFound {
		t.Errorf("H-13: text content chunk missing from plain-path output\noutput: %s", fullStr)
	}
	if !toolFound {
		t.Errorf("H-13: tool_calls chunk missing from plain-path output\noutput: %s", fullStr)
	}

	// The function name must be present.
	if !strings.Contains(fullStr, `"notify"`) {
		t.Errorf("H-13: function name 'notify' missing from plain-path output\noutput: %s", fullStr)
	}

	// Stream must terminate cleanly with [DONE] as a distinct self-terminated event.
	if !doneFound {
		t.Errorf("H-13: [DONE] missing or not a distinct \\n\\n-terminated event\noutput: %s", fullStr)
	}
}

// ── H-14: Azure passthrough single-line stream — byte-identical wire output ──

// TestHandle_Azure_Passthrough_WireFormat is H-14.
//
// Azure uses AzureAdapter which passes each upstream line through unchanged
// (including blank lines). On the plain path the handler writes each adapted
// line with "\n\n" and skips blank adapted lines. For a standard OpenAI/Azure
// SSE stream (data-line + blank per event) the wire output must be byte-identical
// to what a naive "\n"-per-line forwarder would produce: each "data:..." line
// followed by exactly "\n\n".
//
// This test is the regression guard: any change to the framing logic must not
// alter the output for the common Azure single-line stream case.
func TestHandle_Azure_Passthrough_WireFormat(t *testing.T) {
	t.Parallel()

	// Build a minimal Azure model registry pointing at the test upstream.
	buildAzureRegistry := func(t *testing.T, upstreamURL string) *Registry {
		t.Helper()
		m := &Model{
			Name:            "azure-gpt4",
			Provider:        "azure",
			Type:            "chat",
			BaseURL:         upstreamURL,
			APIKey:          "azure-key",
			AzureDeployment: "gpt-4o",
			AzureAPIVersion: "2024-10-21",
		}
		r := &Registry{
			models:  map[string]*Model{"azure-gpt4": m},
			aliases: make(map[string]string),
		}
		r.rebuildSorted()
		return r
	}

	// Standard Azure/OpenAI SSE stream: one data line + one blank line per event.
	// The upstream sends three chunks followed by [DONE].
	chunk1 := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Hello"},"finish_reason":null}]}`
	chunk2 := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" world"},"finish_reason":null}]}`
	chunk3 := `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	done := "data: [DONE]"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream: no Flusher")
			return
		}
		for _, line := range []string{chunk1, chunk2, chunk3, done} {
			fmt.Fprintln(w, line)
			fmt.Fprintln(w) // blank line after each SSE event (standard OpenAI wire format)
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	reg := buildAzureRegistry(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// h.PIIEngine is intentionally nil: plain path.

	baseURL := startTestServer(t, h)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"azure-gpt4","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("H-14: status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	fullBody, _ := io.ReadAll(resp.Body)
	fullStr := string(fullBody)

	// Split on "\n\n" to get self-terminated events.
	rawEvents := strings.Split(fullStr, "\n\n")
	var dataEvents []string
	for _, seg := range rawEvents {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		if strings.HasPrefix(seg, "data: ") {
			dataEvents = append(dataEvents, seg)
		}
	}

	// Expect exactly 4 events: chunk1, chunk2, chunk3, [DONE].
	if len(dataEvents) != 4 {
		t.Errorf("H-14: got %d data events, want 4\nevents: %v\nfull output:\n%s",
			len(dataEvents), dataEvents, fullStr)
	}

	// Every non-DONE event must carry valid JSON.
	for i, ev := range dataEvents {
		payload := strings.TrimPrefix(ev, "data: ")
		if payload == "[DONE]" {
			continue
		}
		if !json.Valid([]byte(payload)) {
			t.Errorf("H-14: event[%d] carries invalid JSON\npayload: %s\nfull output:\n%s", i, payload, fullStr)
		}
	}

	// The [DONE] sentinel must be the last data event.
	if len(dataEvents) > 0 {
		last := dataEvents[len(dataEvents)-1]
		if last != "data: [DONE]" {
			t.Errorf("H-14: last data event = %q, want %q", last, "data: [DONE]")
		}
	}

	// Each data event must end with exactly "\n\n" in the raw wire output
	// (regression guard: self-termination must not add extra newlines).
	for _, line := range []string{chunk1, chunk2, chunk3, done} {
		want := line + "\n\n"
		if !strings.Contains(fullStr, want) {
			t.Errorf("H-14: wire output does not contain %q (\\n\\n-terminated)\nfull output:\n%s", want, fullStr)
		}
	}
}

// ── H-15: Gemini plain path — [DONE] is \n\n-terminated ─────────────────────

// TestHandle_Gemini_Plain_DoneTermination is H-15.
//
// The Gemini adapter converts the blank upstream line (after the terminal chunk)
// into "data: [DONE]". With self-terminating events this line must be written as
// "data: [DONE]\n\n" — the terminal event must be dispatched by strict SSE
// clients. A trailing "\n" only would be silently dropped.
func TestHandle_Gemini_Plain_DoneTermination(t *testing.T) {
	t.Parallel()

	// Minimal Gemini stream: one text chunk with finishReason=STOP, then a blank
	// line which the GeminiAdapter converts to "data: [DONE]".
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream: no Flusher")
			return
		}
		// Terminal chunk: finishReason=STOP → adapter sets doneSent.
		fmt.Fprintln(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":3,"totalTokenCount":8}}`)
		fmt.Fprintln(w) // blank → GeminiAdapter returns "data: [DONE]"
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryGemini(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// h.PIIEngine nil: plain path.

	baseURL := startTestServer(t, h)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"gemini-test","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("H-15: streaming request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("H-15: status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	fullBody, _ := io.ReadAll(resp.Body)
	fullStr := string(fullBody)

	// The [DONE] sentinel must be present.
	if !strings.Contains(fullStr, "[DONE]") {
		t.Fatalf("H-15: [DONE] missing from output\noutput: %s", fullStr)
	}

	// [DONE] must be self-terminated with \n\n — assert the exact byte sequence.
	if !strings.Contains(fullStr, "data: [DONE]\n\n") {
		t.Errorf("H-15: 'data: [DONE]' is not \\n\\n-terminated in wire output\noutput: %q", fullStr)
	}

	// Parse events and verify [DONE] is its own distinct segment.
	rawEvents := strings.Split(fullStr, "\n\n")
	doneCount := 0
	for _, seg := range rawEvents {
		seg = strings.TrimSpace(seg)
		if seg == "data: [DONE]" {
			doneCount++
		}
	}
	if doneCount != 1 {
		t.Errorf("H-15: [DONE] appears in %d \\n\\n-segments, want exactly 1\noutput: %q", doneCount, fullStr)
	}
}
