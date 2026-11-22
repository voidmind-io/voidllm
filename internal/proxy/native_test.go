package proxy

// native_test.go covers the native /v1/messages and /v1/responses endpoints:
// format detection, header validation, usage parsers, stream observation, and
// a few end-to-end handler scenarios.

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/usage"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

func TestFormatForPath(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path string
		want apiFormat
	}{
		{"chat/completions", formatChat},
		{"completions", formatChat},
		{"messages", formatMessages},
		{"messages/count_tokens", formatMessages},
		{"responses", formatResponses},
		{"responses/resp_123", formatOther},
		{"messages/batches", formatOther},
		{"embeddings", formatOther},
		{"rerank", formatOther},
	}
	for _, tc := range tests {
		if got := formatForPath(tc.path); got != tc.want {
			t.Errorf("formatForPath(%q) = %d, want %d", tc.path, got, tc.want)
		}
	}
}

func TestIsAllowedPath_Native(t *testing.T) {
	t.Parallel()
	for _, p := range []string{"messages", "messages/count_tokens", "responses"} {
		if !isAllowedPath(p) {
			t.Errorf("isAllowedPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"responses/resp_1", "messages/batches", "messages/123"} {
		if isAllowedPath(p) {
			t.Errorf("isAllowedPath(%q) = true, want false", p)
		}
	}
	if !isUnsupportedNativeSubpath("responses/resp_1") || !isUnsupportedNativeSubpath("messages/batches") {
		t.Error("sub-resources of native endpoints must be flagged unsupported")
	}
	if isUnsupportedNativeSubpath("messages/count_tokens") || isUnsupportedNativeSubpath("responses") {
		t.Error("count_tokens and the base endpoints must not be flagged unsupported")
	}
}

func TestProviderSupportsFormat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		provider string
		format   apiFormat
		want     bool
	}{
		{"anthropic", formatMessages, true},
		{"openai", formatMessages, false},
		{"vllm", formatMessages, false},
		{"bedrock", formatMessages, false},
		{"openai", formatResponses, true},
		{"custom", formatResponses, true},
		{"vllm", formatResponses, true},
		{"ollama", formatResponses, true},
		{"azure", formatResponses, false},
		{"anthropic", formatResponses, false},
		{"gemini", formatResponses, false},
		{"bedrock", formatResponses, false},
		{"anthropic", formatChat, true},
		{"bedrock", formatOther, true},
	}
	for _, tc := range tests {
		if got := providerSupportsFormat(tc.provider, tc.format); got != tc.want {
			t.Errorf("providerSupportsFormat(%q, %d) = %v, want %v", tc.provider, tc.format, got, tc.want)
		}
	}
}

func TestValidAnthropicVersion(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"2023-06-01":     "2023-06-01",
		"2024-10-22":     "2024-10-22",
		"":               defaultAnthropicVersion,
		"latest":         defaultAnthropicVersion,
		"2023-6-1":       defaultAnthropicVersion,
		"2023-06-01\r\n": defaultAnthropicVersion,
		"2023-06-01 ":    defaultAnthropicVersion,
	}
	for in, want := range tests {
		if got := validAnthropicVersion(in); got != want {
			t.Errorf("validAnthropicVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidBetaList(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		ok   bool
	}{
		{"single", "prompt-caching-2024-07-31", true},
		{"list", "claude-code-20250219,interleaved-thinking-2025-05-14", true},
		{"empty", "", false},
		{"space after comma", "a-1, b-2", false},
		{"uppercase", "Prompt-Caching", false},
		{"underscore", "a_b", false},
		{"trailing comma", "a,", false},
		{"leading comma", ",a", false},
		{"newline", "a\nb", false},
		{"too long", strings.Repeat("a", maxBetaHeaderLen+1), false},
		{"max length", strings.Repeat("a", maxBetaHeaderLen), true},
	}
	for _, tc := range tests {
		if got := validBetaList(tc.in, anthropicBetaTokenRe); got != tc.ok {
			t.Errorf("%s: validBetaList(%q) = %v, want %v", tc.name, tc.in, got, tc.ok)
		}
	}
	if !validBetaList("responses=experimental", openAIBetaTokenRe) {
		t.Error("OpenAI-Beta name=value token must be accepted")
	}
	if validBetaList("responses=experimental", anthropicBetaTokenRe) {
		t.Error("anthropic-beta must not accept name=value tokens")
	}
}

func TestParseMessagesUsage(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"msg_1","type":"message","usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":20}}`)
	got := parseMessagesUsage(body)
	want := UsageInfo{PromptTokens: 115, CompletionTokens: 20, TotalTokens: 135, CachedReadTokens: 100, CacheWriteTokens: 5}
	if got != want {
		t.Errorf("parseMessagesUsage = %+v, want %+v", got, want)
	}
	if got := parseMessagesUsage([]byte(`not json`)); got != (UsageInfo{}) {
		t.Errorf("non-JSON body = %+v, want zero", got)
	}
	if got := parseMessagesUsage([]byte(`{"type":"error"}`)); got != (UsageInfo{}) {
		t.Errorf("no usage = %+v, want zero", got)
	}
}

func TestParseResponsesUsage(t *testing.T) {
	t.Parallel()
	body := []byte(`{"id":"resp_1","usage":{"input_tokens":50,"input_tokens_details":{"cached_tokens":30},"output_tokens":12,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":62}}`)
	got := parseResponsesUsage(body)
	want := UsageInfo{PromptTokens: 50, CompletionTokens: 12, TotalTokens: 62, CachedReadTokens: 30}
	if got != want {
		t.Errorf("parseResponsesUsage = %+v, want %+v", got, want)
	}
	noTotal := parseResponsesUsage([]byte(`{"usage":{"input_tokens":5,"output_tokens":7}}`))
	if noTotal.TotalTokens != 12 {
		t.Errorf("total without total_tokens = %d, want 12", noTotal.TotalTokens)
	}
}

// feed runs every line through observe and reports the terminal classification
// at each blank line, like the streaming loop does.
func feed(n *nativeStream, lines ...string) nativeTerminal {
	last := termNone
	for _, l := range lines {
		if l == "" {
			if k := n.endEvent(); k != termNone {
				last = k
			}
			continue
		}
		n.observe([]byte(l))
	}
	return last
}

func TestNativeStream_Messages(t *testing.T) {
	t.Parallel()
	n := &nativeStream{format: formatMessages}
	kind := feed(n,
		`event: message_start`,
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":25,"cache_read_input_tokens":10,"cache_creation_input_tokens":3,"output_tokens":1}}}`,
		``,
		`event: content_block_delta`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}`,
		``,
		`event: message_stop`,
		`data: {"type":"message_stop"}`,
		``,
	)
	if kind != termOK {
		t.Fatalf("terminal = %d, want termOK", kind)
	}
	got := n.usage()
	want := UsageInfo{PromptTokens: 38, CompletionTokens: 42, TotalTokens: 80, CachedReadTokens: 10, CacheWriteTokens: 3}
	if got != want {
		t.Errorf("usage = %+v, want %+v", got, want)
	}
}

func TestNativeStream_MessagesErrorEvent(t *testing.T) {
	t.Parallel()
	n := &nativeStream{format: formatMessages}
	kind := feed(n,
		`event: error`,
		`data: {"type":"error","error":{"type":"overloaded_error","message":"x"}}`,
		``,
	)
	if kind != termFailed {
		t.Errorf("terminal = %d, want termFailed", kind)
	}
}

func TestNativeStream_Responses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		event string
		want  nativeTerminal
	}{
		{"completed", "response.completed", termOK},
		{"incomplete", "response.incomplete", termIncomplete},
		{"failed", "response.failed", termFailed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := &nativeStream{format: formatResponses}
			kind := feed(n,
				`event: response.output_text.delta`,
				`data: {"type":"response.output_text.delta","delta":"hi"}`,
				``,
				`event: `+tc.event,
				`data: {"type":"`+tc.event+`","response":{"usage":{"input_tokens":9,"input_tokens_details":{"cached_tokens":4},"output_tokens":3,"total_tokens":12}}}`,
				``,
			)
			if kind != tc.want {
				t.Fatalf("terminal = %d, want %d", kind, tc.want)
			}
			want := UsageInfo{PromptTokens: 9, CompletionTokens: 3, TotalTokens: 12, CachedReadTokens: 4}
			if got := n.usage(); got != want {
				t.Errorf("usage = %+v, want %+v", got, want)
			}
		})
	}

	n := &nativeStream{format: formatResponses}
	if kind := feed(n, `event: error`, `data: {"type":"error","code":"x","message":"y"}`, ``); kind != termFailed {
		t.Errorf("error event terminal = %d, want termFailed", kind)
	}
}

func TestNativeEventType_FallbackParse(t *testing.T) {
	t.Parallel()
	if got := string(nativeEventType([]byte(`{"type":"message_stop"}`))); got != "message_stop" {
		t.Errorf("fast path = %q", got)
	}
	if got := string(nativeEventType([]byte(`{"index":0,"type":"content_block_stop"}`))); got != "content_block_stop" {
		t.Errorf("fallback path = %q", got)
	}
	if got := nativeEventType([]byte(`{broken`)); len(got) != 0 {
		t.Errorf("broken JSON = %q, want empty", got)
	}
}

func TestWriteFormatAbortEvent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		format apiFormat
		want   string
	}{
		{formatMessages, "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"stream aborted\"}}\n\n"},
		{formatResponses, "event: error\ndata: {\"type\":\"error\",\"code\":\"stream_incomplete\",\"message\":\"stream aborted\"}\n\n"},
		{formatChat, "data: {\"error\":{\"message\":\"stream aborted\",\"type\":\"stream_incomplete\",\"code\":\"stream_incomplete\"}}\n\n"},
	}
	for _, tc := range tests {
		var buf bytes.Buffer
		w := bufio.NewWriter(&buf)
		if !writeFormatAbortEvent(w, tc.format, "stream_incomplete") {
			t.Fatalf("format %d: write failed", tc.format)
		}
		if buf.String() != tc.want {
			t.Errorf("format %d:\n got %q\nwant %q", tc.format, buf.String(), tc.want)
		}
	}
}

func TestNativeUsableModel(t *testing.T) {
	t.Parallel()
	off := false
	h := &ProxyHandler{}

	vllm := Model{Name: "m", Provider: "vllm"}
	if _, block := h.nativeUsableModel(vllm, formatMessages); block != nativeProviderBlocked {
		t.Errorf("vllm for messages: block = %d, want provider-blocked", block)
	}
	if _, block := h.nativeUsableModel(vllm, formatResponses); block != nativeUsable {
		t.Errorf("vllm for responses: block = %d, want usable", block)
	}

	// With the PII engine active, a public destination is blocked unless PII
	// filtering is explicitly turned off for it.
	h.PIIEngine = newTestPIIEngine(t)
	if _, block := h.nativeUsableModel(vllm, formatResponses); block != nativePIIBlocked {
		t.Errorf("public destination with PII engine: block = %d, want PII-blocked", block)
	}
	vllm.PIIFilter = &off
	if _, block := h.nativeUsableModel(vllm, formatResponses); block != nativeUsable {
		t.Errorf("pii_filter=false: block = %d, want usable", block)
	}

	// Multi-deployment: incompatible and PII-bound deployments are dropped, the
	// rest is kept, and the input model is not modified.
	h.Router = stubPicker{}
	multi := Model{Name: "multi", Provider: "anthropic", Deployments: []Deployment{
		{Name: "a", Provider: "openai"},
		{Name: "b", Provider: "anthropic", PIIFilter: &off},
		{Name: "c", Provider: "anthropic"},
	}}
	usable, block := h.nativeUsableModel(multi, formatMessages)
	if block != nativeUsable || len(usable.Deployments) != 1 || usable.Deployments[0].Name != "b" {
		t.Errorf("multi: block = %d, deployments = %+v, want only b", block, usable.Deployments)
	}
	if len(multi.Deployments) != 3 {
		t.Errorf("input model was modified: %d deployments", len(multi.Deployments))
	}
	if _, block := h.nativeUsableModel(multi, formatResponses); block != nativePIIBlocked {
		// only deployment a (openai) is provider-compatible, and it is public.
		t.Errorf("multi for responses: block = %d, want PII-blocked", block)
	}
	onlyOpenAI := Model{Name: "x", Provider: "openai", Deployments: []Deployment{{Name: "a", Provider: "openai"}}}
	if _, block := h.nativeUsableModel(onlyOpenAI, formatMessages); block != nativeProviderBlocked {
		t.Errorf("no compatible deployment: block = %d, want provider-blocked", block)
	}
}

// stubPicker is a DeploymentPicker that returns the model's deployments as-is.
type stubPicker struct{}

// Pick returns the deployments of model unchanged.
func (stubPicker) Pick(model Model) []Deployment { return model.Deployments }

// ──────────────────────────────────────────────────────────────────────────────
// Handler scenarios
// ──────────────────────────────────────────────────────────────────────────────

var nativeHMACSecret = []byte("native-test-hmac-secret-0123456789")

// nativeRig is a handler wired with auth, a usage logger, and one model.
type nativeRig struct {
	h      *ProxyHandler
	app    *fiber.App
	key    string
	logger *usage.Logger
	db     interface {
		SQL() *sql.DB
	}
}

type nativeUsageRow struct {
	prompt, completion, total, cachedRead, cacheWrite, status int
	ttft                                                      sql.NullInt64
	cost                                                      sql.NullFloat64
}

// newNativeRig builds a rig for one model of the given provider whose upstream
// is served by handler.
func newNativeRig(t *testing.T, providerName string, upstream http.Handler) *nativeRig {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)

	model := &Model{
		Name:     "native-model",
		Provider: providerName,
		BaseURL:  srv.URL,
		APIKey:   "upstream-secret",
		Pricing:  config.PricingConfig{InputPer1M: 1, OutputPer1M: 2},
	}
	reg := &Registry{models: map[string]*Model{model.Name: model}, aliases: make(map[string]string)}
	reg.rebuildSorted()

	d := openCostFormulaDB(t)
	ul := usage.NewLogger(d, config.UsageConfig{BufferSize: 8, FlushInterval: time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ul.Start()

	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.UsageLogger = ul

	keyCache := cache.New[string, auth.KeyInfo]()
	raw, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("keygen.Generate: %v", err)
	}
	keyCache.Set(keygen.Hash(raw, nativeHMACSecret), auth.KeyInfo{
		ID: "native-key", KeyType: keygen.KeyTypeUser, OrgID: "native-org", Role: auth.RoleMember,
	})
	app := fiber.New()
	app.Use(auth.Middleware(keyCache, nativeHMACSecret))
	app.All("/v1/*", h.Handle)

	return &nativeRig{h: h, app: app, key: raw, logger: ul, db: d}
}

// do sends a request and returns status, headers and body.
func (r *nativeRig) do(t *testing.T, method, path, body string, hdr map[string]string) (int, http.Header, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := r.app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, resp.Header, string(b)
}

// rows flushes the logger and returns all usage rows.
func (r *nativeRig) rows(t *testing.T) []nativeUsageRow {
	t.Helper()
	r.logger.Stop()
	q, err := r.db.SQL().QueryContext(context.Background(),
		"SELECT prompt_tokens, completion_tokens, total_tokens, cached_read_tokens, cache_write_tokens, status_code, ttft_ms, cost_estimate FROM usage_events")
	if err != nil {
		t.Fatalf("query usage_events: %v", err)
	}
	defer q.Close()
	var out []nativeUsageRow
	for q.Next() {
		var u nativeUsageRow
		if err := q.Scan(&u.prompt, &u.completion, &u.total, &u.cachedRead, &u.cacheWrite, &u.status, &u.ttft, &u.cost); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, u)
	}
	if err := q.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func sseHandler(events string, captured *atomic.Value) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.Store(capturedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), RawBody: body})
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, events)
	})
}

func TestNative_MessagesNonStreaming(t *testing.T) {
	t.Parallel()
	var captured atomic.Value
	rig := newNativeRig(t, "anthropic", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured.Store(capturedRequest{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), RawBody: body})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req_abc")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "99")
		fmt.Fprint(w, `{"id":"msg_1","type":"message","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":5,"output_tokens":20}}`)
	}))

	status, hdr, body := rig.do(t, http.MethodPost, "/v1/messages",
		`{"model":"native-model","max_tokens":10,"messages":[{"role":"user","content":"hello"}]}`,
		map[string]string{
			"x-api-key":         rig.key,
			"anthropic-version": "2024-10-22",
			"anthropic-beta":    "prompt-caching-2024-07-31",
		})
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, body)
	}
	if hdr.Get("Request-Id") != "req_abc" || hdr.Get("Anthropic-Ratelimit-Requests-Remaining") != "99" {
		t.Errorf("anthropic response headers not forwarded: %v", hdr)
	}
	if !strings.Contains(body, `"msg_1"`) {
		t.Errorf("body not passed through natively: %s", body)
	}

	got := captured.Load().(capturedRequest)
	if got.Path != "/v1/messages" {
		t.Errorf("upstream path = %q, want /v1/messages", got.Path)
	}
	if got.Header.Get("x-api-key") != "upstream-secret" {
		t.Errorf("upstream x-api-key = %q, want the model key", got.Header.Get("x-api-key"))
	}
	if got.Header.Get("Authorization") != "" {
		t.Errorf("Authorization must not reach the upstream, got %q", got.Header.Get("Authorization"))
	}
	if got.Header.Get("anthropic-version") != "2024-10-22" {
		t.Errorf("anthropic-version = %q", got.Header.Get("anthropic-version"))
	}
	if got.Header.Get("anthropic-beta") != "prompt-caching-2024-07-31" {
		t.Errorf("anthropic-beta = %q", got.Header.Get("anthropic-beta"))
	}
	if !strings.Contains(string(got.RawBody), `"messages":[{"role":"user","content":"hello"}]`) {
		t.Errorf("request body was translated: %s", got.RawBody)
	}

	rows := rig.rows(t)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.prompt != 115 || r.completion != 20 || r.total != 135 || r.cachedRead != 100 || r.cacheWrite != 5 || r.status != 200 {
		t.Errorf("usage row = %+v", r)
	}
	if !r.cost.Valid || r.cost.Float64 <= 0 {
		t.Errorf("cost = %+v, want a positive estimate", r.cost)
	}
}

func TestNative_MessagesInvalidHeadersAreSanitized(t *testing.T) {
	t.Parallel()
	var captured atomic.Value
	rig := newNativeRig(t, "anthropic", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Store(capturedRequest{Path: r.URL.Path, Header: r.Header.Clone()})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	status, _, body := rig.do(t, http.MethodPost, "/v1/messages", `{"model":"native-model"}`,
		map[string]string{
			"Authorization":     "Bearer " + rig.key,
			"x-api-key":         "vl_uk_ignored",
			"anthropic-version": "not-a-date",
			"anthropic-beta":    "Bad_Token",
		})
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, body)
	}
	got := captured.Load().(capturedRequest)
	if got.Header.Get("anthropic-version") != defaultAnthropicVersion {
		t.Errorf("anthropic-version = %q, want default", got.Header.Get("anthropic-version"))
	}
	if got.Header.Get("anthropic-beta") != "" {
		t.Errorf("invalid anthropic-beta forwarded: %q", got.Header.Get("anthropic-beta"))
	}
}

func TestNative_MessagesStreamingNoDone(t *testing.T) {
	t.Parallel()
	var captured atomic.Value
	events := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":25,"cache_read_input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":42}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	rig := newNativeRig(t, "anthropic", sseHandler(events, &captured))

	status, _, body := rig.do(t, http.MethodPost, "/v1/messages",
		`{"model":"native-model","stream":true,"max_tokens":10,"messages":[]}`,
		map[string]string{"x-api-key": rig.key})
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, body)
	}
	if body != events {
		t.Errorf("stream was modified:\n got %q\nwant %q", body, events)
	}
	if strings.Contains(body, "stream aborted") || strings.Contains(body, "[DONE]") {
		t.Errorf("unexpected abort or [DONE] event in stream: %q", body)
	}

	got := captured.Load().(capturedRequest)
	if strings.Contains(string(got.RawBody), "stream_options") {
		t.Errorf("stream_options injected into a Messages request: %s", got.RawBody)
	}

	rows := rig.rows(t)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.status != 200 || r.prompt != 35 || r.completion != 42 || r.total != 77 || r.cachedRead != 10 || !r.ttft.Valid {
		t.Errorf("usage row = %+v", r)
	}
}

func TestNative_MessagesStreamTruncatedGetsAbortEvent(t *testing.T) {
	t.Parallel()
	var captured atomic.Value
	events := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":5,"output_tokens":1}}}` + "\n\n"
	rig := newNativeRig(t, "anthropic", sseHandler(events, &captured))

	_, _, body := rig.do(t, http.MethodPost, "/v1/messages",
		`{"model":"native-model","stream":true}`, map[string]string{"x-api-key": rig.key})
	wantTail := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"stream aborted\"}}\n\n"
	if !strings.HasSuffix(body, wantTail) {
		t.Errorf("missing Anthropic abort event; body = %q", body)
	}
	rows := rig.rows(t)
	if len(rows) != 1 || rows[0].status != http.StatusBadGateway || rows[0].prompt != 5 {
		t.Errorf("usage rows = %+v, want one 502 row with partial tokens", rows)
	}
}

func TestNative_ResponsesStreaming(t *testing.T) {
	t.Parallel()
	var captured atomic.Value
	events := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":50,"input_tokens_details":{"cached_tokens":30},"output_tokens":12,"total_tokens":62}}}` + "\n\n"
	rig := newNativeRig(t, "vllm", sseHandler(events, &captured))

	status, _, body := rig.do(t, http.MethodPost, "/v1/responses",
		`{"model":"native-model","stream":true,"input":"hello"}`,
		map[string]string{
			"Authorization": "Bearer " + rig.key,
			"OpenAI-Beta":   "responses=experimental",
		})
	if status != http.StatusOK {
		t.Fatalf("status = %d; body %s", status, body)
	}
	if body != events {
		t.Errorf("stream was modified:\n got %q\nwant %q", body, events)
	}
	got := captured.Load().(capturedRequest)
	if got.Path != "/responses" {
		t.Errorf("upstream path = %q, want /responses", got.Path)
	}
	if strings.Contains(string(got.RawBody), "stream_options") {
		t.Errorf("stream_options injected into a Responses request: %s", got.RawBody)
	}
	if got.Header.Get("OpenAI-Beta") != "responses=experimental" {
		t.Errorf("OpenAI-Beta = %q", got.Header.Get("OpenAI-Beta"))
	}
	if got.Header.Get("Authorization") != "Bearer upstream-secret" {
		t.Errorf("upstream Authorization = %q", got.Header.Get("Authorization"))
	}

	rows := rig.rows(t)
	if len(rows) != 1 {
		t.Fatalf("usage rows = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.status != 200 || r.prompt != 50 || r.completion != 12 || r.total != 62 || r.cachedRead != 30 {
		t.Errorf("usage row = %+v", r)
	}
}

func TestNative_ResponsesFailedStreamLogsErrorStatus(t *testing.T) {
	t.Parallel()
	var captured atomic.Value
	events := "event: response.failed\n" +
		`data: {"type":"response.failed","response":{"usage":{"input_tokens":3,"output_tokens":0,"total_tokens":3}}}` + "\n\n"
	rig := newNativeRig(t, "vllm", sseHandler(events, &captured))
	_, _, body := rig.do(t, http.MethodPost, "/v1/responses",
		`{"model":"native-model","stream":true}`, map[string]string{"x-api-key": rig.key})
	if body != events {
		t.Errorf("a terminal failure event must be forwarded without an added abort event, got %q", body)
	}
	rows := rig.rows(t)
	if len(rows) != 1 || rows[0].status != http.StatusBadGateway {
		t.Errorf("usage rows = %+v, want one 502 row", rows)
	}
}

func TestNative_CountTokensNoUsageEvent(t *testing.T) {
	t.Parallel()
	var captured atomic.Value
	rig := newNativeRig(t, "anthropic", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Store(capturedRequest{Path: r.URL.Path})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"input_tokens":123}`)
	}))
	status, _, body := rig.do(t, http.MethodPost, "/v1/messages/count_tokens",
		`{"model":"native-model","messages":[]}`, map[string]string{"x-api-key": rig.key})
	if status != http.StatusOK || body != `{"input_tokens":123}` {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	if got := captured.Load().(capturedRequest); got.Path != "/v1/messages/count_tokens" {
		t.Errorf("upstream path = %q", got.Path)
	}
	if rows := rig.rows(t); len(rows) != 0 {
		t.Errorf("count_tokens produced %d usage rows, want 0", len(rows))
	}
}

func TestNative_IncompatibleProviderRejectedBeforeUpstream(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	rig := newNativeRig(t, "vllm", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	status, _, body := rig.do(t, http.MethodPost, "/v1/messages",
		`{"model":"native-model"}`, map[string]string{"x-api-key": rig.key})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", status, body)
	}
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"invalid_request_error"`) {
		t.Errorf("body is not an Anthropic error envelope: %s", body)
	}
	if hits.Load() != 0 {
		t.Errorf("upstream was contacted %d times", hits.Load())
	}
}

func TestNative_UnsupportedMethodsAndSubpaths(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	rig := newNativeRig(t, "vllm", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/responses/resp_1"},
		{http.MethodDelete, "/v1/responses/resp_1"},
		{http.MethodGet, "/v1/responses"},
		{http.MethodGet, "/v1/messages"},
	} {
		status, _, body := rig.do(t, tc.method, tc.path, "", map[string]string{"Authorization": "Bearer " + rig.key})
		if status != http.StatusBadRequest {
			t.Errorf("%s %s: status = %d, want 400; body %s", tc.method, tc.path, status, body)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("upstream was contacted %d times", hits.Load())
	}
}

func TestNative_AuthEnvelopeOnMessages(t *testing.T) {
	t.Parallel()
	rig := newNativeRig(t, "anthropic", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	status, _, body := rig.do(t, http.MethodPost, "/v1/messages", `{"model":"native-model"}`, nil)
	if status != http.StatusUnauthorized || !strings.Contains(body, `"authentication_error"`) {
		t.Errorf("messages without key: status %d body %s", status, body)
	}
	status, _, body = rig.do(t, http.MethodPost, "/v1/responses", `{"model":"native-model"}`, nil)
	if status != http.StatusUnauthorized || !strings.Contains(body, `"code":"unauthorized"`) {
		t.Errorf("responses without key must keep the standard envelope: status %d body %s", status, body)
	}
}

func TestNative_PIIEngineBlocksPublicDestination(t *testing.T) {
	t.Parallel()
	var hits atomic.Int32
	rig := newNativeRig(t, "anthropic", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	rig.h.PIIEngine = newTestPIIEngine(t)

	status, _, body := rig.do(t, http.MethodPost, "/v1/messages",
		`{"model":"native-model","messages":[{"role":"user","content":"mail me at `+piiTestEmail+`"}]}`,
		map[string]string{"x-api-key": rig.key})
	if status != http.StatusBadRequest || !strings.Contains(body, "not available while PII filtering is enabled for this model") {
		t.Errorf("status = %d, body = %s", status, body)
	}
	if hits.Load() != 0 {
		t.Errorf("upstream was contacted %d times with PII filtering active", hits.Load())
	}
}
