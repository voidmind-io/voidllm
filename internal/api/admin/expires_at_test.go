package admin_test

// Regression tests for API key expires_at validation and normalization (see
// normalizeExpiresAt in internal/api/admin/keys.go): every value the client
// supplies must parse as RFC3339(Nano) or the request is rejected with 400;
// a value that does parse is stored normalized to canonical UTC
// (db.FormatTimestamp shape) regardless of what offset the client sent.

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// createAPIKeyForExpiryTest issues a POST to create a user_key, including
// expires_at in the body only when raw is non-nil (so nil means "field
// omitted from the JSON body entirely", distinct from an explicit empty
// string). It returns the HTTP status and the decoded response body.
func createAPIKeyForExpiryTest(t *testing.T, app *fiber.App, testKey, orgID, userID, teamID string, raw *string) (int, map[string]any) {
	t.Helper()

	body := map[string]any{
		"name":     "expiry test key",
		"key_type": keygen.KeyTypeUser,
		"user_id":  userID,
		"team_id":  teamID,
	}
	if raw != nil {
		body["expires_at"] = *raw
	}

	req := httptest.NewRequest("POST", keysURL(orgID), bodyJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	var got map[string]any
	decodeBody(t, resp.Body, &got)
	return resp.StatusCode, got
}

// updateAPIKeyExpiresAt issues a PATCH with only the expires_at field set
// (raw == nil means the field is omitted from the JSON body entirely).
func updateAPIKeyExpiresAt(t *testing.T, app *fiber.App, testKey, orgID, keyID string, raw *string) (int, map[string]any) {
	t.Helper()

	body := map[string]any{}
	if raw != nil {
		body["expires_at"] = *raw
	}

	req := httptest.NewRequest("PATCH", keyItemURL(orgID, keyID), bodyJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	var got map[string]any
	decodeBody(t, resp.Body, &got)
	return resp.StatusCode, got
}

// setupExpiryTestFixtures creates an org, a user, and a team membership
// sufficient to create a user_key, plus a system_admin caller key.
func setupExpiryTestFixtures(t *testing.T, dsn string) (app *fiber.App, database *db.DB, testKey, orgID, userID, teamID string) {
	t.Helper()

	app, database, keyCache := setupTestApp(t, dsn)
	suffix := sanitizeSubtestName(dsn)
	org := mustCreateOrg(t, database, "Expiry Org", "expiry-org-"+suffix)
	user := mustCreateUser(t, database, "expiry-"+suffix+"@example.com", "Expiry User")
	team := mustCreateTeam(t, database, org.ID, "Expiry Team", "expiry-team-"+suffix)
	mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)
	key := addTestKeyWithUser(t, keyCache, auth.RoleSystemAdmin, org.ID, user.ID)

	return app, database, key, org.ID, user.ID, team.ID
}

// ---- CreateAPIKey expires_at matrix -------------------------------------------

func TestCreateAPIKey_ExpiresAt_Matrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        *string
		wantStatus int
		wantStored string // only checked when wantStatus == 201
	}{
		{
			name:       "millisecond-precision Z is stored as canonical Z, seconds truncated",
			raw:        strPtr("2026-10-26T21:23:17.123Z"),
			wantStatus: fiber.StatusCreated,
			wantStored: "2026-10-26T21:23:17Z",
		},
		{
			name:       "positive offset is normalized to UTC",
			raw:        strPtr("2026-10-26T23:23:17+02:00"),
			wantStatus: fiber.StatusCreated,
			wantStored: "2026-10-26T21:23:17Z",
		},
		{
			name:       "empty string is rejected",
			raw:        strPtr(""),
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "natural language is rejected",
			raw:        strPtr("tomorrow"),
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "SQLite-style space-separated form is rejected",
			raw:        strPtr("2026-10-26 21:23:17"),
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "omitted field is accepted, no expiry set",
			raw:        nil,
			wantStatus: fiber.StatusCreated,
			wantStored: "",
		},
		{
			name:       "past date is accepted (revoke-on-create)",
			raw:        strPtr("2020-01-01T00:00:00Z"),
			wantStatus: fiber.StatusCreated,
			wantStored: "2020-01-01T00:00:00Z",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := "file:TestCreateAPIKey_ExpiresAt_" + sanitizeSubtestName(tc.name) + "?mode=memory&cache=private"
			app, _, testKey, orgID, userID, teamID := setupExpiryTestFixtures(t, dsn)

			status, got := createAPIKeyForExpiryTest(t, app, testKey, orgID, userID, teamID, tc.raw)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %v", status, tc.wantStatus, got)
			}
			if tc.wantStatus != fiber.StatusCreated {
				return
			}

			gotExpiresAt, _ := got["expires_at"].(string)
			if tc.wantStored == "" {
				if gotExpiresAt != "" {
					t.Errorf("expires_at = %q, want omitted/empty", gotExpiresAt)
				}
				return
			}
			if gotExpiresAt != tc.wantStored {
				t.Errorf("expires_at = %q, want %q", gotExpiresAt, tc.wantStored)
			}
		})
	}
}

// TestCreateAPIKey_ExpiresAt_FutureOffset_LoadActiveKeyFindsIt verifies that a
// key created with a future, non-UTC-offset expires_at is normalized to a
// value LoadActiveKey (the same lookup the auth cache warm-up path uses)
// still considers active.
func TestCreateAPIKey_ExpiresAt_FutureOffset_LoadActiveKeyFindsIt(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateAPIKey_ExpiresAt_FutureOffset_LoadActiveKeyFindsIt?mode=memory&cache=private"
	app, database, testKey, orgID, userID, teamID := setupExpiryTestFixtures(t, dsn)

	future := strPtr("2099-10-26T23:23:17+02:00")
	status, got := createAPIKeyForExpiryTest(t, app, testKey, orgID, userID, teamID, future)
	if status != fiber.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %v", status, got)
	}
	keyID, _ := got["id"].(string)
	if keyID == "" {
		t.Fatal("response missing non-empty id")
	}

	rec, err := database.LoadActiveKey(context.Background(), keyID)
	if err != nil {
		t.Fatalf("LoadActiveKey() error = %v, want nil (future-expiring key must be active)", err)
	}
	if rec.ExpiresAt == nil {
		t.Fatal("LoadActiveKey().ExpiresAt = nil, want set")
	}
	if want := "2099-10-26T21:23:17Z"; got["expires_at"] != want {
		t.Errorf("expires_at = %v, want %q", got["expires_at"], want)
	}
}

// TestCreateAPIKey_ExpiresAt_PastDate_KeyNotUsable verifies that a key
// created with a past expires_at is accepted (201) but LoadActiveKey — the
// same query the key cache and CreateAPIKey's own cache-population step
// use — reports it as not found, i.e. not usable.
func TestCreateAPIKey_ExpiresAt_PastDate_KeyNotUsable(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCreateAPIKey_ExpiresAt_PastDate_KeyNotUsable?mode=memory&cache=private"
	app, database, testKey, orgID, userID, teamID := setupExpiryTestFixtures(t, dsn)

	past := strPtr("2020-01-01T00:00:00Z")
	status, got := createAPIKeyForExpiryTest(t, app, testKey, orgID, userID, teamID, past)
	if status != fiber.StatusCreated {
		t.Fatalf("status = %d, want 201; body: %v", status, got)
	}
	keyID, _ := got["id"].(string)

	_, err := database.LoadActiveKey(context.Background(), keyID)
	if err == nil {
		t.Fatal("LoadActiveKey() for a key created already-expired = nil, want ErrNotFound")
	}
}

// ---- UpdateAPIKey expires_at matrix -------------------------------------------

func TestUpdateAPIKey_ExpiresAt_Matrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		raw        *string
		wantStatus int
		wantStored string
	}{
		{
			name:       "millisecond-precision Z is stored as canonical Z",
			raw:        strPtr("2026-10-26T21:23:17.123Z"),
			wantStatus: fiber.StatusOK,
			wantStored: "2026-10-26T21:23:17Z",
		},
		{
			name:       "positive offset is normalized to UTC",
			raw:        strPtr("2026-10-26T23:23:17+02:00"),
			wantStatus: fiber.StatusOK,
			wantStored: "2026-10-26T21:23:17Z",
		},
		{
			name:       "empty string is rejected",
			raw:        strPtr(""),
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "natural language is rejected",
			raw:        strPtr("tomorrow"),
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "SQLite-style space-separated form is rejected",
			raw:        strPtr("2026-10-26 21:23:17"),
			wantStatus: fiber.StatusBadRequest,
		},
		{
			name:       "past date is accepted (immediate revoke)",
			raw:        strPtr("2020-01-01T00:00:00Z"),
			wantStatus: fiber.StatusOK,
			wantStored: "2020-01-01T00:00:00Z",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := "file:TestUpdateAPIKey_ExpiresAt_" + sanitizeSubtestName(tc.name) + "?mode=memory&cache=private"
			app, _, testKey, orgID, userID, teamID := setupExpiryTestFixtures(t, dsn)

			_, created := createAPIKeyForExpiryTest(t, app, testKey, orgID, userID, teamID, nil)
			keyID, _ := created["id"].(string)
			if keyID == "" {
				t.Fatalf("setup: create returned no id: %v", created)
			}

			status, got := updateAPIKeyExpiresAt(t, app, testKey, orgID, keyID, tc.raw)
			if status != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body: %v", status, tc.wantStatus, got)
			}
			if tc.wantStatus != fiber.StatusOK {
				return
			}

			gotExpiresAt, _ := got["expires_at"].(string)
			if gotExpiresAt != tc.wantStored {
				t.Errorf("expires_at = %q, want %q", gotExpiresAt, tc.wantStored)
			}
		})
	}
}

// TestUpdateAPIKey_ExpiresAt_Omitted_LeavesUnchanged verifies that a PATCH
// request which omits expires_at entirely (as opposed to sending an empty
// string) leaves the key's existing expiry untouched.
func TestUpdateAPIKey_ExpiresAt_Omitted_LeavesUnchanged(t *testing.T) {
	t.Parallel()

	dsn := "file:TestUpdateAPIKey_ExpiresAt_Omitted_LeavesUnchanged?mode=memory&cache=private"
	app, _, testKey, orgID, userID, teamID := setupExpiryTestFixtures(t, dsn)

	original := strPtr("2026-10-26T21:23:17Z")
	_, created := createAPIKeyForExpiryTest(t, app, testKey, orgID, userID, teamID, original)
	keyID, _ := created["id"].(string)
	if keyID == "" {
		t.Fatalf("setup: create returned no id: %v", created)
	}

	// PATCH with expires_at omitted; only touch an unrelated field so the
	// request body is non-empty and clearly a real update.
	req := httptest.NewRequest("PATCH", keyItemURL(orgID, keyID), bodyJSON(t, map[string]any{"name": "renamed"}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+testKey)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got map[string]any
	decodeBody(t, resp.Body, &got)
	gotExpiresAt, _ := got["expires_at"].(string)
	if gotExpiresAt != *original {
		t.Errorf("expires_at after omitted-field update = %q, want unchanged %q", gotExpiresAt, *original)
	}
}

// strPtr returns a pointer to s.
func strPtr(s string) *string { return &s }

// sanitizeSubtestName replaces characters unsafe in a SQLite URI filename.
func sanitizeSubtestName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	return string(out)
}
