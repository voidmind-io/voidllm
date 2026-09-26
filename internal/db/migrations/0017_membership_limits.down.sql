-- Migration: 0017_membership_limits.down.sql
-- Description: Reverses 0017_membership_limits.up.sql.
-- ALTER TABLE DROP COLUMN requires SQLite 3.35+ (modernc.org/sqlite ships
-- 3.45+) and is standard on all supported PostgreSQL versions, following the
-- precedent in 0011_model_fallback_chains.down.sql and
-- 0016_cached_tokens.down.sql. Dropped in reverse order of creation.

ALTER TABLE org_memberships DROP COLUMN requests_per_day;

ALTER TABLE org_memberships DROP COLUMN requests_per_minute;

ALTER TABLE org_memberships DROP COLUMN monthly_token_limit;

ALTER TABLE org_memberships DROP COLUMN daily_token_limit;
