package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"
)

// ErrSessionExpired is returned by Call when the upstream MCP server responds
// with HTTP 404, indicating the session ID is no longer valid.
var ErrSessionExpired = errors.New("MCP session expired")

// cloudMetadataIP is the well-known link-local address used by cloud provider
// instance metadata services (AWS, GCP, Azure, DigitalOcean, etc.).
var cloudMetadataIP = net.ParseIP("169.254.169.254")

// newSSRFSafeTransport returns an http.Transport that, when allowPrivate is
// false, refuses TCP connections to loopback, private, link-local, and cloud
// metadata addresses at dial time. This defends against DNS rebinding attacks:
// even if a hostname resolved to a public IP at registration time, a malicious
// DNS update cannot redirect traffic to an internal address at call time.
// When allowPrivate is true the transport is unrestricted (for self-hosted
// vLLM deployments on private networks).
func newSSRFSafeTransport(allowPrivate bool) *http.Transport {
	dialer := &net.Dialer{}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				// address is already a bare host when no port is present; use as-is.
				host = address
			}
			ip := net.ParseIP(host)
			if ip == nil {
				// Not an IP address — DNS was already resolved by the dialer;
				// if we reach here with a hostname it is safe to pass through.
				return nil
			}
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				return fmt.Errorf("connection to internal address blocked: %s", host)
			}
			if ip.Equal(cloudMetadataIP) {
				return fmt.Errorf("connection to cloud metadata service blocked: %s", host)
			}
			return nil
		}
	}
	return &http.Transport{
		DialContext: dialer.DialContext,
	}
}

// HTTPTransport proxies JSON-RPC requests to a remote MCP server over HTTP.
// It is not safe to use concurrently with Close.
type HTTPTransport struct {
	endpoint   string
	authType   string // "none", "bearer", or "header"
	authHeader string // header name used when authType is "header"
	authToken  string // decrypted token value
	client     *http.Client
}

// NewHTTPTransport creates a transport for the given endpoint with the
// supplied authentication configuration and per-call timeout.
// authType must be one of "none", "bearer", or "header".
// When authType is "bearer", authToken is sent as a Bearer token.
// When authType is "header", authToken is sent under the authHeader header name.
// When allowPrivate is false, the underlying TCP dialer refuses connections to
// loopback, private-range, link-local, and cloud metadata addresses, preventing
// DNS rebinding SSRF attacks even after the URL has been registered.
func NewHTTPTransport(endpoint, authType, authHeader, authToken string, timeout time.Duration, allowPrivate bool) *HTTPTransport {
	t := newSSRFSafeTransport(allowPrivate)
	return &HTTPTransport{
		endpoint:   endpoint,
		authType:   authType,
		authHeader: authHeader,
		authToken:  authToken,
		client: &http.Client{
			Timeout:   timeout,
			Transport: t,
			// Never follow redirects — the remote MCP server should not redirect
			// POST requests and doing so could silently drop the request body.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Call sends raw JSON-RPC bytes to the remote MCP server and returns the
// response body bytes along with any session ID returned by the server.
//
// If sessionID is non-empty it is forwarded to the upstream server via the
// Mcp-Session-Id request header. The server may return a new or updated
// session ID in the same response header, which is returned as newSessionID.
//
// Returns nil body and empty session for HTTP 202 Accepted (notification with
// no response body). Returns ErrSessionExpired when the upstream responds with
// HTTP 404. Returns an error for any other non-200/non-202 status or transport
// failure.
func (t *HTTPTransport) Call(ctx context.Context, raw []byte, sessionID string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	if sessionID != "" {
		req.Header.Set("Mcp-Session-Id", sessionID)
	}

	switch t.authType {
	case "bearer":
		req.Header.Set("Authorization", "Bearer "+t.authToken)
	case "header":
		if t.authHeader != "" {
			req.Header.Set(t.authHeader, t.authToken)
		}
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()

	newSessionID := resp.Header.Get("Mcp-Session-Id")

	// Limit body reads to 10 MiB to prevent OOM on misbehaving upstream servers.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, "", fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusNotFound {
		return nil, "", ErrSessionExpired
	}

	if resp.StatusCode == http.StatusAccepted {
		// Notification acknowledged — no response body expected per the MCP spec.
		return nil, newSessionID, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}

	// If the upstream responded with SSE, extract the JSON payload from the
	// first "data:" line. This handles MCP servers that prefer text/event-stream.
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		return extractSSEData(body), newSessionID, nil
	}

	return body, newSessionID, nil
}

// extractSSEData pulls the first "data:" line from an SSE response body.
func extractSSEData(body []byte) []byte {
	for _, line := range bytes.Split(body, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data: ")) {
			return bytes.TrimPrefix(line, []byte("data: "))
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			return bytes.TrimPrefix(line, []byte("data:"))
		}
	}
	return body // fallback: return as-is
}

// ListTools sends initialize + tools/list to the remote server and parses
// the returned tool definitions. It handles session management automatically.
func (t *HTTPTransport) ListTools(ctx context.Context) ([]Tool, error) {
	// Initialize first to establish session
	initReq, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      0,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "voidllm", "version": "1.0"},
		},
	})
	_, sessionID, initErr := t.Call(ctx, initReq, "")
	if initErr != nil {
		return nil, fmt.Errorf("initialize: %w", initErr)
	}

	// Send notifications/initialized
	notifyReq, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	})
	t.Call(ctx, notifyReq, sessionID) //nolint:errcheck — fire-and-forget

	// Now send tools/list
	req := Request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/list",
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal tools/list: %w", err)
	}

	resp, _, err := t.Call(ctx, raw, sessionID)
	if err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, nil
	}

	var rpcResp struct {
		Result struct {
			Tools []Tool `json:"tools"`
		} `json:"result"`
		Error *Error `json:"error"`
	}
	if err := json.Unmarshal(resp, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode tools/list response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, fmt.Errorf("tools/list error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	return rpcResp.Result.Tools, nil
}

// Close releases idle connections held by the underlying HTTP client.
func (t *HTTPTransport) Close() error {
	t.client.CloseIdleConnections()
	return nil
}
