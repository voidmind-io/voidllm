-- SQLite does not support DROP COLUMN on older versions; these statements work
-- on SQLite >= 3.35.0 (2021-03-12) and on all PostgreSQL versions.
ALTER TABLE models DROP COLUMN aws_region;
ALTER TABLE models DROP COLUMN aws_access_key_enc;
ALTER TABLE models DROP COLUMN aws_secret_key_enc;
ALTER TABLE models DROP COLUMN aws_session_token_enc;

ALTER TABLE model_deployments DROP COLUMN aws_region;
ALTER TABLE model_deployments DROP COLUMN aws_access_key_enc;
ALTER TABLE model_deployments DROP COLUMN aws_secret_key_enc;
ALTER TABLE model_deployments DROP COLUMN aws_session_token_enc;
