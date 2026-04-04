// Package admin provides HTTP handlers for the VoidLLM Admin API.
package admin

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/google/uuid"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/audit"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/health"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/internal/proxy"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
	"github.com/voidmind-io/voidllm/internal/sso"
)

// ModelHealthProvider provides upstream model health status for the admin API.
// It is implemented by *health.Checker and may be nil when health monitoring
// is not enabled.
type ModelHealthProvider interface {
	GetAllHealth() []health.ModelHealth
}

// Handler holds shared dependencies for all admin API handlers.
type Handler struct {
	DB                *db.DB
	HMACSecret        []byte
	EncryptionKey     []byte // AES-256-GCM key for upstream API key encryption
	KeyCache          *cache.Cache[string, auth.KeyInfo]
	Registry          *proxy.Registry
	AccessCache       *proxy.ModelAccessCache  // in-memory model access cache; nil disables refresh
	AliasCache        *proxy.AliasCache        // in-memory model alias cache; nil disables refresh
	MCPServerCache    *proxy.MCPServerCache    // in-memory MCP server cache; nil falls back to DB
	MCPAccessCache    *proxy.MCPAccessCache    // in-memory MCP access cache; nil falls back to DB
	MCPTransportCache *proxy.MCPTransportCache // persistent transport + decrypted token cache; nil disables
	Redis             *voidredis.Client        // nil when Redis is not configured
	AuditLogger       *audit.Logger            // nil when audit logging is disabled
	License           *license.Holder          // thread-safe license holder; Load() never returns nil
	Log               *slog.Logger
	// SSOProvider is the OIDC provider used for SSO login. Nil when SSO is
	// disabled or unlicensed.
	SSOProvider *sso.Provider
	// SSOConfig holds the SSO configuration passed from the application config.
	SSOConfig config.SSOConfig
	// HealthChecker provides upstream model health status. Nil when health
	// monitoring is not enabled.
	HealthChecker ModelHealthProvider
	// MCPHealthChecker provides MCP server health status. Nil when MCP health
	// monitoring is not enabled.
	MCPHealthChecker *health.MCPHealthChecker
	// MCPServer is the management MCP server (list_models, get_usage, etc.).
	// Nil when MCP is not configured — the route is only registered when non-nil.
	MCPServer *mcp.Server
	// CodeModeServer is the Code Mode MCP server (list_servers, search_tools,
	// execute_code). Nil when Code Mode is disabled — the /api/v1/mcp route is
	// only registered when non-nil.
	CodeModeServer *mcp.Server
	// MCPCallTimeout is the maximum duration for a single proxied MCP tool call
	// to an external server. Zero falls back to a 30-second default.
	MCPCallTimeout time.Duration
	// MCPLogger receives asynchronous usage events for proxied MCP tool calls.
	// Nil disables usage logging for MCP proxy calls.
	MCPLogger MCPToolCallLogger
	// MCPAllowPrivateURLs disables SSRF protection for MCP server URLs.
	// Set via YAML config only — not exposed in Admin API.
	MCPAllowPrivateURLs bool
	// ToolCache holds cached tool schemas from upstream MCP servers for use by
	// Code Mode. Nil when Code Mode is disabled.
	ToolCache *mcp.ToolCache
	// CodeExecutor runs Code Mode JavaScript in sandboxed QJS runtimes.
	// Nil when Code Mode is disabled.
	CodeExecutor *mcp.Executor
	// CodePool is the QJS runtime pool backing CodeExecutor. Held here so that
	// app.cleanup can drain and close the pool on graceful shutdown.
	// Nil when Code Mode is disabled.
	CodePool *mcp.RuntimePool
}

// swaggerErrorResponse is the standard API error envelope used in OpenAPI docs.
// It is an alias for apierror.SwaggerResponse kept here for Swagger annotation compatibility.
// The alias is referenced only in swagger @Failure comments (invisible to staticcheck).
//
//lint:ignore U1000 referenced in swagger @Failure annotations which staticcheck cannot see
type swaggerErrorResponse = apierror.SwaggerResponse

// paginationParams holds the parsed cursor and limit for paginated list endpoints.
type paginationParams struct {
	Limit  int
	Cursor string
}

// parsePagination extracts and clamps pagination query parameters from the request.
// limit defaults to 20 and is clamped to [1, 100].
// cursor is a raw UUIDv7 string used as a keyset pagination lower bound.
// An error is returned if cursor is non-empty but not a valid UUID.
func parsePagination(c fiber.Ctx) (paginationParams, error) {
	limit := fiber.Query[int](c, "limit", 20)
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	cursor := c.Query("cursor", "")
	if cursor != "" {
		if _, err := uuid.Parse(cursor); err != nil {
			return paginationParams{}, fmt.Errorf("invalid cursor format")
		}
	}
	return paginationParams{Limit: limit, Cursor: cursor}, nil
}

// refreshAccessCache reloads all model access allowlists from the database into
// the in-memory access cache. It is called after any Set*ModelAccess mutation
// so that the hot path immediately reflects the updated configuration.
// If AccessCache is nil the call is a no-op.
func (h *Handler) refreshAccessCache(ctx context.Context) {
	if h.AccessCache == nil {
		return
	}
	orgA, teamA, keyA, err := h.DB.LoadAllModelAccess(ctx)
	if err != nil {
		h.Log.ErrorContext(ctx, "refresh model access cache", slog.String("error", err.Error()))
		return
	}
	h.AccessCache.Load(orgA, teamA, keyA)
}

// refreshMCPCaches performs a single LoadAllActiveMCPServers query and feeds
// the result to both MCPServerCache and MCPTransportCache. It is called after
// any MCP server mutation so that both hot-path caches are updated atomically
// from one DB round-trip. If both caches are nil the call is a no-op.
func (h *Handler) refreshMCPCaches(ctx context.Context) {
	if h.MCPServerCache == nil && h.MCPTransportCache == nil {
		return
	}
	servers, err := h.DB.LoadAllActiveMCPServers(ctx)
	if err != nil {
		h.Log.ErrorContext(ctx, "refresh mcp caches", slog.String("error", err.Error()))
		return
	}
	if h.MCPServerCache != nil {
		h.MCPServerCache.LoadAll(servers)
	}
	if h.MCPTransportCache != nil {
		h.MCPTransportCache.LoadAll(servers)
	}
}

// refreshMCPAccessCache reloads all MCP access allowlists from the database
// into the in-memory MCP access cache. It is called after any Set*MCPAccess
// mutation so that the hot path immediately reflects the updated configuration.
// If MCPAccessCache is nil the call is a no-op.
func (h *Handler) refreshMCPAccessCache(ctx context.Context) {
	if h.MCPAccessCache == nil {
		return
	}
	orgA, teamA, keyA, err := h.DB.LoadAllMCPAccess(ctx)
	if err != nil {
		h.Log.ErrorContext(ctx, "refresh mcp access cache", slog.String("error", err.Error()))
		return
	}
	h.MCPAccessCache.Load(orgA, teamA, keyA)
}
