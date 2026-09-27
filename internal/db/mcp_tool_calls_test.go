package db

import (
	"context"
	"testing"
	"time"
)

// mustInsertMCPToolCall calls InsertMCPToolCalls with a single call and fatals
// the test on error.
func mustInsertMCPToolCall(t *testing.T, d *DB, call MCPToolCall) {
	t.Helper()
	if err := d.InsertMCPToolCalls(context.Background(), []MCPToolCall{call}); err != nil {
		t.Fatalf("InsertMCPToolCalls: %v", err)
	}
}

// TestInsertMCPToolCalls_EmptyCreatedAtDefaultsToNow verifies that a call
// with an empty CreatedAt is stamped with the current time, already in the
// canonical shape, rather than being left blank or rejected.
func TestInsertMCPToolCalls_EmptyCreatedAtDefaultsToNow(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	id := newSortableID(t)

	before := time.Now().UTC()
	mustInsertMCPToolCall(t, d, MCPToolCall{
		ID:          id,
		KeyID:       "key-1",
		KeyType:     "user_key",
		OrgID:       "org-1",
		ServerAlias: "server-a",
		ToolName:    "tool-a",
		Status:      "success",
	})
	after := time.Now().UTC()

	got := rawColumnValue(t, d, "mcp_tool_calls", "created_at", id)
	parsed, err := time.Parse(TimestampLayout, got)
	if err != nil {
		t.Fatalf("created_at %q is not canonical: %v", got, err)
	}
	if parsed.Before(before.Truncate(time.Second)) || parsed.After(after.Add(time.Second)) {
		t.Errorf("created_at = %v, want within [%v, %v]", parsed, before, after)
	}
}

// TestInsertMCPToolCalls_CanonicalCreatedAtAccepted verifies that a call
// whose CreatedAt is already in the canonical shape is stored verbatim.
func TestInsertMCPToolCalls_CanonicalCreatedAtAccepted(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	id := newSortableID(t)
	const want = "2026-09-26T21:23:17Z"

	mustInsertMCPToolCall(t, d, MCPToolCall{
		ID:          id,
		KeyID:       "key-1",
		KeyType:     "user_key",
		OrgID:       "org-1",
		ServerAlias: "server-a",
		ToolName:    "tool-a",
		Status:      "success",
		CreatedAt:   want,
	})

	if got := rawColumnValue(t, d, "mcp_tool_calls", "created_at", id); got != want {
		t.Errorf("created_at = %q, want %q", got, want)
	}
}

// TestInsertMCPToolCalls_NonCanonicalCreatedAtRejected verifies that
// InsertMCPToolCalls refuses to insert a call whose CreatedAt is a non-empty
// value that is not already in the canonical shape, rather than silently
// reparsing or truncating it. This is the invariant that keeps every writer
// of this column on the single FormatTimestamp code path.
func TestInsertMCPToolCalls_NonCanonicalCreatedAtRejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		createdAt string
	}{
		{name: "RFC3339 with offset instead of Z", createdAt: "2026-09-26T23:23:17+02:00"},
		{name: "SQLite legacy space-separated shape", createdAt: "2026-09-26 21:23:17"},
		{name: "garbage", createdAt: "not-a-timestamp"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			d := openMigratedDB(t)
			err := d.InsertMCPToolCalls(context.Background(), []MCPToolCall{{
				ID:          newSortableID(t),
				KeyID:       "key-1",
				KeyType:     "user_key",
				OrgID:       "org-1",
				ServerAlias: "server-a",
				ToolName:    "tool-a",
				Status:      "success",
				CreatedAt:   tc.createdAt,
			}})
			if err == nil {
				t.Fatalf("InsertMCPToolCalls(CreatedAt=%q) error = nil, want error", tc.createdAt)
			}
		})
	}
}

// TestInsertMCPToolCalls_EmptySliceIsNoop verifies that InsertMCPToolCalls
// returns nil without touching the database when called with an empty slice.
func TestInsertMCPToolCalls_EmptySliceIsNoop(t *testing.T) {
	t.Parallel()

	d := openMigratedDB(t)
	if err := d.InsertMCPToolCalls(context.Background(), nil); err != nil {
		t.Errorf("InsertMCPToolCalls(nil) error = %v, want nil", err)
	}

	var count int
	if err := d.sql.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM mcp_tool_calls").Scan(&count); err != nil {
		t.Fatalf("count mcp_tool_calls: %v", err)
	}
	if count != 0 {
		t.Errorf("mcp_tool_calls row count = %d, want 0", count)
	}
}
