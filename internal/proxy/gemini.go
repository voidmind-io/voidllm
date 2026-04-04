package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

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
}

// geminiPart is a single content part in a Gemini message.
type geminiPart struct {
	Text string `json:"text"`
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

// geminiRequest is the complete request body sent to the Gemini API.
type geminiRequest struct {
	SystemInstruction *geminiSystemInstruction `json:"systemInstruction,omitempty"`
	Contents          []geminiContent          `json:"contents"`
	GenerationConfig  geminiGenerationConfig   `json:"generationConfig,omitempty"`
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
// finish_reason equivalent.
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

// TransformRequest converts an OpenAI chat completion request body into the
// Gemini generateContent format. It:
//   - Separates system messages into a top-level systemInstruction.
//   - Converts the messages array to the Gemini contents format.
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

	// Extract and transform messages.
	type oaiMessage struct {
		Role    string           `json:"role"`
		Content jsonx.RawMessage `json:"content"`
	}

	var contents []geminiContent
	var systemParts []geminiPart

	if raw, ok := doc["messages"]; ok {
		var msgs []oaiMessage
		if err := jsonx.Unmarshal(raw, &msgs); err != nil {
			return nil, fmt.Errorf("gemini transform request: unmarshal messages: %w", err)
		}

		for _, m := range msgs {
			if m.Role == "system" {
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
				continue
			}

			geminiRole := m.Role
			if geminiRole == "assistant" {
				geminiRole = "model"
			}

			// Content may be a plain string or an array of content blocks.
			var text string
			var parts []geminiPart
			if err := jsonx.Unmarshal(m.Content, &text); err == nil {
				parts = []geminiPart{{Text: text}}
			} else {
				// Try array of content blocks: [{type:"text", text:"..."}]
				var blocks []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if err := jsonx.Unmarshal(m.Content, &blocks); err == nil {
					for _, b := range blocks {
						if b.Type == "text" && b.Text != "" {
							parts = append(parts, geminiPart{Text: b.Text})
						}
					}
				}
			}
			if len(parts) == 0 {
				// Fall back to an empty text part so the message is still included.
				parts = []geminiPart{{Text: ""}}
			}

			contents = append(contents, geminiContent{
				Role:  geminiRole,
				Parts: parts,
			})
		}
	}

	// Build generationConfig from OpenAI fields.
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

	// Build the Gemini request.
	req := geminiRequest{
		Contents:         contents,
		GenerationConfig: gc,
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
// into an OpenAI chat completion response body.
func (a *GeminiAdapter) TransformResponse(body []byte) ([]byte, error) {
	var gr geminiResponse
	if err := jsonx.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("gemini transform response: unmarshal: %w", err)
	}

	var text string
	var finishReason string
	if len(gr.Candidates) > 0 {
		c := gr.Candidates[0]
		finishReason = geminiFinishReason(c.FinishReason)
		var parts []string
		for _, p := range c.Content.Parts {
			parts = append(parts, p.Text)
		}
		text = strings.Join(parts, "")
	}
	if finishReason == "" {
		finishReason = "stop"
	}

	model := a.modelName
	if model == "" {
		model = "gemini"
	}
	resp := openAIResponse{
		ID:     fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object: "chat.completion",
		Model:  model,
		Choices: []openAIChoice{
			{
				Index: 0,
				Message: openAIMessage{
					Role:    "assistant",
					Content: text,
				},
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

	if len(gr.Candidates) > 0 {
		c := gr.Candidates[0]
		var parts []string
		for _, p := range c.Content.Parts {
			parts = append(parts, p.Text)
		}
		deltaText = strings.Join(parts, "")

		if c.FinishReason != "" {
			reason := geminiFinishReason(c.FinishReason)
			finishReason = &reason
		}
	}

	// Accumulate usage when present. When finishReason is set this is the
	// terminal chunk and we accept whatever the metadata reports, including
	// zero, so that a final zero-token count from the API is not silently
	// ignored.
	hasFinish := len(gr.Candidates) > 0 && gr.Candidates[0].FinishReason != ""
	if gr.UsageMetadata.PromptTokenCount > 0 || hasFinish {
		a.promptTokens = gr.UsageMetadata.PromptTokenCount
	}
	if gr.UsageMetadata.CandidatesTokenCount > 0 || hasFinish {
		a.completionTokens = gr.UsageMetadata.CandidatesTokenCount
	}

	// Drop content-free intermediate chunks that carry no delta and no
	// finish reason — they add no value to the client stream.
	if deltaText == "" && finishReason == nil {
		return nil
	}

	chunk := openAIChunk{
		ID:     fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object: "chat.completion.chunk",
		Choices: []openAIChunkChoice{
			{
				Index:        0,
				Delta:        openAIChunkDelta{Content: deltaText},
				FinishReason: finishReason,
			},
		},
	}

	out, err := jsonx.Marshal(chunk)
	if err != nil {
		return nil
	}

	if finishReason != nil {
		// Set flag so the blank SSE delimiter that follows becomes data: [DONE].
		a.doneSent = true
	}

	return appendDataPrefix(out)
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
