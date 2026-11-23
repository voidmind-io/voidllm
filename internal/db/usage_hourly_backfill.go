package db

import (
	"context"
	"fmt"
	"time"
)

// UsageHourlyBackfillMarker is the settings key recording that the one-time
// usage_hourly backfill from usage_events has completed on this database.
const UsageHourlyBackfillMarker = "usage_hourly_backfill_v1"

// BackfillUsageHourlyOnce rebuilds missing usage_hourly buckets from
// usage_events exactly once per database. It returns ran=false without
// touching any data when the completion marker is already set. Otherwise it
// runs BackfillUsageHourly and, only on success, sets the marker, so a failed
// or interrupted run is retried on the next start.
func (d *DB) BackfillUsageHourlyOnce(ctx context.Context, now time.Time) (rows int64, ran bool, err error) {
	done, err := d.GetSetting(ctx, UsageHourlyBackfillMarker)
	if err != nil {
		return 0, false, fmt.Errorf("backfill usage hourly: read marker: %w", err)
	}
	if done != "" {
		return 0, false, nil
	}
	rows, err = d.BackfillUsageHourly(ctx, now)
	if err != nil {
		return 0, true, err
	}
	if err := d.SetSetting(ctx, UsageHourlyBackfillMarker, FormatTimestamp(now)); err != nil {
		return rows, true, fmt.Errorf("backfill usage hourly: set marker: %w", err)
	}
	return rows, true, nil
}

// BackfillUsageHourly inserts one usage_hourly bucket per
// (key_id, model_name, hour) found in usage_events, for hours strictly before
// the hour containing now. Buckets that already exist are left untouched
// (ON CONFLICT DO NOTHING), so the statement is idempotent and never
// double-counts what the live usage logger has written. The current hour is
// skipped because the logger is still rolling it up. Column mapping matches
// the usage logger: NULL team/user become ”, NULL cost and duration count as
// 0, and ttft_count counts only events with a recorded TTFT.
//
// The bucket hour is derived from created_at, which is canonical
// "YYYY-MM-DDTHH:MM:SSZ" text on both dialects (see TimestampLayout); rows not
// matching that shape are ignored. It returns the number of buckets inserted.
func (d *DB) BackfillUsageHourly(ctx context.Context, now time.Time) (int64, error) {
	p := d.dialect.Placeholder
	query := "INSERT INTO usage_hourly (" +
		"org_id, team_id, user_id, key_id, model_name, bucket_hour, " +
		"request_count, prompt_tokens, completion_tokens, total_tokens, " +
		"cached_read_tokens, cache_write_tokens, " +
		"cost_sum, duration_sum_ms, ttft_sum_ms, ttft_count) " +
		"SELECT MIN(org_id), COALESCE(MIN(team_id), ''), COALESCE(MIN(user_id), ''), key_id, model_name, " +
		"substr(created_at, 1, 13) || ':00:00Z', " +
		"COUNT(*), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(total_tokens), 0), " +
		"COALESCE(SUM(cached_read_tokens), 0), COALESCE(SUM(cache_write_tokens), 0), " +
		"COALESCE(SUM(cost_estimate), 0), COALESCE(SUM(request_duration_ms), 0), COALESCE(SUM(ttft_ms), 0), COUNT(ttft_ms) " +
		"FROM usage_events " +
		"WHERE created_at LIKE '____-__-__T__:__:__Z' AND created_at < " + p(1) + " " +
		"GROUP BY key_id, model_name, substr(created_at, 1, 13) " +
		"ON CONFLICT (key_id, model_name, bucket_hour) DO NOTHING"

	cutoff := FormatTimestamp(now.UTC().Truncate(time.Hour))
	res, err := d.sql.ExecContext(ctx, query, cutoff)
	if err != nil {
		return 0, fmt.Errorf("backfill usage hourly: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("backfill usage hourly: rows affected: %w", err)
	}
	return n, nil
}
