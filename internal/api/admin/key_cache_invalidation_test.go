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
		// Registered after the database cleanup, so it runs before it.
		t.Cleanup(handler.KeyReloader.Stop)
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

func TestUpdateTeam_LimitChangeRoleMatrix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		body       map[string]any
		wantStatus int
		wantDaily  int64 // expected stored daily limit afterwards
	}{
		{"team_admin renames", auth.RoleTeamAdmin, map[string]any{"name": "Renamed"}, fiber.StatusOK, 100},
		{"team_admin changes daily limit", auth.RoleTeamAdmin, map[string]any{"daily_token_limit": 999}, fiber.StatusForbidden, 100},
		{"team_admin raises monthly limit", auth.RoleTeamAdmin, map[string]any{"monthly_token_limit": 5000}, fiber.StatusForbidden, 100},
		{"team_admin changes rpm", auth.RoleTeamAdmin, map[string]any{"requests_per_minute": 99}, fiber.StatusForbidden, 100},
		{"team_admin changes rpd", auth.RoleTeamAdmin, map[string]any{"requests_per_day": 99}, fiber.StatusForbidden, 100},
		{"team_admin removes limit", auth.RoleTeamAdmin, map[string]any{"daily_token_limit": 0}, fiber.StatusForbidden, 100},
		{"team_admin mixed rename and limit", auth.RoleTeamAdmin, map[string]any{"name": "X", "daily_token_limit": 1}, fiber.StatusForbidden, 100},
		{"team_admin resends unchanged limits", auth.RoleTeamAdmin, map[string]any{
			"name": "Same", "daily_token_limit": 100, "monthly_token_limit": 2000, "requests_per_minute": 10, "requests_per_day": 50,
		}, fiber.StatusOK, 100},
		{"org_admin changes limits", auth.RoleOrgAdmin, map[string]any{"daily_token_limit": 999}, fiber.StatusOK, 999},
		{"system_admin changes limits", auth.RoleSystemAdmin, map[string]any{"daily_token_limit": 999}, fiber.StatusOK, 999},
	}

	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dsn := fmt.Sprintf("file:TestTeamLimitMatrix_%d?mode=memory&cache=private", i)
			app, database, keyCache := setupReloaderApp(t, dsn, true)

			org := mustCreateOrg(t, database, "Matrix Org", fmt.Sprintf("matrix-org-%d", i))
			team, err := database.CreateTeam(context.Background(), db.CreateTeamParams{
				OrgID: org.ID, Name: "Team", Slug: fmt.Sprintf("matrix-team-%d", i),
				DailyTokenLimit: 100, MonthlyTokenLimit: 2000, RequestsPerMinute: 10, RequestsPerDay: 50,
			})
			if err != nil {
				t.Fatalf("CreateTeam: %v", err)
			}
			caller := mustCreateCacheTestUser(t, database, fmt.Sprintf("caller-%d@example.com", i))
			if _, err := database.CreateTeamMembership(context.Background(), db.CreateTeamMembershipParams{
				TeamID: team.ID, UserID: caller.ID, Role: auth.RoleTeamAdmin,
			}); err != nil {
				t.Fatalf("CreateTeamMembership: %v", err)
			}
			key := addTestKeyWithTeam(t, keyCache, tc.role, org.ID, team.ID, caller.ID)

			got := doJSON(t, app, "PATCH", teamItemURL(org.ID, team.ID), key, tc.body)
			if got != tc.wantStatus {
				t.Errorf("status = %d, want %d", got, tc.wantStatus)
			}

			after, err := database.GetTeam(context.Background(), team.ID)
			if err != nil {
				t.Fatalf("GetTeam: %v", err)
			}
			if after.DailyTokenLimit != tc.wantDaily {
				t.Errorf("stored daily limit = %d, want %d", after.DailyTokenLimit, tc.wantDaily)
			}
			if tc.wantStatus == fiber.StatusForbidden && after.Name != "Team" {
				t.Errorf("rejected request still changed the name to %q", after.Name)
			}
		})
	}
}

// TestUpdateTeam_TeamAdminNeverWritesLimits covers the interleaving in which
// an org admin changes a team's limits between the moment a team admin's
// PATCH reads the team and the moment it writes: the team admin re-sends the
// limits it saw, which pass the "unchanged" check against the stale read, and
// must still not be written.
func TestUpdateTeam_TeamAdminNeverWritesLimits(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	_, database, _ := setupReloaderApp(t, "file:TestTeamAdminNeverWritesLimits?mode=memory&cache=private", false)
	org := mustCreateOrg(t, database, "Stale Org", "stale-limits-org")
	team, err := database.CreateTeam(ctx, db.CreateTeamParams{
		OrgID: org.ID, Name: "Team", Slug: "stale-limits-team",
		DailyTokenLimit: 100, MonthlyTokenLimit: 2000, RequestsPerMinute: 10, RequestsPerDay: 50,
	})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}

	// The team admin's request: a rename that re-sends the limits it loaded.
	name := "Renamed"
	daily, monthly := int64(100), int64(2000)
	rpm, rpd := 10, 50
	req := admin.TeamUpdateRequest{
		Name: &name, DailyTokenLimit: &daily, MonthlyTokenLimit: &monthly,
		RequestsPerMinute: &rpm, RequestsPerDay: &rpd,
	}

	// The handler reads the team (still the old limits) ...
	read, err := database.GetTeam(ctx, team.ID)
	if err != nil {
		t.Fatalf("GetTeam: %v", err)
	}
	// ... then an org admin lowers every limit ...
	newDaily, newMonthly := int64(5), int64(50)
	newRPM, newRPD := 1, 2
	if _, err := database.UpdateTeam(ctx, team.ID, db.UpdateTeamParams{
		DailyTokenLimit: &newDaily, MonthlyTokenLimit: &newMonthly,
		RequestsPerMinute: &newRPM, RequestsPerDay: &newRPD,
	}); err != nil {
		t.Fatalf("org admin UpdateTeam: %v", err)
	}
	// ... and only now does the team admin's write happen.
	params, allowed := admin.TeamUpdateParams(auth.RoleTeamAdmin, read, req)
	if !allowed {
		t.Fatal("re-sending the limits that were read was rejected, want allowed")
	}
	if params.DailyTokenLimit != nil || params.MonthlyTokenLimit != nil ||
		params.RequestsPerMinute != nil || params.RequestsPerDay != nil {
		t.Errorf("team admin write carries limit fields: %+v", params)
	}
	if params.Name == nil || *params.Name != name {
		t.Errorf("team admin write lost the rename: %+v", params)
	}
	if _, err := database.UpdateTeam(ctx, team.ID, params); err != nil {
		t.Fatalf("team admin UpdateTeam: %v", err)
	}

	after, err := database.GetTeam(ctx, team.ID)
	if err != nil {
		t.Fatalf("GetTeam: %v", err)
	}
	if after.Name != name {
		t.Errorf("name = %q, want %q", after.Name, name)
	}
	if after.DailyTokenLimit != newDaily || after.MonthlyTokenLimit != newMonthly ||
		after.RequestsPerMinute != newRPM || after.RequestsPerDay != newRPD {
		t.Errorf("limits after the team admin's write = %d/%d/%d/%d, want the org admin's %d/%d/%d/%d",
			after.DailyTokenLimit, after.MonthlyTokenLimit, after.RequestsPerMinute, after.RequestsPerDay,
			newDaily, newMonthly, newRPM, newRPD)
	}

	// Org admins and above do write limits; a team admin changing one is refused.
	for _, role := range []string{auth.RoleOrgAdmin, auth.RoleSystemAdmin} {
		p, ok := admin.TeamUpdateParams(role, read, req)
		if !ok || p.DailyTokenLimit == nil || p.MonthlyTokenLimit == nil || p.RequestsPerMinute == nil || p.RequestsPerDay == nil {
			t.Errorf("%s: allowed=%v params=%+v, want all limit fields written", role, ok, p)
		}
	}
	changed := int64(999)
	if _, ok := admin.TeamUpdateParams(auth.RoleTeamAdmin, read, admin.TeamUpdateRequest{DailyTokenLimit: &changed}); ok {
		t.Error("team admin changing a limit was allowed")
	}
}

// TestUpdateTeam_TeamAdminStaleLimitsOverHTTP sends a team admin PATCH whose
// limits were current when the client loaded the team but have since been
// changed by an org admin. The stored limits must stay the org admin's.
func TestUpdateTeam_TeamAdminStaleLimitsOverHTTP(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	app, database, keyCache := setupReloaderApp(t, "file:TestTeamAdminStaleLimitsHTTP?mode=memory&cache=private", true)
	org := mustCreateOrg(t, database, "Stale Org", "stale-http-org")
	team, err := database.CreateTeam(ctx, db.CreateTeamParams{
		OrgID: org.ID, Name: "Team", Slug: "stale-http-team",
		DailyTokenLimit: 100, MonthlyTokenLimit: 2000, RequestsPerMinute: 10, RequestsPerDay: 50,
	})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	caller := mustCreateCacheTestUser(t, database, "stale-http-caller@example.com")
	if _, err := database.CreateTeamMembership(ctx, db.CreateTeamMembershipParams{
		TeamID: team.ID, UserID: caller.ID, Role: auth.RoleTeamAdmin,
	}); err != nil {
		t.Fatalf("CreateTeamMembership: %v", err)
	}
	key := addTestKeyWithTeam(t, keyCache, auth.RoleTeamAdmin, org.ID, team.ID, caller.ID)

	// The org admin lowers the daily limit after the team admin loaded the team.
	lowered := int64(5)
	if _, err := database.UpdateTeam(ctx, team.ID, db.UpdateTeamParams{DailyTokenLimit: &lowered}); err != nil {
		t.Fatalf("org admin UpdateTeam: %v", err)
	}

	status := doJSON(t, app, "PATCH", teamItemURL(org.ID, team.ID), key, map[string]any{
		"name": "Renamed", "daily_token_limit": 100, "monthly_token_limit": 2000,
		"requests_per_minute": 10, "requests_per_day": 50,
	})
	if status != fiber.StatusForbidden {
		t.Errorf("status = %d, want 403 (the re-sent daily limit no longer matches)", status)
	}
	after, err := database.GetTeam(ctx, team.ID)
	if err != nil {
		t.Fatalf("GetTeam: %v", err)
	}
	if after.DailyTokenLimit != lowered {
		t.Errorf("stored daily limit = %d, want the org admin's %d", after.DailyTokenLimit, lowered)
	}
	if after.Name != "Team" {
		t.Errorf("name = %q, want unchanged", after.Name)
	}
}

func TestDeleteOrg_RemovesOrgKeysFromCache(t *testing.T) {
	t.Parallel()

	for _, mode := range reloaderModes() {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			suffix := strings.ReplaceAll(mode.name, " ", "-")
			dsn := fmt.Sprintf("file:TestDeleteOrgCache_%s?mode=memory&cache=private", strings.ReplaceAll(mode.name, " ", "_"))
			app, database, keyCache := setupReloaderApp(t, dsn, mode.withReloader)

			home := mustCreateOrg(t, database, "Home Org", "home-org-"+suffix)
			doomed := mustCreateOrg(t, database, "Doomed Org", "doomed-org-"+suffix)
			team := mustCreateTeam(t, database, doomed.ID, "Team", "doomed-team-"+suffix)
			member := mustCreateCacheTestUser(t, database, "doomed-member-"+suffix+"@example.com")
			if _, err := database.CreateOrgMembership(context.Background(), db.CreateOrgMembershipParams{
				OrgID: doomed.ID, UserID: member.ID, Role: auth.RoleMember,
			}); err != nil {
				t.Fatalf("CreateOrgMembership: %v", err)
			}
			userHash := mustCreateStoredKey(t, database, db.CreateAPIKeyParams{
				KeyType: keygen.KeyTypeUser, Name: "doomed user key", OrgID: doomed.ID, UserID: &member.ID, CreatedBy: member.ID,
			})
			teamHash := mustCreateStoredKey(t, database, db.CreateAPIKeyParams{
				KeyType: keygen.KeyTypeTeam, Name: "doomed team key", OrgID: doomed.ID, TeamID: &team.ID, CreatedBy: member.ID,
			})
			// Loads the cache from the database, including the keys above.
			adminKey := mustCreateSystemAdminCallerKey(t, database, keyCache, home.ID)
			adminHash := keygen.Hash(adminKey, testHMACSecret)
			for name, hash := range map[string]string{"user key": userHash, "team key": teamHash, "caller key": adminHash} {
				if _, ok := keyCache.Get(hash); !ok {
					t.Fatalf("precondition: %s not cached", name)
				}
			}

			if got := doJSON(t, app, "DELETE", "/api/v1/orgs/"+doomed.ID, adminKey, nil); got != fiber.StatusNoContent {
				t.Fatalf("DELETE status = %d, want 204", got)
			}

			if _, ok := keyCache.Get(userHash); ok {
				t.Error("user key of the deleted org is still cached")
			}
			if _, ok := keyCache.Get(teamHash); ok {
				t.Error("team key of the deleted org is still cached")
			}
			if _, ok := keyCache.Get(adminHash); !ok {
				t.Error("key of another org was dropped by the org deletion")
			}
		})
	}
}
