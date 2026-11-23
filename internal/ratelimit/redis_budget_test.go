package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
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

// count returns how many times sub occurs in the log.
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

// redisTestNow is the instant the Redis budget tests start at: mid-day and
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

// setTime moves the shared clock and the Redis clock to now.
func (e *redisBudgetEnv) setTime(now time.Time) {
	e.clock.Set(now)
	e.mr.SetTime(now)
}

// newCounter returns a counter on the shared Redis that has not been started.
// Its write loop is effectively off (one hour intervals), so that tests decide
// when totals are written by calling flush themselves.
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
	c.own.now = e.clock.Now
	c.flushInterval = time.Hour
	c.resyncInterval = time.Hour
	return c, logs
}

// start starts c and stops it when the test ends.
func (e *redisBudgetEnv) start(t *testing.T, c *RedisBudgetCounter) {
	t.Helper()
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { stopCounter(c) })
}

// counter returns a started counter whose totals are only written when the
// test calls flush (or triggers a resync).
func (e *redisBudgetEnv) counter(t *testing.T, seeder UsageSeeder) (*RedisBudgetCounter, *lockedLog) {
	t.Helper()
	c, logs := e.newCounter(t, seeder)
	e.start(t, c)
	return c, logs
}

// liveCounter returns a started counter whose write loop runs every few
// milliseconds, like in production but faster.
func (e *redisBudgetEnv) liveCounter(t *testing.T, seeder UsageSeeder) (*RedisBudgetCounter, *lockedLog) {
	t.Helper()
	c, logs := e.newCounter(t, seeder)
	c.flushInterval = 5 * time.Millisecond
	c.resyncInterval = 20 * time.Millisecond
	e.start(t, c)
	return c, logs
}

func stopCounter(c *RedisBudgetCounter) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c.Stop(ctx)
}

// flushNow writes c's scheduled totals to Redis.
func flushNow(c *RedisBudgetCounter) {
	c.flush(context.Background(), false)
}

// key returns the Redis key of a budget counter.
func (e *redisBudgetEnv) key(metric, bucket, scope string) string {
	return "t:budget:" + metric + ":" + bucket + ":" + scope
}

// field returns what one instance reported for a counter, 0 when it has no
// field there.
func (e *redisBudgetEnv) field(t *testing.T, key string, c *RedisBudgetCounter) int64 {
	t.Helper()
	return parseBudgetValue(t, key, e.mr.HGet(key, c.instanceID))
}

// sum returns the sum of everything all instances reported for a counter.
func (e *redisBudgetEnv) sum(t *testing.T, key string) int64 {
	t.Helper()
	if !e.mr.Exists(key) {
		return 0
	}
	fields, err := e.mr.HKeys(key)
	if err != nil {
		t.Fatalf("miniredis HKeys(%q): %v", key, err)
	}
	var total int64
	for _, f := range fields {
		total += parseBudgetValue(t, key, e.mr.HGet(key, f))
	}
	return total
}

func parseBudgetValue(t *testing.T, key, s string) int64 {
	t.Helper()
	if s == "" {
		return 0
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("value %q in %s is not an integer", s, key)
	}
	return n
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

func backendErrors(op string) float64 {
	return testutil.ToFloat64(metrics.BudgetBackendErrorsTotal.WithLabelValues(op))
}

// snapshotOf returns c's view of one scope.
func snapshotOf(t *testing.T, c *RedisBudgetCounter, scope string) WindowUsage {
	t.Helper()
	snap, err := c.Snapshot(context.Background(), []string{scope})
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	return snap[scope]
}

// TestRedisBudgetCounter_TwoInstancesShareOneBudget verifies the point of the
// Redis counter, with the real write loop: usage recorded on two instances
// adds up in one budget that both of them enforce, although neither has seen
// enough usage on its own.
func TestRedisBudgetCounter_TwoInstancesShareOneBudget(t *testing.T) {
	env := newRedisBudgetEnv(t)
	a, _ := env.liveCounter(t, &fakeUsageSeeder{})
	b, _ := env.liveCounter(t, &fakeUsageSeeder{})
	if a.instanceID == "" || a.instanceID == b.instanceID {
		t.Fatalf("instance IDs = %q, %q, want two distinct non-empty IDs", a.instanceID, b.instanceID)
	}

	addTokens(a, Scopes{KeyID: "key-a", TeamID: "team-1", OrgID: "org-1"}, 60)
	addTokens(b, Scopes{KeyID: "key-b", TeamID: "team-1", OrgID: "org-1"}, 50)

	orgDay := env.key("tok", redisTestDay, "org:org-1")
	eventually(t, "both totals in redis", func() bool { return env.sum(t, orgDay) == 110 })
	eventually(t, "team totals in redis", func() bool {
		return env.sum(t, env.key("tok", redisTestMonth, "team:team-1")) == 110
	})

	if got := env.field(t, orgDay, a); got != 60 {
		t.Errorf("instance a's org field = %d, want 60 (its own usage)", got)
	}
	if got := env.field(t, orgDay, b); got != 50 {
		t.Errorf("instance b's org field = %d, want 50 (its own usage)", got)
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

// TestRedisBudgetCounter_UserScopeAndCost verifies that the org-bound user
// scope is shared between instances and keys of that user, and that cost is
// carried next to tokens.
func TestRedisBudgetCounter_UserScopeAndCost(t *testing.T) {
	env := newRedisBudgetEnv(t)
	a, _ := env.counter(t, &fakeUsageSeeder{})
	b, _ := env.counter(t, &fakeUsageSeeder{})
	ctx := context.Background()

	a.Add(ctx, Scopes{KeyID: "key-1", UserID: "user-1", OrgID: "org-1"}, Usage{Tokens: 250, CostMicros: 1_200})
	b.Add(ctx, Scopes{KeyID: "key-2", UserID: "user-1", OrgID: "org-1"}, Usage{Tokens: 250, CostMicros: 300})
	// Same user ID in another org: a separate budget.
	b.Add(ctx, Scopes{KeyID: "key-3", UserID: "user-1", OrgID: "org-2"}, Usage{Tokens: 10})
	flushNow(a)
	flushNow(b)

	for key, want := range map[string]int64{
		env.key("tok", redisTestDay, "user:org-1:user-1"):    500,
		env.key("tok", redisTestMonth, "user:org-1:user-1"):  500,
		env.key("cost", redisTestDay, "user:org-1:user-1"):   1_500,
		env.key("cost", redisTestMonth, "user:org-1:user-1"): 1_500,
		env.key("cost", redisTestMonth, "org:org-1"):         1_500,
		env.key("cost", redisTestDay, "key:key-1"):           1_200,
		env.key("tok", redisTestDay, "user:org-2:user-1"):    10,
		env.key("cost", redisTestDay, "user:org-2:user-1"):   0,
	} {
		if got := env.sum(t, key); got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}

	want := Usage{Tokens: 500, CostMicros: 1_500}
	if got := snapshotOf(t, a, UserScope("org-1", "user-1")); got.Daily != want || got.Monthly != want {
		t.Errorf("Snapshot() of the user = %+v, want daily and monthly %+v", got, want)
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

// TestRedisBudgetCounter_KeysAndExpiry pins the key layout, the per-instance
// field and the expiry of daily and monthly counters, and that zero totals
// create no keys.
func TestRedisBudgetCounter_KeysAndExpiry(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.counter(t, &fakeUsageSeeder{})

	c.Add(context.Background(), Scopes{KeyID: "k", UserID: "u", TeamID: "tm", OrgID: "o"}, Usage{Tokens: 7})
	flushNow(c)

	want := map[string]bool{}
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
		fields, err := env.mr.HKeys(k)
		if err != nil || len(fields) != 1 || fields[0] != c.instanceID {
			t.Errorf("fields of %s = %v (err %v), want only the instance ID %q", k, fields, err, c.instanceID)
		}
		if v := env.mr.HGet(k, c.instanceID); v != "7" {
			t.Errorf("%s[%s] = %q, want 7", k, c.instanceID, v)
		}
	}

	// Daily counters live until 24h after their day ended, monthly ones
	// until 48h after their month ended.
	wantDayTTL := time.Date(2026, 3, 12, 0, 0, 0, 0, time.UTC).Sub(redisTestNow)
	wantMonthTTL := time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC).Sub(redisTestNow)
	if ttl := env.mr.TTL("t:budget:tok:d:2026-03-10:org:o"); ttl != wantDayTTL {
		t.Errorf("daily TTL = %v, want %v", ttl, wantDayTTL)
	}
	if ttl := env.mr.TTL("t:budget:tok:m:2026-03:org:o"); ttl != wantMonthTTL {
		t.Errorf("monthly TTL = %v, want %v", ttl, wantMonthTTL)
	}

	// Cost gets its own counters once it is non-zero.
	c.Add(context.Background(), Scopes{KeyID: "k", OrgID: "o"}, Usage{CostMicros: 1500})
	flushNow(c)
	if got := env.field(t, env.key("cost", redisTestDay, "key:k"), c); got != 1500 {
		t.Errorf("daily key cost = %d, want 1500", got)
	}
	if got := env.field(t, env.key("cost", redisTestMonth, "org:o"), c); got != 1500 {
		t.Errorf("monthly org cost = %d, want 1500", got)
	}
	if got := env.field(t, env.key("tok", redisTestDay, "key:k"), c); got != 7 {
		t.Errorf("daily key tokens = %d, want 7 (a cost-only add must not change tokens)", got)
	}
}

// TestRedisBudgetCounter_NoRedisCallsWithoutLimits verifies that a request
// without budgets never reaches Redis, that recording usage does not either,
// and that a request with budgets costs exactly one round trip.
func TestRedisBudgetCounter_NoRedisCallsWithoutLimits(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.counter(t, &fakeUsageSeeder{})

	s := Scopes{KeyID: "k", UserID: "u", TeamID: "tm", OrgID: "o"}
	all := Limits{DailyTokenLimit: 1_000_000, MonthlyTokenLimit: 1_000_000}
	allLimits := ScopeLimits{Key: all, User: all, Team: all, Org: all}

	// The first check loads the sum script; every later one is a single
	// command.
	if err := checkTokens(c, s, allLimits); err != nil {
		t.Fatalf("warm-up Check() = %v, want nil", err)
	}
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
		c.Add(context.Background(), s, Usage{Tokens: 3, CostMicros: 2})
		c.Add(context.Background(), s, Usage{})
	}
	if got := env.mr.CommandCount(); got != before {
		t.Errorf("redis saw %d commands for requests without budgets, want 0", got-before)
	}

	// All eight budgets set: one round trip. miniredis counts the script
	// call itself plus every HVALS the script runs on the server, so one
	// client command for eight counters shows up as nine.
	if err := checkTokens(c, s, allLimits); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
	if got := env.mr.CommandCount() - before; got != 9 {
		t.Errorf("redis saw %d commands for one check with eight budgets, want 9 (1 script call + 8 server-side reads)", got)
	}
	// A single budget: the script call and one server-side read.
	before = env.mr.CommandCount()
	if err := checkTokens(c, s, ScopeLimits{Org: Limits{DailyTokenLimit: 1_000_000}}); err != nil {
		t.Fatalf("Check() = %v, want nil", err)
	}
	if got := env.mr.CommandCount() - before; got != 2 {
		t.Errorf("redis saw %d commands for one check with one budget, want 2", got)
	}
}

// TestRedisBudgetCounter_FlushBatches verifies that any number of Adds
// between two flushes results in one write per counter, and that a flush
// without changes writes nothing.
func TestRedisBudgetCounter_FlushBatches(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.counter(t, &fakeUsageSeeder{})
	s := Scopes{KeyID: "k", OrgID: "o"}

	for range 1000 {
		addTokens(c, s, 2)
	}
	before := env.mr.CommandCount()
	flushNow(c)
	// Key and org, day and month: four counters, each one HSET + EXPIREAT.
	if got := env.mr.CommandCount() - before; got != 8 {
		t.Errorf("flush after 1000 Adds sent %d commands, want 8", got)
	}
	if got := env.field(t, env.key("tok", redisTestDay, "org:o"), c); got != 2000 {
		t.Errorf("org daily = %d, want 2000", got)
	}

	before = env.mr.CommandCount()
	flushNow(c)
	if got := env.mr.CommandCount() - before; got != 0 {
		t.Errorf("flush without changes sent %d commands, want 0", got)
	}

	// Only the scopes that changed are written.
	addTokens(c, Scopes{KeyID: "k2", OrgID: "o"}, 1)
	addTokens(c, Scopes{KeyID: "k2", OrgID: "o"}, 1)
	before = env.mr.CommandCount()
	flushNow(c)
	if got := env.mr.CommandCount() - before; got != 8 {
		t.Errorf("flush after a change to two scopes sent %d commands, want 8", got)
	}
	if got := env.field(t, env.key("tok", redisTestMonth, "org:o"), c); got != 2002 {
		t.Errorf("org monthly = %d, want 2002", got)
	}
	if got := env.field(t, env.key("tok", redisTestDay, "key:k"), c); got != 2000 {
		t.Errorf("key k daily = %d, want 2000 (untouched)", got)
	}
}

// TestRedisBudgetCounter_WritesSelfHeal verifies that totals are absolute: a
// repeated write does not count twice, and a value that Redis lost is
// restored to the full total by the next write, with or without new usage.
func TestRedisBudgetCounter_WritesSelfHeal(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.counter(t, &fakeUsageSeeder{})
	s := Scopes{KeyID: "k", OrgID: "o"}
	orgDay := env.key("tok", redisTestDay, "org:o")
	orgMonth := env.key("tok", redisTestMonth, "org:o")

	addTokens(c, s, 80)
	flushNow(c)
	if got := env.field(t, orgDay, c); got != 80 {
		t.Fatalf("org daily = %d, want 80", got)
	}

	// Writing everything again is not counted again.
	c.markAllDirty()
	flushNow(c)
	c.markAllDirty()
	flushNow(c)
	if got := env.sum(t, orgDay); got != 80 {
		t.Errorf("org daily after repeated writes = %d, want 80", got)
	}

	// The field is lost: the next write after new usage restores the full
	// total, not just the new usage.
	env.mr.HDel(orgDay, c.instanceID)
	addTokens(c, s, 5)
	flushNow(c)
	if got := env.field(t, orgDay, c); got != 85 {
		t.Errorf("org daily after losing the field = %d, want 85", got)
	}

	// Whole hashes are lost and there is no new usage: the periodic full
	// write restores them.
	env.mr.Del(orgDay)
	env.mr.Del(orgMonth)
	flushNow(c)
	if env.mr.Exists(orgDay) {
		t.Fatal("a flush without changes restored the hash; the test would not prove the resync")
	}
	c.EvictStale() // asks the loop for a full write
	eventually(t, "full write restores the totals", func() bool {
		return env.field(t, orgDay, c) == 85 && env.field(t, orgMonth, c) == 85
	})
}

// TestRedisBudgetCounter_RestartDoesNotDoubleCount verifies the numbers
// around a restart: the stopped instance's field stays and keeps counting,
// the restarted instance reports under a new ID starting from zero while its
// shadow is seeded with the persisted usage, and no view counts anything
// twice. It then pins what a Redis data loss leaves behind.
func TestRedisBudgetCounter_RestartDoesNotDoubleCount(t *testing.T) {
	env := newRedisBudgetEnv(t)
	orgDay := env.key("tok", redisTestDay, "org:o")
	keyBDay := env.key("tok", redisTestDay, "key:key-b")
	sa := Scopes{KeyID: "key-a", OrgID: "o"}
	sb := Scopes{KeyID: "key-b", OrgID: "o"}

	a, _ := env.counter(t, &fakeUsageSeeder{})
	b, _ := env.newCounter(t, &fakeUsageSeeder{})
	if err := b.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addTokens(a, sa, 60)
	addTokens(b, sb, 50)
	flushNow(a)
	stopCounter(b) // the final flush writes b's 50

	if got := env.sum(t, orgDay); got != 110 {
		t.Fatalf("org daily after stop = %d, want 110", got)
	}

	// b restarts. The database holds everything recorded so far.
	persisted := &fakeUsageSeeder{rows: []seedRow{
		{keyID: "key-a", orgID: "o", tokens: 60},
		{keyID: "key-b", orgID: "o", tokens: 50},
	}}
	b2, _ := env.counter(t, persisted)
	if b2.instanceID == b.instanceID {
		t.Fatal("restarted instance reuses the old instance ID")
	}
	if got := env.field(t, orgDay, b); got != 50 {
		t.Errorf("stopped instance's field = %d, want 50 (kept)", got)
	}

	addTokens(b2, sb, 5)
	flushNow(b2)
	if got := env.field(t, orgDay, b2); got != 5 {
		t.Errorf("restarted instance's field = %d, want 5 (own usage since its start, not the seed)", got)
	}
	if got := env.sum(t, orgDay); got != 115 {
		t.Errorf("org daily sum = %d, want 115 (60 + 50 + 5)", got)
	}
	if got := env.sum(t, keyBDay); got != 55 {
		t.Errorf("key-b daily sum = %d, want 55 (50 + 5)", got)
	}

	// Shadow of b2 is 110 (seed) + 5; the shared sum is 115 as well. Both
	// instances see exactly 115, not 165 or 225.
	for name, c := range map[string]*RedisBudgetCounter{"a": a, "b2": b2} {
		got := snapshotOf(t, c, OrgScope("o"))
		if got.Daily.Tokens != 115 || got.Monthly.Tokens != 115 {
			t.Errorf("instance %s sees org usage %+v, want 115 daily and monthly", name, got)
		}
		if err := checkTokens(c, sb, ScopeLimits{Org: Limits{DailyTokenLimit: 116}}); err != nil {
			t.Errorf("instance %s: Check() with limit 116 = %v, want nil", name, err)
		}
		if err := checkTokens(c, sb, ScopeLimits{Org: Limits{DailyTokenLimit: 115}}); !errors.Is(err, ErrTokenBudgetExceeded) {
			t.Errorf("instance %s: Check() with limit 115 = %v, want ErrTokenBudgetExceeded", name, err)
		}
	}
	if got := snapshotOf(t, b2, KeyScope("key-b")).Daily.Tokens; got != 55 {
		t.Errorf("b2 sees key-b usage %d, want 55", got)
	}

	// Redis loses everything. The running instances restore their own
	// totals with the next full write; the stopped instance's 50 are gone
	// from Redis and only known to instances seeded after it stopped.
	env.mr.FlushAll()
	a.EvictStale()
	b2.EvictStale()
	eventually(t, "running instances restore their totals", func() bool {
		return env.field(t, orgDay, a) == 60 && env.field(t, orgDay, b2) == 5
	})
	if got := env.sum(t, orgDay); got != 65 {
		t.Errorf("org daily sum after data loss = %d, want 65 (60 + 5)", got)
	}
	if got := snapshotOf(t, b2, OrgScope("o")).Daily.Tokens; got != 115 {
		t.Errorf("b2 sees %d after data loss, want 115 (its shadow)", got)
	}
	if got := snapshotOf(t, a, OrgScope("o")).Daily.Tokens; got != 65 {
		t.Errorf("a sees %d after data loss, want 65 (documented gap: the stopped instance's usage is only in the database)", got)
	}
}

// TestRedisBudgetCounter_OutageFallsBackToShadow verifies the degraded mode:
// with Redis down checks are decided on the local shadow counter (neither
// open nor closed), the transition is logged once in each direction, the
// error metric counts the failures, and after recovery Redis receives the
// full totals including what was recorded during the outage.
func TestRedisBudgetCounter_OutageFallsBackToShadow(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, logs := env.counter(t, &fakeUsageSeeder{})

	s := Scopes{KeyID: "k", OrgID: "o"}
	limits := ScopeLimits{Org: Limits{DailyTokenLimit: 100}}
	orgDay := env.key("tok", redisTestDay, "org:o")

	addTokens(c, s, 80)
	flushNow(c)
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
		t.Fatal("counter not degraded after a failed check")
	}
	if got := backendErrors("check") - checkErrsBefore; got != 1 {
		t.Errorf("check errors = %v, want 1 (later checks must not probe within the probe interval)", got)
	}

	// Usage recorded during the outage counts locally. A regular flush
	// waits for its turn to probe; a forced one fails and keeps the totals
	// scheduled.
	addTokens(c, s, 30)
	flushNow(c)
	if got := backendErrors("flush") - flushErrsBefore; got != 0 {
		t.Errorf("flush errors = %v, want 0 (no write attempt within the probe interval)", got)
	}
	c.flush(context.Background(), true)
	if got := backendErrors("flush") - flushErrsBefore; got != 1 {
		t.Errorf("flush errors after a forced flush = %v, want 1", got)
	}
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

	// Recovery triggers a full write: Redis catches up to 110 without any
	// new usage.
	eventually(t, "redis catches up after the outage", func() bool { return env.field(t, orgDay, c) == 110 })
	if err := checkTokens(c, s, limits); !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("Check() after recovery = %v, want ErrTokenBudgetExceeded", err)
	}
}

// TestRedisBudgetCounter_WindowRollover verifies that the final totals of a
// finished day are still written after midnight, that a new day starts a new
// daily counter while the monthly one keeps counting, and that totals of a
// window whose counter has outlived its grace are dropped.
func TestRedisBudgetCounter_WindowRollover(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.counter(t, &fakeUsageSeeder{})
	s := Scopes{KeyID: "k", OrgID: "o"}

	// Recorded just before midnight, not written yet.
	env.setTime(time.Date(2026, 3, 10, 23, 59, 59, 0, time.UTC))
	addTokens(c, s, 90)

	env.setTime(time.Date(2026, 3, 11, 0, 0, 1, 0, time.UTC))
	if err := checkTokens(c, s, ScopeLimits{Org: Limits{DailyTokenLimit: 90}}); err != nil {
		t.Errorf("daily Check() on the next day = %v, want nil", err)
	}
	if err := checkTokens(c, s, ScopeLimits{Org: Limits{MonthlyTokenLimit: 90}}); !errors.Is(err, ErrTokenBudgetExceeded) {
		t.Errorf("monthly Check() on the next day = %v, want ErrTokenBudgetExceeded", err)
	}
	addTokens(c, s, 5)
	flushNow(c)

	if got := env.field(t, env.key("tok", redisTestDay, "org:o"), c); got != 90 {
		t.Errorf("finished day's counter = %d, want 90 (written after midnight)", got)
	}
	if got := env.field(t, env.key("tok", "d:2026-03-11", "org:o"), c); got != 5 {
		t.Errorf("new day's counter = %d, want 5", got)
	}
	if got := env.field(t, env.key("tok", redisTestMonth, "org:o"), c); got != 95 {
		t.Errorf("monthly counter = %d, want 95 (never the older 90)", got)
	}

	// Usage that could not be written for days: the day counter is past its
	// grace and dropped, the month is still written.
	late := Scopes{KeyID: "late", OrgID: "late-org"}
	addTokens(c, late, 40)
	env.setTime(time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC))
	flushNow(c)
	if env.mr.Exists(env.key("tok", "d:2026-03-11", "key:late")) {
		t.Error("totals of a day past its grace were written")
	}
	if got := env.field(t, env.key("tok", redisTestMonth, "key:late"), c); got != 40 {
		t.Errorf("monthly counter of the late scope = %d, want 40", got)
	}

	// Past the month's grace as well: nothing is written, nothing is kept.
	gone := Scopes{KeyID: "gone", OrgID: "gone-org"}
	addTokens(c, gone, 9)
	env.setTime(time.Date(2026, 4, 3, 0, 0, 0, 0, time.UTC))
	before := env.mr.CommandCount()
	flushNow(c)
	if got := env.mr.CommandCount() - before; got != 0 {
		t.Errorf("flush past every grace sent %d commands, want 0", got)
	}
}

// TestRedisBudgetCounter_Snapshot verifies that Snapshot reports the larger of
// the shared sum and the shadow per scope, tokens and cost, has an entry for
// every requested scope, and falls back to the shadow counter when Redis is
// down.
func TestRedisBudgetCounter_Snapshot(t *testing.T) {
	env := newRedisBudgetEnv(t)
	// Both instances start on a database that already holds 100 tokens and
	// 2 currency units from before shared budgets were used.
	history := &fakeUsageSeeder{rows: []seedRow{{keyID: "k1", teamID: "tm", orgID: "o", tokens: 100, cost: 2}}}
	a, _ := env.counter(t, history)
	b, _ := env.counter(t, history)

	b.Add(context.Background(), Scopes{KeyID: "k2", TeamID: "tm", OrgID: "o"}, Usage{Tokens: 11, CostMicros: 500})
	flushNow(b)

	keys := []string{OrgScope("o"), KeyScope("k1"), KeyScope("k2"), KeyScope("nobody")}
	snap, err := a.Snapshot(context.Background(), keys)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(snap) != len(keys) {
		t.Errorf("Snapshot() returned %d entries, want %d", len(snap), len(keys))
	}
	seeded := Usage{Tokens: 100, CostMicros: 2_000_000}
	live := Usage{Tokens: 11, CostMicros: 500}
	want := map[string]WindowUsage{
		// History that is only in the database is not in Redis: a sees the
		// larger of its shadow (100) and the shared sum (11).
		OrgScope("o"):      {Daily: seeded, Monthly: seeded},
		KeyScope("k1"):     {Daily: seeded, Monthly: seeded},
		KeyScope("k2"):     {Daily: live, Monthly: live},
		KeyScope("nobody"): {},
	}
	for k, w := range want {
		if snap[k] != w {
			t.Errorf("a.Snapshot()[%q] = %+v, want %+v", k, snap[k], w)
		}
	}
	// b recorded the live usage itself, so its shadow has both.
	both := Usage{Tokens: 111, CostMicros: 2_000_500}
	if got := snapshotOf(t, b, OrgScope("o")); got.Daily != both || got.Monthly != both {
		t.Errorf("b.Snapshot() of the org = %+v, want daily and monthly %+v", got, both)
	}

	// Redis down: only the shadow is left, and the failure is counted as a
	// snapshot error.
	before := backendErrors("snapshot")
	env.mr.Close()
	snap, err = a.Snapshot(context.Background(), keys)
	if err != nil {
		t.Fatalf("Snapshot() during outage error = %v", err)
	}
	if got := snap[OrgScope("o")]; got.Daily != seeded || got.Monthly != seeded {
		t.Errorf("Snapshot() during outage = %+v, want the shadow value %+v", got, seeded)
	}
	if got := snap[KeyScope("k2")]; got != (WindowUsage{}) {
		t.Errorf("Snapshot() during outage for a scope only known to redis = %+v, want zero", got)
	}
	if got := backendErrors("snapshot") - before; got != 1 {
		t.Errorf("snapshot errors = %v, want 1", got)
	}
	if err := env.mr.Restart(); err != nil {
		t.Fatalf("miniredis Restart: %v", err)
	}
}

// TestRedisBudgetCounter_StopFlushesFinalTotals verifies that Stop writes the
// totals that were not flushed yet, and that later usage stays local.
func TestRedisBudgetCounter_StopFlushesFinalTotals(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.newCounter(t, &fakeUsageSeeder{})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const n = 500
	s := Scopes{KeyID: "k", OrgID: "o"}
	for range n {
		c.Add(context.Background(), s, Usage{Tokens: 2, CostMicros: 3})
	}
	orgDay := env.key("tok", redisTestDay, "org:o")
	if env.mr.Exists(orgDay) {
		t.Fatal("totals were written before Stop; the test would not prove the final flush")
	}
	stopCounter(c)

	if got := env.field(t, orgDay, c); got != 2*n {
		t.Errorf("org daily tokens after Stop = %d, want %d", got, 2*n)
	}
	if got := env.field(t, env.key("cost", redisTestMonth, "key:k"), c); got != 3*n {
		t.Errorf("key monthly cost after Stop = %d, want %d", got, 3*n)
	}

	before := env.mr.CommandCount()
	addTokens(c, s, 2)
	stopCounter(c) // a second Stop is a no-op
	if got := env.mr.CommandCount(); got != before {
		t.Errorf("redis saw %d commands after Stop, want 0", got-before)
	}
	if got := c.shadow.usage(OrgScope("o"), windowsAt(env.clock.Now())).Daily.Tokens; got != 2*n+2 {
		t.Errorf("shadow org daily = %d, want %d", got, 2*n+2)
	}
}

// TestRedisBudgetCounter_StopGivesUpAtDeadline verifies that Stop returns
// soon after its context ends although Redis is unreachable.
func TestRedisBudgetCounter_StopGivesUpAtDeadline(t *testing.T) {
	env := newRedisBudgetEnv(t)
	c, _ := env.newCounter(t, &fakeUsageSeeder{})
	if err := c.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addTokens(c, Scopes{KeyID: "k", OrgID: "o"}, 1)
	env.mr.Close()

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

// TestRedisBudgetCounter_StartGuards verifies that Start fails when the
// shadow counter cannot be seeded, that a counter that never started can be
// stopped, and that Start cannot be called twice.
func TestRedisBudgetCounter_StartGuards(t *testing.T) {
	env := newRedisBudgetEnv(t)

	failing, _ := env.newCounter(t, &fakeUsageSeeder{err: errors.New("database is down")})
	if err := failing.Start(context.Background()); err == nil {
		t.Fatal("Start() error = nil, want the seed failure")
	}
	if failing.started.Load() {
		t.Error("counter runs although its shadow seed failed")
	}
	stopCounter(failing) // must not block

	seeder := &fakeUsageSeeder{rows: []seedRow{{keyID: "k", orgID: "o", tokens: 40}}}
	c, _ := env.counter(t, seeder)
	if err := c.Start(context.Background()); !errors.Is(err, errBudgetCounterStarted) {
		t.Errorf("second Start() = %v, want errBudgetCounterStarted", err)
	}
	if got := seeder.queries(); got != 1 {
		t.Errorf("seed queries = %d, want 1 (the second Start must not seed again)", got)
	}
	// The seed feeds the shadow counter only, never Redis.
	want := Usage{Tokens: 40}
	if got := snapshotOf(t, c, OrgScope("o")); got.Daily != want || got.Monthly != want {
		t.Errorf("Snapshot() after Start = %+v, want the seeded %+v", got, want)
	}
	c.markAllDirty()
	flushNow(c)
	if keys := env.mr.Keys(); len(keys) != 0 {
		t.Errorf("redis keys after a full write without own usage = %v, want none", keys)
	}
}
