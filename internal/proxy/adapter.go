package proxy

import "net/http"

// UsageInfo holds token counts extracted from a completed upstream response.
// For non-streaming responses the counts come from the response JSON; for
// streaming responses they are accumulated by the adapter during
// TransformStreamLine calls.
type UsageInfo struct {
	// PromptTokens is the number of input tokens consumed.
	PromptTokens int
	// CompletionTokens is the number of output tokens produced.
	CompletionTokens int
	// TotalTokens is the sum of prompt and completion tokens.
	TotalTokens int
}

// Adapter transforms requests and responses between the client's OpenAI-compatible
// format and a provider's native API format. Every method must be safe for
// concurrent use. GetAdapter returns a new instance per call, so stateful
// adapters (e.g. AnthropicAdapter tracking a stream message ID) are safe.
type Adapter interface {
	// TransformRequest rewrites an OpenAI-format request body into the form
	// expected by the upstream provider. The model argument supplies provider-
	// specific configuration (e.g. deployment name for Azure).
	TransformRequest(body []byte, model Model) ([]byte, error)

	// TransformURL builds the full upstream URL from a base URL, the
	// upstreamPath (e.g. "chat/completions"), and model metadata.
	TransformURL(baseURL string, upstreamPath string, model Model) string

	// SetHeaders mutates req's headers to match the upstream provider's auth
	// scheme. It must remove the Authorization header when the provider uses a
	// different header (e.g. x-api-key for Anthropic, api-key for Azure).
	SetHeaders(req *http.Request, model Model)

	// TransformResponse rewrites a complete (non-streaming) upstream response
	// body into OpenAI format.
	TransformResponse(body []byte) ([]byte, error)

	// TransformStreamLine processes a single line from an upstream SSE stream.
	// It returns the (possibly rewritten) line to write to the client, or nil
	// to signal that the line should be silently dropped.
	TransformStreamLine(line []byte) []byte

	// StreamUsage returns the token counts accumulated during streaming.
	// It is only meaningful after all TransformStreamLine calls have completed.
	// Adapters that cannot extract usage from the stream return a zero UsageInfo.
	StreamUsage() UsageInfo
}

// GetAdapter returns the Adapter for the named provider, or nil for providers
// that speak the OpenAI wire format natively (passthrough). A fresh instance
// is returned on every call so that stateful streaming adapters (e.g.
// AnthropicAdapter) do not share state across concurrent requests.
func GetAdapter(provider string) Adapter {
	switch provider {
	case "anthropic":
		return &AnthropicAdapter{}
	case "azure":
		return &AzureAdapter{}
	default:
		return nil
	}
}
