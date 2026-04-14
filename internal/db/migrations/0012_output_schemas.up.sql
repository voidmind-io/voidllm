-- Migration: 0012_output_schemas.up.sql
-- Description: Stores inferred JSON Schema documents for MCP tool outputs, keyed by server and tool name.

CREATE TABLE output_schemas (
    id            TEXT PRIMARY KEY,
    server_id     TEXT NOT NULL REFERENCES mcp_servers(id) ON DELETE CASCADE,
    tool_name     TEXT NOT NULL,
    schema_json   TEXT NOT NULL,
    inferred_at   TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (server_id, tool_name)
);

CREATE INDEX idx_output_schemas_server ON output_schemas (server_id);
