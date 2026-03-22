package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"golang.org/x/crypto/bcrypt"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// mustCreateUserWithPassword creates a user with a known bcrypt password hash
// and fatals the test on error. Returns the created user.
func mustCreateUserWithPassword(t *testing.T, database *db.DB, email, displayName, password string) *db.User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt.GenerateFromPassword: %v", err)
	}
	h := string(hash)
	user, err := database.CreateUser(context.Background(), db.CreateUserParams{
		Email:        email,
		DisplayName:  displayName,
		PasswordHash: &h,
	})
	if err != nil {
		t.Fatalf("mustCreateUserWithPassword(%q): %v", email, err)
	}
	return user
}

// mustCreateOrgMembership creates an org membership for the given user/org/role,
// fataling the test on error.
func mustCreateOrgMembership(t *testing.T, database *db.DB, orgID, userID, role string) {
	t.Helper()
	_, err := database.CreateOrgMembership(context.Background(), db.CreateOrgMembershipParams{
		OrgID:  orgID,
		UserID: userID,
		Role:   role,
	})
	if err != nil {
		t.Fatalf("mustCreateOrgMembership(org=%q, user=%q, role=%q): %v", orgID, userID, role, err)
	}
}

// ---- POST /api/v1/auth/login -------------------------------------------------

func TestLogin(t *testing.T) {
	t.Parallel()

	const testPassword = "correcthorsebatterystaple"

	tests := []struct {
		name       string
		body       any
		setup      func(t *testing.T, database *db.DB) // optional additional setup
		wantStatus int
		checkBody  func(t *testing.T, body map[string]any)
	}{
		{
			name:       "correct credentials returns 200 with token",
			body:       map[string]any{"email": "login-ok@example.com", "password": testPassword},
			wantStatus: fiber.StatusOK,
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				token, ok := body["token"].(string)
				if !ok || token == "" {
					t.Errorf("token field missing or empty: %v", body)
					return
				}
				if !strings.HasPrefix(token, "vl_sk_") {
					t.Errorf("token = %q, want prefix %q", token, "vl_sk_")
				}
			},
		},
		{
			name:       "correct credentials response has expires_at field",
			body:       map[string]any{"email": "login-exp@example.com", "password": testPassword},
			wantStatus: fiber.StatusOK,
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				expiresAt, ok := body["expires_at"].(string)
				if !ok || expiresAt == "" {
					t.Errorf("expires_at field missing or empty: %v", body)
				}
			},
		},
		{
			name:       "wrong password returns 401",
			body:       map[string]any{"email": "login-wp@example.com", "password": "wrongpassword"},
			wantStatus: fiber.StatusUnauthorized,
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				if errMsg(body) != "invalid email or password" {
					t.Errorf("error.message = %q, want %q", errMsg(body), "invalid email or password")
				}
			},
		},
		{
			name:       "unknown email returns 401",
			body:       map[string]any{"email": "nobody@example.com", "password": testPassword},
			wantStatus: fiber.StatusUnauthorized,
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				// Same message as wrong password — no user enumeration.
				if errMsg(body) != "invalid email or password" {
					t.Errorf("error.message = %q, want %q", errMsg(body), "invalid email or password")
				}
			},
		},
		{
			name:       "missing email field returns 400",
			body:       map[string]any{"password": testPassword},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "missing password field returns 400",
			body:       map[string]any{"email": "login-mp@example.com"},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "empty email returns 400",
			body:       map[string]any{"email": "", "password": testPassword},
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "empty password returns 400",
			body:       map[string]any{"email": "login-ep@example.com", "password": ""},
			wantStatus: fiber.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestLogin_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, _ := setupTestApp(t, dsn)

			// For every case that sends a valid/existing email we need to set up
			// a real user + org + membership so resolveSessionRole succeeds.
			if bodyMap, ok := tc.body.(map[string]any); ok {
				email, _ := bodyMap["email"].(string)
				pwd, _ := bodyMap["password"].(string)

				switch email {
				case "login-ok@example.com", "login-exp@example.com":
					org := mustCreateOrg(t, database, "Login Test Org", slugFor(tc.name))
					user := mustCreateUserWithPassword(t, database, email, "Login User", testPassword)
					mustCreateOrgMembership(t, database, org.ID, user.ID, auth.RoleOrgAdmin)
				case "login-wp@example.com":
					// User exists but test sends wrong password.
					org := mustCreateOrg(t, database, "Login Test Org", slugFor(tc.name))
					user := mustCreateUserWithPassword(t, database, email, "Login User WP", testPassword)
					mustCreateOrgMembership(t, database, org.ID, user.ID, auth.RoleMember)
					_ = pwd // unused — test sends a different password in the body
				case "login-ep@example.com", "login-mp@example.com":
					// User exists but request is invalid before DB is reached; still
					// create the row so we don't accidentally get 401 instead of 400.
					org := mustCreateOrg(t, database, "Login Test Org", slugFor(tc.name))
					user := mustCreateUserWithPassword(t, database, email, "Login User EP", testPassword)
					mustCreateOrgMembership(t, database, org.ID, user.ID, auth.RoleMember)
				}
			}

			if tc.setup != nil {
				tc.setup(t, database)
			}

			req := httptest.NewRequest("POST", "/api/v1/auth/login", bodyJSON(t, tc.body))
			req.Header.Set("Content-Type", "application/json")
			// Deliberately no Authorization header — login is a public endpoint.

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				raw, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, raw)
			}

			if tc.checkBody != nil {
				var got map[string]any
				decodeBody(t, resp.Body, &got)
				tc.checkBody(t, got)
			}
		})
	}
}

// TestLogin_PublicEndpoint verifies the login route does not require an
// Authorization header, unlike every other admin endpoint.
func TestLogin_PublicEndpoint(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestLogin_Public?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "Public Login Org", "public-login-org")
	user := mustCreateUserWithPassword(t, database, "public@example.com", "Public User", "secret123")
	mustCreateOrgMembership(t, database, org.ID, user.ID, auth.RoleOrgAdmin)

	req := httptest.NewRequest("POST", "/api/v1/auth/login",
		bodyJSON(t, map[string]any{"email": "public@example.com", "password": "secret123"}))
	req.Header.Set("Content-Type", "application/json")
	// No Authorization header.

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Errorf("status = %d, want 200 (login is public); body: %s", resp.StatusCode, raw)
	}
}

// TestLogin_TokenWorks verifies that the session token returned by a successful
// login is accepted by the auth middleware for a subsequent authenticated request.
func TestLogin_TokenWorks(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestLogin_TokenWorks?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "Token Works Org", "token-works-org")
	user := mustCreateUserWithPassword(t, database, "tokenworks@example.com", "Token Works User", "mypassword99")
	mustCreateOrgMembership(t, database, org.ID, user.ID, auth.RoleOrgAdmin)

	// Step 1: log in to obtain a session token.
	loginReq := httptest.NewRequest("POST", "/api/v1/auth/login",
		bodyJSON(t, map[string]any{"email": "tokenworks@example.com", "password": "mypassword99"}))
	loginReq.Header.Set("Content-Type", "application/json")

	loginResp, err := app.Test(loginReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("login app.Test: %v", err)
	}
	defer loginResp.Body.Close()

	if loginResp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(loginResp.Body)
		t.Fatalf("login status = %d, want 200; body: %s", loginResp.StatusCode, raw)
	}

	var loginBody map[string]any
	decodeBody(t, loginResp.Body, &loginBody)

	token, _ := loginBody["token"].(string)
	if token == "" {
		t.Fatal("login response token is empty")
	}
	if !strings.HasPrefix(token, "vl_sk_") {
		t.Fatalf("token = %q, want prefix vl_sk_", token)
	}

	// Step 2: use the token to call an authenticated endpoint.
	getReq := httptest.NewRequest("GET", "/api/v1/orgs/"+org.ID, nil)
	getReq.Header.Set("Authorization", "Bearer "+token)

	getResp, err := app.Test(getReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("get org app.Test: %v", err)
	}
	defer getResp.Body.Close()

	if getResp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(getResp.Body)
		t.Errorf("GET /orgs/:id with session token status = %d, want 200; body: %s", getResp.StatusCode, raw)
	}
}

// TestLogin_SSOUserCannotLoginWithPassword verifies that an SSO-only account
// (no password_hash) receives the same generic 401 message as an unknown email,
// preventing user enumeration via differing error messages.
func TestLogin_SSOUserCannotLoginWithPassword(t *testing.T) {
	t.Parallel()

	app, database, _ := setupTestApp(t, "file:TestLogin_SSO?mode=memory&cache=private")

	extID := "oidc-sub-sso999"
	_, err := database.CreateUser(context.Background(), db.CreateUserParams{
		Email:        "sso-user@example.com",
		DisplayName:  "SSO User",
		AuthProvider: "oidc",
		ExternalID:   &extID,
		// PasswordHash deliberately nil — SSO-only account.
	})
	if err != nil {
		t.Fatalf("CreateUser SSO: %v", err)
	}

	req := httptest.NewRequest("POST", "/api/v1/auth/login",
		bodyJSON(t, map[string]any{"email": "sso-user@example.com", "password": "anypassword"}))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 401; body: %s", resp.StatusCode, raw)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)

	// SSO accounts return the same message as an unknown email to prevent enumeration.
	if errMsg(got) != "invalid email or password" {
		t.Errorf("error.message = %q, want %q", errMsg(got), "invalid email or password")
	}
}

// errMsg extracts the message string from a JSON error envelope of the form
// {"error": {"code": "...", "message": "..."}}.
// Returns "" if the structure does not match.
func errMsg(body map[string]any) string {
	errObj, _ := body["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	return msg
}

// slugFor derives a short, URL-safe slug from a test case name.
func slugFor(name string) string {
	r := strings.NewReplacer(" ", "-", "/", "-", "_", "-", "#", "-")
	s := strings.ToLower(r.Replace(name))
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}
