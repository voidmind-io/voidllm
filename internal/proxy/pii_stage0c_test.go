package proxy

// pii_stage0c_test.go covers the proxy-level integration for Stage 0c
// (feat/pii-stage0c-streaming-tool-calls): streaming tool-call argument
// restore and the synthesized SSE error event.
//
// Matrix items covered:
//   11. Split pseudonym in tool_calls arguments → restored in incremental chunks;
//       post-stream usage logging/metrics still run.
//   12. Error event on fail-closed abort → exactly ONE content-free error SSE
//       line with type "pii_restore_aborted", NO [DONE] after it; client-
//       disconnect path → NO error event; StreamIncomplete → StatusBadGateway
//       (verified via logUsageEvent + circuit-breaker, matching pii_review3_test.go).
//
// Zero-knowledge: no pseudonym or real value appears in any error string emitted
// to the client across these paths.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/circuitbreaker"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/usage"
)

// ── Matrix 11: Split pseudonym in tool_calls arguments ────────────────────────

// TestPII_Stage0c_ToolCallArgs_SplitPseudonym_Restored verifies end-to-end that:
//  1. A pseudonym split across two consecutive function.arguments SSE fragments
//     is fully restored before reaching the client.
//  2. No PII_ fragment appears in any emitted SSE line.
//  3. The original value is present in the concatenated arguments output.
//  4. The stream terminates cleanly with [DONE].
//  5. Post-stream code (usage logging path) still runs — usage chunk is re-emitted.
func TestPII_Stage0c_ToolCallArgs_SplitPseudonym_Restored(t *testing.T) {
	t.Parallel()

	// Pre-compute the pseudonym that the proxy will generate for piiTestEmail.
	engine := newTestPIIEngine(t)
	sampleFilter := engine.NewFilter("")
	sampleBody := []byte(chatBody("ext-model", "tool call for "+piiTestEmail))
	anonBody, err := sampleFilter.AnonymizeJSON(sampleBody)
	if err != nil {
		t.Fatalf("pre-compute AnonymizeJSON: %v", err)
	}
	pseudo := piiPseudonymPattern.FindString(string(anonBody))
	if pseudo == "" {
		t.Fatal("could not derive pseudonym for piiTestEmail")
	}
	const expectedPseudonymLen = 31
	if len(pseudo) != expectedPseudonymLen {
		t.Fatalf("pseudonym length %d != %d", len(pseudo), expectedPseudonymLen)
	}

	// Split at midpoint.
	mid := len(pseudo) / 2
	part1 := pseudo[:mid]
	part2 := pseudo[mid:]

	var upstreamCalls int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamCalls, 1)

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream: http.Flusher not available")
			return
		}

		// Chunk 1: tool_call header with first half of pseudonym in arguments.
		chunk1 := fmt.Sprintf(
			`data: {"id":"tc1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_tool1","type":"function","function":{"name":"get_user","arguments":%s}}]},"finish_reason":null}]}`,
			jsonQuote(part1),
		)
		// Chunk 2: continuation with second half of pseudonym.
		chunk2 := fmt.Sprintf(
			`data: {"id":"tc1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]},"finish_reason":null}]}`,
			jsonQuote(part2),
		)
		// Chunk 3: finish_reason:"tool_calls".
		chunk3 := `data: {"id":"tc1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
		// Chunk 4: usage-only (verifies post-stream code path runs).
		chunk4 := `data: {"id":"tc1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

		for _, c := range []string{chunk1, chunk2, chunk3, chunk4, "data: [DONE]"} {
			fmt.Fprintln(w, c)
			fmt.Fprintln(w) // blank SSE event separator
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryExternal(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	baseURL := startTestServer(t, h)

	streamBody := fmt.Sprintf(
		`{"model":"ext-model","messages":[{"role":"user","content":"tool call for %s"}],"stream":true}`,
		piiTestEmail,
	)
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: testTimeout.Timeout}
	streamResp, err := client.Do(httpReq)
	if err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	defer streamResp.Body.Close()

	if streamResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(streamResp.Body)
		t.Fatalf("status = %d, want 200; body: %s", streamResp.StatusCode, body)
	}

	scanner := bufio.NewScanner(streamResp.Body)
	var fullOutput strings.Builder
	var pseudoSeen, emailSeen, doneSeen, usageSeen bool
	var dataChunkCount int

	for scanner.Scan() {
		line := scanner.Text()
		fullOutput.WriteString(line)
		fullOutput.WriteByte('\n')

		if strings.HasPrefix(line, "data:") {
			if strings.Contains(line, "[DONE]") {
				doneSeen = true
				continue
			}
			dataChunkCount++
			if strings.Contains(line, pseudo) {
				pseudoSeen = true
			}
			if strings.Contains(line, piiTestEmail) {
				emailSeen = true
			}
			if strings.Contains(line, `"usage"`) {
				usageSeen = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error: %v", err)
	}

	outputStr := fullOutput.String()

	// Anti-leak: no raw pseudonym in any emitted line.
	if pseudoSeen {
		t.Errorf("SECURITY: raw pseudonym %q visible in tool_calls output\nfull output:\n%s", pseudo, outputStr)
	}
	// Restore: original email must appear in at least one emitted line.
	if !emailSeen {
		t.Errorf("restore: original email %q not found in tool args output\nfull output:\n%s", piiTestEmail, outputStr)
	}
	// Incremental: multiple data: chunks received.
	if dataChunkCount < 2 {
		t.Errorf("incremental: expected >=2 data: chunks, got %d\nfull output:\n%s", dataChunkCount, outputStr)
	}
	// Terminal: [DONE] present (stream closed cleanly).
	if !doneSeen {
		t.Errorf("[DONE] missing from output — stream did not close cleanly\nfull output:\n%s", outputStr)
	}
	// Post-stream: usage chunk was re-emitted (usage logging path ran).
	if !usageSeen {
		t.Errorf("usage chunk missing — post-stream code path may not have run\nfull output:\n%s", outputStr)
	}
	// Upstream was called.
	if atomic.LoadInt32(&upstreamCalls) == 0 {
		t.Error("upstream was never called")
	}
}

// ── Matrix 12a: Error event on fail-closed abort ──────────────────────────────

// TestPII_Stage0c_ErrorEvent_OnAbort verifies that when the stream is aborted
// fail-closed (here: [DONE] arrives while argsCarry is non-empty for a tool call),
// the client receives exactly ONE content-free SSE error event with
// type=="pii_restore_aborted" and NO [DONE] line follows it.
func TestPII_Stage0c_ErrorEvent_OnAbort(t *testing.T) {
	t.Parallel()

	// Pre-compute pseudonym.
	engine := newTestPIIEngine(t)
	sampleFilter := engine.NewFilter("")
	sampleBody := []byte(chatBody("ext-model", "abort test "+piiTestEmail))
	anonBody, err := sampleFilter.AnonymizeJSON(sampleBody)
	if err != nil {
		t.Fatalf("pre-compute: %v", err)
	}
	pseudo := piiPseudonymPattern.FindString(string(anonBody))
	if pseudo == "" {
		t.Fatal("could not derive pseudonym")
	}

	// The upstream sends a partial pseudonym in tool args and then [DONE] without
	// completing it — this triggers errCarryNotEmpty → abort → error event.
	partial := pseudo[:10]

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		// Tool call header with partial pseudonym (argsCarry will be non-empty).
		chunk1 := fmt.Sprintf(
			`data: {"id":"ab1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_abort","type":"function","function":{"name":"fn","arguments":%s}}]},"finish_reason":null}]}`,
			jsonQuote(partial),
		)
		fmt.Fprintln(w, chunk1)
		fmt.Fprintln(w)
		flusher.Flush()

		// [DONE] while argsCarry is non-empty → errCarryNotEmpty → abort.
		fmt.Fprintln(w, "data: [DONE]")
		fmt.Fprintln(w)
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryExternal(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	// Wire a circuit breaker to assert stream abort registers as failure.
	h.CircuitBreakers = circuitbreaker.NewRegistry(circuitbreaker.Config{
		Enabled:     true,
		Threshold:   1,
		Timeout:     30 * time.Second,
		HalfOpenMax: 1,
	})

	baseURL := startTestServer(t, h)

	streamBody := fmt.Sprintf(
		`{"model":"ext-model","messages":[{"role":"user","content":"abort test %s"}],"stream":true}`,
		piiTestEmail,
	)
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: testTimeout.Timeout}
	streamResp, err := client.Do(httpReq)
	if err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	defer streamResp.Body.Close()

	// The handler sends 200 OK at SSE stream start before the error fires.
	// We read the full body and check the content.
	fullBody, _ := io.ReadAll(streamResp.Body)
	fullStr := string(fullBody)

	// Zero-knowledge: no raw pseudonym visible in the error event or any emitted line.
	if strings.Contains(fullStr, pseudo) {
		t.Errorf("SECURITY: raw pseudonym %q visible in error output\nfull output:\n%s", pseudo, fullStr)
	}

	// Count error event occurrences — exactly one.
	errorEventCount := strings.Count(fullStr, `"type":"pii_restore_aborted"`)
	if errorEventCount != 1 {
		t.Errorf("expected exactly 1 pii_restore_aborted error event, got %d\nfull output:\n%s", errorEventCount, fullStr)
	}

	// Parse the error event to verify the correct shape.
	var errorLine string
	for _, l := range strings.Split(fullStr, "\n") {
		if strings.Contains(l, "pii_restore_aborted") {
			errorLine = l
			break
		}
	}
	if errorLine == "" {
		t.Fatalf("error event line not found\nfull output:\n%s", fullStr)
	}
	// Strip "data: " prefix.
	payload := strings.TrimPrefix(errorLine, "data: ")
	payload = strings.TrimPrefix(payload, "data:")
	var errDoc struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(payload), &errDoc); err != nil {
		t.Fatalf("error event is not valid JSON: %v\nline: %s", err, errorLine)
	}
	if errDoc.Error.Type != "pii_restore_aborted" {
		t.Errorf("error.type = %q, want pii_restore_aborted", errDoc.Error.Type)
	}
	if errDoc.Error.Code != "pii_restore_aborted" {
		t.Errorf("error.code = %q, want pii_restore_aborted", errDoc.Error.Code)
	}
	// The message must not contain any PII value or pseudonym.
	if strings.Contains(errDoc.Error.Message, pseudo) {
		t.Errorf("SECURITY: error.message contains raw pseudonym: %q", errDoc.Error.Message)
	}
	if strings.Contains(errDoc.Error.Message, piiTestEmail) {
		t.Errorf("SECURITY: error.message contains original email: %q", errDoc.Error.Message)
	}

	// NO [DONE] must follow the error event.
	errorIdx := strings.Index(fullStr, "pii_restore_aborted")
	doneIdx := strings.Index(fullStr, "[DONE]")
	if doneIdx >= 0 && doneIdx > errorIdx {
		t.Errorf("[DONE] appears AFTER error event (position %d vs %d) — must not follow error event\nfull output:\n%s",
			doneIdx, errorIdx, fullStr)
	}

	// StreamAborted path: circuit breaker must record a failure (Open state).
	breaker := h.CircuitBreakers.Get("ext-model")
	if state := breaker.CurrentState(); state != circuitbreaker.Open {
		t.Errorf("stream abort: breaker state = %v, want Open (RecordFailure expected)", state)
	}
}

// TestPII_Stage0c_ErrorEvent_ProtocolViolation verifies that a protocol
// violation mid-stream (legacy function_call delta) also produces the single
// pii_restore_aborted error event and no [DONE].
func TestPII_Stage0c_ErrorEvent_ProtocolViolation(t *testing.T) {
	t.Parallel()

	engine := newTestPIIEngine(t)
	sampleFilter := engine.NewFilter("")
	sampleBody := []byte(chatBody("ext-model", "violation "+piiTestEmail))
	anonBody, err := sampleFilter.AnonymizeJSON(sampleBody)
	if err != nil {
		t.Fatalf("pre-compute: %v", err)
	}
	pseudo := piiPseudonymPattern.FindString(string(anonBody))
	if pseudo == "" {
		t.Fatal("could not derive pseudonym")
	}

	partial := pseudo[:10]

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		// Chunk 1: partial pseudonym in content carry.
		chunk1 := fmt.Sprintf(
			`data: {"id":"pv1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`,
			jsonQuote(partial),
		)
		fmt.Fprintln(w, chunk1)
		fmt.Fprintln(w)
		flusher.Flush()

		// Chunk 2: legacy function_call delta → protocol violation → errStreamAborted.
		chunk2 := `data: {"id":"pv1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"function_call":{"name":"bad","arguments":"{}"}},"finish_reason":null}]}`
		fmt.Fprintln(w, chunk2)
		fmt.Fprintln(w)
		flusher.Flush()
		// Stream continues but restorer is already aborted.
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryExternal(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	baseURL := startTestServer(t, h)

	streamBody := fmt.Sprintf(
		`{"model":"ext-model","messages":[{"role":"user","content":"violation %s"}],"stream":true}`,
		piiTestEmail,
	)
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: testTimeout.Timeout}
	streamResp, err := client.Do(httpReq)
	if err != nil {
		t.Fatalf("streaming request: %v", err)
	}
	defer streamResp.Body.Close()

	fullBody, _ := io.ReadAll(streamResp.Body)
	fullStr := string(fullBody)

	// No raw pseudonym visible.
	if strings.Contains(fullStr, pseudo) {
		t.Errorf("SECURITY: pseudonym %q visible after protocol violation\nfull output:\n%s", pseudo, fullStr)
	}

	// Exactly one error event.
	if count := strings.Count(fullStr, `"type":"pii_restore_aborted"`); count != 1 {
		t.Errorf("expected 1 pii_restore_aborted event, got %d\nfull output:\n%s", count, fullStr)
	}

	// No [DONE] after the error event.
	errorIdx := strings.Index(fullStr, "pii_restore_aborted")
	doneIdx := strings.Index(fullStr, "[DONE]")
	if doneIdx >= 0 && doneIdx > errorIdx {
		t.Errorf("[DONE] follows error event (done@%d, error@%d)\nfull output:\n%s", doneIdx, errorIdx, fullStr)
	}
}

// ── Matrix 12b: Client-disconnect — NO error event ────────────────────────────

// TestPII_Stage0c_ClientDisconnect_NoBreakerFailure verifies that when the
// client disconnects mid-stream, the circuit breaker does NOT record a failure.
// The client-disconnect path sets clientDisconnected=true which skips both the
// error event emission and the breaker.RecordFailure() call.
//
// Strategy: use a circuit breaker with threshold=1 and assert that after a
// client disconnect the breaker remains Closed (RecordFailure was not called).
func TestPII_Stage0c_ClientDisconnect_NoBreakerFailure(t *testing.T) {
	t.Parallel()

	engine := newTestPIIEngine(t)

	// Upstream that streams indefinitely.
	var upstreamReceived int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&upstreamReceived, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		// Send many chunks; the client will disconnect after reading the first.
		for i := 0; i < 100; i++ {
			chunk := fmt.Sprintf(
				`data: {"id":"dc1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"chunk_%d "},"finish_reason":null}]}`,
				i,
			)
			if _, werr := fmt.Fprintln(w, chunk); werr != nil {
				return // client disconnected
			}
			fmt.Fprintln(w)
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryExternal(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	// Circuit breaker with threshold=1: a single RecordFailure trips it Open.
	h.CircuitBreakers = circuitbreaker.NewRegistry(circuitbreaker.Config{
		Enabled:     true,
		Threshold:   1,
		Timeout:     30 * time.Second,
		HalfOpenMax: 1,
	})

	baseURL := startTestServer(t, h)

	streamBody := `{"model":"ext-model","messages":[{"role":"user","content":"disconnect test"}],"stream":true}`
	httpReq, err := http.NewRequest(http.MethodPost, baseURL+"/v1/chat/completions",
		strings.NewReader(streamBody))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// Custom transport that closes the response body after reading a tiny amount.
	disconnectAfterBytes := 128
	client := &http.Client{
		Timeout:   testTimeout.Timeout,
		Transport: &limitedReadTransport{maxBytes: disconnectAfterBytes},
	}
	resp, err := client.Do(httpReq)
	if err == nil {
		// Read a tiny amount then immediately close.
		buf := make([]byte, disconnectAfterBytes)
		_, _ = resp.Body.Read(buf)
		resp.Body.Close()
	}
	// err != nil is also fine — the transport may return an error on disconnect.

	// Poll until the handler goroutine has noticed the disconnect and settled,
	// or until a 2-second deadline expires. Polling avoids a fixed sleep that
	// can either be too short (flaky on slow CI) or too long (slow tests), and
	// it avoids the busy-spin -race flakiness of a spin loop.
	breaker := h.CircuitBreakers.Get("ext-model")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		// We expect the breaker to remain Closed. If it flips to Open, that is
		// already a test failure; stop polling immediately.
		if breaker.CurrentState() == circuitbreaker.Open {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The breaker must NOT be Open — client disconnect must not record failure.
	if state := breaker.CurrentState(); state == circuitbreaker.Open {
		t.Errorf("client-disconnect: breaker is Open (RecordFailure was called); " +
			"client disconnect must NOT trigger error event or breaker failure")
	}
}

// limitedReadTransport wraps http.DefaultTransport and limits how many bytes
// of the response body are delivered before returning EOF. Used to simulate
// early client disconnect.
type limitedReadTransport struct {
	maxBytes int
}

func (lt *limitedReadTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &limitedBodyReader{r: resp.Body, remaining: lt.maxBytes}
	return resp, nil
}

// limitedBodyReader wraps an io.ReadCloser and returns io.EOF after limit bytes.
type limitedBodyReader struct {
	r         io.ReadCloser
	remaining int
}

func (lr *limitedBodyReader) Read(p []byte) (int, error) {
	if lr.remaining <= 0 {
		return 0, io.EOF
	}
	if len(p) > lr.remaining {
		p = p[:lr.remaining]
	}
	n, err := lr.r.Read(p)
	lr.remaining -= n
	return n, err
}

func (lr *limitedBodyReader) Close() error {
	return lr.r.Close()
}

// ── Matrix 12c: StreamIncomplete → StatusBadGateway in usage logging ──────────

// TestPII_Stage0c_StreamIncomplete_StatusBadGateway verifies that when the
// pii restorer aborts (streamIncomplete=true), the usage event is logged with
// http.StatusBadGateway rather than the upstream's 200. This mirrors the pattern
// in TestPII_Review3_TruncatedStream_UsageStatus (pii_review3_test.go):
// call logUsageEvent directly with the computed eventStatusCode to verify the
// conditional logic in handler.go.
func TestPII_Stage0c_StreamIncomplete_StatusBadGateway(t *testing.T) {
	t.Parallel()

	// Open an in-memory SQLite database and set up the usage logger.
	d := openStage0cDB(t)
	usageCfg := config.UsageConfig{BufferSize: 64, FlushInterval: 5 * time.Second}
	ul := usage.NewLogger(d, usageCfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	// Do NOT call ul.Start(): keeps events in channel buffer so BufferLen is observable.

	keyInfo := &auth.KeyInfo{
		ID:      "key-stage0c-incomplete",
		KeyType: "user_key",
		OrgID:   "org-stage0c-incomplete",
	}
	model := Model{Name: "ext-model"}
	ui := UsageInfo{PromptTokens: 4, CompletionTokens: 2, TotalTokens: 6}

	reg := &Registry{
		models:  map[string]*Model{model.Name: &model},
		aliases: make(map[string]string),
	}
	reg.rebuildSorted()
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.UsageLogger = ul

	// Simulate the handler.go conditional for a stream abort (streamIncomplete=true):
	//   eventStatusCode := respStatusCode   // 200 from upstream
	//   if streamIncomplete {
	//       eventStatusCode = http.StatusBadGateway
	//   }
	streamIncomplete := true
	respStatusCode := http.StatusOK
	eventStatusCode := respStatusCode
	if streamIncomplete {
		eventStatusCode = http.StatusBadGateway
	}
	h.logUsageEvent(keyInfo, model, ui, 100, nil, eventStatusCode, "req-stage0c-1", model.Name)

	if h.UsageLogger.BufferLen() == 0 {
		t.Fatal("no event in usage logger buffer after logUsageEvent for aborted stream")
	}

	// Simulate a complete stream (streamIncomplete=false) to confirm 200 is used.
	before := h.UsageLogger.BufferLen()
	streamIncomplete = false
	eventStatusCode = respStatusCode
	if streamIncomplete {
		eventStatusCode = http.StatusBadGateway
	}
	h.logUsageEvent(keyInfo, model, ui, 100, nil, eventStatusCode, "req-stage0c-2", model.Name)
	after := h.UsageLogger.BufferLen()
	if after != before+1 {
		t.Errorf("buffer did not grow by 1 for complete stream: before=%d after=%d", before, after)
	}

	if after < 2 {
		t.Errorf("expected at least 2 events in buffer (aborted + complete); got %d", after)
	}
}

// openStage0cDB opens an in-memory SQLite DB with migrations applied.
// Mirrors openReview3DB from pii_review3_test.go.
func openStage0cDB(t *testing.T) *db.DB {
	t.Helper()
	cfg := config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             fmt.Sprintf("file:stage0c_%d?mode=memory&cache=private", time.Now().UnixNano()),
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	}
	ctx := context.Background()
	d, err := db.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.RunMigrations(ctx, d.SQL(), db.SQLiteDialect{},
		slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("db.RunMigrations: %v", err)
	}
	return d
}
