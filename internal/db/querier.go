package db

import (
	"context"
	"database/sql"
)

// Querier is the minimal database interface accepted by all query functions.
// Both *sql.DB and *sql.Tx satisfy this interface, allowing the same query
// code to run inside or outside a transaction without modification.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
