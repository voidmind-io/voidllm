package db

// Tests for QueryAuditLogs' From/To window handling (see audit.go). Both
// bounds are converted through FormatTimestamp before being bound as query
// parameters, so a caller supplying From/To in any time.Location (not just
// UTC) must still get back exactly the rows falling inside the true UTC
// window — plain TEXT comparison against the canonical
// "YYYY-MM-DDTHH:MM:SSZ" column values is only correct if every bound is
// itself first converted to that same UTC shape.

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// insertAuditLogRaw inserts a minimal audit_logs row with timestamp set to
// the literal, already-canonical raw string, bypassing the audit.Logger
// entirely so the test can place rows at exact, known instants.
func insertAuditLogRaw(t *testing.T, d *DB, id, orgID, timestamp string) {
	t.Helper()
	query := fmt.Sprintf(
		`INSERT INTO audit_logs (id, timestamp, org_id, actor_id, actor_type, actor_key_id, action, resource_type, resource_id, description, ip_address, status_code, request_id)
		 VALUES ('%s', '%s', '%s', 'actor-1', 'user', 'key-1', 'create', 'api_key', 'resource-1', 'test entry', '127.0.0.1', 200, 'req-1')`,
		id, timestamp, orgID,
	)
	if _, err := d.sql.ExecContext(context.Background(), query); err != nil {
		t.Fatalf("insertAuditLogRaw id=%q timestamp=%q: %v", id, timestamp, err)
	}
}

// TestQueryAuditLogs_NonUTCOffsetWindow seeds audit_logs rows at known UTC
// instants and queries with From/To expressed in a fixed +02:00 offset
// (never UTC and never the local system zone), confirming the returned
// entries are exactly the ones whose canonical UTC timestamp falls inside
// the requested window — including both inclusive edges — and no others.
func TestQueryAuditLogs_NonUTCOffsetWindow(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		org := mustCreateOrg(t, d, CreateOrgParams{Name: "TZ Audit Org", Slug: "tz-audit-" + newSortableID(t)})

		// Window (UTC): [2026-09-26T22:00:00Z, 2026-09-27T00:00:00Z] inclusive.
		beforeID := newSortableID(t)
		lowerEdgeID := newSortableID(t)
		insideID := newSortableID(t)
		upperEdgeID := newSortableID(t)
		afterID := newSortableID(t)

		insertAuditLogRaw(t, d, beforeID, org.ID, "2026-09-26T21:00:00Z")    // before window
		insertAuditLogRaw(t, d, lowerEdgeID, org.ID, "2026-09-26T22:00:00Z") // == From, inclusive
		insertAuditLogRaw(t, d, insideID, org.ID, "2026-09-26T23:30:00Z")    // inside window
		insertAuditLogRaw(t, d, upperEdgeID, org.ID, "2026-09-27T00:00:00Z") // == To, inclusive
		insertAuditLogRaw(t, d, afterID, org.ID, "2026-09-27T01:00:00Z")     // after window

		// A fixed, non-UTC offset location distinct from both UTC and the
		// system's own local zone, so the test cannot pass by accident if
		// QueryAuditLogs forgot to convert through FormatTimestamp.
		cest := time.FixedZone("CEST", 2*60*60)
		from := time.Date(2026, 9, 27, 0, 0, 0, 0, cest) // == 2026-09-26T22:00:00Z
		to := time.Date(2026, 9, 27, 2, 0, 0, 0, cest)   // == 2026-09-27T00:00:00Z

		result, err := d.QueryAuditLogs(ctx, AuditLogFilter{
			OrgID: org.ID,
			From:  from,
			To:    to,
			Limit: 50,
		})
		if err != nil {
			t.Fatalf("QueryAuditLogs() error = %v, want nil", err)
		}

		gotIDs := make(map[string]bool, len(result.Entries))
		for _, e := range result.Entries {
			gotIDs[e.ID] = true
		}

		wantIn := []string{lowerEdgeID, insideID, upperEdgeID}
		for _, id := range wantIn {
			if !gotIDs[id] {
				t.Errorf("expected id %s inside the UTC window to be returned, was not; got %d entries", id, len(result.Entries))
			}
		}
		wantOut := []string{beforeID, afterID}
		for _, id := range wantOut {
			if gotIDs[id] {
				t.Errorf("expected id %s outside the UTC window to be excluded, was returned", id)
			}
		}
		if len(result.Entries) != len(wantIn) {
			t.Errorf("QueryAuditLogs() returned %d entries, want exactly %d (%v)", len(result.Entries), len(wantIn), wantIn)
		}
	})
}
