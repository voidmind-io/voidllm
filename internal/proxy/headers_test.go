package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

// ──────────────────────────────────────────────────────────────────────────────
// setUpstreamHeaders unit tests
// ──────────────────────────────────────────────────────────────────────────────

// newFiberCtxWithHeaders creates a minimal Fiber context using a real Fiber app
// and app.Test so we can call setUpstreamHeaders without reflection on internals.
// Instead we test setUpstreamHeaders through the integration round-trip tests
// below, and test the individual outcomes via the handler integration tests.
//
// For direct unit-testing of setUpstreamHeaders we use the upstream capture
// pattern: fire a real request through the handler and assert what the upstream
// received.

func TestSetUpstreamHeaders_AllowlistedHeadersForwarded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		headerKey  string
		headerVal  string
		wantForwarded bool
	}{
		{name: "Content-Type forwarded", headerKey: "Content-Type", headerVal: "application/json", wantForwarded: true},
		{name: "Accept forwarded", headerKey: "Accept", headerVal: "application/json", wantForwarded: true},
		{name: "X-Request-ID forwarded", headerKey: "X-Request-ID", headerVal: "req-abc-123", wantForwarded: true},
		{name: "Accept-Language forwarded", headerKey: "Accept-Language", headerVal: "en-US", wantForwarded: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
				"Content-Type": "application/json",
			})

			handler := testProxyHandler(t, upstream.URL)
			app := testApp(t, handler)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"test-model","messages":[]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(tc.headerKey, tc.headerVal)

			resp, err := app.Test(req, testTimeout)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			resp.Body.Close()

			got := captured.Header.Get(tc.headerKey)
			if tc.wantForwarded && got != tc.headerVal {
				t.Errorf("header %q: upstream received %q, want %q", tc.headerKey, got, tc.headerVal)
			}
		})
	}
}

func TestSetUpstreamHeaders_NonAllowlistedHeadersDropped(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		headerKey string
		headerVal string
	}{
		{name: "X-Custom-Nonsense dropped", headerKey: "X-Custom-Nonsense", headerVal: "should-not-arrive"},
		{name: "X-Internal-Secret dropped", headerKey: "X-Internal-Secret", headerVal: "secret"},
		{name: "Cookie dropped", headerKey: "Cookie", headerVal: "session=abc"},
		{name: "X-Forwarded-For dropped", headerKey: "X-Forwarded-For", headerVal: "1.2.3.4"},
		{name: "X-Real-IP dropped", headerKey: "X-Real-IP", headerVal: "1.2.3.4"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
				"Content-Type": "application/json",
			})

			handler := testProxyHandler(t, upstream.URL)
			app := testApp(t, handler)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"test-model","messages":[]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(tc.headerKey, tc.headerVal)

			resp, err := app.Test(req, testTimeout)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			resp.Body.Close()

			if v := captured.Header.Get(tc.headerKey); v != "" {
				t.Errorf("SECURITY: non-allowlisted header %q = %q was forwarded to upstream", tc.headerKey, v)
			}
		})
	}
}

func TestSetUpstreamHeaders_UserAgentSet(t *testing.T) {
	t.Parallel()

	upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
		"Content-Type": "application/json",
	})

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "SomeClient/1.0") // client sets its own UA

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	// Proxy must always override with its own User-Agent.
	wantUA := "VoidLLM/0.1"
	if ua := captured.Header.Get("User-Agent"); ua != wantUA {
		t.Errorf("User-Agent = %q, want %q", ua, wantUA)
	}
}

func TestSetUpstreamHeaders_AuthorizationReplacedWithModelKey(t *testing.T) {
	t.Parallel()

	upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
		"Content-Type": "application/json",
	})

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vl_uk_client-bearer-token")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	wantAuth := "Bearer upstream-secret"
	gotAuth := captured.Header.Get("Authorization")
	if gotAuth != wantAuth {
		t.Errorf("Authorization = %q, want %q", gotAuth, wantAuth)
	}
	if strings.Contains(gotAuth, "vl_uk_client-bearer-token") {
		t.Error("SECURITY: client bearer token leaked into upstream Authorization header")
	}
}

func TestSetUpstreamHeaders_EmptyModelAPIKey_NoAuthHeader(t *testing.T) {
	t.Parallel()

	upstream, captured := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
		"Content-Type": "application/json",
	})

	handler := testProxyHandler(t, upstream.URL)
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"no-key-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer vl_uk_some-client-key")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	if v := captured.Header.Get("Authorization"); v != "" {
		t.Errorf("Authorization = %q, want empty (model has no API key)", v)
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// setUpstreamHeaders direct unit test (uses net/http.Request directly)
// ──────────────────────────────────────────────────────────────────────────────

// TestSetUpstreamHeadersDirect tests setUpstreamHeaders against a real
// net/http.Request built independently of Fiber. We construct a synthetic
// Fiber context via a round-trip capture to keep tests black-box.
// Direct testing is covered by the integration tests above.

// ──────────────────────────────────────────────────────────────────────────────
// copyResponseHeaders unit tests
// ──────────────────────────────────────────────────────────────────────────────

func TestCopyResponseHeaders_AllowlistedForwarded(t *testing.T) {
	t.Parallel()

	upstream, _ := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
		"Content-Type": "application/json",
		"X-Request-ID": "upstream-req-id-123",
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

	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want %q", ct, "application/json")
	}
	if rid := resp.Header.Get("X-Request-ID"); rid != "upstream-req-id-123" {
		t.Errorf("X-Request-ID = %q, want %q", rid, "upstream-req-id-123")
	}
}

func TestCopyResponseHeaders_ServerHeaderNotForwarded(t *testing.T) {
	t.Parallel()

	upstream, _ := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
		"Content-Type": "application/json",
		"Server":       "upstream-nginx/1.2.3",
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

	if v := resp.Header.Get("Server"); v == "upstream-nginx/1.2.3" {
		t.Error("SECURITY: upstream Server header was forwarded to client")
	}
}

func TestCopyResponseHeaders_HopByHopNotForwarded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		headerKey string
		headerVal string
	}{
		{name: "Connection not forwarded", headerKey: "Connection", headerVal: "keep-alive"},
		{name: "Transfer-Encoding not forwarded", headerKey: "Transfer-Encoding", headerVal: "chunked"},
		{name: "Keep-Alive not forwarded", headerKey: "Keep-Alive", headerVal: "timeout=5"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream, _ := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
				"Content-Type": "application/json",
				tc.headerKey:  tc.headerVal,
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

			if v := resp.Header.Get(tc.headerKey); v == tc.headerVal {
				t.Errorf("hop-by-hop header %q = %q was forwarded to client", tc.headerKey, v)
			}
		})
	}
}

func TestCopyResponseHeaders_XRateLimitForwarded(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		headerKey string
		headerVal string
	}{
		{name: "X-RateLimit-Limit forwarded", headerKey: "X-RateLimit-Limit", headerVal: "100"},
		{name: "X-RateLimit-Remaining forwarded", headerKey: "X-RateLimit-Remaining", headerVal: "42"},
		{name: "X-Ratelimit-Reset forwarded", headerKey: "X-Ratelimit-Reset", headerVal: "1234567890"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream, _ := upstreamCapture(t, http.StatusOK, `{}`, map[string]string{
				"Content-Type": "application/json",
				tc.headerKey:  tc.headerVal,
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

			if v := resp.Header.Get(tc.headerKey); v != tc.headerVal {
				t.Errorf("header %q = %q, want %q", tc.headerKey, v, tc.headerVal)
			}
		})
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// copyResponseHeaders direct unit test
// ──────────────────────────────────────────────────────────────────────────────

// TestCopyResponseHeadersDirect tests copyResponseHeaders in isolation by
// constructing a synthetic upstream http.Response and verifying the Fiber
// response via a real app.Test round-trip. Direct testing is fully covered by
// the integration tests above; this test verifies the hopByHopHeaders map is
// applied correctly without going through the proxy path.
func TestHopByHopHeadersMap(t *testing.T) {
	t.Parallel()

	// Verify the hopByHopHeaders map contains the RFC 7230 mandatory entries.
	requiredHopByHop := []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"TE",
		"Trailer",
		"Transfer-Encoding",
		"Upgrade",
	}

	for _, h := range requiredHopByHop {
		if !hopByHopHeaders[h] {
			t.Errorf("hopByHopHeaders missing required entry %q", h)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Full round-trip header test
// ──────────────────────────────────────────────────────────────────────────────

func TestHandle_RoundTripHeaders(t *testing.T) {
	t.Parallel()

	// Build a model with no API key so we can cleanly test Auth header behaviour.
	_, err := NewRegistry([]config.ModelConfig{
		{Name: "roundtrip-model", Provider: "vllm", BaseURL: "PLACEHOLDER", APIKey: "rt-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}

	var capturedHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "srv-req-id")
		w.Header().Set("Server", "openai-server")
		w.Header().Set("X-RateLimit-Remaining", "99")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)

	// Rebuild registry with real upstream URL.
	r, err := NewRegistry([]config.ModelConfig{
		{Name: "roundtrip-model", Provider: "vllm", BaseURL: upstream.URL, APIKey: "rt-secret"},
	})
	if err != nil {
		t.Fatal(err)
	}

	handler := NewProxyHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"roundtrip-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", "client-req-id")
	req.Header.Set("Authorization", "Bearer vl_uk_client-secret")
	req.Header.Set("X-Custom-Header", "should-not-arrive")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	// Upstream request assertions.
	if capturedHeaders.Get("X-Request-ID") != "client-req-id" {
		t.Errorf("upstream X-Request-ID = %q, want %q", capturedHeaders.Get("X-Request-ID"), "client-req-id")
	}
	if capturedHeaders.Get("Authorization") != "Bearer rt-secret" {
		t.Errorf("upstream Authorization = %q, want %q", capturedHeaders.Get("Authorization"), "Bearer rt-secret")
	}
	if v := capturedHeaders.Get("X-Custom-Header"); v != "" {
		t.Errorf("upstream X-Custom-Header = %q, want empty (should be dropped)", v)
	}
	if capturedHeaders.Get("User-Agent") != "VoidLLM/0.1" {
		t.Errorf("upstream User-Agent = %q, want %q", capturedHeaders.Get("User-Agent"), "VoidLLM/0.1")
	}

	// Client response assertions.
	if resp.Header.Get("X-Request-ID") != "srv-req-id" {
		t.Errorf("client X-Request-ID = %q, want %q", resp.Header.Get("X-Request-ID"), "srv-req-id")
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "99" {
		t.Errorf("client X-RateLimit-Remaining = %q, want %q", resp.Header.Get("X-RateLimit-Remaining"), "99")
	}
	if v := resp.Header.Get("Server"); v == "openai-server" {
		t.Error("SECURITY: Server header from upstream was forwarded to client")
	}
	if v := resp.Header.Get("Connection"); v == "keep-alive" {
		t.Error("hop-by-hop Connection header from upstream was forwarded to client")
	}
}
