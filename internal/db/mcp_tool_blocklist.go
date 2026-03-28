package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// toolBlocklistSelectColumns is the ordered column list used in all
// mcp_tool_blocklist SELECT queries. It must match the scan order in
// scanToolBlocklistEntry exactly.
const toolBlocklistSelectColumns = "id, server_id, tool_name, reason, created_by, created_at"

// ToolBlocklistEntry represents a tool blocked from Code Mode execution
// for a specific MCP server.
type ToolBlocklistEntry struct {
	ID        string  `json:"id"`
	ServerID  string  `json:"server_id"`
	ToolName  string  `json:"tool_name"`
	Reason    string  `json:"reason"`
	CreatedBy *string `json:"created_by"`
	CreatedAt string  `json:"created_at"`
}

// CreateToolBlocklistEntry inserts a new blocklist entry for the given MCP server
// and returns the persisted row. It returns ErrConflict if a blocklist entry for
// the same (server_id, tool_name) pair already exists.
func (d *DB) CreateToolBlocklistEntry(ctx context.Context, serverID, toolName, reason, createdBy string) (*ToolBlocklistEntry, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("create tool blocklist entry: generate id: %w", err)
	}

	p := d.dialect.Placeholder
	insertQuery := "INSERT INTO mcp_tool_blocklist (id, server_id, tool_name, reason, created_by, created_at) " +
		"VALUES (" + p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", CURRENT_TIMESTAMP)"

	selectQuery := "SELECT " + toolBlocklistSelectColumns +
		" FROM mcp_tool_blocklist WHERE id = " + p(1)

	var createdByArg any
	if createdBy != "" {
		createdByArg = createdBy
	}

	var entry *ToolBlocklistEntry
	err = d.WithTx(ctx, func(q Querier) error {
		_, execErr := q.ExecContext(ctx, insertQuery,
			id.String(),
			serverID,
			toolName,
			reason,
			createdByArg,
		)
		if execErr != nil {
			return translateError(execErr)
		}

		row := q.QueryRowContext(ctx, selectQuery, id.String())
		var scanErr error
		entry, scanErr = scanToolBlocklistEntry(row)
		return scanErr
	})
	if err != nil {
		return nil, fmt.Errorf("create tool blocklist entry: %w", err)
	}
	return entry, nil
}

// DeleteToolBlocklistEntry removes a blocklist entry identified by (serverID, toolName).
// It returns ErrNotFound if no matching entry exists.
func (d *DB) DeleteToolBlocklistEntry(ctx context.Context, serverID, toolName string) error {
	p := d.dialect.Placeholder
	query := "DELETE FROM mcp_tool_blocklist WHERE server_id = " + p(1) + " AND tool_name = " + p(2)

	result, err := d.sql.ExecContext(ctx, query, serverID, toolName)
	if err != nil {
		return fmt.Errorf("delete tool blocklist entry (%s, %s): %w", serverID, toolName, translateError(err))
	}

	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete tool blocklist entry (%s, %s) rows affected: %w", serverID, toolName, err)
	}
	if n == 0 {
		return fmt.Errorf("delete tool blocklist entry (%s, %s): %w", serverID, toolName, ErrNotFound)
	}

	return nil
}

// ListToolBlocklist returns all blocklist entries for the given MCP server,
// ordered by tool_name ascending.
func (d *DB) ListToolBlocklist(ctx context.Context, serverID string) ([]ToolBlocklistEntry, error) {
	p := d.dialect.Placeholder
	query := "SELECT " + toolBlocklistSelectColumns +
		" FROM mcp_tool_blocklist WHERE server_id = " + p(1) + " ORDER BY tool_name ASC"

	rows, err := d.sql.QueryContext(ctx, query, serverID)
	if err != nil {
		return nil, fmt.Errorf("list tool blocklist query: %w", err)
	}
	defer rows.Close()

	var entries []ToolBlocklistEntry
	for rows.Next() {
		e, scanErr := scanToolBlocklistEntry(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list tool blocklist scan: %w", scanErr)
		}
		entries = append(entries, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tool blocklist rows: %w", err)
	}

	return entries, nil
}

// ListBlockedToolNames returns the tool names from the blocklist for the given
// MCP server, ordered by tool_name ascending. It is intended for use by the
// executor to efficiently filter blocked tools without fetching full entries.
func (d *DB) ListBlockedToolNames(ctx context.Context, serverID string) ([]string, error) {
	p := d.dialect.Placeholder
	query := "SELECT tool_name FROM mcp_tool_blocklist WHERE server_id = " + p(1) + " ORDER BY tool_name ASC"

	rows, err := d.sql.QueryContext(ctx, query, serverID)
	if err != nil {
		return nil, fmt.Errorf("list blocked tool names query: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return nil, fmt.Errorf("list blocked tool names scan: %w", scanErr)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list blocked tool names rows: %w", err)
	}

	return names, nil
}

// scanToolBlocklistEntry scans a single blocklist row. The scanner may be a
// *sql.Row (from QueryRowContext) or *sql.Rows (from QueryContext); both satisfy
// the interface.
func scanToolBlocklistEntry(scanner interface{ Scan(...any) error }) (*ToolBlocklistEntry, error) {
	var e ToolBlocklistEntry
	err := scanner.Scan(&e.ID, &e.ServerID, &e.ToolName, &e.Reason, &e.CreatedBy, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}
