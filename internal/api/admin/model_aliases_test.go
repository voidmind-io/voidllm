package admin_test

import (
	"context"
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
	"github.com/voidmind-io/voidllm/internal/proxy"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// testModelName is the canonical model name registered in the test registry.
const testModelName = "gpt-4o"

// testConflictModelName is a second model used to test alias-vs-name conflicts.
const testConflictModelName = "claude-3-5-sonnet"

// setupTestAppWithRegistry creates a Fiber app with a populated model registry
// suitable for testing model alias and model access endpoints.
func setupTestAppWithRegistry(t *testing.T, dsn string) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	registry, err := proxy.NewRegistry([]config.ModelConfig{
		{
			Name:     testModelName,
			Provider: "openai",
			BaseURL:  "https://api.openai.com/v1",
		},
		{
			Name:     testConflictModelName,
			Provider: "anthropic",
			BaseURL:  "https://api.anthropic.com",
		},
	})
	if err != nil {
		t.Fatalf("proxy.NewRegistry: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()

	handler := &admin.Handler{
		DB:         database,
		HMACSecret: testHMACSecret,
		KeyCache:   keyCache,
		Registry:   registry,
		License:    license.NewHolder(license.Verify("", true)),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// addRegistryTestKey generates a key whose ID field is set to the given userID,
// which satisfies the NOT NULL REFERENCES users(id) constraint on model_aliases.created_by.
// It should be used whenever the handler writes keyInfo.ID to a DB column referencing users.
func addRegistryTestKey(t *testing.T, keyCache *cache.Cache[string, auth.KeyInfo], role, orgID, userID string) string {
	t.Helper()

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("addRegistryTestKey: generate: %v", err)
	}

	hash := keygen.Hash(plaintext, testHMACSecret)
	keyCache.Set(hash, auth.KeyInfo{
		ID:      userID, // handler passes keyInfo.ID as created_by
		KeyType: keygen.KeyTypeUser,
		Role:    role,
		OrgID:   orgID,
		UserID:  userID,
		Name:    "test key " + role,
	})

	return plaintext
}

// orgAliasURL returns the org model-aliases base URL.
func orgAliasURL(orgID string) string {
	return "/api/v1/orgs/" + orgID + "/model-aliases"
}

// orgAliasItemURL returns the URL for a specific org model alias.
func orgAliasItemURL(orgID, aliasID string) string {
	return "/api/v1/orgs/" + orgID + "/model-aliases/" + aliasID
}

// teamAliasURL returns the team model-aliases base URL.
func teamAliasURL(orgID, teamID string) string {
	return "/api/v1/orgs/" + orgID + "/teams/" + teamID + "/model-aliases"
}

// teamAliasItemURL returns the URL for a specific team model alias.
func teamAliasItemURL(orgID, teamID, aliasID string) string {
	return "/api/v1/orgs/" + orgID + "/teams/" + teamID + "/model-aliases/" + aliasID
}

// ---- POST /api/v1/orgs/:org_id/model-aliases --------------------------------

func TestCreateOrgAlias(t *testing.T) {
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
			name:       "org_admin creates org alias",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"alias": "my-model", "model_name": testModelName},
			wantStatus: fiber.StatusCreated,
			checkField: "id",
		},
		{
			name:       "system_admin creates org alias",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			body:       map[string]any{"alias": "sys-alias", "model_name": testModelName},
			wantStatus: fiber.StatusCreated,
			checkField: "id",
		},
		{
			name:       "unknown model returns 400",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"alias": "bad-alias", "model_name": "does-not-exist"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "empty alias returns 400",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"alias": "", "model_name": testModelName},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "alias with spaces returns 400",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"alias": "has spaces", "model_name": testModelName},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "missing model_name returns 400",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"alias": "valid-alias"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "alias conflicts with existing model name returns 400",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"alias": testConflictModelName, "model_name": testModelName},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			body:       map[string]any{"alias": "cross-org-alias", "model_name": testModelName},
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "member of same org returns 403 from route middleware",
			role:       auth.RoleMember,
			sameOrg:    true,
			body:       map[string]any{"alias": "member-alias", "model_name": testModelName},
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateOrgAlias_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestAppWithRegistry(t, dsn)

			org := mustCreateOrg(t, database, "Alias Org", "alias-org-coa-"+strings.ReplaceAll(tc.name, " ", "-"))
			creator := mustCreateUser(t, database, "creator-coa-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com", "Creator")

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addRegistryTestKey(t, keyCache, tc.role, keyOrgID, creator.ID)

			req := httptest.NewRequest("POST", orgAliasURL(org.ID), bodyJSON(t, tc.body))
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
				if _, ok := got[tc.checkField]; !ok {
					t.Errorf("response missing field %q; got: %v", tc.checkField, got)
				}
			}
		})
	}
}

func TestCreateOrgAlias_ResponseFields(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestCreateOrgAlias_Fields?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Fields Org", "fields-org-coa")
	creator := mustCreateUser(t, database, "creator-fields-coa@example.com", "Creator")
	testKey := addRegistryTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	req := httptest.NewRequest("POST", orgAliasURL(org.ID),
		bodyJSON(t, map[string]any{"alias": "fields-alias", "model_name": testModelName}))
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

	for _, field := range []string{"id", "alias", "model_name", "scope_type", "org_id", "created_by", "created_at"} {
		if _, ok := got[field]; !ok {
			t.Errorf("response missing field %q; got: %v", field, got)
		}
	}
	if got["alias"] != "fields-alias" {
		t.Errorf("alias = %q, want %q", got["alias"], "fields-alias")
	}
	if got["model_name"] != testModelName {
		t.Errorf("model_name = %q, want %q", got["model_name"], testModelName)
	}
	if got["scope_type"] != "org" {
		t.Errorf("scope_type = %q, want %q", got["scope_type"], "org")
	}
	if got["org_id"] != org.ID {
		t.Errorf("org_id = %q, want %q", got["org_id"], org.ID)
	}
}

// ---- GET /api/v1/orgs/:org_id/model-aliases ---------------------------------

func TestListOrgAliases(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestListOrgAliases?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "List Alias Org", "list-alias-org")
	creator := mustCreateUser(t, database, "creator-loa@example.com", "Creator")
	testKey := addRegistryTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	// Create two aliases via API.
	for _, alias := range []string{"alias-a", "alias-b"} {
		req := httptest.NewRequest("POST", orgAliasURL(org.ID),
			bodyJSON(t, map[string]any{"alias": alias, "model_name": testModelName}))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+testKey)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("create alias %q: %v", alias, err)
		}
		resp.Body.Close()
		if resp.StatusCode != fiber.StatusCreated {
			t.Fatalf("create alias %q: status = %d, want 201", alias, resp.StatusCode)
		}
	}

	req := httptest.NewRequest("GET", orgAliasURL(org.ID), nil)
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

	var got []any
	decodeBody(t, resp.Body, &got)

	if len(got) != 2 {
		t.Errorf("len(aliases) = %d, want 2", len(got))
	}
}

func TestListOrgAliases_Empty(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestListOrgAliases_Empty?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Empty Alias Org", "empty-alias-org")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("GET", orgAliasURL(org.ID), nil)
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

	var got []any
	decodeBody(t, resp.Body, &got)

	if len(got) != 0 {
		t.Errorf("len(aliases) = %d, want 0", len(got))
	}
}

// ---- DELETE /api/v1/orgs/:org_id/model-aliases/:alias_id --------------------

func TestDeleteOrgAlias(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestDeleteOrgAlias?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Delete Alias Org", "delete-alias-org")
	creator := mustCreateUser(t, database, "creator-doa@example.com", "Creator")
	testKey := addRegistryTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	// Create alias.
	createReq := httptest.NewRequest("POST", orgAliasURL(org.ID),
		bodyJSON(t, map[string]any{"alias": "to-delete-alias", "model_name": testModelName}))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+testKey)
	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("create alias: %v", err)
	}
	if createResp.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(createResp.Body)
		createResp.Body.Close()
		t.Fatalf("create alias: status = %d; body: %s", createResp.StatusCode, b)
	}
	var created map[string]any
	decodeBody(t, createResp.Body, &created)
	aliasID := created["id"].(string)

	// Delete it.
	delReq := httptest.NewRequest("DELETE", orgAliasItemURL(org.ID, aliasID), nil)
	delReq.Header.Set("Authorization", "Bearer "+testKey)
	delResp, err := app.Test(delReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("delete alias: %v", err)
	}
	defer delResp.Body.Close()

	if delResp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(delResp.Body)
		t.Errorf("status = %d, want 204; body: %s", delResp.StatusCode, b)
	}
}

func TestDeleteOrgAlias_NotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestDeleteOrgAlias_NotFound?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "NF Alias Org", "nf-alias-org")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("DELETE", orgAliasItemURL(org.ID, "00000000-0000-0000-0000-000000000099"), nil)
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

func TestDeleteOrgAlias_ThenListShowsRemoved(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestDeleteOrgAlias_ThenList?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Gone Alias Org", "gone-alias-org")
	creator := mustCreateUser(t, database, "creator-doatl@example.com", "Creator")
	testKey := addRegistryTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	// Create and immediately delete.
	createReq := httptest.NewRequest("POST", orgAliasURL(org.ID),
		bodyJSON(t, map[string]any{"alias": "gone-alias", "model_name": testModelName}))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+testKey)
	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created map[string]any
	decodeBody(t, createResp.Body, &created)
	aliasID := created["id"].(string)

	delReq := httptest.NewRequest("DELETE", orgAliasItemURL(org.ID, aliasID), nil)
	delReq.Header.Set("Authorization", "Bearer "+testKey)
	delResp, err := app.Test(delReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	delResp.Body.Close()

	// List should now be empty.
	listReq := httptest.NewRequest("GET", orgAliasURL(org.ID), nil)
	listReq.Header.Set("Authorization", "Bearer "+testKey)
	listResp, err := app.Test(listReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	defer listResp.Body.Close()

	var got []any
	decodeBody(t, listResp.Body, &got)
	if len(got) != 0 {
		t.Errorf("len(aliases) = %d after delete, want 0", len(got))
	}
}

func TestCreateOrgAlias_DuplicateReturnsConflict(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestCreateOrgAlias_Dup?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Dup Alias Org", "dup-alias-org")
	creator := mustCreateUser(t, database, "creator-dup-coa@example.com", "Creator")
	testKey := addRegistryTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	body := bodyJSON(t, map[string]any{"alias": "dup-alias", "model_name": testModelName})
	req1 := httptest.NewRequest("POST", orgAliasURL(org.ID), body)
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer "+testKey)
	resp1, err := app.Test(req1, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != fiber.StatusCreated {
		t.Fatalf("first create: status = %d, want 201", resp1.StatusCode)
	}

	req2 := httptest.NewRequest("POST", orgAliasURL(org.ID),
		bodyJSON(t, map[string]any{"alias": "dup-alias", "model_name": testModelName}))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+testKey)
	resp2, err := app.Test(req2, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != fiber.StatusConflict {
		b, _ := io.ReadAll(resp2.Body)
		t.Errorf("status = %d, want 409; body: %s", resp2.StatusCode, b)
	}
}

// ---- POST /api/v1/orgs/:org_id/teams/:team_id/model-aliases -----------------

func TestCreateTeamAlias(t *testing.T) {
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
			name:       "org_admin creates team alias",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"alias": "team-model", "model_name": testModelName},
			wantStatus: fiber.StatusCreated,
			checkField: "id",
		},
		{
			name:       "system_admin creates team alias",
			role:       auth.RoleSystemAdmin,
			sameOrg:    false,
			body:       map[string]any{"alias": "sys-team-alias", "model_name": testModelName},
			wantStatus: fiber.StatusCreated,
			checkField: "id",
		},
		{
			name:       "unknown model returns 400",
			role:       auth.RoleOrgAdmin,
			sameOrg:    true,
			body:       map[string]any{"alias": "bad-team-alias", "model_name": "ghost-model"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "org_admin of different org returns 403",
			role:       auth.RoleOrgAdmin,
			sameOrg:    false,
			body:       map[string]any{"alias": "cross-team-alias", "model_name": testModelName},
			wantStatus: fiber.StatusForbidden,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateTeamAlias_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestAppWithRegistry(t, dsn)

			org := mustCreateOrg(t, database, "Team Alias Org", "team-alias-org-"+strings.ReplaceAll(tc.name, " ", "-"))
			team := mustCreateTeam(t, database, org.ID, "Alias Team", "alias-team-cta-"+strings.ReplaceAll(tc.name, " ", "-"))
			creator := mustCreateUser(t, database, "creator-cta-"+strings.ReplaceAll(tc.name, " ", "-")+"@example.com", "Creator")

			keyOrgID := "00000000-0000-0000-0000-000000000000"
			if tc.sameOrg {
				keyOrgID = org.ID
			}
			testKey := addRegistryTestKey(t, keyCache, tc.role, keyOrgID, creator.ID)

			req := httptest.NewRequest("POST", teamAliasURL(org.ID, team.ID), bodyJSON(t, tc.body))
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
				if _, ok := got[tc.checkField]; !ok {
					t.Errorf("response missing field %q; got: %v", tc.checkField, got)
				}
			}
		})
	}
}

func TestCreateTeamAlias_ResponseFields(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestCreateTeamAlias_Fields?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Team Fields Org", "team-fields-org")
	team := mustCreateTeam(t, database, org.ID, "Fields Team", "fields-team-cta")
	creator := mustCreateUser(t, database, "creator-fields-cta@example.com", "Creator")
	testKey := addRegistryTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	req := httptest.NewRequest("POST", teamAliasURL(org.ID, team.ID),
		bodyJSON(t, map[string]any{"alias": "team-fields-alias", "model_name": testModelName}))
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

	for _, field := range []string{"id", "alias", "model_name", "scope_type", "org_id", "team_id", "created_at"} {
		if _, ok := got[field]; !ok {
			t.Errorf("response missing field %q; got: %v", field, got)
		}
	}
	if got["scope_type"] != "team" {
		t.Errorf("scope_type = %q, want %q", got["scope_type"], "team")
	}
	if got["team_id"] != team.ID {
		t.Errorf("team_id = %q, want %q", got["team_id"], team.ID)
	}
}

// ---- GET /api/v1/orgs/:org_id/teams/:team_id/model-aliases ------------------

func TestListTeamAliases(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestListTeamAliases?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "List Team Alias Org", "list-team-alias-org")
	team := mustCreateTeam(t, database, org.ID, "List Alias Team", "list-alias-team")
	creator := mustCreateUser(t, database, "creator-lta@example.com", "Creator")
	testKey := addRegistryTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	// Create one team alias.
	createReq := httptest.NewRequest("POST", teamAliasURL(org.ID, team.ID),
		bodyJSON(t, map[string]any{"alias": "list-team-alias", "model_name": testModelName}))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+testKey)
	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	createResp.Body.Close()
	if createResp.StatusCode != fiber.StatusCreated {
		t.Fatalf("create: status = %d, want 201", createResp.StatusCode)
	}

	req := httptest.NewRequest("GET", teamAliasURL(org.ID, team.ID), nil)
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

	var got []any
	decodeBody(t, resp.Body, &got)

	if len(got) != 1 {
		t.Errorf("len(aliases) = %d, want 1", len(got))
	}
}

func TestListTeamAliases_WrongOrg(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestListTeamAliases_WrongOrg?mode=memory&cache=private")
	orgA := mustCreateOrg(t, database, "Org A", "org-a-lta")
	orgB := mustCreateOrg(t, database, "Org B", "org-b-lta")
	teamB := mustCreateTeam(t, database, orgB.ID, "Team B", "team-b-lta")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Request teamB's aliases via orgA URL — must be 404.
	req := httptest.NewRequest("GET", teamAliasURL(orgA.ID, teamB.ID), nil)
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

// ---- DELETE /api/v1/orgs/:org_id/teams/:team_id/model-aliases/:alias_id -----

func TestDeleteTeamAlias(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestDeleteTeamAlias?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Del Team Alias Org", "del-team-alias-org")
	team := mustCreateTeam(t, database, org.ID, "Del Alias Team", "del-alias-team")
	creator := mustCreateUser(t, database, "creator-dta@example.com", "Creator")
	testKey := addRegistryTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	// Create alias.
	createReq := httptest.NewRequest("POST", teamAliasURL(org.ID, team.ID),
		bodyJSON(t, map[string]any{"alias": "team-alias-del", "model_name": testModelName}))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+testKey)
	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	var created map[string]any
	decodeBody(t, createResp.Body, &created)
	aliasID := created["id"].(string)

	// Delete it.
	delReq := httptest.NewRequest("DELETE", teamAliasItemURL(org.ID, team.ID, aliasID), nil)
	delReq.Header.Set("Authorization", "Bearer "+testKey)
	delResp, err := app.Test(delReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	defer delResp.Body.Close()

	if delResp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(delResp.Body)
		t.Errorf("status = %d, want 204; body: %s", delResp.StatusCode, b)
	}
}

func TestDeleteTeamAlias_NotFound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestDeleteTeamAlias_NotFound?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "NF Del Team Alias Org", "nf-del-team-alias-org")
	team := mustCreateTeam(t, database, org.ID, "NF Team", "nf-team-dta")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("DELETE", teamAliasItemURL(org.ID, team.ID, "00000000-0000-0000-0000-000000000099"), nil)
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

func TestCreateTeamAlias_DuplicateReturnsConflict(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestAppWithRegistry(t, "file:TestCreateTeamAlias_Dup?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Dup Team Alias Org", "dup-team-alias-org")
	team := mustCreateTeam(t, database, org.ID, "Dup Team", "dup-alias-team")
	creator := mustCreateUser(t, database, "creator-dup-cta@example.com", "Creator")
	testKey := addRegistryTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID, creator.ID)

	createBody := map[string]any{"alias": "dup-team-alias", "model_name": testModelName}

	req1 := httptest.NewRequest("POST", teamAliasURL(org.ID, team.ID), bodyJSON(t, createBody))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer "+testKey)
	resp1, err := app.Test(req1, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != fiber.StatusCreated {
		t.Fatalf("first create: status = %d, want 201", resp1.StatusCode)
	}

	req2 := httptest.NewRequest("POST", teamAliasURL(org.ID, team.ID),
		bodyJSON(t, createBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+testKey)
	resp2, err := app.Test(req2, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != fiber.StatusConflict {
		b, _ := io.ReadAll(resp2.Body)
		t.Errorf("status = %d, want 409; body: %s", resp2.StatusCode, b)
	}
}
