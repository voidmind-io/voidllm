package admin_test

// deployments_reload_test.go verifies that createDeployment, updateDeployment,
// and deleteDeployment each invoke the ReloadModels callback at least once on
// every successful request path. This guarantees that a pii_filter change on a
// deployment takes effect immediately on the local instance without requiring a
// restart or a Redis pub/sub event.
//
// Pattern mirrors license_test.go: build a Handler with a ReloadModels closure
// that counts invocations, wire it via setupDeploymentReloadApp, make an HTTP
// request through app.Test, assert the counter.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
)

// setupDeploymentReloadApp creates a minimal Fiber app wired with an in-memory
// SQLite database, an EncryptionKey, and a custom ReloadModels callback. The
// callback is injected so tests can count its invocations.
//
// Redis is intentionally nil so only the local ReloadModels path is tested.
func setupDeploymentReloadApp(
	t *testing.T,
	dsn string,
	reloadModels func(context.Context) error,
) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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
		t.Fatalf("setupDeploymentReloadApp: open DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("setupDeploymentReloadApp: run migrations: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()

	handler := &admin.Handler{
		DB:            database,
		HMACSecret:    testHMACSecret,
		EncryptionKey: testEncryptionKey,
		KeyCache:      keyCache,
		License:       license.NewHolder(license.Verify("", true)),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReloadModels:  reloadModels,
		Redis:         nil, // local-only reload path under test
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// TestCreateDeployment_InvokesReloadModels verifies that a successful POST to
// /api/v1/models/:model_id/deployments calls the ReloadModels callback exactly
// once, independently of Redis.
func TestCreateDeployment_InvokesReloadModels(t *testing.T) {
	t.Parallel()

	var reloadCalled atomic.Int32
	reloadModels := func(_ context.Context) error {
		reloadCalled.Add(1)
		return nil
	}

	app, database, keyCache := setupDeploymentReloadApp(
		t,
		"file:TestCreateDeployment_InvokesReloadModels?mode=memory&cache=private",
		reloadModels,
	)
	m := mustCreateModelForDeployment(t, database, "reload-create-model")
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", deploymentURL(m.ID),
		strings.NewReader(`{"name":"reload-dep","provider":"openai","base_url":"https://api.openai.com/v1","weight":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, body)
	}

	if got := reloadCalled.Load(); got < 1 {
		t.Errorf("ReloadModels called %d times after createDeployment, want >= 1", got)
	}
}

// TestUpdateDeployment_InvokesReloadModels verifies that a successful PATCH to
// /api/v1/models/:model_id/deployments/:deployment_id calls the ReloadModels
// callback at least once.
func TestUpdateDeployment_InvokesReloadModels(t *testing.T) {
	t.Parallel()

	var reloadCalled atomic.Int32
	reloadModels := func(_ context.Context) error {
		reloadCalled.Add(1)
		return nil
	}

	app, database, keyCache := setupDeploymentReloadApp(
		t,
		"file:TestUpdateDeployment_InvokesReloadModels?mode=memory&cache=private",
		reloadModels,
	)
	m := mustCreateModelForDeployment(t, database, "reload-update-model")
	dep := mustCreateDeploymentDB(t, database, m.ID, "reload-update-dep")
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("PATCH", deploymentItemURL(m.ID, dep.ID),
		strings.NewReader(`{"name":"reload-updated-dep"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	if got := reloadCalled.Load(); got < 1 {
		t.Errorf("ReloadModels called %d times after updateDeployment, want >= 1", got)
	}
}

// TestDeleteDeployment_InvokesReloadModels verifies that a successful DELETE to
// /api/v1/models/:model_id/deployments/:deployment_id calls the ReloadModels
// callback at least once.
func TestDeleteDeployment_InvokesReloadModels(t *testing.T) {
	t.Parallel()

	var reloadCalled atomic.Int32
	reloadModels := func(_ context.Context) error {
		reloadCalled.Add(1)
		return nil
	}

	app, database, keyCache := setupDeploymentReloadApp(
		t,
		"file:TestDeleteDeployment_InvokesReloadModels?mode=memory&cache=private",
		reloadModels,
	)
	m := mustCreateModelForDeployment(t, database, "reload-delete-model")
	dep := mustCreateDeploymentDB(t, database, m.ID, "reload-delete-dep")
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("DELETE", deploymentItemURL(m.ID, dep.ID), nil)
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 204; body: %s", resp.StatusCode, body)
	}

	if got := reloadCalled.Load(); got < 1 {
		t.Errorf("ReloadModels called %d times after deleteDeployment, want >= 1", got)
	}
}

// TestDeploymentReload_NilCallbackIsNoOp verifies that all three mutating
// deployment handlers succeed when ReloadModels is nil. This ensures that the
// nil-guard in each handler works correctly and no panic occurs.
func TestDeploymentReload_NilCallbackIsNoOp(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupDeploymentReloadApp(
		t,
		"file:TestDeploymentReload_NilCallbackIsNoOp?mode=memory&cache=private",
		nil, // no callback
	)
	m := mustCreateModelForDeployment(t, database, "nil-reload-model")
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Create
	createReq := httptest.NewRequest("POST", deploymentURL(m.ID),
		strings.NewReader(`{"name":"nil-reload-dep","provider":"openai","base_url":"https://api.openai.com/v1","weight":1}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+token)
	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("create: app.Test: %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(createResp.Body)
		t.Fatalf("create: status = %d, want 201; body: %s", createResp.StatusCode, body)
	}
	var created map[string]any
	decodeBody(t, createResp.Body, &created)
	depID, _ := created["id"].(string)

	// Update
	updateReq := httptest.NewRequest("PATCH", deploymentItemURL(m.ID, depID),
		strings.NewReader(`{"name":"nil-reload-dep-updated"}`))
	updateReq.Header.Set("Content-Type", "application/json")
	updateReq.Header.Set("Authorization", "Bearer "+token)
	updateResp, err := app.Test(updateReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("update: app.Test: %v", err)
	}
	defer updateResp.Body.Close()
	if updateResp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(updateResp.Body)
		t.Fatalf("update: status = %d, want 200; body: %s", updateResp.StatusCode, body)
	}

	// Delete
	deleteReq := httptest.NewRequest("DELETE", deploymentItemURL(m.ID, depID), nil)
	deleteReq.Header.Set("Authorization", "Bearer "+token)
	deleteResp, err := app.Test(deleteReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("delete: app.Test: %v", err)
	}
	defer deleteResp.Body.Close()
	if deleteResp.StatusCode != fiber.StatusNoContent {
		body, _ := io.ReadAll(deleteResp.Body)
		t.Fatalf("delete: status = %d, want 204; body: %s", deleteResp.StatusCode, body)
	}
}

// TestDeploymentReload_CallbackErrorDoesNotFailRequest verifies that all three
// mutating deployment handlers return a success response even when the
// ReloadModels callback returns an error. The error is logged but must not
// propagate to the HTTP client.
func TestDeploymentReload_CallbackErrorDoesNotFailRequest(t *testing.T) {
	t.Parallel()

	stubErr := errors.New("stub reload error")
	var reloadCalled atomic.Int32
	reloadModels := func(_ context.Context) error {
		reloadCalled.Add(1)
		return stubErr
	}

	app, database, keyCache := setupDeploymentReloadApp(
		t,
		"file:TestDeploymentReload_CallbackErrorDoesNotFailRequest?mode=memory&cache=private",
		reloadModels,
	)
	m := mustCreateModelForDeployment(t, database, "err-reload-model")
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Create must succeed despite reload error.
	createReq := httptest.NewRequest("POST", deploymentURL(m.ID),
		strings.NewReader(`{"name":"err-reload-dep","provider":"openai","base_url":"https://api.openai.com/v1","weight":1}`))
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("Authorization", "Bearer "+token)
	createResp, err := app.Test(createReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("create: app.Test: %v", err)
	}
	defer createResp.Body.Close()
	if createResp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(createResp.Body)
		t.Fatalf("create: status = %d, want 201 (reload error must not propagate); body: %s", createResp.StatusCode, body)
	}

	var created map[string]any
	decodeBody(t, createResp.Body, &created)
	depID, _ := created["id"].(string)

	// Update must succeed despite reload error.
	updateReq := httptest.NewRequest("PATCH", deploymentItemURL(m.ID, depID),
		strings.NewReader(`{"name":"err-reload-dep-updated"}`))
	updateReq.Header.Set("Content-Type", "application/json")
	updateReq.Header.Set("Authorization", "Bearer "+token)
	updateResp, err := app.Test(updateReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("update: app.Test: %v", err)
	}
	defer updateResp.Body.Close()
	if updateResp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(updateResp.Body)
		t.Fatalf("update: status = %d, want 200 (reload error must not propagate); body: %s", updateResp.StatusCode, body)
	}

	// Delete must succeed despite reload error.
	deleteReq := httptest.NewRequest("DELETE", deploymentItemURL(m.ID, depID), nil)
	deleteReq.Header.Set("Authorization", "Bearer "+token)
	deleteResp, err := app.Test(deleteReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("delete: app.Test: %v", err)
	}
	defer deleteResp.Body.Close()
	if deleteResp.StatusCode != fiber.StatusNoContent {
		body, _ := io.ReadAll(deleteResp.Body)
		t.Fatalf("delete: status = %d, want 204 (reload error must not propagate); body: %s", deleteResp.StatusCode, body)
	}

	// Reload callback must have been called once per mutating operation (3 total).
	if got := reloadCalled.Load(); got != 3 {
		t.Errorf("ReloadModels called %d times across 3 mutating ops, want 3", got)
	}
}

// TestCreateDeployment_PIIFilter_TriggerReload is the integration test that
// most directly proves the pii_filter path: create a deployment with
// pii_filter: true and verify ReloadModels was called. This ensures that the
// pii_filter column change is immediately visible to the local registry.
func TestCreateDeployment_PIIFilter_TriggerReload(t *testing.T) {
	t.Parallel()

	var reloadCalled atomic.Int32
	reloadModels := func(_ context.Context) error {
		reloadCalled.Add(1)
		return nil
	}

	app, database, keyCache := setupDeploymentReloadApp(
		t,
		"file:TestCreateDeployment_PIIFilter_TriggerReload?mode=memory&cache=private",
		reloadModels,
	)
	m := mustCreateModelForDeployment(t, database, "pii-reload-model")
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	req := httptest.NewRequest("POST", deploymentURL(m.ID),
		strings.NewReader(`{"name":"pii-on-dep","provider":"openai","base_url":"https://api.openai.com/v1","weight":1,"pii_filter":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 201; body: %s", resp.StatusCode, body)
	}

	// Verify pii_filter is reflected in the response.
	var got map[string]any
	decodeBody(t, resp.Body, &got)
	if got["pii_filter"] != true {
		t.Errorf("response pii_filter = %v, want true", got["pii_filter"])
	}

	// ReloadModels must have been called at least once.
	if got := reloadCalled.Load(); got < 1 {
		t.Errorf("ReloadModels called %d times after pii_filter create, want >= 1", got)
	}
}

// TestUpdateDeployment_PIIFilter_TriggerReload verifies that updating
// pii_filter via PATCH calls ReloadModels, ensuring local registry refresh.
func TestUpdateDeployment_PIIFilter_TriggerReload(t *testing.T) {
	t.Parallel()

	var reloadCalled atomic.Int32
	reloadModels := func(_ context.Context) error {
		reloadCalled.Add(1)
		return nil
	}

	app, database, keyCache := setupDeploymentReloadApp(
		t,
		"file:TestUpdateDeployment_PIIFilter_TriggerReload?mode=memory&cache=private",
		reloadModels,
	)
	m := mustCreateModelForDeployment(t, database, "pii-update-reload-model")
	dep := mustCreateDeploymentDB(t, database, m.ID, "pii-update-reload-dep")
	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")

	// Reset the counter: createDeploymentDB goes through the DB directly, not
	// the HTTP handler, so reloadCalled should still be 0 here.
	startCount := reloadCalled.Load()

	req := httptest.NewRequest("PATCH", deploymentItemURL(m.ID, dep.ID),
		strings.NewReader(`{"pii_filter":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	after := reloadCalled.Load()
	if after <= startCount {
		t.Errorf("ReloadModels not called after pii_filter PATCH (before=%d, after=%d)", startCount, after)
	}
}
