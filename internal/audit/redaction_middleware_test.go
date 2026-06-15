package audit_test

// End-to-end redaction tests for the audit middleware. These tests send a real
// HTTP request through a Fiber app wired to the audit middleware, let the event
// flush to an in-memory SQLite database, and then read the persisted description
// back to assert that no secret value is present.

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// TestMiddleware_Redaction_PasswordNotPersistedInDescription verifies that a
// POST body containing a "password" field results in an audit log row whose
// description column contains neither the plaintext password nor any variation
// of it. The field key itself must appear (as "[REDACTED]") so the audit trail
// records that a password was supplied.
func TestMiddleware_Redaction_PasswordNotPersistedInDescription(t *testing.T) {
	t.Parallel()

	logger, database := setupMiddlewareLogger(t, "TestMiddleware_Redaction_Password")
	app := buildApp(t, logger, []middlewareRoute{
		{method: "POST", path: "/users", handler: handlerWithStatus(201)},
	})

	body := `{"email":"alice@example.com","password":"super-secret-pass-99"}`
	req := httptest.NewRequest("POST", "/api/v1/users",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

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

	var description string
	row := database.SQL().QueryRowContext(context.Background(),
		"SELECT description FROM audit_logs LIMIT 1")
	if err := row.Scan(&description); err != nil {
		t.Fatalf("scan description: %v", err)
	}

	if strings.Contains(description, "super-secret-pass-99") {
		t.Errorf("description %q must not contain the plaintext password", description)
	}
	if !strings.Contains(description, `"password"`) {
		t.Errorf("description %q must contain the field key \"password\" (as [REDACTED]) "+
			"so the audit trail shows the field was supplied", description)
	}
	if !strings.Contains(description, "[REDACTED]") {
		t.Errorf("description %q must contain \"[REDACTED]\" for the password value", description)
	}
	if !strings.Contains(description, `"email"`) {
		t.Errorf("description %q must contain non-sensitive field \"email\"", description)
	}
}

// TestMiddleware_Redaction_APIKeyNotPersistedInDescription verifies that
// api_key values are redacted in the persisted audit description.
func TestMiddleware_Redaction_APIKeyNotPersistedInDescription(t *testing.T) {
	t.Parallel()

	logger, database := setupMiddlewareLogger(t, "TestMiddleware_Redaction_APIKey")
	app := buildApp(t, logger, []middlewareRoute{
		{method: "POST", path: "/models", handler: handlerWithStatus(201)},
	})

	body := `{"name":"gpt-4-proxy","api_key":"sk-live-very-secret-key"}`
	req := httptest.NewRequest("POST", "/api/v1/models",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	logger.Stop()

	if got := countAuditLogs(t, database); got != 1 {
		t.Fatalf("audit_logs row count = %d, want 1", got)
	}

	var description string
	row := database.SQL().QueryRowContext(context.Background(),
		"SELECT description FROM audit_logs LIMIT 1")
	if err := row.Scan(&description); err != nil {
		t.Fatalf("scan description: %v", err)
	}

	if strings.Contains(description, "sk-live-very-secret-key") {
		t.Errorf("description %q must not contain the plaintext api_key", description)
	}
	if !strings.Contains(description, "[REDACTED]") {
		t.Errorf("description %q must contain \"[REDACTED]\" for the api_key value", description)
	}
	if !strings.Contains(description, `"name":"gpt-4-proxy"`) {
		t.Errorf("description %q must contain the non-sensitive \"name\" field", description)
	}
}

// TestMiddleware_Redaction_NonSensitiveBodyPreserved verifies that a body
// containing no sensitive fields is preserved verbatim in the description
// (modulo zero-value dropping), and no "[REDACTED]" marker appears.
func TestMiddleware_Redaction_NonSensitiveBodyPreserved(t *testing.T) {
	t.Parallel()

	logger, database := setupMiddlewareLogger(t, "TestMiddleware_Redaction_NonSensitive")
	app := buildApp(t, logger, []middlewareRoute{
		{method: "PATCH", path: "/orgs/:org_id", handler: handlerWithStatus(200)},
	})

	body := `{"display_name":"Acme Corp","max_tokens":4096}`
	req := httptest.NewRequest("PATCH", "/api/v1/orgs/org-abc",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	logger.Stop()

	if got := countAuditLogs(t, database); got != 1 {
		t.Fatalf("audit_logs row count = %d, want 1", got)
	}

	var description string
	row := database.SQL().QueryRowContext(context.Background(),
		"SELECT description FROM audit_logs LIMIT 1")
	if err := row.Scan(&description); err != nil {
		t.Fatalf("scan description: %v", err)
	}

	if strings.Contains(description, "[REDACTED]") {
		t.Errorf("description %q must not contain \"[REDACTED]\" for non-sensitive fields", description)
	}
	if !strings.Contains(description, `"display_name":"Acme Corp"`) {
		t.Errorf("description %q must contain the \"display_name\" field value", description)
	}
}

// TestMiddleware_Redaction_MultipleSecretsInOneRequest verifies that when a
// request body carries several sensitive fields simultaneously, every one of
// them is redacted and no plaintext secret leaks into the persisted description.
func TestMiddleware_Redaction_MultipleSecretsInOneRequest(t *testing.T) {
	t.Parallel()

	logger, database := setupMiddlewareLogger(t, "TestMiddleware_Redaction_MultipleSecrets")
	app := buildApp(t, logger, []middlewareRoute{
		{method: "PUT", path: "/orgs/:org_id/sso", handler: handlerWithStatus(200)},
	})

	body := `{"client_secret":"cs-plaintext","api_key":"ak-plaintext","issuer":"https://idp.example"}`
	req := httptest.NewRequest("PUT", "/api/v1/orgs/org-sso/sso",
		strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}()

	logger.Stop()

	if got := countAuditLogs(t, database); got != 1 {
		t.Fatalf("audit_logs row count = %d, want 1", got)
	}

	var description string
	row := database.SQL().QueryRowContext(context.Background(),
		"SELECT description FROM audit_logs LIMIT 1")
	if err := row.Scan(&description); err != nil {
		t.Fatalf("scan description: %v", err)
	}

	for _, secret := range []string{"cs-plaintext", "ak-plaintext"} {
		if strings.Contains(description, secret) {
			t.Errorf("description %q must not contain secret %q", description, secret)
		}
	}
	if !strings.Contains(description, "[REDACTED]") {
		t.Errorf("description %q must contain \"[REDACTED]\"", description)
	}
	if !strings.Contains(description, `"issuer"`) {
		t.Errorf("description %q must contain non-sensitive \"issuer\" field", description)
	}
}
