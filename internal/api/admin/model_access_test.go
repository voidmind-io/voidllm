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

// orgModelAccessURL returns the org model-access URL.
func orgModelAccessURL(orgID string) string {
	return "/api/v1/orgs/" + orgID + "/model-access"
}

// teamModelAccessURL returns the team model-access URL.
func teamModelAccessURL(orgID, teamID string) string {
	return "/api/v1/orgs/" + orgID + "/teams/" + teamID + "/model-access"
}

// ---- GET /api/v1/orgs/:org_id/model-access ----------------------------------

func TestGetOrgModelAccess_Empty(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestGetOrgModelAccess_Empty?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Access Org", "access-org-get-empty")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("GET", orgModelAccessURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	models, ok := got["models"].([]any)
	if !ok {
		t.Fatalf("models is not an array: %v", got["models"])
	}
	if len(models) != 0 {
		t.Errorf("len(models) = %d, want 0 (empty allowlist = all allowed)", len(models))
	}
}

func TestGetOrgModelAccess_RBAC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		wantStatus int
	}{
		{
			name:       "system_admin can get",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "org_admin of same org can get",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "member of same org returns 403 from route middleware",
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

			dsn := fmt.Sprintf("file:TestGetOrgModelAccess_RBAC_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestAppWithRegistry(t, dsn)

			org := mustCreateOrg(t, database, "RBAC Access Org", "rbac-access-org-"+strings.ReplaceAll(tc.name, " ", "-"))

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			req := httptest.NewRequest("GET", orgModelAccessURL(org.ID), nil)
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, b)
			}
		})
	}
}

// ---- PUT /api/v1/orgs/:org_id/model-access ----------------------------------

func TestSetOrgModelAccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		models     []string
		wantStatus int
		wantModels []string
	}{
		{
			name:       "org_admin sets allowlist",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			models:     []string{testModelName},
			wantStatus: fiber.StatusOK,
			wantModels: []string{testModelName},
		},
		{
			name:       "system_admin sets allowlist",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			models:     []string{testModelName, testConflictModelName},
			wantStatus: fiber.StatusOK,
			wantModels: []string{testModelName, testConflictModelName},
		},
		{
			name:       "empty array clears allowlist",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			models:     []string{},
			wantStatus: fiber.StatusOK,
			wantModels: []string{},
		},
		{
			name:       "unknown model returns 400",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			models:     []string{"nonexistent-model"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "multiple valid models succeeds",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			models:     []string{testModelName, testConflictModelName},
			wantStatus: fiber.StatusOK,
			wantModels: []string{testModelName, testConflictModelName},
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			models:     []string{testModelName},
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestSetOrgModelAccess_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestAppWithRegistry(t, dsn)

			org := mustCreateOrg(t, database, "Set Access Org", "set-access-org-"+strings.ReplaceAll(tc.name, " ", "-"))

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			req := httptest.NewRequest("PUT", orgModelAccessURL(org.ID),
				bodyJSON(t, map[string]any{"models": tc.models}))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, b)
				return
			}

			if tc.wantModels != nil {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				models, ok := got["models"].([]any)
				if !ok {
					t.Fatalf("models is not an array: %v", got["models"])
				}
				if len(models) != len(tc.wantModels) {
					t.Errorf("len(models) = %d, want %d", len(models), len(tc.wantModels))
				}
			}
		})
	}
}

func TestSetAndGetOrgModelAccess_RoundTrip(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestSetGetOrgModelAccess?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Round Trip Org", "round-trip-org-access")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	// Set allowlist.
	setReq := httptest.NewRequest("PUT", orgModelAccessURL(org.ID),
		bodyJSON(t, map[string]any{"models": []string{testModelName}}))
	setReq.Header.Set("Content-Type", "application/json")
	setReq.Header.Set("Authorization", "Bearer "+testKey)
	setResp, err := app.Test(setReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	setResp.Body.Close()
	if setResp.StatusCode != fiber.StatusOK {
		t.Fatalf("PUT status = %d, want 200", setResp.StatusCode)
	}

	// GET back and verify.
	getReq := httptest.NewRequest("GET", orgModelAccessURL(org.ID), nil)
	getReq.Header.Set("Authorization", "Bearer "+testKey)
	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(getResp.Body)
		t.Fatalf("GET status = %d; body: %s", getResp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, getResp.Body, &got)

	models, ok := got["models"].([]any)
	if !ok {
		t.Fatalf("models is not an array: %v", got["models"])
	}
	if len(models) != 1 {
		t.Fatalf("len(models) = %d, want 1", len(models))
	}
	if models[0] != testModelName {
		t.Errorf("models[0] = %q, want %q", models[0], testModelName)
	}
}

func TestSetOrgModelAccess_EmptyAllowlistMeansAll(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestSetOrgEmpty?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Empty Access Org", "empty-access-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	// Set then clear.
	setReq := httptest.NewRequest("PUT", orgModelAccessURL(org.ID),
		bodyJSON(t, map[string]any{"models": []string{testModelName}}))
	setReq.Header.Set("Content-Type", "application/json")
	setReq.Header.Set("Authorization", "Bearer "+testKey)
	setResp, err := app.Test(setReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("first PUT: %v", err)
	}
	setResp.Body.Close()

	clearReq := httptest.NewRequest("PUT", orgModelAccessURL(org.ID),
		bodyJSON(t, map[string]any{"models": []string{}}))
	clearReq.Header.Set("Content-Type", "application/json")
	clearReq.Header.Set("Authorization", "Bearer "+testKey)
	clearResp, err := app.Test(clearReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("clear PUT: %v", err)
	}
	defer clearResp.Body.Close()

	if clearResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(clearResp.Body)
		t.Fatalf("clear PUT status = %d; body: %s", clearResp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, clearResp.Body, &got)
	models, _ := got["models"].([]any)
	if len(models) != 0 {
		t.Errorf("len(models) = %d after clear, want 0", len(models))
	}
}

// ---- GET /api/v1/orgs/:org_id/teams/:team_id/model-access -------------------

func TestGetTeamModelAccess_Empty(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestGetTeamModelAccess_Empty?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Team Access Org", "team-access-org-get")
	team := mustCreateTeam(t, database, org.ID, "Access Team", "access-team-get")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("GET", teamModelAccessURL(org.ID, team.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	models, ok := got["models"].([]any)
	if !ok {
		t.Fatalf("models is not an array: %v", got["models"])
	}
	if len(models) != 0 {
		t.Errorf("len(models) = %d, want 0", len(models))
	}
}

func TestGetTeamModelAccess_WrongOrg(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestGetTeamModelAccess_WrongOrg?mode=memory&cache=private")
	orgA := mustCreateOrg(t, database, "Org A TA", "org-a-ta-gtma")
	orgB := mustCreateOrg(t, database, "Org B TA", "org-b-ta-gtma")
	teamB := mustCreateTeam(t, database, orgB.ID, "Team B TA", "team-b-ta-gtma")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Access teamB via orgA — must 404.
	req := httptest.NewRequest("GET", teamModelAccessURL(orgA.ID, teamB.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("cross-org GET status = %d, want 404; body: %s", resp.StatusCode, b)
	}
}

// ---- PUT /api/v1/orgs/:org_id/teams/:team_id/model-access -------------------

func TestSetTeamModelAccess(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		orgModels  []string // set on org first (empty = no org allowlist)
		teamModels []string
		wantStatus int
	}{
		{
			name:       "org_admin sets team allowlist (no org restriction)",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			orgModels:  nil,
			teamModels: []string{testModelName},
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "team model within org allowlist succeeds",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			orgModels:  []string{testModelName},
			teamModels: []string{testModelName},
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "team model not in org allowlist returns 400",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			orgModels:  []string{testModelName},
			teamModels: []string{testConflictModelName},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "empty array clears team allowlist",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			orgModels:  nil,
			teamModels: []string{},
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "unknown model returns 400",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			orgModels:  nil,
			teamModels: []string{"ghost-model"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			orgModels:  nil,
			teamModels: []string{testModelName},
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestSetTeamModelAccess_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestAppWithRegistry(t, dsn)

			org := mustCreateOrg(t, database, "Set Team Access Org", "set-team-access-"+strings.ReplaceAll(tc.name, " ", "-"))
			team := mustCreateTeam(t, database, org.ID, "Access Team", "access-team-sta-"+strings.ReplaceAll(tc.name, " ", "-"))

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			adminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			// Set org-level access if requested.
			if len(tc.orgModels) > 0 {
				orgReq := httptest.NewRequest("PUT", orgModelAccessURL(org.ID),
					bodyJSON(t, map[string]any{"models": tc.orgModels}))
				orgReq.Header.Set("Content-Type", "application/json")
				orgReq.Header.Set("Authorization", "Bearer "+adminKey)
				orgResp, err := app.Test(orgReq, fiber.TestConfig{Timeout: testTimeout})
				if err != nil {
					t.Fatalf("set org access: %v", err)
				}
				orgResp.Body.Close()
				if orgResp.StatusCode != fiber.StatusOK {
					t.Fatalf("set org access: status = %d, want 200", orgResp.StatusCode)
				}
			}

			// Now set team access.
			req := httptest.NewRequest("PUT", teamModelAccessURL(org.ID, team.ID),
				bodyJSON(t, map[string]any{"models": tc.teamModels}))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, b)
			}
		})
	}
}

func TestSetAndGetTeamModelAccess_RoundTrip(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestSetGetTeamModelAccess?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "RT Team Org", "rt-team-org-access")
	team := mustCreateTeam(t, database, org.ID, "RT Team", "rt-team-access")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	// Set.
	setReq := httptest.NewRequest("PUT", teamModelAccessURL(org.ID, team.ID),
		bodyJSON(t, map[string]any{"models": []string{testModelName}}))
	setReq.Header.Set("Content-Type", "application/json")
	setReq.Header.Set("Authorization", "Bearer "+testKey)
	setResp, err := app.Test(setReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	setResp.Body.Close()
	if setResp.StatusCode != fiber.StatusOK {
		t.Fatalf("PUT status = %d, want 200", setResp.StatusCode)
	}

	// GET back.
	getReq := httptest.NewRequest("GET", teamModelAccessURL(org.ID, team.ID), nil)
	getReq.Header.Set("Authorization", "Bearer "+testKey)
	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(getResp.Body)
		t.Fatalf("GET status = %d; body: %s", getResp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, getResp.Body, &got)

	models, ok := got["models"].([]any)
	if !ok {
		t.Fatalf("models is not an array: %v", got["models"])
	}
	if len(models) != 1 {
		t.Fatalf("len(models) = %d, want 1", len(models))
	}
	if models[0] != testModelName {
		t.Errorf("models[0] = %q, want %q", models[0], testModelName)
	}
}

func TestGetTeamModelAccess_NonMemberReturnsNotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestGetTeamModelAccess_NonMember?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "NM Team Access Org", "nm-team-access-org")
	team := mustCreateTeam(t, database, org.ID, "NM Team", "nm-team-access")

	// A team_admin who is in the org but not the team.
	// (Route requires team_admin; handler does additional membership check for non-org-admins.)
	user := mustCreateUser(t, database, "nonmember@example.com", "Non Member")
	_, err := database.CreateOrgMembership(context.Background(), db.CreateOrgMembershipParams{
		OrgID:  org.ID,
		UserID: user.ID,
		Role:   auth.RoleTeamAdmin,
	})
	if err != nil {
		t.Fatalf("CreateOrgMembership: %v", err)
	}

	testKey := addTestKeyWithUser(t, keyCache, auth.RoleTeamAdmin, org.ID, user.ID)

	req := httptest.NewRequest("GET", teamModelAccessURL(org.ID, team.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("non-member GET status = %d, want 404; body: %s", resp.StatusCode, b)
	}
}

func TestSetTeamModelAccess_MostRestrictiveWins(t *testing.T) {
	t.Parallel()

	// Org allows only testModelName; trying to set testConflictModelName at
	// team level must fail even though it's a known model.
	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestSetTeamModelAccess_MostRestrictive?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Restrictive Org", "restrictive-org-access")
	team := mustCreateTeam(t, database, org.ID, "Restricted Team", "restricted-team-access")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	// Set org allowlist to only testModelName.
	orgReq := httptest.NewRequest("PUT", orgModelAccessURL(org.ID),
		bodyJSON(t, map[string]any{"models": []string{testModelName}}))
	orgReq.Header.Set("Content-Type", "application/json")
	orgReq.Header.Set("Authorization", "Bearer "+testKey)
	orgResp, err := app.Test(orgReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("set org access: %v", err)
	}
	orgResp.Body.Close()
	if orgResp.StatusCode != fiber.StatusOK {
		t.Fatalf("set org access: status = %d, want 200", orgResp.StatusCode)
	}

	// Try to set team access to testConflictModelName — should fail.
	teamReq := httptest.NewRequest("PUT", teamModelAccessURL(org.ID, team.ID),
		bodyJSON(t, map[string]any{"models": []string{testConflictModelName}}))
	teamReq.Header.Set("Content-Type", "application/json")
	teamReq.Header.Set("Authorization", "Bearer "+testKey)
	teamResp, err := app.Test(teamReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("set team access: %v", err)
	}
	defer teamResp.Body.Close()

	if teamResp.StatusCode != fiber.StatusBadRequest {
		b, _ := io.ReadAll(teamResp.Body)
		t.Errorf("status = %d, want 400 (model not allowed by org); body: %s", teamResp.StatusCode, b)
	}
}
