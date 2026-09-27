-- Migration: 0017_membership_limits.up.sql
-- Description: Adds per-user limits to org_memberships, enforced across all
-- API keys a user owns within that org. Limits are currently defined at the
-- org, team, and api_keys level (0001_initial_schema.up.sql) and combined
-- via most-restrictive-wins, but there was no way to cap a single user's
-- aggregate usage independent of any one key -- a user with several keys in
-- the same org could exceed an intended per-user budget by spreading usage
-- across keys. Adding the limit columns to org_memberships closes that gap
-- without introducing a new table.
--
-- Same four columns, same semantics (0 = unlimited), and same types as the
-- limit columns on organizations, teams, and api_keys in
-- 0001_initial_schema.up.sql: daily_token_limit, monthly_token_limit,
-- requests_per_minute, requests_per_day.
--
-- ALTER TABLE ADD COLUMN follows the precedent in
-- 0011_model_fallback_chains.up.sql and 0016_cached_tokens.up.sql and is
-- supported unchanged on both SQLite (modernc.org/sqlite ships 3.45+) and
-- all PostgreSQL versions.

ALTER TABLE org_memberships
    ADD COLUMN daily_token_limit INTEGER NOT NULL DEFAULT 0;

ALTER TABLE org_memberships
    ADD COLUMN monthly_token_limit INTEGER NOT NULL DEFAULT 0;

ALTER TABLE org_memberships
    ADD COLUMN requests_per_minute INTEGER NOT NULL DEFAULT 0;

ALTER TABLE org_memberships
    ADD COLUMN requests_per_day INTEGER NOT NULL DEFAULT 0;
