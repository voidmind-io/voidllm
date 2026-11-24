package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/ratelimit"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// limitApp wires a handler with the given key limits and a vllm model whose
// upstream must never be reached.
func limitApp(t *testing.T, info auth.KeyInfo) (*fiber.App, *ProxyHandler, string) {
	t.Helper()
	h := testProxyHandler(t, "http://127.0.0.1:1")
	h.RateLimiter = ratelimit.NewRateLimiter()
	h.BudgetCounter = ratelimit.NewMemoryBudgetCounter()
	app, key := testAppWithAuth(t, h, info)
	return app, h, key
}

// chargeTokens records n tokens for the given key and org on h's budget
// counter.
func chargeTokens(h *ProxyHandler, keyID, orgID string, n int64) {
	h.BudgetCounter.Add(context.Background(), ratelimit.Scopes{KeyID: keyID, OrgID: orgID}, ratelimit.Usage{Tokens: n})
}

func postLimited(t *testing.T, app *fiber.App, path, key string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"test-model"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

var limitPaths = []string{"/v1/messages", "/v1/responses", "/v1/chat/completions"}

func TestLimitRejection_TokenBudgetHeaders(t *testing.T) {
	t.Parallel()
	for _, path := range limitPaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			info := auth.KeyInfo{ID: "tok-key", KeyType: keygen.KeyTypeUser, OrgID: "tok-org", Role: auth.RoleMember, DailyTokenLimit: 100}
			app, h, key := limitApp(t, info)
			chargeTokens(h, "tok-key", "tok-org", 100)

			resp := postLimited(t, app, path, key)
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", resp.StatusCode)
			}
			if got := resp.Header.Get("x-should-retry"); got != "false" {
				t.Errorf("x-should-retry = %q, want false", got)
			}
			secs, err := strconv.Atoi(resp.Header.Get("Retry-After"))
			if err != nil || secs < 1 || secs > 86400 {
				t.Errorf("Retry-After = %q, want 1..86400 (daily window)", resp.Header.Get("Retry-After"))
			}
		})
	}
}

func TestLimitRejection_MonthlyBudgetRetryAfterIsUntilMonthEnd(t *testing.T) {
	t.Parallel()
	info := auth.KeyInfo{ID: "mon-key", KeyType: keygen.KeyTypeUser, OrgID: "mon-org", Role: auth.RoleMember, MonthlyTokenLimit: 50}
	app, h, key := limitApp(t, info)
	chargeTokens(h, "mon-key", "mon-org", 50)

	resp := postLimited(t, app, "/v1/chat/completions", key)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("x-should-retry") != "false" {
		t.Errorf("x-should-retry = %q, want false", resp.Header.Get("x-should-retry"))
	}
	secs, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || secs < 1 {
		t.Fatalf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	now := time.Now().UTC()
	want := int(time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Sub(now).Seconds())
	if secs < want-5 || secs > want+5 {
		t.Errorf("Retry-After = %d, want ~%d (until the 1st of next UTC month)", secs, want)
	}
}

func TestLimitRejection_RequestRateHeaders(t *testing.T) {
	t.Parallel()
	for _, path := range limitPaths {
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			info := auth.KeyInfo{ID: "rpm-key", KeyType: keygen.KeyTypeUser, OrgID: "rpm-org", Role: auth.RoleMember, RequestsPerMinute: 1}
			app, h, key := limitApp(t, info)
			// Consume the single allowed request without hitting the upstream.
			if err := h.RateLimiter.CheckRate(ratelimit.Scopes{KeyID: "rpm-key", OrgID: "rpm-org"},
				ratelimit.ScopeLimits{Key: ratelimit.Limits{RequestsPerMinute: 1}}); err != nil {
				t.Fatalf("priming CheckRate: %v", err)
			}

			resp := postLimited(t, app, path, key)
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", resp.StatusCode)
			}
			if got := resp.Header.Get("x-should-retry"); got != "" {
				t.Errorf("x-should-retry = %q, want unset for request rate limits", got)
			}
			secs, err := strconv.Atoi(resp.Header.Get("Retry-After"))
			if err != nil || secs < 1 || secs > 60 {
				t.Errorf("Retry-After = %q, want 1..60 (minute window)", resp.Header.Get("Retry-After"))
			}
		})
	}
}

func TestLimitRejection_TokenBudgetBodyShape(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path    string
		wantErr map[string]string
	}{
		{path: "/v1/responses", wantErr: map[string]string{"type": "insufficient_quota", "code": "token_limit_exceeded", "message": "token budget exceeded"}},
		{path: "/v1/chat/completions", wantErr: map[string]string{"code": "token_limit_exceeded", "message": "token budget exceeded"}},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			info := auth.KeyInfo{ID: "tok-key", KeyType: keygen.KeyTypeUser, OrgID: "tok-org", Role: auth.RoleMember, DailyTokenLimit: 100}
			app, h, key := limitApp(t, info)
			chargeTokens(h, "tok-key", "tok-org", 100)

			resp := postLimited(t, app, tt.path, key)
			if resp.StatusCode != http.StatusTooManyRequests {
				t.Fatalf("status = %d, want 429", resp.StatusCode)
			}
			if resp.Header.Get("Retry-After") == "" || resp.Header.Get("x-should-retry") != "false" {
				t.Errorf("Retry-After=%q x-should-retry=%q", resp.Header.Get("Retry-After"), resp.Header.Get("x-should-retry"))
			}
			var body struct {
				Error map[string]string `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			delete(body.Error, "request_id")
			if !reflect.DeepEqual(body.Error, tt.wantErr) {
				t.Errorf("error = %v, want %v", body.Error, tt.wantErr)
			}
		})
	}
}

func TestLimitRejection_TokenBudgetMessagesKeepsAnthropicEnvelope(t *testing.T) {
	t.Parallel()
	info := auth.KeyInfo{ID: "tok-key", KeyType: keygen.KeyTypeUser, OrgID: "tok-org", Role: auth.RoleMember, DailyTokenLimit: 100}
	app, h, key := limitApp(t, info)
	chargeTokens(h, "tok-key", "tok-org", 100)

	resp := postLimited(t, app, "/v1/messages", key)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	var body struct {
		Type  string            `json:"type"`
		Error map[string]string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Type != "error" || body.Error["type"] != "rate_limit_error" {
		t.Errorf("body = %+v, want anthropic rate_limit_error envelope", body)
	}
}
