-- Add model_type column for installations upgrading from v0.0.3.
-- Uses a conditional approach: the INSERT INTO schema_migrations already
-- prevents re-execution, so this only runs once on DBs created with the
-- original 0001 schema that lacked model_type.
ALTER TABLE models ADD COLUMN model_type TEXT NOT NULL DEFAULT 'chat';
