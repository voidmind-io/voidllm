package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

func strp(s string) *string { return &s }

// TestCanRotateAPIKey is the authorization table for rotation scope.
func TestCanRotateAPIKey(t *testing.T) {
	t.Parallel()

	const (
		teamX  = "team-x"
		teamY  = "team-y"
		caller = "user-caller"
		other  = "user-other"
	)

	teamKeyX := &db.APIKey{KeyType: keygen.KeyTypeTeam, TeamID: strp(teamX)}
	teamKeyY := &db.APIKey{KeyType: keygen.KeyTypeTeam, TeamID: strp(teamY)}

	tests := []struct {
		name   string
		caller auth.KeyInfo
		key    *db.APIKey
		want   bool
	}{
		{
			name:   "org_admin rotates any key",
			caller: auth.KeyInfo{Role: auth.RoleOrgAdmin, UserID: caller},
			key:    &db.APIKey{KeyType: keygen.KeyTypeUser, UserID: strp(other)},
			want:   true,
		},
		{
			name:   "org_admin rotates sa_key",
			caller: auth.KeyInfo{Role: auth.RoleOrgAdmin, UserID: caller},
			key:    &db.APIKey{KeyType: keygen.KeyTypeSA},
			want:   true,
		},
		{
			name:   "member rotates own user_key",
			caller: auth.KeyInfo{Role: auth.RoleMember, UserID: caller},
			key:    &db.APIKey{KeyType: keygen.KeyTypeUser, UserID: strp(caller)},
			want:   true,
		},
		{
			name:   "team_key caller (team_admin) rotates team_key of own team",
			caller: auth.KeyInfo{KeyType: keygen.KeyTypeTeam, Role: auth.RoleTeamAdmin, TeamID: teamX},
			key:    teamKeyX,
			want:   true,
		},
		{
			name:   "team_key caller rotates team_key of sibling team",
			caller: auth.KeyInfo{KeyType: keygen.KeyTypeTeam, Role: auth.RoleTeamAdmin, TeamID: teamX},
			key:    teamKeyY,
			want:   false,
		},
		{
			name:   "team_key caller rotates another user's user_key carrying own team_id",
			caller: auth.KeyInfo{KeyType: keygen.KeyTypeTeam, Role: auth.RoleTeamAdmin, TeamID: teamX},
			key:    &db.APIKey{KeyType: keygen.KeyTypeUser, UserID: strp(other), TeamID: strp(teamX)},
			want:   false,
		},
		{
			name:   "team_admin user_key caller rotates another user's key carrying own team_id (that user is org_admin)",
			caller: auth.KeyInfo{KeyType: keygen.KeyTypeUser, Role: auth.RoleTeamAdmin, TeamID: teamX, UserID: caller},
			key:    &db.APIKey{KeyType: keygen.KeyTypeUser, UserID: strp(other), TeamID: strp(teamX)},
			want:   false,
		},
		{
			name:   "team_key caller rotates sa_key of own team",
			caller: auth.KeyInfo{KeyType: keygen.KeyTypeTeam, Role: auth.RoleTeamAdmin, TeamID: teamX},
			key:    &db.APIKey{KeyType: keygen.KeyTypeSA, TeamID: strp(teamX), ServiceAccountID: strp("sa-1")},
			want:   false,
		},
		{
			name:   "member with user_key carrying team X rotates team X's team_key",
			caller: auth.KeyInfo{KeyType: keygen.KeyTypeUser, Role: auth.RoleMember, TeamID: teamX, UserID: caller},
			key:    teamKeyX,
			want:   false,
		},
		{
			name:   "team-bound sa_key caller rotates its team's team_key",
			caller: auth.KeyInfo{KeyType: keygen.KeyTypeSA, Role: auth.RoleTeamAdmin, TeamID: teamX, ServiceAccountID: "sa-1"},
			key:    teamKeyX,
			want:   true,
		},
		{
			name:   "team-bound sa_key caller rotates sibling team's team_key",
			caller: auth.KeyInfo{KeyType: keygen.KeyTypeSA, Role: auth.RoleTeamAdmin, TeamID: teamX, ServiceAccountID: "sa-1"},
			key:    teamKeyY,
			want:   false,
		},
		{
			name:   "caller without team context cannot rotate a team_key with nil team",
			caller: auth.KeyInfo{KeyType: keygen.KeyTypeTeam, Role: auth.RoleTeamAdmin},
			key:    &db.APIKey{KeyType: keygen.KeyTypeTeam},
			want:   false,
		},
		{
			name:   "machine caller with empty UserID does not match a key with nil user",
			caller: auth.KeyInfo{KeyType: keygen.KeyTypeSA, Role: auth.RoleMember},
			key:    &db.APIKey{KeyType: keygen.KeyTypeUser},
			want:   false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			caller := tc.caller
			if got := admin.CanRotateAPIKey(&caller, tc.key); got != tc.want {
				t.Errorf("CanRotateAPIKey() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestRotateAPIKey_TeamKeyCallerScope is the handler-level regression for the
// rotation privilege escalation: a team_key caller must not be able to rotate
// (and thereby obtain the plaintext of) another user's team-scoped key.
func TestRotateAPIKey_TeamKeyCallerScope(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestRotateAPIKey_TeamKeyCallerScope?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "O", "rot-scope-org")
	teamA := mustCreateTeam(t, database, org.ID, "A", "rot-scope-a")
	teamB := mustCreateTeam(t, database, org.ID, "B", "rot-scope-b")
	adminUser := mustCreateUser(t, database, "rot-scope-admin@example.com", "Admin")

	callerKey := addTestKeyInfo(t, keyCache, auth.KeyInfo{
		ID: "rot-scope-team-key", KeyType: keygen.KeyTypeTeam, Role: auth.RoleTeamAdmin,
		OrgID: org.ID, TeamID: teamA.ID,
	})

	adminUserKey := mustCreateAPIKeyDirect(t, database, org.ID, &teamA.ID, &adminUser.ID, adminUser.ID, 0)
	ownTeamKey := mustCreateTeamKeyDirect(t, database, org.ID, teamA.ID, adminUser.ID, 0)
	siblingTeamKey := mustCreateTeamKeyDirect(t, database, org.ID, teamB.ID, adminUser.ID, 0)

	countKeys := func(t *testing.T) int {
		t.Helper()
		keys, err := database.ListAPIKeys(ctx, org.ID, "", "", "", 100, true)
		if err != nil {
			t.Fatalf("ListAPIKeys: %v", err)
		}
		return len(keys)
	}

	rotate := func(t *testing.T, keyID string) (int, string) {
		t.Helper()
		req := httptest.NewRequest("POST", rotateKeyURL(org.ID, keyID), nil)
		req.Header.Set("Authorization", "Bearer "+callerKey)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	t.Run("another user's team-scoped user_key is rejected with no new key and no plaintext", func(t *testing.T) {
		before := countKeys(t)
		status, body := rotate(t, adminUserKey.ID)
		if status != fiber.StatusForbidden {
			t.Fatalf("status = %d, want 403; body: %s", status, body)
		}
		if strings.Contains(body, keygen.PrefixUser) || strings.Contains(body, `"key"`) {
			t.Errorf("403 body leaks key material: %s", body)
		}
		if after := countKeys(t); after != before {
			t.Errorf("key rows = %d after rejected rotate, want %d", after, before)
		}
		stored, err := database.GetAPIKey(ctx, adminUserKey.ID)
		if err != nil {
			t.Fatalf("GetAPIKey: %v", err)
		}
		if stored.ExpiresAt != nil {
			t.Errorf("target key ExpiresAt = %v after rejected rotate, want nil (untouched)", *stored.ExpiresAt)
		}
	})

	t.Run("sibling team's team_key is rejected", func(t *testing.T) {
		before := countKeys(t)
		status, body := rotate(t, siblingTeamKey.ID)
		if status != fiber.StatusForbidden {
			t.Fatalf("status = %d, want 403; body: %s", status, body)
		}
		if after := countKeys(t); after != before {
			t.Errorf("key rows = %d, want %d", after, before)
		}
	})

	t.Run("team_admin user_key caller with team context rotates own team's team_key", func(t *testing.T) {
		userCaller := addTestKeyInfo(t, keyCache, auth.KeyInfo{
			ID: "rot-scope-team-admin-user", KeyType: keygen.KeyTypeUser, Role: auth.RoleTeamAdmin,
			OrgID: org.ID, TeamID: teamA.ID, UserID: adminUser.ID,
		})
		target := mustCreateTeamKeyDirect(t, database, org.ID, teamA.ID, adminUser.ID, 0)
		req := httptest.NewRequest("POST", rotateKeyURL(org.ID, target.ID), nil)
		req.Header.Set("Authorization", "Bearer "+userCaller)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Errorf("status = %d, want 200; body: %s", resp.StatusCode, b)
		}
	})

	t.Run("own team's team_key is rotated (machine caller without user_id)", func(t *testing.T) {
		before := countKeys(t)
		status, body := rotate(t, ownTeamKey.ID)
		if status != fiber.StatusOK {
			t.Fatalf("status = %d, want 200; body: %s", status, body)
		}
		if !strings.Contains(body, keygen.PrefixTeam) {
			t.Errorf("body = %s, want new team key plaintext", body)
		}
		if after := countKeys(t); after != before+1 {
			t.Errorf("key rows = %d, want %d", after, before+1)
		}
	})
}

// TestDeleteTeam_EvictsTeamKeysFromCache verifies DeleteTeam immediately
// evicts team keys and team-bound service-account keys of that team, leaving
// other keys cached, and that a reload does not resurrect them.
func TestDeleteTeam_EvictsTeamKeysFromCache(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestDeleteTeam_EvictsTeamKeys?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "O", "team-evict-org")
	team := mustCreateTeam(t, database, org.ID, "T", "team-evict-t")
	otherTeam := mustCreateTeam(t, database, org.ID, "T2", "team-evict-t2")
	creator := mustCreateUser(t, database, "team-evict-creator@example.com", "Creator")
	adminUser := mustCreateUser(t, database, "team-evict-admin@example.com", "Admin")

	hashOf := func(k *db.APIKey) string { return k.KeyHash }
	teamKey := mustCreateTeamKeyDirect(t, database, org.ID, team.ID, creator.ID, 0)
	otherTeamKey := mustCreateTeamKeyDirect(t, database, org.ID, otherTeam.ID, creator.ID, 0)

	saTeamPlain, saTeam := mustCreateSAKeyCallerViaDB(t, database, keyCache, org.ID, &team.ID, creator.ID)
	saOrgPlain, _ := mustCreateSAKeyCallerViaDB(t, database, keyCache, org.ID, nil, creator.ID)
	saTeamHash := keygen.Hash(saTeamPlain, testHMACSecret)
	saOrgHash := keygen.Hash(saOrgPlain, testHMACSecret)
	_ = saTeam

	callerKey := mustCreateOrgAdminCallerKey(t, database, keyCache, org.ID, adminUser.ID)

	for name, h := range map[string]string{
		"team_key": hashOf(teamKey), "other team_key": hashOf(otherTeamKey),
		"team sa_key": saTeamHash, "org sa_key": saOrgHash,
	} {
		if _, ok := keyCache.Get(h); !ok {
			t.Fatalf("%s not in cache before DeleteTeam", name)
		}
	}

	req := httptest.NewRequest("DELETE", teamItemURL(org.ID, team.ID), nil)
	req.Header.Set("Authorization", "Bearer "+callerKey)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusNoContent {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 204; body: %s", resp.StatusCode, b)
	}

	assertState := func(t *testing.T) {
		t.Helper()
		tests := []struct {
			name   string
			hash   string
			cached bool
		}{
			{"team_key evicted", hashOf(teamKey), false},
			{"team-bound sa_key evicted", saTeamHash, false},
			{"other team's team_key kept", hashOf(otherTeamKey), true},
			{"org-level sa_key kept", saOrgHash, true},
		}
		for _, tc := range tests {
			if _, ok := keyCache.Get(tc.hash); ok != tc.cached {
				t.Errorf("%s: cached = %v, want %v", tc.name, ok, tc.cached)
			}
		}
	}

	t.Run("immediately after delete", assertState)

	t.Run("after full reload", func(t *testing.T) {
		if err := auth.LoadKeysIntoCache(ctx, database, keyCache, noopLogger(t)); err != nil {
			t.Fatalf("LoadKeysIntoCache: %v", err)
		}
		assertState(t)
	})

	t.Run("evicted team key gets 401", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/v1/me", nil)
		req.Header.Set("Authorization", "Bearer "+saTeamPlain)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != fiber.StatusUnauthorized {
			t.Errorf("status = %d, want 401", resp.StatusCode)
		}
	})
}

// TestMCPProxy_TeamBoundServiceAccountKey verifies MCP access for a
// team-bound sa_key: team allowlist applies to global servers, and the team's
// own team-scoped server resolves by alias.
func TestMCPProxy_TeamBoundServiceAccountKey(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	}))
	t.Cleanup(upstream.Close)

	app, database, keyCache := setupMCPProxyApp(t, "file:TestMCPProxy_TeamBoundSAKey?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateTestOrg(t, database, "mcp-sa-team")
	team := mustCreateTeam(t, database, org.ID, "T", "mcp-sa-team-t")
	creator := mustCreateUser(t, database, "mcp-sa-team@example.com", "Creator")

	allowed := createExternalMCPServer(t, database, "global-allowed", upstream.URL)
	denied := createExternalMCPServer(t, database, "global-denied", upstream.URL)
	if err := database.SetOrgMCPAccess(ctx, org.ID, []string{allowed, denied}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}
	if err := database.SetTeamMCPAccess(ctx, team.ID, []string{allowed}); err != nil {
		t.Fatalf("SetTeamMCPAccess: %v", err)
	}
	if _, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name: "Team Only", Alias: "team-only", URL: upstream.URL, AuthType: "none",
		OrgID: &org.ID, TeamID: &team.ID,
	}); err != nil {
		t.Fatalf("CreateMCPServer team-scoped: %v", err)
	}

	teamSAKey, _ := mustCreateSAKeyCallerViaDB(t, database, keyCache, org.ID, &team.ID, creator.ID)
	orgSAKey, _ := mustCreateSAKeyCallerViaDB(t, database, keyCache, org.ID, nil, creator.ID)

	const body = `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`

	tests := []struct {
		name  string
		key   string
		alias string
		want  int
	}{
		{"team sa_key denied global server outside team allowlist", teamSAKey, "global-denied", fiber.StatusForbidden},
		{"team sa_key allowed global server in team allowlist", teamSAKey, "global-allowed", fiber.StatusOK},
		{"team sa_key resolves own team-scoped server", teamSAKey, "team-only", fiber.StatusOK},
		{"org sa_key (no team) cannot see team-scoped server", orgSAKey, "team-only", fiber.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := proxyPost(t, app, tc.alias, tc.key, body)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.want, b)
			}
		})
	}
}
