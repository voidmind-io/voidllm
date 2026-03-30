package proxy

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// AnthropicAdapter translates between the OpenAI chat completion wire format
// and the Anthropic Messages API. An instance must not be reused across
// requests because TransformStreamLine tracks per-stream state.
type AnthropicAdapter struct {
	msgID        string // populated by the first message_start event in a stream
	inputTokens  int    // accumulated from message_start usage
	outputTokens int    // accumulated from message_delta usage
}

// anthropicMessage is the minimal parsed form of a single message in the
// OpenAI messages array, used when extracting system prompts. Content is kept
// as jsonx.RawMessage because it may be either a plain string or a structured
// content-block array.
type anthropicMessage struct {
	Role    string           `json:"role"`
	Content jsonx.RawMessage `json:"content"`
}

// anthropicResponse is the shape of a non-streaming Anthropic Messages API
// response, used to build an equivalent OpenAI chat completion object.
type anthropicResponse struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Model   string `json:"model"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason *string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
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
type openAIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
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
type openAIChunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
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
//   - Removes system messages from the messages array.
//   - Injects a default max_tokens of 4096 when the field is absent.
//   - Removes fields that Anthropic does not accept.
func (a *AnthropicAdapter) TransformRequest(body []byte, _ Model) ([]byte, error) {
	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("anthropic transform request: unmarshal body: %w", err)
	}

	// Extract and rewrite messages.
	if raw, ok := doc["messages"]; ok {
		var msgs []anthropicMessage
		if err := jsonx.Unmarshal(raw, &msgs); err != nil {
			return nil, fmt.Errorf("anthropic transform request: unmarshal messages: %w", err)
		}

		var systemParts []string
		var remaining []anthropicMessage
		for _, m := range msgs {
			if m.Role == "system" {
				// Only plain-string content is valid as a system prompt.
				// Structured content-block arrays are never system messages
				// and are skipped silently.
				var textContent string
				if err := jsonx.Unmarshal(m.Content, &textContent); err == nil {
					systemParts = append(systemParts, textContent)
				}
				continue
			}
			remaining = append(remaining, m)
		}

		if len(systemParts) > 0 {
			systemText := strings.Join(systemParts, "\n")
			systemJSON, err := jsonx.Marshal(systemText)
			if err != nil {
				return nil, fmt.Errorf("anthropic transform request: marshal system: %w", err)
			}
			doc["system"] = jsonx.RawMessage(systemJSON)
		}

		remainingJSON, err := jsonx.Marshal(remaining)
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
// into an OpenAI chat completion response body.
func (a *AnthropicAdapter) TransformResponse(body []byte) ([]byte, error) {
	var ar anthropicResponse
	if err := jsonx.Unmarshal(body, &ar); err != nil {
		return nil, fmt.Errorf("anthropic transform response: unmarshal: %w", err)
	}

	var textParts []string
	for _, block := range ar.Content {
		if block.Type == "text" {
			textParts = append(textParts, block.Text)
		}
	}

	finishReason := mapStopReason(ar.StopReason)

	resp := openAIResponse{
		ID:     ar.ID,
		Object: "chat.completion",
		Model:  ar.Model,
		Choices: []openAIChoice{
			{
				Index: 0,
				Message: openAIMessage{
					Role:    "assistant",
					Content: strings.Join(textParts, ""),
				},
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

	case "content_block_delta":
		var cbd struct {
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := jsonx.Unmarshal(payload, &cbd); err != nil || cbd.Delta.Type != "text_delta" {
			return nil
		}
		chunk := a.buildChunk(openAIChunkDelta{Content: cbd.Delta.Text}, nil)
		return appendDataPrefix(chunk)

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

	case "ping", "content_block_start", "content_block_stop":
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
