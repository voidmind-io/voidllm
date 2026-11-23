package ratelimit

import (
	"context"
	"errors"
	"strconv"
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
	if _, err := r.refresh(context.Background(), db); err != nil {
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
	frozenNextDay := time.Date(2026, 3, 10, 23, 57, 0, 0, time.UTC)
	wantNextDay := []timeRange{{monthStart, frozenNextDay}, {frozenNextDay, time.Time{}}}
	if got := db.takeRanges(); !equalRanges(got, wantNextDay) {
		t.Errorf("first refresh of the next day read %v, want %v (the whole month again)", got, wantNextDay)
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

// TestRefreshedUsage_OutageDoesNotDoubleCountOnRecovery plays a database
// outage that spans several refreshes while the usage logger still persists
// this instance's usage (its writes get through, the reads time out). During
// the outage the previous view keeps growing by the own usage. The first
// refresh that succeeds again must know how old each piece of own usage is:
// everything older than the allowance is in the database total and must not
// be added again, only what is younger may be counted twice.
func TestRefreshedUsage_OutageDoesNotDoubleCountOnRecovery(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	r, clock := newTestRefreshedUsage(start)
	s := Scopes{KeyID: "k", OrgID: "o"}
	org := OrgScope("o")
	other := persistedEvent{keyID: "other", orgID: "o", tokens: 100, at: start.Add(-time.Hour)}
	db := &fakeUsageSeeder{events: []persistedEvent{other}}
	mustRefresh(t, r, db)

	// Five refreshes fail, one minute apart. Before each, the instance
	// records 10 tokens that the usage logger persists.
	wantErr := errors.New("read timeout")
	persisted := []persistedEvent{other}
	for range 5 {
		recordTokens(r, s, 10)
		persisted = append(persisted, persistedEvent{keyID: "k", orgID: "o", tokens: 10, at: clock.Now()})
		clock.Add(time.Minute)
		db.setEvents(wantErr, persisted...)
		expired, err := r.refresh(context.Background(), db)
		if !errors.Is(err, wantErr) {
			t.Fatalf("refresh error = %v, want %v", err, wantErr)
		}
		if expired != 0 {
			t.Fatalf("refresh gave up %d slices within the first minutes of an outage", expired)
		}
	}
	if d, _ := viewTokens(r, org); d != 150 {
		t.Errorf("view during the outage = %d, want 150 (100 + all own usage)", d)
	}
	if n := countSlices(r); n != 5 {
		t.Errorf("%d slices after the failed refreshes, want 5 (one per recording, ages kept)", n)
	}

	// 3 more tokens right before the database answers again; they are not
	// persisted yet.
	recordTokens(r, s, 3)
	db.setEvents(nil, persisted...)
	mustRefresh(t, r, db)
	// Database: 100 + 50. Own usage younger than the allowance: 3. The 50
	// from the outage are between one and five minutes old and must not be
	// added a second time.
	if d, m := viewTokens(r, org); d != 153 || m != 153 {
		t.Errorf("view after recovery = %d/%d, want 153/153 (no second count of the outage's usage)", d, m)
	}

	// Once the 3 are persisted and older than the allowance: exactly the
	// database total.
	db.addEvents(persistedEvent{keyID: "k", orgID: "o", tokens: 3, at: clock.Now()})
	clock.Add(time.Minute)
	mustRefresh(t, r, db)
	if d, _ := viewTokens(r, org); d != 153 {
		t.Errorf("view in the end = %d, want 153", d)
	}
	if n := countSlices(r); n != 0 {
		t.Errorf("%d slices left, want 0", n)
	}
}

// TestRefreshedUsage_LongOutageExpiresOldSlices verifies that the record of
// own usage stays bounded while refreshes fail: slices older than
// refreshSliceMaxAge are given up and reported, younger ones stay, and the
// view in use keeps all own usage.
func TestRefreshedUsage_LongOutageExpiresOldSlices(t *testing.T) {
	t.Parallel()

	start := time.Date(2026, 3, 10, 6, 0, 0, 0, time.UTC)
	r, clock := newTestRefreshedUsage(start)
	s := Scopes{KeyID: "k", OrgID: "o"}
	db := &fakeUsageSeeder{}
	mustRefresh(t, r, db)

	db.setEvents(errors.New("database is down"))
	recordTokens(r, s, 1) // 06:00
	clock.Add(90 * time.Minute)
	recordTokens(r, s, 1) // 07:30
	if expired, err := r.refresh(context.Background(), db); err == nil || expired != 0 {
		t.Fatalf("refresh = %d, %v; want 0 expired slices and an error", expired, err)
	}
	clock.Add(45 * time.Minute) // 08:15: the first slice is older than two hours
	expired, err := r.refresh(context.Background(), db)
	if err == nil || expired != 1 {
		t.Fatalf("refresh = %d, %v; want 1 expired slice and an error", expired, err)
	}
	if n := countSlices(r); n != 1 {
		t.Errorf("%d slices left, want 1", n)
	}
	if d, _ := viewTokens(r, OrgScope("o")); d != 2 {
		t.Errorf("view during the outage = %d, want 2 (expiring a slice does not touch the view in use)", d)
	}
}

// TestRefreshedUsage_LateEventAroundMidnight verifies that an event of the
// last minutes of a day that is inserted after midnight is still picked up:
// the range read again always reaches back by the freeze lag, also on the
// first refresh of the new day.
func TestRefreshedUsage_LateEventAroundMidnight(t *testing.T) {
	t.Parallel()

	monthStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	r, clock := newTestRefreshedUsage(time.Date(2026, 3, 10, 23, 50, 0, 0, time.UTC))
	org := OrgScope("o")
	db := &fakeUsageSeeder{events: []persistedEvent{
		{keyID: "k", orgID: "o", tokens: 100, at: time.Date(2026, 3, 10, 9, 0, 0, 0, time.UTC)},
	}}
	mustRefresh(t, r, db)
	db.takeRanges()

	// First refresh of the new day, 30s after midnight. The month is read
	// in full, frozen only up to five minutes ago.
	clock.Set(time.Date(2026, 3, 11, 0, 0, 30, 0, time.UTC))
	mustRefresh(t, r, db)
	frozenUntil := time.Date(2026, 3, 10, 23, 55, 30, 0, time.UTC)
	want := []timeRange{{monthStart, frozenUntil}, {frozenUntil, time.Time{}}}
	if got := db.takeRanges(); !equalRanges(got, want) {
		t.Errorf("first refresh after midnight read %v, want %v", got, want)
	}
	if d, m := viewTokens(r, org); d != 0 || m != 100 {
		t.Fatalf("view after midnight = %d/%d, want 0/100", d, m)
	}

	// An event from 23:58 is inserted only now, together with one from
	// 00:00:10.
	db.addEvents(
		persistedEvent{keyID: "k", orgID: "o", tokens: 40, at: time.Date(2026, 3, 10, 23, 58, 0, 0, time.UTC)},
		persistedEvent{keyID: "k", orgID: "o", tokens: 7, at: time.Date(2026, 3, 11, 0, 0, 10, 0, time.UTC)},
	)
	clock.Add(time.Minute)
	mustRefresh(t, r, db)
	if d, m := viewTokens(r, org); d != 7 || m != 147 {
		t.Errorf("view with the late event = %d/%d, want 7/147 (late event in the month, not in the new day)", d, m)
	}

	// Later, when both are in the frozen part, nothing changes.
	clock.Add(10 * time.Minute)
	mustRefresh(t, r, db)
	if d, m := viewTokens(r, org); d != 7 || m != 147 {
		t.Errorf("view after freezing = %d/%d, want 7/147", d, m)
	}

	// At the start of a month the frozen range does not reach back into
	// the previous month.
	clock.Set(time.Date(2026, 4, 1, 0, 1, 0, 0, time.UTC))
	db.takeRanges()
	mustRefresh(t, r, db)
	april := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	want = []timeRange{{april, april}, {april, time.Time{}}}
	if got := db.takeRanges(); !equalRanges(got, want) {
		t.Errorf("first refresh of a month read %v, want %v", got, want)
	}
	if d, m := viewTokens(r, org); d != 0 || m != 0 {
		t.Errorf("view in the new month = %d/%d, want 0/0", d, m)
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
				if _, err := r.refresh(context.Background(), db); err != nil {
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

// TestRefreshedUsage_EvictStaleBoundsPendingDuringOutage verifies that the
// own usage kept in the view in use does not grow with every scope that was
// ever used while refreshes fail: scopes without usage in the current month
// are dropped, current ones stay.
func TestRefreshedUsage_EvictStaleBoundsPendingDuringOutage(t *testing.T) {
	t.Parallel()

	r, clock := newTestRefreshedUsage(time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC))
	db := &fakeUsageSeeder{}
	r.evictStale() // no view yet: nothing to do
	mustRefresh(t, r, db)
	db.setEvents(errors.New("database is down"))

	// March: 500 keys that are never used again.
	for i := range 500 {
		recordTokens(r, Scopes{KeyID: "old-" + strconv.Itoa(i), OrgID: "old-org"}, 1)
	}
	pendingEntries := func() int {
		n := 0
		r.view.Load().pending.entries.Range(func(_, _ any) bool { n++; return true })
		return n
	}
	if n := pendingEntries(); n != 501 {
		t.Fatalf("pending entries = %d, want 501 (500 keys + their org)", n)
	}

	// April, still no successful refresh. One key is in use.
	clock.Set(time.Date(2026, 4, 2, 12, 0, 0, 0, time.UTC))
	current := Scopes{KeyID: "current", OrgID: "current-org"}
	recordTokens(r, current, 9)
	if _, err := r.refresh(context.Background(), db); err == nil {
		t.Fatal("refresh error = nil, want the outage")
	}
	r.evictStale()
	if n := pendingEntries(); n != 2 {
		t.Errorf("pending entries after eviction = %d, want 2 (the current key and its org)", n)
	}
	if d, m := viewTokens(r, OrgScope("current-org")); d != 9 || m != 9 {
		t.Errorf("view of the current org = %d/%d, want 9/9", d, m)
	}

	// Usage after an eviction is still counted.
	recordTokens(r, current, 1)
	recordTokens(r, Scopes{KeyID: "old-again", OrgID: "old-org"}, 4)
	if d, _ := viewTokens(r, KeyScope("current")); d != 10 {
		t.Errorf("view of the current key = %d, want 10", d)
	}
	if d, _ := viewTokens(r, OrgScope("old-org")); d != 4 {
		t.Errorf("view of the evicted org after new usage = %d, want 4", d)
	}
}
