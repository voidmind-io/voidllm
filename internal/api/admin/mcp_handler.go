package admin

import (
	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// HandleMCP processes MCP JSON-RPC 2.0 requests over HTTP POST /api/v1/mcp/voidllm.
// It injects the authenticated caller identity into the Go context before
// dispatching to the MCP server so that tool handlers can scope queries to
// the caller's organization without importing Fiber or the auth package.
func (h *Handler) HandleMCP(c fiber.Ctx) error {
	body := append([]byte{}, c.Body()...)
	if len(body) == 0 {
		resp := mcp.NewErrorResponse(nil, mcp.CodeParseError, "empty request body")
		return c.JSON(resp)
	}

	// Inject caller identity into the Go context so MCP tool handlers can
	// access it without depending on fiber.Ctx (which is not safe to capture
	// in goroutines and is recycled after the handler returns).
	ki := auth.KeyInfoFromCtx(c)
	ctx := c.Context()
	if ki != nil {
		ctx = mcp.WithKeyIdentity(ctx, mcp.KeyIdentity{
			OrgID:  ki.OrgID,
			KeyID:  ki.ID,
			UserID: ki.UserID,
			Role:   ki.Role,
		})
	}

	result := h.MCPServer.Handle(ctx, body)

	// Notifications have no ID and return nil per the MCP spec — respond with
	// 202 Accepted so the client knows the notification was received.
	if result == nil {
		return c.SendStatus(fiber.StatusAccepted)
	}

	c.Set("Content-Type", "application/json")
	return c.Send(result)
}
