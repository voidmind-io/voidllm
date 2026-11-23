package ratelimit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/voidmind-io/voidllm/internal/metrics"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
)

// Compile-time assertion: RedisBudgetCounter must implement BudgetCounter.
var _ BudgetCounter = (*RedisBudgetCounter)(nil)

const (
	// budgetFlushInterval is how often changed totals are written to Redis.
	budgetFlushInterval = 250 * time.Millisecond
	// budgetResyncInterval is how often every total of the current windows
	// is written again, changed or not, so that Redis heals from lost data
	// without waiting for new usage.
	budgetResyncInterval = 30 * time.Second
	// budgetReadTimeout bounds the Redis round trip of a check or snapshot.
	budgetReadTimeout = 500 * time.Millisecond
	// budgetWriteTimeout bounds one write pipeline.
	budgetWriteTimeout = 2 * time.Second
	// budgetWriteChunk is the largest number of values sent in one pipeline.
	budgetWriteChunk = 1000
	// budgetProbeInterval is how often a degraded counter lets one operation
	// try Redis again; everything else uses the local counters meanwhile.
	budgetProbeInterval = time.Second
	// budgetDayGrace and budgetMonthGrace are how long daily and monthly
	// counters outlive their window before Redis evicts them. Totals of a
	// finished window are not written any more once its grace has passed.
	budgetDayGrace   = 24 * time.Hour
	budgetMonthGrace = 48 * time.Hour
)

// Values of the "op" label of metrics.BudgetBackendErrorsTotal.
const (
	budgetOpCheck    = "check"
	budgetOpSnapshot = "snapshot"
	budgetOpFlush    = "flush"
)

// errBudgetCounterStarted is returned by a second call to Start.
var errBudgetCounterStarted = errors.New("budget counter already started")

// budgetBuckets holds the Redis bucket names and counter expiries of one UTC
// day and the month it belongs to.
type budgetBuckets struct {
	dayStart    int64
	day         string
	month       string
	dayExpire   time.Time
	monthExpire time.Time
}

// bucketsOf returns the bucket names and expiries for the windows w.
func bucketsOf(w budgetWindows) *budgetBuckets {
	return &budgetBuckets{
		dayStart:    w.dayStart.Unix(),
		day:         voidredis.BudgetDayBucket(w.dayStart),
		month:       voidredis.BudgetMonthBucket(w.monthStart),
		dayExpire:   nextUTCMidnight(w.dayStart).Add(budgetDayGrace),
		monthExpire: nextUTCMonth(w.monthStart).Add(budgetMonthGrace),
	}
}

// dirtyKey identifies totals that still have to be written to Redis: one
// scope in the windows of one day. A scope used across midnight has one entry
// per day, so the final totals of the finished day are still written.
type dirtyKey struct {
	scope    string
	dayStart int64
}

// dirtyEntry holds the windows a dirtyKey refers to and the highest totals
// seen for them. Totals only grow within a window, so the highest is the
// latest.
type dirtyEntry struct {
	at    budgetWindows
	usage WindowUsage
}

// raise lifts the totals of e to those of u where u is higher.
func (e *dirtyEntry) raise(u WindowUsage) {
	e.usage.Daily.Tokens = max(e.usage.Daily.Tokens, u.Daily.Tokens)
	e.usage.Daily.CostMicros = max(e.usage.Daily.CostMicros, u.Daily.CostMicros)
	e.usage.Monthly.Tokens = max(e.usage.Monthly.Tokens, u.Monthly.Tokens)
	e.usage.Monthly.CostMicros = max(e.usage.Monthly.CostMicros, u.Monthly.CostMicros)
}

// RedisBudgetCounter is a BudgetCounter that shares usage through Redis, so
// that all VoidLLM instances on that Redis enforce one budget together.
//
// Model: every instance reports its own absolute totals, never increments.
// A counter is a hash under
// "<prefix>budget:<tok|cost>:<d:YYYY-MM-DD|m:YYYY-MM>:<scope>" with one field
// per instance (a random ID generated at Start) holding the usage that
// instance recorded itself in that window since it started. The usage of a
// scope is the sum of all fields. Because a write replaces the instance's
// field with its current total, a repeated write changes nothing and a lost
// one is corrected by the next: there is nothing to reconcile after an
// outage. Counters expire 24 hours after their day and 48 hours after their
// month ended.
//
// Add never talks to Redis. It updates two local counters: the shadow counter
// (usage persisted in the database when this instance started, plus its own)
// and the own counter (only what this instance recorded). A background loop
// writes the changed own totals every 250ms in one pipeline, and all own
// totals of the current windows every 30s, after Redis recovered, and on
// EvictStale, which restores them if Redis lost data. Stop writes a final
// time.
//
// Check reads only the counters of budgets that are actually set, summed on
// the server in one round trip, and makes no Redis call for a request without
// budgets. Each budget is evaluated against the larger of the shared sum and
// the shadow value. When Redis fails, the check is decided on the shadow
// counter alone: neither fail-open nor fail-closed. While degraded, one
// operation per second probes Redis; the transitions to degraded and back are
// each logged once and failures are counted in
// voidllm_budget_backend_errors_total.
//
// What the shared sum does not contain:
//   - Usage an instance recorded in the last flush interval before it crashed
//     is missing from the other instances' sums. It is in the database (after
//     the usage flush) and therefore in the shadow of every instance started
//     later.
//   - The field of a stopped instance stays until its bucket expires. That is
//     intended: its usage still counts. A hash therefore grows by one field
//     per instance start within its window.
//   - Usage that is only in the database is not in any field: usage from
//     before shared budgets were first used in the current window, and the
//     usage of stopped instances after Redis lost its data. An instance knows
//     such usage only through its shadow, as of its own start. A budget is
//     then enforced on max(sum of the live reports, own shadow), which can be
//     below the true total until the window ends or the instances restart.
//
// The zero value is not usable; construct via NewRedisBudgetCounter and call
// Start before use.
type RedisBudgetCounter struct {
	client *voidredis.Client
	seeder UsageSeeder
	log    *slog.Logger
	shadow *MemoryBudgetCounter
	own    *MemoryBudgetCounter
	now    func() time.Time

	flushInterval  time.Duration
	resyncInterval time.Duration
	instanceID     string

	mu    sync.Mutex // guards dirty
	dirty map[dirtyKey]dirtyEntry
	// flushMu serializes flushes, so that an older total can never be
	// written after a newer one.
	flushMu sync.Mutex

	resync      chan struct{}
	stop        chan struct{}
	done        chan struct{}
	cancel      context.CancelFunc
	startCalled atomic.Bool
	started     atomic.Bool
	stopped     atomic.Bool

	degraded  atomic.Bool
	nextProbe atomic.Int64 // UnixNano; meaningful while degraded
	buckets   atomic.Pointer[budgetBuckets]
}

// NewRedisBudgetCounter returns a RedisBudgetCounter that shares usage
// through client's Redis and seeds its shadow counter from seeder.
func NewRedisBudgetCounter(client *voidredis.Client, seeder UsageSeeder, log *slog.Logger) *RedisBudgetCounter {
	return &RedisBudgetCounter{
		client:         client,
		seeder:         seeder,
		log:            log,
		shadow:         NewMemoryBudgetCounter(),
		own:            NewMemoryBudgetCounter(),
		now:            time.Now,
		flushInterval:  budgetFlushInterval,
		resyncInterval: budgetResyncInterval,
		dirty:          make(map[dirtyKey]dirtyEntry),
		resync:         make(chan struct{}, 1),
		stop:           make(chan struct{}),
		done:           make(chan struct{}),
	}
}

// Start gives the instance its ID, seeds the shadow counter from the database
// and launches the loop that writes totals to Redis. It must be called once,
// before the counter is used; a second call returns an error. Stop shuts the
// loop down.
func (r *RedisBudgetCounter) Start(ctx context.Context) error {
	if !r.startCalled.CompareAndSwap(false, true) {
		return errBudgetCounterStarted
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return fmt.Errorf("generate budget instance id: %w", err)
	}
	r.instanceID = id.String()
	if err := r.shadow.Seed(ctx, r.seeder); err != nil {
		return fmt.Errorf("seed shadow budget counter: %w", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.started.Store(true)
	go r.run(runCtx)
	return nil
}

// Stop writes the final totals to Redis and stops the loop. It blocks until
// the loop has exited; when ctx ends first, the write is abandoned. Usage
// recorded after Stop only reaches the local counters, so Stop must be called
// after everything that records usage has stopped and before the Redis client
// is closed.
func (r *RedisBudgetCounter) Stop(ctx context.Context) {
	if !r.started.Load() || !r.stopped.CompareAndSwap(false, true) {
		return
	}
	close(r.stop)
	select {
	case <-r.done:
	case <-ctx.Done():
		r.log.LogAttrs(ctx, slog.LevelWarn, "budget counter final flush deadline exceeded, abandoning it")
		r.cancel()
		<-r.done
	}
	r.cancel()
}

// Check implements BudgetCounter.
func (r *RedisBudgetCounter) Check(ctx context.Context, s Scopes, l ScopeLimits) error {
	var checkBuf [maxBudgetChecks]budgetCheck
	checks := appendBudgetChecks(checkBuf[:0], s, l)
	if len(checks) == 0 {
		return nil
	}
	w := windowsAt(r.now())
	var spentBuf [maxBudgetChecks]int64
	spent := r.shadow.spent(spentBuf[:0], checks, w)

	if r.backendAllowed(w.now) {
		b := r.bucketsFor(w)
		keys := make([]string, len(checks))
		for i, c := range checks {
			bucket := b.day
			if c.monthly {
				bucket = b.month
			}
			keys[i] = r.client.BudgetKey(voidredis.BudgetMetricTokens, bucket, c.scope)
		}
		shared, err := r.sumBudgets(ctx, keys)
		if err != nil {
			r.backendError(ctx, budgetOpCheck, err)
		} else {
			r.backendOK(ctx)
			for i, v := range shared {
				spent[i] = max(spent[i], v)
			}
		}
	}
	return evaluateBudget(checks, spent, w)
}

// Add implements BudgetCounter.
func (r *RedisBudgetCounter) Add(_ context.Context, s Scopes, u Usage) {
	u = u.clamped()
	if u.Tokens == 0 && u.CostMicros == 0 {
		return
	}
	w := windowsAt(r.now())
	r.shadow.addAt(w, s, u)
	r.own.addAt(w, s, u)
	if r.stopped.Load() {
		return
	}

	dayStart := w.dayStart.Unix()
	r.mu.Lock()
	forEachScope(s, func(scope string) {
		k := dirtyKey{scope: scope, dayStart: dayStart}
		e := r.dirty[k]
		e.at = w
		e.raise(r.own.usage(scope, w))
		r.dirty[k] = e
	})
	r.mu.Unlock()
}

// Snapshot implements BudgetCounter. Like Check it reports the larger of the
// shared sum and the shadow value and falls back to the shadow counter when
// Redis fails, so it never returns an error.
func (r *RedisBudgetCounter) Snapshot(ctx context.Context, scopeKeys []string) (map[string]WindowUsage, error) {
	w := windowsAt(r.now())
	out := r.shadow.snapshotAt(w, scopeKeys)
	if len(scopeKeys) == 0 || !r.backendAllowed(w.now) {
		return out, nil
	}

	b := r.bucketsFor(w)
	keys := make([]string, 0, 4*len(scopeKeys))
	for _, scope := range scopeKeys {
		keys = append(keys,
			r.client.BudgetKey(voidredis.BudgetMetricTokens, b.day, scope),
			r.client.BudgetKey(voidredis.BudgetMetricTokens, b.month, scope),
			r.client.BudgetKey(voidredis.BudgetMetricCost, b.day, scope),
			r.client.BudgetKey(voidredis.BudgetMetricCost, b.month, scope),
		)
	}
	shared, err := r.sumBudgets(ctx, keys)
	if err != nil {
		r.backendError(ctx, budgetOpSnapshot, err)
		return out, nil
	}
	r.backendOK(ctx)
	for i, scope := range scopeKeys {
		u, v := out[scope], shared[4*i:4*i+4]
		u.Daily.Tokens = max(u.Daily.Tokens, v[0])
		u.Monthly.Tokens = max(u.Monthly.Tokens, v[1])
		u.Daily.CostMicros = max(u.Daily.CostMicros, v[2])
		u.Monthly.CostMicros = max(u.Monthly.CostMicros, v[3])
		out[scope] = u
	}
	return out, nil
}

// EvictStale implements BudgetCounter. It evicts expired windows from the
// local counters (Redis counters expire on their own) and has all totals of
// the current windows written to Redis again.
func (r *RedisBudgetCounter) EvictStale() {
	r.shadow.EvictStale()
	r.own.EvictStale()
	r.requestResync()
}

// sumBudgets reads counter sums with the read timeout.
func (r *RedisBudgetCounter) sumBudgets(ctx context.Context, keys []string) ([]int64, error) {
	opCtx, cancel := context.WithTimeout(ctx, budgetReadTimeout)
	defer cancel()
	return r.client.SumBudgets(opCtx, keys)
}

// bucketsFor returns the bucket names and expiries for the windows w. The
// value for the most recently used day is cached, so the request path does
// not format dates.
func (r *RedisBudgetCounter) bucketsFor(w budgetWindows) *budgetBuckets {
	if b := r.buckets.Load(); b != nil && b.dayStart == w.dayStart.Unix() {
		return b
	}
	b := bucketsOf(w)
	r.buckets.Store(b)
	return b
}

// backendAllowed reports whether an operation may go to Redis. While the
// counter is degraded only one caller per budgetProbeInterval gets through.
func (r *RedisBudgetCounter) backendAllowed(now time.Time) bool {
	if !r.degraded.Load() {
		return true
	}
	next := r.nextProbe.Load()
	if now.UnixNano() < next {
		return false
	}
	return r.nextProbe.CompareAndSwap(next, now.Add(budgetProbeInterval).UnixNano())
}

// backendError records a failed Redis operation and switches the counter to
// degraded, logging the transition once. A failure caused by ctx itself
// ending (client gone, shutdown) says nothing about Redis and is ignored.
func (r *RedisBudgetCounter) backendError(ctx context.Context, op string, err error) {
	if ctx.Err() != nil {
		return
	}
	metrics.BudgetBackendErrorsTotal.WithLabelValues(op).Inc()
	if r.degraded.CompareAndSwap(false, true) {
		r.nextProbe.Store(r.now().Add(budgetProbeInterval).UnixNano())
		r.log.LogAttrs(ctx, slog.LevelWarn, "budget backend degraded, enforcing budgets with local counters",
			slog.String("op", op),
			slog.String("error", err.Error()),
		)
	}
}

// backendOK records a successful Redis operation. When the counter was
// degraded it logs the recovery once and has all totals written again,
// because Redis missed the writes of the outage and may have come back empty.
func (r *RedisBudgetCounter) backendOK(ctx context.Context) {
	if !r.degraded.Load() || !r.degraded.CompareAndSwap(true, false) {
		return
	}
	r.log.LogAttrs(ctx, slog.LevelWarn, "budget backend recovered, enforcing budgets with shared counters")
	r.requestResync()
}

// requestResync asks the loop to write all totals of the current windows.
// Requests coalesce while one is pending.
func (r *RedisBudgetCounter) requestResync() {
	select {
	case r.resync <- struct{}{}:
	default:
	}
}

// run is the write loop. Every operation derives from ctx, which Stop cancels
// when its own deadline passes.
func (r *RedisBudgetCounter) run(ctx context.Context) {
	defer close(r.done)

	flushTicker := time.NewTicker(r.flushInterval)
	defer flushTicker.Stop()
	resyncTicker := time.NewTicker(r.resyncInterval)
	defer resyncTicker.Stop()

	for {
		select {
		case <-flushTicker.C:
			r.flush(ctx, false)
		case <-resyncTicker.C:
			r.markAllDirty()
		case <-r.resync:
			r.markAllDirty()
			r.flush(ctx, false)
		case <-r.stop:
			r.flush(ctx, true)
			return
		}
	}
}

// markAllDirty schedules every own total of the current windows for writing.
func (r *RedisBudgetCounter) markAllDirty() {
	w := windowsAt(r.now())
	dayStart := w.dayStart.Unix()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.own.entries.Range(func(key, _ any) bool {
		scope := key.(string)
		u := r.own.usage(scope, w)
		if u == (WindowUsage{}) {
			return true
		}
		k := dirtyKey{scope: scope, dayStart: dayStart}
		e := r.dirty[k]
		e.at = w
		e.raise(u)
		r.dirty[k] = e
		return true
	})
}

// flush writes the totals scheduled since the last flush to Redis, in
// pipelines of at most budgetWriteChunk values (one pipeline unless a resync
// is due on a large installation). Totals that could not be written are
// scheduled again. While the counter is degraded a flush only goes out when
// it is its turn to probe, unless force is set.
func (r *RedisBudgetCounter) flush(ctx context.Context, force bool) {
	r.flushMu.Lock()
	defer r.flushMu.Unlock()

	r.mu.Lock()
	batch := r.dirty
	if len(batch) == 0 {
		r.mu.Unlock()
		return
	}
	r.dirty = make(map[dirtyKey]dirtyEntry, len(batch))
	r.mu.Unlock()

	now := r.now()
	if !force && !r.backendAllowed(now) {
		r.reschedule(batch, now)
		return
	}

	values := make([]voidredis.BudgetValue, 0, 2*len(batch))
	for k, e := range batch {
		// Write the newest totals of the entry's windows that are still
		// live. Without this, the entry of a finished day could write an
		// older monthly total over a newer one of the same month.
		e.raise(r.own.usage(k.scope, e.at))
		batch[k] = e
		values = r.appendValues(values, k.scope, e, now)
	}
	for len(values) > 0 {
		chunk := values[:min(len(values), budgetWriteChunk)]
		values = values[len(chunk):]

		opCtx, cancel := context.WithTimeout(ctx, budgetWriteTimeout)
		err := r.client.SetBudgets(opCtx, r.instanceID, chunk)
		cancel()
		if err != nil {
			r.backendError(ctx, budgetOpFlush, err)
			// Absolute values: writing the whole batch again is harmless.
			r.reschedule(batch, now)
			return
		}
		r.backendOK(ctx)
	}
}

// appendValues appends the Redis values for the totals of one scope. Zero
// totals are skipped, and so are the totals of a window whose counter has
// outlived its grace: nobody reads it any more.
func (r *RedisBudgetCounter) appendValues(dst []voidredis.BudgetValue, scope string, e dirtyEntry, now time.Time) []voidredis.BudgetValue {
	b := bucketsOf(e.at)
	add := func(metric, bucket string, value int64, expireAt time.Time) {
		if value == 0 || !now.Before(expireAt) {
			return
		}
		dst = append(dst, voidredis.BudgetValue{
			Key:      r.client.BudgetKey(metric, bucket, scope),
			Value:    value,
			ExpireAt: expireAt,
		})
	}
	add(voidredis.BudgetMetricTokens, b.day, e.usage.Daily.Tokens, b.dayExpire)
	add(voidredis.BudgetMetricTokens, b.month, e.usage.Monthly.Tokens, b.monthExpire)
	add(voidredis.BudgetMetricCost, b.day, e.usage.Daily.CostMicros, b.dayExpire)
	add(voidredis.BudgetMetricCost, b.month, e.usage.Monthly.CostMicros, b.monthExpire)
	return dst
}

// reschedule puts totals that were not written back for the next flush,
// keeping whatever newer totals were scheduled meanwhile. Entries whose
// windows are both past their grace are dropped, which bounds the schedule
// during a long outage.
func (r *RedisBudgetCounter) reschedule(batch map[dirtyKey]dirtyEntry, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, old := range batch {
		if !now.Before(nextUTCMonth(old.at.monthStart).Add(budgetMonthGrace)) {
			continue
		}
		e, ok := r.dirty[k]
		if !ok {
			e.at = old.at
		}
		e.raise(old.usage)
		r.dirty[k] = e
	}
}
