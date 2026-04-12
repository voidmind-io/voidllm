-- Migration: 0011_model_fallback_chains.down.sql
-- Description: Reverses 0011_model_fallback_chains.up.sql.
-- ALTER TABLE DROP COLUMN requires SQLite 3.35+ (modernc.org/sqlite ships 3.45+)
-- and is standard on all supported PostgreSQL versions.
-- The index must be dropped before the column it covers is removed.

DROP INDEX IF EXISTS idx_models_fallback;

ALTER TABLE usage_events DROP COLUMN requested_model_name;

ALTER TABLE models DROP COLUMN fallback_model_id;
