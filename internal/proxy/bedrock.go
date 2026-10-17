package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/provider"
)

// bedrockEventStreamContentType is the media type of a ConverseStream response.
const bedrockEventStreamContentType = "application/vnd.amazon.eventstream"

var (
	// bedrockToolNameRe is the charset Bedrock accepts for tool names.
	bedrockToolNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	// bedrockToolUseIDRe is the charset Bedrock accepts for tool use ids.
	bedrockToolUseIDRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
	// bedrockExceptionNameRe restricts exception names read from an upstream
	// error response.
	bedrockExceptionNameRe = regexp.MustCompile(`^[A-Za-z]{1,64}$`)
)

// BedrockAdapter translates between the OpenAI chat completion wire format and
// the AWS Bedrock Converse API. An instance must not be reused across requests
// because it tracks per-request and per-stream state.
type BedrockAdapter struct {
	streaming    bool
	includeUsage bool
	modelName    string
	responseID   string

	// toolCallCounter and blockToToolCall map Bedrock content block indices to
	// OpenAI tool-call indices while streaming.
	toolCallCounter int
	blockToToolCall map[int]int

	startSeen     bool
	stoppedBlocks map[int]struct{}
	stopSeen      bool
	metadataSeen  bool

	inputTokens      int
	outputTokens     int
	cacheReadTokens  int
	cacheWriteTokens int
}

// bedrockConverseRequest is the Converse request body. It is the upstream
// allowlist: only these top-level fields can ever reach Bedrock.
type bedrockConverseRequest struct {
	Messages        []bedrockMessage        `json:"messages"`
	System          []bedrockSystemBlock    `json:"system,omitempty"`
	InferenceConfig *bedrockInferenceConfig `json:"inferenceConfig,omitempty"`
	ToolConfig      *bedrockToolConfig      `json:"toolConfig,omitempty"`
}

// bedrockMessage is one Converse message.
type bedrockMessage struct {
	Role    string                `json:"role"`
	Content []bedrockContentBlock `json:"content"`
}

// bedrockContentBlock is a union: exactly one member is non-nil.
type bedrockContentBlock struct {
	Text       *string                 `json:"text,omitempty"`
	ToolUse    *bedrockToolUseBlock    `json:"toolUse,omitempty"`
	ToolResult *bedrockToolResultBlock `json:"toolResult,omitempty"`
	CachePoint *bedrockCachePoint      `json:"cachePoint,omitempty"`
}

// bedrockSystemBlock is a union: exactly one member is non-nil.
type bedrockSystemBlock struct {
	Text       *string            `json:"text,omitempty"`
	CachePoint *bedrockCachePoint `json:"cachePoint,omitempty"`
}

// bedrockCachePoint marks a prompt cache checkpoint. Type is always
// "default"; TTL is empty (5 minutes) or "1h".
type bedrockCachePoint struct {
	Type string `json:"type"`
	TTL  string `json:"ttl,omitempty"`
}

// bedrockInferenceConfig carries the sampling parameters.
type bedrockInferenceConfig struct {
	MaxTokens     *int64   `json:"maxTokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"topP,omitempty"`
	StopSequences []string `json:"stopSequences,omitempty"`
}

// bedrockToolUseBlock is an assistant tool invocation.
type bedrockToolUseBlock struct {
	ToolUseID string           `json:"toolUseId"`
	Name      string           `json:"name"`
	Input     jsonx.RawMessage `json:"input"`
}

// bedrockToolResultBlock is the result of a tool invocation.
type bedrockToolResultBlock struct {
	ToolUseID string                     `json:"toolUseId"`
	Content   []bedrockToolResultContent `json:"content"`
}

// bedrockToolResultContent is a union: exactly one member is set.
type bedrockToolResultContent struct {
	Text *string          `json:"text,omitempty"`
	JSON jsonx.RawMessage `json:"json,omitempty"`
}

// bedrockToolConfig declares the tools and the tool choice.
type bedrockToolConfig struct {
	Tools      []bedrockTool      `json:"tools"`
	ToolChoice *bedrockToolChoice `json:"toolChoice,omitempty"`
}

// bedrockTool wraps a tool specification.
type bedrockTool struct {
	ToolSpec bedrockToolSpec `json:"toolSpec"`
}

// bedrockToolSpec describes one tool.
type bedrockToolSpec struct {
	Name        string             `json:"name"`
	Description string             `json:"description,omitempty"`
	InputSchema bedrockInputSchema `json:"inputSchema"`
}

// bedrockInputSchema wraps the JSON schema of a tool.
type bedrockInputSchema struct {
	JSON jsonx.RawMessage `json:"json"`
}

// bedrockToolChoice is a union: exactly one member is non-nil.
type bedrockToolChoice struct {
	Auto *struct{}              `json:"auto,omitempty"`
	Any  *struct{}              `json:"any,omitempty"`
	Tool *bedrockToolChoiceTool `json:"tool,omitempty"`
}

// bedrockToolChoiceTool forces a specific tool.
type bedrockToolChoiceTool struct {
	Name string `json:"name"`
}

// maxBedrockTokenCount bounds every reported token count. Larger or negative
// values mark the response as malformed and keep the sums free of overflow.
const maxBedrockTokenCount = 1 << 28

// valid reports whether every count is within 0..maxBedrockTokenCount.
func (u bedrockUsage) valid() bool {
	for _, n := range [...]int{u.InputTokens, u.OutputTokens, u.TotalTokens, u.CacheReadInputTokens, u.CacheWriteInputTokens} {
		if n < 0 || n > maxBedrockTokenCount {
			return false
		}
	}
	return true
}

// bedrockUsage is the Converse token usage object. inputTokens counts only
// the non-cached input.
type bedrockUsage struct {
	InputTokens           int `json:"inputTokens"`
	OutputTokens          int `json:"outputTokens"`
	TotalTokens           int `json:"totalTokens"`
	CacheReadInputTokens  int `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
}

// bedrockConverseResponse is the non-streaming Converse response.
type bedrockConverseResponse struct {
	Output struct {
		Message struct {
			Content []bedrockResponseBlock `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string       `json:"stopReason"`
	Usage      bedrockUsage `json:"usage"`
}

// bedrockResponseBlock is one content block of a Converse response. Blocks of
// any other kind (reasoning, citations) leave both members nil and are dropped.
type bedrockResponseBlock struct {
	Text    *string `json:"text"`
	ToolUse *struct {
		ToolUseID string           `json:"toolUseId"`
		Name      string           `json:"name"`
		Input     jsonx.RawMessage `json:"input"`
	} `json:"toolUse"`
}

// TransformRequest converts an OpenAI chat completion request body into a
// Converse request body. The upstream body is built from typed values only;
// unknown OpenAI fields are dropped and nothing from the client is forwarded
// verbatim. Text parts are never joined so PII part boundaries are preserved.
// Every client-visible error message is a fixed literal.
func (a *BedrockAdapter) TransformRequest(body []byte, _ Model) ([]byte, error) {
	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &doc); err != nil {
		return nil, errors.New("bedrock transform request: malformed body")
	}

	if raw, ok := doc["model"]; ok {
		var name string
		if err := jsonx.Unmarshal(raw, &name); err == nil {
			a.modelName = name
		}
	}
	a.responseID = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())

	if _, ok := doc["system"]; ok {
		return nil, newClientRequestError(`top-level system is not supported; send a message with role "system" instead`)
	}
	if _, ok := doc["stop_sequences"]; ok {
		return nil, newClientRequestError("stop_sequences is not supported; use stop instead")
	}

	var req bedrockConverseRequest

	if raw, ok := doc["stream"]; ok && !isJSONNull(raw) {
		var stream bool
		if err := jsonx.Unmarshal(raw, &stream); err != nil {
			return nil, newClientRequestError("stream must be a boolean")
		}
		a.streaming = stream
	}
	if raw, ok := doc["stream_options"]; ok {
		var so struct {
			IncludeUsage bool `json:"include_usage"`
		}
		if err := jsonx.Unmarshal(raw, &so); err == nil {
			a.includeUsage = so.IncludeUsage
		}
	}

	cfg, err := buildBedrockInferenceConfig(doc)
	if err != nil {
		return nil, err
	}
	req.InferenceConfig = cfg

	toolConfig, err := buildBedrockToolConfig(doc)
	if err != nil {
		return nil, err
	}
	req.ToolConfig = toolConfig

	if raw, ok := doc["parallel_tool_calls"]; ok {
		trimmed := strings.TrimSpace(string(raw))
		if trimmed != "true" && trimmed != "false" {
			return nil, newClientRequestError("parallel_tool_calls must be a boolean")
		}
	}

	rawMessages, ok := doc["messages"]
	if !ok {
		return nil, newClientRequestError("messages must be a non-empty array")
	}
	var msgs []anthropicIncomingMessage
	if err := jsonx.Unmarshal(rawMessages, &msgs); err != nil || len(msgs) == 0 {
		return nil, newClientRequestError("messages must be a non-empty array")
	}
	system, messages, err := buildBedrockMessages(msgs)
	if err != nil {
		return nil, err
	}
	req.System = system
	req.Messages = messages

	out, err := jsonx.Marshal(req)
	if err != nil {
		return nil, errors.New("bedrock transform request: marshal failed")
	}
	return out, nil
}

// buildBedrockInferenceConfig validates max_tokens, temperature, top_p and
// stop and returns the inferenceConfig, or nil when none is set.
func buildBedrockInferenceConfig(doc map[string]jsonx.RawMessage) (*bedrockInferenceConfig, error) {
	var cfg bedrockInferenceConfig
	set := false

	maxTokensRaw, hasMaxTokens := doc["max_tokens"]
	if hasMaxTokens && isJSONNull(maxTokensRaw) {
		hasMaxTokens = false
	}
	mctRaw, hasMCT := doc["max_completion_tokens"]
	if hasMCT && isJSONNull(mctRaw) {
		hasMCT = false
	}
	if !hasMaxTokens && hasMCT {
		maxTokensRaw = mctRaw
		hasMaxTokens = true
	}
	if hasMaxTokens {
		n, ok := parseJSONInt(maxTokensRaw)
		if !ok || n <= 0 {
			return nil, newClientRequestError("max_tokens must be a positive integer")
		}
		cfg.MaxTokens = &n
		set = true
	}

	if raw, ok := doc["temperature"]; ok && !isJSONNull(raw) {
		var temp float64
		if err := jsonx.Unmarshal(raw, &temp); err != nil {
			return nil, newClientRequestError("temperature must be a number")
		}
		if temp < 0 {
			return nil, newClientRequestError("temperature must be between 0 and 2")
		}
		if temp > 1 {
			temp = 1
		}
		cfg.Temperature = &temp
		set = true
	}

	if raw, ok := doc["top_p"]; ok && !isJSONNull(raw) {
		var topP float64
		if err := jsonx.Unmarshal(raw, &topP); err != nil {
			return nil, newClientRequestError("top_p must be a number")
		}
		if topP < 0 || topP > 1 {
			return nil, newClientRequestError("top_p must be between 0 and 1")
		}
		cfg.TopP = &topP
		set = true
	}

	if raw, ok := doc["stop"]; ok {
		seqs, err := parseStopSequences(raw)
		if err != nil {
			return nil, err
		}
		if len(seqs) > 0 {
			cfg.StopSequences = seqs
			set = true
		}
	}

	if !set {
		return nil, nil
	}
	return &cfg, nil
}

// buildBedrockToolConfig translates tools and tool_choice. It returns nil when
// there are no tools or when tool_choice is "none".
func buildBedrockToolConfig(doc map[string]jsonx.RawMessage) (*bedrockToolConfig, error) {
	rawTools, ok := doc["tools"]
	if !ok || isJSONNull(rawTools) {
		return nil, nil
	}
	var oaiTools []openAITool
	if err := jsonx.Unmarshal(rawTools, &oaiTools); err != nil {
		return nil, newClientRequestError("tools must be an array of function tools")
	}
	if len(oaiTools) == 0 {
		return nil, nil
	}

	tools := make([]bedrockTool, 0, len(oaiTools))
	names := make([]string, 0, len(oaiTools))
	for _, t := range oaiTools {
		if t.Type != "" && t.Type != "function" {
			return nil, newClientRequestError("unsupported tool type")
		}
		if !bedrockToolNameRe.MatchString(t.Function.Name) {
			return nil, newClientRequestError("tool function name is invalid")
		}
		schema := t.Function.Parameters
		if len(schema) == 0 || isJSONNull(schema) {
			schema = jsonx.RawMessage(`{"type":"object","properties":{}}`)
		} else if strings.TrimSpace(string(schema))[0] != '{' {
			return nil, newClientRequestError("tool parameters must be a JSON object")
		}
		names = append(names, t.Function.Name)
		tools = append(tools, bedrockTool{ToolSpec: bedrockToolSpec{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: bedrockInputSchema{JSON: schema},
		}})
	}

	cfg := &bedrockToolConfig{Tools: tools}
	if raw, ok := doc["tool_choice"]; ok && !isJSONNull(raw) {
		choice, err := translateToolChoice(raw, names)
		if err != nil {
			return nil, newClientRequestError("tool_choice is invalid")
		}
		if choice == nil {
			return nil, nil
		}
		switch choice.Type {
		case "auto":
			cfg.ToolChoice = &bedrockToolChoice{Auto: &struct{}{}}
		case "any":
			cfg.ToolChoice = &bedrockToolChoice{Any: &struct{}{}}
		case "tool":
			cfg.ToolChoice = &bedrockToolChoice{Tool: &bedrockToolChoiceTool{Name: choice.Name}}
		}
	}
	return cfg, nil
}

// buildBedrockMessages converts OpenAI messages into Converse system blocks
// and messages. Consecutive messages of the same Bedrock role are merged by
// appending their blocks; no text is ever concatenated.
func buildBedrockMessages(msgs []anthropicIncomingMessage) ([]bedrockSystemBlock, []bedrockMessage, error) {
	var system []bedrockSystemBlock
	var out []bedrockMessage

	add := func(role string, blocks []bedrockContentBlock) {
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, bedrockMessage{Role: role, Content: blocks})
	}

	for _, m := range msgs {
		switch m.Role {
		case "system", "developer":
			blocks, _, err := collectSystemBlocks(nil, m.Content)
			if err != nil {
				return nil, nil, bedrockContentError(err)
			}
			for _, b := range blocks {
				if b.Text == "" {
					continue
				}
				text := b.Text
				system = append(system, bedrockSystemBlock{Text: &text})
				if cp := bedrockCachePointFrom(b.CacheControl); cp != nil {
					system = append(system, bedrockSystemBlock{CachePoint: cp})
				}
			}

		case "user":
			blocks, err := bedrockTextBlocks(m.Content)
			if err != nil {
				return nil, nil, err
			}
			if len(blocks) == 0 {
				return nil, nil, newClientRequestError("message content must not be empty")
			}
			add("user", blocks)

		case "assistant":
			blocks, err := bedrockTextBlocks(m.Content)
			if err != nil {
				return nil, nil, err
			}
			for _, tc := range m.ToolCalls {
				if !bedrockToolUseIDRe.MatchString(tc.ID) {
					return nil, nil, newClientRequestError("tool_call id is missing or invalid")
				}
				if !bedrockToolNameRe.MatchString(tc.Function.Name) {
					return nil, nil, newClientRequestError("tool_call function name is invalid")
				}
				input, err := parseArgumentsToObject(tc.Function.Arguments)
				if err != nil {
					return nil, nil, newClientRequestError("tool_call arguments must be a JSON object")
				}
				blocks = append(blocks, bedrockContentBlock{ToolUse: &bedrockToolUseBlock{
					ToolUseID: tc.ID,
					Name:      tc.Function.Name,
					Input:     input,
				}})
			}
			if len(blocks) == 0 {
				return nil, nil, newClientRequestError("message content must not be empty")
			}
			add("assistant", blocks)

		case "tool":
			if !bedrockToolUseIDRe.MatchString(m.ToolCallID) {
				return nil, nil, newClientRequestError("tool_call_id is missing or invalid")
			}
			content, err := bedrockToolResultContents(m.Content)
			if err != nil {
				return nil, nil, err
			}
			add("user", []bedrockContentBlock{{ToolResult: &bedrockToolResultBlock{
				ToolUseID: m.ToolCallID,
				Content:   content,
			}}})

		default:
			return nil, nil, newClientRequestError("unsupported message role")
		}
	}

	if len(out) == 0 {
		return nil, nil, newClientRequestError("messages must contain a user or assistant message")
	}
	return system, out, nil
}

// bedrockContentError maps a content parsing error to a client-safe error.
// Client errors pass through; everything else gets a fixed message.
func bedrockContentError(err error) error {
	var clientErr *clientRequestError
	if errors.As(err, &clientErr) {
		return clientErr
	}
	return newClientRequestError("unsupported message content")
}

// bedrockTextBlocks converts OpenAI message content (string, array of text
// parts, or null) into text blocks, one per non-empty part, followed by a
// cachePoint after every part that carried cache_control.
func bedrockTextBlocks(content jsonx.RawMessage) ([]bedrockContentBlock, error) {
	if len(content) == 0 || isJSONNull(content) {
		return nil, nil
	}
	var s string
	if err := jsonx.Unmarshal(content, &s); err == nil {
		if s == "" {
			return nil, nil
		}
		return []bedrockContentBlock{{Text: &s}}, nil
	}
	parts, err := parseTextContentParts(content)
	if err != nil {
		return nil, bedrockContentError(err)
	}
	blocks := make([]bedrockContentBlock, 0, len(parts))
	for _, p := range parts {
		text := p.Text
		blocks = append(blocks, bedrockContentBlock{Text: &text})
		if cp := bedrockCachePointFrom(p.CacheControl); cp != nil {
			blocks = append(blocks, bedrockContentBlock{CachePoint: cp})
		}
	}
	return blocks, nil
}

// bedrockToolResultContents converts the content of a tool message into
// toolResult content items. A string becomes one text item, an array of parts
// one text item per non-empty part (boundaries preserved), and empty content
// becomes an empty JSON object.
func bedrockToolResultContents(content jsonx.RawMessage) ([]bedrockToolResultContent, error) {
	var items []bedrockToolResultContent
	if len(content) > 0 && !isJSONNull(content) {
		var s string
		if err := jsonx.Unmarshal(content, &s); err == nil {
			if s != "" {
				items = append(items, bedrockToolResultContent{Text: &s})
			}
		} else {
			parts, err := parseTextContentParts(content)
			if err != nil {
				return nil, bedrockContentError(err)
			}
			for _, p := range parts {
				text := p.Text
				items = append(items, bedrockToolResultContent{Text: &text})
			}
		}
	}
	if len(items) == 0 {
		items = []bedrockToolResultContent{{JSON: jsonx.RawMessage(`{}`)}}
	}
	return items, nil
}

// bedrockCachePointFrom converts a validated Anthropic style cache_control
// value into a Converse cachePoint. It returns nil when cc is empty. A ttl of
// "1h" is carried over; "5m" and absent map to the default lifetime.
func bedrockCachePointFrom(cc jsonx.RawMessage) *bedrockCachePoint {
	if len(cc) == 0 {
		return nil
	}
	var parsed anthropicCacheControl
	if err := jsonx.Unmarshal(cc, &parsed); err != nil {
		return nil
	}
	cp := &bedrockCachePoint{Type: "default"}
	if parsed.TTL == "1h" {
		cp.TTL = "1h"
	}
	return cp
}

// TransformURL builds the Converse or ConverseStream URL. The upstream path
// is ignored; the model ID is carried as a single percent-encoded path segment.
func (a *BedrockAdapter) TransformURL(baseURL, _ string, model Model) string {
	suffix := "/converse"
	if a.streaming {
		suffix = "/converse-stream"
	}
	return strings.TrimRight(baseURL, "/") + "/model/" + provider.EscapeBedrockModelID(model.BedrockModelID) + suffix
}

// SetHeaders keeps the Bearer Authorization header set by setUpstreamHeaders
// and fixes the content negotiation headers for the Converse API.
func (a *BedrockAdapter) SetHeaders(req *http.Request, _ Model) {
	req.Header.Set("Content-Type", "application/json")
	if a.streaming {
		req.Header.Set("Accept", bedrockEventStreamContentType)
	} else {
		req.Header.Set("Accept", "application/json")
	}
}

// StreamContentType returns the media type of a ConverseStream response.
func (a *BedrockAdapter) StreamContentType() string {
	return bedrockEventStreamContentType
}

// WrapStreamBody decodes the AWS event-stream body into SSE lines.
func (a *BedrockAdapter) WrapStreamBody(body io.ReadCloser) io.ReadCloser {
	return newEventStreamReader(body)
}

// TransformResponse converts a Converse response into an OpenAI chat
// completion. Text blocks are joined; toolUse blocks become tool_calls (with
// null content when no text is present); reasoning and unknown blocks are
// dropped. The id and model are synthesized locally.
func (a *BedrockAdapter) TransformResponse(body []byte) ([]byte, error) {
	var br bedrockConverseResponse
	if err := jsonx.Unmarshal(body, &br); err != nil {
		return nil, errors.New("bedrock transform response: malformed body")
	}

	if !br.Usage.valid() {
		return nil, errors.New("bedrock transform response: malformed body")
	}

	var textParts []string
	var toolCalls []openAIToolCall
	for _, block := range br.Output.Message.Content {
		switch {
		case block.Text != nil:
			textParts = append(textParts, *block.Text)
		case block.ToolUse != nil:
			tu := block.ToolUse
			if !bedrockToolUseIDRe.MatchString(tu.ToolUseID) || !bedrockToolNameRe.MatchString(tu.Name) {
				return nil, errors.New("bedrock transform response: tool_use id or name rejected")
			}
			if isForwardedPseudonym(tu.ToolUseID) || isForwardedPseudonym(tu.Name) {
				return nil, errors.New("bedrock transform response: tool_use id or name rejected")
			}
			args, err := serializeInputToArguments(tu.Input)
			if err != nil {
				return nil, errors.New("bedrock transform response: tool_use input rejected")
			}
			toolCalls = append(toolCalls, openAIToolCall{
				ID:   tu.ToolUseID,
				Type: "function",
				Function: openAIToolCallFunction{
					Name:      tu.Name,
					Arguments: args,
				},
			})
		}
	}

	var content *string
	if len(textParts) > 0 {
		s := strings.Join(textParts, "")
		content = &s
	} else if len(toolCalls) == 0 {
		s := ""
		content = &s
	}

	respModel := a.modelName
	if respModel == "" {
		respModel = "bedrock"
	}
	respID := a.responseID
	if respID == "" {
		respID = fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	}

	// inputTokens counts only the non-cached input; the cache buckets are
	// reported in addition, so the total prompt is the sum of all three.
	prompt := br.Usage.InputTokens + br.Usage.CacheReadInputTokens + br.Usage.CacheWriteInputTokens
	resp := openAIResponse{
		ID:     respID,
		Object: "chat.completion",
		Model:  respModel,
		Choices: []openAIChoice{{
			Index: 0,
			Message: openAIMessage{
				Role:      "assistant",
				Content:   content,
				ToolCalls: toolCalls,
			},
			FinishReason: mapBedrockStopReason(br.StopReason),
		}},
		Usage: openAIUsage{
			PromptTokens:        prompt,
			CompletionTokens:    br.Usage.OutputTokens,
			TotalTokens:         prompt + br.Usage.OutputTokens,
			PromptTokensDetails: cacheUsageDetails(br.Usage.CacheReadInputTokens, br.Usage.CacheWriteInputTokens),
		},
	}
	out, err := jsonx.Marshal(resp)
	if err != nil {
		return nil, errors.New("bedrock transform response: marshal failed")
	}
	return out, nil
}

// mapBedrockStopReason converts a Converse stopReason to an OpenAI
// finish_reason. Unknown reasons map to "stop".
func mapBedrockStopReason(reason string) string {
	switch reason {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "tool_use":
		return "tool_calls"
	case "guardrail_intervened", "content_filtered":
		return "content_filter"
	default:
		return "stop"
	}
}

// MapUpstreamError maps a Bedrock error response to a client-facing code and
// message. Every message is a fixed literal: upstream error text can contain
// account IDs, principal ARNs or request content, so none of it is forwarded.
// The result is client-facing only and must never be logged.
func (a *BedrockAdapter) MapUpstreamError(statusCode int, header http.Header, body []byte) (string, string) {
	name := bedrockExceptionName(header, body)

	switch name {
	case "ValidationException":
		if strings.Contains(bedrockErrorMessage(body), "access to bedrock models is not allowed") {
			return "permission_denied", "Bedrock model access is not enabled for this AWS account"
		}
		return "invalid_request", "Bedrock rejected the request as invalid"
	case "ExpiredTokenException":
		return "expired_api_key", "Bedrock API key has expired"
	case "UnrecognizedClientException", "InvalidSignatureException":
		return "invalid_api_key", "upstream rejected the Bedrock API key"
	case "AccessDeniedException":
		if strings.Contains(bedrockErrorMessage(body), "authentication failed") {
			return "invalid_api_key", "upstream rejected the Bedrock API key"
		}
		return "permission_denied", "Bedrock API key is not authorized for this model"
	case "ResourceNotFoundException":
		return "model_not_found", "Bedrock model or inference profile not found"
	case "ThrottlingException":
		return "rate_limit_exceeded", "Bedrock rate limit exceeded"
	case "ServiceQuotaExceededException":
		return "quota_exceeded", "Bedrock service quota exceeded"
	case "ModelNotReadyException":
		return "model_not_ready", "Bedrock model is not ready"
	case "ModelTimeoutException":
		return "upstream_timeout", "Bedrock model timed out"
	case "ModelErrorException":
		return "upstream_model_error", "Bedrock model returned an error"
	case "InternalServerException":
		return "upstream_error", "Bedrock internal error"
	case "ServiceUnavailableException":
		return "upstream_unavailable", "Bedrock service unavailable"
	}

	switch statusCode {
	case http.StatusBadRequest:
		return "invalid_request", "Bedrock rejected the request"
	case http.StatusUnauthorized:
		return "invalid_api_key", "upstream rejected the Bedrock API key"
	case http.StatusForbidden:
		return "permission_denied", "Bedrock API key is not authorized for this model"
	case http.StatusNotFound:
		return "not_found", "Bedrock resource not found"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded", "Bedrock rate limit exceeded"
	default:
		return "upstream_error", "Bedrock returned an error"
	}
}

// bedrockErrorMessage returns the lowercased upstream "message" (or "Message")
// field of an error body, or "" when absent or not a string. The text is used
// only to classify against fixed substrings and is never forwarded or logged.
func bedrockErrorMessage(body []byte) string {
	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &doc); err != nil {
		return ""
	}
	for _, key := range []string{"message", "Message"} {
		raw, ok := doc[key]
		if !ok {
			continue
		}
		var s string
		if err := jsonx.Unmarshal(raw, &s); err == nil && s != "" {
			return strings.ToLower(s)
		}
	}
	return ""
}

// bedrockExceptionName extracts the exception name from an error response.
// It comes from the X-Amzn-Errortype header (text before the first ':'), else
// from the body's __type or code field (text after the last '#'). A value that
// is not a plain identifier yields an empty name.
func bedrockExceptionName(header http.Header, body []byte) string {
	if v := header.Get("X-Amzn-Errortype"); v != "" {
		if i := strings.IndexByte(v, ':'); i >= 0 {
			v = v[:i]
		}
		if bedrockExceptionNameRe.MatchString(v) {
			return v
		}
	}

	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &doc); err != nil {
		return ""
	}
	str := func(key string) string {
		raw, ok := doc[key]
		if !ok {
			return ""
		}
		var s string
		if err := jsonx.Unmarshal(raw, &s); err != nil {
			return ""
		}
		return s
	}
	v := str("__type")
	if v == "" {
		v = str("code")
	}
	if i := strings.LastIndexByte(v, '#'); i >= 0 {
		v = v[i+1:]
	}
	if bedrockExceptionNameRe.MatchString(v) {
		return v
	}
	return ""
}
