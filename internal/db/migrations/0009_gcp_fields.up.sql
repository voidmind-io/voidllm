-- Migration: 0009_gcp_fields.up.sql
-- Description: Add gcp_project and gcp_location columns to models and
-- model_deployments tables. These are required when provider is "vertex"
-- (Vertex AI) and are empty for all other providers.

ALTER TABLE models ADD COLUMN gcp_project TEXT NOT NULL DEFAULT '';
ALTER TABLE models ADD COLUMN gcp_location TEXT NOT NULL DEFAULT '';

ALTER TABLE model_deployments ADD COLUMN gcp_project TEXT NOT NULL DEFAULT '';
ALTER TABLE model_deployments ADD COLUMN gcp_location TEXT NOT NULL DEFAULT '';
