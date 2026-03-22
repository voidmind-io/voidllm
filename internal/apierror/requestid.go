package apierror

import (
	"context"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
)

type ctxKey int

const requestIDKey ctxKey = iota

// goCtxKey is an unexported struct type used as the key for storing a request
// ID in a standard Go context.Context. Using a named struct prevents key
// collisions with other packages.
type goCtxKey struct{}

// RequestIDMiddleware generates a UUIDv7 request ID for every request and
// stores it in the Fiber context. If the client sends a valid UUID in the
// X-Request-Id header, that value is used instead.
func RequestIDMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		id := c.Get("X-Request-Id")
		if _, err := uuid.Parse(id); err != nil {
			v7, err := uuid.NewV7()
			if err != nil {
				v7 = uuid.New()
			}
			id = v7.String()
		}
		c.Locals(requestIDKey, id)
		c.Set("X-Request-Id", id)
		c.SetContext(context.WithValue(c.Context(), goCtxKey{}, id))
		return c.Next()
	}
}

// RequestIDFromCtx retrieves the request ID from the Fiber context.
// Returns an empty string if no request ID was set.
func RequestIDFromCtx(c fiber.Ctx) string {
	if id, ok := c.Locals(requestIDKey).(string); ok {
		return id
	}
	return ""
}

// RequestIDFromGoCtx retrieves the request ID from a standard Go context.
// This is used by the slog handler wrapper to automatically include the
// request ID in log records. Returns an empty string if not present.
func RequestIDFromGoCtx(ctx context.Context) string {
	if id, ok := ctx.Value(goCtxKey{}).(string); ok {
		return id
	}
	return ""
}
