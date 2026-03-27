-- Migration: 0004_mcp_gateway.down.sql
-- Description: Reverses 0004_mcp_gateway.up.sql.
-- Tables are dropped in reverse dependency order: event log first, then access
-- control join tables, then the server registry. Indexes are dropped implicitly
-- with their parent tables.

DROP TABLE IF EXISTS mcp_tool_calls;
DROP TABLE IF EXISTS key_mcp_access;
DROP TABLE IF EXISTS team_mcp_access;
DROP TABLE IF EXISTS org_mcp_access;
DROP TABLE IF EXISTS mcp_servers;
