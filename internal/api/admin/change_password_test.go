package admin_test

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// doLogin performs a POST /api/v1/auth/login and returns the session token.
// It fatals immediately if login fails or the response contains no token.
func doLogin(t *testing.T, app *fiber.App, email, password string) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/auth/login",
		bodyJSON(t, map[string]any{"email": email, "password": password}))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("doLogin app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("doLogin: status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	var body map[string]any
	decodeBody(t, resp.Body, &body)
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatal("doLogin: response token is empty")
	}
	return token
}

// doLoginStatus performs a POST /api/v1/auth/login and returns only the HTTP
// status code. Unlike doLogin it does not fatal on a non-200 response.
func doLoginStatus(t *testing.T, app *fiber.App, email, password string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/auth/login",
		bodyJSON(t, map[string]any{"email": email, "password": password}))
	req.Header.Set("Content-Type", "application/json")

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("doLoginStatus app.Test: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// doChangePassword calls POST /api/v1/me/password with the given token and
// returns (statusCode, error-message-from-body).  An empty token omits the
// Authorization header entirely. The error message is empty on 200.
func doChangePassword(t *testing.T, app *fiber.App, token, currentPwd, newPwd string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/me/password",
		bodyJSON(t, map[string]any{
			"current_password": currentPwd,
			"new_password":     newPwd,
		}))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("doChangePassword app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == fiber.StatusOK {
		return fiber.StatusOK, ""
	}
	var body map[string]any
	decodeBody(t, resp.Body, &body)
	return resp.StatusCode, errMsg(body)
}

// doGet performs an authenticated GET request and returns only the status code.
func doGet(t *testing.T, app *fiber.App, path, token string) int {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("doGet %s app.Test: %v", path, err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// setupLocalUser creates an org + a local user with the given password inside an
// already-opened test app, and returns the user.
func setupLocalUser(t *testing.T, database *db.DB, orgSlug, email, displayName, password string) *db.User {
	t.Helper()
	org := mustCreateOrg(t, database, displayName+" Org", orgSlug)
	user := mustCreateUserWithPassword(t, database, email, displayName, password)
	mustCreateOrgMembership(t, database, org.ID, user.ID, auth.RoleOrgAdmin)
	return user
}

// injectSession creates a second independent session in the DB and the cache for
// the given user, without going through the Login handler (which would revoke
// existing sessions). Returns the plaintext token.
func injectSession(
	t *testing.T,
	database *db.DB,
	keyCache *cache.Cache[string, auth.KeyInfo],
	user *db.User,
	name string,
) string {
	t.Helper()

	raw, err := keygen.Generate(keygen.KeyTypeSession)
	if err != nil {
		t.Fatalf("injectSession keygen.Generate: %v", err)
	}
	hash := keygen.Hash(raw, testHMACSecret)

	role, orgID, err := database.ResolveUserRole(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("injectSession ResolveUserRole: %v", err)
	}

	exp := time.Now().UTC().Add(24 * time.Hour)
	expStr := exp.Format(time.RFC3339)

	apiKey, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:   hash,
		KeyHint:   keygen.Hint(raw),
		KeyType:   keygen.KeyTypeSession,
		Name:      name,
		OrgID:     orgID,
		UserID:    &user.ID,
		ExpiresAt: &expStr,
		CreatedBy: user.ID,
	})
	if err != nil {
		t.Fatalf("injectSession CreateAPIKey: %v", err)
	}

	keyCache.Set(hash, auth.KeyInfo{
		ID:        apiKey.ID,
		KeyType:   keygen.KeyTypeSession,
		Role:      role,
		OrgID:     orgID,
		UserID:    user.ID,
		Name:      name,
		ExpiresAt: &exp,
	})

	return raw
}

// ---- POST /api/v1/me/password ------------------------------------------------

// TestChangeOwnPassword_HappyPath is the direct regression test for Issue #99.
//
// Before the fix the handler returned 200 without writing the new bcrypt hash to
// the database.  The old password continued to work and the new password never
// did.  This test proves the bug is fixed: after a successful 200 response the
// new password must work for login and the old password must not.
func TestChangeOwnPassword_HappyPath(t *testing.T) {
	t.Parallel()

	const (
		oldPwd = "hunter2batteries"
		newPwd = "correct-horse-staple"
	)

	const email = "changepwd-happy@example.com"
	app, database, _ := setupTestApp(t, "file:TestChangePwd_Happy?mode=memory&cache=private")
	setupLocalUser(t, database, "happy-pwdtest-org", email, "Happy User", oldPwd)

	// Obtain a session and change the password.
	token := doLogin(t, app, email, oldPwd)
	status, msg := doChangePassword(t, app, token, oldPwd, newPwd)
	if status != fiber.StatusOK {
		t.Fatalf("change password: status = %d, msg: %s (Issue #99 regression)", status, msg)
	}

	// New password must now work for login — this is the core of Issue #99.
	// Before the fix this login would fail with 401 because the hash was never updated.
	if got := doLoginStatus(t, app, email, newPwd); got != fiber.StatusOK {
		t.Errorf("login with NEW password: status = %d, want 200 — password was NOT persisted (Issue #99 regression)", got)
	}

	// Old password must now be rejected.
	if got := doLoginStatus(t, app, email, oldPwd); got == fiber.StatusOK {
		t.Errorf("login with OLD password: status = 200, want non-200 — old password still accepted (Issue #99 regression)")
	}
}

// TestChangeOwnPassword_WrongCurrentPassword verifies that a wrong current
// password returns 400 and leaves the stored hash unchanged.
func TestChangeOwnPassword_WrongCurrentPassword(t *testing.T) {
	t.Parallel()

	const (
		realPwd = "myrealpassword"
		newPwd  = "completelynewpwd"
	)

	const email = "changepwd-wrongcurrent@example.com"
	app, database, _ := setupTestApp(t, "file:TestChangePwd_WrongCurrent?mode=memory&cache=private")
	setupLocalUser(t, database, "wrong-current-org", email, "Wrong Current User", realPwd)

	token := doLogin(t, app, email, realPwd)
	status, msg := doChangePassword(t, app, token, "thisiswrong!", newPwd)

	if status != fiber.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
	if msg != "current password is incorrect" {
		t.Errorf("error.message = %q, want %q", msg, "current password is incorrect")
	}

	// Old password must still work — the hash was not changed.
	if got := doLoginStatus(t, app, email, realPwd); got != fiber.StatusOK {
		t.Errorf("login with real password after rejected change: status = %d, want 200", got)
	}
}

// TestChangeOwnPassword_ValidationErrors covers the input validation cases:
// missing current_password, new_password too short, and no auth token.
func TestChangeOwnPassword_ValidationErrors(t *testing.T) {
	t.Parallel()

	const (
		realPwd = "validpassword99"
		email   = "changepwd-validation@example.com"
	)

	tests := []struct {
		name       string
		currentPwd string
		newPwd     string
		withToken  bool
		wantStatus int
		wantErrMsg string
	}{
		{
			name:       "missing current_password returns 400",
			currentPwd: "",
			newPwd:     "validnewpwd99",
			withToken:  true,
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "new_password shorter than 8 chars returns 400",
			currentPwd: realPwd,
			newPwd:     "short",
			withToken:  true,
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "no auth token returns 401",
			currentPwd: realPwd,
			newPwd:     "validnewpwd99",
			withToken:  false,
			wantStatus: fiber.StatusUnauthorized,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsnSuffix := strings.ReplaceAll(tc.name, " ", "_")
			app, database, _ := setupTestApp(t, "file:TestChangePwd_Val_"+dsnSuffix+"?mode=memory&cache=private")

			orgSlug := "val-org-" + strings.ReplaceAll(tc.name, " ", "-")
			if len(orgSlug) > 50 {
				orgSlug = orgSlug[:50]
			}
			setupLocalUser(t, database, orgSlug, email, "Validation User", realPwd)

			var token string
			if tc.withToken {
				token = doLogin(t, app, email, realPwd)
			}

			status, _ := doChangePassword(t, app, token, tc.currentPwd, tc.newPwd)
			if status != tc.wantStatus {
				t.Errorf("status = %d, want %d", status, tc.wantStatus)
			}
		})
	}
}

// TestChangeOwnPassword_MaxLength verifies that a new_password longer than 128
// characters is rejected with 400 before any bcrypt work is done.
func TestChangeOwnPassword_MaxLength(t *testing.T) {
	t.Parallel()

	const (
		pwd   = "validpassword1"
		email = "changepwd-maxlen@example.com"
	)

	app, database, _ := setupTestApp(t, "file:TestChangePwd_MaxLen?mode=memory&cache=private")
	setupLocalUser(t, database, "maxlen-org", email, "MaxLen User", pwd)

	token := doLogin(t, app, email, pwd)

	tooLong := strings.Repeat("a", 129)
	status, msg := doChangePassword(t, app, token, pwd, tooLong)
	if status != fiber.StatusBadRequest {
		t.Errorf("status = %d, want 400 for password > 128 chars", status)
	}
	if !strings.Contains(msg, "at most 128") {
		t.Errorf("error.message = %q, want it to contain %q", msg, "at most 128")
	}
}

// TestChangeOwnPassword_MaxLengthBoundary verifies that a new_password of
// exactly 128 characters is rejected with 400 (one over the limit), and that a
// password of 127 characters is accepted.
func TestChangeOwnPassword_MaxLengthBoundary(t *testing.T) {
	t.Parallel()

	const (
		pwd   = "validpassword2"
		email = "changepwd-maxbound@example.com"
	)

	app, database, _ := setupTestApp(t, "file:TestChangePwd_MaxBound?mode=memory&cache=private")
	setupLocalUser(t, database, "maxbound-org", email, "MaxBound User", pwd)

	// 129 chars must be rejected.
	token := doLogin(t, app, email, pwd)
	tooLong := strings.Repeat("b", 129)
	status, msg := doChangePassword(t, app, token, pwd, tooLong)
	if status != fiber.StatusBadRequest {
		t.Errorf("129-char password: status = %d, msg: %s — want 400", status, msg)
	}

	// 64 chars (well within bcrypt's 72-byte limit and under 128) must be accepted.
	token2 := doLogin(t, app, email, pwd)
	exactly64 := strings.Repeat("c", 64)
	status2, msg2 := doChangePassword(t, app, token2, pwd, exactly64)
	if status2 != fiber.StatusOK {
		t.Errorf("64-char password: status = %d, msg: %s — want 200", status2, msg2)
	}
}

// TestChangeOwnPassword_CurrentSessionSurvivesInDB verifies that after a
// successful password change the current session's row is NOT deleted from the
// database. This prevents the user from being silently logged out when the
// in-memory key cache is next reloaded from the database (~30 s interval).
func TestChangeOwnPassword_CurrentSessionSurvivesInDB(t *testing.T) {
	t.Parallel()

	const (
		oldPwd = "dbsurvive_old"
		newPwd = "dbsurvive_new"
	)

	const email = "changepwd-dbsurvive@example.com"

	dsn := "file:TestChangePwd_DBSurvive?mode=memory&cache=private"
	app, database, keyCache := setupTestApp(t, dsn)

	setupLocalUser(t, database, "db-survive-org", email, "DB Survive User", oldPwd)

	// Obtain the current session via login.
	token1 := doLogin(t, app, email, oldPwd)
	token1Hash := keygen.Hash(token1, testHMACSecret)

	ki1, ok := keyCache.Get(token1Hash)
	if !ok {
		t.Fatal("token1 not found in cache after login")
	}

	// Inject a second session to verify it is removed from the DB.
	userObj, err := database.GetUserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}
	token2 := injectSession(t, database, keyCache, userObj, "Second session DB test")
	token2Hash := keygen.Hash(token2, testHMACSecret)
	ki2, ok := keyCache.Get(token2Hash)
	if !ok {
		t.Fatal("token2 not found in cache after inject")
	}

	// Change the password using token1.
	status, msg := doChangePassword(t, app, token1, oldPwd, newPwd)
	if status != fiber.StatusOK {
		t.Fatalf("change password: status = %d, msg: %s", status, msg)
	}

	// token1's DB row must still exist (RevokeUserSessionsExcept preserves it).
	var count1 int
	if err := database.SQL().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM api_keys WHERE id = ?", ki1.ID).Scan(&count1); err != nil {
		t.Fatalf("count token1 DB row: %v", err)
	}
	if count1 != 1 {
		t.Errorf("token1 DB row count = %d, want 1 — current session must survive in DB", count1)
	}

	// token2's DB row must be deleted.
	var count2 int
	if err := database.SQL().QueryRowContext(context.Background(),
		"SELECT COUNT(*) FROM api_keys WHERE id = ?", ki2.ID).Scan(&count2); err != nil {
		t.Fatalf("count token2 DB row: %v", err)
	}
	if count2 != 0 {
		t.Errorf("token2 DB row count = %d, want 0 — other session must be removed from DB", count2)
	}
}

// TestChangeOwnPassword_SSOAccount verifies that accounts created via an
// external identity provider (auth_provider != "local", NULL password_hash)
// receive 400 "password change not available for this account".
func TestChangeOwnPassword_SSOAccount(t *testing.T) {
	t.Parallel()

	dsn := "file:TestChangePwd_SSO?mode=memory&cache=private"
	app, database, keyCache := setupTestApp(t, dsn)

	// Create an SSO-only user with no password hash.
	extID := "oidc-sub-changepwd"
	ssoUser, err := database.CreateUser(context.Background(), db.CreateUserParams{
		Email:        "changepwd-sso@example.com",
		DisplayName:  "SSO User",
		AuthProvider: "oidc",
		ExternalID:   &extID,
		// PasswordHash intentionally nil — SSO account.
	})
	if err != nil {
		t.Fatalf("CreateUser SSO: %v", err)
	}

	org := mustCreateOrg(t, database, "SSO Org", "sso-org-pwdchg")
	mustCreateOrgMembership(t, database, org.ID, ssoUser.ID, auth.RoleOrgAdmin)

	// Inject a fake session key for the SSO user directly (SSO login bypasses the
	// password handler so there is no real session token to obtain via doLogin).
	ssoToken := injectSession(t, database, keyCache, ssoUser, "SSO test session")

	req := httptest.NewRequest("POST", "/api/v1/me/password",
		bodyJSON(t, map[string]any{
			"current_password": "anything",
			"new_password":     "newpassword99",
		}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ssoToken)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400 for SSO account; body: %s", resp.StatusCode, raw)
	}

	var body map[string]any
	decodeBody(t, resp.Body, &body)
	if got := errMsg(body); !strings.Contains(got, "password change not available") {
		t.Errorf("error.message = %q, want it to contain %q", got, "password change not available")
	}
}

// TestChangeOwnPassword_OnlyOwnPassword verifies that the endpoint always acts
// on the user identified by the bearer token. After User A changes their
// password, User B's password must remain exactly as it was.
func TestChangeOwnPassword_OnlyOwnPassword(t *testing.T) {
	t.Parallel()

	const (
		pwdA = "userApwd1234"
		pwdB = "userBpwd5678"
		newA = "userAnewpwd99"
	)

	dsn := "file:TestChangePwd_OnlyOwn?mode=memory&cache=private"
	app, database, _ := setupTestApp(t, dsn)

	orgA := mustCreateOrg(t, database, "Org A", "only-own-org-a")
	orgB := mustCreateOrg(t, database, "Org B", "only-own-org-b")

	userA := mustCreateUserWithPassword(t, database, "changepwd-owna@example.com", "User A", pwdA)
	userB := mustCreateUserWithPassword(t, database, "changepwd-ownb@example.com", "User B", pwdB)

	mustCreateOrgMembership(t, database, orgA.ID, userA.ID, auth.RoleOrgAdmin)
	mustCreateOrgMembership(t, database, orgB.ID, userB.ID, auth.RoleOrgAdmin)

	// User A changes their own password.
	tokenA := doLogin(t, app, "changepwd-owna@example.com", pwdA)
	status, msg := doChangePassword(t, app, tokenA, pwdA, newA)
	if status != fiber.StatusOK {
		t.Fatalf("User A password change: status = %d, msg: %s", status, msg)
	}

	// User B must still be able to log in with their original password.
	if got := doLoginStatus(t, app, "changepwd-ownb@example.com", pwdB); got != fiber.StatusOK {
		t.Errorf("User B login after User A changed password: status = %d, want 200 — User B's password was incorrectly affected", got)
	}
}

// TestChangeOwnPassword_SessionInvalidation verifies that after a successful
// password change via Token 1:
//   - Token 2 (a different session for the same user) is invalidated (401).
//   - Token 1 (the session that performed the change) remains valid (200).
func TestChangeOwnPassword_SessionInvalidation(t *testing.T) {
	t.Parallel()

	const (
		oldPwd = "sessionold123"
		newPwd = "sessionnew456"
	)

	const email = "changepwd-session@example.com"

	dsn := "file:TestChangePwd_SessionInval?mode=memory&cache=private"
	app, database, keyCache := setupTestApp(t, dsn)

	setupLocalUser(t, database, "session-inval-org", email, "Session User", oldPwd)

	// Token 1: obtained via real login — has a real key ID in cache and DB.
	token1 := doLogin(t, app, email, oldPwd)

	// Read back token1's KeyInfo so we know its real key ID (needed to ensure the
	// handler's "skip current session" guard (ki.ID != currentKeyID) works).
	token1Hash := keygen.Hash(token1, testHMACSecret)
	ki1, ok := keyCache.Get(token1Hash)
	if !ok {
		t.Fatal("token1 not found in cache after login")
	}
	if ki1.ID == "" {
		t.Fatal("token1 KeyInfo has empty ID")
	}

	// Resolve user from DB to get the *db.User for injectSession.
	userObj, err := database.GetUserByEmail(context.Background(), email)
	if err != nil {
		t.Fatalf("GetUserByEmail: %v", err)
	}

	// Token 2: injected directly into DB + cache with a different key ID,
	// simulating a session from another browser / device.
	token2 := injectSession(t, database, keyCache, userObj, "Second session")

	// Pre-condition: both tokens must be valid.
	if got := doGet(t, app, "/api/v1/me", token1); got != fiber.StatusOK {
		t.Fatalf("token1 before change: status = %d, want 200", got)
	}
	if got := doGet(t, app, "/api/v1/me", token2); got != fiber.StatusOK {
		t.Fatalf("token2 before change: status = %d, want 200", got)
	}

	// Change the password using Token 1.
	status, msg := doChangePassword(t, app, token1, oldPwd, newPwd)
	if status != fiber.StatusOK {
		t.Fatalf("change password: status = %d, msg: %s", status, msg)
	}

	// Token 2 must now be rejected (evicted from cache by ChangeOwnPassword).
	if got := doGet(t, app, "/api/v1/me", token2); got != fiber.StatusUnauthorized {
		t.Errorf("token2 after change: status = %d, want 401 (other session must be invalidated)", got)
	}

	// Token 1 (the session that performed the change) must still be valid.
	if got := doGet(t, app, "/api/v1/me", token1); got != fiber.StatusOK {
		t.Errorf("token1 after change: status = %d, want 200 (current session must remain valid)", got)
	}
}
