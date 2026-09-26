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

// ---- Regression: below-org_admin callers with no scope filter must be ------
// ---- rejected with 403, not silently see every key/service-account in the -
// ---- org. A team-bound sa_key is the case that used to fail open: its ------
// ---- resolved role is team_admin, but auth.KeyInfoFromRecord deliberately --
// ---- never populates KeyInfo.TeamID for sa_key callers (it stays derived --
// ---- from the sa_key's own, always-nil, api_keys.team_id — see keyinfo.go)-
// ---- and it has no UserID either, so both scope filters end up empty, ------
// ---- which meant "no filter" at the DB layer before the fix. ---------------

// TestListAPIKeys_TeamBoundSAKey_ScopeRegression verifies that a team-bound
// service-account key calling GET /api/v1/orgs/{org_id}/keys gets 403
// (insufficient scope), rather than falling through to an unfiltered
// ListAPIKeys call that would return every key in the org.
func TestListAPIKeys_TeamBoundSAKey_ScopeRegression(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAPIKeys_TeamBoundSA_Scope?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "O", "list-keys-sa-scope-org")
	team := mustCreateTeam(t, database, org.ID, "T", "list-keys-sa-scope-team")
	creator := mustCreateUser(t, database, "list-keys-sa-scope-creator@example.com", "Creator")
	other := mustCreateUser(t, database, "list-keys-sa-scope-other@example.com", "Other")
	mustCreateMembership(t, database, org.ID, creator.ID, auth.RoleOrgAdmin)

	// Another key in the org that must never leak into the sa_key's response.
	mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &other.ID, other.ID, 0)

	callerKey, _ := mustCreateSAKeyCallerViaDB(t, database, keyCache, org.ID, &team.ID, creator.ID)

	req := httptest.NewRequest("GET", keysURL(org.ID), nil)
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
}

// TestListServiceAccounts_TeamBoundSAKey_ScopeRegression verifies that a
// team-bound service-account key calling GET
// /api/v1/orgs/{org_id}/service-accounts gets 403 (insufficient scope),
// rather than falling through to an unfiltered ListServiceAccountsWithCounts
// call that would return every service account in the org.
func TestListServiceAccounts_TeamBoundSAKey_ScopeRegression(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListSA_TeamBoundSA_Scope?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "O", "list-sa-scope-org")
	team := mustCreateTeam(t, database, org.ID, "T", "list-sa-scope-team")
	creator := mustCreateUser(t, database, "list-sa-scope-creator@example.com", "Creator")

	// Another service account in the org that must never leak into the
	// sa_key's response.
	mustCreateServiceAccountHTTP(t, database, org.ID, creator.ID, "Other SA", nil)

	callerKey, _ := mustCreateSAKeyCallerViaDB(t, database, keyCache, org.ID, &team.ID, creator.ID)

	req := httptest.NewRequest("GET", saURL(org.ID), nil)
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
}

// mustDBBackedUserKeyForMember creates a real, DB-backed user_key for userID
// (who must already have the desired org membership row) and reloads the
// entire key cache from the DB via auth.LoadKeysIntoCache — the same pattern
// mustCreateOrgAdminCallerKey uses, but without creating a membership, so
// callers can control the role via their own mustCreateMembership call first.
func mustDBBackedUserKeyForMember(t *testing.T, database *db.DB, keyCache *cache.Cache[string, auth.KeyInfo], orgID, userID string) string {
	t.Helper()

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("mustDBBackedUserKeyForMember: generate: %v", err)
	}
	if _, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "member-caller",
		OrgID:     orgID,
		UserID:    &userID,
		CreatedBy: userID,
	}); err != nil {
		t.Fatalf("mustDBBackedUserKeyForMember: CreateAPIKey: %v", err)
	}

	if err := auth.LoadKeysIntoCache(context.Background(), database, keyCache, noopLogger(t)); err != nil {
		t.Fatalf("mustDBBackedUserKeyForMember: LoadKeysIntoCache: %v", err)
	}

	return plaintext
}

// TestListAPIKeys_HumanMember_SeesOnlyOwnKeys_RegressionGuard is a regression
// guard for the fail-closed change above: a human member (a user_key, which
// always carries a non-empty UserID) must still succeed with 200 and see
// only their own keys — the new fail-closed check must not accidentally
// reject legitimate, correctly-scoped callers.
func TestListAPIKeys_HumanMember_SeesOnlyOwnKeys_RegressionGuard(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListAPIKeys_Member_Guard?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "O", "list-keys-member-guard-org")
	team := mustCreateTeam(t, database, org.ID, "T", "list-keys-member-guard-team")
	member := mustCreateUser(t, database, "list-keys-member-guard@example.com", "Member")
	other := mustCreateUser(t, database, "list-keys-member-guard-other@example.com", "Other")
	mustCreateMembership(t, database, org.ID, member.ID, auth.RoleMember)

	mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &other.ID, other.ID, 0)

	memberCallerKey := mustDBBackedUserKeyForMember(t, database, keyCache, org.ID, member.ID)

	req := httptest.NewRequest("GET", keysURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+memberCallerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got struct {
		Data []struct {
			ID     string  `json:"id"`
			UserID *string `json:"user_id"`
		} `json:"data"`
	}
	decodeBody(t, resp.Body, &got)
	if len(got.Data) != 1 {
		t.Fatalf("len(data) = %d, want 1 (member sees only own key)", len(got.Data))
	}
	if got.Data[0].UserID == nil || *got.Data[0].UserID != member.ID {
		t.Errorf("data[0].user_id = %v, want %q", got.Data[0].UserID, member.ID)
	}
}

// TestListServiceAccounts_OrgAdmin_SeesAll_RegressionGuard is a regression
// guard for the fail-closed change above: an org_admin caller must still
// succeed with 200 and see every service account in the org.
func TestListServiceAccounts_OrgAdmin_SeesAll_RegressionGuard(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestListSA_OrgAdmin_Guard?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "O", "list-sa-orgadmin-guard-org")
	admin := mustCreateUser(t, database, "list-sa-orgadmin-guard@example.com", "Admin")
	other := mustCreateUser(t, database, "list-sa-orgadmin-guard-other@example.com", "Other")

	mustCreateServiceAccountHTTP(t, database, org.ID, admin.ID, "SA One", nil)
	mustCreateServiceAccountHTTP(t, database, org.ID, other.ID, "SA Two", nil)

	callerKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, admin.ID)

	req := httptest.NewRequest("GET", saURL(org.ID), nil)
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	var got struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	decodeBody(t, resp.Body, &got)
	if len(got.Data) != 2 {
		t.Errorf("len(data) = %d, want 2 (org_admin sees all service accounts)", len(got.Data))
	}
}
