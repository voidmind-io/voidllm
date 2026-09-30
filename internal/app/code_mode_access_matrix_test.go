package app

// TestCodeModeAccessChecker_MatchesAccessibleServers_Matrix is item 3's own
// required proof: codeModeAccessChecker (code_mode.go) must mirror
// codeModeService.accessibleServers(ctx, true)'s own visibility decision —
// the DB-backed, request-time tools/list path — exactly, for every caller
// identity and every server this test constructs, including scope
// combinations (global/org/team/builtin), CodeModeEnabled=false, an org-level
// MCPAccessCache allowlist, and an alias collision between a team-scoped and
// an org-scoped server (resolveServersByAlias' own team > org > global
// priority). Built against a real SQLite DB, exactly like accessibleServers
// itself would run in production, rather than a hand-rolled fake DB whose
// own query logic might silently diverge from the real one.

import (
	"context"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

func TestCodeModeAccessChecker_MatchesAccessibleServers_Matrix(t *testing.T) {
	t.Parallel()

	database := openTestDBForSchemaTests(t)
	ctx := context.Background()

	org1, err := database.CreateOrg(ctx, db.CreateOrgParams{Name: "Matrix Org 1", Slug: "matrix-org-1"})
	if err != nil {
		t.Fatalf("CreateOrg org1: %v", err)
	}
	org2, err := database.CreateOrg(ctx, db.CreateOrgParams{Name: "Matrix Org 2", Slug: "matrix-org-2"})
	if err != nil {
		t.Fatalf("CreateOrg org2: %v", err)
	}
	team1, err := database.CreateTeam(ctx, db.CreateTeamParams{OrgID: org1.ID, Name: "Matrix Team 1", Slug: "matrix-team-1"})
	if err != nil {
		t.Fatalf("CreateTeam team1: %v", err)
	}

	enabled := true
	disabled := false

	global1, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name: "global-1", Alias: "global-1-alias", URL: "https://g1.example.com", AuthType: "none", CodeModeEnabled: &enabled,
	})
	if err != nil {
		t.Fatalf("create global1: %v", err)
	}
	globalGated, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name: "global-gated", Alias: "global-gated-alias", URL: "https://gg.example.com", AuthType: "none", CodeModeEnabled: &enabled,
	})
	if err != nil {
		t.Fatalf("create globalGated: %v", err)
	}
	orgServer, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name: "org-1", Alias: "shared-alias", URL: "https://o1.example.com", AuthType: "none",
		OrgID: &org1.ID, CodeModeEnabled: &enabled,
	})
	if err != nil {
		t.Fatalf("create orgServer: %v", err)
	}
	orgServerDisabled, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name: "org-1-disabled", Alias: "org-1-disabled-alias", URL: "https://o1d.example.com", AuthType: "none",
		OrgID: &org1.ID, CodeModeEnabled: &disabled,
	})
	if err != nil {
		t.Fatalf("create orgServerDisabled: %v", err)
	}
	teamServer, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name: "team-1", Alias: "shared-alias", URL: "https://t1.example.com", AuthType: "none",
		OrgID: &org1.ID, TeamID: &team1.ID, CodeModeEnabled: &enabled,
	})
	if err != nil {
		t.Fatalf("create teamServer: %v", err)
	}
	builtinServer, err := database.CreateMCPServer(ctx, db.CreateMCPServerParams{
		Name: "builtin-1", Alias: "builtin-1-alias", URL: "https://b1.example.com", AuthType: "none",
		Source: "builtin", CodeModeEnabled: &enabled,
	})
	if err != nil {
		t.Fatalf("create builtinServer: %v", err)
	}

	// Org1 is explicitly granted globalGated (org-level MCP access is
	// closed-by-default) — global1 is deliberately left ungranted for every
	// org, to exercise the "closed by default" denial branch too.
	if err := database.SetOrgMCPAccess(ctx, org1.ID, []string{globalGated.ID}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	allServers, err := database.LoadAllActiveMCPServers(ctx)
	if err != nil {
		t.Fatalf("LoadAllActiveMCPServers: %v", err)
	}
	serverCache := proxy.NewMCPServerCache()
	serverCache.LoadAll(allServers)

	mcpAccessCache := proxy.NewMCPAccessCache()
	orgA, teamA, keyA, err := database.LoadAllMCPAccess(ctx)
	if err != nil {
		t.Fatalf("LoadAllMCPAccess: %v", err)
	}
	mcpAccessCache.Load(orgA, teamA, keyA)

	svc := &codeModeService{
		db:  database,
		log: newDiscardLogger(),
	}
	checker := codeModeAccessChecker(serverCache, mcpAccessCache)

	callers := []struct {
		name string
		id   mcp.KeyIdentity
	}{
		{name: "org1 member, no team", id: mcp.KeyIdentity{OrgID: org1.ID, KeyID: "key-org1-member", Role: auth.RoleMember}},
		{name: "org1 team1 member", id: mcp.KeyIdentity{OrgID: org1.ID, TeamID: team1.ID, KeyID: "key-team1-member", Role: auth.RoleMember}},
		{name: "org2 member", id: mcp.KeyIdentity{OrgID: org2.ID, KeyID: "key-org2-member", Role: auth.RoleMember}},
		{name: "system_admin in org1", id: mcp.KeyIdentity{OrgID: org1.ID, KeyID: "key-admin-org1", Role: auth.RoleSystemAdmin}},
		{name: "system_admin with no org at all", id: mcp.KeyIdentity{KeyID: "key-admin-noorg", Role: auth.RoleSystemAdmin}},
	}

	serverIDs := []string{global1.ID, globalGated.ID, orgServer.ID, orgServerDisabled.ID, teamServer.ID, builtinServer.ID}

	// byID indexes allServers for the snapshot half of this matrix below —
	// every field mcpServerScopeSnapshot itself would have captured from the
	// identical row, at a moment when nothing has actually mutated it yet, so
	// a NotifiedServerScope built from it describes exactly the SAME state
	// accessibleServers' own DB query just observed.
	byID := make(map[string]db.MCPServer, len(allServers))
	for _, sv := range allServers {
		byID[sv.ID] = sv
	}

	for _, caller := range callers {
		t.Run(caller.name, func(t *testing.T) {
			hctx, cancel := context.WithTimeout(ctxWithIdentity(caller.id), 5*time.Second)
			defer cancel()

			accessible, err := svc.accessibleServers(hctx, true)
			if err != nil {
				t.Fatalf("accessibleServers: %v", err)
			}
			accessibleIDs := make(map[string]bool, len(accessible))
			for _, sv := range accessible {
				accessibleIDs[sv.ID] = true
			}

			for _, serverID := range serverIDs {
				want := accessibleIDs[serverID]

				got := checker(caller.id, serverID, nil)
				if got != want {
					t.Errorf("server %q: codeModeAccessChecker(live) = %v, accessibleServers = %v (mismatch)", serverID, got, want)
				}

				// Snapshot evaluation of the SAME, unmutated row must agree
				// with both the live checker above and accessibleServers
				// itself — item 2's own requirement that a
				// NotifiedServerScope snapshot is judged by the identical
				// rules as the live path, not a second, potentially
				// drifting implementation of them.
				sv, ok := byID[serverID]
				if !ok {
					t.Fatalf("server %q not found in fixture", serverID)
				}
				snapshot := notifiedServerScopeFromRow(sv)
				gotSnapshot := checker(caller.id, "", &snapshot)
				if gotSnapshot != want {
					t.Errorf("server %q: codeModeAccessChecker(snapshot) = %v, accessibleServers = %v (mismatch)", serverID, gotSnapshot, want)
				}
			}
		})
	}
}

// notifiedServerScopeFromRow builds an mcp.NotifiedServerScope snapshot from
// sv's own current field values — mirroring, field for field,
// internal/api/admin/mcp_servers.go's mcpServerScopeSnapshot (unexported
// there, and admin depends on app, not the reverse, so this test rebuilds the
// identical mapping locally rather than importing it).
func notifiedServerScopeFromRow(sv db.MCPServer) mcp.NotifiedServerScope {
	return mcp.NotifiedServerScope{
		ID:              sv.ID,
		Alias:           sv.Alias,
		OrgID:           sv.OrgID,
		TeamID:          sv.TeamID,
		Source:          sv.Source,
		CodeModeEnabled: sv.CodeModeEnabled,
		Active:          sv.IsActive,
	}
}
