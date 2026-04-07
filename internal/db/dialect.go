package db

import "strconv"

// Dialect abstracts SQL syntax differences between database drivers.
type Dialect interface {
	// Placeholder returns the query parameter placeholder for position n (1-based).
	// SQLite uses "?" for all positions; PostgreSQL uses "$1", "$2", etc.
	Placeholder(n int) string
	// HourTrunc returns a SQL expression that truncates a timestamp column named
	// created_at to the nearest hour, producing an ISO-8601 string result.
	HourTrunc() string
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
}

// SQLiteDialect implements Dialect for SQLite.
type SQLiteDialect struct{}

// Placeholder returns "?" for all positions, as required by the SQLite driver.
func (SQLiteDialect) Placeholder(_ int) string { return "?" }

// HourTrunc returns a strftime expression that rounds created_at down to the
// hour boundary, producing a string in the form "2006-01-02T15:00:00Z".
func (SQLiteDialect) HourTrunc() string {
	return "strftime('%Y-%m-%dT%H:00:00Z', created_at)"
}

// SupportsMigrationLock returns false because SQLite does not support advisory
// locks. SQLite's single-writer model makes migration locking unnecessary.
func (SQLiteDialect) SupportsMigrationLock() bool { return false }

// TimestampLessThan wraps both sides in datetime() which parses TEXT into a
// comparable form. datetime() accepts ISO-8601 with or without subseconds and
// both the "2006-01-02 15:04:05" and "2006-01-02T15:04:05Z" variants.
func (SQLiteDialect) TimestampLessThan(column, placeholder string) string {
	return "datetime(" + column + ") < datetime(" + placeholder + ")"
}

// PostgresDialect implements Dialect for PostgreSQL.
type PostgresDialect struct{}

// Placeholder returns a positional placeholder in the form "$n" as required by
// the PostgreSQL driver (e.g., "$1" for n=1, "$2" for n=2).
func (PostgresDialect) Placeholder(n int) string { return "$" + strconv.Itoa(n) }

// HourTrunc returns a date_trunc expression that rounds created_at down to the
// hour boundary, producing a string in the form "2006-01-02T15:00:00Z" to match
// the ISO-8601 format produced by the SQLite dialect.
func (PostgresDialect) HourTrunc() string {
	return "to_char(date_trunc('hour', created_at), 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')"
}

// SupportsMigrationLock returns true because PostgreSQL supports advisory locks
// via pg_advisory_lock, which prevents concurrent migration runs in multi-replica
// deployments.
func (PostgresDialect) SupportsMigrationLock() bool { return true }

// TimestampLessThan casts both sides to timestamptz which parses any reasonable
// ISO-8601 variant and compares as absolute instants.
func (PostgresDialect) TimestampLessThan(column, placeholder string) string {
	return column + "::timestamptz < (" + placeholder + ")::timestamptz"
}
