package db

// usage_aggregation_test.go covers internal/db/usage_aggregation.go, which had
// zero test coverage before #179 — the hourly rollup layer (UpsertUsageHourly,
// GetHourlyUsageTotals) that powers the fast-path recent-usage dashboard.

import (
	"context"
	"math"
	"testing"
	"time"
)

// baseHourlyRollup returns a HourlyRollup with a fixed bucket hour and every
// numeric column set to a distinct, non-zero value so that upsert-accumulation
// bugs (e.g. a forgotten "+" in the ON CONFLICT clause) are caught even when a
// column happens to start at zero.
func baseHourlyRollup(keyID, modelName string) HourlyRollup {
	return HourlyRollup{
		OrgID:            "org-rollup",
		TeamID:           "team-rollup",
		UserID:           "user-rollup",
		KeyID:            keyID,
		ModelName:        modelName,
		BucketHour:       "2026-03-20T14:00:00Z",
		RequestCount:     1,
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		CachedReadTokens: 30,
		CacheWriteTokens: 20,
		CostSum:          0.015,
		DurationSumMS:    250,
		TTFTSumMS:        40,
		TTFTCount:        1,
	}
}

// ---- UpsertUsageHourly: fresh bucket ---------------------------------------

// TestUpsertUsageHourly_InsertsFreshBucket verifies that upserting a bucket
// that does not yet exist inserts a row with exactly the given values,
// including the two cache-token columns added by migration 0016.
func TestUpsertUsageHourly_InsertsFreshBucket(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()

	r := baseHourlyRollup("key-fresh", "model-fresh")
	if err := d.UpsertUsageHourly(ctx, r); err != nil {
		t.Fatalf("UpsertUsageHourly() error = %v", err)
	}

	var (
		requestCount                    int
		promptTokens, completionTokens  int
		totalTokens                     int
		cachedReadTokens, cacheWriteTok int
		costSum, durationSumMS          float64
		ttftSumMS                       float64
		ttftCount                       int
	)
	row := d.sql.QueryRowContext(ctx,
		`SELECT request_count, prompt_tokens, completion_tokens, total_tokens,
		        cached_read_tokens, cache_write_tokens,
		        cost_sum, duration_sum_ms, ttft_sum_ms, ttft_count
		 FROM usage_hourly WHERE key_id = ? AND model_name = ? AND bucket_hour = ?`,
		"key-fresh", "model-fresh", r.BucketHour,
	)
	if err := row.Scan(&requestCount, &promptTokens, &completionTokens, &totalTokens,
		&cachedReadTokens, &cacheWriteTok, &costSum, &durationSumMS, &ttftSumMS, &ttftCount); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	if requestCount != r.RequestCount {
		t.Errorf("request_count = %d, want %d", requestCount, r.RequestCount)
	}
	if promptTokens != r.PromptTokens {
		t.Errorf("prompt_tokens = %d, want %d", promptTokens, r.PromptTokens)
	}
	if completionTokens != r.CompletionTokens {
		t.Errorf("completion_tokens = %d, want %d", completionTokens, r.CompletionTokens)
	}
	if totalTokens != r.TotalTokens {
		t.Errorf("total_tokens = %d, want %d", totalTokens, r.TotalTokens)
	}
	if cachedReadTokens != r.CachedReadTokens {
		t.Errorf("cached_read_tokens = %d, want %d", cachedReadTokens, r.CachedReadTokens)
	}
	if cacheWriteTok != r.CacheWriteTokens {
		t.Errorf("cache_write_tokens = %d, want %d", cacheWriteTok, r.CacheWriteTokens)
	}
	if math.Abs(costSum-r.CostSum) > 1e-9 {
		t.Errorf("cost_sum = %v, want %v", costSum, r.CostSum)
	}
	if durationSumMS != r.DurationSumMS {
		t.Errorf("duration_sum_ms = %v, want %v", durationSumMS, r.DurationSumMS)
	}
	if ttftSumMS != r.TTFTSumMS {
		t.Errorf("ttft_sum_ms = %v, want %v", ttftSumMS, r.TTFTSumMS)
	}
	if ttftCount != r.TTFTCount {
		t.Errorf("ttft_count = %d, want %d", ttftCount, r.TTFTCount)
	}
}

// ---- UpsertUsageHourly: accumulation on conflict ---------------------------

// TestUpsertUsageHourly_AccumulatesOnConflict verifies that a second upsert
// for the same (key_id, model_name, bucket_hour) bucket adds to every numeric
// column rather than overwriting it — including cached_read_tokens and
// cache_write_tokens, which is the specific behavior #179 added.
func TestUpsertUsageHourly_AccumulatesOnConflict(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()

	first := baseHourlyRollup("key-accum", "model-accum")
	if err := d.UpsertUsageHourly(ctx, first); err != nil {
		t.Fatalf("UpsertUsageHourly() first error = %v", err)
	}

	second := baseHourlyRollup("key-accum", "model-accum")
	// Distinct values from `first` so an accidental overwrite (rather than
	// accumulation) is detectable.
	second.RequestCount = 2
	second.PromptTokens = 40
	second.CompletionTokens = 10
	second.TotalTokens = 50
	second.CachedReadTokens = 15
	second.CacheWriteTokens = 5
	second.CostSum = 0.008
	second.DurationSumMS = 100
	second.TTFTSumMS = 20
	second.TTFTCount = 1

	if err := d.UpsertUsageHourly(ctx, second); err != nil {
		t.Fatalf("UpsertUsageHourly() second error = %v", err)
	}

	var (
		requestCount                    int
		promptTokens, completionTokens  int
		totalTokens                     int
		cachedReadTokens, cacheWriteTok int
		costSum, durationSumMS          float64
		ttftSumMS                       float64
		ttftCount                       int
	)
	row := d.sql.QueryRowContext(ctx,
		`SELECT request_count, prompt_tokens, completion_tokens, total_tokens,
		        cached_read_tokens, cache_write_tokens,
		        cost_sum, duration_sum_ms, ttft_sum_ms, ttft_count
		 FROM usage_hourly WHERE key_id = ? AND model_name = ? AND bucket_hour = ?`,
		"key-accum", "model-accum", first.BucketHour,
	)
	if err := row.Scan(&requestCount, &promptTokens, &completionTokens, &totalTokens,
		&cachedReadTokens, &cacheWriteTok, &costSum, &durationSumMS, &ttftSumMS, &ttftCount); err != nil {
		t.Fatalf("Scan: %v", err)
	}

	// Every column below must be first+second, proving accumulation (not
	// overwrite) for both the pre-existing columns and the two new ones.
	if want := first.RequestCount + second.RequestCount; requestCount != want {
		t.Errorf("request_count = %d, want %d (accumulated)", requestCount, want)
	}
	if want := first.PromptTokens + second.PromptTokens; promptTokens != want {
		t.Errorf("prompt_tokens = %d, want %d (accumulated)", promptTokens, want)
	}
	if want := first.CompletionTokens + second.CompletionTokens; completionTokens != want {
		t.Errorf("completion_tokens = %d, want %d (accumulated)", completionTokens, want)
	}
	if want := first.TotalTokens + second.TotalTokens; totalTokens != want {
		t.Errorf("total_tokens = %d, want %d (accumulated)", totalTokens, want)
	}
	if want := first.CachedReadTokens + second.CachedReadTokens; cachedReadTokens != want {
		t.Errorf("cached_read_tokens = %d, want %d (accumulated)", cachedReadTokens, want)
	}
	if want := first.CacheWriteTokens + second.CacheWriteTokens; cacheWriteTok != want {
		t.Errorf("cache_write_tokens = %d, want %d (accumulated)", cacheWriteTok, want)
	}
	if want := first.CostSum + second.CostSum; math.Abs(costSum-want) > 1e-9 {
		t.Errorf("cost_sum = %v, want %v (accumulated)", costSum, want)
	}
	if want := first.DurationSumMS + second.DurationSumMS; durationSumMS != want {
		t.Errorf("duration_sum_ms = %v, want %v (accumulated)", durationSumMS, want)
	}
	if want := first.TTFTSumMS + second.TTFTSumMS; ttftSumMS != want {
		t.Errorf("ttft_sum_ms = %v, want %v (accumulated)", ttftSumMS, want)
	}
	if want := first.TTFTCount + second.TTFTCount; ttftCount != want {
		t.Errorf("ttft_count = %d, want %d (accumulated)", ttftCount, want)
	}

	// Exactly one row must exist for this bucket key — accumulation, not a
	// second inserted row.
	var rowCount int
	if err := d.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM usage_hourly WHERE key_id = ? AND model_name = ? AND bucket_hour = ?",
		"key-accum", "model-accum", first.BucketHour,
	).Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("row count for bucket = %d, want 1 (upsert must not duplicate rows)", rowCount)
	}
}

// ---- GetHourlyUsageTotals ---------------------------------------------------

// TestGetHourlyUsageTotals_SumsAcrossBuckets verifies that GetHourlyUsageTotals
// sums prompt/completion/total/cached-read/cache-write tokens and cost across
// multiple hourly buckets for the same org, and computes a request-weighted
// average duration.
func TestGetHourlyUsageTotals_SumsAcrossBuckets(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()

	bucket1 := HourlyRollup{
		OrgID: "org-hourly-sum", KeyID: "key-hourly", ModelName: "model-a",
		BucketHour:       "2026-03-20T13:00:00Z",
		RequestCount:     2,
		PromptTokens:     100,
		CompletionTokens: 50,
		TotalTokens:      150,
		CachedReadTokens: 30,
		CacheWriteTokens: 10,
		CostSum:          0.01,
		DurationSumMS:    400, // avg contribution: 400ms over 2 requests
	}
	bucket2 := HourlyRollup{
		OrgID: "org-hourly-sum", KeyID: "key-hourly", ModelName: "model-b",
		BucketHour:       "2026-03-20T14:00:00Z",
		RequestCount:     1,
		PromptTokens:     200,
		CompletionTokens: 80,
		TotalTokens:      280,
		CachedReadTokens: 70,
		CacheWriteTokens: 5,
		CostSum:          0.02,
		DurationSumMS:    300, // avg contribution: 300ms over 1 request
	}

	if err := d.UpsertUsageHourly(ctx, bucket1); err != nil {
		t.Fatalf("UpsertUsageHourly(bucket1) error = %v", err)
	}
	if err := d.UpsertUsageHourly(ctx, bucket2); err != nil {
		t.Fatalf("UpsertUsageHourly(bucket2) error = %v", err)
	}

	since, err := time.Parse(time.RFC3339, "2026-03-20T12:00:00Z")
	if err != nil {
		t.Fatalf("time.Parse: %v", err)
	}

	agg, err := d.GetHourlyUsageTotals(ctx, UsageFilter{OrgID: "org-hourly-sum"}, since)
	if err != nil {
		t.Fatalf("GetHourlyUsageTotals() error = %v", err)
	}

	if agg.TotalRequests != 3 {
		t.Errorf("TotalRequests = %d, want 3", agg.TotalRequests)
	}
	if agg.PromptTokens != 300 {
		t.Errorf("PromptTokens = %d, want 300", agg.PromptTokens)
	}
	if agg.CompletionTokens != 130 {
		t.Errorf("CompletionTokens = %d, want 130", agg.CompletionTokens)
	}
	if agg.TotalTokens != 430 {
		t.Errorf("TotalTokens = %d, want 430", agg.TotalTokens)
	}
	if agg.CachedReadTokens != 100 {
		t.Errorf("CachedReadTokens = %d, want 100", agg.CachedReadTokens)
	}
	if agg.CacheWriteTokens != 15 {
		t.Errorf("CacheWriteTokens = %d, want 15", agg.CacheWriteTokens)
	}
	const wantCost = 0.03
	if math.Abs(agg.CostEstimate-wantCost) > 1e-9 {
		t.Errorf("CostEstimate = %v, want %v", agg.CostEstimate, wantCost)
	}
	// avg = total_duration_sum / total_requests = 700 / 3.
	const wantAvg = 700.0 / 3.0
	if math.Abs(agg.AvgDurationMS-wantAvg) > 1e-6 {
		t.Errorf("AvgDurationMS = %v, want %v", agg.AvgDurationMS, wantAvg)
	}
}

// TestGetHourlyUsageTotals_SinceExcludesEarlierBuckets verifies that buckets
// with bucket_hour before `since` (truncated to the hour) are excluded from
// the sum.
func TestGetHourlyUsageTotals_SinceExcludesEarlierBuckets(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()

	early := HourlyRollup{
		OrgID: "org-hourly-since", KeyID: "key-since", ModelName: "model-early",
		BucketHour: "2026-03-20T10:00:00Z", RequestCount: 1,
		PromptTokens: 999, CompletionTokens: 999, TotalTokens: 1998,
		CachedReadTokens: 999, CacheWriteTokens: 999,
	}
	late := HourlyRollup{
		OrgID: "org-hourly-since", KeyID: "key-since", ModelName: "model-late",
		BucketHour: "2026-03-20T15:00:00Z", RequestCount: 1,
		PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15,
		CachedReadTokens: 3, CacheWriteTokens: 2,
	}
	if err := d.UpsertUsageHourly(ctx, early); err != nil {
		t.Fatalf("UpsertUsageHourly(early) error = %v", err)
	}
	if err := d.UpsertUsageHourly(ctx, late); err != nil {
		t.Fatalf("UpsertUsageHourly(late) error = %v", err)
	}

	since, err := time.Parse(time.RFC3339, "2026-03-20T14:00:00Z")
	if err != nil {
		t.Fatalf("time.Parse: %v", err)
	}

	agg, err := d.GetHourlyUsageTotals(ctx, UsageFilter{OrgID: "org-hourly-since"}, since)
	if err != nil {
		t.Fatalf("GetHourlyUsageTotals() error = %v", err)
	}

	if agg.PromptTokens != 10 {
		t.Errorf("PromptTokens = %d, want 10 (early bucket must be excluded)", agg.PromptTokens)
	}
	if agg.CachedReadTokens != 3 {
		t.Errorf("CachedReadTokens = %d, want 3 (early bucket must be excluded)", agg.CachedReadTokens)
	}
}

// TestGetHourlyUsageTotals_NoBuckets_ReturnsZero verifies that querying an org
// with no hourly rollup rows returns an all-zero UsageAggregate rather than an
// error, matching the COALESCE(SUM(...), 0) semantics of the SQL.
func TestGetHourlyUsageTotals_NoBuckets_ReturnsZero(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()

	since := time.Now().UTC().Add(-time.Hour)
	agg, err := d.GetHourlyUsageTotals(ctx, UsageFilter{OrgID: "org-hourly-empty"}, since)
	if err != nil {
		t.Fatalf("GetHourlyUsageTotals() error = %v", err)
	}

	if agg.TotalRequests != 0 {
		t.Errorf("TotalRequests = %d, want 0", agg.TotalRequests)
	}
	if agg.PromptTokens != 0 || agg.CompletionTokens != 0 || agg.TotalTokens != 0 {
		t.Errorf("token totals = %+v, want all zero", agg)
	}
	if agg.CachedReadTokens != 0 {
		t.Errorf("CachedReadTokens = %d, want 0", agg.CachedReadTokens)
	}
	if agg.CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, want 0", agg.CacheWriteTokens)
	}
	if agg.CostEstimate != 0 {
		t.Errorf("CostEstimate = %v, want 0", agg.CostEstimate)
	}
	if agg.AvgDurationMS != 0 {
		t.Errorf("AvgDurationMS = %v, want 0", agg.AvgDurationMS)
	}
}

// TestGetHourlyUsageTotals_ScopedByTeamAndUserAndKey verifies that the
// optional TeamID/UserID/KeyID filters on UsageFilter narrow the sum, using
// the cache-token columns as the distinguishing signal.
func TestGetHourlyUsageTotals_ScopedByTeamAndUserAndKey(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()

	matching := HourlyRollup{
		OrgID: "org-hourly-scope", TeamID: "team-x", UserID: "user-x", KeyID: "key-x",
		ModelName: "model-x", BucketHour: "2026-03-20T14:00:00Z", RequestCount: 1,
		PromptTokens: 50, CompletionTokens: 10, TotalTokens: 60,
		CachedReadTokens: 20, CacheWriteTokens: 5,
	}
	other := HourlyRollup{
		OrgID: "org-hourly-scope", TeamID: "team-y", UserID: "user-y", KeyID: "key-y",
		ModelName: "model-y", BucketHour: "2026-03-20T14:00:00Z", RequestCount: 1,
		PromptTokens: 1000, CompletionTokens: 1000, TotalTokens: 2000,
		CachedReadTokens: 1000, CacheWriteTokens: 1000,
	}
	if err := d.UpsertUsageHourly(ctx, matching); err != nil {
		t.Fatalf("UpsertUsageHourly(matching) error = %v", err)
	}
	if err := d.UpsertUsageHourly(ctx, other); err != nil {
		t.Fatalf("UpsertUsageHourly(other) error = %v", err)
	}

	since, err := time.Parse(time.RFC3339, "2026-03-20T00:00:00Z")
	if err != nil {
		t.Fatalf("time.Parse: %v", err)
	}

	tests := []struct {
		name   string
		filter UsageFilter
	}{
		{name: "scoped by team", filter: UsageFilter{OrgID: "org-hourly-scope", TeamID: "team-x"}},
		{name: "scoped by user", filter: UsageFilter{OrgID: "org-hourly-scope", UserID: "user-x"}},
		{name: "scoped by key", filter: UsageFilter{OrgID: "org-hourly-scope", KeyID: "key-x"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agg, err := d.GetHourlyUsageTotals(ctx, tc.filter, since)
			if err != nil {
				t.Fatalf("GetHourlyUsageTotals() error = %v", err)
			}
			if agg.CachedReadTokens != 20 {
				t.Errorf("CachedReadTokens = %d, want 20 (only the matching-scope bucket)", agg.CachedReadTokens)
			}
			if agg.CacheWriteTokens != 5 {
				t.Errorf("CacheWriteTokens = %d, want 5 (only the matching-scope bucket)", agg.CacheWriteTokens)
			}
		})
	}
}
