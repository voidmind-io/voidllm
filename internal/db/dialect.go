package db

import "strconv"

// Dialect abstracts SQL syntax differences between database drivers.
type Dialect interface {
	// Placeholder returns the query parameter placeholder for position n (1-based).
	// SQLite uses "?" for all positions; PostgreSQL uses "$1", "$2", etc.
	Placeholder(n int) string
	// SupportsMigrationLock reports whether the dialect supports advisory locking
	// during schema migrations. PostgreSQL supports pg_advisory_lock; SQLite does not.
	SupportsMigrationLock() bool
	// TimestampLessThan returns a SQL expression (without parameter binding) that
	// compares the named TEXT-typed timestamp column to a parameter, handling
	// format differences across drivers. The caller supplies the column name and
	// placeholder string; the dialect wraps both sides in a driver-appropriate
	// cast so string-level comparison is never used.
	//
	// Example (SQLite):   datetime(created_at) < datetime(?)
	// Example (Postgres): created_at::timestamptz < ($1)::timestamptz
	TimestampLessThan(column, placeholder string) string
	// CanonicalTimestamp returns a SQL expression that reformats the given TEXT
	// timestamp expression into the canonical UTC "YYYY-MM-DDTHH:MM:SSZ" shape
	// defined by TimestampLayout. expr may be a column name or any other SQL
	// expression that evaluates to a timestamp string in one of the formats
	// historically produced by a CURRENT_TIMESTAMP default. Used by the
	// timestamp normalization migration to rewrite existing rows in place.
	CanonicalTimestamp(expr string) string
	// ParseableTimestamp returns a SQL boolean expression that reports whether
	// the given TEXT column holds a value the dialect can parse as a timestamp.
	// It guards normalization updates so that rows with corrupt or unrecognized
	// data are skipped rather than causing the UPDATE to fail; such rows are
	// counted and reported as a residual instead.
	ParseableTimestamp(col string) string
	// CanonicalNowDefault returns a SQL expression suitable for use as a column
	// DEFAULT that produces the canonical UTC "YYYY-MM-DDTHH:MM:SSZ" timestamp
	// directly, so newly inserted rows never need normalization even before an
	// application binary that writes CreatedAt explicitly has rolled out to
	// every pod. PostgreSQL's CURRENT_TIMESTAMP default renders as a
	// timezone/locale-dependent text value that requires rewriting; SQLite
	// returns "" because its CURRENT_TIMESTAMP is already UTC and close enough
	// to canonical that no DEFAULT change is needed (the application write path
	// always supplies an explicit, already-canonical value regardless).
	CanonicalNowDefault() string
}

// SQLiteDialect implements Dialect for SQLite.
type SQLiteDialect struct{}

// Placeholder returns "?" for all positions, as required by the SQLite driver.
func (SQLiteDialect) Placeholder(_ int) string { return "?" }

// SupportsMigrationLock returns false because SQLite does not support advisory
// locks. SQLite's single-writer model makes migration locking unnecessary.
func (SQLiteDialect) SupportsMigrationLock() bool { return false }

// TimestampLessThan wraps both sides in datetime() which parses TEXT into a
// comparable form. datetime() accepts ISO-8601 with or without subseconds and
// both the "2006-01-02 15:04:05" and "2006-01-02T15:04:05Z" variants.
func (SQLiteDialect) TimestampLessThan(column, placeholder string) string {
	return "datetime(" + column + ") < datetime(" + placeholder + ")"
}

// CanonicalTimestamp uses strftime to reformat expr into the canonical
// "YYYY-MM-DDTHH:MM:SSZ" shape. strftime tolerates SQLite's own historical
// "YYYY-MM-DD HH:MM:SS" DEFAULT CURRENT_TIMESTAMP output as well as the
// canonical form itself, making this expression idempotent.
func (SQLiteDialect) CanonicalTimestamp(expr string) string {
	return "strftime('%Y-%m-%dT%H:%M:%SZ', " + expr + ")"
}

// ParseableTimestamp reports whether strftime can interpret col as a
// timestamp; strftime returns NULL for values it cannot parse.
func (SQLiteDialect) ParseableTimestamp(col string) string {
	return "strftime('%Y-%m-%dT%H:%M:%SZ', " + col + ") IS NOT NULL"
}

// CanonicalNowDefault returns "" — SQLite's existing CURRENT_TIMESTAMP default
// needs no dialect-specific replacement; see the Dialect interface doc.
func (SQLiteDialect) CanonicalNowDefault() string { return "" }

// PostgresDialect implements Dialect for PostgreSQL.
type PostgresDialect struct{}

// Placeholder returns a positional placeholder in the form "$n" as required by
// the PostgreSQL driver (e.g., "$1" for n=1, "$2" for n=2).
func (PostgresDialect) Placeholder(n int) string { return "$" + strconv.Itoa(n) }

// SupportsMigrationLock returns true because PostgreSQL supports advisory locks
// via pg_advisory_lock, which prevents concurrent migration runs in multi-replica
// deployments.
func (PostgresDialect) SupportsMigrationLock() bool { return true }

// TimestampLessThan casts both sides to timestamptz which parses any reasonable
// ISO-8601 variant and compares as absolute instants.
func (PostgresDialect) TimestampLessThan(column, placeholder string) string {
	return column + "::timestamptz < (" + placeholder + ")::timestamptz"
}

// CanonicalTimestamp casts expr (a TEXT column holding either the canonical
// shape or PostgreSQL's historical timestamptz-as-text DEFAULT
// CURRENT_TIMESTAMP output) to timestamptz, converts it to the UTC wall-clock
// time, and formats it back into the canonical "YYYY-MM-DDTHH:MM:SSZ" shape.
func (PostgresDialect) CanonicalTimestamp(expr string) string {
	return `to_char((` + expr + `)::timestamptz AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`
}

// ParseableTimestamp matches col against a loose timestamp-shaped prefix
// rather than attempting a cast, so a single unparseable row cannot abort the
// whole UPDATE statement (PostgreSQL evaluates a failed ::timestamptz cast as
// a hard error, not a NULL, even inside a WHERE clause).
func (PostgresDialect) ParseableTimestamp(col string) string {
	return col + ` ~ '^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}'`
}

// CanonicalNowDefault returns a to_char expression producing the canonical
// UTC "YYYY-MM-DDTHH:MM:SSZ" shape directly, for use as a column DEFAULT in
// place of PostgreSQL's timezone/locale-dependent CURRENT_TIMESTAMP text
// rendering.
func (PostgresDialect) CanonicalNowDefault() string {
	return `to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')`
}
