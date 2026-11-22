package apierror_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
)

func TestIsAnthropicPath(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"/v1/messages":              true,
		"/v1/messages/":             true,
		"/V1/Messages":              true,
		"/v1/messages/count_tokens": true,
		"/v1/chat/completions":      false,
		"/v1/responses":             false,
		"/v1/messages/batches":      false,
		"/api/v1/messages":          false,
	}
	for p, want := range tests {
		if got := apierror.IsAnthropicPath(p); got != want {
			t.Errorf("IsAnthropicPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestAnthropicErrorType(t *testing.T) {
	t.Parallel()
	tests := map[int]string{
		http.StatusBadRequest:            "invalid_request_error",
		http.StatusUnauthorized:          "authentication_error",
		http.StatusForbidden:             "permission_error",
		http.StatusNotFound:              "not_found_error",
		http.StatusRequestEntityTooLarge: "request_too_large",
		http.StatusUnprocessableEntity:   "invalid_request_error",
		http.StatusTooManyRequests:       "rate_limit_error",
		http.StatusInternalServerError:   "api_error",
		http.StatusBadGateway:            "api_error",
		http.StatusServiceUnavailable:    "overloaded_error",
		529:                              "overloaded_error",
	}
	for status, want := range tests {
		if got := apierror.AnthropicErrorType(status); got != want {
			t.Errorf("AnthropicErrorType(%d) = %q, want %q", status, got, want)
		}
	}
}

func TestSendProxy(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.All("/v1/*", func(c fiber.Ctx) error {
		return apierror.SendProxy(c, fiber.StatusTooManyRequests, "rate_limit_exceeded", "rate limit exceeded")
	})

	get := func(path string) string {
		resp, err := app.Test(httptest.NewRequest(http.MethodPost, path, nil), fiber.TestConfig{Timeout: 5e9})
		if err != nil {
			t.Fatalf("app.Test: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests {
			t.Errorf("%s: status = %d", path, resp.StatusCode)
		}
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	if got, want := get("/v1/messages"), `{"type":"error","error":{"type":"rate_limit_error","message":"rate limit exceeded"}}`; got != want {
		t.Errorf("messages envelope = %s, want %s", got, want)
	}
	if got, want := get("/v1/responses"), `{"error":{"code":"rate_limit_exceeded","message":"rate limit exceeded"}}`; got != want {
		t.Errorf("responses envelope = %s, want %s", got, want)
	}
}
