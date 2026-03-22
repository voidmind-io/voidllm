// Package health provides HTTP handlers for liveness, readiness, and metrics endpoints.
package health

import (
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/db"
)

// Version is set at build time via -ldflags "-X 'github.com/voidmind-io/voidllm/internal/api/health.Version=...'".
var Version = "dev"

// ShutdownChecker reports whether the server is draining connections.
// Implementations must be safe for concurrent use.
type ShutdownChecker interface {
	Draining() bool
}

// Liveness returns a Fiber handler that reports the process is alive.
// It captures the server start time at construction and includes it in each
// response so operators can detect unexpected restarts.
func Liveness() fiber.Handler {
	startTime := time.Now()
	return func(c fiber.Ctx) error {
		return c.JSON(fiber.Map{
			"status":         "ok",
			"version":        Version,
			"uptime_seconds": int64(time.Since(startTime).Seconds()),
		})
	}
}

// Readiness returns a Fiber handler that reports whether the server is ready to
// serve traffic. It pings the database on every call; a failed ping causes a 503.
// If checker is non-nil and Draining returns true, the handler immediately returns
// 503 with {"status":"draining"} so that load balancers stop routing new requests
// to this instance during a graceful shutdown.
func Readiness(database *db.DB, checker ShutdownChecker) fiber.Handler {
	return func(c fiber.Ctx) error {
		if checker != nil && checker.Draining() {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"status": "draining",
			})
		}
		if err := database.Ping(c.Context()); err != nil {
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				"status":   "not ready",
				"database": "error",
			})
		}
		return c.JSON(fiber.Map{
			"status":   "ok",
			"database": "ok",
		})
	}
}
