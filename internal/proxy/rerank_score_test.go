package proxy

// rerank_score_test.go verifies the /v1/rerank and /v1/score paths end to end
// through the handler: usage extraction from vLLM-shaped responses, PII
// anonymization and restoration for every documented request shape, and the
// behaviour of non-passthrough adapters on these paths.

import (
	"context"
	"database/sql"
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

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/usage"
)

// vllmRerankResponse is a realistic vLLM /rerank response: usage carries only
// total_tokens.
const vllmRerankResponse = `{"id":"rerank-1","model":"bge-reranker","usage":{"total_tokens":42},"results":[{"index":1,"document":{"text":"doc b"},"relevance_score":0.91},{"index":0,"document":{"text":"doc a"},"relevance_score":0.12}]}`

// vllmScoreResponse is a realistic vLLM /score response: usage carries
// prompt_tokens and total_tokens.
const vllmScoreResponse = `{"id":"score-1","object":"list","created":1,"model":"bge-reranker","data":[{"index":0,"object":"score","score":0.87}],"usage":{"prompt_tokens":30,"total_tokens":30,"completion_tokens":0}}`

type loggedUsage struct {
	prompt, completion, total int
	cost                      sql.NullFloat64
}

// runUsageRequest sends one authenticated request to path against an upstream
// that answers with respBody, flushes the usage logger, and returns the single
// usage row that was written.
func runUsageRequest(t *testing.T, path, reqBody, respBody string, pricing config.PricingConfig) loggedUsage {
	t.Helper()

	upstream, _ := upstreamCapture(t, http.StatusOK, respBody,
		map[string]string{"Content-Type": "application/json"})

	model := &Model{
		Name:     "rr-model",
		Provider: "vllm",
		Type:     "reranking",
		BaseURL:  upstream.URL,
		APIKey:   "k",
		Pricing:  pricing,
	}
	reg := &Registry{models: map[string]*Model{model.Name: model}, aliases: make(map[string]string)}
	reg.rebuildSorted()

	d := openCostFormulaDB(t)
	ul := usage.NewLogger(d, config.UsageConfig{BufferSize: 8, FlushInterval: time.Hour},
		slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	ul.Start()

	h := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.UsageLogger = ul
	app, rawKey := testAppWithAuth(t, h, auth.KeyInfo{ID: "rr-key", KeyType: "user_key", OrgID: "rr-org", Role: "member"})

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawKey)
	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}
	ul.Stop()

	var u loggedUsage
	row := d.SQL().QueryRowContext(context.Background(),
		"SELECT prompt_tokens, completion_tokens, total_tokens, cost_estimate FROM usage_events LIMIT 1")
	if err := row.Scan(&u.prompt, &u.completion, &u.total, &u.cost); err != nil {
		t.Fatalf("scan usage_events: %v", err)
	}
	return u
}

// TestRerankScore_UsageExtraction verifies what is recorded for vLLM rerank
// and score responses, and that a client billed only via total_tokens is
// accounted as prompt tokens (and therefore priced).
func TestRerankScore_UsageExtraction(t *testing.T) {
	t.Parallel()

	pricing := config.PricingConfig{InputPer1M: 1_000_000}

	t.Run("rerank with only total_tokens", func(t *testing.T) {
		t.Parallel()
		u := runUsageRequest(t, "/v1/rerank",
			`{"model":"rr-model","query":"q","documents":["doc a","doc b"]}`, vllmRerankResponse, pricing)
		if u.total != 42 {
			t.Errorf("total_tokens = %d, want 42", u.total)
		}
		if u.prompt != 42 {
			t.Errorf("prompt_tokens = %d, want 42 (client was billed total_tokens)", u.prompt)
		}
		if u.completion != 0 {
			t.Errorf("completion_tokens = %d, want 0", u.completion)
		}
		if !u.cost.Valid || u.cost.Float64 != 42 {
			t.Errorf("cost_estimate = %+v, want 42", u.cost)
		}
	})

	t.Run("score with prompt_tokens and total_tokens", func(t *testing.T) {
		t.Parallel()
		u := runUsageRequest(t, "/v1/score",
			`{"model":"rr-model","text_1":"a","text_2":["b"]}`, vllmScoreResponse, pricing)
		if u.prompt != 30 || u.total != 30 || u.completion != 0 {
			t.Errorf("prompt/completion/total = %d/%d/%d, want 30/0/30", u.prompt, u.completion, u.total)
		}
		if !u.cost.Valid || u.cost.Float64 != 30 {
			t.Errorf("cost_estimate = %+v, want 30", u.cost)
		}
	})

	t.Run("chat response with total_tokens only is not remapped", func(t *testing.T) {
		t.Parallel()
		u := runUsageRequest(t, "/v1/chat/completions",
			`{"model":"rr-model","messages":[]}`, `{"usage":{"total_tokens":7}}`, pricing)
		if u.prompt != 0 || u.total != 7 {
			t.Errorf("prompt/total = %d/%d, want 0/7", u.prompt, u.total)
		}
	})
}

// TestNormalizeRerankScoreUsage covers the helper directly.
func TestNormalizeRerankScoreUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		path string
		in   UsageInfo
		want UsageInfo
	}{
		{"rerank total only", "rerank", UsageInfo{TotalTokens: 5}, UsageInfo{PromptTokens: 5, TotalTokens: 5}},
		{"score total only", "score", UsageInfo{TotalTokens: 5}, UsageInfo{PromptTokens: 5, TotalTokens: 5}},
		{"prompt already set", "rerank", UsageInfo{PromptTokens: 3, TotalTokens: 5}, UsageInfo{PromptTokens: 3, TotalTokens: 5}},
		{"completion set", "score", UsageInfo{CompletionTokens: 2, TotalTokens: 5}, UsageInfo{CompletionTokens: 2, TotalTokens: 5}},
		{"other path", "embeddings", UsageInfo{TotalTokens: 5}, UsageInfo{TotalTokens: 5}},
		{"no usage", "rerank", UsageInfo{}, UsageInfo{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := normalizeRerankScoreUsage(tc.path, tc.in); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// echoPseudonymUpstream returns an upstream that records the request body and
// answers with a rerank-shaped response whose document text echoes every PII
// pseudonym found in the request body.
func echoPseudonymUpstream(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var last string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		last = string(b)
		mu.Unlock()
		echoed := strings.Join(piiPseudonymPattern.FindAllString(string(b), -1), " ")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"r","usage":{"total_tokens":1},"results":[{"index":0,"document":{"text":%q},"relevance_score":0.5}]}`, echoed)
	}))
	t.Cleanup(srv.Close)
	return srv, func() string {
		mu.Lock()
		defer mu.Unlock()
		return last
	}
}

// TestRerankScore_PIIShapes verifies that every rerank/score request shape is
// scanned: the upstream must see only pseudonyms and the client must get the
// original value back.
func TestRerankScore_PIIShapes(t *testing.T) {
	t.Parallel()

	e := piiTestEmail
	tests := []struct {
		name string
		path string
		body string
	}{
		{"rerank query string", "/v1/rerank", fmt.Sprintf(`{"model":"ext-model","query":"mail %s","documents":["x"]}`, e)},
		{"rerank documents strings", "/v1/rerank", fmt.Sprintf(`{"model":"ext-model","query":"q","documents":["a","mail %s"]}`, e)},
		{"rerank documents objects", "/v1/rerank", fmt.Sprintf(`{"model":"ext-model","query":"q","documents":[{"text":"mail %s"},{"text":"b"}]}`, e)},
		{"score text_1 string", "/v1/score", fmt.Sprintf(`{"model":"ext-model","text_1":"mail %s","text_2":"b"}`, e)},
		{"score text_2 string", "/v1/score", fmt.Sprintf(`{"model":"ext-model","text_1":"a","text_2":"mail %s"}`, e)},
		{"score text_1 array", "/v1/score", fmt.Sprintf(`{"model":"ext-model","text_1":["a","mail %s"],"text_2":["b"]}`, e)},
		{"score text_2 array", "/v1/score", fmt.Sprintf(`{"model":"ext-model","text_1":["a"],"text_2":["b","mail %s"]}`, e)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			upstream, lastBody := echoPseudonymUpstream(t)
			app := testApp(t, piiHandler(t, piiRegistryExternal(t, upstream.URL)))

			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req, testTimeout)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()
			clientBody, _ := io.ReadAll(resp.Body)

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, clientBody)
			}
			up := lastBody()
			if strings.Contains(up, e) {
				t.Errorf("SECURITY: upstream received the original email; body: %s", up)
			}
			if !piiPseudonymPattern.MatchString(up) {
				t.Errorf("upstream body has no pseudonym; body: %s", up)
			}
			if !strings.Contains(string(clientBody), e) {
				t.Errorf("client response did not get the email restored; body: %s", clientBody)
			}
			if piiPseudonymPattern.MatchString(string(clientBody)) {
				t.Errorf("client response still contains a pseudonym; body: %s", clientBody)
			}
		})
	}
}

// TestRerankScore_NonPassthroughAdapter verifies that rerank and score on
// providers with a translating adapter fail with a clean 4xx and never reach
// the upstream.
func TestRerankScore_NonPassthroughAdapter(t *testing.T) {
	t.Parallel()

	providers := []string{"anthropic", "gemini", "vertex"}
	paths := []struct{ path, body string }{
		{"/v1/rerank", `{"model":"ad-model","query":"q","documents":["a","b"]}`},
		{"/v1/score", `{"model":"ad-model","text_1":"a","text_2":"b"}`},
	}
	for _, provider := range providers {
		for _, p := range paths {
			t.Run(provider+p.path, func(t *testing.T) {
				t.Parallel()

				upstream, _, calls := captureUpstream(t, http.StatusOK, `{}`,
					map[string]string{"Content-Type": "application/json"})
				model := &Model{Name: "ad-model", Provider: provider, Type: "chat", BaseURL: upstream.URL, APIKey: "k"}
				reg := &Registry{models: map[string]*Model{model.Name: model}, aliases: make(map[string]string)}
				reg.rebuildSorted()
				app := testApp(t, NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil))))

				req := httptest.NewRequest(http.MethodPost, p.path, strings.NewReader(p.body))
				req.Header.Set("Content-Type", "application/json")
				resp, err := app.Test(req, testTimeout)
				if err != nil {
					t.Fatalf("app.Test: %v", err)
				}
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)

				if resp.StatusCode < 400 || resp.StatusCode >= 500 {
					t.Errorf("status = %d, want 4xx; body: %s", resp.StatusCode, b)
				}
				if n := atomic.LoadInt32(calls); n != 0 {
					t.Errorf("upstream was called %d times, want 0", n)
				}
			})
		}
	}
}
