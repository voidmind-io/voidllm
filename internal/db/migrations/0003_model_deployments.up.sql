-- Migration: 0003_model_deployments.up.sql
-- Description: Add model_deployments table for multi-deployment load balancing.
-- Each deployment is a concrete upstream endpoint for a model. The parent model
-- gains a strategy column (e.g. "round-robin", "weighted", "priority") and a
-- max_retries column controlling how many deployments are tried per request.

-- Per-deployment upstream endpoint definition.
-- weight:   relative probability used for weighted routing (ignored by other strategies).
-- priority: lower value = higher priority, used by the priority strategy.
-- is_active: 0/1 integer flag; soft-delete is via deleted_at.
CREATE TABLE model_deployments (
    id                TEXT PRIMARY KEY,
    model_id          TEXT NOT NULL REFERENCES models(id),
    name              TEXT NOT NULL,
    provider          TEXT NOT NULL,
    base_url          TEXT NOT NULL,
    api_key_encrypted TEXT,
    azure_deployment  TEXT NOT NULL DEFAULT '',
    azure_api_version TEXT NOT NULL DEFAULT '',
    weight            INTEGER NOT NULL DEFAULT 1,
    priority          INTEGER NOT NULL DEFAULT 0,
    is_active         INTEGER NOT NULL DEFAULT 1 CHECK (is_active IN (0, 1)),
    created_at        TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at        TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    deleted_at        TEXT,
    UNIQUE (model_id, name)
);

-- Hot path: load balancer fetches active deployments for a model on every request.
CREATE INDEX idx_model_deployments_model
    ON model_deployments (model_id)
    WHERE deleted_at IS NULL;

-- Load balancing strategy for this model. Empty string means single-deployment
-- (legacy) mode. Valid values: 'round-robin', 'weighted', 'priority'.
ALTER TABLE models ADD COLUMN strategy TEXT NOT NULL DEFAULT '';

-- Maximum number of deployments to try before surfacing an error to the caller.
-- 0 means try all available deployments.
ALTER TABLE models ADD COLUMN max_retries INTEGER NOT NULL DEFAULT 0;
