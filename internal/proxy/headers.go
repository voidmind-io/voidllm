package proxy

import (
	"net/http"
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
// from the upstream are forwarded verbatim so clients can observe rate limits.
func copyResponseHeaders(c fiber.Ctx, resp *http.Response) {
	for _, h := range allowedResponseHeaders {
		if v := resp.Header.Get(h); v != "" {
			c.Set(h, v)
		}
	}

	for key, values := range resp.Header {
		if hopByHopHeaders[key] {
			continue
		}
		if strings.HasPrefix(key, "X-Ratelimit") || strings.HasPrefix(key, "X-RateLimit") {
			for _, v := range values {
				c.Set(key, v)
			}
		}
	}
}
