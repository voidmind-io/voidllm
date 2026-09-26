package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/jackc/pgx/v5/pgconn"
)

// normalizeBatchSize is the number of rows rewritten per UPDATE statement
// while normalizing existing created_at values. It is a package variable
// rather than a constant so tests can lower it to exercise the multi-batch
// path without needing thousands of rows.
var normalizeBatchSize = 5000

// normalizeProgressEvery controls how often (in batches) progress is logged
// while normalizing a table's created_at column.
const normalizeProgressEvery = 20

// normalizeEventTimestamps rewrites usage_events.created_at and
// mcp_tool_calls.created_at into the canonical TimestampLayout shape, then
// normalizes api_keys.expires_at the same way. It is registered as the Go
// migration "0018_canonical_event_timestamps.go" (see goMigrations in
// migrate.go) and is also exposed via the exported NormalizeEventTimestamps
// so the `voidllm migrate` command can run the same logic against a freshly
// copied target database as a safety net.
//
// Rows whose created_at cannot be parsed at all are left untouched; their
// count is logged as a warning rather than failing the migration, since a
// single corrupt historical row must never block startup.
func normalizeEventTimestamps(ctx context.Context, runner migrationRunner, dialect Dialect, log *slog.Logger) error {
	if defaultExpr := dialect.CanonicalNowDefault(); defaultExpr != "" {
		for _, table := range []string{"usage_events", "mcp_tool_calls"} {
			alter := "ALTER TABLE " + table + " ALTER COLUMN created_at SET DEFAULT " + defaultExpr
			if _, err := runner.ExecContext(ctx, alter); err != nil {
				return fmt.Errorf("set canonical created_at default on %s: %w", table, err)
			}
		}
	}

	for _, table := range []string{"usage_events", "mcp_tool_calls"} {
		if err := normalizeCreatedAtColumn(ctx, runner, dialect, table, log); err != nil {
			return fmt.Errorf("normalize %s.created_at: %w", table, err)
		}
	}

	if err := normalizeAPIKeyExpiresAt(ctx, runner, dialect, log); err != nil {
		return fmt.Errorf("normalize api_keys.expires_at: %w", err)
	}

	return nil
}

// NormalizeEventTimestamps rewrites usage_events.created_at,
// mcp_tool_calls.created_at, and api_keys.expires_at into the canonical
// TimestampLayout shape in place, in batches, without requiring downtime. It
// is applied automatically by RunMigrations and is exported so the
// `voidllm migrate` command can run the same normalization against a freshly
// copied target database. Unparseable rows are left untouched and reported
// via log as warnings; this function never returns an error solely because
// of unparseable data.
func NormalizeEventTimestamps(ctx context.Context, sqlDB *sql.DB, dialect Dialect, log *slog.Logger) error {
	return normalizeEventTimestamps(ctx, sqlDB, dialect, log)
}

// normalizeCreatedAtColumn rewrites table's created_at column in
// id-keyset-ordered batches of normalizeBatchSize rows. Each batch is applied
// as its own UPDATE statement (autocommitting outside any enclosing
// transaction), so a table with millions of rows never holds a single
// long-running transaction open. If a batch UPDATE itself fails — for
// example because dialect.ParseableTimestamp let a digit-shaped but
// calendrically invalid value through its guard and dialect.CanonicalTimestamp's
// cast then rejected it — that batch falls back to a row-by-row Go-side
// rewrite (see normalizeBatchFallback) instead of aborting the whole
// migration; a single corrupt row must never block startup. After all
// batches complete, any rows that still do not match the canonical shape
// (because they could not be parsed by either path) are counted and logged
// as a warning.
func normalizeCreatedAtColumn(ctx context.Context, runner migrationRunner, dialect Dialect, table string, log *slog.Logger) error {
	p := dialect.Placeholder
	canonical := dialect.CanonicalTimestamp("created_at")
	parseable := dialect.ParseableTimestamp("created_at")

	lastID := ""
	batches := 0
	var rewritten int64

	for {
		var upperID string
		selectUpper := "SELECT id FROM " + table +
			" WHERE id > " + p(1) +
			" ORDER BY id LIMIT 1 OFFSET " + strconv.Itoa(normalizeBatchSize-1)
		err := runner.QueryRowContext(ctx, selectUpper, lastID).Scan(&upperID)
		if errors.Is(err, sql.ErrNoRows) {
			n, execErr := normalizeBatch(ctx, runner, dialect, table, canonical, parseable, lastID, "", false, log)
			if execErr != nil {
				return execErr
			}
			rewritten += n
			batches++
			break
		}
		if err != nil {
			return fmt.Errorf("find batch upper bound: %w", err)
		}

		n, execErr := normalizeBatch(ctx, runner, dialect, table, canonical, parseable, lastID, upperID, true, log)
		if execErr != nil {
			return execErr
		}
		rewritten += n
		batches++
		lastID = upperID

		if batches%normalizeProgressEvery == 0 {
			log.LogAttrs(ctx, slog.LevelInfo, "normalizing event timestamps: progress",
				slog.String("table", table),
				slog.Int("batches", batches),
				slog.Int64("rows_rewritten", rewritten),
			)
		}
	}

	log.LogAttrs(ctx, slog.LevelInfo, "normalizing event timestamps: table complete",
		slog.String("table", table),
		slog.Int("batches", batches),
		slog.Int64("rows_rewritten", rewritten),
	)

	var residual int64
	countQuery := "SELECT COUNT(*) FROM " + table + " WHERE created_at NOT LIKE " + p(1)
	if err := runner.QueryRowContext(ctx, countQuery, canonicalTimestampLike).Scan(&residual); err != nil {
		return fmt.Errorf("count residual non-canonical rows: %w", err)
	}
	if residual > 0 {
		log.LogAttrs(ctx, slog.LevelWarn, "residual non-canonical created_at values after normalization",
			slog.String("table", table),
			slog.Int64("count", residual),
		)
	}

	return nil
}

// normalizeBatch rewrites created_at for rows in (lowerID, upperID] — or, when
// hasUpper is false, every remaining row with id > lowerID — restricted to
// rows that are not already canonical and that the dialect can parse. It
// returns the number of rows actually rewritten.
//
// dialect.ParseableTimestamp is a cheap, loose guard (a digit-shape regex on
// PostgreSQL) rather than a real parse, so it can let through a value that is
// shaped like a timestamp but is not actually one (e.g. "2026-13-40 99:99:99",
// a nonexistent month and day). When that happens, dialect.CanonicalTimestamp's
// cast fails and the batch UPDATE itself errors. Rather than propagate that
// error — which would abort RunMigrations and block startup on a single bad
// historical row — the batch falls back to normalizeBatchFallback, which
// parses each candidate row individually in Go and skips (rather than fails
// on) the ones that do not actually parse.
func normalizeBatch(ctx context.Context, runner migrationRunner, dialect Dialect, table, canonicalExpr, parseableExpr, lowerID, upperID string, hasUpper bool, log *slog.Logger) (int64, error) {
	p := dialect.Placeholder
	argN := 1

	query := "UPDATE " + table + " SET created_at = " + canonicalExpr + " WHERE id > " + p(argN)
	args := []any{lowerID}
	argN++

	if hasUpper {
		query += " AND id <= " + p(argN)
		args = append(args, upperID)
		argN++
	}

	query += " AND created_at NOT LIKE " + p(argN)
	args = append(args, canonicalTimestampLike)
	argN++

	query += " AND " + parseableExpr

	result, err := runner.ExecContext(ctx, query, args...)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelWarn, "batch UPDATE failed while normalizing timestamps, falling back to row-by-row normalization",
			slog.String("table", table),
			slog.String("id_lower", lowerID),
			slog.String("id_upper", upperID),
			slog.Bool("has_upper", hasUpper),
			slog.String("error_class", errorClass(err)),
		)
		return normalizeBatchFallback(ctx, runner, dialect, table, lowerID, upperID, hasUpper, log)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("batch rows affected: %w", err)
	}
	return n, nil
}

// normalizeBatchFallback rewrites created_at for the same id range a failed
// batch UPDATE covered (see normalizeBatch), one row at a time in Go rather
// than via a single dialect-cast SQL expression. It selects every
// non-canonical row in range, parses each created_at with
// ParseStoredTimestamp, and issues an individual UPDATE for the ones that
// parse. Rows that do not parse are counted and skipped — their id (never
// the stored value) is logged at Warn — since a single corrupt row must not
// block startup. Only a failure of this function's own SELECT or UPDATE
// statements (e.g. the database becoming unavailable mid-migration) returns
// an error; per-row parse failures never do.
func normalizeBatchFallback(ctx context.Context, runner migrationRunner, dialect Dialect, table, lowerID, upperID string, hasUpper bool, log *slog.Logger) (int64, error) {
	p := dialect.Placeholder
	argN := 1

	selectQuery := "SELECT id, created_at FROM " + table + " WHERE id > " + p(argN)
	args := []any{lowerID}
	argN++

	if hasUpper {
		selectQuery += " AND id <= " + p(argN)
		args = append(args, upperID)
		argN++
	}

	selectQuery += " AND created_at NOT LIKE " + p(argN)
	args = append(args, canonicalTimestampLike)

	rows, err := runner.QueryContext(ctx, selectQuery, args...)
	if err != nil {
		return 0, fmt.Errorf("fallback select batch: %w", err)
	}

	type candidate struct {
		id        string
		createdAt string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.createdAt); err != nil {
			rows.Close() //nolint:errcheck // already returning the scan error
			return 0, fmt.Errorf("fallback scan row: %w", err)
		}
		candidates = append(candidates, c)
	}
	closeErr := rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("fallback iterate rows: %w", err)
	}
	if closeErr != nil {
		return 0, fmt.Errorf("fallback close rows: %w", closeErr)
	}

	var rewritten, skipped int64
	for _, c := range candidates {
		parsed, err := ParseStoredTimestamp(c.createdAt)
		if err != nil {
			skipped++
			log.LogAttrs(ctx, slog.LevelWarn, "row could not be normalized, leaving as-is",
				slog.String("table", table),
				slog.String("id", c.id),
			)
			continue
		}

		update := "UPDATE " + table + " SET created_at = " + p(1) + " WHERE id = " + p(2)
		if _, err := runner.ExecContext(ctx, update, FormatTimestamp(parsed), c.id); err != nil {
			return rewritten, fmt.Errorf("fallback update row %s: %w", c.id, err)
		}
		rewritten++
	}

	if skipped > 0 {
		log.LogAttrs(ctx, slog.LevelWarn, "batch fallback: rows could not be parsed and were left as-is",
			slog.String("table", table),
			slog.Int64("count", skipped),
		)
	}

	return rewritten, nil
}

// errorClass extracts a coarse, value-free classification of err for logging
// alongside a failed batch UPDATE. A PostgreSQL driver error (*pgconn.PgError)
// carries a SQLSTATE code identifying the failure category (e.g. "22007" for
// invalid_datetime_format) without including the value that triggered it;
// PostgreSQL's own error message would (e.g. `invalid input syntax for type
// timestamp: "2026-13-40 99:99:99"`), so it is never logged. Any other error
// is classified only by its Go type for the same reason.
func errorClass(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return fmt.Sprintf("%T", err)
}

// normalizeAPIKeyExpiresAt validates and normalizes the small api_keys
// table's expires_at column in a single pass (no batching — the table is
// bounded by the number of keys in the deployment, not by request volume).
// Every non-NULL value is parsed in Go, regardless of whether it already
// matches the canonical shape, because a value can be shape-valid and still
// calendrically invalid (e.g. "9999-99-99T99:99:99Z") — a LIKE-based shape
// check alone would treat such a value as already canonical and never
// inspect it. Values that parse and whose canonical rendering differs from
// what is stored are rewritten; values that cannot be parsed at all,
// including empty strings and shape-valid-but-invalid ones, are logged with
// the key ID only (never the stored value) and left unchanged.
func normalizeAPIKeyExpiresAt(ctx context.Context, runner migrationRunner, dialect Dialect, log *slog.Logger) error {
	p := dialect.Placeholder

	query := "SELECT id, expires_at FROM api_keys WHERE expires_at IS NOT NULL"
	rows, err := runner.QueryContext(ctx, query)
	if err != nil {
		return fmt.Errorf("select api_keys expires_at: %w", err)
	}

	type candidate struct {
		id  string
		raw string
	}
	var candidates []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.id, &c.raw); err != nil {
			rows.Close() //nolint:errcheck // already returning the scan error
			return fmt.Errorf("scan api_keys expires_at row: %w", err)
		}
		candidates = append(candidates, c)
	}
	closeErr := rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate api_keys expires_at rows: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("close api_keys expires_at rows: %w", closeErr)
	}

	for _, c := range candidates {
		parsed, err := ParseStoredTimestamp(c.raw)
		if err != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "api key expires_at could not be normalized, leaving as-is",
				slog.String("key_id", c.id),
			)
			continue
		}

		canonical := FormatTimestamp(parsed)
		if canonical == c.raw {
			continue
		}

		update := "UPDATE api_keys SET expires_at = " + p(1) + " WHERE id = " + p(2)
		if _, err := runner.ExecContext(ctx, update, canonical, c.id); err != nil {
			return fmt.Errorf("update api_keys expires_at id %s: %w", c.id, err)
		}
	}

	return nil
}
