package admin_test

import (
	"fmt"
	"io"
	"net/http/httptest"
	"testing"

	"context"
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

// setupTestAppWithThrottle is like setupTestApp but attaches a real LoginThrottle
// to the handler so that brute-force protection is active.
func setupTestAppWithThrottle(t *testing.T, dsn string) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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
		License:       license.NewHolder(license.Verify("", true)),
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		LoginThrottle: auth.NewLoginThrottle(),
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	return app, database, keyCache
}

// doLoginRequest sends a single POST /api/v1/auth/login request with the given
// credentials and returns the HTTP status code.
func doLoginRequest(t *testing.T, app *fiber.App, email, password string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/auth/login",
		bodyJSON(t, map[string]any{"email": email, "password": password}))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestLoginThrottle_LockoutAfterFiveFailures verifies that five consecutive
// wrong-password attempts for a known user trigger account lockout so that the
// sixth attempt returns 429, regardless of whether the password is correct.
func TestLoginThrottle_LockoutAfterFiveFailures(t *testing.T) {
	t.Parallel()

	const (
		email    = "throttle-lock@example.com"
		password = "correcthorse"
	)

	dsn := "file:TestLoginThrottle_Lockout?mode=memory&cache=private"
	app, database, _ := setupTestAppWithThrottle(t, dsn)

	org := mustCreateOrg(t, database, "Throttle Org", "throttle-lock-org")
	user := mustCreateUserWithPassword(t, database, email, "Throttle User", password)
	mustCreateOrgMembership(t, database, org.ID, user.ID, auth.RoleOrgAdmin)

	// Five wrong-password attempts — each must return 401 (not yet throttled).
	for i := 1; i <= 5; i++ {
		status := doLoginRequest(t, app, email, "wrongpassword")
		if status != fiber.StatusUnauthorized {
			t.Fatalf("attempt %d: status = %d, want 401 (not yet throttled)", i, status)
		}
	}

	// Sixth attempt — must be throttled regardless of which password is used.
	req := httptest.NewRequest("POST", "/api/v1/auth/login",
		bodyJSON(t, map[string]any{"email": email, "password": "wrongpassword"}))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusTooManyRequests {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("sixth attempt: status = %d, want 429; body: %s", resp.StatusCode, raw)
	}

	// Verify the response body uses a generic message — it must not reveal whether
	// the account is locked or the IP is rate-limited.
	var got map[string]any
	decodeBody(t, resp.Body, &got)
	msg := errMsg(got)
	if msg == "" {
		t.Error("429 response has no error.message field")
	}
	// The message must not mention "lock", "block", or reveal account state.
	for _, forbidden := range []string{"lock", "block", "account", "email"} {
		if containsFold(msg, forbidden) {
			t.Errorf("429 message %q must not mention %q (information leak)", msg, forbidden)
		}
	}
}

// TestLoginThrottle_CorrectPasswordAlsoThrottled verifies that after lockout is
// active, even the correct password returns 429 (throttle check happens before
// credential verification).
func TestLoginThrottle_CorrectPasswordAlsoThrottled(t *testing.T) {
	t.Parallel()

	const (
		email    = "throttle-correct@example.com"
		password = "correcthorse2"
	)

	dsn := "file:TestLoginThrottle_CorrectPwd?mode=memory&cache=private"
	app, database, _ := setupTestAppWithThrottle(t, dsn)

	org := mustCreateOrg(t, database, "Throttle Org 2", "throttle-correct-org")
	user := mustCreateUserWithPassword(t, database, email, "Throttle User 2", password)
	mustCreateOrgMembership(t, database, org.ID, user.ID, auth.RoleOrgAdmin)

	// Trigger lockout with five wrong-password attempts.
	for i := 0; i < 5; i++ {
		doLoginRequest(t, app, email, "wrong")
	}

	// Now use the CORRECT password — must still receive 429.
	status := doLoginRequest(t, app, email, password)
	if status != fiber.StatusTooManyRequests {
		t.Errorf("correct-password attempt after lockout: status = %d, want 429", status)
	}
}

// TestLoginThrottle_SuccessResetsCounter verifies that a successful login resets
// the failure counter so that four subsequent failures do not re-trigger lockout.
func TestLoginThrottle_SuccessResetsCounter(t *testing.T) {
	t.Parallel()

	const (
		email    = "throttle-reset@example.com"
		password = "correcthorse3"
	)

	dsn := "file:TestLoginThrottle_Reset?mode=memory&cache=private"
	app, database, _ := setupTestAppWithThrottle(t, dsn)

	org := mustCreateOrg(t, database, "Throttle Org 3", "throttle-reset-org")
	user := mustCreateUserWithPassword(t, database, email, "Throttle User 3", password)
	mustCreateOrgMembership(t, database, org.ID, user.ID, auth.RoleOrgAdmin)

	// Four wrong attempts — one below the lockout threshold.
	for i := 0; i < 4; i++ {
		status := doLoginRequest(t, app, email, "wrong")
		if status != fiber.StatusUnauthorized {
			t.Fatalf("wrong attempt %d: status = %d, want 401", i+1, status)
		}
	}

	// One correct login — must succeed and reset the failure counter.
	status := doLoginRequest(t, app, email, password)
	if status != fiber.StatusOK {
		t.Fatalf("correct login: status = %d, want 200", status)
	}

	// Four more wrong attempts after the reset — must still return 401, not 429.
	for i := 0; i < 4; i++ {
		status := doLoginRequest(t, app, email, "wrong")
		if status != fiber.StatusUnauthorized {
			t.Fatalf("post-reset wrong attempt %d: status = %d, want 401 (counter should be reset)", i+1, status)
		}
	}
}

// TestLoginThrottle_NilThrottleExistingTestsUnchanged verifies that the default
// setupTestApp (no throttle attached) still passes the original login cases.
// This ensures nil-throttle is a clean no-op and does not alter existing tests.
func TestLoginThrottle_NilThrottleExistingTestsUnchanged(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		email      string
		password   string
		setup      func(t *testing.T, database *db.DB, org *db.Org)
		wantStatus int
	}{
		{
			name:     "correct credentials returns 200 without throttle",
			email:    "nil-throttle-ok@example.com",
			password: "pass1",
			setup: func(t *testing.T, database *db.DB, org *db.Org) {
				t.Helper()
				user := mustCreateUserWithPassword(t, database, "nil-throttle-ok@example.com", "User", "pass1")
				mustCreateOrgMembership(t, database, org.ID, user.ID, auth.RoleOrgAdmin)
			},
			wantStatus: fiber.StatusOK,
		},
		{
			name:     "wrong password returns 401 without throttle",
			email:    "nil-throttle-wp@example.com",
			password: "wrongpass",
			setup: func(t *testing.T, database *db.DB, org *db.Org) {
				t.Helper()
				user := mustCreateUserWithPassword(t, database, "nil-throttle-wp@example.com", "User WP", "correctpass")
				mustCreateOrgMembership(t, database, org.ID, user.ID, auth.RoleOrgAdmin)
			},
			wantStatus: fiber.StatusUnauthorized,
		},
		{
			name:       "unknown email returns 401 without throttle",
			email:      "nobody-nil@example.com",
			password:   "anything",
			setup:      func(t *testing.T, database *db.DB, org *db.Org) {},
			wantStatus: fiber.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestLoginThrottle_Nil_%s?mode=memory&cache=private",
				slugFor(tc.name))
			// Use the standard setup WITHOUT a throttle — tests nil-throttle no-op.
			app, database, _ := setupTestApp(t, dsn)
			org := mustCreateOrg(t, database, "Nil Throttle Org", "nil-throttle-org-"+slugFor(tc.name))
			tc.setup(t, database, org)

			status := doLoginRequest(t, app, tc.email, tc.password)
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
		})
	}
}

// containsFold reports whether s contains substr, case-insensitively.
func containsFold(s, substr string) bool {
	return len(s) >= len(substr) &&
		func() bool {
			sl := len(substr)
			for i := 0; i <= len(s)-sl; i++ {
				if equalFold(s[i:i+sl], substr) {
					return true
				}
			}
			return false
		}()
}

// equalFold is a simple ASCII-only case-insensitive equality check sufficient
// for the small set of forbidden words used in tests.
func equalFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}
