package proxy

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/gofiber/fiber/v3"
)

// hopByHopHeaders is the set of headers that must never be forwarded to the
// upstream provider or back to the client. These are connection-specific and
// are stripped at every hop per RFC 7230.
var hopByHopHeaders = map[string]bool{
	"Connection":          true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"TE":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

// allowedRequestHeaders is the allowlist of client headers forwarded upstream.
// Any header not in this list is dropped, including Authorization, which is
// replaced with the upstream API key stored in the model configuration.
// Accept-Encoding is intentionally excluded: stripping it forces the upstream
// to send uncompressed responses, ensuring body size limits apply to actual
// content bytes rather than compressed wire bytes.
var allowedRequestHeaders = []string{
	"Content-Type",
	"Accept",
	"Accept-Language",
	"X-Request-ID",
}

// allowedResponseHeaders is the allowlist of upstream response headers copied
// back to the client. X-RateLimit-* headers are forwarded separately below.
var allowedResponseHeaders = []string{
	"Content-Type",
	"X-Request-ID",
}

// setUpstreamHeaders configures outbound headers on req before sending it to
// the upstream provider. It applies the allowlisted client headers, sets the
// upstream Authorization header from the model's API key, and sets User-Agent.
// The client's own Authorization header is intentionally not forwarded.
func setUpstreamHeaders(req *http.Request, c fiber.Ctx, model Model) {
	for _, h := range allowedRequestHeaders {
		if v := c.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}

	if model.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+model.APIKey)
	}

	req.Header.Set("User-Agent", "VoidLLM/0.1")
}

// copyResponseHeaders copies allowlisted upstream response headers onto the
// Fiber response context. Hop-by-hop headers and implementation-detail headers
// such as Server and X-Powered-By are never forwarded. X-RateLimit-* headers
// and Anthropic-RateLimit-* headers from the upstream are forwarded verbatim so
// clients can observe rate limits.
func copyResponseHeaders(c fiber.Ctx, resp *http.Response) {
	for _, h := range allowedResponseHeaders {
		if v := resp.Header.Get(h); v != "" {
			c.Set(h, v)
		}
	}

	// Anthropic correlation and rate-limit headers carry no secrets and let
	// native Messages clients observe their upstream quota.
	if v := resp.Header.Get("Request-Id"); v != "" {
		c.Set("Request-Id", v)
	}

	for key, values := range resp.Header {
		if hopByHopHeaders[key] {
			continue
		}
		if strings.HasPrefix(key, "X-Ratelimit") || strings.HasPrefix(key, "X-RateLimit") ||
			strings.HasPrefix(key, "Anthropic-Ratelimit-") {
			for _, v := range values {
				c.Set(key, v)
			}
		}
	}
}

// maxBetaHeaderLen caps the total length of a forwarded anthropic-beta or
// OpenAI-Beta header value.
const maxBetaHeaderLen = 512

// defaultAnthropicVersion is the anthropic-version sent upstream when the
// client supplies none or an invalid one.
const defaultAnthropicVersion = "2023-06-01"

var (
	// anthropicVersionRe matches the date-shaped anthropic-version header value.
	anthropicVersionRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	// anthropicBetaTokenRe matches one anthropic-beta feature token.
	anthropicBetaTokenRe = regexp.MustCompile(`^[a-z0-9-]+$`)
	// openAIBetaTokenRe matches one OpenAI-Beta token. It additionally allows
	// "_", "." and "=" because OpenAI beta flags have the form name=version.
	openAIBetaTokenRe = regexp.MustCompile(`^[a-z0-9._=-]+$`)
)

// validAnthropicVersion returns v when it is a date-shaped anthropic-version
// value and the default version otherwise.
func validAnthropicVersion(v string) string {
	if anthropicVersionRe.MatchString(v) {
		return v
	}
	return defaultAnthropicVersion
}

// validBetaList reports whether v is a comma-separated list of tokens that all
// match tokenRe, at most maxBetaHeaderLen bytes long in total. Whitespace
// around tokens is rejected so the value can be forwarded verbatim. An empty
// value is not valid.
func validBetaList(v string, tokenRe *regexp.Regexp) bool {
	if v == "" || len(v) > maxBetaHeaderLen {
		return false
	}
	for _, tok := range strings.Split(v, ",") {
		if !tokenRe.MatchString(tok) {
			return false
		}
	}
	return true
}

// setNativeHeaders applies the headers of a native (non-translated) endpoint on
// top of the base upstream headers set by setUpstreamHeaders. For the Messages
// format the Bearer Authorization is replaced by x-api-key from the model
// configuration and anthropic-version / anthropic-beta are forwarded after
// validation. For the Responses format OpenAI-Beta is forwarded after
// validation. Invalid beta values are dropped silently.
func setNativeHeaders(req *http.Request, c fiber.Ctx, model Model, format apiFormat) {
	switch format {
	case formatMessages:
		req.Header.Del("Authorization")
		if model.APIKey != "" {
			req.Header.Set("x-api-key", model.APIKey)
		}
		req.Header.Set("anthropic-version", validAnthropicVersion(c.Get("anthropic-version")))
		if v := c.Get("anthropic-beta"); validBetaList(v, anthropicBetaTokenRe) {
			req.Header.Set("anthropic-beta", v)
		}
		req.Header.Set("Content-Type", "application/json")
	case formatResponses:
		if v := c.Get("OpenAI-Beta"); validBetaList(v, openAIBetaTokenRe) {
			req.Header.Set("OpenAI-Beta", v)
		}
	}
}
