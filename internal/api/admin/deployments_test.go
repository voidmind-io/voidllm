package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"log/slog"
	"time"
)

// testEncryptionKey is a fixed 32-byte AES-256 key used by all deployment tests
// that exercise the API key encryption path.
var testEncryptionKey = func() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}()

// setupTestAppWithEncKey is like setupTestApp but also sets the EncryptionKey
// field on the Handler so that deployment API key encryption works correctly.
func setupTestAppWithEncKey(t *testing.T, dsn string) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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
		DB:            database,
		HMACSecret:    testHMACSecret,
		EncryptionKey: testEncryptionKey,
		KeyCache:      keyCache,
		License:       license.NewHolder(license.Verify("", true)),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// mustCreateModelForDeployment creates a model directly via the DB for use as a
// deployment parent in test setup.
func mustCreateModelForDeployment(t *testing.T, database *db.DB, name string) *db.Model {
	t.Helper()
	m, err := database.CreateModel(context.Background(), db.CreateModelParams{
		Name:     name,
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		Source:   "api",
	})
	if err != nil {
		t.Fatalf("mustCreateModelForDeployment(%q): %v", name, err)
	}
	return m
}

// mustCreateDeploymentDB creates a deployment directly via the DB for test setup.
func mustCreateDeploymentDB(t *testing.T, database *db.DB, modelID, name string) *db.Deployment {
	t.Helper()
	dep, err := database.CreateDeployment(context.Background(), db.CreateDeploymentParams{
		ModelID:  modelID,
		Name:     name,
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		Weight:   1,
	})
	if err != nil {
		t.Fatalf("mustCreateDeploymentDB(%q): %v", name, err)
	}
	return dep
}

// deploymentURL returns the list/create URL for deployments under a model.
func deploymentURL(modelID string) string {
	return "/api/v1/models/" + modelID + "/deployments"
}

// deploymentItemURL returns the URL for a specific deployment under a model.
func deploymentItemURL(modelID, deploymentID string) string {
	return "/api/v1/models/" + modelID + "/deployments/" + deploymentID
}

// ---- POST /api/v1/models/:model_id/deployments ------------------------------

func TestCreateDeployment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       any
		wantStatus int
		checkBody  func(t *testing.T, got map[string]any)
	}{
		{
			name: "system admin creates deployment returns 201 with fields",
			body: map[string]any{
				"name":     "primary",
				"provider": "openai",
				"base_url": "https://api.openai.com/v1",
				"weight":   2,
				"priority": 0,
			},
			wantStatus: fiber.StatusCreated,
			checkBody: func(t *testing.T, got map[string]any) {
				t.Helper()
				for _, field := range []string{"id", "model_id", "name", "provider", "base_url", "weight", "priority", "is_active", "created_at", "updated_at"} {
					if _, ok := got[field]; !ok {
						t.Errorf("response missing field %q", field)
					}
				}
				if got["name"] != "primary" {
					t.Errorf("name = %v, want %q", got["name"], "primary")
				}
				if got["provider"] != "openai" {
					t.Errorf("provider = %v, want %q", got["provider"], "openai")
				}
				if got["base_url"] != "https://api.openai.com/v1" {
					t.Errorf("base_url = %v, want %q", got["base_url"], "https://api.openai.com/v1")
				}
			},
		},
		{
			name: "create deployment with api key does not return api key",
			body: map[string]any{
				"name":     "with-key",
				"provider": "openai",
				"base_url": "https://api.openai.com/v1",
				"api_key":  "sk-supersecret",
				"weight":   1,
			},
			wantStatus: fiber.StatusCreated,
			checkBody: func(t *testing.T, got map[string]any) {
				t.Helper()
				if _, ok := got["api_key"]; ok {
					t.Error("response must not include api_key field")
				}
				if _, ok := got["api_key_encrypted"]; ok {
					t.Error("response must not include api_key_encrypted field")
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateDeployment_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestAppWithEncKey(t, dsn)

			m := mustCreateModelForDeployment(t, database, "gpt-4-create-dep-"+strings.ReplaceAll(tc.name, " ", "-"))
			testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

			req := httptest.NewRequest("POST", deploymentURL(m.ID), bodyJSON(t, tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, body)
			}

			if tc.checkBody != nil {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				tc.checkBody(t, got)
			}
		})
	}
}

func TestCreateDeployment_ValidatesProvider(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateDeployment_ValidatesProvider?mode=memory&cache=private"
	app, database, keyCache := setupTestAppWithEncKey(t, dsn)

	m := mustCreateModelForDeployment(t, database, "gpt-4-val-provider")
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", deploymentURL(m.ID), bodyJSON(t, map[string]any{
		"name":     "bad-provider",
		"provider": "not-a-real-provider",
		"base_url": "https://api.openai.com/v1",
		"weight":   1,
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
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, fiber.StatusBadRequest, body)
	}
}

func TestCreateDeployment_ValidatesBaseURL(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		baseURL string
	}{
		{
			name:    "empty base_url returns 400",
			baseURL: "",
		},
		{
			name:    "non-http base_url returns 400",
			baseURL: "ftp://example.com/v1",
		},
		{
			name:    "relative url returns 400",
			baseURL: "/v1/completions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateDeployment_BaseURL_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestAppWithEncKey(t, dsn)

			m := mustCreateModelForDeployment(t, database, "gpt-4-val-url-"+strings.ReplaceAll(tc.name, " ", "-"))
			testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

			body := map[string]any{
				"name":     "url-test-dep",
				"provider": "openai",
				"base_url": tc.baseURL,
				"weight":   1,
			}

			req := httptest.NewRequest("POST", deploymentURL(m.ID), bodyJSON(t, body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != fiber.StatusBadRequest {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, fiber.StatusBadRequest, b)
			}
		})
	}
}

func TestCreateDeployment_UnknownModel(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateDeployment_UnknownModel?mode=memory&cache=private"
	app, _, keyCache := setupTestAppWithEncKey(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", deploymentURL("00000000-0000-0000-0000-000000000000"),
		bodyJSON(t, map[string]any{
			"name":     "dep",
			"provider": "openai",
			"base_url": "https://api.openai.com/v1",
			"weight":   1,
		}))
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

func TestCreateDeployment_ForbiddenForNonSystemAdmin(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateDeployment_Forbidden?mode=memory&cache=private"
	app, database, keyCache := setupTestAppWithEncKey(t, dsn)

	m := mustCreateModelForDeployment(t, database, "gpt-4-forbidden-dep")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

	req := httptest.NewRequest("POST", deploymentURL(m.ID), bodyJSON(t, map[string]any{
		"name":     "dep",
		"provider": "openai",
		"base_url": "https://api.openai.com/v1",
		"weight":   1,
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

// ---- GET /api/v1/models/:model_id/deployments -------------------------------

func TestListDeployments(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListDeployments?mode=memory&cache=private"
	app, database, keyCache := setupTestAppWithEncKey(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m := mustCreateModelForDeployment(t, database, "gpt-4-list-http")
	mustCreateDeploymentDB(t, database, m.ID, "dep-alpha")
	mustCreateDeploymentDB(t, database, m.ID, "dep-beta")

	req := httptest.NewRequest("GET", deploymentURL(m.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, fiber.StatusOK, body)
	}

	var got []map[string]any
	decodeBody(t, resp.Body, &got)

	if len(got) != 2 {
		t.Errorf("response len = %d, want 2", len(got))
	}

	// Each item must have an id and model_id but no api_key or api_key_encrypted.
	for i, item := range got {
		if _, ok := item["id"]; !ok {
			t.Errorf("item[%d] missing field %q", i, "id")
		}
		if _, ok := item["model_id"]; !ok {
			t.Errorf("item[%d] missing field %q", i, "model_id")
		}
		if _, ok := item["api_key"]; ok {
			t.Errorf("item[%d] must not include api_key field", i)
		}
		if _, ok := item["api_key_encrypted"]; ok {
			t.Errorf("item[%d] must not include api_key_encrypted field", i)
		}
	}
}

func TestListDeployments_UnknownModel(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListDeployments_UnknownModel?mode=memory&cache=private"
	app, _, keyCache := setupTestAppWithEncKey(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("GET", deploymentURL("00000000-0000-0000-0000-000000000000"), nil)
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

// ---- PATCH /api/v1/models/:model_id/deployments/:deployment_id --------------

func TestUpdateDeployment(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateDeployment?mode=memory&cache=private"
	app, database, keyCache := setupTestAppWithEncKey(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m := mustCreateModelForDeployment(t, database, "gpt-4-update-http")
	dep := mustCreateDeploymentDB(t, database, m.ID, "original-name-dep")

	newName := "updated-name-dep"
	req := httptest.NewRequest("PATCH", deploymentItemURL(m.ID, dep.ID),
		bodyJSON(t, map[string]any{"name": newName}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, fiber.StatusOK, body)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	if got["name"] != newName {
		t.Errorf("name = %v, want %q", got["name"], newName)
	}
	// api_key must never appear in response.
	if _, ok := got["api_key"]; ok {
		t.Error("response must not include api_key field")
	}
}

func TestUpdateDeployment_ValidatesProvider(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateDeployment_ValidatesProvider?mode=memory&cache=private"
	app, database, keyCache := setupTestAppWithEncKey(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m := mustCreateModelForDeployment(t, database, "gpt-4-upd-val-provider")
	dep := mustCreateDeploymentDB(t, database, m.ID, "upd-val-dep")

	req := httptest.NewRequest("PATCH", deploymentItemURL(m.ID, dep.ID),
		bodyJSON(t, map[string]any{"provider": "invalid-provider"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, fiber.StatusBadRequest, body)
	}
}

func TestUpdateDeployment_WrongModel(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateDeployment_WrongModel?mode=memory&cache=private"
	app, database, keyCache := setupTestAppWithEncKey(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Create two models; the deployment belongs to modelA but we PATCH under modelB.
	modelA := mustCreateModelForDeployment(t, database, "gpt-4-wrong-model-a")
	modelB := mustCreateModelForDeployment(t, database, "gpt-4-wrong-model-b")
	dep := mustCreateDeploymentDB(t, database, modelA.ID, "idor-dep")

	req := httptest.NewRequest("PATCH", deploymentItemURL(modelB.ID, dep.ID),
		bodyJSON(t, map[string]any{"name": "hijacked"}))
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

// ---- DELETE /api/v1/models/:model_id/deployments/:deployment_id -------------

func TestDeleteDeployment(t *testing.T) {
	t.Parallel()

	dsn := "file:TestDeleteDeployment?mode=memory&cache=private"
	app, database, keyCache := setupTestAppWithEncKey(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m := mustCreateModelForDeployment(t, database, "gpt-4-delete-http")
	dep := mustCreateDeploymentDB(t, database, m.ID, "dep-to-delete")

	req := httptest.NewRequest("DELETE", deploymentItemURL(m.ID, dep.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, fiber.StatusNoContent, body)
	}

	// Confirm the deployment is no longer accessible.
	getReq := httptest.NewRequest("GET", deploymentURL(m.ID), nil)
	getReq.Header.Set("Authorization", "Bearer "+testKey)
	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test after delete: %v", err)
	}
	defer getResp.Body.Close()

	var list []map[string]any
	decodeBody(t, getResp.Body, &list)
	if len(list) != 0 {
		t.Errorf("ListDeployments after delete returned %d items, want 0", len(list))
	}
}

func TestDeleteDeployment_WrongModel(t *testing.T) {
	t.Parallel()

	dsn := "file:TestDeleteDeployment_WrongModel?mode=memory&cache=private"
	app, database, keyCache := setupTestAppWithEncKey(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Deployment belongs to modelA; try to delete it via modelB's URL.
	modelA := mustCreateModelForDeployment(t, database, "gpt-4-del-wrong-a")
	modelB := mustCreateModelForDeployment(t, database, "gpt-4-del-wrong-b")
	dep := mustCreateDeploymentDB(t, database, modelA.ID, "idor-del-dep")

	req := httptest.NewRequest("DELETE", deploymentItemURL(modelB.ID, dep.ID), nil)
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

func TestDeleteDeployment_NotFound(t *testing.T) {
	t.Parallel()

	dsn := "file:TestDeleteDeployment_NotFound?mode=memory&cache=private"
	app, database, keyCache := setupTestAppWithEncKey(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m := mustCreateModelForDeployment(t, database, "gpt-4-del-notfound")

	req := httptest.NewRequest("DELETE", deploymentItemURL(m.ID, "00000000-0000-0000-0000-000000000000"), nil)
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

// ---- API key never in response (negative assertion) -------------------------

func TestDeploymentAPIKeyNotInResponse(t *testing.T) {
	t.Parallel()

	dsn := "file:TestDeploymentAPIKeyNotInResponse?mode=memory&cache=private"
	app, database, keyCache := setupTestAppWithEncKey(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m := mustCreateModelForDeployment(t, database, "gpt-4-apikey-check")

	// Create with an API key.
	createReq := httptest.NewRequest("POST", deploymentURL(m.ID), bodyJSON(t, map[string]any{
		"name":     "api-key-dep",
		"provider": "openai",
		"base_url": "https://api.openai.com/v1",
		"api_key":  "sk-very-secret-upstream-key",
		"weight":   1,
	}))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+testKey)

	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test create: %v", err)
	}
	defer createResp.Body.Close()

	if createResp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(createResp.Body)
		t.Fatalf("create status = %d; body: %s", createResp.StatusCode, body)
	}

	var created map[string]any
	decodeBody(t, createResp.Body, &created)

	// Neither the plaintext key nor the encrypted field should appear.
	for _, forbidden := range []string{"api_key", "api_key_encrypted"} {
		if _, ok := created[forbidden]; ok {
			t.Errorf("create response must not include field %q", forbidden)
		}
	}

	depID, ok := created["id"].(string)
	if !ok || depID == "" {
		t.Fatalf("created deployment has no string id")
	}

	// List also must not expose the key.
	listReq := httptest.NewRequest("GET", deploymentURL(m.ID), nil)
	listReq.Header.Set("Authorization", "Bearer "+testKey)
	listResp, err := app.Test(listReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test list: %v", err)
	}
	defer listResp.Body.Close()

	var listed []map[string]any
	decodeBody(t, listResp.Body, &listed)

	for i, item := range listed {
		for _, forbidden := range []string{"api_key", "api_key_encrypted"} {
			if _, ok := item[forbidden]; ok {
				t.Errorf("list item[%d] must not include field %q", i, forbidden)
			}
		}
	}

	// Update with a new key — response must also not expose it.
	updateReq := httptest.NewRequest("PATCH", deploymentItemURL(m.ID, depID),
		bodyJSON(t, map[string]any{"api_key": "sk-another-secret"}))
	updateReq.Header.Set("Content-Type", "application/json")
	updateReq.Header.Set("Authorization", "Bearer "+testKey)

	updateResp, err := app.Test(updateReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test update: %v", err)
	}
	defer updateResp.Body.Close()

	var updated map[string]any
	decodeBody(t, updateResp.Body, &updated)

	for _, forbidden := range []string{"api_key", "api_key_encrypted"} {
		if _, ok := updated[forbidden]; ok {
			t.Errorf("update response must not include field %q", forbidden)
		}
	}
}
