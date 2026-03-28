package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/internal/metrics"
	"github.com/voidmind-io/voidllm/internal/usage"
	"github.com/voidmind-io/voidllm/pkg/crypto"
)

// mcpSessions stores the most-recently-seen Mcp-Session-Id for each MCP server
// alias. Keys are server alias strings; values are session ID strings.
// Concurrent access is safe; no explicit locking is required.
var mcpSessions sync.Map // map[string]string — server alias → Mcp-Session-Id

// MCPToolCallLogger logs MCP tool call events asynchronously.
// Implementations must be safe for concurrent use. Log must never block.
// The concrete implementation is usage.MCPLogger.
type MCPToolCallLogger interface {
	Log(event usage.MCPToolCallEvent)
}

// HandleMCPProxy routes a POST MCP request to either the built-in VoidLLM MCP
// server (alias "voidllm") or an external registered MCP server identified by
// the :alias path parameter.
func (h *Handler) HandleMCPProxy(c fiber.Ctx) error {
	alias := c.Params("alias")

	if alias == "voidllm" {
		return h.HandleMCP(c)
	}

	ki := auth.KeyInfoFromCtx(c)
	if ki == nil {
		return c.Status(fiber.StatusUnauthorized).JSON(
			mcp.NewErrorResponse(nil, mcp.CodeInvalidRequest, "missing authentication"))
	}

	server, err := h.DB.GetMCPServerByAliasScoped(c.Context(), alias, ki.OrgID, ki.TeamID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return c.Status(fiber.StatusNotFound).JSON(
				mcp.NewErrorResponse(nil, mcp.CodeInvalidRequest, "unknown MCP server"))
		}
		h.Log.ErrorContext(c.Context(), "mcp proxy: lookup server",
			slog.String("alias", alias),
			slog.String("error", err.Error()))
		return c.Status(fiber.StatusInternalServerError).JSON(
			mcp.NewErrorResponse(nil, mcp.CodeInternalError, "internal error"))
	}

	if !server.IsActive {
		return c.Status(fiber.StatusServiceUnavailable).JSON(
			mcp.NewErrorResponse(nil, mcp.CodeInternalError, "MCP server is disabled"))
	}

	// Global servers (org_id IS NULL, team_id IS NULL) require explicit access
	// control via the org/team/key MCP access tables. Org- and team-scoped
	// servers are implicitly accessible to members of that scope — their
	// visibility is already enforced by GetMCPServerByAliasScoped.
	if server.OrgID == nil && server.TeamID == nil {
		allowed, accessErr := h.DB.CheckMCPAccess(c.Context(), ki.OrgID, ki.TeamID, ki.ID, server.ID)
		if accessErr != nil {
			h.Log.ErrorContext(c.Context(), "mcp proxy: check access",
				slog.String("error", accessErr.Error()))
			return c.Status(fiber.StatusInternalServerError).JSON(
				mcp.NewErrorResponse(nil, mcp.CodeInternalError, "internal error"))
		}
		if !allowed {
			return c.Status(fiber.StatusForbidden).JSON(
				mcp.NewErrorResponse(nil, mcp.CodeInvalidRequest, "access denied to MCP server"))
		}
	}

	var authToken string
	if server.AuthTokenEnc != nil && *server.AuthTokenEnc != "" {
		decrypted, decErr := crypto.DecryptString(*server.AuthTokenEnc, h.EncryptionKey, mcpServerAAD(server.ID))
		if decErr != nil {
			h.Log.ErrorContext(c.Context(), "mcp proxy: decrypt auth token",
				slog.String("server", alias),
				slog.String("error", decErr.Error()))
			return c.Status(fiber.StatusInternalServerError).JSON(
				mcp.NewErrorResponse(nil, mcp.CodeInternalError, "internal error"))
		}
		authToken = decrypted
	}

	timeout := h.MCPCallTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	transport := mcp.NewHTTPTransport(server.URL, server.AuthType, server.AuthHeader, authToken, timeout, h.MCPAllowPrivateURLs)
	defer transport.Close()

	body := append([]byte{}, c.Body()...)
	if len(body) == 0 {
		return c.JSON(mcp.NewErrorResponse(nil, mcp.CodeParseError, "empty request body"))
	}

	toolName := extractToolName(body)

	// Load any existing session for this server alias.
	var sessionID string
	if sid, ok := mcpSessions.Load(alias); ok {
		sessionID = sid.(string)
	}

	isInit := isInitializeRequest(body)

	start := time.Now()
	result, newSID, callErr := transport.Call(c.Context(), body, sessionID)

	// If the upstream reports session expired, delete the stale session and
	// re-initialize once before retrying the original request.
	if errors.Is(callErr, mcp.ErrSessionExpired) && !isInit {
		mcpSessions.Delete(alias)

		initBody := buildInitializeRequest()
		_, initSID, initErr := transport.Call(c.Context(), initBody, "")
		if initErr != nil {
			h.Log.ErrorContext(c.Context(), "mcp proxy: re-initialize after session expiry",
				slog.String("server", alias),
				slog.String("error", initErr.Error()))
			return c.Status(fiber.StatusBadGateway).JSON(
				mcp.NewErrorResponse(nil, mcp.CodeInternalError, "upstream MCP server unavailable"))
		}
		if initSID != "" {
			mcpSessions.Store(alias, initSID)
			notifyBody := buildInitializedNotification()
			// Fire-and-forget: ignore errors — the notification is advisory.
			transport.Call(c.Context(), notifyBody, initSID) //nolint:errcheck
		}
		// Retry the original request with the freshly established session.
		result, newSID, callErr = transport.Call(c.Context(), body, initSID)
	}

	if newSID != "" {
		mcpSessions.Store(alias, newSID)
	}

	duration := time.Since(start)

	status := "success"
	if callErr != nil {
		status = "transport_error"
		metrics.MCPTransportErrorsTotal.WithLabelValues(alias, "call").Inc()
	}
	metrics.MCPToolCallsTotal.WithLabelValues(alias, toolName, status).Inc()
	if toolName != "" {
		metrics.MCPToolCallDurationSeconds.WithLabelValues(alias, toolName).Observe(duration.Seconds())
	}

	if h.MCPLogger != nil {
		h.MCPLogger.Log(usage.MCPToolCallEvent{
			KeyID:            ki.ID,
			KeyType:          ki.KeyType,
			OrgID:            ki.OrgID,
			TeamID:           ki.TeamID,
			UserID:           ki.UserID,
			ServiceAccountID: ki.ServiceAccountID,
			ServerAlias:      alias,
			ToolName:         toolName,
			DurationMS:       int(duration.Milliseconds()),
			Status:           status,
		})
	}

	if callErr != nil {
		h.Log.ErrorContext(c.Context(), "mcp proxy: transport error",
			slog.String("server", alias),
			slog.String("error", callErr.Error()))
		return c.Status(fiber.StatusBadGateway).JSON(
			mcp.NewErrorResponse(nil, mcp.CodeInternalError, "upstream MCP server unavailable"))
	}

	// Notification — upstream returned 202 Accepted with no body.
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

// HandleMCPProxySSE handles GET requests for the MCP SSE transport.
// For the built-in "voidllm" alias it opens a persistent SSE stream.
// For external MCP servers, SSE streaming requires persistent connections
// that are not yet supported and returns 501 Not Implemented.
func (h *Handler) HandleMCPProxySSE(c fiber.Ctx) error {
	alias := c.Params("alias")

	if alias == "voidllm" {
		return h.HandleMCPSSE(c)
	}

	return c.Status(fiber.StatusNotImplemented).JSON(
		mcp.NewErrorResponse(nil, mcp.CodeInternalError,
			"SSE streaming is not supported for external MCP servers"))
}

// validMCPMethods is the set of standard MCP JSON-RPC method names. Only these
// values are allowed as Prometheus label values to prevent cardinality explosion
// from arbitrary user-controlled method strings.
var validMCPMethods = map[string]bool{
	"initialize":                true,
	"notifications/initialized": true,
	"ping":                      true,
	"tools/list":                true,
	"tools/call":                true,
	"resources/list":            true,
	"resources/read":            true,
	"prompts/list":              true,
	"prompts/get":               true,
}

// extractToolName parses the MCP tool name from a JSON-RPC tools/call request
// body. For tools/call, it returns the tool name (truncated to 64 bytes to
// prevent label bloat). For other known MCP methods it returns the method name.
// Unknown methods and unparseable bodies return "unknown".
func extractToolName(body []byte) string {
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "unknown"
	}
	if req.Method == "tools/call" && req.Params.Name != "" {
		name := req.Params.Name
		if len(name) > 64 {
			name = name[:64]
		}
		return name
	}
	if validMCPMethods[req.Method] {
		return req.Method
	}
	return "unknown"
}

// isInitializeRequest reports whether body is an MCP initialize JSON-RPC call.
func isInitializeRequest(body []byte) bool {
	var req struct {
		Method string `json:"method"`
	}
	json.Unmarshal(body, &req) //nolint:errcheck — best-effort parse
	return req.Method == "initialize"
}

// buildInitializeRequest returns a minimal MCP initialize JSON-RPC request
// that VoidLLM sends on behalf of the downstream client when re-establishing
// an expired session.
func buildInitializeRequest() []byte {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      0,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo": map[string]any{
				"name":    "voidllm",
				"version": "1.0",
			},
		},
	}
	b, _ := json.Marshal(req)
	return b
}

// buildInitializedNotification returns the notifications/initialized JSON-RPC
// notification that must be sent after a successful initialize handshake.
func buildInitializedNotification() []byte {
	req := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	b, _ := json.Marshal(req)
	return b
}

// mcpServerAAD returns the additional authenticated data used when
// encrypting and decrypting MCP server auth tokens. Binding the AAD to the
// server ID prevents a ciphertext from one server being replayed for another.
func mcpServerAAD(serverID string) []byte {
	return []byte("mcp_server:" + serverID)
}
