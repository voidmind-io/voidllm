package apierror

import (
	"net/http"
	"path"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// AnthropicResponse is the error envelope of the Anthropic Messages API:
// {"type":"error","error":{"type":"...","message":"..."}}. VoidLLM uses it for
// its own errors on the native /v1/messages endpoints so that Anthropic SDKs
// and agents such as Claude Code can parse them.
type AnthropicResponse struct {
	Type  string          `json:"type"`
	Error AnthropicDetail `json:"error"`
}

// AnthropicDetail holds the Anthropic error type and a human-readable message.
type AnthropicDetail struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

// IsAnthropicPath reports whether the request path addresses one of the native
// Anthropic Messages endpoints (/v1/messages or /v1/messages/count_tokens).
// The comparison is case-insensitive because the router is.
func IsAnthropicPath(requestPath string) bool {
	switch strings.ToLower(path.Clean(requestPath)) {
	case "/v1/messages", "/v1/messages/count_tokens":
		return true
	}
	return false
}

// AnthropicErrorType maps an HTTP status code to the closest Anthropic error
// type: authentication_error, permission_error, not_found_error,
// request_too_large, rate_limit_error, invalid_request_error, overloaded_error
// or api_error.
func AnthropicErrorType(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	}
	if status >= 400 && status < 500 {
		return "invalid_request_error"
	}
	return "api_error"
}

// SendAnthropic writes an Anthropic-shaped JSON error response with the given
// HTTP status code. The error type is derived from the status code.
func SendAnthropic(c fiber.Ctx, status int, message string) error {
	return c.Status(status).JSON(AnthropicResponse{
		Type: "error",
		Error: AnthropicDetail{
			Type:    AnthropicErrorType(status),
			Message: message,
		},
	})
}

// SendProxy writes an error response for a /v1/* proxy request. Requests to
// the native Anthropic Messages endpoints receive the Anthropic envelope (the
// machine-readable code is dropped, the type is derived from status); every
// other proxy request receives the standard VoidLLM envelope exactly like Send.
func SendProxy(c fiber.Ctx, status int, code, message string) error {
	if IsAnthropicPath(c.Path()) {
		return SendAnthropic(c, status, message)
	}
	return Send(c, status, code, message)
}

// QuotaErrorType is the OpenAI error type for an exhausted quota. Clients such
// as the Codex CLI treat it as a terminal condition instead of honouring a long
// Retry-After.
const QuotaErrorType = "insufficient_quota"

// SendProxyQuota behaves like SendProxy, except that on the OpenAI Responses
// endpoint (/v1/responses) the error additionally carries the OpenAI quota
// type (QuotaErrorType) so clients stop instead of sleeping for the whole
// Retry-After. The VoidLLM code is preserved. All other paths, including the
// Anthropic envelope, are identical to SendProxy.
func SendProxyQuota(c fiber.Ctx, status int, code, message string) error {
	if strings.ToLower(path.Clean(c.Path())) != "/v1/responses" {
		return SendProxy(c, status, code, message)
	}
	return c.Status(status).JSON(Response{
		Error: Detail{
			Type:      QuotaErrorType,
			Code:      code,
			Message:   message,
			RequestID: RequestIDFromCtx(c),
		},
	})
}
