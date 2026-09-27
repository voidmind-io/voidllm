package admin_test

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

// TestUpdateAPIKey_ExpiryIntoPastEvictsFromCacheImmediately is the regression
// test for the UpdateAPIKey cache-eviction fix: PATCHing expires_at into the
// past causes the subsequent db.LoadActiveKey (which excludes expired keys)
// to return db.ErrNotFound, which must evict the key from the in-memory cache
// right away — without waiting for the next periodic LoadKeysIntoCache reload
// — so the key stops authenticating immediately.
func TestUpdateAPIKey_ExpiryIntoPastEvictsFromCacheImmediately(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateAPIKey_ExpiryPastEvict?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "O", "updkey-evict-org")
	owner := mustCreateUser(t, database, "updkey-evict-owner@example.com", "Owner")
	callerUser := mustCreateUser(t, database, "updkey-evict-caller@example.com", "Caller")

	mustCreateMembership(t, database, org.ID, owner.ID, auth.RoleMember)

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate user_key: %v", err)
	}
	target, err := database.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "target-user-key",
		OrgID:     org.ID,
		UserID:    &owner.ID,
		CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	callerKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, callerUser.ID)

	// Sanity: the key is cached (via the initial LoadKeysIntoCache pulled in by
	// mustCreateOrgAdminCallerKey) and usable before the PATCH.
	if _, ok := keyCache.Get(target.KeyHash); !ok {
		t.Fatal("target user_key not present in cache before PATCH, want present")
	}
	if status := meRequest(t, app, plaintext); status != fiber.StatusOK {
		t.Fatalf("GET /api/v1/me before PATCH = %d, want 200", status)
	}

	body := map[string]any{"expires_at": "2020-01-01T00:00:00Z"}
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

	t.Run("key evicted from cache immediately, no reload needed", func(t *testing.T) {
		if _, ok := keyCache.Get(target.KeyHash); ok {
			t.Error("user_key still present in cache after expiring it via PATCH, want evicted immediately")
		}
	})

	t.Run("request authenticated with the expired key gets 401", func(t *testing.T) {
		if status := meRequest(t, app, plaintext); status != fiber.StatusUnauthorized {
			t.Errorf("GET /api/v1/me after PATCH = %d, want 401", status)
		}
	})
}

// TestUpdateAPIKey_ExpiryIntoFutureStaysCachedAndWorks verifies the
// non-eviction path: PATCHing expires_at to a future timestamp keeps the key
// cacheable, so the refreshed cache entry carries the new ExpiresAt and the
// key keeps authenticating successfully.
func TestUpdateAPIKey_ExpiryIntoFutureStaysCachedAndWorks(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateAPIKey_ExpiryFutureStays?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "O", "updkey-future-org")
	owner := mustCreateUser(t, database, "updkey-future-owner@example.com", "Owner")
	callerUser := mustCreateUser(t, database, "updkey-future-caller@example.com", "Caller")

	mustCreateMembership(t, database, org.ID, owner.ID, auth.RoleMember)

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate user_key: %v", err)
	}
	target, err := database.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "target-user-key",
		OrgID:     org.ID,
		UserID:    &owner.ID,
		CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	callerKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, callerUser.ID)

	const futureExpiry = "2099-01-01T00:00:00Z"
	body := map[string]any{"expires_at": futureExpiry}
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

	ki, ok := keyCache.Get(target.KeyHash)
	if !ok {
		t.Fatal("target user_key missing from cache after PATCH to a future expiry, want present")
	}
	if ki.ExpiresAt == nil {
		t.Fatal("cached KeyInfo.ExpiresAt is nil, want the new future expiry")
	}
	gotExpiry := ki.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z")
	if gotExpiry != futureExpiry {
		t.Errorf("cached KeyInfo.ExpiresAt = %s, want %s", gotExpiry, futureExpiry)
	}

	if status := meRequest(t, app, plaintext); status != fiber.StatusOK {
		t.Errorf("GET /api/v1/me after PATCH to future expiry = %d, want 200", status)
	}
}

// TestUpdateAPIKey_NameOnlyChangeStaysCachedWithUpdatedName verifies that
// patching only the name field (an unrelated, non-expiry field) keeps the key
// cacheable and refreshes the cached KeyInfo's Name.
func TestUpdateAPIKey_NameOnlyChangeStaysCachedWithUpdatedName(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateAPIKey_NameOnlyStays?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "O", "updkey-nameonly-org")
	owner := mustCreateUser(t, database, "updkey-nameonly-owner@example.com", "Owner")
	callerUser := mustCreateUser(t, database, "updkey-nameonly-caller@example.com", "Caller")

	mustCreateMembership(t, database, org.ID, owner.ID, auth.RoleMember)

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate user_key: %v", err)
	}
	target, err := database.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "original-name",
		OrgID:     org.ID,
		UserID:    &owner.ID,
		CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	callerKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, callerUser.ID)

	const newName = "renamed-key"
	body := map[string]any{"name": newName}
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

	ki, ok := keyCache.Get(target.KeyHash)
	if !ok {
		t.Fatal("target user_key missing from cache after name-only PATCH, want present")
	}
	if ki.Name != newName {
		t.Errorf("cached KeyInfo.Name = %q, want %q", ki.Name, newName)
	}

	if status := meRequest(t, app, plaintext); status != fiber.StatusOK {
		t.Errorf("GET /api/v1/me after name-only PATCH = %d, want 200", status)
	}
}

// TestUpdateAPIKey_NonCacheableRefreshEvictsFromCacheImmediately is the
// regression test for the UpdateAPIKey non-cacheable branch: a key whose
// refreshed LoadActiveKey record is no longer auth.Cacheable (because its
// owning membership row is gone) must be evicted from the cache right away
// by the PATCH handler itself. The membership is removed with a direct
// database.DeleteOrgMembership call rather than the admin DELETE endpoint,
// so the membership-deletion handler's own eviction never runs and the key
// stays cached going into the PATCH — isolating the UpdateAPIKey eviction
// path from the DeleteOrgMembership eviction path.
func TestUpdateAPIKey_NonCacheableRefreshEvictsFromCacheImmediately(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateAPIKey_NonCacheableEvict?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "O", "updkey-noncache-org")
	owner := mustCreateUser(t, database, "updkey-noncache-owner@example.com", "Owner")
	callerUser := mustCreateUser(t, database, "updkey-noncache-caller@example.com", "Caller")

	membership := mustCreateMembership(t, database, org.ID, owner.ID, auth.RoleMember)

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate user_key: %v", err)
	}
	target, err := database.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:   keygen.Hash(plaintext, testHMACSecret),
		KeyHint:   keygen.Hint(plaintext),
		KeyType:   keygen.KeyTypeUser,
		Name:      "target-user-key",
		OrgID:     org.ID,
		UserID:    &owner.ID,
		CreatedBy: owner.ID,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}

	callerKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, callerUser.ID)

	// Sanity: the key is cached and usable before the membership is removed.
	if _, ok := keyCache.Get(target.KeyHash); !ok {
		t.Fatal("target user_key not present in cache before membership removal, want present")
	}
	if status := meRequest(t, app, plaintext); status != fiber.StatusOK {
		t.Fatalf("GET /api/v1/me before membership removal = %d, want 200", status)
	}

	// Remove the membership directly in the DB, bypassing the admin DELETE
	// endpoint entirely, so its own eviction logic does not run and the key
	// remains cached.
	if err := database.DeleteOrgMembership(ctx, membership.ID); err != nil {
		t.Fatalf("DeleteOrgMembership: %v", err)
	}

	// Confirm the key is still cached: the membership removal above went
	// around the handler, so nothing has evicted it yet.
	if _, ok := keyCache.Get(target.KeyHash); !ok {
		t.Fatal("target user_key not present in cache after direct DB membership removal, want still present")
	}

	body := map[string]any{"name": "renamed"}
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

	t.Run("key evicted from cache immediately, no reload needed", func(t *testing.T) {
		if _, ok := keyCache.Get(target.KeyHash); ok {
			t.Error("user_key still present in cache after PATCH refreshed a non-cacheable record, want evicted immediately")
		}
	})

	t.Run("request authenticated with the evicted key gets 401", func(t *testing.T) {
		if status := meRequest(t, app, plaintext); status != fiber.StatusUnauthorized {
			t.Errorf("GET /api/v1/me after PATCH = %d, want 401", status)
		}
	})
}
