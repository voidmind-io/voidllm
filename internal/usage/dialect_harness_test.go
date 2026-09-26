package usage

// forEachDialect mirrors the harness of the same name in internal/db (see
// internal/db/dialect_harness_test.go). It is duplicated here rather than
// imported because the db package's version is unexported and internal/db's
// own test files cannot import internal/usage without creating an import
// cycle (internal/usage already imports internal/db in production code).
// Keeping a second, small copy scoped to this package's own real-write-path
// tests (Logger / MCPLogger) is simpler than restructuring either package.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
)

// postgresTestDSNEnv names the environment variable that opts this package's
// tests into also running against a real PostgreSQL server.
const postgresTestDSNEnv = "VOIDLLM_TEST_POSTGRES_DSN"

// forEachDialect runs fn once against an isolated in-memory SQLite database
// (via openTestDB, this package's existing helper) and, when
// VOIDLLM_TEST_POSTGRES_DSN is set, a second time against a freshly created,
// fully migrated PostgreSQL schema on the server addressed by that DSN.
func forEachDialect(t *testing.T, fn func(t *testing.T, d *db.DB)) {
	t.Helper()

	t.Run("sqlite", func(t *testing.T) {
		t.Parallel()
		dsn := "file:" + sanitizeDSNName(t.Name()) + "?mode=memory&cache=private"
		fn(t, openTestDB(t, dsn))
	})

	dsn := os.Getenv(postgresTestDSNEnv)
	if dsn == "" {
		return
	}

	t.Run("postgres", func(t *testing.T) {
		t.Parallel()
		fn(t, openMigratedPostgresDB(t, dsn))
	})
}

// openMigratedPostgresDB creates a fresh PostgreSQL schema on the server
// addressed by dsn, opens a connection pool scoped to it via a "search_path"
// query parameter, runs every migration against it, and registers cleanup
// that drops the schema and closes both connections. See the identical
// helper in internal/db/dialect_harness_test.go for the full rationale.
func openMigratedPostgresDB(t *testing.T, dsn string) *db.DB {
	t.Helper()
	ctx := context.Background()

	admin, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "postgres",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("openMigratedPostgresDB: open admin connection: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	schema := "vltest_" + randomHex(t, 8)
	if _, err := admin.SQL().ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("openMigratedPostgresDB: create schema %q: %v", schema, err)
	}
	t.Cleanup(func() {
		_, _ = admin.SQL().ExecContext(context.Background(), `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
	})

	scopedDSN, err := withSearchPath(dsn, schema)
	if err != nil {
		t.Fatalf("openMigratedPostgresDB: build scoped dsn: %v", err)
	}

	d, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "postgres",
		DSN:             scopedDSN,
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("openMigratedPostgresDB: open scoped connection: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })

	if err := db.RunMigrations(ctx, d.SQL(), d.Dialect(), slog.Default()); err != nil {
		t.Fatalf("openMigratedPostgresDB: RunMigrations: %v", err)
	}

	return d
}

// withSearchPath returns dsn with a "search_path" query parameter set to
// schema, preserving every other query parameter dsn already carries.
func withSearchPath(dsn, schema string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// randomHex returns n random bytes hex-encoded, failing the test if the
// system CSPRNG is unavailable.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("randomHex: %v", err)
	}
	return hex.EncodeToString(b)
}
