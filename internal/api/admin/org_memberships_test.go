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

// mustCreateMembership creates an org membership directly in the DB for test setup.
func mustCreateMembership(t *testing.T, database *db.DB, orgID, userID, role string) *db.OrgMembership {
	t.Helper()
	m, err := database.CreateOrgMembership(context.Background(), db.CreateOrgMembershipParams{
		OrgID:  orgID,
		UserID: userID,
		Role:   role,
	})
	if err != nil {
		t.Fatalf("mustCreateMembership(org=%q, user=%q): %v", orgID, userID, err)
	}
	return m
}

// ---- POST /api/v1/orgs/:org_id/members --------------------------------------

func TestCreateOrgMembership(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		keyOrgID   func(targetOrgID string) string // returns the key's org_id
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
			name:       "system_admin assigns org_admin role",
			role:       auth.RoleSystemAdmin,
			keyOrgID:   func(_ string) string { return "" },
			body:       func(userID string) any { return map[string]any{"user_id": userID, "role": "org_admin"} },
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
			name:       "org_admin tries to assign org_admin role returns 403",
			role:       auth.RoleOrgAdmin,
			keyOrgID:   func(orgID string) string { return orgID },
			body:       func(userID string) any { return map[string]any{"user_id": userID, "role": "org_admin"} },
			wantStatus: fiber.StatusForbidden,
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
			body:       func(userID string) any { return map[string]any{"user_id": userID, "role": "superuser"} },
			wantStatus: fiber.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateOrgMembership_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Mem Test Org", "mem-test-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "mem-test-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com", "Test User")

			testKey := addTestKey(t, keyCache, tc.role, tc.keyOrgID(org.ID))

			req := httptest.NewRequest("POST", "/api/v1/orgs/"+org.ID+"/members",
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

func TestCreateOrgMembership_NoAuth(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestCreateOrgMembership_NoAuth?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "No Auth Org", "no-auth-mem-org")

	req := httptest.NewRequest("POST", "/api/v1/orgs/"+org.ID+"/members",
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

func TestCreateOrgMembership_Duplicate(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateOrgMembership_Dup?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	org := mustCreateOrg(t, database, "Dup Mem Org", "dup-mem-org")
	user := mustCreateUser(t, database, "dup-mem@example.com", "Dup User")
	mustCreateMembership(t, database, org.ID, user.ID, "member")

	req := httptest.NewRequest("POST", "/api/v1/orgs/"+org.ID+"/members",
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

func TestCreateOrgMembership_ResponseFields(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateOrgMembership_Fields?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	org := mustCreateOrg(t, database, "Fields Org", "fields-mem-org")
	user := mustCreateUser(t, database, "fields-mem@example.com", "Fields User")

	req := httptest.NewRequest("POST", "/api/v1/orgs/"+org.ID+"/members",
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

	for _, field := range []string{"id", "org_id", "user_id", "role", "created_at"} {
		if _, ok := got[field]; !ok {
			t.Errorf("response missing field %q; got: %v", field, got)
		}
	}
	if got["org_id"] != org.ID {
		t.Errorf("org_id = %q, want %q", got["org_id"], org.ID)
	}
	if got["user_id"] != user.ID {
		t.Errorf("user_id = %q, want %q", got["user_id"], user.ID)
	}
	if got["role"] != "member" {
		t.Errorf("role = %q, want %q", got["role"], "member")
	}
}

// ---- GET /api/v1/orgs/:org_id/members ----------------------------------------

func TestListOrgMemberships(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListOrgMemberships?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "List Mem Org", "list-mem-org")
	userA := mustCreateUser(t, database, "list-mem-a@example.com", "User A")
	userB := mustCreateUser(t, database, "list-mem-b@example.com", "User B")
	mustCreateMembership(t, database, org.ID, userA.ID, "member")
	mustCreateMembership(t, database, org.ID, userB.ID, "member")

	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("GET", "/api/v1/orgs/"+org.ID+"/members", nil)
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
		if m["org_id"] != org.ID {
			t.Errorf("membership org_id = %q, want %q", m["org_id"], org.ID)
		}
	}
}

// ---- PATCH /api/v1/orgs/:org_id/members/:membership_id ----------------------

func TestUpdateOrgMembership(t *testing.T) {
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
			name:       "system_admin changes role to org_admin",
			role:       auth.RoleSystemAdmin,
			keyOrgID:   func(_ string) string { return "" },
			body:       map[string]any{"role": "org_admin"},
			wantStatus: fiber.StatusOK,
			checkRole:  "org_admin",
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
			name:       "org_admin tries to promote to org_admin returns 403",
			role:       auth.RoleOrgAdmin,
			keyOrgID:   func(orgID string) string { return orgID },
			body:       map[string]any{"role": "org_admin"},
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "invalid role returns 400",
			role:       auth.RoleSystemAdmin,
			keyOrgID:   func(_ string) string { return "" },
			body:       map[string]any{"role": "superuser"},
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

			dsn := fmt.Sprintf("file:TestUpdateOrgMembership_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Upd Mem Org", "upd-mem-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "upd-mem-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com", "U")
			m := mustCreateMembership(t, database, org.ID, user.ID, "member")

			testKey := addTestKey(t, keyCache, tc.role, tc.keyOrgID(org.ID))

			req := httptest.NewRequest("PATCH",
				"/api/v1/orgs/"+org.ID+"/members/"+m.ID,
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

func TestUpdateOrgMembership_NotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateOrgMembership_NotFound?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")
	org := mustCreateOrg(t, database, "Ghost Org", "ghost-upd-mem-org")

	req := httptest.NewRequest("PATCH",
		"/api/v1/orgs/"+org.ID+"/members/00000000-0000-0000-0000-000000000000",
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

// ---- DELETE /api/v1/orgs/:org_id/members/:membership_id ----------------------

func TestDeleteOrgMembership(t *testing.T) {
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

			dsn := fmt.Sprintf("file:TestDeleteOrgMembership_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Del Mem Org", "del-mem-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "del-mem-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com", "D")
			m := mustCreateMembership(t, database, org.ID, user.ID, "member")

			testKey := addTestKey(t, keyCache, tc.role, tc.keyOrgID(org.ID))

			req := httptest.NewRequest("DELETE",
				"/api/v1/orgs/"+org.ID+"/members/"+m.ID, nil)
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

func TestDeleteOrgMembership_NotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteOrgMembership_NotFound?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")
	org := mustCreateOrg(t, database, "Ghost Del Org", "ghost-del-mem-org")

	req := httptest.NewRequest("DELETE",
		"/api/v1/orgs/"+org.ID+"/members/00000000-0000-0000-0000-000000000000", nil)
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

func TestDeleteOrgMembership_NoAuth(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestDeleteOrgMembership_NoAuth?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "No Auth Del Org", "no-auth-del-mem-org")

	req := httptest.NewRequest("DELETE",
		"/api/v1/orgs/"+org.ID+"/members/00000000-0000-0000-0000-000000000000", nil)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}
