package audit_test

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/audit"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
)

// setupMiddlewareLogger creates an audit logger wired to a fresh in-memory DB
// with a very long flush interval so that events are only written on Stop().
// The caller is responsible for calling logger.Stop() exactly once, typically
// via defer.
func setupMiddlewareLogger(t *testing.T, dbName string) (*audit.Logger, *db.DB) {
	t.Helper()
	database := newTestDB(t, dbName)
	cfg := config.AuditConfig{
		BufferSize:    100,
		FlushInterval: 24 * time.Hour,
	}
	logger := audit.NewLogger(database, cfg, discardLogger())
	logger.Start()
	return logger, database
}

type middlewareRoute struct {
	method  string
	path    string
	handler fiber.Handler
}

// buildApp creates a Fiber app with the audit middleware registered at the
// /api/v1 group level, plus the supplied routes.
func buildApp(t *testing.T, logger *audit.Logger, routes []middlewareRoute) *fiber.App {
	t.Helper()
	app := fiber.New()
	api := app.Group("/api/v1", audit.Middleware(logger))
	for _, r := range routes {
		api.Add([]string{r.method}, r.path, r.handler)
	}
	return app
}

// handlerWithStatus returns a Fiber handler that replies with the given HTTP
// status code and an empty JSON object body.
func handlerWithStatus(status int) fiber.Handler {
	return func(c fiber.Ctx) error {
		return c.Status(status).JSON(fiber.Map{})
	}
}

// TestMiddleware_SkipsGET verifies that GET requests do not produce audit
// events even when the handler returns 200.
func TestMiddleware_SkipsGET(t *testing.T) {
	t.Parallel()

	logger, database := setupMiddlewareLogger(t, "TestMiddleware_SkipsGET")
	app := buildApp(t, logger, []middlewareRoute{
		{method: "GET", path: "/orgs/:org_id", handler: handlerWithStatus(200)},
	})

	req := httptest.NewRequest("GET", "/api/v1/orgs/org-123", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	// Stop flushes remaining events (there should be none).
	logger.Stop()

	if got := countAuditLogs(t, database); got != 0 {
		t.Errorf("audit_logs row count = %d after GET, want 0 (GET must not be audited)", got)
	}
}

// TestMiddleware_SkipsNon2xx verifies that mutation requests that result in a
// non-2xx response do not produce audit events.
func TestMiddleware_SkipsNon2xx(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status int
	}{
		{name: "bad_request_400", status: 400},
		{name: "unauthorized_401", status: 401},
		{name: "forbidden_403", status: 403},
		{name: "not_found_404", status: 404},
		{name: "internal_server_error_500", status: 500},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logger, database := setupMiddlewareLogger(t, "TestMiddleware_SkipsNon2xx_"+tc.name)
			app := buildApp(t, logger, []middlewareRoute{
				{method: "POST", path: "/orgs", handler: handlerWithStatus(tc.status)},
			})

			req := httptest.NewRequest("POST", "/api/v1/orgs", nil)
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			logger.Stop()

			if got := countAuditLogs(t, database); got != 0 {
				t.Errorf("status %d: audit_logs row count = %d, want 0 (non-2xx must not be audited)",
					tc.status, got)
			}
		})
	}
}

// TestMiddleware_LogsMutation verifies that a successful POST (201 Created)
// produces exactly one audit event with action="create" and the correct
// resource type.
func TestMiddleware_LogsMutation(t *testing.T) {
	t.Parallel()

	logger, database := setupMiddlewareLogger(t, "TestMiddleware_LogsMutation")
	app := buildApp(t, logger, []middlewareRoute{
		{method: "POST", path: "/orgs", handler: handlerWithStatus(201)},
	})

	req := httptest.NewRequest("POST", "/api/v1/orgs", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	if resp.StatusCode != 201 {
		t.Fatalf("handler status = %d, want 201", resp.StatusCode)
	}

	logger.Stop()

	if got := countAuditLogs(t, database); got != 1 {
		t.Fatalf("audit_logs row count = %d, want 1", got)
	}

	var action, resourceType string
	row := database.SQL().QueryRowContext(context.Background(),
		"SELECT action, resource_type FROM audit_logs LIMIT 1")
	if err := row.Scan(&action, &resourceType); err != nil {
		t.Fatalf("scan audit_logs row: %v", err)
	}
	if action != "create" {
		t.Errorf("action = %q, want %q", action, "create")
	}
	if resourceType != "org" {
		t.Errorf("resource_type = %q, want %q", resourceType, "org")
	}
}

// TestMiddleware_LogsMutation_Methods verifies that PUT, PATCH, and DELETE
// mutation requests that succeed produce audit events with the correct action,
// resource type, and resource ID.
func TestMiddleware_LogsMutation_Methods(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		method         string
		path           string
		url            string
		wantAction     string
		wantResource   string
		wantResourceID string
		responseStatus int
	}{
		{
			name:           "PUT produces update",
			method:         "PUT",
			path:           "/orgs/:org_id",
			url:            "/api/v1/orgs/org-123",
			wantAction:     "update",
			wantResource:   "org",
			wantResourceID: "org-123",
			responseStatus: 200,
		},
		{
			name:           "PATCH produces update",
			method:         "PATCH",
			path:           "/orgs/:org_id",
			url:            "/api/v1/orgs/org-456",
			wantAction:     "update",
			wantResource:   "org",
			wantResourceID: "org-456",
			responseStatus: 200,
		},
		{
			name:           "DELETE produces delete with team resource ID",
			method:         "DELETE",
			path:           "/orgs/:org_id/teams/:team_id",
			url:            "/api/v1/orgs/org-123/teams/team-456",
			wantAction:     "delete",
			wantResource:   "team",
			wantResourceID: "team-456",
			responseStatus: 204,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logger, database := setupMiddlewareLogger(t, "TestMiddleware_Methods_"+tc.name)
			app := buildApp(t, logger, []middlewareRoute{
				{method: tc.method, path: tc.path, handler: handlerWithStatus(tc.responseStatus)},
			})

			req := httptest.NewRequest(tc.method, tc.url, nil)
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			logger.Stop()

			if got := countAuditLogs(t, database); got != 1 {
				t.Fatalf("audit_logs row count = %d, want 1", got)
			}

			var action, resourceType, resourceID string
			row := database.SQL().QueryRowContext(context.Background(),
				"SELECT action, resource_type, resource_id FROM audit_logs LIMIT 1")
			if err := row.Scan(&action, &resourceType, &resourceID); err != nil {
				t.Fatalf("scan audit_logs row: %v", err)
			}
			if action != tc.wantAction {
				t.Errorf("action = %q, want %q", action, tc.wantAction)
			}
			if resourceType != tc.wantResource {
				t.Errorf("resource_type = %q, want %q", resourceType, tc.wantResource)
			}
			if resourceID != tc.wantResourceID {
				t.Errorf("resource_id = %q, want %q", resourceID, tc.wantResourceID)
			}
		})
	}
}

// TestMiddleware_VerbOverride verifies that explicit verb segments in the URL
// (revoke, activate, deactivate) are used as the action instead of inferring
// the action from the HTTP method.
func TestMiddleware_VerbOverride(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		method       string
		path         string
		url          string
		wantAction   string
		wantResource string
	}{
		{
			name:         "POST revoke key",
			method:       "POST",
			path:         "/orgs/:org_id/keys/:key_id/revoke",
			url:          "/api/v1/orgs/org-123/keys/key-789/revoke",
			wantAction:   "revoke",
			wantResource: "key",
		},
		{
			name:         "PATCH activate model",
			method:       "PATCH",
			path:         "/models/:model_id/activate",
			url:          "/api/v1/models/model-123/activate",
			wantAction:   "activate",
			wantResource: "model",
		},
		{
			name:         "PATCH deactivate model",
			method:       "PATCH",
			path:         "/models/:model_id/deactivate",
			url:          "/api/v1/models/model-456/deactivate",
			wantAction:   "deactivate",
			wantResource: "model",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			logger, database := setupMiddlewareLogger(t, "TestMiddleware_VerbOverride_"+tc.name)
			app := buildApp(t, logger, []middlewareRoute{
				{method: tc.method, path: tc.path, handler: handlerWithStatus(200)},
			})

			req := httptest.NewRequest(tc.method, tc.url, nil)
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			logger.Stop()

			if got := countAuditLogs(t, database); got != 1 {
				t.Fatalf("audit_logs row count = %d, want 1", got)
			}

			var action, resourceType string
			row := database.SQL().QueryRowContext(context.Background(),
				"SELECT action, resource_type FROM audit_logs LIMIT 1")
			if err := row.Scan(&action, &resourceType); err != nil {
				t.Fatalf("scan audit_logs row: %v", err)
			}
			if action != tc.wantAction {
				t.Errorf("action = %q, want %q", action, tc.wantAction)
			}
			if resourceType != tc.wantResource {
				t.Errorf("resource_type = %q, want %q", resourceType, tc.wantResource)
			}
		})
	}
}
