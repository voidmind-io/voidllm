package admin_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/health"
	"github.com/voidmind-io/voidllm/internal/license"
)

// fakeHealthProvider implements admin.ModelHealthProvider by returning a
// fixed set of results, letting dashboard tests control the health-status
// mix without standing up a real Checker or upstream HTTP servers.
type fakeHealthProvider struct {
	results []health.ModelHealth
}

// GetAllHealth returns the fixed results configured on f.
func (f fakeHealthProvider) GetAllHealth() []health.ModelHealth {
	return f.results
}

// setupDashboardTestApp creates a Fiber app wired with a fresh in-memory
// SQLite database, admin routes, and the given health provider (nil is
// valid — it mirrors health monitoring being disabled).
func setupDashboardTestApp(t *testing.T, dsn string, healthProvider admin.ModelHealthProvider) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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

	keyCache := cache.New[string, auth.KeyInfo]()

	handler := &admin.Handler{
		DB:            database,
		HMACSecret:    testHMACSecret,
		KeyCache:      keyCache,
		HealthChecker: healthProvider,
		License:       license.NewHolder(license.Verify("", true)),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// dashboardStatsURL returns the dashboard stats endpoint URL.
func dashboardStatsURL() string {
	return "/api/v1/dashboard/stats"
}

// TestDashboardStats_ModelsUnknown verifies that DashboardStats counts a
// model whose current health status is "unknown" into the models_unknown
// field, alongside the pre-existing healthy/degraded/unhealthy counts.
func TestDashboardStats_ModelsUnknown(t *testing.T) {
	t.Parallel()

	provider := fakeHealthProvider{results: []health.ModelHealth{
		{ModelName: "healthy-model", Status: "healthy"},
		{ModelName: "degraded-model", Status: "degraded"},
		{ModelName: "unhealthy-model", Status: "unhealthy"},
		{ModelName: "unknown-model-1", Status: "unknown"},
		{ModelName: "unknown-model-2", Status: "unknown"},
	}}

	app, database, keyCache := setupDashboardTestApp(t, "file:TestDashboardStats_ModelsUnknown?mode=memory&cache=private", provider)
	org := mustCreateOrg(t, database, "Dashboard Org", "dashboard-org-unknown")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("GET", dashboardStatsURL(), nil)
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

	var got struct {
		ModelsHealthy   int `json:"models_healthy"`
		ModelsUnhealthy int `json:"models_unhealthy"`
		ModelsDegraded  int `json:"models_degraded"`
		ModelsUnknown   int `json:"models_unknown"`
	}
	decodeBody(t, resp.Body, &got)

	if got.ModelsHealthy != 1 {
		t.Errorf("models_healthy = %d, want 1", got.ModelsHealthy)
	}
	if got.ModelsDegraded != 1 {
		t.Errorf("models_degraded = %d, want 1", got.ModelsDegraded)
	}
	if got.ModelsUnhealthy != 1 {
		t.Errorf("models_unhealthy = %d, want 1", got.ModelsUnhealthy)
	}
	if got.ModelsUnknown != 2 {
		t.Errorf("models_unknown = %d, want 2", got.ModelsUnknown)
	}
}

// TestDashboardStats_NoHealthChecker_ModelsUnknownZero verifies that when
// health monitoring is disabled (HealthChecker is nil), models_unknown is
// zero rather than omitted or erroring.
func TestDashboardStats_NoHealthChecker_ModelsUnknownZero(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupDashboardTestApp(t, "file:TestDashboardStats_NoHealthChecker?mode=memory&cache=private", nil)
	org := mustCreateOrg(t, database, "Dashboard Org", "dashboard-org-nohealth")
	testKey := addTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID)

	req := httptest.NewRequest("GET", dashboardStatsURL(), nil)
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

	var got struct {
		ModelsUnknown int `json:"models_unknown"`
	}
	decodeBody(t, resp.Body, &got)

	if got.ModelsUnknown != 0 {
		t.Errorf("models_unknown = %d, want 0 when health monitoring is disabled", got.ModelsUnknown)
	}
}
