package usage

// Regression tests for GetUsageAggregates' time-range and group_by=day /
// group_by=hour behavior (see #226), exercised through the real production
// write path (Logger.Log -> Logger.flush) rather than a raw SQL insert, on
// both SQLite and PostgreSQL (see forEachDialect in dialect_harness_test.go).

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
)

// logEventAt builds an Event scoped to orgID with the given CreatedAt,
// otherwise reusing makeEvent's fully populated field set.
func logEventAt(orgID string, createdAt time.Time) Event {
	ev := makeEvent()
	ev.OrgID = orgID
	ev.CreatedAt = createdAt
	return ev
}

// TestGetUsageAggregates_TimeRangeBoundaries_RealWritePath proves the
// from/to range comparison is byte-correct for rows written through the real
// Logger write path: a row an hour after `from` is included, a row one
// second after `to` (same calendar date) is excluded, and a row exactly at
// `to` is included (the upper bound is inclusive).
func TestGetUsageAggregates_TimeRangeBoundaries_RealWritePath(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *db.DB) {
		orgID := "org-usage-range-" + t.Name()

		l := NewLogger(d, config.UsageConfig{
			BufferSize:    10,
			FlushInterval: 30 * time.Second,
			DropOnFull:    ptr(false),
		}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		l.Start()

		from := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
		to := time.Date(2026, 9, 26, 21, 23, 17, 0, time.UTC)

		l.Log(logEventAt(orgID, from.Add(time.Hour))) // included
		l.Log(logEventAt(orgID, to.Add(time.Second))) // excluded
		l.Log(logEventAt(orgID, to))                  // included (boundary inclusive)
		l.Stop()

		rows, err := d.GetUsageAggregates(context.Background(), orgID, from, to, "")
		if err != nil {
			t.Fatalf("GetUsageAggregates() error = %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("len(rows) = %d, want 1", len(rows))
		}
		if rows[0].TotalRequests != 2 {
			t.Errorf("TotalRequests = %d, want 2 (from+1h included, to+1s excluded, to included)", rows[0].TotalRequests)
		}
	})
}

// TestGetUsageAggregates_GroupByDayAndHour_RealWritePath confirms the day and
// hour bucket expressions produce exactly the documented shapes for a row
// written through the real Logger write path.
func TestGetUsageAggregates_GroupByDayAndHour_RealWritePath(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *db.DB) {
		orgID := "org-usage-bucket-" + t.Name()

		l := NewLogger(d, config.UsageConfig{
			BufferSize:    10,
			FlushInterval: 30 * time.Second,
			DropOnFull:    ptr(false),
		}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
		l.Start()

		eventTime := time.Date(2026, 9, 26, 21, 0, 5, 0, time.UTC)
		from := eventTime.Add(-time.Hour)
		to := eventTime.Add(time.Hour)

		l.Log(logEventAt(orgID, eventTime))
		l.Stop()

		dayRows, err := d.GetUsageAggregates(context.Background(), orgID, from, to, "day")
		if err != nil {
			t.Fatalf("GetUsageAggregates(day) error = %v", err)
		}
		if len(dayRows) != 1 {
			t.Fatalf("len(dayRows) = %d, want 1", len(dayRows))
		}
		if want := "2026-09-26"; dayRows[0].GroupKey != want {
			t.Errorf("day GroupKey = %q, want %q", dayRows[0].GroupKey, want)
		}

		hourRows, err := d.GetUsageAggregates(context.Background(), orgID, from, to, "hour")
		if err != nil {
			t.Fatalf("GetUsageAggregates(hour) error = %v", err)
		}
		if len(hourRows) != 1 {
			t.Fatalf("len(hourRows) = %d, want 1", len(hourRows))
		}
		if want := "2026-09-26T21:00:00Z"; hourRows[0].GroupKey != want {
			t.Errorf("hour GroupKey = %q, want %q", hourRows[0].GroupKey, want)
		}
	})
}
