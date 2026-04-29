package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// sqliteTimestampLayout is the format produced by SQLite's CURRENT_TIMESTAMP
// function when the column type is TEXT. PostgreSQL uses RFC3339.
const sqliteTimestampLayout = "2006-01-02 15:04:05"

// SaveOutputSchema upserts the inferred JSON Schema for a single tool on the
// given MCP server. If a row already exists for the (server_id, tool_name) pair
// its schema_json and inferred_at are updated to the new values.
func (d *DB) SaveOutputSchema(ctx context.Context, serverID, toolName string, schema jsonx.RawMessage) error {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("save output schema: generate id: %w", err)
	}

	p := d.dialect.Placeholder
	query := "INSERT INTO output_schemas (id, server_id, tool_name, schema_json, inferred_at) " +
		"VALUES (" + p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", CURRENT_TIMESTAMP) " +
		"ON CONFLICT(server_id, tool_name) DO UPDATE SET " +
		"schema_json = excluded.schema_json, inferred_at = CURRENT_TIMESTAMP"

	if _, err := d.sql.ExecContext(ctx, query, id.String(), serverID, toolName, string(schema)); err != nil {
		return fmt.Errorf("save output schema (%s, %s): %w", serverID, toolName, translateError(err))
	}
	return nil
}

// GetAllOutputSchemas returns all stored output schemas for the given MCP server
// as a map of tool name to raw JSON schema. When maxAge is greater than zero,
// rows whose inferred_at timestamp is older than maxAge are excluded from the
// result. When maxAge is zero or negative all rows are returned regardless of age.
func (d *DB) GetAllOutputSchemas(ctx context.Context, serverID string, maxAge time.Duration) (map[string]jsonx.RawMessage, error) {
	p := d.dialect.Placeholder
	query := "SELECT tool_name, schema_json, inferred_at FROM output_schemas WHERE server_id = " + p(1)

	rows, err := d.sql.QueryContext(ctx, query, serverID)
	if err != nil {
		return nil, fmt.Errorf("get all output schemas query: %w", err)
	}
	defer rows.Close()

	result := make(map[string]jsonx.RawMessage)
	for rows.Next() {
		var toolName, schemaJSON, inferredAtRaw string
		if scanErr := rows.Scan(&toolName, &schemaJSON, &inferredAtRaw); scanErr != nil {
			return nil, fmt.Errorf("get all output schemas scan: %w", scanErr)
		}
		if maxAge > 0 {
			inferredAt, parseErr := parseTimestamp(inferredAtRaw)
			if parseErr != nil {
				return nil, fmt.Errorf("get all output schemas: parse inferred_at %q: %w", inferredAtRaw, parseErr)
			}
			if time.Since(inferredAt) > maxAge {
				continue
			}
		}
		result[toolName] = jsonx.RawMessage(schemaJSON)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("get all output schemas rows: %w", err)
	}

	return result, nil
}

// IsOutputSchemaStale reports whether the stored output schema for the given
// (serverID, toolName) pair is absent or older than maxAge. A missing row is
// treated as stale so the caller re-infers the schema. When maxAge is zero or
// negative the schema is never considered stale provided a row exists.
func (d *DB) IsOutputSchemaStale(ctx context.Context, serverID, toolName string, maxAge time.Duration) (bool, error) {
	p := d.dialect.Placeholder
	query := "SELECT inferred_at FROM output_schemas WHERE server_id = " + p(1) + " AND tool_name = " + p(2)

	var inferredAtRaw string
	err := d.sql.QueryRowContext(ctx, query, serverID, toolName).Scan(&inferredAtRaw)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		return false, fmt.Errorf("is output schema stale (%s, %s): %w", serverID, toolName, err)
	}

	if maxAge <= 0 {
		return false, nil
	}

	inferredAt, parseErr := parseTimestamp(inferredAtRaw)
	if parseErr != nil {
		return false, fmt.Errorf("is output schema stale: parse inferred_at %q: %w", inferredAtRaw, parseErr)
	}

	return time.Since(inferredAt) > maxAge, nil
}

// parseTimestamp parses a TEXT timestamp as written by CURRENT_TIMESTAMP from
// either SQLite ("2006-01-02 15:04:05") or PostgreSQL (RFC3339 / RFC3339Nano).
// All values are interpreted as UTC.
func parseTimestamp(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse(sqliteTimestampLayout, s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp format %q", s)
}
