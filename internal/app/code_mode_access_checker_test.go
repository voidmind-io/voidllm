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

	teamServer := db.MCPServer{ID: "sv-team", TeamID: ptrStr("team-1"), OrgID: ptrStr("org-1")}
	orgServer := db.MCPServer{ID: "sv-org", OrgID: ptrStr("org-1")}
	globalServer := db.MCPServer{ID: "sv-global"}
	builtinServer := db.MCPServer{ID: "sv-builtin", Source: "builtin"}

	cache := &staticServerCache{byID: map[string]*db.MCPServer{
		"sv-team":    &teamServer,
		"sv-org":     &orgServer,
		"sv-global":  &globalServer,
		"sv-builtin": &builtinServer,
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
			name:     "system admin bypasses every server, regardless of scope",
			serverID: "sv-org",
			id:       mcp.KeyIdentity{OrgID: "org-other", Role: auth.RoleSystemAdmin},
			want:     true,
		},
		{
			name:     "system admin bypasses even an org-scoped server for a different org",
			serverID: "sv-team",
			id:       mcp.KeyIdentity{OrgID: "org-other", TeamID: "team-other", Role: auth.RoleSystemAdmin},
			want:     true,
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
			got := checker(tc.id, tc.serverID)
			if got != tc.want {
				t.Errorf("codeModeAccessChecker(...)(%+v, %q) = %v, want %v", tc.id, tc.serverID, got, tc.want)
			}
		})
	}
}
