-- Migration: 0020_bedrock_fields.down.sql
-- Description: Reverses 0020_bedrock_fields.up.sql.
-- ALTER TABLE DROP COLUMN requires SQLite 3.35+ (modernc.org/sqlite ships
-- 3.45+) and is standard on all supported PostgreSQL versions, following the
-- precedent in 0011_model_fallback_chains.down.sql.

ALTER TABLE model_deployments DROP COLUMN bedrock_model_id;
ALTER TABLE model_deployments DROP COLUMN region;

ALTER TABLE models DROP COLUMN bedrock_model_id;
ALTER TABLE models DROP COLUMN region;
