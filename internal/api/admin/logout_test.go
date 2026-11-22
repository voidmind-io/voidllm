package admin_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// doLogout performs POST /api/v1/auth/logout with the given token (none when
// empty) and returns the HTTP status code.
func doLogout(t *testing.T, app *fiber.App, token string) int {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("doLogout app.Test: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// createUserKeyInDB creates a vl_uk_ key for the user and returns its plaintext.
func createUserKeyInDB(t *testing.T, database *db.DB, orgID, userID string) string {
	t.Helper()
	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("createUserKeyInDB generate: %v", err)
	}
	if _, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "logout-test-user-key",
		OrgID:     orgID,
		UserID:    &userID,
		CreatedBy: userID,
	}); err != nil {
		t.Fatalf("createUserKeyInDB CreateAPIKey: %v", err)
	}
	return plaintext
}

func TestLogout_SessionKey(t *testing.T) {
	t.Parallel()

	const email = "logout-session@example.com"
	app, database, keyCache := setupTestApp(t, "file:TestLogout_Session?mode=memory&cache=private")
	user := setupLocalUser(t, database, "logout-session-org", email, "Logout User", "hunter2batteries")

	token := doLogin(t, app, email, "hunter2batteries")
	other := injectSession(t, database, keyCache, user, "other session")

	if got := doGet(t, app, "/api/v1/me", token); got != fiber.StatusOK {
		t.Fatalf("pre-logout /me = %d, want 200", got)
	}

	if got := doLogout(t, app, token); got != fiber.StatusNoContent {
		t.Fatalf("logout status = %d, want 204", got)
	}

	if got := doGet(t, app, "/api/v1/me", token); got != fiber.StatusUnauthorized {
		t.Errorf("after logout (cache) /me = %d, want 401", got)
	}

	// A cache reload from the DB must not resurrect the revoked session.
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := auth.LoadKeysIntoCache(context.Background(), database, keyCache, log); err != nil {
		t.Fatalf("LoadKeysIntoCache: %v", err)
	}
	if got := doGet(t, app, "/api/v1/me", token); got != fiber.StatusUnauthorized {
		t.Errorf("after logout and cache reload /me = %d, want 401", got)
	}

	if got := doGet(t, app, "/api/v1/me", other); got != fiber.StatusOK {
		t.Errorf("other session /me = %d, want 200", got)
	}
}

func TestLogout_NonSessionAndUnauthenticated(t *testing.T) {
	t.Parallel()

	const email = "logout-nonsession@example.com"
	app, database, keyCache := setupTestApp(t, "file:TestLogout_NonSession?mode=memory&cache=private")
	user := setupLocalUser(t, database, "logout-nonsession-org", email, "Logout User", "hunter2batteries")
	_, orgID, err := database.ResolveUserRole(context.Background(), user.ID)
	if err != nil {
		t.Fatalf("ResolveUserRole: %v", err)
	}
	userKey := createUserKeyInDB(t, database, orgID, user.ID)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := auth.LoadKeysIntoCache(context.Background(), database, keyCache, log); err != nil {
		t.Fatalf("LoadKeysIntoCache: %v", err)
	}

	tests := []struct {
		name       string
		token      string
		wantStatus int
		wantStill  int // /me status afterwards; 0 skips the check
	}{
		{"user key is a no-op", userKey, fiber.StatusNoContent, fiber.StatusOK},
		{"unauthenticated", "", fiber.StatusUnauthorized, 0},
		{"invalid token", "vl_uk_invalid", fiber.StatusUnauthorized, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := doLogout(t, app, tc.token); got != tc.wantStatus {
				t.Fatalf("logout status = %d, want %d", got, tc.wantStatus)
			}
			if tc.wantStill != 0 {
				if got := doGet(t, app, "/api/v1/me", tc.token); got != tc.wantStill {
					t.Errorf("after logout /me = %d, want %d", got, tc.wantStill)
				}
			}
		})
	}
}

func TestLogout_SessionWithAccessRows(t *testing.T) {
	t.Parallel()

	const email = "logout-access@example.com"
	app, database, keyCache := setupTestApp(t, "file:TestLogout_AccessRows?mode=memory&cache=private")
	setupLocalUser(t, database, "logout-access-org", email, "Logout User", "hunter2batteries")
	token := doLogin(t, app, email, "hunter2batteries")

	ctx := context.Background()
	hash := keygen.Hash(token, testHMACSecret)
	ki, ok := keyCache.Get(hash)
	if !ok {
		t.Fatal("session not in cache")
	}
	if err := database.SetKeyModelAccess(ctx, ki.ID, []string{"gpt-4o"}); err != nil {
		t.Fatalf("SetKeyModelAccess: %v", err)
	}

	if got := doLogout(t, app, token); got != fiber.StatusNoContent {
		t.Fatalf("logout with access rows = %d, want 204", got)
	}
	models, err := database.GetKeyModelAccess(ctx, ki.ID)
	if err != nil {
		t.Fatalf("GetKeyModelAccess: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("access rows left after logout: %v", models)
	}
}

func TestChangeOwnPassword_RevokesOtherSessionsWithAccessRows(t *testing.T) {
	t.Parallel()

	const email = "chpwd-access@example.com"
	app, database, keyCache := setupTestApp(t, "file:TestChPwd_AccessRows?mode=memory&cache=private")
	user := setupLocalUser(t, database, "chpwd-access-org", email, "Pwd User", "hunter2batteries")
	token := doLogin(t, app, email, "hunter2batteries")
	other := injectSession(t, database, keyCache, user, "other session")

	ctx := context.Background()
	otherKI, ok := keyCache.Get(keygen.Hash(other, testHMACSecret))
	if !ok {
		t.Fatal("other session not in cache")
	}
	if err := database.SetKeyModelAccess(ctx, otherKI.ID, []string{"gpt-4o"}); err != nil {
		t.Fatalf("SetKeyModelAccess: %v", err)
	}

	if status, msg := doChangePassword(t, app, token, "hunter2batteries", "correct-horse-staple"); status != fiber.StatusOK {
		t.Fatalf("change password = %d (%s), want 200", status, msg)
	}
	if got := doGet(t, app, "/api/v1/me", other); got != fiber.StatusUnauthorized {
		t.Errorf("other session /me = %d, want 401", got)
	}
	if got := doGet(t, app, "/api/v1/me", token); got != fiber.StatusOK {
		t.Errorf("current session /me = %d, want 200", got)
	}
	models, err := database.GetKeyModelAccess(ctx, otherKI.ID)
	if err != nil {
		t.Fatalf("GetKeyModelAccess: %v", err)
	}
	if len(models) != 0 {
		t.Errorf("access rows of revoked session left: %v", models)
	}
}

// createUncachedSession inserts a session key into the DB only. The returned
// hash is what a peer node would have cached.
func createUncachedSession(t *testing.T, database *db.DB, orgID, userID string) (token, hash string) {
	t.Helper()
	token, err := keygen.Generate(keygen.KeyTypeSession)
	if err != nil {
		t.Fatalf("keygen.Generate: %v", err)
	}
	hash = keygen.Hash(token, testHMACSecret)
	exp := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash: hash, KeyHint: keygen.Hint(token), KeyType: keygen.KeyTypeSession,
		Name: "peer session", OrgID: orgID, UserID: &userID, ExpiresAt: &exp, CreatedBy: userID,
	}); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	return token, hash
}

// TestSessionRevocation_UsesDBHashes proves eviction follows the hashes the DB
// revoked and not a scan of the local cache: the cached entry deliberately
// carries no user ID, and one revoked session was never cached locally.
func TestSessionRevocation_UsesDBHashes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		revoke func(t *testing.T, app *fiber.App, email, token string)
	}{
		{"change password", func(t *testing.T, app *fiber.App, email, token string) {
			if status, msg := doChangePassword(t, app, token, "hunter2batteries", "correct-horse-staple"); status != fiber.StatusOK {
				t.Fatalf("change password = %d (%s)", status, msg)
			}
		}},
		{"login", func(t *testing.T, app *fiber.App, email, _ string) {
			doLogin(t, app, email, "hunter2batteries")
		}},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			email := "revoke-hashes-" + string(rune('a'+i)) + "@example.com"
			app, database, keyCache := setupTestApp(t, "file:TestSessionRevocation_"+string(rune('a'+i))+"?mode=memory&cache=private")
			user := setupLocalUser(t, database, "revoke-hashes-org-"+string(rune('a'+i)), email, "Rev User", "hunter2batteries")
			_, orgID, err := database.ResolveUserRole(context.Background(), user.ID)
			if err != nil {
				t.Fatalf("ResolveUserRole: %v", err)
			}
			current := doLogin(t, app, email, "hunter2batteries")

			_, uncachedHash := createUncachedSession(t, database, orgID, user.ID)
			_, oddHash := createUncachedSession(t, database, orgID, user.ID)
			keyCache.Set(oddHash, auth.KeyInfo{ID: "ghost", KeyType: keygen.KeyTypeSession})
			keyCache.Set(uncachedHash, auth.KeyInfo{ID: "ghost2", KeyType: keygen.KeyTypeSession})
			keyCache.Delete(uncachedHash) // simulate a session only cached on a peer

			tc.revoke(t, app, email, current)

			if _, ok := keyCache.Get(oddHash); ok {
				t.Error("revoked session with unrelated cache entry was not evicted by hash")
			}
			var n int
			if err := database.SQL().QueryRowContext(context.Background(),
				"SELECT COUNT(*) FROM api_keys WHERE key_hash IN (?, ?)", oddHash, uncachedHash).Scan(&n); err != nil {
				t.Fatalf("count: %v", err)
			}
			if n != 0 {
				t.Errorf("%d revoked sessions still in DB", n)
			}
		})
	}
}
