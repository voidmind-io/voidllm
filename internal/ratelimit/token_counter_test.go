package ratelimit

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestNewTokenCounter verifies that NewTokenCounter returns a non-nil value.
func TestNewTokenCounter(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	if tc == nil {
		t.Fatal("NewTokenCounter() returned nil")
	}
}

// TestTokenCounter_AddAndCheckTokens tests the basic Add + CheckTokens
// round-trip with a daily limit that is not yet exceeded.
func TestTokenCounter_AddAndCheckTokens(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	tc.Add("key1", "", "org1", 500)

	keyLimits := Limits{DailyTokenLimit: 1000}
	noLimits := Limits{}

	if err := tc.CheckTokens("key1", "", "org1", keyLimits, noLimits, noLimits); err != nil {
		t.Errorf("CheckTokens() = %v, want nil (500 tokens < 1000 limit)", err)
	}
}

// TestTokenCounter_CheckTokens_ExceedsKeyDailyLimit verifies that adding tokens
// up to the key daily limit causes CheckTokens to return ErrTokenBudgetExceeded.
func TestTokenCounter_CheckTokens_ExceedsKeyDailyLimit(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	tc.Add("key-exceed-daily", "", "org-exceed-daily", 1000)

	keyLimits := Limits{DailyTokenLimit: 1000}
	noLimits := Limits{}

	err := tc.CheckTokens("key-exceed-daily", "", "org-exceed-daily", keyLimits, noLimits, noLimits)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("CheckTokens() = %v, want ErrTokenBudgetExceeded (1000 >= limit 1000)", err)
	}
}

// TestTokenCounter_CheckTokens_ExceedsKeyMonthlyLimit verifies monthly limit
// enforcement at the key scope.
func TestTokenCounter_CheckTokens_ExceedsKeyMonthlyLimit(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	tc.Add("key-exceed-monthly", "", "org-exceed-monthly", 5000)

	keyLimits := Limits{MonthlyTokenLimit: 5000}
	noLimits := Limits{}

	err := tc.CheckTokens("key-exceed-monthly", "", "org-exceed-monthly", keyLimits, noLimits, noLimits)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("CheckTokens() = %v, want ErrTokenBudgetExceeded", err)
	}
}

// TestTokenCounter_KeyLimitExceeded_OrgFine verifies that the key scope limit
// is enforced even when the org scope has headroom.
func TestTokenCounter_KeyLimitExceeded_OrgFine(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	// Add 600 tokens for one key in the org — key limit 500, org limit 10000.
	tc.Add("key-tight", "", "org-spacious", 600)

	keyLimits := Limits{DailyTokenLimit: 500}
	noLimits := Limits{}
	orgLimits := Limits{DailyTokenLimit: 10000}

	err := tc.CheckTokens("key-tight", "", "org-spacious", keyLimits, noLimits, orgLimits)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("CheckTokens() = %v, want ErrTokenBudgetExceeded (key 600 >= limit 500, org fine)", err)
	}
}

// TestTokenCounter_OrgLimitExceeded_KeyFine verifies that the org scope limit
// is enforced even when the key scope has headroom.
func TestTokenCounter_OrgLimitExceeded_KeyFine(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()

	// Simulate traffic from two different keys that belong to the same org.
	// keyA and keyB each contribute 300 tokens → org total = 600, org limit = 500.
	tc.Add("key-a-shared", "", "tight-org", 300)
	tc.Add("key-b-shared", "", "tight-org", 300)

	// The next request comes from key-c: key limit is generous, but org is over.
	keyLimits := Limits{DailyTokenLimit: 10000}
	noLimits := Limits{}
	orgLimits := Limits{DailyTokenLimit: 500}

	err := tc.CheckTokens("key-c-shared", "", "tight-org", keyLimits, noLimits, orgLimits)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("CheckTokens() = %v, want ErrTokenBudgetExceeded (org 600 >= limit 500)", err)
	}
}

// TestTokenCounter_TeamLimitExceeded verifies that the team scope limit is
// enforced when the key and org limits have plenty of headroom.
func TestTokenCounter_TeamLimitExceeded(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	// Add 400 tokens attributed to team "tight-team".
	tc.Add("key-team-member", "tight-team", "org-team-test", 400)

	keyLimits := Limits{DailyTokenLimit: 1000}
	teamLimits := Limits{DailyTokenLimit: 300}
	orgLimits := Limits{DailyTokenLimit: 1000}

	err := tc.CheckTokens("key-team-member", "tight-team", "org-team-test", keyLimits, teamLimits, orgLimits)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("CheckTokens() = %v, want ErrTokenBudgetExceeded (team 400 >= limit 300)", err)
	}
}

// TestTokenCounter_TeamLimitFine verifies that a team usage below the limit
// does not trigger ErrTokenBudgetExceeded.
func TestTokenCounter_TeamLimitFine(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	tc.Add("key-team-ok", "ok-team", "org-team-ok", 100)

	keyLimits := Limits{DailyTokenLimit: 1000}
	teamLimits := Limits{DailyTokenLimit: 300}
	orgLimits := Limits{DailyTokenLimit: 1000}

	if err := tc.CheckTokens("key-team-ok", "ok-team", "org-team-ok", keyLimits, teamLimits, orgLimits); err != nil {
		t.Errorf("CheckTokens() = %v, want nil (team 100 < limit 300)", err)
	}
}

// TestTokenCounter_ZeroLimit_Unlimited verifies that a zero limit means
// unlimited — any amount of token usage passes.
func TestTokenCounter_ZeroLimit_Unlimited(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	// Add a very large number of tokens.
	tc.Add("key-unlimited", "team-unlimited", "org-unlimited", 1_000_000)

	noLimits := Limits{} // all zeroes = unlimited

	if err := tc.CheckTokens("key-unlimited", "team-unlimited", "org-unlimited", noLimits, noLimits, noLimits); err != nil {
		t.Errorf("CheckTokens() = %v, want nil (all limits zero = unlimited)", err)
	}
}

// TestTokenCounter_ZeroLimitOnOneScope verifies that a zero daily limit on one
// scope does not cause a spurious failure while another scope has a real limit.
func TestTokenCounter_ZeroLimitOnOneScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		keyLimit  Limits
		teamLimit Limits
		orgLimit  Limits
		tokens    int64
		wantErr   bool
	}{
		{
			name:      "key unlimited, org limit fine",
			keyLimit:  Limits{},
			teamLimit: Limits{},
			orgLimit:  Limits{DailyTokenLimit: 1000},
			tokens:    500,
			wantErr:   false,
		},
		{
			name:      "key unlimited, org limit exceeded",
			keyLimit:  Limits{},
			teamLimit: Limits{},
			orgLimit:  Limits{DailyTokenLimit: 1000},
			tokens:    1000,
			wantErr:   true,
		},
		{
			name:      "org unlimited, key limit fine",
			keyLimit:  Limits{DailyTokenLimit: 1000},
			teamLimit: Limits{},
			orgLimit:  Limits{},
			tokens:    500,
			wantErr:   false,
		},
		{
			name:      "org unlimited, key limit exceeded",
			keyLimit:  Limits{DailyTokenLimit: 1000},
			teamLimit: Limits{},
			orgLimit:  Limits{},
			tokens:    1000,
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			counter := NewTokenCounter()
			counter.Add("key-mixed", "", "org-mixed-"+tc.name, tc.tokens)

			err := counter.CheckTokens("key-mixed", "", "org-mixed-"+tc.name, tc.keyLimit, tc.teamLimit, tc.orgLimit)
			if tc.wantErr && !errors.Is(err, ErrTokenBudgetExceeded) {
				t.Errorf("CheckTokens() = %v, want ErrTokenBudgetExceeded", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("CheckTokens() = %v, want nil", err)
			}
		})
	}
}

// TestTokenCounter_DailyWindowReset verifies that tokens added in a prior day
// window are not counted in the current window. We simulate a prior-day entry
// by injecting a tokenEntry with a stale windowStart directly into the counter.
func TestTokenCounter_DailyWindowReset(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()

	// Compute yesterday's day window start timestamp.
	now := time.Now().UTC()
	yesterday := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, time.UTC).Unix()

	// Directly inject a stale daily entry for the key scope.
	stale := &tokenEntry{}
	stale.windowStart.Store(yesterday)
	stale.tokens.Store(900)
	tc.dailyCounters.Store("key:key-stale-daily", stale)

	// With a limit of 1000 and a stale 900-token entry, CheckTokens should
	// report 0 tokens used (stale window) → passes.
	keyLimits := Limits{DailyTokenLimit: 1000}
	noLimits := Limits{}

	if err := tc.CheckTokens("key-stale-daily", "", "org-stale-daily", keyLimits, noLimits, noLimits); err != nil {
		t.Errorf("CheckTokens() = %v, want nil (stale window should read as 0 tokens)", err)
	}
}

// TestTokenCounter_MonthlyWindowReset verifies that tokens from a prior month
// are not counted in the current month's window.
func TestTokenCounter_MonthlyWindowReset(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()

	// Compute last month's window start.
	now := time.Now().UTC()
	lastMonth := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC).Unix()

	stale := &tokenEntry{}
	stale.windowStart.Store(lastMonth)
	stale.tokens.Store(4500)
	tc.monthlyCounters.Store("key:key-stale-monthly", stale)

	keyLimits := Limits{MonthlyTokenLimit: 5000}
	noLimits := Limits{}

	if err := tc.CheckTokens("key-stale-monthly", "", "org-stale-monthly", keyLimits, noLimits, noLimits); err != nil {
		t.Errorf("CheckTokens() = %v, want nil (stale monthly window should read as 0 tokens)", err)
	}
}

// TestTokenCounter_AddResetsOnWindowRollover verifies that when Add is called
// with a fresh window (simulated by injecting a stale entry and then calling
// Add), the counter resets rather than accumulating.
func TestTokenCounter_AddResetsOnWindowRollover(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()

	// Plant a stale entry with a large token count.
	now := time.Now().UTC()
	yesterday := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, time.UTC).Unix()
	stale := &tokenEntry{}
	stale.windowStart.Store(yesterday)
	stale.tokens.Store(9999)
	tc.dailyCounters.Store("key:key-rollover", stale)

	// Add tokens today — this should claim the new window and reset the count.
	tc.Add("key-rollover", "", "org-rollover", 100)

	keyLimits := Limits{DailyTokenLimit: 500}
	noLimits := Limits{}

	if err := tc.CheckTokens("key-rollover", "", "org-rollover", keyLimits, noLimits, noLimits); err != nil {
		t.Errorf("CheckTokens() = %v, want nil (100 tokens after rollover, limit 500)", err)
	}
}

// TestTokenCounter_EvictStale removes entries from prior windows and verifies
// that current-window entries survive.
func TestTokenCounter_EvictStale(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()

	now := time.Now().UTC()
	yesterday := time.Date(now.Year(), now.Month(), now.Day()-1, 0, 0, 0, 0, time.UTC).Unix()
	lastMonth := time.Date(now.Year(), now.Month()-1, 1, 0, 0, 0, 0, time.UTC).Unix()

	// Inject stale daily entry.
	staleDaily := &tokenEntry{}
	staleDaily.windowStart.Store(yesterday)
	staleDaily.tokens.Store(100)
	tc.dailyCounters.Store("key:stale-daily-key", staleDaily)

	// Inject stale monthly entry.
	staleMonthly := &tokenEntry{}
	staleMonthly.windowStart.Store(lastMonth)
	staleMonthly.tokens.Store(200)
	tc.monthlyCounters.Store("key:stale-monthly-key", staleMonthly)

	// Add a current-window entry that must survive eviction.
	tc.Add("key-current", "", "org-current", 50)

	tc.EvictStale()

	// Stale entries must be removed.
	if _, ok := tc.dailyCounters.Load("key:stale-daily-key"); ok {
		t.Error("stale daily entry still present after EvictStale")
	}
	if _, ok := tc.monthlyCounters.Load("key:stale-monthly-key"); ok {
		t.Error("stale monthly entry still present after EvictStale")
	}

	// Current-window entry must survive.
	if _, ok := tc.dailyCounters.Load("key:key-current"); !ok {
		t.Error("current daily entry was wrongly evicted")
	}
	if _, ok := tc.monthlyCounters.Load("key:key-current"); !ok {
		t.Error("current monthly entry was wrongly evicted")
	}
}

// TestTokenCounter_NoTeamID_NoTeamEntry verifies that passing an empty teamID
// to Add does not create team-scoped counter entries and that CheckTokens with
// an empty teamID ignores team limits.
func TestTokenCounter_NoTeamID_NoTeamEntry(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	tc.Add("key-no-team", "", "org-no-team", 300)

	// Team limits set to very low — but teamID is empty so they must be ignored.
	keyLimits := Limits{DailyTokenLimit: 1000}
	teamLimits := Limits{DailyTokenLimit: 1} // would block if team was checked
	orgLimits := Limits{DailyTokenLimit: 1000}

	if err := tc.CheckTokens("key-no-team", "", "org-no-team", keyLimits, teamLimits, orgLimits); err != nil {
		t.Errorf("CheckTokens() = %v, want nil (empty teamID skips team limit check)", err)
	}

	// Confirm no team entry was created.
	found := false
	tc.dailyCounters.Range(func(k, _ any) bool {
		if k.(string)[:5] == "team:" {
			found = true
			return false
		}
		return true
	})
	if found {
		t.Error("team-scoped entry was created despite empty teamID")
	}
}

// TestTokenCounter_ConcurrentAdd verifies that 100 goroutines each adding 10
// tokens result in exactly 1000 total tokens for the key scope.
func TestTokenCounter_ConcurrentAdd(t *testing.T) {
	t.Parallel()

	const (
		goroutines  = 100
		tokensEach  = 10
		wantTotal   = goroutines * tokensEach
	)

	tc := NewTokenCounter()

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			tc.Add("concurrent-key", "", "concurrent-org", tokensEach)
		}()
	}
	wg.Wait()

	// Read the daily counter directly to verify the exact total.
	now := time.Now().UTC()
	dayWindow := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()

	got := tc.getCount(&tc.dailyCounters, "key:concurrent-key", dayWindow)
	if got != wantTotal {
		t.Errorf("concurrent Add total = %d, want %d", got, wantTotal)
	}
}

// TestTokenCounter_ConcurrentAddAndCheck verifies concurrent Add and
// CheckTokens calls do not race (checked by the -race flag). We do not assert
// exact counts here — that is covered by TestTokenCounter_ConcurrentAdd.
func TestTokenCounter_ConcurrentAddAndCheck(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	keyLimits := Limits{DailyTokenLimit: 100_000}
	noLimits := Limits{}

	var wg sync.WaitGroup
	var checkErrors atomic.Int64

	for i := range 50 {
		wg.Add(2)
		key := fmt.Sprintf("race-key-%d", i%5) // reuse 5 keys to create contention
		go func() {
			defer wg.Done()
			tc.Add(key, "", "race-org", 10)
		}()
		go func() {
			defer wg.Done()
			if err := tc.CheckTokens(key, "", "race-org", keyLimits, noLimits, noLimits); err != nil {
				checkErrors.Add(1)
			}
		}()
	}
	wg.Wait()

	// With limit 100_000 and at most 100 tokens per key, no check should fail.
	if n := checkErrors.Load(); n > 0 {
		t.Errorf("%d CheckTokens calls returned unexpected errors under low load", n)
	}
}

// BenchmarkTokenCounter_Add benchmarks the hot-path token addition.
func BenchmarkTokenCounter_Add(b *testing.B) {
	tc := NewTokenCounter()

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tc.Add("bench-key", "bench-team", "bench-org", 100)
		}
	})
}

// BenchmarkTokenCounter_CheckTokens benchmarks token budget checking.
func BenchmarkTokenCounter_CheckTokens(b *testing.B) {
	tc := NewTokenCounter()
	tc.Add("bench-key", "bench-team", "bench-org", 100)
	keyLimits := Limits{DailyTokenLimit: 1_000_000, MonthlyTokenLimit: 10_000_000}
	noLimits := Limits{}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := tc.CheckTokens("bench-key", "bench-team", "bench-org", keyLimits, noLimits, noLimits); err != nil {
				b.Fatal(err)
			}
		}
	})
}
