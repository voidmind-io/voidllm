package proxy_test

// native_handler_test.go exercises the native /v1/messages and /v1/responses
// endpoints end to end through the real auth middleware, the real
// deployment Router, circuit breakers, rate limiter, token counter, usage
// logger and an in-memory SQLite database. Upstream providers are httptest
// servers. Every environment also captures all log output and fails the test
// at cleanup if prompt or response content, the upstream secret, or the
// client key ever appeared in it.

import (
	"bytes"
	"context"
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

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/circuitbreaker"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/cooldown"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/proxy"
	"github.com/voidmind-io/voidllm/internal/ratelimit"
	"github.com/voidmind-io/voidllm/internal/router"
	"github.com/voidmind-io/voidllm/internal/usage"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

const (
	promptMarker   = "PROMPT-MARKER-4711"
	replyMarker    = "REPLY-MARKER-0815"
	errorMarker    = "ERRBODY-MARKER-2342"
	upstreamSecret = "upstream-secret-native-handler"
)

var nhHMACSecret = []byte("native-handler-hmac-secret-0123456789")

// lockedBuf is a concurrency-safe log sink.
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// nhOpts configures newNHEnv.
type nhOpts struct {
	models        []config.ModelConfig
	mutateKey     func(*auth.KeyInfo)
	breaker       *circuitbreaker.Config
	useRouter     bool
	fallbackDepth int
	rateLimiter   bool
	tokenCounter  bool
	access        func(keyID string) *proxy.ModelAccessCache
}

type nhEnv struct {
	app     *fiber.App
	h       *proxy.ProxyHandler
	d       *db.DB
	tc      *ratelimit.TokenCounter
	logs    *lockedBuf
	key     string
	info    auth.KeyInfo
	keys    *cache.Cache[string, auth.KeyInfo]
	breaker *circuitbreaker.Registry
}

type nhRow struct {
	keyID, model, requested string
	status                  int
	prompt, completion      int
	total                   int
}

func newNHEnv(t *testing.T, o nhOpts) *nhEnv {
	t.Helper()

	logs := &lockedBuf{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	d, err := db.Open(context.Background(), config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             fmt.Sprintf("file:nativehandler_%d?mode=memory&cache=private", time.Now().UnixNano()),
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.RunMigrations(context.Background(), d.SQL(), db.SQLiteDialect{}, log); err != nil {
		t.Fatalf("db.RunMigrations: %v", err)
	}

	var tc *ratelimit.TokenCounter
	if o.tokenCounter {
		tc = ratelimit.NewTokenCounter()
	}
	ul := usage.NewLogger(d, config.UsageConfig{BufferSize: 64, FlushInterval: 20 * time.Millisecond}, log, tc)
	ul.Start()

	reg, err := proxy.NewRegistry(o.models)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	h := proxy.NewProxyHandler(reg, log)
	h.UsageLogger = ul
	h.TokenCounter = tc
	h.FallbackMaxDepth = o.fallbackDepth
	if o.rateLimiter {
		h.RateLimiter = ratelimit.NewRateLimiter()
	}
	var cbr *circuitbreaker.Registry
	if o.breaker != nil {
		cbr = circuitbreaker.NewRegistry(*o.breaker)
		h.CircuitBreakers = cbr
	}
	if o.useRouter {
		h.Router = router.NewRouter(nil, cbr, cooldown.NewRegistry())
	}

	keys := cache.New[string, auth.KeyInfo]()
	env := &nhEnv{h: h, d: d, tc: tc, logs: logs, keys: keys, breaker: cbr}
	env.key, env.info = env.addKey(t, "nh-key", o.mutateKey)
	if o.access != nil {
		h.AccessCache = o.access(env.info.ID)
	}

	app := fiber.New()
	app.Use(auth.Middleware(keys, nhHMACSecret))
	app.All("/v1/*", h.Handle)
	env.app = app

	// Registered last so it runs first (LIFO): stop the logger so nothing is
	// still flushing, then verify the log capture is free of content.
	t.Cleanup(func() {
		out := logs.String()
		for _, forbidden := range []string{promptMarker, replyMarker, errorMarker, upstreamSecret, env.key} {
			if strings.Contains(out, forbidden) {
				t.Errorf("logs contain forbidden content %q:\n%s", forbidden, out)
			}
		}
	})
	t.Cleanup(ul.Stop)
	return env
}

// addKey registers another user key and returns its raw value and info.
func (e *nhEnv) addKey(t *testing.T, id string, mutate func(*auth.KeyInfo)) (string, auth.KeyInfo) {
	t.Helper()
	raw, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("keygen.Generate: %v", err)
	}
	info := auth.KeyInfo{ID: id, KeyType: keygen.KeyTypeUser, OrgID: "nh-org", Role: auth.RoleMember}
	if mutate != nil {
		mutate(&info)
	}
	e.keys.Set(keygen.Hash(raw, nhHMACSecret), info)
	return raw, info
}

func (e *nhEnv) scopes() ratelimit.Scopes {
	return ratelimit.Scopes{KeyID: e.info.ID, OrgID: e.info.OrgID}
}

func (e *nhEnv) do(t *testing.T, path, body string, hdr map[string]string) (int, http.Header, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := e.app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
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

// post sends a request authenticated with the env key via x-api-key for
// messages paths and Bearer otherwise.
func (e *nhEnv) post(t *testing.T, path, body string) (int, string) {
	t.Helper()
	hdr := map[string]string{"Authorization": "Bearer " + e.key}
	if strings.HasPrefix(path, "/v1/messages") {
		hdr = map[string]string{"x-api-key": e.key}
	}
	status, _, out := e.do(t, path, body, hdr)
	return status, out
}

func (e *nhEnv) queryRows(t *testing.T) []nhRow {
	t.Helper()
	q, err := e.d.SQL().QueryContext(context.Background(),
		"SELECT key_id, model_name, COALESCE(requested_model_name,''), status_code, prompt_tokens, completion_tokens, total_tokens FROM usage_events ORDER BY rowid")
	if err != nil {
		t.Fatalf("query usage_events: %v", err)
	}
	defer q.Close()
	var out []nhRow
	for q.Next() {
		var r nhRow
		if err := q.Scan(&r.keyID, &r.model, &r.requested, &r.status, &r.prompt, &r.completion, &r.total); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := q.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// waitRows blocks until at least n usage rows exist and returns all rows.
func (e *nhEnv) waitRows(t *testing.T, n int) []nhRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rows := e.queryRows(t)
		if len(rows) >= n {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage rows = %d, want at least %d", len(rows), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ── upstream helpers ──────────────────────────────────────────────────────

type nhUpstream struct {
	srv  *httptest.Server
	hits atomic.Int32
	mu   sync.Mutex
	last []capturedReq
}

type capturedReq struct {
	Path   string
	Header http.Header
	Body   string
}

func (u *nhUpstream) lastReq() capturedReq {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.last) == 0 {
		return capturedReq{}
	}
	return u.last[len(u.last)-1]
}

// newNHUpstream starts a server whose handler receives the 0-based hit index.
func newNHUpstream(t *testing.T, fn func(w http.ResponseWriter, r *http.Request, hit int)) *nhUpstream {
	t.Helper()
	u := &nhUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.last = append(u.last, capturedReq{Path: r.URL.Path, Header: r.Header.Clone(), Body: string(b)})
		u.mu.Unlock()
		fn(w, r, int(u.hits.Add(1))-1)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func jsonUpstream(t *testing.T, status int, body string) *nhUpstream {
	t.Helper()
	return newNHUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	})
}

// seqStreamUpstream serves events[i] as an SSE stream on the i-th hit (the last
// entry repeats).
func seqStreamUpstream(t *testing.T, events ...string) *nhUpstream {
	t.Helper()
	return newNHUpstream(t, func(w http.ResponseWriter, _ *http.Request, hit int) {
		if hit >= len(events) {
			hit = len(events) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, events[hit])
	})
}

func messagesJSON(in, out int) string {
	return fmt.Sprintf(`{"id":"msg_1","type":"message","content":[{"type":"text","text":%q}],"usage":{"input_tokens":%d,"output_tokens":%d}}`, replyMarker, in, out)
}

func responsesJSON(in, out int) string {
	return fmt.Sprintf(`{"id":"resp_1","status":"completed","output_text":%q,"usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d}}`, replyMarker, in, out, in+out)
}

func messagesBody(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"stream":%v,"max_tokens":16,"messages":[{"role":"user","content":%q}]}`, model, stream, promptMarker)
}

func responsesBody(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"stream":%v,"input":%q}`, model, stream, promptMarker)
}

func anthModel(name, url string) config.ModelConfig {
	return config.ModelConfig{Name: name, Provider: "anthropic", BaseURL: url, APIKey: upstreamSecret}
}

func oaiModel(name, provider, url string) config.ModelConfig {
	return config.ModelConfig{Name: name, Provider: provider, BaseURL: url, APIKey: upstreamSecret}
}

// SSE fixtures.
var (
	msgStart       = "event: message_start\n" + `data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":7,"output_tokens":1}}}` + "\n\n"
	msgDelta       = "event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + replyMarker + `"}}` + "\n\n"
	msgStop        = "event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}` + "\n\n" + "event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"
	msgsClean      = msgStart + msgDelta + msgStop
	msgsTruncated  = msgStart + msgDelta
	respDelta      = "event: response.output_text.delta\n" + `data: {"type":"response.output_text.delta","delta":"` + replyMarker + `"}` + "\n\n"
	respCompleted  = "event: response.completed\n" + `data: {"type":"response.completed","response":{"usage":{"input_tokens":7,"output_tokens":9,"total_tokens":16}}}` + "\n\n"
	respIncomplete = "event: response.incomplete\n" + `data: {"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":7,"output_tokens":9,"total_tokens":16}}}` + "\n\n"
	respsClean     = respDelta + respCompleted
	respsTruncated = respDelta
)

// ── 1. mixed-provider multi-deployment through the real Router ────────────

func TestNativeHandler_MixedProviderDeploymentsRouteByEndpoint(t *testing.T) {
	t.Parallel()

	anth := jsonUpstream(t, http.StatusOK, messagesJSON(5, 6))
	oai := jsonUpstream(t, http.StatusOK, responsesJSON(5, 6))
	env := newNHEnv(t, nhOpts{
		useRouter: true,
		models: []config.ModelConfig{{
			Name:     "mixed",
			Provider: "openai",
			Strategy: "round-robin",
			Deployments: []config.DeploymentConfig{
				{Name: "anth", Provider: "anthropic", BaseURL: anth.srv.URL, APIKey: upstreamSecret},
				{Name: "oai", Provider: "openai", BaseURL: oai.srv.URL, APIKey: upstreamSecret},
			},
		}},
	})

	const n = 6
	for i := 0; i < n; i++ {
		if status, body := env.post(t, "/v1/messages", messagesBody("mixed", false)); status != http.StatusOK {
			t.Fatalf("messages #%d: status %d body %s", i, status, body)
		}
	}
	if anth.hits.Load() != n || oai.hits.Load() != 0 {
		t.Fatalf("after messages: anthropic hits %d (want %d), openai hits %d (want 0)", anth.hits.Load(), n, oai.hits.Load())
	}
	if got := anth.lastReq().Path; got != "/v1/messages" {
		t.Errorf("anthropic upstream path = %q", got)
	}

	for i := 0; i < n; i++ {
		if status, body := env.post(t, "/v1/responses", responsesBody("mixed", false)); status != http.StatusOK {
			t.Fatalf("responses #%d: status %d body %s", i, status, body)
		}
	}
	if anth.hits.Load() != n || oai.hits.Load() != n {
		t.Fatalf("after responses: anthropic hits %d (want %d), openai hits %d (want %d)", anth.hits.Load(), n, oai.hits.Load(), n)
	}
	if got := oai.lastReq().Path; got != "/responses" {
		t.Errorf("openai upstream path = %q", got)
	}
}

// ── 2. cross-model fallback skips incompatible models ─────────────────────

func TestNativeHandler_FallbackSkipsIncompatibleModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		path         string
		primaryProv  string
		incompatProv string
		goodProv     string
		primaryResp  string
		goodResp     string
		bodyFor      func(model string, stream bool) string
		goodUpPath   string
	}{
		{
			name:         "messages",
			path:         "/v1/messages",
			primaryProv:  "anthropic",
			incompatProv: "vllm",
			goodProv:     "anthropic",
			primaryResp:  `{"type":"error","error":{"type":"api_error","message":"down"}}`,
			goodResp:     messagesJSON(3, 4),
			bodyFor:      messagesBody,
			goodUpPath:   "/v1/messages",
		},
		{
			name:         "responses",
			path:         "/v1/responses",
			primaryProv:  "vllm",
			incompatProv: "anthropic",
			goodProv:     "openai",
			primaryResp:  `{"error":{"message":"down"}}`,
			goodResp:     responsesJSON(3, 4),
			bodyFor:      responsesBody,
			goodUpPath:   "/responses",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			primary := jsonUpstream(t, http.StatusInternalServerError, tc.primaryResp)
			incompat := jsonUpstream(t, http.StatusOK, `{"should":"never be called"}`)
			good := jsonUpstream(t, http.StatusOK, tc.goodResp)

			pm := oaiModel("primary", tc.primaryProv, primary.srv.URL)
			pm.Fallback = "fb1"
			im := oaiModel("fb1", tc.incompatProv, incompat.srv.URL)
			im.Fallback = "fb2"
			gm := oaiModel("fb2", tc.goodProv, good.srv.URL)

			env := newNHEnv(t, nhOpts{models: []config.ModelConfig{pm, im, gm}, fallbackDepth: 3})

			status, body := env.post(t, tc.path, tc.bodyFor("primary", false))
			if status != http.StatusOK || !strings.Contains(body, replyMarker) {
				t.Fatalf("status %d body %s, want 200 from the compatible fallback", status, body)
			}
			if primary.hits.Load() != 1 {
				t.Errorf("primary hits = %d, want 1", primary.hits.Load())
			}
			if incompat.hits.Load() != 0 {
				t.Errorf("incompatible fallback was called %d times, want 0", incompat.hits.Load())
			}
			if good.hits.Load() != 1 {
				t.Errorf("compatible fallback hits = %d, want 1", good.hits.Load())
			}
			got := good.lastReq()
			if got.Path != tc.goodUpPath {
				t.Errorf("fallback upstream path = %q, want %q", got.Path, tc.goodUpPath)
			}
			if !strings.Contains(got.Body, `"model":"fb2"`) {
				t.Errorf("fallback request body model not rewritten: %s", got.Body)
			}
			rows := env.waitRows(t, 1)
			if rows[0].model != "fb2" || rows[0].requested != "primary" || rows[0].status != http.StatusOK {
				t.Errorf("usage row = %+v, want model fb2 requested primary status 200", rows[0])
			}
		})
	}
}

// ── 3 + 8. breaker outcomes for native streams ────────────────────────────

func TestNativeHandler_StreamBreakerOutcomes(t *testing.T) {
	t.Parallel()

	// Threshold 2: failure, X, failure. A success in X resets the consecutive
	// failure count so the breaker stays closed; a neutral X leaves it alone so
	// the second failure trips it.
	tests := []struct {
		name      string
		path      string
		provider  string
		body      func(string, bool) string
		scenarios []string // per-hit upstream stream
		wantState circuitbreaker.State
	}{
		{"messages clean message_stop is success", "/v1/messages", "anthropic", messagesBody,
			[]string{msgsTruncated, msgsClean, msgsTruncated}, circuitbreaker.Closed},
		{"responses clean response.completed is success", "/v1/responses", "vllm", responsesBody,
			[]string{respsTruncated, respsClean, respsTruncated}, circuitbreaker.Closed},
		{"messages truncated is failure", "/v1/messages", "anthropic", messagesBody,
			[]string{msgsTruncated, msgsTruncated}, circuitbreaker.Open},
		{"responses truncated is failure", "/v1/responses", "vllm", responsesBody,
			[]string{respsTruncated, respsTruncated}, circuitbreaker.Open},
		{"responses incomplete is neutral", "/v1/responses", "vllm", responsesBody,
			[]string{respsTruncated, respIncomplete, respsTruncated}, circuitbreaker.Open},
		{"responses failed is failure", "/v1/responses", "vllm", responsesBody,
			[]string{
				"event: response.failed\n" + `data: {"type":"response.failed","response":{}}` + "\n\n",
				"event: response.failed\n" + `data: {"type":"response.failed","response":{}}` + "\n\n",
			}, circuitbreaker.Open},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			up := seqStreamUpstream(t, tc.scenarios...)
			env := newNHEnv(t, nhOpts{
				models:  []config.ModelConfig{oaiModel("bm", tc.provider, up.srv.URL)},
				breaker: &circuitbreaker.Config{Enabled: true, Threshold: 2, Timeout: time.Minute, HalfOpenMax: 1},
			})
			for i := range tc.scenarios {
				if status, body := env.post(t, tc.path, tc.body("bm", true)); status != http.StatusOK {
					t.Fatalf("request %d: status %d body %s", i, status, body)
				}
				// The usage row is written after the breaker verdict.
				env.waitRows(t, i+1)
			}
			if got := env.breaker.Get("bm").CurrentState(); got != tc.wantState {
				t.Errorf("breaker state = %v, want %v", got, tc.wantState)
			}
		})
	}
}

func TestNativeHandler_StreamUsageStatusPerTerminal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		path       string
		provider   string
		body       func(string, bool) string
		events     string
		wantStatus int
		wantAbort  bool
	}{
		{"messages clean", "/v1/messages", "anthropic", messagesBody, msgsClean, 200, false},
		{"messages truncated", "/v1/messages", "anthropic", messagesBody, msgsTruncated, 502, true},
		{"responses clean", "/v1/responses", "vllm", responsesBody, respsClean, 200, false},
		{"responses truncated", "/v1/responses", "vllm", responsesBody, respsTruncated, 502, true},
		{"responses incomplete is forwarded without abort but not logged as success", "/v1/responses", "vllm", responsesBody, respDelta + respIncomplete, 502, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			up := seqStreamUpstream(t, tc.events)
			env := newNHEnv(t, nhOpts{models: []config.ModelConfig{oaiModel("sm", tc.provider, up.srv.URL)}})
			status, body := env.post(t, tc.path, tc.body("sm", true))
			if status != http.StatusOK {
				t.Fatalf("status %d body %s", status, body)
			}
			if strings.Contains(body, "stream aborted") != tc.wantAbort {
				t.Errorf("abort event present = %v, want %v; body %q", !tc.wantAbort, tc.wantAbort, body)
			}
			if tc.wantAbort && !strings.HasPrefix(body, tc.events) {
				t.Errorf("upstream bytes were not forwarded verbatim before the abort event: %q", body)
			}
			if !tc.wantAbort && body != tc.events {
				t.Errorf("stream modified:\n got %q\nwant %q", body, tc.events)
			}
			rows := env.waitRows(t, 1)
			if rows[0].status != tc.wantStatus {
				t.Errorf("usage status = %d, want %d", rows[0].status, tc.wantStatus)
			}
		})
	}
}

func TestNativeHandler_MessagesMidStreamErrorEvent(t *testing.T) {
	t.Parallel()

	events := msgStart + msgDelta +
		"event: error\n" + `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n"
	up := seqStreamUpstream(t, events)
	env := newNHEnv(t, nhOpts{
		models:  []config.ModelConfig{anthModel("em", up.srv.URL)},
		breaker: &circuitbreaker.Config{Enabled: true, Threshold: 1, Timeout: time.Minute, HalfOpenMax: 1},
	})

	status, body := env.post(t, "/v1/messages", messagesBody("em", true))
	if status != http.StatusOK {
		t.Fatalf("status %d body %s", status, body)
	}
	if body != events {
		t.Errorf("upstream error event must be forwarded verbatim with no extra event:\n got %q\nwant %q", body, events)
	}
	if n := strings.Count(body, "event: error"); n != 1 {
		t.Errorf("error events = %d, want exactly 1", n)
	}
	rows := env.waitRows(t, 1)
	if rows[0].status != http.StatusBadGateway {
		t.Errorf("usage status = %d, want 502 (failure)", rows[0].status)
	}
	if got := env.breaker.Get("em").CurrentState(); got != circuitbreaker.Open {
		t.Errorf("breaker state = %v, want Open (recorded as failure)", got)
	}
}

func TestNativeHandler_NonStreamingResponsesIncomplete(t *testing.T) {
	t.Parallel()

	upBody := fmt.Sprintf(`{"id":"resp_1","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output_text":%q,"usage":{"input_tokens":11,"output_tokens":13,"total_tokens":24}}`, replyMarker)
	up := jsonUpstream(t, http.StatusOK, upBody)
	env := newNHEnv(t, nhOpts{
		models:  []config.ModelConfig{oaiModel("inc", "openai", up.srv.URL)},
		breaker: &circuitbreaker.Config{Enabled: true, Threshold: 1, Timeout: time.Minute, HalfOpenMax: 1},
	})
	status, body := env.post(t, "/v1/responses", responsesBody("inc", false))
	if status != http.StatusOK || body != upBody {
		t.Fatalf("status %d body %s, want 200 with the upstream body untouched", status, body)
	}
	rows := env.waitRows(t, 1)
	if rows[0].status != http.StatusOK || rows[0].prompt != 11 || rows[0].completion != 13 || rows[0].total != 24 {
		t.Errorf("usage row = %+v, want 200 with 11/13/24 tokens", rows[0])
	}
	if got := env.breaker.Get("inc").CurrentState(); got != circuitbreaker.Closed {
		t.Errorf("breaker state = %v, want Closed", got)
	}
}

// ── 4. token budget ───────────────────────────────────────────────────────

func TestNativeHandler_TokenBudgetEnforced(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		chargePath string
	}{
		{"charged by messages", "/v1/messages"},
		{"charged by responses", "/v1/responses"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			anth := jsonUpstream(t, http.StatusOK, messagesJSON(60, 60))
			oai := jsonUpstream(t, http.StatusOK, responsesJSON(60, 60))
			env := newNHEnv(t, nhOpts{
				tokenCounter: true,
				models: []config.ModelConfig{
					anthModel("claude", anth.srv.URL),
					oaiModel("gpt", "openai", oai.srv.URL),
				},
				mutateKey: func(k *auth.KeyInfo) { k.DailyTokenLimit = 100 },
			})
			limits := ratelimit.ScopeLimits{Key: ratelimit.Limits{DailyTokenLimit: 100}}

			if err := env.tc.CheckTokens(env.scopes(), limits); err != nil {
				t.Fatalf("budget exhausted before any request: %v", err)
			}
			charge := messagesBody("claude", false)
			if tc.chargePath == "/v1/responses" {
				charge = responsesBody("gpt", false)
			}
			if status, body := env.post(t, tc.chargePath, charge); status != http.StatusOK {
				t.Fatalf("charging request: status %d body %s", status, body)
			}
			waitFor(t, "usage charged to the token counter", func() bool {
				return env.tc.CheckTokens(env.scopes(), limits) != nil
			})

			status, body := env.post(t, "/v1/messages", messagesBody("claude", false))
			if status != http.StatusTooManyRequests {
				t.Fatalf("messages over budget: status %d body %s", status, body)
			}
			if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"type":"rate_limit_error"`) {
				t.Errorf("messages over budget: not an Anthropic rate_limit_error envelope: %s", body)
			}

			status, body = env.post(t, "/v1/responses", responsesBody("gpt", false))
			if status != http.StatusTooManyRequests {
				t.Fatalf("responses over budget: status %d body %s", status, body)
			}
			if !strings.Contains(body, `"code":"token_limit_exceeded"`) || strings.Contains(body, "rate_limit_error") {
				t.Errorf("responses over budget: want the existing OpenAI-style envelope, got %s", body)
			}

			wantAnth, wantOAI := int32(1), int32(0)
			if tc.chargePath == "/v1/responses" {
				wantAnth, wantOAI = 0, 1
			}
			if anth.hits.Load() != wantAnth || oai.hits.Load() != wantOAI {
				t.Errorf("upstream hits anthropic=%d openai=%d, want %d/%d (rejected requests must not reach upstream)",
					anth.hits.Load(), oai.hits.Load(), wantAnth, wantOAI)
			}
		})
	}
}

// ── 5. model allowlist ────────────────────────────────────────────────────

func TestNativeHandler_ModelAllowlistDenied(t *testing.T) {
	t.Parallel()

	anth := jsonUpstream(t, http.StatusOK, messagesJSON(1, 1))
	oai := jsonUpstream(t, http.StatusOK, responsesJSON(1, 1))
	env := newNHEnv(t, nhOpts{
		models: []config.ModelConfig{
			anthModel("claude", anth.srv.URL),
			anthModel("claude-allowed", anth.srv.URL),
			oaiModel("gpt", "openai", oai.srv.URL),
		},
		access: func(keyID string) *proxy.ModelAccessCache {
			c := proxy.NewModelAccessCache()
			c.Load(nil, nil, map[string][]string{keyID: {"claude-allowed"}})
			return c
		},
	})

	status, body := env.post(t, "/v1/messages", messagesBody("claude", false))
	if status != http.StatusForbidden {
		t.Fatalf("denied model on messages: status %d body %s", status, body)
	}
	if !strings.Contains(body, `"type":"error"`) || !strings.Contains(body, `"type":"permission_error"`) {
		t.Errorf("not an Anthropic permission_error envelope: %s", body)
	}

	status, body = env.post(t, "/v1/responses", responsesBody("gpt", false))
	if status != http.StatusForbidden || !strings.Contains(body, `"code":"model_access_denied"`) {
		t.Errorf("denied model on responses: status %d body %s", status, body)
	}
	if anth.hits.Load() != 0 || oai.hits.Load() != 0 {
		t.Errorf("denied requests reached upstream: anthropic=%d openai=%d", anth.hits.Load(), oai.hits.Load())
	}

	if status, body = env.post(t, "/v1/messages", messagesBody("claude-allowed", false)); status != http.StatusOK {
		t.Errorf("allowed model: status %d body %s", status, body)
	}
}

// ── 6. count_tokens: rate limited, but no usage row and no token charge ───

func TestNativeHandler_CountTokensRateLimitedButNotBilled(t *testing.T) {
	t.Parallel()

	ct := newNHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/messages/count_tokens" {
			fmt.Fprint(w, `{"input_tokens":500}`)
			return
		}
		fmt.Fprint(w, messagesJSON(2, 3))
	})
	env := newNHEnv(t, nhOpts{
		rateLimiter:  true,
		tokenCounter: true,
		models:       []config.ModelConfig{anthModel("claude", ct.srv.URL)},
		mutateKey: func(k *auth.KeyInfo) {
			k.RequestsPerMinute = 2
			k.DailyTokenLimit = 1
		},
	})

	for i := 0; i < 2; i++ {
		status, body := env.post(t, "/v1/messages/count_tokens", messagesBody("claude", false))
		if status != http.StatusOK || body != `{"input_tokens":500}` {
			t.Fatalf("count_tokens #%d: status %d body %s", i, status, body)
		}
	}
	status, body := env.post(t, "/v1/messages/count_tokens", messagesBody("claude", false))
	if status != http.StatusTooManyRequests {
		t.Fatalf("third count_tokens: status %d body %s, want 429 (requests_per_minute applies)", status, body)
	}
	if !strings.Contains(body, `"type":"rate_limit_error"`) {
		t.Errorf("not an Anthropic rate_limit_error envelope: %s", body)
	}
	if ct.hits.Load() != 2 {
		t.Errorf("upstream hits = %d, want 2", ct.hits.Load())
	}

	// Sentinel: a different, unlimited key makes one billable call. It must be
	// the only usage row.
	other, _ := env.addKey(t, "nh-other", nil)
	if status, body, _ := env.do(t, "/v1/messages", messagesBody("claude", false), map[string]string{"x-api-key": other}); status != http.StatusOK {
		t.Fatalf("sentinel call: status %d body %v", status, body)
	}
	rows := env.waitRows(t, 1)
	time.Sleep(100 * time.Millisecond)
	rows = env.queryRows(t)
	if len(rows) != 1 || rows[0].keyID != "nh-other" {
		t.Errorf("usage rows = %+v, want exactly the sentinel row (count_tokens must not log usage)", rows)
	}
	if err := env.tc.CheckTokens(env.scopes(), ratelimit.ScopeLimits{Key: ratelimit.Limits{DailyTokenLimit: 1}}); err != nil {
		t.Errorf("count_tokens charged tokens to the key: %v", err)
	}
}

// ── 7. upstream 500 and circuit-open ──────────────────────────────────────

func TestNativeHandler_Upstream500ThenCircuitOpen(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		path         string
		provider     string
		upstreamBody string
		body         func(string, bool) string
		wantOpenType string
		wantOpenCode string
	}{
		{
			name: "messages", path: "/v1/messages", provider: "anthropic",
			upstreamBody: `{"type":"error","error":{"type":"api_error","message":"` + errorMarker + `"}}`,
			body:         messagesBody, wantOpenType: `"type":"overloaded_error"`,
		},
		{
			name: "responses", path: "/v1/responses", provider: "openai",
			upstreamBody: `{"error":{"message":"` + errorMarker + `","type":"server_error"}}`,
			body:         responsesBody, wantOpenCode: `"code":"circuit_open"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			up := jsonUpstream(t, http.StatusInternalServerError, tc.upstreamBody)
			env := newNHEnv(t, nhOpts{
				models:  []config.ModelConfig{oaiModel("m", tc.provider, up.srv.URL)},
				breaker: &circuitbreaker.Config{Enabled: true, Threshold: 1, Timeout: time.Minute, HalfOpenMax: 1},
			})

			// Design under test: the upstream 5xx body is forwarded as-is.
			status, body := env.post(t, tc.path, tc.body("m", false))
			if status != http.StatusInternalServerError || body != tc.upstreamBody {
				t.Fatalf("upstream 500: status %d body %s, want 500 with the upstream body forwarded", status, body)
			}
			waitFor(t, "breaker to open", func() bool { return env.breaker.Get("m").CurrentState() == circuitbreaker.Open })

			status, body = env.post(t, tc.path, tc.body("m", false))
			if status != http.StatusServiceUnavailable {
				t.Fatalf("circuit open: status %d body %s, want 503", status, body)
			}
			if tc.wantOpenType != "" && (!strings.Contains(body, `"type":"error"`) || !strings.Contains(body, tc.wantOpenType)) {
				t.Errorf("circuit open on messages: not an overloaded_error envelope: %s", body)
			}
			if tc.wantOpenCode != "" && !strings.Contains(body, tc.wantOpenCode) {
				t.Errorf("circuit open on responses: want the existing envelope, got %s", body)
			}
			if up.hits.Load() != 1 {
				t.Errorf("upstream hits = %d, want 1 (open circuit must not call upstream)", up.hits.Load())
			}
		})
	}
}

// ── 9. x-api-key auth end to end ──────────────────────────────────────────

func TestNativeHandler_XAPIKeyAuth(t *testing.T) {
	t.Parallel()

	anth := newNHUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, messagesJSON(2, 3))
	})
	vllm := newNHUpstream(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"`+replyMarker+`"}}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
	})
	env := newNHEnv(t, nhOpts{models: []config.ModelConfig{
		anthModel("claude", anth.srv.URL),
		oaiModel("chatm", "vllm", vllm.srv.URL),
	}})
	keyB, _ := env.addKey(t, "nh-key-b", nil)

	chatBody := fmt.Sprintf(`{"model":"chatm","messages":[{"role":"user","content":%q}]}`, promptMarker)

	tests := []struct {
		name       string
		path       string
		body       string
		hdr        func() map[string]string
		wantStatus int
		wantBody   string
		wantKeyID  string
	}{
		{"messages x-api-key", "/v1/messages", messagesBody("claude", false),
			func() map[string]string { return map[string]string{"x-api-key": env.key} }, 200, "", "nh-key"},
		{"chat completions x-api-key", "/v1/chat/completions", chatBody,
			func() map[string]string { return map[string]string{"x-api-key": env.key} }, 200, "", "nh-key"},
		{"bearer wins over a different valid x-api-key", "/v1/messages", messagesBody("claude", false),
			func() map[string]string {
				return map[string]string{"Authorization": "Bearer " + env.key, "x-api-key": keyB}
			}, 200, "", "nh-key"},
		{"bearer wins over invalid x-api-key", "/v1/messages", messagesBody("claude", false),
			func() map[string]string {
				return map[string]string{"Authorization": "Bearer " + env.key, "x-api-key": "vl_uk_invalid"}
			}, 200, "", "nh-key"},
		{"invalid bearer is not rescued by valid x-api-key", "/v1/messages", messagesBody("claude", false),
			func() map[string]string {
				return map[string]string{"Authorization": "Bearer vl_uk_invalid", "x-api-key": env.key}
			}, 401, `"type":"authentication_error"`, ""},
		{"invalid x-api-key on messages gets Anthropic envelope", "/v1/messages", messagesBody("claude", false),
			func() map[string]string { return map[string]string{"x-api-key": "vl_uk_invalid"} }, 401, `"type":"authentication_error"`, ""},
		{"invalid x-api-key on chat gets standard envelope", "/v1/chat/completions", chatBody,
			func() map[string]string { return map[string]string{"x-api-key": "vl_uk_invalid"} }, 401, `"code":"unauthorized"`, ""},
	}
	// Sequential: each case inspects the cumulative usage rows.
	wantRows := 0
	for _, tc := range tests {
		status, _, body := env.do(t, tc.path, tc.body, tc.hdr())
		if status != tc.wantStatus {
			t.Errorf("%s: status %d, want %d; body %s", tc.name, status, tc.wantStatus, body)
			continue
		}
		if tc.wantBody != "" && !strings.Contains(body, tc.wantBody) {
			t.Errorf("%s: body %s missing %s", tc.name, body, tc.wantBody)
		}
		if tc.wantStatus == 401 {
			if strings.HasPrefix(tc.path, "/v1/messages") && !strings.Contains(body, `"type":"error"`) {
				t.Errorf("%s: not an Anthropic envelope: %s", tc.name, body)
			}
			continue
		}
		wantRows++
		rows := env.waitRows(t, wantRows)
		if got := rows[len(rows)-1].keyID; got != tc.wantKeyID {
			t.Errorf("%s: request attributed to key %q, want %q", tc.name, got, tc.wantKeyID)
		}
	}

	// No client credential may reach any upstream.
	for name, up := range map[string]*nhUpstream{"anthropic": anth, "vllm": vllm} {
		for _, r := range up.last {
			for h, vals := range r.Header {
				for _, v := range vals {
					if strings.Contains(v, env.key) || strings.Contains(v, keyB) {
						t.Errorf("%s upstream received a VoidLLM key in header %s", name, h)
					}
				}
			}
		}
	}
	if got := anth.lastReq().Header.Get("x-api-key"); got != upstreamSecret {
		t.Errorf("anthropic upstream x-api-key = %q, want the model key", got)
	}
	if got := vllm.lastReq().Header.Get("Authorization"); got != "Bearer "+upstreamSecret {
		t.Errorf("vllm upstream Authorization = %q, want the model key", got)
	}
}

// ── 10. privacy: log capture sanity ───────────────────────────────────────

func TestNativeHandler_LogCaptureIsLive(t *testing.T) {
	t.Parallel()

	// Drives error paths with content in request and response bodies. The
	// cleanup in newNHEnv fails the test if any of it reaches the logs; this
	// test also proves the capture is wired by checking a canary.
	up := newNHUpstream(t, func(w http.ResponseWriter, _ *http.Request, hit int) {
		if hit%2 == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"type":"error","error":{"type":"api_error","message":"`+errorMarker+`"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, msgsTruncated)
	})
	env := newNHEnv(t, nhOpts{
		models:  []config.ModelConfig{anthModel("claude", up.srv.URL)},
		breaker: &circuitbreaker.Config{Enabled: true, Threshold: 100, Timeout: time.Minute, HalfOpenMax: 1},
	})
	env.h.Log.Info("canary")

	env.post(t, "/v1/messages", messagesBody("claude", false))
	env.post(t, "/v1/messages", messagesBody("claude", true))
	env.post(t, "/v1/messages", messagesBody("unknown-model", false))
	env.do(t, "/v1/messages", messagesBody("claude", false), map[string]string{"x-api-key": "vl_uk_wrong"})
	env.waitRows(t, 1) // only the truncated stream logs a row; buffered 5xx do not

	if !strings.Contains(env.logs.String(), "canary") {
		t.Fatal("log capture is not wired to the handler logger")
	}
}
