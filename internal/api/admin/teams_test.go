package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// mustCreateTeam calls the DB directly to create a team for test setup,
// fataling the test on error.
func mustCreateTeam(t *testing.T, database *db.DB, orgID, name, slug string) *db.Team {
	t.Helper()
	team, err := database.CreateTeam(context.Background(), db.CreateTeamParams{
		OrgID: orgID,
		Name:  name,
		Slug:  slug,
	})
	if err != nil {
		t.Fatalf("mustCreateTeam(%q, %q): %v", name, slug, err)
	}
	return team
}

// teamURL returns a base teams URL for the given org.
func teamURL(orgID string) string {
	return "/api/v1/orgs/" + orgID + "/teams"
}

// teamItemURL returns a URL for a specific team within an org.
func teamItemURL(orgID, teamID string) string {
	return "/api/v1/orgs/" + orgID + "/teams/" + teamID
}

// ---- POST /api/v1/orgs/:org_id/teams ----------------------------------------

func TestCreateTeam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool // key OrgID == target org
		body       any
		wantStatus int
		wantField  string
	}{
		{
			name:       "system_admin creates team",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			body:       map[string]any{"name": "Engineering", "slug": "engineering"},
			wantStatus: fiber.StatusCreated,
			wantField:  "id",
		},
		{
			name:       "org_admin of same org creates team",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"name": "Platform", "slug": "platform"},
			wantStatus: fiber.StatusCreated,
			wantField:  "id",
		},
		{
			name:       "missing name returns 400",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			body:       map[string]any{"slug": "no-name"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "missing slug returns 400",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			body:       map[string]any{"name": "No Slug Team"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "invalid slug returns 400",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			body:       map[string]any{"name": "Bad Slug", "slug": "UPPERCASE_invalid!"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			body:       map[string]any{"name": "Cross Org", "slug": "cross-org"},
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "member returns 403",
			role:       auth.RoleMember,
			sameOrg:    true,
			body:       map[string]any{"name": "Member Team", "slug": "member-team"},
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateTeam_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Test Org", "test-org-ct-"+strings.ReplaceAll(tc.name, " ", "-"))

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			req := httptest.NewRequest("POST", teamURL(org.ID), bodyJSON(t, tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, body)
				return
			}

			if tc.wantField != "" {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				if _, ok := got[tc.wantField]; !ok {
					t.Errorf("response missing field %q; got: %v", tc.wantField, got)
				}
			}
		})
	}
}

func TestCreateTeam_NoAuth(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestCreateTeam_NoAuth?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Auth Org", "auth-org-ct-noauth")

	req := httptest.NewRequest("POST", teamURL(org.ID), bodyJSON(t, map[string]any{"name": "X", "slug": "x-team"}))
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header.

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}

func TestCreateTeam_DuplicateSlug(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateTeam_DupSlug?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	org := mustCreateOrg(t, database, "Dup Slug Org", "dup-slug-org-ct")
	mustCreateTeam(t, database, org.ID, "Existing Team", "dup-slug-team")

	req := httptest.NewRequest("POST", teamURL(org.ID),
		bodyJSON(t, map[string]any{"name": "Another Team", "slug": "dup-slug-team"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, fiber.StatusConflict, body)
	}
}

func TestCreateTeam_ResponseFields(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateTeam_Fields?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")
	org := mustCreateOrg(t, database, "Fields Org", "fields-org-ct")

	req := httptest.NewRequest("POST", teamURL(org.ID),
		bodyJSON(t, map[string]any{
			"name":               "Fields Team",
			"slug":               "fields-team",
			"daily_token_limit":  int64(1000),
			"monthly_token_limit": int64(30000),
			"requests_per_minute": 60,
			"requests_per_day":   1440,
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
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, body)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	for _, field := range []string{"id", "org_id", "name", "slug", "created_at", "updated_at"} {
		if _, ok := got[field]; !ok {
			t.Errorf("response missing field %q; got: %v", field, got)
		}
	}
	if got["org_id"] != org.ID {
		t.Errorf("org_id = %q, want %q", got["org_id"], org.ID)
	}
	if got["name"] != "Fields Team" {
		t.Errorf("name = %q, want %q", got["name"], "Fields Team")
	}
	if got["slug"] != "fields-team" {
		t.Errorf("slug = %q, want %q", got["slug"], "fields-team")
	}
}

// ---- GET /api/v1/orgs/:org_id/teams -----------------------------------------

func TestListTeams(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListTeams?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	org := mustCreateOrg(t, database, "List Org", "list-org-teams")
	mustCreateTeam(t, database, org.ID, "Team A", "team-a-lt")
	mustCreateTeam(t, database, org.ID, "Team B", "team-b-lt")

	req := httptest.NewRequest("GET", teamURL(org.ID), nil)
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
		t.Fatalf("data field is not an array: %v", got["data"])
	}
	if len(data) != 2 {
		t.Errorf("len(data) = %d, want 2", len(data))
	}
}

// ---- GET /api/v1/orgs/:org_id/teams/:team_id ---------------------------------

func TestGetTeam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setupTeam  func(t *testing.T, database *db.DB, orgID string) string // returns teamID to request
		requestOrg func(orgID string) string                                // returns org_id for URL
		wantStatus int
	}{
		{
			name: "system_admin gets team",
			setupTeam: func(t *testing.T, database *db.DB, orgID string) string {
				t.Helper()
				return mustCreateTeam(t, database, orgID, "Get Me", "get-me-gt").ID
			},
			requestOrg: func(orgID string) string { return orgID },
			wantStatus: fiber.StatusOK,
		},
		{
			name: "team from wrong org returns 404",
			setupTeam: func(t *testing.T, database *db.DB, orgID string) string {
				t.Helper()
				// Create a second org and a team in it; request that team under orgID.
				other := mustCreateOrg(t, database, "Other Org GT", "other-org-gt")
				return mustCreateTeam(t, database, other.ID, "Wrong Org Team", "wrong-org-team").ID
			},
			requestOrg: func(orgID string) string { return orgID }, // org_id in URL is the first org
			wantStatus: fiber.StatusNotFound,
		},
		{
			name: "non-existent team returns 404",
			setupTeam: func(t *testing.T, database *db.DB, orgID string) string {
				return "00000000-0000-0000-0000-000000000099"
			},
			requestOrg: func(orgID string) string { return orgID },
			wantStatus: fiber.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestGetTeam_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)
			testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

			org := mustCreateOrg(t, database, "Base Org", "base-org-gt-"+strings.ReplaceAll(tc.name, " ", "-"))
			teamID := tc.setupTeam(t, database, org.ID)
			requestOrgID := tc.requestOrg(org.ID)

			req := httptest.NewRequest("GET", teamItemURL(requestOrgID, teamID), nil)
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, body)
				return
			}

			if tc.wantStatus == fiber.StatusOK {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				if got["id"] != teamID {
					t.Errorf("id = %q, want %q", got["id"], teamID)
				}
			}
		})
	}
}

// ---- PATCH /api/v1/orgs/:org_id/teams/:team_id ------------------------------

func TestUpdateTeam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		body       any
		wantStatus int
		checkName  string
	}{
		{
			name:       "system_admin updates team name",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			body:       map[string]any{"name": "Updated Team"},
			wantStatus: fiber.StatusOK,
			checkName:  "Updated Team",
		},
		{
			name:       "org_admin updates own org team",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"name": "OrgAdmin Updated Team"},
			wantStatus: fiber.StatusOK,
			checkName:  "OrgAdmin Updated Team",
		},
		{
			name:       "member returns 403",
			role:       auth.RoleMember,
			sameOrg:    true,
			body:       map[string]any{"name": "Should Fail"},
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			body:       map[string]any{"name": "Cross Org Update"},
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestUpdateTeam_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Update Org", "update-org-team-"+strings.ReplaceAll(tc.name, " ", "-"))
			team := mustCreateTeam(t, database, org.ID, "Original Team", "original-team-upd-"+strings.ReplaceAll(tc.name, " ", "-"))

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			req := httptest.NewRequest("PATCH", teamItemURL(org.ID, team.ID), bodyJSON(t, tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, body)
				return
			}

			if tc.checkName != "" {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				if got["name"] != tc.checkName {
					t.Errorf("name = %q, want %q", got["name"], tc.checkName)
				}
			}
		})
	}
}

func TestUpdateTeam_NotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateTeam_NotFound?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")
	org := mustCreateOrg(t, database, "NF Org", "nf-org-upd-team")

	req := httptest.NewRequest("PATCH", teamItemURL(org.ID, "00000000-0000-0000-0000-000000000099"),
		bodyJSON(t, map[string]any{"name": "Ghost Team"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, fiber.StatusNotFound, body)
	}
}

func TestUpdateTeam_SlugConflict(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateTeam_SlugConflict?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	org := mustCreateOrg(t, database, "Conflict Org", "conflict-org-upd")
	mustCreateTeam(t, database, org.ID, "Taken Team", "taken-slug-upd")
	target := mustCreateTeam(t, database, org.ID, "Target Team", "target-slug-upd")

	req := httptest.NewRequest("PATCH", teamItemURL(org.ID, target.ID),
		bodyJSON(t, map[string]any{"slug": "taken-slug-upd"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, fiber.StatusConflict, body)
	}
}

// ---- DELETE /api/v1/orgs/:org_id/teams/:team_id -----------------------------

func TestDeleteTeam(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		wantStatus int
	}{
		{
			name:       "system_admin deletes team",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			wantStatus: fiber.StatusNoContent,
		},
		{
			name:       "org_admin deletes own org team",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			wantStatus: fiber.StatusNoContent,
		},
		{
			name:       "member returns 403",
			role:       auth.RoleMember,
			sameOrg:    true,
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestDeleteTeam_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Del Org", "del-org-team-"+strings.ReplaceAll(tc.name, " ", "-"))
			team := mustCreateTeam(t, database, org.ID, "To Delete", "to-delete-del-"+strings.ReplaceAll(tc.name, " ", "-"))

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			req := httptest.NewRequest("DELETE", teamItemURL(org.ID, team.ID), nil)
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, body)
			}
		})
	}
}

func TestDeleteTeam_NotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteTeam_NotFound?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")
	org := mustCreateOrg(t, database, "NF Del Org", "nf-del-org-team")

	req := httptest.NewRequest("DELETE", teamItemURL(org.ID, "00000000-0000-0000-0000-000000000099"), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, fiber.StatusNotFound, body)
	}
}

func TestDeleteTeam_ThenGetReturns404(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteTeam_ThenGet?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	org := mustCreateOrg(t, database, "Ephemeral Org", "ephemeral-org-team")
	team := mustCreateTeam(t, database, org.ID, "Ephemeral Team", "ephemeral-team")

	// Delete the team.
	delReq := httptest.NewRequest("DELETE", teamItemURL(org.ID, team.ID), nil)
	delReq.Header.Set("Authorization", "Bearer "+testKey)

	delResp, err := app.Test(delReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test DELETE: %v", err)
	}
	defer delResp.Body.Close()

	if delResp.StatusCode != fiber.StatusNoContent {
		body, _ := io.ReadAll(delResp.Body)
		t.Fatalf("DELETE status = %d, want 204; body: %s", delResp.StatusCode, body)
	}

	// GET should now return 404.
	getReq := httptest.NewRequest("GET", teamItemURL(org.ID, team.ID), nil)
	getReq.Header.Set("Authorization", "Bearer "+testKey)

	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test GET after DELETE: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != fiber.StatusNotFound {
		body, _ := io.ReadAll(getResp.Body)
		t.Errorf("GET after DELETE status = %d, want 404; body: %s", getResp.StatusCode, body)
	}
}

// TestGetTeam_CrossOrgProtection verifies that a team belonging to org B cannot
// be fetched via an org A URL — the response must be 404, not 403.
func TestGetTeam_CrossOrgProtection(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestGetTeam_CrossOrg?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	orgA := mustCreateOrg(t, database, "Org A Cross", "org-a-cross")
	orgB := mustCreateOrg(t, database, "Org B Cross", "org-b-cross")
	teamB := mustCreateTeam(t, database, orgB.ID, "Team B", "team-b-cross")

	// Request teamB via orgA's URL — must be 404.
	req := httptest.NewRequest("GET", teamItemURL(orgA.ID, teamB.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("cross-org GET status = %d, want 404 (not 403); body: %s", resp.StatusCode, body)
	}
}
