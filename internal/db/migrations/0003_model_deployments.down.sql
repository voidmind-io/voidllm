-- Migration: 0003_model_deployments.down.sql
-- Description: Reverses 0003_model_deployments.up.sql.
-- SQLite does not support DROP COLUMN before version 3.35.0 and does not
-- support DROP COLUMN on columns that are part of an index. The strategy and
-- max_retries columns added to models are safe to drop on SQLite 3.35+ and on
-- all supported PostgreSQL versions.

DROP TABLE IF EXISTS model_deployments;

ALTER TABLE models DROP COLUMN strategy;
ALTER TABLE models DROP COLUMN max_retries;
