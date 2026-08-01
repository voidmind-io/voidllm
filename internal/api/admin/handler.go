// Package admin provides HTTP handlers for the VoidLLM Admin API.
package admin

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
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
	"github.com/voidmind-io/voidllm/internal/update"
)

// ModelHealthProvider provides upstream model health status for the admin API.
// It is implemented by *health.Checker and may be nil when health monitoring
// is not enabled.
type ModelHealthProvider interface {
	GetAllHealth() []health.ModelHealth
}

// Handler holds shared dependencies for all admin API handlers.
type Handler struct {
	// fallbackMu serializes fallback mutations (cycle-check + DB write) to make
	// them atomic at the process level. Acquired only when a CreateModel or
	// UpdateModel request includes a fallback_model_name change.
	//
	// Multi-instance cluster-wide serialization would require DB-level locking
	// (SELECT FOR UPDATE / advisory lock). For single-instance and typical
	// enterprise deployments the process-level mutex is sufficient.
	fallbackMu sync.Mutex

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
	FallbackMaxDepth  int                      // from config; 0 = fallback disabled; exposed via GET /license
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
	// to an external server. Zero falls back to a 30-second default. This is
	// the buffered-path (mcp.HTTPTransport.Call) timeout only — it never
	// applies to the transparent streaming proxy path (HandleMCPProxy); see
	// MCPStreamIdleTimeout.
	MCPCallTimeout time.Duration
	// MCPStreamIdleTimeout bounds the transparent streaming proxy path
	// (HandleMCPProxy, mcp.HTTPTransport.Forward) as an idle timeout, not a
	// total-duration one — see config.MCPConfig.StreamIdleTimeout, which this
	// is populated from. Zero falls back to a 120-second default.
	MCPStreamIdleTimeout time.Duration
	// MCPStreamMaxBytes bounds the aggregate bytes HandleMCPProxy relays from
	// a single upstream response on the transparent streaming proxy path
	// before ending the stream — see config.MCPConfig.StreamMaxBytes, which
	// this is populated from via config.MCPConfig.EffectiveStreamMaxBytes.
	// Zero means unbounded: unlike MCPCallTimeout/MCPStreamIdleTimeout above,
	// this field is never given a built-in fallback at the call site, because
	// zero is this field's own valid, deliberate "no limit" value rather
	// than a "not configured" sentinel — see the config field's doc for why
	// that distinction requires a pointer one level up. A Handler built
	// directly (e.g. in tests) without setting this field is therefore
	// unbounded on this path, not defaulted to 100 MiB; production wiring
	// (internal/app) always resolves a concrete value before assigning here.
	MCPStreamMaxBytes int64
	// MCPLogger receives asynchronous usage events for proxied MCP tool calls.
	// Nil disables usage logging for MCP proxy calls.
	MCPLogger MCPToolCallLogger
	// MCPSessionRegistry tracks, per external MCP server and per
	// (organization, API key) SessionScope (see mcp.NewClientSessionScope),
	// which legacy Mcp-Session-Id values the upstream has actually issued to
	// that caller. HandleMCPProxy consults it before relaying an inbound
	// Mcp-Session-Id upstream on the transparent proxy path, and records into
	// it whenever an upstream response carries one — see that handler and
	// mcp.SessionRegistry's own doc for the full contract, including why this
	// lives here rather than on any single *mcp.HTTPTransport. Constructed
	// once, in internal/app, and shared across every request.
	//
	// Nil FAILS CLOSED, not open: HandleMCPProxy treats every caller-supplied
	// Mcp-Session-Id as unrecognized and drops it, rather than skipping the
	// check and relaying every session unconditionally. A nil registry here
	// is a configuration error — production wiring always sets it — and a
	// tenant-isolation control must break loudly on a configuration error
	// (legacy session continuity stops working, which is immediately
	// visible) instead of silently degrading into the pre-fix, unchecked
	// behavior. A Handler built directly without this field, such as in a
	// test that does not exercise this path, gets the same fail-closed
	// treatment — it never sees an unchecked session relayed on its behalf.
	MCPSessionRegistry *mcp.SessionRegistry
	// MCPAllowPrivateURLs disables SSRF protection for MCP server URLs.
	// Set via YAML config only — not exposed in Admin API.
	MCPAllowPrivateURLs bool
	// MCPAllowedOrigins is the explicit Origin allowlist for MCP endpoints
	// (docs/mcp-v2.md §4.1's mandatory Origin validation). Empty (the
	// default) falls back to a built-in localhost-only allowlist — see
	// mcpOriginMiddleware and isDefaultAllowedOrigin, and
	// config.MCPConfig.AllowedOrigins for why this no longer falls back to
	// matching the request's own Host. Set via YAML config only — not
	// exposed in Admin API.
	MCPAllowedOrigins []string
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
	// LoginThrottle enforces per-IP and per-account brute-force protection on
	// the login endpoint. Nil disables throttling (test environments only).
	LoginThrottle *auth.LoginThrottle
	// UpdateChecker provides cached update status read from the settings table.
	// Nil in dev builds (Version == "dev") — GetUpdateStatus returns a static
	// response in that case.
	UpdateChecker *update.Checker
	// ReloadModels triggers an in-process rebuild of the model registry,
	// including the license-aware fallback gating. Called from SetLicense
	// to apply license changes immediately, independent of Redis pub/sub.
	// Must be safe to call concurrently. May be nil — callers must nil-check.
	ReloadModels func(context.Context) error
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
// the result to MCPServerCache, MCPTransportCache, and MCPSessionRegistry. It
// is called after any MCP server mutation (create, update, delete,
// activate/deactivate) so that both hot-path caches are updated atomically
// from one DB round-trip, and so MCPSessionRegistry.Reconcile prunes every
// server ID that just left the active set — see that method's doc for why
// reusing this one query, rather than a dedicated call threaded through each
// mutation path individually, is what lets it catch a server leaving the
// active set through any of them. If all three are nil the call is a no-op.
func (h *Handler) refreshMCPCaches(ctx context.Context) {
	if h.MCPServerCache == nil && h.MCPTransportCache == nil && h.MCPSessionRegistry == nil {
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
	if h.MCPSessionRegistry != nil {
		activeIDs := make([]string, len(servers))
		for i := range servers {
			activeIDs[i] = servers[i].ID
		}
		h.MCPSessionRegistry.Reconcile(activeIDs)
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
