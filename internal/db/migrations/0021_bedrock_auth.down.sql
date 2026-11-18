-- Migration: 0021_bedrock_auth.down.sql
-- Description: Reverses 0021_bedrock_auth.up.sql.
-- ALTER TABLE DROP COLUMN requires SQLite 3.35+ (modernc.org/sqlite ships
-- 3.45+) and is standard on all supported PostgreSQL versions, following the
-- precedent in 0020_bedrock_fields.down.sql.

ALTER TABLE model_deployments DROP COLUMN bedrock_role_arn;
ALTER TABLE model_deployments DROP COLUMN bedrock_auth;

ALTER TABLE models DROP COLUMN bedrock_role_arn;
ALTER TABLE models DROP COLUMN bedrock_auth;
