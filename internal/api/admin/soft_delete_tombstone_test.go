package admin_test

// Handler-level regression tests for #172's soft-delete tombstone fix:
//   - include_deleted listings must display the original (untombstoned)
//     email/slug to admins, even though the underlying DB row is mangled.
//   - Create/Update endpoints for models, deployments, users, and invites
//     must reject any name/email containing the ":deleted:" tombstone
//     marker, so a live row can never be created that collides with, or is
//     mistaken for, a mangled tombstoned row.
//   - Ordinary names/emails, including ones that merely contain a colon,
//     remain unaffected.

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// ---- include_deleted listings strip the tombstone for display ---------------

func TestListUsers_IncludeDeleted_StripsTombstoneFromEmail(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListUsers_IncludeDeleted_Strip?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	u := mustCreateUser(t, database, "strip-me@example.com", "Strip Me")
	if err := database.DeleteUser(context.Background(), u.ID); err != nil {
		t.Fatalf("DeleteUser(): %v", err)
	}

	// The raw DB row must be mangled with a tombstone suffix.
	dbUsers, err := database.ListUsers(context.Background(), "", 100, true)
	if err != nil {
		t.Fatalf("ListUsers(includeDeleted=true) DB call: %v", err)
	}
	var rawEmail string
	for _, du := range dbUsers {
		if du.ID == u.ID {
			rawEmail = du.Email
		}
	}
	wantRaw := "strip-me@example.com:deleted:" + u.ID
	if rawEmail != wantRaw {
		t.Fatalf("raw DB email = %q, want %q (sanity check that the row is actually mangled)", rawEmail, wantRaw)
	}

	req := httptest.NewRequest("GET", "/api/v1/users?include_deleted=true", nil)
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

	var got map[string]any
	decodeBody(t, resp.Body, &got)
	data, ok := got["data"].([]any)
	if !ok {
		t.Fatalf("data is not an array: %v", got["data"])
	}

	found := findByID(t, data, u.ID)
	if found["email"] != "strip-me@example.com" {
		t.Errorf("response email = %v, want %q (tombstone suffix must be stripped for display)", found["email"], "strip-me@example.com")
	}
}

func TestListOrgs_IncludeDeleted_StripsTombstoneFromSlug(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListOrgs_IncludeDeleted_Strip?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	org := mustCreateOrg(t, database, "Strip Me Org", "strip-me-org")
	if err := database.DeleteOrg(context.Background(), org.ID); err != nil {
		t.Fatalf("DeleteOrg(): %v", err)
	}

	dbOrgs, err := database.ListOrgs(context.Background(), "", 100, true)
	if err != nil {
		t.Fatalf("ListOrgs(includeDeleted=true) DB call: %v", err)
	}
	var rawSlug string
	for _, do := range dbOrgs {
		if do.ID == org.ID {
			rawSlug = do.Slug
		}
	}
	wantRaw := "strip-me-org:deleted:" + org.ID
	if rawSlug != wantRaw {
		t.Fatalf("raw DB slug = %q, want %q (sanity check that the row is actually mangled)", rawSlug, wantRaw)
	}

	req := httptest.NewRequest("GET", "/api/v1/orgs?include_deleted=true", nil)
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

	var got map[string]any
	decodeBody(t, resp.Body, &got)
	data, ok := got["data"].([]any)
	if !ok {
		t.Fatalf("data is not an array: %v", got["data"])
	}

	found := findByID(t, data, org.ID)
	if found["slug"] != "strip-me-org" {
		t.Errorf("response slug = %v, want %q (tombstone suffix must be stripped for display)", found["slug"], "strip-me-org")
	}
}

func TestListTeams_IncludeDeleted_StripsTombstoneFromSlug(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListTeams_IncludeDeleted_Strip?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Strip Team Org", "strip-team-org")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, org.ID)

	team := mustCreateTeam(t, database, org.ID, "Strip Me Team", "strip-me-team")
	if err := database.DeleteTeam(context.Background(), team.ID); err != nil {
		t.Fatalf("DeleteTeam(): %v", err)
	}

	dbTeams, err := database.ListTeams(context.Background(), org.ID, "", 100, true)
	if err != nil {
		t.Fatalf("ListTeams(includeDeleted=true) DB call: %v", err)
	}
	var rawSlug string
	for _, dt := range dbTeams {
		if dt.ID == team.ID {
			rawSlug = dt.Slug
		}
	}
	wantRaw := "strip-me-team:deleted:" + team.ID
	if rawSlug != wantRaw {
		t.Fatalf("raw DB slug = %q, want %q (sanity check that the row is actually mangled)", rawSlug, wantRaw)
	}

	req := httptest.NewRequest("GET", teamURL(org.ID)+"?include_deleted=true", nil)
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

	var got map[string]any
	decodeBody(t, resp.Body, &got)
	data, ok := got["data"].([]any)
	if !ok {
		t.Fatalf("data is not an array: %v", got["data"])
	}

	found := findByID(t, data, team.ID)
	if found["slug"] != "strip-me-team" {
		t.Errorf("response slug = %v, want %q (tombstone suffix must be stripped for display)", found["slug"], "strip-me-team")
	}
}

// findByID locates the map entry in data whose "id" field equals id, failing
// the test if no such entry exists.
func findByID(t *testing.T, data []any, id string) map[string]any {
	t.Helper()
	for _, item := range data {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["id"] == id {
			return m
		}
	}
	t.Fatalf("no entry with id %q found in %d results", id, len(data))
	return nil
}

// ---- Create/Update reject names/emails containing the tombstone marker ------

func TestCreateModel_RejectsTombstoneMarkerInName(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupModelTestApp(t, "file:TestCreateModel_RejectsTombstone?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", modelURL(), bodyJSON(t, map[string]any{
		"name":     "gpt-4:deleted:evil",
		"provider": "openai",
		"base_url": "https://api.openai.com/v1",
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

func TestUpdateModel_RejectsTombstoneMarkerInName(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupModelTestApp(t, "file:TestUpdateModel_RejectsTombstone?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m, err := database.CreateModel(context.Background(), db.CreateModelParams{
		Name: "update-tombstone-model", Provider: "openai", BaseURL: "https://api.openai.com/v1", Source: "api",
	})
	if err != nil {
		t.Fatalf("CreateModel setup: %v", err)
	}

	req := httptest.NewRequest("PATCH", modelItemURL(m.ID), bodyJSON(t, map[string]any{
		"name": "renamed:deleted:evil",
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

func TestCreateDeployment_RejectsTombstoneMarkerInName(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithEncKey(t, "file:TestCreateDeployment_RejectsTombstone?mode=memory&cache=private")
	m := mustCreateModelForDeployment(t, database, "deployment-tombstone-model")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", deploymentURL(m.ID), bodyJSON(t, map[string]any{
		"name":     "primary:deleted:evil",
		"provider": "openai",
		"base_url": "https://api.openai.com/v1",
		"weight":   1,
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

func TestUpdateDeployment_RejectsTombstoneMarkerInName(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithEncKey(t, "file:TestUpdateDeployment_RejectsTombstone?mode=memory&cache=private")
	m := mustCreateModelForDeployment(t, database, "deployment-update-tombstone-model")
	dep := mustCreateDeploymentDB(t, database, m.ID, "update-tombstone-dep")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("PATCH", deploymentItemURL(m.ID, dep.ID), bodyJSON(t, map[string]any{
		"name": "renamed:deleted:evil",
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

func TestCreateUser_RejectsTombstoneMarkerInEmail(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateUser_RejectsTombstone?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Tombstone User Org", "tombstone-user-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":        "evil:deleted:123@example.com",
		"display_name": "Evil",
		"password":     "securepassword",
		"org_id":       org.ID,
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

func TestUpdateUser_RejectsTombstoneMarkerInEmail(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateUser_RejectsTombstone?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Tombstone Update Org", "tombstone-update-org")
	u := mustCreateUser(t, database, "update-tombstone@example.com", "Update Tombstone")
	mustCreateMembership(t, database, org.ID, u.ID, "member")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("PATCH", "/api/v1/users/"+u.ID, bodyJSON(t, map[string]any{
		"email": "renamed:deleted:evil@example.com",
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

func TestCreateInvite_RejectsTombstoneMarkerInEmail(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateInvite_RejectsTombstone?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Tombstone Invite Org", "tombstone-invite-org")
	creator := mustCreateUser(t, database, "tombstone-invite-creator@example.com", "Creator")
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	req := httptest.NewRequest("POST", invitesURL(org.ID), bodyJSON(t, map[string]any{
		"email": "evil:deleted:123@example.com",
		"role":  "member",
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

// ---- Happy path: ordinary names/emails are unaffected ------------------------

// TestCreateModel_NameWithColonButNoTombstoneMarker_Succeeds guards against an
// overly broad validation regression that would reject any colon in a name,
// rather than specifically the ":deleted:" marker.
func TestCreateModel_NameWithColonButNoTombstoneMarker_Succeeds(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupModelTestApp(t, "file:TestCreateModel_ColonOK?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", modelURL(), bodyJSON(t, map[string]any{
		"name":     "vertex:gemini-pro",
		"provider": "openai",
		"base_url": "https://api.openai.com/v1",
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 201; body: %s", resp.StatusCode, body)
	}
}

func TestCreateUser_OrdinaryEmail_Succeeds(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateUser_OrdinaryEmailOK?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Ordinary Email Org", "ordinary-email-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":        "perfectly.normal@example.com",
		"display_name": "Perfectly Normal",
		"password":     "securepassword",
		"org_id":       org.ID,
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 201; body: %s", resp.StatusCode, body)
	}
}

func TestCreateInvite_OrdinaryEmail_Succeeds(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateInvite_OrdinaryEmailOK?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Ordinary Invite Org", "ordinary-invite-org")
	creator := mustCreateUser(t, database, "ordinary-invite-creator@example.com", "Creator")
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	req := httptest.NewRequest("POST", invitesURL(org.ID), bodyJSON(t, map[string]any{
		"email": "perfectly.normal@example.com",
		"role":  "member",
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 201; body: %s", resp.StatusCode, body)
	}
}
