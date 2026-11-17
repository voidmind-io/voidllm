package db

import (
	"context"
	"slices"
	"testing"

	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// teamCtxFixture holds the rows shared by the team-context key tests.
type teamCtxFixture struct {
	orgID, userID                string
	teamID, otherTeamID          string
	teamKeyID, teamKeyHash       string
	saTeamKeyID, saTeamKeyHash   string
	saOrgKeyID, saOrgKeyHash     string
	otherTeamKeyHash             string
	otherTeamSAKeyHash           string
	deletedTeamKeyHash           string
	teamUserKeyHash              string
	saID, saOrgID, otherTeamSAID string
}

// newTeamCtxFixture builds an org with two teams, a team_key and a team-bound
// sa_key for team 1, an org-level sa_key, a team_key and team-bound sa_key for
// team 2, a soft-deleted team_key of team 1, and a team-scoped user_key of team 1.
func newTeamCtxFixture(t *testing.T, d *DB, slug string) teamCtxFixture {
	t.Helper()
	ctx := context.Background()
	f := teamCtxFixture{}

	org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: slug + "-org"})
	user := mustCreateUser(t, d, CreateUserParams{Email: slug + "@example.com", DisplayName: "U"})
	team := mustCreateTeam(t, d, CreateTeamParams{
		OrgID: org.ID, Name: "T1", Slug: slug + "-t1",
		RequestsPerMinute: 11, RequestsPerDay: 111, DailyTokenLimit: 1111, MonthlyTokenLimit: 11111,
	})
	other := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T2", Slug: slug + "-t2"})
	f.orgID, f.userID, f.teamID, f.otherTeamID = org.ID, user.ID, team.ID, other.ID

	mk := func(keyType string, p CreateAPIKeyParams) (id, hash string) {
		t.Helper()
		plain, err := keygen.Generate(keyType)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		p.KeyHash = keygen.Hash(plain, testHMACSecret)
		p.KeyHint = keygen.Hint(plain)
		p.KeyType = keyType
		p.OrgID = org.ID
		p.CreatedBy = user.ID
		if p.Name == "" {
			p.Name = "k"
		}
		k := mustCreateAPIKey(t, d, p)
		return k.ID, p.KeyHash
	}

	saTeam := mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "team sa", OrgID: org.ID, TeamID: ptr(team.ID), CreatedBy: user.ID})
	saOrg := mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "org sa", OrgID: org.ID, CreatedBy: user.ID})
	saOther := mustCreateServiceAccount(t, d, CreateServiceAccountParams{Name: "other sa", OrgID: org.ID, TeamID: ptr(other.ID), CreatedBy: user.ID})
	f.saID, f.saOrgID, f.otherTeamSAID = saTeam.ID, saOrg.ID, saOther.ID

	f.teamKeyID, f.teamKeyHash = mk(keygen.KeyTypeTeam, CreateAPIKeyParams{TeamID: ptr(team.ID)})
	f.saTeamKeyID, f.saTeamKeyHash = mk(keygen.KeyTypeSA, CreateAPIKeyParams{ServiceAccountID: ptr(saTeam.ID)})
	f.saOrgKeyID, f.saOrgKeyHash = mk(keygen.KeyTypeSA, CreateAPIKeyParams{ServiceAccountID: ptr(saOrg.ID)})
	_, f.otherTeamKeyHash = mk(keygen.KeyTypeTeam, CreateAPIKeyParams{TeamID: ptr(other.ID)})
	_, f.otherTeamSAKeyHash = mk(keygen.KeyTypeSA, CreateAPIKeyParams{ServiceAccountID: ptr(saOther.ID)})
	_, f.teamUserKeyHash = mk(keygen.KeyTypeUser, CreateAPIKeyParams{TeamID: ptr(team.ID), UserID: ptr(user.ID)})

	delID, delHash := mk(keygen.KeyTypeTeam, CreateAPIKeyParams{TeamID: ptr(team.ID)})
	if err := d.DeleteAPIKey(ctx, delID); err != nil {
		t.Fatalf("DeleteAPIKey: %v", err)
	}
	f.deletedTeamKeyHash = delHash
	return f
}

// TestLoadActiveKey_TeamContext verifies that LoadActiveKey resolves team
// limits and TeamActive for team_key and team-bound sa_key records, flips
// TeamActive after DeleteTeam, and leaves org-level sa_keys unaffected.
func TestLoadActiveKey_TeamContext(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		f := newTeamCtxFixture(t, d, "tctx-load")

		type want struct {
			teamActive bool
			rpm        int
			rpd        int
			daily      int64
			monthly    int64
		}
		check := func(t *testing.T, keyID string, w want) {
			t.Helper()
			got, err := d.LoadActiveKey(ctx, keyID)
			if err != nil {
				t.Fatalf("LoadActiveKey: %v", err)
			}
			if got.TeamActive != w.teamActive {
				t.Errorf("TeamActive = %v, want %v", got.TeamActive, w.teamActive)
			}
			if got.TeamRequestsPerMinute != w.rpm || got.TeamRequestsPerDay != w.rpd ||
				got.TeamDailyTokenLimit != w.daily || got.TeamMonthlyTokenLimit != w.monthly {
				t.Errorf("team limits = %d/%d/%d/%d, want %d/%d/%d/%d",
					got.TeamRequestsPerMinute, got.TeamRequestsPerDay, got.TeamDailyTokenLimit, got.TeamMonthlyTokenLimit,
					w.rpm, w.rpd, w.daily, w.monthly)
			}
		}

		teamLimits := want{teamActive: true, rpm: 11, rpd: 111, daily: 1111, monthly: 11111}

		t.Run("before delete: team-bound sa_key gets team limits and TeamActive", func(t *testing.T) {
			check(t, f.saTeamKeyID, teamLimits)
		})
		t.Run("before delete: team_key gets team limits and TeamActive", func(t *testing.T) {
			check(t, f.teamKeyID, teamLimits)
		})
		t.Run("before delete: org-level sa_key has no team", func(t *testing.T) {
			check(t, f.saOrgKeyID, want{})
		})

		if err := d.DeleteTeam(ctx, f.teamID); err != nil {
			t.Fatalf("DeleteTeam: %v", err)
		}

		t.Run("after delete: team-bound sa_key is not TeamActive", func(t *testing.T) {
			got, err := d.LoadActiveKey(ctx, f.saTeamKeyID)
			if err != nil {
				t.Fatalf("LoadActiveKey: %v", err)
			}
			if got.TeamActive {
				t.Error("TeamActive = true after DeleteTeam, want false")
			}
			if !got.ServiceAccountActive {
				t.Error("ServiceAccountActive = false, want true (service account itself is not deleted)")
			}
		})
		t.Run("after delete: team_key is not TeamActive", func(t *testing.T) {
			check(t, f.teamKeyID, want{})
		})
		t.Run("after delete: org-level sa_key unaffected", func(t *testing.T) {
			got, err := d.LoadActiveKey(ctx, f.saOrgKeyID)
			if err != nil {
				t.Fatalf("LoadActiveKey: %v", err)
			}
			if !got.ServiceAccountActive || got.TeamActive {
				t.Errorf("ServiceAccountActive=%v TeamActive=%v, want true/false", got.ServiceAccountActive, got.TeamActive)
			}
		})
	})
}

// TestListActiveKeyHashesByTeam verifies the hash set returned for a team:
// its team keys and team-bound service-account keys; never team-scoped user
// keys, other teams' keys, deleted keys, or org-level service-account keys.
func TestListActiveKeyHashesByTeam(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		f := newTeamCtxFixture(t, d, "tctx-list")

		got, err := d.ListActiveKeyHashesByTeam(ctx, f.teamID)
		if err != nil {
			t.Fatalf("ListActiveKeyHashesByTeam: %v", err)
		}

		tests := []struct {
			name string
			hash string
			want bool
		}{
			{"team_key included", f.teamKeyHash, true},
			{"team-bound sa_key included", f.saTeamKeyHash, true},
			{"team-scoped user_key excluded (Cacheable keeps it)", f.teamUserKeyHash, false},
			{"other team's team_key excluded", f.otherTeamKeyHash, false},
			{"other team's sa_key excluded", f.otherTeamSAKeyHash, false},
			{"deleted key excluded", f.deletedTeamKeyHash, false},
			{"org-level sa_key excluded", f.saOrgKeyHash, false},
		}
		for _, tc := range tests {
			if slices.Contains(got, tc.hash) != tc.want {
				t.Errorf("%s: contains = %v, want %v", tc.name, !tc.want, tc.want)
			}
		}
		if len(got) != 2 {
			t.Errorf("len(hashes) = %d, want 2", len(got))
		}

		empty, err := d.ListActiveKeyHashesByTeam(ctx, "no-such-team")
		if err != nil {
			t.Fatalf("ListActiveKeyHashesByTeam(unknown): %v", err)
		}
		if len(empty) != 0 {
			t.Errorf("unknown team: len = %d, want 0", len(empty))
		}
	})
}
