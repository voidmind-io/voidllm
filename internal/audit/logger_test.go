package audit_test

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/audit"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
)

// newTestDB opens a named in-memory SQLite database, runs migrations, and
// registers a cleanup to close it when the test ends.
func newTestDB(t *testing.T, name string) *db.DB {
	t.Helper()
	ctx := context.Background()
	dsn := "file:" + name + "?mode=memory&cache=private"
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	return database
}

// discardLogger returns an slog.Logger that discards all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// countAuditLogs queries the number of rows in audit_logs for the given DB.
func countAuditLogs(t *testing.T, database *db.DB) int {
	t.Helper()
	var n int
	row := database.SQL().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM audit_logs")
	if err := row.Scan(&n); err != nil {
		t.Fatalf("count audit_logs: %v", err)
	}
	return n
}

// sampleEvent returns a minimal valid audit.Event with the given action.
func sampleEvent(action string) audit.Event {
	return audit.Event{
		Timestamp:    time.Now().UTC(),
		OrgID:        "org-test",
		ActorID:      "actor-test",
		ActorType:    "user",
		ActorKeyID:   "key-test",
		Action:       action,
		ResourceType: "org",
		ResourceID:   "res-test",
		Description:  "user " + action + " org",
		IPAddress:    "127.0.0.1",
		StatusCode:   200,
	}
}

// TestNewLogger verifies that NewLogger returns a non-nil Logger for a valid
// config and does not panic.
func TestNewLogger(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestNewLogger")
	cfg := config.AuditConfig{
		Enabled:       true,
		BufferSize:    10,
		FlushInterval: 100 * time.Millisecond,
	}

	logger := audit.NewLogger(database, cfg, discardLogger())
	if logger == nil {
		t.Fatal("NewLogger() returned nil, want non-nil Logger")
	}
}

// TestNewLogger_DefaultConfig verifies that zero-value config values are
// handled without panic (defaults are applied internally).
func TestNewLogger_DefaultConfig(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestNewLogger_DefaultConfig")
	cfg := config.AuditConfig{} // zero value: BufferSize=0, FlushInterval=0

	logger := audit.NewLogger(database, cfg, discardLogger())
	if logger == nil {
		t.Fatal("NewLogger() returned nil for zero-value config")
	}
	// Start and immediately stop to verify no panic.
	logger.Start()
	logger.Stop()
}

// TestLog_BufferFull verifies that logging more events than the buffer capacity
// does not block the caller and does not panic. Excess events are silently
// dropped per the contract in Log's godoc.
func TestLog_BufferFull(t *testing.T) {
	t.Parallel()

	// Use a very small buffer to force overflow quickly.
	const bufSize = 3
	database := newTestDB(t, "TestLog_BufferFull")
	cfg := config.AuditConfig{
		BufferSize:    bufSize,
		FlushInterval: 24 * time.Hour, // long interval to prevent auto-flush during the test
	}

	logger := audit.NewLogger(database, cfg, discardLogger())
	logger.Start()
	defer logger.Stop()

	// Log more events than the buffer can hold. None of these calls must block
	// or panic; excess events are silently dropped.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < bufSize*5; i++ {
			logger.Log(sampleEvent("create"))
		}
	}()

	select {
	case <-done:
		// All Log calls returned without blocking.
	case <-time.After(2 * time.Second):
		t.Fatal("Log() blocked for more than 2 seconds with a full buffer")
	}
}

// TestFlush verifies that events logged before Stop are persisted to the
// database when the logger is stopped.
func TestFlush(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestFlush")
	cfg := config.AuditConfig{
		BufferSize:    100,
		FlushInterval: 24 * time.Hour, // prevent timer-driven flush
	}

	logger := audit.NewLogger(database, cfg, discardLogger())
	logger.Start()

	const numEvents = 5
	for i := 0; i < numEvents; i++ {
		logger.Log(sampleEvent("create"))
	}

	// Stop drains remaining events and blocks until flush completes.
	logger.Stop()

	got := countAuditLogs(t, database)
	if got != numEvents {
		t.Errorf("audit_logs row count = %d, want %d after Stop", got, numEvents)
	}
}

// TestFlush_EmptyBatch verifies that stopping the logger immediately after
// starting it (with no events logged) does not error or panic.
func TestFlush_EmptyBatch(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestFlush_EmptyBatch")
	cfg := config.AuditConfig{
		BufferSize:    100,
		FlushInterval: 24 * time.Hour,
	}

	logger := audit.NewLogger(database, cfg, discardLogger())
	logger.Start()
	logger.Stop() // no events logged — flush of empty batch must not error

	got := countAuditLogs(t, database)
	if got != 0 {
		t.Errorf("audit_logs row count = %d, want 0 for empty batch", got)
	}
}

// TestFlush_BatchTimer verifies that events are flushed to the database after
// the configured flush interval elapses, without requiring an explicit Stop.
func TestFlush_BatchTimer(t *testing.T) {
	t.Parallel()

	const flushInterval = 150 * time.Millisecond
	database := newTestDB(t, "TestFlush_BatchTimer")
	cfg := config.AuditConfig{
		BufferSize:    100,
		FlushInterval: flushInterval,
	}

	logger := audit.NewLogger(database, cfg, discardLogger())
	logger.Start()
	defer logger.Stop()

	logger.Log(sampleEvent("update"))

	// Wait long enough for the ticker to fire and flush the event.
	deadline := time.Now().Add(3 * flushInterval)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if countAuditLogs(t, database) == 1 {
			return // event was flushed — test passes
		}
	}

	t.Errorf("event not flushed after %v; audit_logs row count = %d, want 1",
		3*flushInterval, countAuditLogs(t, database))
}
