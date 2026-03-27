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
// It injects the authenticated caller identity into the Go context before
// dispatching to the MCP server so that tool handlers can scope queries to
// the caller's organization without importing Fiber or the auth package.
//
// When the request carries Accept: text/event-stream, the JSON-RPC response is
// wrapped as a Server-Sent Events message per the MCP Streamable HTTP spec.
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

	// When the client advertises SSE support, wrap the JSON-RPC response in a
	// single SSE message event per the MCP Streamable HTTP transport spec.
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
// It sends an initial endpoint event that tells legacy SSE-only MCP clients
// which URL to POST requests to, then keeps the connection alive with periodic
// comment pings until the client disconnects.
func (h *Handler) HandleMCPSSE(c fiber.Ctx) error {
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("X-Accel-Buffering", "no")

	// Capture the endpoint path before entering the stream writer — the fiber
	// context is recycled by fasthttp after the handler returns and must not be
	// accessed inside the SendStreamWriter closure.
	endpointEvent := "event: endpoint\ndata: /api/v1/mcp/voidllm\n\n"

	return c.SendStreamWriter(func(w *bufio.Writer) {
		if _, err := w.WriteString(endpointEvent); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}

		// Hard deadline prevents DoS via connection exhaustion.
		// MCP clients are expected to reconnect after the stream closes.
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
