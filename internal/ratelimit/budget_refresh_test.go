package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// refreshTestAllowance is the persist allowance of the views under test.
const refreshTestAllowance = 15 * time.Second

func newTestRefreshedUsage(start time.Time) (*refreshedUsage, *fakeClock) {
	clock := newFakeClock(start)
	return newRefreshedUsage(clock.Now, refreshTestAllowance), clock
}

// recordTokens records own usage at the view's current time.
func recordTokens(r *refreshedUsage, s Scopes, n int64) {
	r.record(windowsAt(r.now()), s, Usage{Tokens: n})
}

func viewTokens(r *refreshedUsage, scope string) (daily, monthly int64) {
	u := r.usage(scope, windowsAt(r.now()))
	return u.Daily.Tokens, u.Monthly.Tokens
}

func mustRefresh(t *testing.T, r *refreshedUsage, db *fakeUsageSeeder) {
	t.Helper()
	if err := r.refresh(context.Background(), db); err != nil {
		t.Fatalf("refresh: %v", err)
	}
}

func countSlices(r *refreshedUsage) int {
	n := 0
	r.slices.Range(func(_, _ any) bool { n++; return true })
	return n
}

// TestRefreshedUsage_OwnUsageCountedOnce follows own usage through the
// refreshes around its persistence: not lost while the database does not
// hold it, counted twice at most while it is younger than the allowance, and
// exactly once afterwards.
func TestRefreshedUsage_OwnUsageCountedOnce(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	r, clock := newTestRefreshedUsage(start)
	s := Scopes{KeyID: "k", TeamID: "tm", OrgID: "o"}
	org := OrgScope("o")
	db := &fakeUsageSeeder{events: []persistedEvent{
		{keyID: "other", orgID: "o", tokens: 100, at: start.Add(-time.Hour)},
		{keyID: "old", orgID: "o", tokens: 1000, cost: 3, at: start.AddDate(0, 0, -5)},
		{keyID: "last-month", orgID: "o", tokens: 5000, at: start.AddDate(0, -1, 0)},
	}}

	// Nothing before the first refresh, not even own usage: the view is
	// only ever used next to counters that have it.
	recordTokens(r, s, 30)
	if d, m := viewTokens(r, org); d != 0 || m != 0 {
		t.Fatalf("view before the first refresh = %d/%d, want 0/0", d, m)
	}

	// First refresh, 2s later, database without the own 30.
	clock.Add(2 * time.Second)
	mustRefresh(t, r, db)
	if d, m := viewTokens(r, org); d != 130 || m != 1130 {
		t.Errorf("view with unpersisted own usage = %d/%d, want 130/1130", d, m)
	}
	if got := r.usage(org, windowsAt(clock.Now())); got.Monthly.CostMicros != 3_000_000 || got.Daily.CostMicros != 0 {
		t.Errorf("cost = %+v, want 3000000 for the month and 0 for the day", got)
	}
	if d, _ := viewTokens(r, TeamScope("tm")); d != 30 {
		t.Errorf("team view = %d, want 30 (own usage only)", d)
	}

	// Own usage after a refresh is added on top at once.
	recordTokens(r, s, 5)
	if d, _ := viewTokens(r, org); d != 135 {
		t.Errorf("view after more own usage = %d, want 135", d)
	}

	// The database catches up; the own usage is still younger than the
	// allowance, so it is counted twice. This is the documented bound.
	db.addEvents(
		persistedEvent{keyID: "k", teamID: "tm", orgID: "o", tokens: 30, at: start},
		persistedEvent{keyID: "k", teamID: "tm", orgID: "o", tokens: 5, at: start.Add(2 * time.Second)},
	)
	clock.Add(5 * time.Second)
	mustRefresh(t, r, db)
	if d, _ := viewTokens(r, org); d != 170 {
		t.Errorf("view within the allowance = %d, want 170 (135 + own 35 once more)", d)
	}

	// Past the allowance: exactly the database total.
	clock.Add(refreshTestAllowance + 2*refreshSliceLength)
	mustRefresh(t, r, db)
	if d, m := viewTokens(r, org); d != 135 || m != 1135 {
		t.Errorf("view past the allowance = %d/%d, want 135/1135", d, m)
	}
	if n := countSlices(r); n != 0 {
		t.Errorf("%d slices kept past the allowance, want 0", n)
	}
}

// TestRefreshedUsage_ReadsOnlyRecentEventsAgain verifies how the database is
// read: the whole month once, afterwards only the range that newly became
// older than the freeze lag plus everything younger, and the whole month
// again on a new day. It also pins the documented blind spot for events
// inserted later than the freeze lag after they happened.
func TestRefreshedUsage_ReadsOnlyRecentEventsAgain(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	monthStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	r, clock := newTestRefreshedUsage(start)
	org := OrgScope("o")
	db := &fakeUsageSeeder{events: []persistedEvent{
		{keyID: "k", orgID: "o", tokens: 100, at: start.Add(-time.Hour)},
	}}

	mustRefresh(t, r, db)
	frozen1 := start.Add(-refreshFreezeLag)
	wantFirst := []timeRange{{monthStart, frozen1}, {frozen1, time.Time{}}}
	if got := db.takeRanges(); !equalRanges(got, wantFirst) {
		t.Errorf("first refresh read %v, want %v", got, wantFirst)
	}

	// One minute later: the minute that crossed the freeze lag, and the tail.
	db.addEvents(persistedEvent{keyID: "k", orgID: "o", tokens: 7, at: clock.Now()})
	clock.Add(time.Minute)
	mustRefresh(t, r, db)
	frozen2 := frozen1.Add(time.Minute)
	wantSecond := []timeRange{{frozen1, frozen2}, {frozen2, time.Time{}}}
	if got := db.takeRanges(); !equalRanges(got, wantSecond) {
		t.Errorf("second refresh read %v, want %v", got, wantSecond)
	}
	if d, m := viewTokens(r, org); d != 107 || m != 107 {
		t.Errorf("view = %d/%d, want 107/107", d, m)
	}

	// Ten minutes later the 7 are in the frozen part; they are still
	// counted exactly once.
	clock.Add(10 * time.Minute)
	mustRefresh(t, r, db)
	if d, _ := viewTokens(r, org); d != 107 {
		t.Errorf("view after freezing = %d, want 107", d)
	}

	// An event that shows up in a range that is already frozen is missed...
	db.addEvents(persistedEvent{keyID: "k", orgID: "o", tokens: 1000, at: start})
	clock.Add(time.Minute)
	mustRefresh(t, r, db)
	if d, _ := viewTokens(r, org); d != 107 {
		t.Errorf("view with a late event in a frozen range = %d, want 107 (documented blind spot)", d)
	}

	// ...until the month is aggregated in full again, on the next day.
	db.takeRanges()
	clock.Set(time.Date(2026, 3, 11, 0, 2, 0, 0, time.UTC))
	mustRefresh(t, r, db)
	dayStart := time.Date(2026, 3, 11, 0, 0, 0, 0, time.UTC)
	wantNextDay := []timeRange{{monthStart, dayStart}, {dayStart, time.Time{}}}
	if got := db.takeRanges(); !equalRanges(got, wantNextDay) {
		t.Errorf("first refresh of the next day read %v, want %v (never frozen across the day start)", got, wantNextDay)
	}
	if d, m := viewTokens(r, org); d != 0 || m != 1107 {
		t.Errorf("view on the next day = %d/%d, want 0/1107", d, m)
	}
}

func equalRanges(a, b []timeRange) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].from.Equal(b[i].from) || !a[i].until.Equal(b[i].until) {
			return false
		}
	}
	return true
}

// TestRefreshedUsage_FailedRefreshKeepsViewAndOwnUsage verifies that while
// refreshes fail the previous view stays and keeps growing by the own usage
// recorded since, however long the database stays away, that the slices are
// folded so their number stays bounded, and that a failed refresh does not
// advance what is taken as read.
func TestRefreshedUsage_FailedRefreshKeepsViewAndOwnUsage(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	r, clock := newTestRefreshedUsage(start)
	s := Scopes{KeyID: "k", OrgID: "o"}
	org := OrgScope("o")
	other := persistedEvent{keyID: "other", orgID: "o", tokens: 100, at: start.Add(-time.Hour)}
	db := &fakeUsageSeeder{events: []persistedEvent{other}}
	mustRefresh(t, r, db)

	wantErr := errors.New("database is down")
	db.setEvents(wantErr)
	for range 20 {
		recordTokens(r, s, 1)
		clock.Add(time.Minute)
		if err := r.refresh(context.Background(), db); !errors.Is(err, wantErr) {
			t.Fatalf("refresh error = %v, want %v", err, wantErr)
		}
	}
	if d, _ := viewTokens(r, org); d != 120 {
		t.Errorf("view during the outage = %d, want 120 (100 + all own usage)", d)
	}
	if n := countSlices(r); n != 1 {
		t.Errorf("%d slices after failed refreshes, want 1 (folded)", n)
	}

	// The database is back, with another instance's usage from the middle
	// of the outage: the range the failed refreshes could not read is read
	// now. Own usage of the outage that is older than the allowance is taken
	// to be in the database total; if it never got there, only the recording
	// instance's shadow counter still has it, which is why the view is never
	// used alone.
	recordTokens(r, s, 1)
	db.setEvents(nil, other, persistedEvent{keyID: "other", orgID: "o", tokens: 40, at: start.Add(8 * time.Minute)})
	mustRefresh(t, r, db)
	if d, _ := viewTokens(r, org); d != 141 {
		t.Errorf("view right after the outage = %d, want 141 (100 + 40 + the own usage just recorded)", d)
	}
}

// TestRefreshedUsage_WindowRollover verifies that database totals read before
// midnight do not count for the new day, while the month keeps them.
func TestRefreshedUsage_WindowRollover(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 3, 10, 23, 59, 30, 0, time.UTC)
	r, clock := newTestRefreshedUsage(start)
	s := Scopes{KeyID: "k", OrgID: "o"}
	org := OrgScope("o")
	db := &fakeUsageSeeder{events: []persistedEvent{{keyID: "other", orgID: "o", tokens: 100, at: start.Add(-time.Hour)}}}

	mustRefresh(t, r, db)
	recordTokens(r, s, 10) // before midnight

	clock.Set(time.Date(2026, 3, 11, 0, 0, 5, 0, time.UTC))
	recordTokens(r, s, 4) // after midnight
	if d, m := viewTokens(r, org); d != 4 || m != 114 {
		t.Errorf("view after midnight = %d/%d, want 4/114", d, m)
	}

	// Into the next month nothing of the old view counts.
	clock.Set(time.Date(2026, 4, 1, 0, 0, 5, 0, time.UTC))
	if d, m := viewTokens(r, org); d != 0 || m != 0 {
		t.Errorf("view in the next month = %d/%d, want 0/0", d, m)
	}
}

// TestRefreshedUsage_ConcurrentRecordAndRefresh records own usage from many
// goroutines while refreshes swap the view. All of it is younger than the
// allowance and the database never holds it, so every single record must be
// in the final view exactly once.
func TestRefreshedUsage_ConcurrentRecordAndRefresh(t *testing.T) {
	t.Parallel()

	const (
		goroutines = 8
		each       = 2000
	)
	start := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	r, _ := newTestRefreshedUsage(start)
	db := &fakeUsageSeeder{events: []persistedEvent{{keyID: "other", orgID: "o", tokens: 100, at: start.Add(-time.Hour)}}}
	s := Scopes{KeyID: "k", OrgID: "o"}

	stop := make(chan struct{})
	var refresher, recorders sync.WaitGroup
	refresher.Add(1)
	go func() {
		defer refresher.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if err := r.refresh(context.Background(), db); err != nil {
					t.Errorf("refresh: %v", err)
					return
				}
			}
		}
	}()
	recorders.Add(goroutines)
	for range goroutines {
		go func() {
			defer recorders.Done()
			for range each {
				recordTokens(r, s, 1)
			}
		}()
	}
	recorders.Wait()
	close(stop)
	refresher.Wait()

	mustRefresh(t, r, db)
	if d, m := viewTokens(r, OrgScope("o")); d != 100+goroutines*each || m != 100+goroutines*each {
		t.Errorf("view = %d/%d, want %d for both", d, m, 100+goroutines*each)
	}
}
