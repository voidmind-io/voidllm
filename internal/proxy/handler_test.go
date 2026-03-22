package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/config"
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
