package admin

import (
	"bufio"
	"fmt"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// HandleMCP processes MCP JSON-RPC 2.0 requests over HTTP POST /api/v1/mcp/voidllm.
func (h *Handler) HandleMCP(c fiber.Ctx) error {
	return h.handleMCPRequest(c, h.MCPServer)
}

// HandleCodeModeMCP processes MCP JSON-RPC 2.0 requests over HTTP POST /api/v1/mcp.
func (h *Handler) HandleCodeModeMCP(c fiber.Ctx) error {
	return h.handleMCPRequest(c, h.CodeModeServer)
}

// handleMCPRequest is the shared implementation for MCP POST handlers. It
// injects the authenticated caller identity into the Go context before
// dispatching to the given MCP server so that tool handlers can scope queries
// to the caller's organization without importing Fiber or the auth package.
//
// When the request carries Accept: text/event-stream, the JSON-RPC response is
// wrapped as a Server-Sent Events message per the MCP Streamable HTTP spec.
func (h *Handler) handleMCPRequest(c fiber.Ctx, server *mcp.Server) error {
	body := append([]byte{}, c.Body()...)
	if len(body) == 0 {
		return c.JSON(mcp.NewErrorResponse(nil, mcp.CodeParseError, "empty request body"))
	}

	ki := auth.KeyInfoFromCtx(c)
	ctx := c.Context()
	if ki != nil {
		ctx = mcp.WithKeyIdentity(ctx, mcp.KeyIdentity{
			OrgID:  ki.OrgID,
			TeamID: ki.TeamID,
			KeyID:  ki.ID,
			UserID: ki.UserID,
			Role:   ki.Role,
		})
	}

	result := server.Handle(ctx, body)

	if result == nil {
		return c.SendStatus(fiber.StatusAccepted)
	}

	if acceptsSSE(c.Get("Accept")) {
		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("X-Accel-Buffering", "no")
		return c.SendString(fmt.Sprintf("event: message\ndata: %s\n\n", result))
	}

	c.Set("Content-Type", "application/json")
	return c.Send(result)
}

// HandleMCPSSE opens a Server-Sent Events stream on GET /api/v1/mcp/voidllm.
func (h *Handler) HandleMCPSSE(c fiber.Ctx) error {
	return h.handleMCPSSE(c, "/api/v1/mcp/voidllm")
}

// HandleCodeModeMCPSSE opens a Server-Sent Events stream on GET /api/v1/mcp.
func (h *Handler) HandleCodeModeMCPSSE(c fiber.Ctx) error {
	return h.handleMCPSSE(c, "/api/v1/mcp")
}

// handleMCPSSE is the shared implementation for MCP SSE GET handlers. It sends
// an initial endpoint event that tells legacy SSE-only MCP clients which URL to
// POST requests to, then keeps the connection alive with periodic comment pings
// until the client disconnects or a 10-minute deadline expires.
func (h *Handler) handleMCPSSE(c fiber.Ctx, endpoint string) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("X-Accel-Buffering", "no")

	endpointEvent := fmt.Sprintf("event: endpoint\ndata: %s\n\n", endpoint)

	return c.SendStreamWriter(func(w *bufio.Writer) {
		if _, err := w.WriteString(endpointEvent); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}

		deadline := time.After(10 * time.Minute)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				if _, err := w.WriteString(": ping\n\n"); err != nil {
					return
				}
				if err := w.Flush(); err != nil {
					return
				}
			case <-deadline:
				return
			}
		}
	})
}

// acceptsSSE checks whether the Accept header contains the text/event-stream
// media type. It correctly handles comma-separated values and quality parameters.
func acceptsSSE(accept string) bool {
	for _, part := range strings.Split(accept, ",") {
		mt := strings.TrimSpace(part)
		if idx := strings.IndexByte(mt, ';'); idx >= 0 {
			mt = strings.TrimSpace(mt[:idx])
		}
		if mt == "text/event-stream" {
			return true
		}
	}
	return false
}
