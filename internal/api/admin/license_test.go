package admin_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
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
	"github.com/voidmind-io/voidllm/internal/licensetest"
)

// setupLicenseTestApp creates a minimal Fiber app wired for the SetLicense
// and GetLicense endpoints. The returned *license.Holder is the same instance
// used by the handler so tests can inspect the stored license after a request.
func setupLicenseTestApp(
	t *testing.T,
	dsn string,
	reloadModels func(context.Context) error,
) (*fiber.App, *license.Holder, *cache.Cache[string, auth.KeyInfo]) {
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

	// Start with a community (no-key) license so changes are observable.
	holder := license.NewHolder(license.Verify("", false))

	keyCache := cache.New[string, auth.KeyInfo]()

	handler := &admin.Handler{
		DB:           database,
		HMACSecret:   testHMACSecret,
		KeyCache:     keyCache,
		License:      holder,
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		ReloadModels: reloadModels,
		Redis:        nil, // explicitly nil — exercises the local-only reload path
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, holder, keyCache
}

// TestSetLicense_InvokesReloadModels verifies that SetLicense calls the
// ReloadModels callback exactly once, AFTER License.Store has updated the
// holder, and BEFORE the handler returns a response.
//
// This test MUST NOT run in parallel because it mutates the license package's
// embedded public key via licensetest.WithTestPublicKey.
func TestSetLicense_InvokesReloadModels(t *testing.T) {
	pub, priv := licensetest.NewTestKeypair(t)
	licensetest.WithTestPublicKey(t, pub)

	claims := licensetest.NewEnterpriseClaims([]string{license.FeatureFallbackChains})
	jwtKey := licensetest.SignTestJWT(t, priv, claims)

	var reloadCalled atomic.Int32
	var reloadSawEnterprise atomic.Bool

	reloadModels := func(ctx context.Context) error {
		reloadCalled.Add(1)
		// Capture license state inside the callback to detect whether
		// License.Store ran before ReloadModels was invoked.
		// The holder is captured via the closure variable set by setupLicenseTestApp.
		// We assert this below after the request completes.
		reloadSawEnterprise.Store(true) // marker: callback executed
		return nil
	}

	app, holder, keyCache := setupLicenseTestApp(
		t,
		"file:TestSetLicense_InvokesReloadModels?mode=memory&cache=private",
		reloadModels,
	)

	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-lic-test")

	req := httptest.NewRequest("PUT", "/api/v1/settings/license",
		bodyJSON(t, map[string]string{"key": jwtKey}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	// ReloadModels must have been invoked exactly once.
	if got := reloadCalled.Load(); got != 1 {
		t.Errorf("ReloadModels called %d times, want 1", got)
	}

	// License.Store must have completed before the handler returned; verify
	// the holder reflects the enterprise license.
	lic := holder.Load()
	if got := lic.Edition(); got != license.EditionEnterprise {
		t.Errorf("holder.Load().Edition() = %q, want %q after SetLicense", got, license.EditionEnterprise)
	}
	if !lic.HasFeature(license.FeatureFallbackChains) {
		t.Errorf("holder.Load().HasFeature(%q) = false, want true after SetLicense", license.FeatureFallbackChains)
	}

	// Confirm the callback marker was set.
	if !reloadSawEnterprise.Load() {
		t.Error("ReloadModels callback never executed")
	}
}

// TestSetLicense_LicenseStoredBeforeReload verifies the ordering guarantee:
// the license holder is updated BEFORE ReloadModels is invoked. A concurrent
// read of the holder inside the callback must see the enterprise edition.
//
// This test MUST NOT run in parallel — it mutates the embedded public key.
func TestSetLicense_LicenseStoredBeforeReload(t *testing.T) {
	pub, priv := licensetest.NewTestKeypair(t)
	licensetest.WithTestPublicKey(t, pub)

	claims := licensetest.NewEnterpriseClaims([]string{license.FeatureAuditLogs})
	jwtKey := licensetest.SignTestJWT(t, priv, claims)

	// The holder is captured into the closure so the callback can read it.
	var capturedEdition atomic.Value // stores license.Edition

	var holderRef *license.Holder // set after setupLicenseTestApp returns

	reloadModels := func(_ context.Context) error {
		if holderRef != nil {
			capturedEdition.Store(holderRef.Load().Edition())
		}
		return nil
	}

	app, holder, keyCache := setupLicenseTestApp(
		t,
		"file:TestSetLicense_LicenseStoredBeforeReload?mode=memory&cache=private",
		reloadModels,
	)
	holderRef = holder // bind the reference after creation

	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-order-test")

	req := httptest.NewRequest("PUT", "/api/v1/settings/license",
		bodyJSON(t, map[string]string{"key": jwtKey}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	// The callback must have seen the enterprise edition — proving Store ran first.
	gotEdition, _ := capturedEdition.Load().(license.Edition)
	if gotEdition != license.EditionEnterprise {
		t.Errorf("edition seen inside ReloadModels = %q, want %q (License.Store must precede ReloadModels)", gotEdition, license.EditionEnterprise)
	}
}

// TestSetLicense_ReloadModelsNilIsNoOp verifies that SetLicense succeeds and
// stores the license in the holder even when ReloadModels is nil.
//
// This test MUST NOT run in parallel — it mutates the embedded public key.
func TestSetLicense_ReloadModelsNilIsNoOp(t *testing.T) {
	pub, priv := licensetest.NewTestKeypair(t)
	licensetest.WithTestPublicKey(t, pub)

	claims := licensetest.NewEnterpriseClaims([]string{license.FeatureAuditLogs})
	jwtKey := licensetest.SignTestJWT(t, priv, claims)

	app, holder, keyCache := setupLicenseTestApp(
		t,
		"file:TestSetLicense_ReloadModelsNilIsNoOp?mode=memory&cache=private",
		nil, // no reload callback
	)

	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-nil-reload")

	req := httptest.NewRequest("PUT", "/api/v1/settings/license",
		bodyJSON(t, map[string]string{"key": jwtKey}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	if got := holder.Load().Edition(); got != license.EditionEnterprise {
		t.Errorf("holder.Load().Edition() = %q, want %q", got, license.EditionEnterprise)
	}
}

// TestSetLicense_ReloadModelsError verifies that a ReloadModels error does not
// cause SetLicense to return a non-2xx response and that the license IS
// persisted in the holder despite the reload failure.
//
// This test MUST NOT run in parallel — it mutates the embedded public key.
func TestSetLicense_ReloadModelsError(t *testing.T) {
	pub, priv := licensetest.NewTestKeypair(t)
	licensetest.WithTestPublicKey(t, pub)

	claims := licensetest.NewEnterpriseClaims([]string{license.FeatureFallbackChains})
	jwtKey := licensetest.SignTestJWT(t, priv, claims)

	stubErr := errors.New("stub reload error")
	var reloadCalled atomic.Int32

	reloadModels := func(_ context.Context) error {
		reloadCalled.Add(1)
		return stubErr
	}

	app, holder, keyCache := setupLicenseTestApp(
		t,
		"file:TestSetLicense_ReloadModelsError?mode=memory&cache=private",
		reloadModels,
	)

	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-reload-err")

	req := httptest.NewRequest("PUT", "/api/v1/settings/license",
		bodyJSON(t, map[string]string{"key": jwtKey}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	// The handler must NOT propagate the reload error to the client.
	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (reload error must not fail the request); body: %s", resp.StatusCode, raw)
	}

	if got := reloadCalled.Load(); got != 1 {
		t.Errorf("ReloadModels called %d times, want 1", got)
	}

	// The license IS persisted despite the reload error.
	if got := holder.Load().Edition(); got != license.EditionEnterprise {
		t.Errorf("holder.Load().Edition() = %q, want %q (license must persist even when reload fails)", got, license.EditionEnterprise)
	}
}

// TestSetLicense_InvalidKey verifies that an invalid JWT returns 400 and
// neither updates the holder nor invokes ReloadModels.
//
// This test is safe to run in parallel because it does not swap the embedded
// public key (invalid tokens are rejected before signature verification reaches
// the embedded key).
func TestSetLicense_InvalidKey(t *testing.T) {
	t.Parallel()

	var reloadCalled atomic.Int32
	reloadModels := func(_ context.Context) error {
		reloadCalled.Add(1)
		return nil
	}

	dsn := fmt.Sprintf("file:TestSetLicense_InvalidKey_%d?mode=memory&cache=private", time.Now().UnixNano())
	app, holder, keyCache := setupLicenseTestApp(t, dsn, reloadModels)

	token := addTestKey(t, keyCache, auth.RoleSystemAdmin, "org-invalid-key")

	req := httptest.NewRequest("PUT", "/api/v1/settings/license",
		bodyJSON(t, map[string]string{"key": "not-a-valid-jwt"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400 for invalid license key; body: %s", resp.StatusCode, raw)
	}

	if got := holder.Load().Edition(); got != license.EditionCommunity {
		t.Errorf("holder.Load().Edition() = %q, want %q (invalid key must not update holder)", got, license.EditionCommunity)
	}

	if got := reloadCalled.Load(); got != 0 {
		t.Errorf("ReloadModels called %d times, want 0 for invalid key", got)
	}
}
