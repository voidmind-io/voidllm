package proxy

import (
	"net/http"
	"strings"
)

// AzureAdapter adapts requests for the Azure OpenAI Service. Azure speaks the
// OpenAI wire format natively, so only the URL construction and authentication
// header need to change. Request and response bodies are forwarded unchanged.
type AzureAdapter struct{}

// TransformRequest returns the body unchanged; Azure uses the OpenAI request
// format without modification.
func (a *AzureAdapter) TransformRequest(body []byte, _ Model) ([]byte, error) {
	return body, nil
}

// TransformURL builds the Azure OpenAI deployment URL from the base URL and
// model metadata. The resulting URL has the form:
//
//	{baseURL}/openai/deployments/{deployment}/{upstreamPath}?api-version={version}
//
// When AzureAPIVersion is not set on the model, the current GA version
// "2024-10-21" is used as the default.
func (a *AzureAdapter) TransformURL(baseURL, upstreamPath string, model Model) string {
	version := model.AzureAPIVersion
	if version == "" {
		version = "2024-10-21"
	}
	u := strings.TrimRight(baseURL, "/") +
		"/openai/deployments/" + model.AzureDeployment +
		"/" + upstreamPath +
		"?api-version=" + version
	return u
}

// SetHeaders configures Azure-specific authentication. Azure uses the "api-key"
// header instead of Bearer Authorization.
func (a *AzureAdapter) SetHeaders(req *http.Request, model Model) {
	req.Header.Del("Authorization")
	if model.APIKey != "" {
		req.Header.Set("api-key", model.APIKey)
	}
}

// TransformResponse returns the body unchanged; Azure responses are already in
// OpenAI format.
func (a *AzureAdapter) TransformResponse(body []byte) ([]byte, error) {
	return body, nil
}

// TransformStreamLine returns the line unchanged; Azure streams are already in
// OpenAI SSE format.
func (a *AzureAdapter) TransformStreamLine(line []byte) []byte {
	return line
}

// StreamUsage returns a zero UsageInfo. Azure streams the OpenAI wire format,
// so usage is extracted by the streamUsageExtractor in the proxy handler rather
// than by this adapter.
func (a *AzureAdapter) StreamUsage() UsageInfo {
	return UsageInfo{}
}
