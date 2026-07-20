package proxy

// stream_duration_abort_test.go is the regression test for the MEDIUM
// finding: stream-duration expiry on the ORDINARY (non-PII) streaming path
// produced no client-visible abort. Before the fix, when handleStreamingResponse's
// streamTimer fired and cancelled the upstream connection, scanner.Err() tripped
// but the plain (non-PII) scan loop only logged and recorded a circuit-breaker
// failure — it never set streamIncomplete and never emitted a terminal SSE
// event, so the client just saw the connection stop (no [DONE], no deliberate
// error) and the usage event could be logged as a false 200.

import (
	"context"
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

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/usage"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// streamDurationTestHMACSecret is a fixed HMAC secret used to hash and verify
// the test API key issued by startAuthedTestServer below.
var streamDurationTestHMACSecret = []byte("stream-duration-test-hmac-secret-32b")

// startAuthedTestServer mirrors startTestServer (handler_test.go) but wires
// auth.Middleware ahead of the proxy routes and returns a plaintext API key
// that resolves to keyInfo, so logUsageEvent (which no-ops on a nil
// *auth.KeyInfo) actually fires for the request under test. A real TCP
// listener is required (rather than fiber's app.Test harness) because
// SendStreamWriter's goroutine may still be writing after Handle returns —
// see startTestServer's doc for details.
func startAuthedTestServer(t *testing.T, handler *ProxyHandler, keyInfo auth.KeyInfo) (baseURL, rawKey string) {
	t.Helper()

	keyCache := cache.New[string, auth.KeyInfo]()
	var err error
	rawKey, err = keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("keygen.Generate: %v", err)
	}
	keyCache.Set(keygen.Hash(rawKey, streamDurationTestHMACSecret), keyInfo)

	app := fiber.New()
	app.Use(auth.Middleware(keyCache, streamDurationTestHMACSecret))
	app.Get("/v1/models", handler.ModelsHandler)
	app.All("/v1/*", handler.Handle)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}

	go func() {
		_ = app.Listener(ln, fiber.ListenConfig{DisableStartupMessage: true})
	}()
	t.Cleanup(func() { _ = app.Shutdown() })

	addr := ln.Addr().String()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, dialErr := net.Dial("tcp", addr)
		if dialErr == nil {
			conn.Close()
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	return "http://" + addr, rawKey
}

// TestHandle_Plain_StreamDurationExpiry_AbortEventAndIncompleteUsage drives an
// upstream that keeps streaming SSE chunks (never sending [DONE]) past the
// handler's configured MaxStreamDuration, on a model with no adapter (plain
// passthrough, no PII filter) — the ordinary streaming path. It asserts:
//
//   - the client receives exactly one deliberate, content-free abort event
//     (not a dead connection indistinguishable from a network failure)
//   - no [DONE] sentinel is present (correctly signals an incomplete stream)
//   - the usage event logged for the request reflects the incomplete stream
//     (http.StatusBadGateway), not the upstream's 200
//
// MaxStreamDuration is set to 100ms so the test completes quickly.
func TestHandle_Plain_StreamDurationExpiry_AbortEventAndIncompleteUsage(t *testing.T) {
	t.Parallel()

	// upstreamCanceled is closed once the upstream handler observes its own
	// request context being cancelled — proof that ProxyHandler.Handle's
	// stream timer actually tore the upstream connection down (the class of
	// abnormal termination under test), rather than the upstream finishing
	// or the test racing ahead of it.
	upstreamCanceled := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream ResponseWriter does not implement http.Flusher")
			return
		}

		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"a"}}]}`)
		fmt.Fprintln(w)
		flusher.Flush()

		// Keep streaming indefinitely — a genuine [DONE] is never sent. Only
		// the proxy's own stream-duration cancellation (or the test's own
		// bounded timeout) ends this handler.
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				close(upstreamCanceled)
				return
			case <-ticker.C:
				fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"b"}}]}`)
				fmt.Fprintln(w)
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(upstream.Close)

	reg, err := NewRegistry([]config.ModelConfig{
		{
			Name:     "stream-duration-model",
			Provider: "vllm", // GetAdapter("vllm") == nil: exercises the plain scan loop.
			BaseURL:  upstream.URL,
			APIKey:   "upstream-secret",
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	d := openStage0cDB(t)
	usageCfg := config.UsageConfig{BufferSize: 64, FlushInterval: time.Hour}
	ul := usage.NewLogger(d, usageCfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ul.Start()

	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.UsageLogger = ul
	h.MaxStreamDuration = 100 * time.Millisecond

	baseURL, rawKey := startAuthedTestServer(t, h, auth.KeyInfo{
		ID:      "key-stream-duration",
		KeyType: "user_key",
		OrgID:   "org-stream-duration",
	})

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(`{"model":"stream-duration-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
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
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusOK, body)
	}

	fullBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read streamed body: %v", err)
	}
	fullStr := string(fullBody)

	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never observed request cancellation — the stream-duration timer did not fire")
	}

	// No [DONE]: a client that only checks for the sentinel correctly reads
	// this as an incomplete stream.
	if strings.Contains(fullStr, "[DONE]") {
		t.Errorf("[DONE] present after stream-duration expiry, want absent (truncated stream)\noutput: %s", fullStr)
	}

	// Exactly one deliberate, content-free abort event — the same class of
	// event the PII path already emits for this failure mode, and distinct
	// from the adapter-abort code ("stream_transform_aborted") so the two
	// failure modes remain distinguishable. The event's JSON body contains
	// the code twice (as both "type" and "code"), so count \n\n-terminated
	// SSE segments carrying it rather than raw substring occurrences.
	abortSegments := 0
	for _, seg := range strings.Split(fullStr, "\n\n") {
		if strings.Contains(seg, "stream_incomplete") {
			abortSegments++
		}
	}
	if abortSegments == 0 {
		t.Fatalf("deliberate abort event absent after stream-duration expiry — client sees a dead connection instead\noutput: %s", fullStr)
	}
	if abortSegments > 1 {
		t.Errorf("abort event emitted in %d segments, want exactly 1\noutput: %s", abortSegments, fullStr)
	}
	if !strings.Contains(fullStr, `"content":"a"`) {
		t.Fatalf("sanity check failed: initial chunk never observed on the wire\noutput: %s", fullStr)
	}

	// Force a synchronous flush so the usage event is guaranteed to be in the
	// database before the assertion below reads it — no polling required.
	ul.Stop()

	var statusCode int
	row := d.SQL().QueryRowContext(context.Background(),
		"SELECT status_code FROM usage_events ORDER BY rowid DESC LIMIT 1")
	if scanErr := row.Scan(&statusCode); scanErr != nil {
		t.Fatalf("query usage_events: %v", scanErr)
	}
	if statusCode != http.StatusBadGateway {
		t.Errorf("usage event status_code = %d, want %d (incomplete stream, not the upstream's 200)", statusCode, http.StatusBadGateway)
	}
}
