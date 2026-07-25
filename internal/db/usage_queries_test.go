package db

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"
)

// insertTestUsageEvent inserts a single usage_events row directly into the DB.
// It is purposely kept minimal — only the fields required by GetTokenUsageSince.
func insertTestUsageEvent(t *testing.T, d *DB, id, keyID, teamID, orgID string, totalTokens int64, createdAt time.Time) {
	t.Helper()
	teamVal := "NULL"
	if teamID != "" {
		teamVal = fmt.Sprintf("'%s'", teamID)
	}
	query := fmt.Sprintf(
		`INSERT INTO usage_events
			(id, key_id, key_type, org_id, team_id, model_name,
			 prompt_tokens, completion_tokens, total_tokens, status_code, created_at)
		 VALUES
			('%s', '%s', 'user_key', '%s', %s, 'test-model',
			 0, %d, %d, 200, '%s')`,
		id, keyID, orgID, teamVal,
		totalTokens, totalTokens,
		createdAt.UTC().Format(time.RFC3339),
	)
	if _, err := d.sql.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("insertTestUsageEvent id=%q: %v", id, err)
	}
}

// usageEventParams holds the full set of fields used to insert a usage_events
// row for GetUsageAggregates tests. Fields not set default to safe zero values.
type usageEventParams struct {
	id           string
	keyID        string
	teamID       string // empty → NULL
	orgID        string
	modelName    string
	promptTokens int64
	compTokens   int64
	totalTokens  int64
	// cachedReadTokens and cacheWriteTokens default to 0 (their DB column
	// default) when left unset.
	cachedReadTokens int64
	cacheWriteTokens int64
	costEstimate     *float64 // nil → NULL
	durationMS       *int64   // nil → NULL
	createdAt        time.Time
}

// insertUsageEvent inserts a fully-specified usage_events row for aggregate tests.
func insertUsageEvent(t *testing.T, d *DB, p usageEventParams) {
	t.Helper()

	teamVal := "NULL"
	if p.teamID != "" {
		teamVal = fmt.Sprintf("'%s'", p.teamID)
	}
	costVal := "NULL"
	if p.costEstimate != nil {
		costVal = fmt.Sprintf("%f", *p.costEstimate)
	}
	durVal := "NULL"
	if p.durationMS != nil {
		durVal = fmt.Sprintf("%d", *p.durationMS)
	}
	if p.modelName == "" {
		p.modelName = "test-model"
	}

	query := fmt.Sprintf(
		`INSERT INTO usage_events
			(id, key_id, key_type, org_id, team_id, model_name,
			 prompt_tokens, completion_tokens, total_tokens,
			 cached_read_tokens, cache_write_tokens,
			 cost_estimate, request_duration_ms, status_code, created_at)
		 VALUES
			('%s', '%s', 'user_key', '%s', %s, '%s',
			 %d, %d, %d,
			 %d, %d,
			 %s, %s, 200, '%s')`,
		p.id, p.keyID, p.orgID, teamVal, p.modelName,
		p.promptTokens, p.compTokens, p.totalTokens,
		p.cachedReadTokens, p.cacheWriteTokens,
		costVal, durVal,
		p.createdAt.UTC().Format(time.RFC3339),
	)
	if _, err := d.sql.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("insertUsageEvent id=%q: %v", p.id, err)
	}
}

// float64Ptr is a convenience helper for creating *float64 literals.
func float64Ptr(v float64) *float64 { return &v }

// int64Ptr is a convenience helper for creating *int64 literals.
func int64Ptr(v int64) *int64 { return &v }

// ---- GetTokenUsageSince -------------------------------------------------------

func TestGetTokenUsageSince_NoEvents(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	since := time.Now().UTC().Add(-24 * time.Hour)

	total, err := d.GetTokenUsageSince(context.Background(), ScopeKey, "key-no-events", since)
	if err != nil {
		t.Fatalf("GetTokenUsageSince() error = %v, want nil", err)
	}
	if total != 0 {
		t.Errorf("GetTokenUsageSince() = %d, want 0", total)
	}
}

func TestGetTokenUsageSince_ScopeKey_ReturnsOnlyMatchingKey(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	insertTestUsageEvent(t, d, "evt-k1-a", "key-scope-a", "", "org-scope-1", 300, now.Add(-30*time.Minute))
	insertTestUsageEvent(t, d, "evt-k1-b", "key-scope-a", "", "org-scope-1", 200, now.Add(-10*time.Minute))
	// Different key — must not appear in the result.
	insertTestUsageEvent(t, d, "evt-k2-a", "key-scope-b", "", "org-scope-1", 999, now.Add(-5*time.Minute))

	total, err := d.GetTokenUsageSince(ctx, ScopeKey, "key-scope-a", since)
	if err != nil {
		t.Fatalf("GetTokenUsageSince(ScopeKey) error = %v", err)
	}
	if total != 500 {
		t.Errorf("GetTokenUsageSince(ScopeKey) = %d, want 500", total)
	}
}

func TestGetTokenUsageSince_ScopeTeam_ReturnsOnlyMatchingTeam(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	insertTestUsageEvent(t, d, "evt-t1-a", "key-t1", "team-scope-a", "org-t1", 400, now.Add(-40*time.Minute))
	insertTestUsageEvent(t, d, "evt-t1-b", "key-t1", "team-scope-a", "org-t1", 100, now.Add(-20*time.Minute))
	// Different team.
	insertTestUsageEvent(t, d, "evt-t2-a", "key-t2", "team-scope-b", "org-t1", 777, now.Add(-15*time.Minute))

	total, err := d.GetTokenUsageSince(ctx, ScopeTeam, "team-scope-a", since)
	if err != nil {
		t.Fatalf("GetTokenUsageSince(ScopeTeam) error = %v", err)
	}
	if total != 500 {
		t.Errorf("GetTokenUsageSince(ScopeTeam) = %d, want 500", total)
	}
}

func TestGetTokenUsageSince_ScopeOrg_ReturnsOnlyMatchingOrg(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	insertTestUsageEvent(t, d, "evt-o1-a", "key-o1", "", "org-scope-x", 250, now.Add(-50*time.Minute))
	insertTestUsageEvent(t, d, "evt-o1-b", "key-o1", "", "org-scope-x", 250, now.Add(-25*time.Minute))
	// Different org.
	insertTestUsageEvent(t, d, "evt-o2-a", "key-o2", "", "org-scope-y", 888, now.Add(-10*time.Minute))

	total, err := d.GetTokenUsageSince(ctx, ScopeOrg, "org-scope-x", since)
	if err != nil {
		t.Fatalf("GetTokenUsageSince(ScopeOrg) error = %v", err)
	}
	if total != 500 {
		t.Errorf("GetTokenUsageSince(ScopeOrg) = %d, want 500", total)
	}
}

func TestGetTokenUsageSince_EventsBeforeSinceNotCounted(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	// Insert event well before the since boundary.
	since := now.Add(-time.Hour)
	insertTestUsageEvent(t, d, "evt-before-a", "key-before", "", "org-before", 500, now.Add(-2*time.Hour))

	total, err := d.GetTokenUsageSince(ctx, ScopeKey, "key-before", since)
	if err != nil {
		t.Fatalf("GetTokenUsageSince() error = %v", err)
	}
	if total != 0 {
		t.Errorf("GetTokenUsageSince() = %d, want 0 (event before 'since' must be excluded)", total)
	}
}

func TestGetTokenUsageSince_EventsAfterSinceCounted(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	since := now.Add(-time.Hour)
	// Insert event after the since boundary.
	insertTestUsageEvent(t, d, "evt-after-a", "key-after", "", "org-after", 350, now.Add(-30*time.Minute))

	total, err := d.GetTokenUsageSince(ctx, ScopeKey, "key-after", since)
	if err != nil {
		t.Fatalf("GetTokenUsageSince() error = %v", err)
	}
	if total != 350 {
		t.Errorf("GetTokenUsageSince() = %d, want 350", total)
	}
}

func TestGetTokenUsageSince_EventsBeforeAndAfterSince(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-time.Hour)

	// Before — should not be counted.
	insertTestUsageEvent(t, d, "evt-mix-old", "key-mix", "", "org-mix", 1000, now.Add(-2*time.Hour))
	// After — should be counted.
	insertTestUsageEvent(t, d, "evt-mix-new", "key-mix", "", "org-mix", 400, now.Add(-30*time.Minute))

	total, err := d.GetTokenUsageSince(ctx, ScopeKey, "key-mix", since)
	if err != nil {
		t.Fatalf("GetTokenUsageSince() error = %v", err)
	}
	if total != 400 {
		t.Errorf("GetTokenUsageSince() = %d, want 400 (only post-since event)", total)
	}
}

func TestGetTokenUsageSince_InvalidScope(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)

	_, err := d.GetTokenUsageSince(context.Background(), UsageScope(99), "id", time.Now())
	if err == nil {
		t.Fatal("GetTokenUsageSince() with invalid scope error = nil, want error")
	}
}

func TestGetTokenUsageSince_MultipleEventsSum(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	since := now.Add(-24 * time.Hour)

	amounts := []int64{100, 200, 300, 400, 500}
	var wantTotal int64
	for i, amt := range amounts {
		insertTestUsageEvent(t, d,
			fmt.Sprintf("evt-sum-%d", i),
			"key-sum", "", "org-sum", amt,
			now.Add(-time.Duration(i+1)*time.Minute),
		)
		wantTotal += amt
	}

	total, err := d.GetTokenUsageSince(ctx, ScopeKey, "key-sum", since)
	if err != nil {
		t.Fatalf("GetTokenUsageSince() error = %v", err)
	}
	if total != wantTotal {
		t.Errorf("GetTokenUsageSince() = %d, want %d", total, wantTotal)
	}
}

func TestGetTokenUsageSince_TableDriven_Scopes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		scope       UsageScope
		queryID     string
		insertKeyID string
		insertTeam  string
		insertOrgID string
		wantTotal   int64
	}{
		{
			name:        "ScopeKey matches on key_id",
			scope:       ScopeKey,
			queryID:     "key-td-1",
			insertKeyID: "key-td-1",
			insertTeam:  "team-td-1",
			insertOrgID: "org-td-1",
			wantTotal:   600,
		},
		{
			name:        "ScopeTeam matches on team_id",
			scope:       ScopeTeam,
			queryID:     "team-td-2",
			insertKeyID: "key-td-2",
			insertTeam:  "team-td-2",
			insertOrgID: "org-td-2",
			wantTotal:   600,
		},
		{
			name:        "ScopeOrg matches on org_id",
			scope:       ScopeOrg,
			queryID:     "org-td-3",
			insertKeyID: "key-td-3",
			insertTeam:  "team-td-3",
			insertOrgID: "org-td-3",
			wantTotal:   600,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			d := openMigratedDB(t)
			ctx := context.Background()
			now := time.Now().UTC()
			since := now.Add(-time.Hour)

			insertTestUsageEvent(t, d,
				"evt-td-"+tc.queryID,
				tc.insertKeyID, tc.insertTeam, tc.insertOrgID,
				tc.wantTotal,
				now.Add(-30*time.Minute),
			)

			total, err := d.GetTokenUsageSince(ctx, tc.scope, tc.queryID, since)
			if err != nil {
				t.Fatalf("GetTokenUsageSince() error = %v", err)
			}
			if total != tc.wantTotal {
				t.Errorf("GetTokenUsageSince() = %d, want %d", total, tc.wantTotal)
			}
		})
	}
}

// ---- GetUsageAggregates -------------------------------------------------------

func TestGetUsageAggregates_NoEvents_NoGrouping(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now

	rows, err := d.GetUsageAggregates(ctx, "org-empty", from, to, "")
	if err != nil {
		t.Fatalf("GetUsageAggregates() error = %v, want nil", err)
	}
	// No events: SQLite returns one row with COUNT(*)==0 and all SUMs == 0.
	if len(rows) == 0 {
		return // acceptable — empty result is also valid
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 0 or 1", len(rows))
	}
	if rows[0].TotalRequests != 0 {
		t.Errorf("TotalRequests = %d, want 0", rows[0].TotalRequests)
	}
	if rows[0].TotalTokens != 0 {
		t.Errorf("TotalTokens = %d, want 0", rows[0].TotalTokens)
	}
}

func TestGetUsageAggregates_NoGrouping_CorrectTotals(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)

	insertUsageEvent(t, d, usageEventParams{
		id: "agg-tot-1", keyID: "key-agg-1", orgID: "org-agg-tot",
		promptTokens: 100, compTokens: 50, totalTokens: 150,
		createdAt: now.Add(-90 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-tot-2", keyID: "key-agg-1", orgID: "org-agg-tot",
		promptTokens: 200, compTokens: 80, totalTokens: 280,
		createdAt: now.Add(-60 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-tot-3", keyID: "key-agg-1", orgID: "org-agg-tot",
		promptTokens: 50, compTokens: 20, totalTokens: 70,
		createdAt: now.Add(-30 * time.Minute),
	})

	rows, err := d.GetUsageAggregates(ctx, "org-agg-tot", from, to, "")
	if err != nil {
		t.Fatalf("GetUsageAggregates() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	r := rows[0]
	if r.TotalRequests != 3 {
		t.Errorf("TotalRequests = %d, want 3", r.TotalRequests)
	}
	if r.PromptTokens != 350 {
		t.Errorf("PromptTokens = %d, want 350", r.PromptTokens)
	}
	if r.CompletionTokens != 150 {
		t.Errorf("CompletionTokens = %d, want 150", r.CompletionTokens)
	}
	if r.TotalTokens != 500 {
		t.Errorf("TotalTokens = %d, want 500", r.TotalTokens)
	}
}

func TestGetUsageAggregates_GroupByModel(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)

	insertUsageEvent(t, d, usageEventParams{
		id: "agg-m1-a", keyID: "key-m1", orgID: "org-agg-model",
		modelName: "gpt-4", totalTokens: 100, promptTokens: 80, compTokens: 20,
		createdAt: now.Add(-90 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-m1-b", keyID: "key-m1", orgID: "org-agg-model",
		modelName: "gpt-4", totalTokens: 200, promptTokens: 160, compTokens: 40,
		createdAt: now.Add(-60 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-m2-a", keyID: "key-m1", orgID: "org-agg-model",
		modelName: "claude-3", totalTokens: 50, promptTokens: 40, compTokens: 10,
		createdAt: now.Add(-30 * time.Minute),
	})

	rows, err := d.GetUsageAggregates(ctx, "org-agg-model", from, to, "model")
	if err != nil {
		t.Fatalf("GetUsageAggregates(model) error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}

	// Rows are ordered by model_name: "claude-3" < "gpt-4"
	if rows[0].GroupKey != "claude-3" {
		t.Errorf("rows[0].GroupKey = %q, want %q", rows[0].GroupKey, "claude-3")
	}
	if rows[0].TotalRequests != 1 {
		t.Errorf("claude-3 TotalRequests = %d, want 1", rows[0].TotalRequests)
	}
	if rows[1].GroupKey != "gpt-4" {
		t.Errorf("rows[1].GroupKey = %q, want %q", rows[1].GroupKey, "gpt-4")
	}
	if rows[1].TotalRequests != 2 {
		t.Errorf("gpt-4 TotalRequests = %d, want 2", rows[1].TotalRequests)
	}
	if rows[1].TotalTokens != 300 {
		t.Errorf("gpt-4 TotalTokens = %d, want 300", rows[1].TotalTokens)
	}
}

func TestGetUsageAggregates_GroupByTeam(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)

	insertUsageEvent(t, d, usageEventParams{
		id: "agg-t1-a", keyID: "key-t1", teamID: "team-alpha", orgID: "org-agg-team",
		totalTokens: 100, promptTokens: 80, compTokens: 20,
		createdAt: now.Add(-90 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-t2-a", keyID: "key-t2", teamID: "team-beta", orgID: "org-agg-team",
		totalTokens: 250, promptTokens: 200, compTokens: 50,
		createdAt: now.Add(-60 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-t2-b", keyID: "key-t2", teamID: "team-beta", orgID: "org-agg-team",
		totalTokens: 150, promptTokens: 120, compTokens: 30,
		createdAt: now.Add(-30 * time.Minute),
	})

	rows, err := d.GetUsageAggregates(ctx, "org-agg-team", from, to, "team")
	if err != nil {
		t.Fatalf("GetUsageAggregates(team) error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}

	// Rows ordered by team_id: "team-alpha" < "team-beta"
	if rows[0].GroupKey != "team-alpha" {
		t.Errorf("rows[0].GroupKey = %q, want %q", rows[0].GroupKey, "team-alpha")
	}
	if rows[0].TotalRequests != 1 {
		t.Errorf("team-alpha TotalRequests = %d, want 1", rows[0].TotalRequests)
	}
	if rows[1].GroupKey != "team-beta" {
		t.Errorf("rows[1].GroupKey = %q, want %q", rows[1].GroupKey, "team-beta")
	}
	if rows[1].TotalRequests != 2 {
		t.Errorf("team-beta TotalRequests = %d, want 2", rows[1].TotalRequests)
	}
	if rows[1].TotalTokens != 400 {
		t.Errorf("team-beta TotalTokens = %d, want 400", rows[1].TotalTokens)
	}
}

func TestGetUsageAggregates_GroupByKey(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)

	insertUsageEvent(t, d, usageEventParams{
		id: "agg-k1-a", keyID: "key-agg-kA", orgID: "org-agg-key",
		totalTokens: 100, promptTokens: 80, compTokens: 20,
		createdAt: now.Add(-90 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-k1-b", keyID: "key-agg-kA", orgID: "org-agg-key",
		totalTokens: 200, promptTokens: 160, compTokens: 40,
		createdAt: now.Add(-60 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-k2-a", keyID: "key-agg-kB", orgID: "org-agg-key",
		totalTokens: 75, promptTokens: 60, compTokens: 15,
		createdAt: now.Add(-30 * time.Minute),
	})

	rows, err := d.GetUsageAggregates(ctx, "org-agg-key", from, to, "key")
	if err != nil {
		t.Fatalf("GetUsageAggregates(key) error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}

	// Rows ordered by key_id: "key-agg-kA" < "key-agg-kB"
	if rows[0].GroupKey != "key-agg-kA" {
		t.Errorf("rows[0].GroupKey = %q, want %q", rows[0].GroupKey, "key-agg-kA")
	}
	if rows[0].TotalRequests != 2 {
		t.Errorf("kA TotalRequests = %d, want 2", rows[0].TotalRequests)
	}
	if rows[0].TotalTokens != 300 {
		t.Errorf("kA TotalTokens = %d, want 300", rows[0].TotalTokens)
	}
	if rows[1].GroupKey != "key-agg-kB" {
		t.Errorf("rows[1].GroupKey = %q, want %q", rows[1].GroupKey, "key-agg-kB")
	}
	if rows[1].TotalRequests != 1 {
		t.Errorf("kB TotalRequests = %d, want 1", rows[1].TotalRequests)
	}
}

func TestGetUsageAggregates_GroupByDay(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()

	// Use a fixed reference point so days are predictable.
	day1 := time.Date(2024, 3, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2024, 3, 2, 12, 0, 0, 0, time.UTC)
	day3 := time.Date(2024, 3, 3, 12, 0, 0, 0, time.UTC)

	from := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2024, 3, 3, 23, 59, 59, 0, time.UTC)

	insertUsageEvent(t, d, usageEventParams{
		id: "agg-d1-a", keyID: "key-day", orgID: "org-agg-day",
		totalTokens: 100, promptTokens: 80, compTokens: 20,
		createdAt: day1,
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-d1-b", keyID: "key-day", orgID: "org-agg-day",
		totalTokens: 200, promptTokens: 160, compTokens: 40,
		createdAt: day1.Add(2 * time.Hour),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-d2-a", keyID: "key-day", orgID: "org-agg-day",
		totalTokens: 50, promptTokens: 40, compTokens: 10,
		createdAt: day2,
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-d3-a", keyID: "key-day", orgID: "org-agg-day",
		totalTokens: 75, promptTokens: 60, compTokens: 15,
		createdAt: day3,
	})

	rows, err := d.GetUsageAggregates(ctx, "org-agg-day", from, to, "day")
	if err != nil {
		t.Fatalf("GetUsageAggregates(day) error = %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("len(rows) = %d, want 3", len(rows))
	}

	// Rows ordered by date.
	if rows[0].GroupKey != "2024-03-01" {
		t.Errorf("rows[0].GroupKey = %q, want %q", rows[0].GroupKey, "2024-03-01")
	}
	if rows[0].TotalRequests != 2 {
		t.Errorf("day1 TotalRequests = %d, want 2", rows[0].TotalRequests)
	}
	if rows[0].TotalTokens != 300 {
		t.Errorf("day1 TotalTokens = %d, want 300", rows[0].TotalTokens)
	}
	if rows[1].GroupKey != "2024-03-02" {
		t.Errorf("rows[1].GroupKey = %q, want %q", rows[1].GroupKey, "2024-03-02")
	}
	if rows[1].TotalRequests != 1 {
		t.Errorf("day2 TotalRequests = %d, want 1", rows[1].TotalRequests)
	}
	if rows[2].GroupKey != "2024-03-03" {
		t.Errorf("rows[2].GroupKey = %q, want %q", rows[2].GroupKey, "2024-03-03")
	}
}

func TestGetUsageAggregates_TimeRange_ExcludesBefore(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)

	// This event is before 'from' — must be excluded.
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-before-1", keyID: "key-tr", orgID: "org-agg-tr",
		totalTokens: 9999, promptTokens: 9999, compTokens: 0,
		createdAt: now.Add(-2 * time.Hour),
	})
	// This event is in range.
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-in-1", keyID: "key-tr", orgID: "org-agg-tr",
		totalTokens: 100, promptTokens: 80, compTokens: 20,
		createdAt: now.Add(-30 * time.Minute),
	})

	rows, err := d.GetUsageAggregates(ctx, "org-agg-tr", from, to, "")
	if err != nil {
		t.Fatalf("GetUsageAggregates() error = %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("GetUsageAggregates() returned 0 rows, want 1 with in-range event")
	}
	if rows[0].TotalTokens != 100 {
		t.Errorf("TotalTokens = %d, want 100 (pre-range event must be excluded)", rows[0].TotalTokens)
	}
}

func TestGetUsageAggregates_TimeRange_ExcludesAfter(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	from := now.Add(-time.Hour)
	to := now.Add(-10 * time.Minute) // cutoff in the past

	// This event is after 'to' — must be excluded.
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-after-1", keyID: "key-tr2", orgID: "org-agg-tr2",
		totalTokens: 9999, promptTokens: 9999, compTokens: 0,
		createdAt: now.Add(-5 * time.Minute),
	})
	// This event is in range.
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-in-2", keyID: "key-tr2", orgID: "org-agg-tr2",
		totalTokens: 200, promptTokens: 160, compTokens: 40,
		createdAt: now.Add(-30 * time.Minute),
	})

	rows, err := d.GetUsageAggregates(ctx, "org-agg-tr2", from, to, "")
	if err != nil {
		t.Fatalf("GetUsageAggregates() error = %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("GetUsageAggregates() returned 0 rows, want 1 with in-range event")
	}
	if rows[0].TotalTokens != 200 {
		t.Errorf("TotalTokens = %d, want 200 (post-range event must be excluded)", rows[0].TotalTokens)
	}
}

func TestGetUsageAggregates_OrgFilter(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)

	// This event belongs to the target org.
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-org-mine", keyID: "key-org-filter", orgID: "org-target",
		totalTokens: 100, promptTokens: 80, compTokens: 20,
		createdAt: now.Add(-30 * time.Minute),
	})
	// This event belongs to a different org — must be excluded.
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-org-other", keyID: "key-org-other", orgID: "org-other",
		totalTokens: 9999, promptTokens: 9999, compTokens: 0,
		createdAt: now.Add(-20 * time.Minute),
	})

	rows, err := d.GetUsageAggregates(ctx, "org-target", from, to, "")
	if err != nil {
		t.Fatalf("GetUsageAggregates() error = %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("GetUsageAggregates() returned 0 rows, want 1")
	}
	if rows[0].TotalRequests != 1 {
		t.Errorf("TotalRequests = %d, want 1 (cross-org event must be excluded)", rows[0].TotalRequests)
	}
	if rows[0].TotalTokens != 100 {
		t.Errorf("TotalTokens = %d, want 100", rows[0].TotalTokens)
	}
}

func TestGetUsageAggregates_InvalidGroupBy_ReturnsError(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	_, err := d.GetUsageAggregates(ctx, "org-x", now.Add(-time.Hour), now, "invalid_column")
	if err == nil {
		t.Fatal("GetUsageAggregates() with invalid groupBy error = nil, want error")
	}
}

func TestGetUsageAggregates_CostEstimate_NullTreatedAsZero(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)

	// One event with a cost, one with NULL cost.
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-cost-1", keyID: "key-cost", orgID: "org-agg-cost",
		totalTokens: 100, promptTokens: 80, compTokens: 20,
		costEstimate: float64Ptr(0.005),
		createdAt:    now.Add(-90 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-cost-2", keyID: "key-cost", orgID: "org-agg-cost",
		totalTokens: 200, promptTokens: 160, compTokens: 40,
		costEstimate: nil, // NULL
		createdAt:    now.Add(-60 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-cost-3", keyID: "key-cost", orgID: "org-agg-cost",
		totalTokens: 50, promptTokens: 40, compTokens: 10,
		costEstimate: float64Ptr(0.002),
		createdAt:    now.Add(-30 * time.Minute),
	})

	rows, err := d.GetUsageAggregates(ctx, "org-agg-cost", from, to, "")
	if err != nil {
		t.Fatalf("GetUsageAggregates() error = %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("GetUsageAggregates() returned 0 rows, want 1")
	}
	// 0.005 + 0 (NULL) + 0.002 = 0.007
	const wantCost = 0.007
	const tolerance = 1e-9
	if math.Abs(rows[0].CostEstimate-wantCost) > tolerance {
		t.Errorf("CostEstimate = %f, want %f (NULL cost must be treated as 0)", rows[0].CostEstimate, wantCost)
	}
}

func TestGetUsageAggregates_AvgDuration_Calculated(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)

	// Three events with known durations: 100ms, 200ms, 300ms → avg = 200ms.
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-dur-1", keyID: "key-dur", orgID: "org-agg-dur",
		totalTokens: 10, promptTokens: 8, compTokens: 2,
		durationMS: int64Ptr(100),
		createdAt:  now.Add(-90 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-dur-2", keyID: "key-dur", orgID: "org-agg-dur",
		totalTokens: 10, promptTokens: 8, compTokens: 2,
		durationMS: int64Ptr(200),
		createdAt:  now.Add(-60 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-dur-3", keyID: "key-dur", orgID: "org-agg-dur",
		totalTokens: 10, promptTokens: 8, compTokens: 2,
		durationMS: int64Ptr(300),
		createdAt:  now.Add(-30 * time.Minute),
	})

	rows, err := d.GetUsageAggregates(ctx, "org-agg-dur", from, to, "")
	if err != nil {
		t.Fatalf("GetUsageAggregates() error = %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("GetUsageAggregates() returned 0 rows, want 1")
	}
	const wantAvg = 200.0
	const tolerance = 0.001
	if math.Abs(rows[0].AvgDurationMS-wantAvg) > tolerance {
		t.Errorf("AvgDurationMS = %f, want %f", rows[0].AvgDurationMS, wantAvg)
	}
}

// ---- GetUsageAggregates: cached-token sums (#179) --------------------------

// TestGetUsageAggregates_CachedTokens_UngroupedSum verifies that
// cached_read_tokens and cache_write_tokens are summed correctly across
// multiple events in the ungrouped (totals-only) query path.
func TestGetUsageAggregates_CachedTokens_UngroupedSum(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)

	insertUsageEvent(t, d, usageEventParams{
		id: "agg-cache-1", keyID: "key-cache", orgID: "org-agg-cache",
		promptTokens: 100, compTokens: 20, totalTokens: 120,
		cachedReadTokens: 40, cacheWriteTokens: 10,
		createdAt: now.Add(-90 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-cache-2", keyID: "key-cache", orgID: "org-agg-cache",
		promptTokens: 200, compTokens: 30, totalTokens: 230,
		cachedReadTokens: 70, cacheWriteTokens: 5,
		createdAt: now.Add(-60 * time.Minute),
	})

	rows, err := d.GetUsageAggregates(ctx, "org-agg-cache", from, to, "")
	if err != nil {
		t.Fatalf("GetUsageAggregates() error = %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 1", len(rows))
	}
	if rows[0].CachedReadTokens != 110 {
		t.Errorf("CachedReadTokens = %d, want 110", rows[0].CachedReadTokens)
	}
	if rows[0].CacheWriteTokens != 15 {
		t.Errorf("CacheWriteTokens = %d, want 15", rows[0].CacheWriteTokens)
	}
}

// TestGetUsageAggregates_CachedTokens_GroupedByModel verifies that cached-token
// sums are computed per group (not just globally) when group_by=model.
func TestGetUsageAggregates_CachedTokens_GroupedByModel(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)

	insertUsageEvent(t, d, usageEventParams{
		id: "agg-cache-m1", keyID: "key-cache-m", orgID: "org-agg-cache-m",
		modelName: "gpt-4", promptTokens: 100, compTokens: 20, totalTokens: 120,
		cachedReadTokens: 40, cacheWriteTokens: 0,
		createdAt: now.Add(-90 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "agg-cache-m2", keyID: "key-cache-m", orgID: "org-agg-cache-m",
		modelName: "claude-3-5-sonnet", promptTokens: 200, compTokens: 30, totalTokens: 230,
		cachedReadTokens: 50, cacheWriteTokens: 25,
		createdAt: now.Add(-60 * time.Minute),
	})

	rows, err := d.GetUsageAggregates(ctx, "org-agg-cache-m", from, to, "model")
	if err != nil {
		t.Fatalf("GetUsageAggregates() error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}

	byGroup := make(map[string]UsageAggregate, len(rows))
	for _, r := range rows {
		byGroup[r.GroupKey] = r
	}

	gpt4, ok := byGroup["gpt-4"]
	if !ok {
		t.Fatal("missing group for gpt-4")
	}
	if gpt4.CachedReadTokens != 40 {
		t.Errorf("gpt-4 CachedReadTokens = %d, want 40", gpt4.CachedReadTokens)
	}
	if gpt4.CacheWriteTokens != 0 {
		t.Errorf("gpt-4 CacheWriteTokens = %d, want 0", gpt4.CacheWriteTokens)
	}

	claude, ok := byGroup["claude-3-5-sonnet"]
	if !ok {
		t.Fatal("missing group for claude-3-5-sonnet")
	}
	if claude.CachedReadTokens != 50 {
		t.Errorf("claude-3-5-sonnet CachedReadTokens = %d, want 50", claude.CachedReadTokens)
	}
	if claude.CacheWriteTokens != 25 {
		t.Errorf("claude-3-5-sonnet CacheWriteTokens = %d, want 25", claude.CacheWriteTokens)
	}
}

// TestGetUsageAggregates_CachedTokens_EmptyResultIsZero verifies that an org
// with no events in range returns zero (not NULL, not an error) for the two
// new cached-token columns, matching the COALESCE(SUM(...), 0) pattern used
// for the pre-existing token columns.
func TestGetUsageAggregates_CachedTokens_EmptyResultIsZero(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()

	rows, err := d.GetUsageAggregates(ctx, "org-agg-cache-empty", now.Add(-time.Hour), now, "")
	if err != nil {
		t.Fatalf("GetUsageAggregates() error = %v", err)
	}
	if len(rows) == 0 {
		return // acceptable — empty result is also valid, mirrors TestGetUsageAggregates_NoEvents_NoGrouping
	}
	if len(rows) != 1 {
		t.Fatalf("len(rows) = %d, want 0 or 1", len(rows))
	}
	if rows[0].CachedReadTokens != 0 {
		t.Errorf("CachedReadTokens = %d, want 0", rows[0].CachedReadTokens)
	}
	if rows[0].CacheWriteTokens != 0 {
		t.Errorf("CacheWriteTokens = %d, want 0", rows[0].CacheWriteTokens)
	}
}

// ---- GetScopedUsageAggregates / GetCrossOrgUsageAggregates: cached tokens --
//
// These two functions had no test coverage at all prior to #179 (grouped and
// ungrouped alike). The pair of smoke tests below exercises their cached-token
// SQL literals specifically, since each function carries its own independent
// COALESCE(SUM(cached_read_tokens/cache_write_tokens)) pair.

// TestGetScopedUsageAggregates_CachedTokens_Summed verifies that
// GetScopedUsageAggregates (used by MyUsage) sums the two new cache-token
// columns correctly in both the ungrouped and grouped query paths.
func TestGetScopedUsageAggregates_CachedTokens_Summed(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)

	insertUsageEvent(t, d, usageEventParams{
		id: "scoped-cache-1", keyID: "key-scoped-cache", orgID: "org-scoped-cache",
		promptTokens: 100, compTokens: 20, totalTokens: 120,
		cachedReadTokens: 40, cacheWriteTokens: 10,
		createdAt: now.Add(-90 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "scoped-cache-2", keyID: "key-scoped-cache", orgID: "org-scoped-cache",
		promptTokens: 50, compTokens: 5, totalTokens: 55,
		cachedReadTokens: 15, cacheWriteTokens: 0,
		createdAt: now.Add(-60 * time.Minute),
	})

	filter := UsageFilter{OrgID: "org-scoped-cache", KeyID: "key-scoped-cache"}

	ungrouped, err := d.GetScopedUsageAggregates(ctx, filter, from, to, "")
	if err != nil {
		t.Fatalf("GetScopedUsageAggregates(ungrouped) error = %v", err)
	}
	if len(ungrouped) != 1 {
		t.Fatalf("len(ungrouped) = %d, want 1", len(ungrouped))
	}
	if ungrouped[0].CachedReadTokens != 55 {
		t.Errorf("ungrouped CachedReadTokens = %d, want 55", ungrouped[0].CachedReadTokens)
	}
	if ungrouped[0].CacheWriteTokens != 10 {
		t.Errorf("ungrouped CacheWriteTokens = %d, want 10", ungrouped[0].CacheWriteTokens)
	}

	grouped, err := d.GetScopedUsageAggregates(ctx, filter, from, to, "key")
	if err != nil {
		t.Fatalf("GetScopedUsageAggregates(grouped) error = %v", err)
	}
	if len(grouped) != 1 {
		t.Fatalf("len(grouped) = %d, want 1", len(grouped))
	}
	if grouped[0].CachedReadTokens != 55 {
		t.Errorf("grouped CachedReadTokens = %d, want 55", grouped[0].CachedReadTokens)
	}
	if grouped[0].CacheWriteTokens != 10 {
		t.Errorf("grouped CacheWriteTokens = %d, want 10", grouped[0].CacheWriteTokens)
	}
}

// TestGetCrossOrgUsageAggregates_CachedTokens_Summed verifies that
// GetCrossOrgUsageAggregates (used by the system-admin usage endpoint) sums
// the two new cache-token columns correctly in both the ungrouped and
// grouped (by org) query paths, across events from multiple orgs.
func TestGetCrossOrgUsageAggregates_CachedTokens_Summed(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)

	insertUsageEvent(t, d, usageEventParams{
		id: "xorg-cache-1", keyID: "key-xorg-1", orgID: "org-xorg-a",
		promptTokens: 100, compTokens: 20, totalTokens: 120,
		cachedReadTokens: 30, cacheWriteTokens: 5,
		createdAt: now.Add(-90 * time.Minute),
	})
	insertUsageEvent(t, d, usageEventParams{
		id: "xorg-cache-2", keyID: "key-xorg-2", orgID: "org-xorg-b",
		promptTokens: 200, compTokens: 40, totalTokens: 240,
		cachedReadTokens: 60, cacheWriteTokens: 15,
		createdAt: now.Add(-60 * time.Minute),
	})

	ungrouped, err := d.GetCrossOrgUsageAggregates(ctx, from, to, "")
	if err != nil {
		t.Fatalf("GetCrossOrgUsageAggregates(ungrouped) error = %v", err)
	}
	if len(ungrouped) != 1 {
		t.Fatalf("len(ungrouped) = %d, want 1", len(ungrouped))
	}
	if ungrouped[0].CachedReadTokens != 90 {
		t.Errorf("ungrouped CachedReadTokens = %d, want 90", ungrouped[0].CachedReadTokens)
	}
	if ungrouped[0].CacheWriteTokens != 20 {
		t.Errorf("ungrouped CacheWriteTokens = %d, want 20", ungrouped[0].CacheWriteTokens)
	}

	grouped, err := d.GetCrossOrgUsageAggregates(ctx, from, to, "org")
	if err != nil {
		t.Fatalf("GetCrossOrgUsageAggregates(grouped) error = %v", err)
	}
	byGroup := make(map[string]UsageAggregate, len(grouped))
	for _, r := range grouped {
		byGroup[r.GroupKey] = r
	}
	a, ok := byGroup["org-xorg-a"]
	if !ok {
		t.Fatal("missing group for org-xorg-a")
	}
	if a.CachedReadTokens != 30 {
		t.Errorf("org-xorg-a CachedReadTokens = %d, want 30", a.CachedReadTokens)
	}
	b, ok := byGroup["org-xorg-b"]
	if !ok {
		t.Fatal("missing group for org-xorg-b")
	}
	if b.CachedReadTokens != 60 {
		t.Errorf("org-xorg-b CachedReadTokens = %d, want 60", b.CachedReadTokens)
	}
	if b.CacheWriteTokens != 15 {
		t.Errorf("org-xorg-b CacheWriteTokens = %d, want 15", b.CacheWriteTokens)
	}
}
