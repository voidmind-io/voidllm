-- Add AWS Bedrock Converse fields to the models table.
ALTER TABLE models ADD COLUMN aws_region TEXT NOT NULL DEFAULT '';
ALTER TABLE models ADD COLUMN aws_access_key_enc TEXT;
ALTER TABLE models ADD COLUMN aws_secret_key_enc TEXT;
ALTER TABLE models ADD COLUMN aws_session_token_enc TEXT;

-- Add AWS Bedrock Converse fields to the model_deployments table.
ALTER TABLE model_deployments ADD COLUMN aws_region TEXT NOT NULL DEFAULT '';
ALTER TABLE model_deployments ADD COLUMN aws_access_key_enc TEXT;
ALTER TABLE model_deployments ADD COLUMN aws_secret_key_enc TEXT;
ALTER TABLE model_deployments ADD COLUMN aws_session_token_enc TEXT;
