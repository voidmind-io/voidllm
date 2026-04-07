package retention

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
)

// newTestDB creates a temporary file-based SQLite database, runs all
// migrations, and registers cleanup via t.Cleanup. A file-based database is
// used instead of an in-memory one so that connection lifecycle events (e.g.
// a cancelled-context causing database/sql to discard a connection) do not
// destroy the schema and data.
//
// Each test gets its own file in t.TempDir() so parallel tests and -count=N
// reruns are fully isolated.
func newTestDB(t *testing.T) *db.DB {
	t.Helper()

	dir := t.TempDir()
	dsn := "file:" + dir + "/test.db?_busy_timeout=5000"

	cfg := config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	}

	ctx := context.Background()
	d, err := db.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("newTestDB: Open() error = %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := db.RunMigrations(ctx, d.SQL(), db.SQLiteDialect{}, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("newTestDB: RunMigrations() error = %v", err)
	}

	return d
}

// sanitizeName replaces characters that are not safe in SQLite URI names.
func sanitizeName(s string) string {
	out := make([]byte, len(s))
	for i := range len(s) {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			out[i] = c
		} else {
			out[i] = '_'
		}
	}
	return string(out)
}

// seedUsageEvents inserts one row per entry in ages, each with
// created_at = now - age. Timestamps are formatted as RFC3339Nano to match
// the format the cleaner uses for the cutoff parameter.
func seedUsageEvents(t *testing.T, database *db.DB, ages []time.Duration) {
	t.Helper()

	now := time.Now().UTC()
	ctx := context.Background()

	for _, age := range ages {
		id, err := uuid.NewV7()
		if err != nil {
			t.Fatalf("seedUsageEvents: generate id: %v", err)
		}
		ts := now.Add(-age).Format(time.RFC3339Nano)

		_, err = database.SQL().ExecContext(ctx,
			`INSERT INTO usage_events
				(id, key_id, key_type, org_id, model_name, status_code, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			id.String(), "key-test", "user_key", "org-test", "model-test", 200, ts,
		)
		if err != nil {
			t.Fatalf("seedUsageEvents: insert: %v", err)
		}
	}
}

// seedAuditLogs inserts one row per entry in ages, each with
// timestamp = now - age. The format matches auditLogsTimestampFormat
// (time.RFC3339) which is the format the cleaner compares against.
func seedAuditLogs(t *testing.T, database *db.DB, ages []time.Duration) {
	t.Helper()

	now := time.Now().UTC()
	ctx := context.Background()

	for _, age := range ages {
		id, err := uuid.NewV7()
		if err != nil {
			t.Fatalf("seedAuditLogs: generate id: %v", err)
		}
		ts := now.Add(-age).UTC().Format(time.RFC3339)

		_, err = database.SQL().ExecContext(ctx,
			`INSERT INTO audit_logs
				(id, timestamp, action, resource_type)
			VALUES (?, ?, ?, ?)`,
			id.String(), ts, "create", "key",
		)
		if err != nil {
			t.Fatalf("seedAuditLogs: insert: %v", err)
		}
	}
}

// countTableRows returns SELECT COUNT(*) for the given table.
func countTableRows(t *testing.T, database *db.DB, table string) int {
	t.Helper()

	var n int
	err := database.SQL().QueryRowContext(
		context.Background(),
		"SELECT COUNT(*) FROM "+table,
	).Scan(&n)
	if err != nil {
		t.Fatalf("countTableRows(%s): %v", table, err)
	}
	return n
}

// discardLogger returns a *slog.Logger that discards all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestCleaner constructs a Cleaner with disabled betweenBatch delay (no
// sleeps in tests) and the provided config.
func newTestCleaner(database *db.DB, cfg config.RetentionConfig) *Cleaner {
	c := New(database, cfg, discardLogger())
	c.betweenBatch = 0
	return c
}

// TestNew_NilLoggerDefaults verifies that passing nil as the logger argument to
// New causes it to fall back to slog.Default() instead of leaving c.log nil.
func TestNew_NilLoggerDefaults(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)
	cfg := config.RetentionConfig{UsageEvents: time.Hour, Interval: time.Hour}
	c := New(database, cfg, nil)
	if c.log == nil {
		t.Fatal("New(nil) must assign a default logger, got nil")
	}
}

// TestRunOnce_RetentionDisabled verifies that when both retention durations are
// zero, runOnce is a no-op: no rows in either table are deleted.
func TestRunOnce_RetentionDisabled(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)
	cfg := config.RetentionConfig{
		UsageEvents: 0,
		AuditLogs:   0,
	}

	// Insert 5 old usage_events and 5 old audit_logs — all older than any
	// plausible retention window.
	oldAge := 30 * 24 * time.Hour // 30 days
	seedUsageEvents(t, database, []time.Duration{
		oldAge, oldAge, oldAge, oldAge, oldAge,
	})
	seedAuditLogs(t, database, []time.Duration{
		oldAge, oldAge, oldAge, oldAge, oldAge,
	})

	c := newTestCleaner(database, cfg)
	c.runOnce(context.Background())

	if got := countTableRows(t, database, "usage_events"); got != 5 {
		t.Errorf("usage_events count = %d, want 5 (retention disabled, nothing deleted)", got)
	}
	if got := countTableRows(t, database, "audit_logs"); got != 5 {
		t.Errorf("audit_logs count = %d, want 5 (retention disabled, nothing deleted)", got)
	}
}

// TestRunOnce_UsageRetentionOnly verifies that when only UsageEvents retention
// is set, old usage_events are deleted, fresh usage_events remain, and
// audit_logs are entirely untouched regardless of their age.
func TestRunOnce_UsageRetentionOnly(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)
	cfg := config.RetentionConfig{
		UsageEvents: 24 * time.Hour,
		AuditLogs:   0, // disabled
	}

	// 2 old usage events (48h ago) + 2 fresh usage events (1h ago).
	seedUsageEvents(t, database, []time.Duration{
		48 * time.Hour,
		48 * time.Hour,
		1 * time.Hour,
		1 * time.Hour,
	})
	// 3 old audit logs (all older than 24h).
	seedAuditLogs(t, database, []time.Duration{
		48 * time.Hour,
		48 * time.Hour,
		48 * time.Hour,
	})

	c := newTestCleaner(database, cfg)
	c.runOnce(context.Background())

	if got := countTableRows(t, database, "usage_events"); got != 2 {
		t.Errorf("usage_events count = %d, want 2 (2 old deleted, 2 fresh kept)", got)
	}
	if got := countTableRows(t, database, "audit_logs"); got != 3 {
		t.Errorf("audit_logs count = %d, want 3 (audit retention disabled, all kept)", got)
	}
}

// TestRunOnce_AuditRetentionOnly verifies the mirror image of TestRunOnce_UsageRetentionOnly:
// only audit_logs are cleaned; usage_events are untouched.
func TestRunOnce_AuditRetentionOnly(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)
	cfg := config.RetentionConfig{
		UsageEvents: 0, // disabled
		AuditLogs:   24 * time.Hour,
	}

	// 3 old usage events — should not be touched.
	seedUsageEvents(t, database, []time.Duration{
		48 * time.Hour,
		48 * time.Hour,
		48 * time.Hour,
	})
	// 2 old audit logs (48h) + 2 fresh audit logs (1h).
	seedAuditLogs(t, database, []time.Duration{
		48 * time.Hour,
		48 * time.Hour,
		1 * time.Hour,
		1 * time.Hour,
	})

	c := newTestCleaner(database, cfg)
	c.runOnce(context.Background())

	if got := countTableRows(t, database, "usage_events"); got != 3 {
		t.Errorf("usage_events count = %d, want 3 (usage retention disabled, all kept)", got)
	}
	if got := countTableRows(t, database, "audit_logs"); got != 2 {
		t.Errorf("audit_logs count = %d, want 2 (2 old deleted, 2 fresh kept)", got)
	}
}

// TestRunOnce_BothEnabled verifies that when both retention windows are set,
// only the old rows in each table are removed.
func TestRunOnce_BothEnabled(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)
	cfg := config.RetentionConfig{
		UsageEvents: 24 * time.Hour,
		AuditLogs:   24 * time.Hour,
	}

	// usage_events: 3 old, 2 fresh.
	seedUsageEvents(t, database, []time.Duration{
		72 * time.Hour,
		72 * time.Hour,
		72 * time.Hour,
		30 * time.Minute,
		30 * time.Minute,
	})
	// audit_logs: 2 old, 3 fresh.
	seedAuditLogs(t, database, []time.Duration{
		48 * time.Hour,
		48 * time.Hour,
		15 * time.Minute,
		15 * time.Minute,
		15 * time.Minute,
	})

	c := newTestCleaner(database, cfg)
	c.runOnce(context.Background())

	if got := countTableRows(t, database, "usage_events"); got != 2 {
		t.Errorf("usage_events count = %d, want 2 (3 old deleted, 2 fresh kept)", got)
	}
	if got := countTableRows(t, database, "audit_logs"); got != 3 {
		t.Errorf("audit_logs count = %d, want 3 (2 old deleted, 3 fresh kept)", got)
	}
}

// TestCleanupTable_ExactlyBatchSize verifies that when the row count equals
// batchSize, cleanupTable deletes all rows and returns the correct count.
func TestCleanupTable_ExactlyBatchSize(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)

	const batchSz = 3
	// Insert exactly batchSize old rows.
	seedUsageEvents(t, database, []time.Duration{
		48 * time.Hour,
		48 * time.Hour,
		48 * time.Hour,
	})

	cfg := config.RetentionConfig{UsageEvents: 24 * time.Hour}
	c := newTestCleaner(database, cfg)
	c.batchSize = batchSz

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	deleted, err := c.cleanupTable(
		context.Background(),
		"usage_events",
		"created_at",
		cutoff,
	)
	if err != nil {
		t.Fatalf("cleanupTable() error = %v, want nil", err)
	}
	if deleted != batchSz {
		t.Errorf("cleanupTable() deleted = %d, want %d", deleted, batchSz)
	}
	if got := countTableRows(t, database, "usage_events"); got != 0 {
		t.Errorf("usage_events remaining = %d, want 0", got)
	}
}

// TestCleanupTable_BatchSizePlusOne verifies that when rows exceed batchSize,
// cleanupTable loops and deletes all rows across multiple batches.
func TestCleanupTable_BatchSizePlusOne(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)

	const batchSz = 3
	// Insert batchSize+1 old rows.
	seedUsageEvents(t, database, []time.Duration{
		48 * time.Hour,
		48 * time.Hour,
		48 * time.Hour,
		48 * time.Hour,
	})

	cfg := config.RetentionConfig{UsageEvents: 24 * time.Hour}
	c := newTestCleaner(database, cfg)
	c.batchSize = batchSz

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	deleted, err := c.cleanupTable(
		context.Background(),
		"usage_events",
		"created_at",
		cutoff,
	)
	if err != nil {
		t.Fatalf("cleanupTable() error = %v, want nil", err)
	}
	if deleted != 4 {
		t.Errorf("cleanupTable() deleted = %d, want 4", deleted)
	}
	if got := countTableRows(t, database, "usage_events"); got != 0 {
		t.Errorf("usage_events remaining = %d, want 0", got)
	}
}

// TestRunOnce_CutoffBoundary verifies strict less-than semantics: a row whose
// created_at equals the exact cutoff is KEPT; a row 1 second older is deleted.
func TestRunOnce_CutoffBoundary(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)
	cfg := config.RetentionConfig{
		UsageEvents: 24 * time.Hour,
	}

	// Capture now once so all calculations are consistent.
	now := time.Now().UTC()

	// Compute what the cleaner will use as the cutoff string. The cleaner does:
	//   cutoff = time.Now().UTC().Add(-maxAge)
	// We approximate this by computing it here. There is a tiny window where
	// time.Now() inside the cleaner is slightly after ours, but we add a
	// 2-second safety margin on the "at cutoff" row to make the test robust.
	//
	// Row A: exactly at the cutoff second (should be KEPT).
	// Row B: 2 seconds before the cutoff (should be DELETED).
	cutoffApprox := now.Add(-24 * time.Hour)
	tsAtCutoff := cutoffApprox.Format(time.RFC3339Nano)
	tsOneSecOlder := cutoffApprox.Add(-2 * time.Second).Format(time.RFC3339Nano)

	ctx := context.Background()

	idA, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("generate idA: %v", err)
	}
	idB, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("generate idB: %v", err)
	}

	for _, row := range []struct {
		id string
		ts string
	}{
		{idA.String(), tsAtCutoff},
		{idB.String(), tsOneSecOlder},
	} {
		_, err := database.SQL().ExecContext(ctx,
			`INSERT INTO usage_events
				(id, key_id, key_type, org_id, model_name, status_code, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			row.id, "key-test", "user_key", "org-test", "model-test", 200, row.ts,
		)
		if err != nil {
			t.Fatalf("insert boundary row: %v", err)
		}
	}

	c := newTestCleaner(database, cfg)
	c.runOnce(ctx)

	// Only the row older than the cutoff should be gone; the row AT the cutoff
	// must remain (strict < comparison in the SQL).
	if got := countTableRows(t, database, "usage_events"); got != 1 {
		t.Errorf("usage_events count after boundary cleanup = %d, want 1 (row at cutoff must be kept)", got)
	}

	// Confirm the surviving row is the one at the cutoff, not the older one.
	var survivingID string
	err = database.SQL().QueryRowContext(ctx, "SELECT id FROM usage_events LIMIT 1").Scan(&survivingID)
	if err != nil {
		t.Fatalf("select surviving row: %v", err)
	}
	if survivingID != idA.String() {
		t.Errorf("surviving row id = %q, want %q (row at cutoff must be kept)", survivingID, idA.String())
	}
}

// TestCleanupTable_ContextCancellation verifies that cancelling the context
// between batches causes cleanupTable to return (nil, partialCount) — no error,
// and the total reflects only the batches that completed before cancellation.
func TestCleanupTable_ContextCancellation(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)

	const batchSz = 3
	// Insert 3 * batchSize old rows.
	totalRows := batchSz * 3
	ages := make([]time.Duration, totalRows)
	for i := range totalRows {
		ages[i] = 48 * time.Hour
	}
	seedUsageEvents(t, database, ages)

	cfg := config.RetentionConfig{UsageEvents: 24 * time.Hour}
	c := newTestCleaner(database, cfg)
	c.batchSize = batchSz
	// betweenBatch is already 0 from newTestCleaner, but we set it explicitly
	// to make the intent clear.
	c.betweenBatch = 0

	// Cancel the context immediately — before the first iteration check.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	deleted, err := c.cleanupTable(ctx, "usage_events", "created_at", cutoff)

	if err != nil {
		t.Errorf("cleanupTable() with cancelled context: error = %v, want nil (partial success)", err)
	}

	// With an already-cancelled context the loop exits on the first ctx.Err()
	// check before executing any DELETE. deleted must be 0.
	if deleted != 0 {
		t.Errorf("cleanupTable() with pre-cancelled context: deleted = %d, want 0", deleted)
	}

	// All rows should remain because no DELETE ran.
	if got := countTableRows(t, database, "usage_events"); got != totalRows {
		t.Errorf("usage_events remaining = %d, want %d (context cancelled before any batch)", got, totalRows)
	}
}

// TestCleanupTable_ContextCancelAfterOneBatch tests that cancellation between
// batches (not before the first one) leaves exactly the rows that were not
// yet deleted.
func TestCleanupTable_ContextCancelAfterOneBatch(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)

	const batchSz = 3
	totalRows := batchSz * 2 // two full batches worth
	ages := make([]time.Duration, totalRows)
	for i := range totalRows {
		ages[i] = 48 * time.Hour
	}
	seedUsageEvents(t, database, ages)

	cfg := config.RetentionConfig{UsageEvents: 24 * time.Hour}
	c := newTestCleaner(database, cfg)
	c.batchSize = batchSz

	// Use a context that will be cancelled after the first batch. We achieve
	// this by cancelling after the first ExecContext returns (via betweenBatch
	// select). We set betweenBatch to a tiny non-zero duration so the select
	// statement is reached, then cancel from outside.
	ctx, cancel := context.WithCancel(context.Background())

	// Let betweenBatch be non-zero so the ctx.Done() case in the select fires.
	c.betweenBatch = 5 * time.Millisecond

	// Cancel concurrently — after a short head-start to let the first batch run.
	go func() {
		time.Sleep(2 * time.Millisecond)
		cancel()
	}()

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	deleted, err := c.cleanupTable(ctx, "usage_events", "created_at", cutoff)

	if err != nil {
		t.Errorf("cleanupTable() cancelled between batches: error = %v, want nil", err)
	}

	// When context is cancelled the exact reported delete count depends on
	// whether the cancel fires before or during ExecContext:
	//  - Before: deleted=0, remaining=totalRows (no batch ran)
	//  - After first batch: deleted=batchSz, remaining=totalRows-batchSz
	//  - During ExecContext: SQLite may commit the batch but the error path
	//    cannot retrieve RowsAffected, so deleted understates actual progress.
	//
	// In all cases the invariants that must hold are:
	//   remaining >= 0
	//   deleted >= 0
	//   remaining + deleted <= totalRows (no double-counting; deleted may understate)
	remaining := countTableRows(t, database, "usage_events")
	if deleted < 0 || deleted > int64(totalRows) {
		t.Errorf("cleanupTable() deleted = %d, want 0..%d", deleted, totalRows)
	}
	if remaining < 0 || remaining > totalRows {
		t.Errorf("usage_events remaining = %d, want 0..%d", remaining, totalRows)
	}
	if remaining+int(deleted) > totalRows {
		t.Errorf("remaining(%d) + deleted(%d) = %d, want <= %d (rows cannot be created)", remaining, deleted, remaining+int(deleted), totalRows)
	}
}

// TestCleanupTable_AuditLogsFormat verifies that the RFC3339 timestamp format
// is correctly handled for audit_logs comparisons.
func TestCleanupTable_AuditLogsFormat(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)
	cfg := config.RetentionConfig{AuditLogs: 24 * time.Hour}

	// 3 old audit logs and 2 fresh ones.
	seedAuditLogs(t, database, []time.Duration{
		72 * time.Hour,
		72 * time.Hour,
		72 * time.Hour,
		30 * time.Minute,
		30 * time.Minute,
	})

	c := newTestCleaner(database, cfg)
	c.runOnce(context.Background())

	if got := countTableRows(t, database, "audit_logs"); got != 2 {
		t.Errorf("audit_logs count = %d, want 2 (3 old deleted, 2 fresh kept)", got)
	}
}

// TestStop_SafeToCallMultipleTimes verifies that calling Stop() multiple times
// does not panic or deadlock.
func TestStop_SafeToCallMultipleTimes(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)
	cfg := config.RetentionConfig{UsageEvents: 24 * time.Hour, Interval: time.Hour}

	c := New(database, cfg, discardLogger())
	c.Start()

	// First Stop should close the done channel.
	c.Stop()
	// Second Stop should be a no-op (stopOnce ensures the channel is only closed once).
	c.Stop()
}

// TestStart_DisabledIsNoop verifies that Start() with a disabled config does
// not launch a background goroutine (no ticker, no stop channel needed).
func TestStart_DisabledIsNoop(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)
	cfg := config.RetentionConfig{
		UsageEvents: 0,
		AuditLogs:   0,
	}

	c := New(database, cfg, discardLogger())
	c.Start()

	// Stop must be safe to call even when Start was a no-op.
	c.Stop()

	if c.done != nil {
		t.Error("done channel should be nil when retention is disabled (Start was a no-op)")
	}
}

// TestRunOnce_ContextTimeout verifies that passing a pre-cancelled parent
// context to runOnce returns cleanly without panicking, and that cancellation
// propagates into cleanupTable so no rows are deleted.
func TestRunOnce_ContextTimeout(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)
	cfg := config.RetentionConfig{UsageEvents: 24 * time.Hour}

	// Insert 10 old rows.
	ages := make([]time.Duration, 10)
	for i := range ages {
		ages[i] = 48 * time.Hour
	}
	seedUsageEvents(t, database, ages)

	c := newTestCleaner(database, cfg)
	c.batchSize = 3
	c.betweenBatch = 50 * time.Millisecond

	// Use a pre-cancelled context so the first ctx.Err() check inside
	// cleanupTable returns immediately before executing any DELETE.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Must not hang or panic.
	c.runOnce(ctx)

	// With a pre-cancelled context, no batches run — all 10 rows remain.
	if got := countTableRows(t, database, "usage_events"); got != 10 {
		t.Errorf("usage_events remaining = %d, want 10 (context cancelled before any batch)", got)
	}
}

// TestRunOnce_ErrorIsolationBetweenTables verifies the documented contract that
// a failure cleaning one table does not abort cleanup of the other table.
// We force an error on usage_events by dropping the table, then confirm that
// audit_logs is still cleaned and the error is logged.
func TestRunOnce_ErrorIsolationBetweenTables(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)
	cfg := config.RetentionConfig{
		UsageEvents: 24 * time.Hour,
		AuditLogs:   24 * time.Hour,
	}

	// Seed 5 old rows into both tables.
	oldAges := []time.Duration{
		48 * time.Hour, 48 * time.Hour, 48 * time.Hour, 48 * time.Hour, 48 * time.Hour,
	}
	seedUsageEvents(t, database, oldAges)
	seedAuditLogs(t, database, oldAges)

	// Route the cleaner's log output to a buffer so we can assert the error is
	// logged rather than silently swallowed.
	var buf bytes.Buffer
	testLog := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError}))

	c := New(database, cfg, testLog)
	c.betweenBatch = 0

	// Drop usage_events so the DELETE will fail with "no such table".
	if _, err := database.SQL().Exec("DROP TABLE usage_events"); err != nil {
		t.Fatalf("DROP TABLE usage_events: %v", err)
	}

	// runOnce must not panic regardless of the error.
	c.runOnce(context.Background())

	// audit_logs must be fully cleaned even though usage_events failed.
	if got := countTableRows(t, database, "audit_logs"); got != 0 {
		t.Errorf("audit_logs remaining = %d, want 0 (should be cleaned despite usage_events error)", got)
	}

	// The error for usage_events must have been logged.
	if !bytes.Contains(buf.Bytes(), []byte("usage_events")) {
		t.Errorf("expected log output to mention usage_events, got: %s", buf.String())
	}
}

// TestCleanupTable_MultipleBatches_AuditLogs verifies batch-boundary behaviour
// for the audit_logs table specifically, complementing the existing usage_events
// batch tests.
func TestCleanupTable_MultipleBatches_AuditLogs(t *testing.T) {
	t.Parallel()

	database := newTestDB(t)

	const batchSz = 3
	// 7 old rows: 3 full batches of 3 (last batch returns 1, signalling done).
	seedAuditLogs(t, database, []time.Duration{
		48 * time.Hour, 48 * time.Hour, 48 * time.Hour,
		48 * time.Hour, 48 * time.Hour, 48 * time.Hour,
		48 * time.Hour,
	})

	cfg := config.RetentionConfig{AuditLogs: 24 * time.Hour}
	c := newTestCleaner(database, cfg)
	c.batchSize = batchSz
	c.betweenBatch = 0

	cutoff := time.Now().UTC().Add(-24 * time.Hour)
	deleted, err := c.cleanupTable(context.Background(), "audit_logs", "timestamp", cutoff)
	if err != nil {
		t.Fatalf("cleanupTable() error = %v, want nil", err)
	}
	if deleted != 7 {
		t.Errorf("cleanupTable() deleted = %d, want 7", deleted)
	}
	if got := countTableRows(t, database, "audit_logs"); got != 0 {
		t.Errorf("audit_logs remaining = %d, want 0", got)
	}
}
