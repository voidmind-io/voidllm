package db

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// TestLoadActiveKey_SAKeyJoinsFailClosed verifies that keySelectFromJoin
// resolves service-account and team metadata consistently with
// auth.KeyInfoFromRecord: a service account of another org is never joined
// (ServiceAccountActive false), and a sa_key row whose own team_id is set still
// takes its team (and team limits) from the service account.
func TestLoadActiveKey_SAKeyJoinsFailClosed(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		orgA := mustCreateOrg(t, d, CreateOrgParams{Name: "A", Slug: "fc-org-a"})
		orgB := mustCreateOrg(t, d, CreateOrgParams{Name: "B", Slug: "fc-org-b"})
		user := mustCreateUser(t, d, CreateUserParams{Email: "fc-user@example.com", DisplayName: "U"})
		teamSA := mustCreateTeam(t, d, CreateTeamParams{OrgID: orgA.ID, Name: "SA team", Slug: "fc-team-sa", RequestsPerMinute: 11})
		teamRaw := mustCreateTeam(t, d, CreateTeamParams{OrgID: orgA.ID, Name: "Raw team", Slug: "fc-team-raw", RequestsPerMinute: 99})

		sa := mustCreateServiceAccount(t, d, CreateServiceAccountParams{
			Name: "SA", OrgID: orgA.ID, TeamID: ptr(teamSA.ID), CreatedBy: user.ID,
		})

		t.Run("cross-org service account is not active", func(t *testing.T) {
			plain := "vl_sa_failclosedcrossorg0000000000000000000000000000"
			k := mustCreateAPIKey(t, d, CreateAPIKeyParams{
				KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain),
				KeyType: keygen.KeyTypeSA, Name: "cross-org-sa-key",
				OrgID: orgB.ID, ServiceAccountID: ptr(sa.ID), CreatedBy: user.ID,
			})
			got, err := d.LoadActiveKey(ctx, k.ID)
			if err != nil {
				t.Fatalf("LoadActiveKey: %v", err)
			}
			if got.ServiceAccountActive {
				t.Error("ServiceAccountActive = true, want false for a service account of another org")
			}
			if got.ServiceAccountTeamID != nil {
				t.Errorf("ServiceAccountTeamID = %q, want nil", *got.ServiceAccountTeamID)
			}
			if got.TeamActive {
				t.Error("TeamActive = true, want false")
			}
		})

		t.Run("sa_key with its own team_id uses the service account's team", func(t *testing.T) {
			plain := "vl_sa_failclosedownteam00000000000000000000000000000"
			k := mustCreateAPIKey(t, d, CreateAPIKeyParams{
				KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain),
				KeyType: keygen.KeyTypeSA, Name: "own-team-sa-key",
				OrgID: orgA.ID, TeamID: ptr(teamRaw.ID), ServiceAccountID: ptr(sa.ID), CreatedBy: user.ID,
			})
			got, err := d.LoadActiveKey(ctx, k.ID)
			if err != nil {
				t.Fatalf("LoadActiveKey: %v", err)
			}
			if !got.ServiceAccountActive || !got.TeamActive {
				t.Fatalf("ServiceAccountActive = %v, TeamActive = %v, want both true", got.ServiceAccountActive, got.TeamActive)
			}
			if got.TeamRequestsPerMinute != 11 {
				t.Errorf("TeamRequestsPerMinute = %d, want 11 (service account's team), not 99 (k.team_id)", got.TeamRequestsPerMinute)
			}
		})
	})
}

// TestRotateKeyTx_CopiesKeyAccess verifies that RotateKeyTx carries the old
// key's key-level model and MCP allowlists over to the new key, leaving the
// old key's rows in place.
func TestRotateKeyTx_CopiesKeyAccess(t *testing.T) {
	t.Parallel()

	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()
		org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "rot-access-org"})
		user := mustCreateUser(t, d, CreateUserParams{Email: "rot-access@example.com", DisplayName: "U"})
		team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "rot-access-team"})
		key := mustCreateAPIKey(t, d, testKeyParams(org.ID, user.ID, team.ID, user.ID))
		srv := mustCreateMCPServer(t, d, defaultMCPParams("rot-access"))

		if err := d.SetKeyModelAccess(ctx, key.ID, []string{"gpt-a", "gpt-b"}); err != nil {
			t.Fatalf("SetKeyModelAccess: %v", err)
		}
		if err := d.SetKeyMCPAccess(ctx, key.ID, []string{srv.ID}); err != nil {
			t.Fatalf("SetKeyMCPAccess: %v", err)
		}

		plain := "vl_uk_rotaccess00000000000000000000000000000000000"
		res, err := d.RotateKeyTx(ctx, key.ID, FormatTimestamp(time.Now().Add(time.Hour)), CreateAPIKeyParams{
			KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain),
			KeyType: key.KeyType, Name: "rotated", OrgID: key.OrgID,
			TeamID: key.TeamID, UserID: key.UserID, CreatedBy: user.ID,
		})
		if err != nil {
			t.Fatalf("RotateKeyTx: %v", err)
		}

		models, err := d.GetKeyModelAccess(ctx, res.NewKey.ID)
		if err != nil {
			t.Fatalf("GetKeyModelAccess(new): %v", err)
		}
		slices.Sort(models)
		if !slices.Equal(models, []string{"gpt-a", "gpt-b"}) {
			t.Errorf("new key models = %v, want [gpt-a gpt-b]", models)
		}
		servers, err := d.GetKeyMCPAccess(ctx, res.NewKey.ID)
		if err != nil {
			t.Fatalf("GetKeyMCPAccess(new): %v", err)
		}
		if !slices.Equal(servers, []string{srv.ID}) {
			t.Errorf("new key servers = %v, want [%s]", servers, srv.ID)
		}

		oldModels, err := d.GetKeyModelAccess(ctx, key.ID)
		if err != nil {
			t.Fatalf("GetKeyModelAccess(old): %v", err)
		}
		if len(oldModels) != 2 {
			t.Errorf("old key models = %v, want 2 rows untouched", oldModels)
		}
	})
}
