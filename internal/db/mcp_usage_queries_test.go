package db

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// insertMCPToolCall inserts a single mcp_tool_calls row directly into the DB.
func insertMCPToolCall(t *testing.T, d *DB, id, orgID, teamID, userID, serverAlias, toolName, status string, durationMS int, codeMode bool, createdAt time.Time) {
	t.Helper()
	teamVal := "NULL"
	if teamID != "" {
		teamVal = fmt.Sprintf("'%s'", teamID)
	}
	userVal := "NULL"
	if userID != "" {
		userVal = fmt.Sprintf("'%s'", userID)
	}
	codeModeVal := 0
	if codeMode {
		codeModeVal = 1
	}
	query := fmt.Sprintf(
		`INSERT INTO mcp_tool_calls
			(id, key_id, key_type, org_id, team_id, user_id,
			 server_alias, tool_name, duration_ms, status, code_mode, created_at)
		 VALUES
			('%s', 'key-1', 'user_key', '%s', %s, %s,
			 '%s', '%s', %d, '%s', %d, '%s')`,
		id, orgID, teamVal, userVal,
		serverAlias, toolName, durationMS, status, codeModeVal,
		createdAt.UTC().Format(time.RFC3339),
	)
	if _, err := d.sql.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("insertMCPToolCall id=%q: %v", id, err)
	}
}

// ---- GetMCPUsageAggregates ---------------------------------------------------

func TestGetMCPUsageAggregates_NoRows_GroupBy_ReturnsEmpty(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)

	// With a groupBy dimension and no matching rows, no rows are returned at all
	// (GROUP BY on an empty result set produces zero groups).
	results, err := d.GetMCPUsageAggregates(context.Background(), "org-empty-grouped", from, to, "server")
	if err != nil {
		t.Fatalf("GetMCPUsageAggregates(groupBy=server) with no data error = %v, want nil", err)
	}
	if len(results) != 0 {
		t.Errorf("len(results) = %d, want 0 (no rows → no groups)", len(results))
	}
}

func TestGetMCPUsageAggregates_InvalidGroupBy_ReturnsError(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()

	_, err := d.GetMCPUsageAggregates(context.Background(), "org-1", now.Add(-time.Hour), now, "invalid_col")
	if err == nil {
		t.Fatal("GetMCPUsageAggregates() error = nil, want error for invalid groupBy")
	}
}

func TestGetMCPUsageAggregates_GroupByValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		groupBy        string
		wantGroupKey   string
		wantMinResults int
	}{
		{name: "totals only", groupBy: "", wantMinResults: 1},
		{name: "server", groupBy: "server", wantGroupKey: "server-a", wantMinResults: 1},
		{name: "tool", groupBy: "tool", wantGroupKey: "do_thing", wantMinResults: 1},
		{name: "team", groupBy: "team", wantGroupKey: "team-1", wantMinResults: 1},
		{name: "status", groupBy: "status", wantGroupKey: "success", wantMinResults: 1},
		{name: "day", groupBy: "day", wantMinResults: 1},
		{name: "hour", groupBy: "hour", wantMinResults: 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			now := time.Now().UTC()
			from := now.Add(-2 * time.Hour)
			to := now.Add(time.Minute)

			orgID := "org-groupby-" + tc.groupBy

			insertMCPToolCall(t, d, "tc-1-"+tc.groupBy, orgID, "team-1", "", "server-a", "do_thing", "success", 100, false, now.Add(-90*time.Minute))
			insertMCPToolCall(t, d, "tc-2-"+tc.groupBy, orgID, "team-1", "", "server-a", "do_thing", "success", 200, true, now.Add(-60*time.Minute))

			results, err := d.GetMCPUsageAggregates(context.Background(), orgID, from, to, tc.groupBy)
			if err != nil {
				t.Fatalf("GetMCPUsageAggregates(groupBy=%q) error = %v", tc.groupBy, err)
			}
			if len(results) < tc.wantMinResults {
				t.Fatalf("len(results) = %d, want >= %d", len(results), tc.wantMinResults)
			}
			if tc.wantGroupKey != "" {
				found := false
				for _, r := range results {
					if r.GroupKey == tc.wantGroupKey {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("group key %q not found in results", tc.wantGroupKey)
				}
			}
		})
	}
}

func TestGetMCPUsageAggregates_TotalCounts(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)
	orgID := "org-totals"

	insertMCPToolCall(t, d, "t1", orgID, "", "", "server-a", "tool-x", "success", 50, false, now.Add(-90*time.Minute))
	insertMCPToolCall(t, d, "t2", orgID, "", "", "server-a", "tool-x", "error", 120, false, now.Add(-60*time.Minute))
	insertMCPToolCall(t, d, "t3", orgID, "", "", "server-b", "tool-y", "timeout", 0, true, now.Add(-30*time.Minute))
	insertMCPToolCall(t, d, "t4", orgID, "", "", "server-b", "tool-y", "success", 200, true, now.Add(-15*time.Minute))

	results, err := d.GetMCPUsageAggregates(context.Background(), orgID, from, to, "")
	if err != nil {
		t.Fatalf("GetMCPUsageAggregates() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	row := results[0]
	if row.TotalCalls != 4 {
		t.Errorf("TotalCalls = %d, want 4", row.TotalCalls)
	}
	if row.SuccessCount != 2 {
		t.Errorf("SuccessCount = %d, want 2", row.SuccessCount)
	}
	if row.ErrorCount != 1 {
		t.Errorf("ErrorCount = %d, want 1", row.ErrorCount)
	}
	if row.TimeoutCount != 1 {
		t.Errorf("TimeoutCount = %d, want 1", row.TimeoutCount)
	}
	if row.CodeModeCalls != 2 {
		t.Errorf("CodeModeCalls = %d, want 2", row.CodeModeCalls)
	}
}

func TestGetMCPUsageAggregates_NullTeamID_DoesNotCrash(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)
	orgID := "org-null-team"

	// Insert calls with NULL team_id and NULL user_id to verify coalesceSelectCol
	// handles NULL values without a scan error.
	insertMCPToolCall(t, d, "nt-1", orgID, "", "", "server-a", "tool-a", "success", 10, false, now.Add(-30*time.Minute))
	insertMCPToolCall(t, d, "nt-2", orgID, "", "", "server-a", "tool-a", "success", 20, false, now.Add(-20*time.Minute))

	results, err := d.GetMCPUsageAggregates(context.Background(), orgID, from, to, "team")
	if err != nil {
		t.Fatalf("GetMCPUsageAggregates(groupBy=team) with NULL team_id error = %v", err)
	}
	// NULL team_id rows are coalesced to empty string — one group expected.
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].TotalCalls != 2 {
		t.Errorf("TotalCalls = %d, want 2", results[0].TotalCalls)
	}
}

func TestGetMCPUsageAggregates_GroupByDay_ChronologicalOrder(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	orgID := "org-day-order"

	// Insert calls on three distinct days.
	day1 := now.Add(-48 * time.Hour).Truncate(24 * time.Hour).Add(12 * time.Hour)
	day2 := now.Add(-24 * time.Hour).Truncate(24 * time.Hour).Add(12 * time.Hour)
	day3 := now.Truncate(24 * time.Hour).Add(12 * time.Hour)
	from := now.Add(-72 * time.Hour)
	to := now.Add(time.Hour)

	insertMCPToolCall(t, d, "d3-1", orgID, "", "", "server-a", "tool-a", "success", 10, false, day3)
	insertMCPToolCall(t, d, "d1-1", orgID, "", "", "server-a", "tool-a", "success", 10, false, day1)
	insertMCPToolCall(t, d, "d2-1", orgID, "", "", "server-a", "tool-a", "success", 10, false, day2)

	results, err := d.GetMCPUsageAggregates(context.Background(), orgID, from, to, "day")
	if err != nil {
		t.Fatalf("GetMCPUsageAggregates(groupBy=day) error = %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	// Results must be in ascending (chronological) order.
	for i := 1; i < len(results); i++ {
		if results[i].GroupKey < results[i-1].GroupKey {
			t.Errorf("results not in chronological order: results[%d].GroupKey=%q < results[%d].GroupKey=%q",
				i, results[i].GroupKey, i-1, results[i-1].GroupKey)
		}
	}
}

func TestGetMCPUsageAggregates_GroupByStatus_CountDescendingOrder(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	from := now.Add(-2 * time.Hour)
	to := now.Add(time.Minute)
	orgID := "org-status-order"

	// Insert 3 success, 1 error — success should sort first (count DESC).
	for i := range 3 {
		insertMCPToolCall(t, d, fmt.Sprintf("s-%d", i), orgID, "", "", "server-a", "tool-a", "success", 10, false, now.Add(-time.Duration(i+1)*time.Minute))
	}
	insertMCPToolCall(t, d, "e-1", orgID, "", "", "server-a", "tool-a", "error", 10, false, now.Add(-10*time.Minute))

	results, err := d.GetMCPUsageAggregates(context.Background(), orgID, from, to, "status")
	if err != nil {
		t.Fatalf("GetMCPUsageAggregates(groupBy=status) error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].GroupKey != "success" {
		t.Errorf("results[0].GroupKey = %q, want %q (count DESC)", results[0].GroupKey, "success")
	}
}

func TestGetMCPUsageAggregates_ExcludesOutOfRangeEvents(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(-30 * time.Minute)
	orgID := "org-range"

	// Inside range.
	insertMCPToolCall(t, d, "in-1", orgID, "", "", "server-a", "tool-a", "success", 10, false, now.Add(-45*time.Minute))
	// Outside range (before from).
	insertMCPToolCall(t, d, "out-1", orgID, "", "", "server-a", "tool-a", "success", 10, false, now.Add(-90*time.Minute))
	// Outside range (after to).
	insertMCPToolCall(t, d, "out-2", orgID, "", "", "server-a", "tool-a", "success", 10, false, now.Add(-10*time.Minute))

	results, err := d.GetMCPUsageAggregates(context.Background(), orgID, from, to, "")
	if err != nil {
		t.Fatalf("GetMCPUsageAggregates() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].TotalCalls != 1 {
		t.Errorf("TotalCalls = %d, want 1 (out-of-range events must be excluded)", results[0].TotalCalls)
	}
}

func TestGetMCPUsageAggregates_ExcludesOtherOrgs(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)
	orgID := "org-isolation-a"

	insertMCPToolCall(t, d, "a-1", orgID, "", "", "server-a", "tool-a", "success", 10, false, now.Add(-30*time.Minute))
	insertMCPToolCall(t, d, "b-1", "org-isolation-b", "", "", "server-a", "tool-a", "success", 10, false, now.Add(-30*time.Minute))

	results, err := d.GetMCPUsageAggregates(context.Background(), orgID, from, to, "")
	if err != nil {
		t.Fatalf("GetMCPUsageAggregates() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].TotalCalls != 1 {
		t.Errorf("TotalCalls = %d, want 1 (other org's events must be excluded)", results[0].TotalCalls)
	}
}

// ---- GetCrossOrgMCPUsageAggregates -------------------------------------------

func TestGetCrossOrgMCPUsageAggregates_GroupByOrg(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)

	insertMCPToolCall(t, d, "x-1", "org-cross-a", "", "", "server-a", "tool-a", "success", 10, false, now.Add(-30*time.Minute))
	insertMCPToolCall(t, d, "x-2", "org-cross-a", "", "", "server-a", "tool-a", "success", 20, false, now.Add(-20*time.Minute))
	insertMCPToolCall(t, d, "x-3", "org-cross-b", "", "", "server-a", "tool-a", "success", 15, false, now.Add(-15*time.Minute))

	results, err := d.GetCrossOrgMCPUsageAggregates(context.Background(), from, to, "org")
	if err != nil {
		t.Fatalf("GetCrossOrgMCPUsageAggregates(groupBy=org) error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	// The org with more calls (org-cross-a) should appear first (count DESC).
	if results[0].TotalCalls < results[1].TotalCalls {
		t.Errorf("results not in count-desc order: results[0].TotalCalls=%d < results[1].TotalCalls=%d",
			results[0].TotalCalls, results[1].TotalCalls)
	}
}

func TestGetCrossOrgMCPUsageAggregates_Totals(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)

	insertMCPToolCall(t, d, "ct-1", "org-ct-a", "", "", "server-a", "tool-a", "success", 100, false, now.Add(-30*time.Minute))
	insertMCPToolCall(t, d, "ct-2", "org-ct-b", "", "", "server-a", "tool-a", "error", 200, false, now.Add(-20*time.Minute))

	results, err := d.GetCrossOrgMCPUsageAggregates(context.Background(), from, to, "")
	if err != nil {
		t.Fatalf("GetCrossOrgMCPUsageAggregates() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].TotalCalls != 2 {
		t.Errorf("TotalCalls = %d, want 2", results[0].TotalCalls)
	}
	if results[0].SuccessCount != 1 {
		t.Errorf("SuccessCount = %d, want 1", results[0].SuccessCount)
	}
	if results[0].ErrorCount != 1 {
		t.Errorf("ErrorCount = %d, want 1", results[0].ErrorCount)
	}
}

func TestGetCrossOrgMCPUsageAggregates_InvalidGroupBy_ReturnsError(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()

	_, err := d.GetCrossOrgMCPUsageAggregates(context.Background(), now.Add(-time.Hour), now, "badcol")
	if err == nil {
		t.Fatal("GetCrossOrgMCPUsageAggregates() error = nil, want error for invalid groupBy")
	}
}

// ---- GetScopedMCPUsageAggregates ---------------------------------------------

func TestGetScopedMCPUsageAggregates_TeamFilter(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)
	orgID := "org-scoped"

	insertMCPToolCall(t, d, "sc-1", orgID, "team-alpha", "", "server-a", "tool-a", "success", 10, false, now.Add(-30*time.Minute))
	insertMCPToolCall(t, d, "sc-2", orgID, "team-alpha", "", "server-a", "tool-a", "success", 20, false, now.Add(-20*time.Minute))
	insertMCPToolCall(t, d, "sc-3", orgID, "team-beta", "", "server-a", "tool-a", "success", 30, false, now.Add(-15*time.Minute))

	filter := MCPUsageFilter{OrgID: orgID, TeamID: "team-alpha"}
	results, err := d.GetScopedMCPUsageAggregates(context.Background(), filter, from, to, "")
	if err != nil {
		t.Fatalf("GetScopedMCPUsageAggregates() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].TotalCalls != 2 {
		t.Errorf("TotalCalls = %d, want 2 (only team-alpha events)", results[0].TotalCalls)
	}
}

func TestGetScopedMCPUsageAggregates_NoAdditionalFilter_SameAsGetMCPUsageAggregates(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)
	orgID := "org-scoped-nofilter"

	insertMCPToolCall(t, d, "nf-1", orgID, "", "", "server-a", "tool-a", "success", 50, false, now.Add(-30*time.Minute))
	insertMCPToolCall(t, d, "nf-2", orgID, "", "", "server-b", "tool-b", "error", 80, false, now.Add(-20*time.Minute))

	filter := MCPUsageFilter{OrgID: orgID}
	results, err := d.GetScopedMCPUsageAggregates(context.Background(), filter, from, to, "")
	if err != nil {
		t.Fatalf("GetScopedMCPUsageAggregates() error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].TotalCalls != 2 {
		t.Errorf("TotalCalls = %d, want 2", results[0].TotalCalls)
	}
}

func TestGetScopedMCPUsageAggregates_InvalidGroupBy_ReturnsError(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()

	filter := MCPUsageFilter{OrgID: "org-1"}
	_, err := d.GetScopedMCPUsageAggregates(context.Background(), filter, now.Add(-time.Hour), now, "notvalid")
	if err == nil {
		t.Fatal("GetScopedMCPUsageAggregates() error = nil, want error for invalid groupBy")
	}
}

func TestGetScopedMCPUsageAggregates_GroupByServer(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)
	orgID := "org-scoped-server"

	insertMCPToolCall(t, d, "sv-1", orgID, "", "", "server-x", "tool-a", "success", 10, false, now.Add(-30*time.Minute))
	insertMCPToolCall(t, d, "sv-2", orgID, "", "", "server-x", "tool-a", "success", 10, false, now.Add(-25*time.Minute))
	insertMCPToolCall(t, d, "sv-3", orgID, "", "", "server-y", "tool-a", "success", 10, false, now.Add(-20*time.Minute))

	filter := MCPUsageFilter{OrgID: orgID}
	results, err := d.GetScopedMCPUsageAggregates(context.Background(), filter, from, to, "server")
	if err != nil {
		t.Fatalf("GetScopedMCPUsageAggregates(groupBy=server) error = %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	// server-x has 2 calls — should be first (count DESC).
	if results[0].GroupKey != "server-x" {
		t.Errorf("results[0].GroupKey = %q, want %q", results[0].GroupKey, "server-x")
	}
}

func TestGetScopedMCPUsageAggregates_NullUserID_DoesNotCrash(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	now := time.Now().UTC()
	from := now.Add(-time.Hour)
	to := now.Add(time.Minute)
	orgID := "org-null-user"

	insertMCPToolCall(t, d, "nu-1", orgID, "", "", "server-a", "tool-a", "success", 10, false, now.Add(-30*time.Minute))

	filter := MCPUsageFilter{OrgID: orgID}
	results, err := d.GetScopedMCPUsageAggregates(context.Background(), filter, from, to, "user")
	if err != nil {
		t.Fatalf("GetScopedMCPUsageAggregates(groupBy=user) with NULL user_id error = %v", err)
	}
	// NULL user_id rows are coalesced to empty string — one group expected.
	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
}
