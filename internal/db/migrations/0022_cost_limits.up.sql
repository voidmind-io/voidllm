-- Migration: 0022_cost_limits.up.sql
-- Description: Adds daily and monthly cost limits to organizations, teams,
-- api_keys and org_memberships, next to the existing token limits. Amounts are
-- stored in micro-units of the configured currency (1 unit = 1,000,000
-- micros), 0 = unlimited. BIGINT is used because INTEGER is 32 bit on
-- PostgreSQL and would overflow at about 2,147 currency units.
--
-- ALTER TABLE ADD COLUMN follows the precedent in 0017_membership_limits.up.sql
-- and is supported unchanged on both SQLite (modernc.org/sqlite ships 3.45+)
-- and all PostgreSQL versions.

ALTER TABLE organizations
    ADD COLUMN daily_cost_limit BIGINT NOT NULL DEFAULT 0;

ALTER TABLE organizations
    ADD COLUMN monthly_cost_limit BIGINT NOT NULL DEFAULT 0;

ALTER TABLE teams
    ADD COLUMN daily_cost_limit BIGINT NOT NULL DEFAULT 0;

ALTER TABLE teams
    ADD COLUMN monthly_cost_limit BIGINT NOT NULL DEFAULT 0;

ALTER TABLE api_keys
    ADD COLUMN daily_cost_limit BIGINT NOT NULL DEFAULT 0;

ALTER TABLE api_keys
    ADD COLUMN monthly_cost_limit BIGINT NOT NULL DEFAULT 0;

ALTER TABLE org_memberships
    ADD COLUMN daily_cost_limit BIGINT NOT NULL DEFAULT 0;

ALTER TABLE org_memberships
    ADD COLUMN monthly_cost_limit BIGINT NOT NULL DEFAULT 0;
