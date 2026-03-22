package auth

import "github.com/gofiber/fiber/v3"

// contextKey is a package-private type for Fiber locals keys, preventing
// collisions with other middleware that might use the same string.
type contextKey int

const (
	// keyInfoKey is the Fiber locals key for the authenticated KeyInfo.
	keyInfoKey contextKey = iota
)

// KeyInfoFromCtx retrieves the authenticated KeyInfo from the Fiber context.
// Returns nil if no authentication was performed.
func KeyInfoFromCtx(c fiber.Ctx) *KeyInfo {
	if v, ok := c.Locals(keyInfoKey).(*KeyInfo); ok {
		return v
	}
	return nil
}
