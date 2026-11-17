package admin_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/proxy"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// rotateAccessEnv bundles the pieces of the rotation-access test harness.
type rotateAccessEnv struct {
	app            *fiber.App
	database       *db.DB
	keyCache       *cache.Cache[string, auth.KeyInfo]
	accessCache    *proxy.ModelAccessCache
	mcpAccessCache *proxy.MCPAccessCache
}

// newRotateAccessEnv opens a migrated in-memory database and an admin app wired
// with real model and MCP access caches.
func newRotateAccessEnv(t *testing.T, dsn string) rotateAccessEnv {
	t.Helper()

	ctx := context.Background()
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             dsn,
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	env := rotateAccessEnv{
		database:       database,
		keyCache:       cache.New[string, auth.KeyInfo](),
		accessCache:    proxy.NewModelAccessCache(),
		mcpAccessCache: proxy.NewMCPAccessCache(),
	}
	handler := &admin.Handler{
		DB:             database,
		HMACSecret:     testHMACSecret,
		EncryptionKey:  testEncryptionKey,
		KeyCache:       env.keyCache,
		AccessCache:    env.accessCache,
		MCPAccessCache: env.mcpAccessCache,
		License:        license.NewHolder(license.Verify("", true)),
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	env.app = fiber.New()
	admin.RegisterRoutes(env.app, handler, env.keyCache, testHMACSecret, nil)
	return env
}

// rotateOwnKeyWithAllowlists seeds a member with a key that has model and MCP
// allowlists, rotates that key as the member, and returns the new key's id and
// plaintext plus the allowed MCP server id.
func rotateOwnKeyWithAllowlists(t *testing.T, env rotateAccessEnv, slug string) (orgID, newID, newPlain, serverID string) {
	t.Helper()

	ctx := context.Background()
	database, keyCache, app := env.database, env.keyCache, env.app

	org := mustCreateOrg(t, database, "O", slug+"-org")
	member := mustCreateUser(t, database, slug+"-member@example.com", "Member")
	mustCreateMembership(t, database, org.ID, member.ID, auth.RoleMember)
	server := mustCreateGlobalMCPServer(t, database, "Rot Server", slug+"-server")

	oldKey := mustCreateAPIKeyDirect(t, database, org.ID, nil, &member.ID, member.ID, 0)
	if err := database.SetKeyModelAccess(ctx, oldKey.ID, []string{"allowed-model"}); err != nil {
		t.Fatalf("SetKeyModelAccess: %v", err)
	}
	// The MCP org level is closed by default, so the org must grant the server.
	if err := database.SetOrgMCPAccess(ctx, org.ID, []string{server.ID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}
	if err := database.SetKeyMCPAccess(ctx, oldKey.ID, []string{server.ID}); err != nil {
		t.Fatalf("SetKeyMCPAccess: %v", err)
	}

	callerKey := addTestKeyInfo(t, keyCache, auth.KeyInfo{
		ID: slug + "-caller", KeyType: keygen.KeyTypeUser, Role: auth.RoleMember,
		OrgID: org.ID, UserID: member.ID,
	})

	req := httptest.NewRequest("POST", rotateKeyURL(org.ID, oldKey.ID), nil)
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
		NewKey struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		} `json:"new_key"`
	}
	decodeBody(t, resp.Body, &got)
	if got.NewKey.ID == "" || got.NewKey.ID == oldKey.ID {
		t.Fatalf("new key id = %q, want a fresh id", got.NewKey.ID)
	}
	return org.ID, got.NewKey.ID, got.NewKey.Key, server.ID
}

// TestRotateAPIKey_CarriesKeyLevelAccess verifies that rotating a key with
// key-level model and MCP allowlists yields a new key with the same
// allowlists, both in the database and in the in-memory access caches, so the
// new key is not unrestricted (a missing key-level allowlist means open). The
// new key is also admitted to the key cache.
func TestRotateAPIKey_CarriesKeyLevelAccess(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := newRotateAccessEnv(t, "file:TestRotateAPIKey_CarriesKeyLevelAccess?mode=memory&cache=private")
	database, accessCache, mcpAccessCache := env.database, env.accessCache, env.mcpAccessCache
	orgID, newID, newPlain, serverID := rotateOwnKeyWithAllowlists(t, env, "rot-acc")

	if _, ok := env.keyCache.Get(keygen.Hash(newPlain, testHMACSecret)); !ok {
		t.Error("new key not in key cache after successful access cache reload")
	}

	models, err := database.GetKeyModelAccess(ctx, newID)
	if err != nil {
		t.Fatalf("GetKeyModelAccess: %v", err)
	}
	if !slices.Equal(models, []string{"allowed-model"}) {
		t.Errorf("new key models in DB = %v, want [allowed-model]", models)
	}
	servers, err := database.GetKeyMCPAccess(ctx, newID)
	if err != nil {
		t.Fatalf("GetKeyMCPAccess: %v", err)
	}
	if !slices.Equal(servers, []string{serverID}) {
		t.Errorf("new key servers in DB = %v, want [%s]", servers, serverID)
	}

	if !accessCache.Check(orgID, "", newID, "allowed-model") {
		t.Error("model cache: new key denied its allowed model")
	}
	if accessCache.Check(orgID, "", newID, "other-model") {
		t.Error("model cache: new key allowed a model outside its allowlist")
	}
	if !mcpAccessCache.Check(orgID, "", newID, serverID) {
		t.Error("mcp cache: new key denied its allowed server")
	}
	if mcpAccessCache.Check(orgID, "", newID, "some-other-server") {
		t.Error("mcp cache: new key allowed a server outside its allowlist")
	}
}

// TestRotateAPIKey_AccessReloadFailure_DoesNotCacheNewKey verifies the fail
// closed behavior: when the access-cache reload after rotation fails, the new
// key is not written to the key cache (the next periodic reload caches it), so
// it can never authenticate without its key-level allowlist. The reload is made
// to fail by dropping team_mcp_access, which the rotation transaction does not
// touch but LoadAllMCPAccess reads.
func TestRotateAPIKey_AccessReloadFailure_DoesNotCacheNewKey(t *testing.T) {
	t.Parallel()

	env := newRotateAccessEnv(t, "file:TestRotateAPIKey_AccessReloadFailure?mode=memory&cache=private")
	if _, err := env.database.SQL().ExecContext(context.Background(), "DROP TABLE team_mcp_access"); err != nil {
		t.Fatalf("drop team_mcp_access: %v", err)
	}

	_, newID, newPlain, _ := rotateOwnKeyWithAllowlists(t, env, "rot-fail")
	if newID == "" || newPlain == "" {
		t.Fatalf("rotation did not return a new key (id=%q)", newID)
	}
	if _, ok := env.keyCache.Get(keygen.Hash(newPlain, testHMACSecret)); ok {
		t.Error("new key was cached although the access cache reload failed")
	}
}
