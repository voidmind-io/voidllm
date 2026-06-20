package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// insertAuditLogDirect inserts a single audit_logs row into the database,
// bypassing the async logger. It is used to seed deterministic test data for
// ListAuditLogs handler tests.
func insertAuditLogDirect(t *testing.T, database *db.DB, id, orgID, actorID, actorType, action, resourceType string) {
	t.Helper()
	const insertSQL = `INSERT INTO audit_logs
		(id, timestamp, org_id, actor_id, actor_type, actor_key_id,
		 action, resource_type, resource_id, description, ip_address, status_code, request_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	ts := time.Now().UTC().Format(time.RFC3339)
	_, err := database.SQL().ExecContext(context.Background(), insertSQL,
		id, ts, orgID, actorID, actorType, "test-key-id",
		action, resourceType, "res-"+id,
		fmt.Sprintf("%s %s %s", actorID, action, resourceType),
		"127.0.0.1", 200, "",
	)
	if err != nil {
		t.Fatalf("insertAuditLogDirect id=%q: %v", id, err)
	}
}

// seedSAForAudit creates a service account owned by the given user inside the
// given org and returns its ID and name.
func seedSAForAudit(t *testing.T, database *db.DB, orgID, createdBy, name string) (saID, saName string) {
	t.Helper()
	sa, err := database.CreateServiceAccount(context.Background(), db.CreateServiceAccountParams{
		Name:      name,
		OrgID:     orgID,
		TeamID:    nil,
		CreatedBy: createdBy,
	})
	if err != nil {
		t.Fatalf("seedSAForAudit(%q): %v", name, err)
	}
	return sa.ID, sa.Name
}

// auditURL returns the URL for GET /api/v1/audit-logs with an optional org_id
// query parameter.
func auditURL(orgID string) string {
	if orgID == "" {
		return "/api/v1/audit-logs"
	}
	return "/api/v1/audit-logs?org_id=" + orgID
}

// ---- actor_name resolution in GET /api/v1/audit-logs ------------------------

// TestListAuditLogs_ActorName_User verifies that a "user" actor event returns
// actor_name equal to the user's display_name when the user is a member of the
// queried org (org_admin path uses org-scoped resolution).
func TestListAuditLogs_ActorName_User(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAuditLogs_ActorName_User?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Actor Name Org", "actor-name-org-user")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	user := mustCreateUser(t, database, "actor-user@example.com", "Alice Auditperson")
	// org_admin resolution only resolves users who are org members; add the membership.
	mustCreateOrgMembership(t, database, org.ID, user.ID, "member")
	insertAuditLogDirect(t, database, "audit-user-1", org.ID, user.ID, "user", "create", "model")

	req := httptest.NewRequest("GET", auditURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	decodeBody(t, resp.Body, &envelope)

	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one event")
	}

	row := envelope.Data[0]
	if row["actor_type"] != "user" {
		t.Fatalf("actor_type = %q, want %q", row["actor_type"], "user")
	}
	if row["actor_name"] != "Alice Auditperson" {
		t.Errorf("actor_name = %v, want %q (user display_name)", row["actor_name"], "Alice Auditperson")
	}
}

// TestListAuditLogs_ActorName_ServiceAccount verifies that a "service_account"
// actor event returns actor_name equal to the service account's name.
func TestListAuditLogs_ActorName_ServiceAccount(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAuditLogs_ActorName_SA?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "SA Actor Org", "actor-name-org-sa")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	creator := mustCreateUser(t, database, "sa-creator@example.com", "SA Creator")
	saID, saName := seedSAForAudit(t, database, org.ID, creator.ID, "Deploy Bot")
	insertAuditLogDirect(t, database, "audit-sa-1", org.ID, saID, "service_account", "update", "model")

	req := httptest.NewRequest("GET", auditURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	decodeBody(t, resp.Body, &envelope)

	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one event")
	}

	row := envelope.Data[0]
	if row["actor_type"] != "service_account" {
		t.Fatalf("actor_type = %q, want %q", row["actor_type"], "service_account")
	}
	if row["actor_name"] != saName {
		t.Errorf("actor_name = %v, want %q (service account name)", row["actor_name"], saName)
	}
}

// TestListAuditLogs_ActorName_System verifies that a "system" actor event has
// no actor_name key in the JSON response (omitempty — key must be absent).
func TestListAuditLogs_ActorName_System(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAuditLogs_ActorName_System?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "System Actor Org", "actor-name-org-system")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	// system actor has no actor_id in the DB (stored as empty string).
	insertAuditLogDirect(t, database, "audit-sys-1", org.ID, "", "system", "delete", "org")

	req := httptest.NewRequest("GET", auditURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	decodeBody(t, resp.Body, &envelope)

	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one event")
	}

	row := envelope.Data[0]
	if row["actor_type"] != "system" {
		t.Fatalf("actor_type = %q, want %q", row["actor_type"], "system")
	}
	if _, present := row["actor_name"]; present {
		t.Errorf("actor_name must be absent (omitempty) for system actor, but found: %v", row["actor_name"])
	}
}

// TestListAuditLogs_ActorName_UnknownActorID verifies that when an event's
// actor_id does not match any row in the resolved tables, the response is 200
// and actor_name is absent from that event's JSON.
func TestListAuditLogs_ActorName_UnknownActorID(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAuditLogs_ActorName_Unknown?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Unknown Actor Org", "actor-name-org-unknown")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	// Insert event whose actor_id does not correspond to any user row.
	const ghostUserID = "00000000-0000-0000-0000-000000009999"
	insertAuditLogDirect(t, database, "audit-ghost-1", org.ID, ghostUserID, "user", "create", "key")

	req := httptest.NewRequest("GET", auditURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	decodeBody(t, resp.Body, &envelope)

	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one event")
	}

	row := envelope.Data[0]
	if row["actor_id"] != ghostUserID {
		t.Fatalf("actor_id = %v, want %q", row["actor_id"], ghostUserID)
	}
	if _, present := row["actor_name"]; present {
		t.Errorf("actor_name must be absent when actor_id has no matching row, but found: %v", row["actor_name"])
	}
}

// TestListAuditLogs_ActorName_TableDriven exercises all actor_name resolution
// paths in a single table-driven test.
func TestListAuditLogs_ActorName_TableDriven(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		actorType      string
		setup          func(t *testing.T, database *db.DB, orgID string) (actorID string)
		wantActorName  string
		wantNameAbsent bool
	}{
		{
			name:      "user actor returns display_name",
			actorType: "user",
			setup: func(t *testing.T, database *db.DB, orgID string) string {
				t.Helper()
				u := mustCreateUser(t, database, "td-user@example.com", "TD User Name")
				// org_admin resolution requires the user to be a member of the org.
				mustCreateOrgMembership(t, database, orgID, u.ID, "member")
				return u.ID
			},
			wantActorName: "TD User Name",
		},
		{
			name:      "service_account actor returns sa name",
			actorType: "service_account",
			setup: func(t *testing.T, database *db.DB, orgID string) string {
				t.Helper()
				creator := mustCreateUser(t, database, "td-sa-creator@example.com", "SA Creator")
				saID, _ := seedSAForAudit(t, database, orgID, creator.ID, "TD SA Name")
				return saID
			},
			wantActorName: "TD SA Name",
		},
		{
			name:      "system actor has no actor_name",
			actorType: "system",
			setup: func(t *testing.T, _ *db.DB, _ string) string {
				return "" // system actors have no actor_id
			},
			wantNameAbsent: true,
		},
		{
			name:      "user actor with unknown actor_id has no actor_name",
			actorType: "user",
			setup: func(t *testing.T, _ *db.DB, _ string) string {
				return "00000000-0000-0000-0000-dead00000001"
			},
			wantNameAbsent: true,
		},
		{
			name:      "service_account actor with unknown actor_id has no actor_name",
			actorType: "service_account",
			setup: func(t *testing.T, _ *db.DB, _ string) string {
				return "00000000-0000-0000-0000-dead00000002"
			},
			wantNameAbsent: true,
		},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestListAuditLogs_TD_%d?mode=memory&cache=private", i)
			app, database, keyCache := setupTestApp(t, dsn)
			org := mustCreateOrg(t, database, "TD Org", fmt.Sprintf("audit-td-org-%d", i))
			testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

			actorID := tc.setup(t, database, org.ID)
			auditID := fmt.Sprintf("audit-td-%d", i)
			insertAuditLogDirect(t, database, auditID, org.ID, actorID, tc.actorType, "create", "key")

			req := httptest.NewRequest("GET", auditURL(org.ID), nil)
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
			}

			var envelope struct {
				Data []map[string]any `json:"data"`
			}
			decodeBody(t, resp.Body, &envelope)

			if len(envelope.Data) == 0 {
				t.Fatal("data is empty, want at least one event")
			}

			row := envelope.Data[0]

			if tc.wantNameAbsent {
				if _, present := row["actor_name"]; present {
					t.Errorf("actor_name must be absent (omitempty) for %s actor with no match, but found: %v",
						tc.actorType, row["actor_name"])
				}
				return
			}

			actorName, _ := row["actor_name"].(string)
			if actorName != tc.wantActorName {
				t.Errorf("actor_name = %q, want %q", actorName, tc.wantActorName)
			}
		})
	}
}

// TestListAuditLogs_ActorName_ResponseIsValidJSON verifies that the full
// response can be decoded into the typed auditEventResponse-shaped struct and
// that actor_name is populated in the typed fields too.
func TestListAuditLogs_ActorName_ResponseIsValidJSON(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAuditLogs_ActorName_JSON?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "JSON Org", "actor-name-org-json")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	user := mustCreateUser(t, database, "json-user@example.com", "JSON User")
	// org_admin resolution requires the user to be a member of the org.
	mustCreateOrgMembership(t, database, org.ID, user.ID, "member")
	insertAuditLogDirect(t, database, "audit-json-1", org.ID, user.ID, "user", "create", "team")

	req := httptest.NewRequest("GET", auditURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	type auditEventShape struct {
		ID           string `json:"id"`
		Timestamp    string `json:"timestamp"`
		OrgID        string `json:"org_id"`
		ActorID      string `json:"actor_id"`
		ActorType    string `json:"actor_type"`
		ActorName    string `json:"actor_name"`
		Action       string `json:"action"`
		ResourceType string `json:"resource_type"`
		StatusCode   int    `json:"status_code"`
	}

	var envelope struct {
		Data    []auditEventShape `json:"data"`
		HasMore bool              `json:"has_more"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one event")
	}

	ev := envelope.Data[0]
	if ev.OrgID != org.ID {
		t.Errorf("org_id = %q, want %q", ev.OrgID, org.ID)
	}
	if ev.ActorID != user.ID {
		t.Errorf("actor_id = %q, want %q", ev.ActorID, user.ID)
	}
	if ev.ActorType != "user" {
		t.Errorf("actor_type = %q, want %q", ev.ActorType, "user")
	}
	if ev.ActorName != "JSON User" {
		t.Errorf("actor_name = %q, want %q", ev.ActorName, "JSON User")
	}
	if ev.Timestamp == "" {
		t.Error("timestamp is empty")
	}
	if ev.StatusCode != 200 {
		t.Errorf("status_code = %d, want 200", ev.StatusCode)
	}
}

// ---- org-aware actor resolution boundary (security fix) ----------------------

// TestListAuditLogs_OrgAdmin_UserMemberResolves verifies that an org_admin
// querying their own org sees actor_name for a user who IS a member of that org.
func TestListAuditLogs_OrgAdmin_UserMemberResolves(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAuditLogs_OrgAdmin_UserMemberResolves?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Member Org", "org-admin-member-resolves")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	member := mustCreateUser(t, database, "member@example.com", "Member User")
	mustCreateOrgMembership(t, database, org.ID, member.ID, "member")
	insertAuditLogDirect(t, database, "audit-member-1", org.ID, member.ID, "user", "create", "key")

	req := httptest.NewRequest("GET", auditURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	decodeBody(t, resp.Body, &envelope)

	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one event")
	}
	if name := envelope.Data[0]["actor_name"]; name != "Member User" {
		t.Errorf("actor_name = %v, want %q (user is an org member)", name, "Member User")
	}
}

// TestListAuditLogs_OrgAdmin_UserNonMemberAbsent verifies that an org_admin
// querying their own org does NOT see actor_name for a user who is NOT a member
// of that org (cross-org actor — the cross-org leak must stay closed).
func TestListAuditLogs_OrgAdmin_UserNonMemberAbsent(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAuditLogs_OrgAdmin_UserNonMemberAbsent?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Target Org", "org-admin-nonmember-absent")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	// This user exists in the DB but has no membership in org — simulates a
	// system_admin or a user from another org who performed an action logged here.
	outsider := mustCreateUser(t, database, "outsider@example.com", "Outsider User")
	insertAuditLogDirect(t, database, "audit-outsider-1", org.ID, outsider.ID, "user", "delete", "org")

	req := httptest.NewRequest("GET", auditURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	decodeBody(t, resp.Body, &envelope)

	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one event")
	}
	row := envelope.Data[0]
	if row["actor_id"] != outsider.ID {
		t.Fatalf("actor_id = %v, want %q", row["actor_id"], outsider.ID)
	}
	// Cross-org user must NOT resolve — actor_name must be absent.
	if _, present := row["actor_name"]; present {
		t.Errorf("actor_name must be absent for cross-org user actor, but got: %v", row["actor_name"])
	}
}

// TestListAuditLogs_OrgAdmin_SAInOrgResolves verifies that a service account
// whose org_id matches the queried org resolves to its name for an org_admin.
func TestListAuditLogs_OrgAdmin_SAInOrgResolves(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAuditLogs_OrgAdmin_SAInOrgResolves?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "SA Org", "org-admin-sa-in-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	creator := mustCreateUser(t, database, "sa-in-org-creator@example.com", "Creator")
	saID, saName := seedSAForAudit(t, database, org.ID, creator.ID, "In-Org Bot")
	insertAuditLogDirect(t, database, "audit-sa-inorg-1", org.ID, saID, "service_account", "create", "team")

	req := httptest.NewRequest("GET", auditURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	decodeBody(t, resp.Body, &envelope)

	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one event")
	}
	if name := envelope.Data[0]["actor_name"]; name != saName {
		t.Errorf("actor_name = %v, want %q (SA in same org)", name, saName)
	}
}

// TestListAuditLogs_OrgAdmin_SAOutOfOrgAbsent verifies that a service account
// whose org_id differs from the queried org does NOT resolve for an org_admin.
func TestListAuditLogs_OrgAdmin_SAOutOfOrgAbsent(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAuditLogs_OrgAdmin_SAOutOfOrgAbsent?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Target Org", "org-admin-sa-out-of-org")
	otherOrg := mustCreateOrg(t, database, "Other Org", "org-admin-sa-other-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	// SA belongs to otherOrg but has an event logged against org.
	creator := mustCreateUser(t, database, "sa-other-org-creator@example.com", "Creator")
	saID, _ := seedSAForAudit(t, database, otherOrg.ID, creator.ID, "Other-Org Bot")
	insertAuditLogDirect(t, database, "audit-sa-outorg-1", org.ID, saID, "service_account", "delete", "key")

	req := httptest.NewRequest("GET", auditURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	decodeBody(t, resp.Body, &envelope)

	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one event")
	}
	row := envelope.Data[0]
	if row["actor_id"] != saID {
		t.Fatalf("actor_id = %v, want %q", row["actor_id"], saID)
	}
	// SA from a different org must NOT resolve — actor_name must be absent.
	if _, present := row["actor_name"]; present {
		t.Errorf("actor_name must be absent for out-of-org SA, but got: %v", row["actor_name"])
	}
}

// TestListAuditLogs_SystemAdmin_CrossOrgResolvesGlobally verifies that a
// system_admin sees actor_name for a user who is NOT a member of the queried
// org. system_admin uses global (unscoped) resolution by design.
func TestListAuditLogs_SystemAdmin_CrossOrgResolvesGlobally(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAuditLogs_SystemAdmin_CrossOrg?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Some Org", "sysadmin-cross-org-resolve")
	// system_admin key has no org affiliation (empty orgID).
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// User exists but has no membership in org — cross-org actor.
	crossOrgUser := mustCreateUser(t, database, "cross-org@example.com", "Cross Org User")
	insertAuditLogDirect(t, database, "audit-cross-1", org.ID, crossOrgUser.ID, "user", "create", "model")

	req := httptest.NewRequest("GET", auditURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	decodeBody(t, resp.Body, &envelope)

	if len(envelope.Data) == 0 {
		t.Fatal("data is empty, want at least one event")
	}
	// system_admin is omniscient — cross-org actor must still resolve.
	if name := envelope.Data[0]["actor_name"]; name != "Cross Org User" {
		t.Errorf("actor_name = %v, want %q (system_admin resolves globally)", name, "Cross Org User")
	}
}
