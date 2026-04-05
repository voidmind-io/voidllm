package proxy

import (
	"net/http"
	"strings"
)

// BedrockAdapter adapts requests for Amazon Bedrock endpoints that expose an
// OpenAI-compatible API surface (e.g. bedrock-mantle or custom OpenAI-shim
// deployments). The request and response bodies are passed through unchanged;
// only the URL construction differs from the plain OpenAI passthrough.
//
// URL rules:
//   - Hostname contains "bedrock-mantle" → {baseURL}/v1/{upstreamPath}
//   - Any other Bedrock endpoint          → {baseURL}/openai/v1/{upstreamPath}
//
// Authentication uses the standard Bearer token from the Authorization header,
// so no header rewriting is required.
type BedrockAdapter struct{}

// TransformRequest returns the body unchanged; these Bedrock OpenAI-shim
// endpoints accept the OpenAI request format natively.
func (a *BedrockAdapter) TransformRequest(body []byte, _ Model) ([]byte, error) {
	return body, nil
}

// TransformURL builds the upstream URL. The path prefix depends on whether the
// base URL host contains "bedrock-mantle" (uses "/v1/") or not (uses "/openai/v1/").
func (a *BedrockAdapter) TransformURL(baseURL, upstreamPath string, _ Model) string {
	base := strings.TrimRight(baseURL, "/")
	if strings.Contains(base, "bedrock-mantle") {
		return base + "/v1/" + upstreamPath
	}
	return base + "/openai/v1/" + upstreamPath
}

// SetHeaders is a no-op; Bearer token forwarding is handled by the proxy core.
func (a *BedrockAdapter) SetHeaders(_ *http.Request, _ Model) {}

// TransformResponse returns the body unchanged; the shim returns OpenAI-format
// responses natively.
func (a *BedrockAdapter) TransformResponse(body []byte) ([]byte, error) {
	return body, nil
}

// TransformStreamLine returns the line unchanged; the shim streams in OpenAI
// SSE format natively.
func (a *BedrockAdapter) TransformStreamLine(line []byte) []byte {
	return line
}

// StreamUsage returns a zero UsageInfo. Usage is extracted from the SSE stream
// by the handler's streamUsageExtractor.
func (a *BedrockAdapter) StreamUsage() UsageInfo {
	return UsageInfo{}
}
