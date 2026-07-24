package proxy

// cooldown_failover_test.go covers the 429 failover + cooldown feature
// (GitHub issue #184): a 429 from an upstream deployment triggers failover to
// the next candidate (or fallback model), is recorded in the cooldown
// registry so the router deprioritizes that deployment on subsequent
// requests, and is treated as NEUTRAL by the circuit breaker — neither a
// success nor a failure.

import (
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
	"github.com/voidmind-io/voidllm/internal/cooldown"
)

// newTestCircuitBreakers returns a circuit breaker registry configured with
// the given failure threshold and a Timeout long enough that the circuit
// never auto-recovers mid-test.
func newTestCircuitBreakers(threshold int) *circuitbreaker.Registry {
	return circuitbreaker.NewRegistry(circuitbreaker.Config{
		Threshold:   threshold,
		Timeout:     10 * time.Minute,
		HalfOpenMax: 1,
	})
}

// twoDeploymentRegistry builds a Registry containing a single model with two
// explicit deployments, following the construction pattern used by
// TestPII_VULN001_MultiDeploymentPerDeploymentBodySelection in
// pii_handler_test.go.
func twoDeploymentRegistry(modelName string, dep1, dep2 Deployment) *Registry {
	model := Model{
		Name:        modelName,
		Provider:    dep1.Provider,
		BaseURL:     dep1.BaseURL,
		APIKey:      dep1.APIKey,
		Strategy:    "priority",
		Deployments: []Deployment{dep1, dep2},
	}
	return &Registry{
		models:  map[string]*Model{modelName: &model},
		aliases: make(map[string]string),
	}
}

// rateLimitedServer returns an httptest.Server that always responds 429 and
// increments *calls on every request.
func rateLimitedServer(t *testing.T, calls *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"error":{"message":"rate limited"}}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// okServer returns an httptest.Server that always responds 200 with a minimal
// chat-completion body and increments *calls on every request.
func okServer(t *testing.T, calls *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"cmp-ok","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ── Failover on 429 ──────────────────────────────────────────────────────────

// TestHandle_429_FailsOverToNextDeployment verifies that a 429 from the first
// deployment candidate causes the proxy to retry the next candidate, and that
// the client receives that candidate's successful response.
func TestHandle_429_FailsOverToNextDeployment(t *testing.T) {
	t.Parallel()

	var dep1Calls, dep2Calls int32
	srv1 := rateLimitedServer(t, &dep1Calls)
	srv2 := okServer(t, &dep2Calls)

	dep1 := Deployment{Name: "dep-1", Provider: "openai", BaseURL: srv1.URL, APIKey: "k1", Priority: 1}
	dep2 := Deployment{Name: "dep-2", Provider: "openai", BaseURL: srv2.URL, APIKey: "k2", Priority: 2}

	reg := twoDeploymentRegistry("failover-model", dep1, dep2)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.Router = &staticDeploymentPicker{deps: []Deployment{dep1, dep2}}
	handler.CircuitBreakers = newTestCircuitBreakers(1)
	handler.Cooldowns = cooldown.NewRegistry()
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"failover-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}
	if atomic.LoadInt32(&dep1Calls) == 0 {
		t.Error("rate-limited deployment (dep-1) was never called")
	}
	if atomic.LoadInt32(&dep2Calls) == 0 {
		t.Error("second deployment (dep-2) was never called")
	}
}

// ── Circuit breaker neutrality ───────────────────────────────────────────────

// TestHandle_429_LeavesBreakerClosed verifies that a 429 response does not
// trip the circuit breaker for the deployment that returned it. The registry
// uses Threshold=1 so that a single (incorrect) RecordFailure call would be
// immediately observable as an Open breaker.
func TestHandle_429_LeavesBreakerClosed(t *testing.T) {
	t.Parallel()

	var dep1Calls, dep2Calls int32
	srv1 := rateLimitedServer(t, &dep1Calls)
	srv2 := okServer(t, &dep2Calls)

	dep1 := Deployment{Name: "dep-1", Provider: "openai", BaseURL: srv1.URL, APIKey: "k1", Priority: 1}
	dep2 := Deployment{Name: "dep-2", Provider: "openai", BaseURL: srv2.URL, APIKey: "k2", Priority: 2}

	reg := twoDeploymentRegistry("breaker-neutral-model", dep1, dep2)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.Router = &staticDeploymentPicker{deps: []Deployment{dep1, dep2}}
	handler.CircuitBreakers = newTestCircuitBreakers(1)
	handler.Cooldowns = cooldown.NewRegistry()
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"breaker-neutral-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	depKey := deploymentKey("breaker-neutral-model", "dep-1")
	state := handler.CircuitBreakers.Get(depKey).CurrentState()
	if state != circuitbreaker.Closed {
		t.Errorf("breaker for %q state = %v after a single 429, want Closed (429 must not record a breaker failure)", depKey, state)
	}
}

// TestHandle_429_DoesNotResetFailureCounter is the subtle regression guard:
// it proves that a 429 neither increments NOR resets the circuit breaker's
// failure counter. A deployment breaker starts with one pre-recorded failure
// (below the Threshold=2 trip point). A 429 is sent to it — this must be a
// pure no-op for the breaker. A genuine failure (5xx) is then sent to the
// same deployment; if the 429 had incorrectly called RecordSuccess (which
// resets the counter to zero), this second failure would only bring the
// count to 1 and the breaker would stay Closed. If the 429 had incorrectly
// called RecordFailure, the breaker would already have tripped Open before
// the second request is even sent. Only the correct "neutral" behaviour
// leaves the breaker Closed after the pre-existing failure + 429, and Open
// after the pre-existing failure + 429 + one genuine failure.
func TestHandle_429_DoesNotResetFailureCounter(t *testing.T) {
	t.Parallel()

	// respondStatus is flipped between requests: first 429, then 500.
	var respondStatus int32 = http.StatusTooManyRequests
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(atomic.LoadInt32(&respondStatus)))
		fmt.Fprint(w, `{"error":{"message":"upstream"}}`)
	}))
	t.Cleanup(srv.Close)

	const modelName = "counter-model"
	reg := singleDeploymentRegistry(modelName, "openai", srv.URL)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.CircuitBreakers = newTestCircuitBreakers(2)
	handler.Cooldowns = cooldown.NewRegistry()
	app := testApp(t, handler)

	// Single-deployment synthesis uses model.Name as the deployment name, so
	// the breaker key equals the model name (see deploymentKey).
	depKey := deploymentKey(modelName, modelName)

	// Pre-record one failure directly on the breaker: failures=1, still Closed.
	handler.CircuitBreakers.Get(depKey).RecordFailure()
	if state := handler.CircuitBreakers.Get(depKey).CurrentState(); state != circuitbreaker.Closed {
		t.Fatalf("setup: breaker state = %v after 1/2 failures, want Closed", state)
	}

	// Request 1: upstream returns 429. Must be neutral (neither
	// RecordSuccess nor RecordFailure).
	req1 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+modelName+`","messages":[]}`))
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := app.Test(req1, testTimeout)
	if err != nil {
		t.Fatalf("app.Test (429): %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request 1 status = %d, want 429", resp1.StatusCode)
	}
	if state := handler.CircuitBreakers.Get(depKey).CurrentState(); state != circuitbreaker.Closed {
		t.Fatalf("breaker state after 429 = %v, want Closed", state)
	}

	// Request 2: upstream now returns 500 — a genuine failure.
	atomic.StoreInt32(&respondStatus, http.StatusInternalServerError)
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+modelName+`","messages":[]}`))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := app.Test(req2, testTimeout)
	if err != nil {
		t.Fatalf("app.Test (500): %v", err)
	}
	resp2.Body.Close()

	// The breaker must now be Open: pre-existing failure (1) + this genuine
	// failure (2) reaches Threshold=2. If the 429 had reset the counter via
	// RecordSuccess, this would still be Closed (count=1).
	if state := handler.CircuitBreakers.Get(depKey).CurrentState(); state != circuitbreaker.Open {
		t.Errorf("breaker state after pre-failure + 429 + genuine failure = %v, want Open "+
			"(a 429 must not reset the failure counter via RecordSuccess)", state)
	}
}

// singleDeploymentRegistry builds a Registry containing a single
// no-Deployments-slice model backed by upstreamURL, mirroring the shape
// tryModel synthesizes a single candidate from.
func singleDeploymentRegistry(modelName, provider, upstreamURL string) *Registry {
	model := Model{
		Name:     modelName,
		Provider: provider,
		BaseURL:  upstreamURL,
		APIKey:   "key",
	}
	return &Registry{
		models:  map[string]*Model{modelName: &model},
		aliases: make(map[string]string),
	}
}

// ── All candidates rate limited ──────────────────────────────────────────────

// TestHandle_429_AllCandidatesRateLimited verifies that when every deployment
// candidate returns 429, the client receives 429 — not a 502 (upstream
// unavailable) or 503 (circuit open) synthetic error.
func TestHandle_429_AllCandidatesRateLimited(t *testing.T) {
	t.Parallel()

	var dep1Calls, dep2Calls int32
	srv1 := rateLimitedServer(t, &dep1Calls)
	srv2 := rateLimitedServer(t, &dep2Calls)

	dep1 := Deployment{Name: "dep-1", Provider: "openai", BaseURL: srv1.URL, APIKey: "k1", Priority: 1}
	dep2 := Deployment{Name: "dep-2", Provider: "openai", BaseURL: srv2.URL, APIKey: "k2", Priority: 2}

	reg := twoDeploymentRegistry("all-limited-model", dep1, dep2)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.Router = &staticDeploymentPicker{deps: []Deployment{dep1, dep2}}
	handler.CircuitBreakers = newTestCircuitBreakers(1)
	handler.Cooldowns = cooldown.NewRegistry()
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"all-limited-model","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusTooManyRequests {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 429; body: %s", resp.StatusCode, body)
	}
	if atomic.LoadInt32(&dep1Calls) == 0 {
		t.Error("dep-1 was never called")
	}
	if atomic.LoadInt32(&dep2Calls) == 0 {
		t.Error("dep-2 was never called")
	}
}

// ── Cooldown marking ─────────────────────────────────────────────────────────

// TestHandle_429_MarksCooldown verifies that a 429 from a deployment marks
// that deployment as cooling in the shared cooldown.Registry, keyed exactly
// as deploymentKey builds it.
func TestHandle_429_MarksCooldown(t *testing.T) {
	t.Parallel()

	var dep1Calls, dep2Calls int32
	srv1 := rateLimitedServer(t, &dep1Calls)
	srv2 := okServer(t, &dep2Calls)

	dep1 := Deployment{Name: "dep-1", Provider: "openai", BaseURL: srv1.URL, APIKey: "k1", Priority: 1}
	dep2 := Deployment{Name: "dep-2", Provider: "openai", BaseURL: srv2.URL, APIKey: "k2", Priority: 2}

	const modelName = "cooldown-mark-model"
	reg := twoDeploymentRegistry(modelName, dep1, dep2)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.Router = &staticDeploymentPicker{deps: []Deployment{dep1, dep2}}
	handler.CircuitBreakers = newTestCircuitBreakers(1)
	handler.Cooldowns = cooldown.NewRegistry()
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+modelName+`","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	resp.Body.Close()

	depKey := deploymentKey(modelName, "dep-1")
	if !handler.Cooldowns.Cooling(depKey) {
		t.Errorf("Cooldowns.Cooling(%q) = false after a 429, want true", depKey)
	}

	// The deployment that succeeded must NOT be marked cooling.
	okKey := deploymentKey(modelName, "dep-2")
	if handler.Cooldowns.Cooling(okKey) {
		t.Errorf("Cooldowns.Cooling(%q) = true for the successful deployment, want false", okKey)
	}
}

// ── retryAfterOrDefault ──────────────────────────────────────────────────────

// TestRetryAfterOrDefault covers header parsing, precedence, and clamping.
func TestRetryAfterOrDefault(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		headers map[string]string
		nilResp bool
		want    time.Duration
	}{
		{
			name:    "nil response uses default",
			nilResp: true,
			want:    defaultRateLimitCooldown,
		},
		{
			name:    "no headers present uses default",
			headers: map[string]string{},
			want:    defaultRateLimitCooldown,
		},
		{
			name:    "retry-after-ms present",
			headers: map[string]string{"retry-after-ms": "1500"},
			want:    1500 * time.Millisecond,
		},
		{
			name:    "retry-after-ms takes precedence over Retry-After",
			headers: map[string]string{"retry-after-ms": "2000", "Retry-After": "30"},
			want:    2 * time.Second,
		},
		{
			name:    "Retry-After delta-seconds form",
			headers: map[string]string{"Retry-After": "5"},
			want:    5 * time.Second,
		},
		{
			name:    "Retry-After unparseable garbage uses default",
			headers: map[string]string{"Retry-After": "not-a-valid-value"},
			want:    defaultRateLimitCooldown,
		},
		{
			name:    "retry-after-ms far above max clamps",
			headers: map[string]string{"retry-after-ms": "999999999"},
			want:    maxRateLimitCooldown,
		},
		{
			name:    "retry-after-ms negative falls through to default",
			headers: map[string]string{"retry-after-ms": "-100"},
			want:    defaultRateLimitCooldown,
		},
		{
			name:    "retry-after-ms zero is honored (not clamped to default)",
			headers: map[string]string{"retry-after-ms": "0"},
			want:    0,
		},
		{
			name:    "Retry-After negative delta-seconds falls through to default",
			headers: map[string]string{"Retry-After": "-5"},
			want:    defaultRateLimitCooldown,
		},
		{
			// Regression guard for the int64-overflow bug the bounds check in
			// retryAfterOrDefault exists to prevent: 9999999999 seconds,
			// multiplied by time.Second, overflows int64 and wraps to a
			// negative duration, which clampCooldown would then turn into 0 —
			// no cooldown at all, the exact opposite of the intent of a large
			// Retry-After value. The pre-multiplication bounds check against
			// maxRateLimitCooldownSecs must catch this before the overflow
			// happens.
			name:    "Retry-After large enough to overflow int64 seconds clamps to max",
			headers: map[string]string{"Retry-After": "9999999999"},
			want:    maxRateLimitCooldown,
		},
		{
			// The extreme case of the same overflow: math.MaxInt64 itself.
			name:    "Retry-After math.MaxInt64 clamps to max",
			headers: map[string]string{"Retry-After": "9223372036854775807"},
			want:    maxRateLimitCooldown,
		},
		{
			// Same overflow regression, but for the retry-after-ms branch:
			// multiplying by time.Millisecond instead of time.Second.
			name:    "retry-after-ms large enough to overflow int64 ms clamps to max",
			headers: map[string]string{"retry-after-ms": "9999999999999999"},
			want:    maxRateLimitCooldown,
		},
		{
			// Pins the ordinary clamp boundary (a value just above the cap,
			// far from the overflow range) as distinct from the overflow
			// guard above: deleting the bounds check would not overflow here,
			// so this case alone would not catch that mutation, but it
			// verifies the "> max clamps" behavior holds independent of
			// overflow.
			name:    "Retry-After just above cap in seconds clamps to max",
			headers: map[string]string{"Retry-After": fmt.Sprintf("%d", maxRateLimitCooldownSecs+1)},
			want:    maxRateLimitCooldown,
		},
		{
			// Exactly at the cap in seconds: secs > maxRateLimitCooldownSecs
			// is false, so this falls through to clampCooldown(secs *
			// time.Second), which multiplies out to exactly
			// maxRateLimitCooldown and clampCooldown's d > maxRateLimitCooldown
			// check leaves it unchanged. The boundary is inclusive on the
			// non-clamping side of the pre-multiplication check, but the
			// numeric result is the same maxRateLimitCooldown value either
			// way.
			name:    "Retry-After exactly at cap in seconds is honored",
			headers: map[string]string{"Retry-After": fmt.Sprintf("%d", maxRateLimitCooldownSecs)},
			want:    maxRateLimitCooldown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var resp *http.Response
			if !tc.nilResp {
				h := make(http.Header)
				for k, v := range tc.headers {
					h.Set(k, v)
				}
				resp = &http.Response{Header: h}
			}

			got := retryAfterOrDefault(resp)
			if got != tc.want {
				t.Errorf("retryAfterOrDefault() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRetryAfterOrDefault_HTTPDate covers the RFC 7231 HTTP-date form of the
// Retry-After header specifically: past date, near-future date (not
// clamped), and far-future date (must clamp to maxRateLimitCooldown).
//
// Unlike the table-driven cases above, these deliberately do not assert
// exact equality against a duration precomputed from time.Now(): parsing an
// HTTP-date internally calls time.Until, which reads the wall clock again at
// call time, so any expected value baked in at header-construction time
// would be racing the clock. The past- and far-future cases stay exact
// because their expected results are the two saturating clamp bounds (0 and
// maxRateLimitCooldown) — values wide enough that ordinary test-execution
// jitter can never cross the clamp boundary. The near-future case is the one
// that actually carries an unclamped, clock-derived value, so it asserts a
// bound instead of exact equality.
func TestRetryAfterOrDefault_HTTPDate(t *testing.T) {
	t.Parallel()

	t.Run("past date clamps to zero", func(t *testing.T) {
		t.Parallel()

		h := make(http.Header)
		h.Set("Retry-After", time.Now().Add(-24*time.Hour).UTC().Format(http.TimeFormat))
		resp := &http.Response{Header: h}

		got := retryAfterOrDefault(resp)
		if got != 0 {
			t.Errorf("retryAfterOrDefault() = %v, want 0 (date is in the past)", got)
		}
	})

	t.Run("near-future date within cap is honored", func(t *testing.T) {
		t.Parallel()

		const target = 10 * time.Second
		h := make(http.Header)
		h.Set("Retry-After", time.Now().Add(target).UTC().Format(http.TimeFormat))
		resp := &http.Response{Header: h}

		got := retryAfterOrDefault(resp)

		// The header only carries second resolution and some wall-clock time
		// elapses between construction and parsing, so bound the result
		// instead of asserting it equals target exactly: it must be positive,
		// must not exceed target, and must not be more than a generous
		// tolerance below it.
		const tolerance = 3 * time.Second
		if got <= 0 || got > target || target-got > tolerance {
			t.Errorf("retryAfterOrDefault() = %v, want within (%v, %v]", got, target-tolerance, target)
		}
	})

	t.Run("far-future date clamps to max", func(t *testing.T) {
		t.Parallel()

		h := make(http.Header)
		h.Set("Retry-After", time.Now().Add(24*time.Hour).UTC().Format(http.TimeFormat))
		resp := &http.Response{Header: h}

		got := retryAfterOrDefault(resp)
		if got != maxRateLimitCooldown {
			t.Errorf("retryAfterOrDefault() = %v, want %v (must clamp)", got, maxRateLimitCooldown)
		}
	})
}

// ── Model-level fallback on 429 ──────────────────────────────────────────────

// TestHandle_429_TriggersModelFallback verifies that a 429 on model A's only
// deployment triggers the model-level fallback chain to model B, mirroring
// TestHandle_FallbackOn500's pattern but for a rate-limit response.
func TestHandle_429_TriggersModelFallback(t *testing.T) {
	t.Parallel()

	upstreamA, _ := upstreamCapture(t, http.StatusTooManyRequests,
		`{"error":{"message":"rate limited"}}`,
		map[string]string{"Content-Type": "application/json"},
	)
	upstreamB, _ := upstreamCapture(t, http.StatusOK,
		`{"id":"cmp-3","object":"chat.completion","choices":[]}`,
		map[string]string{"Content-Type": "application/json"},
	)

	reg := testRegistryWithFallback(t, upstreamA.URL, upstreamB.URL)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.FallbackMaxDepth = 1
	handler.Cooldowns = cooldown.NewRegistry()
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"model-a","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, testTimeout)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, http.StatusOK, body)
	}
}
