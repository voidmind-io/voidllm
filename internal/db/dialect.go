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
