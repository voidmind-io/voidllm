package db

// Tests for the batch-fallback path normalizeBatch falls into when a batch
// UPDATE itself errors (see normalize_timestamps.go's normalizeBatch /
// normalizeBatchFallback), and for the log hygiene of every path in this file
// that reports an unparseable row: the raw stored value must never appear in
// a log line, and any warning reporting a failure names the affected table.
//
// A row shaped like "2026-13-40 99:99:99" is the trigger throughout: it
// passes PostgresDialect.ParseableTimestamp's loose digit-shape regex guard
// (four digits, two digits, two digits, a space, two digits, colon, two
// digits, colon, two digits) but is not a real calendar date (month 13, day
// 40, hour/minute/second 99), so PostgreSQL's ::timestamptz cast inside
// CanonicalTimestamp fails and the whole batch UPDATE errors. On SQLite the
// same value simply makes strftime return NULL, so ParseableTimestamp is
// false and the row is filtered out of the UPDATE's WHERE clause entirely —
// no batch failure, no fallback, just an untouched row.

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// corruptTimestampShape is a value that is digit-shaped like a timestamp
// (matching PostgresDialect.ParseableTimestamp's guard) but calendrically
// invalid: month 13, day 40, hour/minute/second 99.
const corruptTimestampShape = "2026-13-40 99:99:99"

// TestNormalizeEventTimestamps_CorruptRowSharesBatchWithValidRows seeds a
// corrupt row in the same keyset batch as several legacy-but-valid rows
// (normalizeBatchSize is lowered so a batch of 2 forces the corrupt row to
// share a batch boundary with a valid neighbor) and confirms:
//   - NormalizeEventTimestamps returns nil (never fails on the corrupt row)
//   - every legacy row in the batch — including ones sharing the corrupt
//     row's own batch — is normalized via the fallback path
//   - the corrupt row itself is left completely unchanged
//
// This does not call t.Parallel() anywhere in its call chain (including via
// a shared helper) because it mutates the package-level normalizeBatchSize
// variable, following the same non-parallel discipline as
// TestNormalizeEventTimestamps_MultiBatch in normalize_timestamps_test.go.
func TestNormalizeEventTimestamps_CorruptRowSharesBatchWithValidRows(t *testing.T) {
	original := normalizeBatchSize
	normalizeBatchSize = 2
	defer func() { normalizeBatchSize = original }()

	check := func(t *testing.T, d *DB, legacyLiteral string) {
		t.Helper()
		ctx := context.Background()

		// Four rows in ascending id (== insertion/time) order, with the
		// corrupt row as the second: batch size 2 groups ids [0,1] into one
		// batch and [2,3] into the next, so the corrupt row (index 1) shares
		// its batch with a valid legacy row (index 0).
		var legacyIDs []string
		var corruptID string
		for i := range 4 {
			if i == 1 {
				corruptID = newSortableID(t)
				insertUsageEventRaw(t, d, corruptID, corruptTimestampShape)
				continue
			}
			id := newSortableID(t)
			legacyIDs = append(legacyIDs, id)
			insertUsageEventRaw(t, d, id, legacyLiteral)
		}

		if err := NormalizeEventTimestamps(ctx, d.SQL(), d.Dialect(), slog.Default()); err != nil {
			t.Fatalf("NormalizeEventTimestamps() error = %v, want nil", err)
		}

		const want = "2026-09-26T21:23:17Z"
		for _, id := range legacyIDs {
			if got := rawColumnValue(t, d, "usage_events", "created_at", id); got != want {
				t.Errorf("legacy row %s created_at = %q, want %q (fallback normalization)", id, got, want)
			}
		}

		if got := rawColumnValue(t, d, "usage_events", "created_at", corruptID); got != corruptTimestampShape {
			t.Errorf("corrupt row created_at = %q, want untouched %q", got, corruptTimestampShape)
		}
	}

	t.Run("sqlite", func(t *testing.T) {
		check(t, openMigratedDB(t), "2026-09-26 21:23:17")
	})

	if dsn := postgresTestDSN(); dsn != "" {
		t.Run("postgres", func(t *testing.T) {
			check(t, openMigratedPostgresDB(t, dsn), "2026-09-26 21:23:17.123456+00")
		})
	}
}

// TestNormalizeEventTimestamps_CorruptRowLogHygiene confirms that when
// normalization encounters the corrupt shape (whether via PostgreSQL's
// batch-UPDATE-fails-then-falls-back path or SQLite's up-front
// ParseableTimestamp guard), the raw corrupt value is never written to the
// log, and the warning it does produce (either the batch-fallback warning on
// PostgreSQL, or the residual-non-canonical-rows warning on both dialects)
// names the affected table.
func TestNormalizeEventTimestamps_CorruptRowLogHygiene(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		id := newSortableID(t)
		insertUsageEventRaw(t, d, id, corruptTimestampShape)

		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, nil))

		if err := NormalizeEventTimestamps(ctx, d.SQL(), d.Dialect(), logger); err != nil {
			t.Fatalf("NormalizeEventTimestamps() error = %v, want nil", err)
		}

		output := buf.String()

		if strings.Contains(output, corruptTimestampShape) {
			t.Errorf("log output contains the raw corrupt timestamp value; log must never carry stored values:\n%s", output)
		}

		var warnLines []string
		for _, line := range strings.Split(output, "\n") {
			if strings.Contains(line, "level=WARN") {
				warnLines = append(warnLines, line)
			}
		}
		if len(warnLines) == 0 {
			t.Fatalf("no WARN-level log line produced for corrupt row; log output:\n%s", output)
		}

		foundTableName := false
		for _, line := range warnLines {
			if strings.Contains(line, "usage_events") {
				foundTableName = true
				break
			}
		}
		if !foundTableName {
			t.Errorf("no WARN-level log line names the table usage_events; warn lines:\n%s", strings.Join(warnLines, "\n"))
		}

		// The corrupt row itself must still be left exactly as seeded,
		// regardless of which path (fallback vs. up-front guard) caught it.
		if got := rawColumnValue(t, d, "usage_events", "created_at", id); got != corruptTimestampShape {
			t.Errorf("corrupt row created_at = %q, want untouched %q", got, corruptTimestampShape)
		}
	})
}
