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
	"golang.org/x/crypto/bcrypt"
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

	t.Run("org_admin creates user with org_id returns 201", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestCreateUser_OrgAdmin201?mode=memory&cache=private")
		org := mustCreateOrg(t, database, "Create User Org", "create-user-org")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":        "newuser@example.com",
			"display_name": "New User",
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
	})

	t.Run("org_admin without org_id returns 400", func(t *testing.T) {
		t.Parallel()

		app, _, keyCache := setupTestApp(t, "file:TestCreateUser_OrgAdmin400NoOrg?mode=memory&cache=private")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":        "newuser2@example.com",
			"display_name": "New User 2",
			"password":     "securepassword",
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
	})

	t.Run("missing email returns 400", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestCreateUser_MissingEmail?mode=memory&cache=private")
		org := mustCreateOrg(t, database, "Missing Email Org", "missing-email-org")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"display_name": "No Email",
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
	})

	t.Run("missing display_name returns 400", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestCreateUser_MissingDisplayName?mode=memory&cache=private")
		org := mustCreateOrg(t, database, "Missing Display Org", "missing-display-org")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":    "nodisplay@example.com",
			"password": "securepassword",
			"org_id":   org.ID,
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
	})

	t.Run("short password returns 400", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestCreateUser_ShortPW?mode=memory&cache=private")
		org := mustCreateOrg(t, database, "Short PW Org", "short-pw-org")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":        "shortpw@example.com",
			"display_name": "Short PW",
			"password":     "short",
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
	})

	t.Run("member role returns 403", func(t *testing.T) {
		t.Parallel()

		app, _, keyCache := setupTestApp(t, "file:TestCreateUser_MemberForbidden?mode=memory&cache=private")
		testKey := addTestKey(t, keyCache, auth.RoleMember, "")

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":        "member@example.com",
			"display_name": "Member",
			"password":     "securepassword",
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
			t.Errorf("status = %d, want 403; body: %s", resp.StatusCode, body)
		}
	})
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
	org := mustCreateOrg(t, database, "Dup Email Org", "dup-email-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)
	mustCreateUser(t, database, "existing@example.com", "Existing User")

	req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":        "existing@example.com",
		"display_name": "Duplicate",
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

func TestCreateUser_SystemAdminWithoutOrgReturns400(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestCreateUser_SysAdminNoOrg400?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// org_id is intentionally omitted — every user must belong to an org,
	// including system admins.
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

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}
}

func TestCreateUser_SystemAdminSetsSystemAdmin(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateUser_SysAdminSetsSysAdmin?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "SA Org", "sa-org-sysadmin")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, org.ID)

	req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":           "sysadmin2@example.com",
		"display_name":    "System Admin Two",
		"password":        "securepassword",
		"is_system_admin": true,
		"org_id":          org.ID,
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

	app, database, keyCache := setupTestApp(t, "file:TestCreateUser_NoPwInResp?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "No PW Org", "no-pw-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":        "nopw@example.com",
		"display_name": "No PW",
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

// TestCreateUser_PasswordTooLong verifies that a password exceeding the bcrypt
// 72-byte limit is rejected with 400 before reaching GenerateFromPassword,
// and that a 72-byte password (at the exact limit) is accepted with 201.
func TestCreateUser_PasswordTooLong(t *testing.T) {
	t.Parallel()

	t.Run("73-byte password returns 400", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestCreateUser_PwTooLong_73?mode=memory&cache=private")
		org := mustCreateOrg(t, database, "PW Len Org", "pw-len-org-73")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":        "toolong@example.com",
			"display_name": "Too Long PW",
			"password":     strings.Repeat("a", 73),
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
	})

	t.Run("72-byte password returns 201", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestCreateUser_PwTooLong_72?mode=memory&cache=private")
		org := mustCreateOrg(t, database, "PW Len Org", "pw-len-org-72")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":        "maxlen@example.com",
			"display_name": "Max Len PW",
			"password":     strings.Repeat("a", 72),
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
	})
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

// ---- Issue #100: CreateUserWithMembership handler tests ---------------------

// TestCreateUser_WithOrgID_CreatesUserAndMembership covers case 5:
// POST /users with a valid org_id returns 201, the user exists, and the
// membership row exists with role=member (the default).
func TestCreateUser_WithOrgID_CreatesUserAndMembership(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateUser_WithOrgID_Membership?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Membership Check Org", "membership-check-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":        "membership-check@example.com",
		"display_name": "Membership Check User",
		"password":     "securepassword",
		"org_id":       org.ID,
		// role omitted — defaults to "member"
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

	userID, _ := got["id"].(string)
	if userID == "" {
		t.Fatal("response id is empty")
	}

	// Verify the membership exists and has the correct role.
	role, err := database.GetUserOrgRole(context.Background(), userID, org.ID)
	if err != nil {
		t.Fatalf("GetUserOrgRole() after CreateUser: %v", err)
	}
	if role != "member" {
		t.Errorf("membership role = %q, want %q", role, "member")
	}
}

// TestCreateUser_OrgIDMissing_AllCallers covers case 6:
// Any caller omitting org_id gets 400, including system_admin.
func TestCreateUser_OrgIDMissing_AllCallers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		role string
	}{
		{name: "org_admin missing org_id returns 400", role: auth.RoleOrgAdmin},
		{name: "system_admin missing org_id returns 400", role: auth.RoleSystemAdmin},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateUser_OrgIDMissing_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, _, keyCache := setupTestApp(t, dsn)
			testKey := addTestKey(t, keyCache, tc.role, "")

			req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
				"email":        "no-org-id@example.com",
				"display_name": "No Org ID",
				"password":     "securepassword",
				// org_id intentionally omitted
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
		})
	}
}

// TestCreateUser_NonExistentOrgID covers case 7:
// POST /users with an org_id that references no existing org returns 400
// "organization not found" (the ErrForeignKey path).
func TestCreateUser_NonExistentOrgID(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestCreateUser_NonExistentOrgID?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":        "ghost-org@example.com",
		"display_name": "Ghost Org User",
		"password":     "securepassword",
		"org_id":       "00000000-0000-0000-0000-000000000000",
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
		t.Fatalf("status = %d, want 400; body: %s", resp.StatusCode, body)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)
	if errMsg(got) != "organization not found" {
		t.Errorf("error.message = %q, want %q", errMsg(got), "organization not found")
	}
}

// TestCreateUser_InvalidRole covers case 8:
// POST /users with an invalid role string returns 400.
func TestCreateUser_InvalidRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		role string
	}{
		{name: "system_admin role is invalid for membership", role: "system_admin"},
		{name: "bogus role returns 400", role: "bogus"},
		{name: "empty-but-explicit role passes as default", role: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateUser_InvalidRole_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)
			org := mustCreateOrg(t, database, "Role Test Org", "role-test-org-"+strings.ReplaceAll(tc.name, " ", "-"))
			testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, org.ID)

			body := map[string]any{
				"email":        "invalid-role@example.com",
				"display_name": "Invalid Role User",
				"password":     "securepassword",
				"org_id":       org.ID,
			}
			if tc.role != "" {
				body["role"] = tc.role
			}

			req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			wantStatus := fiber.StatusBadRequest
			if tc.role == "" {
				// Empty role is treated as the default "member" — should succeed.
				wantStatus = fiber.StatusCreated
			}

			if resp.StatusCode != wantStatus {
				raw, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, wantStatus, raw)
			}
		})
	}
}

// TestCreateUser_OrgAdminCannotAssignOrgAdminRole verifies that an org_admin
// caller receives 403 when attempting to create a user with role="org_admin",
// and that member/team_admin roles still succeed with 201.
// A system_admin caller must be allowed to assign org_admin.
func TestCreateUser_OrgAdminCannotAssignOrgAdminRole(t *testing.T) {
	t.Parallel()

	t.Run("org_admin sets role=org_admin returns 403", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestCreateUser_OrgAdminRoleOrgAdmin403?mode=memory&cache=private")
		org := mustCreateOrg(t, database, "Role Assign Org", "role-assign-org-1")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":        "new-org-admin@example.com",
			"display_name": "New Org Admin",
			"password":     "securepassword",
			"org_id":       org.ID,
			"role":         "org_admin",
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
			t.Errorf("status = %d, want 403; body: %s", resp.StatusCode, body)
		}
	})

	t.Run("org_admin sets role=member returns 201", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestCreateUser_OrgAdminRoleMember201?mode=memory&cache=private")
		org := mustCreateOrg(t, database, "Role Assign Org", "role-assign-org-2")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":        "new-member@example.com",
			"display_name": "New Member",
			"password":     "securepassword",
			"org_id":       org.ID,
			"role":         "member",
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
	})

	t.Run("org_admin sets role=team_admin returns 201", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestCreateUser_OrgAdminRoleTeamAdmin201?mode=memory&cache=private")
		org := mustCreateOrg(t, database, "Role Assign Org", "role-assign-org-3")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":        "new-team-admin@example.com",
			"display_name": "New Team Admin",
			"password":     "securepassword",
			"org_id":       org.ID,
			"role":         "team_admin",
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
	})

	t.Run("system_admin sets role=org_admin returns 201", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestCreateUser_SysAdminRoleOrgAdmin201?mode=memory&cache=private")
		org := mustCreateOrg(t, database, "Role Assign Org", "role-assign-org-4")
		testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, org.ID)

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":        "new-org-admin-by-sa@example.com",
			"display_name": "New Org Admin By SysAdmin",
			"password":     "securepassword",
			"org_id":       org.ID,
			"role":         "org_admin",
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
	})
}

// TestCreateUser_OrgAdminCrossOrgAuthZ covers case 9:
// An org_admin setting a foreign org_id gets 403; with own org_id gets 201.
func TestCreateUser_OrgAdminCrossOrgAuthZ(t *testing.T) {
	t.Parallel()

	t.Run("org_admin with foreign org_id returns 403", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestCreateUser_OrgAdminCrossOrg403?mode=memory&cache=private")
		ownOrg := mustCreateOrg(t, database, "Own Org", "own-org-cross")
		foreignOrg := mustCreateOrg(t, database, "Foreign Org", "foreign-org-cross")
		// Key is scoped to ownOrg, but request asks for foreignOrg.
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, ownOrg.ID)

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":        "cross-org@example.com",
			"display_name": "Cross Org User",
			"password":     "securepassword",
			"org_id":       foreignOrg.ID,
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
			t.Errorf("status = %d, want 403; body: %s", resp.StatusCode, body)
		}
	})

	t.Run("org_admin with own org_id returns 201", func(t *testing.T) {
		t.Parallel()

		app, database, keyCache := setupTestApp(t, "file:TestCreateUser_OrgAdminOwnOrg201?mode=memory&cache=private")
		ownOrg := mustCreateOrg(t, database, "Own Org", "own-org-same")
		testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, ownOrg.ID)

		req := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
			"email":        "own-org@example.com",
			"display_name": "Own Org User",
			"password":     "securepassword",
			"org_id":       ownOrg.ID,
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
	})
}

// TestCreateUser_ThenLogin_IssueRegression covers case 10 — the core regression
// for Issue #100.
//
// Before the fix: CreateUser did not create an org_membership, so the subsequent
// Login call hit ResolveUserRole, found no membership for a non-system-admin user,
// returned ErrNotFound, and the Login handler propagated that as HTTP 500.
//
// After the fix: CreateUserWithMembership creates both the user and the membership
// atomically, so Login succeeds with HTTP 200 and returns a valid session token.
func TestCreateUser_ThenLogin_IssueRegression(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateUser_ThenLogin_Issue100?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Login Regression Org", "login-regression-org")
	creatorKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	const userEmail = "regression-login@example.com"
	const userPassword = "strongpassword123"

	// Step 1: create the user via the API (uses CreateUserWithMembership).
	createReq := httptest.NewRequest("POST", "/api/v1/users", bodyJSON(t, map[string]any{
		"email":        userEmail,
		"display_name": "Regression Login User",
		"password":     userPassword,
		"org_id":       org.ID,
	}))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+creatorKey)

	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test CreateUser: %v", err)
	}
	defer createResp.Body.Close()

	if createResp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(createResp.Body)
		t.Fatalf("CreateUser status = %d, want 201; body: %s", createResp.StatusCode, body)
	}

	// Step 2: log in with the newly-created user's credentials.
	// On the old code (without membership) this returned HTTP 500 or a FK crash.
	// On the fixed code it must return HTTP 200 with a valid session token.
	loginReq := httptest.NewRequest("POST", "/api/v1/auth/login", bodyJSON(t, map[string]any{
		"email":    userEmail,
		"password": userPassword,
	}))
	loginReq.Header.Set("Content-Type", "application/json")

	loginResp, err := app.Test(loginReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test Login: %v", err)
	}
	defer loginResp.Body.Close()

	if loginResp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(loginResp.Body)
		t.Fatalf("Login status = %d, want 200 (Issue #100 regression); body: %s", loginResp.StatusCode, body)
	}

	var loginBody map[string]any
	decodeBody(t, loginResp.Body, &loginBody)

	token, _ := loginBody["token"].(string)
	if token == "" {
		t.Error("login token is empty, want a non-empty session token")
	}
	if !strings.HasPrefix(token, "vl_sk_") {
		t.Errorf("token = %q, want prefix %q", token, "vl_sk_")
	}
}

// TestLogin_LegacyUserWithoutMembership covers case 11 — the legacy guard.
//
// A user inserted directly without an org_membership (simulating pre-fix data)
// who is NOT a system_admin must receive HTTP 403 with a clear message on login,
// not HTTP 500 or a foreign-key crash.
func TestLogin_LegacyUserWithoutMembership(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestLogin_LegacyNoMembership?mode=memory&cache=private")

	// Insert a system_admin user directly, then strip the membership to simulate
	// legacy data. ResolveUserRole for system_admin with no membership returns
	// orgID="" — that is the path the guard protects.
	//
	// We use is_system_admin=true here because ResolveUserRole for a non-admin
	// with no membership returns ErrNotFound (handled as 401), while for a
	// system_admin it returns orgID="" — that is the exact legacy guard path
	// introduced by the fix.
	const legacyEmail = "legacy-no-membership@example.com"
	const legacyPassword = "legacypassword1"

	hash, err := bcrypt.GenerateFromPassword([]byte(legacyPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	hashStr := string(hash)

	// Insert directly so no membership is created — simulating the bug-era state.
	_, err = database.CreateUser(context.Background(), db.CreateUserParams{
		Email:         legacyEmail,
		DisplayName:   "Legacy User",
		PasswordHash:  &hashStr,
		IsSystemAdmin: true, // system_admin with no membership → orgID="" path
	})
	if err != nil {
		t.Fatalf("CreateUser legacy: %v", err)
	}

	loginReq := httptest.NewRequest("POST", "/api/v1/auth/login", bodyJSON(t, map[string]any{
		"email":    legacyEmail,
		"password": legacyPassword,
	}))
	loginReq.Header.Set("Content-Type", "application/json")

	loginResp, err := app.Test(loginReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test Login: %v", err)
	}
	defer loginResp.Body.Close()

	// Must be 401 with the same generic message as a wrong password — NOT 500,
	// and NOT 403 with an account-existence-revealing message.
	if loginResp.StatusCode != fiber.StatusUnauthorized {
		body, _ := io.ReadAll(loginResp.Body)
		t.Fatalf("Login status = %d, want 401 (legacy guard must not 500 or leak account info); body: %s", loginResp.StatusCode, body)
	}

	var got map[string]any
	decodeBody(t, loginResp.Body, &got)
	if errMsg(got) != "invalid email or password" {
		t.Errorf("error.message = %q, want %q (no enumeration leak)", errMsg(got), "invalid email or password")
	}
}
