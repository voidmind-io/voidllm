package admin_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// addScopedTestKey registers a cache entry with an explicit user and team.
func addScopedTestKey(t *testing.T, keyCache *cache.Cache[string, auth.KeyInfo], role, orgID, userID, teamID string) string {
	t.Helper()
	plaintext, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	keyCache.Set(keygen.Hash(plaintext, testHMACSecret), auth.KeyInfo{
		ID: "scoped-" + role, KeyType: keygen.KeyTypeUser, Role: role,
		OrgID: orgID, UserID: userID, TeamID: teamID, Name: "scoped " + role,
	})
	return plaintext
}

// getJSON performs an authenticated GET and decodes the JSON object body.
func getJSON(t *testing.T, app *fiber.App, url, key string) map[string]any {
	t.Helper()
	req := httptest.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}
	var got map[string]any
	decodeBody(t, resp.Body, &got)
	return got
}

// wantSummary asserts the summary object of a list response. A nil want means
// the field must be absent.
func wantSummary(t *testing.T, got map[string]any, want map[string]float64) {
	t.Helper()
	raw, present := got["summary"]
	if want == nil {
		if present {
			t.Fatalf("summary present = %v, want absent", raw)
		}
		return
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		t.Fatalf("summary = %v, want object", raw)
	}
	if len(obj) != len(want) {
		t.Errorf("summary = %v, want fields %v", obj, want)
	}
	for k, v := range want {
		if obj[k] != v {
			t.Errorf("summary[%q] = %v, want %v", k, obj[k], v)
		}
	}
}

func TestListSummary(t *testing.T) {
	t.Parallel()
	app, database, keyCache := setupTestApp(t, "file:TestListSummary?mode=memory&cache=private")
	ctx := context.Background()

	org := mustCreateOrg(t, database, "Sum Org", "sum-org")
	u1 := mustCreateUser(t, database, "sum1@example.com", "U1")
	u2 := mustCreateUser(t, database, "sum2@example.com", "U2")
	mustCreateOrgMembership(t, database, org.ID, u1.ID, auth.RoleOrgAdmin)
	mustCreateOrgMembership(t, database, org.ID, u2.ID, auth.RoleMember)
	team := mustCreateTeam(t, database, org.ID, "Sum Team", "sum-team")
	for i := range 24 { // more than the default page size of 20
		mustCreateTeam(t, database, org.ID, fmt.Sprintf("Filler %d", i), fmt.Sprintf("sum-filler-%d", i))
	}
	if _, err := database.CreateTeamMembership(ctx, db.CreateTeamMembershipParams{TeamID: team.ID, UserID: u2.ID, Role: auth.RoleMember}); err != nil {
		t.Fatalf("CreateTeamMembership: %v", err)
	}

	mkKey := func(i int, userID string, teamID *string, exp *string) {
		plain := fmt.Sprintf("vl_uk_listsummary%04d", i)
		_, err := database.CreateAPIKey(ctx, db.CreateAPIKeyParams{
			KeyHash: keygen.Hash(plain, testHMACSecret), KeyHint: keygen.Hint(plain),
			KeyType: keygen.KeyTypeUser, Name: plain, OrgID: org.ID,
			UserID: &userID, TeamID: teamID, ExpiresAt: exp, CreatedBy: userID,
		})
		if err != nil {
			t.Fatalf("CreateAPIKey: %v", err)
		}
	}
	soon := db.FormatTimestamp(time.Now().Add(24 * time.Hour))
	for i := range 22 {
		mkKey(i, u1.ID, nil, nil)
	}
	mkKey(100, u2.ID, nil, &soon)
	mkKey(101, u2.ID, &team.ID, nil)

	for i := range 3 {
		mustCreateServiceAccountHTTP(t, database, org.ID, u1.ID, fmt.Sprintf("sa-u1-%d", i), nil)
	}
	mustCreateServiceAccountHTTP(t, database, org.ID, u2.ID, "sa-u2", &team.ID)

	sysKey := addTestKey(t, keyCache, auth.RoleSystemAdmin, "")
	orgAdmin := addScopedTestKey(t, keyCache, auth.RoleOrgAdmin, org.ID, u1.ID, "")
	member := addScopedTestKey(t, keyCache, auth.RoleMember, org.ID, u2.ID, "")
	teamAdmin := addScopedTestKey(t, keyCache, auth.RoleTeamAdmin, org.ID, u2.ID, team.ID)

	orgBase := "/api/v1/orgs/" + org.ID
	tests := []struct {
		name string
		url  string
		key  string
		want map[string]float64
	}{
		{"teams absent without flag", orgBase + "/teams", orgAdmin, nil},
		{"teams org admin spans pages", orgBase + "/teams?summary=true&limit=5", orgAdmin, map[string]float64{"total": 25, "members": 1, "keys": 1}},
		{"teams team admin only own team", orgBase + "/teams?summary=true", teamAdmin, map[string]float64{"total": 1, "members": 1, "keys": 1}},
		{"members absent without flag", orgBase + "/members", orgAdmin, nil},
		{"members", orgBase + "/members?summary=true&limit=1", orgAdmin, map[string]float64{"total": 2, "admins": 1}},
		{"keys absent without flag", orgBase + "/keys", orgAdmin, nil},
		{"keys org admin spans pages", orgBase + "/keys?summary=true&limit=5", orgAdmin, map[string]float64{"total": 24, "active": 24, "expiring_soon": 1}},
		{"keys member sees own only", orgBase + "/keys?summary=true", member, map[string]float64{"total": 2, "active": 2, "expiring_soon": 1}},
		{"keys team admin team scope", orgBase + "/keys?summary=true", teamAdmin, map[string]float64{"total": 1, "active": 1, "expiring_soon": 0}},
		{"service accounts absent without flag", orgBase + "/service-accounts", orgAdmin, nil},
		{"service accounts org admin", orgBase + "/service-accounts?summary=true&limit=2", orgAdmin, map[string]float64{"total": 4, "org_scoped": 3, "team_scoped": 1}},
		{"service accounts member created_by", orgBase + "/service-accounts?summary=true", member, map[string]float64{"total": 1, "org_scoped": 0, "team_scoped": 1}},
		{"users absent without flag", "/api/v1/users", sysKey, nil},
		{"users", "/api/v1/users?summary=true&limit=1", sysKey, map[string]float64{"total": 2, "system_admins": 0, "sso": 0}},
		{"orgs absent without flag", "/api/v1/orgs", sysKey, nil},
		{"orgs system admin", "/api/v1/orgs?summary=true", sysKey, map[string]float64{"total": 1, "members": 2, "teams": 25}},
		{"orgs single org for org admin", "/api/v1/orgs?summary=true", orgAdmin, map[string]float64{"total": 1, "members": 2, "teams": 25}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wantSummary(t, getJSON(t, app, tt.url, tt.key), tt.want)
		})
	}
}
