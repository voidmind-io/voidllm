package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// maxAnthropicToolBlocks is the adapter-level cap on the number of distinct
// tool_use content blocks that may appear in a single stream. Streams that
// exceed this limit have excess blocks silently dropped (the event returns nil).
// This is a defense-in-depth DoS cap independent of any PII pipeline limits,
// aligned with maxToolCallsPerChoice=64 in the PII Stage 0c StreamRestorer.
const maxAnthropicToolBlocks = 64

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
	modelName    string // stored from TransformRequest for use in TransformResponse
	inputTokens  int    // accumulated from message_start usage
	outputTokens int    // accumulated from message_delta usage
	// cacheReadTokens is cache_read_input_tokens from message_start usage: the
	// portion of prompt tokens served from Anthropic's prompt cache, billed
	// below the normal input rate. Anthropic reports this IN ADDITION TO
	// input_tokens, not as a subset — see UsageInfo's doc for the reconciliation.
	cacheReadTokens int
	// cacheWriteTokens is cache_creation_input_tokens from message_start usage:
	// the portion of prompt tokens written to Anthropic's prompt cache, billed
	// above the normal input rate. Also additive to input_tokens.
	cacheWriteTokens int

	// toolCallCounter is incremented each time a tool_use content_block_start
	// is encountered. It maps the Anthropic content-block index to the
	// OpenAI tool-call index (tool calls may not start at content-block 0 when
	// there are preceding text blocks).
	toolCallCounter int
	// blockToToolCall maps Anthropic content-block-index → OpenAI tool-call-index.
	// Allocated lazily on first tool_use block.
	blockToToolCall map[int]int

	// includeUsage is set from stream_options.include_usage during
	// TransformRequest, before that field is dropped by the allowlist filter
	// (Anthropic has no stream_options equivalent). When true, message_stop
	// emits a trailing usage-only chunk before [DONE], mirroring OpenAI's
	// include_usage behavior.
	includeUsage bool
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
	// CacheControl, when present, is the Anthropic cache_control object (e.g.
	// {"type":"ephemeral"}) on a text block, validated against the closed
	// schema in validateCacheControl and re-marshaled — the raw client bytes
	// are never forwarded verbatim.
	CacheControl jsonx.RawMessage `json:"cache_control,omitempty"`
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
	// DisableParallelToolUse translates OpenAI's parallel_tool_calls:false.
	// Anthropic has no top-level request field for this; it is only settable
	// inside tool_choice. nil (omitted) leaves Anthropic's default (parallel
	// tool calls allowed) unchanged.
	DisableParallelToolUse *bool `json:"disable_parallel_tool_use,omitempty"`
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
		InputTokens int `json:"input_tokens"`
		// CacheReadInputTokens and CacheCreationInputTokens are reported IN
		// ADDITION TO InputTokens by Anthropic, not as a subset of it — see
		// UsageInfo's doc comment for the cross-provider reconciliation.
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		OutputTokens             int `json:"output_tokens"`
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
	// PromptTokensDetails carries cached/cache-write token counts through the
	// buffered (non-streaming) response transform. handler.go's extractUsage
	// runs against the POST-transform body when an adapter is present, so any
	// count not present in this transformed JSON is lost — this field is what
	// makes that round trip work for Anthropic and Gemini. Nil (omitted) when
	// both counts are zero, matching the real OpenAI API's optional field.
	PromptTokensDetails *openAIPromptTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// openAIPromptTokensDetails is the usage.prompt_tokens_details object. See
// wireUsageDetails in handler.go, which parses this same shape back out of
// the transformed response body — the two types must stay in sync.
type openAIPromptTokensDetails struct {
	// CachedTokens is the OpenAI/Gemini-standard field: the subset of
	// prompt_tokens served from cache.
	CachedTokens int `json:"cached_tokens"`
	// CacheCreationTokens is a proxy-internal extension (no OpenAI
	// equivalent) carrying Anthropic's cache-write count through the same
	// object rather than inventing a second top-level field.
	CacheCreationTokens int `json:"cache_creation_tokens,omitempty"`
}

// cacheUsageDetails builds the prompt_tokens_details object for an
// OpenAI-shaped usage payload so cached-token counts survive an adapter's
// response transform. Returns nil when both counts are zero so an unaffected
// response keeps the exact OpenAI wire shape (no empty object emitted).
func cacheUsageDetails(cachedRead, cacheWrite int) *openAIPromptTokensDetails {
	if cachedRead == 0 && cacheWrite == 0 {
		return nil
	}
	return &openAIPromptTokensDetails{
		CachedTokens:        cachedRead,
		CacheCreationTokens: cacheWrite,
	}
}

// openAIChunk is the shape of a single OpenAI streaming chunk. Created,
// Model, and Usage are only populated for the trailing usage-only chunk that
// Anthropic's adapter emits when stream_options.include_usage was requested
// (see AnthropicAdapter.buildStreamUsageChunk); every other producer of this
// type (Anthropic's per-event chunks, Gemini's chunks) leaves them nil, so
// the omitempty pointers keep their JSON output byte-identical to before
// these fields existed.
type openAIChunk struct {
	ID      string              `json:"id"`
	Object  string              `json:"object"`
	Created *int64              `json:"created,omitempty"`
	Model   *string             `json:"model,omitempty"`
	Choices []openAIChunkChoice `json:"choices"`
	Usage   *openAIUsage        `json:"usage,omitempty"`
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

// anthropicAllowedFields is the exhaustive set of top-level fields the
// Anthropic Messages API accepts. After every translation in TransformRequest
// has run, any field not in this set is dropped — including any current or
// future OpenAI-only field. This replaces a maintain-a-blocklist approach
// (every new OpenAI request field previously had to be added to a strip
// list or it reached Anthropic verbatim and produced a 400).
var anthropicAllowedFields = map[string]struct{}{
	"model":          {},
	"messages":       {},
	"system":         {},
	"max_tokens":     {},
	"stream":         {},
	"temperature":    {},
	"top_p":          {},
	"top_k":          {},
	"stop_sequences": {},
	"tools":          {},
	"tool_choice":    {},
	"metadata":       {},
	"thinking":       {},
	"cache_control":  {},
}

// TransformRequest converts an OpenAI chat completion request body into the
// Anthropic Messages API format. It:
//   - Rejects a client-supplied top-level "system" or "stop_sequences" field
//     fail-closed: both are native Anthropic fields, not part of the OpenAI
//     surface, and are never scanned by internal/pii — accepting them from
//     the client would let unscanned content reach the upstream provider.
//     Use a system message (or the "developer" role) and "stop" instead.
//   - Validates a client-supplied top-level cache_control (and every per-part
//     cache_control encountered below) against a closed schema: type must be
//     exactly "ephemeral" and the optional ttl must be "5m" or "1h" — any
//     other shape is rejected fail-closed. The validated value is re-marshaled;
//     the client-supplied bytes are never forwarded verbatim.
//   - Extracts system and developer messages (role "developer" is OpenAI's
//     alias for "system") and merges them into a top-level "system" field, as
//     a plain joined string when every source message was a plain string and
//     no part carried cache_control, or otherwise as an array of Anthropic
//     text blocks — one per non-empty text part, never concatenated, with
//     per-part cache_control preserved.
//   - Translates tool definitions from OpenAI format to Anthropic format.
//   - Validates tool definitions (type must be "function", name must be non-empty
//     and match a conservative charset, parameters if present must be a JSON object).
//   - Translates tool_choice from OpenAI format to Anthropic format, failing closed
//     on unknown values.
//   - Translates parallel_tool_calls:false (with tools surviving) into
//     tool_choice.disable_parallel_tool_use:true, synthesizing {type:"auto"}
//     when no tool_choice was given. true, absent, or no surviving tools →
//     no-op. A present parallel_tool_calls value that is not a JSON boolean
//     is rejected fail-closed.
//   - Translates stop (string, array of strings, or null) into stop_sequences,
//     dropping empty/whitespace-only entries. Any other shape is rejected
//     fail-closed.
//   - Translates user into metadata.user_id; OpenAI's free-form metadata map
//     has no Anthropic equivalent and is never forwarded.
//   - Clamps temperature above 1 down to 1 (Anthropic's range is 0-1).
//   - Validates and charset-checks all forwarded tool ids and function names.
//   - Translates assistant tool_calls and tool-result messages into Anthropic
//     tool_use and tool_result content blocks, merging consecutive tool_result
//     messages into a single Anthropic user turn. An assistant message with
//     tool_calls keeps its own content (string or array of parts, same
//     zero-knowledge part parsing as plain text messages) as leading text
//     blocks, followed by the tool_use blocks.
//   - Preserves array-of-parts tool_result content as an array of Anthropic text
//     blocks rather than concatenating parts (zero-knowledge preservation),
//     keeping per-part cache_control.
//   - Removes system and developer messages from the messages array.
//   - Injects a default max_tokens of 4096 when the field is absent.
//   - Captures stream_options.include_usage for use by TransformStreamLine
//     before the field is dropped (Anthropic has no equivalent).
//   - Drops every field not in anthropicAllowedFields (the upstream allowlist).
func (a *AnthropicAdapter) TransformRequest(body []byte, _ Model) ([]byte, error) {
	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("anthropic transform request: unmarshal body: %w", err)
	}

	// Capture the model name for use in TransformResponse synthesis.
	if raw, ok := doc["model"]; ok {
		var name string
		if err := jsonx.Unmarshal(raw, &name); err == nil {
			a.modelName = name
		}
	}

	// A client-supplied top-level "system" or "stop_sequences" field is
	// rejected. Both are native Anthropic fields, not part of the OpenAI
	// request surface, and are therefore never scanned by internal/pii (which
	// only covers OpenAI-shaped fields — see the "stop" and message-content
	// handling there). The adapter itself sets both fields internally, later
	// in this function, after translating system/developer messages and the
	// OpenAI "stop" field; accepting a client-supplied value here would let
	// unscanned content reach the upstream provider. This check must run
	// before that internal translation writes either key.
	if _, ok := doc["system"]; ok {
		return nil, newClientRequestError(`top-level system is not supported; send a message with role "system" instead`)
	}
	if _, ok := doc["stop_sequences"]; ok {
		return nil, newClientRequestError("stop_sequences is not supported; use stop instead")
	}

	// Validate a client-supplied top-level cache_control against the closed
	// schema (see validateCacheControl). Re-marshal the validated value — the
	// raw client bytes are never forwarded upstream verbatim.
	if raw, ok := doc["cache_control"]; ok {
		validated, err := validateCacheControl(raw)
		if err != nil {
			return nil, fmt.Errorf("anthropic transform request: top-level %w", err)
		}
		if validated == nil {
			delete(doc, "cache_control")
		} else {
			doc["cache_control"] = validated
		}
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

	// Translate parallel_tool_calls:false into tool_choice.disable_parallel_tool_use.
	// true or absent is Anthropic's default (parallel calls allowed) — no-op.
	// When tools were removed above (tool_choice:"none") or were never declared,
	// there is no tool_choice to attach the flag to, so nothing is emitted either.
	if raw, ok := doc["parallel_tool_calls"]; ok {
		var parallel bool
		if err := jsonx.Unmarshal(raw, &parallel); err != nil {
			return nil, newClientRequestError("parallel_tool_calls must be a boolean")
		}
		if !parallel {
			if _, toolsPresent := doc["tools"]; toolsPresent {
				disable := true
				if existing, ok := doc["tool_choice"]; ok {
					var tc anthropicToolChoice
					if err := jsonx.Unmarshal(existing, &tc); err != nil {
						return nil, fmt.Errorf("anthropic transform request: parallel_tool_calls: unmarshal tool_choice: %w", err)
					}
					tc.DisableParallelToolUse = &disable
					tcJSON, err := jsonx.Marshal(tc)
					if err != nil {
						return nil, fmt.Errorf("anthropic transform request: marshal tool_choice: %w", err)
					}
					doc["tool_choice"] = jsonx.RawMessage(tcJSON)
				} else {
					tcJSON, err := jsonx.Marshal(anthropicToolChoice{Type: "auto", DisableParallelToolUse: &disable})
					if err != nil {
						return nil, fmt.Errorf("anthropic transform request: marshal tool_choice: %w", err)
					}
					doc["tool_choice"] = jsonx.RawMessage(tcJSON)
				}
			}
		}
	}

	// Translate stop (string, array of strings, or null) into stop_sequences.
	// A client-supplied "stop_sequences" was already rejected above, so there
	// is never a native value to prefer here. Any shape other than string,
	// array of strings, or null is rejected fail-closed rather than silently
	// dropped.
	if raw, ok := doc["stop"]; ok {
		seqs, err := parseStopSequences(raw)
		if err != nil {
			return nil, fmt.Errorf("anthropic transform request: %w", err)
		}
		if len(seqs) > 0 {
			seqJSON, err := jsonx.Marshal(seqs)
			if err != nil {
				return nil, fmt.Errorf("anthropic transform request: marshal stop_sequences: %w", err)
			}
			doc["stop_sequences"] = jsonx.RawMessage(seqJSON)
		}
	}

	// Clamp temperature to Anthropic's 0-1 range; OpenAI allows up to 2.
	if raw, ok := doc["temperature"]; ok {
		var temp float64
		if err := jsonx.Unmarshal(raw, &temp); err == nil && temp > 1 {
			doc["temperature"] = jsonx.RawMessage("1")
		}
	}

	// Translate user into metadata.user_id. Anthropic's metadata object only
	// has a user_id field; OpenAI's free-form metadata map has no equivalent
	// and is deliberately never forwarded — metadata is always rebuilt from
	// user alone, discarding whatever the client sent in metadata itself.
	delete(doc, "metadata")
	if raw, ok := doc["user"]; ok {
		var userID string
		if err := jsonx.Unmarshal(raw, &userID); err == nil && userID != "" {
			metaJSON, err := jsonx.Marshal(struct {
				UserID string `json:"user_id"`
			}{UserID: userID})
			if err != nil {
				return nil, fmt.Errorf("anthropic transform request: marshal metadata: %w", err)
			}
			doc["metadata"] = jsonx.RawMessage(metaJSON)
		}
	}

	// Capture stream_options.include_usage before the allowlist filter drops
	// the field (Anthropic has no stream_options equivalent). When set,
	// TransformStreamLine emits a trailing usage-only chunk at message_stop.
	if raw, ok := doc["stream_options"]; ok {
		var so struct {
			IncludeUsage bool `json:"include_usage"`
		}
		if err := jsonx.Unmarshal(raw, &so); err == nil {
			a.includeUsage = so.IncludeUsage
		}
	}

	// Extract and rewrite messages.
	if raw, ok := doc["messages"]; ok {
		var msgs []anthropicIncomingMessage
		if err := jsonx.Unmarshal(raw, &msgs); err != nil {
			return nil, fmt.Errorf("anthropic transform request: unmarshal messages: %w", err)
		}

		// systemBlocks accumulates text blocks from every system/developer
		// message, in order, across the whole conversation. systemAllStrings
		// stays true only while every source message's content was a plain
		// JSON string; a single array-shaped message content flips it false
		// for the whole request, forcing the array-of-blocks output shape.
		var systemBlocks []anthropicContentBlock
		systemAllStrings := true
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
			case "system", "developer":
				// Flush any pending tool results before processing a system message
				// (should not occur in practice, but be safe). Role "developer" is
				// OpenAI's newer alias for "system" and is treated identically.
				flushToolResults()
				var wasString bool
				var err error
				systemBlocks, wasString, err = collectSystemBlocks(systemBlocks, m.Content)
				if err != nil {
					return nil, fmt.Errorf("anthropic transform request: %w", err)
				}
				if !wasString {
					systemAllStrings = false
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
							Type         string           `json:"type"`
							Text         string           `json:"text"`
							CacheControl jsonx.RawMessage `json:"cache_control,omitempty"`
						}
						if jsonx.Unmarshal(m.Content, &parts) == nil {
							// Build an array of Anthropic text blocks, one per text part.
							// Non-text part types are skipped (zero-knowledge: we only
							// forward what we understand). Per-part cache_control is
							// validated against the closed schema (see validateCacheControl)
							// and re-marshaled.
							blocks := make([]anthropicContentBlock, 0, len(parts))
							for _, p := range parts {
								if p.Type == "text" {
									cc, err := validateCacheControl(p.CacheControl)
									if err != nil {
										return nil, fmt.Errorf("anthropic transform request: tool result content: %w", err)
									}
									blocks = append(blocks, anthropicContentBlock{
										Type:         "text",
										Text:         p.Text,
										CacheControl: cc,
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
					// Content is kept as text blocks for both a plain string and an
					// array of parts, using the same part parsing (and cache_control
					// validation) as buildTextMessage — parts are never concatenated.
					// A null or absent content, or an empty string, contributes no
					// text block; the tool_use blocks alone are sufficient content.
					if m.Content != nil && !bytes.Equal(bytes.TrimSpace([]byte(m.Content)), []byte("null")) {
						var textContent string
						if err := jsonx.Unmarshal(m.Content, &textContent); err == nil {
							if textContent != "" {
								blocks = append(blocks, anthropicContentBlock{
									Type: "text",
									Text: textContent,
								})
							}
						} else {
							textBlocks, err := parseTextContentParts(m.Content)
							if err != nil {
								return nil, fmt.Errorf("anthropic transform request: %w", err)
							}
							blocks = append(blocks, textBlocks...)
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

		if len(systemBlocks) > 0 {
			// A block can only carry cache_control when its source message's
			// content was an array (the string branch of collectSystemBlocks
			// never sets it), so this check is redundant with systemAllStrings
			// today; it is kept explicit per the emission rule so the two
			// conditions cannot silently drift apart.
			hasCacheControl := false
			for _, b := range systemBlocks {
				if len(b.CacheControl) > 0 {
					hasCacheControl = true
					break
				}
			}
			var systemJSON []byte
			var err error
			if systemAllStrings && !hasCacheControl {
				// Every source message was a plain string: emit the historical
				// joined-string shape (byte-identical to prior behavior).
				texts := make([]string, len(systemBlocks))
				for i, b := range systemBlocks {
					texts[i] = b.Text
				}
				systemJSON, err = jsonx.Marshal(strings.Join(texts, "\n"))
			} else {
				systemJSON, err = jsonx.Marshal(systemBlocks)
			}
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

	// Drop every field not on the upstream allowlist. This runs last so it
	// catches every OpenAI-only field regardless of whether a translation
	// above handled it explicitly (stop, user, parallel_tool_calls,
	// stream_options, max_completion_tokens, and any future field Anthropic
	// has never heard of).
	for field := range doc {
		if _, ok := anthropicAllowedFields[field]; !ok {
			delete(doc, field)
		}
	}

	out, err := jsonx.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("anthropic transform request: marshal output: %w", err)
	}
	return out, nil
}

// TransformURL maps the OpenAI endpoint path to the equivalent Anthropic path.
// chat/completions becomes /v1/messages and models becomes /v1/models — both
// are real Anthropic API endpoints. The models mapping is exercised by the
// health-check / connection-test models-list probe (see
// internal/health.BuildProbeRequest) and by any direct, non-GET /v1/models
// request that reaches the proxy hot path (GET /v1/models is served locally
// by ModelsHandler and never reaches this adapter). All other paths are
// forwarded as-is under the configured base URL.
func (a *AnthropicAdapter) TransformURL(baseURL, upstreamPath string, _ Model) string {
	base := strings.TrimRight(baseURL, "/")
	switch upstreamPath {
	case "chat/completions":
		return base + "/v1/messages"
	case "models":
		return base + "/v1/models"
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
			// FIX 1 (parity security): reject any tool_use id or name that contains
			// or matches the PII pseudonym shape. On the non-streaming path,
			// filter.Restore performs a global string replacement over the whole body;
			// if a (malicious or compromised) upstream returns an id or name that
			// matches a pseudonym in this request's reverse map, Restore would replace
			// it with the real PII value and the client would see PII in
			// tool_calls[].id or tool_calls[].function.name.
			if isForwardedPseudonym(block.ID) {
				return nil, errors.New("anthropic transform response: tool_use id rejected")
			}
			if isForwardedPseudonym(block.Name) {
				return nil, errors.New("anthropic transform response: tool_use name rejected")
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

	// FIX 2: synthesize id and model locally rather than forwarding upstream
	// values. The upstream Anthropic id (ar.ID) and model (ar.Model) are
	// forwarded strings that pass through filter.Restore; if a malicious or
	// compromised upstream echoed a PII pseudonym into these fields, Restore
	// would substitute real PII into a structural client field. Using a proxy-
	// generated id (timestamp-based) and the client-requested model name
	// (captured from TransformRequest) prevents this leak.
	respModel := a.modelName
	if respModel == "" {
		respModel = "claude"
	}
	// Anthropic reports cache_read_input_tokens and cache_creation_input_tokens
	// IN ADDITION TO input_tokens (not as a subset — see UsageInfo's doc). Add
	// both into promptTokens here so PromptTokens carries the same
	// all-inclusive meaning for Anthropic as it does for every other
	// provider; this is the under-reporting fix for #179.
	promptTokens := ar.Usage.InputTokens + ar.Usage.CacheReadInputTokens + ar.Usage.CacheCreationInputTokens
	resp := openAIResponse{
		ID:     fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object: "chat.completion",
		Model:  respModel,
		Choices: []openAIChoice{
			{
				Index:        0,
				Message:      msg,
				FinishReason: finishReason,
			},
		},
		Usage: openAIUsage{
			PromptTokens:        promptTokens,
			CompletionTokens:    ar.Usage.OutputTokens,
			TotalTokens:         promptTokens + ar.Usage.OutputTokens,
			PromptTokensDetails: cacheUsageDetails(ar.Usage.CacheReadInputTokens, ar.Usage.CacheCreationInputTokens),
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
// maxAnthropicToolBlocks. Blocks beyond the cap abort the stream
// (errStreamTransformAborted) rather than silently dropping a tool call.
// Negative content-block indices are also rejected with abort.
//
// FIX 8: a duplicate content_block_start for an already-seen content-block
// index aborts the stream (errStreamTransformAborted) rather than silently
// overwriting or dropping the mapping. A duplicate indicates a malformed
// upstream stream and the fail-closed signal lets the handler emit a
// content-free error event instead of continuing with inconsistent state.
func (a *AnthropicAdapter) TransformStreamLine(line []byte) ([][]byte, error) {
	s := string(line)

	// Blank line — SSE event delimiter, pass through.
	if s == "" {
		return [][]byte{line}, nil
	}

	// Drop Anthropic event-type lines; OpenAI does not use them.
	if strings.HasPrefix(s, "event:") {
		return nil, nil
	}

	const dataPrefix = "data: "
	if !strings.HasPrefix(s, dataPrefix) {
		return [][]byte{line}, nil
	}

	payload := []byte(s[len(dataPrefix):])

	var event map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(payload, &event); err != nil {
		// Not valid JSON — pass through unchanged so the client can observe it.
		return [][]byte{line}, nil
	}

	var eventType string
	if raw, ok := event["type"]; ok {
		_ = jsonx.Unmarshal(raw, &eventType)
	}

	switch eventType {
	case "message_start":
		// Extract the message ID and input token counts for this stream.
		// cache_read_input_tokens and cache_creation_input_tokens are only
		// ever reported here (message_delta only updates output_tokens).
		var ms struct {
			Message struct {
				ID    string `json:"id"`
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := jsonx.Unmarshal(payload, &ms); err == nil {
			if ms.Message.ID != "" {
				a.msgID = ms.Message.ID
			}
			a.inputTokens = ms.Message.Usage.InputTokens
			a.cacheReadTokens = ms.Message.Usage.CacheReadInputTokens
			a.cacheWriteTokens = ms.Message.Usage.CacheCreationInputTokens
		}
		if a.msgID == "" {
			a.msgID = "chatcmpl-proxy"
		}
		chunk := a.buildChunk(openAIChunkDelta{Role: "assistant"}, nil)
		return [][]byte{appendDataPrefix(chunk)}, nil

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
			// Unparseable content_block_start — abort; we cannot track block state.
			return nil, errStreamTransformAborted
		}
		if cbs.ContentBlock.Type != "tool_use" {
			return nil, nil
		}

		// FIX 4: reject negative content-block indices — abort fail-closed.
		if cbs.Index < 0 {
			return nil, errStreamTransformAborted
		}
		// FIX 4: cap the total number of distinct tool_use blocks — abort.
		if a.toolCallCounter >= maxAnthropicToolBlocks {
			return nil, errStreamTransformAborted
		}

		// FIX 8: duplicate content-block index — abort (stream state is corrupt).
		if a.blockToToolCall != nil {
			if _, exists := a.blockToToolCall[cbs.Index]; exists {
				return nil, errStreamTransformAborted
			}
		}

		// FIX 1 (streaming): charset-validate tool_use id and name before registering
		// the block. Invalid values may carry PII pseudonyms; abort fail-closed.
		if cbs.ContentBlock.ID != "" && !anthropicToolIDRe.MatchString(cbs.ContentBlock.ID) {
			return nil, errStreamTransformAborted
		}
		if cbs.ContentBlock.Name != "" && !anthropicToolIDRe.MatchString(cbs.ContentBlock.Name) {
			return nil, errStreamTransformAborted
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
		return [][]byte{appendDataPrefix(chunk)}, nil

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
			// Unparseable delta — abort; content may be corrupted or malformed.
			return nil, errStreamTransformAborted
		}
		switch cbd.Delta.Type {
		case "text_delta":
			chunk := a.buildChunk(openAIChunkDelta{Content: cbd.Delta.Text}, nil)
			return [][]byte{appendDataPrefix(chunk)}, nil
		case "input_json_delta":
			// Look up the tool-call index for this content-block index.
			if a.blockToToolCall == nil {
				// input_json_delta before any content_block_start — protocol violation.
				return nil, errStreamTransformAborted
			}
			toolCallIdx, ok := a.blockToToolCall[cbd.Index]
			if !ok {
				// Delta references an unknown block index — protocol violation.
				return nil, errStreamTransformAborted
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
			return [][]byte{appendDataPrefix(chunk)}, nil
		default:
			// Unknown delta type — drop silently (no state impact).
			return nil, nil
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
			// Unparseable message_delta — abort; finish_reason would be lost.
			return nil, errStreamTransformAborted
		}
		a.outputTokens = md.Usage.OutputTokens
		reason := mapStopReason(md.Delta.StopReason)
		chunk := a.buildChunk(openAIChunkDelta{}, &reason)
		return [][]byte{appendDataPrefix(chunk)}, nil

	case "message_stop":
		// When the client requested stream_options.include_usage, emit a
		// trailing usage-only chunk before [DONE], mirroring OpenAI's final
		// streaming chunk. This is the last event of a clean stream — an
		// aborted stream never reaches this case, since an earlier
		// errStreamTransformAborted return stops the handler from calling
		// TransformStreamLine again.
		if a.includeUsage {
			if usageLine := a.buildStreamUsageChunk(); usageLine != nil {
				return [][]byte{appendDataPrefix(usageLine), []byte("data: [DONE]")}, nil
			}
		}
		return [][]byte{[]byte("data: [DONE]")}, nil

	case "ping", "content_block_stop":
		return nil, nil

	default:
		return nil, nil
	}
}

// StreamUsage returns the token counts accumulated during the Anthropic stream.
// inputTokens, cacheReadTokens, and cacheWriteTokens are captured from the
// message_start event and outputTokens from the message_delta event. All are
// zero until those events have been processed. cacheReadTokens and
// cacheWriteTokens are added into PromptTokens (see UsageInfo's doc) so the
// stream path reports the same all-inclusive prompt-token count as the
// buffered path.
func (a *AnthropicAdapter) StreamUsage() UsageInfo {
	promptTokens := a.inputTokens + a.cacheReadTokens + a.cacheWriteTokens
	return UsageInfo{
		PromptTokens:     promptTokens,
		CompletionTokens: a.outputTokens,
		TotalTokens:      promptTokens + a.outputTokens,
		CachedReadTokens: a.cacheReadTokens,
		CacheWriteTokens: a.cacheWriteTokens,
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

// buildStreamUsageChunk assembles the trailing usage-only chunk emitted at
// message_stop when the client requested stream_options.include_usage. Its
// usage numbers are computed identically to StreamUsage (reusing
// cacheUsageDetails for the cached/cache-write breakdown), and its choices
// array is present but empty, matching OpenAI's final streaming chunk shape.
// The model field falls back to "claude" when modelName was never captured
// (TransformRequest was never called or the request had no "model" field),
// matching TransformResponse's fallback.
// Returns nil on a marshal failure so the caller falls back to a bare [DONE].
func (a *AnthropicAdapter) buildStreamUsageChunk() []byte {
	id := a.msgID
	if id == "" {
		id = "chatcmpl-proxy"
	}
	ui := a.StreamUsage()
	created := time.Now().Unix()
	model := a.modelName
	if model == "" {
		model = "claude"
	}
	usage := openAIUsage{
		PromptTokens:        ui.PromptTokens,
		CompletionTokens:    ui.CompletionTokens,
		TotalTokens:         ui.TotalTokens,
		PromptTokensDetails: cacheUsageDetails(ui.CachedReadTokens, ui.CacheWriteTokens),
	}
	chunk := openAIChunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: &created,
		Model:   &model,
		Choices: []openAIChunkChoice{},
		Usage:   &usage,
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
	// FIX 4: a named tool_choice MUST reference a declared tool unconditionally.
	// If there are no declared tools at all, or the name is not among them,
	// fail-closed. A named tool_choice with no tools array is a protocol error.
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
	return &anthropicToolChoice{Type: "tool", Name: obj.Function.Name}, nil
}

// anthropicCacheControl is the closed schema for a cache_control value
// accepted from the client, whether per-part (on a system/user/assistant/
// tool-result text part) or top-level. type must be exactly "ephemeral";
// ttl, if present, must be "5m" or "1h". No other key or value is accepted.
type anthropicCacheControl struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// errInvalidCacheControl is the client-safe, static error returned for every
// cache_control validation failure in validateCacheControl. The message is a
// fixed literal — it never echoes the caller-supplied shape.
var errInvalidCacheControl = newClientRequestError(`cache_control must be {"type":"ephemeral"} with optional ttl "5m" or "1h"`)

// validateCacheControl parses raw against the closed cache_control schema
// (anthropicCacheControl) and returns the re-marshaled, validated JSON — the
// client-supplied bytes are never forwarded verbatim. A nil, empty, or JSON
// null raw value means "no cache_control was supplied" and returns (nil,
// nil). Any other shape — an unknown type, an unknown ttl, or an unrecognised
// key — is rejected fail-closed.
func validateCacheControl(raw jsonx.RawMessage) (jsonx.RawMessage, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}

	var fields map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errInvalidCacheControl
	}
	for k := range fields {
		if k != "type" && k != "ttl" {
			return nil, errInvalidCacheControl
		}
	}

	rawType, ok := fields["type"]
	if !ok {
		return nil, errInvalidCacheControl
	}
	var cc anthropicCacheControl
	if err := jsonx.Unmarshal(rawType, &cc.Type); err != nil || cc.Type != "ephemeral" {
		return nil, errInvalidCacheControl
	}

	if rawTTL, ok := fields["ttl"]; ok {
		var ttl string
		if err := jsonx.Unmarshal(rawTTL, &ttl); err != nil || (ttl != "5m" && ttl != "1h") {
			return nil, errInvalidCacheControl
		}
		cc.TTL = ttl
	}

	out, err := jsonx.Marshal(cc)
	if err != nil {
		return nil, fmt.Errorf("marshal cache_control: %w", err)
	}
	return jsonx.RawMessage(out), nil
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

// anthropicTextContentPart is a single array-content part accepted on a
// user, assistant, or tool-result text position. Only type:"text" parts are
// meaningful to Anthropic; any other type is rejected fail-closed by
// parseTextContentParts.
type anthropicTextContentPart struct {
	Type         string           `json:"type"`
	Text         string           `json:"text"`
	CacheControl jsonx.RawMessage `json:"cache_control,omitempty"`
}

// errUnsupportedContentPart is the client-safe, static error returned
// whenever a content-part type other than "text" is rejected fail-closed
// (parseTextContentParts and collectSystemBlocks). The message never echoes
// the caller-supplied type value.
var errUnsupportedContentPart = newClientRequestError("only text content parts are supported for this model")

// parseTextContentParts parses a raw JSON array of OpenAI content parts into
// Anthropic text content blocks — one block per non-empty type:"text" part,
// in order, never concatenated. Per-part cache_control is validated against
// the closed schema (validateCacheControl) and re-marshaled; the raw client
// bytes are never forwarded verbatim. Empty text parts are skipped. Any other
// part type is rejected fail-closed. Returns an error if content does not
// unmarshal as an array of objects shaped like anthropicTextContentPart.
func parseTextContentParts(content jsonx.RawMessage) ([]anthropicContentBlock, error) {
	var parts []anthropicTextContentPart
	if err := jsonx.Unmarshal(content, &parts); err != nil {
		return nil, errors.New("unrecognised content shape")
	}
	var blocks []anthropicContentBlock
	for _, p := range parts {
		if p.Type != "text" {
			// Fail-closed for unsupported part types to avoid silent corruption.
			return nil, errUnsupportedContentPart
		}
		if p.Text == "" {
			continue
		}
		cc, err := validateCacheControl(p.CacheControl)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, anthropicContentBlock{
			Type:         "text",
			Text:         p.Text,
			CacheControl: cc,
		})
	}
	return blocks, nil
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
//
// Per-part cache_control, when present on an array content part, is validated
// against the closed schema (see validateCacheControl) and re-marshaled onto
// the corresponding Anthropic text block.
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
	blocks, err := parseTextContentParts(content)
	if err != nil {
		return anthropicOutboundMessage{}, err
	}
	if len(blocks) == 0 {
		blocks = []anthropicContentBlock{{Type: "text", Text: ""}}
	}
	return anthropicOutboundMessage{
		Role:    role,
		Content: blocks,
	}, nil
}

// parseStopSequences converts an OpenAI "stop" field (a plain string, an
// array of strings, or null) into an Anthropic stop_sequences array. Empty
// or whitespace-only entries are dropped. A null value, an empty string, or
// an array that reduces to zero entries after dropping empties all yield a
// nil (empty) slice, signalling the caller to leave stop_sequences unset.
//
// Any other shape — a number, an object, or an array containing a non-string
// element — is rejected fail-closed rather than silently dropped.
func parseStopSequences(raw jsonx.RawMessage) ([]string, error) {
	var single string
	if err := jsonx.Unmarshal(raw, &single); err == nil {
		if strings.TrimSpace(single) == "" {
			return nil, nil
		}
		return []string{single}, nil
	}

	var arr []string
	if err := jsonx.Unmarshal(raw, &arr); err == nil {
		out := make([]string, 0, len(arr))
		for _, s := range arr {
			if strings.TrimSpace(s) == "" {
				continue
			}
			out = append(out, s)
		}
		return out, nil
	}

	return nil, newClientRequestError("stop must be a string or an array of strings")
}

// systemContentPart is a single array-content part accepted on a system or
// developer message. Only type:"text" parts are meaningful to Anthropic;
// any other type is rejected fail-closed by collectSystemBlocks.
type systemContentPart struct {
	Type         string           `json:"type"`
	Text         string           `json:"text"`
	CacheControl jsonx.RawMessage `json:"cache_control,omitempty"`
}

// collectSystemBlocks appends the Anthropic text block(s) for one system or
// developer message's content onto blocks, and reports whether that
// message's content was a plain JSON string (as opposed to an array of
// parts). The caller uses the returned bool, ANDed across every system/
// developer message in the request, to decide whether the final "system"
// field can be emitted as a plain joined string.
//
// A nil content field (the key was absent) contributes nothing. A plain
// string (including JSON null, which unmarshals as the empty string)
// produces exactly one block, byte-identical to the adapter's historical
// behavior. An array of content parts produces one block per non-empty
// type:"text" part — empty parts are skipped, and per-part cache_control is
// validated against the closed schema (see validateCacheControl) and
// re-marshaled. A non-text part type is rejected fail-closed, matching
// buildTextMessage's contract for user/assistant array content: forwarding a
// part type we don't understand risks silently corrupting the message.
// Any other content shape (e.g. a bare number) is also rejected fail-closed.
func collectSystemBlocks(blocks []anthropicContentBlock, content jsonx.RawMessage) ([]anthropicContentBlock, bool, error) {
	if content == nil {
		// No content field at all: contribute nothing, matching the adapter's
		// historical behavior of silently skipping a contentless system
		// message rather than treating absence the same as an explicit null.
		return blocks, true, nil
	}

	var textContent string
	if err := jsonx.Unmarshal(content, &textContent); err == nil {
		return append(blocks, anthropicContentBlock{Type: "text", Text: textContent}), true, nil
	}

	var parts []systemContentPart
	if err := jsonx.Unmarshal(content, &parts); err != nil {
		return blocks, false, errors.New("unrecognised system content shape")
	}
	for _, p := range parts {
		if p.Type != "text" {
			return blocks, false, errUnsupportedContentPart
		}
		if p.Text == "" {
			continue
		}
		cc, err := validateCacheControl(p.CacheControl)
		if err != nil {
			return blocks, false, err
		}
		blocks = append(blocks, anthropicContentBlock{
			Type:         "text",
			Text:         p.Text,
			CacheControl: cc,
		})
	}
	return blocks, false, nil
}
