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

// mustCreateTeamMembership creates a team membership directly in the DB for test setup.
func mustCreateTeamMembership(t *testing.T, database *db.DB, teamID, userID, role string) *db.TeamMembership {
	t.Helper()
	m, err := database.CreateTeamMembership(context.Background(), db.CreateTeamMembershipParams{
		TeamID: teamID,
		UserID: userID,
		Role:   role,
	})
	if err != nil {
		t.Fatalf("mustCreateTeamMembership(team=%q, user=%q): %v", teamID, userID, err)
	}
	return m
}

// teamMemberURL returns the base URL for team members within a given org and team.
func teamMemberURL(orgID, teamID string) string {
	return "/api/v1/orgs/" + orgID + "/teams/" + teamID + "/members"
}

// teamMemberItemURL returns the URL for a specific team membership.
func teamMemberItemURL(orgID, teamID, membershipID string) string {
	return "/api/v1/orgs/" + orgID + "/teams/" + teamID + "/members/" + membershipID
}

// ---- POST /api/v1/orgs/:org_id/teams/:team_id/members -----------------------

func TestCreateTeamMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		keyOrgID   func(targetOrgID string) string
		body       func(userID string) any
		wantStatus int
		checkField string // non-empty: assert this field exists in a 201 response
	}{
		{
			name:       "system_admin creates member",
			role:       auth.RoleSystemAdmin,
			keyOrgID:   func(_ string) string { return "" },
			body:       func(userID string) any { return map[string]any{"user_id": userID, "role": "member"} },
			wantStatus: fiber.StatusCreated,
			checkField: "id",
		},
		{
			name:       "system_admin assigns team_admin role",
			role:       auth.RoleSystemAdmin,
			keyOrgID:   func(_ string) string { return "" },
			body:       func(userID string) any { return map[string]any{"user_id": userID, "role": "team_admin"} },
			wantStatus: fiber.StatusCreated,
			checkField: "id",
		},
		{
			name:       "org_admin of same org creates member",
			role:       auth.RoleOrgAdmin,
			keyOrgID:   func(orgID string) string { return orgID },
			body:       func(userID string) any { return map[string]any{"user_id": userID, "role": "member"} },
			wantStatus: fiber.StatusCreated,
			checkField: "id",
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			keyOrgID:   func(_ string) string { return "00000000-0000-0000-0000-000000000001" },
			body:       func(userID string) any { return map[string]any{"user_id": userID, "role": "member"} },
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "member role returns 403",
			role:       auth.RoleMember,
			keyOrgID:   func(orgID string) string { return orgID },
			body:       func(userID string) any { return map[string]any{"user_id": userID, "role": "member"} },
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "missing user_id returns 400",
			role:       auth.RoleSystemAdmin,
			keyOrgID:   func(_ string) string { return "" },
			body:       func(_ string) any { return map[string]any{"role": "member"} },
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "invalid role returns 400",
			role:       auth.RoleSystemAdmin,
			keyOrgID:   func(_ string) string { return "" },
			body:       func(userID string) any { return map[string]any{"user_id": userID, "role": "org_admin"} },
			wantStatus: fiber.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateTeamMembership_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "TM Test Org", "tmem-test-"+strings.ReplaceAll(tc.name, " ", "-"))
			team := mustCreateTeam(t, database, org.ID, "TM Test Team", "tmem-team-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "tmem-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com", "Test User")

			testKey := addTestKey(t, keyCache, tc.role, tc.keyOrgID(org.ID))

			req := httptest.NewRequest("POST", teamMemberURL(org.ID, team.ID),
				bodyJSON(t, tc.body(user.ID)))
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

			if tc.checkField != "" {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				if _, ok := got[tc.checkField]; !ok {
					t.Errorf("response missing field %q; got: %v", tc.checkField, got)
				}
			}
		})
	}
}

func TestCreateTeamMembership_NoAuth(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestCreateTeamMembership_NoAuth?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "No Auth TM Org", "no-auth-tmem-org")
	team := mustCreateTeam(t, database, org.ID, "No Auth Team", "no-auth-tmem-team")

	req := httptest.NewRequest("POST", teamMemberURL(org.ID, team.ID),
		bodyJSON(t, map[string]any{"user_id": "some-id", "role": "member"}))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}

func TestCreateTeamMembership_Duplicate(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateTeamMembership_Dup?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	org := mustCreateOrg(t, database, "Dup TM Org", "dup-tmem-org")
	team := mustCreateTeam(t, database, org.ID, "Dup Team", "dup-tmem-team")
	user := mustCreateUser(t, database, "dup-tmem@example.com", "Dup User")
	mustCreateTeamMembership(t, database, team.ID, user.ID, "member")

	req := httptest.NewRequest("POST", teamMemberURL(org.ID, team.ID),
		bodyJSON(t, map[string]any{"user_id": user.ID, "role": "member"}))
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

func TestCreateTeamMembership_CrossOrg(t *testing.T) {
	t.Parallel()

	// Create team in orgA, but attempt to add a member via orgB's URL.
	app, database, keyCache := setupTestApp(t, "file:TestCreateTeamMembership_CrossOrg?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	orgA := mustCreateOrg(t, database, "Org A", "cross-tmem-org-a")
	orgB := mustCreateOrg(t, database, "Org B", "cross-tmem-org-b")
	teamA := mustCreateTeam(t, database, orgA.ID, "Team A", "cross-tmem-team-a")
	user := mustCreateUser(t, database, "cross-tmem@example.com", "Cross User")

	// Use orgB's URL but teamA's ID (team belongs to orgA, not orgB).
	req := httptest.NewRequest("POST", teamMemberURL(orgB.ID, teamA.ID),
		bodyJSON(t, map[string]any{"user_id": user.ID, "role": "member"}))
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

func TestCreateTeamMembership_ResponseFields(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateTeamMembership_Fields?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	org := mustCreateOrg(t, database, "Fields TM Org", "fields-tmem-org")
	team := mustCreateTeam(t, database, org.ID, "Fields Team", "fields-tmem-team")
	user := mustCreateUser(t, database, "fields-tmem@example.com", "Fields User")

	req := httptest.NewRequest("POST", teamMemberURL(org.ID, team.ID),
		bodyJSON(t, map[string]any{"user_id": user.ID, "role": "member"}))
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

	for _, field := range []string{"id", "team_id", "user_id", "role", "created_at"} {
		if _, ok := got[field]; !ok {
			t.Errorf("response missing field %q; got: %v", field, got)
		}
	}
	if got["team_id"] != team.ID {
		t.Errorf("team_id = %q, want %q", got["team_id"], team.ID)
	}
	if got["user_id"] != user.ID {
		t.Errorf("user_id = %q, want %q", got["user_id"], user.ID)
	}
	if got["role"] != "member" {
		t.Errorf("role = %q, want %q", got["role"], "member")
	}
}

// ---- GET /api/v1/orgs/:org_id/teams/:team_id/members ------------------------

func TestListTeamMemberships(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListTeamMemberships?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "List TM Org", "list-tmem-org")
	team := mustCreateTeam(t, database, org.ID, "List Team", "list-tmem-team")
	userA := mustCreateUser(t, database, "list-tmem-a@example.com", "User A")
	userB := mustCreateUser(t, database, "list-tmem-b@example.com", "User B")
	mustCreateTeamMembership(t, database, team.ID, userA.ID, "member")
	mustCreateTeamMembership(t, database, team.ID, userB.ID, "team_admin")

	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("GET", teamMemberURL(org.ID, team.ID), nil)
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
		t.Fatalf("data is not array: %v", got["data"])
	}
	if len(data) != 2 {
		t.Errorf("len(data) = %d, want 2", len(data))
	}
	for _, entry := range data {
		m := entry.(map[string]any)
		if m["team_id"] != team.ID {
			t.Errorf("membership team_id = %q, want %q", m["team_id"], team.ID)
		}
	}
}

// ---- PATCH /api/v1/orgs/:org_id/teams/:team_id/members/:membership_id -------

func TestUpdateTeamMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		keyOrgID   func(targetOrgID string) string
		body       any
		wantStatus int
		checkRole  string // non-empty: assert role field in response
	}{
		{
			name:       "system_admin changes role to team_admin",
			role:       auth.RoleSystemAdmin,
			keyOrgID:   func(_ string) string { return "" },
			body:       map[string]any{"role": "team_admin"},
			wantStatus: fiber.StatusOK,
			checkRole:  "team_admin",
		},
		{
			name:       "org_admin changes role to member",
			role:       auth.RoleOrgAdmin,
			keyOrgID:   func(orgID string) string { return orgID },
			body:       map[string]any{"role": "member"},
			wantStatus: fiber.StatusOK,
			checkRole:  "member",
		},
		{
			name:       "invalid role returns 400",
			role:       auth.RoleSystemAdmin,
			keyOrgID:   func(_ string) string { return "" },
			body:       map[string]any{"role": "org_admin"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "member role returns 403",
			role:       auth.RoleMember,
			keyOrgID:   func(orgID string) string { return orgID },
			body:       map[string]any{"role": "member"},
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestUpdateTeamMembership_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Upd TM Org", "upd-tmem-"+strings.ReplaceAll(tc.name, " ", "-"))
			team := mustCreateTeam(t, database, org.ID, "Upd Team", "upd-tmem-team-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "upd-tmem-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com", "U")
			m := mustCreateTeamMembership(t, database, team.ID, user.ID, "member")

			testKey := addTestKey(t, keyCache, tc.role, tc.keyOrgID(org.ID))

			req := httptest.NewRequest("PATCH",
				teamMemberItemURL(org.ID, team.ID, m.ID),
				bodyJSON(t, tc.body))
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

			if tc.checkRole != "" {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				if got["role"] != tc.checkRole {
					t.Errorf("role = %q, want %q", got["role"], tc.checkRole)
				}
			}
		})
	}
}

func TestUpdateTeamMembership_NotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateTeamMembership_NotFound?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")
	org := mustCreateOrg(t, database, "Ghost TM Org", "ghost-upd-tmem-org")
	team := mustCreateTeam(t, database, org.ID, "Ghost Team", "ghost-upd-tmem-team")

	req := httptest.NewRequest("PATCH",
		teamMemberItemURL(org.ID, team.ID, "00000000-0000-0000-0000-000000000000"),
		bodyJSON(t, map[string]any{"role": "member"}))
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

// ---- DELETE /api/v1/orgs/:org_id/teams/:team_id/members/:membership_id ------

func TestDeleteTeamMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		keyOrgID   func(targetOrgID string) string
		wantStatus int
	}{
		{
			name:       "system_admin deletes membership",
			role:       auth.RoleSystemAdmin,
			keyOrgID:   func(_ string) string { return "" },
			wantStatus: fiber.StatusNoContent,
		},
		{
			name:       "org_admin of same org deletes membership",
			role:       auth.RoleOrgAdmin,
			keyOrgID:   func(orgID string) string { return orgID },
			wantStatus: fiber.StatusNoContent,
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			keyOrgID:   func(_ string) string { return "00000000-0000-0000-0000-000000000001" },
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "member role returns 403",
			role:       auth.RoleMember,
			keyOrgID:   func(orgID string) string { return orgID },
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestDeleteTeamMembership_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Del TM Org", "del-tmem-"+strings.ReplaceAll(tc.name, " ", "-"))
			team := mustCreateTeam(t, database, org.ID, "Del Team", "del-tmem-team-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "del-tmem-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com", "D")
			m := mustCreateTeamMembership(t, database, team.ID, user.ID, "member")

			testKey := addTestKey(t, keyCache, tc.role, tc.keyOrgID(org.ID))

			req := httptest.NewRequest("DELETE",
				teamMemberItemURL(org.ID, team.ID, m.ID), nil)
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

func TestDeleteTeamMembership_NotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteTeamMembership_NotFound?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")
	org := mustCreateOrg(t, database, "Ghost Del TM Org", "ghost-del-tmem-org")
	team := mustCreateTeam(t, database, org.ID, "Ghost Del Team", "ghost-del-tmem-team")

	req := httptest.NewRequest("DELETE",
		teamMemberItemURL(org.ID, team.ID, "00000000-0000-0000-0000-000000000000"), nil)
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

func TestDeleteTeamMembership_NoAuth(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestDeleteTeamMembership_NoAuth?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "No Auth Del TM Org", "no-auth-del-tmem-org")
	team := mustCreateTeam(t, database, org.ID, "No Auth Del Team", "no-auth-del-tmem-team")

	req := httptest.NewRequest("DELETE",
		teamMemberItemURL(org.ID, team.ID, "00000000-0000-0000-0000-000000000000"), nil)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}
