package proxy

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/shutdown"
)

func waitInFlight(t *testing.T, st *shutdown.State, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st.InFlight() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("InFlight = %d, want %d", st.InFlight(), want)
}

// TestHandle_StreamKeepsShutdownTrackingUntilStreamEnds verifies that Handle
// returning does not release the in-flight slot of a streaming request: the
// stream writer owns it and releases it only once the stream has ended.
func TestHandle_StreamKeepsShutdownTrackingUntilStreamEnds(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)

	const modelName = "shutdown-stream-model"
	h, _, _ := newTerminationTestHandler(t, terminationRegistry(t, modelName, upstream.URL))
	st := shutdown.New()
	h.ShutdownState = st
	baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-shutdown-stream"))

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":true}`, modelName)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read first line: %v", err)
	}
	// Handle has long returned by now; the stream is still live.
	time.Sleep(100 * time.Millisecond)
	if got := st.InFlight(); got != 1 {
		t.Fatalf("InFlight while stream live = %d, want 1", got)
	}

	close(release)
	if _, err := io.Copy(io.Discard, br); err != nil {
		t.Fatalf("drain body: %v", err)
	}
	waitInFlight(t, st, 0)
}

// TestHandle_NonStreamPathsReleaseShutdownTracking verifies buffered responses
// and early errors still release the in-flight slot when Handle returns.
func TestHandle_NonStreamPathsReleaseShutdownTracking(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	t.Cleanup(upstream.Close)

	const modelName = "shutdown-buffered-model"
	h, _, _ := newTerminationTestHandler(t, terminationRegistry(t, modelName, upstream.URL))
	st := shutdown.New()
	h.ShutdownState = st
	baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-shutdown-buffered"))

	for name, model := range map[string]string{"buffered": modelName, "unknown model": "no-such-model"} {
		req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
			strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}]}`, model)))
		if err != nil {
			t.Fatalf("%s: build request: %v", name, err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+rawKey)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: do request: %v", name, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		waitInFlight(t, st, 0)
	}
}

// TestHandle_NativeDisconnectDrain_ReleasesProbeSlotOnce verifies that a
// native stream whose client disconnects frees the half-open probe slot while
// the upstream is still being drained, and records no second outcome later.
func TestHandle_NativeDisconnectDrain_ReleasesProbeSlotOnce(t *testing.T) {
	t.Parallel()

	finish := make(chan struct{})
	var finishOnce sync.Once
	closeFinish := func() { finishOnce.Do(func() { close(finish) }) }
	t.Cleanup(closeFinish)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":7,\"output_tokens\":1}}}\n\n")
		fl.Flush()
		for {
			select {
			case <-finish:
				fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n\n")
				fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				fl.Flush()
				return
			case <-r.Context().Done():
				return
			case <-ticker.C:
				fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"b\"}}\n\n")
				fl.Flush()
			}
		}
	}))
	t.Cleanup(upstream.Close)

	const modelName = "native-drain-model"
	reg, err := NewRegistry([]config.ModelConfig{{Name: modelName, Provider: "anthropic", BaseURL: upstream.URL, APIKey: "upstream-secret"}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	h, _, d := newTerminationTestHandler(t, reg)
	depKey := deploymentKey(modelName, modelName)
	h.CircuitBreakers = halfOpenBreakerWithFreeSlot(t, depKey)
	b := h.CircuitBreakers.Get(depKey)
	baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-native-drain"))

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	body := fmt.Sprintf(`{"model":%q,"stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, modelName)
	fmt.Fprintf(conn, "POST /v1/messages HTTP/1.1\r\nHost: %s\r\nx-api-key: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		u.Host, rawKey, len(body), body)
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if _, err := reader.Read(make([]byte, 4096)); err != nil {
		t.Fatalf("read first chunk: %v", err)
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	_ = conn.Close()

	// The upstream is still streaming (drain in progress); the probe slot must
	// be freed. Take it to stand in for another request's probe.
	deadline := time.Now().Add(3 * time.Second)
	for !b.Allow() {
		if time.Now().After(deadline) {
			t.Fatal("probe slot not released while the disconnect drain is running")
		}
		time.Sleep(10 * time.Millisecond)
	}

	closeFinish()
	waitForLoggedStatusCode(t, d)
	// Give any erroneous second record time to land.
	time.Sleep(100 * time.Millisecond)

	// A second neutral record would have released the slot just taken above.
	if b.Allow() {
		t.Error("slot taken during the drain was released again: breaker outcome recorded twice")
	}
}
