package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// invitesURL returns the base invites URL for an org.
func invitesURL(orgID string) string {
	return "/api/v1/orgs/" + orgID + "/invites"
}

// inviteItemURL returns the URL for a specific invite within an org.
func inviteItemURL(orgID, inviteID string) string {
	return "/api/v1/orgs/" + orgID + "/invites/" + inviteID
}

// mustCreateInviteViaAPI calls CreateInvite through the HTTP handler and
// returns the parsed response body. It fatals the test if the status is not 201.
func mustCreateInviteViaAPI(t *testing.T, app *fiber.App, orgID, callerKey, email, role string) map[string]any {
	t.Helper()
	body := map[string]any{"email": email, "role": role}
	req := httptest.NewRequest("POST", invitesURL(orgID), bodyJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("mustCreateInviteViaAPI: app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("mustCreateInviteViaAPI: status = %d, want 201; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)
	return got
}

// mustCreateInviteDB inserts an invite token directly via the DB for test setup.
func mustCreateInviteDB(t *testing.T, database *db.DB, orgID, email, role, createdBy string) *db.InviteToken {
	t.Helper()
	plaintext, err := keygen.Generate(keygen.KeyTypeInvite)
	if err != nil {
		t.Fatalf("mustCreateInviteDB: generate token: %v", err)
	}
	tokenHash := keygen.Hash(plaintext, testHMACSecret)
	tokenHint := keygen.Hint(plaintext)
	invite, err := database.CreateInviteToken(context.Background(), db.CreateInviteTokenParams{
		TokenHash: tokenHash,
		TokenHint: tokenHint,
		OrgID:     orgID,
		Email:     email,
		Role:      role,
		ExpiresAt: "2099-01-01T00:00:00Z",
		CreatedBy: createdBy,
	})
	if err != nil {
		t.Fatalf("mustCreateInviteDB(%q, %q): %v", orgID, email, err)
	}
	return invite
}

// ---- POST /api/v1/orgs/:org_id/invites --------------------------------------

func TestCreateInvite(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		body       any
		wantStatus int
		checkField string
	}{
		{
			name:       "org_admin creates member invite",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"email": "newmember@example.com", "role": "member"},
			wantStatus: fiber.StatusCreated,
			checkField: "token",
		},
		{
			name:       "system_admin creates member invite",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			body:       map[string]any{"email": "sysadmin-invite@example.com", "role": "member"},
			wantStatus: fiber.StatusCreated,
			checkField: "token",
		},
		{
			name:       "system_admin creates org_admin invite",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			body:       map[string]any{"email": "orgadmin-invite@example.com", "role": "org_admin"},
			wantStatus: fiber.StatusCreated,
			checkField: "token",
		},
		{
			name:       "missing email returns 400",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"role": "member"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "invalid email no at-sign returns 400",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"email": "notanemail", "role": "member"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "invalid role returns 400",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"email": "x@example.com", "role": "superuser"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "org_admin cannot invite org_admin returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"email": "promo@example.com", "role": "org_admin"},
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "member returns 403 from route middleware",
			role:       auth.RoleMember,
			sameOrg:    true,
			body:       map[string]any{"email": "member-try@example.com", "role": "member"},
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			body:       map[string]any{"email": "cross-org@example.com", "role": "member"},
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateInvite_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Invite Org", "invite-org-ci-"+strings.ReplaceAll(tc.name, " ", "-"))
			creator := mustCreateUser(t, database, "creator-ci-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com", "Creator")

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKeyWithUser(t, keyCache, tc.role, keyOrgID, creator.ID)

			req := httptest.NewRequest("POST", invitesURL(org.ID), bodyJSON(t, tc.body))
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

			if tc.checkField != "" {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				if v, ok := got[tc.checkField]; !ok || v == "" {
					t.Errorf("response missing non-empty field %q; got: %v", tc.checkField, got)
				}
			}
		})
	}
}

func TestCreateInvite_NoAuth(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestCreateInvite_NoAuth?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Auth Org", "auth-org-invite-noauth")

	req := httptest.NewRequest("POST", invitesURL(org.ID),
		bodyJSON(t, map[string]any{"email": "x@x.com", "role": "member"}))
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

func TestCreateInvite_DefaultRole(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateInvite_DefaultRole?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Default Role Org", "default-role-org")
	creator := mustCreateUser(t, database, "creator-dr@example.com", "Creator DR")
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	// Send without role — should default to "member".
	req := httptest.NewRequest("POST", invitesURL(org.ID),
		bodyJSON(t, map[string]any{"email": "default-role@example.com"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)
	if got["role"] != "member" {
		t.Errorf("role = %q, want %q", got["role"], "member")
	}
}

func TestCreateInvite_ResponseFields(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateInvite_ResponseFields?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Fields Org", "fields-org-invite")
	creator := mustCreateUser(t, database, "creator-rf@example.com", "Creator RF")
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	req := httptest.NewRequest("POST", invitesURL(org.ID),
		bodyJSON(t, map[string]any{"email": "fields@example.com", "role": "member"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	for _, field := range []string{"id", "token", "token_hint", "email", "role", "org_id", "expires_at", "created_at"} {
		if v, ok := got[field]; !ok || v == "" {
			t.Errorf("response missing non-empty field %q; got: %v", field, got)
		}
	}
	if got["email"] != "fields@example.com" {
		t.Errorf("email = %q, want %q", got["email"], "fields@example.com")
	}
	if got["org_id"] != org.ID {
		t.Errorf("org_id = %q, want %q", got["org_id"], org.ID)
	}
	// Token must start with the invite prefix.
	token, _ := got["token"].(string)
	if !strings.HasPrefix(token, keygen.PrefixInvite) {
		t.Errorf("token = %q, want prefix %q", token, keygen.PrefixInvite)
	}
}

func TestCreateInvite_ReplacesExistingUnredeemed(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateInvite_Replaces?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Replace Org", "replace-org-invite")
	creator := mustCreateUser(t, database, "creator-repl@example.com", "Creator Repl")
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)
	const email = "replace@example.com"

	// First invite.
	first := mustCreateInviteViaAPI(t, app, org.ID, testKey, email, "member")
	firstID := first["id"].(string)

	// Second invite for same email — should succeed (first is revoked).
	second := mustCreateInviteViaAPI(t, app, org.ID, testKey, email, "member")
	secondID := second["id"].(string)

	if firstID == secondID {
		t.Errorf("expected new invite ID, got same ID %q", firstID)
	}
}

// ---- GET /api/v1/orgs/:org_id/invites ----------------------------------------

func TestListInvites(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListInvites?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "List Invites Org", "list-invites-org")
	// addTestKey uses "test-key-id-<role>" as the key ID; use a real user ID for created_by.
	creator := mustCreateUser(t, database, "creator-li@example.com", "Creator LI")
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	mustCreateInviteDB(t, database, org.ID, "a@example.com", "member", creator.ID)
	mustCreateInviteDB(t, database, org.ID, "b@example.com", "member", creator.ID)

	req := httptest.NewRequest("GET", invitesURL(org.ID), nil)
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

	data, ok := got["data"].([]any)
	if !ok {
		t.Fatalf("data is not an array: %v", got["data"])
	}
	if len(data) != 2 {
		t.Errorf("len(data) = %d, want 2", len(data))
	}

	// Each entry must have a status field — never a raw token (plaintext).
	for i, raw := range data {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Errorf("data[%d] is not an object", i)
			continue
		}
		if _, ok := entry["status"]; !ok {
			t.Errorf("data[%d] missing 'status' field", i)
		}
		if _, ok := entry["token"]; ok {
			t.Errorf("data[%d] must not expose 'token' field", i)
		}
	}
}

func TestListInvites_EmptyOrg(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListInvites_Empty?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Empty Invites Org", "empty-invites-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("GET", invitesURL(org.ID), nil)
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

	data, ok := got["data"].([]any)
	if !ok {
		t.Fatalf("data is not an array: %v", got["data"])
	}
	if len(data) != 0 {
		t.Errorf("len(data) = %d, want 0", len(data))
	}
}

func TestListInvites_RBAC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		wantStatus int
	}{
		{
			name:       "system_admin can list",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "org_admin of same org can list",
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

			dsn := fmt.Sprintf("file:TestListInvites_RBAC_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "RBAC Org", "rbac-org-li-"+strings.ReplaceAll(tc.name, " ", "-"))

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			req := httptest.NewRequest("GET", invitesURL(org.ID), nil)
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

// ---- DELETE /api/v1/orgs/:org_id/invites/:invite_id --------------------------

func TestRevokeInvite(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRevokeInvite?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Revoke Org", "revoke-org-invite")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)
	creator := mustCreateUser(t, database, "creator-rv@example.com", "Creator RV")

	invite := mustCreateInviteDB(t, database, org.ID, "revoke@example.com", "member", creator.ID)

	req := httptest.NewRequest("DELETE", inviteItemURL(org.ID, invite.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 204; body: %s", resp.StatusCode, b)
	}
}

func TestRevokeInvite_NotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRevokeInvite_NotFound?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "NF Revoke Org", "nf-revoke-org-invite")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("DELETE", inviteItemURL(org.ID, "00000000-0000-0000-0000-000000000099"), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNotFound {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 404; body: %s", resp.StatusCode, b)
	}
}

func TestRevokeInvite_RBAC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		wantStatus int
	}{
		{
			name:       "system_admin can revoke",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			wantStatus: fiber.StatusNoContent,
		},
		{
			name:       "org_admin of same org can revoke",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			wantStatus: fiber.StatusNoContent,
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

			dsn := fmt.Sprintf("file:TestRevokeInvite_RBAC_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "RBAC Revoke Org", "rbac-revoke-"+strings.ReplaceAll(tc.name, " ", "-"))
			creator := mustCreateUser(t, database, "creator-rbac-rv-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com", "Creator")
			invite := mustCreateInviteDB(t, database, org.ID, "rbac-revoke@example.com", "member", creator.ID)

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			req := httptest.NewRequest("DELETE", inviteItemURL(org.ID, invite.ID), nil)
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

func TestRevokeInvite_ThenListShowsGone(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRevokeInvite_ThenList?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Gone Org", "gone-org-invite")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)
	creator := mustCreateUser(t, database, "creator-tl@example.com", "Creator TL")

	invite := mustCreateInviteDB(t, database, org.ID, "gone@example.com", "member", creator.ID)

	// Delete it.
	delReq := httptest.NewRequest("DELETE", inviteItemURL(org.ID, invite.ID), nil)
	delReq.Header.Set("Authorization", "Bearer "+testKey)
	delResp, err := app.Test(delReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test DELETE: %v", err)
	}
	defer delResp.Body.Close()

	if delResp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(delResp.Body)
		t.Fatalf("DELETE status = %d, want 204; body: %s", delResp.StatusCode, b)
	}

	// List should now be empty.
	listReq := httptest.NewRequest("GET", invitesURL(org.ID), nil)
	listReq.Header.Set("Authorization", "Bearer "+testKey)
	listResp, err := app.Test(listReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test GET after DELETE: %v", err)
	}
	defer listResp.Body.Close()

	var got map[string]any
	if err := json.NewDecoder(listResp.Body).Decode(&got); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	data, _ := got["data"].([]any)
	if len(data) != 0 {
		t.Errorf("len(data) = %d after revoke, want 0", len(data))
	}
}
