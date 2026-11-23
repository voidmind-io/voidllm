package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/voidmind-io/voidllm/internal/metrics"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
)

// The Redis budget tests do not run in parallel: they assert on deltas of the
// process-wide voidllm_budget_backend_errors_total metric.

// lockedLog is a concurrency-safe log sink that can count messages.
type lockedLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// count returns how many log lines contain sub.
func (l *lockedLog) count(sub string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), sub)
}

// redisBudgetEnv is one miniredis with a fixed clock shared by every counter
// created from it, standing in for several VoidLLM instances on one Redis.
type redisBudgetEnv struct {
	mr    *miniredis.Miniredis
	clock *fakeClock
}

// redisTestNow is the instant the Redis budget tests run at: mid-day and
// mid-month, far from any window boundary.
var redisTestNow = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

const (
	redisTestDay   = "d:2026-03-10"
	redisTestMonth = "m:2026-03"
)

func newRedisBudgetEnv(t *testing.T) *redisBudgetEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	mr.SetTime(redisTestNow)
	return &redisBudgetEnv{mr: mr, clock: newFakeClock(redisTestNow)}
}

// newCounter returns a counter on the shared Redis that has not been started.
func (e *redisBudgetEnv) newCounter(t *testing.T, seeder UsageSeeder) (*RedisBudgetCounter, *lockedLog) {
	t.Helper()
	client, err := voidredis.New("redis://"+e.mr.Addr(), "t")
	if err != nil {
		t.Fatalf("voidredis.New: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	logs := &lockedLog{}
	c := NewRedisBudgetCounter(client, seeder, slog.New(slog.NewTextHandler(logs, nil)))
	c.now = e.clock.Now
	c.shadow.now = e.clock.Now
	return c, logs
}

// counter returns a started counter on the shared Redis. It is stopped when
// the test ends.
func (e *redisBudgetEnv) counter(t *testing.T, seeder UsageSeeder) (*RedisBudgetCounter, *lockedLog) {
	t.Helper()
	c, logs := e.newCounter(t, seeder)
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c.Stop(ctx)
	})
	return c, logs
}

// key returns the Redis key of a budget counter.
func (e *redisBudgetEnv) key(metric, bucket, scope string) string {
	return "t:budget:" + metric + ":" + bucket + ":" + scope
}

// value returns the integer stored under key, 0 when it does not exist.
func (e *redisBudgetEnv) value(t *testing.T, key string) int64 {
	t.Helper()
	if !e.mr.Exists(key) {
		return 0
	}
	s, err := e.mr.Get(key)
	if err != nil {
		t.Fatalf("miniredis Get(%q): %v", key, err)
	}
	var n int64
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			t.Fatalf("miniredis Get(%q) = %q, want an integer", key, s)
		}
		n = n*10 + int64(ch-'0')
	}
	return n
}

// seeded reports whether both current buckets carry a finished seed marker.
func (e *redisBudgetEnv) seeded() bool {
	for _, bucket := range []string{redisTestDay, redisTestMonth} {
		if v, err := e.mr.Get("t:budget:seeded:" + bucket); err != nil || v != "done" {
			return false
		}
	}
	return true
}

// eventually polls cond until it holds or the deadline passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// settle waits until the Redis command count stops changing, i.e. until the
// workers have nothing left to do.
func (e *redisBudgetEnv) settle(t *testing.T) {
	t.Helper()
	last, stableSince := e.mr.CommandCount(), time.Now()
	eventually(t, "redis to go quiet", func() bool {
		if n := e.mr.CommandCount(); n != last {
			last, stableSince = n, time.Now()
			return false
		}
		return time.Since(stableSince) > 100*time.Millisecond
	})
}

func backendErrors(op string) float64 {
	return testutil.ToFloat64(metrics.BudgetBackendErrorsTotal.WithLabelValues(op))
}

// TestRedisBudgetCounter_TwoInstancesShareOneBudget verifies the point of the
// Redis counter: usage recorded on two instances adds up in one budget that
// both of them enforce, although neither has seen enough usage on its own.
func TestRedisBudgetCounter_TwoInstancesShareOneBudget(t *testing.T) {
	env := newRedisBudgetEnv(t)
	a, _ := env.counter(t, &fakeUsageSeeder{})
	b, _ := env.counter(t, &fakeUsageSeeder{})

	addTokens(a, Scopes{KeyID: "key-a", TeamID: "team-1", OrgID: "org-1"}, 60)
	addTokens(b, Scopes{KeyID: "key-b", TeamID: "team-1", OrgID: "org-1"}, 50)

	orgDay := env.key("tok", redisTestDay, "org:org-1")
	eventually(t, "both increments in redis", func() bool { return env.value(t, orgDay) == 110 })

	for _, key := range []string{
		env.key("tok", redisTestMonth, "org:org-1"),
		env.key("tok", redisTestDay, "team:team-1"),
		env.key("tok", redisTestMonth, "team:team-1"),
	} {
		if got := env.value(t, key); got != 110 {
			t.Errorf("%s = %d, want 110", key, got)
		}
	}
	if got := env.value(t, env.key("tok", redisTestDay, "key:key-a")); got != 60 {
		t.Errorf("key-a daily = %d, want 60", got)
	}
	if got := env.value(t, env.key("tok", redisTestDay, "key:key-b")); got != 50 {
		t.Errorf("key-b daily = %d, want 50", got)
	}

	tight := ScopeLimits{Org: Limits{DailyTokenLimit: 100}}
	roomy := ScopeLimits{Org: Limits{DailyTokenLimit: 111}, Team: Limits{MonthlyTokenLimit: 111}}
	for name, c := range map[string]*RedisBudgetCounter{"a": a, "b": b} {
		s := Scopes{KeyID: "key-" + name, TeamID: "team-1", OrgID: "org-1"}
		err := checkTokens(c, s, tight)
		if !errors.Is(err, ErrTokenBudgetExceeded) {
			t.Errorf("instance %s: Check() = %v, want ErrTokenBudgetExceeded (shared 110 >= 100)", name, err)
		}
		var le *LimitError
		if !errors.As(err, &le) || le.Scope != "org" || le.Window != "daily" || le.Metric != "tokens" {
			t.Errorf("instance %s: LimitError = %+v, want org/daily/tokens", name, le)
		}
		if d, _ := RetryAfterFromError(err); d != 12*time.Hour {
			t.Errorf("instance %s: RetryAfter = %v, want 12h", name, d)
		}
		if err := checkTokens(c, s, roomy); err != nil {
			t.Errorf("instance %s: Check() = %v, want nil (110 < 111)", name, err)
		}
	}

	// The team budget of a monthly window is shared the same way.
	err := checkTokens(a, Scopes{KeyID: "key-a", TeamID: "team-1", OrgID: "org-1"},
		ScopeLimits{Team: Limits{MonthlyTokenLimit: 110}})
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("team monthly Check() = %v, want ErrTokenBudgetExceeded", err)
	}
}

// TestRedisBudgetCounter_UserScope verifies that the org-bound user scope is
// counted in Redis and shared between instances and keys of that user.
func TestRedisBudgetCounter_UserScope(t *testing.T) {
	env := newRedisBudgetEnv(t)
	a, _ := env.counter(t, &fakeUsageSeeder{})
	b, _ := env.counter(t, &fakeUsageSeeder{})

	addTokens(a, Scopes{KeyID: "key-1", UserID: "user-1", OrgID: "org-1"}, 250)
	addTokens(b, Scopes{KeyID: "key-2", UserID: "user-1", OrgID: "org-1"}, 250)
	// Same user ID in another org: a separate budget.
	addTokens(b, Scopes{KeyID: "key-3", UserID: "user-1", OrgID: "org-2"}, 10)

	userDay := env.key("tok", redisTestDay, "user:org-1:user-1")
	eventually(t, "user counter in redis", func() bool { return env.value(t, userDay) == 500 })
	env.settle(t)
	if got := env.value(t, env.key("tok", redisTestMonth, "user:org-1:user-1")); got != 500 {
		t.Errorf("user monthly = %d, want 500", got)
	}
	if got := env.value(t, env.key("tok", redisTestDay, "user:org-2:user-1")); got != 10 {
		t.Errorf("other org's user daily = %d, want 10", got)
	}

	limits := ScopeLimits{User: Limits{DailyTokenLimit: 400}}
	err := checkTokens(a, Scopes{KeyID: "key-9", UserID: "user-1", OrgID: "org-1"}, limits)
	if !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() = %v, want ErrTokenBudgetExceeded (user 500 >= 400)", err)
	}
	var le *LimitError
	if !errors.As(err, &le) || le.Scope != "user" {
		t.Errorf("LimitError = %+v, want scope user", le)
	}
	if err := checkTokens(a, Scopes{KeyID: "key-3", UserID: "user-1", OrgID: "org-2"}, limits); err != nil {
		t.Errorf("other org Check() = %v, want nil", err)
	}
	// A key without a user is not subject to the user limit.
	if err := checkTokens(a, Scopes{KeyID: "key-1", OrgID: "org-1"}, limits); err != nil {
		t.Errorf("Check() without user = %v, want nil", err)
	}
}

// TestRedisBudgetCounter_KeysAndExpiry pins the key layout and the expiry of
// daily and monthly counters, and that zero amounts create no keys.
func TestRedisBudgetCounter_KeysAndExpiry(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.counter(t, &fakeUsageSeeder{})

	c.Add(context.Background(), Scopes{KeyID: "k", UserID: "u", TeamID: "tm", OrgID: "o"}, Usage{Tokens: 7})
	eventually(t, "increment in redis", func() bool {
		return env.value(t, env.key("tok", redisTestMonth, "org:o")) == 7
	})
	env.settle(t)

	want := map[string]bool{
		"t:budget:seeded:d:2026-03-10": true,
		"t:budget:seeded:m:2026-03":    true,
	}
	for _, scope := range []string{"key:k", "user:o:u", "team:tm", "org:o"} {
		want["t:budget:tok:d:2026-03-10:"+scope] = true
		want["t:budget:tok:m:2026-03:"+scope] = true
	}
	got := env.mr.Keys()
	if len(got) != len(want) {
		t.Errorf("redis keys = %v, want exactly %d keys (no cost keys for a zero cost)", got, len(want))
	}
	for _, k := range got {
		if !want[k] {
			t.Errorf("unexpected redis key %q", k)
		}
	}

	// Daily counters live until 24h after their day ended, monthly ones
	// until 48h after their month ended; the markers expire with them.
	wantDayTTL := time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC).Sub(redisTestNow)
	wantMonthTTL := time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC).Sub(redisTestNow)
	for key, wantTTL := range map[string]time.Duration{
		"t:budget:tok:d:2026-03-10:org:o": wantDayTTL,
		"t:budget:seeded:d:2026-03-10":    wantDayTTL,
		"t:budget:tok:m:2026-03:org:o":    wantMonthTTL,
		"t:budget:seeded:m:2026-03":       wantMonthTTL,
	} {
		if ttl := env.mr.TTL(key); ttl != wantTTL {
			t.Errorf("TTL(%s) = %v, want %v", key, ttl, wantTTL)
		}
	}

	// Cost gets its own counters once it is non-zero.
	c.Add(context.Background(), Scopes{KeyID: "k", OrgID: "o"}, Usage{CostMicros: 1500})
	eventually(t, "cost in redis", func() bool {
		return env.value(t, env.key("cost", redisTestMonth, "org:o")) == 1500
	})
	if got := env.value(t, env.key("cost", redisTestDay, "key:k")); got != 1500 {
		t.Errorf("daily key cost = %d, want 1500", got)
	}
	if got := env.value(t, env.key("tok", redisTestDay, "key:k")); got != 7 {
		t.Errorf("daily key tokens = %d, want 7 (a cost-only add must not touch tokens)", got)
	}
}

// TestRedisBudgetCounter_NoRedisCallsWithoutLimits verifies that a request
// without budgets never reaches Redis, that a request with budgets costs
// exactly one round trip, and that zero usage is not queued.
func TestRedisBudgetCounter_NoRedisCallsWithoutLimits(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.counter(t, &fakeUsageSeeder{})
	eventually(t, "startup rehydration", env.seeded)
	env.settle(t)

	s := Scopes{KeyID: "k", UserID: "u", TeamID: "tm", OrgID: "o"}
	before := env.mr.CommandCount()

	for range 50 {
		if err := checkTokens(c, s, ScopeLimits{}); err != nil {
			t.Fatalf("Check() without limits = %v, want nil", err)
		}
		// Limits on scopes the key does not have do not count either.
		if err := checkTokens(c, Scopes{KeyID: "k", OrgID: "o"}, ScopeLimits{
			User: Limits{DailyTokenLimit: 1}, Team: Limits{MonthlyTokenLimit: 1},
		}); err != nil {
			t.Fatalf("Check() with limits on absent scopes = %v, want nil", err)
		}
		c.Add(context.Background(), s, Usage{})
	}
	env.settle(t)
	if got := env.mr.CommandCount(); got != before {
		t.Errorf("redis saw %d commands for requests without budgets, want 0", got-before)
	}

	// All eight budgets set: still a single MGET.
	all := Limits{DailyTokenLimit: 10, MonthlyTokenLimit: 10}
	if err := checkTokens(c, s, ScopeLimits{Key: all, User: all, Team: all, Org: all}); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
	if got := env.mr.CommandCount(); got != before+1 {
		t.Errorf("redis saw %d commands for one check with budgets, want 1", got-before)
	}
}

// TestRedisBudgetCounter_OutageFallsBackToShadow verifies the degraded mode:
// with Redis down checks are decided on the local shadow counter (neither
// open nor closed), the transition is logged once in each direction, and the
// error metric counts the failures.
func TestRedisBudgetCounter_OutageFallsBackToShadow(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, logs := env.counter(t, &fakeUsageSeeder{})

	s := Scopes{KeyID: "k", OrgID: "o"}
	limits := ScopeLimits{Org: Limits{DailyTokenLimit: 100}}
	orgDay := env.key("tok", redisTestDay, "org:o")

	addTokens(c, s, 80)
	eventually(t, "increment in redis", func() bool { return env.value(t, orgDay) == 80 })
	if err := checkTokens(c, s, limits); err != nil {
		t.Fatalf("healthy Check() = %v, want nil", err)
	}
	if n := logs.count("budget backend degraded"); n != 0 {
		t.Fatalf("degraded logged %d times while healthy", n)
	}

	checkErrsBefore, flushErrsBefore := backendErrors("check"), backendErrors("flush")
	env.mr.Close()

	// Fail-open would allow everything, fail-closed would reject this.
	for range 5 {
		if err := checkTokens(c, s, limits); err != nil {
			t.Fatalf("Check() during outage = %v, want nil (shadow 80 < 100)", err)
		}
	}
	if !c.degraded.Load() {
		t.Fatal("counter not degraded after failed checks")
	}
	if got := backendErrors("check") - checkErrsBefore; got != 1 {
		t.Errorf("check errors = %v, want 1 (later checks must not probe within the probe interval)", got)
	}

	// Usage recorded during the outage still counts locally.
	addTokens(c, s, 30)
	eventually(t, "failed flush", func() bool { return backendErrors("flush") > flushErrsBefore })
	if err := checkTokens(c, s, limits); !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() during outage = %v, want ErrTokenBudgetExceeded (shadow 110 >= 100)", err)
	}
	if n := logs.count("budget backend degraded"); n != 1 {
		t.Errorf("degraded logged %d times, want once", n)
	}
	if n := logs.count("budget backend recovered"); n != 0 {
		t.Errorf("recovered logged %d times during the outage", n)
	}

	if err := env.mr.Restart(); err != nil {
		t.Fatalf("miniredis Restart: %v", err)
	}
	env.mr.SetTime(redisTestNow)
	eventually(t, "recovery", func() bool {
		env.clock.Add(2 * budgetProbeInterval)
		_ = checkTokens(c, s, limits)
		return !c.degraded.Load()
	})
	if n := logs.count("budget backend recovered"); n != 1 {
		t.Errorf("recovered logged %d times, want once", n)
	}
	if n := logs.count("budget backend degraded"); n != 1 {
		t.Errorf("degraded logged %d times after recovery, want once", n)
	}

	// Back on shared counters: the increment lost during the outage is
	// missing in Redis (80), but this instance still enforces its own view.
	if err := checkTokens(c, s, limits); !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() after recovery = %v, want ErrTokenBudgetExceeded (shadow 110)", err)
	}
	addTokens(c, s, 5)
	eventually(t, "increments flow again", func() bool { return env.value(t, orgDay) == 85 })
}

// TestRedisBudgetCounter_RehydratesOncePerBucket verifies that of several
// instances exactly one adds the persisted usage to a bucket, that re-checks
// do not add it again, and that a flushed Redis is rehydrated once more.
func TestRedisBudgetCounter_RehydratesOncePerBucket(t *testing.T) {
	env := newRedisBudgetEnv(t)
	seeder := &fakeUsageSeeder{rows: []seedRow{
		{keyID: "k1", teamID: "tm", orgID: "o", userID: "u", tokens: 300, cost: 0.0123},
		{keyID: "k2", orgID: "o", tokens: 200},
	}}
	a, _ := env.counter(t, seeder)
	b, _ := env.counter(t, seeder)
	eventually(t, "rehydration", env.seeded)
	env.settle(t)

	assertSeeded := func(stage string) {
		t.Helper()
		for key, want := range map[string]int64{
			env.key("tok", redisTestDay, "org:o"):        500,
			env.key("tok", redisTestMonth, "org:o"):      500,
			env.key("tok", redisTestDay, "key:k1"):       300,
			env.key("tok", redisTestMonth, "key:k2"):     200,
			env.key("tok", redisTestDay, "team:tm"):      300,
			env.key("tok", redisTestMonth, "user:o:u"):   300,
			env.key("cost", redisTestDay, "org:o"):       12300,
			env.key("cost", redisTestMonth, "key:k1"):    12300,
			env.key("cost", redisTestMonth, "key:k2"):    0,
			env.key("cost", redisTestMonth, "user:o:u"):  12300,
			env.key("tok", redisTestMonth, "user:o:k2"):  0,
			env.key("tok", redisTestMonth, "team:other"): 0,
		} {
			if got := env.value(t, key); got != want {
				t.Errorf("%s: %s = %d, want %d", stage, key, got, want)
			}
		}
	}
	assertSeeded("after start")

	// Each instance reads the database twice for its shadow counter; the
	// bucket rehydration adds one read per bucket in total, not per instance.
	if got := seeder.queries(); got != 6 {
		t.Errorf("seed queries = %d, want 6 (2 per shadow + 1 per bucket)", got)
	}

	// A fresh instance enforces the persisted usage right away.
	limits := ScopeLimits{Org: Limits{MonthlyTokenLimit: 500}}
	for name, c := range map[string]*RedisBudgetCounter{"a": a, "b": b} {
		if err := checkTokens(c, Scopes{KeyID: "k9", OrgID: "o"}, limits); !errors.Is(err, ErrTokenBudgetExceeded) {
			t.Errorf("instance %s: Check() = %v, want ErrTokenBudgetExceeded", name, err)
		}
	}

	// Periodic re-checks find the markers and leave the counters alone.
	a.EvictStale()
	b.EvictStale()
	env.settle(t)
	assertSeeded("after re-check")
	if got := seeder.queries(); got != 6 {
		t.Errorf("seed queries after re-check = %d, want 6", got)
	}

	// Redis loses everything: the next re-check rehydrates, once.
	env.mr.FlushAll()
	a.EvictStale()
	eventually(t, "rehydration after flush", env.seeded)
	b.EvictStale()
	env.settle(t)
	assertSeeded("after flush")
	if got := seeder.queries(); got != 8 {
		t.Errorf("seed queries after flush = %d, want 8", got)
	}
}

// TestRedisBudgetCounter_RehydratesAfterAbandonedClaim verifies that a claim
// left behind by an instance that died mid-rehydration only delays the
// bucket until the claim expires.
func TestRedisBudgetCounter_RehydratesAfterAbandonedClaim(t *testing.T) {
	env := newRedisBudgetEnv(t)
	for _, bucket := range []string{redisTestDay, redisTestMonth} {
		marker := "t:budget:seeded:" + bucket
		if err := env.mr.Set(marker, "pending:dead-instance"); err != nil {
			t.Fatalf("miniredis Set: %v", err)
		}
		env.mr.SetTTL(marker, budgetSeedClaimTTL)
	}

	seeder := &fakeUsageSeeder{rows: []seedRow{{keyID: "k", orgID: "o", tokens: 400}}}
	c, _ := env.counter(t, seeder)
	env.settle(t)

	orgMonth := env.key("tok", redisTestMonth, "org:o")
	if got := env.value(t, orgMonth); got != 0 {
		t.Fatalf("org monthly = %d while another instance holds the claim, want 0", got)
	}
	// The shadow counter covers the gap.
	limits := ScopeLimits{Org: Limits{MonthlyTokenLimit: 400}}
	if err := checkTokens(c, Scopes{KeyID: "k", OrgID: "o"}, limits); !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() = %v, want ErrTokenBudgetExceeded from the shadow counter", err)
	}

	env.mr.FastForward(budgetSeedClaimTTL + time.Second)
	env.clock.Add(budgetSeedClaimTTL + time.Second)
	c.EvictStale()
	eventually(t, "rehydration after the claim expired", func() bool { return env.value(t, orgMonth) == 400 })
	eventually(t, "markers done", env.seeded)
}

// TestRedisBudgetCounter_RehydrationFailureIsRetried verifies that a failed
// database read leaves the bucket unseeded and that a later attempt succeeds.
func TestRedisBudgetCounter_RehydrationFailureIsRetried(t *testing.T) {
	env := newRedisBudgetEnv(t)
	seeder := &fakeUsageSeeder{}
	c, logs := env.newCounter(t, seeder)
	seedErrsBefore := backendErrors("seed")

	// A database that is down at startup fails Start: the shadow counter
	// must not begin empty.
	seeder.mu.Lock()
	seeder.err = errors.New("database is down")
	seeder.mu.Unlock()
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("Start() error = nil, want the shadow seed failure")
	}
	if c.started.Load() {
		t.Fatal("counter started although its shadow seed failed")
	}

	// Start again with a database that works for the shadow seed (two
	// queries) and fails afterwards, i.e. for the bucket rehydration.
	seeder.mu.Lock()
	seeder.err = nil
	seeder.mu.Unlock()
	failing := &failAfterSeeder{inner: seeder, okCalls: 2}
	c.seeder = failing
	seeder.setRows([]seedRow{{keyID: "k", orgID: "o", tokens: 40}})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		c.Stop(ctx)
	})

	eventually(t, "failed rehydration", func() bool { return backendErrors("seed")-seedErrsBefore >= 2 })
	env.settle(t)
	if env.seeded() {
		t.Fatal("bucket marked as seeded although the database read failed")
	}
	if n := logs.count("budget rehydration failed"); n != 2 {
		t.Errorf("rehydration failure logged %d times, want 2 (day and month)", n)
	}

	// The database recovers; the abandoned claims have to expire first.
	failing.heal()
	env.mr.FastForward(budgetSeedClaimTTL + time.Second)
	env.clock.Add(budgetSeedClaimTTL + time.Second)
	c.EvictStale()
	eventually(t, "rehydration", env.seeded)
	if got := env.value(t, env.key("tok", redisTestDay, "org:o")); got != 40 {
		t.Errorf("org daily = %d, want 40", got)
	}
}

// failAfterSeeder passes the first okCalls queries to inner and fails every
// later one until heal is called.
type failAfterSeeder struct {
	inner   UsageSeeder
	mu      sync.Mutex
	okCalls int
	healed  bool
}

func (f *failAfterSeeder) QueryUsageSeed(ctx context.Context, since time.Time) (RowScanner, error) {
	f.mu.Lock()
	ok := f.healed || f.okCalls > 0
	if f.okCalls > 0 {
		f.okCalls--
	}
	f.mu.Unlock()
	if !ok {
		return nil, errors.New("database is down")
	}
	return f.inner.QueryUsageSeed(ctx, since)
}

func (f *failAfterSeeder) heal() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.healed = true
}

// TestRedisBudgetCounter_WindowRollover verifies that a new day starts a new
// daily bucket in Redis while the monthly bucket keeps counting.
func TestRedisBudgetCounter_WindowRollover(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.counter(t, &fakeUsageSeeder{})
	s := Scopes{KeyID: "k", OrgID: "o"}

	addTokens(c, s, 90)
	eventually(t, "increment in redis", func() bool {
		return env.value(t, env.key("tok", redisTestMonth, "org:o")) == 90
	})

	nextDay := redisTestNow.Add(24 * time.Hour)
	env.clock.Set(nextDay)
	env.mr.SetTime(nextDay)

	if err := checkTokens(c, s, ScopeLimits{Org: Limits{DailyTokenLimit: 90}}); err != nil {
		t.Errorf("daily Check() on the next day = %v, want nil", err)
	}
	if err := checkTokens(c, s, ScopeLimits{Org: Limits{MonthlyTokenLimit: 90}}); !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("monthly Check() on the next day = %v, want ErrTokenBudgetExceeded", err)
	}

	addTokens(c, s, 5)
	eventually(t, "increment in the new day bucket", func() bool {
		return env.value(t, env.key("tok", "d:2026-03-11", "org:o")) == 5
	})
	eventually(t, "new day marker", func() bool {
		v, err := env.mr.Get("t:budget:seeded:d:2026-03-11")
		return err == nil && v == "done"
	})
	if got := env.value(t, env.key("tok", redisTestMonth, "org:o")); got != 95 {
		t.Errorf("org monthly = %d, want 95", got)
	}
	if got := env.value(t, env.key("tok", redisTestDay, "org:o")); got != 90 {
		t.Errorf("previous day bucket = %d, want 90 (untouched)", got)
	}
}

// TestRedisBudgetCounter_Snapshot verifies that Snapshot reports the shared
// usage of every requested scope, tokens and cost, and falls back to the
// shadow counter when Redis is down.
func TestRedisBudgetCounter_Snapshot(t *testing.T) {
	env := newRedisBudgetEnv(t)
	seeder := &fakeUsageSeeder{rows: []seedRow{{keyID: "k1", teamID: "tm", orgID: "o", tokens: 100, cost: 2}}}
	a, _ := env.counter(t, seeder)
	eventually(t, "rehydration", env.seeded)

	// Another instance, started later, records more usage.
	seeder.setRows(nil)
	b, _ := env.counter(t, seeder)
	b.Add(context.Background(), Scopes{KeyID: "k2", TeamID: "tm", OrgID: "o"}, Usage{Tokens: 11, CostMicros: 500})
	eventually(t, "increment in redis", func() bool {
		return env.value(t, env.key("cost", redisTestMonth, "org:o")) == 2_000_500
	})
	env.settle(t)

	keys := []string{OrgScope("o"), TeamScope("tm"), KeyScope("k1"), KeyScope("k2"), KeyScope("nobody")}
	snap, err := a.Snapshot(context.Background(), keys)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	shared := Usage{Tokens: 111, CostMicros: 2_000_500}
	want := map[string]WindowUsage{
		OrgScope("o"):      {Daily: shared, Monthly: shared},
		TeamScope("tm"):    {Daily: shared, Monthly: shared},
		KeyScope("k1"):     {Daily: Usage{Tokens: 100, CostMicros: 2_000_000}, Monthly: Usage{Tokens: 100, CostMicros: 2_000_000}},
		KeyScope("k2"):     {Daily: Usage{Tokens: 11, CostMicros: 500}, Monthly: Usage{Tokens: 11, CostMicros: 500}},
		KeyScope("nobody"): {},
	}
	if len(snap) != len(want) {
		t.Errorf("Snapshot() returned %d entries, want %d", len(snap), len(want))
	}
	for k, w := range want {
		if snap[k] != w {
			t.Errorf("Snapshot()[%q] = %+v, want %+v", k, snap[k], w)
		}
	}

	// Redis down: instance a only knows what it was seeded with.
	env.mr.Close()
	snap, err = a.Snapshot(context.Background(), keys)
	if err != nil {
		t.Fatalf("Snapshot() during outage error = %v", err)
	}
	local := Usage{Tokens: 100, CostMicros: 2_000_000}
	if got := snap[OrgScope("o")]; got.Daily != local || got.Monthly != local {
		t.Errorf("Snapshot() during outage = %+v, want the shadow value %+v", got, local)
	}
	if got := snap[KeyScope("k2")]; got != (WindowUsage{}) {
		t.Errorf("Snapshot() during outage for a scope only known to redis = %+v, want zero", got)
	}
	if err := env.mr.Restart(); err != nil {
		t.Fatalf("miniredis Restart: %v", err)
	}
}

// TestRedisBudgetCounter_StopDrainsQueue verifies that Stop writes everything
// that was queued before it returns, and that later usage stays local.
func TestRedisBudgetCounter_StopDrainsQueue(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.newCounter(t, &fakeUsageSeeder{})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const n = 500
	s := Scopes{KeyID: "k", OrgID: "o"}
	for range n {
		addTokens(c, s, 2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c.Stop(ctx)

	orgDay := env.key("tok", redisTestDay, "org:o")
	if got := env.value(t, orgDay); got != 2*n {
		t.Errorf("org daily after Stop = %d, want %d", got, 2*n)
	}

	before := env.mr.CommandCount()
	addTokens(c, s, 2)
	c.Stop(ctx) // a second Stop is a no-op
	if got := env.mr.CommandCount(); got != before {
		t.Errorf("redis saw %d commands after Stop, want 0", got-before)
	}
	if got := c.shadow.usage(OrgScope("o"), windowsAt(env.clock.Now())).Daily.Tokens; got != 2*n+2 {
		t.Errorf("shadow org daily = %d, want %d", got, 2*n+2)
	}
}

// TestRedisBudgetCounter_StopGivesUpAtDeadline verifies that Stop returns
// soon after its context ends although Redis is unreachable and increments
// are still queued.
func TestRedisBudgetCounter_StopGivesUpAtDeadline(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.newCounter(t, &fakeUsageSeeder{})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	eventually(t, "startup rehydration", env.seeded)
	env.settle(t)

	env.mr.Close()
	for range 200 {
		addTokens(c, Scopes{KeyID: "k", OrgID: "o"}, 1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		c.Stop(ctx)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return after its deadline")
	}
	if err := env.mr.Restart(); err != nil {
		t.Fatalf("miniredis Restart: %v", err)
	}
}

// TestRedisBudgetCounter_QueueFullDropsAndCounts verifies that Add never
// blocks: with a full queue the increment is dropped from Redis, counted, and
// kept in the shadow counter.
func TestRedisBudgetCounter_QueueFullDropsAndCounts(t *testing.T) {
	env := newRedisBudgetEnv(t)
	// Not started: nothing consumes the queue.
	c, logs := env.newCounter(t, &fakeUsageSeeder{})

	const extra = 7
	s := Scopes{KeyID: "k", OrgID: "o"}
	before := backendErrors("queue_full")
	for range budgetQueueSize + extra {
		addTokens(c, s, 1)
	}

	if got := len(c.queue); got != budgetQueueSize {
		t.Errorf("queue length = %d, want %d", got, budgetQueueSize)
	}
	if got := backendErrors("queue_full") - before; got != extra {
		t.Errorf("queue_full errors = %v, want %d", got, extra)
	}
	if n := logs.count("budget counter queue full"); n != 1 {
		t.Errorf("queue full logged %d times, want once per interval", n)
	}
	want := int64(budgetQueueSize + extra)
	if got := c.shadow.usage(OrgScope("o"), windowsAt(env.clock.Now())).Monthly.Tokens; got != want {
		t.Errorf("shadow org monthly = %d, want %d (dropped increments stay local)", got, want)
	}
	if err := checkTokens(c, s, ScopeLimits{Org: Limits{MonthlyTokenLimit: want}}); !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() = %v, want ErrTokenBudgetExceeded from the shadow counter", err)
	}
}
