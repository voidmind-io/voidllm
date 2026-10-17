-- Migration: 0020_bedrock_fields.up.sql
-- Description: Adds region and bedrock_model_id columns to models and
-- model_deployments so VoidLLM can route to AWS Bedrock.
--
-- region is the AWS region of the Bedrock runtime endpoint (e.g. "us-east-1").
-- bedrock_model_id is the Bedrock model identifier or inference profile ID
-- (e.g. "anthropic.claude-3-5-sonnet-20241022-v2:0"), which is sent in the
-- request path and therefore cannot be derived from the VoidLLM model name.
-- Both are required when provider is "bedrock" and are empty for all other
-- providers.
--
-- Stored as TEXT NOT NULL DEFAULT '' to mirror gcp_project / gcp_location from
-- 0009_gcp_fields.up.sql. Validation that they are set for bedrock models
-- lives in Go, not in the schema.
--
-- ALTER TABLE ADD COLUMN with a NOT NULL DEFAULT is supported unchanged on
-- both SQLite (modernc.org/sqlite ships 3.45+) and all PostgreSQL versions,
-- following the precedent in 0011_model_fallback_chains.up.sql. Existing
-- rows backfill to '' via the column default.

ALTER TABLE models ADD COLUMN region TEXT NOT NULL DEFAULT '';
ALTER TABLE models ADD COLUMN bedrock_model_id TEXT NOT NULL DEFAULT '';

ALTER TABLE model_deployments ADD COLUMN region TEXT NOT NULL DEFAULT '';
ALTER TABLE model_deployments ADD COLUMN bedrock_model_id TEXT NOT NULL DEFAULT '';
