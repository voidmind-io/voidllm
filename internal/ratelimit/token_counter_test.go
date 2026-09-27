package ratelimit

import (
	"context"
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
	scopes := Scopes{KeyID: "key1", OrgID: "org1"}
	tc.Add(scopes, 500)

	keyLimits := Limits{DailyTokenLimit: 1000}
	noLimits := Limits{}

	if err := tc.CheckTokens(scopes, ScopeLimits{Key: keyLimits, Team: noLimits, Org: noLimits}); err != nil {
		t.Errorf("CheckTokens() = %v, want nil (500 tokens < 1000 limit)", err)
	}
}

// TestTokenCounter_CheckTokens_ExceedsKeyDailyLimit verifies that adding tokens
// up to the key daily limit causes CheckTokens to return ErrTokenBudgetExceeded.
func TestTokenCounter_CheckTokens_ExceedsKeyDailyLimit(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	scopes := Scopes{KeyID: "key-exceed-daily", OrgID: "org-exceed-daily"}
	tc.Add(scopes, 1000)

	keyLimits := Limits{DailyTokenLimit: 1000}
	noLimits := Limits{}

	err := tc.CheckTokens(scopes, ScopeLimits{Key: keyLimits, Team: noLimits, Org: noLimits})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("CheckTokens() = %v, want ErrTokenBudgetExceeded (1000 >= limit 1000)", err)
	}
}

// TestTokenCounter_CheckTokens_ExceedsKeyMonthlyLimit verifies monthly limit
// enforcement at the key scope.
func TestTokenCounter_CheckTokens_ExceedsKeyMonthlyLimit(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	scopes := Scopes{KeyID: "key-exceed-monthly", OrgID: "org-exceed-monthly"}
	tc.Add(scopes, 5000)

	keyLimits := Limits{MonthlyTokenLimit: 5000}
	noLimits := Limits{}

	err := tc.CheckTokens(scopes, ScopeLimits{Key: keyLimits, Team: noLimits, Org: noLimits})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("CheckTokens() = %v, want ErrTokenBudgetExceeded", err)
	}
}

// TestTokenCounter_KeyLimitExceeded_OrgFine verifies that the key scope limit
// is enforced even when the org scope has headroom.
func TestTokenCounter_KeyLimitExceeded_OrgFine(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	scopes := Scopes{KeyID: "key-tight", OrgID: "org-spacious"}
	// Add 600 tokens for one key in the org — key limit 500, org limit 10000.
	tc.Add(scopes, 600)

	keyLimits := Limits{DailyTokenLimit: 500}
	noLimits := Limits{}
	orgLimits := Limits{DailyTokenLimit: 10000}

	err := tc.CheckTokens(scopes, ScopeLimits{Key: keyLimits, Team: noLimits, Org: orgLimits})
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
	tc.Add(Scopes{KeyID: "key-a-shared", OrgID: "tight-org"}, 300)
	tc.Add(Scopes{KeyID: "key-b-shared", OrgID: "tight-org"}, 300)

	// The next request comes from key-c: key limit is generous, but org is over.
	keyLimits := Limits{DailyTokenLimit: 10000}
	noLimits := Limits{}
	orgLimits := Limits{DailyTokenLimit: 500}

	err := tc.CheckTokens(Scopes{KeyID: "key-c-shared", OrgID: "tight-org"}, ScopeLimits{Key: keyLimits, Team: noLimits, Org: orgLimits})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("CheckTokens() = %v, want ErrTokenBudgetExceeded (org 600 >= limit 500)", err)
	}
}

// TestTokenCounter_TeamLimitExceeded verifies that the team scope limit is
// enforced when the key and org limits have plenty of headroom.
func TestTokenCounter_TeamLimitExceeded(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	scopes := Scopes{KeyID: "key-team-member", TeamID: "tight-team", OrgID: "org-team-test"}
	// Add 400 tokens attributed to team "tight-team".
	tc.Add(scopes, 400)

	keyLimits := Limits{DailyTokenLimit: 1000}
	teamLimits := Limits{DailyTokenLimit: 300}
	orgLimits := Limits{DailyTokenLimit: 1000}

	err := tc.CheckTokens(scopes, ScopeLimits{Key: keyLimits, Team: teamLimits, Org: orgLimits})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("CheckTokens() = %v, want ErrTokenBudgetExceeded (team 400 >= limit 300)", err)
	}
}

// TestTokenCounter_TeamLimitFine verifies that a team usage below the limit
// does not trigger ErrTokenBudgetExceeded.
func TestTokenCounter_TeamLimitFine(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	scopes := Scopes{KeyID: "key-team-ok", TeamID: "ok-team", OrgID: "org-team-ok"}
	tc.Add(scopes, 100)

	keyLimits := Limits{DailyTokenLimit: 1000}
	teamLimits := Limits{DailyTokenLimit: 300}
	orgLimits := Limits{DailyTokenLimit: 1000}

	if err := tc.CheckTokens(scopes, ScopeLimits{Key: keyLimits, Team: teamLimits, Org: orgLimits}); err != nil {
		t.Errorf("CheckTokens() = %v, want nil (team 100 < limit 300)", err)
	}
}

// TestTokenCounter_UserLimitExceeded verifies that a per-user limit, sourced
// from the org membership, is enforced across every key that user owns in
// that org, even when the key and org limits have plenty of headroom.
func TestTokenCounter_UserLimitExceeded(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	// Two keys owned by the same user in the same org contribute to the same
	// user-scope budget.
	tc.Add(Scopes{KeyID: "key-user-a", UserID: "tight-user", OrgID: "org-user-test"}, 250)
	tc.Add(Scopes{KeyID: "key-user-b", UserID: "tight-user", OrgID: "org-user-test"}, 250)

	keyLimits := Limits{DailyTokenLimit: 1000}
	userLimits := Limits{DailyTokenLimit: 400}
	orgLimits := Limits{DailyTokenLimit: 1000}

	err := tc.CheckTokens(Scopes{KeyID: "key-user-a", UserID: "tight-user", OrgID: "org-user-test"},
		ScopeLimits{Key: keyLimits, User: userLimits, Org: orgLimits})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("CheckTokens() = %v, want ErrTokenBudgetExceeded (user 500 >= limit 400)", err)
	}
}

// TestTokenCounter_UserLimitIsolatedPerOrg verifies that the user-scope
// counter is org-bound: the same user ID in two different orgs gets
// independent budgets.
func TestTokenCounter_UserLimitIsolatedPerOrg(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	userLimits := Limits{DailyTokenLimit: 300}
	noLimits := Limits{}

	tc.Add(Scopes{KeyID: "shared-user-key-a", UserID: "shared-user", OrgID: "org-a"}, 300)
	err := tc.CheckTokens(Scopes{KeyID: "shared-user-key-a", UserID: "shared-user", OrgID: "org-a"},
		ScopeLimits{Key: noLimits, User: userLimits, Org: noLimits})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("org A CheckTokens() = %v, want ErrTokenBudgetExceeded", err)
	}

	// Same user ID, different org — must not inherit org A's usage.
	err = tc.CheckTokens(Scopes{KeyID: "shared-user-key-b", UserID: "shared-user", OrgID: "org-b"},
		ScopeLimits{Key: noLimits, User: userLimits, Org: noLimits})
	if err != nil {
		t.Errorf("org B CheckTokens() = %v, want nil (must not share org A's user budget)", err)
	}
}

// TestTokenCounter_EmptyUserID_NoUserEntry verifies that passing an empty
// UserID to Add does not create a user-scoped counter entry and that
// CheckTokens with an empty UserID ignores the user limit.
func TestTokenCounter_EmptyUserID_NoUserEntry(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	scopes := Scopes{KeyID: "key-no-user", OrgID: "org-no-user"}
	tc.Add(scopes, 300)

	keyLimits := Limits{DailyTokenLimit: 1000}
	userLimits := Limits{DailyTokenLimit: 1} // would block if the user scope was checked
	orgLimits := Limits{DailyTokenLimit: 1000}

	if err := tc.CheckTokens(scopes, ScopeLimits{Key: keyLimits, User: userLimits, Org: orgLimits}); err != nil {
		t.Errorf("CheckTokens() = %v, want nil (empty UserID skips user limit check)", err)
	}

	found := false
	tc.dailyCounters.Range(func(k, _ any) bool {
		if len(k.(string)) >= 5 && k.(string)[:5] == "user:" {
			found = true
			return false
		}
		return true
	})
	if found {
		t.Error("user-scoped entry was created despite empty UserID")
	}
}

// TestTokenCounter_ZeroLimit_Unlimited verifies that a zero limit means
// unlimited — any amount of token usage passes.
func TestTokenCounter_ZeroLimit_Unlimited(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	// Add a very large number of tokens.
	tc.Add(Scopes{KeyID: "key-unlimited", TeamID: "team-unlimited", OrgID: "org-unlimited"}, 1_000_000)

	noLimits := Limits{} // all zeroes = unlimited

	if err := tc.CheckTokens(Scopes{KeyID: "key-unlimited", TeamID: "team-unlimited", OrgID: "org-unlimited"},
		ScopeLimits{Key: noLimits, Team: noLimits, Org: noLimits}); err != nil {
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
			scopes := Scopes{KeyID: "key-mixed", OrgID: "org-mixed-" + tc.name}
			counter.Add(scopes, tc.tokens)

			err := counter.CheckTokens(scopes, ScopeLimits{Key: tc.keyLimit, Team: tc.teamLimit, Org: tc.orgLimit})
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

	if err := tc.CheckTokens(Scopes{KeyID: "key-stale-daily", OrgID: "org-stale-daily"},
		ScopeLimits{Key: keyLimits, Team: noLimits, Org: noLimits}); err != nil {
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

	if err := tc.CheckTokens(Scopes{KeyID: "key-stale-monthly", OrgID: "org-stale-monthly"},
		ScopeLimits{Key: keyLimits, Team: noLimits, Org: noLimits}); err != nil {
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
	tc.Add(Scopes{KeyID: "key-rollover", OrgID: "org-rollover"}, 100)

	keyLimits := Limits{DailyTokenLimit: 500}
	noLimits := Limits{}

	if err := tc.CheckTokens(Scopes{KeyID: "key-rollover", OrgID: "org-rollover"},
		ScopeLimits{Key: keyLimits, Team: noLimits, Org: noLimits}); err != nil {
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
	tc.Add(Scopes{KeyID: "key-current", OrgID: "org-current"}, 50)

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
	scopes := Scopes{KeyID: "key-no-team", OrgID: "org-no-team"}
	tc.Add(scopes, 300)

	// Team limits set to very low — but teamID is empty so they must be ignored.
	keyLimits := Limits{DailyTokenLimit: 1000}
	teamLimits := Limits{DailyTokenLimit: 1} // would block if team was checked
	orgLimits := Limits{DailyTokenLimit: 1000}

	if err := tc.CheckTokens(scopes, ScopeLimits{Key: keyLimits, Team: teamLimits, Org: orgLimits}); err != nil {
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
		goroutines = 100
		tokensEach = 10
		wantTotal  = goroutines * tokensEach
	)

	tc := NewTokenCounter()
	scopes := Scopes{KeyID: "concurrent-key", OrgID: "concurrent-org"}

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			tc.Add(scopes, tokensEach)
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
		scopes := Scopes{KeyID: fmt.Sprintf("race-key-%d", i%5), OrgID: "race-org"} // reuse 5 keys to create contention
		go func() {
			defer wg.Done()
			tc.Add(scopes, 10)
		}()
		go func() {
			defer wg.Done()
			if err := tc.CheckTokens(scopes, ScopeLimits{Key: keyLimits, Team: noLimits, Org: noLimits}); err != nil {
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

// ---- Seed ---------------------------------------------------------------------

// seedRow is one (key_id, team_id, org_id, user_id, total_tokens) tuple
// returned by a fakeUsageSeeder, matching the column order of
// db.QueryUsageSeed / ratelimit.UsageSeeder.
type seedRow struct {
	keyID, teamID, orgID, userID string
	tokens                       int64
}

// fakeUsageSeeder is an in-memory UsageSeeder for testing TokenCounter.Seed
// without a real database.
type fakeUsageSeeder struct {
	rows []seedRow
}

func (f *fakeUsageSeeder) QueryUsageSeed(_ context.Context, _ time.Time) (RowScanner, error) {
	return &fakeRowScanner{rows: f.rows, idx: -1}, nil
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
	return nil
}

func (f *fakeRowScanner) Close() error { return nil }
func (f *fakeRowScanner) Err() error   { return nil }

// TestTokenCounter_Seed_PopulatesUserScope verifies that Seed loads rows with
// a non-empty user_id into the org-bound user-scope counter, alongside the
// key, team, and org scopes, so that a freshly started process immediately
// enforces per-user limits against already-persisted usage.
func TestTokenCounter_Seed_PopulatesUserScope(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	seeder := &fakeUsageSeeder{rows: []seedRow{
		{keyID: "seed-key-1", teamID: "seed-team-1", orgID: "seed-org-1", userID: "seed-user-1", tokens: 300},
		{keyID: "seed-key-2", teamID: "seed-team-1", orgID: "seed-org-1", userID: "seed-user-1", tokens: 200},
	}}

	if err := tc.Seed(context.Background(), seeder); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	userLimits := Limits{DailyTokenLimit: 400}
	noLimits := Limits{}

	// Two keys owned by seed-user-1 contributed 300+200=500 tokens — over the
	// 400 limit — even though a fresh CheckTokens call for a third key by the
	// same user never called Add directly.
	err := tc.CheckTokens(Scopes{KeyID: "seed-key-3", UserID: "seed-user-1", OrgID: "seed-org-1"},
		ScopeLimits{Key: noLimits, User: userLimits, Team: noLimits, Org: noLimits})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("CheckTokens() after Seed = %v, want ErrTokenBudgetExceeded (seeded user total 500 >= limit 400)", err)
	}
}

// TestTokenCounter_Seed_UserScopeIsolatedPerOrg verifies that Seed keys the
// user-scope counter by org, matching Add/CheckTokens: two rows with the same
// user_id but different org_id must not share a budget.
func TestTokenCounter_Seed_UserScopeIsolatedPerOrg(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	seeder := &fakeUsageSeeder{rows: []seedRow{
		{keyID: "seed-iso-key-a", orgID: "seed-iso-org-a", userID: "seed-iso-user", tokens: 500},
		{keyID: "seed-iso-key-b", orgID: "seed-iso-org-b", userID: "seed-iso-user", tokens: 50},
	}}

	if err := tc.Seed(context.Background(), seeder); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	userLimits := Limits{DailyTokenLimit: 400}
	noLimits := Limits{}

	// Org A: seeded 500 >= limit 400 → exceeded.
	err := tc.CheckTokens(Scopes{KeyID: "seed-iso-key-a", UserID: "seed-iso-user", OrgID: "seed-iso-org-a"},
		ScopeLimits{Key: noLimits, User: userLimits, Org: noLimits})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("org A CheckTokens() = %v, want ErrTokenBudgetExceeded", err)
	}

	// Org B: seeded only 50 < limit 400 → fine, must not inherit org A's total.
	err = tc.CheckTokens(Scopes{KeyID: "seed-iso-key-b", UserID: "seed-iso-user", OrgID: "seed-iso-org-b"},
		ScopeLimits{Key: noLimits, User: userLimits, Org: noLimits})
	if err != nil {
		t.Errorf("org B CheckTokens() = %v, want nil (must not share org A's seeded user budget)", err)
	}
}

// TestTokenCounter_Seed_EmptyUserID_NoUserEntry verifies that Seed does not
// create a user-scoped counter for rows with an empty user_id (team_key and
// sa_key usage events have no owning user).
func TestTokenCounter_Seed_EmptyUserID_NoUserEntry(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	seeder := &fakeUsageSeeder{rows: []seedRow{
		{keyID: "seed-noUser-key", teamID: "seed-noUser-team", orgID: "seed-noUser-org", userID: "", tokens: 900},
	}}

	if err := tc.Seed(context.Background(), seeder); err != nil {
		t.Fatalf("Seed() error = %v", err)
	}

	found := false
	tc.dailyCounters.Range(func(k, _ any) bool {
		if key, ok := k.(string); ok && len(key) >= 5 && key[:5] == "user:" {
			found = true
			return false
		}
		return true
	})
	if found {
		t.Error("user-scoped entry was created for a seed row with empty user_id")
	}

	// Key and team and org scopes must still be seeded normally.
	teamLimits := Limits{DailyTokenLimit: 800}
	noLimits := Limits{}
	err := tc.CheckTokens(Scopes{KeyID: "seed-noUser-key", TeamID: "seed-noUser-team", OrgID: "seed-noUser-org"},
		ScopeLimits{Key: noLimits, Team: teamLimits, Org: noLimits})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("CheckTokens() = %v, want ErrTokenBudgetExceeded (team seeded 900 >= limit 800)", err)
	}
}

// TestTokenCounter_Seed_QueryError propagates a query error from the seeder.
func TestTokenCounter_Seed_QueryError(t *testing.T) {
	t.Parallel()

	tc := NewTokenCounter()
	wantErr := errors.New("boom")
	err := tc.Seed(context.Background(), &erroringSeeder{err: wantErr})
	if err == nil {
		t.Fatal("Seed() error = nil, want non-nil")
	}
	if !errors.Is(err, wantErr) {
		t.Errorf("Seed() error = %v, want it to wrap %v", err, wantErr)
	}
}

// erroringSeeder always fails QueryUsageSeed.
type erroringSeeder struct{ err error }

func (e *erroringSeeder) QueryUsageSeed(_ context.Context, _ time.Time) (RowScanner, error) {
	return nil, e.err
}

// BenchmarkTokenCounter_Add benchmarks the hot-path token addition.
func BenchmarkTokenCounter_Add(b *testing.B) {
	tc := NewTokenCounter()
	scopes := Scopes{KeyID: "bench-key", TeamID: "bench-team", OrgID: "bench-org"}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			tc.Add(scopes, 100)
		}
	})
}

// BenchmarkTokenCounter_CheckTokens benchmarks token budget checking.
func BenchmarkTokenCounter_CheckTokens(b *testing.B) {
	tc := NewTokenCounter()
	scopes := Scopes{KeyID: "bench-key", TeamID: "bench-team", OrgID: "bench-org"}
	tc.Add(scopes, 100)
	keyLimits := Limits{DailyTokenLimit: 1_000_000, MonthlyTokenLimit: 10_000_000}
	noLimits := Limits{}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if err := tc.CheckTokens(scopes, ScopeLimits{Key: keyLimits, Team: noLimits, Org: noLimits}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
