package admin

import (
	"context"
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

// mcpSessions stores the most-recently-seen Mcp-Session-Id per (orgID, alias)
// pair. Keys are "alias:orgID" strings; values are session ID strings.
// Concurrent access is safe via sync.Map.
var mcpSessions sync.Map // map[string]string — "alias:orgID" → Mcp-Session-Id

// mcpReInitMu serialises session re-initialisation. Re-init only happens on
// ErrSessionExpired which is rare; a global mutex avoids a per-key lock map
// while still preventing concurrent duplicate re-inits for the same server.
var mcpReInitMu sync.Mutex

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

	toolName := extractToolNameForLog(body)
	method := extractMethodForMetrics(body)

	// Session key is scoped per (alias, orgID) to prevent cross-org session
	// confusion when the same alias is registered under different organisations.
	sessionKey := alias + ":" + ki.OrgID

	// Load any existing session for this server alias + org.
	var sessionID string
	if sid, ok := mcpSessions.Load(sessionKey); ok {
		sessionID = sid.(string)
	}

	isInit := isInitializeRequest(body)

	start := time.Now()
	result, newSID, callErr := transport.Call(c.Context(), body, sessionID)

	// If the upstream reports session expired, delete the stale session and
	// re-initialize once before retrying the original request.
	// mcpReInitMu prevents concurrent goroutines from racing to re-initialize
	// the same (or any) session simultaneously; re-init is rare so a global
	// lock is sufficient.
	if errors.Is(callErr, mcp.ErrSessionExpired) && !isInit {
		mcpReInitMu.Lock()
		// Double-check: if another goroutine already refreshed the session,
		// the stored session will differ from the stale one we used — retry
		// with the new one. If it is still the same stale session, re-init.
		if sid, ok := mcpSessions.Load(sessionKey); ok && sid.(string) != sessionID {
			mcpReInitMu.Unlock()
			result, newSID, callErr = transport.Call(c.Context(), body, sid.(string))
		} else {
			mcpSessions.Delete(sessionKey)
			initBody := buildInitializeRequest()
			_, initSID, initErr := transport.Call(c.Context(), initBody, "")
			if initErr != nil {
				mcpReInitMu.Unlock()
				h.Log.ErrorContext(c.Context(), "mcp proxy: re-initialize after session expiry",
					slog.String("server", alias),
					slog.String("error", initErr.Error()))
				return c.Status(fiber.StatusBadGateway).JSON(
					mcp.NewErrorResponse(nil, mcp.CodeInternalError, "upstream MCP server unavailable"))
			}
			if initSID != "" {
				mcpSessions.Store(sessionKey, initSID)
				notifyBody := buildInitializedNotification()
				// Fire-and-forget: ignore errors — the notification is advisory.
				transport.Call(c.Context(), notifyBody, initSID) //nolint:errcheck
			}
			mcpReInitMu.Unlock()
			// Retry the original request with the freshly established session.
			result, newSID, callErr = transport.Call(c.Context(), body, initSID)
		}
	}

	if newSID != "" {
		mcpSessions.Store(sessionKey, newSID)
	}

	duration := time.Since(start)

	status := "success"
	if callErr != nil {
		status = "transport_error"
		metrics.MCPTransportErrorsTotal.WithLabelValues(alias, "call").Inc()
	}
	metrics.MCPToolCallsTotal.WithLabelValues(alias, method, status).Inc()
	if method != "" {
		metrics.MCPToolCallDurationSeconds.WithLabelValues(alias, method).Observe(duration.Seconds())
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

// extractToolNameForLog parses the MCP tool name from a JSON-RPC request body
// for use in the async usage logger (DB). For tools/call it returns the tool
// name (truncated to 64 bytes). For other known MCP methods it returns the
// method name. Unknown methods and unparseable bodies return "unknown".
// This function may return user-controlled tool names and must NOT be used as
// a Prometheus label value.
func extractToolNameForLog(body []byte) string {
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

// extractMethodForMetrics returns a Prometheus-safe label value from an MCP
// JSON-RPC request body. Only values from the bounded validMCPMethods set are
// returned as-is. For tools/call the literal string "tools/call" is returned
// regardless of the tool name, preventing cardinality explosion from arbitrary
// user-controlled tool names in Prometheus metrics. Unparseable bodies and
// unknown methods return "unknown".
func extractMethodForMetrics(body []byte) string {
	var req struct {
		Method string `json:"method"`
	}
	if json.Unmarshal(body, &req) != nil {
		return "unknown"
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

// buildToolCallRequest serialises a JSON-RPC tools/call request body for the
// given tool name and argument object. The error path of json.Marshal is
// unreachable for the static structure used here; the result is always valid.
func buildToolCallRequest(toolName string, args json.RawMessage) []byte {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	}
	b, _ := json.Marshal(req)
	return b
}

// CallMCPTool executes a single MCP tool call against an upstream server on
// behalf of the given caller identity. It performs the same server lookup,
// access control, credential decryption, session management, metrics recording,
// and usage logging as HandleMCPProxy. codeMode should be true when the call
// originates from a Code Mode execution. executionID is the UUIDv7 that groups
// all tool calls from a single execute_code invocation; pass an empty string
// for non-Code-Mode calls.
func (h *Handler) CallMCPTool(ctx context.Context, ki *auth.KeyInfo, serverAlias, toolName string, args json.RawMessage, codeMode bool, executionID string) (json.RawMessage, error) {
	// Built-in VoidLLM management server — dispatch in-process instead of HTTP.
	if serverAlias == "voidllm" && h.MCPServer != nil {
		return h.callBuiltinTool(ctx, ki, toolName, args)
	}

	server, err := h.DB.GetMCPServerByAliasScoped(ctx, serverAlias, ki.OrgID, ki.TeamID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return nil, fmt.Errorf("CallMCPTool %s: unknown MCP server", serverAlias)
		}
		return nil, fmt.Errorf("CallMCPTool %s: lookup: %w", serverAlias, err)
	}

	if !server.IsActive {
		return nil, fmt.Errorf("CallMCPTool %s: server is disabled", serverAlias)
	}

	// Global servers require explicit access control via the access tables.
	if server.OrgID == nil && server.TeamID == nil {
		allowed, accessErr := h.DB.CheckMCPAccess(ctx, ki.OrgID, ki.TeamID, ki.ID, server.ID)
		if accessErr != nil {
			return nil, fmt.Errorf("CallMCPTool %s: check access: %w", serverAlias, accessErr)
		}
		if !allowed {
			return nil, fmt.Errorf("CallMCPTool %s: access denied", serverAlias)
		}
	}

	var authToken string
	if server.AuthTokenEnc != nil && *server.AuthTokenEnc != "" {
		decrypted, decErr := crypto.DecryptString(*server.AuthTokenEnc, h.EncryptionKey, mcpServerAAD(server.ID))
		if decErr != nil {
			return nil, fmt.Errorf("CallMCPTool %s: decrypt auth token: %w", serverAlias, decErr)
		}
		authToken = decrypted
	}

	timeout := h.MCPCallTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	transport := mcp.NewHTTPTransport(server.URL, server.AuthType, server.AuthHeader, authToken, timeout, h.MCPAllowPrivateURLs)
	defer transport.Close() //nolint:errcheck

	body := buildToolCallRequest(toolName, args)

	sessionKey := serverAlias + ":" + ki.OrgID
	var sessionID string
	if sid, ok := mcpSessions.Load(sessionKey); ok {
		sessionID = sid.(string)
	}

	start := time.Now()
	result, newSID, callErr := transport.Call(ctx, body, sessionID)

	if errors.Is(callErr, mcp.ErrSessionExpired) {
		mcpReInitMu.Lock()
		// Compare the loaded session with the one we originally used. If it
		// changed, another goroutine already refreshed it — use the new one.
		// If it is the same stale session, delete it and re-initialize.
		if sid, ok := mcpSessions.Load(sessionKey); ok && sid.(string) != sessionID {
			mcpReInitMu.Unlock()
			result, newSID, callErr = transport.Call(ctx, body, sid.(string))
		} else {
			mcpSessions.Delete(sessionKey)
			initBody := buildInitializeRequest()
			_, initSID, initErr := transport.Call(ctx, initBody, "")
			if initErr != nil {
				mcpReInitMu.Unlock()
				return nil, fmt.Errorf("CallMCPTool %s: re-initialize: %w", serverAlias, initErr)
			}
			if initSID != "" {
				mcpSessions.Store(sessionKey, initSID)
				notifyBody := buildInitializedNotification()
				transport.Call(ctx, notifyBody, initSID) //nolint:errcheck
			}
			mcpReInitMu.Unlock()
			result, newSID, callErr = transport.Call(ctx, body, initSID)
		}
	}

	if newSID != "" {
		mcpSessions.Store(sessionKey, newSID)
	}

	duration := time.Since(start)

	status := "success"
	if callErr != nil {
		status = "transport_error"
		metrics.MCPTransportErrorsTotal.WithLabelValues(serverAlias, "call").Inc()
	}
	metrics.MCPToolCallsTotal.WithLabelValues(serverAlias, "tools/call", status).Inc()
	metrics.MCPToolCallDurationSeconds.WithLabelValues(serverAlias, "tools/call").Observe(duration.Seconds())

	if h.MCPLogger != nil {
		h.MCPLogger.Log(usage.MCPToolCallEvent{
			KeyID:               ki.ID,
			KeyType:             ki.KeyType,
			OrgID:               ki.OrgID,
			TeamID:              ki.TeamID,
			UserID:              ki.UserID,
			ServiceAccountID:    ki.ServiceAccountID,
			ServerAlias:         serverAlias,
			ToolName:            toolName,
			DurationMS:          int(duration.Milliseconds()),
			Status:              status,
			CodeMode:            codeMode,
			CodeModeExecutionID: executionID,
		})
	}

	if callErr != nil {
		return nil, fmt.Errorf("CallMCPTool %s/%s: transport: %w", serverAlias, toolName, callErr)
	}

	return result, nil
}

// callBuiltinTool dispatches a tool call to the built-in VoidLLM management
// MCP server in-process, without HTTP. The caller's identity is injected into
// the MCP context so tool handlers can enforce RBAC.
func (h *Handler) callBuiltinTool(ctx context.Context, ki *auth.KeyInfo, toolName string, args json.RawMessage) (json.RawMessage, error) {
	mcpCtx := mcp.WithKeyIdentity(ctx, mcp.KeyIdentity{
		OrgID:  ki.OrgID,
		TeamID: ki.TeamID,
		KeyID:  ki.ID,
		UserID: ki.UserID,
		Role:   ki.Role,
	})
	body := buildToolCallRequest(toolName, args)
	result := h.MCPServer.Handle(mcpCtx, body)
	if result == nil {
		return nil, fmt.Errorf("builtin tool %s: no response", toolName)
	}
	return result, nil
}

// MakeToolFetcher returns a ToolFetcher that retrieves tool schemas from the
// upstream MCP server identified by alias. It creates a fresh HTTPTransport,
// sends initialize + tools/list, and parses the response. The lookup uses
// GetMCPServerByAliasAny so that org-scoped and team-scoped servers are
// resolved in addition to global servers. Access control is enforced
// separately at the call layer; the fetcher only reads URL and auth config.
func (h *Handler) MakeToolFetcher() mcp.ToolFetcher {
	return func(ctx context.Context, alias string) ([]mcp.Tool, error) {
		server, err := h.DB.GetMCPServerByAliasAny(ctx, alias)
		if err != nil {
			return nil, fmt.Errorf("tool fetcher %s: lookup: %w", alias, err)
		}

		var authToken string
		if server.AuthTokenEnc != nil && *server.AuthTokenEnc != "" {
			decrypted, decErr := crypto.DecryptString(*server.AuthTokenEnc, h.EncryptionKey, mcpServerAAD(server.ID))
			if decErr != nil {
				return nil, fmt.Errorf("tool fetcher %s: decrypt auth token: %w", alias, decErr)
			}
			authToken = decrypted
		}

		timeout := h.MCPCallTimeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		transport := mcp.NewHTTPTransport(server.URL, server.AuthType, server.AuthHeader, authToken, timeout, h.MCPAllowPrivateURLs)
		defer transport.Close() //nolint:errcheck

		tools, err := transport.ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("tool fetcher %s: list tools: %w", alias, err)
		}
		return tools, nil
	}
}
