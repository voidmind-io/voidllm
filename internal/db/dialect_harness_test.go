package db

// forEachDialect is the shared cross-dialect test harness for this package.
// Every test that must prove behavior identical on both supported database
// engines (in particular the canonical-timestamp normalization and query
// range fixes) calls forEachDialect instead of openMigratedDB directly.
//
// SQLite always runs, via the existing openMigratedDB helper (see
// orgs_test.go). PostgreSQL additionally runs whenever the
// VOIDLLM_TEST_POSTGRES_DSN environment variable is set, pointed at a
// throwaway schema created fresh for that one subtest and dropped on
// cleanup, so PostgreSQL subtests never interfere with each other or leave
// state behind even when the suite is interrupted mid-run.

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
)

// postgresTestDSNEnv names the environment variable that opts this package's
// tests into also running against a real PostgreSQL server. It is unset in
// ordinary `go test` runs, so forEachDialect exercises SQLite only unless a
// caller deliberately points it at a running PostgreSQL instance.
const postgresTestDSNEnv = "VOIDLLM_TEST_POSTGRES_DSN"

// postgresTestDSN returns the VOIDLLM_TEST_POSTGRES_DSN environment variable,
// or "" when it is unset. Tests that need to build their own PostgreSQL
// subtest outside of forEachDialect (because they cannot use t.Parallel(),
// for example) call this directly.
func postgresTestDSN() string {
	return os.Getenv(postgresTestDSNEnv)
}

// forEachDialect runs fn once against an isolated in-memory SQLite database
// and, when VOIDLLM_TEST_POSTGRES_DSN is set, a second time against a freshly
// created, fully migrated PostgreSQL schema on the server addressed by that
// DSN. Each dialect runs as its own named subtest ("sqlite" / "postgres") in
// parallel, so a failure names the dialect unambiguously and the two never
// share connection state.
func forEachDialect(t *testing.T, fn func(t *testing.T, d *DB)) {
	t.Helper()

	t.Run("sqlite", func(t *testing.T) {
		t.Parallel()
		fn(t, openMigratedDB(t))
	})

	dsn := postgresTestDSN()
	if dsn == "" {
		return
	}

	t.Run("postgres", func(t *testing.T) {
		t.Parallel()
		fn(t, openMigratedPostgresDB(t, dsn))
	})
}

// openMigratedPostgresDB creates a fresh PostgreSQL schema on the server
// addressed by dsn, opens a second connection pool pointed at that schema via
// a "search_path" parameter appended to dsn, runs every migration against it,
// and registers cleanup that drops the schema (CASCADE) and closes both
// connections. The schema name is a random suffix rather than anything
// derived from the test name, so concurrent callers — including other test
// binaries pointed at the same VOIDLLM_TEST_POSTGRES_DSN server — can never
// collide.
func openMigratedPostgresDB(t *testing.T, dsn string) *DB {
	t.Helper()
	ctx := context.Background()

	admin, err := Open(ctx, config.DatabaseConfig{
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

	d, err := Open(ctx, config.DatabaseConfig{
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

	if err := RunMigrations(ctx, d.SQL(), d.Dialect(), slog.Default()); err != nil {
		t.Fatalf("openMigratedPostgresDB: RunMigrations: %v", err)
	}

	return d
}

// withSearchPath returns dsn with a "search_path" query parameter set to
// schema, preserving every other query parameter dsn already carries (e.g.
// sslmode, timezone). pgx's stdlib driver sends any query parameter it does
// not itself interpret as a PostgreSQL connection run-time parameter, so this
// is equivalent to `SET search_path = schema` on every connection opened from
// the resulting pool.
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
// system CSPRNG is unavailable (it never is in practice; this only guards
// against a nil error path so the helper cannot silently return "").
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("randomHex: %v", err)
	}
	return hex.EncodeToString(b)
}
