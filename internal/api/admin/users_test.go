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

// mustCreateUser inserts a user directly via the DB for test setup.
func mustCreateUser(t *testing.T, database *db.DB, email, displayName string) *db.User {
	t.Helper()
	// A minimal bcrypt hash for a known password — we use cost 4 (MinCost) to keep tests fast.
	// The HTTP handler tests use plaintext passwords in the request body; the handler hashes them.
	// This helper is only for DB-layer setup where we need a pre-hashed value.
	hash := "$2a$04$testu1testu2testu3testu4testhashXXXXXXXXXXXXXXXXXXXX"
	user, err := database.CreateUser(context.Background(), db.CreateUserParams{
		Email:        email,
		DisplayName:  displayName,
		PasswordHash: &hash,
		AuthProvider: "local",
	})
	if err != nil {
		t.Fatalf("mustCreateUser(%q): %v", email, err)
	}
	return user
}

// ---- POST /api/v1/users ------------------------------------------------------

func TestCreateUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		body       any
		wantStatus int
	}{
		{
			name: "org_admin creates user returns 201",
			role: auth.RoleOrgAdmin,
			body: map[string]any{
				"email":        "newuser@example.com",
				"display_name": "New User",
				"password":     "securepassword",
			},
			wantStatus: fiber.StatusCreated,
		},
		{
			name: "missing email returns 400",
			role: auth.RoleOrgAdmin,
			body: map[string]any{
				"display_name": "No Email",
				"password":     "securepassword",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "missing display_name returns 400",
			role: auth.RoleOrgAdmin,
			body: map[string]any{
				"email":    "nodisplay@example.com",
				"password": "securepassword",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "short password returns 400",
			role: auth.RoleOrgAdmin,
			body: map[string]any{
				"email":        "shortpw@example.com",
				"display_name": "Short PW",
				"password":     "short",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "member role returns 403",
			role: auth.RoleMember,
			body: map[string]any{
				"email":        "member@example.com",
				"display_name": "Member",
				"password":     "securepassword",
			},
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateUser_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, _, keyCache := setupTestApp(t, dsn)
			testKey := addTestKey(t, keyCache, tc.role, "")

			req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, tc.body))
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
			}
		})
	}
}

func TestCreateUser_NoAuth(t *testing.T) {
	t.Parallel()

	app, _, _ := setupTestApp(t, "file:TestCreateUser_NoAuth?mode=memory&cache=private")

	req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":        "noauth@example.com",
		"display_name": "No Auth",
		"password":     "securepassword",
	}))
	req.Header.Set("Content-Type", "application/json")
	// Deliberately no Authorization header.

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}

func TestCreateUser_DuplicateEmail(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateUser_DupEmail?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")
	mustCreateUser(t, database, "existing@example.com", "Existing User")

	req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":        "existing@example.com",
		"display_name": "Duplicate",
		"password":     "securepassword",
	}))
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

func TestCreateUser_OrgAdminCannotSetSystemAdmin(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestCreateUser_OrgAdminSysAdmin?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

	req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":           "elevate@example.com",
		"display_name":    "Elevated",
		"password":        "securepassword",
		"is_system_admin": true,
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, fiber.StatusForbidden, body)
	}
}

func TestCreateUser_SystemAdminSetsSystemAdmin(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestCreateUser_SysAdminSetsSysAdmin?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":           "sysadmin2@example.com",
		"display_name":    "System Admin Two",
		"password":        "securepassword",
		"is_system_admin": true,
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
	if got["is_system_admin"] != true {
		t.Errorf("is_system_admin = %v, want true", got["is_system_admin"])
	}
}

func TestCreateUser_ResponseHasNoPassword(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestCreateUser_NoPwInResp?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

	req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":        "nopw@example.com",
		"display_name": "No PW",
		"password":     "securepassword",
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

	for _, forbidden := range []string{"password", "password_hash"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("response contains %q field, must never be present", forbidden)
		}
	}
	// Required fields must be present.
	for _, required := range []string{"id", "email", "display_name", "auth_provider", "created_at", "updated_at"} {
		if _, ok := got[required]; !ok {
			t.Errorf("response missing required field %q", required)
		}
	}
}

// ---- GET /api/v1/users/:user_id ----------------------------------------------

func TestGetUser(t *testing.T) {
	t.Parallel()

	t.Run("org_admin gets member of own org returns 200", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestGetUser_OrgAdminOwnMember?mode=memory&cache=private")

		org := mustCreateOrg(t, database, "Test Org", "test-get-user-org")
		u := mustCreateUser(t, database, "getuser@example.com", "Get User")
		mustCreateMembership(t, database, org.ID, u.ID, "member")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

		req := httptest.NewRequest("GET", "/api/v1/users/"+u.ID, nil)
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
		if got["id"] != u.ID {
			t.Errorf("id = %q, want %q", got["id"], u.ID)
		}
		for _, forbidden := range []string{"password", "password_hash"} {
			if _, ok := got[forbidden]; ok {
				t.Errorf("response contains %q field, must never be present", forbidden)
			}
		}
	})

	t.Run("org_admin cross-org user returns 404", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestGetUser_OrgAdminCrossOrg?mode=memory&cache=private")

		orgA := mustCreateOrg(t, database, "Org A", "test-get-user-orga")
		orgB := mustCreateOrg(t, database, "Org B", "test-get-user-orgb")
		u := mustCreateUser(t, database, "getuser-b@example.com", "User B")
		mustCreateMembership(t, database, orgB.ID, u.ID, "member")
		// key is scoped to orgA — user is only in orgB
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, orgA.ID)

		req := httptest.NewRequest("GET", "/api/v1/users/"+u.ID, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)

		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 404; body: %s", resp.StatusCode, body)
		}
	})

	t.Run("non-existent user returns 404", func(t *testing.T) {
		t.Parallel()

		app, _, keyCache := setupTestApp(t, "file:TestGetUser_NotFound?mode=memory&cache=private")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

		req := httptest.NewRequest("GET", "/api/v1/users/00000000-0000-0000-0000-000000000099", nil)
		req.Header.Set("Authorization", "Bearer "+testKey)

		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 404; body: %s", resp.StatusCode, body)
		}
	})
}

// ---- GET /api/v1/users -------------------------------------------------------

func TestListUsers(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListUsers?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	mustCreateUser(t, database, "user1@example.com", "User One")
	mustCreateUser(t, database, "user2@example.com", "User Two")
	mustCreateUser(t, database, "user3@example.com", "User Three")

	// First page: limit=2.
	req := httptest.NewRequest("GET", "/api/v1/users?limit=2", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test first page: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var page1 map[string]any
	decodeBody(t, resp.Body, &page1)

	data1, ok := page1["data"].([]any)
	if !ok {
		t.Fatalf("page1 data is not array: %v", page1["data"])
	}
	if len(data1) != 2 {
		t.Errorf("page1 len(data) = %d, want 2", len(data1))
	}
	hasMore, _ := page1["has_more"].(bool)
	if !hasMore {
		t.Error("page1 has_more = false, want true")
	}
	cursor, _ := page1["next_cursor"].(string)
	if cursor == "" {
		t.Error("page1 next_cursor is empty, want a cursor value")
	}

	// Second page using cursor.
	req2 := httptest.NewRequest("GET", "/api/v1/users?limit=2&cursor="+cursor, nil)
	req2.Header.Set("Authorization", "Bearer "+testKey)

	resp2, err := app.Test(req2, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test second page: %v", err)
	}
	defer resp2.Body.Close()

	var page2 map[string]any
	decodeBody(t, resp2.Body, &page2)

	data2, ok := page2["data"].([]any)
	if !ok {
		t.Fatalf("page2 data is not array: %v", page2["data"])
	}
	if len(data2) != 1 {
		t.Errorf("page2 len(data) = %d, want 1", len(data2))
	}
	hasMore2, _ := page2["has_more"].(bool)
	if hasMore2 {
		t.Errorf("page2 has_more = true, want false")
	}
}

func TestListUsers_MemberForbidden(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestListUsers_Member?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleMember, "")

	req := httptest.NewRequest("GET", "/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusForbidden)
	}
}

func TestListUsers_OrgAdminForbidden(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestListUsers_OrgAdmin?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

	req := httptest.NewRequest("GET", "/api/v1/users", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusForbidden {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusForbidden)
	}
}

// ---- PATCH /api/v1/users/:user_id --------------------------------------------

func TestUpdateUser(t *testing.T) {
	t.Parallel()

	t.Run("org_admin updates display_name of own org member", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestUpdateUser_OrgAdminDisplayName?mode=memory&cache=private")

		org := mustCreateOrg(t, database, "Test Org", "update-user-org")
		u := mustCreateUser(t, database, "patch-target-oa@example.com", "Patch Target")
		mustCreateMembership(t, database, org.ID, u.ID, "member")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

		req := httptest.NewRequest("PATCH", "/api/v1/users/"+u.ID,
			bodyJSON(t, map[string]any{"display_name": "Updated Name"}))
		req.Header.Set("Content-Type", "application/json")
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
		if got["display_name"] != "Updated Name" {
			t.Errorf("display_name = %v, want %q", got["display_name"], "Updated Name")
		}
	})

	t.Run("system_admin updates email", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestUpdateUser_SystemAdminEmail?mode=memory&cache=private")
		testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

		u := mustCreateUser(t, database, "patch-target-sa@example.com", "Patch Target SA")

		req := httptest.NewRequest("PATCH", "/api/v1/users/"+u.ID,
			bodyJSON(t, map[string]any{"email": "updated@example.com"}))
		req.Header.Set("Content-Type", "application/json")
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
		if got["email"] != "updated@example.com" {
			t.Errorf("email = %v, want %q", got["email"], "updated@example.com")
		}
	})

	t.Run("org_admin cross-org user returns 404", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestUpdateUser_OrgAdminCrossOrg?mode=memory&cache=private")

		orgA := mustCreateOrg(t, database, "Org A", "update-user-orga")
		orgB := mustCreateOrg(t, database, "Org B", "update-user-orgb")
		u := mustCreateUser(t, database, "cross-org-update@example.com", "Cross Org")
		mustCreateMembership(t, database, orgB.ID, u.ID, "member")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, orgA.ID)

		req := httptest.NewRequest("PATCH", "/api/v1/users/"+u.ID,
			bodyJSON(t, map[string]any{"display_name": "Should Fail"}))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testKey)

		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != fiber.StatusNotFound {
			body, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 404; body: %s", resp.StatusCode, body)
		}
	})
}

func TestUpdateUser_Password_NotInResponse(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateUser_PwNotInResp?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "PW Test Org", "pw-update-org")
	u := mustCreateUser(t, database, "pwupdate@example.com", "PW Update")
	mustCreateMembership(t, database, org.ID, u.ID, "member")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("PATCH", "/api/v1/users/"+u.ID, bodyJSON(t, map[string]any{
		"password": "newpassword123",
	}))
	req.Header.Set("Content-Type", "application/json")
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

	for _, forbidden := range []string{"password", "password_hash"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("response contains %q field, must never be present", forbidden)
		}
	}
}

func TestUpdateUser_OrgAdminCannotSetSystemAdmin(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateUser_OrgAdminSysAdmin?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Elevate Test Org", "elevate-test-org")
	u := mustCreateUser(t, database, "elevate-patch@example.com", "Elevate Patch")
	mustCreateMembership(t, database, org.ID, u.ID, "member")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("PATCH", "/api/v1/users/"+u.ID, bodyJSON(t, map[string]any{
		"is_system_admin": true,
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, fiber.StatusForbidden, body)
	}
}

func TestUpdateUser_NotFound(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestUpdateUser_NotFound?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

	req := httptest.NewRequest("PATCH", "/api/v1/users/00000000-0000-0000-0000-000000000099",
		bodyJSON(t, map[string]any{"display_name": "Ghost"}))
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

// ---- DELETE /api/v1/users/:user_id -------------------------------------------

func TestDeleteUser(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		wantStatus int
	}{
		{
			name:       "system_admin deletes user",
			role:       auth.RoleSystemAdmin,
			wantStatus: fiber.StatusNoContent,
		},
		{
			name:       "member returns 403",
			role:       auth.RoleMember,
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "org_admin returns 403",
			role:       auth.RoleOrgAdmin,
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestDeleteUser_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)
			testKey := addTestKey(t, keyCache, tc.role, "")

			u := mustCreateUser(t, database,
				"del-target-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com",
				"Del Target")

			req := httptest.NewRequest("DELETE", "/api/v1/users/"+u.ID, nil)
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

func TestDeleteUser_NotFound(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestDeleteUser_NotFound?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("DELETE", "/api/v1/users/00000000-0000-0000-0000-000000000099", nil)
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

func TestDeleteUser_ThenGetReturns404(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteUser_ThenGet?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")
	u := mustCreateUser(t, database, "ephemeral@example.com", "Ephemeral")

	// Delete the user.
	delReq := httptest.NewRequest("DELETE", "/api/v1/users/"+u.ID, nil)
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
	// Require org_admin key for GET /users/:id.
	orgAdminKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")
	getReq := httptest.NewRequest("GET", "/api/v1/users/"+u.ID, nil)
	getReq.Header.Set("Authorization", "Bearer "+orgAdminKey)

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
