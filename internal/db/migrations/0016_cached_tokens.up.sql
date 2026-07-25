-- Migration: 0016_cached_tokens.up.sql
-- Description: Adds storage for prompt-cache token counts and their per-model
-- prices, so cost estimates stay accurate for cache-heavy traffic (#179).
-- Every major provider bills cached tokens differently from fresh input
-- tokens, but VoidLLM previously stored no cached-token counts at all.
--
-- Naming: the read-side counter is named cached_read_tokens, not anything
-- containing "content" -- internal/usage/logger_test.go
-- (TestLog_NoContentColumns) asserts no usage_events column name contains
-- "content", as a guard for the project's zero-knowledge, no-content-storage
-- constraint. This migration stores only token counts, never content.
--
-- Two separate counters, not one: cache READS (tokens served from an
-- existing prompt cache) are billed BELOW the normal input rate, while
-- cache WRITES (Anthropic's cache_creation_input_tokens, i.e. tokens spent
-- populating the cache) are billed ABOVE it. A single "cached tokens"
-- counter cannot represent both directions of that price differential, so
-- cached_read_tokens and cache_write_tokens are tracked independently, with
-- matching cached_input_price_per_1m / cache_write_price_per_1m columns on
-- models to price each side.
--
-- ALTER TABLE ADD COLUMN follows the precedent in
-- 0011_model_fallback_chains.up.sql and is supported unchanged on both
-- SQLite (modernc.org/sqlite ships 3.45+) and all PostgreSQL versions.

ALTER TABLE usage_events
    ADD COLUMN cached_read_tokens INTEGER NOT NULL DEFAULT 0;

ALTER TABLE usage_events
    ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;

ALTER TABLE usage_hourly
    ADD COLUMN cached_read_tokens INTEGER NOT NULL DEFAULT 0;

ALTER TABLE usage_hourly
    ADD COLUMN cache_write_tokens INTEGER NOT NULL DEFAULT 0;

-- NULL = not configured, matching input_price_per_1m / output_price_per_1m.
ALTER TABLE models
    ADD COLUMN cached_input_price_per_1m REAL;

ALTER TABLE models
    ADD COLUMN cache_write_price_per_1m REAL;
