package auth

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// testTimeout is the per-request timeout passed to app.Test in all subtests.
const testTimeout = 5 * time.Second

// testSecret is a 32-byte HMAC secret used in all auth tests.
var testSecret = []byte("test-secret-key-32-bytes-long!!!")

// setupMiddlewareApp builds a Fiber app with auth.Middleware pre-wired and a
// single GET "/" handler that returns 200 with body "ok". It returns the app
// and the key cache so individual tests can populate it.
func setupMiddlewareApp(t *testing.T) (*fiber.App, *cache.Cache[string, KeyInfo]) {
	t.Helper()
	keyCache := cache.New[string, KeyInfo]()
	app := fiber.New()
	app.Use(Middleware(keyCache, testSecret))
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString("ok")
	})
	return app, keyCache
}

// storeKey generates a key of the given type, stores the corresponding
// KeyInfo in the cache, and returns the plaintext key.
func storeKey(t *testing.T, kc *cache.Cache[string, KeyInfo], info KeyInfo, keyType string) string {
	t.Helper()
	raw, err := keygen.Generate(keyType)
	if err != nil {
		t.Fatalf("keygen.Generate: %v", err)
	}
	hash := keygen.Hash(raw, testSecret)
	kc.Set(hash, info)
	return raw
}

func TestExtractBearerToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		want   string
	}{
		{
			name:   "valid bearer token",
			header: "Bearer vl_uk_abc123",
			want:   "vl_uk_abc123",
		},
		{
			name:   "empty header",
			header: "",
			want:   "",
		},
		{
			name:   "basic auth scheme",
			header: "Basic dXNlcjpwYXNz",
			want:   "",
		},
		{
			name:   "bearer with no token after space",
			header: "Bearer ",
			want:   "",
		},
		{
			name:   "bearer lowercase",
			header: "bearer vl_uk_abc",
			want:   "",
		},
		{
			name:   "bearer no space",
			header: "Bearervl_uk_abc",
			want:   "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := extractBearerToken(tc.header)
			if got != tc.want {
				t.Errorf("extractBearerToken(%q) = %q, want %q", tc.header, got, tc.want)
			}
		})
	}
}

func TestMiddleware(t *testing.T) {
	t.Parallel()

	baseInfo := KeyInfo{
		ID:      "key-001",
		KeyType: keygen.KeyTypeUser,
		Role:    RoleMember,
		OrgID:   "org-001",
	}

	past := time.Now().Add(-1 * time.Hour)
	future := time.Now().Add(1 * time.Hour)

	tests := []struct {
		name       string
		authHeader string
		setup      func(t *testing.T, kc *cache.Cache[string, KeyInfo]) string // returns auth header value
		wantStatus int
	}{
		{
			name: "missing authorization header",
			setup: func(t *testing.T, kc *cache.Cache[string, KeyInfo]) string {
				return ""
			},
			wantStatus: fiber.StatusUnauthorized,
		},
		{
			name: "basic auth scheme rejected",
			setup: func(t *testing.T, kc *cache.Cache[string, KeyInfo]) string {
				return "Basic dXNlcjpwYXNz"
			},
			wantStatus: fiber.StatusUnauthorized,
		},
		{
			name: "bearer with no token",
			setup: func(t *testing.T, kc *cache.Cache[string, KeyInfo]) string {
				return "Bearer "
			},
			wantStatus: fiber.StatusUnauthorized,
		},
		{
			name: "empty authorization header",
			setup: func(t *testing.T, kc *cache.Cache[string, KeyInfo]) string {
				return " "
			},
			wantStatus: fiber.StatusUnauthorized,
		},
		{
			name: "valid bearer but key not in cache",
			setup: func(t *testing.T, kc *cache.Cache[string, KeyInfo]) string {
				raw, err := keygen.Generate(keygen.KeyTypeUser)
				if err != nil {
					t.Fatalf("keygen.Generate: %v", err)
				}
				// deliberately do not store in cache
				return "Bearer " + raw
			},
			wantStatus: fiber.StatusUnauthorized,
		},
		{
			name: "bearer token with unrecognized prefix",
			setup: func(t *testing.T, kc *cache.Cache[string, KeyInfo]) string {
				return "Bearer notavalidprefix_abc123"
			},
			wantStatus: fiber.StatusUnauthorized,
		},
		{
			name: "expired key",
			setup: func(t *testing.T, kc *cache.Cache[string, KeyInfo]) string {
				info := baseInfo
				info.ExpiresAt = &past
				raw := storeKey(t, kc, info, keygen.KeyTypeUser)
				return "Bearer " + raw
			},
			wantStatus: fiber.StatusUnauthorized,
		},
		{
			name: "valid key with no expiration",
			setup: func(t *testing.T, kc *cache.Cache[string, KeyInfo]) string {
				info := baseInfo
				info.ExpiresAt = nil
				raw := storeKey(t, kc, info, keygen.KeyTypeUser)
				return "Bearer " + raw
			},
			wantStatus: fiber.StatusOK,
		},
		{
			name: "valid key with future expiration",
			setup: func(t *testing.T, kc *cache.Cache[string, KeyInfo]) string {
				info := baseInfo
				info.ExpiresAt = &future
				raw := storeKey(t, kc, info, keygen.KeyTypeUser)
				return "Bearer " + raw
			},
			wantStatus: fiber.StatusOK,
		},
		{
			name: "valid team key",
			setup: func(t *testing.T, kc *cache.Cache[string, KeyInfo]) string {
				info := baseInfo
				info.KeyType = keygen.KeyTypeTeam
				raw := storeKey(t, kc, info, keygen.KeyTypeTeam)
				return "Bearer " + raw
			},
			wantStatus: fiber.StatusOK,
		},
		{
			name: "valid service account key",
			setup: func(t *testing.T, kc *cache.Cache[string, KeyInfo]) string {
				info := baseInfo
				info.KeyType = keygen.KeyTypeSA
				raw := storeKey(t, kc, info, keygen.KeyTypeSA)
				return "Bearer " + raw
			},
			wantStatus: fiber.StatusOK,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app, keyCache := setupMiddlewareApp(t)
			authHeader := tc.setup(t, keyCache)

			req := httptest.NewRequest("GET", "/", nil)
			if authHeader != "" {
				req.Header.Set("Authorization", authHeader)
			}

			resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
}
