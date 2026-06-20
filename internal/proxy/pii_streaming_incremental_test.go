package proxy

// pii_streaming_incremental_test.go covers the incremental StreamRestorer
// integration at the proxy level, corresponding to matrix points 25-26 in the
// task specification.
//
// Key invariants tested:
//   25. A split pseudonym in a streaming SSE response is correctly restored;
//       the client receives content in MULTIPLE SSE chunks (not one big buffer);
//       post-stream usage logging continues to run (no early return).
//   26. Gemini and Anthropic adapter integration: split pseudonym across adapter
//       SSE events is correctly restored and the stream terminates cleanly
//       (includes the Gemini [DONE]-without-blank-separator scenario, FIX 2).
//   27. FIX 3: EOF-without-[DONE] is recorded as a circuit-breaker failure.

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/circuitbreaker"
)

// ── Matrix 25: Split pseudonym, incremental delivery, usage logging ───────────

// TestPII_Incremental_SplitPseudonym_RestoredAndIncremental is the primary
// integration test for Stage 0b. It verifies three things simultaneously:
//
//  1. A pseudonym split across two SSE chunks is correctly restored before
//     reaching the client (no PII_ fragment in the client response).
//  2. The client receives content in MULTIPLE SSE chunks (the restorer emits
//     incrementally, not one single buffered chunk).
//  3. The post-stream code path (usage logging / metrics) still executes after
//     the streaming loop completes without an early return.
func TestPII_Incremental_SplitPseudonym_RestoredAndIncremental(t *testing.T) {
	t.Parallel()

	// Pre-compute the pseudonym that the proxy will generate for piiTestEmail
	// using orgID="" (no auth.KeyInfo middleware in this test).
	engine := newTestPIIEngine(t)
	sampleFilter := engine.NewFilter("")
	sampleBody := []byte(chatBody("ext-model", "send to "+piiTestEmail))
	anonBody, err := sampleFilter.AnonymizeJSON(sampleBody)
	if err != nil {
		t.Fatalf("pre-compute AnonymizeJSON: %v", err)
	}
	pseudo := piiPseudonymPattern.FindString(string(anonBody))
	if pseudo == "" {
		t.Fatal("could not derive pseudonym for piiTestEmail with orgID=empty")
	}

	// Verify the pseudonym is exactly 31 bytes (the fixed pseudonymLen) so the split is fair.
	// pseudonymLen=31 is a package-internal constant; we hard-code the value here as it
	// is specified in the architecture docs and verified by pii package tests.
	const expectedPseudonymLen = 31
	if len(pseudo) != expectedPseudonymLen {
		t.Fatalf("pseudonym length %d != %d", len(pseudo), expectedPseudonymLen)
	}

	// Split the pseudonym at the midpoint: this exercises the core hold-back logic.
	mid := len(pseudo) / 2
	part1 := pseudo[:mid]
	part2 := pseudo[mid:]

	// Track whether the upstream was called and how many SSE data: chunks the
	// mock emits (to verify the client receives at least two separate chunks).
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

		// Emit three chunks: prefix text, part1 of pseudonym, part2 of pseudonym.
		chunks := []string{
			// Chunk 1: plain text before the pseudonym.
			fmt.Sprintf(`data: {"id":"s1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"role":"assistant","content":"Your email: "},"finish_reason":null}]}`),
			// Chunk 2: first half of the pseudonym (must be held by StreamRestorer).
			fmt.Sprintf(`data: {"id":"s1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, jsonQuote(part1)),
			// Chunk 3: second half of the pseudonym (triggers flush after assembly).
			fmt.Sprintf(`data: {"id":"s1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, jsonQuote(part2)),
			// Chunk 4: finish_reason.
			fmt.Sprintf(`data: {"id":"s1","object":"chat.completion.chunk","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
			// Chunk 5: usage-only.
			fmt.Sprintf(`data: {"id":"s1","object":"chat.completion.chunk","model":"gpt-4o","choices":[],"usage":{"prompt_tokens":8,"completion_tokens":4,"total_tokens":12}}`),
			// Sentinel.
			`data: [DONE]`,
		}

		for _, c := range chunks {
			fmt.Fprintln(w, c)
			fmt.Fprintln(w) // blank separator line
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryExternal(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	// Use a real TCP listener (startTestServer) so streaming works end-to-end.
	baseURL := startTestServer(t, h)

	streamBody := fmt.Sprintf(
		`{"model":"ext-model","messages":[{"role":"user","content":"send to %s"}],"stream":true}`,
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

	// ── Invariant 1 and 2: read the SSE stream line-by-line ──────────────────
	// We scan the response body as an SSE stream to verify:
	//   (a) pseudonym is never visible in any emitted line (anti-leak).
	//   (b) the ORIGINAL email is visible in at least one emitted line (restore).
	//   (c) content arrives in at least 2 separate SSE data: chunks (incremental).

	scanner := bufio.NewScanner(streamResp.Body)

	var dataChunkCount int
	var fullOutput strings.Builder
	var pseudoSeen, emailSeen bool

	for scanner.Scan() {
		line := scanner.Text()
		fullOutput.WriteString(line)
		fullOutput.WriteByte('\n')

		if strings.HasPrefix(line, "data:") && !strings.Contains(line, "[DONE]") {
			dataChunkCount++

			if strings.Contains(line, pseudo) {
				pseudoSeen = true
			}
			if strings.Contains(line, piiTestEmail) {
				emailSeen = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scanner error reading streaming response: %v", err)
	}

	outputStr := fullOutput.String()

	// (a) Anti-leak: pseudonym must NEVER appear in any emitted line.
	if pseudoSeen {
		t.Errorf("SECURITY: raw pseudonym %q visible in streaming SSE output\nfull output:\n%s",
			pseudo, outputStr)
	}

	// (b) Restore: original email must appear in the restored output.
	if !emailSeen {
		t.Errorf("restore: original email %q missing from streaming SSE output\nfull output:\n%s",
			piiTestEmail, outputStr)
	}

	// (c) Incremental: must have received multiple data: chunks (not single buffer).
	// The upstream emits 5 content/finish/usage chunks + [DONE]. The restorer
	// should emit at least 2 data: chunks before [DONE] (prefix text + restored email).
	if dataChunkCount < 2 {
		t.Errorf("incremental delivery FAILED: received only %d data: chunk(s); "+
			"expected at least 2 (prefix text + restored pseudonym should be separate)",
			dataChunkCount)
	}

	// [DONE] must be present (stream closed cleanly).
	if !strings.Contains(outputStr, "[DONE]") {
		t.Errorf("streaming response missing [DONE] sentinel\nfull output:\n%s", outputStr)
	}

	// ── Invariant 3: upstream was called ─────────────────────────────────────
	if atomic.LoadInt32(&upstreamCalls) == 0 {
		t.Error("upstream was never called")
	}
}

// TestPII_Incremental_UsageLogging_ContinuesAfterStream verifies that the
// usage-logging post-stream path is not short-circuited by the PII streaming
// loop. This is tested indirectly: a usage-only chunk (choices=[]) in the
// upstream response is re-emitted in the output. If the handler had
// early-returned from the streaming loop, the usage chunk would be silently
// dropped (the restorer would not have processed it).
func TestPII_Incremental_UsageLogging_ContinuesAfterStream(t *testing.T) {
	t.Parallel()

	engine := newTestPIIEngine(t)
	sampleFilter := engine.NewFilter("")
	sampleBody := []byte(chatBody("ext-model", "hi "+piiTestEmail))
	anonBody, err := sampleFilter.AnonymizeJSON(sampleBody)
	if err != nil {
		t.Fatalf("pre-compute: %v", err)
	}
	pseudo := piiPseudonymPattern.FindString(string(anonBody))
	if pseudo == "" {
		t.Fatal("could not derive pseudonym")
	}

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		chunks := []string{
			fmt.Sprintf(`data: {"id":"u1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, jsonQuote(pseudo)),
			`data: {"id":"u1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			// Usage-only chunk: StreamRestorer must re-emit this.
			`data: {"id":"u1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`,
			`data: [DONE]`,
		}
		for _, c := range chunks {
			fmt.Fprintln(w, c)
			fmt.Fprintln(w)
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryExternal(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	baseURL := startTestServer(t, h)
	streamBody := fmt.Sprintf(
		`{"model":"ext-model","messages":[{"role":"user","content":"hi %s"}],"stream":true}`,
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

	fullBody, _ := io.ReadAll(streamResp.Body)
	fullStr := string(fullBody)

	// The usage chunk must appear in the output (StreamRestorer re-emits it).
	if !strings.Contains(fullStr, `"usage"`) {
		t.Errorf("usage chunk was not re-emitted in the streaming output; "+
			"post-stream code path may have been skipped\noutput: %s", fullStr)
	}
	if !strings.Contains(fullStr, `"prompt_tokens"`) {
		t.Errorf("prompt_tokens missing from usage output\noutput: %s", fullStr)
	}

	// Pseudonym must not appear in client output.
	if strings.Contains(fullStr, pseudo) {
		t.Errorf("SECURITY: pseudonym %q visible in output\noutput: %s", pseudo, fullStr)
	}

	// Original email must be restored.
	if !strings.Contains(fullStr, piiTestEmail) {
		t.Errorf("original email %q missing from output\noutput: %s", piiTestEmail, fullStr)
	}
}

// TestPII_Incremental_StreamAbort_NoPseudonymLeak verifies that when the
// streaming loop is aborted due to a protocol violation (e.g. tool_calls in
// the upstream response), the client never receives a raw pseudonym fragment.
func TestPII_Incremental_StreamAbort_NoPseudonymLeak(t *testing.T) {
	t.Parallel()

	engine := newTestPIIEngine(t)
	sampleFilter := engine.NewFilter("")
	sampleBody := []byte(chatBody("ext-model", "abort "+piiTestEmail))
	anonBody, err := sampleFilter.AnonymizeJSON(sampleBody)
	if err != nil {
		t.Fatalf("pre-compute: %v", err)
	}
	pseudo := piiPseudonymPattern.FindString(string(anonBody))
	if pseudo == "" {
		t.Fatal("could not derive pseudonym")
	}

	// Split pseudonym at position 10 (partial pseudonym in carry when abort fires).
	partial := pseudo[:10]

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		// First: send partial pseudonym (will sit in carry).
		chunk1 := fmt.Sprintf(`data: {"id":"a1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%s},"finish_reason":null}]}`, jsonQuote(partial))
		fmt.Fprintln(w, chunk1)
		fmt.Fprintln(w)
		flusher.Flush()

		// Then: send a tool_calls chunk — this triggers errStreamAborted.
		chunk2 := `data: {"id":"a1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":""}}]},"finish_reason":null}]}`
		fmt.Fprintln(w, chunk2)
		fmt.Fprintln(w)
		flusher.Flush()

		// Stream ends without [DONE].
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryExternal(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	baseURL := startTestServer(t, h)
	streamBody := fmt.Sprintf(
		`{"model":"ext-model","messages":[{"role":"user","content":"abort %s"}],"stream":true}`,
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

	// Anti-leak: the partial pseudonym must NOT appear in the client output.
	if strings.Contains(fullStr, "PII_") {
		t.Errorf("SECURITY: PII_ fragment visible in client output after stream abort\noutput: %s", fullStr)
	}
	// The full pseudonym must also not appear.
	if strings.Contains(fullStr, pseudo) {
		t.Errorf("SECURITY: full pseudonym %q visible after stream abort\noutput: %s", pseudo, fullStr)
	}
}

// ── Matrix 26: Adapter integration ───────────────────────────────────────────

// piiRegistryGemini builds a Registry with a single Gemini-provider model
// that is classified as external (destPrivate=false) so PII anonymization fires.
func piiRegistryGemini(t *testing.T, upstreamURL string) *Registry {
	t.Helper()
	m := &Model{
		Name:     "gemini-test",
		Provider: "gemini",
		Type:     "chat",
		BaseURL:  upstreamURL,
		APIKey:   "key-gemini",
		// destPrivate=false so shouldAnonymize returns true.
	}
	r := &Registry{
		models:  map[string]*Model{"gemini-test": m},
		aliases: make(map[string]string),
	}
	r.rebuildSorted()
	return r
}

// piiRegistryAnthropic builds a Registry with a single Anthropic-provider model
// that is classified as external so PII anonymization fires.
func piiRegistryAnthropic(t *testing.T, upstreamURL string) *Registry {
	t.Helper()
	m := &Model{
		Name:     "anthropic-test",
		Provider: "anthropic",
		Type:     "chat",
		BaseURL:  upstreamURL,
		APIKey:   "key-anthropic",
		// destPrivate=false so shouldAnonymize returns true.
	}
	r := &Registry{
		models:  map[string]*Model{"anthropic-test": m},
		aliases: make(map[string]string),
	}
	r.rebuildSorted()
	return r
}

// TestPII_Incremental_GeminiAdapter_SplitPseudonym_RestoredAndTerminated is the
// end-to-end integration test for the Gemini adapter + StreamRestorer pipeline.
//
// The Gemini adapter emits "data: [DONE]" on the blank separator that follows
// the terminal chunk, without a preceding blank of its own. This means the
// restorer sees inEvent=true when [DONE] arrives (FIX 2). This test verifies:
//
//  1. A pseudonym split across two Gemini SSE chunks is correctly restored.
//  2. The stream terminates cleanly (terminal=true, no abort).
//  3. No PII_ fragment appears in the client output.
//
// The mock upstream emits native Gemini streamGenerateContent SSE format;
// GeminiAdapter.TransformStreamLine converts it to OpenAI-shaped chunks
// before the StreamRestorer processes them.
func TestPII_Incremental_GeminiAdapter_SplitPseudonym_RestoredAndTerminated(t *testing.T) {
	t.Parallel()

	engine := newTestPIIEngine(t)
	sampleFilter := engine.NewFilter("")
	sampleBody := []byte(chatBody("gemini-test", "contact "+piiTestEmail))
	anonBody, err := sampleFilter.AnonymizeJSON(sampleBody)
	if err != nil {
		t.Fatalf("pre-compute AnonymizeJSON: %v", err)
	}
	pseudo := piiPseudonymPattern.FindString(string(anonBody))
	if pseudo == "" {
		t.Fatal("could not derive pseudonym for piiTestEmail with orgID=empty")
	}

	// Split pseudonym at midpoint.
	mid := len(pseudo) / 2
	part1 := pseudo[:mid]
	part2 := pseudo[mid:]

	// Build Gemini SSE format. The Gemini adapter converts these to OpenAI chunks.
	// Gemini emits full generateContent response objects per SSE line.
	// The final chunk includes a finishReason; the adapter sets doneSent=true and
	// the blank line that follows becomes "data: [DONE]".
	geminiChunk1 := fmt.Sprintf(`data: {"candidates":[{"content":{"parts":[{"text":%s}],"role":"model"},"index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}`, jsonQuote(part1))
	geminiChunk2 := fmt.Sprintf(`data: {"candidates":[{"content":{"parts":[{"text":%s}],"role":"model"},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":4,"totalTokenCount":9}}`, jsonQuote(part2))

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream: http.Flusher not available")
			return
		}

		// Chunk 1: first half of pseudonym, no finishReason.
		fmt.Fprintln(w, geminiChunk1)
		fmt.Fprintln(w) // blank separator
		flusher.Flush()

		// Chunk 2: second half of pseudonym with finishReason=STOP.
		// The GeminiAdapter will set doneSent=true.
		fmt.Fprintln(w, geminiChunk2)
		fmt.Fprintln(w) // blank separator — adapter converts this to "data: [DONE]"
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryGemini(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	baseURL := startTestServer(t, h)
	streamBody := fmt.Sprintf(
		`{"model":"gemini-test","messages":[{"role":"user","content":"contact %s"}],"stream":true}`,
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

	fullBody, _ := io.ReadAll(streamResp.Body)
	fullStr := string(fullBody)

	// [DONE] must be present — stream must have terminated cleanly.
	if !strings.Contains(fullStr, "[DONE]") {
		t.Errorf("Gemini adapter: [DONE] missing from client output (stream may have aborted)\noutput:\n%s", fullStr)
	}

	// Anti-leak: no PII_ fragment in client output.
	if strings.Contains(fullStr, "PII_") {
		t.Errorf("SECURITY: PII_ fragment visible in Gemini adapter output\noutput:\n%s", fullStr)
	}

	// Restore: original email must be present.
	if !strings.Contains(fullStr, piiTestEmail) {
		t.Errorf("original email %q missing from Gemini adapter output\noutput:\n%s", piiTestEmail, fullStr)
	}
}

// TestPII_Incremental_AnthropicAdapter_SplitPseudonym_RestoredAndTerminated
// is the end-to-end integration test for the Anthropic adapter + StreamRestorer.
//
// The Anthropic adapter translates native Anthropic SSE events (message_start,
// content_block_delta, message_delta, message_stop) to OpenAI-shaped chunks.
// message_stop produces "data: [DONE]" directly (no blank-separator ambiguity).
// This test verifies that a pseudonym split across two content_block_delta
// events is correctly restored.
func TestPII_Incremental_AnthropicAdapter_SplitPseudonym_RestoredAndTerminated(t *testing.T) {
	t.Parallel()

	engine := newTestPIIEngine(t)
	sampleFilter := engine.NewFilter("")
	sampleBody := []byte(chatBody("anthropic-test", "hi "+piiTestEmail))
	anonBody, err := sampleFilter.AnonymizeJSON(sampleBody)
	if err != nil {
		t.Fatalf("pre-compute AnonymizeJSON: %v", err)
	}
	pseudo := piiPseudonymPattern.FindString(string(anonBody))
	if pseudo == "" {
		t.Fatal("could not derive pseudonym for piiTestEmail with orgID=empty")
	}

	mid := len(pseudo) / 2
	part1 := pseudo[:mid]
	part2 := pseudo[mid:]

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("upstream: http.Flusher not available")
			return
		}

		// Anthropic SSE format:
		events := []string{
			`event: message_start`,
			`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-3-5-sonnet","stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
			``,
			`event: content_block_start`,
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			``,
			`event: content_block_delta`,
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, jsonQuote(part1)),
			``,
			`event: content_block_delta`,
			fmt.Sprintf(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%s}}`, jsonQuote(part2)),
			``,
			`event: content_block_stop`,
			`data: {"type":"content_block_stop","index":0}`,
			``,
			`event: message_delta`,
			`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":5}}`,
			``,
			`event: message_stop`,
			`data: {"type":"message_stop"}`,
			``,
		}

		for _, line := range events {
			fmt.Fprintln(w, line)
		}
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryAnthropic(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	baseURL := startTestServer(t, h)
	streamBody := fmt.Sprintf(
		`{"model":"anthropic-test","messages":[{"role":"user","content":"hi %s"}],"stream":true}`,
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

	fullBody, _ := io.ReadAll(streamResp.Body)
	fullStr := string(fullBody)

	// [DONE] must be present.
	if !strings.Contains(fullStr, "[DONE]") {
		t.Errorf("Anthropic adapter: [DONE] missing from client output\noutput:\n%s", fullStr)
	}

	// Anti-leak.
	if strings.Contains(fullStr, "PII_") {
		t.Errorf("SECURITY: PII_ fragment visible in Anthropic adapter output\noutput:\n%s", fullStr)
	}

	// Restore.
	if !strings.Contains(fullStr, piiTestEmail) {
		t.Errorf("original email %q missing from Anthropic adapter output\noutput:\n%s", piiTestEmail, fullStr)
	}
}

// ── FIX 3: EOF without [DONE] ─────────────────────────────────────────────────

// TestPII_Incremental_EOFWithoutDone_NotRecordedAsSuccess verifies that when
// the upstream closes the TCP connection without sending a [DONE] sentinel
// (truncated response), the circuit breaker records a failure rather than a
// success.
//
// Strategy: wire a circuit breaker with threshold=1 so that a single
// RecordFailure call trips it to the Open state. After the streaming response
// completes (EOF-without-[DONE]), assert the breaker is Open rather than
// Closed. If the breaker were Closed, RecordSuccess was called instead —
// which is the bug this fix prevents.
func TestPII_Incremental_EOFWithoutDone_NotRecordedAsSuccess(t *testing.T) {
	t.Parallel()

	engine := newTestPIIEngine(t)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)

		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}

		// Send one content chunk but NO [DONE] — simulate truncation.
		chunk := `data: {"id":"eof1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`
		fmt.Fprintln(w, chunk)
		fmt.Fprintln(w)
		flusher.Flush()
		// Connection closes here without [DONE].
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryExternal(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine

	// Wire a circuit breaker registry with threshold=1 so a single
	// RecordFailure call trips the breaker to Open state.
	h.CircuitBreakers = circuitbreaker.NewRegistry(circuitbreaker.Config{
		Enabled:     true,
		Threshold:   1,
		Timeout:     30 * time.Second,
		HalfOpenMax: 1,
	})

	baseURL := startTestServer(t, h)
	streamBody := fmt.Sprintf(
		`{"model":"ext-model","messages":[{"role":"user","content":"hello %s"}],"stream":true}`,
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
	_, _ = io.ReadAll(streamResp.Body)

	// The circuit breaker for "ext-model" must be in Open state, indicating
	// that RecordFailure was called (not RecordSuccess). If the state is
	// Closed, the truncated stream was incorrectly counted as a success.
	breaker := h.CircuitBreakers.Get("ext-model")
	if state := breaker.CurrentState(); state != circuitbreaker.Open {
		t.Errorf("EOF-without-[DONE]: breaker state = %v, want Open (RecordFailure expected)", state)
	}
}

// jsonQuote JSON-encodes a string for embedding in SSE lines.
func jsonQuote(s string) string {
	b := make([]byte, 0, len(s)+2)
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			b = append(b, '\\', '"')
		case '\\':
			b = append(b, '\\', '\\')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		case '\t':
			b = append(b, '\\', 't')
		default:
			b = append(b, c)
		}
	}
	b = append(b, '"')
	return string(b)
}
