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
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// mustCreateUserMemberships sets up both org membership and team membership for a user,
// satisfying the validation requirements when creating a user_key.
func mustCreateUserMemberships(t *testing.T, database *db.DB, orgID, teamID, userID string) {
	t.Helper()
	if _, err := database.CreateOrgMembership(context.Background(), db.CreateOrgMembershipParams{
		OrgID:  orgID,
		UserID: userID,
		Role:   auth.RoleOrgAdmin,
	}); err != nil {
		t.Fatalf("mustCreateUserMemberships: CreateOrgMembership: %v", err)
	}
	if _, err := database.CreateTeamMembership(context.Background(), db.CreateTeamMembershipParams{
		TeamID: teamID,
		UserID: userID,
		Role:   auth.RoleMember,
	}); err != nil {
		t.Fatalf("mustCreateUserMemberships: CreateTeamMembership: %v", err)
	}
}

// keysURL returns the API keys base URL for an org.
func keysURL(orgID string) string {
	return "/api/v1/orgs/" + orgID + "/keys"
}

// keyItemURL returns the URL for a specific API key within an org.
func keyItemURL(orgID, keyID string) string {
	return "/api/v1/orgs/" + orgID + "/keys/" + keyID
}

// ---- POST /api/v1/orgs/:org_id/keys -----------------------------------------

func TestCreateAPIKey_UserKey(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateAPIKey_UserKey?mode=memory&cache=private"
	app, database, keyCache := setupTestApp(t, dsn)

	org := mustCreateOrg(t, database, "Acme", "acme-key-1")
	user := mustCreateUser(t, database, "creator@example.com", "Creator")
	team := mustCreateTeam(t, database, org.ID, "Dev", "dev-key-1")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	body := map[string]any{
		"name":     "My User Key",
		"key_type": keygen.KeyTypeUser,
		"user_id":  user.ID,
		"team_id":  team.ID,
	}
	req := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, body))
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

	// Plaintext key must be present in create response.
	key, ok := got["key"].(string)
	if !ok || key == "" {
		t.Errorf("response missing non-empty 'key' field; got: %v", got)
	}
	// key must start with the user_key prefix.
	if !strings.HasPrefix(key, keygen.PrefixUser) {
		t.Errorf("key = %q, want prefix %q", key, keygen.PrefixUser)
	}
	// key_hint must be present.
	if _, ok := got["key_hint"]; !ok {
		t.Errorf("response missing 'key_hint' field; got: %v", got)
	}
	// id must be present.
	if _, ok := got["id"]; !ok {
		t.Errorf("response missing 'id' field; got: %v", got)
	}
}

func TestCreateAPIKey_KeyNeverInGetResponse(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateAPIKey_KeyNeverInGet?mode=memory&cache=private"
	app, database, keyCache := setupTestApp(t, dsn)

	org := mustCreateOrg(t, database, "Acme", "acme-key-2")
	user := mustCreateUser(t, database, "creator2@example.com", "Creator2")
	team := mustCreateTeam(t, database, org.ID, "Dev", "dev-key-2")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	// Create a key.
	createBody := map[string]any{
		"name":     "Ephemeral Key",
		"key_type": keygen.KeyTypeUser,
		"user_id":  user.ID,
		"team_id":  team.ID,
	}
	createReq := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+testKey)

	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("create app.Test: %v", err)
	}
	defer createResp.Body.Close()

	if createResp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(createResp.Body)
		t.Fatalf("create status = %d; body: %s", createResp.StatusCode, b)
	}

	var created map[string]any
	decodeBody(t, createResp.Body, &created)
	keyID, _ := created["id"].(string)

	// GET must not include 'key' or 'key_hash'.
	getReq := httptest.NewRequest("GET", keyItemURL(org.ID, keyID), nil)
	getReq.Header.Set("Authorization", "Bearer "+testKey)

	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("get app.Test: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(getResp.Body)
		t.Fatalf("get status = %d; body: %s", getResp.StatusCode, b)
	}

	var gotGet map[string]any
	decodeBody(t, getResp.Body, &gotGet)

	if _, exists := gotGet["key"]; exists {
		t.Errorf("GET response must not contain 'key' field")
	}
	if _, exists := gotGet["key_hash"]; exists {
		t.Errorf("GET response must not contain 'key_hash' field")
	}
	if _, ok := gotGet["key_hint"]; !ok {
		t.Errorf("GET response missing 'key_hint' field")
	}
}

func TestCreateAPIKey_ValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       map[string]any
		wantStatus int
	}{
		{
			name:       "missing key_type returns 400",
			body:       map[string]any{"name": "Key"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "invalid key_type returns 400",
			body:       map[string]any{"name": "Key", "key_type": "bad_type"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "user_key without user_id returns 400",
			body: map[string]any{
				"name":     "Key",
				"key_type": keygen.KeyTypeUser,
				"team_id":  "some-team-id",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "user_key without team_id returns 400",
			body: map[string]any{
				"name":     "Key",
				"key_type": keygen.KeyTypeUser,
				"user_id":  "some-user-id",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "team_key without team_id returns 400",
			body: map[string]any{
				"name":     "Key",
				"key_type": keygen.KeyTypeTeam,
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "team_key with user_id returns 400",
			body: map[string]any{
				"name":     "Key",
				"key_type": keygen.KeyTypeTeam,
				"team_id":  "some-team-id",
				"user_id":  "some-user-id",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "sa_key without service_account_id returns 400",
			body: map[string]any{
				"name":     "Key",
				"key_type": keygen.KeyTypeSA,
			},
			wantStatus: fiber.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateAPIKey_Val_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "O", "o-val-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "val"+strings.ReplaceAll(tc.name, " ", "")+"@example.com", "V")
			testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

			req := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, tc.body))
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

func TestCreateAPIKey_RBAC(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		wantStatus int
	}{
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "member of different org returns 403",
			role:       auth.RoleMember,
			sameOrg:    false,
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateAPIKey_RBAC_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "O", "rbac-key-org-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "rbac"+strings.ReplaceAll(tc.name, " ", "")+"@example.com", "R")
			team := mustCreateTeam(t, database, org.ID, "T", "t-rbac-"+strings.ReplaceAll(tc.name, " ", "-"))

			keyOrgID := "00000000-0000-0000-0000-000000000099"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKeyWithUser(t, keyCache, tc.role, keyOrgID, user.ID)

			body := map[string]any{
				"name":     "Key",
				"key_type": keygen.KeyTypeUser,
				"user_id":  user.ID,
				"team_id":  team.ID,
			}
			req := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, body))
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

// TestCreateAPIKey_MemberSelf verifies that a member can create a user_key for
// themselves when they have the required org and team membership.
func TestCreateAPIKey_MemberSelf(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateAPIKey_MemberSelf?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "O", "member-self-key-org")
	user := mustCreateUser(t, database, "memberself@example.com", "M")
	team := mustCreateTeam(t, database, org.ID, "T", "t-member-self")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)

	testKey := addTestKeyWithUser(t, keyCache, auth.RoleMember, org.ID, user.ID)

	body := map[string]any{
		"name":     "My Key",
		"key_type": keygen.KeyTypeUser,
		"team_id":  team.ID,
		// user_id is omitted — handler forces it to the caller's own ID.
	}
	req := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 201; body: %s", resp.StatusCode, b)
	}
}

// TestCreateAPIKey_MemberForbiddenTeamKey verifies that a member cannot create a
// team_key even when they belong to the org.
func TestCreateAPIKey_MemberForbiddenTeamKey(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateAPIKey_MemberForbiddenTeamKey?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "O", "member-team-key-org")
	user := mustCreateUser(t, database, "memberteam@example.com", "M")
	team := mustCreateTeam(t, database, org.ID, "T", "t-member-team")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)

	testKey := addTestKeyWithUser(t, keyCache, auth.RoleMember, org.ID, user.ID)

	body := map[string]any{
		"name":     "Team Key",
		"key_type": keygen.KeyTypeTeam,
		"team_id":  team.ID,
	}
	req := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusForbidden {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 403; body: %s", resp.StatusCode, b)
	}
}

func TestCreateAPIKey_NoAuth(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestCreateAPIKey_NoAuth?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "O", "noauth-key-org")

	req := httptest.NewRequest("POST", keysURL(org.ID),
		bodyJSON(t, map[string]any{"name": "K", "key_type": keygen.KeyTypeUser}))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

// ---- GET /api/v1/orgs/:org_id/keys/:key_id ----------------------------------

func TestGetAPIKey_Handler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setupKeyID func(t *testing.T, app *fiber.App, database *db.DB, keyCache *cache.Cache[string, auth.KeyInfo], orgID, userID, teamID, testKey string) string
		wantStatus int
		noKeyField bool
	}{
		{
			name: "get existing key returns 200 with no 'key' field",
			setupKeyID: func(t *testing.T, app *fiber.App, database *db.DB, keyCache *cache.Cache[string, auth.KeyInfo], orgID, userID, teamID, testKey string) string {
				t.Helper()
				// Create via API to get the key ID.
				body := map[string]any{
					"name":     "Fetch Me",
					"key_type": keygen.KeyTypeUser,
					"user_id":  userID,
					"team_id":  teamID,
				}
				req := httptest.NewRequest("POST", keysURL(orgID), bodyJSON(t, body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+testKey)
				resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
				if err != nil {
					t.Fatalf("create: %v", err)
				}
				defer resp.Body.Close()
				var created map[string]any
				decodeBody(t, resp.Body, &created)
				return created["id"].(string)
			},
			wantStatus: fiber.StatusOK,
			noKeyField: true,
		},
		{
			name: "cross-org key returns 404",
			setupKeyID: func(t *testing.T, _ *fiber.App, _ *db.DB, _ *cache.Cache[string, auth.KeyInfo], _, _, _, _ string) string {
				return "00000000-0000-0000-0000-000000000001"
			},
			wantStatus: fiber.StatusNotFound,
		},
		{
			name: "not found returns 404",
			setupKeyID: func(t *testing.T, _ *fiber.App, _ *db.DB, _ *cache.Cache[string, auth.KeyInfo], _, _, _, _ string) string {
				return "00000000-0000-0000-0000-000000000002"
			},
			wantStatus: fiber.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestGetAPIKey_H_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "O", "get-handler-org-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "gh"+strings.ReplaceAll(tc.name, " ", "")+"@example.com", "G")
			team := mustCreateTeam(t, database, org.ID, "T", "t-gh-"+strings.ReplaceAll(tc.name, " ", "-"))
			mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
			testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

			keyID := tc.setupKeyID(t, app, database, keyCache, org.ID, user.ID, team.ID, testKey)

			req := httptest.NewRequest("GET", keyItemURL(org.ID, keyID), nil)
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

			if tc.noKeyField && tc.wantStatus == fiber.StatusOK {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				if _, exists := got["key"]; exists {
					t.Errorf("GET response must not contain 'key' field")
				}
				if _, exists := got["key_hash"]; exists {
					t.Errorf("GET response must not contain 'key_hash' field")
				}
			}
		})
	}
}

// ---- GET /api/v1/orgs/:org_id/keys ------------------------------------------

func TestListAPIKeys_Handler(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAPIKeys_Handler?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "O", "list-handler-org")
	user := mustCreateUser(t, database, "list-handler@example.com", "L")
	team := mustCreateTeam(t, database, org.ID, "T", "t-list-handler")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	// Create 3 keys.
	for i := range 3 {
		body := map[string]any{
			"name":     fmt.Sprintf("key-%d", i),
			"key_type": keygen.KeyTypeUser,
			"user_id":  user.ID,
			"team_id":  team.ID,
		}
		req := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testKey)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("create key %d: %v", i, err)
		}
		resp.Body.Close()
	}

	// First page: limit=2.
	req := httptest.NewRequest("GET", keysURL(org.ID)+"?limit=2", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("list page1: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("list status = %d; body: %s", resp.StatusCode, b)
	}

	var page1 map[string]any
	decodeBody(t, resp.Body, &page1)

	data, ok := page1["data"].([]any)
	if !ok {
		t.Fatalf("data is not array: %v", page1["data"])
	}
	if len(data) != 2 {
		t.Errorf("page1 len(data) = %d, want 2", len(data))
	}
	hasMore, _ := page1["has_more"].(bool)
	if !hasMore {
		t.Errorf("page1 has_more = false, want true")
	}
	cursor, _ := page1["next_cursor"].(string)
	if cursor == "" {
		t.Error("page1 next_cursor is empty")
	}

	// Verify no 'key' field in list items.
	for i, item := range data {
		m := item.(map[string]any)
		if _, exists := m["key"]; exists {
			t.Errorf("data[%d] contains 'key' field, must not", i)
		}
		if _, exists := m["key_hash"]; exists {
			t.Errorf("data[%d] contains 'key_hash' field, must not", i)
		}
	}

	// Second page.
	req2 := httptest.NewRequest("GET", keysURL(org.ID)+"?limit=2&cursor="+cursor, nil)
	req2.Header.Set("Authorization", "Bearer "+testKey)

	resp2, err := app.Test(req2, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("list page2: %v", err)
	}
	defer resp2.Body.Close()

	var page2 map[string]any
	decodeBody(t, resp2.Body, &page2)

	data2, _ := page2["data"].([]any)
	if len(data2) != 1 {
		t.Errorf("page2 len(data) = %d, want 1", len(data2))
	}
	hasMore2, _ := page2["has_more"].(bool)
	if hasMore2 {
		t.Errorf("page2 has_more = true, want false")
	}
}

// ---- PATCH /api/v1/orgs/:org_id/keys/:key_id --------------------------------

func TestUpdateAPIKey_Handler(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       map[string]any
		wantStatus int
		check      func(t *testing.T, got map[string]any)
	}{
		{
			name:       "update name returns 200",
			body:       map[string]any{"name": "Renamed Key"},
			wantStatus: fiber.StatusOK,
			check: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["name"] != "Renamed Key" {
					t.Errorf("name = %q, want %q", got["name"], "Renamed Key")
				}
			},
		},
		{
			name: "update limits returns 200",
			body: map[string]any{
				"daily_token_limit":   float64(9999),
				"monthly_token_limit": float64(99999),
			},
			wantStatus: fiber.StatusOK,
			check: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["daily_token_limit"] != float64(9999) {
					t.Errorf("daily_token_limit = %v, want 9999", got["daily_token_limit"])
				}
				if got["monthly_token_limit"] != float64(99999) {
					t.Errorf("monthly_token_limit = %v, want 99999", got["monthly_token_limit"])
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestUpdateAPIKey_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "O", "upd-key-org-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "upd"+strings.ReplaceAll(tc.name, " ", "")+"@example.com", "U")
			team := mustCreateTeam(t, database, org.ID, "T", "t-upd-k-"+strings.ReplaceAll(tc.name, " ", "-"))
			mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
			testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

			// Create a key to update.
			createBody := map[string]any{
				"name":     "Original",
				"key_type": keygen.KeyTypeUser,
				"user_id":  user.ID,
				"team_id":  team.ID,
			}
			createReq := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, createBody))
			createReq.Header.Set("Content-Type", "application/json")
			createReq.Header.Set("Authorization", "Bearer "+testKey)
			createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			var created map[string]any
			decodeBody(t, createResp.Body, &created)
			createResp.Body.Close()
			keyID := created["id"].(string)

			// Patch.
			req := httptest.NewRequest("PATCH", keyItemURL(org.ID, keyID), bodyJSON(t, tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("patch: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, b)
				return
			}

			if tc.check != nil {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				tc.check(t, got)
			}
		})
	}
}

func TestUpdateAPIKey_NotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateAPIKey_NotFound?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "O", "upd-key-notfound")
	user := mustCreateUser(t, database, "upd-nf@example.com", "U")
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	req := httptest.NewRequest("PATCH", keyItemURL(org.ID, "00000000-0000-0000-0000-000000000000"),
		bodyJSON(t, map[string]any{"name": "Ghost"}))
	req.Header.Set("Content-Type", "application/json")
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

// ---- DELETE /api/v1/orgs/:org_id/keys/:key_id -------------------------------

func TestDeleteAPIKey_Handler(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteAPIKey_Handler?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "O", "del-key-org-h")
	user := mustCreateUser(t, database, "del-key@example.com", "D")
	team := mustCreateTeam(t, database, org.ID, "T", "t-del-key-h")
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	// Create a key.
	createBody := map[string]any{
		"name":     "To Delete",
		"key_type": keygen.KeyTypeUser,
		"user_id":  user.ID,
		"team_id":  team.ID,
	}
	createReq := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+testKey)
	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created map[string]any
	decodeBody(t, createResp.Body, &created)
	createResp.Body.Close()
	keyID := created["id"].(string)

	// Delete.
	delReq := httptest.NewRequest("DELETE", keyItemURL(org.ID, keyID), nil)
	delReq.Header.Set("Authorization", "Bearer "+testKey)

	delResp, err := app.Test(delReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	delResp.Body.Close()

	if delResp.StatusCode != fiber.StatusNoContent {
		t.Errorf("DELETE status = %d, want 204", delResp.StatusCode)
	}

	// Subsequent GET must return 404.
	getReq := httptest.NewRequest("GET", keyItemURL(org.ID, keyID), nil)
	getReq.Header.Set("Authorization", "Bearer "+testKey)
	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("get after delete: %v", err)
	}
	getResp.Body.Close()

	if getResp.StatusCode != fiber.StatusNotFound {
		t.Errorf("GET after DELETE status = %d, want 404", getResp.StatusCode)
	}
}

func TestDeleteAPIKey_NotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteAPIKey_NotFound?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "O", "del-key-nf-org")
	user := mustCreateUser(t, database, "del-nf@example.com", "D")
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	req := httptest.NewRequest("DELETE", keyItemURL(org.ID, "00000000-0000-0000-0000-000000000000"), nil)
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

func TestDeleteAPIKey_RemovesFromCache(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteAPIKey_Cache?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "O", "del-cache-org")
	user := mustCreateUser(t, database, "del-cache@example.com", "D")
	team := mustCreateTeam(t, database, org.ID, "T", "t-del-cache")

	// Add org and team membership so the handler validates membership and caches the new key.
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)

	testKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, user.ID)

	// Create a key via API — this adds it to KeyCache.
	createBody := map[string]any{
		"name":     "Cache Test Key",
		"key_type": keygen.KeyTypeUser,
		"user_id":  user.ID,
		"team_id":  team.ID,
	}
	createReq := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+testKey)
	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created map[string]any
	decodeBody(t, createResp.Body, &created)
	createResp.Body.Close()

	keyID := created["id"].(string)
	plaintextKey, _ := created["key"].(string)

	// Verify the key is in cache before deletion.
	keyHash := keygen.Hash(plaintextKey, testHMACSecret)
	if _, ok := keyCache.Get(keyHash); !ok {
		t.Fatal("key not found in cache after creation, expected it to be cached")
	}

	// Delete the key.
	delReq := httptest.NewRequest("DELETE", keyItemURL(org.ID, keyID), nil)
	delReq.Header.Set("Authorization", "Bearer "+testKey)
	delResp, err := app.Test(delReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	delResp.Body.Close()

	if delResp.StatusCode != fiber.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", delResp.StatusCode)
	}

	// The cache entry must be gone.
	if _, ok := keyCache.Get(keyHash); ok {
		t.Error("key still in cache after DELETE, expected it to be evicted")
	}
}
