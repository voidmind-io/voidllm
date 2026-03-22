package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// migrationsFS holds the embedded SQL migration files.
// Populated in Phase 1.4 when migration files are added.
//
//go:embed migrations
var migrationsFS embed.FS

// RunMigrations applies any unapplied migrations from the embedded migrations
// directory to the given database. It creates the schema_migrations tracking
// table if it does not exist, reads all "*.up.sql" files in alphabetical order,
// and skips any migration whose filename is already recorded in that table.
// Each migration is applied inside its own transaction. This function is
// idempotent: calling it multiple times on a fully migrated database is safe.
func RunMigrations(ctx context.Context, sqlDB *sql.DB, dialect Dialect) error {
	// CURRENT_TIMESTAMP is ANSI SQL and is supported by both SQLite and PostgreSQL.
	const createTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
    filename   TEXT PRIMARY KEY,
    applied_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);`

	if _, err := sqlDB.ExecContext(ctx, createTable); err != nil {
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
		applied, err := isMigrationApplied(ctx, sqlDB, dialect, name)
		if err != nil {
			return fmt.Errorf("check migration %q: %w", name, err)
		}
		if applied {
			continue
		}

		if err := applyMigration(ctx, sqlDB, dialect, name); err != nil {
			return fmt.Errorf("apply migration %q: %w", name, err)
		}
	}

	return nil
}

// isMigrationApplied reports whether the given filename has already been
// recorded in the schema_migrations table.
func isMigrationApplied(ctx context.Context, sqlDB *sql.DB, dialect Dialect, filename string) (bool, error) {
	var count int
	query := "SELECT COUNT(*) FROM schema_migrations WHERE filename = " + dialect.Placeholder(1)
	err := sqlDB.QueryRowContext(ctx, query, filename).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// applyMigration reads the named file from migrationsFS, executes its SQL
// content, and records the filename in schema_migrations — all within a single
// transaction.
func applyMigration(ctx context.Context, sqlDB *sql.DB, dialect Dialect, filename string) error {
	content, err := migrationsFS.ReadFile("migrations/" + filename)
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}

	tx, err := sqlDB.BeginTx(ctx, nil)
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
