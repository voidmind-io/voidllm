package circuitbreaker_test

// breaker_test.go is the first test file for internal/circuitbreaker. It
// covers the classic Closed -> Open -> HalfOpen -> Closed state machine and,
// in particular, RecordNeutral: an outcome (e.g. an upstream HTTP 429) that
// must neither trip nor heal the breaker but, while HalfOpen, must still
// release the probe-slot reservation Allow() took for the request so a
// future request can probe again. Without that release, halfOpenActive stays
// elevated forever and the breaker locks itself out of ever leaving
// HalfOpen — Allow() returns false permanently.
//
// Only the exported API (plus CurrentState, which is exported for exactly
// this purpose) is used, so this file lives in the black-box
// circuitbreaker_test package rather than reaching into unexported fields.

import (
	"sync"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/circuitbreaker"
)

// ── Basic state machine (context for the RecordNeutral fix) ─────────────────

// TestAllow_Closed_AlwaysTrue verifies that a freshly constructed breaker
// starts Closed and admits every request.
func TestAllow_Closed_AlwaysTrue(t *testing.T) {
	t.Parallel()

	b := circuitbreaker.NewBreaker(circuitbreaker.Config{
		Threshold:   3,
		Timeout:     time.Minute,
		HalfOpenMax: 1,
	})

	for i := 0; i < 5; i++ {
		if !b.Allow() {
			t.Fatalf("Allow() call %d = false, want true (Closed state)", i)
		}
	}
	if state := b.CurrentState(); state != circuitbreaker.Closed {
		t.Errorf("CurrentState() = %v, want Closed", state)
	}
}

// TestRecordFailure_Closed_TripsAtExactThreshold verifies that the breaker
// opens exactly when the consecutive-failure count reaches Threshold, not
// before and not after.
func TestRecordFailure_Closed_TripsAtExactThreshold(t *testing.T) {
	t.Parallel()

	const threshold = 3
	b := circuitbreaker.NewBreaker(circuitbreaker.Config{
		Threshold:   threshold,
		Timeout:     time.Minute,
		HalfOpenMax: 1,
	})

	for i := 1; i < threshold; i++ {
		b.RecordFailure()
		if state := b.CurrentState(); state != circuitbreaker.Closed {
			t.Fatalf("CurrentState() after %d/%d failures = %v, want Closed", i, threshold, state)
		}
	}
	b.RecordFailure() // the threshold-th failure
	if state := b.CurrentState(); state != circuitbreaker.Open {
		t.Fatalf("CurrentState() after %d/%d failures = %v, want Open", threshold, threshold, state)
	}
}

// TestAllow_Open_BlocksUntilTimeout verifies that Allow() rejects requests
// while Open and admits exactly one probe once Timeout has elapsed,
// transitioning to HalfOpen.
func TestAllow_Open_BlocksUntilTimeout(t *testing.T) {
	t.Parallel()

	const timeout = 20 * time.Millisecond
	b := circuitbreaker.NewBreaker(circuitbreaker.Config{
		Threshold:   1,
		Timeout:     timeout,
		HalfOpenMax: 1,
	})

	b.RecordFailure() // trips at threshold 1
	if state := b.CurrentState(); state != circuitbreaker.Open {
		t.Fatalf("setup: CurrentState() = %v, want Open", state)
	}
	if b.Allow() {
		t.Error("Allow() immediately after trip = true, want false (still within Timeout)")
	}

	time.Sleep(timeout + 15*time.Millisecond)

	if !b.Allow() {
		t.Fatal("Allow() after Timeout elapsed = false, want true (transition to HalfOpen)")
	}
	if state := b.CurrentState(); state != circuitbreaker.HalfOpen {
		t.Errorf("CurrentState() after post-timeout Allow() = %v, want HalfOpen", state)
	}
}

// TestRecordSuccess_HalfOpen_ClosesAndResetsFailures verifies that a success
// while HalfOpen closes the circuit.
func TestRecordSuccess_HalfOpen_ClosesAndResetsFailures(t *testing.T) {
	t.Parallel()

	b := circuitbreaker.NewBreaker(circuitbreaker.Config{
		Threshold:   1,
		Timeout:     10 * time.Millisecond,
		HalfOpenMax: 1,
	})

	b.RecordFailure()
	time.Sleep(25 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("setup: Allow() did not admit the HalfOpen probe")
	}

	b.RecordSuccess()
	if state := b.CurrentState(); state != circuitbreaker.Closed {
		t.Errorf("CurrentState() after HalfOpen success = %v, want Closed", state)
	}
}

// TestRecordFailure_HalfOpen_ReopensImmediately verifies that a failed probe
// while HalfOpen reopens the circuit regardless of Threshold.
func TestRecordFailure_HalfOpen_ReopensImmediately(t *testing.T) {
	t.Parallel()

	b := circuitbreaker.NewBreaker(circuitbreaker.Config{
		Threshold:   5, // large threshold — a single HalfOpen failure must still reopen
		Timeout:     10 * time.Millisecond,
		HalfOpenMax: 1,
	})

	b.RecordFailure()
	b.RecordFailure()
	b.RecordFailure()
	b.RecordFailure()
	b.RecordFailure() // reaches threshold 5, trips Open
	if state := b.CurrentState(); state != circuitbreaker.Open {
		t.Fatalf("setup: CurrentState() = %v, want Open", state)
	}

	time.Sleep(25 * time.Millisecond)
	if !b.Allow() {
		t.Fatal("setup: Allow() did not admit the HalfOpen probe")
	}

	b.RecordFailure()
	if state := b.CurrentState(); state != circuitbreaker.Open {
		t.Errorf("CurrentState() after HalfOpen failure = %v, want Open (immediate reopen, ignoring Threshold)", state)
	}
}

// ── RecordNeutral ─────────────────────────────────────────────────────────

// TestRecordNeutral_Closed_IsNoOp verifies that RecordNeutral in Closed
// state neither changes state nor touches the failure counter: a subsequent
// threshold-worth of RecordFailure calls still trips the breaker at exactly
// Threshold, proving the counter was untouched by the intervening neutral
// calls.
func TestRecordNeutral_Closed_IsNoOp(t *testing.T) {
	t.Parallel()

	const threshold = 3
	b := circuitbreaker.NewBreaker(circuitbreaker.Config{
		Threshold:   threshold,
		Timeout:     time.Minute,
		HalfOpenMax: 1,
	})

	// Interleave neutral calls with failures below the threshold: if
	// RecordNeutral incremented or reset the counter, this loop would trip
	// the breaker early or never.
	for i := 1; i < threshold; i++ {
		b.RecordNeutral()
		b.RecordFailure()
		b.RecordNeutral()
		if state := b.CurrentState(); state != circuitbreaker.Closed {
			t.Fatalf("CurrentState() after %d/%d failures (with interleaved neutrals) = %v, want Closed", i, threshold, state)
		}
	}

	b.RecordNeutral()
	b.RecordFailure() // the threshold-th failure
	if state := b.CurrentState(); state != circuitbreaker.Open {
		t.Errorf("CurrentState() after reaching threshold = %v, want Open (RecordNeutral must not have touched the failure counter)", state)
	}
}

// TestRecordNeutral_Open_StaysOpenAndBlocked verifies that RecordNeutral in
// Open state is a no-op: the circuit stays Open and Allow() keeps returning
// false until Timeout elapses, exactly as if RecordNeutral had never been
// called.
func TestRecordNeutral_Open_StaysOpenAndBlocked(t *testing.T) {
	t.Parallel()

	const timeout = 200 * time.Millisecond
	b := circuitbreaker.NewBreaker(circuitbreaker.Config{
		Threshold:   1,
		Timeout:     timeout,
		HalfOpenMax: 1,
	})

	b.RecordFailure() // trips at threshold 1
	if state := b.CurrentState(); state != circuitbreaker.Open {
		t.Fatalf("setup: CurrentState() = %v, want Open", state)
	}

	for i := 0; i < 5; i++ {
		b.RecordNeutral()
	}

	if state := b.CurrentState(); state != circuitbreaker.Open {
		t.Errorf("CurrentState() after RecordNeutral x5 in Open = %v, want Open", state)
	}
	if b.Allow() {
		t.Error("Allow() before Timeout elapsed (after RecordNeutral calls) = true, want false")
	}
}

// TestRecordNeutral_HalfOpen_ReleasesProbeReservation is the regression case
// for the fix: without RecordNeutral releasing the probe slot Allow() took,
// a HalfOpen breaker with HalfOpenMax=1 would lock itself out of ever
// probing again once a single neutral (e.g. 429) outcome was recorded —
// Allow() would return false forever. With the fix, RecordNeutral releases
// the reservation and a subsequent Allow() succeeds.
func TestRecordNeutral_HalfOpen_ReleasesProbeReservation(t *testing.T) {
	t.Parallel()

	const timeout = 10 * time.Millisecond
	b := circuitbreaker.NewBreaker(circuitbreaker.Config{
		Threshold:   1,
		Timeout:     timeout,
		HalfOpenMax: 1,
	})

	// Trip the breaker.
	b.RecordFailure()
	if state := b.CurrentState(); state != circuitbreaker.Open {
		t.Fatalf("setup: CurrentState() = %v, want Open", state)
	}

	// Wait past Timeout so the circuit is eligible to transition to
	// HalfOpen. A real sleep is used deliberately: this package has no
	// injectable clock.
	time.Sleep(timeout + 15*time.Millisecond)

	// Allow() transitions Open -> HalfOpen and consumes the single probe
	// slot (HalfOpenMax=1).
	if !b.Allow() {
		t.Fatal("first Allow() after Timeout = false, want true (enters HalfOpen and admits the probe)")
	}
	if state := b.CurrentState(); state != circuitbreaker.HalfOpen {
		t.Fatalf("CurrentState() after first Allow() = %v, want HalfOpen", state)
	}

	// With the single probe slot already consumed, a second Allow() must be
	// rejected — there is no free slot until the in-flight probe resolves.
	if b.Allow() {
		t.Fatal("second Allow() while the only probe slot is in flight = true, want false")
	}

	// The in-flight probe's outcome is neutral (e.g. the upstream returned
	// 429). RecordNeutral must release the reservation without changing
	// state.
	b.RecordNeutral()
	if state := b.CurrentState(); state != circuitbreaker.HalfOpen {
		t.Errorf("CurrentState() after RecordNeutral = %v, want HalfOpen (unchanged)", state)
	}

	// This is the bug the fix addresses: without RecordNeutral releasing the
	// reservation, this Allow() call returns false forever (halfOpenActive
	// stays at 1, permanently >= HalfOpenMax). With the fix it must return
	// true.
	if !b.Allow() {
		t.Fatal("Allow() after RecordNeutral released the probe slot = false, want true — " +
			"this is the exact regression RecordNeutral fixes: without it, a neutral outcome " +
			"(e.g. HTTP 429) during the HalfOpen probe permanently locks the breaker out of probing again")
	}
}

// TestRecordNeutral_HalfOpen_NeverGoesNegative verifies that calling
// RecordNeutral repeatedly while HalfOpen (more times than there are
// reserved probe slots to release) never drives halfOpenActive below zero.
// If it did, a subsequent Allow() would incorrectly permit unbounded
// concurrent probes instead of respecting HalfOpenMax.
func TestRecordNeutral_HalfOpen_NeverGoesNegative(t *testing.T) {
	t.Parallel()

	const timeout = 10 * time.Millisecond
	b := circuitbreaker.NewBreaker(circuitbreaker.Config{
		Threshold:   1,
		Timeout:     timeout,
		HalfOpenMax: 1,
	})

	b.RecordFailure()
	time.Sleep(timeout + 15*time.Millisecond)

	if !b.Allow() {
		t.Fatal("setup: Allow() did not admit the HalfOpen probe")
	}
	if state := b.CurrentState(); state != circuitbreaker.HalfOpen {
		t.Fatalf("setup: CurrentState() = %v, want HalfOpen", state)
	}

	// Call RecordNeutral far more times than there are outstanding
	// reservations (only one was ever taken). If halfOpenActive were
	// allowed to underflow below zero, the next two Allow() calls below
	// would both incorrectly return true (a negative count always compares
	// less than HalfOpenMax).
	for i := 0; i < 10; i++ {
		b.RecordNeutral()
	}

	if !b.Allow() {
		t.Fatal("Allow() after over-calling RecordNeutral = false, want true (one free slot)")
	}
	if b.Allow() {
		t.Error("second consecutive Allow() after over-calling RecordNeutral = true, want false " +
			"(halfOpenActive must be floored at zero, not negative, so HalfOpenMax=1 is still enforced)")
	}
}

// ── Permits ───────────────────────────────────────────────────────────────

// TestPermits_HalfOpen_DoesNotMutate is the regression case for the fix: a
// candidate-selection caller (e.g. router.filterAvailable) must be able to
// call Permits any number of times on a HalfOpen breaker with a free slot
// without consuming it — a subsequent Allow() must still see that slot and
// return true. Before Permits existed, the only way to peek was Allow()
// itself, which reserved the slot as a side effect and starved the real
// attempt.
func TestPermits_HalfOpen_DoesNotMutate(t *testing.T) {
	t.Parallel()

	const timeout = 10 * time.Millisecond
	b := circuitbreaker.NewBreaker(circuitbreaker.Config{
		Threshold:   1,
		Timeout:     timeout,
		HalfOpenMax: 1,
	})

	b.RecordFailure()
	time.Sleep(timeout + 15*time.Millisecond)

	// Enter HalfOpen via the sole Allow() call that reserves the one slot,
	// then immediately release it so the breaker is HalfOpen with one free
	// slot for the rest of the test.
	if !b.Allow() {
		t.Fatal("setup: Allow() did not admit the HalfOpen probe")
	}
	b.RecordNeutral()
	if state := b.CurrentState(); state != circuitbreaker.HalfOpen {
		t.Fatalf("setup: CurrentState() = %v, want HalfOpen", state)
	}

	// Repeated Permits() calls on a HalfOpen breaker with a free slot must
	// all report true and must never consume the slot.
	for i := 0; i < 10; i++ {
		if !b.Permits() {
			t.Fatalf("Permits() call %d = false, want true (HalfOpen with a free slot)", i)
		}
	}
	if state := b.CurrentState(); state != circuitbreaker.HalfOpen {
		t.Errorf("CurrentState() after repeated Permits() = %v, want HalfOpen (unchanged)", state)
	}

	// The slot must still be available: Allow() must succeed exactly as if
	// Permits() had never been called.
	if !b.Allow() {
		t.Fatal("Allow() after repeated Permits() calls = false, want true — " +
			"Permits() must never reserve a probe slot")
	}
}

// TestPermits_Open_BeforeTimeout_DoesNotTransition verifies that Permits on
// an Open breaker before the recovery timeout has elapsed returns false and,
// critically, does not transition the breaker to HalfOpen — unlike Allow,
// which would perform that transition and reserve the first probe slot.
// Repeated calls must leave the breaker Open and still blocked.
func TestPermits_Open_BeforeTimeout_DoesNotTransition(t *testing.T) {
	t.Parallel()

	const timeout = time.Hour // never elapses during this test
	b := circuitbreaker.NewBreaker(circuitbreaker.Config{
		Threshold:   1,
		Timeout:     timeout,
		HalfOpenMax: 1,
	})

	b.RecordFailure() // trips at threshold 1
	if state := b.CurrentState(); state != circuitbreaker.Open {
		t.Fatalf("setup: CurrentState() = %v, want Open", state)
	}

	for i := 0; i < 5; i++ {
		if b.Permits() {
			t.Errorf("Permits() call %d = true, want false (Open, timeout not elapsed)", i)
		}
	}
	if state := b.CurrentState(); state != circuitbreaker.Open {
		t.Errorf("CurrentState() after repeated Permits() = %v, want Open (must not transition to HalfOpen)", state)
	}

	// Allow() must still behave exactly as if Permits() were never called:
	// blocked, because the timeout genuinely has not elapsed.
	if b.Allow() {
		t.Error("Allow() after repeated Permits() calls = true, want false (timeout still not elapsed)")
	}
}

// TestPermits_AgreesWithAllow exercises all three states and asserts that
// Permits reports exactly what Allow would then do, for a breaker whose
// prior state Permits itself is guaranteed not to have disturbed (each
// subtest builds its own breaker to keep the two calls independent).
func TestPermits_AgreesWithAllow(t *testing.T) {
	t.Parallel()

	t.Run("Closed", func(t *testing.T) {
		t.Parallel()
		b := circuitbreaker.NewBreaker(circuitbreaker.Config{
			Threshold:   3,
			Timeout:     time.Minute,
			HalfOpenMax: 1,
		})
		permits := b.Permits()
		allow := b.Allow()
		if permits != allow {
			t.Errorf("Permits() = %v, Allow() = %v; want equal (Closed)", permits, allow)
		}
		if !permits {
			t.Error("Closed breaker: Permits() = false, want true")
		}
	})

	t.Run("Open before timeout", func(t *testing.T) {
		t.Parallel()
		b := circuitbreaker.NewBreaker(circuitbreaker.Config{
			Threshold:   1,
			Timeout:     time.Hour,
			HalfOpenMax: 1,
		})
		b.RecordFailure()
		permits := b.Permits()
		allow := b.Allow()
		if permits != allow {
			t.Errorf("Permits() = %v, Allow() = %v; want equal (Open, before timeout)", permits, allow)
		}
		if permits {
			t.Error("Open breaker before timeout: Permits() = true, want false")
		}
	})

	t.Run("Open after timeout", func(t *testing.T) {
		t.Parallel()
		const timeout = 10 * time.Millisecond
		b := circuitbreaker.NewBreaker(circuitbreaker.Config{
			Threshold:   1,
			Timeout:     timeout,
			HalfOpenMax: 1,
		})
		b.RecordFailure()
		time.Sleep(timeout + 15*time.Millisecond)

		permits := b.Permits()
		allow := b.Allow()
		if permits != allow {
			t.Errorf("Permits() = %v, Allow() = %v; want equal (Open, after timeout)", permits, allow)
		}
		if !permits {
			t.Error("Open breaker after timeout: Permits() = false, want true")
		}
	})

	t.Run("HalfOpen with free slot", func(t *testing.T) {
		t.Parallel()
		const timeout = 10 * time.Millisecond
		b := circuitbreaker.NewBreaker(circuitbreaker.Config{
			Threshold:   1,
			Timeout:     timeout,
			HalfOpenMax: 2,
		})
		b.RecordFailure()
		time.Sleep(timeout + 15*time.Millisecond)
		if !b.Allow() { // consumes 1 of 2 slots, enters HalfOpen
			t.Fatal("setup: Allow() did not admit the HalfOpen probe")
		}

		permits := b.Permits()
		allow := b.Allow()
		if permits != allow {
			t.Errorf("Permits() = %v, Allow() = %v; want equal (HalfOpen, free slot)", permits, allow)
		}
		if !permits {
			t.Error("HalfOpen breaker with a free slot: Permits() = false, want true")
		}
	})

	t.Run("HalfOpen with no free slot", func(t *testing.T) {
		t.Parallel()
		const timeout = 10 * time.Millisecond
		b := circuitbreaker.NewBreaker(circuitbreaker.Config{
			Threshold:   1,
			Timeout:     timeout,
			HalfOpenMax: 1,
		})
		b.RecordFailure()
		time.Sleep(timeout + 15*time.Millisecond)
		if !b.Allow() { // consumes the only slot, enters HalfOpen
			t.Fatal("setup: Allow() did not admit the HalfOpen probe")
		}

		permits := b.Permits()
		allow := b.Allow()
		if permits != allow {
			t.Errorf("Permits() = %v, Allow() = %v; want equal (HalfOpen, no free slot)", permits, allow)
		}
		if permits {
			t.Error("HalfOpen breaker with no free slot: Permits() = true, want false")
		}
	})
}

// ── Concurrency ───────────────────────────────────────────────────────────

// TestBreaker_ConcurrentAccess fires Allow, RecordSuccess, RecordFailure,
// and RecordNeutral concurrently from many goroutines. It makes no
// assertions about the resulting state (which is inherently
// nondeterministic under concurrent, mixed outcomes) — its only purpose is
// to be race-free under `go test -race` and to never panic or deadlock.
func TestBreaker_ConcurrentAccess(t *testing.T) {
	t.Parallel()

	b := circuitbreaker.NewBreaker(circuitbreaker.Config{
		Threshold:   4,
		Timeout:     5 * time.Millisecond,
		HalfOpenMax: 2,
	})

	const goroutines = 50
	const opsEach = 200

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < opsEach; i++ {
				if b.Allow() {
					switch (seed + i) % 3 {
					case 0:
						b.RecordSuccess()
					case 1:
						b.RecordFailure()
					default:
						b.RecordNeutral()
					}
				}
				_ = b.CurrentState()
			}
		}(g)
	}
	wg.Wait()

	// The breaker must still be in one of the three valid states — mostly a
	// sanity check that nothing corrupted the state field under race.
	switch b.CurrentState() {
	case circuitbreaker.Closed, circuitbreaker.Open, circuitbreaker.HalfOpen:
	default:
		t.Errorf("CurrentState() = %v after concurrent access, want a valid State", b.CurrentState())
	}
}
