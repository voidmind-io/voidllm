package admin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/jsonx"
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

	var server *db.MCPServer
	if h.MCPServerCache != nil {
		server, _ = h.MCPServerCache.Get(alias, ki.OrgID, ki.TeamID)
	}
	if server == nil {
		var err error
		server, err = h.DB.GetMCPServerByAliasScoped(c.Context(), alias, ki.OrgID, ki.TeamID)
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
		var allowed bool
		if h.MCPAccessCache != nil {
			allowed = h.MCPAccessCache.Check(ki.OrgID, ki.TeamID, ki.ID, server.ID)
		} else {
			var accessErr error
			allowed, accessErr = h.DB.CheckMCPAccess(c.Context(), ki.OrgID, ki.TeamID, ki.ID, server.ID)
			if accessErr != nil {
				h.Log.ErrorContext(c.Context(), "mcp proxy: check access",
					slog.String("error", accessErr.Error()))
				return c.Status(fiber.StatusInternalServerError).JSON(
					mcp.NewErrorResponse(nil, mcp.CodeInternalError, "internal error"))
			}
		}
		if !allowed {
			return c.Status(fiber.StatusForbidden).JSON(
				mcp.NewErrorResponse(nil, mcp.CodeInvalidRequest, "access denied to MCP server"))
		}
	}

	// Resolve transport from cache (avoids per-request transport creation and
	// AES-256-GCM decryption). Fall back to an ad-hoc transport on cache miss
	// (cold start or server not yet loaded into the cache).
	var transport *mcp.HTTPTransport
	var adHoc bool
	if h.MCPTransportCache != nil {
		if resolved, ok := h.MCPTransportCache.Get(server.ID); ok {
			transport = resolved.Transport
		}
	}
	if transport == nil {
		adHoc = true
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
		transport = mcp.NewHTTPTransport(server.URL, server.AuthType, server.AuthHeader, authToken, timeout, h.MCPAllowPrivateURLs)
	}
	if adHoc {
		defer transport.Close() //nolint:errcheck
	}

	body := append([]byte{}, c.Body()...)
	if len(body) == 0 {
		return c.JSON(mcp.NewErrorResponse(nil, mcp.CodeParseError, "empty request body"))
	}

	meta := parseMCPRequestMeta(body)

	// Session key is scoped per (alias, orgID) to prevent cross-org session
	// confusion when the same alias is registered under different organisations.
	sessionKey := alias + ":" + ki.OrgID

	// Load any existing session for this server alias + org.
	var sessionID string
	if sid, ok := mcpSessions.Load(sessionKey); ok {
		sessionID = sid.(string)
	}

	start := time.Now()
	result, newSID, callErr := transport.Call(c.Context(), body, sessionID)

	// If the upstream reports session expired, delete the stale session and
	// re-initialize once before retrying the original request.
	// mcpReInitMu prevents concurrent goroutines from racing to re-initialize
	// the same (or any) session simultaneously; re-init is rare so a global
	// lock is sufficient.
	if errors.Is(callErr, mcp.ErrSessionExpired) && !meta.IsInit {
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
	metricsMethod := meta.MetricsMethod()
	metrics.MCPToolCallsTotal.WithLabelValues(alias, metricsMethod, status).Inc()
	if metricsMethod != "" {
		metrics.MCPToolCallDurationSeconds.WithLabelValues(alias, metricsMethod).Observe(duration.Seconds())
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
			ToolName:         meta.ToolName,
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

// mcpRequestMeta holds the parsed metadata from a single MCP JSON-RPC request
// body. It is produced once per request by parseMCPRequestMeta and consumed by
// logging, metrics, and session logic, avoiding three separate body parses.
type mcpRequestMeta struct {
	// Method is the raw JSON-RPC method string (e.g. "tools/call").
	Method string
	// ToolName is a usage-log-safe identifier: the tools/call tool name
	// (truncated to 64 bytes) for tools/call requests, the method name for
	// other known MCP methods, or "unknown" otherwise. May contain
	// user-controlled data — must NOT be used as a Prometheus label.
	ToolName string
	// IsInit reports whether the request is an "initialize" call.
	IsInit bool
}

// MetricsMethod returns a Prometheus-safe label value derived from the parsed
// method. Only values present in validMCPMethods are returned as-is; everything
// else (including unknown methods from user input) maps to "unknown", preventing
// cardinality explosion in Prometheus metrics.
func (m mcpRequestMeta) MetricsMethod() string {
	if validMCPMethods[m.Method] {
		return m.Method
	}
	return "unknown"
}

// parseMCPRequestMeta parses the JSON-RPC method and params.name fields from
// body in a single pass and returns an mcpRequestMeta. It is the sole body-
// parse for all metadata needs in HandleMCPProxy.
func parseMCPRequestMeta(body []byte) mcpRequestMeta {
	var req struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if jsonx.Unmarshal(body, &req) != nil {
		return mcpRequestMeta{Method: "unknown", ToolName: "unknown"}
	}

	meta := mcpRequestMeta{
		Method: req.Method,
		IsInit: req.Method == "initialize",
	}

	if req.Method == "tools/call" && req.Params.Name != "" {
		name := req.Params.Name
		if len(name) > 64 {
			name = name[:64]
		}
		meta.ToolName = name
	} else if validMCPMethods[req.Method] {
		meta.ToolName = req.Method
	} else {
		meta.ToolName = "unknown"
	}

	return meta
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
	b, _ := jsonx.Marshal(req)
	return b
}

// buildInitializedNotification returns the notifications/initialized JSON-RPC
// notification that must be sent after a successful initialize handshake.
func buildInitializedNotification() []byte {
	req := map[string]any{
		"jsonrpc": "2.0",
		"method":  "notifications/initialized",
	}
	b, _ := jsonx.Marshal(req)
	return b
}

// mcpServerAAD returns the additional authenticated data used when
// encrypting and decrypting MCP server auth tokens. Binding the AAD to the
// server ID prevents a ciphertext from one server being replayed for another.
func mcpServerAAD(serverID string) []byte {
	return []byte("mcp_server:" + serverID)
}

// buildToolCallRequest serialises a JSON-RPC tools/call request body for the
// given tool name and argument object. The error path of jsonx.Marshal is
// unreachable for the static structure used here; the result is always valid.
func buildToolCallRequest(toolName string, args jsonx.RawMessage) []byte {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	}
	b, _ := jsonx.Marshal(req)
	return b
}

// CallMCPTool executes a single MCP tool call against an upstream server on
// behalf of the given caller identity. It performs the same server lookup,
// access control, credential decryption, session management, metrics recording,
// and usage logging as HandleMCPProxy. codeMode should be true when the call
// originates from a Code Mode execution. executionID is the UUIDv7 that groups
// all tool calls from a single execute_code invocation; pass an empty string
// for non-Code-Mode calls.
func (h *Handler) CallMCPTool(ctx context.Context, ki *auth.KeyInfo, serverAlias, toolName string, args jsonx.RawMessage, codeMode bool, executionID string) (jsonx.RawMessage, error) {
	// Built-in VoidLLM management server — dispatch in-process instead of HTTP.
	if serverAlias == "voidllm" && h.MCPServer != nil {
		return h.callBuiltinTool(ctx, ki, toolName, args)
	}

	var server *db.MCPServer
	if h.MCPServerCache != nil {
		server, _ = h.MCPServerCache.Get(serverAlias, ki.OrgID, ki.TeamID)
	}
	if server == nil {
		var err error
		server, err = h.DB.GetMCPServerByAliasScoped(ctx, serverAlias, ki.OrgID, ki.TeamID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return nil, fmt.Errorf("CallMCPTool %s: unknown MCP server", serverAlias)
			}
			return nil, fmt.Errorf("CallMCPTool %s: lookup: %w", serverAlias, err)
		}
	}

	if !server.IsActive {
		return nil, fmt.Errorf("CallMCPTool %s: server is disabled", serverAlias)
	}

	// Global servers require explicit access control via the access tables.
	if server.OrgID == nil && server.TeamID == nil {
		var allowed bool
		if h.MCPAccessCache != nil {
			allowed = h.MCPAccessCache.Check(ki.OrgID, ki.TeamID, ki.ID, server.ID)
		} else {
			var accessErr error
			allowed, accessErr = h.DB.CheckMCPAccess(ctx, ki.OrgID, ki.TeamID, ki.ID, server.ID)
			if accessErr != nil {
				return nil, fmt.Errorf("CallMCPTool %s: check access: %w", serverAlias, accessErr)
			}
		}
		if !allowed {
			return nil, fmt.Errorf("CallMCPTool %s: access denied", serverAlias)
		}
	}

	var transport *mcp.HTTPTransport
	var adHocTool bool
	if h.MCPTransportCache != nil {
		if resolved, ok := h.MCPTransportCache.Get(server.ID); ok {
			transport = resolved.Transport
		}
	}
	if transport == nil {
		adHocTool = true
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
		transport = mcp.NewHTTPTransport(server.URL, server.AuthType, server.AuthHeader, authToken, timeout, h.MCPAllowPrivateURLs)
	}
	if adHocTool {
		defer transport.Close() //nolint:errcheck
	}

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
func (h *Handler) callBuiltinTool(ctx context.Context, ki *auth.KeyInfo, toolName string, args jsonx.RawMessage) (jsonx.RawMessage, error) {
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
// upstream MCP server identified by serverID. It creates a fresh HTTPTransport,
// sends initialize + tools/list, and parses the response. The lookup uses
// GetMCPServer (by database ID) so that org-scoped, team-scoped, and global
// servers are all resolved without ambiguity. Access control is enforced
// separately at the call layer; the fetcher only reads URL and auth config.
func (h *Handler) MakeToolFetcher() mcp.ToolFetcher {
	return func(ctx context.Context, serverID string) ([]mcp.Tool, error) {
		server, err := h.DB.GetMCPServer(ctx, serverID)
		if err != nil {
			return nil, fmt.Errorf("tool fetcher %s: lookup: %w", serverID, err)
		}

		var transport *mcp.HTTPTransport
		var adHocFetch bool
		if h.MCPTransportCache != nil {
			if resolved, ok := h.MCPTransportCache.Get(server.ID); ok {
				transport = resolved.Transport
			}
		}
		if transport == nil {
			adHocFetch = true
			var authToken string
			if server.AuthTokenEnc != nil && *server.AuthTokenEnc != "" {
				decrypted, decErr := crypto.DecryptString(*server.AuthTokenEnc, h.EncryptionKey, mcpServerAAD(server.ID))
				if decErr != nil {
					return nil, fmt.Errorf("tool fetcher %s: decrypt auth token: %w", serverID, decErr)
				}
				authToken = decrypted
			}
			timeout := h.MCPCallTimeout
			if timeout == 0 {
				timeout = 30 * time.Second
			}
			transport = mcp.NewHTTPTransport(server.URL, server.AuthType, server.AuthHeader, authToken, timeout, h.MCPAllowPrivateURLs)
		}
		if adHocFetch {
			defer transport.Close() //nolint:errcheck
		}

		tools, err := transport.ListTools(ctx)
		if err != nil {
			return nil, fmt.Errorf("tool fetcher %s: list tools: %w", serverID, err)
		}
		return tools, nil
	}
}
