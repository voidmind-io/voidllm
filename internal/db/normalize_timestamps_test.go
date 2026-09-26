package db

// Tests for normalizeEventTimestamps / NormalizeEventTimestamps (see
// normalize_timestamps.go). These exercise the migration's per-row rewrite
// logic directly against an already-fully-migrated database, following the
// same "re-apply the migration's own logic against seeded legacy data"
// approach as migration_0015_legacy_test.go: RunMigrations always runs 0018
// to completion during openMigratedDB/forEachDialect setup (when the tables
// are still empty), so legacy rows are seeded afterward via raw SQL and the
// exported NormalizeEventTimestamps entry point is invoked again directly.

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"
)

// insertUsageEventRaw inserts a minimal usage_events row with created_at set
// to the literal raw string, bypassing FormatTimestamp entirely so the test
// can seed exactly the historical shapes a pre-fix installation would have
// written.
func insertUsageEventRaw(t *testing.T, d *DB, id, createdAt string) {
	t.Helper()
	query := fmt.Sprintf(
		`INSERT INTO usage_events (id, key_id, key_type, org_id, model_name, status_code, created_at)
		 VALUES ('%s', 'key-1', 'user_key', 'org-1', 'test-model', 200, '%s')`,
		id, createdAt,
	)
	if _, err := d.sql.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("insertUsageEventRaw id=%q createdAt=%q: %v", id, createdAt, err)
	}
}

// insertUsageEventDefaultCreatedAt inserts a minimal usage_events row without
// specifying created_at at all, so the column's own DEFAULT fires.
func insertUsageEventDefaultCreatedAt(t *testing.T, d *DB, id string) {
	t.Helper()
	query := fmt.Sprintf(
		`INSERT INTO usage_events (id, key_id, key_type, org_id, model_name, status_code)
		 VALUES ('%s', 'key-1', 'user_key', 'org-1', 'test-model', 200)`,
		id,
	)
	if _, err := d.sql.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("insertUsageEventDefaultCreatedAt id=%q: %v", id, err)
	}
}

// insertMCPToolCallRaw inserts a minimal mcp_tool_calls row with created_at
// set to the literal raw string.
func insertMCPToolCallRaw(t *testing.T, d *DB, id, createdAt string) {
	t.Helper()
	query := fmt.Sprintf(
		`INSERT INTO mcp_tool_calls (id, key_id, key_type, org_id, server_alias, tool_name, status, code_mode, created_at)
		 VALUES ('%s', 'key-1', 'user_key', 'org-1', 'server-a', 'tool-a', 'success', 0, '%s')`,
		id, createdAt,
	)
	if _, err := d.sql.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("insertMCPToolCallRaw id=%q createdAt=%q: %v", id, createdAt, err)
	}
}

// newSortableID returns a fresh UUIDv7 string, time-sortable like the IDs the
// real insert paths generate, so seeded rows interact correctly with the
// migration's id-keyset batching.
func newSortableID(t *testing.T) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("uuid.NewV7: %v", err)
	}
	return id.String()
}

// runNormalize invokes the exported NormalizeEventTimestamps entry point
// against d, fataling the test on error (normalization itself must never
// fail; unparseable rows are reported via log, not error).
func runNormalize(t *testing.T, d *DB) {
	t.Helper()
	if err := NormalizeEventTimestamps(context.Background(), d.SQL(), d.Dialect(), slog.Default()); err != nil {
		t.Fatalf("NormalizeEventTimestamps: %v", err)
	}
}

// ---- legacy literal shapes ----------------------------------------------------

// TestNormalizeEventTimestamps_LegacyLiterals seeds usage_events and
// mcp_tool_calls rows with the exact legacy shapes each dialect's own
// CURRENT_TIMESTAMP default historically produced and confirms
// NormalizeEventTimestamps rewrites every one of them to the canonical
// TimestampLayout shape.
func TestNormalizeEventTimestamps_LegacyLiterals(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		var legacyLiterals []string
		switch d.Dialect().(type) {
		case SQLiteDialect:
			// SQLite's own DEFAULT CURRENT_TIMESTAMP rendering: space
			// separated, always UTC, no fractional seconds, no offset.
			legacyLiterals = []string{"2026-09-26 21:23:17"}
		case PostgresDialect:
			// PostgreSQL's text rendering of a timestamptz DEFAULT
			// CURRENT_TIMESTAMP: fractional seconds and a two-digit zone
			// offset without a colon, in both a UTC (+00) and a non-UTC
			// (+02) shape.
			legacyLiterals = []string{
				"2026-09-26 21:23:17.123456+00",
				"2026-09-26 23:23:17+02",
			}
		default:
			t.Fatalf("unhandled dialect %T", d.Dialect())
		}

		const want = "2026-09-26T21:23:17Z"

		for _, literal := range legacyLiterals {
			usageID := newSortableID(t)
			mcpID := newSortableID(t)
			insertUsageEventRaw(t, d, usageID, literal)
			insertMCPToolCallRaw(t, d, mcpID, literal)

			runNormalize(t, d)

			if got := rawColumnValue(t, d, "usage_events", "created_at", usageID); got != want {
				t.Errorf("usage_events.created_at for literal %q = %q, want %q", literal, got, want)
			}
			if got := rawColumnValue(t, d, "mcp_tool_calls", "created_at", mcpID); got != want {
				t.Errorf("mcp_tool_calls.created_at for literal %q = %q, want %q", literal, got, want)
			}
		}
	})
}

// TestNormalizeEventTimestamps_RFC3339WithOffset seeds a row whose created_at
// is already an RFC3339 timestamp but carries a non-UTC offset (the shape a
// caller who bypassed FormatTimestamp, or an older binary with a different
// bug, might have written) and confirms it is rewritten to canonical UTC.
func TestNormalizeEventTimestamps_RFC3339WithOffset(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		id := newSortableID(t)
		insertUsageEventRaw(t, d, id, "2026-09-26T23:23:17+02:00")

		runNormalize(t, d)

		const want = "2026-09-26T21:23:17Z"
		if got := rawColumnValue(t, d, "usage_events", "created_at", id); got != want {
			t.Errorf("created_at after normalize = %q, want %q", got, want)
		}
	})
}

// TestNormalizeEventTimestamps_CanonicalRowUnchanged seeds a row already in
// the canonical shape and confirms normalization leaves it byte-for-byte
// identical.
func TestNormalizeEventTimestamps_CanonicalRowUnchanged(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		id := newSortableID(t)
		want := FormatTimestamp(time.Date(2026, 9, 26, 21, 23, 17, 0, time.UTC))
		insertUsageEventRaw(t, d, id, want)

		runNormalize(t, d)

		if got := rawColumnValue(t, d, "usage_events", "created_at", id); got != want {
			t.Errorf("canonical created_at changed by normalize: got %q, want unchanged %q", got, want)
		}
	})
}

// TestNormalizeEventTimestamps_UnparseableRowLeftUntouched seeds a row whose
// created_at is garbage — data corruption or a value normalizeEventTimestamps
// has never seen — and confirms normalization does not fail and leaves the
// row exactly as it was, rather than aborting or corrupting it further.
func TestNormalizeEventTimestamps_UnparseableRowLeftUntouched(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		id := newSortableID(t)
		const garbage = "not-a-timestamp-at-all"
		insertUsageEventRaw(t, d, id, garbage)

		runNormalize(t, d)

		if got := rawColumnValue(t, d, "usage_events", "created_at", id); got != garbage {
			t.Errorf("unparseable created_at = %q, want untouched %q", got, garbage)
		}
	})
}

// TestNormalizeEventTimestamps_Idempotent seeds a mix of legacy and canonical
// rows, normalizes twice, and confirms the second run is a complete no-op —
// every row's created_at after the second run equals its value after the
// first.
func TestNormalizeEventTimestamps_Idempotent(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		var legacyLiteral string
		switch d.Dialect().(type) {
		case SQLiteDialect:
			legacyLiteral = "2026-09-26 21:23:17"
		case PostgresDialect:
			legacyLiteral = "2026-09-26 21:23:17.123456+00"
		default:
			t.Fatalf("unhandled dialect %T", d.Dialect())
		}

		legacyID := newSortableID(t)
		canonicalID := newSortableID(t)
		insertUsageEventRaw(t, d, legacyID, legacyLiteral)
		insertUsageEventRaw(t, d, canonicalID, FormatTimestamp(time.Now()))

		runNormalize(t, d)
		afterFirst := map[string]string{
			legacyID:    rawColumnValue(t, d, "usage_events", "created_at", legacyID),
			canonicalID: rawColumnValue(t, d, "usage_events", "created_at", canonicalID),
		}

		runNormalize(t, d)
		afterSecond := map[string]string{
			legacyID:    rawColumnValue(t, d, "usage_events", "created_at", legacyID),
			canonicalID: rawColumnValue(t, d, "usage_events", "created_at", canonicalID),
		}

		for id, want := range afterFirst {
			if got := afterSecond[id]; got != want {
				t.Errorf("row %s changed on second normalize run: got %q, want unchanged %q", id, got, want)
			}
		}
	})
}

// TestNormalizeEventTimestamps_MultiBatch lowers normalizeBatchSize to 2 and
// seeds more legacy rows than that in each of usage_events and
// mcp_tool_calls, forcing normalizeCreatedAtColumn's keyset-batched loop
// through multiple iterations, and confirms every row is still normalized
// correctly regardless of how many batches it took.
//
// This test deliberately does not call t.Parallel() anywhere in its call
// chain (including via forEachDialect) because it mutates the package-level
// normalizeBatchSize variable; running while other tests' parallel subtests
// are executing would race. Go only runs t.Parallel() subtests after every
// non-parallel top-level test (this one included) has returned, so as long
// as this function's body runs entirely synchronously the mutation is safe.
func TestNormalizeEventTimestamps_MultiBatch(t *testing.T) {
	original := normalizeBatchSize
	normalizeBatchSize = 2
	defer func() { normalizeBatchSize = original }()

	const rowsPerTable = 5

	check := func(t *testing.T, d *DB, legacyLiteral string) {
		t.Helper()
		usageIDs := make([]string, rowsPerTable)
		mcpIDs := make([]string, rowsPerTable)
		for i := range rowsPerTable {
			usageIDs[i] = newSortableID(t)
			insertUsageEventRaw(t, d, usageIDs[i], legacyLiteral)
			mcpIDs[i] = newSortableID(t)
			insertMCPToolCallRaw(t, d, mcpIDs[i], legacyLiteral)
		}

		runNormalize(t, d)

		const want = "2026-09-26T21:23:17Z"
		for _, id := range usageIDs {
			if got := rawColumnValue(t, d, "usage_events", "created_at", id); got != want {
				t.Errorf("usage_events row %s = %q, want %q (multi-batch path)", id, got, want)
			}
		}
		for _, id := range mcpIDs {
			if got := rawColumnValue(t, d, "mcp_tool_calls", "created_at", id); got != want {
				t.Errorf("mcp_tool_calls row %s = %q, want %q (multi-batch path)", id, got, want)
			}
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

// ---- DEFAULT column behavior --------------------------------------------------

// TestNormalizeEventTimestamps_SQLiteDefaultGetsNormalized inserts a
// usage_events row without specifying created_at at all on SQLite, so the
// column's own DEFAULT CURRENT_TIMESTAMP fires (SQLite's default is left
// unchanged by migration 0018 — see CanonicalNowDefault), producing the
// legacy space-separated shape, and confirms NormalizeEventTimestamps still
// catches and rewrites it.
func TestNormalizeEventTimestamps_SQLiteDefaultGetsNormalized(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	id := newSortableID(t)
	insertUsageEventDefaultCreatedAt(t, d, id)

	before := rawColumnValue(t, d, "usage_events", "created_at", id)
	if before == "" {
		t.Fatal("created_at is empty after insert relying on DEFAULT")
	}

	runNormalize(t, d)

	got := rawColumnValue(t, d, "usage_events", "created_at", id)
	if _, err := time.Parse(TimestampLayout, got); err != nil {
		t.Errorf("created_at after normalize = %q, want canonical shape: %v", got, err)
	}
}

// TestNormalizeEventTimestamps_PostgresDefaultAlreadyCanonical inserts a
// usage_events row without specifying created_at on PostgreSQL, where
// migration 0018 already changed the column DEFAULT to a canonical-shaped
// expression (see CanonicalNowDefault), and confirms the row is canonical
// immediately — no normalization pass is needed for rows written after the
// migration ran, even by a caller that (incorrectly) omits created_at.
func TestNormalizeEventTimestamps_PostgresDefaultAlreadyCanonical(t *testing.T) {
	t.Parallel()

	dsn := postgresTestDSN()
	if dsn == "" {
		t.Skip("VOIDLLM_TEST_POSTGRES_DSN not set")
	}

	d := openMigratedPostgresDB(t, dsn)
	id := newSortableID(t)
	insertUsageEventDefaultCreatedAt(t, d, id)

	got := rawColumnValue(t, d, "usage_events", "created_at", id)
	if _, err := time.Parse(TimestampLayout, got); err != nil {
		t.Errorf("created_at from PostgreSQL DEFAULT = %q, want already canonical: %v", got, err)
	}
}

// ---- api_keys.expires_at ------------------------------------------------------

// TestNormalizeAPIKeyExpiresAt covers the api_keys.expires_at half of
// NormalizeEventTimestamps: legacy-shaped values are rewritten to canonical
// UTC, and unparseable values (including an empty string) are left in place
// without failing the overall normalization pass.
func TestNormalizeAPIKeyExpiresAt(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		org := mustCreateOrg(t, d, CreateOrgParams{Name: "Expiry Test Org", Slug: "expiry-test-" + newSortableID(t)})
		user := mustCreateUser(t, d, CreateUserParams{
			Email:        "expiry-" + newSortableID(t) + "@example.com",
			DisplayName:  "Expiry Tester",
			PasswordHash: testPasswordHash(t),
		})

		mustCreateTestKey := func(expiresAt *string) *APIKey {
			t.Helper()
			key, err := d.CreateAPIKey(ctx, CreateAPIKeyParams{
				KeyHash:   "hash-" + newSortableID(t),
				KeyHint:   "vl_uk_test",
				KeyType:   "user_key",
				Name:      "expiry test key",
				OrgID:     org.ID,
				UserID:    &user.ID,
				ExpiresAt: expiresAt,
				CreatedBy: user.ID,
			})
			if err != nil {
				t.Fatalf("CreateAPIKey: %v", err)
			}
			return key
		}

		legacy := "2026-09-26 21:23:17"
		empty := ""
		garbage := "not-a-timestamp"

		legacyKey := mustCreateTestKey(&legacy)
		emptyKey := mustCreateTestKey(&empty)
		garbageKey := mustCreateTestKey(&garbage)
		nilKey := mustCreateTestKey(nil)

		runNormalize(t, d)

		const want = "2026-09-26T21:23:17Z"
		if got := rawColumnValue(t, d, "api_keys", "expires_at", legacyKey.ID); got != want {
			t.Errorf("legacy expires_at after normalize = %q, want %q", got, want)
		}

		var gotEmpty, gotGarbage string
		if err := d.sql.QueryRowContext(ctx, "SELECT expires_at FROM api_keys WHERE id = "+d.dialect.Placeholder(1), emptyKey.ID).Scan(&gotEmpty); err != nil {
			t.Fatalf("read empty expires_at: %v", err)
		}
		if gotEmpty != "" {
			t.Errorf("empty-string expires_at after normalize = %q, want left untouched (empty)", gotEmpty)
		}
		if err := d.sql.QueryRowContext(ctx, "SELECT expires_at FROM api_keys WHERE id = "+d.dialect.Placeholder(1), garbageKey.ID).Scan(&gotGarbage); err != nil {
			t.Fatalf("read garbage expires_at: %v", err)
		}
		if gotGarbage != garbage {
			t.Errorf("garbage expires_at after normalize = %q, want left untouched %q", gotGarbage, garbage)
		}

		var nilExpiresAt sql.NullString
		if err := d.sql.QueryRowContext(ctx,
			"SELECT expires_at FROM api_keys WHERE id = "+d.dialect.Placeholder(1), nilKey.ID,
		).Scan(&nilExpiresAt); err != nil {
			t.Fatalf("read nil expires_at: %v", err)
		}
		if nilExpiresAt.Valid {
			t.Errorf("nil expires_at after normalize = %q, want still NULL", nilExpiresAt.String)
		}
	})
}
