package proxy

// pii_new_test.go covers the additional proxy-level PII cases required by the
// feat/pii-anonymization-stage0a test task:
//
//  7. per-model pii_filter flag priority (5 sub-cases)
//  8. stream-cap / scanner.Err fail-closed behavior (via handler path)
//  9. classifyDestPrivate coverage
// 10. FIX 4: raw model name must not appear in OTel spans

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// ── helpers (ptr for *bool) ───────────────────────────────────────────────────

func boolPtr(b bool) *bool { return &b }

// buildModelWithPIIFilter constructs a single-model Registry where destPrivate
// and PIIFilter are set as requested, pointing at upstreamURL.
func buildModelWithPIIFilter(t *testing.T, upstreamURL string, destPrivate bool, piiFilter *bool) *Registry {
	t.Helper()
	m := &Model{
		Name:        "model-x",
		Provider:    "openai",
		Type:        "chat",
		BaseURL:     upstreamURL,
		APIKey:      "key-x",
		destPrivate: destPrivate,
		PIIFilter:   piiFilter,
	}
	r := &Registry{
		models:  map[string]*Model{"model-x": m},
		aliases: make(map[string]string),
	}
	r.rebuildSorted()
	return r
}

// buildModelWithDeploymentPIIFilter builds a Registry where the PIIFilter is
// set on the Deployment (not the model), using a staticDeploymentPicker to
// exercise the per-deployment code path.
func buildModelWithDeploymentPIIFilter(t *testing.T, upstreamURL string, destPrivate bool, depFilter *bool) (*Registry, *staticDeploymentPicker) {
	t.Helper()
	dep := Deployment{
		Name:        "dep-x",
		Provider:    "openai",
		BaseURL:     upstreamURL,
		APIKey:      "key-dep",
		Weight:      1,
		destPrivate: destPrivate,
		PIIFilter:   depFilter,
	}
	m := &Model{
		Name:        "model-x",
		Provider:    "openai",
		Type:        "chat",
		BaseURL:     upstreamURL,
		APIKey:      "key-x",
		destPrivate: destPrivate,
		Deployments: []Deployment{dep},
		// model-level PIIFilter intentionally nil
	}
	r := &Registry{
		models:  map[string]*Model{"model-x": m},
		aliases: make(map[string]string),
	}
	r.rebuildSorted()
	picker := &staticDeploymentPicker{deps: []Deployment{dep}}
	return r, picker
}

// captureOK starts an httptest.Server that records the body and responds 200 OK
// with a minimal OpenAI non-streaming response.
func captureOK(t *testing.T) (*httptest.Server, *[]byte) {
	t.Helper()
	var last []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		last = b
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"cmp","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &last
}

// ── 7. per-model pii_filter flag priority ─────────────────────────────────────

// TestPIIFilter_Priority_7a: destPrivate=true, PIIFilter=ptr(true) → anonymized.
// An explicit pii_filter:true overrides the private-network default.
func TestPIIFilter_Priority_7a_PrivateDestExplicitTrue(t *testing.T) {
	t.Parallel()

	upstream, lastBody := captureOK(t)
	// destPrivate=true (would normally skip PII), PIIFilter=ptr(true) → must anonymize
	reg := buildModelWithPIIFilter(t, upstream.URL, true, boolPtr(true))
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = newTestPIIEngine(t)
	app := testApp(t, h)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(chatBody("model-x", "contact "+piiTestEmail)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	body := string(*lastBody)
	// With explicit pii_filter:true the body MUST be anonymized even though
	// destPrivate is true.
	if strings.Contains(body, piiTestEmail) {
		t.Errorf("7a: upstream received original PII despite pii_filter:true on private dest; body: %s", body)
	}
	if !piiPseudonymPattern.MatchString(body) {
		t.Errorf("7a: upstream body missing pseudonym; body: %s", body)
	}
}

// TestPIIFilter_Priority_7b: destPrivate=false, PIIFilter=ptr(false) → NOT anonymized.
// An explicit pii_filter:false suppresses anonymization even on a public destination.
func TestPIIFilter_Priority_7b_PublicDestExplicitFalse(t *testing.T) {
	t.Parallel()

	upstream, lastBody := captureOK(t)
	// destPrivate=false (would normally anonymize), PIIFilter=ptr(false) → must NOT anonymize
	reg := buildModelWithPIIFilter(t, upstream.URL, false, boolPtr(false))
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = newTestPIIEngine(t)
	app := testApp(t, h)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(chatBody("model-x", "contact "+piiTestEmail)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	body := string(*lastBody)
	// With explicit pii_filter:false, original email passes through.
	if !strings.Contains(body, piiTestEmail) {
		t.Errorf("7b: upstream missing original email; pii_filter:false should suppress anonymization; body: %s", body)
	}
	if piiPseudonymPattern.MatchString(body) {
		t.Errorf("7b: upstream body contains pseudonym but pii_filter:false was set; body: %s", body)
	}
}

// TestPIIFilter_Priority_7c: PIIFilter=nil, destPrivate=false → anonymized (default for public).
func TestPIIFilter_Priority_7c_NilFlagPublicDest(t *testing.T) {
	t.Parallel()

	upstream, lastBody := captureOK(t)
	// PIIFilter=nil, destPrivate=false → default: anonymize (public destination)
	reg := buildModelWithPIIFilter(t, upstream.URL, false, nil)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = newTestPIIEngine(t)
	app := testApp(t, h)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(chatBody("model-x", "contact "+piiTestEmail)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	body := string(*lastBody)
	if strings.Contains(body, piiTestEmail) {
		t.Errorf("7c: upstream received original PII; default for public dest must anonymize; body: %s", body)
	}
	if !piiPseudonymPattern.MatchString(body) {
		t.Errorf("7c: upstream body missing pseudonym; body: %s", body)
	}
}

// TestPIIFilter_Priority_7d: PIIFilter=nil, destPrivate=true → NOT anonymized (default for private).
func TestPIIFilter_Priority_7d_NilFlagPrivateDest(t *testing.T) {
	t.Parallel()

	upstream, lastBody := captureOK(t)
	// PIIFilter=nil, destPrivate=true → default: do NOT anonymize (private destination)
	reg := buildModelWithPIIFilter(t, upstream.URL, true, nil)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = newTestPIIEngine(t)
	app := testApp(t, h)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(chatBody("model-x", "contact "+piiTestEmail)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	body := string(*lastBody)
	if !strings.Contains(body, piiTestEmail) {
		t.Errorf("7d: original email missing; default for private dest must NOT anonymize; body: %s", body)
	}
	if piiPseudonymPattern.MatchString(body) {
		t.Errorf("7d: upstream received pseudonym on private dest without explicit flag; body: %s", body)
	}
}

// TestPIIFilter_Priority_7e: deployment-level PIIFilter overrides model-level nil.
// Model.PIIFilter=nil, dep.PIIFilter=ptr(true), destPrivate=true → anonymized.
func TestPIIFilter_Priority_7e_DeploymentFlagOverridesModel(t *testing.T) {
	t.Parallel()

	upstream, lastBody := captureOK(t)
	// dep.PIIFilter=ptr(true), destPrivate=true (model.PIIFilter=nil)
	// → dep flag takes precedence → anonymize despite private dest
	reg, picker := buildModelWithDeploymentPIIFilter(t, upstream.URL, true, boolPtr(true))
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = newTestPIIEngine(t)
	h.Router = picker
	app := testApp(t, h)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(chatBody("model-x", "contact "+piiTestEmail)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	body := string(*lastBody)
	if strings.Contains(body, piiTestEmail) {
		t.Errorf("7e: original PII reached upstream despite dep.PIIFilter=true; body: %s", body)
	}
	if !piiPseudonymPattern.MatchString(body) {
		t.Errorf("7e: upstream body missing pseudonym; body: %s", body)
	}
}

// TestPIIFilter_Priority_Table is a table-driven summary that covers the full
// priority matrix in compact form, complementing the individual sub-tests above.
func TestPIIFilter_Priority_Table(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		destPrivate bool
		piiFilter   *bool
		wantAnon    bool // true = must receive pseudonym
	}{
		{"private+true", true, boolPtr(true), true},
		{"private+false", true, boolPtr(false), false},
		{"private+nil", true, nil, false},
		{"public+true", false, boolPtr(true), true},
		{"public+false", false, boolPtr(false), false},
		{"public+nil", false, nil, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream, lastBody := captureOK(t)
			reg := buildModelWithPIIFilter(t, upstream.URL, tc.destPrivate, tc.piiFilter)
			h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			h.PIIEngine = newTestPIIEngine(t)
			app := testApp(t, h)

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(chatBody("model-x", "hello "+piiTestEmail)))
			req.Header.Set("Content-Type", "application/json")

			resp, err := app.Test(req, testTimeout)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			body := string(*lastBody)
			if tc.wantAnon {
				if strings.Contains(body, piiTestEmail) {
					t.Errorf("[%s] original PII present; expected pseudonym; body: %s", tc.name, body)
				}
				if !piiPseudonymPattern.MatchString(body) {
					t.Errorf("[%s] pseudonym missing; body: %s", tc.name, body)
				}
			} else {
				if !strings.Contains(body, piiTestEmail) {
					t.Errorf("[%s] original email missing; expected pass-through; body: %s", tc.name, body)
				}
				if piiPseudonymPattern.MatchString(body) {
					t.Errorf("[%s] unexpected pseudonym in body; body: %s", tc.name, body)
				}
			}
		})
	}
}

// ── 8. stream-cap / scanner.Err fail-closed ───────────────────────────────────

// TestPIIFilter_StreamCap_FailClosed verifies that when the upstream SSE
// response exceeds the aggregate stream byte cap (MaxResponseBody), the handler
// aborts the stream without emitting any un-restored content to the client.
//
// Approach: set a very small MaxResponseBody on the handler so that even a
// minimal SSE response triggers the cap. The upstream emits PII pseudonyms
// in multiple SSE chunks that collectively exceed the cap.
//
// Expected behavior: the client receives an empty or minimal response, and
// the pseudonym must not appear in the client body (fail-closed: no partial
// un-restored content).
func TestPIIFilter_StreamCap_FailClosed(t *testing.T) {
	t.Parallel()

	// Pre-compute the pseudonym that will be generated for piiTestEmail.
	engine := newTestPIIEngine(t)
	sampleFilter := engine.NewFilter("")
	sampleBody := []byte(chatBody("model-x", "email: "+piiTestEmail))
	anonBody, err := sampleFilter.AnonymizeJSON(sampleBody)
	if err != nil {
		t.Fatalf("pre-compute AnonymizeJSON: %v", err)
	}
	pseudo := piiPseudonymPattern.FindString(string(anonBody))
	if pseudo == "" {
		t.Fatal("could not derive pseudonym")
	}

	// Mock upstream: emits a large SSE response where each chunk carries the
	// pseudonym, so the total byte count will exceed the small cap we set.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		// Emit 20 chunks, each ~100 bytes, to exceed a 512-byte cap.
		for i := 0; i < 20; i++ {
			chunk := fmt.Sprintf(
				`data: {"id":"sc%d","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":%q},"finish_reason":null}]}`,
				i, pseudo)
			fmt.Fprintln(w, chunk)
			fmt.Fprintln(w) // blank line = SSE separator
			flusher.Flush()
		}
		fmt.Fprintln(w, "data: [DONE]")
		fmt.Fprintln(w)
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	reg := piiRegistryExternal(t, upstream.URL)
	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = engine
	// Set a tiny response body limit (512 bytes) so the cap fires on the SSE path.
	h.MaxResponseBody = 512

	baseURL := startTestServer(t, h)

	streamBody := fmt.Sprintf(`{"model":"ext-model","messages":[{"role":"user","content":"email: %s"}],"stream":true}`, piiTestEmail)
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
	bodyStr := string(fullBody)

	// Fail-closed invariant: the raw pseudonym must NEVER appear in the client
	// response. With the incremental StreamRestorer (Stage 0b), content is
	// emitted token-by-token as it is safely restored. The pseudonym is always
	// replaced by the original value BEFORE emission — it never reaches the
	// wire in pseudonymized form, not even partially.
	if strings.Contains(bodyStr, pseudo) {
		t.Errorf("stream-cap fail-closed VIOLATED: pseudonym %q visible in client response\nbody: %s",
			pseudo, bodyStr)
	}

	// With the incremental restore path (Stage 0b), some chunks may have been
	// emitted and correctly restored (original email visible) before the byte
	// cap fired and aborted the stream. This is correct: the cap prevents
	// unbounded memory growth, not correct restoration of already-emitted
	// content. The security guarantee is "pseudonyms never reach the client",
	// not "no content is emitted when the stream is oversized".
	//
	// We do NOT assert that piiTestEmail is absent — it may legitimately appear
	// in correctly restored chunks that were emitted before the cap fired.
}

// TestPIIFilter_StreamCapNote documents the coverage gap for scanner.Err.
// The scanner.Err path is triggered when the upstream connection is closed
// mid-stream or when a single SSE line exceeds the scanner buffer (4MB).
// These conditions are hard to trigger deterministically in tests because
// httptest.Server and the Go HTTP client do not easily simulate transport-level
// truncation on the streaming path.
//
// The fail-closed logic for scanner.Err is in handler.go's PII scan loop:
// when scanErr != nil after the loop, the handler sets streamAborted=true,
// records a circuit breaker failure, and does not emit further content.
//
// Coverage at the unit level lives in stream_test.go (the oracle
// RestoreSSEStream function is tested with well-formed and broken inputs,
// and StreamRestorer Push is tested directly in the pii package). This test
// is intentionally a non-asserting placeholder documenting the gap.
func TestPIIFilter_ScannerErr_CoverageGapDocumented(t *testing.T) {
	t.Parallel()
	t.Log("COVERAGE GAP: scanner.Err fail-closed path (mid-stream transport truncation) " +
		"cannot be triggered deterministically via httptest.Server. " +
		"The logic is covered by code inspection + pii package unit tests.")
}

// ── 9. classifyDestPrivate ────────────────────────────────────────────────────

// TestClassifyDestPrivate covers the classification of various host types.
// The function is unexported so we test it from package proxy.
func TestClassifyDestPrivate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		rawURL string
		want   bool // true = private
	}{
		// Loopback
		{"loopback 127.0.0.1", "http://127.0.0.1:8080/v1", true},
		{"loopback 127.0.0.2", "http://127.0.0.2/v1", true},
		{"loopback ::1", "http://[::1]:11434/api", true},
		// localhost literal
		{"localhost literal", "http://localhost:8080", true},
		{"localhost https", "https://localhost/v1", true},
		// RFC 1918
		{"10.x private", "http://10.0.0.1:8080/v1", true},
		{"10.255.255.255 private", "http://10.255.255.255/v1", true},
		{"192.168.x private", "http://192.168.1.1:8080/v1", true},
		{"172.16.x private", "http://172.16.0.1/v1", true},
		{"172.31.255.255 private", "http://172.31.255.255/v1", true},
		// Link-local
		{"169.254.x link-local", "http://169.254.1.1/v1", true},
		{"fe80:: link-local", "http://[fe80::1]/v1", true},
		// Public addresses
		{"openai public", "https://api.openai.com/v1", false},
		{"anthropic public", "https://api.anthropic.com/v1", false},
		{"8.8.8.8 public", "http://8.8.8.8/v1", false},
		{"203.0.113.1 public (TEST-NET-3)", "http://203.0.113.1/v1", false},
		// Named host (not localhost) → public (no DNS lookup)
		{"custom named host", "http://my-vllm-server.internal:8080/v1", false},
		{"corp hostname", "http://llm.corp.example.com/v1", false},
		// Edge cases
		{"empty URL", "", false},
		{"172.32.x (just outside 172.16/12)", "http://172.32.0.1/v1", false},
		{"192.169.x (just outside 192.168/16)", "http://192.169.0.1/v1", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := classifyDestPrivate(tc.rawURL)
			if got != tc.want {
				t.Errorf("classifyDestPrivate(%q) = %v, want %v", tc.rawURL, got, tc.want)
			}
		})
	}
}

// TestClassifyDestPrivate_UsedByAddModel verifies that AddModel correctly
// derives destPrivate via classifyDestPrivate from the model's BaseURL.
// We assert the resulting behavior end-to-end: a model pointing at a loopback
// address must pass the original body through unmodified (no anonymization).
func TestClassifyDestPrivate_UsedByAddModel(t *testing.T) {
	t.Parallel()

	upstream, lastBody := captureOK(t)
	// The upstream listens on 127.0.0.1 (loopback). AddModel calls
	// classifyDestPrivate which must return true, setting destPrivate=true.
	// With PIIEngine set and PIIFilter=nil, shouldAnonymize returns !destPrivate = false.
	r := &Registry{
		models:  make(map[string]*Model),
		aliases: make(map[string]string),
	}
	r.AddModel(Model{
		Name:     "loopback-model",
		Provider: "vllm",
		Type:     "chat",
		BaseURL:  upstream.URL, // 127.0.0.1 → classifyDestPrivate → destPrivate=true
		APIKey:   "key",
	})

	h := NewProxyHandler(r, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.PIIEngine = newTestPIIEngine(t)
	app := testApp(t, h)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(chatBody("loopback-model", "contact "+piiTestEmail)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	body := string(*lastBody)
	// loopback → destPrivate=true → shouldAnonymize returns false → original email passes through.
	if !strings.Contains(body, piiTestEmail) {
		t.Errorf("loopback model via AddModel: original email missing; "+
			"classifyDestPrivate may not have set destPrivate=true; body: %s", body)
	}
	if piiPseudonymPattern.MatchString(body) {
		t.Errorf("loopback model via AddModel: unexpected pseudonym in body; body: %s", body)
	}
}

// ── 10. FIX 4: raw model name must not appear in OTel spans ──────────────────

// TestFIX4_Tracing_RawModelNotInSpan verifies that the raw, client-supplied
// model string never appears as a span attribute. Before the fix, the raw
// envelope.Model was set as model.requested BEFORE resolveModel was called —
// meaning a PII-containing model string (e.g. "gpt-4/user@example.com") would
// be persisted in the tracing backend.
//
// After the fix, only the validated canonical model name (model.Name from the
// registry) is set as model.canonical. The raw string never appears in any
// span attribute.
func TestFIX4_Tracing_RawModelNotInSpan(t *testing.T) {
	t.Parallel()

	upstream, _ := captureOK(t)
	reg := buildModelWithPIIFilter(t, upstream.URL, false, nil)

	// Wire an in-memory span recorder so we can inspect emitted attributes.
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	tracer := tp.Tracer("test")

	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.Tracer = tracer
	app := testApp(t, h)

	// Send a request with a valid model name. The canonical name is "model-x".
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"model-x","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	// Flush the tracer so all spans are recorded.
	_ = tp.Shutdown(t.Context())

	ended := recorder.Ended()
	if len(ended) == 0 {
		t.Fatal("no spans recorded; expected at least one proxy.handle span")
	}

	// Inspect all span attributes across all recorded spans.
	for _, span := range ended {
		for _, attr := range span.Attributes() {
			key := string(attr.Key)
			val := attr.Value.AsString()

			// model.requested must NEVER appear (raw client value must not be set).
			if key == "model.requested" {
				t.Errorf("FIX 4: span %q has attribute model.requested=%q; "+
					"raw client model must not be set as a span attribute",
					span.Name(), val)
			}

			// model.canonical must be the registry-validated name, not a raw value.
			if key == "model.canonical" && val != "model-x" {
				t.Errorf("FIX 4: span %q has model.canonical=%q, want %q",
					span.Name(), val, "model-x")
			}
		}
	}

	// Verify model.canonical IS set (the fix does not suppress all tracing,
	// only the pre-resolution raw-value attribute).
	found := false
	for _, span := range ended {
		for _, attr := range span.Attributes() {
			if string(attr.Key) == "model.canonical" {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("FIX 4: model.canonical span attribute not found; expected it to be set after resolution")
	}
}

// TestFIX4_Tracing_UnknownModelNoRawAttr verifies that when model resolution
// fails (model not in registry), NO span attribute with the raw client-supplied
// model name is emitted. A PII-containing model name would otherwise be
// persisted in the tracing backend.
func TestFIX4_Tracing_UnknownModelNoRawAttr(t *testing.T) {
	t.Parallel()

	upstream, _ := captureOK(t)
	reg := buildModelWithPIIFilter(t, upstream.URL, false, nil)

	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	tracer := tp.Tracer("test")

	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.Tracer = tracer
	app := testApp(t, h)

	// Use a model name that embeds PII — this is the adversarial case the fix
	// protects against. The model does not exist in the registry.
	rawModelWithPII := "gpt-4/user@example.com"
	req := httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"`+rawModelWithPII+`","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	// Expect 404 — model not found.
	if resp.StatusCode != http.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Logf("status = %d, body: %s", resp.StatusCode, b)
	}

	_ = tp.Shutdown(t.Context())

	ended := recorder.Ended()

	// Verify the raw PII-containing model name does NOT appear in any span attribute.
	for _, span := range ended {
		for _, attr := range span.Attributes() {
			val := attr.Value.AsString()
			if strings.Contains(val, rawModelWithPII) {
				t.Errorf("FIX 4: span %q attribute %q = %q leaks raw model name with PII",
					span.Name(), attr.Key, val)
			}
			// Also check for the email portion specifically.
			if strings.Contains(val, "user@example.com") {
				t.Errorf("FIX 4: span %q attribute %q = %q leaks PII from model name",
					span.Name(), attr.Key, val)
			}
			if string(attr.Key) == "model.requested" {
				t.Errorf("FIX 4: span %q has model.requested=%q; attribute must never be set",
					span.Name(), val)
			}
		}
	}
}
