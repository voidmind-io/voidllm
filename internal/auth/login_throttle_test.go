package auth

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newThrottleWithClock returns a LoginThrottle whose clock is controlled by the
// supplied pointer. Tests advance *cur to simulate the passage of time without
// real sleeps.
func newThrottleWithClock(cur *time.Time) *LoginThrottle {
	return &LoginThrottle{
		now: func() time.Time { return *cur },
	}
}

// mustAllow calls t.Allow and fails the test if it returns an error.
func mustAllow(t *testing.T, th *LoginThrottle, ip, email string) {
	t.Helper()
	if err := th.Allow(ip, email); err != nil {
		t.Fatalf("Allow(%q, %q) unexpected error: %v", ip, email, err)
	}
}

// TestIPLimit verifies that the 10th attempt from a single IP succeeds and the
// 11th returns ErrTooManyAttempts. A different IP is unaffected.
func TestIPLimit(t *testing.T) {
	t.Parallel()

	cur := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	th := newThrottleWithClock(&cur)

	// Attempts 1–10: all must succeed.
	for i := 0; i < loginMaxAttemptsPerIPPerMinute; i++ {
		if err := th.Allow("1.2.3.4", "user@example.com"); err != nil {
			t.Fatalf("attempt %d: unexpected error: %v", i+1, err)
		}
	}

	// Attempt 11: must be blocked.
	err := th.Allow("1.2.3.4", "user@example.com")
	if !errors.Is(err, ErrTooManyAttempts) {
		t.Errorf("attempt 11: got %v, want ErrTooManyAttempts", err)
	}

	// A different IP must still be allowed.
	if err := th.Allow("9.9.9.9", "user@example.com"); err != nil {
		t.Errorf("different IP unexpectedly blocked: %v", err)
	}
}

// TestIPWindowRollover verifies that advancing the clock into a new minute
// resets the per-IP counter and allows requests again.
func TestIPWindowRollover(t *testing.T) {
	t.Parallel()

	cur := time.Date(2024, 1, 1, 12, 0, 30, 0, time.UTC) // 12:00:30
	th := newThrottleWithClock(&cur)

	// Exhaust the limit within the current minute window.
	for i := 0; i < loginMaxAttemptsPerIPPerMinute; i++ {
		mustAllow(t, th, "2.2.2.2", "a@example.com")
	}
	if err := th.Allow("2.2.2.2", "a@example.com"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("expected block before rollover, got: %v", err)
	}

	// Advance clock into the next minute.
	cur = cur.Add(31 * time.Second) // now 12:01:01

	// The counter should have reset — first attempt in new window must succeed.
	if err := th.Allow("2.2.2.2", "a@example.com"); err != nil {
		t.Errorf("after window rollover: unexpected error: %v", err)
	}
}

// TestAccountLockout verifies that 5 consecutive RecordFailure calls lock the
// account and Allow returns ErrTooManyAttempts for that email.
// A different email must remain unaffected.
func TestAccountLockout(t *testing.T) {
	t.Parallel()

	cur := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	th := newThrottleWithClock(&cur)

	// Four failures must not yet lock the account.
	for i := 0; i < loginLockoutThreshold-1; i++ {
		th.RecordFailure("locked@example.com")
		if err := th.Allow("3.3.3.3", "locked@example.com"); err != nil {
			t.Fatalf("after %d failures: unexpected block: %v", i+1, err)
		}
	}

	// Fifth failure triggers lockout.
	th.RecordFailure("locked@example.com")
	err := th.Allow("3.3.3.3", "locked@example.com")
	if !errors.Is(err, ErrTooManyAttempts) {
		t.Errorf("after %d failures: got %v, want ErrTooManyAttempts", loginLockoutThreshold, err)
	}

	// A different email must still pass.
	if err := th.Allow("3.3.3.3", "other@example.com"); err != nil {
		t.Errorf("other email unexpectedly blocked: %v", err)
	}
}

// TestLockoutExpiry verifies that advancing the clock past loginLockoutDuration
// allows the account again and resets its failure counter via lazy eviction.
func TestLockoutExpiry(t *testing.T) {
	t.Parallel()

	cur := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	th := newThrottleWithClock(&cur)

	// Trigger lockout.
	for i := 0; i < loginLockoutThreshold; i++ {
		th.RecordFailure("expiry@example.com")
	}
	if err := th.Allow("4.4.4.4", "expiry@example.com"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("expected lockout immediately after threshold, got: %v", err)
	}

	// Advance clock to just before expiry — still blocked.
	cur = cur.Add(loginLockoutDuration - time.Second)
	if err := th.Allow("4.4.4.4", "expiry@example.com"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("expected lockout 1s before expiry, got: %v", err)
	}

	// Advance clock past expiry — must be allowed again.
	cur = cur.Add(2 * time.Second) // 1 second after lockout expires
	if err := th.Allow("4.4.4.4", "expiry@example.com"); err != nil {
		t.Errorf("after lockout expiry: unexpected error: %v", err)
	}

	// Failure counter must have been reset by lazy eviction inside Allow.
	// Confirm by inducing only (threshold-1) new failures — must not re-lock.
	for i := 0; i < loginLockoutThreshold-1; i++ {
		th.RecordFailure("expiry@example.com")
	}
	if err := th.Allow("4.4.4.4", "expiry@example.com"); err != nil {
		t.Errorf("after expiry + %d new failures: unexpected block: %v", loginLockoutThreshold-1, err)
	}
}

// TestRecordSuccessResetsFails verifies that a successful login clears the
// failure counter so that subsequent failures start fresh.
func TestRecordSuccessResetsFails(t *testing.T) {
	t.Parallel()

	cur := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	th := newThrottleWithClock(&cur)

	// Four failures — one short of lockout threshold (5).
	for i := 0; i < loginLockoutThreshold-1; i++ {
		th.RecordFailure("reset@example.com")
	}

	// Successful login resets the counter to zero.
	th.RecordSuccess("reset@example.com")

	// Four more failures after the reset — counter is at 4, below threshold.
	for i := 0; i < loginLockoutThreshold-1; i++ {
		th.RecordFailure("reset@example.com")
	}
	if err := th.Allow("5.5.5.5", "reset@example.com"); err != nil {
		t.Errorf("after success + %d new failures: unexpected block: %v", loginLockoutThreshold-1, err)
	}

	// The 5th failure post-reset reaches the threshold and triggers lockout.
	th.RecordFailure("reset@example.com")
	if err := th.Allow("5.5.5.5", "reset@example.com"); !errors.Is(err, ErrTooManyAttempts) {
		t.Errorf("after threshold failures post-reset: expected ErrTooManyAttempts, got: %v", err)
	}
}

// TestEmailNormalisation verifies that "User@Example.COM " and
// "user@example.com" resolve to the same throttle bucket.
func TestEmailNormalisation(t *testing.T) {
	t.Parallel()

	cur := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	th := newThrottleWithClock(&cur)

	// Record failures using the mixed-case + padded form.
	for i := 0; i < loginLockoutThreshold; i++ {
		th.RecordFailure("User@Example.COM ")
	}

	// Allow must see the lockout when queried with the canonical form.
	err := th.Allow("6.6.6.6", "user@example.com")
	if !errors.Is(err, ErrTooManyAttempts) {
		t.Errorf("canonical form not blocked after failures recorded via mixed-case: %v", err)
	}

	// Also confirm the reverse: failures recorded via canonical form block the
	// padded / mixed-case variant.
	cur2 := time.Date(2024, 1, 1, 13, 0, 0, 0, time.UTC)
	th2 := newThrottleWithClock(&cur2)
	for i := 0; i < loginLockoutThreshold; i++ {
		th2.RecordFailure("user@example.com")
	}
	err2 := th2.Allow("6.6.6.6", "User@Example.COM ")
	if !errors.Is(err2, ErrTooManyAttempts) {
		t.Errorf("padded/mixed form not blocked after failures recorded via canonical: %v", err2)
	}
}

// TestEvictStale verifies that EvictStale removes expired IP windows and idle
// account entries from the internal sync.Maps.
// This test is in the same package to access ipCounters and accounts directly.
func TestEvictStale(t *testing.T) {
	t.Parallel()

	cur := time.Date(2024, 1, 1, 12, 0, 30, 0, time.UTC)
	th := newThrottleWithClock(&cur)

	// Create an IP entry by making a request.
	mustAllow(t, th, "7.7.7.7", "evict@example.com")
	// Create an account entry by recording a failure then clearing it.
	th.RecordFailure("evict@example.com")
	th.RecordSuccess("evict@example.com") // failures=0, lockedUntil=zero

	// Verify both entries exist before eviction.
	if _, ok := th.ipCounters.Load("7.7.7.7"); !ok {
		t.Fatal("IP entry not present before EvictStale")
	}
	if _, ok := th.accounts.Load("evict@example.com"); !ok {
		t.Fatal("account entry not present before EvictStale")
	}

	// Advance clock into a new minute so the IP window is stale.
	cur = cur.Add(31 * time.Second) // 12:01:01 — window 12:00 is now stale

	th.EvictStale()

	// IP entry must be gone.
	if _, ok := th.ipCounters.Load("7.7.7.7"); ok {
		t.Error("stale IP entry still present after EvictStale")
	}
	// Account entry with zero failures and no lockout must be gone.
	if _, ok := th.accounts.Load("evict@example.com"); ok {
		t.Error("idle account entry still present after EvictStale")
	}

	// Active lockout entries must NOT be evicted.
	for i := 0; i < loginLockoutThreshold; i++ {
		th.RecordFailure("active@example.com")
	}
	th.EvictStale()
	if _, ok := th.accounts.Load("active@example.com"); !ok {
		t.Error("active locked account was incorrectly evicted by EvictStale")
	}

	// Partial failures (1–4) with an old lastFailure must be evicted.
	th.RecordFailure("partial@example.com") // failures=1, no lockout
	// Advance clock beyond loginAccountIdleEviction.
	cur = cur.Add(loginAccountIdleEviction + time.Second)
	th.EvictStale()
	if _, ok := th.accounts.Load("partial@example.com"); ok {
		t.Error("partial-failure account with stale lastFailure was not evicted by EvictStale")
	}

	// Partial failures with a recent lastFailure must NOT be evicted.
	th.RecordFailure("recent@example.com") // failures=1, lastFailure=now (cur after advance)
	// EvictStale immediately — lastFailure is fresh, so must stay.
	th.EvictStale()
	if _, ok := th.accounts.Load("recent@example.com"); !ok {
		t.Error("recent partial-failure account was incorrectly evicted by EvictStale")
	}
}

// TestConcurrencySmoke fires many concurrent Allow calls from multiple
// goroutines and verifies there are no data races (run with -race).
func TestConcurrencySmoke(t *testing.T) {
	t.Parallel()

	cur := time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC)
	th := newThrottleWithClock(&cur)

	const goroutines = 50
	const callsEach = 40

	var wg sync.WaitGroup
	var allowed, blocked atomic.Int64

	for g := 0; g < goroutines; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			ip := "10.0.0." + string(rune('A'+g%26))
			for i := 0; i < callsEach; i++ {
				if err := th.Allow(ip, "smoke@example.com"); err == nil {
					allowed.Add(1)
				} else {
					blocked.Add(1)
				}
				th.RecordFailure("smoke@example.com")
			}
		}()
	}

	wg.Wait()

	// Sanity check: some requests must have been allowed, some blocked.
	if allowed.Load() == 0 {
		t.Error("no requests were allowed — throttle may be broken")
	}
	if blocked.Load() == 0 {
		t.Error("no requests were blocked — throttle may not be working")
	}
}
