package admin

import (
	"github.com/gofiber/fiber/v3"
)

// GetUpdateStatus returns the current version and any available update
// information. The result is read from the settings table cache populated by
// the background update checker, so this handler never makes a network call.
// When no update checker is wired (e.g. dev builds), a minimal response with
// needs_update: false is returned.
func (h *Handler) GetUpdateStatus(c fiber.Ctx) error {
	if h.UpdateChecker == nil {
		return c.JSON(map[string]any{
			"current_version": "dev",
			"needs_update":    false,
		})
	}
	return c.JSON(h.UpdateChecker.GetInfo(c.Context()))
}
