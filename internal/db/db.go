package db

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib" // registers "pgx" driver for database/sql
	_ "modernc.org/sqlite"

	"github.com/voidmind-io/voidllm/internal/config"
)

// DB wraps *sql.DB with dialect awareness and transaction support.
type DB struct {
	sql     *sql.DB
	dialect Dialect
}

// Open opens and configures a database connection from cfg.
// Supported drivers: "sqlite", "postgres".
// For SQLite, WAL mode and foreign key enforcement pragmas are applied immediately
// after the connection pool is established.
func Open(ctx context.Context, cfg config.DatabaseConfig) (*DB, error) {
	var (
		sqlDB   *sql.DB
		dialect Dialect
		err     error
	)

	switch cfg.Driver {
	case "sqlite":
		dialect = SQLiteDialect{}
		sqlDB, err = sql.Open("sqlite", cfg.DSN)
		if err != nil {
			return nil, fmt.Errorf("open sqlite: %w", err)
		}

		// SQLite is single-writer; pin to one connection so that pragmas (which
		// are per-connection) always apply regardless of which pooled connection
		// executes a query. This also eliminates SQLITE_BUSY contention from
		// concurrent writers acquiring separate connections.
		sqlDB.SetMaxOpenConns(1)
		sqlDB.SetMaxIdleConns(1)
		sqlDB.SetConnMaxLifetime(0) // connections are cheap and long-lived in single-conn mode

		// Pragmas are per-connection. With MaxOpenConns=1 above, these settings
		// will always be in effect for every query issued against this pool.
		pragmas := []string{
			"PRAGMA foreign_keys = ON;",
			"PRAGMA journal_mode = WAL;",
			"PRAGMA synchronous = NORMAL;",
			"PRAGMA busy_timeout = 5000;",
		}
		for _, p := range pragmas {
			if _, err := sqlDB.ExecContext(ctx, p); err != nil {
				_ = sqlDB.Close()
				return nil, fmt.Errorf("apply sqlite pragma %q: %w", p, err)
			}
		}

	case "postgres":
		sqlDB, err = sql.Open("pgx", cfg.DSN)
		if err != nil {
			return nil, fmt.Errorf("open postgres: %w", err)
		}
		sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
		sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
		sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
		sqlDB.SetConnMaxIdleTime(cfg.ConnMaxLifetime / 2)
		dialect = PostgresDialect{}

	default:
		return nil, fmt.Errorf("unsupported database driver: %q", cfg.Driver)
	}

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &DB{sql: sqlDB, dialect: dialect}, nil
}

// Close releases the underlying database connection pool.
func (db *DB) Close() error {
	return db.sql.Close()
}

// Ping verifies the database is reachable.
func (db *DB) Ping(ctx context.Context) error {
	return db.sql.PingContext(ctx)
}

// SQL returns the underlying *sql.DB. It is intended for use by the migration
// runner and should not be used for application queries.
func (db *DB) SQL() *sql.DB {
	return db.sql
}

// Dialect returns the SQL dialect for this connection.
func (db *DB) Dialect() Dialect {
	return db.dialect
}

// WithTx executes fn inside a read-committed transaction.
// The transaction is rolled back automatically if fn returns an error or panics.
// Callers must not retain or use the Querier after fn returns.
func (db *DB) WithTx(ctx context.Context, fn func(q Querier) error) error {
	tx, err := db.sql.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // deliberate: no-op after Commit

	if err := fn(tx); err != nil {
		return err
	}

	return tx.Commit()
}
