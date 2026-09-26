package db

// Regression tests for GetMCPUsageAggregates' time-range and group_by=day /
// group_by=hour behavior (see #226): before the canonical-timestamp fix,
// created_at was written by CURRENT_TIMESTAMP (SQLite: "2026-09-26 21:23:17";
// PostgreSQL: a timestamptz-as-text rendering) while range bounds were bound
// as RFC3339 ("...T...Z"); plain TEXT comparison made ' ' < 'T', so the whole
// from-date was silently dropped and rows after `to` on the to-date leaked
// in. These tests write rows through the real InsertMCPToolCalls path (with
// an explicit, already-canonical CreatedAt) and confirm the query boundaries
// and bucket formats are exactly right on both dialects.

import (
	"context"
	"testing"
	"time"
)

// mustInsertMCPToolCallAt inserts a single mcp_tool_calls row via the real
// InsertMCPToolCalls path with CreatedAt set to createdAt, formatted through
// FormatTimestamp exactly as the production MCP logger does.
func mustInsertMCPToolCallAt(t *testing.T, d *DB, orgID string, createdAt time.Time) {
	t.Helper()
	mustInsertMCPToolCall(t, d, MCPToolCall{
		ID:          newSortableID(t),
		KeyID:       "key-1",
		KeyType:     "user_key",
		OrgID:       orgID,
		ServerAlias: "server-a",
		ToolName:    "tool-a",
		Status:      "success",
		CreatedAt:   FormatTimestamp(createdAt),
	})
}

// TestGetMCPUsageAggregates_TimeRangeBoundaries_RealWritePath proves the
// from/to range comparison is byte-correct for rows written through the real
// insert path: a row an hour after `from` is included, a row one second after
// `to` (same calendar date) is excluded, and a row exactly at `to` is
// included (the upper bound is inclusive).
func TestGetMCPUsageAggregates_TimeRangeBoundaries_RealWritePath(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		orgID := "org-mcp-range-" + newSortableID(t)
		from := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
		to := time.Date(2026, 9, 26, 21, 23, 17, 0, time.UTC)

		mustInsertMCPToolCallAt(t, d, orgID, from.Add(time.Hour)) // included
		mustInsertMCPToolCallAt(t, d, orgID, to.Add(time.Second)) // excluded
		mustInsertMCPToolCallAt(t, d, orgID, to)                  // included (boundary inclusive)

		results, err := d.GetMCPUsageAggregates(context.Background(), orgID, from, to, "")
		if err != nil {
			t.Fatalf("GetMCPUsageAggregates() error = %v", err)
		}
		if len(results) != 1 {
			t.Fatalf("len(results) = %d, want 1", len(results))
		}
		if results[0].TotalCalls != 2 {
			t.Errorf("TotalCalls = %d, want 2 (from+1h included, to+1s excluded, to included)", results[0].TotalCalls)
		}
	})
}

// TestGetMCPUsageAggregates_GroupByDayAndHour_RealWritePath confirms the
// day and hour bucket expressions produce exactly the documented shapes for
// a row written through the real insert path.
func TestGetMCPUsageAggregates_GroupByDayAndHour_RealWritePath(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		orgID := "org-mcp-bucket-" + newSortableID(t)
		eventTime := time.Date(2026, 9, 26, 21, 0, 5, 0, time.UTC)
		from := eventTime.Add(-time.Hour)
		to := eventTime.Add(time.Hour)

		mustInsertMCPToolCallAt(t, d, orgID, eventTime)

		dayResults, err := d.GetMCPUsageAggregates(context.Background(), orgID, from, to, "day")
		if err != nil {
			t.Fatalf("GetMCPUsageAggregates(day) error = %v", err)
		}
		if len(dayResults) != 1 {
			t.Fatalf("len(dayResults) = %d, want 1", len(dayResults))
		}
		if want := "2026-09-26"; dayResults[0].GroupKey != want {
			t.Errorf("day GroupKey = %q, want %q", dayResults[0].GroupKey, want)
		}

		hourResults, err := d.GetMCPUsageAggregates(context.Background(), orgID, from, to, "hour")
		if err != nil {
			t.Fatalf("GetMCPUsageAggregates(hour) error = %v", err)
		}
		if len(hourResults) != 1 {
			t.Fatalf("len(hourResults) = %d, want 1", len(hourResults))
		}
		if want := "2026-09-26T21:00:00Z"; hourResults[0].GroupKey != want {
			t.Errorf("hour GroupKey = %q, want %q", hourResults[0].GroupKey, want)
		}
	})
}
