package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// testTimeout is the per-request timeout used in all app.Test calls.
const testTimeout = 5 * time.Second

// testHMACSecret is the HMAC secret used for all test key hashing.
var testHMACSecret = []byte("test-hmac-secret-for-admin-api-tests")

// setupTestApp creates a Fiber app wired with a fresh in-memory SQLite database
// and the admin routes registered. The DSN must be unique per test to ensure
// test isolation with SQLite's named in-memory databases.
func setupTestApp(t *testing.T, dsn string) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
	t.Helper()

	ctx := context.Background()
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()

	handler := &admin.Handler{
		DB:         database,
		HMACSecret: testHMACSecret,
		KeyCache:   keyCache,
		License:    license.NewHolder(license.Verify("", true)),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// addTestKey generates a random API key of the given role and org, stores it in
// the cache under its HMAC hash, and returns the plaintext key for use in
// Authorization headers.
func addTestKey(t *testing.T, keyCache *cache.Cache[string, auth.KeyInfo], role string, orgID string) string {
	t.Helper()

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}

	hash := keygen.Hash(plaintext, testHMACSecret)
	keyCache.Set(hash, auth.KeyInfo{
		ID:      "test-key-id-" + role,
		KeyType: keygen.KeyTypeUser,
		Role:    role,
		OrgID:   orgID,
		Name:    "test key " + role,
	})

	return plaintext
}

// mustCreateOrg calls the DB directly to create an org for test setup,
// fataling the test on error.
func mustCreateOrg(t *testing.T, database *db.DB, name, slug string) *db.Org {
	t.Helper()
	org, err := database.CreateOrg(context.Background(), db.CreateOrgParams{
		Name: name,
		Slug: slug,
	})
	if err != nil {
		t.Fatalf("mustCreateOrg(%q, %q): %v", name, slug, err)
	}
	return org
}

// bodyJSON marshals v to a JSON strings.Reader for use as a request body.
func bodyJSON(t *testing.T, v any) io.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal request body: %v", err)
	}
	return strings.NewReader(string(b))
}

// decodeBody decodes the response body into v, fataling on error.
func decodeBody(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

// ---- POST /api/v1/orgs -------------------------------------------------------

func TestCreateOrg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		body       any
		wantStatus int
		wantField  string // non-empty: check JSON field exists in response
	}{
		{
			name:       "system_admin creates org",
			role:       auth.RoleSystemAdmin,
			body:       map[string]any{"name": "Acme Corp", "slug": "acme"},
			wantStatus: fiber.StatusCreated,
			wantField:  "id",
		},
		{
			name:       "missing name returns 400",
			role:       auth.RoleSystemAdmin,
			body:       map[string]any{"slug": "acme-no-name"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "missing slug returns 400",
			role:       auth.RoleSystemAdmin,
			body:       map[string]any{"name": "No Slug Corp"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "member role returns 403",
			role:       auth.RoleMember,
			body:       map[string]any{"name": "Member Org", "slug": "member-org"},
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "org_admin role returns 403",
			role:       auth.RoleOrgAdmin,
			body:       map[string]any{"name": "OrgAdmin Org", "slug": "orgadmin-org"},
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateOrg_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, _, keyCache := setupTestApp(t, dsn)
			testKey := addTestKey(t, keyCache, tc.role, "")

			req := httptest.NewRequest("POST", "/api/v1/orgs", bodyJSON(t, tc.body))
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

func TestCreateOrg_NoAuthHeader(t *testing.T) {
	t.Parallel()

	app, _, _ := setupTestApp(t, "file:TestCreateOrg_NoAuth?mode=memory&cache=private")

	req := httptest.NewRequest("POST", "/api/v1/orgs", bodyJSON(t, map[string]any{"name": "Test", "slug": "test"}))
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

func TestCreateOrg_DuplicateSlug(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestCreateOrg_DupSlug?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")
	mustCreateOrg(t, database, "Existing Org", "duplicate-slug")

	req := httptest.NewRequest("POST", "/api/v1/orgs",
		bodyJSON(t, map[string]any{"name": "Another Org", "slug": "duplicate-slug"}))
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

func TestCreateOrg_ResponseFields(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestCreateOrg_Fields?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", "/api/v1/orgs",
		bodyJSON(t, map[string]any{"name": "Fields Test Corp", "slug": "fields-test"}))
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

	for _, field := range []string{"id", "name", "slug", "created_at", "updated_at"} {
		if _, ok := got[field]; !ok {
			t.Errorf("response missing field %q; got: %v", field, got)
		}
	}
	if got["name"] != "Fields Test Corp" {
		t.Errorf("name = %q, want %q", got["name"], "Fields Test Corp")
	}
	if got["slug"] != "fields-test" {
		t.Errorf("slug = %q, want %q", got["slug"], "fields-test")
	}
}

// ---- GET /api/v1/orgs/:org_id ------------------------------------------------

func TestGetOrg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		setupRole  string
		setupOrgID func(orgID string) string // returns the org_id to use in the request
		wantStatus int
	}{
		{
			name:      "system_admin gets any org",
			setupRole: auth.RoleSystemAdmin,
			setupOrgID: func(orgID string) string {
				return orgID
			},
			wantStatus: fiber.StatusOK,
		},
		{
			name:      "org_admin gets own org",
			setupRole: auth.RoleOrgAdmin,
			setupOrgID: func(orgID string) string {
				return orgID // key OrgID == requested org
			},
			wantStatus: fiber.StatusOK,
		},
		{
			name:      "org_admin gets different org returns 403",
			setupRole: auth.RoleOrgAdmin,
			setupOrgID: func(_ string) string {
				return "00000000-0000-0000-0000-000000000001" // different org
			},
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:      "non-existent org returns 404",
			setupRole: auth.RoleSystemAdmin,
			setupOrgID: func(_ string) string {
				return "00000000-0000-0000-0000-000000000099"
			},
			wantStatus: fiber.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestGetOrg_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Test Org", "test-get-org-"+strings.ReplaceAll(tc.name, " ", "-"))
			testKey := addTestKey(t, keyCache, tc.setupRole, org.ID)
			requestedOrgID := tc.setupOrgID(org.ID)

			req := httptest.NewRequest("GET", "/api/v1/orgs/"+requestedOrgID, nil)
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

			if tc.wantStatus == fiber.StatusOK {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				if got["id"] != org.ID {
					t.Errorf("id = %q, want %q", got["id"], org.ID)
				}
			}
		})
	}
}

// ---- GET /api/v1/orgs --------------------------------------------------------

func TestListOrgs_SystemAdmin(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListOrgs_SystemAdmin?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	mustCreateOrg(t, database, "Alpha Org", "alpha")
	mustCreateOrg(t, database, "Beta Org", "beta")
	mustCreateOrg(t, database, "Gamma Org", "gamma")

	req := httptest.NewRequest("GET", "/api/v1/orgs", nil)
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
		t.Fatalf("response data field is not an array: %v", got["data"])
	}
	if len(data) != 3 {
		t.Errorf("len(data) = %d, want 3", len(data))
	}
}

func TestListOrgs_OrgAdmin_OnlyOwnOrg(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListOrgs_OrgAdmin?mode=memory&cache=private")

	ownOrg := mustCreateOrg(t, database, "My Org", "my-org")
	mustCreateOrg(t, database, "Other Org", "other-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, ownOrg.ID)

	req := httptest.NewRequest("GET", "/api/v1/orgs", nil)
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
		t.Fatalf("response data field is not an array: %v", got["data"])
	}
	if len(data) != 1 {
		t.Errorf("len(data) = %d, want 1 (only own org)", len(data))
		return
	}
	entry := data[0].(map[string]any)
	if entry["id"] != ownOrg.ID {
		t.Errorf("data[0].id = %q, want %q", entry["id"], ownOrg.ID)
	}
}

func TestListOrgs_Pagination(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListOrgs_Pagination?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	mustCreateOrg(t, database, "Pag Org 1", "pag-1")
	mustCreateOrg(t, database, "Pag Org 2", "pag-2")
	mustCreateOrg(t, database, "Pag Org 3", "pag-3")

	// First page: limit=2
	req := httptest.NewRequest("GET", "/api/v1/orgs?limit=2", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test first page: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("first page status = %d, want 200; body: %s", resp.StatusCode, body)
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
		t.Errorf("page1 has_more = false, want true")
	}
	cursor, _ := page1["next_cursor"].(string)
	if cursor == "" {
		t.Error("page1 next_cursor is empty, want a cursor value")
	}

	// Second page: use cursor from first page
	req2 := httptest.NewRequest("GET", "/api/v1/orgs?limit=2&cursor="+cursor, nil)
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

func TestListOrgs_PaginationLimitClamping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		query     string
		wantCount int
	}{
		{
			name:      "limit zero is treated as default 20",
			query:     "?limit=0",
			wantCount: 2, // only 2 orgs exist
		},
		{
			name:      "limit negative is treated as default 20",
			query:     "?limit=-5",
			wantCount: 2,
		},
		{
			name:      "limit above 100 is clamped to 100",
			query:     "?limit=999",
			wantCount: 2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestListOrgs_Clamp_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)
			testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

			mustCreateOrg(t, database, "Clamp Org 1", "clamp-1-"+strings.ReplaceAll(tc.name, " ", "-"))
			mustCreateOrg(t, database, "Clamp Org 2", "clamp-2-"+strings.ReplaceAll(tc.name, " ", "-"))

			req := httptest.NewRequest("GET", "/api/v1/orgs"+tc.query, nil)
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
			if len(data) != tc.wantCount {
				t.Errorf("len(data) = %d, want %d", len(data), tc.wantCount)
			}
		})
	}
}

func TestListOrgs_IncludeDeleted_SystemAdmin(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListOrgs_IncludeDeleted?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	org := mustCreateOrg(t, database, "Active Org", "active-org")
	toDelete := mustCreateOrg(t, database, "Deleted Org", "deleted-org")

	// Soft-delete the second org directly via DB.
	if err := database.DeleteOrg(context.Background(), toDelete.ID); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}

	// Without include_deleted: only the active org is returned.
	req := httptest.NewRequest("GET", "/api/v1/orgs", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	var got map[string]any
	decodeBody(t, resp.Body, &got)
	data, _ := got["data"].([]any)
	if len(data) != 1 {
		t.Errorf("without include_deleted len(data) = %d, want 1", len(data))
	}
	if len(data) == 1 {
		entry := data[0].(map[string]any)
		if entry["id"] != org.ID {
			t.Errorf("data[0].id = %q, want active org %q", entry["id"], org.ID)
		}
	}

	// With include_deleted=true: both orgs are returned.
	req2 := httptest.NewRequest("GET", "/api/v1/orgs?include_deleted=true", nil)
	req2.Header.Set("Authorization", "Bearer "+testKey)

	resp2, err := app.Test(req2, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test include_deleted: %v", err)
	}
	defer resp2.Body.Close()

	var got2 map[string]any
	decodeBody(t, resp2.Body, &got2)
	data2, _ := got2["data"].([]any)
	if len(data2) != 2 {
		t.Errorf("with include_deleted len(data) = %d, want 2", len(data2))
	}
}

func TestListOrgs_IncludeDeleted_NonAdmin_Ignored(t *testing.T) {
	t.Parallel()

	// include_deleted=true should be silently ignored for non-system-admin callers.
	app, database, keyCache := setupTestApp(t, "file:TestListOrgs_IncludeDeletedNonAdmin?mode=memory&cache=private")

	ownOrg := mustCreateOrg(t, database, "My Visible Org", "visible-org")
	toDelete := mustCreateOrg(t, database, "Hidden Org", "hidden-org")

	if err := database.DeleteOrg(context.Background(), toDelete.ID); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}

	// org_admin key scoped to ownOrg.
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, ownOrg.ID)

	req := httptest.NewRequest("GET", "/api/v1/orgs?include_deleted=true", nil)
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
	data, _ := got["data"].([]any)
	// org_admin always gets exactly their own org, regardless of include_deleted.
	if len(data) != 1 {
		t.Errorf("len(data) = %d, want 1 (org_admin sees only own org)", len(data))
	}
}

// ---- PATCH /api/v1/orgs/:org_id ---------------------------------------------

func TestUpdateOrg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		sameOrg    bool // key OrgID == target org
		body       any
		wantStatus int
		checkName  string
	}{
		{
			name:       "system_admin updates name",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			body:       map[string]any{"name": "Updated Corp"},
			wantStatus: fiber.StatusOK,
			checkName:  "Updated Corp",
		},
		{
			name:       "org_admin updates own org",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"name": "OrgAdmin Updated"},
			wantStatus: fiber.StatusOK,
			checkName:  "OrgAdmin Updated",
		},
		{
			name:       "member returns 403",
			role:       auth.RoleMember,
			sameOrg:    true,
			body:       map[string]any{"name": "Should Fail"},
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "org_admin on different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			body:       map[string]any{"name": "Cross Org Update"},
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestUpdateOrg_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "Original Name", "update-test-"+strings.ReplaceAll(tc.name, " ", "-"))

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addTestKey(t, keyCache, tc.role, keyOrgID)

			req := httptest.NewRequest("PATCH", "/api/v1/orgs/"+org.ID, bodyJSON(t, tc.body))
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

func TestUpdateOrg_NotFound(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestUpdateOrg_NotFound?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("PATCH", "/api/v1/orgs/00000000-0000-0000-0000-000000000099",
		bodyJSON(t, map[string]any{"name": "Ghost Org"}))
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

func TestUpdateOrg_SlugConflict(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateOrg_SlugConflict?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	mustCreateOrg(t, database, "Taken Org", "taken-slug")
	org := mustCreateOrg(t, database, "Target Org", "target-slug")

	newSlug := "taken-slug"
	req := httptest.NewRequest("PATCH", "/api/v1/orgs/"+org.ID,
		bodyJSON(t, map[string]any{"slug": newSlug}))
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

// ---- DELETE /api/v1/orgs/:org_id ---------------------------------------------

func TestDeleteOrg(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		wantStatus int
	}{
		{
			name:       "system_admin deletes org",
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

			dsn := fmt.Sprintf("file:TestDeleteOrg_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "To Delete", "to-delete-"+strings.ReplaceAll(tc.name, " ", "-"))
			testKey := addTestKey(t, keyCache, tc.role, org.ID)

			req := httptest.NewRequest("DELETE", "/api/v1/orgs/"+org.ID, nil)
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

func TestDeleteOrg_NotFound(t *testing.T) {
	t.Parallel()

	app, _, keyCache := setupTestApp(t, "file:TestDeleteOrg_NotFound?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("DELETE", "/api/v1/orgs/00000000-0000-0000-0000-000000000099", nil)
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

func TestDeleteOrg_ThenGetReturns404(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteOrg_ThenGet?mode=memory&cache=private")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	org := mustCreateOrg(t, database, "Ephemeral Org", "ephemeral-org")

	// Delete the org.
	delReq := httptest.NewRequest("DELETE", "/api/v1/orgs/"+org.ID, nil)
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
	getReq := httptest.NewRequest("GET", "/api/v1/orgs/"+org.ID, nil)
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
