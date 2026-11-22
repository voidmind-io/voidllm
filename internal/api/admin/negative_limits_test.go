package admin_test

import (
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestOrgTeamLimits_RejectNegative(t *testing.T) {
	t.Parallel()

	app, database, keyCache := setupTestApp(t, "file:TestNegLimits?mode=memory&cache=private")
	org := mustCreateOrg(t, database, "Neg Org", "neg-org")
	team := mustCreateTeam(t, database, org.ID, "Neg Team", "neg-team")
	adminKey := mustCreateSystemAdminCallerKey(t, database, keyCache, org.ID)

	fields := []string{"daily_token_limit", "monthly_token_limit", "requests_per_minute", "requests_per_day"}
	endpoints := []struct {
		name, method, path string
		extra              map[string]any
	}{
		{"create org", "POST", "/api/v1/orgs", map[string]any{"name": "N2", "slug": "neg-org-2"}},
		{"update org", "PATCH", "/api/v1/orgs/" + org.ID, nil},
		{"create team", "POST", "/api/v1/orgs/" + org.ID + "/teams", map[string]any{"name": "T2", "slug": "neg-team-2"}},
		{"update team", "PATCH", "/api/v1/orgs/" + org.ID + "/teams/" + team.ID, nil},
	}
	for _, ep := range endpoints {
		for _, f := range fields {
			t.Run(ep.name+" "+f, func(t *testing.T) {
				body := map[string]any{f: -1}
				for k, v := range ep.extra {
					body[k] = v
				}
				req := httptest.NewRequest(ep.method, ep.path, bodyJSON(t, body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+adminKey)
				resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
				if err != nil {
					t.Fatalf("app.Test: %v", err)
				}
				defer resp.Body.Close()
				if resp.StatusCode != fiber.StatusBadRequest {
					t.Fatalf("status = %d, want 400", resp.StatusCode)
				}
				var out map[string]any
				decodeBody(t, resp.Body, &out)
				if want := f + " must be >= 0"; !containsStr(out, want) {
					t.Errorf("response %v does not mention %q", out, want)
				}
			})
		}
	}
}

// containsStr reports whether any string value nested in m equals want.
func containsStr(m map[string]any, want string) bool {
	for _, v := range m {
		switch x := v.(type) {
		case string:
			if x == want {
				return true
			}
		case map[string]any:
			if containsStr(x, want) {
				return true
			}
		}
	}
	return false
}
