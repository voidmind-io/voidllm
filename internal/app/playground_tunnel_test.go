package app

import (
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

	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/proxy"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// testTunnelHMACSecret is a fixed HMAC secret used across the playground
// tunnel tests to hash and verify test API keys via auth.Middleware.
var testTunnelHMACSecret = []byte("playground-tunnel-test-hmac-secret-32b")

// tunnelCapturedRequest records the last request received by a test upstream
// server so assertions can be made against the path/method the proxy handler
// actually forwarded.
type tunnelCapturedRequest struct {
	Method string
	Path   string
}

// tunnelUpstream starts an httptest.Server that records the last request it
// received into a *tunnelCapturedRequest and always replies with a canned
// chat-completion-shaped JSON body.
func tunnelUpstream(t *testing.T) (*httptest.Server, *tunnelCapturedRequest) {
	t.Helper()
	captured := &tunnelCapturedRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Method = r.Method
		captured.Path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// newTunnelTestApp builds a minimal, real *Application wired for setupRoutes:
// a proxy handler forwarding to upstreamURL for "test-model", an in-memory
// key cache, and a bare admin handler (never invoked by these tests — its
// routes are registered but not exercised). adminPort mirrors
// config.AdminConfig.Port semantics: 0 (or equal to proxyPort) selects
// single-port mode, any other value selects dual-port mode.
func newTunnelTestApp(t *testing.T, upstreamURL string, proxyPort, adminPort int, proxyCfg config.ProxyConfig) *Application {
	t.Helper()

	reg, err := proxy.NewRegistry([]config.ModelConfig{
		{
			Name:     "test-model",
			Provider: "vllm",
			BaseURL:  upstreamURL,
			APIKey:   "upstream-secret",
		},
	})
	if err != nil {
		t.Fatalf("proxy.NewRegistry: %v", err)
	}

	silent := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxyHandler := proxy.NewProxyHandler(reg, silent)

	proxyCfg.Port = proxyPort

	a := &Application{
		cfg: &config.Config{
			Server: config.ServerConfig{
				Proxy: proxyCfg,
				Admin: config.AdminConfig{Port: adminPort},
			},
		},
		log:          silent,
		keyCache:     cache.New[string, auth.KeyInfo](),
		hmacSecret:   testTunnelHMACSecret,
		proxyHandler: proxyHandler,
		adminHandler: &admin.Handler{Log: silent},
	}
	a.setupRoutes()

	t.Cleanup(func() {
		_ = a.proxyApp.Shutdown()
		if a.adminApp != nil {
			_ = a.adminApp.Shutdown()
		}
	})
	return a
}

// issueTunnelTestKey generates a plaintext API key, hashes it with the app's
// HMAC secret, and seeds it into the app's key cache with keyInfo. Returns
// the plaintext key, suitable for use in an Authorization: Bearer header.
func issueTunnelTestKey(t *testing.T, a *Application, keyInfo auth.KeyInfo) string {
	t.Helper()
	rawKey, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("keygen.Generate: %v", err)
	}
	a.keyCache.Set(keygen.Hash(rawKey, a.hmacSecret), keyInfo)
	return rawKey
}

// tunnelTestTimeout is the per-request timeout passed to fiber's app.Test.
var tunnelTestTimeout = fiber.TestConfig{Timeout: 5 * time.Second}

// ──────────────────────────────────────────────────────────────────────────
// Dual-port mode: the tunnel must reach the proxy handler from the admin app.
// ──────────────────────────────────────────────────────────────────────────

func TestPlaygroundTunnel_DualPort_ChatCompletionsReachesProxy(t *testing.T) {
	t.Parallel()

	upstream, captured := tunnelUpstream(t)
	a := newTunnelTestApp(t, upstream.URL, 18080, 18443, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-1", OrgID: "org-1", Role: "member"})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := a.adminApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("adminApp.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, http.StatusOK, body)
	}
	if captured.Path != "/chat/completions" {
		t.Errorf("upstream path = %q, want %q", captured.Path, "/chat/completions")
	}
}

func TestPlaygroundTunnel_DualPort_EmbeddingsReachesProxy(t *testing.T) {
	t.Parallel()

	upstream, captured := tunnelUpstream(t)
	a := newTunnelTestApp(t, upstream.URL, 18081, 18444, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-2", OrgID: "org-1", Role: "member"})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/embeddings",
		strings.NewReader(`{"model":"test-model","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := a.adminApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("adminApp.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, http.StatusOK, body)
	}
	if captured.Path != "/embeddings" {
		t.Errorf("upstream path = %q, want %q", captured.Path, "/embeddings")
	}
}

// TestPlaygroundTunnel_DualPort_V1NotServedOnAdminApp is a regression test:
// /v1/* must remain unmounted on the admin app in dual-port mode. Only the
// authenticated /api/v1/playground/* tunnel reaches the proxy handler there.
func TestPlaygroundTunnel_DualPort_V1NotServedOnAdminApp(t *testing.T) {
	t.Parallel()

	upstream, captured := tunnelUpstream(t)
	a := newTunnelTestApp(t, upstream.URL, 18082, 18445, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-3", OrgID: "org-1", Role: "member"})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := a.adminApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("adminApp.Test: %v", err)
	}
	defer resp.Body.Close()

	// /v1/* has no registered POST route on the admin app in dual-port mode —
	// only the GET "/*" SPA catch-all exists. Fiber therefore replies with
	// 405 Method Not Allowed (this was the exact pre-fix symptom described in
	// the issue), not 200/502 from the proxy handler. Decisively, the proxy
	// handler was never invoked, so the upstream never saw the request.
	if resp.StatusCode != http.StatusMethodNotAllowed {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d (POST /v1/* must not be routed on the admin app), body = %s",
			resp.StatusCode, http.StatusMethodNotAllowed, body)
	}
	if captured.Path != "" {
		t.Fatalf("SECURITY: /v1/* reached the proxy handler on the admin app in dual-port mode; upstream saw path %q", captured.Path)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Single-port mode: the tunnel must also work, and /v1/* is unchanged.
// ──────────────────────────────────────────────────────────────────────────

func TestPlaygroundTunnel_SinglePort_TunnelAndV1BothWork(t *testing.T) {
	t.Parallel()

	upstream, captured := tunnelUpstream(t)
	a := newTunnelTestApp(t, upstream.URL, 18083, 0, config.ProxyConfig{})
	if a.adminApp != nil {
		t.Fatalf("adminApp should be nil in single-port mode")
	}
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-4", OrgID: "org-1", Role: "member"})

	// The tunnel works on the single combined app.
	tunnelReq := httptest.NewRequest(http.MethodPost, "/api/v1/playground/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	tunnelReq.Header.Set("Content-Type", "application/json")
	tunnelReq.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := a.proxyApp.Test(tunnelReq, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("proxyApp.Test (tunnel): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("tunnel status = %d, want %d, body = %s", resp.StatusCode, http.StatusOK, body)
	}
	if captured.Path != "/chat/completions" {
		t.Errorf("upstream path = %q, want %q", captured.Path, "/chat/completions")
	}

	// Direct /v1/* still works unchanged.
	v1Req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	v1Req.Header.Set("Content-Type", "application/json")
	v1Req.Header.Set("Authorization", "Bearer "+rawKey)

	resp2, err := a.proxyApp.Test(v1Req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("proxyApp.Test (/v1/*): %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("/v1/* status = %d, want %d, body = %s", resp2.StatusCode, http.StatusOK, body)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Auth: the tunnel enforces the same Bearer-key middleware as /v1/*.
// ──────────────────────────────────────────────────────────────────────────

func TestPlaygroundTunnel_Auth_MissingKey(t *testing.T) {
	t.Parallel()

	upstream, _ := tunnelUpstream(t)
	a := newTunnelTestApp(t, upstream.URL, 18084, 18446, config.ProxyConfig{})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.adminApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("adminApp.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestPlaygroundTunnel_Auth_InvalidKey(t *testing.T) {
	t.Parallel()

	upstream, _ := tunnelUpstream(t)
	a := newTunnelTestApp(t, upstream.URL, 18085, 18447, config.ProxyConfig{})

	invalidKey, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("keygen.Generate: %v", err)
	}
	// Deliberately NOT seeded into a.keyCache — hash lookup will miss.

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+invalidKey)

	resp, err := a.adminApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("adminApp.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Path handling: rewrite to the canonical /v1/<rest> path, isAllowedPath
// still gates disallowed endpoints reached through the tunnel.
// ──────────────────────────────────────────────────────────────────────────

func TestPlaygroundTunnel_PathRewriting_AllowedPath(t *testing.T) {
	t.Parallel()

	upstream, captured := tunnelUpstream(t)
	a := newTunnelTestApp(t, upstream.URL, 18086, 18448, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-5", OrgID: "org-1", Role: "member"})

	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := a.adminApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("adminApp.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, http.StatusOK, body)
	}
	if captured.Path != "/chat/completions" {
		t.Errorf("upstream path = %q, want %q", captured.Path, "/chat/completions")
	}
}

func TestPlaygroundTunnel_PathRewriting_DisallowedPathRejected(t *testing.T) {
	t.Parallel()

	upstream, captured := tunnelUpstream(t)
	a := newTunnelTestApp(t, upstream.URL, 18087, 18449, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-6", OrgID: "org-1", Role: "member"})

	// "fine-tunes" is not in isAllowedPath's allow-list.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playground/fine-tunes",
		strings.NewReader(`{"model":"test-model"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	resp, err := a.adminApp.Test(req, tunnelTestTimeout)
	if err != nil {
		t.Fatalf("adminApp.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d, body = %s", resp.StatusCode, http.StatusBadRequest, body)
	}
	if captured.Path != "" {
		t.Errorf("upstream should never have been called for a disallowed path, got path %q", captured.Path)
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Streaming: SSE responses pass through the tunnel unbuffered.
// ──────────────────────────────────────────────────────────────────────────

// startTunnelListener starts fiberApp on a real TCP listener (fiber's
// app.Test harness buffers responses and is unsuitable for asserting on
// streaming behaviour) and returns its base URL.
func startTunnelListener(t *testing.T, fiberApp *fiber.App) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	go func() {
		// Listener is closed by fiberApp.Shutdown via t.Cleanup in newTunnelTestApp.
		_ = fiberApp.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
	}()

	addr := ln.Addr().String()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.Dial("tcp", addr)
		if dialErr == nil {
			_ = conn.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return "http://" + addr
}

func TestPlaygroundTunnel_Streaming(t *testing.T) {
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
			fmt.Fprintln(w)
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	a := newTunnelTestApp(t, upstream.URL, 18088, 18450, config.ProxyConfig{})
	rawKey := issueTunnelTestKey(t, a, auth.KeyInfo{ID: "key-7", OrgID: "org-1", Role: "member"})
	baseURL := startTunnelListener(t, a.adminApp)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/playground/chat/completions",
		strings.NewReader(`{"model":"test-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read streaming response: %v", err)
	}
	bodyStr := string(body)
	for _, want := range chunks {
		if !strings.Contains(bodyStr, want) {
			t.Errorf("streaming response missing chunk %q", want)
		}
	}
}

// ──────────────────────────────────────────────────────────────────────────
// Admin app timeouts / body limit: adopt the proxy's configured values so
// the tunnel can carry long-lived streaming completions, but only within
// [floor, maxAdminTunnelTimeout] / [floor, maxAdminTunnelBodyLimit] — see
// those constants in routes.go for why the cap exists (Fiber v3 has no
// per-route timeout, so an unbounded value here would also apply to the
// admin app's unauthenticated endpoints).
// ──────────────────────────────────────────────────────────────────────────

// TestSetupRoutes_AdminTimeoutsFloorWhenProxySmaller covers the regime where
// the proxy's configured values are BELOW the admin app's own floor: the
// floor (30s / fiber.DefaultBodyLimit) applies.
func TestSetupRoutes_AdminTimeoutsFloorWhenProxySmaller(t *testing.T) {
	t.Parallel()

	upstream, _ := tunnelUpstream(t)
	// Proxy configured with values below the admin app's own 30s/4MB floor.
	proxyCfg := config.ProxyConfig{
		ReadTimeout:    5 * time.Second,
		WriteTimeout:   5 * time.Second,
		MaxRequestBody: 1024,
	}
	a := newTunnelTestApp(t, upstream.URL, 18090, 18452, proxyCfg)

	cfg := a.adminApp.Config()
	if cfg.ReadTimeout != 30*time.Second {
		t.Errorf("admin ReadTimeout = %v, want the 30s floor", cfg.ReadTimeout)
	}
	if cfg.WriteTimeout != 30*time.Second {
		t.Errorf("admin WriteTimeout = %v, want the 30s floor", cfg.WriteTimeout)
	}
	if cfg.BodyLimit != fiber.DefaultBodyLimit {
		t.Errorf("admin BodyLimit = %d, want Fiber's default floor %d", cfg.BodyLimit, fiber.DefaultBodyLimit)
	}
}

// TestSetupRoutes_AdminTimeoutsAdoptProxyBetweenFloorAndCap covers the
// regime where the proxy's configured values sit strictly between the floor
// and maxAdminTunnelTimeout/maxAdminTunnelBodyLimit: the proxy's values are
// adopted as-is.
func TestSetupRoutes_AdminTimeoutsAdoptProxyBetweenFloorAndCap(t *testing.T) {
	t.Parallel()

	upstream, _ := tunnelUpstream(t)
	proxyCfg := config.ProxyConfig{
		ReadTimeout:    60 * time.Second,
		WriteTimeout:   90 * time.Second,
		MaxRequestBody: 6 * 1024 * 1024,
	}
	a := newTunnelTestApp(t, upstream.URL, 18089, 18451, proxyCfg)

	cfg := a.adminApp.Config()
	if cfg.ReadTimeout != proxyCfg.ReadTimeout {
		t.Errorf("admin ReadTimeout = %v, want the adopted proxy value %v", cfg.ReadTimeout, proxyCfg.ReadTimeout)
	}
	if cfg.WriteTimeout != proxyCfg.WriteTimeout {
		t.Errorf("admin WriteTimeout = %v, want the adopted proxy value %v", cfg.WriteTimeout, proxyCfg.WriteTimeout)
	}
	if cfg.BodyLimit != proxyCfg.MaxRequestBody {
		t.Errorf("admin BodyLimit = %d, want the adopted proxy value %d", cfg.BodyLimit, proxyCfg.MaxRequestBody)
	}
}

// TestSetupRoutes_AdminTimeoutsCappedWhenProxyLarger covers the regime where
// the proxy's configured values are ABOVE maxAdminTunnelTimeout /
// maxAdminTunnelBodyLimit: the cap applies instead of the proxy value. This
// is the security-relevant regression test — without the cap, a generous
// proxy timeout would be adopted unbounded onto the admin app's
// unauthenticated endpoints (e.g. POST /api/v1/auth/login).
func TestSetupRoutes_AdminTimeoutsCappedWhenProxyLarger(t *testing.T) {
	t.Parallel()

	upstream, _ := tunnelUpstream(t)
	proxyCfg := config.ProxyConfig{
		ReadTimeout:    150 * time.Second,
		WriteTimeout:   300 * time.Second,
		MaxRequestBody: 30 * 1024 * 1024,
	}
	a := newTunnelTestApp(t, upstream.URL, 18091, 18453, proxyCfg)

	cfg := a.adminApp.Config()
	if cfg.ReadTimeout != maxAdminTunnelTimeout {
		t.Errorf("admin ReadTimeout = %v, want capped at %v", cfg.ReadTimeout, maxAdminTunnelTimeout)
	}
	if cfg.WriteTimeout != maxAdminTunnelTimeout {
		t.Errorf("admin WriteTimeout = %v, want capped at %v", cfg.WriteTimeout, maxAdminTunnelTimeout)
	}
	if cfg.BodyLimit != maxAdminTunnelBodyLimit {
		t.Errorf("admin BodyLimit = %d, want capped at %d", cfg.BodyLimit, maxAdminTunnelBodyLimit)
	}
}
