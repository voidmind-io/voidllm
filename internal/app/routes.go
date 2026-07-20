package app

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/api/health"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

// playgroundTunnelPrefix is the route prefix for the authenticated,
// same-origin playground tunnel. See playgroundTunnel for details.
const playgroundTunnelPrefix = "/api/v1/playground/"

// maxAdminTunnelTimeout bounds how far setupRoutes may raise the admin app's
// ReadTimeout/WriteTimeout to accommodate the playground tunnel's streaming
// completions in dual-port mode.
//
// Fiber v3 has no per-route timeout: ReadTimeout/WriteTimeout are per-app
// config, so whatever value is adopted for the tunnel also applies to every
// OTHER route on the admin app — including the UNAUTHENTICATED ones (e.g.
// POST /api/v1/auth/login, POST /api/v1/invites/redeem, GET
// /api/v1/auth/providers, GET /api/v1/invites/peek). Adopting the proxy's
// configured timeout unbounded would let a slowloris-style attacker exhaust
// admin-app connection slots far more cheaply whenever an operator
// configures a generous proxy timeout (e.g. 300s) than with Fiber's short
// default. Do not remove this cap without also solving that exposure.
//
// The playground tunnel exists for the interactive Playground, where a human
// is waiting at a browser — 120s is ample for that use case. Operators who
// need longer-running completions should drive them against the proxy port
// directly, which carries no such cap.
const maxAdminTunnelTimeout = 120 * time.Second

// maxAdminTunnelBodyLimit bounds how far setupRoutes may raise the admin
// app's BodyLimit for the same reason as maxAdminTunnelTimeout: BodyLimit is
// per-app in Fiber v3, so raising it for the playground tunnel raises it for
// every unauthenticated admin endpoint too. Playground prompts are typed
// into a browser, so 8 MiB is far beyond realistic interactive use, while
// still capping the memory a single unauthenticated admin request can pin.
const maxAdminTunnelBodyLimit = 8 << 20 // 8 MiB

// maxAdminTunnelStreamDuration bounds how long a playground-tunnel request may
// keep streaming before ProxyHandler.Handle tears it down itself (clean
// upstream cancellation plus a terminal SSE abort event — see
// proxy.SetTunnelStreamCap).
//
// This MUST stay strictly below maxAdminTunnelTimeout, with real headroom for
// the final flush. In fasthttp 1.71, WriteTimeout is a single absolute socket
// write deadline set once before response serialization begins — it is never
// refreshed by successful SSE flushes. If a tunneled stream is still
// producing tokens when the admin app's capped WriteTimeout (see
// maxAdminTunnelTimeout) elapses, the connection is simply killed: the client
// gets a partial response, no terminal [DONE], and none of the deliberate
// abort events the proxy emits elsewhere — indistinguishable from a network
// failure. Firing the proxy's own stream timer first guarantees the client
// always sees the deliberate, well-formed teardown path instead.
//
// Do not raise this value without raising maxAdminTunnelTimeout by at least
// as much first — the two constants must be changed together.
const maxAdminTunnelStreamDuration = 105 * time.Second

// playgroundTunnel returns a Fiber handler that delegates authenticated
// requests under /api/v1/playground/* to the hot-path proxy handler,
// in-process. It exists so the embedded dashboard Playground (served from
// the SPA-serving app) always has a same-origin route to the proxy, even in
// dual-port mode where /v1/* is intentionally NOT mounted on the admin app
// (see setupRoutes). There is no network hop and no CORS involved — the
// request is handled by the same Fiber app instance.
//
// The request path is rewritten from "/api/v1/playground/<rest>" to
// "/v1/<rest>" before delegating, so that ProxyHandler.Handle derives the
// same upstream path it would for a direct /v1/<rest> request (see
// handler.go: upstreamPath := path.Clean(strings.TrimPrefix(c.Path(), "/v1/"))).
// The handler's own isAllowedPath check still gates which upstream endpoints
// are reachable through the tunnel — it is not bypassed or duplicated here.
//
// It also calls proxy.SetTunnelStreamCap with maxAdminTunnelStreamDuration so
// that a streaming completion tunneled through this handler is torn down by
// Handle's own stream timer before the admin app's capped WriteTimeout (see
// maxAdminTunnelTimeout) can kill the socket outright. This only ever
// shortens the stream budget for tunneled requests — direct /v1/* traffic
// never calls SetTunnelStreamCap and is completely unaffected.
func (a *Application) playgroundTunnel() fiber.Handler {
	return func(c fiber.Ctx) error {
		proxy.SetTunnelStreamCap(c, maxAdminTunnelStreamDuration)
		rest := strings.TrimPrefix(c.Path(), playgroundTunnelPrefix)
		c.Path("/v1/" + rest)
		return a.proxyHandler.Handle(c)
	}
}

// warnIfSinglePortTLS emits one WARN when admin TLS is configured but the
// admin port is sharing the proxy port (TLS termination unsupported there).
func (a *Application) warnIfSinglePortTLS(adminPort int) {
	if a.cfg.Server.Admin.TLS.Enabled {
		a.log.LogAttrs(context.Background(), slog.LevelWarn,
			"admin TLS configured but ignored in single-port mode",
			slog.Int("admin_port", adminPort),
			slog.Int("proxy_port", a.cfg.Server.Proxy.Port),
		)
	}
}

// devCORSMiddleware returns a Fiber handler that sets permissive CORS headers
// for every response. It is only installed when dev mode is active so that the
// Vite development server can reach both the proxy and admin apps without
// browser pre-flight errors. It must never be used in production.
func devCORSMiddleware() fiber.Handler {
	return func(c fiber.Ctx) error {
		c.Set("Access-Control-Allow-Origin", "*")
		c.Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		c.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		c.Set("Access-Control-Max-Age", "3600")
		if c.Method() == "OPTIONS" {
			return c.SendStatus(204)
		}
		return c.Next()
	}
}

// setupRoutes creates the Fiber app(s) and registers all routes. In single-port
// mode one Fiber app handles everything; in dual-port mode proxy and admin run
// on separate Fiber apps. The resulting apps are stored in a.proxyApp and
// a.adminApp. setupRoutes must be called after all dependencies are initialised.
func (a *Application) setupRoutes() {
	a.proxyApp = fiber.New(fiber.Config{
		ReadTimeout:    a.cfg.Server.Proxy.ReadTimeout,
		WriteTimeout:   a.cfg.Server.Proxy.WriteTimeout,
		IdleTimeout:    a.cfg.Server.Proxy.IdleTimeout,
		BodyLimit:      a.cfg.Server.Proxy.MaxRequestBody,
		ReadBufferSize: 16384, // 16 KB — default 4 KB too small for browser headers
		JSONEncoder:    func(v any) ([]byte, error) { return jsonx.Marshal(v) },
		JSONDecoder:    func(data []byte, v any) error { return jsonx.Unmarshal(data, v) },
	})

	// RequestID middleware must be FIRST so that all downstream handlers
	// (including error responses) can include the trace ID.
	a.proxyApp.Use(apierror.RequestIDMiddleware())

	// Dev mode: permissive CORS for the Vite dev server. Never used in production.
	if a.devMode {
		a.proxyApp.Use(devCORSMiddleware())
	}

	// Health and metrics are always mounted on the proxy port.
	a.proxyApp.Get("/healthz", health.Liveness())
	a.proxyApp.Get("/readyz", health.Readiness(a.database, a.shutdownState))
	a.proxyApp.Get("/health", health.Liveness())
	a.proxyApp.Get("/metrics", health.Metrics())

	// Proxy hot path: all /v1/* routes require a valid Bearer key.
	// GET /v1/models is handled by VoidLLM directly (not proxied to upstream).
	// It must be registered BEFORE the catch-all to take precedence.
	a.proxyApp.Get("/v1/models", auth.Middleware(a.keyCache, a.hmacSecret), a.proxyHandler.ModelsHandler)
	a.proxyApp.All("/v1/*", auth.Middleware(a.keyCache, a.hmacSecret), a.proxyHandler.Handle)

	adminPort := a.cfg.Server.Admin.Port

	if adminPort == 0 || adminPort == a.cfg.Server.Proxy.Port {
		// Single-port mode: admin routes share the proxy app.
		a.warnIfSinglePortTLS(adminPort)
		admin.RegisterRoutes(a.proxyApp, a.adminHandler, a.keyCache, a.hmacSecret, a.auditLogger)

		// Playground tunnel: registered unconditionally on the app that serves
		// the SPA so the UI can use one URL regardless of single/dual-port mode.
		// Must be registered before the SPA catch-all.
		a.proxyApp.All(playgroundTunnelPrefix+"*", auth.Middleware(a.keyCache, a.hmacSecret), a.playgroundTunnel())

		// Swagger UI is served after API routes but before the SPA catch-all.
		registerSwaggerHandlers(a.proxyApp)

		// SPA catch-all must be LAST — after all API routes.
		registerSPAHandler(a.proxyApp, a.log)
		return
	}

	// Dual-port mode: proxy and admin run on separate ports.
	//
	// ReadTimeout/WriteTimeout/BodyLimit are raised above the admin app's own
	// 30s/DefaultBodyLimit floors when the proxy is configured more generously:
	// the playground tunnel (/api/v1/playground/*, see playgroundTunnel)
	// forwards streaming LLM completions through this app in dual-port mode,
	// and a shorter timeout or body limit here would silently cut those
	// requests off.
	//
	// The adopted value is capped at maxAdminTunnelTimeout / maxAdminTunnelBodyLimit
	// — see those constants for why the cap must never be removed.
	adminReadTimeout := 30 * time.Second
	if a.cfg.Server.Proxy.ReadTimeout > adminReadTimeout {
		adminReadTimeout = a.cfg.Server.Proxy.ReadTimeout
	}
	if adminReadTimeout > maxAdminTunnelTimeout {
		adminReadTimeout = maxAdminTunnelTimeout
	}
	adminWriteTimeout := 30 * time.Second
	if a.cfg.Server.Proxy.WriteTimeout > adminWriteTimeout {
		adminWriteTimeout = a.cfg.Server.Proxy.WriteTimeout
	}
	if adminWriteTimeout > maxAdminTunnelTimeout {
		adminWriteTimeout = maxAdminTunnelTimeout
	}
	adminBodyLimit := fiber.DefaultBodyLimit
	if a.cfg.Server.Proxy.MaxRequestBody > adminBodyLimit {
		adminBodyLimit = a.cfg.Server.Proxy.MaxRequestBody
	}
	if adminBodyLimit > maxAdminTunnelBodyLimit {
		adminBodyLimit = maxAdminTunnelBodyLimit
	}

	a.adminApp = fiber.New(fiber.Config{
		ReadTimeout:    adminReadTimeout,
		WriteTimeout:   adminWriteTimeout,
		IdleTimeout:    60 * time.Second,
		BodyLimit:      adminBodyLimit,
		ReadBufferSize: 16384, // 16 KB — default 4 KB too small for browser headers
		JSONEncoder:    func(v any) ([]byte, error) { return jsonx.Marshal(v) },
		JSONDecoder:    func(data []byte, v any) error { return jsonx.Unmarshal(data, v) },
	})

	// Request ID middleware on the admin app in dual-port mode.
	a.adminApp.Use(apierror.RequestIDMiddleware())

	// Dev mode: permissive CORS for the Vite dev server on the admin app too.
	if a.devMode {
		a.adminApp.Use(devCORSMiddleware())
	}

	a.adminApp.Get("/healthz", health.Liveness())
	a.adminApp.Get("/readyz", health.Readiness(a.database, a.shutdownState))
	a.adminApp.Get("/health", health.Liveness())
	a.adminApp.Get("/metrics", health.Metrics())
	admin.RegisterRoutes(a.adminApp, a.adminHandler, a.keyCache, a.hmacSecret, a.auditLogger)

	// Playground tunnel: registered unconditionally on the app that serves the
	// SPA so the UI can use one URL regardless of single/dual-port mode. In
	// dual-port mode the SPA (and therefore this route) lives on the admin
	// app; /v1/* itself is deliberately NOT mounted here (see setupRoutes
	// single-port branch and package docs). Must be registered before the SPA
	// catch-all.
	a.adminApp.All(playgroundTunnelPrefix+"*", auth.Middleware(a.keyCache, a.hmacSecret), a.playgroundTunnel())

	// Swagger UI is served after API routes but before the SPA catch-all.
	registerSwaggerHandlers(a.adminApp)

	// SPA catch-all must be LAST on the admin app — after all API routes.
	// In dual-port mode the UI is served from the admin port, not the proxy port.
	registerSPAHandler(a.adminApp, a.log)
}

// startListening launches goroutines that call Listen on the Fiber app(s).
// Errors are logged via the instance logger. startListening must be called
// after setupRoutes.
func (a *Application) startListening() {
	proxyAddr := fmt.Sprintf(":%d", a.cfg.Server.Proxy.Port)

	if a.adminApp == nil {
		// Single-port mode.
		a.log.LogAttrs(context.Background(), slog.LevelInfo, "starting server",
			slog.String("addr", proxyAddr),
			slog.String("mode", "combined"),
		)
		go func() {
			if err := a.proxyApp.Listen(proxyAddr); err != nil {
				a.log.LogAttrs(context.Background(), slog.LevelError, "proxy server stopped",
					slog.String("error", err.Error()),
				)
			}
		}()
		return
	}

	// Dual-port mode.
	adminAddr := fmt.Sprintf(":%d", a.cfg.Server.Admin.Port)
	adminTLS := a.cfg.Server.Admin.TLS.Enabled
	a.log.LogAttrs(context.Background(), slog.LevelInfo, "starting servers",
		slog.String("proxy_addr", proxyAddr),
		slog.String("admin_addr", adminAddr),
		slog.String("mode", "split"),
		slog.Bool("admin_tls", adminTLS),
	)
	go func() {
		if err := a.proxyApp.Listen(proxyAddr); err != nil {
			a.log.LogAttrs(context.Background(), slog.LevelError, "proxy server stopped",
				slog.String("error", err.Error()),
			)
		}
	}()
	go func() {
		if adminTLS {
			certFile := a.cfg.Server.Admin.TLS.Cert
			keyFile := a.cfg.Server.Admin.TLS.Key
			if err := a.adminApp.Listen(adminAddr, fiber.ListenConfig{
				CertFile:    certFile,
				CertKeyFile: keyFile,
			}); err != nil {
				a.log.LogAttrs(context.Background(), slog.LevelError, "admin server stopped",
					slog.String("error", err.Error()),
					slog.Bool("tls", true),
					slog.String("cert", certFile),
					slog.String("key", keyFile),
				)
			}
			return
		}
		if err := a.adminApp.Listen(adminAddr); err != nil {
			a.log.LogAttrs(context.Background(), slog.LevelError, "admin server stopped",
				slog.String("error", err.Error()),
			)
		}
	}()
}
