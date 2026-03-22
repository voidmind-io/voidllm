-- =============================================================================
-- Migration: 0001_initial_schema.up.sql
-- =============================================================================
--
-- Conventions:
--   - All IDs are UUIDv7 (time-sortable, no collisions)
--   - All timestamps are UTC (TEXT stored as ISO 8601, CURRENT_TIMESTAMP default)
--   - Soft-delete via deleted_at (NULL = active)
--   - 0 = unlimited for all limit fields
--   - Explicit-allow for model access (empty allowlist = no access)
--   - Key prefixes: vl_uk_ (user), vl_tk_ (team), vl_sa_ (service account)
--   - API key hashing: SHA-256 + Salt (not bcrypt -- hot path performance)
--   - Upstream API keys: AES-256-GCM encrypted via VOIDLLM_ENCRYPTION_KEY
--
-- Entity Hierarchy:
--
--   users
--     ├── org_memberships ──→ organizations
--     └── team_memberships ──→ teams ──→ organizations
--                                └──→ service_accounts
--
--   models (from YAML and/or API)
--     ├── model_aliases (org/team-scoped)
--     ├── org_model_access (allowlist per org)
--     ├── team_model_access (subset of org allowlist)
--     └── key_model_access (optional per-key override)
--
--   api_keys (user/team/sa-scoped, always org-linked)
--
--   usage_events (denormalized, append-only, no FKs)
--


-- =============================================================================
-- 1. USERS
-- =============================================================================
-- User identity. Supports local auth (email + password) and external IdP (OIDC).
-- is_system_admin is a global flag, not tied to any org.

CREATE TABLE users (
    id              TEXT PRIMARY KEY,
    email           TEXT NOT NULL UNIQUE,
    display_name    TEXT NOT NULL,
    password_hash   TEXT,                               -- NULL for SSO/OIDC users
    auth_provider   TEXT NOT NULL DEFAULT 'local',      -- 'local' | 'oidc'
    external_id     TEXT,                               -- ID from external IdP
    is_system_admin INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at      TEXT
);

-- Fast lookup for OIDC login: find user by provider + external ID
CREATE UNIQUE INDEX idx_users_external
    ON users (auth_provider, external_id)
    WHERE external_id IS NOT NULL;


-- =============================================================================
-- 2. ORGANIZATIONS
-- =============================================================================
-- Top-level entity. Everything (teams, keys, usage) rolls up to an org.
-- timezone is optional, reserved for future per-org usage aggregation.

CREATE TABLE organizations (
    id                    TEXT PRIMARY KEY,
    name                  TEXT NOT NULL,
    slug                  TEXT NOT NULL UNIQUE,          -- URL-safe identifier
    timezone              TEXT,                          -- e.g. 'Europe/Berlin'
    daily_token_limit     INTEGER NOT NULL DEFAULT 0,     -- 0 = unlimited
    monthly_token_limit   INTEGER NOT NULL DEFAULT 0,
    requests_per_minute   INTEGER NOT NULL DEFAULT 0,
    requests_per_day      INTEGER NOT NULL DEFAULT 0,
    created_at            TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at            TEXT
);


-- =============================================================================
-- 3. ORG MEMBERSHIPS
-- =============================================================================
-- Links users to organizations with a role.
-- A user can belong to an org without being in any team (e.g. org admin).

CREATE TABLE org_memberships (
    id         TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL REFERENCES organizations(id),
    user_id    TEXT NOT NULL REFERENCES users(id),
    role       TEXT NOT NULL DEFAULT 'member',           -- 'org_admin' | 'member'
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,

    UNIQUE (org_id, user_id)
);


-- =============================================================================
-- 4. TEAMS
-- =============================================================================
-- Belongs to exactly one org. Slug is unique within the org.
-- Limits default to 0 (unlimited) and inherit from org via most-restrictive-wins.

CREATE TABLE teams (
    id                    TEXT PRIMARY KEY,
    org_id                TEXT NOT NULL REFERENCES organizations(id),
    name                  TEXT NOT NULL,
    slug                  TEXT NOT NULL,
    daily_token_limit     INTEGER NOT NULL DEFAULT 0,
    monthly_token_limit   INTEGER NOT NULL DEFAULT 0,
    requests_per_minute   INTEGER NOT NULL DEFAULT 0,
    requests_per_day      INTEGER NOT NULL DEFAULT 0,
    created_at            TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at            TEXT,

    UNIQUE (org_id, slug)
);


-- =============================================================================
-- 5. TEAM MEMBERSHIPS
-- =============================================================================
-- Links users to teams with a role. A user can be in multiple teams.

CREATE TABLE team_memberships (
    id         TEXT PRIMARY KEY,
    team_id    TEXT NOT NULL REFERENCES teams(id),
    user_id    TEXT NOT NULL REFERENCES users(id),
    role       TEXT NOT NULL DEFAULT 'member',           -- 'team_admin' | 'member'
    created_at TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,

    UNIQUE (team_id, user_id)
);


-- =============================================================================
-- 6. SERVICE ACCOUNTS
-- =============================================================================
-- Non-human identity for CI/CD, automation, etc.
-- team_id NULL = org-scoped, team_id NOT NULL = team-scoped.

CREATE TABLE service_accounts (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    org_id      TEXT NOT NULL REFERENCES organizations(id),
    team_id     TEXT REFERENCES teams(id),              -- NULL = org-scoped
    created_by  TEXT NOT NULL REFERENCES users(id),
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at  TEXT
);


-- =============================================================================
-- 7. MODELS
-- =============================================================================
-- Upstream LLM providers. Can be loaded from YAML (source='yaml') or created
-- via Admin API (source='api'). YAML-sourced models are refreshed on restart.
-- Upstream API keys are encrypted at rest via VOIDLLM_ENCRYPTION_KEY.
-- aliases: space-separated list of extra names that route to this model.
-- timeout: Go duration string (e.g. "30s", "2m"). Empty = global defaults.

CREATE TABLE models (
    id                    TEXT PRIMARY KEY,
    name                  TEXT NOT NULL UNIQUE,          -- Canonical: 'gpt-oss-120b'
    provider              TEXT NOT NULL,                 -- 'vllm' | 'openai' | 'anthropic' | 'azure' | 'custom'
    base_url              TEXT NOT NULL,                 -- 'http://vllm-large:8000/v1'
    api_key_encrypted     TEXT,                          -- AES-256-GCM, NULL if no key needed
    max_context_tokens    INTEGER,
    input_price_per_1m    REAL,                          -- NULL = no pricing configured
    output_price_per_1m   REAL,
    -- Azure-specific (only used when provider = 'azure')
    azure_deployment      TEXT,                          -- Azure deployment name
    azure_api_version     TEXT,                          -- e.g. '2024-02-01'

    aliases               TEXT NOT NULL DEFAULT '',      -- space-separated extra route names
    timeout               TEXT NOT NULL DEFAULT '',      -- Go duration string, empty = global default
    is_active             INTEGER NOT NULL DEFAULT 1,
    source                TEXT NOT NULL DEFAULT 'api',   -- 'yaml' | 'api'
    created_by            TEXT REFERENCES users(id),     -- NULL for yaml-sourced
    created_at            TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at            TEXT
);


-- =============================================================================
-- 8. MODEL ALIASES
-- =============================================================================
-- Dynamic aliases per org or team. Resolution order: team → org → yaml → exact.
-- Global aliases are defined in voidllm.yaml, not in this table.

CREATE TABLE model_aliases (
    id          TEXT PRIMARY KEY,
    alias       TEXT NOT NULL,
    model_name  TEXT NOT NULL,                           -- References models.name
    scope_type  TEXT NOT NULL,                           -- 'org' | 'team'
    org_id      TEXT REFERENCES organizations(id),
    team_id     TEXT REFERENCES teams(id),
    created_by  TEXT NOT NULL REFERENCES users(id),
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- One alias per scope (team "alpha" and team "beta" can both have alias "default")
CREATE UNIQUE INDEX idx_model_aliases_unique
    ON model_aliases (alias, scope_type,
        COALESCE(org_id, '00000000-0000-0000-0000-000000000000'),
        COALESCE(team_id, '00000000-0000-0000-0000-000000000000'));


-- =============================================================================
-- 9. API KEYS
-- =============================================================================
-- Auth tokens for proxy access. Hashed with SHA-256 + Salt.
-- key_hint stores a preview for identification: 'vd_uk_a3f2...****'
-- org_id is always set (denormalized) to avoid joins on auth check.
--
-- Key types:
--   user_key  (vl_uk_...) → user_id set, team_id set
--   team_key  (vl_tk_...) → team_id set, user_id NULL
--   sa_key    (vl_sa_...) → service_account_id set

CREATE TABLE api_keys (
    id                    TEXT PRIMARY KEY,
    key_hash              TEXT NOT NULL UNIQUE,           -- HMAC-SHA256(secret, key)
    key_hint              TEXT NOT NULL,                  -- 'vl_uk_a3f2...e8b1'
    key_type              TEXT NOT NULL,                  -- 'user_key' | 'team_key' | 'sa_key'
    name                  TEXT NOT NULL DEFAULT '',

    -- Scope: exactly one of user/team/sa is the "owner", org is always set
    org_id                TEXT NOT NULL REFERENCES organizations(id),
    team_id               TEXT REFERENCES teams(id),
    user_id               TEXT REFERENCES users(id),
    service_account_id    TEXT REFERENCES service_accounts(id),

    -- Limits (0 = unlimited, inherits from team/org via most-restrictive-wins)
    daily_token_limit     INTEGER NOT NULL DEFAULT 0,
    monthly_token_limit   INTEGER NOT NULL DEFAULT 0,
    requests_per_minute   INTEGER NOT NULL DEFAULT 0,
    requests_per_day      INTEGER NOT NULL DEFAULT 0,

    expires_at            TEXT,                           -- NULL = no expiration
    last_used_at          TEXT,
    created_by            TEXT NOT NULL REFERENCES users(id),
    created_at            TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at            TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at            TEXT
);

-- Auth hot path: lookup by hash (only active keys)
CREATE INDEX idx_api_keys_hash
    ON api_keys (key_hash)
    WHERE deleted_at IS NULL;


-- =============================================================================
-- 10. MODEL ACCESS CONTROL (Explicit Allow)
-- =============================================================================
-- Empty table = NO access (explicit-allow policy).
-- Most-restrictive-wins: org allows subset of all models,
-- team allows subset of org, key allows subset of team.

-- Org-level: which models can this org use?
CREATE TABLE org_model_access (
    id          TEXT PRIMARY KEY,
    org_id      TEXT NOT NULL REFERENCES organizations(id),
    model_name  TEXT NOT NULL,                           -- References models.name
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,

    UNIQUE (org_id, model_name)
);

-- Team-level: subset of org's allowed models
CREATE TABLE team_model_access (
    id          TEXT PRIMARY KEY,
    team_id     TEXT NOT NULL REFERENCES teams(id),
    model_name  TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,

    UNIQUE (team_id, model_name)
);

-- Key-level: optional override, subset of team's allowed models
CREATE TABLE key_model_access (
    id          TEXT PRIMARY KEY,
    key_id      TEXT NOT NULL REFERENCES api_keys(id),
    model_name  TEXT NOT NULL,
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,

    UNIQUE (key_id, model_name)
);


-- =============================================================================
-- 11. USAGE EVENTS
-- =============================================================================
-- Append-only log of every proxy request. Fully denormalized on purpose:
--   - No foreign keys (survives soft-deletes of related entities)
--   - All dimension IDs inline (no joins needed for aggregation)
--   - Indexes optimized for time-range queries per org/team/key/user
--
-- request_id: correlates this event with audit_logs and proxy traces.
-- Aggregation is on-demand, not pre-computed.

CREATE TABLE usage_events (
    id                    TEXT PRIMARY KEY,
    request_id            TEXT NOT NULL DEFAULT '',
    key_id                TEXT NOT NULL,
    key_type              TEXT NOT NULL,
    org_id                TEXT NOT NULL,
    team_id               TEXT,
    user_id               TEXT,
    service_account_id    TEXT,
    model_name            TEXT NOT NULL,
    prompt_tokens         INTEGER NOT NULL DEFAULT 0,
    completion_tokens     INTEGER NOT NULL DEFAULT 0,
    total_tokens          INTEGER NOT NULL DEFAULT 0,
    cost_estimate         REAL,                           -- NULL if no pricing configured
    request_duration_ms   INTEGER,
    ttft_ms               INTEGER,                        -- Time to First Token (NULL for non-streaming)
    tokens_per_second     REAL,                           -- completion_tokens / duration
    status_code           INTEGER NOT NULL,
    created_at            TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Primary query patterns: usage per org/team/key/user in time range
CREATE INDEX idx_usage_org_time  ON usage_events (org_id, created_at);
CREATE INDEX idx_usage_team_time ON usage_events (team_id, created_at) WHERE team_id IS NOT NULL;
CREATE INDEX idx_usage_key_time  ON usage_events (key_id, created_at);
CREATE INDEX idx_usage_user_time ON usage_events (user_id, created_at);


-- =============================================================================
-- 12. USAGE HOURLY
-- =============================================================================
-- Pre-aggregated hourly rollups per key + model + hour bucket.
-- Drives dashboard charts and rate-limit accounting without full table scans.
-- team_id and user_id default to '' (not NULL) to allow composite PK simplicity.

CREATE TABLE usage_hourly (
    org_id              TEXT NOT NULL,
    team_id             TEXT NOT NULL DEFAULT '',
    user_id             TEXT NOT NULL DEFAULT '',
    key_id              TEXT NOT NULL,
    model_name          TEXT NOT NULL,
    bucket_hour         TEXT NOT NULL,
    request_count       INTEGER NOT NULL DEFAULT 0,
    prompt_tokens       INTEGER NOT NULL DEFAULT 0,
    completion_tokens   INTEGER NOT NULL DEFAULT 0,
    total_tokens        INTEGER NOT NULL DEFAULT 0,
    cost_sum            REAL NOT NULL DEFAULT 0,
    duration_sum_ms     REAL NOT NULL DEFAULT 0,
    ttft_sum_ms         REAL NOT NULL DEFAULT 0,
    ttft_count          INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (key_id, model_name, bucket_hour)
);

CREATE INDEX idx_usage_hourly_org  ON usage_hourly (org_id, bucket_hour);
CREATE INDEX idx_usage_hourly_team ON usage_hourly (team_id, bucket_hour)
    WHERE team_id != '';


-- =============================================================================
-- 13. AUDIT LOGS
-- =============================================================================
-- Append-only record of admin and system actions. Fully denormalized (no FKs),
-- same philosophy as usage_events: survives entity soft/hard deletes.
--
-- actor_type:   'user' | 'service_account' | 'system'
-- actor_key_id: api_key id used for the action (empty string if not applicable)
-- request_id:   correlates with usage_events and proxy traces
-- status_code:  HTTP status of the response (0 if not request-driven)

CREATE TABLE audit_logs (
    id            TEXT PRIMARY KEY,
    request_id    TEXT NOT NULL DEFAULT '',
    timestamp     TEXT NOT NULL,
    org_id        TEXT NOT NULL DEFAULT '',
    actor_id      TEXT NOT NULL DEFAULT '',
    actor_type    TEXT NOT NULL DEFAULT '',
    actor_key_id  TEXT NOT NULL DEFAULT '',
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   TEXT NOT NULL DEFAULT '',
    description   TEXT NOT NULL DEFAULT '',
    ip_address    TEXT NOT NULL DEFAULT '',
    status_code   INTEGER NOT NULL DEFAULT 0
);

-- Primary query patterns: audit trail per org in time range, per actor, per resource
CREATE INDEX idx_audit_logs_org_time  ON audit_logs (org_id, timestamp);
CREATE INDEX idx_audit_logs_actor     ON audit_logs (actor_id, timestamp);
CREATE INDEX idx_audit_logs_resource  ON audit_logs (resource_type, resource_id);


-- =============================================================================
-- 14. INVITE TOKENS
-- =============================================================================
-- Single-use org invitation links. token_hash is the HMAC-SHA256 of the raw
-- token; token_hint is a short display prefix for the admin UI.
-- redeemed_at NULL = still valid (subject to expires_at).

CREATE TABLE invite_tokens (
    id          TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    token_hint  TEXT NOT NULL,
    org_id      TEXT NOT NULL REFERENCES organizations(id),
    email       TEXT NOT NULL,
    role        TEXT NOT NULL DEFAULT 'member',
    expires_at  TEXT NOT NULL,
    redeemed_at TEXT,
    created_by  TEXT NOT NULL REFERENCES users(id),
    created_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_invite_tokens_hash ON invite_tokens (token_hash);
CREATE UNIQUE INDEX idx_invite_tokens_active_email
    ON invite_tokens (org_id, email) WHERE redeemed_at IS NULL;


-- =============================================================================
-- 15. ORG SSO CONFIG
-- =============================================================================
-- Per-org OIDC SSO configuration. One row per org (enforced by UNIQUE constraint).
-- client_secret_enc is AES-256-GCM encrypted via VOIDLLM_ENCRYPTION_KEY.
-- scopes and allowed_domains are stored as JSON arrays.

CREATE TABLE org_sso_config (
    id                TEXT PRIMARY KEY,
    org_id            TEXT NOT NULL,
    enabled           INTEGER NOT NULL DEFAULT 0,
    issuer            TEXT NOT NULL DEFAULT '',
    client_id         TEXT NOT NULL DEFAULT '',
    client_secret_enc TEXT NOT NULL DEFAULT '',
    redirect_url      TEXT NOT NULL DEFAULT '',
    scopes            TEXT NOT NULL DEFAULT '["openid","email","profile"]',
    allowed_domains   TEXT NOT NULL DEFAULT '[]',
    auto_provision    INTEGER NOT NULL DEFAULT 1,
    default_role      TEXT NOT NULL DEFAULT 'member',
    group_sync        INTEGER NOT NULL DEFAULT 0,
    group_claim       TEXT NOT NULL DEFAULT 'groups',
    created_at        TEXT NOT NULL DEFAULT '',
    updated_at        TEXT NOT NULL DEFAULT '',
    UNIQUE (org_id)
);

CREATE INDEX idx_org_sso_config_org ON org_sso_config (org_id);


-- =============================================================================
-- 16. SETTINGS
-- =============================================================================
-- Key-value store for global runtime configuration (e.g. license key,
-- feature flags). Keyed by a well-known string; value is always TEXT.

CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL DEFAULT (CURRENT_TIMESTAMP)
);
