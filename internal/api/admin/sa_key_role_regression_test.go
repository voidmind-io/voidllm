package admin_test

import (
	"context"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// mustCreateSAKeyCallerViaDB creates a service account (team-bound when teamID
// is non-nil, org-level otherwise) and an sa_key for it directly via the DB
// layer — mirroring the real create-key API, which rejects team_id on sa_key
// create requests, so the key row's own TeamID is always left nil — then
// reloads the key cache from the DB via auth.LoadKeysIntoCache so the
// returned key resolves its role exactly the way a real deployment would:
// through db.LoadAllActiveKeys -> auth.KeyInfoFromRecord, not a hand-built
// auth.KeyInfo. It returns the plaintext key and the created service account.
func mustCreateSAKeyCallerViaDB(t *testing.T, database *db.DB, keyCache *cache.Cache[string, auth.KeyInfo], orgID string, teamID *string, createdBy string) (string, *db.ServiceAccount) {
	t.Helper()

	sa, err := database.CreateServiceAccount(context.Background(), db.CreateServiceAccountParams{
		Name:      "regression-sa",
		OrgID:     orgID,
		TeamID:    teamID,
		CreatedBy: createdBy,
	})
	if err != nil {
		t.Fatalf("mustCreateSAKeyCallerViaDB: CreateServiceAccount: %v", err)
	}

	plaintext, err := keygen.Generate(keygen.KeyTypeSA)
	if err != nil {
		t.Fatalf("mustCreateSAKeyCallerViaDB: generate: %v", err)
	}
	if _, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:          keygen.Hash(plaintext, testHMACSecret),
		KeyHint:          keygen.Hint(plaintext),
		KeyType:          keygen.KeyTypeSA,
		Name:             "regression-sa-key",
		OrgID:            orgID,
		ServiceAccountID: &sa.ID,
		CreatedBy:        createdBy,
	}); err != nil {
		t.Fatalf("mustCreateSAKeyCallerViaDB: CreateAPIKey: %v", err)
	}

	if err := auth.LoadKeysIntoCache(context.Background(), database, keyCache, noopLogger(t)); err != nil {
		t.Fatalf("mustCreateSAKeyCallerViaDB: LoadKeysIntoCache: %v", err)
	}

	return plaintext, sa
}

// ---- Regression: sa_key role resolution must come from the owning service --
// ---- account's own team_id, not from api_keys.team_id (which is never set --
// ---- on sa_key rows by the real create-key API).                          --

// TestSAKeyRoleRegression_TeamBound verifies, against a real DB-backed caller
// resolved through LoadKeysIntoCache/KeyInfoFromRecord (not a hand-built
// auth.KeyInfo), that a team-bound service-account key:
//
//   - resolves to team_admin, not org_admin;
//   - cannot change the rate/token limits of another key in the org (the DB
//     write never happens; the request itself is answered 404, not 403 —
//     see the in-line comment on that subtest for why);
//   - is rejected by an org_admin-only route (PATCH /api/v1/orgs/{org_id}
//     and PATCH .../members/{id}), since those require org_admin and a
//     team-bound sa_key only ever resolves to team_admin.
//
// Before the fix, every sa_key resolved to org_admin regardless of its
// service account's team_id, because api_keys.team_id is never populated on
// sa_key rows — this test would have failed (200/200/200 instead of
// 404/403/403) against the pre-fix code.
func TestSAKeyRoleRegression_TeamBound(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestSAKeyRoleRegression_TeamBound?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "O", "sarole-teambound-org")
	team := mustCreateTeam(t, database, org.ID, "T", "sarole-teambound-team")
	creator := mustCreateUser(t, database, "sarole-teambound-creator@example.com", "Creator")
	other := mustCreateUser(t, database, "sarole-teambound-other@example.com", "Other")

	callerKey, sa := mustCreateSAKeyCallerViaDB(t, database, keyCache, org.ID, &team.ID, creator.ID)
	if sa.TeamID == nil || *sa.TeamID != team.ID {
		t.Fatalf("service account TeamID = %v, want %q", sa.TeamID, team.ID)
	}

	const originalRPM = 7
	target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &other.ID, other.ID, originalRPM)

	// (a) Cannot change limits on another key in the org.
	//
	// Observed status is 404, not 403: UpdateAPIKey gates non-org_admin
	// callers through apiKeyVisibleToCallerKey, which for a team_admin
	// caller only matches when caller.TeamID equals the target key's
	// TeamID. auth.KeyInfoFromRecord deliberately never populates
	// KeyInfo.TeamID for sa_key callers (it stays derived from the sa_key's
	// own, always-nil, api_keys.team_id — see keyinfo.go) even when the
	// underlying service account is team-bound, so caller.TeamID is always
	// "" for every sa_key. That never equals a real team ID, so the request
	// is treated as if the key were simply not visible (404) rather than
	// visible-but-forbidden (403). The DB write is still correctly
	// prevented either way. ListAPIKeys and ListServiceAccounts hit this
	// same empty-TeamID gap from the list side — a team-bound sa_key caller
	// has neither TeamID nor UserID to scope by — and are guarded
	// separately: both return 403 when a below-org_admin caller resolves to
	// no scope filter at all, rather than falling through to an unfiltered,
	// org-wide list.
	t.Run("cannot change limits on another key", func(t *testing.T) {
		req := httptest.NewRequest("PATCH", keyItemURL(org.ID, target.ID),
			bodyJSON(t, map[string]any{"requests_per_minute": float64(1)}))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+callerKey)

		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusNotFound {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 404; body: %s", resp.StatusCode, b)
		}

		reloaded, err := database.GetAPIKey(context.Background(), target.ID)
		if err != nil {
			t.Fatalf("reload target key: %v", err)
		}
		if reloaded.RequestsPerMinute != originalRPM {
			t.Errorf("RequestsPerMinute after rejected PATCH = %d, want unchanged %d", reloaded.RequestsPerMinute, originalRPM)
		}
	})

	// (b) Rejected by an org_admin-only route: PATCH /api/v1/orgs/{org_id}.
	t.Run("rejected by org_admin-only PATCH org route", func(t *testing.T) {
		req := httptest.NewRequest("PATCH", "/api/v1/orgs/"+org.ID,
			bodyJSON(t, map[string]any{"name": "Renamed By SA"}))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+callerKey)

		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusForbidden {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 403; body: %s", resp.StatusCode, b)
		}

		reloadedOrg, err := database.GetOrg(context.Background(), org.ID)
		if err != nil {
			t.Fatalf("reload org: %v", err)
		}
		if reloadedOrg.Name == "Renamed By SA" {
			t.Error("org name was changed by a team-bound sa_key caller, want unchanged")
		}
	})

	// (b) Rejected by an org_admin-only route: PATCH .../members/{id}.
	t.Run("rejected by org_admin-only PATCH member route", func(t *testing.T) {
		membership := mustCreateMembership(t, database, org.ID, other.ID, auth.RoleMember)

		req := httptest.NewRequest("PATCH", "/api/v1/orgs/"+org.ID+"/members/"+membership.ID,
			bodyJSON(t, map[string]any{"role": auth.RoleOrgAdmin}))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+callerKey)

		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusForbidden {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 403; body: %s", resp.StatusCode, b)
		}
	})
}

// TestSAKeyRoleRegression_OrgLevel verifies, against a real DB-backed caller
// resolved through LoadKeysIntoCache/KeyInfoFromRecord, that an org-level
// (no team) service-account key resolves to org_admin and may change the
// rate/token limits of another key in the org, but may never change its own
// limits — a machine caller can never grant itself a higher or unlimited
// budget.
func TestSAKeyRoleRegression_OrgLevel(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestSAKeyRoleRegression_OrgLevel?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "O", "sarole-orglevel-org")
	team := mustCreateTeam(t, database, org.ID, "T", "sarole-orglevel-team")
	creator := mustCreateUser(t, database, "sarole-orglevel-creator@example.com", "Creator")
	other := mustCreateUser(t, database, "sarole-orglevel-other@example.com", "Other")

	callerKey, sa := mustCreateSAKeyCallerViaDB(t, database, keyCache, org.ID, nil, creator.ID)
	if sa.TeamID != nil {
		t.Fatalf("service account TeamID = %q, want nil (org-level)", *sa.TeamID)
	}

	// Find the caller's own key ID so we can also verify it cannot raise its
	// own limits.
	callerKeyHash := keygen.Hash(callerKey, testHMACSecret)
	callerInfo, ok := keyCache.Get(callerKeyHash)
	if !ok {
		t.Fatal("caller key not found in cache after LoadKeysIntoCache()")
	}
	if callerInfo.Role != auth.RoleOrgAdmin {
		t.Fatalf("caller Role = %q, want %q (org-level sa_key)", callerInfo.Role, auth.RoleOrgAdmin)
	}

	const originalRPM = 7
	target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &other.ID, other.ID, originalRPM)

	t.Run("can change another user's key limits", func(t *testing.T) {
		req := httptest.NewRequest("PATCH", keyItemURL(org.ID, target.ID),
			bodyJSON(t, map[string]any{"requests_per_minute": float64(1)}))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+callerKey)

		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 200; body: %s", resp.StatusCode, b)
		}

		reloaded, err := database.GetAPIKey(context.Background(), target.ID)
		if err != nil {
			t.Fatalf("reload target key: %v", err)
		}
		if reloaded.RequestsPerMinute != 1 {
			t.Errorf("RequestsPerMinute after PATCH = %d, want 1", reloaded.RequestsPerMinute)
		}
	})

	t.Run("cannot change its own key limits", func(t *testing.T) {
		req := httptest.NewRequest("PATCH", keyItemURL(org.ID, callerInfo.ID),
			bodyJSON(t, map[string]any{"requests_per_minute": float64(1)}))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+callerKey)

		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusForbidden {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 403; body: %s", resp.StatusCode, b)
		}
	})
}
