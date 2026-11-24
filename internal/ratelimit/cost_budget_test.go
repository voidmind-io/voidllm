package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"
)

// costTestNow is the instant the cost budget tests run at. It equals
// redisTestNow, so both backends see the same windows.
var costTestNow = redisTestNow

var (
	costUntilMidnight = 12 * time.Hour
	costUntilMonth    = time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).Sub(costTestNow)
)

// costBackends runs fn once against a MemoryBudgetCounter and once against a
// RedisBudgetCounter on miniredis, both with the clock at costTestNow.
func costBackends(t *testing.T, fn func(t *testing.T, c BudgetCounter)) {
	t.Helper()
	t.Run("memory", func(t *testing.T) {
		m := NewMemoryBudgetCounter()
		m.now = newFakeClock(costTestNow).Now
		fn(t, m)
	})
	t.Run("redis", func(t *testing.T) {
		env := newRedisBudgetEnv(t)
		c, _ := env.counter(t, &fakeUsageSeeder{})
		fn(t, c)
	})
}

// addCost records cost (micro-units) without tokens for s.
func addCost(c BudgetCounter, s Scopes, micros int64) {
	c.Add(context.Background(), s, Usage{CostMicros: micros})
}

// limitErrorOf returns the *LimitError in err's chain.
func limitErrorOf(t *testing.T, err error) *LimitError {
	t.Helper()
	var le *LimitError
	if !errors.As(err, &le) {
		t.Fatalf("err = %v, want a *LimitError", err)
	}
	return le
}

// TestCostBudget_ExceedPerScopeAndWindow verifies, for both backends, that a
// used-up daily or monthly cost budget of each of the four scopes rejects with
// ErrCostBudgetExceeded and describes that budget, and that usage one
// micro-unit below the limit still passes.
func TestCostBudget_ExceedPerScopeAndWindow(t *testing.T) {
	s := Scopes{KeyID: "k", UserID: "u", TeamID: "tm", OrgID: "o"}
	scopes := []struct {
		kind string
		with func(Limits) ScopeLimits
	}{
		{"key", func(l Limits) ScopeLimits { return ScopeLimits{Key: l} }},
		{"user", func(l Limits) ScopeLimits { return ScopeLimits{User: l} }},
		{"team", func(l Limits) ScopeLimits { return ScopeLimits{Team: l} }},
		{"org", func(l Limits) ScopeLimits { return ScopeLimits{Org: l} }},
	}
	windows := []struct {
		name  string
		limit func(int64) Limits
		reset time.Duration
	}{
		{"daily", func(n int64) Limits { return Limits{DailyCostLimit: n} }, costUntilMidnight},
		{"monthly", func(n int64) Limits { return Limits{MonthlyCostLimit: n} }, costUntilMonth},
	}

	costBackends(t, func(t *testing.T, c BudgetCounter) {
		addCost(c, s, 2_500_000)
		for _, sc := range scopes {
			for _, w := range windows {
				t.Run(sc.kind+"/"+w.name, func(t *testing.T) {
					if err := checkTokens(c, s, sc.with(w.limit(2_500_001))); err != nil {
						t.Errorf("Check() one micro-unit below the limit = %v, want nil", err)
					}
					err := checkTokens(c, s, sc.with(w.limit(2_500_000)))
					if !errors.Is(err, ErrCostBudgetExceeded) {
						t.Fatalf("Check() = %v, want ErrCostBudgetExceeded", err)
					}
					if errors.Is(err, ErrTokenBudgetExceeded) {
						t.Errorf("Check() = %v, must not be ErrTokenBudgetExceeded", err)
					}
					le := limitErrorOf(t, err)
					if le.RetryMetric != "cost" || le.RetryScope != sc.kind || le.RetryWindow != w.name {
						t.Errorf("LimitError = %+v, want metric cost, scope %s, window %s", le, sc.kind, w.name)
					}
					if le.RetryAfter != w.reset {
						t.Errorf("RetryAfter = %v, want %v", le.RetryAfter, w.reset)
					}
				})
			}
		}
	})
}

// TestCostBudget_AbsentScopesAreSkipped verifies that cost limits of the user
// and team scope do not apply to a key that has no user or team.
func TestCostBudget_AbsentScopesAreSkipped(t *testing.T) {
	costBackends(t, func(t *testing.T, c BudgetCounter) {
		addCost(c, Scopes{KeyID: "k", UserID: "u", TeamID: "tm", OrgID: "o"}, 100)
		limits := ScopeLimits{User: Limits{DailyCostLimit: 1}, Team: Limits{MonthlyCostLimit: 1}}
		if err := checkTokens(c, Scopes{KeyID: "k", OrgID: "o"}, limits); err != nil {
			t.Errorf("Check() without user and team = %v, want nil", err)
		}
	})
}

// TestCostBudget_LatestResetAndTokenPrecedence verifies the reset reported
// when several budgets are exceeded and which sentinel the error wraps.
func TestCostBudget_LatestResetAndTokenPrecedence(t *testing.T) {
	s := Scopes{KeyID: "k", OrgID: "o"}
	tests := []struct {
		name       string
		limits     ScopeLimits
		wantErr    error
		wantReset  time.Duration
		wantMetric string
		wantScope  string
		wantWindow string
	}{
		{
			name:       "daily and monthly cost: latest reset",
			limits:     ScopeLimits{Key: Limits{DailyCostLimit: 500}, Org: Limits{MonthlyCostLimit: 500}},
			wantErr:    ErrCostBudgetExceeded,
			wantReset:  costUntilMonth,
			wantMetric: "cost", wantScope: "org", wantWindow: "monthly",
		},
		{
			name:       "monthly cost before daily cost in check order",
			limits:     ScopeLimits{Key: Limits{MonthlyCostLimit: 500}, Org: Limits{DailyCostLimit: 500}},
			wantErr:    ErrCostBudgetExceeded,
			wantReset:  costUntilMonth,
			wantMetric: "cost", wantScope: "key", wantWindow: "monthly",
		},
		{
			name:       "tokens and cost in the same window: token error describes the token budget",
			limits:     ScopeLimits{Key: Limits{DailyCostLimit: 500}, Org: Limits{DailyTokenLimit: 100}},
			wantErr:    ErrTokenBudgetExceeded,
			wantReset:  costUntilMidnight,
			wantMetric: "tokens", wantScope: "org", wantWindow: "daily",
		},
		{
			name:       "daily tokens and monthly cost: token error, blocked until the cost budget resets",
			limits:     ScopeLimits{Key: Limits{DailyTokenLimit: 100, MonthlyCostLimit: 500}},
			wantErr:    ErrTokenBudgetExceeded,
			wantReset:  costUntilMonth,
			wantMetric: "cost", wantScope: "key", wantWindow: "monthly",
		},
		{
			name:       "monthly tokens and daily cost: token error with the token reset",
			limits:     ScopeLimits{Key: Limits{DailyCostLimit: 500}, Org: Limits{MonthlyTokenLimit: 100}},
			wantErr:    ErrTokenBudgetExceeded,
			wantReset:  costUntilMonth,
			wantMetric: "tokens", wantScope: "org", wantWindow: "monthly",
		},
		{
			name:       "cost exceeded, tokens set but not exceeded: cost error",
			limits:     ScopeLimits{Key: Limits{DailyTokenLimit: 101, MonthlyTokenLimit: 101, DailyCostLimit: 500}},
			wantErr:    ErrCostBudgetExceeded,
			wantReset:  costUntilMidnight,
			wantMetric: "cost", wantScope: "key", wantWindow: "daily",
		},
		{
			name:       "tokens exceeded, cost set but not exceeded: token error",
			limits:     ScopeLimits{Key: Limits{DailyTokenLimit: 100, DailyCostLimit: 501, MonthlyCostLimit: 501}},
			wantErr:    ErrTokenBudgetExceeded,
			wantReset:  costUntilMidnight,
			wantMetric: "tokens", wantScope: "key", wantWindow: "daily",
		},
	}

	costBackends(t, func(t *testing.T, c BudgetCounter) {
		c.Add(context.Background(), s, Usage{Tokens: 100, CostMicros: 500})
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				err := checkTokens(c, s, tt.limits)
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("Check() = %v, want %v", err, tt.wantErr)
				}
				other := ErrCostBudgetExceeded
				if errors.Is(tt.wantErr, ErrCostBudgetExceeded) {
					other = ErrTokenBudgetExceeded
				}
				if errors.Is(err, other) {
					t.Errorf("Check() = %v, must not also be %v", err, other)
				}
				le := limitErrorOf(t, err)
				if le.RetryAfter != tt.wantReset {
					t.Errorf("RetryAfter = %v, want %v", le.RetryAfter, tt.wantReset)
				}
				if le.RetryMetric != tt.wantMetric || le.RetryScope != tt.wantScope || le.RetryWindow != tt.wantWindow {
					t.Errorf("LimitError = %+v, want metric %s, scope %s, window %s",
						le, tt.wantMetric, tt.wantScope, tt.wantWindow)
				}
			})
		}
	})
}

// TestCostBudget_WithoutCostLimits verifies the unlicensed path: with the cost
// limits removed no cost budget rejects, token budgets still do, and cost
// keeps being counted, so that the full limits reject at once when they are
// applied again.
func TestCostBudget_WithoutCostLimits(t *testing.T) {
	s := Scopes{KeyID: "k", UserID: "u", TeamID: "tm", OrgID: "o"}
	cost := Limits{DailyCostLimit: 1_000, MonthlyCostLimit: 1_000}
	limits := ScopeLimits{Key: cost, User: cost, Team: cost, Org: cost}

	costBackends(t, func(t *testing.T, c BudgetCounter) {
		c.Add(context.Background(), s, Usage{Tokens: 40, CostMicros: 900})
		if err := checkTokens(c, s, limits.WithoutCostLimits()); err != nil {
			t.Fatalf("Check() without cost limits = %v, want nil", err)
		}
		// Usage beyond the limit while nothing is enforced.
		c.Add(context.Background(), s, Usage{Tokens: 40, CostMicros: 900})
		if err := checkTokens(c, s, limits.WithoutCostLimits()); err != nil {
			t.Fatalf("Check() without cost limits over the budget = %v, want nil", err)
		}

		snap, err := c.Snapshot(context.Background(), []string{KeyScope("k"), UserScope("o", "u"), TeamScope("tm"), OrgScope("o")})
		if err != nil {
			t.Fatalf("Snapshot() error = %v", err)
		}
		want := Usage{Tokens: 80, CostMicros: 1_800}
		for scope, u := range snap {
			if u.Daily != want || u.Monthly != want {
				t.Errorf("Snapshot()[%q] = %+v, want daily and monthly %+v (counting continues)", scope, u, want)
			}
		}

		if err := checkTokens(c, s, limits); !errors.Is(err, ErrCostBudgetExceeded) {
			t.Errorf("Check() with cost limits = %v, want ErrCostBudgetExceeded", err)
		}

		withTokens := limits
		withTokens.Org.DailyTokenLimit = 80
		if err := checkTokens(c, s, withTokens.WithoutCostLimits()); !errors.Is(err, ErrTokenBudgetExceeded) {
			t.Errorf("Check() without cost limits, tokens used up = %v, want ErrTokenBudgetExceeded", err)
		}
	})
}

// TestScopeLimits_WithoutCostLimits verifies that only the cost limits are
// removed and the receiver is left untouched.
func TestScopeLimits_WithoutCostLimits(t *testing.T) {
	t.Parallel()

	full := Limits{
		RequestsPerMinute: 1, RequestsPerDay: 2,
		DailyTokenLimit: 3, MonthlyTokenLimit: 4,
		DailyCostLimit: 5, MonthlyCostLimit: 6,
	}
	in := ScopeLimits{Key: full, User: full, Team: full, Org: full}
	stripped := Limits{RequestsPerMinute: 1, RequestsPerDay: 2, DailyTokenLimit: 3, MonthlyTokenLimit: 4}

	got := in.WithoutCostLimits()
	if want := (ScopeLimits{Key: stripped, User: stripped, Team: stripped, Org: stripped}); got != want {
		t.Errorf("WithoutCostLimits() = %+v, want %+v", got, want)
	}
	if want := (ScopeLimits{Key: full, User: full, Team: full, Org: full}); in != want {
		t.Errorf("receiver changed to %+v", in)
	}
}

// TestCostBudget_ZeroCostUsageNeverBlocks verifies that usage without cost
// (a model without prices) never uses up a cost budget, however many tokens
// it consumes.
func TestCostBudget_ZeroCostUsageNeverBlocks(t *testing.T) {
	s := Scopes{KeyID: "k", UserID: "u", TeamID: "tm", OrgID: "o"}
	tiny := Limits{DailyCostLimit: 1, MonthlyCostLimit: 1}
	limits := ScopeLimits{Key: tiny, User: tiny, Team: tiny, Org: tiny}

	costBackends(t, func(t *testing.T, c BudgetCounter) {
		if err := checkTokens(c, s, limits); err != nil {
			t.Fatalf("Check() before any usage = %v, want nil", err)
		}
		for range 20 {
			addTokens(c, s, 1_000_000)
		}
		if err := checkTokens(c, s, limits); err != nil {
			t.Errorf("Check() after usage without cost = %v, want nil", err)
		}
	})
}

// TestCostBudget_WindowReset verifies that a daily cost budget frees up at
// the next UTC midnight while the monthly one stays used up.
func TestCostBudget_WindowReset(t *testing.T) {
	t.Parallel()

	clock := newFakeClock(costTestNow)
	c := NewMemoryBudgetCounter()
	c.now = clock.Now
	s := Scopes{KeyID: "k", OrgID: "o"}
	addCost(c, s, 700)

	daily := ScopeLimits{Key: Limits{DailyCostLimit: 700}}
	monthly := ScopeLimits{Key: Limits{MonthlyCostLimit: 700}}
	if err := checkTokens(c, s, daily); !errors.Is(err, ErrCostBudgetExceeded) {
		t.Fatalf("daily Check() = %v, want ErrCostBudgetExceeded", err)
	}

	clock.Add(costUntilMidnight)
	if err := checkTokens(c, s, daily); err != nil {
		t.Errorf("daily Check() after midnight = %v, want nil", err)
	}
	if err := checkTokens(c, s, monthly); !errors.Is(err, ErrCostBudgetExceeded) {
		t.Errorf("monthly Check() after midnight = %v, want ErrCostBudgetExceeded", err)
	}

	clock.Set(time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC))
	if err := checkTokens(c, s, monthly); err != nil {
		t.Errorf("monthly Check() in the next month = %v, want nil", err)
	}
}

// TestCostBudget_SeededFromPersistedUsage verifies that cost persisted before
// the process started counts towards the cost budgets of both backends.
func TestCostBudget_SeededFromPersistedUsage(t *testing.T) {
	seeder := func() *fakeUsageSeeder {
		return &fakeUsageSeeder{rows: []seedRow{
			{keyID: "k", orgID: "o", tokens: 10, cost: 0.75},
			{keyID: "k", orgID: "o", tokens: 10, cost: 0.50, earlierThisMonth: true},
		}}
	}
	s := Scopes{KeyID: "k", OrgID: "o"}
	check := func(t *testing.T, c BudgetCounter) {
		t.Helper()
		if err := checkTokens(c, s, ScopeLimits{Org: Limits{DailyCostLimit: 750_001}}); err != nil {
			t.Errorf("daily Check() = %v, want nil (0.75 seeded today)", err)
		}
		if err := checkTokens(c, s, ScopeLimits{Org: Limits{DailyCostLimit: 750_000}}); !errors.Is(err, ErrCostBudgetExceeded) {
			t.Errorf("daily Check() = %v, want ErrCostBudgetExceeded", err)
		}
		if err := checkTokens(c, s, ScopeLimits{Key: Limits{MonthlyCostLimit: 1_250_001}}); err != nil {
			t.Errorf("monthly Check() = %v, want nil (1.25 seeded this month)", err)
		}
		if err := checkTokens(c, s, ScopeLimits{Key: Limits{MonthlyCostLimit: 1_250_000}}); !errors.Is(err, ErrCostBudgetExceeded) {
			t.Errorf("monthly Check() = %v, want ErrCostBudgetExceeded", err)
		}
	}

	t.Run("memory", func(t *testing.T) {
		m := NewMemoryBudgetCounter()
		m.now = newFakeClock(costTestNow).Now
		if err := m.Seed(context.Background(), seeder()); err != nil {
			t.Fatalf("Seed() error = %v", err)
		}
		check(t, m)
	})
	t.Run("redis", func(t *testing.T) {
		env := newRedisBudgetEnv(t)
		c, _ := env.counter(t, seeder())
		check(t, c)
	})
}

// TestRedisBudgetCounter_TwoInstancesShareOneCostBudget verifies that cost
// recorded on two instances adds up in one budget that both enforce, although
// neither has recorded enough on its own.
func TestRedisBudgetCounter_TwoInstancesShareOneCostBudget(t *testing.T) {
	env := newRedisBudgetEnv(t)
	a, _ := env.counter(t, &fakeUsageSeeder{})
	b, _ := env.counter(t, &fakeUsageSeeder{})

	addCost(a, Scopes{KeyID: "key-a", TeamID: "team-1", OrgID: "org-1"}, 6_000)
	addCost(b, Scopes{KeyID: "key-b", TeamID: "team-1", OrgID: "org-1"}, 5_000)

	tight := ScopeLimits{Org: Limits{DailyCostLimit: 10_000}}
	for name, c := range map[string]*RedisBudgetCounter{"a": a, "b": b} {
		if err := checkTokens(c, Scopes{KeyID: "key-" + name, TeamID: "team-1", OrgID: "org-1"}, tight); err != nil {
			t.Fatalf("instance %s before the totals are shared: Check() = %v, want nil", name, err)
		}
	}

	flushNow(a)
	flushNow(b)
	if got := env.sum(t, env.key("cost", redisTestDay, "org:org-1")); got != 11_000 {
		t.Fatalf("shared org cost = %d, want 11000", got)
	}

	roomy := ScopeLimits{Org: Limits{DailyCostLimit: 11_001}, Team: Limits{MonthlyCostLimit: 11_001}}
	for name, c := range map[string]*RedisBudgetCounter{"a": a, "b": b} {
		s := Scopes{KeyID: "key-" + name, TeamID: "team-1", OrgID: "org-1"}
		err := checkTokens(c, s, tight)
		if !errors.Is(err, ErrCostBudgetExceeded) {
			t.Errorf("instance %s: Check() = %v, want ErrCostBudgetExceeded (shared 11000 >= 10000)", name, err)
			continue
		}
		if le := limitErrorOf(t, err); le.RetryMetric != "cost" || le.RetryScope != "org" || le.RetryWindow != "daily" || le.RetryAfter != costUntilMidnight {
			t.Errorf("instance %s: LimitError = %+v, want cost/org/daily, RetryAfter %v", name, le, costUntilMidnight)
		}
		if err := checkTokens(c, s, roomy); err != nil {
			t.Errorf("instance %s: Check() = %v, want nil (11000 < 11001)", name, err)
		}
		if err := checkTokens(c, s, ScopeLimits{Team: Limits{MonthlyCostLimit: 11_000}}); !errors.Is(err, ErrCostBudgetExceeded) {
			t.Errorf("instance %s: team monthly Check() = %v, want ErrCostBudgetExceeded", name, err)
		}
		// The key budgets are not shared: each key spent less than 11000.
		if err := checkTokens(c, s, ScopeLimits{Key: Limits{DailyCostLimit: 6_001}}); err != nil {
			t.Errorf("instance %s: key Check() = %v, want nil", name, err)
		}
	}

	// A token budget of the same size is measured in tokens, not micro-units:
	// no tokens were recorded, so it is free.
	if err := checkTokens(a, Scopes{KeyID: "key-a", OrgID: "org-1"}, ScopeLimits{Org: Limits{DailyTokenLimit: 1}}); err != nil {
		t.Errorf("token Check() = %v, want nil (cost must not count as tokens)", err)
	}
}

// TestRedisBudgetCounter_CostCheckIsOneRoundTrip verifies that a check with
// all sixteen budgets (token and cost) is still a single script call, and
// that cost limits alone are enough to make it.
func TestRedisBudgetCounter_CostCheckIsOneRoundTrip(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.counter(t, &fakeUsageSeeder{})

	s := Scopes{KeyID: "k", UserID: "u", TeamID: "tm", OrgID: "o"}
	all := Limits{
		DailyTokenLimit: 1_000_000, MonthlyTokenLimit: 1_000_000,
		DailyCostLimit: 1_000_000, MonthlyCostLimit: 1_000_000,
	}
	allLimits := ScopeLimits{Key: all, User: all, Team: all, Org: all}

	// The first check loads the sum script.
	if err := checkTokens(c, s, allLimits); err != nil {
		t.Fatalf("warm-up Check() = %v, want nil", err)
	}

	// miniredis counts the script call plus every HVALS the script runs.
	before := env.mr.CommandCount()
	if err := checkTokens(c, s, allLimits); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
	if got := env.mr.CommandCount() - before; got != 17 {
		t.Errorf("redis saw %d commands for one check with sixteen budgets, want 17 (1 script call + 16 server-side reads)", got)
	}

	before = env.mr.CommandCount()
	if err := checkTokens(c, s, ScopeLimits{Org: Limits{MonthlyCostLimit: 1_000_000}}); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
	if got := env.mr.CommandCount() - before; got != 2 {
		t.Errorf("redis saw %d commands for one check with one cost budget, want 2", got)
	}

	// Without cost limits the cost counters are not read.
	before = env.mr.CommandCount()
	if err := checkTokens(c, s, allLimits.WithoutCostLimits()); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
	if got := env.mr.CommandCount() - before; got != 9 {
		t.Errorf("redis saw %d commands for one check with eight token budgets, want 9", got)
	}
}

// TestRedisBudgetCounter_CostOutageFallsBackToShadow verifies that cost
// budgets are decided on the local counters while Redis is down: neither
// fail-open nor fail-closed.
func TestRedisBudgetCounter_CostOutageFallsBackToShadow(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.counter(t, &fakeUsageSeeder{})

	s := Scopes{KeyID: "k", OrgID: "o"}
	limits := ScopeLimits{Org: Limits{DailyCostLimit: 100}}
	addCost(c, s, 80)
	flushNow(c)
	if err := checkTokens(c, s, limits); err != nil {
		t.Fatalf("healthy Check() = %v, want nil", err)
	}

	env.mr.Close()
	if err := checkTokens(c, s, limits); err != nil {
		t.Fatalf("Check() during outage = %v, want nil (shadow 80 < 100)", err)
	}
	if !c.degraded.Load() {
		t.Fatal("counter not degraded after a failed check")
	}
	addCost(c, s, 30)
	if err := checkTokens(c, s, limits); !errors.Is(err, ErrCostBudgetExceeded) {
		t.Errorf("Check() during outage = %v, want ErrCostBudgetExceeded (shadow 110 >= 100)", err)
	}
}

// TestRedisBudgetCounter_CostFromRefreshedView verifies that cost another
// instance persisted, which is neither in Redis nor in this instance's
// shadow, counts towards the cost budget after a refresh.
func TestRedisBudgetCounter_CostFromRefreshedView(t *testing.T) {
	env := newRedisBudgetEnv(t)
	seeder := &fakeUsageSeeder{}
	c, _ := env.counter(t, seeder)

	s := Scopes{KeyID: "k", OrgID: "o"}
	limits := ScopeLimits{Org: Limits{MonthlyCostLimit: 2_000_000}}
	if err := checkTokens(c, s, limits); err != nil {
		t.Fatalf("Check() before the refresh = %v, want nil", err)
	}

	seeder.addEvents(persistedEvent{keyID: "other", orgID: "o", tokens: 5, cost: 2.5, at: costTestNow.Add(-time.Hour)})
	refreshNow(c)

	err := checkTokens(c, s, limits)
	if !errors.Is(err, ErrCostBudgetExceeded) {
		t.Fatalf("Check() after the refresh = %v, want ErrCostBudgetExceeded (2.5 persisted >= 2.0)", err)
	}
	if le := limitErrorOf(t, err); le.RetryMetric != "cost" || le.RetryScope != "org" || le.RetryWindow != "monthly" {
		t.Errorf("LimitError = %+v, want cost/org/monthly", le)
	}
}
