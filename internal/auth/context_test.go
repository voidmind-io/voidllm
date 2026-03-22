package auth

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

func TestKeyInfoFromCtx_WithoutMiddleware(t *testing.T) {
	t.Parallel()

	// When no middleware has run, KeyInfoFromCtx must return nil.
	app := fiber.New()
	var got *KeyInfo
	app.Get("/", func(c fiber.Ctx) error {
		got = KeyInfoFromCtx(c)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if got != nil {
		t.Errorf("KeyInfoFromCtx without middleware = %+v, want nil", got)
	}
}

func TestKeyInfoFromCtx_AfterMiddleware(t *testing.T) {
	t.Parallel()

	keyCache := cache.New[string, KeyInfo]()
	future := time.Now().Add(24 * time.Hour)
	want := KeyInfo{
		ID:                "ctx-key-001",
		KeyType:           keygen.KeyTypeUser,
		Role:              RoleOrgAdmin,
		OrgID:             "org-ctx",
		TeamID:            "team-ctx",
		UserID:            "user-ctx",
		ServiceAccountID:  "",
		Name:              "context test key",
		DailyTokenLimit:   1000,
		MonthlyTokenLimit: 10000,
		RequestsPerMinute: 60,
		RequestsPerDay:    1440,
		ExpiresAt:         &future,
	}
	raw := storeKey(t, keyCache, want, keygen.KeyTypeUser)

	var got *KeyInfo
	app := fiber.New()
	app.Use(Middleware(keyCache, testSecret))
	app.Get("/", func(c fiber.Ctx) error {
		got = KeyInfoFromCtx(c)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("Authorization", "Bearer "+raw)

	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("unexpected status %d", resp.StatusCode)
	}
	if got == nil {
		t.Fatal("KeyInfoFromCtx after middleware = nil, want non-nil")
	}
	if got.ID != want.ID {
		t.Errorf("KeyInfo.ID = %q, want %q", got.ID, want.ID)
	}
	if got.Role != want.Role {
		t.Errorf("KeyInfo.Role = %q, want %q", got.Role, want.Role)
	}
	if got.OrgID != want.OrgID {
		t.Errorf("KeyInfo.OrgID = %q, want %q", got.OrgID, want.OrgID)
	}
	if got.TeamID != want.TeamID {
		t.Errorf("KeyInfo.TeamID = %q, want %q", got.TeamID, want.TeamID)
	}
	if got.UserID != want.UserID {
		t.Errorf("KeyInfo.UserID = %q, want %q", got.UserID, want.UserID)
	}
	if got.Name != want.Name {
		t.Errorf("KeyInfo.Name = %q, want %q", got.Name, want.Name)
	}
	if got.DailyTokenLimit != want.DailyTokenLimit {
		t.Errorf("KeyInfo.DailyTokenLimit = %d, want %d", got.DailyTokenLimit, want.DailyTokenLimit)
	}
	if got.MonthlyTokenLimit != want.MonthlyTokenLimit {
		t.Errorf("KeyInfo.MonthlyTokenLimit = %d, want %d", got.MonthlyTokenLimit, want.MonthlyTokenLimit)
	}
	if got.RequestsPerMinute != want.RequestsPerMinute {
		t.Errorf("KeyInfo.RequestsPerMinute = %d, want %d", got.RequestsPerMinute, want.RequestsPerMinute)
	}
	if got.RequestsPerDay != want.RequestsPerDay {
		t.Errorf("KeyInfo.RequestsPerDay = %d, want %d", got.RequestsPerDay, want.RequestsPerDay)
	}
}

func TestKeyInfoFromCtx_NilLocals(t *testing.T) {
	t.Parallel()

	// Locals key exists but holds a wrong type: must still return nil gracefully.
	app := fiber.New()
	var got *KeyInfo
	app.Get("/", func(c fiber.Ctx) error {
		// Store something that is not a *KeyInfo under the same key.
		c.Locals(keyInfoKey, "not-a-key-info")
		got = KeyInfoFromCtx(c)
		return c.SendString("ok")
	})

	req := httptest.NewRequest("GET", "/", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer resp.Body.Close()

	if got != nil {
		t.Errorf("KeyInfoFromCtx with wrong locals type = %+v, want nil", got)
	}
}
