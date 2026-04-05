package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsv4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// BedrockConverseAdapter translates between the OpenAI chat completion wire
// format and the Amazon Bedrock Converse API. It handles:
//   - TransformRequest:  OpenAI messages → Bedrock ConverseRequest JSON
//   - TransformURL:      builds /model/{modelID}/converse path
//   - SetHeaders:        SigV4-signs the request in-place
//   - TransformResponse: Bedrock ConverseResponse → OpenAI chat completion JSON
//   - WrapStream:        wraps the binary AWS Event Stream body as an SSE reader
//
// A new instance must be used per request because TransformRequest and WrapStream
// capture per-request state.
type BedrockConverseAdapter struct {
	streaming    bool   // set during TransformRequest when stream:true is detected
	modelName    string // stored from TransformRequest for use in TransformResponse
	inputTokens  int
	outputTokens int
}

// bedrockConverseRequest is the Bedrock Converse API request body shape.
type bedrockConverseRequest struct {
	Messages        []bedrockMessage        `json:"messages"`
	System          []bedrockContent        `json:"system,omitempty"`
	InferenceConfig *bedrockInferenceConfig `json:"inferenceConfig,omitempty"`
}

// bedrockMessage is a single turn in the Bedrock Converse messages array.
type bedrockMessage struct {
	Role    string           `json:"role"`
	Content []bedrockContent `json:"content"`
}

// bedrockContent is a text content block.
type bedrockContent struct {
	Text string `json:"text"`
}

// bedrockInferenceConfig holds generation parameters for the Converse API.
type bedrockInferenceConfig struct {
	MaxTokens   *int     `json:"maxTokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"topP,omitempty"`
}

// bedrockConverseResponse is the non-streaming Bedrock Converse API response.
type bedrockConverseResponse struct {
	Output struct {
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	} `json:"output"`
	StopReason string `json:"stopReason"`
	Usage      struct {
		InputTokens  int `json:"inputTokens"`
		OutputTokens int `json:"outputTokens"`
		TotalTokens  int `json:"totalTokens"`
	} `json:"usage"`
}

// openAIInboundRequest is the minimal OpenAI chat completion request parsed
// to extract the fields the Bedrock translation requires.
type openAIInboundRequest struct {
	Messages            []openAIInboundMessage `json:"messages"`
	MaxTokens           *int                   `json:"max_tokens,omitempty"`
	MaxCompletionTokens *int                   `json:"max_completion_tokens,omitempty"`
	Temperature         *float64               `json:"temperature,omitempty"`
	TopP                *float64               `json:"top_p,omitempty"`
}

// openAIInboundMessage is one entry in the OpenAI messages array.
// Content is kept as a raw JSON value because the OpenAI spec allows it to be
// either a plain string or an array of content-part objects.
type openAIInboundMessage struct {
	Role    string           `json:"role"`
	Content jsonx.RawMessage `json:"content"`
}

// contentText extracts the text from an OpenAI content field that is either a
// plain JSON string or an array of content-part objects ({"type":"text","text":"…"}).
// An empty string is returned when the value cannot be decoded or contains no text.
func contentText(raw jsonx.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// Try plain string first.
	var s string
	if err := jsonx.Unmarshal(raw, &s); err == nil {
		return s
	}
	// Fall back to array of content parts.
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := jsonx.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// TransformRequest converts an OpenAI chat completion request into the Bedrock
// Converse API JSON format. System messages are extracted into the top-level
// "system" array; all other roles map directly (non-standard roles become "user").
func (a *BedrockConverseAdapter) TransformRequest(body []byte, _ Model) ([]byte, error) {
	// Extract stream and model fields from the raw document before full unmarshal
	// so they are available to TransformURL and TransformResponse.
	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("bedrock-converse transform request: unmarshal: %w", err)
	}
	if raw, ok := doc["stream"]; ok {
		var streamVal bool
		if err := jsonx.Unmarshal(raw, &streamVal); err == nil {
			a.streaming = streamVal
		}
	}
	if raw, ok := doc["model"]; ok {
		var name string
		if err := jsonx.Unmarshal(raw, &name); err == nil {
			a.modelName = name
		}
	}

	var req openAIInboundRequest
	if err := jsonx.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("bedrock-converse transform request: unmarshal: %w", err)
	}

	var systemBlocks []bedrockContent
	var messages []bedrockMessage

	for _, m := range req.Messages {
		text := contentText(m.Content)
		if m.Role == "system" {
			if text != "" {
				systemBlocks = append(systemBlocks, bedrockContent{Text: text})
			}
			continue
		}
		role := m.Role
		if role != "user" && role != "assistant" {
			role = "user"
		}
		messages = append(messages, bedrockMessage{
			Role:    role,
			Content: []bedrockContent{{Text: text}},
		})
	}

	converseReq := bedrockConverseRequest{
		Messages: messages,
		System:   systemBlocks,
	}

	// Prefer max_tokens; fall back to max_completion_tokens (OpenAI alias).
	maxTokens := req.MaxTokens
	if maxTokens == nil {
		maxTokens = req.MaxCompletionTokens
	}
	if maxTokens != nil || req.Temperature != nil || req.TopP != nil {
		converseReq.InferenceConfig = &bedrockInferenceConfig{
			MaxTokens:   maxTokens,
			Temperature: req.Temperature,
			TopP:        req.TopP,
		}
	}

	out, err := jsonx.Marshal(converseReq)
	if err != nil {
		return nil, fmt.Errorf("bedrock-converse transform request: marshal: %w", err)
	}
	return out, nil
}

// TransformURL builds the Bedrock Runtime endpoint URL for the Converse API.
// The model name is URL-path-escaped so IDs like
// "anthropic.claude-3-5-sonnet-20241022-v2:0" embed safely. When the request
// was detected as streaming during TransformRequest the converse-stream path is
// returned; otherwise the non-streaming converse path is used.
func (a *BedrockConverseAdapter) TransformURL(baseURL, _ string, model Model) string {
	base := strings.TrimRight(baseURL, "/")
	modelID := url.PathEscape(model.Name)
	endpoint := "converse"
	if a.streaming {
		endpoint = "converse-stream"
	}
	return base + "/model/" + modelID + "/" + endpoint
}

// SetHeaders signs the outbound HTTP request with AWS Signature Version 4.
// The model's AWSRegion, AWSAccessKey, and AWSSecretKey fields supply the
// signing credentials. The request body must already be populated before this
// is called because the payload hash is included in the signature.
func (a *BedrockConverseAdapter) SetHeaders(req *http.Request, model Model) {
	req.Header.Del("Authorization")
	req.Header.Set("Content-Type", "application/json")

	if model.AWSAccessKey == "" || model.AWSSecretKey == "" {
		slog.Warn("bedrock-converse: AWS credentials missing, request will be unsigned")
		return
	}

	// Read the body to compute its SHA-256 hash for the signature. The body
	// was set to a bytes.Reader by buildUpstreamRequest, so we can read and
	// replace it safely.
	var bodyHash string
	if req.Body != nil && req.Body != http.NoBody {
		bodyBytes, err := io.ReadAll(req.Body)
		if err == nil {
			bodyHash = sha256Hex(bodyBytes)
			req.Body = io.NopCloser(bytes.NewReader(bodyBytes))
			req.ContentLength = int64(len(bodyBytes))
		}
	}
	if bodyHash == "" {
		bodyHash = sha256Hex(nil)
	}

	creds := aws.Credentials{
		AccessKeyID:     model.AWSAccessKey,
		SecretAccessKey: model.AWSSecretKey,
		SessionToken:    model.AWSSessionToken,
	}

	region := model.AWSRegion
	if region == "" {
		region = "us-east-1"
	}

	signer := awsv4.NewSigner()
	if err := signer.SignHTTP(
		req.Context(), creds, req, bodyHash,
		"bedrock-runtime", region, time.Now().UTC(),
	); err != nil {
		slog.Warn("bedrock SigV4 signing failed", slog.String("error", err.Error()))
	}
}

// TransformResponse converts a non-streaming Bedrock Converse response into
// an OpenAI chat completion response.
func (a *BedrockConverseAdapter) TransformResponse(body []byte) ([]byte, error) {
	var br bedrockConverseResponse
	if err := jsonx.Unmarshal(body, &br); err != nil {
		return nil, fmt.Errorf("bedrock-converse transform response: unmarshal: %w", err)
	}

	var textParts []string
	for _, block := range br.Output.Message.Content {
		if block.Text != "" {
			textParts = append(textParts, block.Text)
		}
	}

	finishReason := mapBedrockStopReason(br.StopReason)
	totalTokens := br.Usage.TotalTokens
	if totalTokens == 0 {
		totalTokens = br.Usage.InputTokens + br.Usage.OutputTokens
	}

	a.inputTokens = br.Usage.InputTokens
	a.outputTokens = br.Usage.OutputTokens

	model := a.modelName
	if model == "" {
		model = "bedrock"
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
					Content: strings.Join(textParts, ""),
				},
				FinishReason: finishReason,
			},
		},
		Usage: openAIUsage{
			PromptTokens:     br.Usage.InputTokens,
			CompletionTokens: br.Usage.OutputTokens,
			TotalTokens:      totalTokens,
		},
	}

	out, err := jsonx.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("bedrock-converse transform response: marshal: %w", err)
	}
	return out, nil
}

// TransformStreamLine is a passthrough for lines emitted by the
// bedrockEventStreamReader, which already produces OpenAI SSE format.
func (a *BedrockConverseAdapter) TransformStreamLine(line []byte) []byte {
	return line
}

// StreamUsage returns the accumulated token counts populated by the
// bedrockEventStreamReader as it processes metadata events.
func (a *BedrockConverseAdapter) StreamUsage() UsageInfo {
	return UsageInfo{
		PromptTokens:     a.inputTokens,
		CompletionTokens: a.outputTokens,
		TotalTokens:      a.inputTokens + a.outputTokens,
	}
}

// WrapStream implements StreamWrapper. It wraps the binary AWS Event Stream
// response body and returns a reader that emits OpenAI-compatible SSE lines.
func (a *BedrockConverseAdapter) WrapStream(body io.ReadCloser) io.ReadCloser {
	return newBedrockEventStreamReader(body, a)
}

// sha256Hex returns the lowercase hex-encoded SHA-256 hash of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// mapBedrockStopReason maps a Bedrock Converse stopReason to the equivalent
// OpenAI finish_reason string.
func mapBedrockStopReason(reason string) string {
	switch reason {
	case "end_turn":
		return "stop"
	case "max_tokens":
		return "length"
	case "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}
