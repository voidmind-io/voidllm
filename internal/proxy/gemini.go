package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// maxGeminiToolBlocks is the adapter-level cap on the number of distinct
// functionCall parts that may be synthesised as tool_calls in a single
// streaming response. Chunks that would exceed this limit have excess
// blocks dropped (the chunk method returns nil). This is a defence-in-depth
// DoS cap independent of any PII pipeline limits, mirroring
// maxAnthropicToolBlocks, and aligned with maxToolCallsPerChoice=64 in the
// PII Stage 0c StreamRestorer.
const maxGeminiToolBlocks = 64

// GeminiAdapter translates between the OpenAI chat completion wire format and
// the Google Gemini / Vertex AI generateContent API. An instance must not be
// reused across requests because TransformRequest and TransformStreamLine track
// per-request streaming state.
type GeminiAdapter struct {
	streaming        bool   // set during TransformRequest when stream:true is detected
	modelName        string // stored from TransformRequest for use in TransformResponse
	promptTokens     int    // accumulated from usageMetadata during streaming
	completionTokens int    // accumulated from usageMetadata during streaming
	// doneSent is true after a terminal chunk (finishReason present) has been
	// written. The blank SSE delimiter that follows is then converted to
	// data: [DONE] and this flag is cleared.
	doneSent bool
	// streamToolCallCounter is the per-stream sequential index counter for
	// synthesised tool-call ids in streaming mode. It is incremented once per
	// functionCall part emitted.
	streamToolCallCounter int
	// sawFunctionCall tracks whether any functionCall part has been emitted
	// during this stream. When true and a terminal chunk arrives, the
	// finish_reason is overridden to "tool_calls" regardless of what Gemini
	// sends (Gemini always sends STOP, never FUNCTION_CALL, for tool calls).
	sawFunctionCall bool
}

// geminiPart is a single content part in a Gemini message. Parts may carry
// plain text, a function call (model → caller), or a function response
// (caller → model after tool execution).
type geminiPart struct {
	Text             string                  `json:"text,omitempty"`
	FunctionCall     *geminiFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *geminiFunctionResponse `json:"functionResponse,omitempty"`
}

// geminiFunctionCall is a model-initiated function invocation in a Gemini part.
// Args holds an arbitrary JSON object (the call arguments). Gemini does not
// carry a tool-call id — ids are synthesised by the adapter on the response
// path.
type geminiFunctionCall struct {
	Name string           `json:"name"`
	Args jsonx.RawMessage `json:"args"`
}

// geminiFunctionResponse carries the result of a tool call sent back to the
// model. Name must match the function name of the original call. Response is
// a JSON object (Gemini requires an object, not a bare string).
type geminiFunctionResponse struct {
	Name     string           `json:"name"`
	Response jsonx.RawMessage `json:"response"`
}

// geminiContent is a single message in the Gemini contents array.
type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// geminiSystemInstruction is the top-level system instruction sent to Gemini.
type geminiSystemInstruction struct {
	Parts []geminiPart `json:"parts"`
}

// geminiGenerationConfig maps OpenAI generation parameters to the Gemini
// generationConfig object. Zero values are omitted so we do not send
// conflicting defaults.
type geminiGenerationConfig struct {
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"topP,omitempty"`
	MaxOutputTokens  *int     `json:"maxOutputTokens,omitempty"`
	StopSequences    []string `json:"stopSequences,omitempty"`
	CandidateCount   *int     `json:"candidateCount,omitempty"`
	ResponseMIMEType string   `json:"responseMimeType,omitempty"`
}

// geminiTool wraps one or more function declarations for the Gemini API.
type geminiTool struct {
	FunctionDeclarations []geminiFunctionDeclaration `json:"functionDeclarations"`
}

// geminiFunctionDeclaration describes a single callable function for Gemini.
// Parameters is the OpenAI-style JSON Schema object forwarded verbatim.
type geminiFunctionDeclaration struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Parameters  jsonx.RawMessage `json:"parameters,omitempty"`
}

// geminiToolConfig controls which tools the model may call.
type geminiToolConfig struct {
	FunctionCallingConfig geminiFunctionCallingConfig `json:"functionCallingConfig"`
}

// geminiFunctionCallingConfig specifies whether and how the model may call
// functions. Mode values: "AUTO", "ANY", "NONE".
type geminiFunctionCallingConfig struct {
	Mode                 string   `json:"mode"`
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// geminiRequest is the complete request body sent to the Gemini API.
type geminiRequest struct {
	SystemInstruction *geminiSystemInstruction `json:"systemInstruction,omitempty"`
	Contents          []geminiContent          `json:"contents"`
	GenerationConfig  geminiGenerationConfig   `json:"generationConfig,omitempty"`
	Tools             []geminiTool             `json:"tools,omitempty"`
	ToolConfig        *geminiToolConfig        `json:"toolConfig,omitempty"`
}

// geminiResponse is the non-streaming Gemini generateContent response shape.
type geminiResponse struct {
	Candidates []struct {
		Content struct {
			Role  string       `json:"role"`
			Parts []geminiPart `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// geminiFinishReason maps a Gemini finishReason string to the OpenAI
// finish_reason equivalent. Note: the Gemini API has no "FUNCTION_CALL"
// finishReason value — successful tool calls end with "STOP". The
// "tool_calls" finish_reason is set by the caller based on whether any
// functionCall parts were present, not via this function.
func geminiFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION":
		return "content_filter"
	default:
		return "stop"
	}
}

// geminiSynthesiseToolCallID produces a deterministic, charset-safe tool-call
// id for a given per-response sequential index. The format is "call_g<index>"
// which satisfies anthropicToolIDRe ([A-Za-z0-9_.+\-]+).
func geminiSynthesiseToolCallID(idx int) string {
	return fmt.Sprintf("call_g%d", idx)
}

// TransformRequest converts an OpenAI chat completion request body into the
// Gemini generateContent format. It:
//   - Separates system messages into a top-level systemInstruction.
//   - Converts the messages array to the Gemini contents format, including
//     assistant tool_calls (→ functionCall parts) and role:"tool" messages
//     (→ functionResponse parts).
//   - Translates OpenAI tools definitions to Gemini functionDeclarations.
//   - Translates tool_choice to a Gemini functionCallingConfig.
//   - Maps generation parameters (temperature, top_p, max_tokens, stop, n).
//   - Detects stream:true and stores it in adapter state for TransformURL.
func (a *GeminiAdapter) TransformRequest(body []byte, _ Model) ([]byte, error) {
	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("gemini transform request: unmarshal body: %w", err)
	}

	// Detect streaming.
	if raw, ok := doc["stream"]; ok {
		var streamVal bool
		if err := jsonx.Unmarshal(raw, &streamVal); err == nil {
			a.streaming = streamVal
		}
	}

	// Capture the model name for use in TransformResponse.
	if raw, ok := doc["model"]; ok {
		var name string
		if err := jsonx.Unmarshal(raw, &name); err == nil {
			a.modelName = name
		}
	}

	// ── Tools translation ─────────────────────────────────────────────────────
	// Collect declared tool names for tool_choice validation and the function-
	// call id → name lookup map later.
	//
	// FIX 5: retain the translated geminiTool slice and *geminiToolConfig so they
	// can be assigned directly to the geminiRequest at the end, avoiding the
	// marshal-into-doc → re-unmarshal-from-doc round-trip that was here before.
	var declaredToolNames []string
	var outTools []geminiTool
	var outToolConfig *geminiToolConfig

	if raw, ok := doc["tools"]; ok {
		var oaiTools []openAITool
		if err := jsonx.Unmarshal(raw, &oaiTools); err != nil {
			return nil, errors.New("gemini transform request: invalid tools array")
		}
		decls := make([]geminiFunctionDeclaration, 0, len(oaiTools))
		for _, t := range oaiTools {
			// Fail-closed: type must be absent or "function".
			if t.Type != "" && t.Type != "function" {
				return nil, errors.New("gemini transform request: unsupported tool type")
			}
			// Fail-closed: function name must be non-empty.
			if t.Function.Name == "" {
				return nil, errors.New("gemini transform request: tool function name is empty")
			}
			// Charset-validate function name (mirrors Anthropic adapter).
			if !anthropicToolIDRe.MatchString(t.Function.Name) {
				return nil, errors.New("gemini transform request: tool function name contains invalid characters")
			}
			// Fail-closed: parameters, if present, must be a JSON object.
			if t.Function.Parameters != nil {
				trimmed := strings.TrimSpace(string(t.Function.Parameters))
				if len(trimmed) > 0 && trimmed[0] != '{' {
					return nil, errors.New("gemini transform request: tool parameters must be a JSON object")
				}
			}
			declaredToolNames = append(declaredToolNames, t.Function.Name)
			decls = append(decls, geminiFunctionDeclaration{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				Parameters:  t.Function.Parameters,
			})
		}
		// Gemini groups all declarations into a single Tools element.
		// FIX 5: store directly; no marshal into doc needed.
		outTools = []geminiTool{{FunctionDeclarations: decls}}
	}

	// ── tool_choice translation ───────────────────────────────────────────────
	if raw, ok := doc["tool_choice"]; ok {
		tc, err := geminiTranslateToolChoice(raw, declaredToolNames)
		if err != nil {
			return nil, fmt.Errorf("gemini transform request: tool_choice: %w", err)
		}
		if tc == nil {
			// "none" → remove both tools and tool_choice from request.
			outTools = nil
			outToolConfig = nil
		} else {
			// FIX 5: store directly; no marshal into doc needed.
			outToolConfig = tc
		}
	}

	// ── Messages translation ──────────────────────────────────────────────────
	// anthropicIncomingMessage is reused here: it covers role, content,
	// tool_calls, tool_call_id — all the fields we need for Gemini too.
	type oaiMessage = anthropicIncomingMessage

	var contents []geminiContent
	var systemParts []geminiPart

	// toolCallIDToName maps a tool_call_id (from an assistant tool_calls message)
	// to the function name. This is needed when translating role:"tool" messages
	// because Gemini functionResponse requires the function name, not the id.
	toolCallIDToName := make(map[string]string)

	if raw, ok := doc["messages"]; ok {
		var msgs []oaiMessage
		if err := jsonx.Unmarshal(raw, &msgs); err != nil {
			return nil, fmt.Errorf("gemini transform request: unmarshal messages: %w", err)
		}

		for _, m := range msgs {
			switch m.Role {
			case "system":
				// System content may be a plain string or a content-block array.
				var text string
				if err := jsonx.Unmarshal(m.Content, &text); err == nil {
					systemParts = append(systemParts, geminiPart{Text: text})
				} else {
					var blocks []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					}
					if err := jsonx.Unmarshal(m.Content, &blocks); err == nil {
						for _, b := range blocks {
							if b.Type == "text" && b.Text != "" {
								systemParts = append(systemParts, geminiPart{Text: b.Text})
							}
						}
					}
				}

			case "assistant":
				if len(m.ToolCalls) > 0 {
					// Assistant message with tool_calls → Gemini model content with
					// functionCall parts. Build the id→name map at the same time.
					var parts []geminiPart

					// Include a text part first if there is non-empty text content.
					if m.Content != nil {
						var textContent string
						if err := jsonx.Unmarshal(m.Content, &textContent); err == nil && textContent != "" {
							parts = append(parts, geminiPart{Text: textContent})
						}
					}

					for _, tc := range m.ToolCalls {
						// FIX 3: reject empty tool_call_id — fail-closed.
						if tc.ID == "" {
							return nil, errors.New("gemini transform request: tool_call id is empty")
						}
						// Charset-validate tool_call id.
						if !anthropicToolIDRe.MatchString(tc.ID) {
							return nil, errors.New("gemini transform request: tool_call id contains invalid characters")
						}
						// FIX 3: reject empty function name — fail-closed.
						if tc.Function.Name == "" {
							return nil, errors.New("gemini transform request: tool_call function name is empty")
						}
						// Charset-validate function name (defence-in-depth).
						if !anthropicToolIDRe.MatchString(tc.Function.Name) {
							return nil, errors.New("gemini transform request: tool_call function name contains invalid characters")
						}
						// Reject pseudonym-shaped function names on the request path
						// to prevent forwarding names that could be mistaken for PII pseudonyms.
						if isForwardedPseudonym(tc.Function.Name) {
							return nil, errors.New("gemini transform request: tool_call function name rejected")
						}
						// Parse arguments JSON string → object.
						// Empty string is treated as {} (same as Anthropic adapter).
						args, err := parseArgumentsToObject(tc.Function.Arguments)
						if err != nil {
							return nil, errors.New("gemini transform request: invalid tool_call arguments")
						}
						// FIX 3: reject duplicate id → conflicting name mappings.
						// Silently overwriting would corrupt conversation state.
						if existing, ok := toolCallIDToName[tc.ID]; ok && existing != tc.Function.Name {
							return nil, errors.New("gemini transform request: duplicate tool_call_id with conflicting function name")
						}
						// Record id → name for later tool-result correlation.
						toolCallIDToName[tc.ID] = tc.Function.Name
						// Gemini functionCall has no id field — do not forward the OpenAI id.
						parts = append(parts, geminiPart{
							FunctionCall: &geminiFunctionCall{
								Name: tc.Function.Name,
								Args: args,
							},
						})
					}

					contents = append(contents, geminiContent{
						Role:  "model",
						Parts: parts,
					})
				} else {
					// Plain text assistant message.
					parts := geminiTextParts(m.Content)
					contents = append(contents, geminiContent{
						Role:  "model",
						Parts: parts,
					})
				}

			case "tool":
				// OpenAI role:"tool" → Gemini user content with functionResponse part.
				//
				// API convention note: Gemini v1beta uses role "user" for
				// functionResponse parts. This matches the official Gemini REST API
				// documentation (https://ai.google.dev/api/generate-content#v1beta.Content)
				// where function responses are sent as part of the user turn. We
				// accumulate consecutive tool messages into a single user content to
				// keep the conversation well-formed (Gemini does not require merging
				// but it is cleaner; multiple consecutive functionResponse parts in one
				// user content is accepted by the API).
				//
				// ASSUMPTION: consecutive role:"tool" messages are merged into a single
				// Gemini "user" content block (multiple functionResponse parts). Gemini
				// accepts this form per the v1beta spec. If a future API version
				// requires one content per functionResponse, this loop must be split.
				//
				// Zero-knowledge preservation: if the tool result content is a string,
				// wrap as {"content": <string>}. If it is an array of content parts,
				// wrap as {"content": [<text1>, <text2>, ...]} — preserving part
				// boundaries exactly, never concatenating (same reasoning as the
				// Anthropic adapter fix: concatenation can reconstruct deliberately
				// split PII that the scanner scanned as separate parts).

				// FIX 3: reject empty tool_call_id — fail-closed. An empty id
				// cannot be correlated to a function name.
				if m.ToolCallID == "" {
					return nil, errors.New("gemini transform request: tool message has empty tool_call_id")
				}
				// Charset-validate tool_call_id before lookup.
				if !anthropicToolIDRe.MatchString(m.ToolCallID) {
					return nil, errors.New("gemini transform request: tool_call_id contains invalid characters")
				}

				// Fail-closed when tool_call_id is not in the id→name map.
				// Silently using an empty function name would produce a malformed
				// functionResponse that the Gemini API may accept but would
				// silently corrupt the conversation state.
				var funcName string
				{
					name, ok := toolCallIDToName[m.ToolCallID]
					if !ok {
						return nil, errors.New("gemini transform request: tool_call_id references unknown tool call")
					}
					funcName = name
				}

				response, err := geminiWrapToolResult(m.Content)
				if err != nil {
					return nil, errors.New("gemini transform request: cannot wrap tool result")
				}

				// Append to the most recent user content if it already contains
				// functionResponse parts; otherwise create a new user content.
				frPart := geminiPart{
					FunctionResponse: &geminiFunctionResponse{
						Name:     funcName,
						Response: response,
					},
				}
				if len(contents) > 0 && contents[len(contents)-1].Role == "user" &&
					hasOnlyFunctionResponseParts(contents[len(contents)-1].Parts) {
					// Merge into the existing user content.
					contents[len(contents)-1].Parts = append(contents[len(contents)-1].Parts, frPart)
				} else {
					contents = append(contents, geminiContent{
						Role:  "user",
						Parts: []geminiPart{frPart},
					})
				}

			default:
				// user and any other roles: emit as plain text content.
				geminiRole := m.Role
				if geminiRole == "assistant" {
					geminiRole = "model"
				}
				parts := geminiTextParts(m.Content)
				contents = append(contents, geminiContent{
					Role:  geminiRole,
					Parts: parts,
				})
			}
		}
	}

	// ── generationConfig ──────────────────────────────────────────────────────
	var gc geminiGenerationConfig

	if raw, ok := doc["temperature"]; ok {
		var v float64
		if err := jsonx.Unmarshal(raw, &v); err == nil {
			gc.Temperature = &v
		}
	}
	if raw, ok := doc["top_p"]; ok {
		var v float64
		if err := jsonx.Unmarshal(raw, &v); err == nil {
			gc.TopP = &v
		}
	}
	// max_completion_tokens takes precedence over max_tokens.
	if raw, ok := doc["max_completion_tokens"]; ok {
		var v int
		if err := jsonx.Unmarshal(raw, &v); err == nil {
			gc.MaxOutputTokens = &v
		}
	} else if raw, ok := doc["max_tokens"]; ok {
		var v int
		if err := jsonx.Unmarshal(raw, &v); err == nil {
			gc.MaxOutputTokens = &v
		}
	}
	if raw, ok := doc["stop"]; ok {
		// stop may be a string or an array of strings.
		var single string
		if err := jsonx.Unmarshal(raw, &single); err == nil {
			gc.StopSequences = []string{single}
		} else {
			var arr []string
			if err := jsonx.Unmarshal(raw, &arr); err == nil {
				gc.StopSequences = arr
			}
		}
	}
	if raw, ok := doc["n"]; ok {
		var v int
		if err := jsonx.Unmarshal(raw, &v); err == nil {
			gc.CandidateCount = &v
		}
	}
	if raw, ok := doc["response_format"]; ok {
		var rf struct {
			Type string `json:"type"`
		}
		if err := jsonx.Unmarshal(raw, &rf); err == nil {
			switch rf.Type {
			case "json_object":
				gc.ResponseMIMEType = "application/json"
				// json_schema with responseSchema mapping is not yet supported.
			}
		}
	}

	// ── Build final Gemini request ────────────────────────────────────────────
	// FIX 5: outTools and outToolConfig were built and retained directly above
	// during the tools/tool_choice translation steps — no re-unmarshal needed.

	req := geminiRequest{
		Contents:         contents,
		GenerationConfig: gc,
		Tools:            outTools,
		ToolConfig:       outToolConfig,
	}
	if len(systemParts) > 0 {
		req.SystemInstruction = &geminiSystemInstruction{Parts: systemParts}
	}

	out, err := jsonx.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("gemini transform request: marshal: %w", err)
	}
	return out, nil
}

// TransformURL builds the full Gemini or Vertex AI generateContent URL.
// For the Gemini API the form is:
//
//	{baseURL}/v1beta/models/{model}:generateContent
//
// For Vertex AI (model.Provider == "vertex") the form is:
//
//	{baseURL}/v1/projects/{project}/locations/{location}/publishers/google/models/{model}:generateContent
//
// Streaming variants replace "generateContent" with
// "streamGenerateContent?alt=sse".
func (a *GeminiAdapter) TransformURL(baseURL, _ string, model Model) string {
	base := strings.TrimRight(baseURL, "/")

	isVertex := model.Provider == "vertex"

	endpoint := "generateContent"
	if a.streaming {
		endpoint = "streamGenerateContent?alt=sse"
	}

	if isVertex {
		return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:%s",
			base, model.GCPProject, model.GCPLocation, model.Name, endpoint)
	}

	return fmt.Sprintf("%s/v1beta/models/%s:%s", base, model.Name, endpoint)
}

// SetHeaders configures the outbound request for the Gemini or Vertex AI API.
// Vertex AI uses the Bearer Authorization header that setUpstreamHeaders already
// sets. The Gemini API uses the x-goog-api-key header instead.
func (a *GeminiAdapter) SetHeaders(req *http.Request, model Model) {
	isVertex := model.Provider == "vertex"
	if isVertex {
		// Vertex AI uses Bearer token — keep the Authorization header as set.
		return
	}
	// Gemini API authenticates via x-goog-api-key, not Bearer.
	req.Header.Del("Authorization")
	if model.APIKey != "" {
		req.Header.Set("x-goog-api-key", model.APIKey)
	}
}

// TransformResponse converts a complete Gemini generateContent response body
// into an OpenAI chat completion response body. It handles both plain text
// parts and functionCall parts. When functionCall parts are present they are
// translated to OpenAI tool_calls with synthesised ids; if no text parts are
// present the message content is null (pointer nil).
func (a *GeminiAdapter) TransformResponse(body []byte) ([]byte, error) {
	var gr geminiResponse
	if err := jsonx.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("gemini transform response: unmarshal: %w", err)
	}

	var textParts []string
	var toolCalls []openAIToolCall
	var finishReason string

	if len(gr.Candidates) > 0 {
		c := gr.Candidates[0]

		tcIdx := 0
		for _, p := range c.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				// FIX 3: reject empty function name — fail-closed.
				if p.FunctionCall.Name == "" {
					return nil, errors.New("gemini transform response: function name is empty")
				}
				// Charset-validate the function name received from upstream
				// (defence-in-depth: the model might hallucinate names).
				if !anthropicToolIDRe.MatchString(p.FunctionCall.Name) {
					return nil, errors.New("gemini transform response: function name contains invalid characters")
				}
				// Reject any function name that matches the PII pseudonym shape.
				// On the non-streaming path, filter.Restore performs a global
				// string replacement over the whole body; if a (malicious or
				// compromised) upstream returns a function name that matches a
				// pseudonym in this request's reverse map, Restore would replace
				// it with the real PII value and the client would see PII in
				// tool_calls[].function.name.
				if isForwardedPseudonym(p.FunctionCall.Name) {
					return nil, errors.New("gemini transform response: function name rejected")
				}
				// Serialise args object → JSON string for OpenAI function.arguments.
				argsStr, err := serializeInputToArguments(p.FunctionCall.Args)
				if err != nil {
					return nil, errors.New("gemini transform response: invalid function call args")
				}
				id := geminiSynthesiseToolCallID(tcIdx)
				toolCalls = append(toolCalls, openAIToolCall{
					ID:   id,
					Type: "function",
					Function: openAIToolCallFunction{
						Name:      p.FunctionCall.Name,
						Arguments: argsStr,
					},
				})
				tcIdx++
			case p.Text != "":
				textParts = append(textParts, p.Text)
			}
		}

		// FIX 1: derive finish_reason from functionCall PRESENCE, not from
		// Gemini's finishReason string. Gemini uses "STOP" for successful tool
		// calls (the API has no "FUNCTION_CALL" finishReason value). Any
		// response that produced at least one tool call must finish with
		// "tool_calls" so the PII Stage 0c restorer accepts it.
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		} else {
			finishReason = geminiFinishReason(c.FinishReason)
		}
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	model := a.modelName
	if model == "" {
		model = "gemini"
	}

	// Build the message. When tool_calls are present and no text was produced,
	// content is null (nil pointer). When text is present, content is the
	// joined string. This mirrors the Anthropic adapter's behaviour.
	var msgContent *string
	if len(textParts) > 0 {
		s := strings.Join(textParts, "")
		msgContent = &s
	} else if len(toolCalls) == 0 {
		// No text and no tool calls — preserve empty string for consistency.
		s := ""
		msgContent = &s
	}
	// When toolCalls is non-empty and textParts is empty, msgContent stays nil.

	msg := openAIMessage{
		Role:    "assistant",
		Content: msgContent,
	}
	if len(toolCalls) > 0 {
		msg.ToolCalls = toolCalls
	}

	resp := openAIResponse{
		ID:     fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object: "chat.completion",
		Model:  model,
		Choices: []openAIChoice{
			{
				Index:        0,
				Message:      msg,
				FinishReason: finishReason,
			},
		},
		Usage: openAIUsage{
			PromptTokens:     gr.UsageMetadata.PromptTokenCount,
			CompletionTokens: gr.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      gr.UsageMetadata.TotalTokenCount,
		},
	}

	out, err := jsonx.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("gemini transform response: marshal: %w", err)
	}
	return out, nil
}

// TransformStreamLine processes one raw SSE line from the Gemini
// streamGenerateContent stream and returns the equivalent OpenAI SSE line,
// or nil to drop the line.
//
// Gemini SSE lines carry full generateContent response objects (not deltas).
// Each data payload is mapped to an OpenAI chat.completion.chunk. When the
// candidate includes a finishReason, the final content chunk is emitted. The
// blank SSE delimiter that immediately follows the terminal chunk is converted
// into the data: [DONE] terminator that OpenAI clients expect.
//
// For chunks that contain functionCall parts: a tool_calls delta is emitted
// per functionCall, conformant to the OpenAI Stage 0c shape expected by the
// PII StreamRestorer. Each delta carries index, id, type:"function",
// function.name, and function.arguments (the full args JSON string — Gemini
// delivers arguments complete, not fragmented). This is 0c-conformant: the
// first observation carries id + type + name + arguments together; Stage 0c's
// handleToolCallsDelta restores arguments in one delta.
//
// The content-free-chunk drop (~line 441 in the original) is corrected: a
// chunk that carries only functionCall parts (no text) is NOT dropped.
func (a *GeminiAdapter) TransformStreamLine(line []byte) []byte {
	s := string(line)

	// Blank SSE event delimiter.
	if s == "" {
		if a.doneSent {
			// The blank line that follows the terminal chunk: emit [DONE] here
			// so the client sees it as a properly delimited event, then clear
			// the flag so subsequent blank lines pass through normally.
			a.doneSent = false
			return []byte("data: [DONE]")
		}
		return line
	}

	const dataPrefix = "data: "
	if !strings.HasPrefix(s, dataPrefix) {
		return nil
	}

	payload := []byte(s[len(dataPrefix):])

	// Gemini may send "[DONE]" itself in some environments — pass it through.
	if strings.TrimSpace(string(payload)) == "[DONE]" {
		return line
	}

	var gr geminiResponse
	if err := jsonx.Unmarshal(payload, &gr); err != nil {
		// Not a valid Gemini JSON payload — drop the line.
		return nil
	}

	var deltaText string
	var finishReason *string
	var toolCallParts []*geminiFunctionCall

	if len(gr.Candidates) > 0 {
		c := gr.Candidates[0]
		for _, p := range c.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				toolCallParts = append(toolCallParts, p.FunctionCall)
			case p.Text != "":
				deltaText += p.Text
			}
		}

		if c.FinishReason != "" {
			// FIX 1: derive finish_reason from sawFunctionCall presence, not
			// from Gemini's finishReason string. Gemini always sends "STOP" for
			// a successful tool call. We track sawFunctionCall across the
			// stream; if any functionCall parts have been emitted (including in
			// THIS chunk, handled below after toolCallParts are processed), we
			// override to "tool_calls".
			//
			// For the same-chunk case (functionCall parts + finishReason STOP
			// in one chunk), the toolCallParts slice is already populated at
			// this point, so we use len(toolCallParts) > 0 || a.sawFunctionCall
			// to cover both cases correctly.
			var reason string
			if len(toolCallParts) > 0 || a.sawFunctionCall {
				reason = "tool_calls"
			} else {
				reason = geminiFinishReason(c.FinishReason)
			}
			finishReason = &reason
		}
	}

	// Accumulate usage when present.
	hasFinish := len(gr.Candidates) > 0 && gr.Candidates[0].FinishReason != ""
	if gr.UsageMetadata.PromptTokenCount > 0 || hasFinish {
		a.promptTokens = gr.UsageMetadata.PromptTokenCount
	}
	if gr.UsageMetadata.CandidatesTokenCount > 0 || hasFinish {
		a.completionTokens = gr.UsageMetadata.CandidatesTokenCount
	}

	// Drop content-free intermediate chunks that carry no delta text, no tool
	// calls, and no finish reason — they add no value to the client stream.
	// NOTE: a functionCall-only chunk (no text) must NOT be dropped; the
	// original check "deltaText == "" && finishReason == nil" was broadened here
	// to also require len(toolCallParts) == 0. This is the content-free-drop fix.
	if deltaText == "" && finishReason == nil && len(toolCallParts) == 0 {
		return nil
	}

	// If there are both text content and tool call parts in the same chunk,
	// emit a text chunk first, then the tool call chunk. However, Gemini
	// typically does not mix text and functionCall in the same chunk. For
	// simplicity (and because Stage 0c rejects mixed content+tool_calls in the
	// same chunk), emit text chunks via the normal path and tool call chunks
	// via a separate emission. When both are present, emit text first by
	// building a text-only delta here and handling tool calls below.
	//
	// In practice Gemini always sends one finishReason with either the last
	// text part or the functionCall parts — not both simultaneously.
	if deltaText != "" {
		chunk := openAIChunk{
			ID:     fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
			Object: "chat.completion.chunk",
			Choices: []openAIChunkChoice{
				{
					Index: 0,
					Delta: openAIChunkDelta{Content: deltaText},
					// Do not set finishReason here when there are also tool calls
					// to emit; set it on the tool-calls chunk below.
					FinishReason: func() *string {
						if len(toolCallParts) == 0 {
							return finishReason
						}
						return nil
					}(),
				},
			},
		}
		out, err := jsonx.Marshal(chunk)
		if err != nil {
			return nil
		}
		if finishReason != nil && len(toolCallParts) == 0 {
			a.doneSent = true
		}
		// If there are also tool call parts, we cannot return here — we need
		// to emit both. For the common case (only text), return now.
		if len(toolCallParts) == 0 {
			return appendDataPrefix(out)
		}
		// Mixed chunk: text has been built. Fall through to emit tool calls
		// as a separate chunk. (We discard the text chunk here — in the rare
		// mixed case only the tool calls chunk is returned. This is acceptable
		// because Stage 0c cannot handle mixed content+tool_calls in one chunk,
		// and Gemini should not produce this combination in practice.)
		_ = out
	}

	// Emit tool call deltas.
	if len(toolCallParts) > 0 {
		var tcs []openAIToolCall
		for _, fc := range toolCallParts {
			// Cap per-stream tool-call count (defence-in-depth).
			if a.streamToolCallCounter >= maxGeminiToolBlocks {
				// Excess blocks are dropped.
				break
			}
			// FIX 3: reject empty function name — fail-closed (drop this block).
			if fc.Name == "" {
				a.streamToolCallCounter++
				continue
			}
			// Charset-validate the name received from upstream.
			if !anthropicToolIDRe.MatchString(fc.Name) {
				// Invalid name — drop this block, continue with others.
				a.streamToolCallCounter++
				continue
			}
			// Serialise args → JSON string.
			argsStr, err := serializeInputToArguments(fc.Args)
			if err != nil {
				a.streamToolCallCounter++
				continue
			}
			id := geminiSynthesiseToolCallID(a.streamToolCallCounter)
			tcIdx := a.streamToolCallCounter
			a.streamToolCallCounter++

			tcs = append(tcs, openAIToolCall{
				Index: &tcIdx,
				ID:    id,
				Type:  "function",
				Function: openAIToolCallFunction{
					Name:      fc.Name,
					Arguments: argsStr,
				},
			})
		}
		// FIX 1: mark that this stream has emitted at least one functionCall
		// tool call delta. This is used by the finish_reason override logic to
		// emit "tool_calls" even when Gemini sends finishReason "STOP".
		if len(tcs) > 0 {
			a.sawFunctionCall = true
		}

		if len(tcs) > 0 {
			chunk := openAIChunk{
				ID:     fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
				Object: "chat.completion.chunk",
				Choices: []openAIChunkChoice{
					{
						Index:        0,
						Delta:        openAIChunkDelta{ToolCalls: tcs},
						FinishReason: finishReason,
					},
				},
			}
			out, err := jsonx.Marshal(chunk)
			if err != nil {
				return nil
			}
			if finishReason != nil {
				a.doneSent = true
			}
			return appendDataPrefix(out)
		}

		// All tool call parts were dropped (invalid names or args). If we have a
		// finishReason, still need to emit the finish chunk with no delta.
		if finishReason != nil {
			chunk := openAIChunk{
				ID:     fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
				Object: "chat.completion.chunk",
				Choices: []openAIChunkChoice{
					{
						Index:        0,
						Delta:        openAIChunkDelta{},
						FinishReason: finishReason,
					},
				},
			}
			out, err := jsonx.Marshal(chunk)
			if err != nil {
				return nil
			}
			a.doneSent = true
			return appendDataPrefix(out)
		}
		return nil
	}

	// Pure finish-reason chunk (no text, no tool calls) — emit finish chunk.
	if finishReason != nil {
		chunk := openAIChunk{
			ID:     fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
			Object: "chat.completion.chunk",
			Choices: []openAIChunkChoice{
				{
					Index:        0,
					Delta:        openAIChunkDelta{},
					FinishReason: finishReason,
				},
			},
		}
		out, err := jsonx.Marshal(chunk)
		if err != nil {
			return nil
		}
		a.doneSent = true
		return appendDataPrefix(out)
	}

	return nil
}

// StreamUsage returns the token counts accumulated during the Gemini stream.
// Both fields are zero until the final stream chunk (carrying usageMetadata)
// has been processed by TransformStreamLine.
func (a *GeminiAdapter) StreamUsage() UsageInfo {
	return UsageInfo{
		PromptTokens:     a.promptTokens,
		CompletionTokens: a.completionTokens,
		TotalTokens:      a.promptTokens + a.completionTokens,
	}
}

// ── Helper functions ──────────────────────────────────────────────────────────

// geminiTextParts extracts plain-text geminiParts from a raw OpenAI content
// value (string or array of text blocks). A null or nil content yields a
// single empty-text part so the message is well-formed.
func geminiTextParts(content jsonx.RawMessage) []geminiPart {
	if content == nil {
		return []geminiPart{{Text: ""}}
	}
	// Try plain string first.
	var text string
	if err := jsonx.Unmarshal(content, &text); err == nil {
		return []geminiPart{{Text: text}}
	}
	// Try array of content blocks.
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := jsonx.Unmarshal(content, &blocks); err == nil {
		var parts []geminiPart
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, geminiPart{Text: b.Text})
			}
		}
		if len(parts) > 0 {
			return parts
		}
	}
	// Fall back to a single empty text part.
	return []geminiPart{{Text: ""}}
}

// geminiWrapToolResult converts an OpenAI tool-result content value into a
// Gemini functionResponse.response JSON object.
//
// Zero-knowledge contract: the content boundaries established by the PII
// scanner are preserved. For a plain-string result, the wrapper is
// {"content": <string>}. For an array-of-parts result, the wrapper is
// {"content": [<text1>, <text2>, ...]} — never concatenated into one string,
// because concatenation could reconstruct PII that was deliberately split
// across parts (same reasoning as the Anthropic tool_result fix in PR #138).
func geminiWrapToolResult(content jsonx.RawMessage) (jsonx.RawMessage, error) {
	if content == nil {
		// Null/absent content → empty wrapper.
		return jsonx.RawMessage(`{"content":""}`), nil
	}

	// Try plain string.
	var s string
	if err := jsonx.Unmarshal(content, &s); err == nil {
		type wrapper struct {
			Content string `json:"content"`
		}
		b, merr := jsonx.Marshal(wrapper{Content: s})
		if merr != nil {
			return nil, merr
		}
		return jsonx.RawMessage(b), nil
	}

	// Try array of content parts — preserve as list, never concatenate.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := jsonx.Unmarshal(content, &parts); err == nil {
		// FIX 3: initialise to an empty slice (not nil) so that zero text-part
		// arrays serialise as {"content":[]} rather than {"content":null}.
		// An all-image or all-non-text array legitimately produces zero text
		// items; null would be structurally incorrect for a list field.
		texts := make([]string, 0, len(parts))
		for _, p := range parts {
			if p.Type == "text" {
				texts = append(texts, p.Text)
			}
		}
		type wrapper struct {
			Content []string `json:"content"`
		}
		b, merr := jsonx.Marshal(wrapper{Content: texts})
		if merr != nil {
			return nil, merr
		}
		return jsonx.RawMessage(b), nil
	}

	return nil, errors.New("unrecognised tool result content shape")
}

// hasOnlyFunctionResponseParts reports whether all parts in the given slice
// are functionResponse parts. Used to decide whether to merge consecutive
// tool-result messages into the same user content block.
func hasOnlyFunctionResponseParts(parts []geminiPart) bool {
	if len(parts) == 0 {
		return false
	}
	for _, p := range parts {
		if p.FunctionResponse == nil {
			return false
		}
	}
	return true
}

// geminiTranslateToolChoice converts an OpenAI tool_choice value to a Gemini
// geminiToolConfig. Returns nil when the effective choice is "none" (caller
// should remove tools and tool_choice). Returns an error on unknown values
// (fail-closed — no silent fallback).
//
// Mapping:
//
//	"auto"                          → Mode "AUTO"
//	"required"                      → Mode "ANY"
//	"none"                          → nil (remove tools)
//	{type:"function",function:{name}} → Mode "ANY" + AllowedFunctionNames:[name]
func geminiTranslateToolChoice(raw jsonx.RawMessage, declaredNames []string) (*geminiToolConfig, error) {
	// Try string first.
	var s string
	if err := jsonx.Unmarshal(raw, &s); err == nil {
		switch s {
		case "auto":
			return &geminiToolConfig{
				FunctionCallingConfig: geminiFunctionCallingConfig{Mode: "AUTO"},
			}, nil
		case "required":
			return &geminiToolConfig{
				FunctionCallingConfig: geminiFunctionCallingConfig{Mode: "ANY"},
			}, nil
		case "none":
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
	// Charset-validate the named function (mirrors Anthropic adapter; may carry PII pseudonym).
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
	return &geminiToolConfig{
		FunctionCallingConfig: geminiFunctionCallingConfig{
			Mode:                 "ANY",
			AllowedFunctionNames: []string{obj.Function.Name},
		},
	}, nil
}
