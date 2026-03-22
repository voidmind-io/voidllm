package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
)

// openTestDB opens an in-memory SQLite DB for migration tests.
// Each call with a unique DSN gets an isolated database.
func openTestDB(t *testing.T, dsn string) *sql.DB {
	t.Helper()

	cfg := config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	}
	ctx := context.Background()
	db, err := Open(ctx, cfg)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db.SQL()
}

func TestRunMigrations_AppliesMigration(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sqlDB := openTestDB(t, "file:TestRunMigrations_AppliesMigration?mode=memory&cache=private")

	if err := RunMigrations(ctx, sqlDB, SQLiteDialect{}); err != nil {
		t.Errorf("RunMigrations() error = %v, want nil", err)
	}
}

func TestRunMigrations_Idempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sqlDB := openTestDB(t, "file:TestRunMigrations_Idempotent?mode=memory&cache=private")

	if err := RunMigrations(ctx, sqlDB, SQLiteDialect{}); err != nil {
		t.Fatalf("RunMigrations() first call error = %v", err)
	}
	if err := RunMigrations(ctx, sqlDB, SQLiteDialect{}); err != nil {
		t.Errorf("RunMigrations() second call error = %v, want nil (idempotent)", err)
	}
}

func TestRunMigrations_CreatesSchemaTable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sqlDB := openTestDB(t, "file:TestRunMigrations_CreatesSchemaTable?mode=memory&cache=private")

	if err := RunMigrations(ctx, sqlDB, SQLiteDialect{}); err != nil {
		t.Fatalf("RunMigrations() error = %v", err)
	}

	// Query the schema_migrations table to confirm it exists.
	var count int
	err := sqlDB.QueryRowContext(ctx, "SELECT COUNT(*) FROM schema_migrations").Scan(&count)
	if err != nil {
		t.Errorf("schema_migrations table not found after RunMigrations: %v", err)
	}
}

func TestRunMigrations_RecordsMigrationFilename(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sqlDB := openTestDB(t, "file:TestRunMigrations_RecordsMigrationFilename?mode=memory&cache=private")

	if err := RunMigrations(ctx, sqlDB, SQLiteDialect{}); err != nil {
		t.Fatalf("RunMigrations() error = %v", err)
	}

	// Confirm the migration filename was recorded in schema_migrations.
	var count int
	err := sqlDB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM schema_migrations WHERE filename = '0001_initial_schema.up.sql'",
	).Scan(&count)
	if err != nil {
		t.Fatalf("schema_migrations query: %v", err)
	}
	if count != 1 {
		t.Errorf("schema_migrations row count for 0001 = %d, want 1", count)
	}
}

func TestRunMigrations_TablesExist(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sqlDB := openTestDB(t, "file:TestRunMigrations_TablesExist?mode=memory&cache=private")

	if err := RunMigrations(ctx, sqlDB, SQLiteDialect{}); err != nil {
		t.Fatalf("RunMigrations() error = %v", err)
	}

	// Spot-check that key tables from the initial schema were created.
	tables := []string{
		"users",
		"organizations",
		"org_memberships",
		"teams",
		"team_memberships",
		"service_accounts",
		"models",
		"model_aliases",
		"api_keys",
		"org_model_access",
		"team_model_access",
		"key_model_access",
		"usage_events",
	}
	for _, table := range tables {
		var name string
		err := sqlDB.QueryRowContext(ctx,
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q not found after migration: %v", table, err)
		}
	}
}
