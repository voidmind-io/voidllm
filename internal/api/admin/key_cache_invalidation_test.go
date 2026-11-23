package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// setupReloaderApp is setupTestApp with an auth.Reloader wired into the
// handler, as in production. A nil reloader exercises the direct-reload path.
func setupReloaderApp(t *testing.T, dsn string, withReloader bool) (*fiber.App, *db.DB, *cache.Cache[string, auth.KeyInfo]) {
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

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	keyCache := cache.New[string, auth.KeyInfo]()
	handler := &admin.Handler{
		DB:         database,
		HMACSecret: testHMACSecret,
		KeyCache:   keyCache,
		License:    license.NewHolder(license.Verify("", true)),
		Log:        log,
	}
	if withReloader {
		handler.KeyReloader = auth.NewReloader(database, keyCache, log)
	}

	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)
	return app, database, keyCache
}

// mustCreateCacheTestUser creates a local user and fatals on error.
func mustCreateCacheTestUser(t *testing.T, database *db.DB, email string) *db.User {
	t.Helper()
	u, err := database.CreateUser(context.Background(), db.CreateUserParams{
		Email: email, DisplayName: email, AuthProvider: "local",
	})
	if err != nil {
		t.Fatalf("mustCreateCacheTestUser(%q): %v", email, err)
	}
	return u
}

// mustCreateStoredKey inserts a DB-backed key and returns its cache hash.
func mustCreateStoredKey(t *testing.T, database *db.DB, p db.CreateAPIKeyParams) string {
	t.Helper()
	plaintext, err := keygen.Generate(p.KeyType)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	p.KeyHash = keygen.Hash(plaintext, testHMACSecret)
	p.KeyHint = keygen.Hint(plaintext)
	if _, err := database.CreateAPIKey(context.Background(), p); err != nil {
		t.Fatalf("CreateAPIKey: %v", err)
	}
	return p.KeyHash
}

// doJSON issues an authenticated request and returns the status code.
func doJSON(t *testing.T, app *fiber.App, method, url, key string, body any) int {
	t.Helper()
	status, _ := doJSONBody(t, app, method, url, key, body)
	return status
}

// doJSONBody issues an authenticated request and returns status and body.
func doJSONBody(t *testing.T, app *fiber.App, method, url, key string, body any) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(method, url, bodyJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

func reloaderModes() []struct {
	name         string
	withReloader bool
} {
	return []struct {
		name         string
		withReloader bool
	}{
		{"with reloader", true},
		{"direct reload", false},
	}
}

func TestUpdateOrg_LimitVisibleInKeyCacheImmediately(t *testing.T) {
	t.Parallel()

	for _, mode := range reloaderModes() {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			dsn := fmt.Sprintf("file:TestUpdateOrgCache_%s?mode=memory&cache=private", strings.ReplaceAll(mode.name, " ", "_"))
			app, database, keyCache := setupReloaderApp(t, dsn, mode.withReloader)

			org := mustCreateOrg(t, database, "Cache Org", "cache-org-"+strings.ReplaceAll(mode.name, " ", "-"))
			admUser := mustCreateCacheTestUser(t, database, "adm-org@example.com")
			adminKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, admUser.ID)
			memberHash := mustCreateStoredKey(t, database, db.CreateAPIKeyParams{
				KeyType: keygen.KeyTypeUser, Name: "member", OrgID: org.ID, UserID: &admUser.ID, CreatedBy: admUser.ID,
			})
			if err := auth.LoadKeysIntoCache(context.Background(), database, keyCache, slog.Default()); err != nil {
				t.Fatalf("load: %v", err)
			}
			if ki, ok := keyCache.Get(memberHash); !ok || ki.OrgDailyTokenLimit != 0 {
				t.Fatalf("precondition: ok=%v limit=%d", ok, ki.OrgDailyTokenLimit)
			}

			if got := doJSON(t, app, "PATCH", "/api/v1/orgs/"+org.ID, adminKey,
				map[string]any{"daily_token_limit": 1234, "requests_per_minute": 9}); got != fiber.StatusOK {
				t.Fatalf("PATCH status = %d, want 200", got)
			}

			ki, ok := keyCache.Get(memberHash)
			if !ok {
				t.Fatal("key missing from cache after org update")
			}
			if ki.OrgDailyTokenLimit != 1234 || ki.OrgRequestsPerMinute != 9 {
				t.Errorf("org limits in cache = daily %d rpm %d, want 1234 and 9", ki.OrgDailyTokenLimit, ki.OrgRequestsPerMinute)
			}
		})
	}
}

func TestUpdateTeam_LimitVisibleInKeyCacheImmediately(t *testing.T) {
	t.Parallel()

	for _, mode := range reloaderModes() {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			dsn := fmt.Sprintf("file:TestUpdateTeamCache_%s?mode=memory&cache=private", strings.ReplaceAll(mode.name, " ", "_"))
			app, database, keyCache := setupReloaderApp(t, dsn, mode.withReloader)

			org := mustCreateOrg(t, database, "Cache Org", "cache-org-t-"+strings.ReplaceAll(mode.name, " ", "-"))
			team := mustCreateTeam(t, database, org.ID, "Team", "cache-team-"+strings.ReplaceAll(mode.name, " ", "-"))
			admUser := mustCreateCacheTestUser(t, database, "adm-team@example.com")
			adminKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, admUser.ID)
			teamHash := mustCreateStoredKey(t, database, db.CreateAPIKeyParams{
				KeyType: keygen.KeyTypeTeam, Name: "team key", OrgID: org.ID, TeamID: &team.ID, CreatedBy: admUser.ID,
			})
			if err := auth.LoadKeysIntoCache(context.Background(), database, keyCache, slog.Default()); err != nil {
				t.Fatalf("load: %v", err)
			}

			if got := doJSON(t, app, "PATCH", teamItemURL(org.ID, team.ID), adminKey,
				map[string]any{"monthly_token_limit": 777, "requests_per_day": 5}); got != fiber.StatusOK {
				t.Fatalf("PATCH status = %d, want 200", got)
			}

			ki, ok := keyCache.Get(teamHash)
			if !ok {
				t.Fatal("team key missing from cache after team update")
			}
			if ki.TeamMonthlyTokenLimit != 777 || ki.TeamRequestsPerDay != 5 {
				t.Errorf("team limits in cache = monthly %d rpd %d, want 777 and 5", ki.TeamMonthlyTokenLimit, ki.TeamRequestsPerDay)
			}
		})
	}
}

func TestOrgMembership_ChangesVisibleInKeyCacheImmediately(t *testing.T) {
	t.Parallel()

	for _, mode := range reloaderModes() {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			dsn := fmt.Sprintf("file:TestMembershipCache_%s?mode=memory&cache=private", strings.ReplaceAll(mode.name, " ", "_"))
			app, database, keyCache := setupReloaderApp(t, dsn, mode.withReloader)

			org := mustCreateOrg(t, database, "Cache Org", "cache-org-m-"+strings.ReplaceAll(mode.name, " ", "-"))
			admUser := mustCreateCacheTestUser(t, database, "adm-mem@example.com")
			adminKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, admUser.ID)

			// A user key without a membership is not cacheable.
			member := mustCreateCacheTestUser(t, database, "member-mem@example.com")
			memberHash := mustCreateStoredKey(t, database, db.CreateAPIKeyParams{
				KeyType: keygen.KeyTypeUser, Name: "member key", OrgID: org.ID, UserID: &member.ID, CreatedBy: member.ID,
			})
			if _, ok := keyCache.Get(memberHash); ok {
				t.Fatal("precondition: key cached without membership")
			}

			// Create: key becomes usable.
			status, raw := doJSONBody(t, app, "POST", "/api/v1/orgs/"+org.ID+"/members", adminKey,
				map[string]any{"user_id": member.ID, "role": auth.RoleMember})
			if status != fiber.StatusCreated {
				t.Fatalf("POST status = %d, want 201; body: %s", status, raw)
			}
			var created struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(raw, &created); err != nil {
				t.Fatalf("decode membership: %v", err)
			}
			if _, ok := keyCache.Get(memberHash); !ok {
				t.Fatal("key not cached right after membership create")
			}

			// Update: user limit visible.
			if got := doJSON(t, app, "PATCH", "/api/v1/orgs/"+org.ID+"/members/"+created.ID, adminKey,
				map[string]any{"daily_token_limit": 4321}); got != fiber.StatusOK {
				t.Fatalf("PATCH status = %d, want 200", got)
			}
			if ki, ok := keyCache.Get(memberHash); !ok || ki.UserDailyTokenLimit != 4321 {
				t.Errorf("after update: ok=%v user daily limit=%d, want 4321", ok, ki.UserDailyTokenLimit)
			}

			// Delete: key gone.
			if got := doJSON(t, app, "DELETE", "/api/v1/orgs/"+org.ID+"/members/"+created.ID, adminKey, nil); got != fiber.StatusNoContent {
				t.Fatalf("DELETE status = %d, want 204", got)
			}
			if _, ok := keyCache.Get(memberHash); ok {
				t.Error("key still cached after membership delete")
			}
		})
	}
}
