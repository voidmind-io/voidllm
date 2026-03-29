package db

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// mcpServerToolSelectColumns is the ordered column list used in all
// mcp_server_tools SELECT queries. It must match the scan order in
// scanMCPServerTool exactly.
const mcpServerToolSelectColumns = "id, server_id, name, description, input_schema, fetched_at"

// MCPServerTool represents a cached tool schema fetched from an upstream MCP server.
type MCPServerTool struct {
	ID          string `json:"id"`
	ServerID    string `json:"server_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	InputSchema string `json:"input_schema"` // JSON text
	FetchedAt   string `json:"fetched_at"`
}

// UpsertServerTools replaces the full tool set for the given server in a single
// transaction. All existing rows for server_id are deleted before the new tools
// are inserted, making this a full cache refresh operation.
func (d *DB) UpsertServerTools(ctx context.Context, serverID string, tools []MCPServerTool) error {
	p := d.dialect.Placeholder

	deleteQuery := "DELETE FROM mcp_server_tools WHERE server_id = " + p(1)
	insertQuery := "INSERT INTO mcp_server_tools (id, server_id, name, description, input_schema, fetched_at) " +
		"VALUES (" + p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", CURRENT_TIMESTAMP)"

	err := d.WithTx(ctx, func(q Querier) error {
		if _, execErr := q.ExecContext(ctx, deleteQuery, serverID); execErr != nil {
			return fmt.Errorf("delete existing tools: %w", translateError(execErr))
		}

		for _, tool := range tools {
			id, genErr := uuid.NewV7()
			if genErr != nil {
				return fmt.Errorf("generate id for tool %q: %w", tool.Name, genErr)
			}
			if _, execErr := q.ExecContext(ctx, insertQuery,
				id.String(),
				serverID,
				tool.Name,
				tool.Description,
				tool.InputSchema,
			); execErr != nil {
				return fmt.Errorf("insert tool %q: %w", tool.Name, translateError(execErr))
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("upsert server tools for %s: %w", serverID, err)
	}
	return nil
}

// ListServerTools returns all cached tools for the given MCP server,
// ordered by name ascending.
func (d *DB) ListServerTools(ctx context.Context, serverID string) ([]MCPServerTool, error) {
	p := d.dialect.Placeholder
	query := "SELECT " + mcpServerToolSelectColumns +
		" FROM mcp_server_tools WHERE server_id = " + p(1) + " ORDER BY name ASC"

	rows, err := d.sql.QueryContext(ctx, query, serverID)
	if err != nil {
		return nil, fmt.Errorf("list server tools query: %w", err)
	}
	defer rows.Close()

	var tools []MCPServerTool
	for rows.Next() {
		t, scanErr := scanMCPServerTool(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("list server tools scan: %w", scanErr)
		}
		tools = append(tools, *t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list server tools rows: %w", err)
	}

	return tools, nil
}

// ListAllServerTools returns a map of server alias to tool list for every active,
// non-deleted MCP server that has cached tools. A single JOIN query loads all data.
// The map is keyed by server alias (not server ID).
func (d *DB) ListAllServerTools(ctx context.Context) (map[string][]MCPServerTool, error) {
	query := "SELECT t.id, t.server_id, t.name, t.description, t.input_schema, t.fetched_at, s.alias" +
		" FROM mcp_server_tools t" +
		" JOIN mcp_servers s ON s.id = t.server_id" +
		" WHERE s.is_active = 1 AND s.deleted_at IS NULL" +
		" ORDER BY s.alias ASC, t.name ASC"

	rows, err := d.sql.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list all server tools query: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]MCPServerTool)
	for rows.Next() {
		var t MCPServerTool
		var alias string
		if scanErr := rows.Scan(
			&t.ID, &t.ServerID, &t.Name, &t.Description, &t.InputSchema, &t.FetchedAt,
			&alias,
		); scanErr != nil {
			return nil, fmt.Errorf("list all server tools scan: %w", scanErr)
		}
		result[alias] = append(result[alias], t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list all server tools rows: %w", err)
	}

	return result, nil
}

// DeleteServerTools removes all cached tools for the given MCP server.
// It is a no-op if no tools are cached for that server.
func (d *DB) DeleteServerTools(ctx context.Context, serverID string) error {
	p := d.dialect.Placeholder
	query := "DELETE FROM mcp_server_tools WHERE server_id = " + p(1)

	if _, err := d.sql.ExecContext(ctx, query, serverID); err != nil {
		return fmt.Errorf("delete server tools for %s: %w", serverID, translateError(err))
	}
	return nil
}

// scanMCPServerTool scans a single mcp_server_tools row. The scanner may be a
// *sql.Row (from QueryRowContext) or *sql.Rows (from QueryContext); both satisfy
// the interface.
func scanMCPServerTool(scanner interface{ Scan(...any) error }) (*MCPServerTool, error) {
	var t MCPServerTool
	err := scanner.Scan(&t.ID, &t.ServerID, &t.Name, &t.Description, &t.InputSchema, &t.FetchedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
