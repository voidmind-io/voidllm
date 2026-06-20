package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// maxAnthropicToolBlocks is the adapter-level cap on the number of distinct
// tool_use content blocks that may appear in a single stream. Streams that
// exceed this limit have excess blocks silently dropped (the event returns nil).
// This is a defense-in-depth DoS cap independent of any PII pipeline limits.
const maxAnthropicToolBlocks = 1024

// anthropicToolIDRe is the conservative charset used for tool ids and function
// names forwarded to or received from Anthropic. Ids and names must match this
// pattern; any value that fails (including PII pseudonym shapes) causes a
// fail-closed error on the request side and a dropped/failed block on the
// response side.
//
// Note: comprehensive outbound scanning of id/name values for PII pseudonyms
// is a separate Stage 0a concern; this regex is the adapter-scoped defense.
var anthropicToolIDRe = regexp.MustCompile(`^[A-Za-z0-9_.+\-]+$`)

// AnthropicAdapter translates between the OpenAI chat completion wire format
// and the Anthropic Messages API. An instance must not be reused across
// requests because TransformStreamLine tracks per-stream state.
type AnthropicAdapter struct {
	msgID        string // populated by the first message_start event in a stream
	inputTokens  int    // accumulated from message_start usage
	outputTokens int    // accumulated from message_delta usage

	// toolCallCounter is incremented each time a tool_use content_block_start
	// is encountered. It maps the Anthropic content-block index to the
	// OpenAI tool-call index (tool calls may not start at content-block 0 when
	// there are preceding text blocks).
	toolCallCounter int
	// blockToToolCall maps Anthropic content-block-index → OpenAI tool-call-index.
	// Allocated lazily on first tool_use block.
	blockToToolCall map[int]int
}

// anthropicIncomingMessage is the parsed form of a single message in the
// OpenAI messages array sent to the proxy by the caller. Content is kept as
// jsonx.RawMessage because it may be either a plain string or a structured
// content-block array. ToolCalls carries the assistant tool_calls array if
// present. ToolCallID is the id linking a tool-result message to a tool call.
type anthropicIncomingMessage struct {
	Role       string           `json:"role"`
	Content    jsonx.RawMessage `json:"content,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	Name       string           `json:"name,omitempty"`
}

// anthropicContentBlock is a single content block in an Anthropic message.
// The Content field is used only for tool_result blocks and holds a JSON string
// or a JSON array of Anthropic text blocks, depending on how the original
// OpenAI tool message content was shaped. All other block types use Text.
type anthropicContentBlock struct {
	Type      string           `json:"type"`
	Text      string           `json:"text,omitempty"`
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Input     jsonx.RawMessage `json:"input,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	// Content is a raw JSON value: either a JSON string (for plain-string tool
	// results) or a JSON array of Anthropic text blocks (for structured
	// tool results). Omitted when nil.
	Content jsonx.RawMessage `json:"content,omitempty"`
}

// anthropicOutboundMessage is the Anthropic Messages API message shape sent
// upstream. Content is a slice of typed content blocks.
type anthropicOutboundMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

// anthropicToolDefinition is the Anthropic tool shape (name, description, input_schema).
type anthropicToolDefinition struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	InputSchema jsonx.RawMessage `json:"input_schema"`
}

// anthropicToolChoice is the Anthropic tool_choice object.
type anthropicToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
}

// openAIToolFunction is the function field inside an OpenAI tool definition.
type openAIToolFunction struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Parameters  jsonx.RawMessage `json:"parameters,omitempty"`
}

// openAITool is a single tool in the OpenAI tools array.
type openAITool struct {
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

// openAIToolCallFunction is the function field inside an OpenAI tool_calls element.
// Name carries omitempty so that argument-fragment deltas (which set only Arguments)
// do not emit an empty name field. Arguments must never carry omitempty because
// OpenAI SDKs expect "arguments":"" present on the first tool_call header delta.
type openAIToolCallFunction struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// openAIToolCall is one element in an OpenAI tool_calls array (request or response).
// Index is present only in streaming deltas (omitempty keeps it absent in
// non-streaming responses).
type openAIToolCall struct {
	Index    *int                   `json:"index,omitempty"`
	ID       string                 `json:"id,omitempty"`
	Type     string                 `json:"type,omitempty"`
	Function openAIToolCallFunction `json:"function"`
}

// anthropicResponse is the shape of a non-streaming Anthropic Messages API
// response, used to build an equivalent OpenAI chat completion object.
type anthropicResponse struct {
	ID         string                   `json:"id"`
	Type       string                   `json:"type"`
	Model      string                   `json:"model"`
	Content    []anthropicResponseBlock `json:"content"`
	StopReason *string                  `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// anthropicResponseBlock is a single content block in an Anthropic response.
// For type=="text" the Text field is populated; for type=="tool_use" the ID,
// Name, and Input fields are populated.
type anthropicResponseBlock struct {
	Type  string           `json:"type"`
	Text  string           `json:"text,omitempty"`
	ID    string           `json:"id,omitempty"`
	Name  string           `json:"name,omitempty"`
	Input jsonx.RawMessage `json:"input,omitempty"`
}

// openAIResponse is the OpenAI chat completion response shape produced by
// TransformResponse when translating from Anthropic format.
type openAIResponse struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Model   string         `json:"model"`
	Choices []openAIChoice `json:"choices"`
	Usage   openAIUsage    `json:"usage"`
}

// openAIChoice is a single choice in an OpenAI chat completion response.
type openAIChoice struct {
	Index        int           `json:"index"`
	Message      openAIMessage `json:"message"`
	FinishReason string        `json:"finish_reason"`
}

// openAIMessage is the message payload inside an OpenAI completion choice.
// Content may be null (omitempty with pointer) when ToolCalls are present
// and there is no text content; for non-tool responses it is always a string.
// ToolCalls carries translated tool_calls when the assistant response included
// tool_use blocks.
type openAIMessage struct {
	Role      string           `json:"role"`
	Content   *string          `json:"content"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

// openAIUsage holds token usage counts in OpenAI response format.
type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// openAIChunk is the shape of a single OpenAI streaming chunk.
type openAIChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Choices []openAIChunkChoice `json:"choices"`
}

// openAIChunkChoice is a single choice entry within a streaming chunk.
type openAIChunkChoice struct {
	Index        int              `json:"index"`
	Delta        openAIChunkDelta `json:"delta"`
	FinishReason *string          `json:"finish_reason"`
}

// openAIChunkDelta carries incremental content within a streaming chunk.
// Role and Content are used for text streams. ToolCalls is used when the
// stream carries tool-call deltas conformant to OpenAI Stage 0c shape.
type openAIChunkDelta struct {
	Role      string           `json:"role,omitempty"`
	Content   string           `json:"content,omitempty"`
	ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
}

// openAIOnlyFields lists request fields that Anthropic rejects; they are
// stripped from the body before forwarding. max_completion_tokens is handled
// specially (converted to max_tokens) before this strip runs; it is included
// here as defense-in-depth so any residual reference is removed.
var openAIOnlyFields = []string{
	"n",
	"frequency_penalty",
	"presence_penalty",
	"logprobs",
	"top_logprobs",
	"logit_bias",
	"response_format",
	"seed",
	"service_tier",
	"store",
	"user",
	"stream_options",
	"max_completion_tokens",
}

// TransformRequest converts an OpenAI chat completion request body into the
// Anthropic Messages API format. It:
//   - Extracts system messages and merges them into a top-level "system" field.
//   - Translates tool definitions from OpenAI format to Anthropic format.
//   - Validates tool definitions (type must be "function", name must be non-empty
//     and match a conservative charset, parameters if present must be a JSON object).
//   - Translates tool_choice from OpenAI format to Anthropic format, failing closed
//     on unknown values.
//   - Validates and charset-checks all forwarded tool ids and function names.
//   - Translates assistant tool_calls and tool-result messages into Anthropic
//     tool_use and tool_result content blocks, merging consecutive tool_result
//     messages into a single Anthropic user turn.
//   - Preserves array-of-parts tool_result content as an array of Anthropic text
//     blocks rather than concatenating parts (zero-knowledge preservation).
//   - Removes system messages from the messages array.
//   - Injects a default max_tokens of 4096 when the field is absent.
//   - Removes fields that Anthropic does not accept.
func (a *AnthropicAdapter) TransformRequest(body []byte, _ Model) ([]byte, error) {
	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("anthropic transform request: unmarshal body: %w", err)
	}

	// Collect declared tool names for tool_choice validation.
	var declaredToolNames []string

	// Translate tools array from OpenAI shape to Anthropic shape.
	if raw, ok := doc["tools"]; ok {
		var oaiTools []openAITool
		if err := jsonx.Unmarshal(raw, &oaiTools); err != nil {
			return nil, fmt.Errorf("anthropic transform request: unmarshal tools: %w", err)
		}
		antTools := make([]anthropicToolDefinition, 0, len(oaiTools))
		for _, t := range oaiTools {
			// FIX 6: type must be absent or "function".
			if t.Type != "" && t.Type != "function" {
				return nil, errors.New("anthropic transform request: unsupported tool type")
			}
			// FIX 6: function name must be non-empty.
			if t.Function.Name == "" {
				return nil, errors.New("anthropic transform request: tool function name is empty")
			}
			// FIX 5: charset-validate function name.
			if !anthropicToolIDRe.MatchString(t.Function.Name) {
				return nil, errors.New("anthropic transform request: tool function name contains invalid characters")
			}
			// FIX 6: parameters, if present, must be a JSON object.
			if t.Function.Parameters != nil {
				trimmed := strings.TrimSpace(string(t.Function.Parameters))
				if len(trimmed) > 0 && trimmed[0] != '{' {
					return nil, errors.New("anthropic transform request: tool parameters must be a JSON object")
				}
			}
			schema := t.Function.Parameters
			if schema == nil {
				// Anthropic requires input_schema; use an empty object schema.
				schema = jsonx.RawMessage(`{"type":"object","properties":{}}`)
			}
			declaredToolNames = append(declaredToolNames, t.Function.Name)
			antTools = append(antTools, anthropicToolDefinition{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: schema,
			})
		}
		toolsJSON, err := jsonx.Marshal(antTools)
		if err != nil {
			return nil, fmt.Errorf("anthropic transform request: marshal tools: %w", err)
		}
		doc["tools"] = jsonx.RawMessage(toolsJSON)
	}

	// Translate tool_choice from OpenAI format to Anthropic format.
	if raw, ok := doc["tool_choice"]; ok {
		translated, err := translateToolChoice(raw, declaredToolNames)
		if err != nil {
			return nil, fmt.Errorf("anthropic transform request: tool_choice: %w", err)
		}
		if translated == nil {
			// "none" or empty → remove both tools and tool_choice.
			delete(doc, "tool_choice")
			delete(doc, "tools")
		} else {
			choiceJSON, err := jsonx.Marshal(translated)
			if err != nil {
				return nil, fmt.Errorf("anthropic transform request: marshal tool_choice: %w", err)
			}
			doc["tool_choice"] = jsonx.RawMessage(choiceJSON)
		}
	}

	// Extract and rewrite messages.
	if raw, ok := doc["messages"]; ok {
		var msgs []anthropicIncomingMessage
		if err := jsonx.Unmarshal(raw, &msgs); err != nil {
			return nil, fmt.Errorf("anthropic transform request: unmarshal messages: %w", err)
		}

		var systemParts []string
		var outMsgs []anthropicOutboundMessage

		// pendingToolResults accumulates consecutive role:"tool" messages so they
		// can be merged into a single Anthropic user turn with multiple tool_result
		// blocks (Anthropic requires them grouped in one user message).
		var pendingToolResults []anthropicContentBlock

		flushToolResults := func() {
			if len(pendingToolResults) == 0 {
				return
			}
			outMsgs = append(outMsgs, anthropicOutboundMessage{
				Role:    "user",
				Content: pendingToolResults,
			})
			pendingToolResults = nil
		}

		for _, m := range msgs {
			switch m.Role {
			case "system":
				// Flush any pending tool results before processing a system message
				// (should not occur in practice, but be safe).
				flushToolResults()
				// Only plain-string content is valid as a system prompt.
				var textContent string
				if err := jsonx.Unmarshal(m.Content, &textContent); err == nil {
					systemParts = append(systemParts, textContent)
				}

			case "tool":
				// OpenAI tool-result message → Anthropic tool_result content block.
				// These are accumulated and flushed as a single user turn.
				//
				// FIX 1 (zero-knowledge): when content is an array of parts, emit
				// tool_result.content as an ARRAY of Anthropic text blocks — one per
				// OpenAI text part — preserving the exact part boundaries that the PII
				// scanner saw. Concatenating parts would reconstruct PII that was
				// deliberately split (e.g. "alice@" + "example.com"). For a plain
				// string, emit it as a JSON string (unchanged).
				// FIX 5: validate tool_call_id charset.
				if m.ToolCallID != "" && !anthropicToolIDRe.MatchString(m.ToolCallID) {
					return nil, errors.New("anthropic transform request: tool_call_id contains invalid characters")
				}
				var contentRaw jsonx.RawMessage
				if m.Content != nil {
					var contentStr string
					if err := jsonx.Unmarshal(m.Content, &contentStr); err == nil {
						// Plain string content — marshal it back as a JSON string.
						encoded, merr := jsonx.Marshal(contentStr)
						if merr != nil {
							return nil, fmt.Errorf("anthropic transform request: marshal tool result content: %w", merr)
						}
						contentRaw = jsonx.RawMessage(encoded)
					} else {
						// Not a plain string — try array of content parts.
						var parts []struct {
							Type string `json:"type"`
							Text string `json:"text"`
						}
						if jsonx.Unmarshal(m.Content, &parts) == nil {
							// Build an array of Anthropic text blocks, one per text part.
							// Non-text part types are skipped (zero-knowledge: we only
							// forward what we understand).
							type anthropicTextBlock struct {
								Type string `json:"type"`
								Text string `json:"text"`
							}
							var blocks []anthropicTextBlock
							for _, p := range parts {
								if p.Type == "text" {
									blocks = append(blocks, anthropicTextBlock{
										Type: "text",
										Text: p.Text,
									})
								}
							}
							encoded, merr := jsonx.Marshal(blocks)
							if merr != nil {
								return nil, fmt.Errorf("anthropic transform request: marshal tool result content array: %w", merr)
							}
							contentRaw = jsonx.RawMessage(encoded)
						}
						// If neither shape unmarshals, contentRaw stays nil (omitted).
					}
				}
				pendingToolResults = append(pendingToolResults, anthropicContentBlock{
					Type:      "tool_result",
					ToolUseID: m.ToolCallID,
					Content:   contentRaw,
				})

			case "assistant":
				// Flush accumulated tool results before an assistant message.
				flushToolResults()

				if len(m.ToolCalls) > 0 {
					// Assistant message with tool_calls → Anthropic assistant message
					// with tool_use content blocks (and optionally a text block first).
					var blocks []anthropicContentBlock
					// Include a text block only if content is a non-empty string.
					if m.Content != nil {
						var textContent string
						if err := jsonx.Unmarshal(m.Content, &textContent); err == nil && textContent != "" {
							blocks = append(blocks, anthropicContentBlock{
								Type: "text",
								Text: textContent,
							})
						}
					}
					for _, tc := range m.ToolCalls {
						// FIX 5: validate tool_call id and function name charset.
						if tc.ID != "" && !anthropicToolIDRe.MatchString(tc.ID) {
							return nil, errors.New("anthropic transform request: tool_call id contains invalid characters")
						}
						if tc.Function.Name != "" && !anthropicToolIDRe.MatchString(tc.Function.Name) {
							return nil, errors.New("anthropic transform request: tool_call function name contains invalid characters")
						}
						input, err := parseArgumentsToObject(tc.Function.Arguments)
						if err != nil {
							return nil, fmt.Errorf("anthropic transform request: invalid tool_call arguments: %w", err)
						}
						blocks = append(blocks, anthropicContentBlock{
							Type:  "tool_use",
							ID:    tc.ID,
							Name:  tc.Function.Name,
							Input: input,
						})
					}
					outMsgs = append(outMsgs, anthropicOutboundMessage{
						Role:    "assistant",
						Content: blocks,
					})
				} else {
					// Plain text assistant message.
					msg, err := buildTextMessage("assistant", m.Content)
					if err != nil {
						return nil, fmt.Errorf("anthropic transform request: %w", err)
					}
					outMsgs = append(outMsgs, msg)
				}

			default:
				// user and any other roles: flush pending tool results first, then
				// emit as plain text message (same as existing behavior).
				flushToolResults()
				msg, err := buildTextMessage(m.Role, m.Content)
				if err != nil {
					return nil, fmt.Errorf("anthropic transform request: %w", err)
				}
				outMsgs = append(outMsgs, msg)
			}
		}

		// Flush any trailing tool results.
		flushToolResults()

		if len(systemParts) > 0 {
			systemText := strings.Join(systemParts, "\n")
			systemJSON, err := jsonx.Marshal(systemText)
			if err != nil {
				return nil, fmt.Errorf("anthropic transform request: marshal system: %w", err)
			}
			doc["system"] = jsonx.RawMessage(systemJSON)
		}

		remainingJSON, err := jsonx.Marshal(outMsgs)
		if err != nil {
			return nil, fmt.Errorf("anthropic transform request: marshal messages: %w", err)
		}
		doc["messages"] = jsonx.RawMessage(remainingJSON)
	}

	// Anthropic requires max_tokens. Accept max_completion_tokens as an
	// OpenAI-compatible alias and convert it. If neither field is present,
	// inject a safe default of 4096.
	if _, ok := doc["max_tokens"]; !ok {
		if mct, ok := doc["max_completion_tokens"]; ok {
			doc["max_tokens"] = mct
			delete(doc, "max_completion_tokens")
		} else {
			doc["max_tokens"] = jsonx.RawMessage("4096")
		}
	} else {
		// max_tokens already present; remove max_completion_tokens if it
		// was also sent to avoid confusing Anthropic.
		delete(doc, "max_completion_tokens")
	}

	// Remove fields Anthropic does not accept.
	for _, field := range openAIOnlyFields {
		delete(doc, field)
	}

	out, err := jsonx.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("anthropic transform request: marshal output: %w", err)
	}
	return out, nil
}

// TransformURL maps the OpenAI endpoint path to the equivalent Anthropic path.
// chat/completions becomes /v1/messages; all other paths are forwarded as-is.
func (a *AnthropicAdapter) TransformURL(baseURL, upstreamPath string, _ Model) string {
	base := strings.TrimRight(baseURL, "/")
	if upstreamPath == "chat/completions" {
		return base + "/v1/messages"
	}
	return base + "/" + upstreamPath
}

// SetHeaders configures the outbound request for the Anthropic API. It removes
// the Bearer Authorization header set by setUpstreamHeaders and substitutes
// the x-api-key header that Anthropic requires.
func (a *AnthropicAdapter) SetHeaders(req *http.Request, model Model) {
	req.Header.Del("Authorization")
	if model.APIKey != "" {
		req.Header.Set("x-api-key", model.APIKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Content-Type", "application/json")
}

// TransformResponse converts a complete Anthropic Messages API response body
// into an OpenAI chat completion response body. Text blocks are joined into
// the message content string. tool_use blocks are translated to OpenAI
// tool_calls; when tool_use blocks are present and there is no text content,
// the message content is null.
//
// FIX 5: tool_use id and name in the response are charset-validated; if
// either fails, the transform returns an error (fail-closed).
// FIX 7: tool_use input, if present, must be a JSON object; non-object input
// causes the transform to return an error (fail-closed).
func (a *AnthropicAdapter) TransformResponse(body []byte) ([]byte, error) {
	var ar anthropicResponse
	if err := jsonx.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("anthropic transform response: unmarshal: %w", err)
	}

	var textParts []string
	var toolCalls []openAIToolCall

	for _, block := range ar.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			// FIX 5: charset-validate response tool_use id and name.
			if block.ID != "" && !anthropicToolIDRe.MatchString(block.ID) {
				return nil, errors.New("anthropic transform response: tool_use id contains invalid characters")
			}
			if block.Name != "" && !anthropicToolIDRe.MatchString(block.Name) {
				return nil, errors.New("anthropic transform response: tool_use name contains invalid characters")
			}
			// FIX 7: if input is present, it must be a JSON object.
			argsStr, err := serializeInputToArguments(block.Input)
			if err != nil {
				return nil, fmt.Errorf("anthropic transform response: invalid tool_use input: %w", err)
			}
			toolCalls = append(toolCalls, openAIToolCall{
				ID:   block.ID,
				Type: "function",
				Function: openAIToolCallFunction{
					Name:      block.Name,
					Arguments: argsStr,
				},
			})
		}
	}

	finishReason := mapStopReason(ar.StopReason)

	// Build the message. When tool_use blocks are present and no text was
	// produced, content is null (pointer is nil). When text is present, content
	// is the joined string regardless of whether tool calls are also present.
	var msgContent *string
	if len(textParts) > 0 {
		s := strings.Join(textParts, "")
		msgContent = &s
	} else if len(toolCalls) == 0 {
		// No text and no tool calls — preserve empty string content for
		// consistency with the existing non-tool behavior.
		s := ""
		msgContent = &s
	}
	// When toolCalls is non-empty and textParts is empty, msgContent stays nil
	// (null in JSON).

	msg := openAIMessage{
		Role:    "assistant",
		Content: msgContent,
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	resp := openAIResponse{
		ID:     ar.ID,
		Object: "chat.completion",
		Model:  ar.Model,
		Choices: []openAIChoice{
			{
				Index:        0,
				Message:      msg,
				FinishReason: finishReason,
			},
		},
		Usage: openAIUsage{
			PromptTokens:     ar.Usage.InputTokens,
			CompletionTokens: ar.Usage.OutputTokens,
			TotalTokens:      ar.Usage.InputTokens + ar.Usage.OutputTokens,
		},
	}

	out, err := jsonx.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("anthropic transform response: marshal: %w", err)
	}
	return out, nil
}

// TransformStreamLine processes one raw SSE line from the Anthropic stream and
// returns the equivalent OpenAI SSE line, or nil to drop the line.
//
// Anthropic uses named event types (event: content_block_delta, data: {...})
// which have no OpenAI equivalent. This method:
//   - Drops bare "event:" lines.
//   - Passes through blank lines (SSE delimiters).
//   - Translates "data:" payloads by their "type" field.
//   - Returns nil for event types that have no OpenAI equivalent.
//
// For tool_use content blocks this method emits OpenAI-conformant tool_calls
// deltas compatible with PII Stage 0c:
//   - content_block_start with type=="tool_use" emits a header delta carrying
//     index, id, type:"function", function.name, and function.arguments:"".
//   - content_block_delta with type=="input_json_delta" emits argument fragments
//     carrying index and function.arguments:<partial_json>.
//
// FIX 4: the total number of distinct tool_use blocks is capped at
// maxAnthropicToolBlocks. Blocks beyond the cap are dropped (nil returned).
// Negative or oversized content-block indices are also rejected (nil returned).
//
// FIX 8: a duplicate content_block_start for an already-seen content-block
// index is dropped (nil returned) rather than overwriting the mapping.
// Residual: TransformStreamLine cannot signal a hard abort because it returns
// []byte only; a malformed upstream tool stream may silently drop an affected
// tool call. This is a correctness limitation, not a PII leak, tracked as a
// follow-up (adapter abort-signal requires an interface change).
func (a *AnthropicAdapter) TransformStreamLine(line []byte) []byte {
	s := string(line)

	// Blank line — SSE event delimiter, pass through.
	if s == "" {
		return line
	}

	// Drop Anthropic event-type lines; OpenAI does not use them.
	if strings.HasPrefix(s, "event:") {
		return nil
	}

	const dataPrefix = "data: "
	if !strings.HasPrefix(s, dataPrefix) {
		return line
	}

	payload := []byte(s[len(dataPrefix):])

	var event map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(payload, &event); err != nil {
		// Not valid JSON — pass through unchanged so the client can observe it.
		return line
	}

	var eventType string
	if raw, ok := event["type"]; ok {
		_ = jsonx.Unmarshal(raw, &eventType)
	}

	switch eventType {
	case "message_start":
		// Extract the message ID and input token count for this stream.
		var ms struct {
			Message struct {
				ID    string `json:"id"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := jsonx.Unmarshal(payload, &ms); err == nil {
			if ms.Message.ID != "" {
				a.msgID = ms.Message.ID
			}
			a.inputTokens = ms.Message.Usage.InputTokens
		}
		if a.msgID == "" {
			a.msgID = "chatcmpl-proxy"
		}
		chunk := a.buildChunk(openAIChunkDelta{Role: "assistant"}, nil)
		return appendDataPrefix(chunk)

	case "content_block_start":
		// Only tool_use blocks produce output at this stage; text content_block_start
		// is dropped (text content arrives via text_delta events).
		var cbs struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
		}
		if err := jsonx.Unmarshal(payload, &cbs); err != nil {
			return nil
		}
		if cbs.ContentBlock.Type != "tool_use" {
			return nil
		}

		// FIX 4: reject negative content-block indices.
		if cbs.Index < 0 {
			return nil
		}
		// FIX 4: cap the total number of distinct tool_use blocks.
		if a.toolCallCounter >= maxAnthropicToolBlocks {
			return nil
		}

		// FIX 8: reject duplicate content-block indices (do not overwrite).
		if a.blockToToolCall != nil {
			if _, exists := a.blockToToolCall[cbs.Index]; exists {
				// Duplicate — drop silently. See residual note in TransformStreamLine doc.
				return nil
			}
		}

		// FIX 1 (streaming): charset-validate tool_use id and name before registering
		// the block. Invalid values may carry PII pseudonyms; drop the block (nil)
		// rather than forwarding it, consistent with other streaming drop behavior.
		if cbs.ContentBlock.ID != "" && !anthropicToolIDRe.MatchString(cbs.ContentBlock.ID) {
			return nil
		}
		if cbs.ContentBlock.Name != "" && !anthropicToolIDRe.MatchString(cbs.ContentBlock.Name) {
			return nil
		}

		// Assign the next tool-call index to this content-block index.
		if a.blockToToolCall == nil {
			a.blockToToolCall = make(map[int]int)
		}
		toolCallIdx := a.toolCallCounter
		a.blockToToolCall[cbs.Index] = toolCallIdx
		a.toolCallCounter++

		// Emit header delta: index, id, type:"function", function.name, function.arguments:"".
		tcIdx := toolCallIdx
		emptyArgs := ""
		tc := openAIToolCall{
			Index: &tcIdx,
			ID:    cbs.ContentBlock.ID,
			Type:  "function",
			Function: openAIToolCallFunction{
				Name:      cbs.ContentBlock.Name,
				Arguments: emptyArgs,
			},
		}
		chunk := a.buildChunk(openAIChunkDelta{ToolCalls: []openAIToolCall{tc}}, nil)
		return appendDataPrefix(chunk)

	case "content_block_delta":
		var cbd struct {
			Index int `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := jsonx.Unmarshal(payload, &cbd); err != nil {
			return nil
		}
		switch cbd.Delta.Type {
		case "text_delta":
			chunk := a.buildChunk(openAIChunkDelta{Content: cbd.Delta.Text}, nil)
			return appendDataPrefix(chunk)
		case "input_json_delta":
			// Look up the tool-call index for this content-block index.
			if a.blockToToolCall == nil {
				return nil
			}
			toolCallIdx, ok := a.blockToToolCall[cbd.Index]
			if !ok {
				return nil
			}
			// Emit arguments fragment delta: index + function.arguments only.
			tcIdx := toolCallIdx
			tc := openAIToolCall{
				Index: &tcIdx,
				Function: openAIToolCallFunction{
					Arguments: cbd.Delta.PartialJSON,
				},
			}
			chunk := a.buildChunk(openAIChunkDelta{ToolCalls: []openAIToolCall{tc}}, nil)
			return appendDataPrefix(chunk)
		default:
			return nil
		}

	case "message_delta":
		var md struct {
			Delta struct {
				StopReason *string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := jsonx.Unmarshal(payload, &md); err != nil {
			return nil
		}
		a.outputTokens = md.Usage.OutputTokens
		reason := mapStopReason(md.Delta.StopReason)
		chunk := a.buildChunk(openAIChunkDelta{}, &reason)
		return appendDataPrefix(chunk)

	case "message_stop":
		return []byte("data: [DONE]")

	case "ping", "content_block_stop":
		return nil

	default:
		return nil
	}
}

// StreamUsage returns the token counts accumulated during the Anthropic stream.
// inputTokens is captured from the message_start event and outputTokens from
// the message_delta event. Both are zero until those events have been processed.
func (a *AnthropicAdapter) StreamUsage() UsageInfo {
	return UsageInfo{
		PromptTokens:     a.inputTokens,
		CompletionTokens: a.outputTokens,
		TotalTokens:      a.inputTokens + a.outputTokens,
	}
}

// buildChunk assembles an OpenAI streaming chunk using the adapter's current
// message ID.
func (a *AnthropicAdapter) buildChunk(delta openAIChunkDelta, finishReason *string) []byte {
	id := a.msgID
	if id == "" {
		id = "chatcmpl-proxy"
	}
	chunk := openAIChunk{
		ID:     id,
		Object: "chat.completion.chunk",
		Choices: []openAIChunkChoice{
			{
				Index:        0,
				Delta:        delta,
				FinishReason: finishReason,
			},
		},
	}
	out, err := jsonx.Marshal(chunk)
	if err != nil {
		return nil
	}
	return out
}

// appendDataPrefix prepends "data: " to a JSON byte slice.
func appendDataPrefix(b []byte) []byte {
	const prefix = "data: "
	return append([]byte(prefix), b...)
}

// mapStopReason converts an Anthropic stop_reason string to the OpenAI
// finish_reason equivalent. A nil input returns "stop".
func mapStopReason(reason *string) string {
	if reason == nil {
		return "stop"
	}
	switch *reason {
	case "end_turn", "stop_sequence":
		return "stop"
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

// translateToolChoice converts the OpenAI tool_choice value (string or object)
// to an Anthropic tool_choice object. Returns nil when the choice should be
// removed (i.e., OpenAI "none").
//
// FIX 6: unknown string values and unknown object shapes are rejected with an
// error (fail-closed) rather than silently falling back to "auto". A named
// tool_choice that references a tool name not in declaredNames is also rejected.
func translateToolChoice(raw jsonx.RawMessage, declaredNames []string) (*anthropicToolChoice, error) {
	// Try string first ("auto", "required", "none").
	var s string
	if err := jsonx.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return &anthropicToolChoice{Type: "auto"}, nil
		case "required":
			return &anthropicToolChoice{Type: "any"}, nil
		case "none":
			// Signal caller to remove tools and tool_choice.
			return nil, nil
		default:
			return nil, errors.New("unknown tool_choice value")
		}
	}

	// Try object: {"type":"function","function":{"name":"<name>"}}.
	var obj struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := jsonx.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("unrecognised tool_choice shape: %w", err)
	}
	if obj.Type != "function" {
		return nil, errors.New("unknown tool_choice object type")
	}
	if obj.Function.Name == "" {
		return nil, errors.New("tool_choice function name is empty")
	}
	// FIX 2: unconditionally validate charset of the named tool_choice function
	// name, regardless of whether any tools were declared. An invalid name may
	// carry PII pseudonyms and must be rejected fail-closed.
	if !anthropicToolIDRe.MatchString(obj.Function.Name) {
		return nil, errors.New("tool_choice function name contains invalid characters")
	}
	// FIX 6: named tool_choice must reference a declared tool.
	if len(declaredNames) > 0 {
		found := false
		for _, n := range declaredNames {
			if n == obj.Function.Name {
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("tool_choice references undeclared tool")
		}
	}
	return &anthropicToolChoice{Type: "tool", Name: obj.Function.Name}, nil
}

// parseArgumentsToObject converts an OpenAI function.arguments JSON string
// into a jsonx.RawMessage object suitable for the Anthropic input field.
// OpenAI stores arguments as a JSON-encoded string (e.g. `"{\"key\":\"val\"}"`);
// Anthropic expects a native JSON object.
//
// Empty or whitespace-only arguments are treated as an empty object and return {}.
// Non-empty arguments must be valid JSON and must be a JSON object (start with '{');
// any other valid JSON type (array, number, string, boolean, null) or invalid JSON
// returns an error, since Anthropic will reject anything other than an object.
func parseArgumentsToObject(arguments string) (jsonx.RawMessage, error) {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return jsonx.RawMessage(`{}`), nil
	}
	if !jsonx.Valid([]byte(trimmed)) {
		return nil, errors.New("anthropic transform request: invalid tool_call arguments")
	}
	if trimmed[0] != '{' {
		return nil, errors.New("anthropic transform request: invalid tool_call arguments")
	}
	return jsonx.RawMessage(trimmed), nil
}

// serializeInputToArguments converts an Anthropic tool_use input field
// (a raw JSON object) into a JSON string for the OpenAI function.arguments field.
// If the input is nil or empty, an empty object string "{}" is returned.
//
// FIX 7: if the input is present but not a JSON object, an error is returned
// (fail-closed — Anthropic tool_use input must always be an object).
func serializeInputToArguments(input jsonx.RawMessage) (string, error) {
	if len(input) == 0 {
		return "{}", nil
	}
	trimmed := strings.TrimSpace(string(input))
	if len(trimmed) == 0 {
		return "{}", nil
	}
	if trimmed[0] != '{' {
		return "", errors.New("tool_use input is not a JSON object")
	}
	return string(input), nil
}

// buildTextMessage constructs an Anthropic outbound message from a raw JSON
// content value.
//
// FIX 2: when content is an array of content parts, each {"type":"text","text":...}
// part is translated into a separate Anthropic text content block, preserving
// part boundaries exactly (same zero-knowledge reasoning as FIX 1 — no joining).
// For a plain string, a single text block is emitted (byte-identical to prior
// behavior). For unsupported non-text part types, an error is returned
// (fail-closed) rather than silently corrupting the message.
//
// PR #136 compatibility: a JSON null content value is treated as empty content
// and emits a message with a single empty text block. Only genuinely unsupported
// shapes (a number, or an array with unknown part types) return an error.
func buildTextMessage(role string, content jsonx.RawMessage) (anthropicOutboundMessage, error) {
	// nil RawMessage or JSON null both mean "no content" — treat as empty.
	if content == nil || bytes.Equal(bytes.TrimSpace([]byte(content)), []byte("null")) {
		return anthropicOutboundMessage{
			Role:    role,
			Content: []anthropicContentBlock{{Type: "text", Text: ""}},
		}, nil
	}

	// Try plain string first.
	var textContent string
	if err := jsonx.Unmarshal(content, &textContent); err == nil {
		return anthropicOutboundMessage{
			Role:    role,
			Content: []anthropicContentBlock{{Type: "text", Text: textContent}},
		}, nil
	}

	// Try array of content parts.
	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL *struct {
			URL string `json:"url"`
		} `json:"image_url,omitempty"`
	}
	if err := jsonx.Unmarshal(content, &parts); err == nil {
		var blocks []anthropicContentBlock
		for _, p := range parts {
			switch p.Type {
			case "text":
				blocks = append(blocks, anthropicContentBlock{
					Type: "text",
					Text: p.Text,
				})
			default:
				// Fail-closed for unsupported part types to avoid silent corruption.
				return anthropicOutboundMessage{}, errors.New("unsupported content part type")
			}
		}
		if len(blocks) == 0 {
			blocks = []anthropicContentBlock{{Type: "text", Text: ""}}
		}
		return anthropicOutboundMessage{
			Role:    role,
			Content: blocks,
		}, nil
	}

	// Neither plain string, null, nor array — return an error rather than corrupting.
	return anthropicOutboundMessage{}, errors.New("unrecognised content shape")
}
