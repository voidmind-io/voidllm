package ratelimit

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/voidmind-io/voidllm/internal/metrics"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
)

// Compile-time assertion: RedisBudgetCounter must implement BudgetCounter.
var _ BudgetCounter = (*RedisBudgetCounter)(nil)

const (
	// budgetQueueSize bounds the increments waiting to be written to Redis.
	budgetQueueSize = 8192
	// budgetOpTimeout bounds every single Redis round trip of the counter.
	budgetOpTimeout = 500 * time.Millisecond
	// budgetProbeInterval is how often a degraded counter lets one check try
	// Redis again; all other checks use the local counters meanwhile.
	budgetProbeInterval = time.Second
	// budgetSeedClaimTTL is how long a rehydration claim is held. It must
	// exceed budgetSeedTimeout so that a claim cannot expire under a
	// rehydration that is still running.
	budgetSeedClaimTTL = 2 * time.Minute
	// budgetSeedTimeout bounds one rehydration (database read and Redis
	// write).
	budgetSeedTimeout = 90 * time.Second
	// budgetSeedRetry is the pause before a bucket that could not be
	// confirmed as rehydrated is looked at again.
	budgetSeedRetry = 30 * time.Second
	// budgetDropWarnInterval limits the warning about dropped increments.
	budgetDropWarnInterval = time.Minute
	// budgetDayGrace and budgetMonthGrace are how long daily and monthly
	// counters outlive their window before Redis evicts them.
	budgetDayGrace   = 24 * time.Hour
	budgetMonthGrace = 48 * time.Hour
)

// Values of the "op" label of metrics.BudgetBackendErrorsTotal.
const (
	budgetOpCheck     = "check"
	budgetOpFlush     = "flush"
	budgetOpSeed      = "seed"
	budgetOpQueueFull = "queue_full"
)

// budgetOp is one recorded usage waiting to be written to Redis.
type budgetOp struct {
	scopes Scopes
	usage  Usage
	at     budgetWindows
}

// budgetBuckets holds the Redis bucket names and counter expiries of one UTC
// day and the month it belongs to.
type budgetBuckets struct {
	dayStart    int64
	day         string
	month       string
	dayExpire   time.Time
	monthExpire time.Time
}

// bucketSeed is the rehydration state of one bucket as known to the worker.
type bucketSeed struct {
	bucket    string
	confirmed bool
	retryAt   time.Time
}

// RedisBudgetCounter is a BudgetCounter whose counters live in Redis, so that
// all VoidLLM instances sharing that Redis enforce one budget together.
//
// Counters are plain integers under
// "<prefix>budget:<tok|cost>:<d:YYYY-MM-DD|m:YYYY-MM>:<scope>". They expire
// 24 hours after their day and 48 hours after their month ended.
//
// Add never talks to Redis on the caller's goroutine. It updates a local
// shadow MemoryBudgetCounter synchronously and hands the increment to a
// bounded queue that a single worker writes to Redis (INCRBY + EXPIREAT in
// one pipeline, 500ms per round trip). When the queue is full the increment
// is dropped from Redis and counted in voidllm_budget_backend_errors_total;
// it stays in the shadow counter and in the database. The same holds for an
// increment whose write fails.
//
// Check reads only the counters of budgets that are actually set, in one
// MGET, and makes no Redis call for a request without budgets. Each budget is
// evaluated against the larger of the shared and the shadow value, so this
// instance never enforces less than its own view. When Redis fails, the check
// is decided on the shadow counter alone (the usage persisted when this
// instance started plus everything it recorded itself): neither fail-open nor
// fail-closed. While degraded, one check per second probes Redis; the
// transitions to degraded and back are each logged once.
//
// Rehydration: a bucket whose seed marker ("<prefix>budget:seeded:<bucket>")
// is missing has never been filled, because Redis budgets were just enabled,
// a new day or month began, or Redis lost its data. The instance that wins
// the marker with SET NX adds the usage persisted in the database for that
// bucket and marks it done in the same transaction. Markers are re-checked at
// startup, when a new bucket begins, on every EvictStale and after Redis
// recovered. Usage of the bucket that had reached the database when the
// rehydration read it, and that instances also add to the bucket themselves,
// is counted twice. That errs on the strict side and is bounded by what was
// recorded between the start of the bucket (or the loss of the Redis data)
// and its rehydration: at most about one usage flush interval at a window
// rollover and on first use, and up to the marker re-check interval when
// Redis lost its data without the connection failing.
//
// The zero value is not usable; construct via NewRedisBudgetCounter and call
// Start before use.
type RedisBudgetCounter struct {
	client *voidredis.Client
	seeder UsageSeeder
	log    *slog.Logger
	shadow *MemoryBudgetCounter
	now    func() time.Time

	queue   chan budgetOp
	recheck chan struct{}
	stop    chan struct{}
	done    chan struct{}
	cancel  context.CancelFunc
	started atomic.Bool
	stopped atomic.Bool

	degraded     atomic.Bool
	nextProbe    atomic.Int64 // UnixNano; meaningful while degraded
	lastDropWarn atomic.Int64 // UnixNano
	buckets      atomic.Pointer[budgetBuckets]
}

// NewRedisBudgetCounter returns a RedisBudgetCounter that keeps its counters
// in client's Redis and rehydrates them from seeder.
func NewRedisBudgetCounter(client *voidredis.Client, seeder UsageSeeder, log *slog.Logger) *RedisBudgetCounter {
	return &RedisBudgetCounter{
		client:  client,
		seeder:  seeder,
		log:     log,
		shadow:  NewMemoryBudgetCounter(),
		now:     time.Now,
		queue:   make(chan budgetOp, budgetQueueSize),
		recheck: make(chan struct{}, 1),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
}

// Start seeds the shadow counter from the database and launches the worker
// that writes increments to Redis and rehydrates buckets. It must be called
// exactly once, before the counter is used. Stop shuts the worker down.
func (r *RedisBudgetCounter) Start(ctx context.Context) error {
	if err := r.shadow.Seed(ctx, r.seeder); err != nil {
		return fmt.Errorf("seed shadow budget counter: %w", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.started.Store(true)
	go r.run(runCtx)
	return nil
}

// Stop writes the queued increments to Redis and stops the worker. It blocks
// until the worker has exited; when ctx ends first, the remaining work is
// abandoned. Usage recorded after Stop only reaches the shadow counter. Stop
// must be called before the Redis client is closed.
func (r *RedisBudgetCounter) Stop(ctx context.Context) {
	if !r.started.Load() || !r.stopped.CompareAndSwap(false, true) {
		return
	}
	close(r.stop)
	select {
	case <-r.done:
	case <-ctx.Done():
		r.log.LogAttrs(ctx, slog.LevelWarn, "budget counter drain deadline exceeded, abandoning queued increments",
			slog.Int("queued", len(r.queue)),
		)
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
		shared, err := r.getBudgets(ctx, keys)
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
func (r *RedisBudgetCounter) Add(ctx context.Context, s Scopes, u Usage) {
	u = u.clamped()
	w := windowsAt(r.now())
	r.shadow.addAt(w, s, u)
	if (u.Tokens == 0 && u.CostMicros == 0) || r.stopped.Load() {
		return
	}
	select {
	case r.queue <- budgetOp{scopes: s, usage: u, at: w}:
	default:
		metrics.BudgetBackendErrorsTotal.WithLabelValues(budgetOpQueueFull).Inc()
		last := r.lastDropWarn.Load()
		if w.now.UnixNano()-last >= int64(budgetDropWarnInterval) && r.lastDropWarn.CompareAndSwap(last, w.now.UnixNano()) {
			r.log.LogAttrs(ctx, slog.LevelWarn, "budget counter queue full, usage not added to shared counters")
		}
	}
}

// Snapshot implements BudgetCounter. Like Check it reports the larger of the
// shared and the shadow value and falls back to the shadow counter when Redis
// fails, so it never returns an error.
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
	shared, err := r.getBudgets(ctx, keys)
	if err != nil {
		r.backendError(ctx, budgetOpCheck, err)
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
// shadow counter (Redis counters expire on their own) and asks the worker to
// verify the seed markers of the current buckets again.
func (r *RedisBudgetCounter) EvictStale() {
	r.shadow.EvictStale()
	r.requestRecheck()
}

// getBudgets reads counters with the per-operation timeout.
func (r *RedisBudgetCounter) getBudgets(ctx context.Context, keys []string) ([]int64, error) {
	opCtx, cancel := context.WithTimeout(ctx, budgetOpTimeout)
	defer cancel()
	return r.client.GetBudgets(opCtx, keys)
}

// bucketsFor returns the bucket names and expiries for the windows w. The
// value for the most recently used day is cached, so the request path does
// not format dates.
func (r *RedisBudgetCounter) bucketsFor(w budgetWindows) *budgetBuckets {
	dayStart := w.dayStart.Unix()
	if b := r.buckets.Load(); b != nil && b.dayStart == dayStart {
		return b
	}
	b := &budgetBuckets{
		dayStart:    dayStart,
		day:         voidredis.BudgetDayBucket(w.dayStart),
		month:       voidredis.BudgetMonthBucket(w.monthStart),
		dayExpire:   nextUTCMidnight(w.dayStart).Add(budgetDayGrace),
		monthExpire: nextUTCMonth(w.monthStart).Add(budgetMonthGrace),
	}
	r.buckets.Store(b)
	return b
}

// backendAllowed reports whether a read may go to Redis. While the counter is
// degraded only one caller per budgetProbeInterval gets through.
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
// degraded it logs the recovery once and has the seed markers verified again,
// because Redis may have come back empty.
func (r *RedisBudgetCounter) backendOK(ctx context.Context) {
	if !r.degraded.Load() || !r.degraded.CompareAndSwap(true, false) {
		return
	}
	r.log.LogAttrs(ctx, slog.LevelWarn, "budget backend recovered, enforcing budgets with shared counters")
	r.requestRecheck()
}

// requestRecheck asks the worker to verify the seed markers of the current
// buckets. Requests coalesce while one is pending.
func (r *RedisBudgetCounter) requestRecheck() {
	select {
	case r.recheck <- struct{}{}:
	default:
	}
}

// run is the worker loop. It rehydrates the current buckets when needed and
// writes queued increments to Redis until Stop is called, then drains the
// queue. Every operation derives from ctx, which Stop cancels when its own
// deadline passes.
func (r *RedisBudgetCounter) run(ctx context.Context) {
	defer close(r.done)

	var day, month bucketSeed
	r.ensureSeeded(ctx, &day, &month)
	for {
		select {
		case op := <-r.queue:
			r.ensureSeeded(ctx, &day, &month)
			r.flush(ctx, op)
		case <-r.recheck:
			day, month = bucketSeed{}, bucketSeed{}
			r.ensureSeeded(ctx, &day, &month)
		case <-r.stop:
			for ctx.Err() == nil {
				select {
				case op := <-r.queue:
					r.flush(ctx, op)
				default:
					return
				}
			}
			return
		}
	}
}

// flush writes one recorded usage to the Redis counters of all its scopes.
// Zero amounts are skipped.
func (r *RedisBudgetCounter) flush(ctx context.Context, op budgetOp) {
	b := r.bucketsFor(op.at)
	incs := make([]voidredis.BudgetIncrement, 0, 16)
	forEachScope(op.scopes, func(scope string) {
		incs = r.appendIncrements(incs, b, scope, op.usage, op.usage)
	})

	opCtx, cancel := context.WithTimeout(ctx, budgetOpTimeout)
	defer cancel()
	if err := r.client.IncrBudgets(opCtx, incs); err != nil {
		r.backendError(ctx, budgetOpFlush, err)
		return
	}
	r.backendOK(ctx)
}

// appendIncrements appends the increments that add daily to the day bucket
// and monthly to the month bucket of one scope, skipping zero amounts.
func (r *RedisBudgetCounter) appendIncrements(dst []voidredis.BudgetIncrement, b *budgetBuckets, scope string, daily, monthly Usage) []voidredis.BudgetIncrement {
	add := func(metric, bucket string, delta int64, expireAt time.Time) {
		if delta == 0 {
			return
		}
		dst = append(dst, voidredis.BudgetIncrement{
			Key:      r.client.BudgetKey(metric, bucket, scope),
			Delta:    delta,
			ExpireAt: expireAt,
		})
	}
	add(voidredis.BudgetMetricTokens, b.day, daily.Tokens, b.dayExpire)
	add(voidredis.BudgetMetricTokens, b.month, monthly.Tokens, b.monthExpire)
	add(voidredis.BudgetMetricCost, b.day, daily.CostMicros, b.dayExpire)
	add(voidredis.BudgetMetricCost, b.month, monthly.CostMicros, b.monthExpire)
	return dst
}

// ensureSeeded makes sure the current day and month buckets are rehydrated,
// or known to be, tracking what it learned in day and month.
func (r *RedisBudgetCounter) ensureSeeded(ctx context.Context, day, month *bucketSeed) {
	w := windowsAt(r.now())
	b := r.bucketsFor(w)
	r.ensureBucket(ctx, day, b.day, w, false)
	r.ensureBucket(ctx, month, b.month, w, true)
}

// ensureBucket rehydrates one bucket unless state already confirms it, or a
// recent attempt asked to wait.
func (r *RedisBudgetCounter) ensureBucket(ctx context.Context, state *bucketSeed, bucket string, w budgetWindows, monthly bool) {
	if state.bucket != bucket {
		*state = bucketSeed{bucket: bucket}
	}
	if state.confirmed || w.now.Before(state.retryAt) {
		return
	}
	state.confirmed = r.seedBucket(ctx, bucket, w, monthly)
	if !state.confirmed {
		state.retryAt = w.now.Add(budgetSeedRetry)
	}
}

// seedBucket claims the seed marker of a bucket and, when it wins, adds the
// usage persisted in the database for that bucket to the Redis counters. It
// reports whether the bucket is now known to be rehydrated, by this instance
// or by another one. It reports false while another instance holds the claim
// and when anything fails; an abandoned claim expires on its own.
func (r *RedisBudgetCounter) seedBucket(ctx context.Context, bucket string, w budgetWindows, monthly bool) bool {
	seedCtx, cancel := context.WithTimeout(ctx, budgetSeedTimeout)
	defer cancel()

	id, err := uuid.NewRandom()
	if err != nil {
		r.seedFailed(ctx, bucket, fmt.Errorf("generate claim token: %w", err))
		return false
	}
	token := "pending:" + id.String()

	claimed, err := r.claimSeed(seedCtx, bucket, token)
	if err != nil {
		r.backendError(ctx, budgetOpSeed, err)
		return false
	}
	if !claimed {
		done, err := r.seedDone(seedCtx, bucket)
		if err != nil {
			r.backendError(ctx, budgetOpSeed, err)
			return false
		}
		return done
	}

	b := r.bucketsFor(w)
	since, expireAt := w.dayStart, b.dayExpire
	if monthly {
		since, expireAt = w.monthStart, b.monthExpire
	}

	totals := make(map[string]Usage)
	err = scanUsageSeed(seedCtx, r.seeder, since, func(scope string, u Usage) {
		t := totals[scope]
		t.Tokens += u.Tokens
		t.CostMicros += u.CostMicros
		totals[scope] = t
	})
	if err != nil {
		r.seedFailed(ctx, bucket, err)
		return false
	}

	incs := make([]voidredis.BudgetIncrement, 0, len(totals))
	for scope, t := range totals {
		if monthly {
			incs = r.appendIncrements(incs, b, scope, Usage{}, t)
		} else {
			incs = r.appendIncrements(incs, b, scope, t, Usage{})
		}
	}

	committed, err := r.client.CommitBudgetSeed(seedCtx, bucket, token, incs, expireAt)
	if err != nil {
		r.backendError(ctx, budgetOpSeed, err)
		return false
	}
	if !committed {
		r.log.LogAttrs(ctx, slog.LevelWarn, "budget rehydration claim lost, leaving the bucket to its new owner",
			slog.String("bucket", bucket),
		)
		return false
	}
	r.backendOK(ctx)
	r.log.LogAttrs(ctx, slog.LevelInfo, "budget counters rehydrated from database",
		slog.String("bucket", bucket),
		slog.Int("scopes", len(totals)),
	)
	return true
}

// claimSeed claims the seed marker with the per-operation timeout.
func (r *RedisBudgetCounter) claimSeed(ctx context.Context, bucket, token string) (bool, error) {
	opCtx, cancel := context.WithTimeout(ctx, budgetOpTimeout)
	defer cancel()
	return r.client.ClaimBudgetSeed(opCtx, bucket, token, budgetSeedClaimTTL)
}

// seedDone reads the seed marker with the per-operation timeout.
func (r *RedisBudgetCounter) seedDone(ctx context.Context, bucket string) (bool, error) {
	opCtx, cancel := context.WithTimeout(ctx, budgetOpTimeout)
	defer cancel()
	return r.client.BudgetSeedDone(opCtx, bucket)
}

// seedFailed records a rehydration that failed for a reason other than Redis
// itself, such as the database read.
func (r *RedisBudgetCounter) seedFailed(ctx context.Context, bucket string, err error) {
	if ctx.Err() != nil {
		return
	}
	metrics.BudgetBackendErrorsTotal.WithLabelValues(budgetOpSeed).Inc()
	r.log.LogAttrs(ctx, slog.LevelError, "budget rehydration failed",
		slog.String("bucket", bucket),
		slog.String("error", err.Error()),
	)
}
