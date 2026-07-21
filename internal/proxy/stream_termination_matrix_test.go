package proxy

// stream_termination_matrix_test.go is the regression suite for the plain
// (non-PII) streaming path's termination classification fix (external review
// of the previous commit). It exercises every row of the termination matrix
// from the review:
//
//	clean EOF after [DONE]          -> success, no abort, no downgrade
//	clean EOF WITHOUT [DONE]        -> incomplete, exactly one abort event
//	scanner error BEFORE [DONE]     -> incomplete, exactly one abort event,
//	                                    well-formed SSE framing
//	scanner error AFTER [DONE]      -> success, NO abort, NO downgrade
//	adapter transform error         -> exactly one abort event (already
//	                                    covered end-to-end by
//	                                    TestHandle_Plain_AdapterAbort_FailClosed
//	                                    in stream_abort_test.go; repeated here
//	                                    for a self-contained matrix)
//	write/flush failure, no scanner
//	error pending                   -> disconnect: no abort, no downgrade
//	write failure with a pending
//	scanner error                   -> disconnect: no abort, no downgrade
//	                                    (NOT "incomplete + failed breaker")
//	shutdown/timer cancellation     -> incomplete, exactly one abort event
//	                                    when the client is still connected
//	                                    (already covered end-to-end by
//	                                    TestHandle_Plain_StreamDurationExpiry_AbortEventAndIncompleteUsage
//	                                    in stream_duration_abort_test.go)
//
// Two rows are the actual bugs this suite guards against and were confirmed
// to FAIL against the pre-fix code:
//
//   - TestStreamTermination_CleanEOFWithoutDone_IsIncomplete (clean EOF
//     without [DONE] was silently logged as a 200 success with no abort
//     event before the fix)
//   - TestStreamTermination_ScannerErrorAfterDone_IsSuccess (a scanner error
//     observed strictly after [DONE] was already delivered incorrectly
//     downgraded an already-completed stream to incomplete/502 and emitted a
//     spurious abort event before the fix)
//
// A third row exercises a fix introduced alongside the classification
// rewrite (writeStreamAbortEvent now reports whether its write succeeded so
// the plain path can reclassify a failed best-effort abort delivery as a
// disconnect):
//
//   - TestStreamTermination_WriteFailureWithPendingScannerError_IsDisconnect

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/circuitbreaker"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/usage"
)

// ── shared test scaffolding ────────────────────────────────────────────────

// newTerminationTestHandler builds a ProxyHandler wired to an isolated
// in-memory usage DB and a threshold-1 circuit breaker registry (so a
// single RecordFailure/RecordSuccess call is externally observable via
// breaker.CurrentState()). FlushInterval is deliberately short so the
// background logger periodically persists buffered events to the DB on its
// own — see waitForLoggedStatusCode, which polls the DB directly instead of
// relying on a single explicit ul.Stop() at a moment the test can't
// otherwise pin down (handleStreamingResponse's SendStreamWriter goroutine
// keeps running after Handle itself has already returned — Handle's own
// top-level trackDone defer fires as soon as SendStreamWriter is registered,
// well before the goroutine's body finishes, since the sync.Once inside
// trackDone makes the goroutine's later, matching defer a no-op — so neither
// the client receiving a response nor the request round-trip completing
// proves the goroutine, and therefore its breaker recording and usage
// logging, has actually finished). The returned *usage.Logger is stopped via
// t.Cleanup.
func newTerminationTestHandler(t *testing.T, reg *Registry) (*ProxyHandler, *usage.Logger, *db.DB) {
	t.Helper()

	d := openStage0cDB(t)
	usageCfg := config.UsageConfig{BufferSize: 64, FlushInterval: 20 * time.Millisecond}
	ul := usage.NewLogger(d, usageCfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ul.Start()
	t.Cleanup(ul.Stop)

	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.UsageLogger = ul
	h.CircuitBreakers = circuitbreaker.NewRegistry(circuitbreaker.Config{
		Enabled:     true,
		Threshold:   1,
		Timeout:     30 * time.Second,
		HalfOpenMax: 1,
	})

	return h, ul, d
}

// waitForLoggedStatusCode polls the usage_events table until a row appears
// (each test uses its own freshly opened, empty database — see
// openStage0cDB — so "a row appears" unambiguously means the request under
// test was logged) and returns its status_code. Polling the DB, rather than
// the logger's in-memory buffer, sidesteps a race inherent to the latter:
// the background flush goroutine started by ul.Start() competes with the
// test to drain the buffered-channel the instant an event is enqueued, so
// len(channel) can read back 0 immediately after Log() even though the
// event was genuinely recorded (see the "Do NOT call ul.Start()" comment in
// pii_review3_test.go for the same race observed from the opposite angle).
func waitForLoggedStatusCode(t *testing.T, d *db.DB) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := d.SQL().QueryRowContext(context.Background(),
			"SELECT COUNT(*) FROM usage_events").Scan(&count); err != nil {
			t.Fatalf("count usage_events: %v", err)
		}
		if count > 0 {
			var statusCode int
			row := d.SQL().QueryRowContext(context.Background(),
				"SELECT status_code FROM usage_events ORDER BY rowid DESC LIMIT 1")
			if err := row.Scan(&statusCode); err != nil {
				t.Fatalf("query usage_events: %v", err)
			}
			return statusCode
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("usage event was not logged within deadline")
	return 0
}

// countSegmentsContaining splits body on the SSE event separator "\n\n" and
// counts how many non-empty segments contain needle. Splitting on "\n\n"
// (rather than a raw substring count) avoids double-counting a payload whose
// JSON body happens to mention needle twice in unrelated fields, and mirrors
// how a real SSE client would delimit events.
func countSegmentsContaining(body, needle string) int {
	count := 0
	for _, seg := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(seg) == "" {
			continue
		}
		if strings.Contains(seg, needle) {
			count++
		}
	}
	return count
}

// assertWellFormedSSEFraming splits body on the SSE event separator "\n\n"
// and requires every non-empty segment to consist entirely of complete
// lines: each "data: " line's payload must either be the literal [DONE]
// sentinel or parse as valid JSON. A write failure mid-line (partial JSON
// left dangling across a segment boundary) would fail this check.
func assertWellFormedSSEFraming(t *testing.T, body string) {
	t.Helper()
	for _, seg := range strings.Split(body, "\n\n") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		for _, line := range strings.Split(seg, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(line, "data: ")
			if payload == "[DONE]" {
				continue
			}
			var v any
			if err := json.Unmarshal([]byte(payload), &v); err != nil {
				t.Errorf("malformed SSE framing: line %q in segment %q does not parse as JSON: %v", line, seg, err)
			}
		}
	}
}

// terminationRegistry builds a single-model Registry using the "vllm"
// provider, for which GetAdapter returns nil — exercising the plain
// (non-adapter) passthrough branch of the scan loop.
func terminationRegistry(t *testing.T, name, upstreamURL string) *Registry {
	t.Helper()
	reg, err := NewRegistry([]config.ModelConfig{
		{
			Name:     name,
			Provider: "vllm",
			BaseURL:  upstreamURL,
			APIKey:   "upstream-secret",
		},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// doStreamRequest issues a streaming chat-completion request against baseURL
// using rawKey for auth and returns the full response body. It fails the
// test on any transport-level error or non-200 status.
func doStreamRequest(t *testing.T, baseURL, rawKey, model string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":true}`, model)))
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
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	fullBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read streamed body: %v", err)
	}
	return string(fullBody)
}

// streamRequestThenAbruptlyDisconnect dials baseURL over a raw TCP
// connection, sends a streaming chat-completion request by hand, reads the
// response headers and the first SSE chunk, then forces the connection
// closed with SO_LINGER set to 0.
//
// A plain net/http.Client (even with its request context cancelled) closes
// the connection with a graceful FIN, which a peer on the loopback interface
// can often still write a few more bytes to before the OS actually tears the
// socket down — making "the server's write fails" nondeterministic and easy
// to flake. SO_LINGER=0 instead makes the OS discard the connection state
// immediately and answer the server's next write with an abrupt RST
// (ECONNRESET), so the disconnect the test is simulating is unambiguous and
// prompt.
func streamRequestThenAbruptlyDisconnect(t *testing.T, baseURL, rawKey, model string) {
	t.Helper()

	u, err := url.Parse(baseURL)
	if err != nil {
		t.Fatalf("parse base URL: %v", err)
	}

	conn, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	reqBody := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hi"}],"stream":true}`, model)
	rawReq := fmt.Sprintf(
		"POST /v1/chat/completions HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		u.Host, rawKey, len(reqBody), reqBody,
	)
	if _, err := conn.Write([]byte(rawReq)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Read the first SSE chunk so the server has genuinely started streaming
	// (and TrackStart/the scan loop are underway) before we cut the connection.
	buf := make([]byte, 4096)
	if _, err := reader.Read(buf); err != nil && err != io.EOF {
		t.Fatalf("read first chunk: %v", err)
	}

	if tcpConn, ok := conn.(*net.TCPConn); ok {
		if err := tcpConn.SetLinger(0); err != nil {
			t.Fatalf("SetLinger(0): %v", err)
		}
	}
	if err := conn.Close(); err != nil {
		t.Fatalf("close connection: %v", err)
	}
}

// terminationKeyInfo is the fixed auth.KeyInfo used by every test in this
// file so usage events are attributable and non-nil-keyInfo logUsageEvent
// actually fires.
func terminationKeyInfo(orgID string) auth.KeyInfo {
	return auth.KeyInfo{
		ID:      "key-" + orgID,
		KeyType: "user_key",
		OrgID:   orgID,
	}
}

// ── row: clean EOF after [DONE] — success, no abort, no downgrade ─────────

func TestStreamTermination_RawPassthrough_PreservesCompleteEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		event string
	}{
		{
			name:  "multiple data fields",
			event: "data: {\"a\":\ndata: 1}\n\n",
		},
		{
			name:  "event metadata and comment",
			event: ": upstream comment\nevent: completion\nid: response-7\nretry: 2500\ndata: {\"choices\":[]}\n\n",
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			wire := tc.event + "data: [DONE]\n\n"
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				if _, err := fmt.Fprint(w, wire); err != nil {
					t.Errorf("write upstream stream: %v", err)
					return
				}
				w.(http.Flusher).Flush()
			}))
			t.Cleanup(upstream.Close)

			model := fmt.Sprintf("raw-event-%d", i)
			reg := terminationRegistry(t, model, upstream.URL)
			h, _, d := newTerminationTestHandler(t, reg)

			baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-"+model))
			body := doStreamRequest(t, baseURL, rawKey, model)

			if body != wire {
				t.Errorf("raw stream changed in transit\n got: %q\nwant: %q", body, wire)
			}
			if status := waitForLoggedStatusCode(t, d); status != http.StatusOK {
				t.Errorf("usage status_code = %d, want 200", status)
			}
		})
	}
}

func TestStreamTermination_CleanEOFAfterDone_IsSuccess(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"a"}}]}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "data: [DONE]")
		fmt.Fprintln(w)
		flusher.Flush()
		// Clean return: net/http sends the terminating chunk and closes
		// normally — a genuine clean EOF right after [DONE].
	}))
	t.Cleanup(upstream.Close)

	reg := terminationRegistry(t, "eof-after-done", upstream.URL)
	h, _, d := newTerminationTestHandler(t, reg)

	baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-eof-after-done"))
	body := doStreamRequest(t, baseURL, rawKey, "eof-after-done")

	if !strings.Contains(body, "[DONE]") {
		t.Errorf("[DONE] missing from output\noutput: %s", body)
	}
	if n := countSegmentsContaining(body, `"error"`); n != 0 {
		t.Errorf("unexpected abort event(s): %d\noutput: %s", n, body)
	}
	// The terminal is acted upon only once its blank event separator has
	// reached the client.
	if !strings.Contains(body, "data: [DONE]\n\n") {
		t.Errorf("wire output missing blank-terminated \"data: [DONE]\\n\\n\"\noutput: %q", body)
	}

	if status := waitForLoggedStatusCode(t, d); status != http.StatusOK {
		t.Errorf("usage status_code = %d, want 200", status)
	}

	breaker := h.CircuitBreakers.Get("eof-after-done")
	if state := breaker.CurrentState(); state != circuitbreaker.Closed {
		t.Errorf("breaker state = %v, want Closed (RecordSuccess expected)", state)
	}
}

// ── row: clean EOF WITHOUT [DONE] — incomplete, exactly one abort event ───

// TestStreamTermination_CleanEOFWithoutDone_IsIncomplete is one of the two
// bug-fix regression tests. Confirmed to FAIL against the pre-fix code: the
// old plain scan loop only ever inspected scanner.Err() after the loop, so a
// clean EOF (scanErr == nil) that never sent [DONE] was indistinguishable
// from a normal completion — no abort event was emitted and the usage event
// was logged with the upstream's 200 instead of 502.
func TestStreamTermination_CleanEOFWithoutDone_IsIncomplete(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"a"}}]}`)
		fmt.Fprintln(w)
		flusher.Flush()
		// Handler returns without ever sending [DONE] — net/http still
		// closes the chunked body cleanly (scanner.Err() == nil client-side).
	}))
	t.Cleanup(upstream.Close)

	reg := terminationRegistry(t, "eof-no-done", upstream.URL)
	h, _, d := newTerminationTestHandler(t, reg)

	baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-eof-no-done"))
	body := doStreamRequest(t, baseURL, rawKey, "eof-no-done")

	if strings.Contains(body, "[DONE]") {
		t.Errorf("[DONE] present, want absent (truncated stream)\noutput: %s", body)
	}
	if n := countSegmentsContaining(body, "stream_incomplete"); n != 1 {
		t.Errorf("stream_incomplete abort segments = %d, want exactly 1\noutput: %s", n, body)
	}
	assertWellFormedSSEFraming(t, body)

	if status := waitForLoggedStatusCode(t, d); status != http.StatusBadGateway {
		t.Errorf("usage status_code = %d, want %d (incomplete stream)", status, http.StatusBadGateway)
	}

	breaker := h.CircuitBreakers.Get("eof-no-done")
	if state := breaker.CurrentState(); state != circuitbreaker.Open {
		t.Errorf("breaker state = %v, want Open (RecordFailure expected)", state)
	}
}

// ── row: scanner error BEFORE [DONE] — incomplete, well-formed framing ────

// TestStreamTermination_ScannerErrorBeforeDone_IsIncompleteAndWellFormed
// drives a genuine transport-level scanner error (an abrupt hijacked-connection
// close) before any [DONE] is ever sent, and asserts the abort event lands on
// a clean SSE event boundary — never merged with, or fragmenting, a preceding
// event.
func TestStreamTermination_ScannerErrorBeforeDone_IsIncompleteAndWellFormed(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream: no Flusher")
			return
		}
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"a"}}]}`)
		fmt.Fprintln(w)
		flusher.Flush()

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream: no Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		// Abrupt close (no proper chunked terminator) — the proxy's scanner
		// observes a genuine read error, not a clean EOF.
		conn.Close()
	}))
	t.Cleanup(upstream.Close)

	reg := terminationRegistry(t, "scanerr-before-done", upstream.URL)
	h, _, d := newTerminationTestHandler(t, reg)

	baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-scanerr-before-done"))
	body := doStreamRequest(t, baseURL, rawKey, "scanerr-before-done")

	if strings.Contains(body, "[DONE]") {
		t.Errorf("[DONE] present, want absent\noutput: %s", body)
	}
	if n := countSegmentsContaining(body, "stream_incomplete"); n != 1 {
		t.Errorf("stream_incomplete abort segments = %d, want exactly 1\noutput: %s", n, body)
	}
	if !strings.Contains(body, `"content":"a"`) {
		t.Fatalf("sanity check failed: initial chunk never observed\noutput: %s", body)
	}
	assertWellFormedSSEFraming(t, body)

	if status := waitForLoggedStatusCode(t, d); status != http.StatusBadGateway {
		t.Errorf("usage status_code = %d, want %d", status, http.StatusBadGateway)
	}
}

// ── row: scanner error right after a non-blank line, no trailing blank ────

// TestStreamTermination_RawPassthrough_AbortAfterNonBlankLine_IsSeparateEvent
// guards the raw passthrough branch's own SSE framing specifically: unlike
// TestStreamTermination_ScannerErrorBeforeDone_IsIncompleteAndWellFormed, the
// upstream here never sends a trailing blank line after its last content
// chunk before the abrupt close. The old raw branch wrote only "line + \n"
// and relied on that upstream blank to close the event — without it, the
// injected abort event landed in the SAME still-open SSE event as the
// preceding content chunk ("data: <chunk>\ndata: <abort>\n\n"), which a real
// SSE client merges into a single malformed field. The fixed branch closes
// the open event when scanning ends, so the content chunk and the abort event
// land in two separate, individually parseable "\n\n"-delimited segments.
func TestStreamTermination_RawPassthrough_AbortAfterNonBlankLine_IsSeparateEvent(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream: no Flusher")
			return
		}
		// Deliberately no trailing blank line after the content chunk.
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"a"}}]}`+"\n")
		flusher.Flush()

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream: no Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		// Abrupt close right after the non-blank data line — no trailing
		// blank ever arrives, so the proxy's own framing must close the
		// event, not the upstream's.
		conn.Close()
	}))
	t.Cleanup(upstream.Close)

	reg := terminationRegistry(t, "abort-after-nonblank", upstream.URL)
	h, _, d := newTerminationTestHandler(t, reg)

	baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-abort-after-nonblank"))
	body := doStreamRequest(t, baseURL, rawKey, "abort-after-nonblank")

	var segments []string
	for _, seg := range strings.Split(body, "\n\n") {
		seg = strings.TrimSpace(seg)
		if seg != "" {
			segments = append(segments, seg)
		}
	}
	if len(segments) != 2 {
		t.Fatalf("segments = %d, want exactly 2 (content chunk and abort event as separate \"\\n\\n\"-delimited segments)\noutput: %q", len(segments), body)
	}
	if !strings.Contains(segments[0], `"content":"a"`) || strings.Contains(segments[0], "stream_incomplete") {
		t.Errorf("first segment = %q, want only the content chunk", segments[0])
	}
	if !strings.Contains(segments[1], "stream_incomplete") || strings.Contains(segments[1], `"content":"a"`) {
		t.Errorf("second segment = %q, want only the abort event", segments[1])
	}
	assertWellFormedSSEFraming(t, body)

	if status := waitForLoggedStatusCode(t, d); status != http.StatusBadGateway {
		t.Errorf("usage status_code = %d, want %d", status, http.StatusBadGateway)
	}
}

// ── row: scanner error AFTER [DONE] — success, NO abort, NO downgrade ─────

// TestStreamTermination_ScannerErrorAfterDone_IsSuccess is the second
// bug-fix regression test. Confirmed to FAIL against the pre-fix code: the
// old plain scan loop had no terminalSeen tracking at all, so ANY non-nil
// scanner.Err() — even one observed strictly after [DONE] had already been
// delivered to the client — unconditionally set streamIncomplete, emitted a
// spurious "stream_incomplete" abort event alongside the already-sent
// [DONE], and recorded a circuit-breaker failure for what was, from the
// client's perspective, a completed response.
func TestStreamTermination_ScannerErrorAfterDone_IsSuccess(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream: no Flusher")
			return
		}
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"a"}}]}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, "data: [DONE]")
		fmt.Fprintln(w)
		flusher.Flush()

		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("upstream: no Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		// Abrupt close immediately AFTER [DONE] was already flushed to the
		// client — the proxy's scanner still observes a genuine read error
		// on its NEXT Scan() call, but it happens strictly after the
		// terminal was written.
		conn.Close()
	}))
	t.Cleanup(upstream.Close)

	reg := terminationRegistry(t, "scanerr-after-done", upstream.URL)
	h, _, d := newTerminationTestHandler(t, reg)

	baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-scanerr-after-done"))
	body := doStreamRequest(t, baseURL, rawKey, "scanerr-after-done")

	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("[DONE] missing from output\noutput: %s", body)
	}
	if n := countSegmentsContaining(body, "stream_incomplete"); n != 0 {
		t.Errorf("stream_incomplete abort segments = %d, want 0 (stream already completed)\noutput: %s", n, body)
	}
	if n := countSegmentsContaining(body, `"error"`); n != 0 {
		t.Errorf("unexpected abort event(s) after a completed stream: %d\noutput: %s", n, body)
	}

	if status := waitForLoggedStatusCode(t, d); status != http.StatusOK {
		t.Errorf("usage status_code = %d, want 200 (no downgrade after [DONE])", status)
	}

	breaker := h.CircuitBreakers.Get("scanerr-after-done")
	if state := breaker.CurrentState(); state != circuitbreaker.Closed {
		t.Errorf("breaker state = %v, want Closed (RecordSuccess expected, not RecordFailure)", state)
	}
}

// ── row: adapter transform error — exactly one abort event ────────────────

// TestStreamTermination_AdapterAbort_ExactlyOneEvent is a lightweight,
// self-contained repetition of TestHandle_Plain_AdapterAbort_FailClosed
// (stream_abort_test.go, H-9) so this file's matrix is complete on its own,
// with the addition of a usage-status assertion.
func TestStreamTermination_AdapterAbort_ExactlyOneEvent(t *testing.T) {
	t.Parallel()

	upstream := anthropicAbortUpstream(t)
	reg := abortRegistryPlain(t, upstream.URL)
	h, _, d := newTerminationTestHandler(t, reg)

	baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-adapter-abort"))
	body := doStreamRequest(t, baseURL, rawKey, "abort-plain")

	if strings.Contains(body, "[DONE]") {
		t.Errorf("[DONE] present, want absent\noutput: %s", body)
	}
	if n := countSegmentsContaining(body, "stream_transform_aborted"); n != 1 {
		t.Errorf("stream_transform_aborted segments = %d, want exactly 1\noutput: %s", n, body)
	}

	if status := waitForLoggedStatusCode(t, d); status != http.StatusBadGateway {
		t.Errorf("usage status_code = %d, want %d", status, http.StatusBadGateway)
	}

	breaker := h.CircuitBreakers.Get("abort-plain")
	if state := breaker.CurrentState(); state != circuitbreaker.Open {
		t.Errorf("breaker state = %v, want Open", state)
	}
}

// ── row: write/flush failure, no scanner error pending — disconnect ───────

// TestStreamTermination_ClientDisconnect_NoScannerError_IsDisconnect drives a
// client that cancels mid-stream while the upstream is still happily
// producing chunks (no upstream failure ever occurs). The write failure
// alone must classify the outcome as a disconnect: usage keeps the
// upstream's 200 (streamIncomplete stays false) and the breaker records
// neither success nor failure.
func TestStreamTermination_ClientDisconnect_NoScannerError_IsDisconnect(t *testing.T) {
	t.Parallel()

	upstreamCanceled := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream: no Flusher")
			return
		}
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"a"}}]}`)
		fmt.Fprintln(w)
		flusher.Flush()

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

	reg := terminationRegistry(t, "disconnect-clean", upstream.URL)
	h, _, d := newTerminationTestHandler(t, reg)

	baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-disconnect-clean"))
	streamRequestThenAbruptlyDisconnect(t, baseURL, rawKey, "disconnect-clean")

	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never observed request cancellation")
	}

	if status := waitForLoggedStatusCode(t, d); status != http.StatusOK {
		t.Errorf("usage status_code = %d, want 200 (disconnect keeps the upstream status, not 502)", status)
	}

	breaker := h.CircuitBreakers.Get("disconnect-clean")
	if state := breaker.CurrentState(); state != circuitbreaker.Closed {
		t.Errorf("breaker state = %v, want Closed (neither RecordSuccess nor RecordFailure on disconnect)", state)
	}
}
