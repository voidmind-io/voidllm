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

// addTestKeyWithUser generates a key like addTestKey but also populates UserID in
// the cache entry. CreateServiceAccount requires a non-empty UserID (user keys only).
func addTestKeyWithUser(t *testing.T, keyCache *cache.Cache[string, auth.KeyInfo], role, orgID, userID string) string {
	t.Helper()

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}

	hash := keygen.Hash(plaintext, testHMACSecret)
	keyCache.Set(hash, auth.KeyInfo{
		ID:      "test-key-id-" + role + "-user",
		KeyType: keygen.KeyTypeUser,
		Role:    role,
		OrgID:   orgID,
		UserID:  userID,
		Name:    "test key " + role,
	})

	return plaintext
}

// mustCreateSAUser creates a real user in the DB for use as the created_by field.
// Returns the user's ID.
func mustCreateSAUser(t *testing.T, database *db.DB, email string) string {
	t.Helper()
	user := mustCreateUser(t, database, email, "SA Creator")
	return user.ID
}

// mustCreateServiceAccount inserts a service account directly via the DB for test setup.
func mustCreateServiceAccountHTTP(t *testing.T, database *db.DB, orgID, userID, name string, teamID *string) *db.ServiceAccount {
	t.Helper()
	sa, err := database.CreateServiceAccount(context.Background(), db.CreateServiceAccountParams{
		Name:      name,
		OrgID:     orgID,
		TeamID:    teamID,
		CreatedBy: userID,
	})
	if err != nil {
		t.Fatalf("mustCreateServiceAccountHTTP(%q): %v", name, err)
	}
	return sa
}

// saURL returns the base service accounts URL for the given org.
func saURL(orgID string) string {
	return "/api/v1/orgs/" + orgID + "/service-accounts"
}

// saItemURL returns the URL for a specific service account within an org.
func saItemURL(orgID, saID string) string {
	return "/api/v1/orgs/" + orgID + "/service-accounts/" + saID
}

// ---- POST /api/v1/orgs/:org_id/service-accounts -----------------------------

func TestCreateServiceAccount(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool   // key OrgID == target org
		withUserID bool   // populate UserID in key cache entry
		body       any
		wantStatus int
		wantField  string
	}{
		{
			name:       "system_admin creates org-scoped SA",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			withUserID: true,
			body:       map[string]any{"name": "CI Bot"},
			wantStatus: fiber.StatusCreated,
			wantField:  "id",
		},
		{
			name:       "org_admin of same org creates SA",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			withUserID: true,
			body:       map[string]any{"name": "Deploy Bot"},
			wantStatus: fiber.StatusCreated,
			wantField:  "id",
		},
		{
			name:       "missing name returns 400",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			withUserID: true,
			body:       map[string]any{},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			withUserID: true,
			body:       map[string]any{"name": "Cross Bot"},
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "member of same org creates SA",
			role:       auth.RoleMember,
			sameOrg:    true,
			withUserID: true,
			body:       map[string]any{"name": "Member Bot"},
			wantStatus: fiber.StatusCreated,
			wantField:  "id",
		},
		{
			name:       "member of different org returns 403",
			role:       auth.RoleMember,
			sameOrg:    false,
			withUserID: true,
			body:       map[string]any{"name": "Cross Member Bot"},
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "key without UserID returns 400",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			withUserID: false, // simulates a non-user key
			body:       map[string]any{"name": "SA Key Bot"},
			wantStatus: fiber.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateSA_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "SA Test Org", "sa-test-org-"+strings.ReplaceAll(tc.name, " ", "-"))
			userID := mustCreateSAUser(t, database, "creator"+strings.ReplaceAll(tc.name, " ", "")+"@example.com")

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}

			var testKey string
			if tc.withUserID {
				testKey = addTestKeyWithUser(t, keyCache, tc.role, keyOrgID, userID)
			} else {
				testKey = addTestKey(t, keyCache, tc.role, keyOrgID)
			}

			req := httptest.NewRequest("POST", saURL(org.ID), bodyJSON(t, tc.body))
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

func TestCreateServiceAccount_NoAuth(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestCreateSA_NoAuth?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "SA NoAuth Org", "sa-noauth-org")

	req := httptest.NewRequest("POST", saURL(org.ID), bodyJSON(t, map[string]any{"name": "Bot"}))
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

func TestCreateServiceAccount_OrgScoped_TeamIDAbsent(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateSA_OrgScoped?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "OrgScoped SA Org", "orgscoped-sa-org")
	userID := mustCreateSAUser(t, database, "orgscoped@example.com")
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleSystemAdmin, "", userID)

	req := httptest.NewRequest("POST", saURL(org.ID), bodyJSON(t, map[string]any{"name": "Org Bot"}))
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
	if _, ok := got["team_id"]; ok {
		t.Errorf("response has team_id field, want it absent for org-scoped SA; got: %v", got)
	}
}

func TestCreateServiceAccount_TeamScoped_TeamIDPresent(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateSA_TeamScoped?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "TeamScoped SA Org", "teamscoped-sa-org")
	team := mustCreateTeam(t, database, org.ID, "Engineering", "engineering-sa")
	userID := mustCreateSAUser(t, database, "teamscoped@example.com")
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleSystemAdmin, "", userID)

	req := httptest.NewRequest("POST", saURL(org.ID),
		bodyJSON(t, map[string]any{"name": "Team Bot", "team_id": team.ID}))
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
	if got["team_id"] != team.ID {
		t.Errorf("team_id = %v, want %q", got["team_id"], team.ID)
	}
}

func TestCreateServiceAccount_CrossOrgTeam_Returns404(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateSA_CrossOrgTeam?mode=memory&cache=private")
	org1 := mustCreateOrg(t, database, "Org1 SA", "org1-sa-cross")
	org2 := mustCreateOrg(t, database, "Org2 SA", "org2-sa-cross")
	teamInOrg2 := mustCreateTeam(t, database, org2.ID, "Eng Org2", "eng-org2-sa")
	userID := mustCreateSAUser(t, database, "crossorg@example.com")
	testKey := addTestKeyWithUser(t, keyCache, auth.RoleSystemAdmin, "", userID)

	// POST to org1 but with a team that belongs to org2.
	req := httptest.NewRequest("POST", saURL(org1.ID),
		bodyJSON(t, map[string]any{"name": "Cross Bot", "team_id": teamInOrg2.ID}))
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
}

// ---- GET /api/v1/orgs/:org_id/service-accounts/:sa_id -----------------------

func TestGetServiceAccount_HTTP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setupRole  string
		crossOrg   bool // request with different org_id in URL
		notFound   bool // use a non-existent SA ID
		wantStatus int
	}{
		{
			name:       "system_admin gets SA",
			setupRole:  auth.RoleSystemAdmin,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "org_admin gets SA in own org",
			setupRole:  auth.RoleOrgAdmin,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "cross-org SA returns 404",
			setupRole:  auth.RoleSystemAdmin,
			crossOrg:   true,
			wantStatus: fiber.StatusNotFound,
		},
		{
			name:       "non-existent SA returns 404",
			setupRole:  auth.RoleSystemAdmin,
			notFound:   true,
			wantStatus: fiber.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestGetSA_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Get SA Org", "get-sa-http-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "getsa@example.com", "GetSA")
			sa := mustCreateServiceAccountHTTP(t, database, org.ID, user.ID, "Get Bot", nil)

			testKey := addTestKey(t, keyCache, tc.setupRole, org.ID)

			var requestURL string
			switch {
			case tc.crossOrg:
				// Use a different org ID in the URL — the SA belongs to org, not otherOrg.
				otherOrg := mustCreateOrg(t, database, "Other SA Org", "other-sa-http-"+strings.ReplaceAll(tc.name, " ", "-"))
				requestURL = saItemURL(otherOrg.ID, sa.ID)
			case tc.notFound:
				requestURL = saItemURL(org.ID, "00000000-0000-0000-0000-000000000099")
			default:
				requestURL = saItemURL(org.ID, sa.ID)
			}

			req := httptest.NewRequest("GET", requestURL, nil)
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
				if got["id"] != sa.ID {
					t.Errorf("id = %v, want %q", got["id"], sa.ID)
				}
			}
		})
	}
}

// ---- GET /api/v1/orgs/:org_id/service-accounts (list) -----------------------

func TestListServiceAccounts_HTTP(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListSA_HTTP?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "List SA HTTP Org", "list-sa-http-org")
	user := mustCreateUser(t, database, "listsa@example.com", "ListSA")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, org.ID)

	mustCreateServiceAccountHTTP(t, database, org.ID, user.ID, "Bot Alpha", nil)
	mustCreateServiceAccountHTTP(t, database, org.ID, user.ID, "Bot Beta", nil)
	mustCreateServiceAccountHTTP(t, database, org.ID, user.ID, "Bot Gamma", nil)

	req := httptest.NewRequest("GET", saURL(org.ID), nil)
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
	if len(data) != 3 {
		t.Errorf("len(data) = %d, want 3", len(data))
	}
}

// ---- PATCH /api/v1/orgs/:org_id/service-accounts/:sa_id --------------------

func TestUpdateServiceAccount_HTTP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		saID       func(saID string) string
		body       any
		wantStatus int
		checkName  string
	}{
		{
			name:       "system_admin updates name",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			saID:       func(id string) string { return id },
			body:       map[string]any{"name": "Updated Bot"},
			wantStatus: fiber.StatusOK,
			checkName:  "Updated Bot",
		},
		{
			name:       "org_admin updates own org's SA",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			saID:       func(id string) string { return id },
			body:       map[string]any{"name": "OrgAdmin Updated Bot"},
			wantStatus: fiber.StatusOK,
			checkName:  "OrgAdmin Updated Bot",
		},
		{
			name:       "non-existent SA returns 404",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			saID:       func(_ string) string { return "00000000-0000-0000-0000-000000000099" },
			body:       map[string]any{"name": "Ghost Bot"},
			wantStatus: fiber.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestUpdateSA_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Update SA Org", "update-sa-http-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "updatesa@example.com"+"_"+strings.ReplaceAll(tc.name, " ", ""), "UpdateSA")
			sa := mustCreateServiceAccountHTTP(t, database, org.ID, user.ID, "Original Bot", nil)

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			saID := tc.saID(sa.ID)
			req := httptest.NewRequest("PATCH", saItemURL(org.ID, saID), bodyJSON(t, tc.body))
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
					t.Errorf("name = %v, want %q", got["name"], tc.checkName)
				}
			}
		})
	}
}

// ---- DELETE /api/v1/orgs/:org_id/service-accounts/:sa_id -------------------

func TestDeleteServiceAccount_HTTP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool
		saID       func(saID string) string
		wantStatus int
	}{
		{
			name:       "system_admin deletes SA",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			saID:       func(id string) string { return id },
			wantStatus: fiber.StatusNoContent,
		},
		{
			name:       "org_admin deletes SA in own org",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			saID:       func(id string) string { return id },
			wantStatus: fiber.StatusNoContent,
		},
		{
			name:       "non-existent SA returns 404",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			saID:       func(_ string) string { return "00000000-0000-0000-0000-000000000099" },
			wantStatus: fiber.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestDeleteSA_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Delete SA Org", "delete-sa-http-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "deletesa@example.com"+"_"+strings.ReplaceAll(tc.name, " ", ""), "DeleteSA")
			sa := mustCreateServiceAccountHTTP(t, database, org.ID, user.ID, "Delete Bot", nil)

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			saID := tc.saID(sa.ID)
			req := httptest.NewRequest("DELETE", saItemURL(org.ID, saID), nil)
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

func TestDeleteServiceAccount_ThenGetReturns404(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteSA_ThenGet?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Del Then Get SA Org", "del-then-get-sa-org")
	user := mustCreateUser(t, database, "delthenget@example.com", "DelThenGet")
	sa := mustCreateServiceAccountHTTP(t, database, org.ID, user.ID, "Ephemeral Bot", nil)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, org.ID)

	// Delete the SA.
	delReq := httptest.NewRequest("DELETE", saItemURL(org.ID, sa.ID), nil)
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
	getReq := httptest.NewRequest("GET", saItemURL(org.ID, sa.ID), nil)
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
