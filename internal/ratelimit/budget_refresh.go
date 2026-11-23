package ratelimit

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// refreshSliceLength is the granularity at which an instance remembers
	// when it recorded its own usage. It adds to how much own usage a
	// refreshed view can count twice, see refreshedUsage.
	refreshSliceLength = 5 * time.Second
	// refreshFreezeLag is how old usage events must be before a refresh
	// stops re-reading them. Events are stamped when they happen and
	// inserted when the usage logger flushes, so recent ranges can still
	// gain rows; a range older than this is taken to be complete.
	refreshFreezeLag = 5 * time.Minute
	// refreshSliceMaxAge bounds how long own usage is remembered while
	// refreshes fail. Usage that old has long been persisted or lost.
	refreshSliceMaxAge = 2 * time.Hour
)

// refreshedView is the result of one refresh: the usage the database held at
// the refresh, for the windows of that moment, and the own usage the database
// did not hold yet.
type refreshedView struct {
	at      budgetWindows
	base    map[string]WindowUsage // read-only after creation
	pending *MemoryBudgetCounter
}

// refreshedUsage is an instance's view of the usage persisted in the database
// by all instances, refreshed periodically, plus its own usage that has not
// been persisted yet. It closes the gap that neither the shared Redis sums
// nor the shadow counter cover: usage that only the database knows about and
// that was recorded after this instance started, for example by instances
// that reported to a Redis that has since lost its data, or by instances
// running before shared budgets were in use.
//
// A refresh at time T reads the totals of the current month and day from the
// usage events, aggregated by the database. From then on the view of a scope
// is
//
//	database total at T + own usage recorded after T - allowance
//
// where allowance is how long the usage logger may take to persist an event.
// Own usage older than that is taken to be part of the database total; own
// usage younger than that is added on top, because the database may not hold
// it yet. The part of it that the database did hold already is counted twice
// until the next refresh. That errs on the strict side and is bounded by the
// usage this instance itself recorded within allowance + refreshSliceLength
// before a refresh. In the other direction the view lags the true total by at
// most the refresh interval plus the time the usage logger needs to persist
// an event.
//
// To keep a refresh cheap the month is aggregated in full only by the first
// refresh and the first one of each day. Every other refresh adds the events
// of the range that became older than refreshFreezeLag since the previous one
// to a frozen total, and reads only the events younger than that again. The
// frozen range always ends refreshFreezeLag before the refresh, also right
// after midnight. An event inserted more than refreshFreezeLag after it
// happened is therefore missing until the next full aggregation.
//
// While refreshes fail, the previous view stays in use and keeps growing by
// own usage. The record of when own usage happened is kept across failures
// (up to refreshSliceMaxAge), so that the first refresh that succeeds again
// can tell what the database already holds.
//
// Before the first refresh the view is empty, and a view never has to be
// trusted alone: callers combine it with their other counters by taking the
// larger value.
type refreshedUsage struct {
	now       func() time.Time
	allowance time.Duration

	// refreshMu serializes refreshes and guards the frozen state.
	refreshMu   sync.Mutex
	frozen      map[string]WindowUsage // events before frozenUntil
	frozenUntil time.Time
	frozenAt    budgetWindows // windows frozen was aggregated for

	// mu orders recording against the swap of a refresh: recording holds it
	// shared, the swap exclusively, so that every recorded usage is either in
	// the slices the new pending counter is built from or added to it
	// afterwards, never neither and never both.
	mu     sync.RWMutex
	slices sync.Map // slice index (int64) -> *MemoryBudgetCounter
	view   atomic.Pointer[refreshedView]
}

// newRefreshedUsage returns an empty view. allowance must cover the usage
// logger's flush interval and the time its database write takes.
func newRefreshedUsage(now func() time.Time, allowance time.Duration) *refreshedUsage {
	return &refreshedUsage{now: now, allowance: allowance}
}

// sliceIndex returns the index of the time slice containing t.
func sliceIndex(t time.Time) int64 {
	return t.UnixNano() / int64(refreshSliceLength)
}

// record remembers own usage recorded at w.now.
func (r *refreshedUsage) record(w budgetWindows, s Scopes, u Usage) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	idx := sliceIndex(w.now)
	v, ok := r.slices.Load(idx)
	if !ok {
		v, _ = r.slices.LoadOrStore(idx, NewMemoryBudgetCounter())
	}
	v.(*MemoryBudgetCounter).addAt(w, s, u)

	if view := r.view.Load(); view != nil {
		view.pending.addAt(w, s, u)
	}
}

// refresh reads the persisted totals and replaces the view with them. On
// failure the previous view stays in place and keeps growing by own usage.
// expired is the number of time slices of own usage that were given up
// because refreshes have been failing for longer than refreshSliceMaxAge.
func (r *refreshedUsage) refresh(ctx context.Context, source UsageSeeder) (expired int, err error) {
	r.refreshMu.Lock()
	defer r.refreshMu.Unlock()

	at := windowsAt(r.now())
	base, err := r.readTotals(ctx, source, at)
	if err != nil {
		return r.expireSlices(at.now), err
	}
	cutoff := sliceIndex(at.now.Add(-r.allowance))

	r.mu.Lock()
	defer r.mu.Unlock()
	pending := NewMemoryBudgetCounter()
	current := windowsAt(r.now())
	r.slices.Range(func(key, value any) bool {
		// A slice is kept while any part of it is younger than the
		// allowance, so the slice containing the cutoff stays.
		if key.(int64) < cutoff {
			r.slices.Delete(key)
			return true
		}
		mergeCounter(pending, value.(*MemoryBudgetCounter), current)
		return true
	})
	r.view.Store(&refreshedView{at: at, base: base, pending: pending})
	return 0, nil
}

// readTotals returns the persisted totals of the windows at, per counter
// scope. It advances the frozen state only when every read succeeded. The
// caller holds refreshMu.
func (r *refreshedUsage) readTotals(ctx context.Context, source UsageSeeder, at budgetWindows) (map[string]WindowUsage, error) {
	// The frozen range ends refreshFreezeLag ago in every case, also right
	// after midnight: events of the last minutes of a day may still be
	// inserted then and must stay in the part that is read again. The split
	// between day and month is made by the query for either part. Only the
	// start of the month bounds it, because nothing before counts.
	freezeUntil := at.now.Add(-refreshFreezeLag)
	if freezeUntil.Before(at.monthStart) {
		freezeUntil = at.monthStart
	}

	var frozen map[string]WindowUsage
	sameWindows := r.frozen != nil && r.frozenAt.dayStart.Equal(at.dayStart) && r.frozenAt.monthStart.Equal(at.monthStart)
	if sameWindows && !freezeUntil.Before(r.frozenUntil) {
		frozen = make(map[string]WindowUsage, len(r.frozen))
		for scope, u := range r.frozen {
			frozen[scope] = u
		}
		if freezeUntil.After(r.frozenUntil) {
			if err := scanUsageTotals(ctx, source, r.frozenUntil, freezeUntil, at.dayStart, frozen); err != nil {
				return nil, err
			}
		}
	} else {
		frozen = make(map[string]WindowUsage)
		if err := scanUsageTotals(ctx, source, at.monthStart, freezeUntil, at.dayStart, frozen); err != nil {
			return nil, err
		}
	}

	base := make(map[string]WindowUsage, len(frozen))
	for scope, u := range frozen {
		base[scope] = u
	}
	if err := scanUsageTotals(ctx, source, freezeUntil, time.Time{}, at.dayStart, base); err != nil {
		return nil, err
	}

	r.frozen, r.frozenUntil, r.frozenAt = frozen, freezeUntil, at
	return base, nil
}

// expireSlices drops the time slices of own usage older than
// refreshSliceMaxAge and returns how many it dropped. It runs when a refresh
// failed, so that the slices stay bounded while the database is unavailable.
func (r *refreshedUsage) expireSlices(now time.Time) int {
	oldest := sliceIndex(now.Add(-refreshSliceMaxAge))
	dropped := 0
	r.slices.Range(func(key, _ any) bool {
		if key.(int64) < oldest {
			r.slices.Delete(key)
			dropped++
		}
		return true
	})
	return dropped
}

// mergeCounter adds the usage src holds in the windows w to dst.
func mergeCounter(dst, src *MemoryBudgetCounter, w budgetWindows) {
	src.entries.Range(func(key, _ any) bool {
		scope := key.(string)
		u := src.usage(scope, w)
		if u != (WindowUsage{}) {
			dst.addScope(scope, w, u.Daily, u.Monthly)
		}
		return true
	})
}

// usage returns the view of one scope in the windows w, zero before the
// first refresh. Database totals of a window that has ended since the refresh
// do not count for the window that followed it.
func (r *refreshedUsage) usage(scope string, w budgetWindows) WindowUsage {
	view := r.view.Load()
	if view == nil {
		return WindowUsage{}
	}
	u := view.pending.usage(scope, w)
	base := view.base[scope]
	if view.at.dayStart.Equal(w.dayStart) {
		u.Daily.Tokens += base.Daily.Tokens
		u.Daily.CostMicros += base.Daily.CostMicros
	}
	if view.at.monthStart.Equal(w.monthStart) {
		u.Monthly.Tokens += base.Monthly.Tokens
		u.Monthly.CostMicros += base.Monthly.CostMicros
	}
	return u
}

// scanUsageTotals reads the usage persisted for events from from (inclusive)
// until until (exclusive; the zero time means no upper bound), aggregated by
// the database, and adds it to totals per counter scope. Events since
// dayStart also count towards the daily totals.
func scanUsageTotals(ctx context.Context, source UsageSeeder, from, until, dayStart time.Time, totals map[string]WindowUsage) error {
	rows, err := source.QueryUsageTotals(ctx, from, until, dayStart)
	if err != nil {
		return fmt.Errorf("query usage totals: %w", err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor, rows.Err is checked below

	for rows.Next() {
		var orgID, teamID, userID, keyID string
		var tokens, costMicros, dayTokens, dayCostMicros int64
		if err := rows.Scan(&orgID, &teamID, &userID, &keyID, &tokens, &costMicros, &dayTokens, &dayCostMicros); err != nil {
			return fmt.Errorf("scan usage totals row: %w", err)
		}
		monthly := Usage{Tokens: tokens, CostMicros: costMicros}.clamped()
		daily := Usage{Tokens: dayTokens, CostMicros: dayCostMicros}.clamped()
		forEachScope(Scopes{KeyID: keyID, UserID: userID, TeamID: teamID, OrgID: orgID}, func(scope string) {
			t := totals[scope]
			t.Daily.Tokens += daily.Tokens
			t.Daily.CostMicros += daily.CostMicros
			t.Monthly.Tokens += monthly.Tokens
			t.Monthly.CostMicros += monthly.CostMicros
			totals[scope] = t
		})
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate usage totals: %w", err)
	}
	return nil
}
