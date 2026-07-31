-- Migration: 0017_mcp_protocol_version.down.sql
-- Description: Reverses 0017_mcp_protocol_version.up.sql.
-- ALTER TABLE DROP COLUMN requires SQLite 3.35+ (modernc.org/sqlite ships
-- 3.45+) and is standard on all supported PostgreSQL versions, following the
-- precedent in 0011_model_fallback_chains.down.sql.

ALTER TABLE mcp_servers DROP COLUMN protocol_version;
