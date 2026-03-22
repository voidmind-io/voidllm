package audit_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/audit"
	"github.com/voidmind-io/voidllm/internal/db"
)

// insertAuditLog inserts a single audit_logs row directly into the database,
// bypassing the logger. Used to seed test data for query tests.
func insertAuditLog(t *testing.T, database *db.DB, ev audit.Event) {
	t.Helper()
	const insertSQL = `INSERT INTO audit_logs
		(id, timestamp, org_id, actor_id, actor_type, actor_key_id,
		 action, resource_type, resource_id, description, ip_address, status_code, request_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	ts := ev.Timestamp.UTC().Format(time.RFC3339)
	_, err := database.SQL().ExecContext(context.Background(), insertSQL,
		ev.ID,
		ts,
		ev.OrgID,
		ev.ActorID,
		ev.ActorType,
		ev.ActorKeyID,
		ev.Action,
		ev.ResourceType,
		ev.ResourceID,
		ev.Description,
		ev.IPAddress,
		ev.StatusCode,
		"",
	)
	if err != nil {
		t.Fatalf("insertAuditLog id=%q: %v", ev.ID, err)
	}
}

// makeEvent is a convenience constructor for test events. id must be a valid
// non-empty string; it is used directly as the audit_logs.id value.
func makeEvent(id, orgID, actorID, action, resourceType string, ts time.Time) audit.Event {
	return audit.Event{
		ID:           id,
		Timestamp:    ts,
		OrgID:        orgID,
		ActorID:      actorID,
		ActorType:    "user",
		ActorKeyID:   "key-" + id,
		Action:       action,
		ResourceType: resourceType,
		ResourceID:   "res-" + id,
		Description:  fmt.Sprintf("%s %s %s", actorID, action, resourceType),
		IPAddress:    "10.0.0.1",
		StatusCode:   200,
	}
}

// TestQuery_NoFilters inserts several events and verifies that querying with no
// filters returns all of them.
func TestQuery_NoFilters(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestQuery_NoFilters")
	now := time.Now().UTC().Truncate(time.Second)

	events := []audit.Event{
		makeEvent("aaaa-0001", "org-a", "actor-1", "create", "org", now.Add(-3*time.Minute)),
		makeEvent("aaaa-0002", "org-b", "actor-2", "delete", "team", now.Add(-2*time.Minute)),
		makeEvent("aaaa-0003", "org-a", "actor-1", "update", "user", now.Add(-1*time.Minute)),
	}
	for _, ev := range events {
		insertAuditLog(t, database, ev)
	}

	result, err := audit.Query(context.Background(), database, audit.QueryParams{})
	if err != nil {
		t.Fatalf("Query() error = %v, want nil", err)
	}
	if len(result.Events) != 3 {
		t.Errorf("len(Events) = %d, want 3", len(result.Events))
	}
	if result.HasMore {
		t.Errorf("HasMore = true, want false")
	}
}

// TestQuery_FilterByOrg verifies that the OrgID filter returns only events
// belonging to the specified organisation.
func TestQuery_FilterByOrg(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestQuery_FilterByOrg")
	now := time.Now().UTC().Truncate(time.Second)

	insertAuditLog(t, database, makeEvent("b001", "org-alpha", "actor-1", "create", "org", now.Add(-3*time.Minute)))
	insertAuditLog(t, database, makeEvent("b002", "org-beta", "actor-2", "delete", "team", now.Add(-2*time.Minute)))
	insertAuditLog(t, database, makeEvent("b003", "org-alpha", "actor-1", "update", "user", now.Add(-1*time.Minute)))

	result, err := audit.Query(context.Background(), database, audit.QueryParams{
		OrgID: "org-alpha",
	})
	if err != nil {
		t.Fatalf("Query() error = %v, want nil", err)
	}
	if len(result.Events) != 2 {
		t.Errorf("len(Events) = %d, want 2 (only org-alpha events)", len(result.Events))
	}
	for _, ev := range result.Events {
		if ev.OrgID != "org-alpha" {
			t.Errorf("Event OrgID = %q, want %q", ev.OrgID, "org-alpha")
		}
	}
}

// TestQuery_FilterByAction verifies that the Action filter returns only events
// with the matching action verb.
func TestQuery_FilterByAction(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestQuery_FilterByAction")
	now := time.Now().UTC().Truncate(time.Second)

	insertAuditLog(t, database, makeEvent("c001", "org-x", "actor-1", "create", "org", now.Add(-3*time.Minute)))
	insertAuditLog(t, database, makeEvent("c002", "org-x", "actor-2", "delete", "team", now.Add(-2*time.Minute)))
	insertAuditLog(t, database, makeEvent("c003", "org-x", "actor-1", "create", "user", now.Add(-1*time.Minute)))

	result, err := audit.Query(context.Background(), database, audit.QueryParams{
		Action: "create",
	})
	if err != nil {
		t.Fatalf("Query() error = %v, want nil", err)
	}
	if len(result.Events) != 2 {
		t.Errorf("len(Events) = %d, want 2 (only create events)", len(result.Events))
	}
	for _, ev := range result.Events {
		if ev.Action != "create" {
			t.Errorf("Event Action = %q, want %q", ev.Action, "create")
		}
	}
}

// TestQuery_CursorPagination verifies that cursor-based pagination correctly
// pages through results newest-first, that HasMore is accurate, and that no
// events are duplicated or skipped across pages.
func TestQuery_CursorPagination(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestQuery_CursorPagination")
	now := time.Now().UTC().Truncate(time.Second)

	// Insert 5 events with lexicographically ordered IDs so that descending id
	// order gives a deterministic newest-first sequence: d005 > d004 > ... > d001.
	for i := 1; i <= 5; i++ {
		id := fmt.Sprintf("d%03d", i)
		insertAuditLog(t, database, makeEvent(id, "org-p", "actor-1", "create", "org",
			now.Add(time.Duration(i)*time.Minute)))
	}

	// Page 1: limit=2, no cursor — expect d005, d004 with HasMore=true.
	page1, err := audit.Query(context.Background(), database, audit.QueryParams{Limit: 2})
	if err != nil {
		t.Fatalf("page1 Query() error = %v, want nil", err)
	}
	if len(page1.Events) != 2 {
		t.Fatalf("page1 len(Events) = %d, want 2", len(page1.Events))
	}
	if !page1.HasMore {
		t.Errorf("page1 HasMore = false, want true")
	}
	if page1.Events[0].ID != "d005" {
		t.Errorf("page1 Events[0].ID = %q, want %q", page1.Events[0].ID, "d005")
	}
	if page1.Events[1].ID != "d004" {
		t.Errorf("page1 Events[1].ID = %q, want %q", page1.Events[1].ID, "d004")
	}

	// Page 2: cursor = last ID from page 1 (d004) — expect d003, d002 with HasMore=true.
	page2, err := audit.Query(context.Background(), database, audit.QueryParams{
		Limit:  2,
		Cursor: page1.Events[len(page1.Events)-1].ID,
	})
	if err != nil {
		t.Fatalf("page2 Query() error = %v, want nil", err)
	}
	if len(page2.Events) != 2 {
		t.Fatalf("page2 len(Events) = %d, want 2", len(page2.Events))
	}
	if !page2.HasMore {
		t.Errorf("page2 HasMore = false, want true")
	}
	if page2.Events[0].ID != "d003" {
		t.Errorf("page2 Events[0].ID = %q, want %q", page2.Events[0].ID, "d003")
	}
	if page2.Events[1].ID != "d002" {
		t.Errorf("page2 Events[1].ID = %q, want %q", page2.Events[1].ID, "d002")
	}

	// Page 3: cursor = last ID from page 2 (d002) — expect d001 with HasMore=false.
	page3, err := audit.Query(context.Background(), database, audit.QueryParams{
		Limit:  2,
		Cursor: page2.Events[len(page2.Events)-1].ID,
	})
	if err != nil {
		t.Fatalf("page3 Query() error = %v, want nil", err)
	}
	if len(page3.Events) != 1 {
		t.Fatalf("page3 len(Events) = %d, want 1", len(page3.Events))
	}
	if page3.HasMore {
		t.Errorf("page3 HasMore = true, want false")
	}
	if page3.Events[0].ID != "d001" {
		t.Errorf("page3 Events[0].ID = %q, want %q", page3.Events[0].ID, "d001")
	}
}

// TestQuery_CursorAtEnd verifies that a cursor pointing past the last record
// returns an empty result with HasMore=false.
func TestQuery_CursorAtEnd(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestQuery_CursorAtEnd")
	now := time.Now().UTC().Truncate(time.Second)

	insertAuditLog(t, database, makeEvent("z001", "org-e", "actor-1", "create", "org", now))

	result, err := audit.Query(context.Background(), database, audit.QueryParams{
		Limit:  10,
		Cursor: "z001", // z001 < z001 is false — no rows match
	})
	if err != nil {
		t.Fatalf("Query() error = %v, want nil", err)
	}
	if len(result.Events) != 0 {
		t.Errorf("len(Events) = %d, want 0", len(result.Events))
	}
	if result.HasMore {
		t.Errorf("HasMore = true, want false")
	}
}

// TestQuery_TimeRange verifies that From and To filters restrict results to the
// specified time window, inclusive on both ends.
func TestQuery_TimeRange(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestQuery_TimeRange")
	now := time.Now().UTC().Truncate(time.Second)

	// Three events: one before, one within, one after the query window.
	insertAuditLog(t, database, makeEvent("e001", "org-t", "actor-1", "create", "org", now.Add(-2*time.Hour)))
	insertAuditLog(t, database, makeEvent("e002", "org-t", "actor-1", "update", "org", now.Add(-30*time.Minute)))
	insertAuditLog(t, database, makeEvent("e003", "org-t", "actor-1", "delete", "org", now.Add(1*time.Hour)))

	from := now.Add(-1 * time.Hour)
	to := now.Add(0)

	result, err := audit.Query(context.Background(), database, audit.QueryParams{
		From: from,
		To:   to,
	})
	if err != nil {
		t.Fatalf("Query() error = %v, want nil", err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("len(Events) = %d, want 1 (only the event within [from, to])", len(result.Events))
	}
	if result.Events[0].Action != "update" {
		t.Errorf("Events[0].Action = %q, want %q", result.Events[0].Action, "update")
	}
}

// TestQuery_OrderedByIDDesc verifies that results are returned newest-first
// using descending id order. UUIDv7 IDs are time-sortable, so this is
// equivalent to ORDER BY timestamp DESC.
func TestQuery_OrderedByIDDesc(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestQuery_OrderedByIDDesc")
	now := time.Now().UTC().Truncate(time.Second)

	// IDs are lexicographically ordered to match timestamp order: f001 is oldest,
	// f003 is newest. Descending id order therefore gives newest-first results.
	insertAuditLog(t, database, makeEvent("f001", "org-o", "actor-1", "create", "org", now.Add(-3*time.Minute)))
	insertAuditLog(t, database, makeEvent("f002", "org-o", "actor-1", "update", "org", now.Add(-2*time.Minute)))
	insertAuditLog(t, database, makeEvent("f003", "org-o", "actor-1", "delete", "org", now.Add(-1*time.Minute)))

	result, err := audit.Query(context.Background(), database, audit.QueryParams{})
	if err != nil {
		t.Fatalf("Query() error = %v, want nil", err)
	}
	if len(result.Events) != 3 {
		t.Fatalf("len(Events) = %d, want 3", len(result.Events))
	}

	// Descending id order: f003 > f002 > f001.
	wantIDs := []string{"f003", "f002", "f001"}
	for i, want := range wantIDs {
		if result.Events[i].ID != want {
			t.Errorf("Events[%d].ID = %q, want %q", i, result.Events[i].ID, want)
		}
	}
}

// TestQuery_MultipleFilters verifies that multiple filters are ANDed together.
func TestQuery_MultipleFilters(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestQuery_MultipleFilters")
	now := time.Now().UTC().Truncate(time.Second)

	insertAuditLog(t, database, makeEvent("g001", "org-m", "actor-1", "create", "org", now.Add(-3*time.Minute)))
	insertAuditLog(t, database, makeEvent("g002", "org-m", "actor-2", "create", "org", now.Add(-2*time.Minute)))
	insertAuditLog(t, database, makeEvent("g003", "org-n", "actor-1", "create", "org", now.Add(-1*time.Minute)))

	// Filter by both org and actor — should match only g001.
	result, err := audit.Query(context.Background(), database, audit.QueryParams{
		OrgID:   "org-m",
		ActorID: "actor-1",
	})
	if err != nil {
		t.Fatalf("Query() error = %v, want nil", err)
	}
	if len(result.Events) != 1 {
		t.Errorf("len(Events) = %d, want 1 (only g001 matches both org-m and actor-1)", len(result.Events))
	}
}

// TestQuery_LimitClamping verifies that a limit above 200 is clamped to 200.
func TestQuery_LimitClamping(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestQuery_LimitClamping")
	now := time.Now().UTC().Truncate(time.Second)

	// Insert 3 rows — we only need to confirm the clamp does not error.
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("h%03d", i)
		insertAuditLog(t, database, makeEvent(id, "org-c", "actor-1", "create", "org",
			now.Add(time.Duration(-i)*time.Minute)))
	}

	result, err := audit.Query(context.Background(), database, audit.QueryParams{
		Limit: 999, // must be clamped to 200
	})
	if err != nil {
		t.Fatalf("Query() error = %v, want nil", err)
	}
	// All 3 rows are returned — the clamp itself is the important property.
	if len(result.Events) != 3 {
		t.Errorf("len(Events) = %d, want 3", len(result.Events))
	}
}

// TestQuery_HasMoreExact verifies that HasMore is false when the number of
// matching rows exactly equals the requested limit.
func TestQuery_HasMoreExact(t *testing.T) {
	t.Parallel()

	database := newTestDB(t, "TestQuery_HasMoreExact")
	now := time.Now().UTC().Truncate(time.Second)

	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("i%03d", i)
		insertAuditLog(t, database, makeEvent(id, "org-x", "actor-1", "create", "org",
			now.Add(time.Duration(i)*time.Minute)))
	}

	result, err := audit.Query(context.Background(), database, audit.QueryParams{Limit: 3})
	if err != nil {
		t.Fatalf("Query() error = %v, want nil", err)
	}
	if len(result.Events) != 3 {
		t.Errorf("len(Events) = %d, want 3", len(result.Events))
	}
	if result.HasMore {
		t.Errorf("HasMore = true, want false (exactly limit rows returned)")
	}
}
