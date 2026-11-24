package db

import (
	"context"
	"testing"

	"github.com/voidmind-io/voidllm/internal/money"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// costLimitTables lists the tables that carry the 0022 cost limit columns.
var costLimitTables = []string{"organizations", "teams", "api_keys", "org_memberships"}

// bigCost exceeds the int32 range to prove the columns are 64 bit on both
// dialects (25,000 currency units = 25e9 micros).
const bigCost = money.Micros(25_000_000_000)

func TestMigration0022_ColumnsExistWithZeroDefaults(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		for _, table := range costLimitTables {
			for _, col := range []string{"daily_cost_limit", "monthly_cost_limit"} {
				if !columnExists(t, d, table, col) {
					t.Errorf("column %s.%s missing after migrations", table, col)
				}
			}
		}

		org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "m0022-defaults"})
		if org.DailyCostLimit != 0 || org.MonthlyCostLimit != 0 {
			t.Errorf("new org cost limits = %d/%d, want 0/0", org.DailyCostLimit, org.MonthlyCostLimit)
		}
	})
}

// TestMigration0022_DownThenUp creates rows with cost limits, removes the
// columns with the real down migration, re-applies the real up migration and
// checks the rows survive with zero limits.
func TestMigration0022_DownThenUp(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()

		org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "m0022-updown", DailyCostLimit: bigCost, MonthlyCostLimit: 2 * bigCost})
		user := mustCreateUser(t, d, CreateUserParams{Email: "m0022@example.com", DisplayName: "U"})
		team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "m0022-team", DailyCostLimit: bigCost})
		mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})
		key := mustCreateAPIKey(t, d, func() CreateAPIKeyParams {
			p := testKeyParams(org.ID, user.ID, team.ID, user.ID)
			p.DailyCostLimit = bigCost
			return p
		}())

		execInTx(t, d, readMigration0020(t, "0022_cost_limits.down.sql"))
		for _, table := range costLimitTables {
			for _, col := range []string{"daily_cost_limit", "monthly_cost_limit"} {
				if columnExists(t, d, table, col) {
					t.Fatalf("column %s.%s still exists after the down migration", table, col)
				}
			}
		}
		var n int
		if err := d.sql.QueryRowContext(ctx, "SELECT COUNT(*) FROM organizations WHERE id = "+d.dialect.Placeholder(1), org.ID).Scan(&n); err != nil || n != 1 {
			t.Fatalf("org row after down migration: n=%d err=%v", n, err)
		}

		execInTx(t, d, readMigration0020(t, "0022_cost_limits.up.sql"))
		got, err := d.GetOrg(ctx, org.ID)
		if err != nil {
			t.Fatalf("GetOrg after up migration: %v", err)
		}
		if got.DailyCostLimit != 0 || got.MonthlyCostLimit != 0 {
			t.Errorf("org cost limits after down+up = %d/%d, want 0/0", got.DailyCostLimit, got.MonthlyCostLimit)
		}
		k, err := d.GetAPIKey(ctx, key.ID)
		if err != nil {
			t.Fatalf("GetAPIKey after up migration: %v", err)
		}
		if k.DailyCostLimit != 0 {
			t.Errorf("key daily cost limit after down+up = %d, want 0", k.DailyCostLimit)
		}
	})
}

func TestCostLimits_RoundTripAllScopes(t *testing.T) {
	t.Parallel()
	forEachDialect(t, func(t *testing.T, d *DB) {
		ctx := context.Background()

		org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "cost-rt", DailyCostLimit: bigCost, MonthlyCostLimit: 3 * bigCost})
		if org.DailyCostLimit != bigCost || org.MonthlyCostLimit != 3*bigCost {
			t.Errorf("created org = %d/%d", org.DailyCostLimit, org.MonthlyCostLimit)
		}
		user := mustCreateUser(t, d, CreateUserParams{Email: "cost-rt@example.com", DisplayName: "U"})
		team := mustCreateTeam(t, d, CreateTeamParams{OrgID: org.ID, Name: "T", Slug: "cost-rt-team", DailyCostLimit: 11_000_000, MonthlyCostLimit: 22_000_000})
		if team.DailyCostLimit != 11_000_000 || team.MonthlyCostLimit != 22_000_000 {
			t.Errorf("created team = %d/%d", team.DailyCostLimit, team.MonthlyCostLimit)
		}
		membership := mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})
		if membership.DailyCostLimit != 0 || membership.MonthlyCostLimit != 0 {
			t.Errorf("new membership = %d/%d, want 0/0", membership.DailyCostLimit, membership.MonthlyCostLimit)
		}
		membership, err := d.UpdateOrgMembership(ctx, membership.ID, UpdateOrgMembershipParams{
			DailyCostLimit: ptr(money.Micros(5_500_000)), MonthlyCostLimit: ptr(money.Micros(bigCost)),
		})
		if err != nil {
			t.Fatalf("UpdateOrgMembership: %v", err)
		}
		if membership.DailyCostLimit != 5_500_000 || membership.MonthlyCostLimit != bigCost {
			t.Errorf("updated membership = %d/%d", membership.DailyCostLimit, membership.MonthlyCostLimit)
		}

		keyParams := testKeyParams(org.ID, user.ID, team.ID, user.ID)
		keyParams.DailyCostLimit = 1_250_000
		keyParams.MonthlyCostLimit = 7_000_000
		key := mustCreateAPIKey(t, d, keyParams)
		if key.DailyCostLimit != 1_250_000 || key.MonthlyCostLimit != 7_000_000 {
			t.Errorf("created key = %d/%d", key.DailyCostLimit, key.MonthlyCostLimit)
		}

		rec, err := d.LoadActiveKey(ctx, key.ID)
		if err != nil {
			t.Fatalf("LoadActiveKey: %v", err)
		}
		type pair struct{ daily, monthly money.Micros }
		for scope, want := range map[string]pair{
			"key":  {1_250_000, 7_000_000},
			"org":  {bigCost, 3 * bigCost},
			"team": {11_000_000, 22_000_000},
			"user": {5_500_000, bigCost},
		} {
			var got pair
			switch scope {
			case "key":
				got = pair{rec.DailyCostLimit, rec.MonthlyCostLimit}
			case "org":
				got = pair{rec.OrgDailyCostLimit, rec.OrgMonthlyCostLimit}
			case "team":
				got = pair{rec.TeamDailyCostLimit, rec.TeamMonthlyCostLimit}
			case "user":
				got = pair{rec.UserDailyCostLimit, rec.UserMonthlyCostLimit}
			}
			if got != want {
				t.Errorf("LoadActiveKey %s cost limits = %v, want %v", scope, got, want)
			}
		}

		all, _, err := d.LoadAllActiveKeys(ctx)
		if err != nil {
			t.Fatalf("LoadAllActiveKeys: %v", err)
		}
		var found bool
		for _, r := range all {
			if r.ID == key.ID {
				found = true
				if r.OrgDailyCostLimit != bigCost || r.UserMonthlyCostLimit != bigCost || r.TeamDailyCostLimit != 11_000_000 || r.DailyCostLimit != 1_250_000 {
					t.Errorf("LoadAllActiveKeys record cost limits wrong: %+v", r)
				}
			}
		}
		if !found {
			t.Fatal("key missing from LoadAllActiveKeys")
		}

		// Partial update: only the daily limit changes.
		updatedOrg, err := d.UpdateOrg(ctx, org.ID, UpdateOrgParams{DailyCostLimit: ptr(money.Micros(0))})
		if err != nil {
			t.Fatalf("UpdateOrg: %v", err)
		}
		if updatedOrg.DailyCostLimit != 0 || updatedOrg.MonthlyCostLimit != 3*bigCost {
			t.Errorf("updated org = %d/%d, want 0/%d", updatedOrg.DailyCostLimit, updatedOrg.MonthlyCostLimit, 3*bigCost)
		}
		updatedTeam, err := d.UpdateTeam(ctx, team.ID, UpdateTeamParams{MonthlyCostLimit: ptr(money.Micros(1))})
		if err != nil {
			t.Fatalf("UpdateTeam: %v", err)
		}
		if updatedTeam.DailyCostLimit != 11_000_000 || updatedTeam.MonthlyCostLimit != 1 {
			t.Errorf("updated team = %d/%d", updatedTeam.DailyCostLimit, updatedTeam.MonthlyCostLimit)
		}
		updatedKey, err := d.UpdateAPIKey(ctx, key.ID, UpdateAPIKeyParams{DailyCostLimit: ptr(bigCost)})
		if err != nil {
			t.Fatalf("UpdateAPIKey: %v", err)
		}
		if updatedKey.DailyCostLimit != bigCost || updatedKey.MonthlyCostLimit != 7_000_000 {
			t.Errorf("updated key = %d/%d", updatedKey.DailyCostLimit, updatedKey.MonthlyCostLimit)
		}

		// Rotation keeps the cost limits.
		plain, err := keygen.Generate(keygen.KeyTypeUser)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		rotated, err := d.RotateKeyTx(ctx, key.ID, "2099-01-01T00:00:00Z", CreateAPIKeyParams{
			KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain), KeyType: keygen.KeyTypeUser,
			Name: "rotated", OrgID: org.ID, UserID: ptr(user.ID), TeamID: ptr(team.ID), CreatedBy: user.ID,
			DailyCostLimit: updatedKey.DailyCostLimit, MonthlyCostLimit: updatedKey.MonthlyCostLimit,
		})
		if err != nil {
			t.Fatalf("RotateKeyTx: %v", err)
		}
		if rotated.NewKey.DailyCostLimit != bigCost || rotated.NewKey.MonthlyCostLimit != 7_000_000 {
			t.Errorf("rotated key = %d/%d", rotated.NewKey.DailyCostLimit, rotated.NewKey.MonthlyCostLimit)
		}
	})
}

func TestUpdateOrgMembership_CostLimitOnlyIsAnUpdate(t *testing.T) {
	t.Parallel()
	d := openMigratedDB(t)
	org := mustCreateOrg(t, d, CreateOrgParams{Name: "O", Slug: "cost-only"})
	user := mustCreateUser(t, d, CreateUserParams{Email: "cost-only@example.com", DisplayName: "U"})
	m := mustCreateMembership(t, d, CreateOrgMembershipParams{OrgID: org.ID, UserID: user.ID, Role: "member"})

	got, err := d.UpdateOrgMembership(context.Background(), m.ID, UpdateOrgMembershipParams{DailyCostLimit: ptr(money.Micros(42))})
	if err != nil {
		t.Fatalf("UpdateOrgMembership: %v", err)
	}
	if got.DailyCostLimit != 42 {
		t.Errorf("DailyCostLimit = %d, want 42", got.DailyCostLimit)
	}
}
