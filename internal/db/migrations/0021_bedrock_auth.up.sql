-- Migration: 0021_bedrock_auth.up.sql
-- Description: Adds bedrock_auth and bedrock_role_arn columns to models and
-- model_deployments so VoidLLM can authenticate to AWS Bedrock with IAM
-- credentials in addition to API keys.
--
-- bedrock_auth selects the authentication mode for bedrock models and defaults
-- to "api_key", which preserves the behaviour of all existing rows.
-- bedrock_role_arn is the IAM role ARN to assume when the auth mode requires
-- one, and is empty otherwise. Both are empty/default for non-bedrock
-- providers. Validation lives in Go, not in the schema.
--
-- Stored as TEXT NOT NULL DEFAULT to mirror 0020_bedrock_fields.up.sql.
-- ALTER TABLE ADD COLUMN with a NOT NULL DEFAULT is supported unchanged on
-- both SQLite (modernc.org/sqlite ships 3.45+) and all PostgreSQL versions.
-- Existing rows backfill via the column default.

ALTER TABLE models ADD COLUMN bedrock_auth TEXT NOT NULL DEFAULT 'api_key';
ALTER TABLE models ADD COLUMN bedrock_role_arn TEXT NOT NULL DEFAULT '';

ALTER TABLE model_deployments ADD COLUMN bedrock_auth TEXT NOT NULL DEFAULT 'api_key';
ALTER TABLE model_deployments ADD COLUMN bedrock_role_arn TEXT NOT NULL DEFAULT '';
