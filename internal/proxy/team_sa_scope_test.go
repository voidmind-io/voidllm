package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/ratelimit"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// teamSAKeyInfo builds the KeyInfo of a team-bound sa_key the way the key
// cache does: through auth.KeyInfoFromRecord from a DB record whose
// api_keys.team_id is nil and whose service account carries the team.
func teamSAKeyInfo(t *testing.T, id, orgID, teamID string, teamRPM int) auth.KeyInfo {
	t.Helper()
	ki, ok := auth.KeyInfoFromRecord(db.KeyRecord{
		ID:                    id,
		KeyType:               keygen.KeyTypeSA,
		OrgID:                 orgID,
		ServiceAccountActive:  true,
		ServiceAccountTeamID:  &teamID,
		TeamActive:            true,
		TeamRequestsPerMinute: teamRPM,
	})
	if !ok {
		t.Fatal("KeyInfoFromRecord: ok = false for team-bound sa_key")
	}
	if ki.TeamID != teamID {
		t.Fatalf("KeyInfo.TeamID = %q, want %q", ki.TeamID, teamID)
	}
	return ki
}

// TestHandle_TeamBoundSAKey_ModelAllowlist verifies a team-bound sa_key is
// subject to the team's model allowlist.
func TestHandle_TeamBoundSAKey_ModelAllowlist(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[]}`)
	}))
	t.Cleanup(upstream.Close)

	const orgID, teamID = "sa-allow-org", "sa-allow-team"

	tests := []struct {
		name  string
		model string
		want  int
	}{
		{"model in team allowlist is allowed", "test-model", http.StatusOK},
		{"model not in team allowlist is denied", "no-key-model", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler := testProxyHandler(t, upstream.URL)
			access := NewModelAccessCache()
			access.Load(nil, map[string][]string{teamID: {"test-model"}}, nil)
			handler.AccessCache = access

			app, key := testAppWithAuth(t, handler, teamSAKeyInfo(t, "sa-allow-key", orgID, teamID, 0))

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model":"`+tc.model+`","messages":[]}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+key)
			resp, err := app.Test(req, testTimeout)
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				b, _ := io.ReadAll(resp.Body)
				t.Errorf("status = %d, want %d; body: %s", resp.StatusCode, tc.want, b)
			}
		})
	}
}

// TestHandle_TeamBoundSAKey_TeamRPM verifies a team-bound sa_key is subject to
// the team's requests_per_minute, and that two sa_keys of the same team share
// the team counter while another team's key does not.
func TestHandle_TeamBoundSAKey_TeamRPM(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[]}`)
	}))
	t.Cleanup(upstream.Close)

	handler := testProxyHandler(t, upstream.URL)
	handler.RateLimiter = ratelimit.NewRateLimiter()

	const orgID = "sa-rpm-org"
	appA, keyA := testAppWithAuth(t, handler, teamSAKeyInfo(t, "sa-rpm-key-a", orgID, "sa-rpm-team-a", 2))
	appB, keyB := testAppWithAuth(t, handler, teamSAKeyInfo(t, "sa-rpm-key-b", orgID, "sa-rpm-team-b", 2))

	send := func(app *fiber.App, key string) int {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"test-model","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := app.Test(req, testTimeout)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	for i := 1; i <= 2; i++ {
		if got := send(appA, keyA); got != http.StatusOK {
			t.Fatalf("team A request %d: status = %d, want 200", i, got)
		}
	}
	if got := send(appA, keyA); got != http.StatusTooManyRequests {
		t.Errorf("team A request 3: status = %d, want 429 (team RPM 2)", got)
	}
	if got := send(appB, keyB); got != http.StatusOK {
		t.Errorf("team B request 1: status = %d, want 200 (separate team counter)", got)
	}
}
