package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// addTestKeyWithTeam generates a user_key-typed cache entry scoped to both a
// team and a user, for exercising team_admin authorization paths that depend
// on TeamID.
func addTestKeyWithTeam(t *testing.T, keyCache *cache.Cache[string, auth.KeyInfo], role, orgID, teamID, userID string) string {
	t.Helper()

	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}

	hash := keygen.Hash(plaintext, testHMACSecret)
	keyCache.Set(hash, auth.KeyInfo{
		ID:      "test-key-id-" + role + "-team",
		KeyType: keygen.KeyTypeUser,
		Role:    role,
		OrgID:   orgID,
		TeamID:  teamID,
		UserID:  userID,
		Name:    "test key " + role,
	})

	return plaintext
}

// addTestKeyInfo registers a cache-only KeyInfo entry under a freshly
// generated key of ki.KeyType, filling in a default ID and Name when left
// blank. Unlike addTestKey/addTestKeyWithUser/addTestKeyWithTeam it gives the
// caller full control over every KeyInfo field — in particular KeyType (for
// session_key and sa_key callers) and ID (to simulate a machine caller acting
// on a key that is, or is not, itself — see canSetKeyLimits).
func addTestKeyInfo(t *testing.T, keyCache *cache.Cache[string, auth.KeyInfo], ki auth.KeyInfo) string {
	t.Helper()

	plaintext, err := keygen.Generate(ki.KeyType)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	if ki.ID == "" {
		ki.ID = "test-key-id-" + ki.KeyType + "-" + ki.Role
	}
	if ki.Name == "" {
		ki.Name = "test key " + ki.KeyType
	}

	keyCache.Set(keygen.Hash(plaintext, testHMACSecret), ki)

	return plaintext
}

// ---- POST .../keys — key-limit authorization matrix -------------------------

// TestCreateAPIKey_LimitAuthorization verifies that only org_admin/system_admin
// callers may set non-zero limit fields at creation time. member and
// team_admin callers (who are always forced to create a user_key for
// themselves) must send zero/omitted limits or be rejected.
func TestCreateAPIKey_LimitAuthorization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		role       string
		limits     map[string]any
		wantStatus int
	}{
		{name: "member zero limits allowed", role: auth.RoleMember, limits: nil, wantStatus: fiber.StatusCreated},
		{name: "member daily_token_limit forbidden", role: auth.RoleMember, limits: map[string]any{"daily_token_limit": float64(1000)}, wantStatus: fiber.StatusForbidden},
		{name: "member monthly_token_limit forbidden", role: auth.RoleMember, limits: map[string]any{"monthly_token_limit": float64(1000)}, wantStatus: fiber.StatusForbidden},
		{name: "member requests_per_minute forbidden", role: auth.RoleMember, limits: map[string]any{"requests_per_minute": float64(10)}, wantStatus: fiber.StatusForbidden},
		{name: "member requests_per_day forbidden", role: auth.RoleMember, limits: map[string]any{"requests_per_day": float64(10)}, wantStatus: fiber.StatusForbidden},
		{name: "team_admin zero limits allowed", role: auth.RoleTeamAdmin, limits: nil, wantStatus: fiber.StatusCreated},
		{name: "team_admin daily_token_limit forbidden", role: auth.RoleTeamAdmin, limits: map[string]any{"daily_token_limit": float64(1000)}, wantStatus: fiber.StatusForbidden},
		{name: "org_admin nonzero limits allowed", role: auth.RoleOrgAdmin, limits: map[string]any{"daily_token_limit": float64(1000), "requests_per_minute": float64(5)}, wantStatus: fiber.StatusCreated},
		{name: "system_admin nonzero limits allowed", role: auth.RoleSystemAdmin, limits: map[string]any{"monthly_token_limit": float64(5000)}, wantStatus: fiber.StatusCreated},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dsn := fmt.Sprintf("file:TestCreateAPIKey_LimitAuth_%s?mode=memory&cache=private",
				strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "O", "limauth-"+strings.ReplaceAll(tc.name, " ", "-"))
			user := mustCreateUser(t, database, "limauth-"+strings.ReplaceAll(tc.name, " ", "")+"@example.com", "U")
			team := mustCreateTeam(t, database, org.ID, "T", "t-limauth-"+strings.ReplaceAll(tc.name, " ", "-"))
			mustCreateUserMemberships(t, database, org.ID, team.ID, user.ID)

			var testKey string
			if tc.role == auth.RoleTeamAdmin {
				testKey = addTestKeyWithTeam(t, keyCache, tc.role, org.ID, team.ID, user.ID)
			} else {
				testKey = addTestKeyWithUser(t, keyCache, tc.role, org.ID, user.ID)
			}

			body := map[string]any{
				"name":     "Key",
				"key_type": keygen.KeyTypeUser,
				"team_id":  team.ID,
			}
			if auth.HasRole(tc.role, auth.RoleOrgAdmin) {
				// org admins/system admins are not forced to their own user_id.
				body["user_id"] = user.ID
			}
			for k, v := range tc.limits {
				body[k] = v
			}

			req := httptest.NewRequest("POST", keysURL(org.ID), bodyJSON(t, body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+testKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, b)
			}
		})
	}
}

// updateLimitTestEnv bundles everything a PATCH-limits test case needs to set
// up: the caller's key, the target key ID, and the DB handle for post-hoc
// verification that a rejected request made no write.
type updateLimitTestEnv struct {
	app       *fiber.App
	database  *db.DB
	callerKey string
	targetKey *db.APIKey
}

// TestUpdateAPIKey_LimitAuthorization is the authorization matrix for the
// PATCH path: changing a limit value requires
// org_admin/system_admin — human (user_key/session_key) or an org-level
// sa_key (sa_key with no team) resolving to org_admin. team_admin is never
// permitted, even on a team-scoped key within their own team or the team key
// acting on itself, since a team-scoped key has no distinct owner. An
// org-level sa_key may set limits on other keys in the org but never on
// itself or another key belonging to the same service account. Resending the
// unchanged value is always allowed so idempotent clients do not break. On
// every 403/400 the key's limits in the DB must remain exactly as they were
// before the request.
func TestUpdateAPIKey_LimitAuthorization(t *testing.T) {
	t.Parallel()

	const originalRPM = 7 // fixed starting value baked into every target key below

	tests := []struct {
		name       string
		setup      func(t *testing.T, database *db.DB, org *db.Org, team, otherTeam *db.Team, owner, other, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv
		body       map[string]any
		wantStatus int
		wantChange bool // whether requests_per_minute is expected to differ from originalRPM after the call
	}{
		{
			name: "member changes own key limit forbidden",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, _, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				callerKey := addTestKeyWithUser(t, keyCache, auth.RoleMember, org.ID, caller.ID)
				target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &caller.ID, caller.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusForbidden,
			wantChange: false,
		},
		{
			name: "member resends unchanged value on own key allowed",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, _, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				callerKey := addTestKeyWithUser(t, keyCache, auth.RoleMember, org.ID, caller.ID)
				target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &caller.ID, caller.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(originalRPM)},
			wantStatus: fiber.StatusOK,
			wantChange: false,
		},
		{
			name: "member sends negative value returns 400",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, _, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				callerKey := addTestKeyWithUser(t, keyCache, auth.RoleMember, org.ID, caller.ID)
				target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &caller.ID, caller.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(-5)},
			wantStatus: fiber.StatusBadRequest,
			wantChange: false,
		},
		{
			name: "team_admin changes own personal key forbidden",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, _, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				callerKey := addTestKeyWithTeam(t, keyCache, auth.RoleTeamAdmin, org.ID, team.ID, caller.ID)
				// The team_admin's own personal key, scoped to their team.
				target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &caller.ID, caller.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusForbidden,
			wantChange: false,
		},
		{
			// team_admin has no carve-out for a team-scoped key — a
			// team-scoped key has no distinct owner, so a team admin could
			// otherwise raise or remove the limits on a key they themselves use.
			name: "team_admin changes team_key of own team forbidden",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, _, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				callerKey := addTestKeyWithTeam(t, keyCache, auth.RoleTeamAdmin, org.ID, team.ID, caller.ID)
				target := mustCreateTeamKeyDirect(t, database, org.ID, team.ID, caller.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusForbidden,
			wantChange: false,
		},
		{
			// team_admin may not set limits on anyone's key, including a
			// key they can see and manage but do not personally own.
			name: "team_admin changes other team member's personal key forbidden",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, _, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				callerKey := addTestKeyWithTeam(t, keyCache, auth.RoleTeamAdmin, org.ID, team.ID, caller.ID)
				target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &owner.ID, owner.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusForbidden,
			wantChange: false,
		},
		{
			// A team_key credential itself (not a team_admin user acting on
			// it) authenticating and PATCHing its own limits. team_admin is
			// blocked unconditionally, so self-targeting adds no extra branch,
			// but this proves the team_key's own bearer token is treated the
			// same as any other team_admin caller.
			name: "team_key authenticating as itself forbidden",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, _, _ *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				target := mustCreateTeamKeyDirect(t, database, org.ID, team.ID, owner.ID, originalRPM)
				callerKey := addTestKeyInfo(t, keyCache, auth.KeyInfo{
					ID:      target.ID,
					KeyType: keygen.KeyTypeTeam,
					Role:    auth.RoleTeamAdmin,
					OrgID:   org.ID,
					TeamID:  team.ID,
				})
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusForbidden,
			wantChange: false,
		},
		{
			// A team-bound sa_key resolves to team_admin (KeyInfoFromRecord)
			// and is blocked the same way, including on itself.
			name: "team-bound sa_key authenticating as itself forbidden",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, _, _ *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				sa := mustCreateServiceAccountHTTP(t, database, org.ID, owner.ID, "TeamSA", &team.ID)
				target := mustCreateSAKeyDirect(t, database, org.ID, &team.ID, sa.ID, owner.ID, originalRPM)
				callerKey := addTestKeyInfo(t, keyCache, auth.KeyInfo{
					ID:               target.ID,
					KeyType:          keygen.KeyTypeSA,
					Role:             auth.RoleTeamAdmin,
					OrgID:            org.ID,
					TeamID:           team.ID,
					ServiceAccountID: sa.ID,
				})
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusForbidden,
			wantChange: false,
		},
		{
			// An org-level sa_key (KeyType sa_key, TeamID "") resolves to
			// org_admin and may set limits on another user's key.
			name: "org-level sa_key changes another user's key allowed",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, other, _ *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				sa := mustCreateServiceAccountHTTP(t, database, org.ID, owner.ID, "OrgSA", nil)
				callerRow := mustCreateSAKeyDirect(t, database, org.ID, nil, sa.ID, owner.ID, originalRPM)
				callerKey := addTestKeyInfo(t, keyCache, auth.KeyInfo{
					ID:               callerRow.ID,
					KeyType:          keygen.KeyTypeSA,
					Role:             auth.RoleOrgAdmin,
					OrgID:            org.ID,
					ServiceAccountID: sa.ID,
				})
				target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &other.ID, other.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusOK,
			wantChange: true,
		},
		{
			// An org-level sa_key may never change its own limits, so a
			// machine caller can never grant itself a higher or unlimited budget.
			name: "org-level sa_key changes own key forbidden",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, _, _ *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				sa := mustCreateServiceAccountHTTP(t, database, org.ID, owner.ID, "OrgSA", nil)
				target := mustCreateSAKeyDirect(t, database, org.ID, nil, sa.ID, owner.ID, originalRPM)
				callerKey := addTestKeyInfo(t, keyCache, auth.KeyInfo{
					ID:               target.ID,
					KeyType:          keygen.KeyTypeSA,
					Role:             auth.RoleOrgAdmin,
					OrgID:            org.ID,
					ServiceAccountID: sa.ID,
				})
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusForbidden,
			wantChange: false,
		},
		{
			// An org-level sa_key may never change the limits of a sibling
			// key that belongs to the same service account either.
			name: "org-level sa_key changes another key of same service account forbidden",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, _, _ *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				sa := mustCreateServiceAccountHTTP(t, database, org.ID, owner.ID, "OrgSA", nil)
				callerRow := mustCreateSAKeyDirect(t, database, org.ID, nil, sa.ID, owner.ID, originalRPM)
				callerKey := addTestKeyInfo(t, keyCache, auth.KeyInfo{
					ID:               callerRow.ID,
					KeyType:          keygen.KeyTypeSA,
					Role:             auth.RoleOrgAdmin,
					OrgID:            org.ID,
					ServiceAccountID: sa.ID,
				})
				target := mustCreateSAKeyDirect(t, database, org.ID, nil, sa.ID, owner.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusForbidden,
			wantChange: false,
		},
		{
			// Human callers (non-empty UserID) with org_admin include session
			// keys, not just user keys — canSetKeyLimits keys off UserID, not
			// KeyType.
			name: "org_admin session key changes another user's key allowed",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, _, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				callerKey := addTestKeyInfo(t, keyCache, auth.KeyInfo{
					KeyType: keygen.KeyTypeSession,
					Role:    auth.RoleOrgAdmin,
					OrgID:   org.ID,
					UserID:  caller.ID,
				})
				target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &owner.ID, owner.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusOK,
			wantChange: true,
		},
		{
			name: "org_admin session key changes own key allowed",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, _, _, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				callerKey := addTestKeyInfo(t, keyCache, auth.KeyInfo{
					KeyType: keygen.KeyTypeSession,
					Role:    auth.RoleOrgAdmin,
					OrgID:   org.ID,
					UserID:  caller.ID,
				})
				target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &caller.ID, caller.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusOK,
			wantChange: true,
		},
		{
			name: "team_admin cannot reach team_key of a different team",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, otherTeam *db.Team, owner, _, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				callerKey := addTestKeyWithTeam(t, keyCache, auth.RoleTeamAdmin, org.ID, team.ID, caller.ID)
				target := mustCreateTeamKeyDirect(t, database, org.ID, otherTeam.ID, owner.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusNotFound,
			wantChange: false,
		},
		{
			name: "team_admin cannot reach another user's key outside their team",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, otherTeam *db.Team, owner, other, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				callerKey := addTestKeyWithTeam(t, keyCache, auth.RoleTeamAdmin, org.ID, team.ID, caller.ID)
				target := mustCreateAPIKeyDirect(t, database, org.ID, &otherTeam.ID, &other.ID, other.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusNotFound,
			wantChange: false,
		},
		{
			name: "org_admin changes any key in org allowed",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, owner, _, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				callerKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, caller.ID)
				target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &owner.ID, owner.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusOK,
			wantChange: true,
		},
		{
			name: "org_admin changes own key allowed",
			setup: func(t *testing.T, database *db.DB, org *db.Org, team, _ *db.Team, _, _, caller *db.User, keyCache *cache.Cache[string, auth.KeyInfo]) updateLimitTestEnv {
				t.Helper()
				callerKey := addTestKeyWithUser(t, keyCache, auth.RoleOrgAdmin, org.ID, caller.ID)
				target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &caller.ID, caller.ID, originalRPM)
				return updateLimitTestEnv{database: database, callerKey: callerKey, targetKey: target}
			},
			body:       map[string]any{"requests_per_minute": float64(1)},
			wantStatus: fiber.StatusOK,
			wantChange: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			suffix := strings.ReplaceAll(tc.name, " ", "-")
			dsn := fmt.Sprintf("file:TestUpdateAPIKey_LimitAuth_%s?mode=memory&cache=private", strings.ReplaceAll(tc.name, " ", "_"))
			app, database, keyCache := setupTestApp(t, dsn)

			org := mustCreateOrg(t, database, "O", "updlim-org-"+suffix)
			team := mustCreateTeam(t, database, org.ID, "T", "updlim-team-"+suffix)
			otherTeam := mustCreateTeam(t, database, org.ID, "T2", "updlim-team2-"+suffix)
			owner := mustCreateUser(t, database, "updlim-owner-"+suffix+"@example.com", "Owner")
			other := mustCreateUser(t, database, "updlim-other-"+suffix+"@example.com", "Other")
			caller := mustCreateUser(t, database, "updlim-caller-"+suffix+"@example.com", "Caller")

			env := tc.setup(t, database, org, team, otherTeam, owner, other, caller, keyCache)
			env.app = app

			req := httptest.NewRequest("PATCH", keyItemURL(org.ID, env.targetKey.ID), bodyJSON(t, tc.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+env.callerKey)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.wantStatus, b)
			}

			// Reload the key directly from the DB and verify no unauthorized
			// write occurred: requests_per_minute must only differ from the
			// original when wantChange is true.
			reloaded, err := database.GetAPIKey(context.Background(), env.targetKey.ID)
			if err != nil {
				t.Fatalf("reload target key: %v", err)
			}
			changed := reloaded.RequestsPerMinute != originalRPM
			if changed != tc.wantChange {
				t.Errorf("requests_per_minute after request = %d (changed=%v), want changed=%v (original=%d)",
					reloaded.RequestsPerMinute, changed, tc.wantChange, originalRPM)
			}
		})
	}
}

// mustCreateAPIKeyDirect inserts a user_key directly via the DB layer
// (bypassing handler-level authorization) with the given org/team/user
// ownership and an initial requests_per_minute value.
func mustCreateAPIKeyDirect(t *testing.T, database *db.DB, orgID string, teamID, userID *string, createdBy string, rpm int) *db.APIKey {
	t.Helper()
	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("mustCreateAPIKeyDirect: generate: %v", err)
	}
	key, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:           keygen.Hash(plaintext, testHMACSecret),
		KeyHint:           keygen.Hint(plaintext),
		KeyType:           keygen.KeyTypeUser,
		Name:              "direct-user-key",
		OrgID:             orgID,
		TeamID:            teamID,
		UserID:            userID,
		RequestsPerMinute: rpm,
		CreatedBy:         createdBy,
	})
	if err != nil {
		t.Fatalf("mustCreateAPIKeyDirect: CreateAPIKey: %v", err)
	}
	return key
}

// mustCreateTeamKeyDirect inserts a team_key directly via the DB layer with an
// initial requests_per_minute value.
func mustCreateTeamKeyDirect(t *testing.T, database *db.DB, orgID, teamID, createdBy string, rpm int) *db.APIKey {
	t.Helper()
	plaintext, err := keygen.Generate(keygen.KeyTypeTeam)
	if err != nil {
		t.Fatalf("mustCreateTeamKeyDirect: generate: %v", err)
	}
	key, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:           keygen.Hash(plaintext, testHMACSecret),
		KeyHint:           keygen.Hint(plaintext),
		KeyType:           keygen.KeyTypeTeam,
		Name:              "direct-team-key",
		OrgID:             orgID,
		TeamID:            &teamID,
		RequestsPerMinute: rpm,
		CreatedBy:         createdBy,
	})
	if err != nil {
		t.Fatalf("mustCreateTeamKeyDirect: CreateAPIKey: %v", err)
	}
	return key
}

// mustCreateSAKeyDirect inserts an sa_key directly via the DB layer, scoped to
// the given service account and optional team, with an initial
// requests_per_minute value. A nil teamID produces an org-level sa_key.
func mustCreateSAKeyDirect(t *testing.T, database *db.DB, orgID string, teamID *string, serviceAccountID, createdBy string, rpm int) *db.APIKey {
	t.Helper()
	plaintext, err := keygen.Generate(keygen.KeyTypeSA)
	if err != nil {
		t.Fatalf("mustCreateSAKeyDirect: generate: %v", err)
	}
	key, err := database.CreateAPIKey(context.Background(), db.CreateAPIKeyParams{
		KeyHash:           keygen.Hash(plaintext, testHMACSecret),
		KeyHint:           keygen.Hint(plaintext),
		KeyType:           keygen.KeyTypeSA,
		Name:              "direct-sa-key",
		OrgID:             orgID,
		TeamID:            teamID,
		ServiceAccountID:  &serviceAccountID,
		RequestsPerMinute: rpm,
		CreatedBy:         createdBy,
	})
	if err != nil {
		t.Fatalf("mustCreateSAKeyDirect: CreateAPIKey: %v", err)
	}
	return key
}

// TestUpdateAPIKey_MemberNameChangeWithLimitField exercises the behavior that
// UpdateAPIKey nils out every limit field on the write for a caller who cannot set limits,
// so a limit value merely echoed back from a stale read can never be written
// to the DB by that caller — whether or not it happens to equal the current
// stored value.
//
// A nil'd-and-skipped SET clause and a SET clause that (re)writes the exact
// same value are indistinguishable from outside the request/response cycle,
// so this test instead proves the externally observable guarantee that
// depends on it: canSetKeyLimits gates the entire PATCH, not just the limit
// fields. Between the member's original read of the key (originalRPM) and
// their PATCH, an org_admin lowers the limit directly in the DB — simulating
// a concurrent admin change the member has not seen yet:
//
//   - The member echoes back the value they last knew (originalRPM), which no
//     longer matches the current stored value: limitChanged is true, so the
//     whole request is rejected with 403 — including the harmless name
//     change bundled in the same body. Without gating the whole request on
//     the limit change, a naive implementation might apply the name change
//     and only reject the limit field, silently diverging from what the
//     member asked for.
//   - The member catches up and echoes back the actual current value
//     (adminLoweredRPM): limitChanged is now false, so the request succeeds
//     and the name change is applied. The DB write for this
//     request carries a nil RequestsPerMinute (the column is left alone)
//     rather than adminLoweredRPM (a value the member never set and does not
//     own), which matters the instant a second admin write lands between
//     this member's GetAPIKey read and UpdateAPIKey write — a race this test
//     cannot reproduce deterministically over HTTP, but whose only
//     independently testable, deterministic consequence is exactly the
//     request-level atomicity asserted here.
func TestUpdateAPIKey_MemberNameChangeWithLimitField(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestUpdateAPIKey_MemberEchoF2?mode=memory&cache=private")

	org := mustCreateOrg(t, database, "O", "member-echo-f2-org")
	team := mustCreateTeam(t, database, org.ID, "T", "t-member-echo-f2")
	member := mustCreateUser(t, database, "member-echo-f2@example.com", "Member")

	const originalRPM = 7
	target := mustCreateAPIKeyDirect(t, database, org.ID, &team.ID, &member.ID, member.ID, originalRPM)

	memberKey := addTestKeyWithUser(t, keyCache, auth.RoleMember, org.ID, member.ID)

	// An org_admin lowers the limit directly in the DB, out from under the
	// member, who still believes the stored value is originalRPM.
	const adminLoweredRPM = 3
	loweredRPM := adminLoweredRPM
	if _, err := database.UpdateAPIKey(context.Background(), target.ID, db.UpdateAPIKeyParams{
		RequestsPerMinute: &loweredRPM,
	}); err != nil {
		t.Fatalf("simulate admin lowering limit directly in DB: %v", err)
	}

	// The member's PATCH echoes back the stale value (originalRPM) alongside
	// a name change. Because the echoed value differs from what is actually
	// stored, this is treated as a forbidden limit change and the whole
	// request — including the accompanying name change — is rejected.
	staleReq := httptest.NewRequest("PATCH", keyItemURL(org.ID, target.ID),
		bodyJSON(t, map[string]any{"name": "Renamed By Member", "requests_per_minute": float64(originalRPM)}))
	staleReq.Header.Set("Content-Type", "application/json")
	staleReq.Header.Set("Authorization", "Bearer "+memberKey)

	staleResp, err := app.Test(staleReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test (stale echo): %v", err)
	}
	defer staleResp.Body.Close()
	if staleResp.StatusCode != fiber.StatusForbidden {
		b, _ := io.ReadAll(staleResp.Body)
		t.Fatalf("stale echo PATCH status = %d, want 403; body: %s", staleResp.StatusCode, b)
	}

	afterStale, err := database.GetAPIKey(context.Background(), target.ID)
	if err != nil {
		t.Fatalf("reload after stale echo PATCH: %v", err)
	}
	if afterStale.RequestsPerMinute != adminLoweredRPM {
		t.Errorf("RequestsPerMinute after rejected stale PATCH = %d, want %d (unchanged)", afterStale.RequestsPerMinute, adminLoweredRPM)
	}
	if afterStale.Name != target.Name {
		t.Errorf("Name after rejected stale PATCH = %q, want unchanged %q — a forbidden limit change must reject the whole request", afterStale.Name, target.Name)
	}

	// The member catches up: the requests_per_minute value they submit now
	// equals the actual current stored value, so canSetKeyLimits is never
	// consulted for a "change" and the request succeeds in full.
	freshReq := httptest.NewRequest("PATCH", keyItemURL(org.ID, target.ID),
		bodyJSON(t, map[string]any{"name": "Renamed By Member", "requests_per_minute": float64(adminLoweredRPM)}))
	freshReq.Header.Set("Content-Type", "application/json")
	freshReq.Header.Set("Authorization", "Bearer "+memberKey)

	freshResp, err := app.Test(freshReq, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test (fresh echo): %v", err)
	}
	defer freshResp.Body.Close()
	if freshResp.StatusCode != fiber.StatusOK {
		b, _ := io.ReadAll(freshResp.Body)
		t.Fatalf("fresh echo PATCH status = %d, want 200; body: %s", freshResp.StatusCode, b)
	}

	final, err := database.GetAPIKey(context.Background(), target.ID)
	if err != nil {
		t.Fatalf("reload after fresh echo PATCH: %v", err)
	}
	if final.Name != "Renamed By Member" {
		t.Errorf("Name after fresh echo PATCH = %q, want %q", final.Name, "Renamed By Member")
	}
	if final.RequestsPerMinute != adminLoweredRPM {
		t.Errorf("RequestsPerMinute after fresh echo PATCH = %d, want %d (member's echo must never overwrite the admin-set value)", final.RequestsPerMinute, adminLoweredRPM)
	}
}
