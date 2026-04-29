-- Migration: 0011_model_fallback_chains.up.sql
-- Description: Adds optional fallback target to models and tracks the originally
-- requested model name on usage events for fallback analytics (Enterprise, #45).
--
-- fallback_model_id: self-referential nullable FK. ON DELETE SET NULL means
-- deleting a fallback target safely orphans the reference rather than cascading.
-- ALTER TABLE ADD COLUMN with an inline REFERENCES clause is supported on
-- SQLite 3.6.19+ (modernc.org/sqlite ships 3.45+) and all PostgreSQL versions.
--
-- requested_model_name: records what the client originally asked for. Empty
-- string default keeps existing rows valid; new write paths populate it.
-- Distinguishes "asked for X, served X" from "asked for X, served Y via fallback".

ALTER TABLE models
    ADD COLUMN fallback_model_id TEXT REFERENCES models(id) ON DELETE SET NULL;

ALTER TABLE usage_events
    ADD COLUMN requested_model_name TEXT NOT NULL DEFAULT '';

-- Speeds up "find all models that fall back to model X" used by the admin API
-- when checking whether a model is safe to delete. Partial because most rows
-- have NULL fallback_model_id and we only care about the non-NULL subset.
CREATE INDEX idx_models_fallback
    ON models(fallback_model_id)
    WHERE fallback_model_id IS NOT NULL;
