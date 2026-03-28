-- Migration: 0004_mcp_gateway.up.sql
-- Description: MCP Gateway — external MCP server registration, access control,
-- and tool call tracking. Mirrors the model access control pattern:
-- org_mcp_access grants an org access to a server, team_mcp_access restricts
-- that to a subset of teams, and key_mcp_access is an optional per-key override.
-- mcp_tool_calls is an append-only, fully denormalized event log (same philosophy
-- as usage_events — no foreign keys, survives soft-deletes of related entities).


-- =============================================================================
-- 1. MCP SERVERS
-- =============================================================================
-- Registry of external MCP servers. auth_token_enc is AES-256-GCM encrypted
-- via VOIDLLM_ENCRYPTION_KEY; NULL when auth_type = 'none'.
-- alias is a stable short name used in tool call logs and config references.
-- is_active: 0/1 integer flag; soft-delete is via deleted_at.

CREATE TABLE mcp_servers (
    id              TEXT PRIMARY KEY,
    name            TEXT NOT NULL,
    alias           TEXT NOT NULL,
    url             TEXT NOT NULL,
    auth_type       TEXT NOT NULL DEFAULT 'none',       -- 'none' | 'header' | 'bearer'
    auth_header     TEXT NOT NULL DEFAULT '',           -- header name, e.g. 'Authorization'
    auth_token_enc  TEXT,                               -- AES-256-GCM encrypted token, NULL if auth_type='none'
    org_id          TEXT REFERENCES organizations(id),  -- NULL for global servers
    team_id         TEXT REFERENCES teams(id),          -- NULL for global/org-scoped servers
    is_active       INTEGER NOT NULL DEFAULT 1,
    created_by      TEXT,                               -- NULL for system/yaml-sourced entries
    source          TEXT NOT NULL DEFAULT 'api',        -- 'api' | 'yaml'
    created_at      TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at      TEXT,
    CHECK (is_active IN (0, 1))
);

-- Scope-aware alias uniqueness: (org_id, team_id, alias) must be unique within
-- active (non-deleted) rows. NULL is mapped to '' via coalesce so that multiple
-- NULLs compare as equal (SQLite's NULL != NULL semantics would otherwise allow
-- unbounded duplicates). Soft-deleted rows are excluded from the constraint.
CREATE UNIQUE INDEX idx_mcp_servers_alias_scope
    ON mcp_servers (coalesce(org_id, ''), coalesce(team_id, ''), alias)
    WHERE deleted_at IS NULL;

-- Admin UI: list active servers quickly
CREATE INDEX idx_mcp_servers_active
    ON mcp_servers (alias)
    WHERE deleted_at IS NULL;


-- =============================================================================
-- 2. MCP ACCESS CONTROL (Explicit Allow)
-- =============================================================================
-- Empty table = NO access (explicit-allow policy, mirrors org/team_model_access).
-- Resolution order: key_mcp_access (if set) → team_mcp_access → org_mcp_access.

-- Org-level: which MCP servers can this org use?
CREATE TABLE org_mcp_access (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations(id),
    server_id   TEXT NOT NULL REFERENCES mcp_servers(id),
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,

    UNIQUE (org_id, server_id)
);

-- Team-level: subset of org's allowed servers
CREATE TABLE team_mcp_access (
    id          TEXT PRIMARY KEY,
    team_id     TEXT NOT NULL REFERENCES teams(id),
    server_id   TEXT NOT NULL REFERENCES mcp_servers(id),
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,

    UNIQUE (team_id, server_id)
);

-- Key-level: optional per-key override, subset of team's allowed servers
CREATE TABLE key_mcp_access (
    id          TEXT PRIMARY KEY,
    key_id      TEXT NOT NULL REFERENCES api_keys(id),
    server_id   TEXT NOT NULL REFERENCES mcp_servers(id),
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,

    UNIQUE (key_id, server_id)
);


-- =============================================================================
-- 3. MCP TOOL CALLS
-- =============================================================================
-- Append-only log of every MCP tool invocation routed through the gateway.
-- Fully denormalized on purpose (same philosophy as usage_events):
--   - No foreign keys (survives soft-deletes of related entities)
--   - All dimension IDs inline (no joins needed for aggregation)
--
-- server_alias is stored instead of server_id so the record remains meaningful
-- even if the server row is hard-deleted.
-- status: 'success' | 'error' | 'timeout'

CREATE TABLE mcp_tool_calls (
    id                  TEXT PRIMARY KEY,
    request_id          TEXT NOT NULL DEFAULT '',
    key_id              TEXT NOT NULL,
    key_type            TEXT NOT NULL,
    org_id              TEXT NOT NULL,
    team_id             TEXT,
    user_id             TEXT,
    service_account_id  TEXT,
    server_alias        TEXT NOT NULL,
    tool_name           TEXT NOT NULL,
    duration_ms         INTEGER,
    status              TEXT NOT NULL DEFAULT 'success',
    created_at          TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Primary query patterns: tool call history per org and per server in time range
CREATE INDEX idx_mcp_tool_calls_org    ON mcp_tool_calls (org_id, created_at);
CREATE INDEX idx_mcp_tool_calls_server ON mcp_tool_calls (server_alias, created_at);
