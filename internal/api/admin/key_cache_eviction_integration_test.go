package admin_test

import (
	"context"
	"io"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// meRequest issues an authenticated GET /api/v1/me with the given plaintext
// key and returns the response status code. /api/v1/me requires only a valid
// authenticated key, no particular role, making it a suitable probe for
// "is this key still usable at all".
func meRequest(t *testing.T, app *fiber.App, plaintextKey string) int {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+plaintextKey)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test GET /api/v1/me: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// TestDeleteUser_EvictsAllKeysFromCacheImmediately is the integration
// regression test for the DeleteUser cache-eviction fix: soft-deleting a user
// must immediately evict every key that user owns from the in-memory key
// cache — without waiting for the next periodic LoadKeysIntoCache reload —
// and a subsequent full reload must not resurrect it, since auth.Cacheable
// refuses any user_key/session_key whose owning user is soft-deleted
// (UserActive false).
func TestDeleteUser_EvictsAllKeysFromCacheImmediately(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteUser_EvictsKeys?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "O", "del-user-evict-org")
	target := mustCreateUser(t, database, "del-user-evict-target@example.com", "Target")
	mustCreateMembership(t, database, org.ID, target.ID, auth.RoleOrgAdmin)

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate user_key: %v", err)
	}
	targetKey, err := database.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "target-user-key",
		OrgID:     org.ID,
		UserID:    &target.ID,
		CreatedBy: target.ID,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	// A real, DB-backed system_admin caller — required to survive DeleteUser's
	// own effect on the cache (it evicts only the target's keys, but a
	// DB-backed caller is what mirrors a real deployment).
	sysAdminKey := mustCreateSystemAdminCallerKey(t, database, keyCache, org.ID)

	if _, ok := keyCache.Get(targetKey.KeyHash); !ok {
		t.Fatal("target user_key not present in cache after initial LoadKeysIntoCache, want present")
	}
	if status := meRequest(t, app, plaintext); status != fiber.StatusOK {
		t.Fatalf("GET /api/v1/me before delete = %d, want 200", status)
	}

	delReq := httptest.NewRequest("DELETE", "/api/v1/users/"+target.ID, nil)
	delReq.Header.Set("Authorization", "Bearer "+sysAdminKey)
	delResp, err := app.Test(delReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test DELETE user: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(delResp.Body)
		t.Fatalf("DELETE user status = %d, want 204; body: %s", delResp.StatusCode, b)
	}

	t.Run("key evicted from cache immediately, no reload needed", func(t *testing.T) {
		if _, ok := keyCache.Get(targetKey.KeyHash); ok {
			t.Error("user_key still present in cache after DeleteUser, want evicted immediately")
		}
	})

	t.Run("request authenticated with the evicted key gets 401", func(t *testing.T) {
		if status := meRequest(t, app, plaintext); status != fiber.StatusUnauthorized {
			t.Errorf("GET /api/v1/me after delete = %d, want 401", status)
		}
	})

	t.Run("stays absent after a full LoadKeysIntoCache reload", func(t *testing.T) {
		if err := auth.LoadKeysIntoCache(ctx, database, keyCache, noopLogger(t)); err != nil {
			t.Fatalf("LoadKeysIntoCache: %v", err)
		}
		if _, ok := keyCache.Get(targetKey.KeyHash); ok {
			t.Error("user_key reappeared in cache after full reload, want absent (Cacheable must reject a soft-deleted user's key)")
		}
	})
}

// TestDeleteOrgMembership_EvictsThenReAddRestores covers the full lifecycle
// for a member removed from, and later re-added to, an org: removal evicts
// their key from the cache immediately (401 on an admin route), a full
// reload keeps it absent (no surviving membership row for auth.Cacheable to
// find), and re-adding the membership via POST .../members makes the key
// usable again immediately — CreateOrgMembership reloads the whole key cache
// from the DB on success, so no periodic-refresh wait is needed.
func TestDeleteOrgMembership_EvictsThenReAddRestores(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteOrgMembership_EvictReAdd?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "O", "del-mem-evict-org")
	target := mustCreateUser(t, database, "del-mem-evict-target@example.com", "Target")
	m := mustCreateMembership(t, database, org.ID, target.ID, auth.RoleMember)

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate user_key: %v", err)
	}
	targetKey, err := database.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "target-member-key",
		OrgID:     org.ID,
		UserID:    &target.ID,
		CreatedBy: target.ID,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	callerUser := mustCreateUser(t, database, "del-mem-evict-caller@example.com", "Caller")
	callerKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, callerUser.ID)

	if _, ok := keyCache.Get(targetKey.KeyHash); !ok {
		t.Fatal("target user_key not present in cache after initial LoadKeysIntoCache, want present")
	}
	if status := meRequest(t, app, plaintext); status != fiber.StatusOK {
		t.Fatalf("GET /api/v1/me before delete = %d, want 200", status)
	}

	delReq := httptest.NewRequest("DELETE", "/api/v1/orgs/"+org.ID+"/members/"+m.ID, nil)
	delReq.Header.Set("Authorization", "Bearer "+callerKey)
	delResp, err := app.Test(delReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test DELETE membership: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(delResp.Body)
		t.Fatalf("DELETE membership status = %d, want 204; body: %s", delResp.StatusCode, b)
	}

	t.Run("key evicted from cache immediately, no reload needed", func(t *testing.T) {
		if _, ok := keyCache.Get(targetKey.KeyHash); ok {
			t.Error("user_key still present in cache after DeleteOrgMembership, want evicted immediately")
		}
	})

	t.Run("request authenticated with the evicted key gets 401", func(t *testing.T) {
		if status := meRequest(t, app, plaintext); status != fiber.StatusUnauthorized {
			t.Errorf("GET /api/v1/me after delete = %d, want 401", status)
		}
	})

	t.Run("stays absent after a full LoadKeysIntoCache reload", func(t *testing.T) {
		if err := auth.LoadKeysIntoCache(ctx, database, keyCache, noopLogger(t)); err != nil {
			t.Fatalf("LoadKeysIntoCache: %v", err)
		}
		if _, ok := keyCache.Get(targetKey.KeyHash); ok {
			t.Error("user_key reappeared in cache after full reload, want absent (no surviving membership row)")
		}
		if status := meRequest(t, app, plaintext); status != fiber.StatusUnauthorized {
			t.Errorf("GET /api/v1/me after full reload = %d, want 401", status)
		}
	})

	t.Run("re-adding the membership restores the key immediately", func(t *testing.T) {
		reAddReq := httptest.NewRequest("POST", "/api/v1/orgs/"+org.ID+"/members",
			bodyJSON(t, map[string]any{"user_id": target.ID, "role": "member"}))
		reAddReq.Header.Set("Content-Type", "application/json")
		reAddReq.Header.Set("Authorization", "Bearer "+callerKey)
		reAddResp, err := app.Test(reAddReq, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test POST members (re-add): %v", err)
		}
		defer reAddResp.Body.Close()
		if reAddResp.StatusCode != fiber.StatusCreated {
			b, _ := io.ReadAll(reAddResp.Body)
			t.Fatalf("POST members (re-add) status = %d, want 201; body: %s", reAddResp.StatusCode, b)
		}

		if _, ok := keyCache.Get(targetKey.KeyHash); !ok {
			t.Error("user_key missing from cache immediately after re-adding the membership, want present")
		}
		if status := meRequest(t, app, plaintext); status != fiber.StatusOK {
			t.Errorf("GET /api/v1/me after re-adding membership = %d, want 200 (no periodic-refresh wait needed)", status)
		}
	})
}

// TestSystemAdminWithoutMembership_KeyStaysCacheable is a regression guard
// for legacy rows: a system_admin user (users.is_system_admin = true) with no
// org_memberships row at all — a state that predates or otherwise bypasses
// org membership bookkeeping — must still have a cacheable, working key.
// auth.Cacheable's user_key/session_key branch requires UserActive AND
// (MembershipExists OR IsSystemAdmin == 1); this exercises the IsSystemAdmin
// side of that OR directly against a real DB row.
func TestSystemAdminWithoutMembership_KeyStaysCacheable(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestSysAdminNoMembership_Cacheable?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "O", "sysadmin-no-membership-org")
	user, err := database.CreateUser(ctx, db.CreateUserParams{
		Email:         "sysadmin-no-membership@example.com",
		DisplayName:   "Legacy System Admin",
		AuthProvider:  "local",
		IsSystemAdmin: true,
	})
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// Deliberately no CreateOrgMembership call — simulating a legacy row with
	// no membership at all.

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate user_key: %v", err)
	}
	key, err := database.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "legacy-sysadmin-key",
		OrgID:     org.ID,
		UserID:    &user.ID,
		CreatedBy: user.ID,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	if err := auth.LoadKeysIntoCache(ctx, database, keyCache, noopLogger(t)); err != nil {
		t.Fatalf("LoadKeysIntoCache: %v", err)
	}

	ki, ok := keyCache.Get(key.KeyHash)
	if !ok {
		t.Fatal("legacy system_admin key missing from cache after LoadKeysIntoCache, want present (Cacheable must allow it)")
	}
	if ki.Role != auth.RoleSystemAdmin {
		t.Errorf("cached Role = %q, want %q", ki.Role, auth.RoleSystemAdmin)
	}

	if status := meRequest(t, app, plaintext); status != fiber.StatusOK {
		t.Errorf("GET /api/v1/me = %d, want 200", status)
	}
}

// TestDeleteServiceAccount_EvictsKeyNotPresentInLocalCache verifies that
// DeleteServiceAccount succeeds without error even when the key it must evict
// is not present in this instance's local cache (e.g. the cache was cleared,
// or the key was never loaded here) — DeleteServiceAccount sources the hashes
// to evict from ListActiveKeyHashesByServiceAccount (the DB), not from a scan
// of the local cache, so a cache miss on Delete is a harmless no-op rather
// than a reason to skip eviction or fail the request.
func TestDeleteServiceAccount_EvictsKeyNotPresentInLocalCache(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteSA_EvictsUncached?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "O", "sa-evict-uncached-org")
	admin := mustCreateUser(t, database, "sa-evict-uncached-admin@example.com", "Admin")
	sa := mustCreateServiceAccountHTTP(t, database, org.ID, admin.ID, "Uncached Bot", nil)

	plaintext, err := keygen.Generate(keygen.KeyTypeSA)
	if err != nil {
		t.Fatalf("generate sa_key: %v", err)
	}
	if _, err := database.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:          keygen.Hash(plaintext, testHMACSecret),
		KeyHint:          keygen.Hint(plaintext),
		KeyType:          keygen.KeyTypeSA,
		Name:             "uncached-sa-key",
		OrgID:            org.ID,
		ServiceAccountID: &sa.ID,
		CreatedBy:        admin.ID,
	}); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	keyHash := keygen.Hash(plaintext, testHMACSecret)

	callerKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, admin.ID)

	// Simulate the sa_key never having been loaded locally (or the cache
	// having been cleared) by explicitly evicting it right before the delete
	// call — Get must report it absent going into the DELETE request.
	keyCache.Delete(keyHash)
	if _, ok := keyCache.Get(keyHash); ok {
		t.Fatal("test setup: sa_key unexpectedly still present in cache")
	}

	delReq := httptest.NewRequest("DELETE", saItemURL(org.ID, sa.ID), nil)
	delReq.Header.Set("Authorization", "Bearer "+callerKey)
	delResp, err := app.Test(delReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test DELETE service account: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(delResp.Body)
		t.Fatalf("DELETE service account status = %d, want 204 (no error even though the key was never cache-resident); body: %s", delResp.StatusCode, b)
	}

	// The DB-based hash lookup must still find the key belonging to the
	// now-deleted service account (soft-delete never touches api_keys rows).
	hashes, err := database.ListActiveKeyHashesByServiceAccount(ctx, sa.ID)
	if err != nil {
		t.Fatalf("ListActiveKeyHashesByServiceAccount: %v", err)
	}
	found := false
	for _, h := range hashes {
		if h == keyHash {
			found = true
			break
		}
	}
	if !found {
		t.Error("ListActiveKeyHashesByServiceAccount no longer returns the sa_key's hash after delete, want it still present (api_keys row is untouched)")
	}
}

// TestUpdateAPIKey_CacheMatchesFreshKeyInfoFromRecord verifies that after an
// org admin PATCHes a key's limits, the resulting cache entry is exactly what
// a fresh auth.KeyInfoFromRecord(db.LoadActiveKey(...)) of the DB row would
// produce — including org, team, and per-user membership limits resolved via
// JOIN — rather than a partially-patched copy of the previously cached value.
func TestUpdateAPIKey_CacheMatchesFreshKeyInfoFromRecord(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateAPIKey_CacheMatchesFresh?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "O", "updkey-cachematch-org")
	team := mustCreateTeam(t, database, org.ID, "T", "updkey-cachematch-team")
	owner := mustCreateUser(t, database, "updkey-cachematch-owner@example.com", "Owner")
	callerUser := mustCreateUser(t, database, "updkey-cachematch-caller@example.com", "Caller")

	m := mustCreateMembership(t, database, org.ID, owner.ID, auth.RoleMember)
	rpd := 250
	if _, err := database.UpdateOrgMembership(ctx, m.ID, db.UpdateOrgMembershipParams{
		RequestsPerDay: &rpd,
	}); err != nil {
		t.Fatalf("seed owner's membership limit: %v", err)
	}

	target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &owner.ID, owner.ID, 5)

	callerKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, callerUser.ID)

	body := map[string]any{
		"daily_token_limit":   float64(4_000),
		"monthly_token_limit": float64(90_000),
		"requests_per_minute": float64(12),
		"requests_per_day":    float64(120),
	}
	req := httptest.NewRequest("PATCH", keyItemURL(org.ID, target.ID), bodyJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+callerKey)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test PATCH: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH status = %d, want 200; body: %s", resp.StatusCode, b)
	}

	cached, ok := keyCache.Get(target.KeyHash)
	if !ok {
		t.Fatal("target key missing from cache after PATCH, want present")
	}

	rec, err := database.LoadActiveKey(ctx, target.ID)
	if err != nil {
		t.Fatalf("LoadActiveKey: %v", err)
	}
	want, ok := auth.KeyInfoFromRecord(*rec)
	if !ok {
		t.Fatal("KeyInfoFromRecord(rec) ok = false, want true for a user_key with a surviving membership")
	}

	if !reflect.DeepEqual(cached, want) {
		t.Errorf("cached KeyInfo = %+v,\nwant (fresh KeyInfoFromRecord) = %+v", cached, want)
	}

	// Spot-check the fields that matter operationally, in case the struct
	// grows a field DeepEqual would silently pass on if both sides forgot it.
	if cached.DailyTokenLimit != 4_000 {
		t.Errorf("cached.DailyTokenLimit = %d, want 4000", cached.DailyTokenLimit)
	}
	if cached.MonthlyTokenLimit != 90_000 {
		t.Errorf("cached.MonthlyTokenLimit = %d, want 90000", cached.MonthlyTokenLimit)
	}
	if cached.RequestsPerMinute != 12 {
		t.Errorf("cached.RequestsPerMinute = %d, want 12", cached.RequestsPerMinute)
	}
	if cached.RequestsPerDay != 120 {
		t.Errorf("cached.RequestsPerDay = %d, want 120", cached.RequestsPerDay)
	}
	if cached.UserRequestsPerDay != rpd {
		t.Errorf("cached.UserRequestsPerDay = %d, want %d (owner's own membership limit)", cached.UserRequestsPerDay, rpd)
	}
}
