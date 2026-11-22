package admin_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"testing"

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
