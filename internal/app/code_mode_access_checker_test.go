package app

// Unit tests for codeModeAccessChecker (code_mode.go) — the mcp.AccessChecker
// wiring that scopes subscriptions/listen tools/list_changed notifications on
// the Code Mode server to exactly the callers that could actually see the
// changed server's tools. Every case here mirrors one branch of
// codeModeAccessChecker's own doc; TestAccessibleServers_* elsewhere in this
// package covers the same access decision at the accessibleServers level
// (the request-time tools/list path) — these two are meant to be kept in
// lockstep, per codeModeAccessChecker's own doc, which is why the case names
// below deliberately mirror accessibleServers' own branches.

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

func TestCodeModeAccessChecker(t *testing.T) {
	t.Parallel()

	teamServer := db.MCPServer{ID: "sv-team", Alias: "team-alias", TeamID: ptrStr("team-1"), OrgID: ptrStr("org-1"), CodeModeEnabled: true}
	orgServer := db.MCPServer{ID: "sv-org", Alias: "org-alias", OrgID: ptrStr("org-1"), CodeModeEnabled: true}
	globalServer := db.MCPServer{ID: "sv-global", Alias: "global-alias", CodeModeEnabled: true}
	builtinServer := db.MCPServer{ID: "sv-builtin", Alias: "builtin-alias", Source: "builtin", CodeModeEnabled: true}
	codeModeDisabledOrgServer := db.MCPServer{ID: "sv-org-disabled", Alias: "org-disabled-alias", OrgID: ptrStr("org-1"), CodeModeEnabled: false}

	cache := &staticServerCache{byID: map[string]*db.MCPServer{
		"sv-team":         &teamServer,
		"sv-org":          &orgServer,
		"sv-global":       &globalServer,
		"sv-builtin":      &builtinServer,
		"sv-org-disabled": &codeModeDisabledOrgServer,
	}}

	accessCache := proxy.NewMCPAccessCache()
	accessCache.Load(
		map[string][]string{"org-1": {"sv-global"}},
		nil,
		map[string][]string{"key-explicit": {"sv-global"}},
	)

	tests := []struct {
		name        string
		serverID    string
		id          mcp.KeyIdentity
		accessCache *proxy.MCPAccessCache
		want        bool
	}{
		{
			// codeModeAccessChecker mirrors accessibleServers exactly: a
			// system_admin's own isSystemAdmin bypass only ever applies
			// within the global-server/MCPAccessCache branch (see
			// accessibleServers' own doc) — it is never a blanket bypass of
			// org/team scoping itself, since accessibleServers' own DB query
			// already scopes `all` to the caller's own org/team before that
			// branch is ever reached. A system admin belonging to a
			// DIFFERENT org than an org-scoped server is denied, exactly
			// like any other role.
			name:     "system admin does NOT bypass org scoping for a different org",
			serverID: "sv-org",
			id:       mcp.KeyIdentity{OrgID: "org-other", Role: auth.RoleSystemAdmin},
			want:     false,
		},
		{
			name:     "system admin does NOT bypass team scoping for a different org/team",
			serverID: "sv-team",
			id:       mcp.KeyIdentity{OrgID: "org-other", TeamID: "team-other", Role: auth.RoleSystemAdmin},
			want:     false,
		},
		{
			name:     "system admin's own org/team still grants access to that org/team's scoped servers",
			serverID: "sv-team",
			id:       mcp.KeyIdentity{OrgID: "org-1", TeamID: "team-1", Role: auth.RoleSystemAdmin},
			want:     true,
		},
		{
			name:        "system admin bypasses MCPAccessCache for a global, non-builtin server",
			serverID:    "sv-global",
			id:          mcp.KeyIdentity{OrgID: "org-closed", Role: auth.RoleSystemAdmin},
			accessCache: accessCache,
			want:        true,
		},
		{
			name:     "CodeModeEnabled=false denies an otherwise-visible org-scoped server",
			serverID: "sv-org-disabled",
			id:       mcp.KeyIdentity{OrgID: "org-1", Role: auth.RoleMember},
			want:     false,
		},
		{
			name:     "builtin server is always accessible, no matter the caller",
			serverID: "sv-builtin",
			id:       mcp.KeyIdentity{OrgID: "org-other", Role: auth.RoleMember},
			want:     true,
		},
		{
			name:     "team-scoped server: exact team match grants access",
			serverID: "sv-team",
			id:       mcp.KeyIdentity{OrgID: "org-1", TeamID: "team-1", Role: auth.RoleMember},
			want:     true,
		},
		{
			name:     "team-scoped server: a different team is denied even within the same org",
			serverID: "sv-team",
			id:       mcp.KeyIdentity{OrgID: "org-1", TeamID: "team-2", Role: auth.RoleMember},
			want:     false,
		},
		{
			name:     "team-scoped server: no team at all (org-only key) is denied",
			serverID: "sv-team",
			id:       mcp.KeyIdentity{OrgID: "org-1", Role: auth.RoleMember},
			want:     false,
		},
		{
			name:     "org-scoped server: any member of the org is granted access, no explicit allowlist entry needed",
			serverID: "sv-org",
			id:       mcp.KeyIdentity{OrgID: "org-1", Role: auth.RoleMember},
			want:     true,
		},
		{
			name:     "org-scoped server: a different org is denied",
			serverID: "sv-org",
			id:       mcp.KeyIdentity{OrgID: "org-2", Role: auth.RoleMember},
			want:     false,
		},
		{
			name:        "global server: falls back to MCPAccessCache — org-level allow grants access",
			serverID:    "sv-global",
			id:          mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-no-explicit-entry", Role: auth.RoleMember},
			accessCache: accessCache,
			want:        true,
		},
		{
			name:        "global server: MCPAccessCache denies an org with no allowlist entry",
			serverID:    "sv-global",
			id:          mcp.KeyIdentity{OrgID: "org-closed", KeyID: "key-x", Role: auth.RoleMember},
			accessCache: accessCache,
			want:        false,
		},
		{
			name:        "global server: nil MCPAccessCache denies unconditionally",
			serverID:    "sv-global",
			id:          mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-x", Role: auth.RoleMember},
			accessCache: nil,
			want:        false,
		},
		{
			name:     "unknown server ID (e.g. just deleted) is always denied",
			serverID: "sv-does-not-exist",
			id:       mcp.KeyIdentity{OrgID: "org-1", Role: auth.RoleSystemAdmin},
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			checker := codeModeAccessChecker(cache, tc.accessCache)
			got := checker(tc.id, tc.serverID, nil)
			if got != tc.want {
				t.Errorf("codeModeAccessChecker(...)(%+v, %q, nil) = %v, want %v", tc.id, tc.serverID, got, tc.want)
			}
		})
	}
}

// TestCodeModeAccessChecker_Snapshot mirrors TestCodeModeAccessChecker's own
// cases, but through the snapshot branch (serverID == "", snapshot != nil) —
// item 2's own requirement that a NotifiedServerScope snapshot is evaluated
// by the exact same rules (MCPAccessCache, the alias-winner rule,
// CodeModeEnabled) as the live path, not a simplified copy of them. Each case
// builds a NotifiedServerScope snapshot of a server that is deliberately
// ABSENT from the live serverCache — proving the snapshot path never needs a
// live cache entry to resolve — with every other live server from
// TestCodeModeAccessChecker's own fixture still present as an alias-winner
// sibling where relevant.
func TestCodeModeAccessChecker_Snapshot(t *testing.T) {
	t.Parallel()

	orgServer := db.MCPServer{ID: "sv-org", Alias: "org-alias", OrgID: ptrStr("org-1"), CodeModeEnabled: true}
	teamServer := db.MCPServer{ID: "sv-team", Alias: "team-alias", TeamID: ptrStr("team-1"), OrgID: ptrStr("org-1"), CodeModeEnabled: true}
	cache := &staticServerCache{byID: map[string]*db.MCPServer{
		"sv-org":  &orgServer,
		"sv-team": &teamServer,
	}}

	accessCache := proxy.NewMCPAccessCache()
	accessCache.Load(
		map[string][]string{"org-1": {"sv-global-deleted"}},
		nil,
		nil,
	)

	tests := []struct {
		name        string
		snapshot    mcp.NotifiedServerScope
		id          mcp.KeyIdentity
		accessCache *proxy.MCPAccessCache
		want        bool
	}{
		{
			name:     "Active=false denies unconditionally, even an otherwise-matching org",
			snapshot: mcp.NotifiedServerScope{ID: "sv-org-deleted", Alias: "org-deleted-alias", OrgID: ptrStr("org-1"), CodeModeEnabled: true, Active: false},
			id:       mcp.KeyIdentity{OrgID: "org-1", Role: auth.RoleMember},
			want:     false,
		},
		{
			name:     "CodeModeEnabled=false denies an otherwise-visible org-scoped snapshot",
			snapshot: mcp.NotifiedServerScope{ID: "sv-org-disabled", Alias: "org-disabled-alias", OrgID: ptrStr("org-1"), CodeModeEnabled: false, Active: true},
			id:       mcp.KeyIdentity{OrgID: "org-1", Role: auth.RoleMember},
			want:     false,
		},
		{
			name:     "org-scoped snapshot: matching org is granted access",
			snapshot: mcp.NotifiedServerScope{ID: "sv-org-deleted", Alias: "org-deleted-alias", OrgID: ptrStr("org-1"), CodeModeEnabled: true, Active: true},
			id:       mcp.KeyIdentity{OrgID: "org-1", Role: auth.RoleMember},
			want:     true,
		},
		{
			name:     "org-scoped snapshot: a different org is denied",
			snapshot: mcp.NotifiedServerScope{ID: "sv-org-deleted", Alias: "org-deleted-alias", OrgID: ptrStr("org-1"), CodeModeEnabled: true, Active: true},
			id:       mcp.KeyIdentity{OrgID: "org-2", Role: auth.RoleMember},
			want:     false,
		},
		{
			name:     "team-scoped snapshot: matching team is granted access, matching org alone is not",
			snapshot: mcp.NotifiedServerScope{ID: "sv-team-deleted", Alias: "team-deleted-alias", OrgID: ptrStr("org-1"), TeamID: ptrStr("team-1"), CodeModeEnabled: true, Active: true},
			id:       mcp.KeyIdentity{OrgID: "org-1", Role: auth.RoleMember},
			want:     false,
		},
		{
			name:     "team-scoped snapshot: matching team AND org is granted access",
			snapshot: mcp.NotifiedServerScope{ID: "sv-team-deleted", Alias: "team-deleted-alias", OrgID: ptrStr("org-1"), TeamID: ptrStr("team-1"), CodeModeEnabled: true, Active: true},
			id:       mcp.KeyIdentity{OrgID: "org-1", TeamID: "team-1", Role: auth.RoleMember},
			want:     true,
		},
		{
			name:        "global, non-builtin snapshot: falls back to MCPAccessCache — org-level allow grants access",
			snapshot:    mcp.NotifiedServerScope{ID: "sv-global-deleted", Alias: "global-deleted-alias", CodeModeEnabled: true, Active: true},
			id:          mcp.KeyIdentity{OrgID: "org-1", Role: auth.RoleMember},
			accessCache: accessCache,
			want:        true,
		},
		{
			name:        "global, non-builtin snapshot: MCPAccessCache denies an org with no allowlist entry",
			snapshot:    mcp.NotifiedServerScope{ID: "sv-global-deleted", Alias: "global-deleted-alias", CodeModeEnabled: true, Active: true},
			id:          mcp.KeyIdentity{OrgID: "org-closed", Role: auth.RoleMember},
			accessCache: accessCache,
			want:        false,
		},
		{
			name:     "builtin snapshot is always accessible, no matter the caller",
			snapshot: mcp.NotifiedServerScope{ID: "sv-builtin-deleted", Alias: "builtin-deleted-alias", Source: "builtin", CodeModeEnabled: true, Active: true},
			id:       mcp.KeyIdentity{OrgID: "org-other", Role: auth.RoleMember},
			want:     true,
		},
		{
			name:     "alias-winner rule: a snapshot whose alias is already won by a live, higher-priority sibling is denied",
			snapshot: mcp.NotifiedServerScope{ID: "sv-org-deleted", Alias: "team-alias", OrgID: ptrStr("org-1"), CodeModeEnabled: true, Active: true},
			id:       mcp.KeyIdentity{OrgID: "org-1", TeamID: "team-1", Role: auth.RoleMember},
			want:     false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			checker := codeModeAccessChecker(cache, tc.accessCache)
			got := checker(tc.id, "", &tc.snapshot)
			if got != tc.want {
				t.Errorf("codeModeAccessChecker(...)(%+v, \"\", %+v) = %v, want %v", tc.id, tc.snapshot, got, tc.want)
			}
		})
	}
}
