package auth

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

func TestHasRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		role     string
		required string
		want     bool
	}{
		// system_admin is highest rank — satisfies all requirements.
		{name: "system_admin satisfies member", role: RoleSystemAdmin, required: RoleMember, want: true},
		{name: "system_admin satisfies team_admin", role: RoleSystemAdmin, required: RoleTeamAdmin, want: true},
		{name: "system_admin satisfies org_admin", role: RoleSystemAdmin, required: RoleOrgAdmin, want: true},
		{name: "system_admin satisfies system_admin", role: RoleSystemAdmin, required: RoleSystemAdmin, want: true},

		// org_admin satisfies everything below itself but not system_admin.
		{name: "org_admin satisfies member", role: RoleOrgAdmin, required: RoleMember, want: true},
		{name: "org_admin satisfies team_admin", role: RoleOrgAdmin, required: RoleTeamAdmin, want: true},
		{name: "org_admin satisfies org_admin", role: RoleOrgAdmin, required: RoleOrgAdmin, want: true},
		{name: "org_admin does not satisfy system_admin", role: RoleOrgAdmin, required: RoleSystemAdmin, want: false},

		// team_admin satisfies member and itself but not org_admin or higher.
		{name: "team_admin satisfies member", role: RoleTeamAdmin, required: RoleMember, want: true},
		{name: "team_admin satisfies team_admin", role: RoleTeamAdmin, required: RoleTeamAdmin, want: true},
		{name: "team_admin does not satisfy org_admin", role: RoleTeamAdmin, required: RoleOrgAdmin, want: false},
		{name: "team_admin does not satisfy system_admin", role: RoleTeamAdmin, required: RoleSystemAdmin, want: false},

		// member satisfies only itself.
		{name: "member satisfies member", role: RoleMember, required: RoleMember, want: true},
		{name: "member does not satisfy team_admin", role: RoleMember, required: RoleTeamAdmin, want: false},
		{name: "member does not satisfy org_admin", role: RoleMember, required: RoleOrgAdmin, want: false},
		{name: "member does not satisfy system_admin", role: RoleMember, required: RoleSystemAdmin, want: false},

		// Unknown roles are always denied regardless of the requirement.
		{name: "unknown role denied for member requirement", role: "unknown", required: RoleMember, want: false},
		{name: "unknown role denied for team_admin requirement", role: "unknown", required: RoleTeamAdmin, want: false},
		{name: "unknown role denied for org_admin requirement", role: "unknown", required: RoleOrgAdmin, want: false},
		{name: "unknown role denied for system_admin requirement", role: "unknown", required: RoleSystemAdmin, want: false},

		// Empty role string is unknown — always denied.
		{name: "empty role denied for member requirement", role: "", required: RoleMember, want: false},
		{name: "empty role denied for empty required", role: "", required: "", want: false},

		// Unknown required role — always denied even for valid callers.
		{name: "member denied for unknown required role", role: RoleMember, required: "unknown_required", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := HasRole(tc.role, tc.required)
			if got != tc.want {
				t.Errorf("HasRole(%q, %q) = %v, want %v", tc.role, tc.required, got, tc.want)
			}
		})
	}
}

// setupRBACApp builds a Fiber app with Middleware + RequireRole(required) + a
// 200 handler. It returns the app and the cache so tests can populate keys.
func setupRBACApp(t *testing.T, required string) (*fiber.App, *cache.Cache[string, KeyInfo]) {
	t.Helper()
	keyCache := cache.New[string, KeyInfo]()
	app := fiber.New()
	app.Use(Middleware(keyCache, testSecret))
	app.Use(RequireRole(required))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})
	return app, keyCache
}

// storeKeyWithRole is a convenience wrapper that stores a KeyInfo with a
// specific role in the cache and returns the plaintext key.
func storeKeyWithRole(t *testing.T, kc *cache.Cache[string, KeyInfo], role string) string {
	t.Helper()
	info := KeyInfo{
		ID:      "rbac-key-" + role,
		KeyType: keygen.KeyTypeUser,
		Role:    role,
		OrgID:   "org-rbac",
	}
	return storeKey(t, kc, info, keygen.KeyTypeUser)
}

func TestRequireRole(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		callerRole string // empty means no auth header at all
		required   string
		wantStatus int
	}{
		// Require org_admin: system_admin and org_admin pass; lower roles are forbidden.
		{
			name:       "system_admin passes org_admin requirement",
			callerRole: RoleSystemAdmin,
			required:   RoleOrgAdmin,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "org_admin passes org_admin requirement",
			callerRole: RoleOrgAdmin,
			required:   RoleOrgAdmin,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "team_admin fails org_admin requirement",
			callerRole: RoleTeamAdmin,
			required:   RoleOrgAdmin,
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "member fails org_admin requirement",
			callerRole: RoleMember,
			required:   RoleOrgAdmin,
			wantStatus: fiber.StatusForbidden,
		},

		// Require team_admin: system_admin, org_admin, team_admin pass; member fails.
		{
			name:       "system_admin passes team_admin requirement",
			callerRole: RoleSystemAdmin,
			required:   RoleTeamAdmin,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "org_admin passes team_admin requirement",
			callerRole: RoleOrgAdmin,
			required:   RoleTeamAdmin,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "team_admin passes team_admin requirement",
			callerRole: RoleTeamAdmin,
			required:   RoleTeamAdmin,
			wantStatus: fiber.StatusOK,
		},
		{
			name:       "member fails team_admin requirement",
			callerRole: RoleMember,
			required:   RoleTeamAdmin,
			wantStatus: fiber.StatusForbidden,
		},

		// Require member: all valid roles pass.
		{
			name:       "member passes member requirement",
			callerRole: RoleMember,
			required:   RoleMember,
			wantStatus: fiber.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, keyCache := setupRBACApp(t, tc.required)
			raw := storeKeyWithRole(t, keyCache, tc.callerRole)

			req := httptest.NewRequest("GET", "/", nil)
			req.Header.Set("Authorization", "Bearer "+raw)

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}

func TestRequireRoleWithNoAuth(t *testing.T) {
	t.Parallel()

	// RequireRole without Middleware in the chain: KeyInfoFromCtx returns nil → 401.
	keyCache := cache.New[string, KeyInfo]()
	app := fiber.New()
	// Intentionally omit Middleware so KeyInfoFromCtx always returns nil.
	app.Use(RequireRole(RoleOrgAdmin))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})

	// Populate the cache with a valid key so the only missing piece is the
	// Middleware, not the key itself.
	info := KeyInfo{ID: "x", KeyType: keygen.KeyTypeUser, Role: RoleSystemAdmin, OrgID: "o"}
	_ = storeKey(t, keyCache, info, keygen.KeyTypeUser)

	req := httptest.NewRequest("GET", "/", nil)
	// No Authorization header — Middleware is absent so it wouldn't matter anyway.

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusUnauthorized {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusUnauthorized)
	}
}
