package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/ratelimit"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// userLimitTestHMACSecret is a fixed secret used only by this file's tests.
var userLimitTestHMACSecret = []byte("user-limit-e2e-test-hmac-secret")

// testAppWithTwoUserKeys wires a ProxyHandler with the real auth middleware and
// two distinct API keys that both belong to the same user in the same org, so
// a per-user limit sourced from the org membership can be verified to enforce
// across every key that user owns, not just per individual key.
func testAppWithTwoUserKeys(t *testing.T, handler *ProxyHandler, userID, orgID string, userRPM int) (app *fiber.App, key1, key2 string) {
	t.Helper()

	keyCache := cache.New[string, auth.KeyInfo]()

	mint := func(id string) string {
		raw, err := keygen.Generate(keygen.KeyTypeUser)
		if err != nil {
			t.Fatalf("keygen.Generate: %v", err)
		}
		hash := keygen.Hash(raw, userLimitTestHMACSecret)
		keyCache.Set(hash, auth.KeyInfo{
			ID:                    id,
			KeyType:               keygen.KeyTypeUser,
			Role:                  auth.RoleMember,
			OrgID:                 orgID,
			UserID:                userID,
			Name:                  "user limit e2e key " + id,
			UserRequestsPerMinute: userRPM,
		})
		return raw
	}

	key1 = mint("user-limit-e2e-key-1")
	key2 = mint("user-limit-e2e-key-2")

	app = fiber.New()
	app.Use(auth.Middleware(keyCache, userLimitTestHMACSecret))
	app.Get("/v1/models", handler.ModelsHandler)
	app.All("/v1/*", handler.Handle)

	return app, key1, key2
}

// TestHandle_UserLimitEnforcedAcrossMultipleKeys is the end-to-end test for a
// member with a per-user limit of requests_per_minute=5 sending requests
// alternating over two of their own API keys. Since the limit applies to the
// user (org-bound), not to either key individually, the 6th request overall
// — regardless of which of the two keys it uses — must be rejected with 429
// rate_limit_exceeded.
func TestHandle_UserLimitEnforcedAcrossMultipleKeys(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[]}`)
	}))
	t.Cleanup(upstream.Close)

	handler := testProxyHandler(t, upstream.URL)
	handler.RateLimiter = ratelimit.NewRateLimiter()

	const orgID = "user-limit-e2e-org"
	const userID = "user-limit-e2e-user"
	app, key1, key2 := testAppWithTwoUserKeys(t, handler, userID, orgID, 5)

	sendRequest := func(key string) *http.Response {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"test-model","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)

		resp, err := app.Test(req, testTimeout)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		return resp
	}

	keys := []string{key1, key2}
	for i := range 5 {
		resp := sendRequest(keys[i%2])
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d (key %d): status = %d, want 200; body: %s", i+1, i%2+1, resp.StatusCode, body)
		}
	}

	// The 6th request, alternating to the other key, must be rejected: the
	// user-scope counter (shared across both keys) is now at its limit of 5.
	resp := sendRequest(keys[5%2])
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("6th request: status = %d, want 429; body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "rate_limit_exceeded") {
		t.Errorf("6th request body = %s, want it to contain %q", body, "rate_limit_exceeded")
	}
}

// TestHandle_UserLimitDoesNotAffectDifferentUser verifies that the per-user
// limit enforced across a user's keys does not leak onto a different user's
// key in the same org — a sibling regression guard for the fix above.
func TestHandle_UserLimitDoesNotAffectDifferentUser(t *testing.T) {
	t.Parallel()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[]}`)
	}))
	t.Cleanup(upstream.Close)

	handler := testProxyHandler(t, upstream.URL)
	handler.RateLimiter = ratelimit.NewRateLimiter()

	const orgID = "user-limit-e2e-org-2"

	keyCache := cache.New[string, auth.KeyInfo]()
	mint := func(id, userID string, rpm int) string {
		raw, err := keygen.Generate(keygen.KeyTypeUser)
		if err != nil {
			t.Fatalf("keygen.Generate: %v", err)
		}
		hash := keygen.Hash(raw, userLimitTestHMACSecret)
		keyCache.Set(hash, auth.KeyInfo{
			ID: id, KeyType: keygen.KeyTypeUser, Role: auth.RoleMember,
			OrgID: orgID, UserID: userID, Name: "e2e-" + id,
			UserRequestsPerMinute: rpm,
		})
		return raw
	}

	exhaustedUserKey := mint("victim-key", "victim-user", 1)
	otherUserKey := mint("other-key", "other-user", 1)

	app := fiber.New()
	app.Use(auth.Middleware(keyCache, userLimitTestHMACSecret))
	app.All("/v1/*", handler.Handle)

	sendRequest := func(key string) *http.Response {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"test-model","messages":[]}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := app.Test(req, testTimeout)
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		return resp
	}

	// Exhaust victim-user's budget of 1.
	resp1 := sendRequest(exhaustedUserKey)
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("victim first request status = %d, want 200", resp1.StatusCode)
	}
	resp2 := sendRequest(exhaustedUserKey)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("victim second request status = %d, want 429", resp2.StatusCode)
	}

	// other-user, same org, must be entirely unaffected.
	resp3 := sendRequest(otherUserKey)
	body3, _ := io.ReadAll(resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("other user request status = %d, want 200 (must not share victim's budget); body: %s", resp3.StatusCode, body3)
	}
}
