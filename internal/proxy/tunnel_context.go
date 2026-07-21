package proxy

import (
	"time"

	"github.com/gofiber/fiber/v3"
)

// tunnelContextKey is a package-private type for Fiber locals keys, preventing
// collisions with other middleware that might use the same string.
type tunnelContextKey int

const (
	// tunnelStreamCapKey is the Fiber locals key for the tunnel-specific
	// maximum streaming duration set by SetTunnelStreamCap.
	tunnelStreamCapKey tunnelContextKey = iota
)

// SetTunnelStreamCap marks c as originating from an in-process request tunnel
// (e.g. the admin app's playground tunnel) and records the maximum streaming
// duration callers of ProxyHandler.Handle are willing to tolerate for it. It
// must be called before Handle.
//
// This exists because in dual-port mode the admin app's WriteTimeout is a
// single, absolute fasthttp socket write deadline (see maxAdminTunnelTimeout
// in internal/app/routes.go) that is never refreshed by successful SSE
// flushes. If a tunneled stream is still producing tokens when that deadline
// arrives, the client sees a dead connection instead of the proxy's own
// deliberate stream-timeout handling (clean upstream cancellation and a
// terminal abort event). Capping the stream budget here, strictly below that
// socket deadline, ensures the proxy's own timer always fires first.
//
// Handle takes the minimum of d and its own configured/effective stream
// duration for the request — it can only shorten the budget, never extend
// it. Requests that never call SetTunnelStreamCap are completely unaffected.
func SetTunnelStreamCap(c fiber.Ctx, d time.Duration) {
	c.Locals(tunnelStreamCapKey, d)
}

// tunnelStreamCapFromCtx retrieves the tunnel stream-duration cap set by
// SetTunnelStreamCap, if any. ok is false for every request that did not go
// through a tunnel (the overwhelming majority of hot-path traffic), in which
// case d must be ignored.
func tunnelStreamCapFromCtx(c fiber.Ctx) (d time.Duration, ok bool) {
	d, ok = c.Locals(tunnelStreamCapKey).(time.Duration)
	return d, ok
}
