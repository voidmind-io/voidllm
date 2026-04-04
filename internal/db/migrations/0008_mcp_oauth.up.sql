ALTER TABLE mcp_servers ADD COLUMN oauth_token_url TEXT DEFAULT '';
ALTER TABLE mcp_servers ADD COLUMN oauth_client_id TEXT DEFAULT '';
ALTER TABLE mcp_servers ADD COLUMN oauth_client_secret_enc TEXT;
ALTER TABLE mcp_servers ADD COLUMN oauth_scopes TEXT DEFAULT '';
