package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeClock is a settable time source for budget counters.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock(t time.Time) *fakeClock {
	c := &fakeClock{}
	c.Set(t)
	return c
}

func (c *fakeClock) Now() time.Time      { return time.Unix(0, c.ns.Load()).UTC() }
func (c *fakeClock) Set(t time.Time)     { c.ns.Store(t.UnixNano()) }
func (c *fakeClock) Add(d time.Duration) { c.ns.Add(int64(d)) }

// addTokens records n tokens for s.
func addTokens(c BudgetCounter, s Scopes, n int64) {
	c.Add(context.Background(), s, Usage{Tokens: n})
}

// checkTokens runs a budget check for s against l.
func checkTokens(c BudgetCounter, s Scopes, l ScopeLimits) error {
	return c.Check(context.Background(), s, l)
}

// hasScopeWithPrefix reports whether the counter holds an entry whose scope
// key starts with prefix.
func hasScopeWithPrefix(m *MemoryBudgetCounter, prefix string) bool {
	found := false
	m.entries.Range(func(k, _ any) bool {
		if strings.HasPrefix(k.(string), prefix) {
			found = true
			return false
		}
		return true
	})
	return found
}

// TestNewMemoryBudgetCounter verifies that the constructor returns a usable
// value.
func TestNewMemoryBudgetCounter(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	if tc == nil {
		t.Fatal("NewMemoryBudgetCounter() returned nil")
	}
	if err := checkTokens(tc, Scopes{KeyID: "k", OrgID: "o"}, ScopeLimits{Key: Limits{DailyTokenLimit: 1}}); err != nil {
		t.Errorf("Check() on empty counter = %v, want nil", err)
	}
}

// TestMemoryBudgetCounter_AddAndCheck tests the basic Add + Check round-trip
// with a daily limit that is not yet exceeded.
func TestMemoryBudgetCounter_AddAndCheck(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "key1", OrgID: "org1"}
	addTokens(tc, scopes, 500)

	if err := checkTokens(tc, scopes, ScopeLimits{Key: Limits{DailyTokenLimit: 1000}}); err != nil {
		t.Errorf("Check() = %v, want nil (500 tokens < 1000 limit)", err)
	}
}

// TestMemoryBudgetCounter_Check_ExceedsKeyDailyLimit verifies that adding
// tokens up to the key daily limit causes Check to return
// ErrTokenBudgetExceeded.
func TestMemoryBudgetCounter_Check_ExceedsKeyDailyLimit(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "key-exceed-daily", OrgID: "org-exceed-daily"}
	addTokens(tc, scopes, 1000)

	err := checkTokens(tc, scopes, ScopeLimits{Key: Limits{DailyTokenLimit: 1000}})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() = %v, want ErrTokenBudgetExceeded (1000 >= limit 1000)", err)
	}
}

// TestMemoryBudgetCounter_Check_OneBelowLimitAllowed pins the boundary: a
// budget is available while the usage is below the limit.
func TestMemoryBudgetCounter_Check_OneBelowLimitAllowed(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "key-boundary", OrgID: "org-boundary"}
	addTokens(tc, scopes, 999)

	limits := ScopeLimits{Key: Limits{DailyTokenLimit: 1000}}
	if err := checkTokens(tc, scopes, limits); err != nil {
		t.Errorf("Check() at 999/1000 = %v, want nil", err)
	}
	addTokens(tc, scopes, 1)
	if err := checkTokens(tc, scopes, limits); !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() at 1000/1000 = %v, want ErrTokenBudgetExceeded", err)
	}
}

// TestMemoryBudgetCounter_Check_ExceedsKeyMonthlyLimit verifies monthly limit
// enforcement at the key scope.
func TestMemoryBudgetCounter_Check_ExceedsKeyMonthlyLimit(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "key-exceed-monthly", OrgID: "org-exceed-monthly"}
	addTokens(tc, scopes, 5000)

	err := checkTokens(tc, scopes, ScopeLimits{Key: Limits{MonthlyTokenLimit: 5000}})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() = %v, want ErrTokenBudgetExceeded", err)
	}
}

// TestMemoryBudgetCounter_KeyLimitExceeded_OrgFine verifies that the key
// scope limit is enforced even when the org scope has headroom.
func TestMemoryBudgetCounter_KeyLimitExceeded_OrgFine(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "key-tight", OrgID: "org-spacious"}
	addTokens(tc, scopes, 600)

	err := checkTokens(tc, scopes, ScopeLimits{
		Key: Limits{DailyTokenLimit: 500},
		Org: Limits{DailyTokenLimit: 10000},
	})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() = %v, want ErrTokenBudgetExceeded (key 600 >= limit 500, org fine)", err)
	}
}

// TestMemoryBudgetCounter_OrgLimitExceeded_KeyFine verifies that the org
// scope limit is enforced even when the key scope has headroom.
func TestMemoryBudgetCounter_OrgLimitExceeded_KeyFine(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()

	// Two different keys of the same org contribute 300 tokens each: org
	// total 600, org limit 500.
	addTokens(tc, Scopes{KeyID: "key-a-shared", OrgID: "tight-org"}, 300)
	addTokens(tc, Scopes{KeyID: "key-b-shared", OrgID: "tight-org"}, 300)

	// The next request comes from a third key: its own limit is generous,
	// but the org is over.
	err := checkTokens(tc, Scopes{KeyID: "key-c-shared", OrgID: "tight-org"}, ScopeLimits{
		Key: Limits{DailyTokenLimit: 10000},
		Org: Limits{DailyTokenLimit: 500},
	})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() = %v, want ErrTokenBudgetExceeded (org 600 >= limit 500)", err)
	}
}

// TestMemoryBudgetCounter_TeamLimitExceeded verifies that the team scope
// limit is enforced when the key and org limits have plenty of headroom.
func TestMemoryBudgetCounter_TeamLimitExceeded(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "key-team-member", TeamID: "tight-team", OrgID: "org-team-test"}
	addTokens(tc, scopes, 400)

	err := checkTokens(tc, scopes, ScopeLimits{
		Key:  Limits{DailyTokenLimit: 1000},
		Team: Limits{DailyTokenLimit: 300},
		Org:  Limits{DailyTokenLimit: 1000},
	})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() = %v, want ErrTokenBudgetExceeded (team 400 >= limit 300)", err)
	}
}

// TestMemoryBudgetCounter_TeamLimitFine verifies that a team usage below the
// limit does not trigger ErrTokenBudgetExceeded.
func TestMemoryBudgetCounter_TeamLimitFine(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "key-team-ok", TeamID: "ok-team", OrgID: "org-team-ok"}
	addTokens(tc, scopes, 100)

	err := checkTokens(tc, scopes, ScopeLimits{
		Key:  Limits{DailyTokenLimit: 1000},
		Team: Limits{DailyTokenLimit: 300},
		Org:  Limits{DailyTokenLimit: 1000},
	})
	if err != nil {
		t.Errorf("Check() = %v, want nil (team 100 < limit 300)", err)
	}
}

// TestMemoryBudgetCounter_UserLimitExceeded verifies that a per-user limit,
// sourced from the org membership, is enforced across every key that user
// owns in that org, even when the key and org limits have plenty of headroom.
func TestMemoryBudgetCounter_UserLimitExceeded(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	// Two keys owned by the same user in the same org contribute to the same
	// user-scope budget.
	addTokens(tc, Scopes{KeyID: "key-user-a", UserID: "tight-user", OrgID: "org-user-test"}, 250)
	addTokens(tc, Scopes{KeyID: "key-user-b", UserID: "tight-user", OrgID: "org-user-test"}, 250)

	err := checkTokens(tc, Scopes{KeyID: "key-user-a", UserID: "tight-user", OrgID: "org-user-test"}, ScopeLimits{
		Key:  Limits{DailyTokenLimit: 1000},
		User: Limits{DailyTokenLimit: 400},
		Org:  Limits{DailyTokenLimit: 1000},
	})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() = %v, want ErrTokenBudgetExceeded (user 500 >= limit 400)", err)
	}
}

// TestMemoryBudgetCounter_UserLimitIsolatedPerOrg verifies that the
// user-scope counter is org-bound: the same user ID in two different orgs
// gets independent budgets.
func TestMemoryBudgetCounter_UserLimitIsolatedPerOrg(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	limits := ScopeLimits{User: Limits{DailyTokenLimit: 300}}

	addTokens(tc, Scopes{KeyID: "shared-user-key-a", UserID: "shared-user", OrgID: "org-a"}, 300)
	err := checkTokens(tc, Scopes{KeyID: "shared-user-key-a", UserID: "shared-user", OrgID: "org-a"}, limits)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("org A Check() = %v, want ErrTokenBudgetExceeded", err)
	}

	// Same user ID, different org: must not inherit org A's usage.
	err = checkTokens(tc, Scopes{KeyID: "shared-user-key-b", UserID: "shared-user", OrgID: "org-b"}, limits)
	if err != nil {
		t.Errorf("org B Check() = %v, want nil (must not share org A's user budget)", err)
	}
}

// TestMemoryBudgetCounter_EmptyUserID_NoUserEntry verifies that passing an
// empty UserID to Add does not create a user-scoped counter entry and that
// Check with an empty UserID ignores the user limit.
func TestMemoryBudgetCounter_EmptyUserID_NoUserEntry(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "key-no-user", OrgID: "org-no-user"}
	addTokens(tc, scopes, 300)

	err := checkTokens(tc, scopes, ScopeLimits{
		Key:  Limits{DailyTokenLimit: 1000},
		User: Limits{DailyTokenLimit: 1}, // would block if the user scope was checked
		Org:  Limits{DailyTokenLimit: 1000},
	})
	if err != nil {
		t.Errorf("Check() = %v, want nil (empty UserID skips user limit check)", err)
	}
	if hasScopeWithPrefix(tc, "user:") {
		t.Error("user-scoped entry was created despite empty UserID")
	}
}

// TestMemoryBudgetCounter_ZeroLimit_Unlimited verifies that a zero limit
// means unlimited: any amount of token usage passes.
func TestMemoryBudgetCounter_ZeroLimit_Unlimited(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "key-unlimited", TeamID: "team-unlimited", OrgID: "org-unlimited"}
	addTokens(tc, scopes, 1_000_000)

	if err := checkTokens(tc, scopes, ScopeLimits{}); err != nil {
		t.Errorf("Check() = %v, want nil (all limits zero = unlimited)", err)
	}
}

// TestMemoryBudgetCounter_ZeroLimitOnOneScope verifies that a zero daily
// limit on one scope does not cause a spurious failure while another scope
// has a real limit.
func TestMemoryBudgetCounter_ZeroLimitOnOneScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		keyLimit Limits
		orgLimit Limits
		tokens   int64
		wantErr  bool
	}{
		{name: "key unlimited, org limit fine", orgLimit: Limits{DailyTokenLimit: 1000}, tokens: 500},
		{name: "key unlimited, org limit exceeded", orgLimit: Limits{DailyTokenLimit: 1000}, tokens: 1000, wantErr: true},
		{name: "org unlimited, key limit fine", keyLimit: Limits{DailyTokenLimit: 1000}, tokens: 500},
		{name: "org unlimited, key limit exceeded", keyLimit: Limits{DailyTokenLimit: 1000}, tokens: 1000, wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			counter := NewMemoryBudgetCounter()
			scopes := Scopes{KeyID: "key-mixed", OrgID: "org-mixed-" + tc.name}
			addTokens(counter, scopes, tc.tokens)

			err := checkTokens(counter, scopes, ScopeLimits{Key: tc.keyLimit, Org: tc.orgLimit})
			if tc.wantErr && !errors.Is(err, ErrTokenBudgetExceeded) {
				t.Errorf("Check() = %v, want ErrTokenBudgetExceeded", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Check() = %v, want nil", err)
			}
		})
	}
}

// TestMemoryBudgetCounter_DailyWindowReset verifies that tokens added on a
// prior day are not counted in the current day, while the monthly window
// keeps them.
func TestMemoryBudgetCounter_DailyWindowReset(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Date(2026, 3, 10, 23, 0, 0, 0, time.UTC))
	tc := NewMemoryBudgetCounter()
	tc.now = clock.Now

	scopes := Scopes{KeyID: "key-stale-daily", OrgID: "org-stale-daily"}
	addTokens(tc, scopes, 900)
	clock.Add(2 * time.Hour) // next day, same month

	if err := checkTokens(tc, scopes, ScopeLimits{Key: Limits{DailyTokenLimit: 900}}); err != nil {
		t.Errorf("daily Check() = %v, want nil (yesterday's usage must read as 0)", err)
	}
	err := checkTokens(tc, scopes, ScopeLimits{Key: Limits{MonthlyTokenLimit: 900}})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("monthly Check() = %v, want ErrTokenBudgetExceeded (same month)", err)
	}
}

// TestMemoryBudgetCounter_MonthlyWindowReset verifies that tokens from a
// prior month are not counted in the current month's window.
func TestMemoryBudgetCounter_MonthlyWindowReset(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Date(2026, 3, 31, 23, 0, 0, 0, time.UTC))
	tc := NewMemoryBudgetCounter()
	tc.now = clock.Now

	scopes := Scopes{KeyID: "key-stale-monthly", OrgID: "org-stale-monthly"}
	addTokens(tc, scopes, 4500)
	clock.Add(2 * time.Hour) // April 1st

	if err := checkTokens(tc, scopes, ScopeLimits{Key: Limits{MonthlyTokenLimit: 4500}}); err != nil {
		t.Errorf("Check() = %v, want nil (last month's usage must read as 0)", err)
	}
}

// TestMemoryBudgetCounter_AddResetsOnWindowRollover verifies that an Add in a
// new window starts from zero rather than accumulating on the old window.
func TestMemoryBudgetCounter_AddResetsOnWindowRollover(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Date(2026, 3, 10, 23, 0, 0, 0, time.UTC))
	tc := NewMemoryBudgetCounter()
	tc.now = clock.Now

	scopes := Scopes{KeyID: "key-rollover", OrgID: "org-rollover"}
	addTokens(tc, scopes, 9999)
	clock.Add(2 * time.Hour)
	addTokens(tc, scopes, 100)

	if err := checkTokens(tc, scopes, ScopeLimits{Key: Limits{DailyTokenLimit: 500}}); err != nil {
		t.Errorf("Check() = %v, want nil (100 tokens after rollover, limit 500)", err)
	}
	got := tc.usage(KeyScope("key-rollover"), windowsAt(clock.Now()))
	if got.Daily.Tokens != 100 || got.Monthly.Tokens != 10099 {
		t.Errorf("usage = daily %d / monthly %d, want 100 / 10099", got.Daily.Tokens, got.Monthly.Tokens)
	}
}

// TestMemoryBudgetCounter_LateAddDoesNotRollBack verifies that usage stamped
// with an already finished window neither reopens that window nor lands in
// the current one.
func TestMemoryBudgetCounter_LateAddDoesNotRollBack(t *testing.T) {
	t.Parallel()

	today := windowsAt(time.Date(2026, 3, 11, 0, 0, 1, 0, time.UTC))
	yesterday := windowsAt(time.Date(2026, 3, 10, 23, 59, 59, 0, time.UTC))

	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "key-late", OrgID: "org-late"}
	tc.addAt(today, scopes, Usage{Tokens: 10})
	tc.addAt(yesterday, scopes, Usage{Tokens: 500})

	got := tc.usage(KeyScope("key-late"), today)
	if got.Daily.Tokens != 10 {
		t.Errorf("daily tokens = %d, want 10 (late add must not count today)", got.Daily.Tokens)
	}
	if got.Monthly.Tokens != 510 {
		t.Errorf("monthly tokens = %d, want 510 (same month)", got.Monthly.Tokens)
	}
}

// TestMemoryBudgetCounter_EvictStale removes entries without usage in the
// current windows and verifies that current entries survive.
func TestMemoryBudgetCounter_EvictStale(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC))
	tc := NewMemoryBudgetCounter()
	tc.now = clock.Now

	addTokens(tc, Scopes{KeyID: "stale-month-key", OrgID: "stale-org"}, 100)

	clock.Set(time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC))
	addTokens(tc, Scopes{KeyID: "stale-day-key", OrgID: "current-org"}, 200)

	clock.Set(time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC))
	addTokens(tc, Scopes{KeyID: "key-current", OrgID: "current-org"}, 50)

	tc.EvictStale()

	for _, scope := range []string{KeyScope("stale-month-key"), OrgScope("stale-org")} {
		if _, ok := tc.entries.Load(scope); ok {
			t.Errorf("entry %q from last month still present after EvictStale", scope)
		}
	}
	// Used this month but not today: the monthly window is still live.
	if _, ok := tc.entries.Load(KeyScope("stale-day-key")); !ok {
		t.Error("entry with usage in the current month was wrongly evicted")
	}
	if got := tc.usage(KeyScope("stale-day-key"), windowsAt(clock.Now())); got.Monthly.Tokens != 200 || got.Daily.Tokens != 0 {
		t.Errorf("stale-day-key usage = daily %d / monthly %d, want 0 / 200", got.Daily.Tokens, got.Monthly.Tokens)
	}
	for _, scope := range []string{KeyScope("key-current"), OrgScope("current-org")} {
		if _, ok := tc.entries.Load(scope); !ok {
			t.Errorf("current entry %q was wrongly evicted", scope)
		}
	}
}

// TestMemoryBudgetCounter_NoTeamID_NoTeamEntry verifies that passing an empty
// TeamID to Add does not create team-scoped counter entries and that Check
// with an empty TeamID ignores team limits.
func TestMemoryBudgetCounter_NoTeamID_NoTeamEntry(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "key-no-team", OrgID: "org-no-team"}
	addTokens(tc, scopes, 300)

	err := checkTokens(tc, scopes, ScopeLimits{
		Key:  Limits{DailyTokenLimit: 1000},
		Team: Limits{DailyTokenLimit: 1}, // would block if team was checked
		Org:  Limits{DailyTokenLimit: 1000},
	})
	if err != nil {
		t.Errorf("Check() = %v, want nil (empty TeamID skips team limit check)", err)
	}
	if hasScopeWithPrefix(tc, "team:") {
		t.Error("team-scoped entry was created despite empty TeamID")
	}
}

// TestMemoryBudgetCounter_ConcurrentAdd verifies that 100 goroutines each
// adding 10 tokens result in exactly 1000 total tokens for the key scope.
func TestMemoryBudgetCounter_ConcurrentAdd(t *testing.T) {
	t.Parallel()

	const (
		goroutines = 100
		tokensEach = 10
		wantTotal  = goroutines * tokensEach
	)

	clock := newFakeClock(time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC))
	tc := NewMemoryBudgetCounter()
	tc.now = clock.Now
	scopes := Scopes{KeyID: "concurrent-key", OrgID: "concurrent-org"}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			addTokens(tc, scopes, tokensEach)
		}()
	}
	wg.Wait()

	got := tc.usage(KeyScope("concurrent-key"), windowsAt(clock.Now()))
	if got.Daily.Tokens != wantTotal || got.Monthly.Tokens != wantTotal {
		t.Errorf("concurrent Add total = daily %d / monthly %d, want %d", got.Daily.Tokens, got.Monthly.Tokens, wantTotal)
	}
}

// TestMemoryBudgetCounter_ConcurrentAddAndCheck verifies concurrent Add and
// Check calls do not race (checked by the -race flag). Exact counts are
// covered by TestMemoryBudgetCounter_ConcurrentAdd.
func TestMemoryBudgetCounter_ConcurrentAddAndCheck(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	limits := ScopeLimits{Key: Limits{DailyTokenLimit: 100_000}}

	var wg sync.WaitGroup
	var checkErrors atomic.Int64

	for i := range 50 {
		wg.Add(2)
		scopes := Scopes{KeyID: fmt.Sprintf("race-key-%d", i%5), OrgID: "race-org"} // reuse 5 keys to create contention
		go func() {
			defer wg.Done()
			addTokens(tc, scopes, 10)
		}()
		go func() {
			defer wg.Done()
			if err := checkTokens(tc, scopes, limits); err != nil {
				checkErrors.Add(1)
			}
		}()
	}
	wg.Wait()

	// With limit 100_000 and at most 100 tokens per key, no check should fail.
	if n := checkErrors.Load(); n > 0 {
		t.Errorf("%d Check calls returned unexpected errors under low load", n)
	}
}

// TestMemoryBudgetCounter_RolloverLosesNoIncrements proves the fix for lost
// increments at a window rollover: while many goroutines add concurrently the
// clock jumps from the last second of March to the first second of April, so
// both the daily and the monthly window roll over under contention. Every
// Add that was stamped with the new window must be counted in it, exactly.
func TestMemoryBudgetCounter_RolloverLosesNoIncrements(t *testing.T) {
	t.Parallel()

	const (
		rounds     = 300
		goroutines = 16
		addsEach   = 200
		tokens     = 3
		costMicros = 7
	)
	before := time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC)
	after := time.Date(2026, 4, 1, 0, 0, 1, 0, time.UTC)

	for round := range rounds {
		var flipped atomic.Bool
		var stampedAfter atomic.Int64

		tc := NewMemoryBudgetCounter()
		// Add reads the clock exactly once, so stampedAfter counts the Adds
		// that belong to the new windows.
		tc.now = func() time.Time {
			if flipped.Load() {
				stampedAfter.Add(1)
				return after
			}
			return before
		}
		scopes := Scopes{KeyID: "roll-key", UserID: "roll-user", TeamID: "roll-team", OrgID: "roll-org"}

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for range goroutines {
			go func() {
				defer wg.Done()
				<-start
				for range addsEach {
					tc.Add(context.Background(), scopes, Usage{Tokens: tokens, CostMicros: costMicros})
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Flip somewhere in the middle of the run; the offset varies by
			// round so the rollover hits different interleavings.
			for range round % 50 {
				addTokens(tc, Scopes{KeyID: "spin", OrgID: "spin"}, 0)
			}
			flipped.Store(true)
		}()
		close(start)
		wg.Wait()

		n := stampedAfter.Load()
		w := windowsAt(after)
		for _, scope := range []string{
			KeyScope("roll-key"), UserScope("roll-org", "roll-user"), TeamScope("roll-team"), OrgScope("roll-org"),
		} {
			got := tc.usage(scope, w)
			want := Usage{Tokens: n * tokens, CostMicros: n * costMicros}
			if got.Daily != want || got.Monthly != want {
				t.Fatalf("round %d scope %s: usage = daily %+v / monthly %+v, want %+v for %d adds in the new window",
					round, scope, got.Daily, got.Monthly, want, n)
			}
		}
	}
}

// TestMemoryBudgetCounter_EvictStaleLosesNoIncrements runs EvictStale against
// concurrent Adds that arrive right after a month rollover, when every entry
// is stale and eligible for eviction. No Add may end up in an entry that was
// evicted.
func TestMemoryBudgetCounter_EvictStaleLosesNoIncrements(t *testing.T) {
	t.Parallel()

	const (
		rounds     = 200
		goroutines = 8
		addsEach   = 100
	)
	for round := range rounds {
		clock := newFakeClock(time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC))
		tc := NewMemoryBudgetCounter()
		tc.now = clock.Now
		scopes := Scopes{KeyID: "evict-key", TeamID: "evict-team", OrgID: "evict-org"}
		addTokens(tc, scopes, 1000) // last month's usage: stale after the jump
		clock.Set(time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC))

		start := make(chan struct{})
		stop := make(chan struct{})
		var adders, evictor sync.WaitGroup
		adders.Add(goroutines)
		for range goroutines {
			go func() {
				defer adders.Done()
				<-start
				for range addsEach {
					addTokens(tc, scopes, 1)
				}
			}()
		}
		evictor.Add(1)
		go func() {
			defer evictor.Done()
			<-start
			for {
				select {
				case <-stop:
					return
				default:
					tc.EvictStale()
				}
			}
		}()
		close(start)
		adders.Wait()
		close(stop)
		evictor.Wait()

		w := windowsAt(clock.Now())
		const want = goroutines * addsEach
		for _, scope := range []string{KeyScope("evict-key"), TeamScope("evict-team"), OrgScope("evict-org")} {
			got := tc.usage(scope, w)
			if got.Daily.Tokens != want || got.Monthly.Tokens != want {
				t.Fatalf("round %d scope %s: tokens = daily %d / monthly %d, want %d",
					round, scope, got.Daily.Tokens, got.Monthly.Tokens, want)
			}
		}
	}
}

// TestMemoryBudgetCounter_CostCarriedThrough verifies that cost is counted
// next to tokens and reported by Snapshot, and that it does not influence
// Check.
func TestMemoryBudgetCounter_CostCarriedThrough(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "cost-key", UserID: "cost-user", TeamID: "cost-team", OrgID: "cost-org"}
	tc.Add(context.Background(), scopes, Usage{Tokens: 10, CostMicros: 2_500_000})
	tc.Add(context.Background(), scopes, Usage{Tokens: 5, CostMicros: 500_000})
	// Negative amounts are ignored.
	tc.Add(context.Background(), scopes, Usage{Tokens: -100, CostMicros: -100})

	keys := []string{
		KeyScope("cost-key"), UserScope("cost-org", "cost-user"), TeamScope("cost-team"), OrgScope("cost-org"),
		KeyScope("unknown"),
	}
	snap, err := tc.Snapshot(context.Background(), keys)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(snap) != len(keys) {
		t.Fatalf("Snapshot() returned %d entries, want %d", len(snap), len(keys))
	}
	want := Usage{Tokens: 15, CostMicros: 3_000_000}
	for _, k := range keys[:4] {
		if snap[k].Daily != want || snap[k].Monthly != want {
			t.Errorf("Snapshot()[%q] = %+v, want daily and monthly %+v", k, snap[k], want)
		}
	}
	if got := snap[KeyScope("unknown")]; got != (WindowUsage{}) {
		t.Errorf("Snapshot() for an unknown scope = %+v, want zero", got)
	}

	if err := checkTokens(tc, scopes, ScopeLimits{Key: Limits{DailyTokenLimit: 16}}); err != nil {
		t.Errorf("Check() = %v, want nil (15 tokens < 16; cost has no limit)", err)
	}
}

// ---- Seed ---------------------------------------------------------------------

// seedRow is one (key_id, team_id, org_id, user_id, total_tokens,
// cost_estimate, in_day) tuple returned by a fakeUsageSeeder, matching the
// column order of db.QueryUsageSeed / ratelimit.UsageSeeder. A row is from
// the current day unless earlierThisMonth is set.
type seedRow struct {
	keyID, teamID, orgID, userID string
	tokens                       int64
	cost                         float64
	earlierThisMonth             bool
}

// persistedEvent is one usage event in the fake database that
// QueryUsageTotals reads: usage of one key recorded at a point in time.
type persistedEvent struct {
	keyID, teamID, orgID, userID string
	tokens                       int64
	cost                         float64
	at                           time.Time
}

// fakeUsageSeeder is an in-memory UsageSeeder for testing seeding and
// refreshing without a real database. It returns rows for every seed query
// and counts those queries. QueryUsageTotals reads events, honouring its time
// range, and remembers the ranges it was asked for.
type fakeUsageSeeder struct {
	mu        sync.Mutex
	rows      []seedRow
	err       error
	calls     int
	events    []persistedEvent
	eventsErr error
	ranges    []timeRange
}

// timeRange is one [from, until) range a QueryUsageTotals call asked for.
type timeRange struct{ from, until time.Time }

func (f *fakeUsageSeeder) QueryUsageTotals(_ context.Context, from, until, dayStart time.Time) (RowScanner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ranges = append(f.ranges, timeRange{from, until})
	if f.eventsErr != nil {
		return nil, f.eventsErr
	}
	var rows []persistedEvent
	for _, e := range f.events {
		if e.at.Before(from) || (!until.IsZero() && !e.at.Before(until)) {
			continue
		}
		rows = append(rows, e)
	}
	return &fakeTotalsScanner{rows: rows, dayStart: dayStart, idx: -1}, nil
}

// setEvents replaces the events subsequent QueryUsageTotals calls read and
// clears or sets their error.
func (f *fakeUsageSeeder) setEvents(err error, events ...persistedEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events, f.eventsErr = events, err
}

// addEvents appends to the events QueryUsageTotals reads.
func (f *fakeUsageSeeder) addEvents(events ...persistedEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, events...)
}

// takeRanges returns the ranges QueryUsageTotals was asked for since the last
// call and forgets them.
func (f *fakeUsageSeeder) takeRanges() []timeRange {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.ranges
	f.ranges = nil
	return out
}

// fakeTotalsScanner implements ratelimit.RowScanner with the column order of
// QueryUsageTotals, one row per event.
type fakeTotalsScanner struct {
	rows     []persistedEvent
	dayStart time.Time
	idx      int
}

func (f *fakeTotalsScanner) Next() bool {
	f.idx++
	return f.idx < len(f.rows)
}

func (f *fakeTotalsScanner) Scan(dest ...any) error {
	r := f.rows[f.idx]
	*dest[0].(*string) = r.orgID
	*dest[1].(*string) = r.teamID
	*dest[2].(*string) = r.userID
	*dest[3].(*string) = r.keyID
	*dest[4].(*int64) = r.tokens
	*dest[5].(*float64) = r.cost
	*dest[6].(*int64) = 0
	*dest[7].(*float64) = 0
	if !r.at.Before(f.dayStart) {
		*dest[6].(*int64) = r.tokens
		*dest[7].(*float64) = r.cost
	}
	return nil
}

func (f *fakeTotalsScanner) Close() error { return nil }
func (f *fakeTotalsScanner) Err() error   { return nil }

func (f *fakeUsageSeeder) QueryUsageSeed(_ context.Context, _, _ time.Time) (RowScanner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return &fakeRowScanner{rows: append([]seedRow(nil), f.rows...), idx: -1}, nil
}

// queries returns how many times QueryUsageSeed was called.
func (f *fakeUsageSeeder) queries() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeRowScanner implements ratelimit.RowScanner over an in-memory slice.
type fakeRowScanner struct {
	rows []seedRow
	idx  int
}

func (f *fakeRowScanner) Next() bool {
	f.idx++
	return f.idx < len(f.rows)
}

func (f *fakeRowScanner) Scan(dest ...any) error {
	r := f.rows[f.idx]
	*dest[0].(*string) = r.keyID
	*dest[1].(*string) = r.teamID
	*dest[2].(*string) = r.orgID
	*dest[3].(*string) = r.userID
	*dest[4].(*int64) = r.tokens
	*dest[5].(*float64) = r.cost
	*dest[6].(*int64) = 1
	if r.earlierThisMonth {
		*dest[6].(*int64) = 0
	}
	return nil
}

func (f *fakeRowScanner) Close() error { return nil }
func (f *fakeRowScanner) Err() error   { return nil }

// TestMemoryBudgetCounter_Seed_PopulatesUserScope verifies that Seed loads
// rows with a non-empty user_id into the org-bound user-scope counter,
// alongside the key, team, and org scopes, so that a freshly started process
// immediately enforces per-user limits against already-persisted usage.
func TestMemoryBudgetCounter_Seed_PopulatesUserScope(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	seeder := &fakeUsageSeeder{rows: []seedRow{
		{keyID: "seed-key-1", teamID: "seed-team-1", orgID: "seed-org-1", userID: "seed-user-1", tokens: 300},
		{keyID: "seed-key-2", teamID: "seed-team-1", orgID: "seed-org-1", userID: "seed-user-1", tokens: 200},
	}}

	if err := tc.Seed(context.Background(), seeder); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	// Two keys owned by seed-user-1 contributed 300+200=500 tokens, over the
	// 400 limit, even though a third key of the same user never called Add.
	err := checkTokens(tc, Scopes{KeyID: "seed-key-3", UserID: "seed-user-1", OrgID: "seed-org-1"},
		ScopeLimits{User: Limits{DailyTokenLimit: 400}})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() after Seed = %v, want ErrTokenBudgetExceeded (seeded user total 500 >= limit 400)", err)
	}
}

// TestMemoryBudgetCounter_Seed_OneQueryFeedsBothWindows verifies that Seed
// reads the database once, that every row counts towards the month but only
// rows of the current day towards the day, and that cost is converted to
// micro-units by rounding.
func TestMemoryBudgetCounter_Seed_OneQueryFeedsBothWindows(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	seeder := &fakeUsageSeeder{rows: []seedRow{
		{keyID: "seed-once-key", teamID: "seed-once-team", orgID: "seed-once-org", userID: "seed-once-user", tokens: 300, cost: 0.0123456},
		{keyID: "seed-once-key", orgID: "seed-once-org", tokens: 200, cost: 0.0000004}, // rounds to 0
		{keyID: "seed-once-key", orgID: "seed-once-org", tokens: 1, cost: 0.0000006},   // rounds to 1
		{keyID: "seed-once-key", teamID: "seed-once-team", orgID: "seed-once-org", tokens: 4000, cost: 1, earlierThisMonth: true},
	}}
	if err := tc.Seed(context.Background(), seeder); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}
	if got := seeder.queries(); got != 1 {
		t.Errorf("Seed() ran %d queries, want 1", got)
	}

	snap, err := tc.Snapshot(context.Background(), []string{KeyScope("seed-once-key"), TeamScope("seed-once-team")})
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	wantKey := WindowUsage{
		Daily:   Usage{Tokens: 501, CostMicros: 12347},
		Monthly: Usage{Tokens: 4501, CostMicros: 1_012_347},
	}
	if got := snap[KeyScope("seed-once-key")]; got != wantKey {
		t.Errorf("key usage = %+v, want %+v", got, wantKey)
	}
	wantTeam := WindowUsage{
		Daily:   Usage{Tokens: 300, CostMicros: 12346},
		Monthly: Usage{Tokens: 4300, CostMicros: 1_012_346},
	}
	if got := snap[TeamScope("seed-once-team")]; got != wantTeam {
		t.Errorf("team usage = %+v, want %+v", got, wantTeam)
	}
}

// TestMemoryBudgetCounter_Seed_UserScopeIsolatedPerOrg verifies that Seed
// keys the user-scope counter by org, matching Add and Check: two rows with
// the same user_id but different org_id must not share a budget.
func TestMemoryBudgetCounter_Seed_UserScopeIsolatedPerOrg(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	seeder := &fakeUsageSeeder{rows: []seedRow{
		{keyID: "seed-iso-key-a", orgID: "seed-iso-org-a", userID: "seed-iso-user", tokens: 500},
		{keyID: "seed-iso-key-b", orgID: "seed-iso-org-b", userID: "seed-iso-user", tokens: 50},
	}}

	if err := tc.Seed(context.Background(), seeder); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	limits := ScopeLimits{User: Limits{DailyTokenLimit: 400}}

	// Org A: seeded 500 >= limit 400.
	err := checkTokens(tc, Scopes{KeyID: "seed-iso-key-a", UserID: "seed-iso-user", OrgID: "seed-iso-org-a"}, limits)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("org A Check() = %v, want ErrTokenBudgetExceeded", err)
	}

	// Org B: seeded only 50 < limit 400, must not inherit org A's total.
	err = checkTokens(tc, Scopes{KeyID: "seed-iso-key-b", UserID: "seed-iso-user", OrgID: "seed-iso-org-b"}, limits)
	if err != nil {
		t.Errorf("org B Check() = %v, want nil (must not share org A's seeded user budget)", err)
	}
}

// TestMemoryBudgetCounter_Seed_EmptyUserID_NoUserEntry verifies that Seed
// does not create a user-scoped counter for rows with an empty user_id
// (team_key and sa_key usage events have no owning user).
func TestMemoryBudgetCounter_Seed_EmptyUserID_NoUserEntry(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	seeder := &fakeUsageSeeder{rows: []seedRow{
		{keyID: "seed-noUser-key", teamID: "seed-noUser-team", orgID: "seed-noUser-org", userID: "", tokens: 900},
	}}

	if err := tc.Seed(context.Background(), seeder); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}
	if hasScopeWithPrefix(tc, "user:") {
		t.Error("user-scoped entry was created for a seed row with empty user_id")
	}

	// Key, team and org scopes must still be seeded normally.
	err := checkTokens(tc, Scopes{KeyID: "seed-noUser-key", TeamID: "seed-noUser-team", OrgID: "seed-noUser-org"},
		ScopeLimits{Team: Limits{DailyTokenLimit: 800}})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() = %v, want ErrTokenBudgetExceeded (team seeded 900 >= limit 800)", err)
	}
}

// TestMemoryBudgetCounter_Seed_QueryError propagates a query error from the
// seeder.
func TestMemoryBudgetCounter_Seed_QueryError(t *testing.T) {
	t.Parallel()

	tc := NewMemoryBudgetCounter()
	wantErr := errors.New("boom")
	err := tc.Seed(context.Background(), &fakeUsageSeeder{err: wantErr})
	if err == nil {
		t.Fatal("Seed() error = nil, want non-nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Seed() error = %v, want it to wrap %v", err, wantErr)
	}
}

// TestCostToMicros covers the conversion used when seeding cost.
func TestCostToMicros(t *testing.T) {
	t.Parallel()

	nan := 0.0
	nan /= nan
	tests := []struct {
		name string
		in   float64
		want int64
	}{
		{"zero", 0, 0},
		{"negative", -1.5, 0},
		{"nan", nan, 0},
		{"one micro", 0.000001, 1},
		{"rounds up", 0.0000016, 2},
		{"rounds down", 0.0000014, 1},
		{"major units", 25, 25_000_000},
		{"float noise", 0.000123, 123},
		{"beyond int64", 1e300, 1<<63 - 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CostToMicros(tc.in); got != tc.want {
				t.Errorf("CostToMicros(%v) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// BenchmarkMemoryBudgetCounter_Add benchmarks the hot-path usage addition.
func BenchmarkMemoryBudgetCounter_Add(b *testing.B) {
	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "bench-key", TeamID: "bench-team", OrgID: "bench-org"}
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tc.Add(ctx, scopes, Usage{Tokens: 100})
		}
	})
}

// BenchmarkMemoryBudgetCounter_Check benchmarks token budget checking.
func BenchmarkMemoryBudgetCounter_Check(b *testing.B) {
	tc := NewMemoryBudgetCounter()
	scopes := Scopes{KeyID: "bench-key", TeamID: "bench-team", OrgID: "bench-org"}
	ctx := context.Background()
	tc.Add(ctx, scopes, Usage{Tokens: 100})
	limits := ScopeLimits{Key: Limits{DailyTokenLimit: 1_000_000, MonthlyTokenLimit: 10_000_000}}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := tc.Check(ctx, scopes, limits); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestCheck_RetryAfterLatestReset verifies that the returned LimitError
// carries the latest reset among all exceeded windows, and names the budget
// that reset belongs to.
func TestCheck_RetryAfterLatestReset(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 3, 10, 18, 30, 0, 0, time.UTC)
	tc := NewMemoryBudgetCounter()
	tc.now = newFakeClock(now).Now
	s := Scopes{KeyID: "ra-key", OrgID: "ra-org"}
	addTokens(tc, s, 100)

	untilMidnight := 5*time.Hour + 30*time.Minute
	untilMonth := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).Sub(now)

	daily := ScopeLimits{Org: Limits{DailyTokenLimit: 100}}
	err := checkTokens(tc, s, daily)
	if d, ok := RetryAfterFromError(err); !ok || d != untilMidnight {
		t.Errorf("daily RetryAfter = %v, want %v", d, untilMidnight)
	}
	var le *LimitError
	if !errors.As(err, &le) || le.Metric != "tokens" || le.Scope != "org" || le.Window != "daily" {
		t.Errorf("daily LimitError = %+v, want metric tokens, scope org, window daily", le)
	}

	monthly := ScopeLimits{Key: Limits{MonthlyTokenLimit: 100}}
	err = checkTokens(tc, s, monthly)
	if d, ok := RetryAfterFromError(err); !ok || d != untilMonth {
		t.Errorf("monthly RetryAfter = %v, want %v", d, untilMonth)
	}
	if !errors.As(err, &le) || le.Scope != "key" || le.Window != "monthly" {
		t.Errorf("monthly LimitError = %+v, want scope key, window monthly", le)
	}

	// Daily (key, checked first) and monthly (org, checked last) exceeded at
	// once: blocked until the month resets. Returning the first exceeded
	// window would report the daily reset and fail this assertion.
	both := ScopeLimits{Key: Limits{DailyTokenLimit: 100}, Org: Limits{MonthlyTokenLimit: 100}}
	err = checkTokens(tc, s, both)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Fatalf("err = %v, want ErrTokenBudgetExceeded", err)
	}
	if d, ok := RetryAfterFromError(err); !ok || d != untilMonth {
		t.Errorf("combined RetryAfter = %v, want %v (latest reset)", d, untilMonth)
	}
	if !errors.As(err, &le) || le.Scope != "org" || le.Window != "monthly" {
		t.Errorf("combined LimitError = %+v, want scope org, window monthly", le)
	}
}
