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

// TestDeleteServiceAccount_EvictsKeysFromCacheImmediately is the integration
// regression test for the sa_key cache-eviction fix on DeleteServiceAccount,
// mirroring the existing behavior of DeleteAPIKey: soft-deleting a service
// account must immediately evict every sa_key minted under it from the
// in-memory key cache — without waiting for the next periodic
// LoadKeysIntoCache reload — so any proxy or admin request authenticated
// with that key is rejected the instant the service account is gone, and a
// subsequent full cache reload must not resurrect it (auth.Cacheable skips
// sa_key rows whose service account is soft-deleted).
func TestDeleteServiceAccount_EvictsKeysFromCacheImmediately(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteSA_EvictsKeys?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "O", "sa-evict-org")
	admin := mustCreateUser(t, database, "sa-evict-admin@example.com", "Admin")
	sa := mustCreateServiceAccountHTTP(t, database, org.ID, admin.ID, "Evict Bot", nil)

	plaintext, err := keygen.Generate(keygen.KeyTypeSA)
	if err != nil {
		t.Fatalf("generate sa_key: %v", err)
	}
	saKey, err := database.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:          keygen.Hash(plaintext, testHMACSecret),
		KeyHint:          keygen.Hint(plaintext),
		KeyType:          keygen.KeyTypeSA,
		Name:             "evict-sa-key",
		OrgID:            org.ID,
		ServiceAccountID: &sa.ID,
		CreatedBy:        admin.ID,
	})
	if err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	keyHash := keygen.Hash(plaintext, testHMACSecret)

	// mustCreateOrgAdminCallerKey mints a real, DB-backed org_admin user_key for
	// admin and reloads the cache from the DB via auth.LoadKeysIntoCache — this
	// is the "load cache -> present" step for the sa_key too, since a single
	// LoadKeysIntoCache call loads every active key in the org, and gives us a
	// caller key that survives the cache reloads exercised later in this test
	// (a cache-only entry from addTestKey would not).
	callerKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, admin.ID)

	if _, ok := keyCache.Get(keyHash); !ok {
		t.Fatal("sa_key not present in cache after initial LoadKeysIntoCache, want present")
	}

	// Soft-delete the service account via the real admin DELETE endpoint.
	delReq := httptest.NewRequest("DELETE", saItemURL(org.ID, sa.ID), nil)
	delReq.Header.Set("Authorization", "Bearer "+callerKey)
	delResp, err := app.Test(delReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test DELETE service account: %v", err)
	}
	defer delResp.Body.Close()
	if delResp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(delResp.Body)
		t.Fatalf("DELETE service account status = %d, want 204; body: %s", delResp.StatusCode, b)
	}

	t.Run("key evicted from cache immediately, no reload needed", func(t *testing.T) {
		if _, ok := keyCache.Get(keyHash); ok {
			t.Error("sa_key still present in cache after DeleteServiceAccount, want evicted immediately")
		}
	})

	t.Run("request authenticated with the evicted key gets 401", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+plaintext)

		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusUnauthorized {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 401; body: %s", resp.StatusCode, b)
		}
	})

	t.Run("stays absent after a full LoadKeysIntoCache reload", func(t *testing.T) {
		if err := auth.LoadKeysIntoCache(ctx, database, keyCache, noopLogger(t)); err != nil {
			t.Fatalf("LoadKeysIntoCache: %v", err)
		}
		if _, ok := keyCache.Get(keyHash); ok {
			t.Error("sa_key reappeared in cache after full reload, want absent (Cacheable must reject it)")
		}
	})

	t.Run("RotateAPIKey on the key is rejected with 400", func(t *testing.T) {
		req := httptest.NewRequest("POST", keyItemURL(org.ID, saKey.ID)+"/rotate", nil)
		req.Header.Set("Authorization", "Bearer "+callerKey)

		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusBadRequest {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 400; body: %s", resp.StatusCode, b)
		}
	})
}
