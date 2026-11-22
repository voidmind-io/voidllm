package auth

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// TestMiddleware_XAPIKey verifies the x-api-key fallback used by Anthropic SDKs.
func TestMiddleware_XAPIKey(t *testing.T) {
	t.Parallel()

	info := KeyInfo{ID: "key-x", KeyType: keygen.KeyTypeUser, Role: RoleMember, OrgID: "org-x"}

	tests := []struct {
		name       string
		headers    func(valid string) map[string]string
		wantStatus int
	}{
		{"x-api-key only", func(v string) map[string]string { return map[string]string{"x-api-key": v} }, fiber.StatusOK},
		{"x-api-key with surrounding space", func(v string) map[string]string { return map[string]string{"x-api-key": " " + v + " "} }, fiber.StatusOK},
		{"bearer only", func(v string) map[string]string { return map[string]string{"Authorization": "Bearer " + v} }, fiber.StatusOK},
		{"bearer wins over invalid x-api-key", func(v string) map[string]string {
			return map[string]string{"Authorization": "Bearer " + v, "x-api-key": "vl_uk_invalid"}
		}, fiber.StatusOK},
		{"empty-scheme authorization blocks x-api-key", func(v string) map[string]string {
			return map[string]string{"Authorization": "Bearer ", "x-api-key": v}
		}, fiber.StatusUnauthorized},
		{"empty authorization header blocks x-api-key", func(v string) map[string]string {
			return map[string]string{"Authorization": "", "x-api-key": v}
		}, fiber.StatusUnauthorized},
		{"invalid bearer wins over valid x-api-key", func(v string) map[string]string {
			return map[string]string{"Authorization": "Bearer vl_uk_invalid", "x-api-key": v}
		}, fiber.StatusUnauthorized},
		{"non-bearer authorization never falls back to x-api-key", func(v string) map[string]string {
			return map[string]string{"Authorization": "Basic dXNlcjpwYXNz", "x-api-key": v}
		}, fiber.StatusUnauthorized},
		{"lowercase bearer scheme", func(v string) map[string]string { return map[string]string{"Authorization": "bearer " + v} }, fiber.StatusOK},
		{"invalid x-api-key", func(string) map[string]string { return map[string]string{"x-api-key": "vl_uk_invalid"} }, fiber.StatusUnauthorized},
		{"malformed x-api-key", func(string) map[string]string { return map[string]string{"x-api-key": "sk-ant-nope"} }, fiber.StatusUnauthorized},
		{"no credentials", func(string) map[string]string { return nil }, fiber.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := "/v1/messages"
			kc := cache.New[string, KeyInfo]()
			app := fiber.New()
			app.Use(Middleware(kc, testSecret))
			app.All("/*", func(c fiber.Ctx) error { return c.SendString("ok") })
			raw := storeKey(t, kc, info, keygen.KeyTypeUser)

			req := httptest.NewRequest(fiber.MethodGet, path, nil)
			for k, v := range tc.headers(raw) {
				req.Header.Set(k, v)
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

// TestMiddleware_AnthropicEnvelopeOnMessages verifies that auth failures on the
// native Messages endpoints use the Anthropic error envelope while other
// endpoints keep the standard one.
func TestMiddleware_AnthropicEnvelopeOnMessages(t *testing.T) {
	t.Parallel()

	app := fiber.New()
	app.Use(Middleware(cache.New[string, KeyInfo](), testSecret))
	app.All("/v1/*", func(c fiber.Ctx) error { return c.SendString("ok") })

	read := func(path string) (int, string) {
		req := httptest.NewRequest(fiber.MethodPost, path, strings.NewReader("{}"))
		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		var sb strings.Builder
		buf := make([]byte, 512)
		for {
			n, rerr := resp.Body.Read(buf)
			sb.Write(buf[:n])
			if rerr != nil {
				break
			}
		}
		return resp.StatusCode, sb.String()
	}

	for _, p := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		status, body := read(p)
		want := `{"type":"error","error":{"type":"authentication_error","message":"missing authorization header"}}`
		if status != fiber.StatusUnauthorized || body != want {
			t.Errorf("%s: status %d body %s", p, status, body)
		}
	}
	status, body := read("/v1/responses")
	if status != fiber.StatusUnauthorized || !strings.Contains(body, `"code":"unauthorized"`) {
		t.Errorf("/v1/responses: status %d body %s", status, body)
	}
}

// TestMiddleware_XAPIKeyRejectedOutsideProxyRoutes keeps the admin API
// Bearer-only.
func TestMiddleware_XAPIKeyRejectedOutsideProxyRoutes(t *testing.T) {
	t.Parallel()

	kc := cache.New[string, KeyInfo]()
	app := fiber.New()
	app.Use(Middleware(kc, testSecret))
	app.All("/*", func(c fiber.Ctx) error { return c.SendString("ok") })
	raw := storeKey(t, kc, KeyInfo{ID: "k", KeyType: keygen.KeyTypeUser, Role: RoleMember, OrgID: "o"}, keygen.KeyTypeUser)

	for path, want := range map[string]int{
		"/api/v1/orgs": fiber.StatusUnauthorized,
		"/v1/models":   fiber.StatusOK,
	} {
		req := httptest.NewRequest(fiber.MethodGet, path, nil)
		req.Header.Set("x-api-key", raw)
		resp, err := app.Test(req, fiber.TestConfig{Timeout: testTimeout})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: status = %d, want %d", path, resp.StatusCode, want)
		}
	}
}
