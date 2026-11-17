package auth

import (
	"testing"

	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// TestKeyInfoFromRecord_ServiceAccountTeamContext is the table for how a
// sa_key's service account determines KeyInfo.TeamID and Role.
func TestKeyInfoFromRecord_ServiceAccountTeamContext(t *testing.T) {
	t.Parallel()

	saTeam := "team-sa-ctx"
	rowTeam := "team-row"
	empty := ""

	tests := []struct {
		name     string
		record   db.KeyRecord
		wantTeam string
		wantRole string
	}{
		{
			name:     "active team-bound SA: TeamID is the SA team",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeSA, ServiceAccountActive: true, ServiceAccountTeamID: &saTeam},
			wantTeam: saTeam,
			wantRole: RoleTeamAdmin,
		},
		{
			name:     "org SA: TeamID empty",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeSA, ServiceAccountActive: true},
			wantTeam: "",
			wantRole: RoleOrgAdmin,
		},
		{
			name:     "org SA with empty-string team: TeamID empty",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeSA, ServiceAccountActive: true, ServiceAccountTeamID: &empty},
			wantTeam: "",
			wantRole: RoleOrgAdmin,
		},
		{
			name:     "inactive team-bound SA: TeamID empty",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeSA, ServiceAccountActive: false, ServiceAccountTeamID: &saTeam},
			wantTeam: "",
			wantRole: RoleMember,
		},
		{
			name:     "inactive org SA: TeamID empty",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeSA, ServiceAccountActive: false},
			wantTeam: "",
			wantRole: RoleMember,
		},
		{
			name:     "user_key never takes TeamID from a service account team",
			record:   db.KeyRecord{KeyType: keygen.KeyTypeUser, ServiceAccountActive: true, ServiceAccountTeamID: &saTeam, MembershipRole: RoleMember, TeamID: &rowTeam},
			wantTeam: rowTeam,
			wantRole: RoleMember,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ki, _ := KeyInfoFromRecord(tc.record)
			if ki.TeamID != tc.wantTeam {
				t.Errorf("TeamID = %q, want %q", ki.TeamID, tc.wantTeam)
			}
			if ki.Role != tc.wantRole {
				t.Errorf("Role = %q, want %q", ki.Role, tc.wantRole)
			}
		})
	}
}

// TestCacheable_TeamBoundServiceAccount covers the deleted-team handling of
// Cacheable for sa_keys and team keys.
func TestCacheable_TeamBoundServiceAccount(t *testing.T) {
	t.Parallel()

	team := "team-1"

	tests := []struct {
		name string
		rec  db.KeyRecord
		want bool
	}{
		{"team-bound sa_key, team active", db.KeyRecord{KeyType: keygen.KeyTypeSA, ServiceAccountActive: true, ServiceAccountTeamID: &team, TeamActive: true}, true},
		{"team-bound sa_key, team deleted", db.KeyRecord{KeyType: keygen.KeyTypeSA, ServiceAccountActive: true, ServiceAccountTeamID: &team, TeamActive: false}, false},
		{"team-bound sa_key, SA inactive, team active", db.KeyRecord{KeyType: keygen.KeyTypeSA, ServiceAccountActive: false, ServiceAccountTeamID: &team, TeamActive: true}, false},
		{"org sa_key unaffected by TeamActive=false", db.KeyRecord{KeyType: keygen.KeyTypeSA, ServiceAccountActive: true, TeamActive: false}, true},
		{"team_key, team deleted", db.KeyRecord{KeyType: keygen.KeyTypeTeam, TeamActive: false}, false},
		{"team_key, team active", db.KeyRecord{KeyType: keygen.KeyTypeTeam, TeamActive: true}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := Cacheable(tc.rec); got != tc.want {
				t.Errorf("Cacheable() = %v, want %v", got, tc.want)
			}
		})
	}
}
