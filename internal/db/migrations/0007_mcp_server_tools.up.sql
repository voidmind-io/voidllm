-- Migration: 0007_mcp_server_tools.up.sql
-- Description: Persistent cache of tool schemas fetched from upstream MCP servers.

CREATE TABLE mcp_server_tools (
    id            TEXT PRIMARY KEY,
    server_id     TEXT NOT NULL REFERENCES mcp_servers(id),
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    input_schema  TEXT NOT NULL DEFAULT '{}',
    fetched_at    TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (server_id, name)
);

CREATE INDEX idx_mcp_server_tools_server ON mcp_server_tools (server_id);
