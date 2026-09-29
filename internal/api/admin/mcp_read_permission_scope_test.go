package admin_test

// Tests covering checkMCPServerReadPermission's team rule, exercised through
// every handler that calls it: GetMCPServer, ListMCPServerBlocklist,
// HandleListMCPServerTools, and HandleRefreshMCPServerTools. Also re-verifies
// the pre-existing org-scoped and global rules as regression cases, so a
// future change to the team branch cannot silently loosen or tighten those
// two paths.

import (
	"context"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// readPermCase describes one caller identity and the HTTP status every
// read-permission-checked endpoint must return for that caller against a
// fixed target server.
type readPermCase struct {
	name       string
	role       string
	orgID      string
	teamID     string // empty when the key is not team-scoped
	userID     string
	wantStatus int
}

// assertReadEndpointsStatus issues a request to each of the three read-only,
// repeat-safe endpoints gated by checkMCPServerReadPermission (GetMCPServer,
// ListMCPServerBlocklist, HandleListMCPServerTools) and asserts wantStatus for
// all three. HandleRefreshMCPServerTools is intentionally excluded here — it
// has a 60s cooldown after a successful call, so it is exercised separately
// with one fresh server per case.
func assertReadEndpointsStatus(t *testing.T, app *fiber.App, serverID, key string, wantStatus int) {
	t.Helper()
	endpoints := []struct {
		method, name, url string
	}{
		{http.MethodGet, "GetMCPServer", "/api/v1/mcp-servers/" + serverID},
		{http.MethodGet, "ListMCPServerBlocklist", "/api/v1/mcp-servers/" + serverID + "/blocklist"},
		{http.MethodGet, "HandleListMCPServerTools", "/api/v1/mcp-servers/" + serverID + "/tools"},
	}
	for _, ep := range endpoints {
		resp := mcpServerRequest(t, app, ep.method, ep.url, key, nil)
		resp.Body.Close()
		if resp.StatusCode != wantStatus {
			t.Errorf("%s (%s %s): status = %d, want %d", ep.name, ep.method, ep.url, resp.StatusCode, wantStatus)
		}
	}
}

// keyForCase mints a key in keyCache matching a readPermCase, using
// addTestKeyWithTeam when TeamID is set and addTestKey otherwise (addTestKey
// does not accept a team, and a blank TeamID must round-trip as "" on the
// resulting auth.KeyInfo, matching an org-only key).
func keyForCase(t *testing.T, keyCache *cache.Cache[string, auth.KeyInfo], tc readPermCase) string {
	t.Helper()
	if tc.teamID != "" {
		return addTestKeyWithTeam(t, keyCache, tc.role, tc.orgID, tc.teamID, tc.userID)
	}
	return addTestKey(t, keyCache, tc.role, tc.orgID)
}

// TestCheckMCPServerReadPermission_TeamScoped covers the team rule added to
// checkMCPServerReadPermission: a team-scoped server is readable by members
// and team_admins of its own team, by org_admins of its org (regardless of
// team), and by system_admin — but forbidden for members/team_admins of a
// sibling team in the same org, and for anyone from a different org.
func TestCheckMCPServerReadPermission_TeamScoped(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCheckMCPServerReadPermission_TeamScoped?mode=memory&cache=private"
	database, keyCache, app := setupMCPServersTestAppWithToolCache(t, dsn, []mcp.Tool{{Name: "t"}})

	orgA := mustCreateOrg(t, database, "Read Perm Org A", "read-perm-org-a")
	orgB := mustCreateOrg(t, database, "Read Perm Org B", "read-perm-org-b")
	teamA1 := mustCreateTeam(t, database, orgA.ID, "Read Perm Team A1", "read-perm-team-a1")
	teamA2 := mustCreateTeam(t, database, orgA.ID, "Read Perm Team A2", "read-perm-team-a2")

	sv, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "Team A1 Server",
		Alias:    "read-perm-team-a1-srv",
		URL:      "https://example.com",
		AuthType: "none",
		OrgID:    &orgA.ID,
		TeamID:   &teamA1.ID,
	})
	if err != nil {
		t.Fatalf("create team-scoped server: %v", err)
	}

	cases := []readPermCase{
		{"team A1 member allowed", auth.RoleMember, orgA.ID, teamA1.ID, "user-a1-member", fiber.StatusOK},
		{"team A1 team_admin allowed", auth.RoleTeamAdmin, orgA.ID, teamA1.ID, "user-a1-admin", fiber.StatusOK},
		{"org A org_admin allowed regardless of team", auth.RoleOrgAdmin, orgA.ID, "", "user-a-orgadmin", fiber.StatusOK},
		{"system admin allowed", auth.RoleSystemAdmin, "", "", "user-sysadmin", fiber.StatusOK},
		{"team A2 member forbidden (sibling team)", auth.RoleMember, orgA.ID, teamA2.ID, "user-a2-member", fiber.StatusForbidden},
		{"team A2 team_admin forbidden (sibling team)", auth.RoleTeamAdmin, orgA.ID, teamA2.ID, "user-a2-admin", fiber.StatusForbidden},
		{"org B member forbidden (different org)", auth.RoleMember, orgB.ID, "", "user-b-member", fiber.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key := keyForCase(t, keyCache, tc)
			assertReadEndpointsStatus(t, app, sv.ID, key, tc.wantStatus)
		})
	}
}

// TestCheckMCPServerReadPermission_OrgScoped_Regression re-verifies the
// pre-existing org-scoped rule (unaffected by the new team branch): any role
// from the server's own org may read it; a member of a different org may not.
func TestCheckMCPServerReadPermission_OrgScoped_Regression(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCheckMCPServerReadPermission_OrgScoped_Regression?mode=memory&cache=private"
	database, keyCache, app := setupMCPServersTestAppWithToolCache(t, dsn, []mcp.Tool{{Name: "t"}})

	orgA := mustCreateOrg(t, database, "Org Regression A", "org-regression-a")
	orgB := mustCreateOrg(t, database, "Org Regression B", "org-regression-b")

	sv := mustCreateOrgScopedMCPServer(t, database, orgA.ID, "Org Regression Server", "org-regression-srv")

	cases := []readPermCase{
		{"same-org member allowed (regression)", auth.RoleMember, orgA.ID, "", "user-a-member", fiber.StatusOK},
		{"same-org org_admin allowed", auth.RoleOrgAdmin, orgA.ID, "", "user-a-admin", fiber.StatusOK},
		{"system admin allowed", auth.RoleSystemAdmin, "", "", "user-sysadmin", fiber.StatusOK},
		{"different-org member forbidden (regression)", auth.RoleMember, orgB.ID, "", "user-b-member", fiber.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key := keyForCase(t, keyCache, tc)
			assertReadEndpointsStatus(t, app, sv.ID, key, tc.wantStatus)
		})
	}
}

// TestCheckMCPServerReadPermission_GlobalScoped_Regression re-verifies the
// pre-existing global-server rule (unaffected by the new team branch): only
// system_admin may read a global server; any org role, including org_admin,
// is forbidden.
func TestCheckMCPServerReadPermission_GlobalScoped_Regression(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCheckMCPServerReadPermission_GlobalScoped_Regression?mode=memory&cache=private"
	database, keyCache, app := setupMCPServersTestAppWithToolCache(t, dsn, []mcp.Tool{{Name: "t"}})

	orgA := mustCreateOrg(t, database, "Global Regression Org", "global-regression-org")

	sv := mustCreateGlobalMCPServer(t, database, "Global Regression Server", "global-regression-srv")

	cases := []readPermCase{
		{"system admin allowed", auth.RoleSystemAdmin, "", "", "user-sysadmin", fiber.StatusOK},
		{"org member forbidden (regression)", auth.RoleMember, orgA.ID, "", "user-a-member", fiber.StatusForbidden},
		{"org_admin forbidden (regression)", auth.RoleOrgAdmin, orgA.ID, "", "user-a-admin", fiber.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			key := keyForCase(t, keyCache, tc)
			assertReadEndpointsStatus(t, app, sv.ID, key, tc.wantStatus)
		})
	}
}

// TestCheckMCPServerReadPermission_TeamBoundServiceAccount verifies the
// effective-team fallback: a service-account key whose owning service
// account is bound to a team (KeyInfo.ServiceAccountTeamID, not
// KeyInfo.TeamID — see auth.KeyInfoFromRecord) may read its own team's
// server, exactly as a team_admin user key scoped to that team can. Each
// KeyInfo is produced via the real auth.KeyInfoFromRecord path from a
// db.KeyRecord shaped like the sa_key rows LoadActiveKey actually returns,
// not hand-built, so the test exercises the real role/field resolution the
// production auth path applies.
func TestCheckMCPServerReadPermission_TeamBoundServiceAccount(t *testing.T) {
	t.Parallel()

	dsn := "file:TestCheckMCPServerReadPermission_TeamBoundServiceAccount?mode=memory&cache=private"
	database, keyCache, app := setupMCPServersTestAppWithToolCache(t, dsn, []mcp.Tool{{Name: "t"}})

	orgA := mustCreateOrg(t, database, "SA Read Perm Org A", "sa-read-perm-org-a")
	teamA1 := mustCreateTeam(t, database, orgA.ID, "SA Read Perm Team A1", "sa-read-perm-team-a1")
	teamA2 := mustCreateTeam(t, database, orgA.ID, "SA Read Perm Team A2", "sa-read-perm-team-a2")

	sv, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:     "SA Team A1 Server",
		Alias:    "sa-read-perm-team-a1-srv",
		URL:      "https://example.com",
		AuthType: "none",
		OrgID:    &orgA.ID,
		TeamID:   &teamA1.ID,
	})
	if err != nil {
		t.Fatalf("create team-scoped server: %v", err)
	}

	// saKeyForTeam builds a db.KeyRecord shaped like a sa_key row LoadActiveKey
	// would return for a service account bound to teamID (or org-scoped, when
	// teamID is empty), resolves it through the real KeyInfoFromRecord, and
	// registers the result in keyCache. api_keys.team_id (record.TeamID) is
	// deliberately left nil, matching production: it is never populated on
	// sa_key rows.
	saKeyForTeam := func(t *testing.T, teamID string) string {
		t.Helper()
		record := db.KeyRecord{
			KeyType:              keygen.KeyTypeSA,
			OrgID:                orgA.ID,
			ServiceAccountActive: true,
		}
		if teamID != "" {
			record.ServiceAccountTeamID = &teamID
		}
		ki, ok := auth.KeyInfoFromRecord(record)
		if !ok {
			t.Fatal("KeyInfoFromRecord() ok = false, want true")
		}
		return addTestKeyInfo(t, keyCache, ki)
	}

	t.Run("team-bound SA allowed on its own team's server", func(t *testing.T) {
		t.Parallel()
		key := saKeyForTeam(t, teamA1.ID)
		assertReadEndpointsStatus(t, app, sv.ID, key, fiber.StatusOK)
	})

	t.Run("team-bound SA forbidden on a sibling team's server", func(t *testing.T) {
		t.Parallel()
		key := saKeyForTeam(t, teamA2.ID)
		assertReadEndpointsStatus(t, app, sv.ID, key, fiber.StatusForbidden)
	})

	t.Run("org-scoped SA (no team) allowed on a team-scoped server via org_admin", func(t *testing.T) {
		t.Parallel()
		// An org-scoped service account (ServiceAccountTeamID nil) resolves
		// to RoleOrgAdmin via KeyInfoFromRecord, and org_admin bypasses the
		// team check entirely (see the org_admin branch above) — this is not
		// the effective-team fallback, it is the pre-existing org_admin
		// bypass applying to a role that happens to be SA-derived.
		key := saKeyForTeam(t, "")
		assertReadEndpointsStatus(t, app, sv.ID, key, fiber.StatusOK)
	})
}

// TestHandleRefreshMCPServerTools_ReadPermission_TeamScoped exercises the
// same team-scoped read-permission cases against HandleRefreshMCPServerTools.
// It is kept separate from the shared assertReadEndpointsStatus matrix because
// a successful refresh starts a 60s cooldown on that server — each case here
// therefore creates and refreshes its own dedicated server so cases cannot
// interfere with one another.
func TestHandleRefreshMCPServerTools_ReadPermission_TeamScoped(t *testing.T) {
	t.Parallel()

	dsn := "file:TestHandleRefreshMCPServerTools_ReadPermission_TeamScoped?mode=memory&cache=private"
	database, keyCache, app := setupMCPServersTestAppWithToolCache(t, dsn, []mcp.Tool{{Name: "t"}})

	orgA := mustCreateOrg(t, database, "Refresh Perm Org A", "refresh-perm-org-a")
	orgB := mustCreateOrg(t, database, "Refresh Perm Org B", "refresh-perm-org-b")
	teamA1 := mustCreateTeam(t, database, orgA.ID, "Refresh Perm Team A1", "refresh-perm-team-a1")
	teamA2 := mustCreateTeam(t, database, orgA.ID, "Refresh Perm Team A2", "refresh-perm-team-a2")

	cases := []readPermCase{
		{"team A1 member allowed", auth.RoleMember, orgA.ID, teamA1.ID, "user-a1-member", fiber.StatusOK},
		{"team A1 team_admin allowed", auth.RoleTeamAdmin, orgA.ID, teamA1.ID, "user-a1-admin", fiber.StatusOK},
		{"org A org_admin allowed regardless of team", auth.RoleOrgAdmin, orgA.ID, "", "user-a-orgadmin", fiber.StatusOK},
		{"system admin allowed", auth.RoleSystemAdmin, "", "", "user-sysadmin", fiber.StatusOK},
		{"team A2 member forbidden (sibling team)", auth.RoleMember, orgA.ID, teamA2.ID, "user-a2-member", fiber.StatusForbidden},
		{"team A2 team_admin forbidden (sibling team)", auth.RoleTeamAdmin, orgA.ID, teamA2.ID, "user-a2-admin", fiber.StatusForbidden},
		{"org B member forbidden (different org)", auth.RoleMember, orgB.ID, "", "user-b-member", fiber.StatusForbidden},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sv, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
				Name:     "Refresh Perm Server",
				Alias:    "refresh-perm-srv-" + string(rune('a'+i)),
				URL:      "https://example.com",
				AuthType: "none",
				OrgID:    &orgA.ID,
				TeamID:   &teamA1.ID,
			})
			if err != nil {
				t.Fatalf("create team-scoped server: %v", err)
			}

			key := keyForCase(t, keyCache, tc)
			resp := mcpServerRequest(t, app, http.MethodPost, "/api/v1/mcp-servers/"+sv.ID+"/refresh-tools", key, nil)
			resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Errorf("HandleRefreshMCPServerTools: status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}
