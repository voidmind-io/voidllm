package admin_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/money"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// costFixture is an app with one org, team, user (org member), user key and a
// system admin caller key.
type costFixture struct {
	app        *fiber.App
	database   *db.DB
	keyCache   *cache.Cache[string, auth.KeyInfo]
	org        *db.Org
	team       *db.Team
	user       *db.User
	membership string
	apiKeyID   string
	adminKey   string
}

// newCostFixture builds a costFixture. licensed selects the dev license (all
// features) or the community license (no cost budgets).
func newCostFixture(t *testing.T, name string, licensed bool) *costFixture {
	t.Helper()

	ctx := context.Background()
	database, err := db.Open(ctx, config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             "file:" + name + "?mode=memory&cache=private",
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatalf("open test DB: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := db.RunMigrations(ctx, database.SQL(), db.SQLiteDialect{}, slog.Default()); err != nil {
		t.Fatalf("run migrations: %v", err)
	}

	keyCache := cache.New[string, auth.KeyInfo]()
	handler := &admin.Handler{
		DB:         database,
		HMACSecret: testHMACSecret,
		KeyCache:   keyCache,
		License:    license.NewHolder(license.Verify("", licensed)),
		Currency:   "EUR",
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	app := fiber.New()
	admin.RegisterRoutes(app, handler, keyCache, testHMACSecret, nil)

	f := &costFixture{app: app, database: database, keyCache: keyCache}
	f.org = mustCreateOrg(t, database, "Cost Org", "cost-org-"+name)
	f.team = mustCreateTeam(t, database, f.org.ID, "Cost Team", "cost-team")
	f.user = mustCreateUser(t, database, name+"@example.com", "U")
	mustCreateUserMemberships(t, database, f.org.ID, f.team.ID, f.user.ID)
	memberships, err := database.ListOrgMemberships(ctx, f.org.ID, "", 10)
	if err != nil || len(memberships) == 0 {
		t.Fatalf("list memberships: %v", err)
	}
	f.membership = memberships[0].ID
	f.adminKey = mustCreateSystemAdminCallerKey(t, database, keyCache, f.org.ID)

	plain, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	k, err := database.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain), KeyType: keygen.KeyTypeUser,
		Name: "cost key", OrgID: f.org.ID, UserID: &f.user.ID, TeamID: &f.team.ID, CreatedBy: f.user.ID,
	})
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	f.apiKeyID = k.ID
	return f
}

// call sends a JSON request and returns the status and decoded JSON object.
func (f *costFixture) call(t *testing.T, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bodyJSON(t, body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := f.app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		decodeBody(t, strings.NewReader(string(raw)), &out)
	}
	return resp.StatusCode, out
}

func (f *costFixture) orgPath() string  { return "/api/v1/orgs/" + f.org.ID }
func (f *costFixture) teamPath() string { return f.orgPath() + "/teams/" + f.team.ID }
func (f *costFixture) keyPath() string  { return f.orgPath() + "/keys/" + f.apiKeyID }
func (f *costFixture) memberPath() string {
	return f.orgPath() + "/members/" + f.membership
}

func wantCost(t *testing.T, what string, got map[string]any, daily, monthly string) {
	t.Helper()
	if got["daily_cost_limit"] != daily || got["monthly_cost_limit"] != monthly {
		t.Errorf("%s cost limits = %v / %v, want %s / %s (body %v)", what, got["daily_cost_limit"], got["monthly_cost_limit"], daily, monthly, got)
	}
}

func TestCostLimits_RoundTripPerScope(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t, "CostRoundTrip", true)

	// Organization: create with a string and a number literal.
	status, org := f.call(t, "POST", "/api/v1/orgs", f.adminKey, map[string]any{
		"name": "Second", "slug": "cost-second", "daily_cost_limit": "25.00", "monthly_cost_limit": 500,
	})
	if status != fiber.StatusCreated {
		t.Fatalf("create org status = %d, body %v", status, org)
	}
	wantCost(t, "created org", org, "25.00", "500.00")

	status, got := f.call(t, "PATCH", f.orgPath(), f.adminKey, map[string]any{"daily_cost_limit": "0.5"})
	if status != fiber.StatusOK {
		t.Fatalf("patch org status = %d, body %v", status, got)
	}
	wantCost(t, "patched org", got, "0.50", "0.00")
	status, got = f.call(t, "PATCH", f.orgPath(), f.adminKey, map[string]any{"monthly_cost_limit": 12.5})
	if status != fiber.StatusOK {
		t.Fatalf("patch org monthly status = %d, body %v", status, got)
	}
	wantCost(t, "patched org monthly", got, "0.50", "12.50")
	status, got = f.call(t, "GET", f.orgPath(), f.adminKey, nil)
	if status != fiber.StatusOK {
		t.Fatalf("get org status = %d", status)
	}
	wantCost(t, "get org", got, "0.50", "12.50")

	// Team.
	status, team := f.call(t, "POST", f.orgPath()+"/teams", f.adminKey, map[string]any{
		"name": "T2", "slug": "cost-t2", "daily_cost_limit": "1.234567", "monthly_cost_limit": "100",
	})
	if status != fiber.StatusCreated {
		t.Fatalf("create team status = %d, body %v", status, team)
	}
	wantCost(t, "created team", team, "1.234567", "100.00")
	status, got = f.call(t, "PATCH", f.teamPath(), f.adminKey, map[string]any{"daily_cost_limit": "7", "monthly_cost_limit": "70.10"})
	if status != fiber.StatusOK {
		t.Fatalf("patch team status = %d, body %v", status, got)
	}
	wantCost(t, "patched team", got, "7.00", "70.10")
	status, got = f.call(t, "GET", f.teamPath(), f.adminKey, nil)
	if status != fiber.StatusOK {
		t.Fatalf("get team status = %d", status)
	}
	wantCost(t, "get team", got, "7.00", "70.10")

	// API key.
	status, key := f.call(t, "POST", f.orgPath()+"/keys", f.adminKey, map[string]any{
		"name": "K2", "key_type": keygen.KeyTypeUser, "user_id": f.user.ID, "team_id": f.team.ID,
		"daily_cost_limit": "3", "monthly_cost_limit": "30",
	})
	if status != fiber.StatusCreated {
		t.Fatalf("create key status = %d, body %v", status, key)
	}
	wantCost(t, "created key", key, "3.00", "30.00")
	status, got = f.call(t, "PATCH", f.keyPath(), f.adminKey, map[string]any{"daily_cost_limit": "4.25"})
	if status != fiber.StatusOK {
		t.Fatalf("patch key status = %d, body %v", status, got)
	}
	wantCost(t, "patched key", got, "4.25", "0.00")
	status, got = f.call(t, "GET", f.keyPath(), f.adminKey, nil)
	if status != fiber.StatusOK {
		t.Fatalf("get key status = %d", status)
	}
	wantCost(t, "get key", got, "4.25", "0.00")

	// Rotation keeps the limits.
	status, rotated := f.call(t, "POST", f.keyPath()+"/rotate", f.adminKey, nil)
	if status != fiber.StatusCreated && status != fiber.StatusOK {
		t.Fatalf("rotate key status = %d, body %v", status, rotated)
	}
	newKey, _ := rotated["new_key"].(map[string]any)
	newID, _ := newKey["id"].(string)
	status, got = f.call(t, "GET", f.orgPath()+"/keys/"+newID, f.adminKey, nil)
	if status != fiber.StatusOK {
		t.Fatalf("get rotated key status = %d, body %v", status, got)
	}
	wantCost(t, "rotated key", got, "4.25", "0.00")

	// Org membership.
	status, got = f.call(t, "PATCH", f.memberPath(), f.adminKey, map[string]any{"daily_cost_limit": "9", "monthly_cost_limit": "90"})
	if status != fiber.StatusOK {
		t.Fatalf("patch membership status = %d, body %v", status, got)
	}
	wantCost(t, "patched membership", got, "9.00", "90.00")
	m, err := f.database.GetOrgMembership(context.Background(), f.membership)
	if err != nil {
		t.Fatalf("GetOrgMembership: %v", err)
	}
	if m.DailyCostLimit != 9_000_000 || m.MonthlyCostLimit != 90_000_000 {
		t.Errorf("stored membership cost limits = %d/%d", m.DailyCostLimit, m.MonthlyCostLimit)
	}

	// Setting back to 0 clears the limit.
	status, got = f.call(t, "PATCH", f.teamPath(), f.adminKey, map[string]any{"daily_cost_limit": "0", "monthly_cost_limit": 0})
	if status != fiber.StatusOK {
		t.Fatalf("clear team status = %d, body %v", status, got)
	}
	wantCost(t, "cleared team", got, "0.00", "0.00")
}

func TestCostLimits_InvalidInputNamesField(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t, "CostInvalid", true)

	endpoints := []struct {
		name, method, path string
		extra              map[string]any
	}{
		{"create org", "POST", "/api/v1/orgs", map[string]any{"name": "N", "slug": "cost-inv-org"}},
		{"update org", "PATCH", f.orgPath(), nil},
		{"create team", "POST", f.orgPath() + "/teams", map[string]any{"name": "N", "slug": "cost-inv-team"}},
		{"update team", "PATCH", f.teamPath(), nil},
		{"create key", "POST", f.orgPath() + "/keys", map[string]any{"name": "N", "key_type": keygen.KeyTypeUser, "user_id": f.user.ID, "team_id": f.team.ID}},
		{"update key", "PATCH", f.keyPath(), nil},
		{"update membership", "PATCH", f.memberPath(), nil},
	}
	values := []any{"-1", -1, "abc", "1e3", json.RawMessage("1e3"), "0.0000001", "1,5", "", true, []any{}, "1000000000001"}
	for _, ep := range endpoints {
		for _, field := range []string{"daily_cost_limit", "monthly_cost_limit"} {
			for i, v := range values {
				t.Run(fmt.Sprintf("%s %s %d", ep.name, field, i), func(t *testing.T) {
					body := map[string]any{field: v}
					for k, x := range ep.extra {
						body[k] = x
					}
					status, out := f.call(t, ep.method, ep.path, f.adminKey, body)
					if status != fiber.StatusBadRequest {
						t.Fatalf("value %#v: status = %d, want 400 (body %v)", v, status, out)
					}
					if !strings.Contains(fmt.Sprint(out), field+" must be") {
						t.Errorf("response %v does not name %q", out, field)
					}
				})
			}
		}
	}
}

func TestCostLimits_LicenseGate(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t, "CostGate", false)
	ctx := context.Background()

	assertForbiddenFeature := func(t *testing.T, what string, status int, body map[string]any) {
		t.Helper()
		if status != fiber.StatusForbidden {
			t.Fatalf("%s: status = %d, want 403 (body %v)", what, status, body)
		}
		if !strings.Contains(fmt.Sprint(body), "feature_unavailable") {
			t.Errorf("%s: body %v does not carry code feature_unavailable", what, body)
		}
	}

	// Non-zero is rejected on every write path.
	status, body := f.call(t, "POST", "/api/v1/orgs", f.adminKey, map[string]any{"name": "N", "slug": "gate-org", "daily_cost_limit": "1"})
	assertForbiddenFeature(t, "create org", status, body)
	status, body = f.call(t, "PATCH", f.orgPath(), f.adminKey, map[string]any{"monthly_cost_limit": "1"})
	assertForbiddenFeature(t, "update org", status, body)
	status, body = f.call(t, "POST", f.orgPath()+"/teams", f.adminKey, map[string]any{"name": "N", "slug": "gate-team", "daily_cost_limit": "1"})
	assertForbiddenFeature(t, "create team", status, body)
	status, body = f.call(t, "PATCH", f.teamPath(), f.adminKey, map[string]any{"daily_cost_limit": "1"})
	assertForbiddenFeature(t, "update team", status, body)
	status, body = f.call(t, "POST", f.orgPath()+"/keys", f.adminKey, map[string]any{
		"name": "N", "key_type": keygen.KeyTypeUser, "user_id": f.user.ID, "team_id": f.team.ID, "monthly_cost_limit": "1",
	})
	assertForbiddenFeature(t, "create key", status, body)
	status, body = f.call(t, "PATCH", f.keyPath(), f.adminKey, map[string]any{"daily_cost_limit": "1"})
	assertForbiddenFeature(t, "update key", status, body)
	status, body = f.call(t, "PATCH", f.memberPath(), f.adminKey, map[string]any{"daily_cost_limit": "1"})
	assertForbiddenFeature(t, "update membership", status, body)

	// Nothing was written by the rejected requests.
	org, err := f.database.GetOrg(ctx, f.org.ID)
	if err != nil || org.DailyCostLimit != 0 || org.MonthlyCostLimit != 0 {
		t.Fatalf("org after rejected writes = %+v, err %v", org, err)
	}

	// Zero (and omitting the fields) is always allowed.
	status, body = f.call(t, "PATCH", f.orgPath(), f.adminKey, map[string]any{"daily_cost_limit": "0", "monthly_cost_limit": 0, "name": "Renamed"})
	if status != fiber.StatusOK {
		t.Fatalf("zero limits status = %d, body %v", status, body)
	}
	status, body = f.call(t, "POST", f.orgPath()+"/teams", f.adminKey, map[string]any{"name": "Z", "slug": "gate-zero", "daily_cost_limit": "0"})
	if status != fiber.StatusCreated {
		t.Fatalf("create team with zero limit status = %d, body %v", status, body)
	}

	// Limits stored while licensed survive a lapsed license: the unchanged
	// value may be echoed back, a different value is rejected, and clearing
	// to 0 is allowed.
	if _, err := f.database.UpdateTeam(ctx, f.team.ID, db.UpdateTeamParams{DailyCostLimit: ptrMicros(5_000_000)}); err != nil {
		t.Fatalf("seed team limit: %v", err)
	}
	if _, err := f.database.UpdateOrgMembership(ctx, f.membership, db.UpdateOrgMembershipParams{MonthlyCostLimit: ptrMicros(8_000_000)}); err != nil {
		t.Fatalf("seed membership limit: %v", err)
	}
	if _, err := f.database.UpdateAPIKey(ctx, f.apiKeyID, db.UpdateAPIKeyParams{DailyCostLimit: ptrMicros(2_500_000)}); err != nil {
		t.Fatalf("seed key limit: %v", err)
	}
	if _, err := f.database.UpdateOrg(ctx, f.org.ID, db.UpdateOrgParams{MonthlyCostLimit: ptrMicros(9_000_000)}); err != nil {
		t.Fatalf("seed org limit: %v", err)
	}

	status, body = f.call(t, "PATCH", f.teamPath(), f.adminKey, map[string]any{"daily_cost_limit": "5.00", "name": "Echo"})
	if status != fiber.StatusOK {
		t.Fatalf("echo unchanged team limit status = %d, body %v", status, body)
	}
	wantCost(t, "echoed team", body, "5.00", "0.00")
	status, body = f.call(t, "PATCH", f.teamPath(), f.adminKey, map[string]any{"daily_cost_limit": "5.01"})
	assertForbiddenFeature(t, "change team limit", status, body)
	status, body = f.call(t, "PATCH", f.memberPath(), f.adminKey, map[string]any{"monthly_cost_limit": "8"})
	if status != fiber.StatusOK {
		t.Fatalf("echo unchanged membership limit status = %d, body %v", status, body)
	}
	status, body = f.call(t, "PATCH", f.keyPath(), f.adminKey, map[string]any{"daily_cost_limit": "2.5"})
	if status != fiber.StatusOK {
		t.Fatalf("echo unchanged key limit status = %d, body %v", status, body)
	}
	status, body = f.call(t, "PATCH", f.orgPath(), f.adminKey, map[string]any{"monthly_cost_limit": "9"})
	if status != fiber.StatusOK {
		t.Fatalf("echo unchanged org limit status = %d, body %v", status, body)
	}
	status, body = f.call(t, "PATCH", f.orgPath(), f.adminKey, map[string]any{"monthly_cost_limit": "10"})
	assertForbiddenFeature(t, "change org limit", status, body)
	status, body = f.call(t, "PATCH", f.teamPath(), f.adminKey, map[string]any{"daily_cost_limit": "0"})
	if status != fiber.StatusOK {
		t.Fatalf("clear team limit status = %d, body %v", status, body)
	}
	wantCost(t, "cleared team", body, "0.00", "0.00")
}

func TestCostLimits_TeamAdminCannotChangeTeamCostLimits(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t, "CostTeamAdmin", true)
	ctx := context.Background()

	if _, err := f.database.UpdateTeam(ctx, f.team.ID, db.UpdateTeamParams{DailyCostLimit: ptrMicros(5_000_000)}); err != nil {
		t.Fatalf("seed team limit: %v", err)
	}
	teamAdmin := addTestKeyWithTeam(t, f.keyCache, auth.RoleTeamAdmin, f.org.ID, f.team.ID, f.user.ID)

	status, body := f.call(t, "PATCH", f.teamPath(), teamAdmin, map[string]any{"daily_cost_limit": "50"})
	if status != fiber.StatusForbidden {
		t.Fatalf("team admin change status = %d, want 403 (body %v)", status, body)
	}
	status, body = f.call(t, "PATCH", f.teamPath(), teamAdmin, map[string]any{"monthly_cost_limit": "1"})
	if status != fiber.StatusForbidden {
		t.Fatalf("team admin change monthly status = %d, want 403 (body %v)", status, body)
	}

	status, body = f.call(t, "PATCH", f.keyPath(), teamAdmin, map[string]any{"daily_cost_limit": "1"})
	if status != fiber.StatusForbidden {
		t.Fatalf("team admin change key cost limit status = %d, want 403 (body %v)", status, body)
	}

	// Re-sending the stored value is allowed but never written.
	status, body = f.call(t, "PATCH", f.teamPath(), teamAdmin, map[string]any{"daily_cost_limit": "5.00", "name": "Renamed"})
	if status != fiber.StatusOK {
		t.Fatalf("team admin unchanged status = %d, body %v", status, body)
	}
	wantCost(t, "team after unchanged echo", body, "5.00", "0.00")

	// Members and team admins cannot set cost limits on keys at creation.
	member := addTestKeyWithUser(t, f.keyCache, auth.RoleMember, f.org.ID, f.user.ID)
	status, body = f.call(t, "POST", f.orgPath()+"/keys", member, map[string]any{
		"name": "M", "key_type": keygen.KeyTypeUser, "team_id": f.team.ID, "daily_cost_limit": "1",
	})
	if status != fiber.StatusForbidden {
		t.Fatalf("member create key with cost limit status = %d, want 403 (body %v)", status, body)
	}
}

func TestMe_ExposesCurrency(t *testing.T) {
	t.Parallel()
	f := newCostFixture(t, "CostCurrency", true)
	token := addTestKeyWithUser(t, f.keyCache, auth.RoleMember, f.org.ID, f.user.ID)

	status, body := f.call(t, "GET", "/api/v1/me", token, nil)
	if status != fiber.StatusOK {
		t.Fatalf("me status = %d, body %v", status, body)
	}
	if body["currency"] != "EUR" {
		t.Errorf("currency = %v, want EUR", body["currency"])
	}
}

func ptrMicros(v money.Micros) *money.Micros { return &v }
