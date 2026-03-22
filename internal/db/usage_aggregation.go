package db

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// HourlyRollup holds pre-aggregated usage data for a single
// (key, model, hour) bucket. Sums and counts are stored — averages
// are computed at query time.
type HourlyRollup struct {
	OrgID            string
	TeamID           string // empty string = no team
	UserID           string // empty string = no user
	KeyID            string
	ModelName        string
	BucketHour       string // "2026-03-20T14:00:00Z"
	RequestCount     int
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostSum          float64
	DurationSumMS    float64
	TTFTSumMS        float64
	TTFTCount        int
}

// GetHourlyUsageTotals returns aggregated totals from the usage_hourly table
// for rows matching filter whose bucket_hour is at or after since. It is
// significantly faster than scanning raw usage_events for recent windows such
// as the 24-hour dashboard period, because usage_hourly contains pre-aggregated
// per-hour buckets rather than one row per request.
//
// OrgID on filter is required. TeamID, UserID, and KeyID are optional additional
// filters. The returned UsageAggregate has an empty GroupKey (totals only).
func (d *DB) GetHourlyUsageTotals(ctx context.Context, filter UsageFilter, since time.Time) (UsageAggregate, error) {
	p := d.dialect.Placeholder
	argN := 1

	conditions := []string{"bucket_hour >= " + p(argN)}
	truncated := since.UTC().Truncate(time.Hour)
	args := []any{truncated.Format(time.RFC3339)}
	argN++

	conditions = append(conditions, "org_id = "+p(argN))
	args = append(args, filter.OrgID)
	argN++

	if filter.TeamID != "" {
		conditions = append(conditions, "team_id = "+p(argN))
		args = append(args, filter.TeamID)
		argN++
	}
	if filter.UserID != "" {
		conditions = append(conditions, "user_id = "+p(argN))
		args = append(args, filter.UserID)
		argN++
	}
	if filter.KeyID != "" {
		conditions = append(conditions, "key_id = "+p(argN))
		args = append(args, filter.KeyID)
		argN++ //nolint:ineffassign // argN kept for consistency if further conditions are added
	}

	query := "SELECT COALESCE(SUM(request_count), 0), " +
		"COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0), " +
		"COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost_sum), 0), " +
		"CASE WHEN SUM(request_count) > 0 THEN SUM(duration_sum_ms) * 1.0 / SUM(request_count) ELSE 0 END " +
		"FROM usage_hourly WHERE " + strings.Join(conditions, " AND ")

	var a UsageAggregate
	err := d.sql.QueryRowContext(ctx, query, args...).Scan(
		&a.TotalRequests, &a.PromptTokens, &a.CompletionTokens,
		&a.TotalTokens, &a.CostEstimate, &a.AvgDurationMS,
	)
	if err != nil {
		return a, fmt.Errorf("get hourly usage totals: %w", err)
	}
	return a, nil
}

// UpsertUsageHourly inserts or increments an hourly usage rollup bucket.
// If a row already exists for the (key_id, model_name, bucket_hour) primary key,
// all numeric columns are atomically incremented by the values in r.
func (d *DB) UpsertUsageHourly(ctx context.Context, r HourlyRollup) error {
	p := d.dialect.Placeholder
	query := "INSERT INTO usage_hourly (" +
		"org_id, team_id, user_id, key_id, model_name, bucket_hour, " +
		"request_count, prompt_tokens, completion_tokens, total_tokens, " +
		"cost_sum, duration_sum_ms, ttft_sum_ms, ttft_count" +
		") VALUES (" +
		p(1) + ", " + p(2) + ", " + p(3) + ", " + p(4) + ", " + p(5) + ", " + p(6) + ", " +
		p(7) + ", " + p(8) + ", " + p(9) + ", " + p(10) + ", " +
		p(11) + ", " + p(12) + ", " + p(13) + ", " + p(14) +
		") ON CONFLICT (key_id, model_name, bucket_hour) DO UPDATE SET " +
		"request_count = request_count + excluded.request_count, " +
		"prompt_tokens = prompt_tokens + excluded.prompt_tokens, " +
		"completion_tokens = completion_tokens + excluded.completion_tokens, " +
		"total_tokens = total_tokens + excluded.total_tokens, " +
		"cost_sum = cost_sum + excluded.cost_sum, " +
		"duration_sum_ms = duration_sum_ms + excluded.duration_sum_ms, " +
		"ttft_sum_ms = ttft_sum_ms + excluded.ttft_sum_ms, " +
		"ttft_count = ttft_count + excluded.ttft_count"

	_, err := d.sql.ExecContext(ctx, query,
		r.OrgID, r.TeamID, r.UserID, r.KeyID, r.ModelName, r.BucketHour,
		r.RequestCount, r.PromptTokens, r.CompletionTokens, r.TotalTokens,
		r.CostSum, r.DurationSumMS, r.TTFTSumMS, r.TTFTCount,
	)
	if err != nil {
		return fmt.Errorf("upsert usage hourly: %w", err)
	}
	return nil
}
