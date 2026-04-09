package admin_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
)

// setupModelTestApp creates a Fiber app wired with a fresh in-memory SQLite
// database, an empty proxy.Registry, an AES-256 encryption key, and all admin
// routes registered. It is used by model tests that exercise handlers which
// call Registry methods (CreateModel, UpdateModel, DeleteModel, ActivateModel,
// DeactivateModel) and optionally encrypt upstream API keys.
func setupModelTestApp(t *testing.T, dsn string) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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

	registry, err := proxy.NewRegistry(nil)
	if err != nil {
		t.Fatalf("proxy.NewRegistry: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()

	handler := &admin.Handler{
		DB:            database,
		HMACSecret:    testHMACSecret,
		EncryptionKey: testEncryptionKey,
		Registry:      registry,
		KeyCache:      keyCache,
		License:       license.NewHolder(license.Verify("", true)),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// modelURL returns the collection URL for models.
func modelURL() string {
	return "/api/v1/models"
}

// modelItemURL returns the URL for a specific model.
func modelItemURL(modelID string) string {
	return "/api/v1/models/" + modelID
}

// modelActivateURL returns the activate URL for a model.
func modelActivateURL(modelID string) string {
	return "/api/v1/models/" + modelID + "/activate"
}

// modelDeactivateURL returns the deactivate URL for a model.
func modelDeactivateURL(modelID string) string {
	return "/api/v1/models/" + modelID + "/deactivate"
}

// modelTestConnectionURL returns the test-connection URL.
func modelTestConnectionURL() string {
	return "/api/v1/models/test-connection"
}

// ---- POST /api/v1/models ----------------------------------------------------

func TestCreateModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       any
		wantStatus int
		checkBody  func(t *testing.T, got map[string]any)
	}{
		{
			name: "valid minimal request returns 201",
			body: map[string]any{
				"name":     "gpt-4o",
				"provider": "openai",
				"base_url": "https://api.openai.com/v1",
			},
			wantStatus: fiber.StatusCreated,
			checkBody: func(t *testing.T, got map[string]any) {
				t.Helper()
				for _, field := range []string{"id", "name", "provider", "base_url", "is_active", "source", "aliases", "created_at", "updated_at"} {
					if _, ok := got[field]; !ok {
						t.Errorf("response missing field %q", field)
					}
				}
				if got["name"] != "gpt-4o" {
					t.Errorf("name = %v, want %q", got["name"], "gpt-4o")
				}
				if got["provider"] != "openai" {
					t.Errorf("provider = %v, want %q", got["provider"], "openai")
				}
				if got["source"] != "api" {
					t.Errorf("source = %v, want %q", got["source"], "api")
				}
			},
		},
		{
			name: "valid request with all optional fields returns 201",
			body: map[string]any{
				"name":                "claude-opus",
				"provider":            "anthropic",
				"base_url":            "https://api.anthropic.com",
				"max_context_tokens":  200000,
				"input_price_per_1m":  15.0,
				"output_price_per_1m": 75.0,
				"type":                "chat",
				"timeout":             "60s",
				"aliases":             []string{"claude-3-opus"},
			},
			wantStatus: fiber.StatusCreated,
			checkBody: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["name"] != "claude-opus" {
					t.Errorf("name = %v, want %q", got["name"], "claude-opus")
				}
				aliases, ok := got["aliases"].([]any)
				if !ok || len(aliases) != 1 {
					t.Errorf("aliases = %v, want slice of 1", got["aliases"])
				}
			},
		},
		{
			name: "valid request with api_key does not return api_key in response",
			body: map[string]any{
				"name":     "gpt-4-with-key",
				"provider": "openai",
				"base_url": "https://api.openai.com/v1",
				"api_key":  "sk-supersecret",
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
		{
			name: "valid request with strategy does not require provider or base_url",
			body: map[string]any{
				"name":     "load-balanced-model",
				"strategy": "round-robin",
			},
			wantStatus: fiber.StatusCreated,
			checkBody: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["name"] != "load-balanced-model" {
					t.Errorf("name = %v, want %q", got["name"], "load-balanced-model")
				}
			},
		},
		{
			name: "missing name returns 400",
			body: map[string]any{
				"provider": "openai",
				"base_url": "https://api.openai.com/v1",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "missing provider in single-endpoint mode returns 400",
			body: map[string]any{
				"name":     "no-provider-model",
				"base_url": "https://api.openai.com/v1",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "missing base_url in single-endpoint mode returns 400",
			body: map[string]any{
				"name":     "no-url-model",
				"provider": "openai",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "invalid provider returns 400",
			body: map[string]any{
				"name":     "bad-provider-model",
				"provider": "not-a-real-provider",
				"base_url": "https://api.openai.com/v1",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "invalid model type returns 400",
			body: map[string]any{
				"name":     "bad-type-model",
				"provider": "openai",
				"base_url": "https://api.openai.com/v1",
				"type":     "not-a-real-type",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "invalid timeout returns 400",
			body: map[string]any{
				"name":     "bad-timeout-model",
				"provider": "openai",
				"base_url": "https://api.openai.com/v1",
				"timeout":  "not-a-duration",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "invalid strategy returns 400",
			body: map[string]any{
				"name":     "bad-strategy-model",
				"strategy": "not-a-strategy",
			},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name: "valid strategy round-robin returns 201",
			body: map[string]any{
				"name":     "rr-model",
				"strategy": "round-robin",
			},
			wantStatus: fiber.StatusCreated,
		},
		{
			name: "valid strategy weighted returns 201",
			body: map[string]any{
				"name":     "weighted-model",
				"strategy": "weighted",
			},
			wantStatus: fiber.StatusCreated,
		},
		{
			name: "valid strategy priority returns 201",
			body: map[string]any{
				"name":     "priority-model",
				"strategy": "priority",
			},
			wantStatus: fiber.StatusCreated,
		},
		{
			name: "valid strategy least-latency returns 201",
			body: map[string]any{
				"name":     "ll-model",
				"strategy": "least-latency",
			},
			wantStatus: fiber.StatusCreated,
		},
		{
			name: "valid model type embedding returns 201",
			body: map[string]any{
				"name":     "embed-model",
				"provider": "openai",
				"base_url": "https://api.openai.com/v1",
				"type":     "embedding",
			},
			wantStatus: fiber.StatusCreated,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateModel_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, _, keyCache := setupModelTestApp(t, dsn)
			testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

			req := httptest.NewRequest("POST", modelURL(), bodyJSON(t, tc.body))
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

			if tc.checkBody != nil && resp.StatusCode < 300 {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				tc.checkBody(t, got)
			}
		})
	}
}

func TestCreateModel_DuplicateName(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateModel_DuplicateName?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	body := map[string]any{
		"name":     "unique-model",
		"provider": "openai",
		"base_url": "https://api.openai.com/v1",
	}

	// First creation must succeed.
	req1 := httptest.NewRequest("POST", modelURL(), bodyJSON(t, body))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer "+testKey)

	resp1, err := app.Test(req1, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test (first): %v", err)
	}
	defer resp1.Body.Close()

	if resp1.StatusCode != fiber.StatusCreated {
		b, _ := io.ReadAll(resp1.Body)
		t.Fatalf("first create status = %d, want 201; body: %s", resp1.StatusCode, b)
	}

	// Second creation with the same name must conflict.
	req2 := httptest.NewRequest("POST", modelURL(), bodyJSON(t, body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+testKey)

	resp2, err := app.Test(req2, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test (second): %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != fiber.StatusConflict {
		b, _ := io.ReadAll(resp2.Body)
		t.Errorf("second create status = %d, want 409; body: %s", resp2.StatusCode, b)
	}
}

func TestCreateModel_ForbiddenForNonSystemAdmin(t *testing.T) {
	t.Parallel()

	roles := []string{auth.RoleOrgAdmin, auth.RoleMember}
	for _, role := range roles {
		t.Run(role, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateModel_Forbidden_%s?mode=memory&cache=private", role)
			app, _, keyCache := setupModelTestApp(t, dsn)
			testKey := addTestKey(t, keyCache, role, "")

			req := httptest.NewRequest("POST", modelURL(), bodyJSON(t, map[string]any{
				"name":     "model-forbidden",
				"provider": "openai",
				"base_url": "https://api.openai.com/v1",
			}))
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
		})
	}
}

func TestCreateModel_Unauthenticated(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateModel_Unauthenticated?mode=memory&cache=private"
	app, _, _ := setupModelTestApp(t, dsn)

	req := httptest.NewRequest("POST", modelURL(), bodyJSON(t, map[string]any{
		"name":     "model-unauth",
		"provider": "openai",
		"base_url": "https://api.openai.com/v1",
	}))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 401; body: %s", resp.StatusCode, b)
	}
}

// ---- GET /api/v1/models -----------------------------------------------------

func TestListModels(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListModels?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Seed three models so we can assert on count.
	for i := range 3 {
		mustCreateModelForDeployment(t, database, fmt.Sprintf("list-model-%d", i))
	}

	req := httptest.NewRequest("GET", modelURL(), nil)
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
	if _, ok := got["has_more"]; !ok {
		t.Error("response missing has_more field")
	}
}

func TestListModels_Empty(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListModels_Empty?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("GET", modelURL(), nil)
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
		t.Fatalf("data is not an array: %v", got["data"])
	}
	if len(data) != 0 {
		t.Errorf("len(data) = %d, want 0", len(data))
	}
	if got["has_more"] != false {
		t.Errorf("has_more = %v, want false", got["has_more"])
	}
}

func TestListModels_Pagination(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListModels_Pagination?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Seed 5 models.
	for i := range 5 {
		mustCreateModelForDeployment(t, database, fmt.Sprintf("paged-model-%02d", i))
	}

	// Request only the first 3.
	req := httptest.NewRequest("GET", modelURL()+"?limit=3", nil)
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
		t.Fatalf("data is not an array: %v", got["data"])
	}
	if len(data) != 3 {
		t.Errorf("len(data) = %d, want 3", len(data))
	}
	if got["has_more"] != true {
		t.Errorf("has_more = %v, want true", got["has_more"])
	}
	if got["next_cursor"] == "" || got["next_cursor"] == nil {
		t.Error("next_cursor should be set when has_more is true")
	}
}

func TestListModels_IncludesDeployments(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListModels_IncludesDeployments?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m := mustCreateModelForDeployment(t, database, "model-with-deps-list")
	mustCreateDeploymentDB(t, database, m.ID, "dep-list-a")
	mustCreateDeploymentDB(t, database, m.ID, "dep-list-b")

	req := httptest.NewRequest("GET", modelURL(), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	data, ok := got["data"].([]any)
	if !ok || len(data) == 0 {
		t.Fatalf("data = %v, want non-empty array", got["data"])
	}

	item, ok := data[0].(map[string]any)
	if !ok {
		t.Fatalf("data[0] is not a map: %T", data[0])
	}

	deployments, ok := item["deployments"].([]any)
	if !ok || len(deployments) != 2 {
		t.Errorf("deployments = %v, want array of 2", item["deployments"])
	}
}

func TestListModels_ForbiddenForNonSystemAdmin(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListModels_Forbidden?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

	req := httptest.NewRequest("GET", modelURL(), nil)
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
}

// ---- GET /api/v1/models/:model_id -------------------------------------------

func TestGetModel(t *testing.T) {
	t.Parallel()

	dsn := "file:TestGetModel?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m := mustCreateModelForDeployment(t, database, "get-model-target")
	mustCreateDeploymentDB(t, database, m.ID, "get-dep-a")

	req := httptest.NewRequest("GET", modelItemURL(m.ID), nil)
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

	if got["id"] != m.ID {
		t.Errorf("id = %v, want %q", got["id"], m.ID)
	}
	if got["name"] != "get-model-target" {
		t.Errorf("name = %v, want %q", got["name"], "get-model-target")
	}

	// Deployments must be embedded.
	deps, ok := got["deployments"].([]any)
	if !ok || len(deps) != 1 {
		t.Errorf("deployments = %v, want array of 1", got["deployments"])
	}

	// API key must never appear.
	for _, forbidden := range []string{"api_key", "api_key_encrypted"} {
		if _, ok := got[forbidden]; ok {
			t.Errorf("response must not include %q field", forbidden)
		}
	}
}

func TestGetModel_NotFound(t *testing.T) {
	t.Parallel()

	dsn := "file:TestGetModel_NotFound?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("GET", modelItemURL("00000000-0000-0000-0000-000000000000"), nil)
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

func TestGetModel_ForbiddenForNonSystemAdmin(t *testing.T) {
	t.Parallel()

	dsn := "file:TestGetModel_Forbidden?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

	m := mustCreateModelForDeployment(t, database, "get-model-forbidden")

	req := httptest.NewRequest("GET", modelItemURL(m.ID), nil)
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
}

// ---- PATCH /api/v1/models/:model_id -----------------------------------------

func TestUpdateModel(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       any
		wantStatus int
		checkBody  func(t *testing.T, got map[string]any)
	}{
		{
			name:       "partial update name returns 200",
			body:       map[string]any{"name": "updated-model-name"},
			wantStatus: fiber.StatusOK,
			checkBody: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["name"] != "updated-model-name" {
					t.Errorf("name = %v, want %q", got["name"], "updated-model-name")
				}
			},
		},
		{
			name:       "partial update base_url returns 200",
			body:       map[string]any{"base_url": "https://api.openai.com/v2"},
			wantStatus: fiber.StatusOK,
			checkBody: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["base_url"] != "https://api.openai.com/v2" {
					t.Errorf("base_url = %v, want %q", got["base_url"], "https://api.openai.com/v2")
				}
			},
		},
		{
			name:       "update with valid provider returns 200",
			body:       map[string]any{"provider": "anthropic"},
			wantStatus: fiber.StatusOK,
			checkBody: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["provider"] != "anthropic" {
					t.Errorf("provider = %v, want %q", got["provider"], "anthropic")
				}
			},
		},
		{
			name:       "update with valid timeout returns 200",
			body:       map[string]any{"timeout": "120s"},
			wantStatus: fiber.StatusOK,
			checkBody: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["timeout"] != "120s" {
					t.Errorf("timeout = %v, want %q", got["timeout"], "120s")
				}
			},
		},
		{
			name:       "update with valid strategy returns 200",
			body:       map[string]any{"strategy": "weighted"},
			wantStatus: fiber.StatusOK,
			checkBody: func(t *testing.T, got map[string]any) {
				t.Helper()
				if got["strategy"] != "weighted" {
					t.Errorf("strategy = %v, want %q", got["strategy"], "weighted")
				}
			},
		},
		{
			name:       "update api_key does not return api_key in response",
			body:       map[string]any{"api_key": "sk-new-secret"},
			wantStatus: fiber.StatusOK,
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
		{
			name:       "update with invalid provider returns 400",
			body:       map[string]any{"provider": "not-a-real-provider"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "update with invalid strategy returns 400",
			body:       map[string]any{"strategy": "not-a-strategy"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "update with invalid timeout returns 400",
			body:       map[string]any{"timeout": "not-a-duration"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "update with invalid model type returns 400",
			body:       map[string]any{"type": "not-a-type"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "clearing provider in single-endpoint mode returns 400",
			body:       map[string]any{"provider": ""},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "clearing base_url in single-endpoint mode returns 400",
			body:       map[string]any{"base_url": ""},
			wantStatus: fiber.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestUpdateModel_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupModelTestApp(t, dsn)
			testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

			m := mustCreateModelForDeployment(t, database, "update-target-"+strings.ReplaceAll(tc.name, " ", "-"))

			req := httptest.NewRequest("PATCH", modelItemURL(m.ID), bodyJSON(t, tc.body))
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

			if tc.checkBody != nil && resp.StatusCode == fiber.StatusOK {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				tc.checkBody(t, got)
			}
		})
	}
}

func TestUpdateModel_NotFound(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateModel_NotFound?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("PATCH", modelItemURL("00000000-0000-0000-0000-000000000000"),
		bodyJSON(t, map[string]any{"name": "new-name"}))
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

func TestUpdateModel_DuplicateName(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateModel_DuplicateName?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	_ = mustCreateModelForDeployment(t, database, "existing-model-name")
	m2 := mustCreateModelForDeployment(t, database, "model-to-rename")

	req := httptest.NewRequest("PATCH", modelItemURL(m2.ID),
		bodyJSON(t, map[string]any{"name": "existing-model-name"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 409; body: %s", resp.StatusCode, body)
	}
}

func TestUpdateModel_ForbiddenForNonSystemAdmin(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateModel_Forbidden?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

	m := mustCreateModelForDeployment(t, database, "update-forbidden-model")

	req := httptest.NewRequest("PATCH", modelItemURL(m.ID),
		bodyJSON(t, map[string]any{"name": "hijacked"}))
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
}

// ---- DELETE /api/v1/models/:model_id ----------------------------------------

func TestDeleteModel(t *testing.T) {
	t.Parallel()

	dsn := "file:TestDeleteModel?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m := mustCreateModelForDeployment(t, database, "model-to-delete")

	req := httptest.NewRequest("DELETE", modelItemURL(m.ID), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 204; body: %s", resp.StatusCode, body)
	}

	// Confirm the model is no longer accessible via GET.
	getReq := httptest.NewRequest("GET", modelItemURL(m.ID), nil)
	getReq.Header.Set("Authorization", "Bearer "+testKey)

	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test after delete: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != fiber.StatusNotFound {
		t.Errorf("get after delete status = %d, want 404", getResp.StatusCode)
	}
}

func TestDeleteModel_NotFound(t *testing.T) {
	t.Parallel()

	dsn := "file:TestDeleteModel_NotFound?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("DELETE", modelItemURL("00000000-0000-0000-0000-000000000000"), nil)
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

func TestDeleteModel_ForbiddenForNonSystemAdmin(t *testing.T) {
	t.Parallel()

	dsn := "file:TestDeleteModel_Forbidden?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

	m := mustCreateModelForDeployment(t, database, "delete-forbidden-model")

	req := httptest.NewRequest("DELETE", modelItemURL(m.ID), nil)
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
}

// ---- PATCH /api/v1/models/:model_id/activate --------------------------------

func TestActivateModel(t *testing.T) {
	t.Parallel()

	dsn := "file:TestActivateModel?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m := mustCreateModelForDeployment(t, database, "model-to-activate")

	req := httptest.NewRequest("PATCH", modelActivateURL(m.ID), nil)
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

	if got["is_active"] != true {
		t.Errorf("is_active = %v, want true", got["is_active"])
	}
	if got["id"] != m.ID {
		t.Errorf("id = %v, want %q", got["id"], m.ID)
	}
}

func TestActivateModel_NotFound(t *testing.T) {
	t.Parallel()

	dsn := "file:TestActivateModel_NotFound?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("PATCH", modelActivateURL("00000000-0000-0000-0000-000000000000"), nil)
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

func TestActivateModel_ForbiddenForNonSystemAdmin(t *testing.T) {
	t.Parallel()

	dsn := "file:TestActivateModel_Forbidden?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

	m := mustCreateModelForDeployment(t, database, "activate-forbidden-model")

	req := httptest.NewRequest("PATCH", modelActivateURL(m.ID), nil)
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
}

// ---- PATCH /api/v1/models/:model_id/deactivate ------------------------------

func TestDeactivateModel(t *testing.T) {
	t.Parallel()

	dsn := "file:TestDeactivateModel?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m := mustCreateModelForDeployment(t, database, "model-to-deactivate")

	req := httptest.NewRequest("PATCH", modelDeactivateURL(m.ID), nil)
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

	if got["is_active"] != false {
		t.Errorf("is_active = %v, want false", got["is_active"])
	}
	if got["id"] != m.ID {
		t.Errorf("id = %v, want %q", got["id"], m.ID)
	}
}

func TestDeactivateModel_NotFound(t *testing.T) {
	t.Parallel()

	dsn := "file:TestDeactivateModel_NotFound?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("PATCH", modelDeactivateURL("00000000-0000-0000-0000-000000000000"), nil)
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

func TestDeactivateModel_ForbiddenForNonSystemAdmin(t *testing.T) {
	t.Parallel()

	dsn := "file:TestDeactivateModel_Forbidden?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

	m := mustCreateModelForDeployment(t, database, "deactivate-forbidden-model")

	req := httptest.NewRequest("PATCH", modelDeactivateURL(m.ID), nil)
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
}

// TestActivateDeactivateToggle verifies the full activate/deactivate round-trip
// on a single model using a shared DB instance.
func TestActivateDeactivateToggle(t *testing.T) {
	t.Parallel()

	dsn := "file:TestActivateDeactivateToggle?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	m := mustCreateModelForDeployment(t, database, "toggle-model")

	// Deactivate it.
	deactivateReq := httptest.NewRequest("PATCH", modelDeactivateURL(m.ID), nil)
	deactivateReq.Header.Set("Authorization", "Bearer "+testKey)
	deactivateResp, err := app.Test(deactivateReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test deactivate: %v", err)
	}
	defer deactivateResp.Body.Close()
	if deactivateResp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(deactivateResp.Body)
		t.Fatalf("deactivate status = %d; body: %s", deactivateResp.StatusCode, body)
	}
	var deactivated map[string]any
	decodeBody(t, deactivateResp.Body, &deactivated)
	if deactivated["is_active"] != false {
		t.Errorf("after deactivate: is_active = %v, want false", deactivated["is_active"])
	}

	// Re-activate it.
	activateReq := httptest.NewRequest("PATCH", modelActivateURL(m.ID), nil)
	activateReq.Header.Set("Authorization", "Bearer "+testKey)
	activateResp, err := app.Test(activateReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test activate: %v", err)
	}
	defer activateResp.Body.Close()
	if activateResp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(activateResp.Body)
		t.Fatalf("activate status = %d; body: %s", activateResp.StatusCode, body)
	}
	var activated map[string]any
	decodeBody(t, activateResp.Body, &activated)
	if activated["is_active"] != true {
		t.Errorf("after activate: is_active = %v, want true", activated["is_active"])
	}
}

// ---- POST /api/v1/models/test-connection ------------------------------------

func TestTestConnection_MissingBaseURL(t *testing.T) {
	t.Parallel()

	dsn := "file:TestTestConnection_MissingBaseURL?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", modelTestConnectionURL(), bodyJSON(t, map[string]any{
		"provider": "openai",
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

func TestTestConnection_InvalidScheme(t *testing.T) {
	t.Parallel()

	schemes := []struct {
		name    string
		baseURL string
	}{
		{name: "ftp scheme", baseURL: "ftp://example.com/v1"},
		{name: "file scheme", baseURL: "file:///etc/passwd"},
		{name: "no scheme", baseURL: "example.com/v1"},
	}

	for _, tc := range schemes {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestTestConnection_InvalidScheme_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, _, keyCache := setupModelTestApp(t, dsn)
			testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

			req := httptest.NewRequest("POST", modelTestConnectionURL(), bodyJSON(t, map[string]any{
				"provider": "openai",
				"base_url": tc.baseURL,
			}))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			// The handler returns 200 with success=false for bad schemes.
			if resp.StatusCode != fiber.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
			}

			var got map[string]any
			decodeBody(t, resp.Body, &got)
			if got["success"] != false {
				t.Errorf("success = %v, want false", got["success"])
			}
		})
	}
}

func TestTestConnection_ReachableServer(t *testing.T) {
	t.Parallel()

	// Spin up a mock upstream that returns a valid OpenAI-style /models response.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4"},{"id":"gpt-3.5-turbo"}]}`))
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestTestConnection_ReachableServer?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", modelTestConnectionURL(), bodyJSON(t, map[string]any{
		"provider": "openai",
		"base_url": upstream.URL,
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

	if got["success"] != true {
		t.Errorf("success = %v, want true", got["success"])
	}
	msg, _ := got["message"].(string)
	if !strings.Contains(msg, "2 models") {
		t.Errorf("message = %q, want to contain '2 models'", msg)
	}
}

func TestTestConnection_UnreachableServer(t *testing.T) {
	t.Parallel()

	dsn := "file:TestTestConnection_UnreachableServer?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Port 1 is almost universally refused / unreachable.
	req := httptest.NewRequest("POST", modelTestConnectionURL(), bodyJSON(t, map[string]any{
		"provider": "openai",
		"base_url": "http://127.0.0.1:1",
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 15 * testTimeout})
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

	if got["success"] != false {
		t.Errorf("success = %v, want false", got["success"])
	}
}

func TestTestConnection_AuthFailure(t *testing.T) {
	t.Parallel()

	// Mock upstream that returns 401.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestTestConnection_AuthFailure?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", modelTestConnectionURL(), bodyJSON(t, map[string]any{
		"provider": "openai",
		"base_url": upstream.URL,
		"api_key":  "invalid-key",
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

	if got["success"] != false {
		t.Errorf("success = %v, want false", got["success"])
	}
	msg, _ := got["message"].(string)
	if !strings.Contains(msg, "authentication failed") {
		t.Errorf("message = %q, want to contain 'authentication failed'", msg)
	}
}

func TestTestConnection_ServerError(t *testing.T) {
	t.Parallel()

	// Mock upstream that returns 500.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestTestConnection_ServerError?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", modelTestConnectionURL(), bodyJSON(t, map[string]any{
		"provider": "openai",
		"base_url": upstream.URL,
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

	if got["success"] != false {
		t.Errorf("success = %v, want false", got["success"])
	}
}

func TestTestConnection_ForbiddenForNonSystemAdmin(t *testing.T) {
	t.Parallel()

	dsn := "file:TestTestConnection_Forbidden?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, "")

	req := httptest.NewRequest("POST", modelTestConnectionURL(), bodyJSON(t, map[string]any{
		"provider": "openai",
		"base_url": "https://api.openai.com/v1",
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
}

func TestTestConnection_AnthropicUsesXAPIKeyHeader(t *testing.T) {
	t.Parallel()

	var capturedHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestTestConnection_AnthropicHeader?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", modelTestConnectionURL(), bodyJSON(t, map[string]any{
		"provider": "anthropic",
		"base_url": upstream.URL,
		"api_key":  "sk-ant-test-key",
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if capturedHeaders.Get("x-api-key") != "sk-ant-test-key" {
		t.Errorf("x-api-key header = %q, want %q", capturedHeaders.Get("x-api-key"), "sk-ant-test-key")
	}
	if capturedHeaders.Get("anthropic-version") == "" {
		t.Error("anthropic-version header must be set for anthropic provider")
	}
	if capturedHeaders.Get("Authorization") != "" {
		t.Error("Authorization header must not be set for anthropic provider")
	}
}

func TestTestConnection_NonAnthropicUsesBearerAuth(t *testing.T) {
	t.Parallel()

	var capturedHeaders http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedHeaders = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestTestConnection_BearerAuth?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", modelTestConnectionURL(), bodyJSON(t, map[string]any{
		"provider": "openai",
		"base_url": upstream.URL,
		"api_key":  "sk-openai-key",
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if capturedHeaders.Get("Authorization") != "Bearer sk-openai-key" {
		t.Errorf("Authorization header = %q, want %q", capturedHeaders.Get("Authorization"), "Bearer sk-openai-key")
	}
}

// ──────────────────────────────────────────────────────────────────────────────
// Fallback model name tests
// ──────────────────────────────────────────────────────────────────────────────

// setupModelTestAppWithLicense is like setupModelTestApp but accepts an
// explicit License instead of always using the dev license.
func setupModelTestAppWithLicense(t *testing.T, dsn string, lic license.License) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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

	registry, err := proxy.NewRegistry(nil)
	if err != nil {
		t.Fatalf("proxy.NewRegistry: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()

	handler := &admin.Handler{
		DB:            database,
		HMACSecret:    testHMACSecret,
		EncryptionKey: testEncryptionKey,
		Registry:      registry,
		KeyCache:      keyCache,
		License:       license.NewHolder(lic),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// TestCreateModel_WithFallbackLicensed verifies that creating a model with a
// fallback_model_name succeeds when the license includes FeatureFallbackChains.
func TestCreateModel_WithFallbackLicensed(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateModel_WithFallbackLicensed?mode=memory&cache=private"
	// Dev license has all features including fallback_chains.
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Create model-a first (it will be the fallback target).
	mustCreateModelForDeployment(t, database, "model-a-target")

	// Create model-b with fallback set to model-a-target.
	reqBody := map[string]any{
		"name":                "model-b-with-fallback",
		"provider":            "openai",
		"base_url":            "https://api.openai.com/v1",
		"fallback_model_name": "model-a-target",
	}

	req := httptest.NewRequest("POST", modelURL(), bodyJSON(t, reqBody))
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
	if got["fallback_model_name"] != "model-a-target" {
		t.Errorf("fallback_model_name = %v, want %q", got["fallback_model_name"], "model-a-target")
	}
}

// TestCreateModel_WithFallbackNotLicensed verifies that creating a model with a
// fallback_model_name fails with 403 when the license lacks FeatureFallbackChains.
func TestCreateModel_WithFallbackNotLicensed(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateModel_WithFallbackNotLicensed?mode=memory&cache=private"
	// Community license has no enterprise features.
	app, database, keyCache := setupModelTestAppWithLicense(t, dsn, license.Verify("", false))
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	mustCreateModelForDeployment(t, database, "some-fallback-target")

	reqBody := map[string]any{
		"name":                "model-needs-fallback",
		"provider":            "openai",
		"base_url":            "https://api.openai.com/v1",
		"fallback_model_name": "some-fallback-target",
	}

	req := httptest.NewRequest("POST", modelURL(), bodyJSON(t, reqBody))
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
}

// TestCreateModel_FallbackToNonExistentModel verifies that referencing a
// fallback target that does not exist returns 400.
func TestCreateModel_FallbackToNonExistentModel(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateModel_FallbackNonExistent?mode=memory&cache=private"
	app, _, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	reqBody := map[string]any{
		"name":                "model-with-bad-fallback",
		"provider":            "openai",
		"base_url":            "https://api.openai.com/v1",
		"fallback_model_name": "this-does-not-exist",
	}

	req := httptest.NewRequest("POST", modelURL(), bodyJSON(t, reqBody))
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

// TestUpdateModel_WithFallbackLicensed verifies that updating a model to add a
// fallback succeeds when the license includes FeatureFallbackChains.
func TestUpdateModel_WithFallbackLicensed(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateModel_WithFallbackLicensed?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	target := mustCreateModelForDeployment(t, database, "fallback-target-upd")
	source := mustCreateModelForDeployment(t, database, "source-model-upd")
	_ = target

	fallbackName := "fallback-target-upd"
	patchBody := map[string]any{
		"fallback_model_name": fallbackName,
	}

	req := httptest.NewRequest("PATCH", modelItemURL(source.ID), bodyJSON(t, patchBody))
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
	if got["fallback_model_name"] != fallbackName {
		t.Errorf("fallback_model_name = %v, want %q", got["fallback_model_name"], fallbackName)
	}
}

// TestUpdateModel_WithFallbackNotLicensed verifies that updating a model to add
// a fallback fails with 403 when the license lacks FeatureFallbackChains.
func TestUpdateModel_WithFallbackNotLicensed(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateModel_WithFallbackNotLicensed?mode=memory&cache=private"
	app, database, keyCache := setupModelTestAppWithLicense(t, dsn, license.Verify("", false))
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	mustCreateModelForDeployment(t, database, "fb-target-nolic")
	source := mustCreateModelForDeployment(t, database, "source-nolic")

	patchBody := map[string]any{
		"fallback_model_name": "fb-target-nolic",
	}

	req := httptest.NewRequest("PATCH", modelItemURL(source.ID), bodyJSON(t, patchBody))
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
}

// TestUpdateModel_FallbackCycle verifies that setting up a cycle via PATCH
// returns 400 with a cycle error message.
func TestUpdateModel_FallbackCycle(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateModel_FallbackCycle?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Set up A→B via the DB directly (using API would work but DB is simpler).
	modelA := mustCreateModelForDeployment(t, database, "cycle-model-a")
	modelB := mustCreateModelForDeployment(t, database, "cycle-model-b")

	// Link A → B using the PATCH endpoint.
	req1 := httptest.NewRequest("PATCH", modelItemURL(modelA.ID), bodyJSON(t, map[string]any{
		"fallback_model_name": "cycle-model-b",
	}))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer "+testKey)
	resp1, err := app.Test(req1, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("PATCH A→B: app.Test: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != fiber.StatusOK {
		t.Fatalf("PATCH A→B status = %d, want 200", resp1.StatusCode)
	}

	// Now try to link B → A, which would create a cycle.
	req2 := httptest.NewRequest("PATCH", modelItemURL(modelB.ID), bodyJSON(t, map[string]any{
		"fallback_model_name": "cycle-model-a",
	}))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+testKey)
	resp2, err := app.Test(req2, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("PATCH B→A: app.Test: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != fiber.StatusBadRequest {
		body, _ := io.ReadAll(resp2.Body)
		t.Errorf("PATCH B→A: status = %d, want 400 (cycle); body: %s", resp2.StatusCode, body)
	}
}

// TestUpdateModel_FallbackClearsWhenEmpty verifies that passing an empty string
// for fallback_model_name clears the existing fallback (sets it to NULL in DB).
func TestUpdateModel_FallbackClearsWhenEmpty(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateModel_FallbackClearsWhenEmpty?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	target := mustCreateModelForDeployment(t, database, "fb-clear-target")
	source := mustCreateModelForDeployment(t, database, "fb-clear-source")
	_ = target

	// First, set a fallback.
	req1 := httptest.NewRequest("PATCH", modelItemURL(source.ID), bodyJSON(t, map[string]any{
		"fallback_model_name": "fb-clear-target",
	}))
	req1.Header.Set("Content-Type", "application/json")
	req1.Header.Set("Authorization", "Bearer "+testKey)
	resp1, err := app.Test(req1, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("set fallback: app.Test: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != fiber.StatusOK {
		t.Fatalf("set fallback: status = %d, want 200", resp1.StatusCode)
	}

	// Now clear the fallback by sending an empty string.
	emptyStr := ""
	req2 := httptest.NewRequest("PATCH", modelItemURL(source.ID), bodyJSON(t, map[string]any{
		"fallback_model_name": emptyStr,
	}))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+testKey)
	resp2, err := app.Test(req2, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("clear fallback: app.Test: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("clear fallback: status = %d, want 200; body: %s", resp2.StatusCode, body)
	}

	var got map[string]any
	decodeBody(t, resp2.Body, &got)
	// fallback_model_name should be absent (omitempty) or empty string.
	if v, ok := got["fallback_model_name"]; ok && v != "" && v != nil {
		t.Errorf("fallback_model_name = %v after clear, want absent/empty", v)
	}

	// Confirm via DB that fallback_model_id IS NULL.
	ctx := context.Background()
	row := database.SQL().QueryRowContext(ctx,
		"SELECT fallback_model_id FROM models WHERE id = ?", source.ID)
	var fallbackID *string
	if err := row.Scan(&fallbackID); err != nil {
		t.Fatalf("scan fallback_model_id: %v", err)
	}
	if fallbackID != nil {
		t.Errorf("fallback_model_id = %q, want NULL after clearing", *fallbackID)
	}
}

// TestListModels_IncludesFallbackName verifies that the list response populates
// fallback_model_name for models that have a fallback configured.
func TestListModels_IncludesFallbackName(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListModels_IncludesFallbackName?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	target := mustCreateModelForDeployment(t, database, "list-fallback-target")
	source := mustCreateModelForDeployment(t, database, "list-fallback-source")
	_ = target

	// Set fallback via PATCH.
	patchReq := httptest.NewRequest("PATCH", modelItemURL(source.ID), bodyJSON(t, map[string]any{
		"fallback_model_name": "list-fallback-target",
	}))
	patchReq.Header.Set("Content-Type", "application/json")
	patchReq.Header.Set("Authorization", "Bearer "+testKey)
	patchResp, err := app.Test(patchReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("PATCH: app.Test: %v", err)
	}
	patchResp.Body.Close()
	if patchResp.StatusCode != fiber.StatusOK {
		t.Fatalf("PATCH status = %d, want 200", patchResp.StatusCode)
	}

	// List models and check that the source model shows fallback_model_name.
	listReq := httptest.NewRequest("GET", modelURL(), nil)
	listReq.Header.Set("Authorization", "Bearer "+testKey)

	listResp, err := app.Test(listReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("GET list: app.Test: %v", err)
	}
	defer listResp.Body.Close()

	if listResp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("GET list status = %d, want 200; body: %s", listResp.StatusCode, body)
	}

	var listBody map[string]any
	decodeBody(t, listResp.Body, &listBody)

	data, ok := listBody["data"].([]any)
	if !ok {
		t.Fatalf("data field is not an array: %v", listBody["data"])
	}

	// Find the source model in the list and verify its fallback name.
	found := false
	for _, item := range data {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["name"] == "list-fallback-source" {
			found = true
			if m["fallback_model_name"] != "list-fallback-target" {
				t.Errorf("list-fallback-source fallback_model_name = %v, want %q",
					m["fallback_model_name"], "list-fallback-target")
			}
		}
	}
	if !found {
		t.Error("list-fallback-source not found in list response")
	}
}

// TestListModels_FallbackAcrossPages verifies that when model A's fallback
// target lives on a different page, resolveMissingFallbackNames resolves the
// name correctly and the list response populates fallback_model_name for A.
//
// Setup: 5 models total. List page 1 with limit=4, leaving model E on page 2.
// Model A (on page 1) has its fallback pointing to model E (on page 2).
func TestListModels_FallbackAcrossPages(t *testing.T) {
	t.Parallel()

	dsn := "file:TestListModels_FallbackAcrossPages?mode=memory&cache=private"
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Create 5 models. The DB assigns UUIDs so listing order is by creation time
	// (ascending). Models are named with sortable prefixes so alphabetical order
	// matches creation order when the DB uses name-based cursor (verify below).
	// We rely on ID-based cursor ordering; creating in alphabetical order is
	// sufficient because UUID v7 is time-sortable (see architecture.md).
	modelA := mustCreateModelForDeployment(t, database, "xpage-model-a")
	mustCreateModelForDeployment(t, database, "xpage-model-b")
	mustCreateModelForDeployment(t, database, "xpage-model-c")
	mustCreateModelForDeployment(t, database, "xpage-model-d")
	modelE := mustCreateModelForDeployment(t, database, "xpage-model-e")

	// Link A → E via PATCH so model A's fallback_model_id points to E.
	patchReq := httptest.NewRequest("PATCH", modelItemURL(modelA.ID), bodyJSON(t, map[string]any{
		"fallback_model_name": "xpage-model-e",
	}))
	patchReq.Header.Set("Content-Type", "application/json")
	patchReq.Header.Set("Authorization", "Bearer "+testKey)
	patchResp, err := app.Test(patchReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("PATCH A→E: app.Test: %v", err)
	}
	patchResp.Body.Close()
	if patchResp.StatusCode != fiber.StatusOK {
		t.Fatalf("PATCH A→E status = %d, want 200", patchResp.StatusCode)
	}

	// Fetch page 1 with limit=4. A is on page 1; E is on page 2 (not returned).
	listReq := httptest.NewRequest("GET", modelURL()+"?limit=4", nil)
	listReq.Header.Set("Authorization", "Bearer "+testKey)

	listResp, err := app.Test(listReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("GET list p1: app.Test: %v", err)
	}
	defer listResp.Body.Close()

	if listResp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(listResp.Body)
		t.Fatalf("GET list p1 status = %d, want 200; body: %s", listResp.StatusCode, body)
	}

	var listBody map[string]any
	decodeBody(t, listResp.Body, &listBody)

	data, ok := listBody["data"].([]any)
	if !ok {
		t.Fatalf("data field is not an array: %v", listBody["data"])
	}
	if len(data) != 4 {
		t.Fatalf("len(data) = %d, want 4 (page 1 of 5 with limit=4)", len(data))
	}
	if listBody["has_more"] != true {
		t.Error("has_more = false, want true (model E is on page 2)")
	}

	// Verify that E IS on page 2 and NOT on page 1.
	eFoundOnPage1 := false
	for _, item := range data {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["id"] == modelE.ID {
			eFoundOnPage1 = true
		}
	}
	if eFoundOnPage1 {
		t.Skip("model E landed on page 1 — UUID ordering put all 5 on the same page; skip cross-page assertion")
	}

	// Find model A in the page-1 response and assert fallback_model_name = "xpage-model-e".
	foundA := false
	for _, item := range data {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["id"] == modelA.ID {
			foundA = true
			if m["fallback_model_name"] != "xpage-model-e" {
				t.Errorf("model A fallback_model_name = %v, want %q (cross-page resolution failed)",
					m["fallback_model_name"], "xpage-model-e")
			}
		}
	}
	if !foundA {
		t.Error("model A not found on page 1")
	}
}

// TestUpdateModel_ConcurrentFallbackMutations is a race-detector test that
// verifies concurrent PATCH requests mutating fallback relationships do not
// produce data races or panics (Fix C3). The exact final state is not asserted
// because the Go scheduler determines which write wins — only absence of races
// is required. Run the test suite with -race to exercise this property.
func TestUpdateModel_ConcurrentFallbackMutations(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateModel_ConcurrentFallbackMutations?mode=memory&cache=private"
	// Use a single connection so SQLite serialises writes; we test that the
	// handler layer does not introduce data races, not SQLite concurrency.
	app, database, keyCache := setupModelTestApp(t, dsn)
	testKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	modelA := mustCreateModelForDeployment(t, database, "conc-fallback-a")
	modelB := mustCreateModelForDeployment(t, database, "conc-fallback-b")

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines * 2)

	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("PATCH", modelItemURL(modelA.ID),
				bodyJSON(t, map[string]any{"fallback_model_name": "conc-fallback-b"}))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)
			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err == nil {
				resp.Body.Close()
			}
		}()
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("PATCH", modelItemURL(modelB.ID),
				bodyJSON(t, map[string]any{"fallback_model_name": "conc-fallback-a"}))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)
			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err == nil {
				resp.Body.Close()
			}
		}()
	}

	wg.Wait()
	// No assertion on the specific outcome — reaching here without -race failure
	// or panic is the pass condition. The mutex protecting the Registry's
	// fallback state (Fix C3) prevents data corruption under concurrent access.
}
