-- Migration: 0014_pii_filter_flag.up.sql
-- Description: Add pii_filter column to models and model_deployments.
--
-- The pii_filter flag controls per-model/deployment PII anonymization and
-- overrides the network-level default configured in voidllm.yaml. Without this
-- column the flag was present in YAML config but lost on the YAML → DB → registry
-- roundtrip, so the DB-persisted value always silently fell back to the network
-- default regardless of what was declared in the YAML.
--
-- Semantics (three-state nullable boolean):
--   NULL  - not set; inherit the network-level pii_filter default
--   1     - PII anonymization explicitly enabled for this model/deployment
--   0     - PII anonymization explicitly disabled for this model/deployment
--
-- Stored as INTEGER (SQLite and PostgreSQL compatible). The CHECK constraint
-- mirrors the convention used for other boolean columns in this schema
-- (e.g. is_active on model_deployments).

ALTER TABLE models
    ADD COLUMN pii_filter INTEGER CHECK (pii_filter IN (0, 1));

ALTER TABLE model_deployments
    ADD COLUMN pii_filter INTEGER CHECK (pii_filter IN (0, 1));
