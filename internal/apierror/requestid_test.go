package apierror_test

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/voidmind-io/voidllm/internal/apierror"
)

// newMiddlewareApp builds a Fiber app with RequestIDMiddleware registered
// and a simple 200 OK handler at GET /test.
func newMiddlewareApp() *fiber.App {
	app := fiber.New()
	app.Use(apierror.RequestIDMiddleware())
	app.Get("/test", func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	})
	return app
}

func TestRequestIDMiddleware_GeneratesID(t *testing.T) {
	t.Parallel()

	app := newMiddlewareApp()

	// No X-Request-Id header — middleware must generate a UUIDv7.
	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}

	got := resp.Header.Get("X-Request-Id")
	if got == "" {
		t.Fatal("X-Request-Id response header is empty, want a UUIDv7")
	}

	if _, err := uuid.Parse(got); err != nil {
		t.Errorf("X-Request-Id %q is not a valid UUID: %v", got, err)
	}
}

func TestRequestIDMiddleware_AcceptsClientID(t *testing.T) {
	t.Parallel()

	// A valid UUID supplied by the client must be echoed back unchanged.
	const clientID = "019726de-f1c4-7a5d-8e2b-000000000002"

	app := newMiddlewareApp()

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-Id", clientID)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}

	got := resp.Header.Get("X-Request-Id")
	if got != clientID {
		t.Errorf("X-Request-Id = %q, want %q", got, clientID)
	}
}

func TestRequestIDMiddleware_RejectsInvalidID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		headerValue string
	}{
		{name: "non-UUID string", headerValue: "not-a-uuid"},
		{name: "empty string", headerValue: ""},
		{name: "partial UUID", headerValue: "019726de-f1c4-7a5d"},
		{name: "UUID with extra chars", headerValue: "019726de-f1c4-7a5d-8e2b-000000000003-extra"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := newMiddlewareApp()

			req := httptest.NewRequest("GET", "/test", nil)
			if tc.headerValue != "" {
				req.Header.Set("X-Request-Id", tc.headerValue)
			}
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}

			got := resp.Header.Get("X-Request-Id")
			if got == "" {
				t.Fatal("X-Request-Id response header is empty, want a generated UUIDv7")
			}

			// Must be a valid UUID, different from the invalid input.
			if _, err := uuid.Parse(got); err != nil {
				t.Errorf("generated X-Request-Id %q is not a valid UUID: %v", got, err)
			}

			if got == tc.headerValue {
				t.Errorf("X-Request-Id = %q, want a newly generated value (not the invalid input)", got)
			}
		})
	}
}

func TestRequestIDMiddleware_SetsResponseHeader(t *testing.T) {
	t.Parallel()

	app := newMiddlewareApp()

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}

	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("X-Request-Id response header is not set")
	}
}

func TestRequestIDFromCtx_NoMiddleware(t *testing.T) {
	t.Parallel()

	var capturedID string

	// App without RequestIDMiddleware — RequestIDFromCtx must return "".
	app := fiber.New()
	app.Get("/test", func(c fiber.Ctx) error {
		capturedID = apierror.RequestIDFromCtx(c)
		return c.SendStatus(fiber.StatusOK)
	})

	req := httptest.NewRequest("GET", "/test", nil)
	if _, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second}); err != nil {
		t.Fatalf("app.Test: %v", err)
	}

	if capturedID != "" {
		t.Errorf("RequestIDFromCtx without middleware = %q, want empty string", capturedID)
	}
}
