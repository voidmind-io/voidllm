-- Migration: 0022_cost_limits.down.sql
-- Description: Reverses 0022_cost_limits.up.sql.
-- ALTER TABLE DROP COLUMN requires SQLite 3.35+ (modernc.org/sqlite ships
-- 3.45+) and is standard on all supported PostgreSQL versions, following the
-- precedent in 0017_membership_limits.down.sql. Dropped in reverse order of
-- creation.

ALTER TABLE org_memberships DROP COLUMN monthly_cost_limit;

ALTER TABLE org_memberships DROP COLUMN daily_cost_limit;

ALTER TABLE api_keys DROP COLUMN monthly_cost_limit;

ALTER TABLE api_keys DROP COLUMN daily_cost_limit;

ALTER TABLE teams DROP COLUMN monthly_cost_limit;

ALTER TABLE teams DROP COLUMN daily_cost_limit;

ALTER TABLE organizations DROP COLUMN monthly_cost_limit;

ALTER TABLE organizations DROP COLUMN daily_cost_limit;
