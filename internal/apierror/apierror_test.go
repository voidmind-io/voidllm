package apierror_test

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
)

// newApp builds a minimal Fiber app that invokes fn on GET /test.
func newApp(fn fiber.Handler) *fiber.App {
	app := fiber.New()
	app.Get("/test", fn)
	return app
}

// decodeResponse reads the response body and unmarshals it into a map.
func decodeResponse(t *testing.T, body io.Reader) map[string]any {
	t.Helper()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal body: %v\nraw: %s", err, raw)
	}
	return payload
}

func TestSend(t *testing.T) {
	t.Parallel()

	app := newApp(func(c fiber.Ctx) error {
		return apierror.Send(c, fiber.StatusTeapot, "teapot", "I am a teapot")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}

	if resp.StatusCode != fiber.StatusTeapot {
		t.Errorf("status = %d, want %d", resp.StatusCode, fiber.StatusTeapot)
	}

	payload := decodeResponse(t, resp.Body)

	errObj, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("response missing \"error\" object, got: %v", payload)
	}

	if got, _ := errObj["code"].(string); got != "teapot" {
		t.Errorf("error.code = %q, want %q", got, "teapot")
	}

	if got, _ := errObj["message"].(string); got != "I am a teapot" {
		t.Errorf("error.message = %q, want %q", got, "I am a teapot")
	}
}

func TestSend_WithRequestID(t *testing.T) {
	t.Parallel()

	const clientID = "019726de-f1c4-7a5d-8e2b-000000000001"

	app := fiber.New()
	app.Use(apierror.RequestIDMiddleware())
	app.Get("/test", func(c fiber.Ctx) error {
		return apierror.Send(c, fiber.StatusBadRequest, "bad_request", "something wrong")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	req.Header.Set("X-Request-Id", clientID)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}

	payload := decodeResponse(t, resp.Body)

	errObj, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("response missing \"error\" object, got: %v", payload)
	}

	if got, _ := errObj["request_id"].(string); got != clientID {
		t.Errorf("error.request_id = %q, want %q", got, clientID)
	}
}

func TestSend_WithoutRequestID(t *testing.T) {
	t.Parallel()

	// No RequestIDMiddleware — request_id must be absent (omitempty).
	app := newApp(func(c fiber.Ctx) error {
		return apierror.Send(c, fiber.StatusBadRequest, "bad_request", "no id here")
	})

	req := httptest.NewRequest("GET", "/test", nil)
	resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}

	payload := decodeResponse(t, resp.Body)

	errObj, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("response missing \"error\" object, got: %v", payload)
	}

	if _, present := errObj["request_id"]; present {
		t.Errorf("error.request_id should be omitted when no middleware is active, got %v", errObj["request_id"])
	}
}

func TestHelpers(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		handler    fiber.Handler
		wantStatus int
		wantCode   string
	}{
		{
			name: "NotFound returns 404 not_found",
			handler: func(c fiber.Ctx) error {
				return apierror.NotFound(c, "resource not found")
			},
			wantStatus: fiber.StatusNotFound,
			wantCode:   "not_found",
		},
		{
			name: "BadRequest returns 400 bad_request",
			handler: func(c fiber.Ctx) error {
				return apierror.BadRequest(c, "invalid input")
			},
			wantStatus: fiber.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name: "Conflict returns 409 conflict",
			handler: func(c fiber.Ctx) error {
				return apierror.Conflict(c, "already exists")
			},
			wantStatus: fiber.StatusConflict,
			wantCode:   "conflict",
		},
		{
			name: "InternalError returns 500 internal_error",
			handler: func(c fiber.Ctx) error {
				return apierror.InternalError(c, "unexpected failure")
			},
			wantStatus: fiber.StatusInternalServerError,
			wantCode:   "internal_error",
		},
		{
			name: "Unauthorized returns 401 unauthorized",
			handler: func(c fiber.Ctx) error {
				return apierror.Unauthorized(c, "missing credentials")
			},
			wantStatus: fiber.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name: "Forbidden returns 403 forbidden",
			handler: func(c fiber.Ctx) error {
				return apierror.Forbidden(c, "insufficient role")
			},
			wantStatus: fiber.StatusForbidden,
			wantCode:   "forbidden",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			app := newApp(tc.handler)

			req := httptest.NewRequest("GET", "/test", nil)
			resp, err := app.Test(req, fiber.TestConfig{Timeout: 5 * time.Second})
			if err != nil {
				t.Fatalf("app.Test: %v", err)
			}

			if resp.StatusCode != tc.wantStatus {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}

			payload := decodeResponse(t, resp.Body)

			errObj, ok := payload["error"].(map[string]any)
			if !ok {
				t.Fatalf("response missing \"error\" object, got: %v", payload)
			}

			if got, _ := errObj["code"].(string); got != tc.wantCode {
				t.Errorf("error.code = %q, want %q", got, tc.wantCode)
			}

			if msg, _ := errObj["message"].(string); msg == "" {
				t.Error("error.message is empty, want a non-empty string")
			}
		})
	}
}
