-- Migration: 0016_cached_tokens.down.sql
-- Description: Reverses 0016_cached_tokens.up.sql.
-- ALTER TABLE DROP COLUMN requires SQLite 3.35+ (modernc.org/sqlite ships
-- 3.45+) and is standard on all supported PostgreSQL versions, following the
-- precedent in 0011_model_fallback_chains.down.sql. Dropped in reverse order
-- of creation.

ALTER TABLE models DROP COLUMN cache_write_price_per_1m;

ALTER TABLE models DROP COLUMN cached_input_price_per_1m;

ALTER TABLE usage_hourly DROP COLUMN cache_write_tokens;

ALTER TABLE usage_hourly DROP COLUMN cached_read_tokens;

ALTER TABLE usage_events DROP COLUMN cache_write_tokens;

ALTER TABLE usage_events DROP COLUMN cached_read_tokens;
