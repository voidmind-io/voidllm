package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Compile-time assertion: MemoryBudgetCounter must implement BudgetCounter.
var _ BudgetCounter = (*MemoryBudgetCounter)(nil)

// usageWindow accumulates the usage of one scope within one window. start is
// the Unix timestamp of the window start and never changes after creation: a
// new window is a new usageWindow, so a count can never be attributed to the
// wrong window.
type usageWindow struct {
	start  int64
	tokens atomic.Int64
	cost   atomic.Int64
}

// evictedWindow marks a budgetEntry that EvictStale removed from the counter.
// An Add that still holds such an entry must fetch a fresh one.
var evictedWindow = &usageWindow{}

// budgetEntry holds the current daily and monthly window of one scope.
type budgetEntry struct {
	daily   atomic.Pointer[usageWindow]
	monthly atomic.Pointer[usageWindow]
}

// MemoryBudgetCounter is a BudgetCounter that keeps all usage in the memory
// of this process. Counters are incremented when usage is recorded and read
// before each proxy request; both paths are lock-free. Budgets are therefore
// enforced per instance. The zero value is not usable; construct via
// NewMemoryBudgetCounter.
type MemoryBudgetCounter struct {
	entries sync.Map // scope key (string) -> *budgetEntry
	now     func() time.Time
}

// NewMemoryBudgetCounter constructs an empty MemoryBudgetCounter.
func NewMemoryBudgetCounter() *MemoryBudgetCounter {
	return &MemoryBudgetCounter{now: time.Now}
}

// Check implements BudgetCounter.
func (m *MemoryBudgetCounter) Check(_ context.Context, s Scopes, l ScopeLimits) error {
	var checkBuf [maxBudgetChecks]budgetCheck
	checks := appendBudgetChecks(checkBuf[:0], s, l)
	if len(checks) == 0 {
		return nil
	}
	w := windowsAt(m.now())
	var spentBuf [maxBudgetChecks]int64
	spent := m.spent(spentBuf[:0], checks, w)
	return evaluateBudget(checks, spent, w)
}

// Add implements BudgetCounter. It must be called as soon as usage is known so
// that subsequent Check calls see up-to-date totals.
func (m *MemoryBudgetCounter) Add(_ context.Context, s Scopes, u Usage) {
	m.addAt(windowsAt(m.now()), s, u)
}

// Snapshot implements BudgetCounter. It never fails.
func (m *MemoryBudgetCounter) Snapshot(_ context.Context, scopeKeys []string) (map[string]WindowUsage, error) {
	return m.snapshotAt(windowsAt(m.now()), scopeKeys), nil
}

// Seed loads the usage persisted for the current month into the counter, so
// that its state matches what was recorded before this process started. It
// reads the month once; events of the current day also count towards the
// daily windows. It should be called once at startup before the proxy begins
// serving requests.
func (m *MemoryBudgetCounter) Seed(ctx context.Context, seeder UsageSeeder) error {
	w := windowsAt(m.now())
	err := scanUsageSeed(ctx, seeder, w, func(scope string, u Usage, inDay bool) {
		daily := Usage{}
		if inDay {
			daily = u
		}
		m.addScope(scope, w, daily, u)
	})
	if err != nil {
		return fmt.Errorf("seed budget counters: %w", err)
	}
	return nil
}

// EvictStale implements BudgetCounter. It removes the entries of scopes that
// have no usage in the current day or month to reclaim memory.
func (m *MemoryBudgetCounter) EvictStale() {
	w := windowsAt(m.now())
	dayStart, monthStart := w.dayStart.Unix(), w.monthStart.Unix()

	m.entries.Range(func(key, value any) bool {
		e := value.(*budgetEntry)
		monthly := e.monthly.Load()
		if !windowStale(monthly, monthStart) || !windowStale(e.daily.Load(), dayStart) {
			return true
		}
		// Retire the entry through its monthly window, which every Add goes
		// through first: an Add that rolled the window in the meantime makes
		// this swap fail and keeps the entry, and an Add that comes later
		// sees the marker and moves on to a fresh entry. Either way no usage
		// ends up in an entry that is no longer reachable.
		if e.monthly.CompareAndSwap(monthly, evictedWindow) {
			m.entries.CompareAndDelete(key, e)
		}
		return true
	})
}

// windowStale reports whether w holds no usage of the window starting at
// current.
func windowStale(w *usageWindow, current int64) bool {
	return w == nil || w == evictedWindow || w.start < current
}

// addAt records u for every scope in s in the windows w. Negative amounts are
// ignored: usage only ever grows within a window.
func (m *MemoryBudgetCounter) addAt(w budgetWindows, s Scopes, u Usage) {
	u = u.clamped()
	forEachScope(s, func(scope string) {
		m.addScope(scope, w, u, u)
	})
}

// clamped returns u with negative amounts replaced by zero.
func (u Usage) clamped() Usage {
	if u.Tokens < 0 {
		u.Tokens = 0
	}
	if u.CostMicros < 0 {
		u.CostMicros = 0
	}
	return u
}

// addScope adds daily to the daily window and monthly to the monthly window
// of one scope.
func (m *MemoryBudgetCounter) addScope(scope string, w budgetWindows, daily, monthly Usage) {
	dayStart, monthStart := w.dayStart.Unix(), w.monthStart.Unix()
	for {
		e := m.entry(scope)
		// The monthly window comes first: it doubles as the marker for an
		// entry retired by EvictStale (see there).
		if !addToWindow(&e.monthly, monthStart, monthly) {
			m.entries.CompareAndDelete(scope, e)
			continue
		}
		addToWindow(&e.daily, dayStart, daily)
		return
	}
}

// addToWindow adds u to the window starting at start. When p holds an older
// window (or none) a fresh window that already contains u is swapped in with
// a compare-and-swap, so a window is only ever published complete and
// concurrent adds across a rollover cannot lose increments: the loser of the
// swap retries against the winner's window. Usage stamped with a window older
// than the published one arrived after its window ended and is dropped. It
// returns false when the entry was retired by EvictStale.
func addToWindow(p *atomic.Pointer[usageWindow], start int64, u Usage) bool {
	for {
		w := p.Load()
		switch {
		case w == evictedWindow:
			return false
		case w != nil && w.start == start:
			w.tokens.Add(u.Tokens)
			w.cost.Add(u.CostMicros)
			return true
		case w != nil && w.start > start:
			return true
		}
		fresh := &usageWindow{start: start}
		fresh.tokens.Store(u.Tokens)
		fresh.cost.Store(u.CostMicros)
		if p.CompareAndSwap(w, fresh) {
			return true
		}
	}
}

// entry returns the budgetEntry of scope, creating it when missing.
func (m *MemoryBudgetCounter) entry(scope string) *budgetEntry {
	if v, ok := m.entries.Load(scope); ok {
		return v.(*budgetEntry)
	}
	actual, _ := m.entries.LoadOrStore(scope, &budgetEntry{})
	return actual.(*budgetEntry)
}

// usage returns the usage of scope in the windows w. Windows other than the
// current ones count as empty.
func (m *MemoryBudgetCounter) usage(scope string, w budgetWindows) WindowUsage {
	v, ok := m.entries.Load(scope)
	if !ok {
		return WindowUsage{}
	}
	e := v.(*budgetEntry)
	return WindowUsage{
		Daily:   windowUsage(e.daily.Load(), w.dayStart.Unix()),
		Monthly: windowUsage(e.monthly.Load(), w.monthStart.Unix()),
	}
}

// windowUsage returns the usage held by w when it is the window starting at
// start, and zero usage otherwise.
func windowUsage(w *usageWindow, start int64) Usage {
	if w == nil || w == evictedWindow || w.start != start {
		return Usage{}
	}
	return Usage{Tokens: w.tokens.Load(), CostMicros: w.cost.Load()}
}

// spent appends the usage so far for each of checks to dst, in order: tokens
// for token budgets and cost in micro-units for cost budgets.
func (m *MemoryBudgetCounter) spent(dst []int64, checks []budgetCheck, w budgetWindows) []int64 {
	for _, c := range checks {
		dst = append(dst, c.spentOf(m.usage(c.scope, w)))
	}
	return dst
}

// snapshotAt returns the usage of each of scopeKeys in the windows w.
func (m *MemoryBudgetCounter) snapshotAt(w budgetWindows, scopeKeys []string) map[string]WindowUsage {
	out := make(map[string]WindowUsage, len(scopeKeys))
	for _, scope := range scopeKeys {
		out[scope] = m.usage(scope, w)
	}
	return out
}
