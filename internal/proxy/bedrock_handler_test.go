package proxy

// bedrock_handler_test.go drives the full proxy handler against an httptest
// upstream that speaks the real Bedrock wire formats: binary event-stream
// frames (Content-Type application/vnd.amazon.eventstream) for ConverseStream
// and JSON with X-Amzn-Errortype for errors.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/circuitbreaker"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/cooldown"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/usage"
)

const (
	bhModelID  = "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
	bhModelEsc = "eu.anthropic.claude-sonnet-4-5-20250929-v1%3A0"
	bhAPIKey   = "bedrock-upstream-secret"
)

// lockedBuffer is a goroutine-safe log sink.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// bhRecorded is one request seen by the fake Bedrock upstream.
type bhRecorded struct {
	RequestURI string
	Header     http.Header
	Body       []byte
}

// bhUpstream is a fake Bedrock runtime. handler decides each response.
type bhUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	reqs []bhRecorded
}

func (u *bhUpstream) requests() []bhRecorded {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]bhRecorded(nil), u.reqs...)
}

func newBHUpstream(t *testing.T, h func(w http.ResponseWriter, r *http.Request, body []byte)) *bhUpstream {
	t.Helper()
	u := &bhUpstream{}
	u.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.reqs = append(u.reqs, bhRecorded{RequestURI: r.RequestURI, Header: r.Header.Clone(), Body: body})
		u.mu.Unlock()
		h(w, r, body)
	}))
	t.Cleanup(u.Close)
	return u
}

// bhWriteFrames sends frames as an event-stream response, flushing each one.
func bhWriteFrames(w http.ResponseWriter, frames ...[]byte) {
	w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
	w.Header().Set("X-Amzn-Requestid", "req-123")
	w.WriteHeader(http.StatusOK)
	for _, f := range frames {
		_, _ = w.Write(f)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}
}

func bhFrameStart() []byte { return esEventFrame("messageStart", `{"role":"assistant"}`) }

func bhFrameText(text string) []byte {
	b, _ := json.Marshal(text)
	return esEventFrame("contentBlockDelta", fmt.Sprintf(`{"contentBlockIndex":0,"delta":{"text":%s}}`, b))
}

func bhFrameStop(reason string) []byte {
	return esEventFrame("messageStop", fmt.Sprintf(`{"stopReason":%q}`, reason))
}

func bhFrameMeta(in, out, read, write int) []byte {
	return esEventFrame("metadata", fmt.Sprintf(
		`{"usage":{"inputTokens":%d,"outputTokens":%d,"totalTokens":%d,"cacheReadInputTokens":%d,"cacheWriteInputTokens":%d},"metrics":{"latencyMs":5}}`,
		in, out, in+out, read, write))
}

func bhWriteError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	if errType != "" {
		w.Header().Set("X-Amzn-Errortype", errType)
	}
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `{"message":%q}`, message)
}

const bhOKJSON = `{"output":{"message":{"role":"assistant","content":[{"text":"pong"}]}},"stopReason":"end_turn","usage":{"inputTokens":4,"outputTokens":2,"totalTokens":6}}`

// bhEnv is a fully wired proxy handler for a single bedrock model.
type bhEnv struct {
	h       *ProxyHandler
	baseURL string
	rawKey  string
	db      *db.DB
	logs    *lockedBuffer
}

// newBHEnv serves reg through a real listener with auth, usage logging into a
// real in-memory DB, a threshold-1 breaker and a JSON log capture.
func newBHEnv(t *testing.T, reg *Registry) *bhEnv {
	t.Helper()
	logs := &lockedBuffer{}
	log := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	d := openStage0cDB(t)
	ul := usage.NewLogger(d, config.UsageConfig{BufferSize: 64, FlushInterval: 20 * time.Millisecond}, log, nil)
	ul.Start()
	t.Cleanup(ul.Stop)

	h := NewProxyHandler(reg, log)
	h.UsageLogger = ul
	h.CircuitBreakers = circuitbreaker.NewRegistry(circuitbreaker.Config{
		Enabled: true, Threshold: 1, Timeout: 30 * time.Second, HalfOpenMax: 1,
	})
	h.Cooldowns = cooldown.NewRegistry()

	baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-bedrock"))
	return &bhEnv{h: h, baseURL: baseURL, rawKey: rawKey, db: d, logs: logs}
}

func bhRegistry(t *testing.T, upstreamURL string) *Registry {
	t.Helper()
	reg, err := NewRegistry([]config.ModelConfig{{
		Name:           "claude",
		Provider:       "bedrock",
		BaseURL:        upstreamURL,
		APIKey:         bhAPIKey,
		Aliases:        []string{"sonnet"},
		Region:         "eu-central-1",
		BedrockModelID: bhModelID,
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

type bhResponse struct {
	Status int
	Header http.Header
	Body   string
}

func (e *bhEnv) post(t *testing.T, body string) bhResponse {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, e.baseURL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.rawKey)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return bhResponse{Status: resp.StatusCode, Header: resp.Header, Body: string(b)}
}

type bhUsageRow struct {
	Status, Prompt, Completion, CachedRead, CacheWrite int
	Model                                              string
}

// waitUsage polls for the n-th usage event (1-based, insertion order).
func (e *bhEnv) waitUsage(t *testing.T, n int) bhUsageRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := e.db.SQL().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM usage_events").Scan(&count); err != nil {
			t.Fatalf("count usage: %v", err)
		}
		if count >= n {
			var r bhUsageRow
			row := e.db.SQL().QueryRowContext(context.Background(),
				"SELECT status_code, prompt_tokens, completion_tokens, cached_read_tokens, cache_write_tokens, model_name "+
					"FROM usage_events ORDER BY created_at, rowid LIMIT 1 OFFSET "+fmt.Sprint(n-1))
			if err := row.Scan(&r.Status, &r.Prompt, &r.Completion, &r.CachedRead, &r.CacheWrite, &r.Model); err != nil {
				t.Fatalf("scan usage: %v", err)
			}
			return r
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("usage event %d not logged in time", n)
	return bhUsageRow{}
}

// waitBreaker polls until the breaker for key reaches want.
func (e *bhEnv) waitBreaker(t *testing.T, key string, want circuitbreaker.State) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.h.CircuitBreakers.Get(key).CurrentState() == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("breaker %q state = %v, want %v", key, e.h.CircuitBreakers.Get(key).CurrentState(), want)
}

func bhSSEPayloads(body string) []string {
	var out []string
	for _, seg := range strings.Split(body, "\n\n") {
		seg = strings.TrimSpace(seg)
		if strings.HasPrefix(seg, "data: ") {
			out = append(out, strings.TrimPrefix(seg, "data: "))
		}
	}
	return out
}

func bhStreamText(t *testing.T, body string) string {
	t.Helper()
	var sb strings.Builder
	for _, p := range bhSSEPayloads(body) {
		if p == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(p), &c); err != nil {
			t.Fatalf("payload %q is not JSON: %v", p, err)
		}
		for _, ch := range c.Choices {
			sb.WriteString(ch.Delta.Content)
		}
	}
	return sb.String()
}

func TestBedrockHandler_StreamPlain(t *testing.T) {
	t.Parallel()

	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		bhWriteFrames(w,
			bhFrameStart(), bhFrameText("Hel"), bhFrameText("lo"),
			esEventFrame("contentBlockStop", `{"contentBlockIndex":0}`),
			bhFrameStop("end_turn"), bhFrameMeta(10, 5, 100, 20))
	})
	env := newBHEnv(t, bhRegistry(t, up.URL))

	resp := env.post(t, `{"model":"sonnet","stream":true,"stream_options":{"include_usage":true},"max_tokens":16,`+
		`"messages":[{"role":"system","content":"sys"},{"role":"user","content":"hi"}]}`)

	if resp.Status != http.StatusOK {
		t.Fatalf("status = %d; body %s", resp.Status, resp.Body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("client Content-Type = %q, want text/event-stream", ct)
	}
	if got := bhStreamText(t, resp.Body); got != "Hello" {
		t.Errorf("streamed text = %q, want Hello", got)
	}
	payloads := bhSSEPayloads(resp.Body)
	if payloads[len(payloads)-1] != "[DONE]" {
		t.Errorf("last event = %q, want [DONE]", payloads[len(payloads)-1])
	}
	if strings.Count(resp.Body, "[DONE]") != 1 {
		t.Errorf("[DONE] count != 1:\n%s", resp.Body)
	}
	if !strings.Contains(resp.Body, `"finish_reason":"stop"`) {
		t.Errorf("no finish chunk:\n%s", resp.Body)
	}
	if !strings.Contains(resp.Body, `"prompt_tokens":130`) || !strings.Contains(resp.Body, `"cached_tokens":100`) {
		t.Errorf("usage chunk missing or wrong:\n%s", resp.Body)
	}
	if strings.Contains(resp.Body, "vnd.amazon") || strings.Contains(resp.Body, "messageStart") {
		t.Errorf("raw Bedrock framing leaked to client:\n%s", resp.Body)
	}

	reqs := up.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
	r := reqs[0]
	if r.RequestURI != "/model/"+bhModelEsc+"/converse-stream" {
		t.Errorf("upstream URI = %q", r.RequestURI)
	}
	if r.Header.Get("Authorization") != "Bearer "+bhAPIKey {
		t.Errorf("upstream Authorization = %q", r.Header.Get("Authorization"))
	}
	if r.Header.Get("Accept") != "application/vnd.amazon.eventstream" || r.Header.Get("Content-Type") != "application/json" {
		t.Errorf("upstream headers = %v", r.Header)
	}
	var sent map[string]json.RawMessage
	if err := json.Unmarshal(r.Body, &sent); err != nil {
		t.Fatalf("upstream body: %v", err)
	}
	for k := range sent {
		switch k {
		case "messages", "system", "inferenceConfig", "toolConfig":
		default:
			t.Errorf("upstream body has key %q", k)
		}
	}

	row := env.waitUsage(t, 1)
	if row.Status != 200 || row.Prompt != 130 || row.Completion != 5 || row.CachedRead != 100 || row.CacheWrite != 20 {
		t.Errorf("usage row = %+v", row)
	}
	env.waitBreaker(t, "claude", circuitbreaker.Closed)
}

func TestBedrockHandler_StreamMetadataBeforeStop(t *testing.T) {
	t.Parallel()
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		bhWriteFrames(w, bhFrameStart(), bhFrameText("x"), bhFrameMeta(3, 4, 0, 0), bhFrameStop("max_tokens"))
	})
	env := newBHEnv(t, bhRegistry(t, up.URL))
	resp := env.post(t, `{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.Status != http.StatusOK || !strings.Contains(resp.Body, "data: [DONE]\n\n") {
		t.Fatalf("status %d body %s", resp.Status, resp.Body)
	}
	if !strings.Contains(resp.Body, `"finish_reason":"length"`) {
		t.Errorf("finish reason missing:\n%s", resp.Body)
	}
	if row := env.waitUsage(t, 1); row.Status != 200 || row.Prompt != 3 || row.Completion != 4 {
		t.Errorf("usage row = %+v", row)
	}
}

func TestBedrockHandler_StreamToolCall(t *testing.T) {
	t.Parallel()
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		bhWriteFrames(w, bhFrameStart(),
			esEventFrame("contentBlockStart", `{"contentBlockIndex":1,"start":{"toolUse":{"toolUseId":"tool-1","name":"get_weather"}}}`),
			esEventFrame("contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"{\"city\":"}}}`),
			esEventFrame("contentBlockDelta", `{"contentBlockIndex":1,"delta":{"toolUse":{"input":"\"Berlin\"}"}}}`),
			esEventFrame("contentBlockStop", `{"contentBlockIndex":1}`),
			bhFrameStop("tool_use"), bhFrameMeta(7, 3, 0, 0))
	})
	env := newBHEnv(t, bhRegistry(t, up.URL))
	resp := env.post(t, `{"model":"claude","stream":true,"messages":[{"role":"user","content":"weather?"}],`+
		`"tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object","properties":{"city":{"type":"string"}}}}}]}`)
	if resp.Status != 200 {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	if !strings.Contains(resp.Body, `"id":"tool-1"`) || !strings.Contains(resp.Body, `"name":"get_weather"`) ||
		!strings.Contains(resp.Body, `"finish_reason":"tool_calls"`) {
		t.Errorf("tool call stream wrong:\n%s", resp.Body)
	}
	var args strings.Builder
	for _, p := range bhSSEPayloads(resp.Body) {
		var c struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(p), &c) == nil {
			for _, ch := range c.Choices {
				for _, tc := range ch.Delta.ToolCalls {
					args.WriteString(tc.Function.Arguments)
				}
			}
		}
	}
	if args.String() != `{"city":"Berlin"}` {
		t.Errorf("assembled arguments = %q", args.String())
	}
	// The upstream request must carry the toolConfig.
	if !bytes.Contains(up.requests()[0].Body, []byte(`"toolSpec"`)) {
		t.Errorf("upstream body lacks toolConfig: %s", up.requests()[0].Body)
	}
}

func TestBedrockHandler_NonStreamingEndToEnd(t *testing.T) {
	t.Parallel()
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":{"message":{"role":"assistant","content":[{"text":"pong"}]}},"stopReason":"end_turn",`+
			`"usage":{"inputTokens":4,"outputTokens":2,"totalTokens":6,"cacheReadInputTokens":10,"cacheWriteInputTokens":1}}`)
	})
	env := newBHEnv(t, bhRegistry(t, up.URL))
	resp := env.post(t, `{"model":"sonnet","messages":[{"role":"user","content":"ping"}]}`)

	if resp.Status != 200 {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	var out struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			Prompt int `json:"prompt_tokens"`
			Total  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(resp.Body), &out); err != nil {
		t.Fatalf("response not JSON: %v\n%s", err, resp.Body)
	}
	if out.Object != "chat.completion" || out.Choices[0].Message.Content != "pong" || out.Choices[0].FinishReason != "stop" {
		t.Errorf("response = %+v", out)
	}
	if out.Model != "claude" {
		t.Errorf("model = %q, want the canonical model name", out.Model)
	}
	if !strings.HasPrefix(out.ID, "chatcmpl-") || out.Usage.Prompt != 15 || out.Usage.Total != 17 {
		t.Errorf("response envelope = %+v", out)
	}
	reqs := up.requests()
	if reqs[0].RequestURI != "/model/"+bhModelEsc+"/converse" {
		t.Errorf("upstream URI = %q", reqs[0].RequestURI)
	}
	if reqs[0].Header.Get("Accept") != "application/json" {
		t.Errorf("Accept = %q", reqs[0].Header.Get("Accept"))
	}
	row := env.waitUsage(t, 1)
	if row.Status != 200 || row.Prompt != 15 || row.Completion != 2 || row.CachedRead != 10 || row.CacheWrite != 1 {
		t.Errorf("usage row = %+v", row)
	}
	env.waitBreaker(t, "claude", circuitbreaker.Closed)
}

func TestBedrockHandler_ClientRequestErrorNeverReachesUpstream(t *testing.T) {
	t.Parallel()
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) { bhWriteFrames(w) })
	env := newBHEnv(t, bhRegistry(t, up.URL))

	tests := []struct{ name, body, wantMsg string }{
		{"image part", `{"model":"claude","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, "only text content parts are supported for this model"},
		{"top-level system", `{"model":"claude","system":"x","messages":[{"role":"user","content":"a"}]}`, `top-level system is not supported; send a message with role "system" instead`},
		{"only system", `{"model":"claude","messages":[{"role":"system","content":"a"}]}`, "messages must contain a user or assistant message"},
		{"bad tool name", `{"model":"claude","messages":[{"role":"user","content":"a"}],"tools":[{"type":"function","function":{"name":"bad name"}}]}`, "tool function name is invalid"},
	}
	for _, tc := range tests {
		resp := env.post(t, tc.body)
		if resp.Status != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400; body %s", tc.name, resp.Status, resp.Body)
		}
		var eb struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(resp.Body), &eb); err != nil || eb.Error.Message != tc.wantMsg {
			t.Errorf("%s: message = %q (err %v), want %q", tc.name, eb.Error.Message, err, tc.wantMsg)
		}
	}
	if n := len(up.requests()); n != 0 {
		t.Errorf("upstream received %d requests, want 0", n)
	}
}

func TestBedrockHandler_ErrorMapping(t *testing.T) {
	t.Parallel()

	const acct = "123456789012"
	tests := []struct {
		name        string
		status      int
		errType     string
		message     string
		stream      bool
		wantCode    string
		wantMessage string // exact
	}{
		{"validation passes message", 400, "ValidationException", "The maximum tokens you requested exceeds the model limit", false, "invalid_request", "Bedrock rejected the request as invalid"},
		{"validation on stream request", 400, "ValidationException", "bad input", true, "invalid_request", "Bedrock rejected the request as invalid"},
		{"access denied static", 403, "AccessDeniedException", "User: arn:aws:iam::" + acct + ":user/alice is not authorized", false, "permission_denied", "Bedrock API key is not authorized for this model"},
		{"access denied on stream request", 403, "AccessDeniedException", "arn:aws:iam::" + acct + ":role/x", true, "permission_denied", "Bedrock API key is not authorized for this model"},
		{"unrecognized client", 403, "UnrecognizedClientException", "token invalid " + acct, false, "invalid_api_key", "upstream rejected the Bedrock API key"},
		{"401 without type", 401, "", "nope " + acct, false, "invalid_api_key", "upstream rejected the Bedrock API key"},
		{"expired", 400, "ExpiredTokenException", "expired " + acct, false, "expired_api_key", "Bedrock API key has expired"},
		{"not found", 404, "ResourceNotFoundException", "arn " + acct, false, "model_not_found", "Bedrock model or inference profile not found"},
		{"throttling", 429, "ThrottlingException", "Too many requests " + acct, false, "rate_limit_exceeded", "Bedrock rate limit exceeded"},
		{"quota", 400, "ServiceQuotaExceededException", "quota " + acct, false, "quota_exceeded", "Bedrock service quota exceeded"},
		{"internal", 500, "InternalServerException", "boom " + acct, false, "upstream_error", "Bedrock internal error"},
		{"unavailable", 503, "ServiceUnavailableException", "down " + acct, false, "upstream_unavailable", "Bedrock service unavailable"},
		{"unknown 418", 418, "", "teapot " + acct, false, "upstream_error", "Bedrock returned an error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				bhWriteError(w, tc.status, tc.errType, tc.message)
			})
			env := newBHEnv(t, bhRegistry(t, up.URL))
			stream := ""
			if tc.stream {
				stream = `"stream":true,`
			}
			resp := env.post(t, `{"model":"claude",`+stream+`"messages":[{"role":"user","content":"hi"}]}`)

			if resp.Status != tc.status {
				t.Errorf("status = %d, want the unchanged upstream status %d", resp.Status, tc.status)
			}
			if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var body struct {
				Error struct {
					Code      string `json:"code"`
					Message   string `json:"message"`
					RequestID string `json:"request_id"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
				t.Fatalf("body is not apierror JSON: %v\n%s", err, resp.Body)
			}
			if body.Error.Code != tc.wantCode || body.Error.Message != tc.wantMessage {
				t.Errorf("error = %+v, want code %q message %q", body.Error, tc.wantCode, tc.wantMessage)
			}
			if strings.Contains(resp.Body, acct) {
				t.Errorf("response leaks account id: %s", resp.Body)
			}
			if strings.Contains(resp.Body, `"message":"{`) || strings.Contains(resp.Body, "Errortype") {
				t.Errorf("raw upstream body forwarded: %s", resp.Body)
			}
		})
	}
}

func TestBedrockHandler_ErrorMapping_RequestIDPropagated(t *testing.T) {
	t.Parallel()
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		bhWriteError(w, 429, "ThrottlingException", "x")
	})
	env := newBHEnv(t, bhRegistry(t, up.URL))
	resp := env.post(t, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	var raw map[string]map[string]any
	if err := json.Unmarshal([]byte(resp.Body), &raw); err != nil {
		t.Fatal(err)
	}
	for k := range raw["error"] {
		switch k {
		case "code", "message", "request_id":
		default:
			t.Errorf("unexpected error field %q", k)
		}
	}
	// A 429 must be breaker neutral and mark the deployment cooling.
	env.waitBreaker(t, "claude", circuitbreaker.Closed)
	if !env.h.Cooldowns.Cooling("claude") {
		t.Error("deployment not cooling after 429")
	}
}

func TestBedrockHandler_5xxTripsBreaker(t *testing.T) {
	t.Parallel()
	var status atomic.Int32
	status.Store(500)
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		bhWriteError(w, int(status.Load()), "InternalServerException", "x")
	})
	env := newBHEnv(t, bhRegistry(t, up.URL))
	env.post(t, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	env.waitBreaker(t, "claude", circuitbreaker.Open)
}

func TestBedrockHandler_429FailoverAcrossDeployments(t *testing.T) {
	t.Parallel()

	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			up1 := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				bhWriteError(w, 429, "ThrottlingException", "slow down")
			})
			up2 := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				if strings.HasSuffix(r.URL.Path, "/converse-stream") {
					bhWriteFrames(w, bhFrameStart(), bhFrameText("from-two"), bhFrameStop("end_turn"), bhFrameMeta(1, 1, 0, 0))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, strings.Replace(bhOKJSON, "pong", "from-two", 1))
			})
			dep1 := Deployment{Name: "d1", Provider: "bedrock", BaseURL: up1.URL, APIKey: "k1", Region: "eu-central-1", BedrockModelID: "eu.model-one-v1:0", Priority: 1}
			dep2 := Deployment{Name: "d2", Provider: "bedrock", BaseURL: up2.URL, APIKey: "k2", Region: "us-east-1", BedrockModelID: "us.model-two-v1:0", Priority: 2}
			reg := twoDeploymentRegistry("claude", dep1, dep2)

			env := newBHEnv(t, reg)
			env.h.Router = &staticDeploymentPicker{deps: []Deployment{dep1, dep2}}

			s := ""
			if stream {
				s = `"stream":true,`
			}
			resp := env.post(t, `{"model":"claude",`+s+`"messages":[{"role":"user","content":"hi"}]}`)
			if resp.Status != 200 || !strings.Contains(resp.Body, "from-two") {
				t.Fatalf("status %d body %s", resp.Status, resp.Body)
			}
			r1, r2 := up1.requests(), up2.requests()
			if len(r1) != 1 || len(r2) != 1 {
				t.Fatalf("requests = %d / %d, want 1 / 1", len(r1), len(r2))
			}
			if !strings.HasPrefix(r1[0].RequestURI, "/model/eu.model-one-v1%3A0/converse") {
				t.Errorf("dep1 URI = %q (deployment BedrockModelID not applied)", r1[0].RequestURI)
			}
			if !strings.HasPrefix(r2[0].RequestURI, "/model/us.model-two-v1%3A0/converse") {
				t.Errorf("dep2 URI = %q (deployment BedrockModelID not applied)", r2[0].RequestURI)
			}
			if r1[0].Header.Get("Authorization") != "Bearer k1" || r2[0].Header.Get("Authorization") != "Bearer k2" {
				t.Errorf("per-deployment keys not applied: %q %q", r1[0].Header.Get("Authorization"), r2[0].Header.Get("Authorization"))
			}
			if !env.h.Cooldowns.Cooling(deploymentKey("claude", "d1")) {
				t.Error("dep1 must be cooling after 429")
			}
			if env.h.Cooldowns.Cooling(deploymentKey("claude", "d2")) {
				t.Error("dep2 must not be cooling")
			}
			env.waitBreaker(t, deploymentKey("claude", "d1"), circuitbreaker.Closed)
		})
	}
}

func TestBedrockHandler_StreamTruncated(t *testing.T) {
	t.Parallel()
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		// Clean connection end after content but before messageStop/metadata.
		bhWriteFrames(w, bhFrameStart(), bhFrameText("partial"))
	})
	env := newBHEnv(t, bhRegistry(t, up.URL))
	resp := env.post(t, `{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if resp.Status != 200 {
		t.Fatalf("status = %d", resp.Status)
	}
	if !strings.Contains(resp.Body, "partial") {
		t.Errorf("delivered content lost:\n%s", resp.Body)
	}
	if !strings.Contains(resp.Body, `"code":"stream_incomplete"`) {
		t.Errorf("no stream_incomplete abort event:\n%s", resp.Body)
	}
	if strings.Contains(resp.Body, "[DONE]") {
		t.Errorf("[DONE] must not be sent for a truncated stream:\n%s", resp.Body)
	}
	if row := env.waitUsage(t, 1); row.Status != http.StatusBadGateway {
		t.Errorf("usage status = %d, want 502", row.Status)
	}
	env.waitBreaker(t, "claude", circuitbreaker.Open)
}

func TestBedrockHandler_StreamTruncatedMidFrame(t *testing.T) {
	t.Parallel()
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		f := bhFrameText("never complete")
		bhWriteFrames(w, bhFrameStart(), f[:len(f)-6])
	})
	env := newBHEnv(t, bhRegistry(t, up.URL))
	resp := env.post(t, `{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if !strings.Contains(resp.Body, `"code":"stream_incomplete"`) || strings.Contains(resp.Body, "never complete") || strings.Contains(resp.Body, "[DONE]") {
		t.Errorf("unexpected body:\n%s", resp.Body)
	}
	if row := env.waitUsage(t, 1); row.Status != http.StatusBadGateway {
		t.Errorf("usage status = %d, want 502", row.Status)
	}
}

func TestBedrockHandler_StreamCorruptCRC(t *testing.T) {
	t.Parallel()
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		bad := bhFrameText("tampered")
		bad[len(bad)-1] ^= 0xff
		bhWriteFrames(w, bhFrameStart(), bhFrameText("good"), bad, bhFrameStop("end_turn"), bhFrameMeta(1, 1, 0, 0))
	})
	env := newBHEnv(t, bhRegistry(t, up.URL))
	resp := env.post(t, `{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if !strings.Contains(resp.Body, "good") {
		t.Errorf("frames before the corruption must be delivered:\n%s", resp.Body)
	}
	if strings.Contains(resp.Body, "tampered") {
		t.Errorf("corrupt frame content delivered:\n%s", resp.Body)
	}
	if !strings.Contains(resp.Body, `"error"`) || strings.Contains(resp.Body, "[DONE]") {
		t.Errorf("expected abort event and no [DONE]:\n%s", resp.Body)
	}
	if row := env.waitUsage(t, 1); row.Status != http.StatusBadGateway {
		t.Errorf("usage status = %d, want 502", row.Status)
	}
	env.waitBreaker(t, "claude", circuitbreaker.Open)
}

func TestBedrockHandler_StreamInStreamException(t *testing.T) {
	t.Parallel()
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		h := append(esStringHeader(":message-type", "exception"), esStringHeader(":exception-type", "throttlingException")...)
		bhWriteFrames(w, bhFrameStart(), bhFrameText("before"), encodeESFrame(h, []byte(`{"message":"EXCEPTION-PAYLOAD-SECRET"}`)))
	})
	env := newBHEnv(t, bhRegistry(t, up.URL))
	resp := env.post(t, `{"model":"claude","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	if !strings.Contains(resp.Body, "before") || !strings.Contains(resp.Body, `"code":"stream_transform_aborted"`) {
		t.Errorf("unexpected body:\n%s", resp.Body)
	}
	if strings.Contains(resp.Body, "EXCEPTION-PAYLOAD-SECRET") || strings.Contains(resp.Body, "[DONE]") {
		t.Errorf("exception payload leaked or [DONE] sent:\n%s", resp.Body)
	}
	if row := env.waitUsage(t, 1); row.Status != http.StatusBadGateway {
		t.Errorf("usage status = %d, want 502", row.Status)
	}
	env.waitBreaker(t, "claude", circuitbreaker.Open)
}

func TestBedrockHandler_StreamPII_RestoresSplitPseudonym(t *testing.T) {
	t.Parallel()

	engine := newTestPIIEngine(t)
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		pseudo := piiPseudonymPattern.FindString(string(body))
		if pseudo == "" {
			bhWriteFrames(w, bhFrameStart(), bhFrameText("NO-PSEUDONYM-IN-REQUEST"), bhFrameStop("end_turn"), bhFrameMeta(1, 1, 0, 0))
			return
		}
		mid := len(pseudo) / 2
		bhWriteFrames(w, bhFrameStart(), bhFrameText("Your email: "), bhFrameText(pseudo[:mid]), bhFrameText(pseudo[mid:]),
			bhFrameText(" ok"), bhFrameStop("end_turn"), bhFrameMeta(8, 4, 0, 0))
	})

	// destPrivate defaults to false, so the network default anonymizes.
	m := &Model{
		Name: "claude", Provider: "bedrock", Type: "chat", BaseURL: up.URL, APIKey: bhAPIKey,
		Region: "eu-central-1", BedrockModelID: bhModelID,
	}
	reg := &Registry{models: map[string]*Model{"claude": m}, aliases: map[string]string{}}
	env := newBHEnv(t, reg)
	env.h.PIIEngine = engine

	resp := env.post(t, fmt.Sprintf(`{"model":"claude","stream":true,"messages":[{"role":"user","content":"send to %s"}]}`, piiTestEmail))
	if resp.Status != 200 {
		t.Fatalf("status %d: %s", resp.Status, resp.Body)
	}
	got := bhStreamText(t, resp.Body)
	if got != "Your email: "+piiTestEmail+" ok" {
		t.Errorf("restored text = %q", got)
	}
	if strings.Contains(resp.Body, "PII_EM_") {
		t.Errorf("pseudonym leaked to client:\n%s", resp.Body)
	}
	reqs := up.requests()
	if len(reqs) != 1 {
		t.Fatalf("upstream requests = %d", len(reqs))
	}
	if bytes.Contains(reqs[0].Body, []byte(piiTestEmail)) {
		t.Errorf("real email reached the upstream: %s", reqs[0].Body)
	}
	if !piiPseudonymPattern.Match(reqs[0].Body) {
		t.Errorf("upstream body carries no pseudonym: %s", reqs[0].Body)
	}
	if !strings.Contains(resp.Body, "data: [DONE]") {
		t.Errorf("no [DONE]:\n%s", resp.Body)
	}
	if row := env.waitUsage(t, 1); row.Status != 200 || row.Prompt != 8 || row.Completion != 4 {
		t.Errorf("usage row = %+v", row)
	}
}

func TestBedrockHandler_NonStreamingPII_RestoresPseudonym(t *testing.T) {
	t.Parallel()
	engine := newTestPIIEngine(t)
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, body []byte) {
		pseudo := piiPseudonymPattern.FindString(string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"output":{"message":{"content":[{"text":"mail %s"}]}},"stopReason":"end_turn","usage":{"inputTokens":1,"outputTokens":1}}`, pseudo)
	})
	m := &Model{Name: "claude", Provider: "bedrock", Type: "chat", BaseURL: up.URL, Region: "eu-central-1", BedrockModelID: bhModelID}
	env := newBHEnv(t, &Registry{models: map[string]*Model{"claude": m}, aliases: map[string]string{}})
	env.h.PIIEngine = engine

	resp := env.post(t, fmt.Sprintf(`{"model":"claude","messages":[{"role":"user","content":"send to %s"}]}`, piiTestEmail))
	if resp.Status != 200 || !strings.Contains(resp.Body, "mail "+piiTestEmail) || strings.Contains(resp.Body, "PII_EM_") {
		t.Errorf("status %d body %s", resp.Status, resp.Body)
	}
}

func TestBedrockHandler_NonBedrockAdapterEventstreamNotStreaming(t *testing.T) {
	t.Parallel()

	frame := bhFrameText("raw-binary")
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			t.Parallel()
			up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
				_, _ = w.Write(frame)
			})
			reg, err := NewRegistry([]config.ModelConfig{{Name: "plain", Provider: "openai", BaseURL: up.URL, APIKey: "k"}})
			if err != nil {
				t.Fatal(err)
			}
			env := newBHEnv(t, reg)
			s := ""
			if stream {
				s = `"stream":true,`
			}
			resp := env.post(t, `{"model":"plain",`+s+`"messages":[{"role":"user","content":"hi"}]}`)
			if resp.Status != 200 {
				t.Fatalf("status = %d", resp.Status)
			}
			if resp.Body != string(frame) {
				t.Errorf("body was transformed for a non-bedrock adapter:\n got %q\nwant %q", resp.Body, frame)
			}
			if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
				t.Errorf("Content-Type = %q, eventstream must not be treated as SSE", ct)
			}
		})
	}
}

func TestBedrockHandler_MalformedSuccessResponse(t *testing.T) {
	t.Parallel()
	const secret = "TOOL-SECRET-OUTPUT"
	tests := []struct {
		name string
		body string
	}{
		{"not json", `<html>` + secret + `</html>`},
		{"bad tool id", `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"bad id ` + secret + `","name":"f","input":{}}}]}}}`},
		{"non-object input", `{"output":{"message":{"content":[{"toolUse":{"toolUseId":"a","name":"f","input":"` + secret + `"}}]}}}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			})
			env := newBHEnv(t, bhRegistry(t, up.URL))
			resp := env.post(t, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
			if resp.Status < 500 {
				t.Errorf("status = %d, want a 5xx for an untranslatable response", resp.Status)
			}
			if strings.Contains(resp.Body, secret) || strings.Contains(env.logs.String(), secret) {
				t.Errorf("upstream content leaked (body %q)", resp.Body)
			}
		})
	}
}

// TestBedrockHandler_NoContentInLogs runs every request/response/error path
// with unique marker strings in prompts, responses, upstream error texts and
// exception payloads, then asserts none of them appears in any log line.
func TestBedrockHandler_NoContentInLogs(t *testing.T) {
	t.Parallel()

	const (
		promptSecret   = "PROMPT-SECRET-7f3a"
		responseSecret = "RESPONSE-SECRET-91bc"
		errorSecret    = "UPSTREAM-ERROR-TEXT-55de"
		excSecret      = "EXCEPTION-PAYLOAD-0a1b"
		acct           = "210987654321"
	)

	var mode atomic.Value
	mode.Store("stream-ok")
	up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
		switch mode.Load().(string) {
		case "stream-ok":
			bhWriteFrames(w, bhFrameStart(), bhFrameText(responseSecret), bhFrameStop("end_turn"), bhFrameMeta(2, 2, 0, 0))
		case "json-ok":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, strings.Replace(bhOKJSON, "pong", responseSecret, 1))
		case "validation":
			bhWriteError(w, 400, "ValidationException", errorSecret+" arn:aws:iam::"+acct+":user/x")
		case "denied":
			bhWriteError(w, 403, "AccessDeniedException", errorSecret+" "+acct)
		case "throttle":
			bhWriteError(w, 429, "ThrottlingException", errorSecret)
		case "html500":
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(500)
			_, _ = io.WriteString(w, "<html>"+errorSecret+"</html>")
		case "exception":
			h := append(esStringHeader(":message-type", "exception"), esStringHeader(":exception-type", "modelStreamErrorException")...)
			h = append(h, esStringHeader(":error-message", errorSecret)...)
			bhWriteFrames(w, bhFrameStart(), bhFrameText(responseSecret), encodeESFrame(h, []byte(`{"message":"`+excSecret+`","originalMessage":"`+promptSecret+`"}`)))
		case "error-frame":
			h := append(esStringHeader(":message-type", "error"), esStringHeader(":error-code", "InternalFailure")...)
			h = append(h, esStringHeader(":error-message", errorSecret)...)
			bhWriteFrames(w, bhFrameStart(), encodeESFrame(h, []byte(excSecret)))
		case "corrupt":
			bad := bhFrameText(responseSecret)
			bad[len(bad)-2] ^= 0xff
			bhWriteFrames(w, bhFrameStart(), bad)
		case "bad-json-payload":
			bhWriteFrames(w, bhFrameStart(), esEventFrame("contentBlockDelta", `{"delta":`+responseSecret))
		case "bad-tool":
			bhWriteFrames(w, bhFrameStart(),
				esEventFrame("contentBlockStart", `{"contentBlockIndex":0,"start":{"toolUse":{"toolUseId":"bad id `+responseSecret+`","name":"f"}}}`))
		case "truncated":
			f := bhFrameText(responseSecret)
			bhWriteFrames(w, bhFrameStart(), f[:len(f)/2])
		case "garbage":
			w.Header().Set("Content-Type", "application/vnd.amazon.eventstream")
			w.WriteHeader(200)
			_, _ = io.WriteString(w, promptSecret+responseSecret+errorSecret+" this is not an event stream at all")
		}
	})
	env := newBHEnv(t, bhRegistry(t, up.URL))

	body := func(stream bool) string {
		s := ""
		if stream {
			s = `"stream":true,"stream_options":{"include_usage":true},`
		}
		return `{"model":"claude",` + s + `"messages":[{"role":"system","content":"` + promptSecret + `"},{"role":"user","content":"` + promptSecret + `"}]}`
	}

	type step struct {
		mode   string
		stream bool
	}
	steps := []step{
		{"stream-ok", true}, {"json-ok", false}, {"validation", false}, {"validation", true},
		{"denied", false}, {"throttle", false}, {"html500", false}, {"exception", true},
		{"error-frame", true}, {"corrupt", true}, {"bad-json-payload", true}, {"bad-tool", true},
		{"truncated", true}, {"garbage", true},
	}
	for _, s := range steps {
		mode.Store(s.mode)
		// Reset the breaker so a prior failure does not short-circuit the next step.
		env.h.CircuitBreakers = circuitbreaker.NewRegistry(circuitbreaker.Config{Enabled: true, Threshold: 100, Timeout: time.Second, HalfOpenMax: 1})
		env.h.Cooldowns = cooldown.NewRegistry()
		resp := env.post(t, body(s.stream))
		if s.mode == "denied" || s.mode == "throttle" || s.mode == "html500" || s.mode == "exception" ||
			s.mode == "error-frame" || s.mode == "corrupt" || s.mode == "bad-json-payload" || s.mode == "bad-tool" ||
			s.mode == "truncated" || s.mode == "garbage" {
			for _, leak := range []string{errorSecret, excSecret, promptSecret, acct} {
				if strings.Contains(resp.Body, leak) {
					t.Errorf("%s: client response leaks %q:\n%s", s.mode, leak, resp.Body)
				}
			}
		}
	}
	// A request the adapter rejects: its text must not be echoed or logged.
	resp := env.post(t, `{"model":"claude","messages":[{"role":"user","content":[{"type":"`+promptSecret+`","text":"`+promptSecret+`"}]}]}`)
	if resp.Status != 400 || strings.Contains(resp.Body, promptSecret) {
		t.Errorf("rejected request: status %d body %s", resp.Status, resp.Body)
	}

	// Drain usage logging so every asynchronous log line has been written.
	env.waitUsage(t, 1)
	time.Sleep(300 * time.Millisecond)

	logs := env.logs.String()
	if logs == "" {
		t.Fatal("log capture is empty; the test would prove nothing")
	}
	t.Logf("captured %d log lines", strings.Count(logs, "\n"))
	for _, leak := range []string{promptSecret, responseSecret, errorSecret, excSecret, acct} {
		if strings.Contains(logs, leak) {
			idx := strings.Index(logs, leak)
			start := max(0, idx-200)
			t.Errorf("logs contain %q:\n...%s...", leak, logs[start:min(len(logs), idx+200)])
		}
	}
}

func TestBedrockHandler_ErrorMapping_RealAWSShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		status      int
		errType     string
		body        string
		wantCode    string
		wantMessage string
		leak        string
	}{
		{"invalid bearer key", 403, "AccessDeniedException:http://internal.amazon.com/coral/com.amazon.coral.service/", `{"Message":"Authentication failed: Please make sure your API Key is valid."}`, "invalid_api_key", "upstream rejected the Bedrock API key", "Authentication failed"},
		{"no model access", 400, "ValidationException:http://internal.amazon.com/coral/com.amazon.bedrock/", `{"message":"Error 002: Access to Bedrock models is not allowed for this account"}`, "permission_denied", "Bedrock model access is not enabled for this AWS account", "Error 002"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			up := newBHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ []byte) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("X-Amzn-Errortype", tc.errType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			})
			env := newBHEnv(t, bhRegistry(t, up.URL))
			resp := env.post(t, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)

			if resp.Status != tc.status {
				t.Errorf("status = %d, want %d", resp.Status, tc.status)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(resp.Body), &body); err != nil {
				t.Fatalf("body is not apierror JSON: %v\n%s", err, resp.Body)
			}
			if body.Error.Code != tc.wantCode || body.Error.Message != tc.wantMessage {
				t.Errorf("error = %+v, want code %q message %q", body.Error, tc.wantCode, tc.wantMessage)
			}
			if strings.Contains(resp.Body, tc.leak) || strings.Contains(resp.Body, "amazon.com") {
				t.Errorf("response leaks upstream text: %s", resp.Body)
			}
		})
	}
}
