package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"
)

// migrationsFS holds the embedded SQL migration files.
// Populated in Phase 1.4 when migration files are added.
//
//go:embed migrations
var migrationsFS embed.FS

// migrationRunner is the minimal interface required to run schema migrations.
// Both *sql.DB and *sql.Conn satisfy this interface, allowing migration code
// to operate on a dedicated connection (required for PostgreSQL advisory locks)
// or on the pool directly (SQLite).
type migrationRunner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}

// migrationLockKey is the PostgreSQL advisory lock key used to serialize
// concurrent migration runs. Derived from CRC-32 of "voidllm-schema-migrations".
// Stable across deployments so all pods contend on the same lock.
const migrationLockKey int64 = 8370235791

// RunMigrations applies any unapplied migrations from the embedded migrations
// directory to the given database. It creates the schema_migrations tracking
// table if it does not exist, reads all "*.up.sql" files in alphabetical order,
// and skips any migration whose filename is already recorded in that table.
// Each migration is applied inside its own transaction. This function is
// idempotent: calling it multiple times on a fully migrated database is safe.
//
// When the dialect supports advisory locking (PostgreSQL), all migration work
// runs on a single dedicated *sql.Conn so that the advisory lock remains held
// for the duration of the migration run. This prevents concurrent pods from
// applying the same migrations simultaneously during rolling upgrades.
func RunMigrations(ctx context.Context, sqlDB *sql.DB, dialect Dialect, log *slog.Logger) error {
	var runner migrationRunner = sqlDB

	if dialect.SupportsMigrationLock() {
		conn, err := sqlDB.Conn(ctx)
		if err != nil {
			return fmt.Errorf("acquire connection for migration lock: %w", err)
		}
		defer conn.Close() //nolint:errcheck // best-effort cleanup

		log.LogAttrs(ctx, slog.LevelInfo, "waiting for migration lock")
		if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); err != nil {
			return fmt.Errorf("acquire migration lock: %w", err)
		}
		defer func() {
			if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", migrationLockKey); err != nil {
				log.LogAttrs(context.Background(), slog.LevelWarn, "failed to release migration lock",
					slog.String("error", err.Error()))
			}
		}()
		log.LogAttrs(ctx, slog.LevelInfo, "migration lock acquired")

		runner = conn
	}

	// CURRENT_TIMESTAMP is ANSI SQL and is supported by both SQLite and PostgreSQL.
	const createTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    filename   TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

	if _, err := runner.ExecContext(ctx, createTable); err != nil {
		return fmt.Errorf("create schema_migrations table: %w", err)
	}

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations directory: %w", err)
	}

	var filenames []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".up.sql") {
			filenames = append(filenames, e.Name())
		}
	}
	sort.Strings(filenames)

	for _, name := range filenames {
		applied, err := isMigrationApplied(ctx, runner, dialect, name)
		if err != nil {
			return fmt.Errorf("check migration %q: %w", name, err)
		}
		if applied {
			continue
		}

		log.LogAttrs(ctx, slog.LevelInfo, "applying migration", slog.String("file", name))
		if err := applyMigration(ctx, runner, dialect, name); err != nil {
			return fmt.Errorf("apply migration %q: %w", name, err)
		}
		log.LogAttrs(ctx, slog.LevelInfo, "migration applied", slog.String("file", name))
	}

	return nil
}

// isMigrationApplied reports whether the given filename has already been
// recorded in the schema_migrations table.
func isMigrationApplied(ctx context.Context, runner migrationRunner, dialect Dialect, filename string) (bool, error) {
	var count int
	query := "SELECT COUNT(*) FROM schema_migrations WHERE filename = " + dialect.Placeholder(1)
	err := runner.QueryRowContext(ctx, query, filename).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// applyMigration reads the named file from migrationsFS, executes its SQL
// content, and records the filename in schema_migrations — all within a single
// transaction.
func applyMigration(ctx context.Context, runner migrationRunner, dialect Dialect, filename string) error {
	content, err := migrationsFS.ReadFile("migrations/" + filename)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	tx, err := runner.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // deliberate: no-op after Commit

	if _, err := tx.ExecContext(ctx, string(content)); err != nil {
		return fmt.Errorf("execute sql: %w", err)
	}

	insert := "INSERT INTO schema_migrations (filename) VALUES (" + dialect.Placeholder(1) + ")"
	if _, err := tx.ExecContext(ctx, insert, filename); err != nil {
		return fmt.Errorf("record migration: %w", err)
	}

	return tx.Commit()
}
