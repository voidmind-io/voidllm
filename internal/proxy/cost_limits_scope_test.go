package proxy

import (
	"net/http"
	"testing"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/ratelimit"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// captureChecker records the ScopeLimits it is asked to check.
type captureChecker struct {
	got ratelimit.ScopeLimits
}

func (c *captureChecker) CheckRate(_ ratelimit.Scopes, l ratelimit.ScopeLimits) error {
	c.got = l
	return nil
}

// TestCheckLimits_FillsCostLimits verifies the cached cost limits reach the
// four scope limits handed to the rate limiter (as micro-units).
func TestCheckLimits_FillsCostLimits(t *testing.T) {
	t.Parallel()

	info := auth.KeyInfo{
		ID: "cost-key", KeyType: keygen.KeyTypeUser, OrgID: "cost-org", Role: auth.RoleMember,
		DailyCostLimit: 1, MonthlyCostLimit: 2,
		UserDailyCostLimit: 3, UserMonthlyCostLimit: 4,
		TeamDailyCostLimit: 5, TeamMonthlyCostLimit: 6,
		OrgDailyCostLimit: 7, OrgMonthlyCostLimit: 8,
	}
	h := testProxyHandler(t, "http://127.0.0.1:1")
	checker := &captureChecker{}
	h.RateLimiter = checker
	app, key := testAppWithAuth(t, h, info)

	resp := postLimited(t, app, "/v1/chat/completions", key)
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatalf("status = %d, request was not authenticated", resp.StatusCode)
	}

	want := map[string][2]int64{
		"key": {1, 2}, "user": {3, 4}, "team": {5, 6}, "org": {7, 8},
	}
	got := map[string][2]int64{
		"key":  {checker.got.Key.DailyCostLimit, checker.got.Key.MonthlyCostLimit},
		"user": {checker.got.User.DailyCostLimit, checker.got.User.MonthlyCostLimit},
		"team": {checker.got.Team.DailyCostLimit, checker.got.Team.MonthlyCostLimit},
		"org":  {checker.got.Org.DailyCostLimit, checker.got.Org.MonthlyCostLimit},
	}
	for scope, w := range want {
		if got[scope] != w {
			t.Errorf("%s cost limits = %v, want %v", scope, got[scope], w)
		}
	}
}
