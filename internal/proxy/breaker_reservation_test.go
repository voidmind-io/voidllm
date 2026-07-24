package proxy

// breaker_reservation_test.go pins the Allow()/Record* balance invariant
// documented on circuitbreaker.Breaker.Allow: every Allow() call that
// returns true reserves a HalfOpen probe slot that only RecordSuccess,
// RecordFailure, or RecordNeutral releases. tryModel must therefore call
// Allow() exactly once per candidate it actually attempts — never for
// candidates it only considers and never twice for one it does attempt — and
// the streaming client-disconnect path must release the reservation via
// RecordNeutral() rather than leaving it dangling. Violating either half of
// this permanently locks a HalfOpenMax=1 deployment out of ever probing
// again.

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

// halfOpenBreakerWithFreeSlot returns a circuitbreaker.Registry (HalfOpenMax=1)
// whose breaker for key is already in HalfOpen state with its single probe
// slot free (halfOpenActive=0): tripped via one RecordFailure (Threshold=1),
// advanced past a short Timeout, admitted through Allow() once to perform the
// Open->HalfOpen transition and reserve the slot, then immediately released
// via RecordNeutral() so the slot is free again for the test to observe being
// consumed (or not) by the code path under test. This is the same
// construction internal/circuitbreaker's TestPermits_HalfOpen_DoesNotMutate
// uses to reach a HalfOpen-with-free-slot state.
func halfOpenBreakerWithFreeSlot(t *testing.T, key string) *circuitbreaker.Registry {
	t.Helper()
	const timeout = 10 * time.Millisecond
	reg := circuitbreaker.NewRegistry(circuitbreaker.Config{
		Enabled:     true,
		Threshold:   1,
		Timeout:     timeout,
		HalfOpenMax: 1,
	})
	b := reg.Get(key)
	b.RecordFailure()
	time.Sleep(timeout + 15*time.Millisecond)
	if !b.Allow() {
		t.Fatalf("setup: Allow() for %q did not admit the HalfOpen probe", key)
	}
	b.RecordNeutral()
	if state := b.CurrentState(); state != circuitbreaker.HalfOpen {
		t.Fatalf("setup: breaker for %q state = %v, want HalfOpen", key, state)
	}
	return reg
}

// ── Reserved but never attempted ─────────────────────────────────────────

// TestHandle_HalfOpenBreaker_UnattemptedCandidate_SlotNotConsumed verifies
// that a candidate which the router lists but tryModel never actually tries
// — because an earlier candidate already succeeded — leaves that candidate's
// breaker reservation completely untouched. dep-2's breaker starts HalfOpen
// with its one free slot; dep-1 succeeds first, so dep-2 must never be
// attempted (and never call Allow()), and the slot must still be available
// afterward.
func TestHandle_HalfOpenBreaker_UnattemptedCandidate_SlotNotConsumed(t *testing.T) {
	t.Parallel()

	var dep1Calls, dep2Calls int32
	srv1 := okServer(t, &dep1Calls)
	srv2 := okServer(t, &dep2Calls) // must never actually be hit

	dep1 := Deployment{Name: "dep-1", Provider: "openai", BaseURL: srv1.URL, APIKey: "k1", Priority: 1}
	dep2 := Deployment{Name: "dep-2", Provider: "openai", BaseURL: srv2.URL, APIKey: "k2", Priority: 2}

	const modelName = "unattempted-candidate-model"
	reg := twoDeploymentRegistry(modelName, dep1, dep2)
	handler := NewProxyHandler(reg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler.Router = &staticDeploymentPicker{deps: []Deployment{dep1, dep2}}

	dep2Key := deploymentKey(modelName, "dep-2")
	handler.CircuitBreakers = halfOpenBreakerWithFreeSlot(t, dep2Key)
	handler.Cooldowns = cooldown.NewRegistry()
	app := testApp(t, handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"`+modelName+`","messages":[]}`))
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
		t.Fatal("dep-1 was never called")
	}
	if atomic.LoadInt32(&dep2Calls) != 0 {
		t.Fatal("dep-2 was called — it should never have been attempted since dep-1 succeeded first")
	}

	dep2Breaker := handler.CircuitBreakers.Get(dep2Key)
	if !dep2Breaker.Allow() {
		t.Error("dep-2's HalfOpen probe slot was consumed even though dep-2 was never attempted — " +
			"a reservation may only be taken at the moment a candidate is actually tried, " +
			"never merely because it was considered")
	}
}

// ── Streaming client disconnect releases the reservation ────────────────

// TestHandle_StreamingClientDisconnect_ReleasesHalfOpenReservation mirrors
// TestStreamTermination_ClientDisconnect_NoScannerError_IsDisconnect
// (stream_termination_matrix_test.go) but starts the deployment's breaker
// HalfOpen with its single slot already reserved-then-released (i.e. free),
// and asserts that after the client disconnects mid-stream, the slot Allow()
// takes for the request is released via RecordNeutral() — not left
// dangling. Before the fix, the clientDisconnected case in
// handleStreamingResponse recorded nothing, so this reservation would stay
// consumed forever and a HalfOpenMax=1 deployment would never be probed
// again.
func TestHandle_StreamingClientDisconnect_ReleasesHalfOpenReservation(t *testing.T) {
	t.Parallel()

	upstreamCanceled := make(chan struct{})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("upstream: no Flusher")
			return
		}
		fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"a"}}]}`)
		fmt.Fprintln(w)
		flusher.Flush()

		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				close(upstreamCanceled)
				return
			case <-ticker.C:
				fmt.Fprintln(w, `data: {"choices":[{"delta":{"content":"b"}}]}`)
				fmt.Fprintln(w)
				flusher.Flush()
			}
		}
	}))
	t.Cleanup(upstream.Close)

	const modelName = "half-open-disconnect-model"
	reg := terminationRegistry(t, modelName, upstream.URL)
	h, _, d := newTerminationTestHandler(t, reg)

	// Override the Threshold=1/Timeout=30s registry newTerminationTestHandler
	// wires by default with one whose breaker is already HalfOpen with its
	// single slot free, so the request under test is the one that reserves
	// (via Allow()) and must release (via RecordNeutral()) that exact slot.
	depKey := deploymentKey(modelName, modelName)
	h.CircuitBreakers = halfOpenBreakerWithFreeSlot(t, depKey)

	baseURL, rawKey := startAuthedTestServer(t, h, terminationKeyInfo("org-half-open-disconnect"))
	streamRequestThenAbruptlyDisconnect(t, baseURL, rawKey, modelName)

	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream never observed request cancellation")
	}

	// waitForLoggedStatusCode's DB poll happens-after handleStreamingResponse's
	// SendStreamWriter goroutine finishes (see its own doc comment for why
	// that ordering is safe to rely on here), so by the time it returns, any
	// breaker recording from the disconnect path has already completed.
	if status := waitForLoggedStatusCode(t, d); status != http.StatusOK {
		t.Errorf("usage status_code = %d, want 200 (disconnect keeps the upstream status, not 502)", status)
	}

	if !h.CircuitBreakers.Get(depKey).Allow() {
		t.Error("dep breaker's HalfOpen probe slot was not released after a client disconnect — " +
			"the streaming client-disconnect path must call RecordNeutral() to balance the Allow() reservation")
	}
}
