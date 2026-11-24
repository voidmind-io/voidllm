package proxy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/metrics"
	"github.com/voidmind-io/voidllm/internal/money"
	"github.com/voidmind-io/voidllm/internal/ratelimit"
)

// Cost enforcement fixtures. The priced model costs 1 per million input
// tokens and 2 per million output tokens, so a non-streaming response with
// 1000 input and 1000 output tokens costs 0.003 (3000 micro-units) and a
// streamed one with 7 input and 9 output tokens 0.000025 (25 micro-units).
const (
	cePriced   = "ce-priced"
	ceUnpriced = "ce-unpriced"
	ceUserID   = "ce-user"
	ceTeamID   = "ce-team"

	ceJSONCost   = money.Micros(3000)
	ceStreamCost = money.Micros(25)
)

var cePricing = config.PricingConfig{InputPer1M: 1, OutputPer1M: 2}

// ceChatStream is an OpenAI chat completions stream reporting 7 prompt and 9
// completion tokens.
const ceChatStream = `data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n" +
	`data: {"id":"c1","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":9,"total_tokens":16}}` + "\n\n" +
	"data: [DONE]\n\n"

func ceChatJSON(in, out int) string {
	return fmt.Sprintf(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`, in, out, in+out)
}

func ceChatBody(model string, stream bool) string {
	return fmt.Sprintf(`{"model":%q,"stream":%v,"messages":[{"role":"user","content":"hi"}]}`, model, stream)
}

// ceEndpoint is one proxy endpoint the cost budget is enforced on.
type ceEndpoint struct {
	name     string
	path     string
	provider string
	body     func(model string, stream bool) string
	json     func(in, out int) string
	stream   string
	// wantRejection checks the body of a cost rejection on this endpoint.
	wantRejection func(t *testing.T, body string)
}

// ceError decodes the error object of a rejection body together with the
// top-level type the Anthropic envelope carries.
func ceError(t *testing.T, body string) (topType string, detail map[string]any) {
	t.Helper()
	var parsed struct {
		Type  string         `json:"type"`
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatalf("rejection body is not JSON: %v: %s", err, body)
	}
	return parsed.Type, parsed.Error
}

var ceEndpoints = []ceEndpoint{
	{
		name: "chat_completions", path: "/v1/chat/completions", provider: "openai",
		body: ceChatBody, json: ceChatJSON, stream: ceChatStream,
		wantRejection: func(t *testing.T, body string) {
			t.Helper()
			top, detail := ceError(t, body)
			if top != "" || detail["code"] != "cost_limit_exceeded" || detail["message"] != "cost budget exceeded" {
				t.Errorf("chat/completions rejection = %s, want code cost_limit_exceeded, message %q", body, "cost budget exceeded")
			}
			if detail["type"] == "insufficient_quota" {
				t.Errorf("chat/completions rejection carries the quota type reserved for /v1/responses: %s", body)
			}
		},
	},
	{
		name: "responses", path: "/v1/responses", provider: "openai",
		body: responsesBody, json: responsesJSON, stream: respsClean,
		wantRejection: func(t *testing.T, body string) {
			t.Helper()
			_, detail := ceError(t, body)
			if detail["type"] != "insufficient_quota" || detail["code"] != "cost_limit_exceeded" || detail["message"] != "cost budget exceeded" {
				t.Errorf("responses rejection = %s, want type insufficient_quota, code cost_limit_exceeded, message %q", body, "cost budget exceeded")
			}
		},
	},
	{
		name: "messages", path: "/v1/messages", provider: "anthropic",
		body: messagesBody, json: messagesJSON, stream: msgsClean,
		wantRejection: func(t *testing.T, body string) {
			t.Helper()
			top, detail := ceError(t, body)
			if top != "error" || detail["type"] != "rate_limit_error" || detail["message"] != "cost budget exceeded" {
				t.Errorf("messages rejection = %s, want an Anthropic rate_limit_error envelope with message %q", body, "cost budget exceeded")
			}
		},
	},
}

// ceScopes are the four scopes a cost limit can be set on.
var ceScopes = []struct {
	name  string
	daily func(k *auth.KeyInfo, limit money.Micros)
}{
	{"key", func(k *auth.KeyInfo, l money.Micros) { k.DailyCostLimit = l }},
	{"user", func(k *auth.KeyInfo, l money.Micros) { k.UserDailyCostLimit = l }},
	{"team", func(k *auth.KeyInfo, l money.Micros) { k.TeamDailyCostLimit = l }},
	{"org", func(k *auth.KeyInfo, l money.Micros) { k.OrgDailyCostLimit = l }},
}

// ceLicensed and ceCommunity are license holders with and without the feature
// that allows cost budgets.
func ceLicensed() *license.Holder  { return license.NewHolder(license.Verify("", true)) }
func ceCommunity() *license.Holder { return license.NewHolder(license.Verify("", false)) }

// newCEEnv builds an environment with a real usage logger and budget counter,
// a priced and an unpriced model of ep's provider on one upstream, and a key
// that belongs to a user and a team.
func newCEEnv(t *testing.T, ep ceEndpoint, stream bool, lic *license.Holder, mutate func(*auth.KeyInfo)) (*nhEnv, *nhUpstream) {
	t.Helper()
	var up *nhUpstream
	if stream {
		up = seqStreamUpstream(t, ep.stream)
	} else {
		up = jsonUpstream(t, http.StatusOK, ep.json(1000, 1000))
	}
	priced := oaiModel(cePriced, ep.provider, up.srv.URL)
	priced.Pricing = cePricing
	env := newNHEnv(t, nhOpts{
		budgetCounter: true,
		models:        []config.ModelConfig{priced, oaiModel(ceUnpriced, ep.provider, up.srv.URL)},
		mutateKey: func(k *auth.KeyInfo) {
			k.UserID = ceUserID
			k.TeamID = ceTeamID
			if mutate != nil {
				mutate(k)
			}
		},
	})
	env.h.License = lic
	return env, up
}

// ceUsage returns the daily usage the budget counter holds for each scope of
// the env key, in the order key, user, team, org.
func ceUsage(t *testing.T, env *nhEnv) [4]ratelimit.Usage {
	t.Helper()
	keys := []string{
		ratelimit.KeyScope(env.info.ID),
		ratelimit.UserScope(env.info.OrgID, ceUserID),
		ratelimit.TeamScope(ceTeamID),
		ratelimit.OrgScope(env.info.OrgID),
	}
	snap, err := env.tc.Snapshot(context.Background(), keys)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	var out [4]ratelimit.Usage
	for i, k := range keys {
		out[i] = snap[k].Daily
	}
	return out
}

// waitCECost blocks until every scope of the env key has recorded want.
func waitCECost(t *testing.T, env *nhEnv, want money.Micros) {
	t.Helper()
	waitFor(t, fmt.Sprintf("cost %d recorded on all four scopes", want), func() bool {
		for _, u := range ceUsage(t, env) {
			if u.CostMicros != int64(want) {
				return false
			}
		}
		return true
	})
}

// cePost sends an authenticated request and returns status, headers and body.
func cePost(t *testing.T, env *nhEnv, path, body string) (int, http.Header, string) {
	t.Helper()
	hdr := map[string]string{"Authorization": "Bearer " + env.key}
	if strings.HasPrefix(path, "/v1/messages") {
		hdr = map[string]string{"x-api-key": env.key}
	}
	return env.do(t, path, body, hdr)
}

// ceRetryAfter returns the Retry-After header in seconds.
func ceRetryAfter(t *testing.T, h http.Header) int {
	t.Helper()
	secs, err := strconv.Atoi(h.Get("Retry-After"))
	if err != nil || secs < 1 {
		t.Fatalf("Retry-After = %q, want a positive number of seconds", h.Get("Retry-After"))
	}
	return secs
}

func ceSecondsUntilMonthEnd() int {
	now := time.Now().UTC()
	return int(time.Date(now.Year(), now.Month()+1, 1, 0, 0, 0, 0, time.UTC).Sub(now).Seconds())
}

// TestCostEnforcement_RejectsOnceBudgetIsSpent drives a real request through
// the proxy on every endpoint, streaming and not, with a daily cost limit on
// each of the four scopes that equals the cost of that one request. The
// request must be served and charged at its exact cost; the next one must be
// rejected with the cost error in the endpoint's envelope without reaching
// the upstream.
func TestCostEnforcement_RejectsOnceBudgetIsSpent(t *testing.T) {
	t.Parallel()
	for _, ep := range ceEndpoints {
		for _, stream := range []bool{false, true} {
			for _, scope := range ceScopes {
				name := fmt.Sprintf("%s/stream=%v/%s", ep.name, stream, scope.name)
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					cost := ceJSONCost
					if stream {
						cost = ceStreamCost
					}
					env, up := newCEEnv(t, ep, stream, ceLicensed(), func(k *auth.KeyInfo) { scope.daily(k, cost) })

					status, _, body := cePost(t, env, ep.path, ep.body(cePriced, stream))
					if status != http.StatusOK {
						t.Fatalf("first request: status %d body %s", status, body)
					}
					waitCECost(t, env, cost)

					status, hdr, body := cePost(t, env, ep.path, ep.body(cePriced, stream))
					if status != http.StatusTooManyRequests {
						t.Fatalf("request over the cost budget: status %d body %s, want 429", status, body)
					}
					ep.wantRejection(t, body)
					if strings.Contains(body, "token_limit_exceeded") || strings.Contains(body, "token budget") {
						t.Errorf("cost rejection mentions the token budget: %s", body)
					}
					if got := hdr.Get("x-should-retry"); got != "false" {
						t.Errorf("x-should-retry = %q, want false", got)
					}
					if secs := ceRetryAfter(t, hdr); secs > 86400 {
						t.Errorf("Retry-After = %d, want at most 86400 (daily window)", secs)
					}
					if got := up.hits.Load(); got != 1 {
						t.Errorf("upstream hits = %d, want 1 (the rejected request must not be forwarded)", got)
					}
					wantLog := "scope=" + scope.name
					if out := env.logs.String(); !strings.Contains(out, "cost budget exceeded") ||
						!strings.Contains(out, "metric=cost") || !strings.Contains(out, wantLog) || !strings.Contains(out, "window=daily") {
						t.Errorf("log lacks the cost rejection with metric=cost %s window=daily:\n%s", wantLog, out)
					}
				})
			}
		}
	}
}

// TestCostEnforcement_BelowLimitPasses verifies that a request is served
// while the recorded cost is one micro-unit below the limit and rejected once
// the limit is reached.
func TestCostEnforcement_BelowLimitPasses(t *testing.T) {
	t.Parallel()
	ep := ceEndpoints[0]
	env, up := newCEEnv(t, ep, false, ceLicensed(), func(k *auth.KeyInfo) { k.DailyCostLimit = 2*ceJSONCost - 1 })

	for i := 1; i <= 2; i++ {
		if status, _, body := cePost(t, env, ep.path, ep.body(cePriced, false)); status != http.StatusOK {
			t.Fatalf("request %d: status %d body %s", i, status, body)
		}
		waitCECost(t, env, money.Micros(i)*ceJSONCost)
	}
	if status, _, body := cePost(t, env, ep.path, ep.body(cePriced, false)); status != http.StatusTooManyRequests {
		t.Fatalf("third request: status %d body %s, want 429", status, body)
	}
	if got := up.hits.Load(); got != 2 {
		t.Errorf("upstream hits = %d, want 2", got)
	}
}

// TestCostEnforcement_NoLicenseCountsButDoesNotReject verifies, on every
// endpoint, that without a license allowing cost budgets (community license
// or no license holder at all) cost limits on all four scopes reject nothing
// while the cost keeps being counted.
func TestCostEnforcement_NoLicenseCountsButDoesNotReject(t *testing.T) {
	t.Parallel()
	holders := []struct {
		name   string
		holder func() *license.Holder
	}{
		{"community", ceCommunity},
		{"no_holder", func() *license.Holder { return nil }},
	}
	for _, ep := range ceEndpoints {
		for _, stream := range []bool{false, true} {
			for _, h := range holders {
				t.Run(fmt.Sprintf("%s/stream=%v/%s", ep.name, stream, h.name), func(t *testing.T) {
					t.Parallel()
					cost := ceJSONCost
					if stream {
						cost = ceStreamCost
					}
					env, up := newCEEnv(t, ep, stream, h.holder(), func(k *auth.KeyInfo) {
						for _, scope := range ceScopes {
							scope.daily(k, 1)
						}
						k.MonthlyCostLimit, k.UserMonthlyCostLimit = 1, 1
						k.TeamMonthlyCostLimit, k.OrgMonthlyCostLimit = 1, 1
					})

					for i := 1; i <= 3; i++ {
						status, hdr, body := cePost(t, env, ep.path, ep.body(cePriced, stream))
						if status != http.StatusOK {
							t.Fatalf("request %d without license: status %d body %s, want 200", i, status, body)
						}
						if hdr.Get("x-should-retry") != "" {
							t.Errorf("request %d: x-should-retry = %q, want unset", i, hdr.Get("x-should-retry"))
						}
						waitCECost(t, env, money.Micros(i)*cost)
					}
					if got := up.hits.Load(); got != 3 {
						t.Errorf("upstream hits = %d, want 3", got)
					}
				})
			}
		}
	}
}

// TestCostEnforcement_FollowsLicenseAtRuntime verifies that enforcement
// starts as soon as a license is activated, against the cost counted before,
// and stops again when the license is gone, without touching token budgets.
func TestCostEnforcement_FollowsLicenseAtRuntime(t *testing.T) {
	t.Parallel()
	ep := ceEndpoints[0]
	holder := ceCommunity()
	env, _ := newCEEnv(t, ep, false, holder, func(k *auth.KeyInfo) { k.OrgMonthlyCostLimit = ceJSONCost })

	if status, _, body := cePost(t, env, ep.path, ep.body(cePriced, false)); status != http.StatusOK {
		t.Fatalf("unlicensed request: status %d body %s", status, body)
	}
	waitCECost(t, env, ceJSONCost)
	if status, _, body := cePost(t, env, ep.path, ep.body(cePriced, false)); status != http.StatusOK {
		t.Fatalf("unlicensed request over the limit: status %d body %s, want 200", status, body)
	}
	waitCECost(t, env, 2*ceJSONCost)

	holder.Store(license.Verify("", true))
	status, hdr, body := cePost(t, env, ep.path, ep.body(cePriced, false))
	if status != http.StatusTooManyRequests {
		t.Fatalf("request after license activation: status %d body %s, want 429", status, body)
	}
	ep.wantRejection(t, body)
	if secs, want := ceRetryAfter(t, hdr), ceSecondsUntilMonthEnd(); secs < want-5 || secs > want+5 {
		t.Errorf("Retry-After = %d, want ~%d (until the 1st of next UTC month)", secs, want)
	}

	holder.Store(license.Verify("", false))
	if status, _, body := cePost(t, env, ep.path, ep.body(cePriced, false)); status != http.StatusOK {
		t.Fatalf("request after the license is gone: status %d body %s, want 200", status, body)
	}
}

// TestCostEnforcement_RetryAfterIsLatestReset verifies that with a daily and
// a monthly cost budget spent, Retry-After points at the monthly reset.
func TestCostEnforcement_RetryAfterIsLatestReset(t *testing.T) {
	t.Parallel()
	for _, ep := range ceEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			t.Parallel()
			env, _ := newCEEnv(t, ep, false, ceLicensed(), func(k *auth.KeyInfo) {
				k.DailyCostLimit = ceJSONCost
				k.TeamMonthlyCostLimit = ceJSONCost
			})
			if status, _, body := cePost(t, env, ep.path, ep.body(cePriced, false)); status != http.StatusOK {
				t.Fatalf("first request: status %d body %s", status, body)
			}
			waitCECost(t, env, ceJSONCost)

			status, hdr, body := cePost(t, env, ep.path, ep.body(cePriced, false))
			if status != http.StatusTooManyRequests {
				t.Fatalf("status %d body %s, want 429", status, body)
			}
			ep.wantRejection(t, body)
			if secs, want := ceRetryAfter(t, hdr), ceSecondsUntilMonthEnd(); secs < want-5 || secs > want+5 {
				t.Errorf("Retry-After = %d, want ~%d (until the 1st of next UTC month)", secs, want)
			}
			if hdr.Get("x-should-retry") != "false" {
				t.Errorf("x-should-retry = %q, want false", hdr.Get("x-should-retry"))
			}
		})
	}
}

// TestCostEnforcement_TokenRejectionWinsWhenBothAreSpent verifies that a
// request over a token and a cost budget at once keeps the token error, with
// Retry-After covering the later of the two resets.
func TestCostEnforcement_TokenRejectionWinsWhenBothAreSpent(t *testing.T) {
	t.Parallel()
	for _, ep := range ceEndpoints {
		t.Run(ep.name, func(t *testing.T) {
			t.Parallel()
			env, _ := newCEEnv(t, ep, false, ceLicensed(), func(k *auth.KeyInfo) {
				k.DailyTokenLimit = 2000
				k.OrgMonthlyCostLimit = ceJSONCost
			})
			if status, _, body := cePost(t, env, ep.path, ep.body(cePriced, false)); status != http.StatusOK {
				t.Fatalf("first request: status %d body %s", status, body)
			}
			waitCECost(t, env, ceJSONCost)
			if got := ceUsage(t, env)[0].Tokens; got != 2000 {
				t.Fatalf("tokens recorded = %d, want 2000", got)
			}

			status, hdr, body := cePost(t, env, ep.path, ep.body(cePriced, false))
			if status != http.StatusTooManyRequests {
				t.Fatalf("status %d body %s, want 429", status, body)
			}
			if strings.Contains(body, "cost_limit_exceeded") || strings.Contains(body, "cost budget") {
				t.Errorf("rejection = %s, want the token error when both budgets are spent", body)
			}
			if !strings.Contains(body, "token budget exceeded") {
				t.Errorf("rejection = %s, want message %q", body, "token budget exceeded")
			}
			if ep.path != "/v1/messages" && !strings.Contains(body, `"code":"token_limit_exceeded"`) {
				t.Errorf("rejection = %s, want code token_limit_exceeded", body)
			}
			if secs, want := ceRetryAfter(t, hdr), ceSecondsUntilMonthEnd(); secs < want-5 || secs > want+5 {
				t.Errorf("Retry-After = %d, want ~%d (the monthly cost budget resets last)", secs, want)
			}
			if hdr.Get("x-should-retry") != "false" {
				t.Errorf("x-should-retry = %q, want false", hdr.Get("x-should-retry"))
			}
		})
	}
}

// TestCostEnforcement_UnpricedModelNeverUsesUpCostBudget verifies that a
// model without prices adds no cost, so the smallest possible cost limit on
// every scope never blocks it, while its tokens are still counted.
func TestCostEnforcement_UnpricedModelNeverUsesUpCostBudget(t *testing.T) {
	t.Parallel()
	for _, ep := range ceEndpoints {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", ep.name, stream), func(t *testing.T) {
				t.Parallel()
				env, up := newCEEnv(t, ep, stream, ceLicensed(), func(k *auth.KeyInfo) {
					for _, scope := range ceScopes {
						scope.daily(k, 1)
					}
					k.MonthlyCostLimit, k.UserMonthlyCostLimit = 1, 1
					k.TeamMonthlyCostLimit, k.OrgMonthlyCostLimit = 1, 1
				})
				perRequest := int64(2000)
				if stream {
					perRequest = 16
				}

				for i := int64(1); i <= 3; i++ {
					if status, _, body := cePost(t, env, ep.path, ep.body(ceUnpriced, stream)); status != http.StatusOK {
						t.Fatalf("request %d to the unpriced model: status %d body %s, want 200", i, status, body)
					}
					waitFor(t, "tokens of the unpriced model counted", func() bool {
						return ceUsage(t, env)[3].Tokens == i*perRequest
					})
				}
				for i, u := range ceUsage(t, env) {
					if u.CostMicros != 0 {
						t.Errorf("scope %s: cost = %d, want 0 for a model without prices", ceScopes[i].name, u.CostMicros)
					}
				}
				if got := up.hits.Load(); got != 3 {
					t.Errorf("upstream hits = %d, want 3", got)
				}
			})
		}
	}
}

// TestCostEnforcement_CountTokens verifies that count_tokens adds no cost and
// is rejected like any other request once a cost budget is spent.
func TestCostEnforcement_CountTokens(t *testing.T) {
	t.Parallel()

	up := newNHUpstream(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/messages/count_tokens" {
			fmt.Fprint(w, `{"input_tokens":500}`)
			return
		}
		fmt.Fprint(w, messagesJSON(1000, 1000))
	})
	priced := anthModel(cePriced, up.srv.URL)
	priced.Pricing = cePricing
	env := newNHEnv(t, nhOpts{
		budgetCounter: true,
		models:        []config.ModelConfig{priced},
		mutateKey: func(k *auth.KeyInfo) {
			k.UserID, k.TeamID = ceUserID, ceTeamID
			k.DailyCostLimit = ceJSONCost
		},
	})
	env.h.License = ceLicensed()

	for i := 0; i < 3; i++ {
		if status, _, body := cePost(t, env, "/v1/messages/count_tokens", messagesBody(cePriced, false)); status != http.StatusOK {
			t.Fatalf("count_tokens #%d within budget: status %d body %s", i, status, body)
		}
	}
	if status, _, body := cePost(t, env, "/v1/messages", messagesBody(cePriced, false)); status != http.StatusOK {
		t.Fatalf("messages: status %d body %s", status, body)
	}
	// Exactly the cost of the one generation: count_tokens added nothing.
	waitCECost(t, env, ceJSONCost)

	status, hdr, body := cePost(t, env, "/v1/messages/count_tokens", messagesBody(cePriced, false))
	if status != http.StatusTooManyRequests {
		t.Fatalf("count_tokens over the cost budget: status %d body %s, want 429", status, body)
	}
	top, detail := ceError(t, body)
	if top != "error" || detail["type"] != "rate_limit_error" || detail["message"] != "cost budget exceeded" {
		t.Errorf("count_tokens rejection = %s, want an Anthropic rate_limit_error envelope", body)
	}
	if hdr.Get("x-should-retry") != "false" || hdr.Get("Retry-After") == "" {
		t.Errorf("x-should-retry = %q, Retry-After = %q", hdr.Get("x-should-retry"), hdr.Get("Retry-After"))
	}
	if got := up.hits.Load(); got != 4 {
		t.Errorf("upstream hits = %d, want 4", got)
	}
}

// TestCostEnforcement_RejectionMetric verifies that a cost rejection is
// counted under the "cost" label. Other tests reject in parallel, so the
// counter is only required to grow.
func TestCostEnforcement_RejectionMetric(t *testing.T) {
	t.Parallel()
	ep := ceEndpoints[0]
	env, _ := newCEEnv(t, ep, false, ceLicensed(), func(k *auth.KeyInfo) { k.DailyCostLimit = ceJSONCost })
	if status, _, body := cePost(t, env, ep.path, ep.body(cePriced, false)); status != http.StatusOK {
		t.Fatalf("first request: status %d body %s", status, body)
	}
	waitCECost(t, env, ceJSONCost)

	before := testutil.ToFloat64(metrics.RateLimitRejectionsTotal.WithLabelValues("cost"))
	if status, _, body := cePost(t, env, ep.path, ep.body(cePriced, false)); status != http.StatusTooManyRequests {
		t.Fatalf("status %d body %s, want 429", status, body)
	}
	if after := testutil.ToFloat64(metrics.RateLimitRejectionsTotal.WithLabelValues("cost")); after < before+1 {
		t.Errorf(`rejections{scope="cost"} went from %v to %v, want an increase of at least 1`, before, after)
	}
}
