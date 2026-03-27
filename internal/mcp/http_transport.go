package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

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
func NewHTTPTransport(endpoint, authType, authHeader, authToken string, timeout time.Duration) *HTTPTransport {
	return &HTTPTransport{
		endpoint:   endpoint,
		authType:   authType,
		authHeader: authHeader,
		authToken:  authToken,
		client: &http.Client{
			Timeout: timeout,
			// Never follow redirects — the remote MCP server should not redirect
			// POST requests and doing so could silently drop the request body.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// Call sends raw JSON-RPC bytes to the remote MCP server and returns the
// response body bytes.
// Returns nil, nil for HTTP 202 Accepted (notification with no response body).
// Returns an error for any non-200/non-202 status code or transport failure.
func (t *HTTPTransport) Call(ctx context.Context, raw []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

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
		return nil, fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()

	// Limit body reads to 10 MiB to prevent OOM on misbehaving upstream servers.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode == http.StatusAccepted {
		// Notification acknowledged — no response body expected per the MCP spec.
		return nil, nil
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("upstream returned HTTP %d", resp.StatusCode)
	}

	return body, nil
}

// ListTools sends a tools/list JSON-RPC request and parses the returned tool
// definitions. Returns nil, nil if the server responds with 202 Accepted.
func (t *HTTPTransport) ListTools(ctx context.Context) ([]Tool, error) {
	req := Request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/list",
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal tools/list: %w", err)
	}

	resp, err := t.Call(ctx, raw)
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
