package db

import (
	"context"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
)

func sqliteCfg() config.DatabaseConfig {
	return config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             "file::memory:?cache=shared&mode=memory",
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	}
}

func TestOpen_ValidSQLite(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := Open(ctx, sqliteCfg())
	if err != nil {
		t.Fatalf("Open() error = %v, want nil", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Ping(ctx); err != nil {
		t.Errorf("Ping() after Open() error = %v, want nil", err)
	}
}

func TestOpen_UnsupportedDriver(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cfg := config.DatabaseConfig{
		Driver:          "mysql",
		DSN:             "user:pass@/dbname",
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	}

	_, err := Open(ctx, cfg)
	if err == nil {
		t.Fatal("Open() error = nil, want error for unsupported driver")
	}
}

func TestClose_AfterOpen(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := Open(ctx, sqliteCfg())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if err := db.Close(); err != nil {
		t.Errorf("Close() error = %v, want nil", err)
	}
}

func TestPing_OpenDB(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := Open(ctx, sqliteCfg())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if err := db.Ping(ctx); err != nil {
		t.Errorf("Ping() error = %v, want nil", err)
	}
}

func TestPing_AfterClose(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := Open(ctx, sqliteCfg())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if err := db.Ping(ctx); err == nil {
		t.Error("Ping() after Close() error = nil, want error")
	}
}

func TestWithTx_Commit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := Open(ctx, sqliteCfg())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Create a table outside the transaction.
	if _, err := db.SQL().ExecContext(ctx, "CREATE TABLE tx_commit_test (val TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Insert inside a transaction that succeeds.
	err = db.WithTx(ctx, func(q Querier) error {
		_, err := q.ExecContext(ctx, "INSERT INTO tx_commit_test VALUES ('hello')")
		return err
	})
	if err != nil {
		t.Fatalf("WithTx() error = %v, want nil", err)
	}

	// Verify the row was committed.
	var count int
	if err := db.SQL().QueryRowContext(ctx, "SELECT COUNT(*) FROM tx_commit_test").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 1 {
		t.Errorf("row count = %d, want 1 (transaction not committed)", count)
	}
}

func TestWithTx_Rollback(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := Open(ctx, sqliteCfg())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Create a table outside the transaction.
	if _, err := db.SQL().ExecContext(ctx, "CREATE TABLE tx_rollback_test (val TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Insert inside a transaction that returns an error.
	sentinelErr := context.DeadlineExceeded // any non-nil error
	err = db.WithTx(ctx, func(q Querier) error {
		if _, err := q.ExecContext(ctx, "INSERT INTO tx_rollback_test VALUES ('should-not-persist')"); err != nil {
			return err
		}
		return sentinelErr
	})
	if err != sentinelErr {
		t.Fatalf("WithTx() error = %v, want %v", err, sentinelErr)
	}

	// Verify the row was NOT committed.
	var count int
	if err := db.SQL().QueryRowContext(ctx, "SELECT COUNT(*) FROM tx_rollback_test").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 0 {
		t.Errorf("row count = %d, want 0 (transaction should have been rolled back)", count)
	}
}

func TestWithTx_Nested(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := Open(ctx, sqliteCfg())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	if _, err := db.SQL().ExecContext(ctx, "CREATE TABLE tx_nested_test (val TEXT)"); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Multiple ExecContext calls inside the same transaction.
	err = db.WithTx(ctx, func(q Querier) error {
		if _, err := q.ExecContext(ctx, "INSERT INTO tx_nested_test VALUES ('a')"); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, "INSERT INTO tx_nested_test VALUES ('b')"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx() error = %v, want nil", err)
	}

	var count int
	if err := db.SQL().QueryRowContext(ctx, "SELECT COUNT(*) FROM tx_nested_test").Scan(&count); err != nil {
		t.Fatalf("count query: %v", err)
	}
	if count != 2 {
		t.Errorf("row count = %d, want 2", count)
	}
}

func TestSQLitePragma_ForeignKeys(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := Open(ctx, sqliteCfg())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var fkEnabled int
	if err := db.SQL().QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fkEnabled); err != nil {
		t.Fatalf("PRAGMA foreign_keys query: %v", err)
	}
	if fkEnabled != 1 {
		t.Errorf("PRAGMA foreign_keys = %d, want 1", fkEnabled)
	}
}

func TestSQLitePragma_JournalMode(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := Open(ctx, sqliteCfg())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	var mode string
	if err := db.SQL().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode query: %v", err)
	}
	// In-memory SQLite (?mode=memory) overrides the WAL pragma and always
	// reports "memory". The WAL pragma is intentionally applied by Open() and
	// is effective for file-based databases in production.
	if mode != "wal" && mode != "memory" {
		t.Errorf("PRAGMA journal_mode = %q, want %q or %q", mode, "wal", "memory")
	}
}

func TestDialect_SQLite(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := Open(ctx, sqliteCfg())
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	d := db.Dialect()
	if _, ok := d.(SQLiteDialect); !ok {
		t.Errorf("Dialect() = %T, want SQLiteDialect", d)
	}
}
