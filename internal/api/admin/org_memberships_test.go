package admin_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
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

// mustCreateOrgAdminCallerKey creates a real, DB-backed org_admin user_key for
// orgID/userID and populates the key cache from the DB via
// auth.LoadKeysIntoCache. Unlike a cache-only entry (addTestKey), a
// DB-backed key survives the full cache reload that UpdateOrgMembership
// triggers on every successful PATCH — required whenever a test issues
// more than one request with the same caller key across a PATCH boundary.
func mustCreateOrgAdminCallerKey(t *testing.T, database *db.DB, keyCache *cache.Cache[string, auth.KeyInfo], orgID, userID string) string {
	t.Helper()

	if _, err := database.CreateOrgMembership(context.Background(), db.CreateOrgMembershipParams{
		OrgID:  orgID,
		UserID: userID,
		Role:   auth.RoleOrgAdmin,
	}); err != nil {
		t.Fatalf("mustCreateOrgAdminCallerKey: CreateOrgMembership: %v", err)
	}

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("mustCreateOrgAdminCallerKey: generate: %v", err)
	}
	if _, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "org-admin-caller",
		OrgID:     orgID,
		UserID:    &userID,
		CreatedBy: userID,
	}); err != nil {
		t.Fatalf("mustCreateOrgAdminCallerKey: CreateAPIKey: %v", err)
	}

	if err := auth.LoadKeysIntoCache(context.Background(), database, keyCache, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("mustCreateOrgAdminCallerKey: LoadKeysIntoCache: %v", err)
	}

	return plaintext
}

// mustCreateSystemAdminCallerKey creates a real, DB-backed system_admin
// user_key and populates the key cache from the DB via auth.LoadKeysIntoCache
// — see mustCreateOrgAdminCallerKey for why a DB-backed key is required
// whenever a test issues more than one request with the same caller key
// across an UpdateOrgMembership PATCH's cache reload. system_admin is resolved
// from users.is_system_admin, not from an org membership row, so this caller
// deliberately has no membership in orgID.
func mustCreateSystemAdminCallerKey(t *testing.T, database *db.DB, keyCache *cache.Cache[string, auth.KeyInfo], orgID string) string {
	t.Helper()

	user, err := database.CreateUser(context.Background(), db.CreateUserParams{
		Email:         fmt.Sprintf("sysadmin-caller-%s@example.com", orgID),
		DisplayName:   "System Admin Caller",
		AuthProvider:  "local",
		IsSystemAdmin: true,
	})
	if err != nil {
		t.Fatalf("mustCreateSystemAdminCallerKey: CreateUser: %v", err)
	}

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("mustCreateSystemAdminCallerKey: generate: %v", err)
	}
	if _, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "system-admin-caller",
		OrgID:     orgID,
		UserID:    &user.ID,
		CreatedBy: user.ID,
	}); err != nil {
		t.Fatalf("mustCreateSystemAdminCallerKey: CreateAPIKey: %v", err)
	}

	if err := auth.LoadKeysIntoCache(context.Background(), database, keyCache, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("mustCreateSystemAdminCallerKey: LoadKeysIntoCache: %v", err)
	}

	return plaintext
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

// ---- PATCH .../members/:id — per-user limits --------------------------------

// TestUpdateOrgMembership_Limits covers persistence, validation, and RBAC for
// the per-user rate/token limit fields added to org memberships. org_admin and
// system_admin may set them (with or without a role change in the same
// request); negative values are rejected; a member caller never reaches the
// handler (RequireRole middleware rejects at the route level).
//
// Every case whose PATCH is expected to succeed uses a real, DB-backed caller
// key (mustCreateOrgAdminCallerKey / mustCreateSystemAdminCallerKey) rather
// than a cache-only one: UpdateOrgMembership does a full key-cache reload
// from the DB on success, and a cache-only key does not survive that
// reload — the follow-up GET .../members list request below would otherwise
// get 401 instead of exercising the list-endpoint assertions it is there for.
func TestUpdateOrgMembership_Limits(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		keyOrgID   func(targetOrgID string) string
		body       map[string]any
		wantStatus int
		check      func(t *testing.T, got map[string]any)
	}{
		{
			name:     "org_admin sets limits without touching role",
			role:     auth.RoleOrgAdmin,
			keyOrgID: func(orgID string) string { return orgID },
			body: map[string]any{
				"daily_token_limit":   float64(10_000),
				"monthly_token_limit": float64(200_000),
				"requests_per_minute": float64(5),
				"requests_per_day":    float64(100),
			},
			wantStatus: fiber.StatusOK,
			check: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["role"] != "member" {
					t.Errorf("role = %v, want unchanged %q", got["role"], "member")
				}
				if got["daily_token_limit"] != float64(10_000) {
					t.Errorf("daily_token_limit = %v, want 10000", got["daily_token_limit"])
				}
				if got["monthly_token_limit"] != float64(200_000) {
					t.Errorf("monthly_token_limit = %v, want 200000", got["monthly_token_limit"])
				}
				if got["requests_per_minute"] != float64(5) {
					t.Errorf("requests_per_minute = %v, want 5", got["requests_per_minute"])
				}
				if got["requests_per_day"] != float64(100) {
					t.Errorf("requests_per_day = %v, want 100", got["requests_per_day"])
				}
			},
		},
		{
			name:     "system_admin sets limits alongside a role change",
			role:     auth.RoleSystemAdmin,
			keyOrgID: func(_ string) string { return "" },
			body: map[string]any{
				"role":                "org_admin",
				"requests_per_minute": float64(42),
			},
			wantStatus: fiber.StatusOK,
			check: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["role"] != "org_admin" {
					t.Errorf("role = %v, want %q", got["role"], "org_admin")
				}
				if got["requests_per_minute"] != float64(42) {
					t.Errorf("requests_per_minute = %v, want 42", got["requests_per_minute"])
				}
			},
		},
		{
			name:       "negative daily_token_limit returns 400",
			role:       auth.RoleOrgAdmin,
			keyOrgID:   func(orgID string) string { return orgID },
			body:       map[string]any{"daily_token_limit": float64(-1)},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "negative requests_per_minute returns 400",
			role:       auth.RoleOrgAdmin,
			keyOrgID:   func(orgID string) string { return orgID },
			body:       map[string]any{"requests_per_minute": float64(-1)},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "member caller rejected at route level",
			role:       auth.RoleMember,
			keyOrgID:   func(orgID string) string { return orgID },
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestUpdateOrgMembership_Limits_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Lim Mem Org", "limmem-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "limmem-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com", "U")
			m := mustCreateMembership(t, database, org.ID, user.ID, "member")

			// Successful PATCHes trigger a full key-cache reload; the
			// caller key must be DB-backed to survive it and still
			// authenticate the follow-up list request below. Failing cases
			// never reach the reload, so a cache-only key is fine there.
			var testKey string
			switch {
			case tc.wantStatus != fiber.StatusOK:
				testKey = addTestKey(t, keyCache, tc.role, tc.keyOrgID(org.ID))
			case tc.role == auth.RoleSystemAdmin:
				testKey = mustCreateSystemAdminCallerKey(t, database, keyCache, org.ID)
			default:
				caller := mustCreateUser(t, database, "limmem-caller-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com", "Caller")
				testKey = mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, caller.ID)
			}

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

			if tc.check != nil {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				tc.check(t, got)
			}

			// On success, verify the DB record and the list endpoint agree.
			if resp.StatusCode == fiber.StatusOK {
				reloaded, err := database.GetOrgMembership(context.Background(), m.ID)
				if err != nil {
					t.Fatalf("GetOrgMembership: %v", err)
				}
				if want, ok := tc.body["requests_per_minute"].(float64); ok {
					if reloaded.RequestsPerMinute != int(want) {
						t.Errorf("DB RequestsPerMinute = %d, want %d", reloaded.RequestsPerMinute, int(want))
					}
				}

				listReq := httptest.NewRequest("GET", "/api/v1/orgs/"+org.ID+"/members", nil)
				listReq.Header.Set("Authorization", "Bearer "+testKey)
				listResp, err := app.Test(listReq, fiber.TestConfig{Timeout: testTimeout})
				if err != nil {
					t.Fatalf("list app.Test: %v", err)
				}
				defer listResp.Body.Close()
				var listGot map[string]any
				decodeBody(t, listResp.Body, &listGot)
				data, _ := listGot["data"].([]any)
				var found map[string]any
				for _, entry := range data {
					em := entry.(map[string]any)
					if em["id"] == m.ID {
						found = em
						break
					}
				}
				if found == nil {
					t.Fatalf("membership %q not found in list response", m.ID)
				}
				if want, ok := tc.body["requests_per_minute"].(float64); ok {
					if found["requests_per_minute"] != want {
						t.Errorf("list requests_per_minute = %v, want %v", found["requests_per_minute"], want)
					}
				}
			}
		})
	}
}

// TestUpdateOrgMembership_LimitsFullCacheReload verifies that a successful
// PATCH reloads the entire key cache from the DB (auth.LoadKeysIntoCache)
// rather than patching matching entries in place. Every DB-backed key
// belonging to the target user in the target org picks up the new User*
// limits; the same user's key in a different org keeps that org's own
// membership limits, untouched by this PATCH; another user's key in the same
// org keeps its own membership limits too. Because a full reload replaces
// the cache wholesale (cache.Cache.LoadAll) rather than merging into it, a
// cache-only entry with no backing DB row does not survive the reload at
// all — a deliberate behavior change from the prior in-place Range-then-Set
// patch, which never touched entries outside the target user/org and so
// would have left such an entry (and any of its staleness) untouched
// forever. The full reload is the DB's source of truth, with no race against
// the periodic background reload goroutine.
func TestUpdateOrgMembership_LimitsFullCacheReload(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateOrgMembership_FullReload?mode=memory&cache=private")

	orgA := mustCreateOrg(t, database, "Org A", "fullreload-org-a")
	orgB := mustCreateOrg(t, database, "Org B", "fullreload-org-b")
	targetUser := mustCreateUser(t, database, "fullreload-target@example.com", "Target")
	otherUser := mustCreateUser(t, database, "fullreload-other@example.com", "Other")
	callerUser := mustCreateUser(t, database, "fullreload-caller@example.com", "Caller")

	mTarget := mustCreateMembership(t, database, orgA.ID, targetUser.ID, "member")

	// targetUser also has a membership in orgB, seeded with its own, distinct
	// limit, so orgB's key can be shown to keep that value rather than
	// coincidentally reading back as zero.
	mTargetOrgB := mustCreateMembership(t, database, orgB.ID, targetUser.ID, "member")
	orgBRPD := 55
	if _, err := database.UpdateOrgMembership(context.Background(), mTargetOrgB.ID, db.UpdateOrgMembershipParams{
		RequestsPerDay: &orgBRPD,
	}); err != nil {
		t.Fatalf("seed orgB membership limit: %v", err)
	}

	// otherUser's own orgA membership is likewise seeded with a distinct
	// baseline, so its key can be shown to keep its own value rather than
	// picking up targetUser's new one.
	mOther := mustCreateMembership(t, database, orgA.ID, otherUser.ID, "member")
	otherRPM := 11
	if _, err := database.UpdateOrgMembership(context.Background(), mOther.ID, db.UpdateOrgMembershipParams{
		RequestsPerMinute: &otherRPM,
	}); err != nil {
		t.Fatalf("seed other user's membership limit: %v", err)
	}

	// Two real, DB-backed keys owned by targetUser in orgA — both must pick
	// up the new limits after the reload.
	keyA1 := mustCreateAPIKeyDirect(t, database, orgA.ID, nil, &targetUser.ID, targetUser.ID, 0)
	keyA2 := mustCreateAPIKeyDirect(t, database, orgA.ID, nil, &targetUser.ID, targetUser.ID, 0)
	// Same user, a key in orgB — must reflect orgB's own membership limit,
	// unaffected by the orgA PATCH below.
	keyB := mustCreateAPIKeyDirect(t, database, orgB.ID, nil, &targetUser.ID, targetUser.ID, 0)
	// A different user's key in orgA — must reflect otherUser's own
	// membership limit, unaffected by targetUser's PATCH.
	keyOther := mustCreateAPIKeyDirect(t, database, orgA.ID, nil, &otherUser.ID, otherUser.ID, 0)

	// A real, DB-backed org_admin caller — required to survive the reload
	// this PATCH triggers below even though this test issues no further
	// request with it afterward; a cache-only caller would work for the
	// PATCH itself just as well, since authentication happens before the
	// reload runs. mustCreateOrgAdminCallerKey's own LoadKeysIntoCache call
	// also conveniently populates the cache with every key created above,
	// giving this test a "before" snapshot for free.
	callerKey := mustCreateOrgAdminCallerKey(t, database, keyCache, orgA.ID, callerUser.ID)

	// A cache-only entry with no backing DB row — must be gone after the
	// PATCH's full reload below.
	const cacheOnlyHash = "fullreload-cache-only-not-in-db"
	keyCache.Set(cacheOnlyHash, auth.KeyInfo{
		ID: "cache-only-key", KeyType: "user_key", Role: auth.RoleMember,
		OrgID: orgA.ID, UserID: targetUser.ID, Name: "cache-only",
	})

	body := map[string]any{
		"daily_token_limit":   float64(7_777),
		"monthly_token_limit": float64(88_888),
		"requests_per_minute": float64(9),
		"requests_per_day":    float64(99),
	}
	req := httptest.NewRequest("PATCH", "/api/v1/orgs/"+orgA.ID+"/members/"+mTarget.ID, bodyJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	// Both of targetUser's orgA keys must reflect the new limits.
	for _, k := range []*db.APIKey{keyA1, keyA2} {
		ki, ok := keyCache.Get(k.KeyHash)
		if !ok {
			t.Fatalf("key %q missing from cache after reload", k.ID)
		}
		if ki.UserDailyTokenLimit != 7_777 {
			t.Errorf("%s: UserDailyTokenLimit = %d, want 7777", k.ID, ki.UserDailyTokenLimit)
		}
		if ki.UserMonthlyTokenLimit != 88_888 {
			t.Errorf("%s: UserMonthlyTokenLimit = %d, want 88888", k.ID, ki.UserMonthlyTokenLimit)
		}
		if ki.UserRequestsPerMinute != 9 {
			t.Errorf("%s: UserRequestsPerMinute = %d, want 9", k.ID, ki.UserRequestsPerMinute)
		}
		if ki.UserRequestsPerDay != 99 {
			t.Errorf("%s: UserRequestsPerDay = %d, want 99", k.ID, ki.UserRequestsPerDay)
		}
	}

	// targetUser's key in orgB must keep orgB's own membership limit.
	kiB, ok := keyCache.Get(keyB.KeyHash)
	if !ok {
		t.Fatal("key-b missing from cache after reload")
	}
	if kiB.UserRequestsPerDay != orgBRPD {
		t.Errorf("key-b: UserRequestsPerDay = %d, want %d (orgB's own membership limit, untouched by the orgA PATCH)",
			kiB.UserRequestsPerDay, orgBRPD)
	}
	if kiB.UserDailyTokenLimit != 0 || kiB.UserMonthlyTokenLimit != 0 || kiB.UserRequestsPerMinute != 0 {
		t.Errorf("key-b: other user-limit fields = %+v, want all zero (orgB membership never set them)", kiB)
	}

	// otherUser's key in orgA must keep otherUser's own membership limit.
	kiOther, ok := keyCache.Get(keyOther.KeyHash)
	if !ok {
		t.Fatal("key-other missing from cache after reload")
	}
	if kiOther.UserRequestsPerMinute != otherRPM {
		t.Errorf("key-other: UserRequestsPerMinute = %d, want %d (otherUser's own membership limit, untouched by targetUser's PATCH)",
			kiOther.UserRequestsPerMinute, otherRPM)
	}
	if kiOther.UserDailyTokenLimit != 0 || kiOther.UserMonthlyTokenLimit != 0 || kiOther.UserRequestsPerDay != 0 {
		t.Errorf("key-other: other user-limit fields = %+v, want all zero (otherUser membership never set them)", kiOther)
	}

	// The cache-only entry has no backing DB row, so the full reload evicted
	// it entirely — the documented behavior difference from the prior
	// in-place patch, which would have left it untouched.
	if _, ok := keyCache.Get(cacheOnlyHash); ok {
		t.Error("cache-only entry (no backing DB row) still present after reload, want evicted")
	}
}
