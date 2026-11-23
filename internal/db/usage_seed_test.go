package db

import (
	"context"
	"math"
	"sort"
	"testing"
	"time"
)

// seedEvent is one usage_events row for the QueryUsageSeed tests. Empty
// teamID / userID and a nil cost are stored as SQL NULL.
type seedEvent struct {
	id, keyID, teamID, userID, orgID string
	tokens                           int64
	cost                             *float64
	createdAt                        time.Time
}

func insertSeedEvent(t *testing.T, d *DB, e seedEvent) {
	t.Helper()
	p := d.dialect.Placeholder
	query := "INSERT INTO usage_events " +
		"(id, key_id, key_type, org_id, team_id, user_id, model_name, total_tokens, cost_estimate, status_code, created_at) " +
		"VALUES (" + p(1) + ", " + p(2) + ", 'user_key', " + p(3) + ", " + p(4) + ", " + p(5) + ", 'test-model', " +
		p(6) + ", " + p(7) + ", 200, " + p(8) + ")"

	nullable := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	var cost any
	if e.cost != nil {
		cost = *e.cost
	}
	_, err := d.sql.ExecContext(context.Background(), query,
		e.id, e.keyID, e.orgID, nullable(e.teamID), nullable(e.userID), e.tokens, cost, FormatTimestamp(e.createdAt))
	if err != nil {
		t.Fatalf("insertSeedEvent id=%q: %v", e.id, err)
	}
}

// seedRow is one scanned QueryUsageSeed row, with the cost already converted
// to micro-units by rounding, the way the budget counters consume it.
type seedRow struct {
	keyID, teamID, orgID, userID string
	tokens                       int64
	costMicros                   int64
}

func querySeedRows(t *testing.T, d *DB, since time.Time) []seedRow {
	t.Helper()
	rows, err := d.QueryUsageSeed(context.Background(), since)
	if err != nil {
		t.Fatalf("QueryUsageSeed() error = %v", err)
	}
	defer rows.Close() //nolint:errcheck // test cursor

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("rows.Columns() error = %v", err)
	}
	if len(cols) != 6 {
		t.Fatalf("QueryUsageSeed() returned %d columns, want 6", len(cols))
	}

	var out []seedRow
	for rows.Next() {
		var r seedRow
		var cost float64
		if err := rows.Scan(&r.keyID, &r.teamID, &r.orgID, &r.userID, &r.tokens, &cost); err != nil {
			t.Fatalf("rows.Scan() error = %v", err)
		}
		r.costMicros = int64(math.Round(cost * 1e6))
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err() = %v", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].tokens < out[j].tokens })
	return out
}

// TestQueryUsageSeed verifies, on every supported dialect, the rows the
// budget counters are seeded from: one row per event on or after since, NULL
// team and user as empty strings, and the cost estimate as a number that is 0
// for events without pricing.
func TestQueryUsageSeed(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		since := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		cost := func(v float64) *float64 { return &v }

		events := []seedEvent{
			// Before the window: must not be returned.
			{id: "e0", keyID: "k1", orgID: "o1", tokens: 9999, cost: cost(5), createdAt: since.Add(-time.Second)},
			// Exactly at the window start: included.
			{id: "e1", keyID: "k1", teamID: "t1", userID: "u1", orgID: "o1", tokens: 100, cost: cost(0.0123), createdAt: since},
			// No team, no user, no cost.
			{id: "e2", keyID: "k2", orgID: "o1", tokens: 200, createdAt: since.Add(time.Hour)},
			// Sub-micro cost rounds to zero, a bit more rounds up to one.
			{id: "e3", keyID: "k3", userID: "u2", orgID: "o2", tokens: 300, cost: cost(0.0000004), createdAt: since.Add(48 * time.Hour)},
			{id: "e4", keyID: "k3", teamID: "t2", orgID: "o2", tokens: 400, cost: cost(0.0000006), createdAt: since.Add(49 * time.Hour)},
			// Whole currency units.
			{id: "e5", keyID: "k4", orgID: "o2", tokens: 500, cost: cost(25), createdAt: since.Add(72 * time.Hour)},
		}
		for _, e := range events {
			insertSeedEvent(t, d, e)
		}

		got := querySeedRows(t, d, since)
		want := []seedRow{
			{keyID: "k1", teamID: "t1", orgID: "o1", userID: "u1", tokens: 100, costMicros: 12300},
			{keyID: "k2", orgID: "o1", tokens: 200, costMicros: 0},
			{keyID: "k3", orgID: "o2", userID: "u2", tokens: 300, costMicros: 0},
			{keyID: "k3", teamID: "t2", orgID: "o2", tokens: 400, costMicros: 1},
			{keyID: "k4", orgID: "o2", tokens: 500, costMicros: 25_000_000},
		}
		if len(got) != len(want) {
			t.Fatalf("QueryUsageSeed() returned %d rows, want %d: %+v", len(got), len(want), got)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("row %d = %+v, want %+v", i, got[i], want[i])
			}
		}

		// A later window start narrows the result.
		later := querySeedRows(t, d, since.Add(48*time.Hour))
		if len(later) != 3 || later[0].tokens != 300 {
			t.Errorf("QueryUsageSeed(later) = %+v, want the 3 events from the later window", later)
		}

		// Nothing in the window: no rows, no error.
		if none := querySeedRows(t, d, since.Add(365*24*time.Hour)); len(none) != 0 {
			t.Errorf("QueryUsageSeed(future) = %+v, want no rows", none)
		}
	})
}
