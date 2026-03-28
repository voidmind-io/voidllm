-- Migration: 0005_code_mode_blocklist.up.sql
-- Description: Per-tool blocklist for Code Mode. Admins can block specific
-- tools from being available in Code Mode sandboxed execution.

CREATE TABLE mcp_tool_blocklist (
    id          TEXT PRIMARY KEY,
    server_id   TEXT NOT NULL REFERENCES mcp_servers(id),
    tool_name   TEXT NOT NULL,
    reason      TEXT NOT NULL DEFAULT '',
    created_by  TEXT,
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,

    UNIQUE (server_id, tool_name)
);

CREATE INDEX idx_mcp_tool_blocklist_server ON mcp_tool_blocklist (server_id);
