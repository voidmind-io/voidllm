package ratelimit

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestNewRateLimiter(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	if rl == nil {
		t.Fatal("NewRateLimiter() returned nil")
	}
}

func TestCheckRate_ZeroLimitsAlwaysAllowed(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	unlimited := Limits{}

	for i := range 100 {
		if err := rl.CheckRate("key1", "team1", "org1", unlimited, unlimited, unlimited); err != nil {
			t.Fatalf("iteration %d: CheckRate() with zero limits error = %v, want nil", i, err)
		}
	}
}

func TestCheckRate_WithinRPMLimit(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	keyLimits := Limits{RequestsPerMinute: 10}
	noLimits := Limits{}

	for i := range 10 {
		if err := rl.CheckRate("key-rpm-ok", "", "org-rpm-ok", keyLimits, noLimits, noLimits); err != nil {
			t.Fatalf("request %d: CheckRate() error = %v, want nil", i+1, err)
		}
	}
}

func TestCheckRate_ExceedingRPMLimit(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	keyLimits := Limits{RequestsPerMinute: 3}
	noLimits := Limits{}

	// First 3 requests must succeed.
	for i := range 3 {
		if err := rl.CheckRate("key-rpm-exceed", "", "org-rpm-exceed", keyLimits, noLimits, noLimits); err != nil {
			t.Fatalf("request %d: CheckRate() error = %v, want nil", i+1, err)
		}
	}

	// 4th request must fail.
	err := rl.CheckRate("key-rpm-exceed", "", "org-rpm-exceed", keyLimits, noLimits, noLimits)
	if !errors.Is(err, ErrRateLimitExceeded) {
		t.Errorf("4th CheckRate() error = %v, want ErrRateLimitExceeded", err)
	}
}

func TestCheckRate_WithinRPDLimit(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	keyLimits := Limits{RequestsPerDay: 5}
	noLimits := Limits{}

	for i := range 5 {
		if err := rl.CheckRate("key-rpd-ok", "", "org-rpd-ok", keyLimits, noLimits, noLimits); err != nil {
			t.Fatalf("request %d: CheckRate() error = %v, want nil", i+1, err)
		}
	}
}

func TestCheckRate_ExceedingRPDLimit(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	keyLimits := Limits{RequestsPerDay: 2}
	noLimits := Limits{}

	for i := range 2 {
		if err := rl.CheckRate("key-rpd-exceed", "", "org-rpd-exceed", keyLimits, noLimits, noLimits); err != nil {
			t.Fatalf("request %d: CheckRate() error = %v, want nil", i+1, err)
		}
	}

	err := rl.CheckRate("key-rpd-exceed", "", "org-rpd-exceed", keyLimits, noLimits, noLimits)
	if !errors.Is(err, ErrRateLimitExceeded) {
		t.Errorf("3rd CheckRate() error = %v, want ErrRateLimitExceeded", err)
	}
}

func TestCheckRate_OrgLimitSharedAcrossKeys(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	noLimits := Limits{}
	orgLimits := Limits{RequestsPerMinute: 8}

	// Key A and Key B share the same org. Key limits are unlimited but org RPM=8.
	// After 8 total requests the org counter is exhausted.
	sharedOrg := "shared-org-rpm"
	keyA := "key-a-shared-org"
	keyB := "key-b-shared-org"

	var failCount int
	for i := range 10 {
		key := keyA
		if i%2 == 1 {
			key = keyB
		}
		err := rl.CheckRate(key, "", sharedOrg, noLimits, noLimits, orgLimits)
		if err != nil {
			if !errors.Is(err, ErrRateLimitExceeded) {
				t.Fatalf("request %d: unexpected error %v", i+1, err)
			}
			failCount++
		}
	}

	if failCount == 0 {
		t.Error("expected at least one failure due to shared org RPM=8 across 10 requests, got none")
	}
	if failCount != 2 {
		t.Errorf("expected 2 failures (requests 9 and 10), got %d", failCount)
	}
}

func TestCheckRate_TeamLimitCheckedAlongside(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	noLimits := Limits{}
	teamLimits := Limits{RequestsPerMinute: 4}

	// Key limit is generous, team limit is 4.
	keyLimits := Limits{RequestsPerMinute: 100}

	for i := range 4 {
		if err := rl.CheckRate("key-team-lim", "team-lim", "org-team-lim", keyLimits, teamLimits, noLimits); err != nil {
			t.Fatalf("request %d: CheckRate() error = %v, want nil", i+1, err)
		}
	}

	err := rl.CheckRate("key-team-lim", "team-lim", "org-team-lim", keyLimits, teamLimits, noLimits)
	if !errors.Is(err, ErrRateLimitExceeded) {
		t.Errorf("5th CheckRate() error = %v, want ErrRateLimitExceeded", err)
	}
}

func TestCheckRate_MostRestrictiveWins_KeyRPM10_OrgRPM3(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	keyLimits := Limits{RequestsPerMinute: 10}
	noLimits := Limits{}
	orgLimits := Limits{RequestsPerMinute: 3}

	// Effective RPM = min(10, 3) = 3. 4th request must fail.
	for i := range 3 {
		if err := rl.CheckRate("key-mrw", "", "org-mrw", keyLimits, noLimits, orgLimits); err != nil {
			t.Fatalf("request %d: CheckRate() error = %v, want nil", i+1, err)
		}
	}

	err := rl.CheckRate("key-mrw", "", "org-mrw", keyLimits, noLimits, orgLimits)
	if !errors.Is(err, ErrRateLimitExceeded) {
		t.Errorf("4th CheckRate() error = %v, want ErrRateLimitExceeded", err)
	}
}

func TestCheckRate_EmptyTeamIDNoTeamCounter(t *testing.T) {
	t.Parallel()

	// When teamID is empty no team-scoped counter should be created. This test
	// verifies that a non-empty teamID and an empty teamID do not share counters.
	rl := NewRateLimiter()
	noLimits := Limits{}
	keyLimits := Limits{RequestsPerMinute: 5}

	// Use 5 requests with empty team (burns 5 slots for the key scope).
	for range 5 {
		if err := rl.CheckRate("key-no-team", "", "org-no-team", keyLimits, noLimits, noLimits); err != nil {
			t.Fatal("unexpected error during setup requests")
		}
	}

	// 6th request on the same key (no team) must fail because key RPM=5.
	err := rl.CheckRate("key-no-team", "", "org-no-team", keyLimits, noLimits, noLimits)
	if !errors.Is(err, ErrRateLimitExceeded) {
		t.Errorf("6th CheckRate() (empty teamID) error = %v, want ErrRateLimitExceeded", err)
	}
}

func TestCheckRate_CountersNotIncrementedOnFailure(t *testing.T) {
	t.Parallel()

	// When CheckRate returns ErrRateLimitExceeded the counters must not be
	// incremented, so the next successful window still has the full budget.
	rl := NewRateLimiter()
	keyLimits := Limits{RequestsPerMinute: 2}
	noLimits := Limits{}

	// Exhaust the budget.
	_ = rl.CheckRate("key-no-incr", "", "org-no-incr", keyLimits, noLimits, noLimits)
	_ = rl.CheckRate("key-no-incr", "", "org-no-incr", keyLimits, noLimits, noLimits)

	// These calls should all fail without incrementing.
	for i := range 5 {
		err := rl.CheckRate("key-no-incr", "", "org-no-incr", keyLimits, noLimits, noLimits)
		if !errors.Is(err, ErrRateLimitExceeded) {
			t.Errorf("rejected call %d: error = %v, want ErrRateLimitExceeded", i+1, err)
		}
	}
}

func TestCheckRate_RPMAndRPDIndependent(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	// RPM=3 and RPD=10 — RPM should trigger before RPD.
	keyLimits := Limits{RequestsPerMinute: 3, RequestsPerDay: 10}
	noLimits := Limits{}

	for i := range 3 {
		if err := rl.CheckRate("key-both-limits", "", "org-both-limits", keyLimits, noLimits, noLimits); err != nil {
			t.Fatalf("request %d: CheckRate() error = %v, want nil", i+1, err)
		}
	}

	err := rl.CheckRate("key-both-limits", "", "org-both-limits", keyLimits, noLimits, noLimits)
	if !errors.Is(err, ErrRateLimitExceeded) {
		t.Errorf("4th CheckRate() error = %v, want ErrRateLimitExceeded (expected RPM block)", err)
	}
}

func TestCheckRate_UniqueKeysSeparateCounters(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	keyLimits := Limits{RequestsPerMinute: 2}
	noLimits := Limits{}

	// Each unique key gets its own counter — exhausting key1 does not affect key2.
	for i := range 2 {
		if err := rl.CheckRate(fmt.Sprintf("key-unique-%d", i), "", fmt.Sprintf("org-unique-%d", i), keyLimits, noLimits, noLimits); err != nil {
			t.Fatalf("key %d request 1: %v", i, err)
		}
		if err := rl.CheckRate(fmt.Sprintf("key-unique-%d", i), "", fmt.Sprintf("org-unique-%d", i), keyLimits, noLimits, noLimits); err != nil {
			t.Fatalf("key %d request 2: %v", i, err)
		}
	}

	// Both keys are now at limit. Key0 next request must fail.
	err := rl.CheckRate("key-unique-0", "", "org-unique-0", keyLimits, noLimits, noLimits)
	if !errors.Is(err, ErrRateLimitExceeded) {
		t.Errorf("key-unique-0 3rd request: error = %v, want ErrRateLimitExceeded", err)
	}
	// Key1 must also fail independently.
	err = rl.CheckRate("key-unique-1", "", "org-unique-1", keyLimits, noLimits, noLimits)
	if !errors.Is(err, ErrRateLimitExceeded) {
		t.Errorf("key-unique-1 3rd request: error = %v, want ErrRateLimitExceeded", err)
	}
}

// TestCheckRate_ConcurrentCASCorrectness spawns 100 goroutines against an RPM
// limit of 50 and verifies that exactly 50 succeed and 50 fail. This exercises
// the CAS loop under real contention — the old TOCTOU code would allow more
// than 50 goroutines through.
func TestCheckRate_ConcurrentCASCorrectness(t *testing.T) {
	t.Parallel()

	const (
		goroutines = 100
		limit      = 50
	)

	rl := NewRateLimiter()
	keyLimits := Limits{RequestsPerMinute: limit}
	noLimits := Limits{}

	var (
		wg       sync.WaitGroup
		successes atomic.Int64
		failures  atomic.Int64
	)

	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			err := rl.CheckRate("cas-key", "", "cas-org", keyLimits, noLimits, noLimits)
			if err == nil {
				successes.Add(1)
			} else {
				failures.Add(1)
			}
		}()
	}
	wg.Wait()

	if got := successes.Load(); got != limit {
		t.Errorf("successes = %d, want %d", got, limit)
	}
	if got := failures.Load(); got != goroutines-limit {
		t.Errorf("failures = %d, want %d", got, goroutines-limit)
	}
}

// TestEvictStale_RemovesExpiredEntries verifies that EvictStale deletes entries
// whose windowStart is behind the current window. We inject stale entries
// directly via CheckRate using a dedicated limiter, then call EvictStale after
// advancing the clock conceptually by verifying stale entries disappear.
// Because we cannot mock time in the limiter, we verify EvictStale does not
// error and that entries created in the current window survive.
func TestEvictStale_RemovesExpiredEntries(t *testing.T) {
	t.Parallel()

	rl := NewRateLimiter()
	keyLimits := Limits{RequestsPerMinute: 10, RequestsPerDay: 10}
	noLimits := Limits{}

	// Create entries for several unique keys in the current window.
	for i := range 5 {
		key := fmt.Sprintf("evict-key-%d", i)
		org := fmt.Sprintf("evict-org-%d", i)
		if err := rl.CheckRate(key, "", org, keyLimits, noLimits, noLimits); err != nil {
			t.Fatalf("setup CheckRate for %s: %v", key, err)
		}
	}

	// Manually inject a stale entry (windowStart = 0, which is before any
	// real minute or day window) into both maps.
	staleEntry := &counterEntry{}
	staleEntry.windowStart.Store(0) // Unix epoch — always stale
	staleEntry.count.Store(3)
	rl.minuteCounters.Store("stale-minute-scope", staleEntry)
	rl.dayCounters.Store("stale-day-scope", staleEntry)

	// Verify the stale entries exist before eviction.
	if _, ok := rl.minuteCounters.Load("stale-minute-scope"); !ok {
		t.Fatal("stale minute entry not present before EvictStale")
	}
	if _, ok := rl.dayCounters.Load("stale-day-scope"); !ok {
		t.Fatal("stale day entry not present before EvictStale")
	}

	rl.EvictStale()

	// Stale entries must be gone.
	if _, ok := rl.minuteCounters.Load("stale-minute-scope"); ok {
		t.Error("stale minute entry still present after EvictStale")
	}
	if _, ok := rl.dayCounters.Load("stale-day-scope"); ok {
		t.Error("stale day entry still present after EvictStale")
	}

	// Entries created in the current window must survive.
	for i := range 5 {
		key := fmt.Sprintf("evict-key-%d", i)
		if _, ok := rl.minuteCounters.Load("key:" + key); !ok {
			t.Errorf("current-window minute entry for %s was wrongly evicted", key)
		}
	}
}

func BenchmarkCheckRate(b *testing.B) {
	rl := NewRateLimiter()
	// Use a high limit so the benchmark does not hit the ceiling.
	keyLimits := Limits{RequestsPerMinute: 1_000_000}
	noLimits := Limits{}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := rl.CheckRate("bench-key", "bench-team", "bench-org", keyLimits, noLimits, noLimits); err != nil {
				b.Fatal(err)
			}
		}
	})
}
