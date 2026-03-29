package app

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/voidmind-io/voidllm/internal/api/admin"
	"github.com/voidmind-io/voidllm/internal/api/health"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/jsonx"
)

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
		admin.RegisterRoutes(a.proxyApp, a.adminHandler, a.keyCache, a.hmacSecret, a.auditLogger)

		// Swagger UI is served after API routes but before the SPA catch-all.
		registerSwaggerHandlers(a.proxyApp)

		// SPA catch-all must be LAST — after all API routes.
		registerSPAHandler(a.proxyApp, a.log)
		return
	}

	// Dual-port mode: proxy and admin run on separate ports.
	a.adminApp = fiber.New(fiber.Config{
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   30 * time.Second,
		IdleTimeout:    60 * time.Second,
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
	a.log.LogAttrs(context.Background(), slog.LevelInfo, "starting servers",
		slog.String("proxy_addr", proxyAddr),
		slog.String("admin_addr", adminAddr),
		slog.String("mode", "split"),
	)
	go func() {
		if err := a.proxyApp.Listen(proxyAddr); err != nil {
			a.log.LogAttrs(context.Background(), slog.LevelError, "proxy server stopped",
				slog.String("error", err.Error()),
			)
		}
	}()
	go func() {
		if err := a.adminApp.Listen(adminAddr); err != nil {
			a.log.LogAttrs(context.Background(), slog.LevelError, "admin server stopped",
				slog.String("error", err.Error()),
			)
		}
	}()
}
