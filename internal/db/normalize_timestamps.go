package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
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
// long-running transaction open. After all batches complete, any rows that
// still do not match the canonical shape (because they could not be parsed)
// are counted and logged as a warning.
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
			n, execErr := normalizeBatch(ctx, runner, dialect, table, canonical, parseable, lastID, "", false)
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

		n, execErr := normalizeBatch(ctx, runner, dialect, table, canonical, parseable, lastID, upperID, true)
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
func normalizeBatch(ctx context.Context, runner migrationRunner, dialect Dialect, table, canonicalExpr, parseableExpr, lowerID, upperID string, hasUpper bool) (int64, error) {
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
		return 0, fmt.Errorf("update batch: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("batch rows affected: %w", err)
	}
	return n, nil
}

// normalizeAPIKeyExpiresAt normalizes the small api_keys table's expires_at
// column in a single pass (no batching — the table is bounded by the number
// of keys in the deployment, not by request volume). Values that cannot be
// parsed, including empty strings, are logged with the key ID and left
// unchanged.
func normalizeAPIKeyExpiresAt(ctx context.Context, runner migrationRunner, dialect Dialect, log *slog.Logger) error {
	p := dialect.Placeholder

	query := "SELECT id, expires_at FROM api_keys WHERE expires_at IS NOT NULL AND expires_at NOT LIKE " + p(1)
	rows, err := runner.QueryContext(ctx, query, canonicalTimestampLike)
	if err != nil {
		return fmt.Errorf("select non-canonical expires_at: %w", err)
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

		update := "UPDATE api_keys SET expires_at = " + p(1) + " WHERE id = " + p(2)
		if _, err := runner.ExecContext(ctx, update, FormatTimestamp(parsed), c.id); err != nil {
			return fmt.Errorf("update api_keys expires_at id %s: %w", c.id, err)
		}
	}

	return nil
}
