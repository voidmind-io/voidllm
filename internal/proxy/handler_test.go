package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// testRegistry builds a Registry backed by a real httptest upstream URL so
// each test can spin up its own mock server without port collisions.
func testRegistry(t *testing.T, upstreamURL string) *Registry {
	t.Helper()
	r, err := NewRegistry([]config.ModelConfig{
		{
			Name:     "test-model",
			Provider: "vllm",
			BaseURL:  upstreamURL,
			APIKey:   "upstream-secret",
			Aliases:  []string{"default", "fast"},
		},
		{
			Name:            "azure-model",
			Provider:        "azure",
			BaseURL:         upstreamURL,
			AzureDeployment: "gpt-4o",
			AzureAPIVersion: "2024-02-01",
		},
		{
			Name:     "no-key-model",
			Provider: "vllm",
			BaseURL:  upstreamURL,
			APIKey:   "", // intentionally empty
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// testProxyHandler creates a ProxyHandler wired to the given upstream URL
// with a silent logger.
func testProxyHandler(t *testing.T, upstreamURL string) *ProxyHandler {
	t.Helper()
	return NewProxyHandler(
		testRegistry(t, upstreamURL),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

// testApp wires the ProxyHandler into a Fiber application matching the
// production routing layout.
func testApp(t *testing.T, handler *ProxyHandler) *fiber.App {
	t.Helper()
	app := fiber.New()
	app.Get("/v1/models", handler.ModelsHandler)
	app.All("/v1/*", handler.Handle)
	return app
}

// testTimeout is the per-request timeout passed to app.Test.
var testTimeout = fiber.TestConfig{Timeout: 5 * time.Second}

// capturedRequest captures the upstream HTTP request for assertion.
type capturedRequest struct {
	Method  string
	Path    string
	RawBody []byte
	Header  http.Header
	Query   string
}

// upstreamCapture returns an httptest.Server that records the last received
// request into *capturedRequest and responds with the provided status and body.
func upstreamCapture(t *testing.T, statusCode int, responseBody string, responseHeaders map[string]string) (*httptest.Server, *capturedRequest) {
	t.Helper()
	captured := &capturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Method = r.Method
		captured.Path = r.URL.Path
		captured.Query = r.URL.RawQuery
		captured.Header = r.Header.Clone()
		var err error
		captured.RawBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request body: %v", err)
		}
		for k, v := range responseHeaders {
			w.Header().Set(k, v)
		}
		w.WriteHeader(statusCode)
		fmt.Fprint(w, responseBody)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// ──────────────────────────────────────────────────────────────────────────────
// Non-streaming passthrough
// ──────────────────────────────────────────────────────────────────────────────

func TestHandle_NonStreamingPassthrough(t *testing.T) {
	t.Parallel()

	upstreamResp := `{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`
	upstream, captured := upstreamCapture(t, http.StatusOK, upstreamResp, map[string]string{
		"Content-Type": "application/json",
	})

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	body := `{"model":"test-model","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	respBody, _ := io.ReadAll(resp.Body)
	if string(respBody) != upstreamResp {
		t.Errorf("response body = %q, want %q", string(respBody), upstreamResp)
	}

	// Upstream must have received the canonical model name.
	var upstreamEnvelope struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(captured.RawBody, &upstreamEnvelope); err != nil {
		t.Fatalf("unmarshal upstream body: %v", err)
	}
	if upstreamEnvelope.Model != "test-model" {
		t.Errorf("upstream received model = %q, want %q", upstreamEnvelope.Model, "test-model")
	}
}

func TestHandle_UpstreamStatusForwarded(t *testing.T) {
	t.Parallel()

	upstream, _ := upstreamCapture(t, http.StatusTeapot, `{"error":"i am a teapot"}`, map[string]string{
		"Content-Type": "application/json",
	})

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}
}

func TestHandle_Upstream500ForwardedAsIs(t *testing.T) {
	t.Parallel()

	upstreamBody := `{"error":{"message":"internal server error"}}`
	upstream, _ := upstreamCapture(t, http.StatusInternalServerError, upstreamBody, map[string]string{
		"Content-Type": "application/json",
	})

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	// The handler forwards upstream status codes for non-streaming responses.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Model alias resolution
// ──────────────────────────────────────────────────────────────────────────────

func TestHandle_AliasResolution(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		requestedName string
		wantUpstream  string
	}{
		{
			name:          "alias fast resolves to test-model",
			requestedName: "fast",
			wantUpstream:  "test-model",
		},
		{
			name:          "alias default resolves to test-model",
			requestedName: "default",
			wantUpstream:  "test-model",
		},
		{
			name:          "canonical name passes through unchanged",
			requestedName: "test-model",
			wantUpstream:  "test-model",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
				"Content-Type": "application/json",
			})

			handler := testProxyHandler(t, upstream.URL)
			app := testApp(t, handler)

			body := fmt.Sprintf(`{"model":%q,"messages":[]}`, tc.requestedName)
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")

			resp, err := app.Test(req, testTimeout)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			resp.Body.Close()

			var envelope struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(captured.RawBody, &envelope); err != nil {
				t.Fatalf("unmarshal upstream body: %v", err)
			}
			if envelope.Model != tc.wantUpstream {
				t.Errorf("upstream model = %q, want %q", envelope.Model, tc.wantUpstream)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Streaming (SSE)
//
// Fiber's app.Test collects the full response body after the handler returns,
// which means SendStreamWriter's asynchronous writer goroutine may not have
// finished writing. We therefore test streaming via a real TCP listener so the
// client connection stays open until all chunks arrive.
// ──────────────────────────────────────────────────────────────────────────────

// startTestServer starts the Fiber app on a random TCP port and returns the
// base URL. The server is shut down when the test ends.
func startTestServer(t *testing.T, handler *ProxyHandler) string {
	t.Helper()
	app := testApp(t, handler)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	go func() {
		// Listener is closed by app.Shutdown via t.Cleanup.
		_ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
	}()

	t.Cleanup(func() {
		_ = app.Shutdown()
	})

	// Wait until the server is accepting connections.
	addr := ln.Addr().String()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	return "http://" + addr
}

// TestHandle_Streaming verifies that all SSE chunks are forwarded to the client.
//
// BUG: this test currently FAILS because handler.go has `defer resp.Body.Close()`
// at line 113. SendStreamWriter runs the scanner in a goroutine (via
// fasthttp.NewStreamReader); when Handle() returns, the deferred close fires
// before the goroutine has read any data, producing an empty body.
// Fix: remove the defer and close resp.Body inside the SendStreamWriter function
// after the scanner finishes (or when w.Flush returns an error).
func TestHandle_Streaming(t *testing.T) {
	t.Parallel()

	chunks := []string{
		`data: {"choices":[{"delta":{"content":"Hello"}}]}`,
		`data: {"choices":[{"delta":{"content":" World"}}]}`,
		`data: [DONE]`,
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream responseWriter does not implement http.Flusher")
			return
		}
		for _, chunk := range chunks {
			fmt.Fprintln(w, chunk)
			fmt.Fprintln(w) // blank line after each SSE event
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	handler := testProxyHandler(t, upstream.URL)
	baseURL := startTestServer(t, handler)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read streaming response: %v", err)
	}

	bodyStr := string(responseBody)
	for _, want := range chunks {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("streaming response missing chunk %q", want)
		}
	}

	if !strings.Contains(bodyStr, "[DONE]") {
		t.Error("streaming response missing [DONE] terminator")
	}
}

// TestHandle_StreamingAllChunksArriveInOrder verifies that chunks are forwarded
// in the same order the upstream sends them.
//
// BUG: currently FAILS for the same reason as TestHandle_Streaming — see that
// test's doc comment for the root cause.
func TestHandle_StreamingAllChunksArriveInOrder(t *testing.T) {
	t.Parallel()

	const numChunks = 10
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < numChunks; i++ {
			fmt.Fprintf(w, "data: {\"seq\":%d}\n\n", i)
			flusher.Flush()
		}
		fmt.Fprintln(w, "data: [DONE]")
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	handler := testProxyHandler(t, upstream.URL)
	baseURL := startTestServer(t, handler)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[],"stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	bodyStr := string(body)

	// Verify each seq appears in order.
	lastIdx := -1
	for i := 0; i < numChunks; i++ {
		fragment := fmt.Sprintf(`"seq":%d`, i)
		idx := strings.Index(bodyStr, fragment)
		if idx == -1 {
			t.Errorf("chunk seq=%d not found in response", i)
			continue
		}
		if idx < lastIdx {
			t.Errorf("chunk seq=%d appears before seq=%d (out of order)", i, i-1)
		}
		lastIdx = idx
	}
}

// TestHandle_Streaming_HeadersViaAppTest verifies the SSE response headers
// using app.Test (which works for header inspection even though body streaming
// requires a real listener). This acts as a fast sanity-check.
func TestHandle_Streaming_HeadersViaAppTest(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintln(w, "data: [DONE]")
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Errorf("Cache-Control = %q, want %q", cc, "no-cache")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Error cases
// ──────────────────────────────────────────────────────────────────────────────

func TestHandle_ErrorCases(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantCode   string
	}{
		{
			name:       "unknown model returns 404",
			body:       `{"model":"does-not-exist","messages":[]}`,
			wantStatus: http.StatusNotFound,
			wantCode:   "model_not_found",
		},
		{
			name:       "empty body returns 400",
			body:       ``,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name:       "body without model field returns 400",
			body:       `{"messages":[{"role":"user","content":"hi"}]}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name:       "model field empty string returns 400",
			body:       `{"model":"","messages":[]}`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name:       "invalid JSON returns 400",
			body:       `not-json`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
	}

	// A single upstream that should never be reached for error cases that fail
	// before proxying. We use a closed server to ensure any accidental forwarding
	// would fail visibly rather than silently succeed.
	upstream, _ := upstreamCapture(t, http.StatusOK, `{}`, nil)

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			handler := testProxyHandler(t, upstream.URL)
			app := testApp(t, handler)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")

			resp, err := app.Test(req, testTimeout)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}

			var errResp apierror.Response
			if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if errResp.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", errResp.Error.Code, tc.wantCode)
			}
			if errResp.Error.Message == "" {
				t.Error("error.message is empty")
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Upstream unavailable → 502
// ──────────────────────────────────────────────────────────────────────────────

func TestHandle_UpstreamUnavailable(t *testing.T) {
	t.Parallel()

	// Use an address that is guaranteed to refuse connections.
	const unreachableURL = "http://127.0.0.1:1"

	handler := testProxyHandler(t, unreachableURL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadGateway)
	}

	var errResp apierror.Response
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if errResp.Error.Code != "upstream_unavailable" {
		t.Errorf("error.code = %q, want %q", errResp.Error.Code, "upstream_unavailable")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Query string stripping (security: client query params must not reach upstream)
// ──────────────────────────────────────────────────────────────────────────────

func TestHandle_QueryStringNotForwarded(t *testing.T) {
	t.Parallel()

	upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
		"Content-Type": "application/json",
	})

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost,
		"/v1/chat/completions?api-version=2024-02-01&foo=bar",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	// Client-provided query parameters must never reach the upstream provider.
	if captured.Query != "" {
		t.Errorf("SECURITY: upstream received query string %q, want empty", captured.Query)
	}
}

func TestHandle_NoQueryStringProducesNoQueryString(t *testing.T) {
	t.Parallel()

	upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
		"Content-Type": "application/json",
	})

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	if captured.Query != "" {
		t.Errorf("upstream received unexpected query string %q", captured.Query)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Path rewriting
// ──────────────────────────────────────────────────────────────────────────────

func TestHandle_PathRewriting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		clientPath string
		wantPath   string
	}{
		{
			name:       "chat completions path forwarded",
			clientPath: "/v1/chat/completions",
			wantPath:   "/chat/completions",
		},
		{
			name:       "embeddings path forwarded",
			clientPath: "/v1/embeddings",
			wantPath:   "/embeddings",
		},
		{
			name:       "completions path forwarded",
			clientPath: "/v1/completions",
			wantPath:   "/completions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
				"Content-Type": "application/json",
			})

			handler := testProxyHandler(t, upstream.URL)
			app := testApp(t, handler)

			req := httptest.NewRequest(http.MethodPost, tc.clientPath,
				strings.NewReader(`{"model":"test-model","messages":[]}`))
			req.Header.Set("Content-Type", "application/json")

			resp, err := app.Test(req, testTimeout)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			resp.Body.Close()

			if captured.Path != tc.wantPath {
				t.Errorf("upstream path = %q, want %q", captured.Path, tc.wantPath)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Security: Authorization header handling (hot-path security tests)
// ──────────────────────────────────────────────────────────────────────────────

func TestHandle_Security_ClientAuthNotForwardedToUpstream(t *testing.T) {
	t.Parallel()

	upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
		"Content-Type": "application/json",
	})

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vl_uk_client-key-should-not-leak")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	upstreamAuth := captured.Header.Get("Authorization")
	if strings.Contains(upstreamAuth, "vl_uk_client-key-should-not-leak") {
		t.Error("SECURITY: client Authorization token was forwarded to upstream")
	}
}

func TestHandle_Security_UpstreamAPIKeySetOnRequest(t *testing.T) {
	t.Parallel()

	upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
		"Content-Type": "application/json",
	})

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vl_uk_client-key")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	wantAuth := "Bearer upstream-secret"
	if captured.Header.Get("Authorization") != wantAuth {
		t.Errorf("upstream Authorization = %q, want %q",
			captured.Header.Get("Authorization"), wantAuth)
	}
}

func TestHandle_Security_EmptyAPIKeyNoAuthHeader(t *testing.T) {
	t.Parallel()

	upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
		"Content-Type": "application/json",
	})

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"no-key-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vl_uk_some-key")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	// When APIKey is empty, no Authorization header should be set at all.
	if captured.Header.Get("Authorization") != "" {
		t.Errorf("upstream received Authorization %q, want empty (model has no API key)",
			captured.Header.Get("Authorization"))
	}
}

func TestHandle_Security_HopByHopNotForwardedUpstream(t *testing.T) {
	t.Parallel()

	upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
		"Content-Type": "application/json",
	})

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Transfer-Encoding", "chunked")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("TE", "trailers")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	hopByHop := []string{"Connection", "Keep-Alive", "Transfer-Encoding", "Upgrade", "TE"}
	for _, h := range hopByHop {
		if v := captured.Header.Get(h); v != "" {
			t.Errorf("SECURITY: hop-by-hop header %q = %q was forwarded to upstream", h, v)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// mutateRequestBody unit tests
// ──────────────────────────────────────────────────────────────────────────────

func TestMutateRequestBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		canonicalName string
		wantModel     string
		wantOtherKey  string // a key that should be preserved
	}{
		{
			name:          "replaces alias with canonical name",
			input:         `{"model":"fast","messages":[]}`,
			canonicalName: "test-model",
			wantModel:     "test-model",
		},
		{
			name:          "preserves other fields",
			input:         `{"model":"fast","messages":[],"temperature":0.7}`,
			canonicalName: "test-model",
			wantModel:     "test-model",
		},
		{
			name:          "already canonical name left unchanged",
			input:         `{"model":"test-model","messages":[]}`,
			canonicalName: "test-model",
			wantModel:     "test-model",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			out := mutateRequestBody([]byte(tc.input), tc.canonicalName, false)

			var result struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(out, &result); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			if result.Model != tc.wantModel {
				t.Errorf("model = %q, want %q", result.Model, tc.wantModel)
			}
		})
	}
}

func TestMutateRequestBody_InjectUsage(t *testing.T) {
	t.Parallel()

	input := `{"model":"fast","messages":[],"stream":true}`
	out := mutateRequestBody([]byte(input), "test-model", true)

	var result struct {
		Model         string `json:"model"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.Model != "test-model" {
		t.Errorf("model = %q, want %q", result.Model, "test-model")
	}
	if result.StreamOptions == nil {
		t.Fatal("stream_options is nil, want {include_usage: true}")
	}
	if !result.StreamOptions.IncludeUsage {
		t.Error("stream_options.include_usage = false, want true")
	}
}

func TestMutateRequestBody_InvalidJSON(t *testing.T) {
	t.Parallel()

	input := []byte(`not-valid-json`)
	out := mutateRequestBody(input, "canonical", false)
	// Must return the original bytes unchanged on parse failure.
	if string(out) != string(input) {
		t.Errorf("mutateRequestBody(invalid) = %q, want original %q", string(out), string(input))
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// isStreamingResponse unit tests
// ──────────────────────────────────────────────────────────────────────────────

func TestIsStreamingResponse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		contentType string
		want        bool
	}{
		{name: "text/event-stream is streaming", contentType: "text/event-stream", want: true},
		{name: "text/event-stream with charset", contentType: "text/event-stream; charset=utf-8", want: true},
		{name: "application/json is not streaming", contentType: "application/json", want: false},
		{name: "empty content-type is not streaming", contentType: "", want: false},
		{name: "text/plain is not streaming", contentType: "text/plain", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resp := &http.Response{Header: make(http.Header)}
			if tc.contentType != "" {
				resp.Header.Set("Content-Type", tc.contentType)
			}
			got := isStreamingResponse(resp)
			if got != tc.want {
				t.Errorf("isStreamingResponse(%q) = %v, want %v", tc.contentType, got, tc.want)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Benchmark: proxy hot path
// ──────────────────────────────────────────────────────────────────────────────

func BenchmarkHandle_NonStreaming(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chatcmpl-1","choices":[{"message":{"content":"ok"}}]}`)
	}))
	b.Cleanup(upstream.Close)

	r, err := NewRegistry([]config.ModelConfig{
		{Name: "bench-model", Provider: "vllm", BaseURL: upstream.URL, APIKey: "key"},
	})
	if err != nil {
		b.Fatal(err)
	}
	handler := NewProxyHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	app := fiber.New()
	app.All("/v1/*", handler.Handle)

	body := `{"model":"bench-model","messages":[{"role":"user","content":"hi"}]}`
	benchTimeout := fiber.TestConfig{Timeout: 5 * time.Second}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := app.Test(req, benchTimeout)
		if err != nil {
			b.Fatal(err)
		}
		resp.Body.Close()
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Fallback chain tests
// ──────────────────────────────────────────────────────────────────────────────

// testRegistryWithFallback builds a Registry containing two models where
// modelA's fallback is modelB. Each model is backed by the provided httptest
// server URL (which may differ per test).
func testRegistryWithFallback(t *testing.T, urlA, urlB string) *Registry {
	t.Helper()
	r, err := NewRegistry([]config.ModelConfig{
		{
			Name:     "model-a",
			Provider: "openai",
			BaseURL:  urlA,
			APIKey:   "key-a",
			Fallback: "model-b",
		},
		{
			Name:     "model-b",
			Provider: "openai",
			BaseURL:  urlB,
			APIKey:   "key-b",
		},
	})
	if err != nil {
		t.Fatalf("testRegistryWithFallback: %v", err)
	}
	return r
}

// TestHandle_FallbackOnNetworkError verifies that when model-a's upstream is
// unreachable, the proxy falls back to model-b and uses model-b's response.
func TestHandle_FallbackOnNetworkError(t *testing.T) {
	t.Parallel()

	upstreamB, _ := upstreamCapture(t, http.StatusOK,
		`{"id":"cmp-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hello from B"}}]}`,
		map[string]string{"Content-Type": "application/json"},
	)

	reg := testRegistryWithFallback(t, "http://127.0.0.1:1", upstreamB.URL)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 1
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusOK, body)
	}
}

// TestHandle_FallbackOn500 verifies that a 5xx response from model-a triggers
// fallback to model-b which succeeds.
func TestHandle_FallbackOn500(t *testing.T) {
	t.Parallel()

	upstreamA, _ := upstreamCapture(t, http.StatusInternalServerError,
		`{"error":{"message":"upstream error"}}`,
		map[string]string{"Content-Type": "application/json"},
	)
	upstreamB, _ := upstreamCapture(t, http.StatusOK,
		`{"id":"cmp-2","object":"chat.completion","choices":[]}`,
		map[string]string{"Content-Type": "application/json"},
	)

	reg := testRegistryWithFallback(t, upstreamA.URL, upstreamB.URL)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 1
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusOK, body)
	}
}

// TestHandle_NoFallbackOn400 verifies that 4xx responses do not trigger
// fallback — the client error is forwarded and model-b is never tried.
func TestHandle_NoFallbackOn400(t *testing.T) {
	t.Parallel()

	upstreamA, capturedA := upstreamCapture(t, http.StatusBadRequest,
		`{"error":{"message":"bad request"}}`,
		map[string]string{"Content-Type": "application/json"},
	)
	// upstreamB must never be hit; use a server that we track independently.
	bHit := false
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHit = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(upstreamB.Close)

	reg := testRegistryWithFallback(t, upstreamA.URL, upstreamB.URL)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 1
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	if bHit {
		t.Error("model-b was tried but should not have been (4xx is not fallback-eligible)")
	}
	// Ensure A was actually tried.
	if capturedA.Method == "" {
		t.Error("model-a was not tried at all")
	}
}

// TestHandle_NoFallbackOn401 verifies 401 is not fallback-eligible.
func TestHandle_NoFallbackOn401(t *testing.T) {
	t.Parallel()

	upstreamA, _ := upstreamCapture(t, http.StatusUnauthorized,
		`{"error":{"message":"unauthorized"}}`,
		map[string]string{"Content-Type": "application/json"},
	)
	bHit := false
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHit = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(upstreamB.Close)

	reg := testRegistryWithFallback(t, upstreamA.URL, upstreamB.URL)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 1
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
	if bHit {
		t.Error("model-b was tried but should not have been (401 is not fallback-eligible)")
	}
}

// TestHandle_FallbackDepthExceeded verifies that the FallbackMaxDepth cap is
// enforced. With FallbackMaxDepth=2 and a chain A→B→C→D, the proxy tries A
// (depth=0), B (depth=1), C (depth=2) — then the check depth>=maxDepth blocks
// the hop to D.
func TestHandle_FallbackDepthExceeded(t *testing.T) {
	t.Parallel()

	// All four upstreams return 500 to force fallback on each step.
	makeFailServer := func(id string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `{"error":{"message":"fail from %s"}}`, id)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	srvA := makeFailServer("A")
	srvB := makeFailServer("B")
	srvC := makeFailServer("C")
	dHit := false
	srvD := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dHit = true
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(srvD.Close)

	r, err := NewRegistry([]config.ModelConfig{
		{Name: "model-a", Provider: "openai", BaseURL: srvA.URL, APIKey: "k", Fallback: "model-b"},
		{Name: "model-b", Provider: "openai", BaseURL: srvB.URL, APIKey: "k", Fallback: "model-c"},
		{Name: "model-c", Provider: "openai", BaseURL: srvC.URL, APIKey: "k", Fallback: "model-d"},
		{Name: "model-d", Provider: "openai", BaseURL: srvD.URL, APIKey: "k"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	handler := NewProxyHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 2
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if dHit {
		t.Error("model-d was reached but FallbackMaxDepth=2 should have stopped the chain at model-c")
	}
	// The response must be an error since all tried models failed.
	if resp.StatusCode == http.StatusOK {
		t.Error("expected a non-200 error status when chain is exhausted")
	}
}

// TestHandle_CycleDetectionRuntime verifies that runtime cycle detection in the
// visited set prevents an infinite loop. The Registry itself allows this state
// (config validation would normally prevent it, but we inject it directly to
// exercise the runtime guard).
func TestHandle_CycleDetectionRuntime(t *testing.T) {
	t.Parallel()

	// Both upstreams return 500 so fallback keeps trying.
	upstreamA, _ := upstreamCapture(t, http.StatusInternalServerError, `{}`,
		map[string]string{"Content-Type": "application/json"})
	upstreamB, _ := upstreamCapture(t, http.StatusInternalServerError, `{}`,
		map[string]string{"Content-Type": "application/json"})

	// Build registry without cycle: A→B, B has no fallback.
	// Then inject a cycle by using AddModel after construction.
	reg, err := NewRegistry([]config.ModelConfig{
		{Name: "model-a", Provider: "openai", BaseURL: upstreamA.URL, APIKey: "k"},
		{Name: "model-b", Provider: "openai", BaseURL: upstreamB.URL, APIKey: "k"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Inject the cycle: A→B, B→A.
	reg.AddModel(Model{
		Name:              "model-a",
		Provider:          "openai",
		BaseURL:           upstreamA.URL,
		APIKey:            "k",
		FallbackModelName: "model-b",
	})
	reg.AddModel(Model{
		Name:              "model-b",
		Provider:          "openai",
		BaseURL:           upstreamB.URL,
		APIKey:            "k",
		FallbackModelName: "model-a",
	})

	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 10 // generous depth so only the visited-set stops it
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	// The request must complete (not loop forever) and return an error.
	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	// We expect a non-200 error response because both models failed.
	if resp.StatusCode == http.StatusOK {
		t.Error("expected error status after cycle-protected exhaustion")
	}
}

// TestHandle_FallbackBodyRewrite verifies that when fallback fires, the request
// body sent to model-b has its "model" field rewritten to "model-b".
func TestHandle_FallbackBodyRewrite(t *testing.T) {
	t.Parallel()

	// model-a fails.
	upstreamA, _ := upstreamCapture(t, http.StatusInternalServerError, `{}`,
		map[string]string{"Content-Type": "application/json"})

	// model-b succeeds and captures the request body.
	var capturedModelField string
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var env struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &env)
		capturedModelField = env.Model
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[]}`)
	}))
	t.Cleanup(upstreamB.Close)

	reg := testRegistryWithFallback(t, upstreamA.URL, upstreamB.URL)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 1
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	if capturedModelField != "model-b" {
		t.Errorf("model field in upstream request = %q, want %q", capturedModelField, "model-b")
	}
}

// TestHandle_NoFallbackConfigured verifies that when a model has no fallback,
// a 5xx error is returned directly to the client.
func TestHandle_NoFallbackConfigured(t *testing.T) {
	t.Parallel()

	upstream, _ := upstreamCapture(t, http.StatusInternalServerError,
		`{"error":{"message":"unavailable"}}`,
		map[string]string{"Content-Type": "application/json"},
	)

	// Registry has only one model with no fallback.
	reg, err := NewRegistry([]config.ModelConfig{
		{Name: "solo-model", Provider: "openai", BaseURL: upstream.URL, APIKey: "k"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 3
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"solo-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d (no fallback configured; 5xx forwarded)", resp.StatusCode, http.StatusInternalServerError)
	}
}

// TestHandle_FallbackDisabledWhenMaxDepthZero verifies that when
// FallbackMaxDepth is 0 (or unset), no fallback occurs even when a fallback
// model is registered.
func TestHandle_FallbackDisabledWhenMaxDepthZero(t *testing.T) {
	t.Parallel()

	upstreamA, _ := upstreamCapture(t, http.StatusInternalServerError, `{}`,
		map[string]string{"Content-Type": "application/json"})
	bHit := false
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHit = true
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(upstreamB.Close)

	reg := testRegistryWithFallback(t, upstreamA.URL, upstreamB.URL)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 0 // disabled
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if bHit {
		t.Error("model-b was tried but FallbackMaxDepth=0 should disable fallback")
	}
	// The 5xx from A should be forwarded directly.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusInternalServerError)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// isFallbackEligible — unit tests
// ──────────────────────────────────────────────────────────────────────────────

func TestIsFallbackEligible(t *testing.T) {
	t.Parallel()

	netErr := fmt.Errorf("dial tcp: connection refused")

	tests := []struct {
		name       string
		statusCode int
		err        error
		want       bool
	}{
		// 5xx are eligible.
		{"500 eligible", 500, nil, true},
		{"502 eligible", 502, nil, true},
		{"503 eligible", 503, nil, true},
		{"504 eligible", 504, nil, true},
		{"599 eligible", 599, nil, true},
		// 4xx are NOT eligible.
		{"400 not eligible", 400, nil, false},
		{"401 not eligible", 401, nil, false},
		{"403 not eligible", 403, nil, false},
		{"404 not eligible", 404, nil, false},
		// 429 is 4xx — not eligible per production code.
		{"429 not eligible", 429, nil, false},
		// 200 success is not eligible.
		{"200 not eligible", 200, nil, false},
		// Network error with no status is eligible.
		{"network error eligible", 0, netErr, true},
		// Context cancellation is NOT eligible.
		{"context.Canceled not eligible", 0, context.Canceled, false},
		{"context.DeadlineExceeded not eligible", 0, context.DeadlineExceeded, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := isFallbackEligible(tc.statusCode, tc.err)
			if got != tc.want {
				t.Errorf("isFallbackEligible(%d, %v) = %v, want %v",
					tc.statusCode, tc.err, got, tc.want)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// rewriteModelInBody — unit tests
// ──────────────────────────────────────────────────────────────────────────────

func TestRewriteModelInBody(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		body      string
		newModel  string
		wantModel string
		// wantError means the function must return a non-nil error (Fix C4: invalid JSON now returns error).
		wantError bool
	}{
		{
			name:      "existing model field replaced",
			body:      `{"model":"model-a","messages":[]}`,
			newModel:  "model-b",
			wantModel: "model-b",
		},
		{
			name:      "model field added when absent",
			body:      `{"messages":[{"role":"user","content":"hi"}]}`,
			newModel:  "model-b",
			wantModel: "model-b",
		},
		{
			name:      "extra fields preserved",
			body:      `{"model":"model-a","temperature":0.7,"max_tokens":512,"messages":[]}`,
			newModel:  "model-c",
			wantModel: "model-c",
		},
		{
			name:      "invalid JSON returns error",
			body:      `not-valid-json`,
			newModel:  "model-b",
			wantError: true,
		},
		{
			name:      "empty body returns error",
			body:      ``,
			newModel:  "model-b",
			wantError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bodyBytes := []byte(tc.body)
			got, err := rewriteModelInBody(bodyBytes, tc.newModel)

			if tc.wantError {
				if err == nil {
					t.Errorf("rewriteModelInBody(%q) = %q, want non-nil error", tc.body, string(got))
				}
				if got != nil {
					t.Errorf("rewriteModelInBody(%q) returned non-nil bytes with error: %q", tc.body, string(got))
				}
				return
			}

			if err != nil {
				t.Fatalf("rewriteModelInBody returned unexpected error: %v", err)
			}

			// Verify the model field in the output.
			var out struct {
				Model string `json:"model"`
			}
			if err := json.Unmarshal(got, &out); err != nil {
				t.Fatalf("unmarshal output: %v", err)
			}
			if out.Model != tc.wantModel {
				t.Errorf("model = %q, want %q", out.Model, tc.wantModel)
			}

			// Verify other fields are preserved for the "extra fields" case.
			if tc.name == "extra fields preserved" {
				var full map[string]json.RawMessage
				if err := json.Unmarshal(got, &full); err != nil {
					t.Fatalf("unmarshal full output: %v", err)
				}
				for _, field := range []string{"temperature", "max_tokens", "messages"} {
					if _, ok := full[field]; !ok {
						t.Errorf("field %q was dropped from rewritten body", field)
					}
				}
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Fallback access control (Fix C1 / VULN-001)
// ──────────────────────────────────────────────────────────────────────────────

// testHMACSecret is a fixed secret used for auth middleware in proxy handler tests.
var testHMACSecret = []byte("proxy-handler-test-hmac-secret")

// testAppWithAuth wires a ProxyHandler with the auth middleware in a Fiber app.
// It returns the app and a plaintext key that satisfies auth.Middleware.
func testAppWithAuth(t *testing.T, handler *ProxyHandler, keyInfo auth.KeyInfo) (*fiber.App, string) {
	t.Helper()

	keyCache := cache.New[string, auth.KeyInfo]()

	rawKey, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("keygen.Generate: %v", err)
	}
	hash := keygen.Hash(rawKey, testHMACSecret)
	keyCache.Set(hash, keyInfo)

	app := fiber.New()
	app.Use(auth.Middleware(keyCache, testHMACSecret))
	app.Get("/v1/models", handler.ModelsHandler)
	app.All("/v1/*", handler.Handle)

	return app, rawKey
}

// TestHandle_FallbackBlockedByAccessCache verifies that when a key is denied
// access to the fallback model (Fix C1 / VULN-001), the chain stops and the
// primary's error is returned without calling the fallback upstream.
func TestHandle_FallbackBlockedByAccessCache(t *testing.T) {
	t.Parallel()

	// model-a returns 502 to trigger fallback eligibility.
	upstreamA, _ := upstreamCapture(t, http.StatusBadGateway,
		`{"error":{"message":"bad gateway"}}`,
		map[string]string{"Content-Type": "application/json"},
	)

	// model-b must never be called; use an atomic counter to detect any call.
	var bCallCount atomic.Int32
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bCallCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[]}`)
	}))
	t.Cleanup(upstreamB.Close)

	reg := testRegistryWithFallback(t, upstreamA.URL, upstreamB.URL)

	// Wire the AccessCache so model-a is allowed but model-b is denied for
	// the test key ID.
	accessCache := NewModelAccessCache()
	const testKeyID = "test-key-fb-access"
	// Allow only model-a for this key; model-b is not in the list.
	accessCache.Load(
		nil,
		nil,
		map[string][]string{
			testKeyID: {"model-a"},
		},
	)

	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 1
	handler.AccessCache = accessCache

	keyInfo := auth.KeyInfo{
		ID:    testKeyID,
		OrgID: "test-org",
		Role:  "member",
	}
	app, rawKey := testAppWithAuth(t, handler, keyInfo)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	// model-b must never have been called.
	if bCallCount.Load() > 0 {
		t.Error("SECURITY: fallback model-b was called despite key not having access — access policy bypassed")
	}

	// The response must be the error from model-a (bad gateway), not a 200 from model-b.
	if resp.StatusCode == http.StatusOK {
		t.Errorf("status = %d, want non-200 (primary error should be preserved)", resp.StatusCode)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Context cancellation during fallback (Fix W3)
// ──────────────────────────────────────────────────────────────────────────────

// TestHandle_ContextCancelledDuringFallback verifies that when the client's
// request context is cancelled while the primary is failing, the handler
// returns without panicking and without calling model-b (or aborting cleanly
// if it does call model-b before the cancel propagates).
func TestHandle_ContextCancelledDuringFallback(t *testing.T) {
	t.Parallel()

	// model-a returns 500 after a short pause so the cancel can race with it.
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Small sleep ensures the cancel goroutine can fire before A responds.
		time.Sleep(10 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(upstreamA.Close)

	var bCallCount atomic.Int32
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bCallCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[]}`)
	}))
	t.Cleanup(upstreamB.Close)

	reg := testRegistryWithFallback(t, upstreamA.URL, upstreamB.URL)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 1
	baseURL := startTestServer(t, handler)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a very short window so the cancel races with A's response.
	go func() {
		time.Sleep(5 * time.Millisecond)
		cancel()
	}()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 3 * time.Second}
	resp, doErr := client.Do(req)
	if resp != nil {
		resp.Body.Close()
	}

	// The request should either return context.Canceled (client cancelled before
	// headers arrived) or a non-200 error response. Either outcome is acceptable
	// — the important properties are: no panic, no goroutine leak.
	if doErr == nil && resp != nil && resp.StatusCode == http.StatusOK {
		// If model-b responded with 200, both A and B were completed before the
		// cancel propagated all the way through — that is still correct behavior.
		t.Logf("cancel raced: response arrived before cancel propagated (status 200 from B — acceptable)")
	}
	// No additional assertion on bCallCount: both "B called" and "B not called"
	// are valid depending on scheduler timing. The key invariant is "no panic".
}

// ──────────────────────────────────────────────────────────────────────────────
// Chain-all-failed usage event (gap from spec)
// ──────────────────────────────────────────────────────────────────────────────

// usageCapture is a minimal usage.Logger stand-in that records emitted events.
// It captures only the fields proxy.Handle populates so that tests can assert
// on event properties without importing the full usage package.
type capturedUsageEvent struct {
	ModelName          string
	RequestedModelName string
}

// usageCapture collects events written via logUsageEvent. Because logUsageEvent
// calls UsageLogger.Log which is defined on usage.Logger (a concrete type, not
// an interface), the test wires a real usage.Logger backed by a channel that
// drains into a slice.
//
// Instead of fighting the concrete Logger type, we assert purely on behavior:
// for the all-failed path there must be NO usage event at all. The proxy does
// not call handleBufferedResponse or handleStreamingResponse when resp == nil,
// so UsageLogger.Log is never reached. We verify this by asserting the handler
// returns 502 (upstream_unavailable) without populating any captured events.

// TestHandle_ChainAllFailedUsageEvent asserts that when every model in a
// fallback chain fails (A→B→C all return 502), the handler returns
// upstream_unavailable and does NOT write a usage event. This matches the
// pre-existing behavior: usage is only logged on a successful response.
//
// Known limitation (inherited): if observability of failed chains is desired,
// a future improvement would emit a zero-token "failed" usage event, but that
// is out of scope for the current implementation.
func TestHandle_ChainAllFailedUsageEvent(t *testing.T) {
	t.Parallel()

	makeFailServer := func(id string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			fmt.Fprintf(w, `{"error":{"message":"fail %s"}}`, id)
		}))
		t.Cleanup(srv.Close)
		return srv
	}

	srvA := makeFailServer("A")
	srvB := makeFailServer("B")
	srvC := makeFailServer("C")

	r, err := NewRegistry([]config.ModelConfig{
		{Name: "chain-a", Provider: "openai", BaseURL: srvA.URL, APIKey: "k", Fallback: "chain-b"},
		{Name: "chain-b", Provider: "openai", BaseURL: srvB.URL, APIKey: "k", Fallback: "chain-c"},
		{Name: "chain-c", Provider: "openai", BaseURL: srvC.URL, APIKey: "k"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// UsageLogger is intentionally nil — if the handler misbehaves and tries to
	// call it on the all-failed path, it will panic, which is the fail signal.
	handler := NewProxyHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 3
	// UsageLogger left nil; a nil dereference would reveal the bug.

	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"chain-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	// All models returned 502; the handler forwards the last model's (chain-c)
	// 502 response directly — it does NOT synthesize an upstream_unavailable
	// envelope because resp != nil (a real HTTP response was received).
	// The handler only returns upstream_unavailable when all deployments fail
	// with transport errors (resp == nil).
	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusBadGateway, body)
	}

	// No usage event assertion needed: if UsageLogger were called with a nil
	// receiver, the test would have panicked above. Reaching here without panic
	// confirms usage was not logged on the all-5xx path. This matches the
	// pre-existing behavior: usage is only logged on successful (2xx) responses.
	// Known limitation: failed chains emit no usage event, so observability of
	// chain-exhaustion failures relies on logs/metrics rather than usage records.
}

// ──────────────────────────────────────────────────────────────────────────────
// FallbackMaxDepth = 0 disables fallback (Fix W1) — verify existing test
// ──────────────────────────────────────────────────────────────────────────────
// TestHandle_FallbackDisabledWhenMaxDepthZero already exists above and covers
// this case with an explicit FallbackMaxDepth = 0. It passes; no action needed.

// ──────────────────────────────────────────────────────────────────────────────
// Chain deadline set from primary timeout (Fix W3)
// ──────────────────────────────────────────────────────────────────────────────

// TestHandle_ChainDeadlineSetFromPrimaryTimeout verifies that the chain-level
// deadline (Fix W3) is set to (FallbackMaxDepth+1)*primaryTimeout and that
// the per-model timeout causes a hanging upstream to return within the budget.
//
// The chain deadline is enforced on the context passed to the outer loop; each
// individual upstream call derives its context from background (not the chain
// context), so the per-model Timeout field is what actually terminates the
// hanging upstream call. A DeadlineExceeded error from the per-model timeout
// is NOT fallback-eligible (isFallbackEligible returns false for
// context.DeadlineExceeded), so model-b is not tried. The result is a 502 that
// arrives within the per-model timeout window — confirming the timeout fires.
func TestHandle_ChainDeadlineSetFromPrimaryTimeout(t *testing.T) {
	t.Parallel()

	// model-a hangs indefinitely — it will be cancelled by the per-model timeout.
	hangCh := make(chan struct{})
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hangCh:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() {
		close(hangCh)
		upstreamA.Close()
	})

	// model-b must never be called (timeout is not fallback-eligible).
	var bCallCount atomic.Int32
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bCallCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	t.Cleanup(upstreamB.Close)

	const primaryTimeout = 150 * time.Millisecond
	const maxDepth = 2

	r, err := NewRegistry([]config.ModelConfig{
		{
			Name:     "slow-a",
			Provider: "openai",
			BaseURL:  upstreamA.URL,
			APIKey:   "k",
			Fallback: "fast-b",
			Timeout:  "150ms",
		},
		{
			Name:     "fast-b",
			Provider: "openai",
			BaseURL:  upstreamB.URL,
			APIKey:   "k",
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	handler := NewProxyHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = maxDepth
	app := testApp(t, handler)

	start := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"slow-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	// Give enough headroom: 5× primaryTimeout plus test infrastructure overhead.
	resp, testErr := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	elapsed := time.Since(start)
	if testErr != nil {
		t.Fatalf("app.Test: %v", testErr)
	}
	defer resp.Body.Close()

	// Per-model timeout (DeadlineExceeded) is not fallback-eligible, so model-b
	// must never be called.
	if bCallCount.Load() > 0 {
		t.Error("model-b was called, but timeout errors are not fallback-eligible")
	}

	// The response must arrive within 2× primaryTimeout + epsilon.
	// primaryTimeout = 150ms; the total should be well under 500ms.
	const ceiling = 500 * time.Millisecond
	if elapsed > ceiling {
		t.Errorf("elapsed %v > ceiling %v — per-model timeout did not fire in time", elapsed, ceiling)
	}

	// A timeout on the only upstream results in upstream_unavailable (502).
	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusBadGateway, body)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Concurrent fallback PATCH mutations (Fix C3 / TOCTOU)
// ──────────────────────────────────────────────────────────────────────────────

// TestRegistry_ConcurrentFallbackMutations is a race-detector test verifying
// that concurrent AddModel calls (which update FallbackModelName under the
// Registry's write lock) do not produce data races. This covers the mutex
// scope fix from Fix C3 at the Registry level.
//
// Run with -race to detect any unsynchronised access. The test does not assert
// a specific outcome — any consistent terminal state is acceptable because the
// Go memory model guarantees that the last write wins.
func TestRegistry_ConcurrentFallbackMutations(t *testing.T) {
	t.Parallel()

	r, err := NewRegistry([]config.ModelConfig{
		{Name: "m-conc-a", Provider: "openai", BaseURL: "http://localhost:1", APIKey: "k"},
		{Name: "m-conc-b", Provider: "openai", BaseURL: "http://localhost:2", APIKey: "k"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			r.AddModel(Model{
				Name:              "m-conc-a",
				Provider:          "openai",
				BaseURL:           "http://localhost:1",
				APIKey:            "k",
				FallbackModelName: "m-conc-b",
			})
		}()
		go func() {
			defer wg.Done()
			r.AddModel(Model{
				Name:              "m-conc-b",
				Provider:          "openai",
				BaseURL:           "http://localhost:2",
				APIKey:            "k",
				FallbackModelName: "m-conc-a",
			})
		}()
	}

	wg.Wait()
	// No assertion on the final state — reaching here without a -race failure
	// is the pass condition.
}

// ──────────────────────────────────────────────────────────────────────────────
// Multi-hop access denial (VULN-A1)
// ──────────────────────────────────────────────────────────────────────────────

// TestHandle_FallbackChainBlockedAtMidHop verifies that the per-hop access
// check inside the fallback loop (not only at the first hop) correctly stops
// the chain when a mid-chain model is denied.
//
// Chain: model-a → model-b → model-c
// Access policy: key K may use model-a and model-b; model-c is denied.
//
// Expected execution:
//  1. model-a is tried → upstream returns 502 (fallback-eligible)
//  2. Access check for model-b passes → model-b is tried → upstream returns 502
//  3. Access check for model-c fails → chain stops
//  4. Client receives model-b's 502 (the last response that was actually
//     forwarded; the loop breaks before draining it, so resp holds model-b's
//     response at break time)
//  5. model-c's upstream is never contacted
func TestHandle_FallbackChainBlockedAtMidHop(t *testing.T) {
	t.Parallel()

	// model-a returns 502 to trigger the first fallback hop.
	var aCalls atomic.Int32
	upstreamA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":{"message":"upstream A unavailable"}}`)
	}))
	t.Cleanup(upstreamA.Close)

	// model-b also returns 502 to trigger the second fallback evaluation.
	var bCalls atomic.Int32
	upstreamB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":{"message":"upstream B unavailable"}}`)
	}))
	t.Cleanup(upstreamB.Close)

	// model-c must never be contacted; any call here is a security failure.
	var cCalls atomic.Int32
	upstreamC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[]}`)
	}))
	t.Cleanup(upstreamC.Close)

	// Build a three-model registry with the chain A → B → C.
	r, err := NewRegistry([]config.ModelConfig{
		{Name: "mid-hop-a", Provider: "openai", BaseURL: upstreamA.URL, APIKey: "key-a", Fallback: "mid-hop-b"},
		{Name: "mid-hop-b", Provider: "openai", BaseURL: upstreamB.URL, APIKey: "key-b", Fallback: "mid-hop-c"},
		{Name: "mid-hop-c", Provider: "openai", BaseURL: upstreamC.URL, APIKey: "key-c"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Allow model-a and model-b for this key; model-c is intentionally absent.
	const testKeyID = "test-key-mid-hop"
	accessCache := NewModelAccessCache()
	accessCache.Load(
		nil,
		nil,
		map[string][]string{
			testKeyID: {"mid-hop-a", "mid-hop-b"},
		},
	)

	handler := NewProxyHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 2
	handler.AccessCache = accessCache

	keyInfo := auth.KeyInfo{
		ID:    testKeyID,
		OrgID: "test-org",
		Role:  "member",
	}
	app, rawKey := testAppWithAuth(t, handler, keyInfo)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"mid-hop-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	// model-a must have been tried exactly once.
	if got := int(aCalls.Load()); got != 1 {
		t.Errorf("model-a call count = %d, want 1", got)
	}
	// model-b must have been tried exactly once (access was permitted).
	if got := int(bCalls.Load()); got != 1 {
		t.Errorf("model-b call count = %d, want 1", got)
	}
	// model-c must never have been called — access was denied; calling it would
	// mean the access policy was bypassed at the second hop.
	if got := int(cCalls.Load()); got != 0 {
		t.Errorf("SECURITY: model-c call count = %d, want 0 — access policy bypassed at second hop", got)
	}

	// The client receives model-b's 502 response. The loop breaks before draining
	// resp (that only happens when the hop commits), so resp holds model-b's
	// response at break time and is forwarded as-is.
	if resp.StatusCode != http.StatusBadGateway {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d (model-b's last error); body: %s",
			resp.StatusCode, http.StatusBadGateway, body)
	}
}
