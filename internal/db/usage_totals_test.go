package db

import (
	"context"
	"testing"
	"time"
)

// totalsRow is one scanned QueryUsageTotals row; costs are in micro-units.
type totalsRow struct {
	orgID, teamID, userID, keyID string
	tokens, dayTokens            int64
	cost, dayCost                int64
}

func queryTotals(t *testing.T, d *DB, from, until, dayStart time.Time) map[string]totalsRow {
	t.Helper()
	rows, err := d.QueryUsageTotals(context.Background(), from, until, dayStart)
	if err != nil {
		t.Fatalf("QueryUsageTotals() error = %v", err)
	}
	defer rows.Close() //nolint:errcheck // test cursor

	out := make(map[string]totalsRow)
	for rows.Next() {
		var r totalsRow
		if err := rows.Scan(&r.orgID, &r.teamID, &r.userID, &r.keyID, &r.tokens, &r.cost, &r.dayTokens, &r.dayCost); err != nil {
			t.Fatalf("rows.Scan() error = %v", err)
		}
		id := r.keyID + "/" + r.teamID + "/" + r.userID
		if _, dup := out[id]; dup {
			t.Fatalf("QueryUsageTotals() returned %q twice; rows must be aggregated", id)
		}
		out[id] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err() = %v", err)
	}
	return out
}

// TestQueryUsageTotals verifies, on every supported dialect, the aggregate
// the budget counters refresh from: one row per org, team, user and key,
// summed over the events of the requested range, with the day columns limited
// to the events since the day start.
func TestQueryUsageTotals(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		monthStart := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		dayStart := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC)
		noon := dayStart.Add(12 * time.Hour)
		cost := func(v float64) *float64 { return &v }

		events := []seedEvent{
			// Last month: outside every range used below.
			{id: "t0", keyID: "k1", orgID: "o1", tokens: 9999, cost: cost(9), createdAt: monthStart.Add(-time.Second)},
			// First instant of the month: month only.
			{id: "t1", keyID: "k1", teamID: "t1", userID: "u1", orgID: "o1", tokens: 100, cost: cost(0.01), createdAt: monthStart},
			// Last second before the day: month only.
			{id: "t2", keyID: "k1", teamID: "t1", userID: "u1", orgID: "o1", tokens: 20, cost: cost(0.002), createdAt: dayStart.Add(-time.Second)},
			// First instant of the day and later: month and day.
			{id: "t3", keyID: "k1", teamID: "t1", userID: "u1", orgID: "o1", tokens: 3, cost: cost(0.0003), createdAt: dayStart},
			{id: "t4", keyID: "k1", teamID: "t1", userID: "u1", orgID: "o1", tokens: 5, cost: cost(0.0005), createdAt: noon},
			// Another key without team, user and cost.
			{id: "t5", keyID: "k2", orgID: "o1", tokens: 50, createdAt: noon.Add(-time.Hour)},
			// Another org, only earlier in the month.
			{id: "t6", keyID: "k3", userID: "u9", orgID: "o2", tokens: 700, cost: cost(25), createdAt: monthStart.Add(48 * time.Hour)},
			// Costs are rounded per event: three events of 0.4 micro-units
			// are 0, not 1; one of 0.6 is 1.
			{id: "t7", keyID: "k4", orgID: "o2", tokens: 1, cost: cost(0.0000004), createdAt: noon},
			{id: "t8", keyID: "k4", orgID: "o2", tokens: 1, cost: cost(0.0000004), createdAt: noon},
			{id: "t9", keyID: "k4", orgID: "o2", tokens: 1, cost: cost(0.0000004), createdAt: noon},
			{id: "t10", keyID: "k4", orgID: "o2", tokens: 1, cost: cost(0.0000006), createdAt: noon},
			// Negative values count as zero.
			{id: "t11", keyID: "k4", orgID: "o2", tokens: -500, cost: cost(-3), createdAt: noon},
		}
		for _, e := range events {
			insertSeedEvent(t, d, e)
		}

		// Open range from the month start.
		got := queryTotals(t, d, monthStart, time.Time{}, dayStart)
		want := map[string]totalsRow{
			"k1/t1/u1": {orgID: "o1", teamID: "t1", userID: "u1", keyID: "k1", tokens: 128, dayTokens: 8, cost: 12800, dayCost: 800},
			"k2//":     {orgID: "o1", keyID: "k2", tokens: 50, dayTokens: 50},
			"k3//u9":   {orgID: "o2", userID: "u9", keyID: "k3", tokens: 700, cost: 25_000_000},
			"k4//":     {orgID: "o2", keyID: "k4", tokens: 4, dayTokens: 4, cost: 1, dayCost: 1},
		}
		if len(got) != len(want) {
			t.Fatalf("QueryUsageTotals() returned %d rows, want %d: %+v", len(got), len(want), got)
		}
		for k, w := range want {
			if got[k] != w {
				t.Errorf("row %s = %+v, want %+v", k, got[k], w)
			}
		}

		// Bounded range: from is inclusive, until exclusive.
		bounded := queryTotals(t, d, dayStart, noon, dayStart)
		wantBounded := map[string]totalsRow{
			"k1/t1/u1": {orgID: "o1", teamID: "t1", userID: "u1", keyID: "k1", tokens: 3, dayTokens: 3, cost: 300, dayCost: 300},
			"k2//":     {orgID: "o1", keyID: "k2", tokens: 50, dayTokens: 50},
		}
		if len(bounded) != len(wantBounded) {
			t.Fatalf("bounded QueryUsageTotals() returned %d rows, want %d: %+v", len(bounded), len(wantBounded), bounded)
		}
		for k, w := range wantBounded {
			if bounded[k] != w {
				t.Errorf("bounded row %s = %+v, want %+v", k, bounded[k], w)
			}
		}

		// The two halves of a split range add up to the whole.
		before := queryTotals(t, d, monthStart, noon, dayStart)
		after := queryTotals(t, d, noon, time.Time{}, dayStart)
		if sum := before["k1/t1/u1"].tokens + after["k1/t1/u1"].tokens; sum != 128 {
			t.Errorf("split ranges sum to %d tokens for k1, want 128", sum)
		}
		if sum := before["k1/t1/u1"].dayTokens + after["k1/t1/u1"].dayTokens; sum != 8 {
			t.Errorf("split ranges sum to %d day tokens for k1, want 8", sum)
		}

		// Nothing in the range: no rows, no error.
		future := monthStart.AddDate(1, 0, 0)
		if none := queryTotals(t, d, future, time.Time{}, future); len(none) != 0 {
			t.Errorf("QueryUsageTotals(future) = %+v, want no rows", none)
		}
	})
}
