package db

// Regression tests for the Go-migration mechanism itself (see migrate.go's
// goMigrations registry), exercised through the 0018 canonical-timestamps
// migration since it is currently the only registered Go migration.

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"
)

const migration0018Name = "0018_canonical_event_timestamps.go"

// TestMigration0018_Ordering confirms that "0018_canonical_event_timestamps.go"
// sorts after every existing "*.up.sql" migration filename — in particular
// after "0017_membership_limits.up.sql" — under the exact sort.Strings call
// RunMigrations uses to merge Go migrations with embedded SQL files into one
// ordered sequence. This is a property of the registered name string itself,
// so it is verified directly rather than by racing against timestamps.
func TestMigration0018_Ordering(t *testing.T) {
	t.Parallel()

	names := []string{
		"0017_membership_limits.up.sql",
		migration0018Name,
	}
	sort.Strings(names)

	if names[0] != "0017_membership_limits.up.sql" || names[1] != migration0018Name {
		t.Fatalf("sort.Strings(%v) = %v, want 0017 before 0018", []string{"0017_membership_limits.up.sql", migration0018Name}, names)
	}
	if !strings.HasPrefix(migration0018Name, "0018_") {
		t.Fatalf("migration name %q does not carry the expected 0018 prefix", migration0018Name)
	}
}

// TestMigration0018_RecordedExactlyOnce confirms that RunMigrations records
// "0018_canonical_event_timestamps.go" in schema_migrations exactly once —
// never duplicated, even though a Go migration's INSERT into
// schema_migrations happens outside the transaction machinery applyMigration
// uses for SQL files (see recordMigrationApplied).
func TestMigration0018_RecordedExactlyOnce(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)

	var count int
	err := d.sql.QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM schema_migrations WHERE filename = ?", migration0018Name,
	).Scan(&count)
	if err != nil {
		t.Fatalf("count schema_migrations rows: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_migrations rows for %q = %d, want 1", migration0018Name, count)
	}
}

// TestMigration0018_SecondRunDoesNotReapply proves that a second call to
// RunMigrations against an already-migrated database does not re-execute the
// Go migration's normalization logic. It does so by deliberately corrupting a
// row's created_at into a non-canonical shape *after* the first (real)
// migration run has already completed, then calling RunMigrations again: if
// the migration reran, normalizeEventTimestamps would rewrite the row back to
// canonical and the residual would disappear. Instead the row must stay
// exactly as corrupted, and schema_migrations must still hold exactly one
// row for the migration's filename (not two).
func TestMigration0018_SecondRunDoesNotReapply(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()

	id := newSortableID(t)
	insertUsageEventRaw(t, d, id, FormatTimestamp(time.Date(2026, 9, 26, 21, 23, 17, 0, time.UTC)))

	const corrupted = "2026-09-26 21:23:17" // deliberately non-canonical, seeded after migration already ran once
	if _, err := d.sql.ExecContext(ctx,
		"UPDATE usage_events SET created_at = ? WHERE id = ?", corrupted, id,
	); err != nil {
		t.Fatalf("corrupt row after first migration run: %v", err)
	}

	if err := RunMigrations(ctx, d.sql, d.dialect, slog.Default()); err != nil {
		t.Fatalf("second RunMigrations() error = %v, want nil", err)
	}

	if got := rawColumnValue(t, d, "usage_events", "created_at", id); got != corrupted {
		t.Errorf("created_at after second RunMigrations = %q, want untouched %q (migration must not have rerun)", got, corrupted)
	}

	var count int
	if err := d.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE filename = ?", migration0018Name,
	).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations rows: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_migrations rows for %q after second RunMigrations = %d, want 1 (not re-recorded)", migration0018Name, count)
	}
}
