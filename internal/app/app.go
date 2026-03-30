// Package app manages the top-level VoidLLM server lifecycle: construction,
// startup, signal handling, and phased graceful shutdown.
package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/gofiber/fiber/v3"

	otelapi "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/voidmind-io/voidllm/internal/api/admin"
	apihealth "github.com/voidmind-io/voidllm/internal/api/health"
	"github.com/voidmind-io/voidllm/internal/audit"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
	"github.com/voidmind-io/voidllm/internal/circuitbreaker"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/docs"
	"github.com/voidmind-io/voidllm/internal/health"
	"github.com/voidmind-io/voidllm/internal/license"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/internal/metrics"
	voidotel "github.com/voidmind-io/voidllm/internal/otel"
	"github.com/voidmind-io/voidllm/internal/proxy"
	"github.com/voidmind-io/voidllm/internal/ratelimit"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
	"github.com/voidmind-io/voidllm/internal/router"
	"github.com/voidmind-io/voidllm/internal/shutdown"
	"github.com/voidmind-io/voidllm/internal/sso"
	"github.com/voidmind-io/voidllm/internal/usage"
	"github.com/voidmind-io/voidllm/pkg/crypto"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// Application is the top-level VoidLLM server lifecycle coordinator. It owns
// every long-lived dependency and orchestrates startup, signal handling, and
// phased graceful shutdown. All fields are unexported; callers interact only
// through New, Start, PrintBootstrapCredentials, and WaitForShutdown.
type Application struct {
	cfg             *config.Config
	log             *slog.Logger
	devMode         bool
	licHolder       *license.Holder
	rawLicenseKey   string
	bootstrapResult *auth.BootstrapResult

	database   *db.DB
	encKey     []byte
	hmacSecret []byte

	registry          *proxy.Registry
	keyCache          *cache.Cache[string, auth.KeyInfo]
	accessCache       *proxy.ModelAccessCache
	aliasCache        *proxy.AliasCache
	mcpServerCache    *proxy.MCPServerCache
	mcpAccessCache    *proxy.MCPAccessCache
	mcpTransportCache *proxy.MCPTransportCache

	rateLimiter   ratelimit.Checker
	tokenCounter  *ratelimit.TokenCounter
	usageLogger   *usage.Logger
	mcpLogger     *usage.MCPLogger
	auditLogger   *audit.Logger
	healthChecker *health.Checker

	shutdownState *shutdown.State
	proxyHandler  *proxy.ProxyHandler
	adminHandler  *admin.Handler

	redisClient *voidredis.Client
	redisCancel context.CancelFunc

	proxyApp *fiber.App
	adminApp *fiber.App

	// otelShutdown flushes and closes the OTel TracerProvider on shutdown.
	// It is nil when OTel tracing is not enabled.
	otelShutdown func(context.Context) error

	// stopFuncs holds cleanup callbacks registered during Start. They are
	// invoked in LIFO order during cleanup().
	stopFuncs []func()
}

// dbUsageSeeder adapts *db.DB to the ratelimit.UsageSeeder interface. The DB
// method returns *sql.Rows (concrete type) while the interface requires
// ratelimit.RowScanner; this wrapper bridges the return type without introducing
// an import cycle between the db and ratelimit packages.
type dbUsageSeeder db.DB

func (s *dbUsageSeeder) QueryUsageSeed(ctx context.Context, since time.Time) (ratelimit.RowScanner, error) {
	return (*db.DB)(s).QueryUsageSeed(ctx, since)
}

// New constructs a fully-initialised Application by wiring all dependencies in
// the exact order required by their dependency graph. Startup order:
//
//  1. Open DB and run migrations
//  2. Parse encryption key
//  3. Sync YAML models to DB
//  4. Build registry from YAML + overlay DB models
//  5. HKDF-derive HMAC secret
//  6. Bootstrap auth, load keys into cache
//  7. Seed token counter from DB, create rate limiter
//  8. Start usage logger (and audit logger if enabled)
//  9. Load model access cache and alias cache from DB
//  10. Connect Redis (optional) and start pub/sub subscriber
//  11. Create shutdown state, proxy handler, admin handler
//
// New returns a non-nil error if any required step fails; in that case no
// goroutines have been started and no cleanup is needed by the caller.
func New(cfg *config.Config, log *slog.Logger, devMode bool) (*Application, error) {
	ctx := context.Background()

	enterpriseDev := devMode && (os.Getenv("VOIDLLM_ENTERPRISE_DEV") == "1" || os.Getenv("VOIDLLM_ENTERPRISE_DEV") == "true")
	if enterpriseDev {
		log.LogAttrs(ctx, slog.LevelWarn, "ENTERPRISE DEV MODE: all enterprise features enabled without license")
	}

	// Step 1: open database and run migrations.
	database, err := db.Open(ctx, cfg.Database)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	if err := db.RunMigrations(ctx, database.SQL(), database.Dialect()); err != nil {
		database.Close() //nolint:errcheck // best-effort on error path
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	// Check DB for a cached license JWT (refreshed by heartbeat on a previous run).
	configKey := cfg.Settings.License
	cachedKey, _ := database.GetSetting(ctx, "license_jwt")

	// Prefer DB-cached key (refreshed by heartbeat), fall back to config.
	licenseKey := configKey
	if cachedKey != "" {
		licenseKey = cachedKey
	}

	lic := license.Verify(licenseKey, enterpriseDev)

	// If DB-cached key failed (e.g. corrupted), try config key as fallback.
	if lic.Edition() != license.EditionEnterprise && configKey != "" && configKey != licenseKey {
		lic = license.Verify(configKey, enterpriseDev)
		if lic.Edition() == license.EditionEnterprise {
			licenseKey = configKey
		}
	}

	licHolder := license.NewHolder(lic)
	log.LogAttrs(ctx, slog.LevelInfo, "license loaded",
		slog.String("edition", string(lic.Edition())),
		slog.Bool("valid", lic.Valid()),
	)

	// Declare variables that the deferred cleanup needs to reference before
	// they are assigned by the steps below.
	var (
		encKey      []byte
		hmacSecret  []byte
		usageLogger *usage.Logger
		auditLogger *audit.Logger
	)

	// From this point on, any early return must clean up in reverse order.
	// The defer fires on every return; success=true suppresses it on the
	// happy path.
	success := false
	defer func() {
		if success {
			return
		}
		if usageLogger != nil {
			usageLogger.Stop()
		}
		if auditLogger != nil {
			auditLogger.Stop()
		}
		crypto.ZeroKey(hmacSecret)
		crypto.ZeroKey(encKey)
		database.Close() //nolint:errcheck // best-effort on error path
	}()

	// Step 2: parse encryption key.
	encKey, err = crypto.ParseKey(cfg.Settings.EncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("parse encryption key: %w", err)
	}

	// Step 3: sync YAML-configured models into the DB so they survive restarts
	// and can be discovered by the Admin API.
	if err := database.SyncYAMLModels(ctx, cfg.Models, encKey); err != nil {
		return nil, fmt.Errorf("sync YAML models: %w", err)
	}
	if err := database.SyncYAMLMCPServers(ctx, cfg.MCPServers, encKey); err != nil {
		return nil, fmt.Errorf("sync YAML MCP servers: %w", err)
	}
	builtinServer, err := database.EnsureBuiltinMCPServer(ctx)
	if err != nil {
		return nil, fmt.Errorf("ensure builtin MCP server: %w", err)
	}

	// Probe all active MCP servers to detect deprecated SSE transport.
	// Servers that only support SSE are auto-deactivated with a warning.
	if allServers, listErr := database.ListMCPServers(ctx); listErr == nil {
		for _, s := range allServers {
			if s.Source == "builtin" {
				continue // built-in server has no external URL to probe
			}
			var token string
			if s.AuthTokenEnc != nil && *s.AuthTokenEnc != "" {
				token, _ = crypto.DecryptString(*s.AuthTokenEnc, encKey, []byte("mcp_server:"+s.ID))
			}
			transport := mcp.NewHTTPTransport(s.URL, s.AuthType, s.AuthHeader, token, 10*time.Second, cfg.Settings.MCP.AllowPrivateURLs)
			_, probeErr := transport.ListTools(ctx)
			transport.Close()
			if errors.Is(probeErr, mcp.ErrSSENotSupported) {
				isActive := false
				database.UpdateMCPServer(ctx, s.ID, db.UpdateMCPServerParams{IsActive: &isActive}) //nolint:errcheck
				log.WarnContext(ctx, "MCP server uses deprecated SSE transport, deactivated",
					slog.String("alias", s.Alias),
					slog.String("url", s.URL))
			}
		}
	}

	// Step 4a: build the in-memory registry from the YAML config.
	registry, err := proxy.NewRegistry(cfg.Models)
	if err != nil {
		return nil, fmt.Errorf("build model registry: %w", err)
	}

	// loadModelsIntoRegistry fetches all active models from the DB, decrypts
	// their API keys, and upserts each one into the registry. It is called once
	// at startup and again whenever a ChannelModels invalidation is received via
	// Redis pub/sub so that all instances stay consistent.
	loadModelsIntoRegistry := func(loadCtx context.Context) error {
		dbModels, loadErr := database.ListActiveModels(loadCtx)
		if loadErr != nil {
			return fmt.Errorf("list active models: %w", loadErr)
		}
		for _, m := range dbModels {
			var apiKey string
			if m.APIKeyEncrypted != nil {
				var decErr error
				apiKey, decErr = crypto.DecryptString(*m.APIKeyEncrypted, encKey, []byte("model:"+m.ID))
				if decErr != nil {
					// A decryption failure here means the stored ciphertext is
					// corrupt or the encryption key has changed. Log and continue
					// so the remaining models are still loaded.
					log.LogAttrs(loadCtx, slog.LevelError, "failed to decrypt model api key",
						slog.String("model", m.Name),
						slog.String("error", decErr.Error()),
					)
				}
			}
			var aliases []string
			if m.Aliases != "" {
				aliases = strings.Split(m.Aliases, ",")
			}
			var timeout time.Duration
			if m.Timeout != "" {
				if d, parseErr := time.ParseDuration(m.Timeout); parseErr == nil {
					timeout = d
				} else {
					log.LogAttrs(loadCtx, slog.LevelWarn, "model: invalid timeout string, ignoring",
						slog.String("model", m.Name),
						slog.String("timeout", m.Timeout),
						slog.String("error", parseErr.Error()),
					)
				}
			}
			modelType := m.ModelType
			if modelType == "" {
				modelType = "chat"
			}

			// Load per-deployment endpoints for load-balanced models.
			dbDeps, depsErr := database.ListActiveDeployments(loadCtx, m.ID)
			if depsErr != nil {
				return fmt.Errorf("list active deployments for model %s: %w", m.Name, depsErr)
			}
			deployments := make([]proxy.Deployment, 0, len(dbDeps))
			for _, dep := range dbDeps {
				var depAPIKey string
				if dep.APIKeyEncrypted != nil {
					var decErr error
					depAPIKey, decErr = crypto.DecryptString(*dep.APIKeyEncrypted, encKey, deploymentAAD(dep.ID))
					if decErr != nil {
						log.LogAttrs(loadCtx, slog.LevelError, "failed to decrypt deployment api key",
							slog.String("model", m.Name),
							slog.String("deployment", dep.Name),
							slog.String("error", decErr.Error()),
						)
					}
				}
				deployments = append(deployments, proxy.Deployment{
					Name:            dep.Name,
					Provider:        dep.Provider,
					BaseURL:         dep.BaseURL,
					APIKey:          depAPIKey,
					AzureDeployment: dep.AzureDeployment,
					AzureAPIVersion: dep.AzureAPIVersion,
					Weight:          dep.Weight,
					Priority:        dep.Priority,
				})
			}

			registry.AddModel(proxy.Model{
				Name:             m.Name,
				Provider:         m.Provider,
				Type:             modelType,
				BaseURL:          m.BaseURL,
				APIKey:           apiKey,
				Aliases:          aliases,
				MaxContextTokens: m.MaxContextTokens,
				Pricing:          config.PricingConfig{InputPer1M: m.InputPricePer1M, OutputPer1M: m.OutputPricePer1M},
				AzureDeployment:  m.AzureDeployment,
				AzureAPIVersion:  m.AzureAPIVersion,
				Timeout:          timeout,
				Strategy:         m.Strategy,
				MaxRetries:       m.MaxRetries,
				Deployments:      deployments,
			})
		}
		return nil
	}

	// Step 4b: overlay DB models on top of YAML registry.
	if err := loadModelsIntoRegistry(ctx); err != nil {
		return nil, fmt.Errorf("load models from database: %w", err)
	}

	// Step 5: derive HMAC secret from the encryption key using HKDF (RFC 5869).
	// Hash: SHA-256, IKM: encKey, salt: nil, info: "voidllm-hmac-key".
	hkdfReader := hkdf.New(sha256.New, encKey, nil, []byte("voidllm-hmac-key"))
	hmacSecret = make([]byte, 32)
	if _, err := io.ReadFull(hkdfReader, hmacSecret); err != nil {
		return nil, fmt.Errorf("derive HMAC secret: %w", err)
	}

	// Step 6: bootstrap auth (creates system admin if absent), then load all
	// active API keys into the in-memory cache for hot-path lookups.
	keyCache := cache.New[string, auth.KeyInfo]()

	bootstrapResult, err := auth.Bootstrap(ctx, database.SQL(), database.Dialect(), keyCache, cfg.Settings, hmacSecret, log)
	if err != nil {
		return nil, fmt.Errorf("bootstrap auth: %w", err)
	}

	if err := auth.LoadKeysIntoCache(ctx, database, keyCache, log); err != nil {
		return nil, fmt.Errorf("load keys into cache: %w", err)
	}

	// Step 7: seed token counter from recent usage events.
	tokenCounter := ratelimit.NewTokenCounter()
	if err := tokenCounter.Seed(ctx, (*dbUsageSeeder)(database)); err != nil {
		return nil, fmt.Errorf("seed token counter: %w", err)
	}

	// Step 8: start usage logger (always) and audit logger (if enabled).
	usageLogger = usage.NewLogger(database, cfg.Settings.Usage, log, tokenCounter)
	usageLogger.Start()

	metrics.RegisterDBCollectors(database.SQL())
	metrics.RegisterUsageCollector(usageLogger)

	if cfg.Settings.Audit.Enabled && lic.HasFeature(license.FeatureAuditLogs) {
		auditLogger = audit.NewLogger(database, cfg.Settings.Audit, log)
		auditLogger.Start()
		log.LogAttrs(ctx, slog.LevelInfo, "audit logging enabled")
	}

	// Step 9: load model access cache and alias cache from DB.
	accessCache := proxy.NewModelAccessCache()
	orgA, teamA, keyA, err := database.LoadAllModelAccess(ctx)
	if err != nil {
		return nil, fmt.Errorf("load model access cache: %w", err)
	}
	accessCache.Load(orgA, teamA, keyA)

	aliasCache := proxy.NewAliasCache()
	orgAliases, teamAliases, err := database.LoadAllModelAliases(ctx)
	if err != nil {
		log.Error("load model aliases", slog.String("error", err.Error()))
	} else {
		aliasCache.Load(orgAliases, teamAliases)
	}

	mcpServerCache := proxy.NewMCPServerCache()
	if mcpServers, mcpServersErr := database.LoadAllActiveMCPServers(ctx); mcpServersErr == nil {
		mcpServerCache.LoadAll(mcpServers)
	} else {
		log.Error("load mcp server cache", slog.String("error", mcpServersErr.Error()))
	}

	mcpAccessCache := proxy.NewMCPAccessCache()
	if mcpOrgA, mcpTeamA, mcpKeyA, mcpAccessErr := database.LoadAllMCPAccess(ctx); mcpAccessErr == nil {
		mcpAccessCache.Load(mcpOrgA, mcpTeamA, mcpKeyA)
	} else {
		log.Error("load mcp access cache", slog.String("error", mcpAccessErr.Error()))
	}

	mcpTransportCache := proxy.NewMCPTransportCache(encKey, cfg.Settings.MCP.AllowPrivateURLs, cfg.Settings.MCP.CallTimeout, log)
	if mcpServersForTransport, mcpTransportErr := database.LoadAllActiveMCPServers(ctx); mcpTransportErr == nil {
		mcpTransportCache.LoadAll(mcpServersForTransport)
	} else {
		log.Error("load mcp transport cache", slog.String("error", mcpTransportErr.Error()))
	}

	// Step 10: connect Redis (optional). On failure, continue without Redis.
	redisCtx, redisCancel := context.WithCancel(context.Background())
	var redisClient *voidredis.Client

	if cfg.Redis.Enabled {
		var redisErr error
		redisClient, redisErr = voidredis.New(cfg.Redis.URL, cfg.Redis.KeyPrefix)
		if redisErr != nil {
			log.LogAttrs(ctx, slog.LevelError, "redis connection failed, continuing without redis",
				slog.String("error", redisErr.Error()),
			)
		} else {
			log.LogAttrs(ctx, slog.LevelInfo, "redis connected")

			// Start the pub/sub subscriber goroutine. It blocks until redisCtx
			// is canceled (on shutdown) and handles cache invalidation messages
			// published by other VoidLLM instances.
			go redisClient.SubscribeInvalidations(redisCtx, log, func(channel, payload string) {
				switch channel {
				case voidredis.ChannelKeys:
					// Payload is the key hash — evict exactly that entry.
					keyCache.Delete(payload)
					log.LogAttrs(context.Background(), slog.LevelDebug, "redis: key cache invalidated")
				case voidredis.ChannelModels:
					// Reload all active models into the registry from the DB.
					if loadErr := loadModelsIntoRegistry(context.Background()); loadErr != nil {
						log.LogAttrs(context.Background(), slog.LevelError,
							"redis: reload model registry failed",
							slog.String("error", loadErr.Error()),
						)
						return
					}
					log.LogAttrs(context.Background(), slog.LevelDebug, "redis: model registry reloaded")
				case voidredis.ChannelAccess:
					// Reload full model access cache from the database.
					orgA, teamA, keyA, loadErr := database.LoadAllModelAccess(context.Background())
					if loadErr != nil {
						log.LogAttrs(context.Background(), slog.LevelError,
							"redis: reload access cache failed",
							slog.String("error", loadErr.Error()),
						)
						return
					}
					accessCache.Load(orgA, teamA, keyA)
					log.LogAttrs(context.Background(), slog.LevelDebug, "redis: access cache reloaded")
				case voidredis.ChannelAliases:
					// Reload full alias cache from the database.
					orgAl, teamAl, loadErr := database.LoadAllModelAliases(context.Background())
					if loadErr != nil {
						log.LogAttrs(context.Background(), slog.LevelError,
							"redis: reload alias cache failed",
							slog.String("error", loadErr.Error()),
						)
						return
					}
					aliasCache.Load(orgAl, teamAl)
					log.LogAttrs(context.Background(), slog.LevelDebug, "redis: alias cache reloaded")
				}
			})
		}
	}

	// Select rate limiter implementation: distributed (Redis) when available,
	// in-memory otherwise. This must run after the Redis connection attempt so
	// that redisClient is non-nil only when the connection succeeded.
	var rateLimiter ratelimit.Checker
	if redisClient != nil {
		rateLimiter = ratelimit.NewRedisChecker(redisClient, log)
		log.LogAttrs(ctx, slog.LevelInfo, "rate limiting: distributed (Redis)")
	} else {
		rateLimiter = ratelimit.NewRateLimiter()
		log.LogAttrs(ctx, slog.LevelInfo, "rate limiting: in-memory (single instance)")
	}

	// Step 11: create shutdown state, proxy handler, admin handler.
	shutdownState := shutdown.New()

	var cbRegistry *circuitbreaker.Registry
	if cfg.Settings.CircuitBreaker.Enabled {
		cbRegistry = circuitbreaker.NewRegistry(circuitbreaker.Config{
			Enabled:     true,
			Threshold:   cfg.Settings.CircuitBreaker.Threshold,
			Timeout:     cfg.Settings.CircuitBreaker.Timeout,
			HalfOpenMax: cfg.Settings.CircuitBreaker.HalfOpenMax,
		})
		log.LogAttrs(ctx, slog.LevelInfo, "circuit breaker enabled",
			slog.Int("threshold", cfg.Settings.CircuitBreaker.Threshold),
			slog.Duration("timeout", cfg.Settings.CircuitBreaker.Timeout),
		)
	}

	// OTel tracing: initialise only when the feature is licensed and enabled.
	var otelShutdownFn func(context.Context) error
	var tracer trace.Tracer
	if cfg.Settings.OTel.Enabled && lic.HasFeature(license.FeatureOTelTracing) {
		var setupErr error
		otelShutdownFn, setupErr = voidotel.Setup(ctx,
			cfg.Settings.OTel.Endpoint,
			cfg.Settings.OTel.Insecure,
			*cfg.Settings.OTel.SampleRate,
			"voidllm", "0.3.0",
		)
		if setupErr != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "otel setup failed, tracing disabled",
				slog.String("error", setupErr.Error()),
			)
		} else {
			tracer = otelapi.Tracer("voidllm.proxy")
			log.LogAttrs(ctx, slog.LevelInfo, "opentelemetry tracing enabled",
				slog.String("endpoint", cfg.Settings.OTel.Endpoint),
				slog.Float64("sample_rate", *cfg.Settings.OTel.SampleRate),
			)
		}
	}

	// SSO/OIDC: initialise only when the feature is licensed and enabled.
	var ssoProvider *sso.Provider
	if cfg.Settings.SSO.Enabled && lic.HasFeature(license.FeatureSSOOIDC) {
		var ssoErr error
		ssoProvider, ssoErr = sso.NewProvider(ctx, cfg.Settings.SSO)
		if ssoErr != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "SSO/OIDC setup failed, SSO disabled",
				slog.String("error", ssoErr.Error()),
			)
		} else {
			log.LogAttrs(ctx, slog.LevelInfo, "SSO/OIDC enabled",
				slog.String("issuer", cfg.Settings.SSO.Issuer),
			)
		}
	}

	// Build the health checker when at least one probe level is enabled.
	var healthChecker *health.Checker
	hcCfg := cfg.Settings.HealthCheck
	if hcCfg.Health.Enabled || hcCfg.Models.Enabled || hcCfg.Functional.Enabled {
		healthChecker = health.NewChecker(registry, hcCfg, log)
	}
	if hcCfg.Functional.Enabled {
		log.LogAttrs(ctx, slog.LevelWarn,
			"functional health probe enabled — sends billable requests to upstream providers",
			slog.Duration("interval", hcCfg.Functional.Interval),
		)
	}

	// Create the deployment router for load balancing. Both dependencies are
	// optional: healthChecker may be nil when health probes are disabled, and
	// cbRegistry may be nil when circuit breaking is disabled.
	modelRouter := router.NewRouter(healthChecker, cbRegistry)

	proxyHandler := proxy.NewProxyHandler(registry, log)
	proxyHandler.AccessCache = accessCache
	proxyHandler.AliasCache = aliasCache
	proxyHandler.CircuitBreakers = cbRegistry
	proxyHandler.Router = modelRouter
	proxyHandler.UsageLogger = usageLogger
	proxyHandler.RateLimiter = rateLimiter
	proxyHandler.TokenCounter = tokenCounter
	proxyHandler.ShutdownState = shutdownState
	proxyHandler.Tracer = tracer
	proxyHandler.MaxRequestBody = cfg.Server.Proxy.MaxRequestBody
	proxyHandler.MaxResponseBody = cfg.Server.Proxy.MaxResponseBody
	proxyHandler.MaxStreamDuration = cfg.Server.Proxy.MaxStreamDuration

	adminHandler := &admin.Handler{
		DB:                database,
		HMACSecret:        hmacSecret,
		EncryptionKey:     encKey,
		KeyCache:          keyCache,
		Registry:          registry,
		AccessCache:       accessCache,
		AliasCache:        aliasCache,
		MCPServerCache:    mcpServerCache,
		MCPAccessCache:    mcpAccessCache,
		MCPTransportCache: mcpTransportCache,
		Redis:             redisClient,
		AuditLogger:       auditLogger,
		License:           licHolder,
		Log:               log,
		SSOProvider:       ssoProvider,
		SSOConfig:         cfg.Settings.SSO,
	}
	// Only assign the health checker when it was actually created — a typed nil
	// (*health.Checker)(nil) satisfies the interface but is NOT == nil when
	// checked as ModelHealthProvider, causing nil-pointer panics.
	if healthChecker != nil {
		adminHandler.HealthChecker = healthChecker
	}

	// builtinMCPServer is set after RegisterVoidLLMTools below; the ToolFetcher
	// closure captures this variable so the assignment is visible at call time.
	// toolStore is also hoisted so the built-in tools can be persisted after
	// RegisterVoidLLMTools populates the server.
	var builtinMCPServer *mcp.Server
	var toolStore mcp.ToolStore

	// Code Mode: create the runtime pool, executor, and tool cache when enabled.
	// These are assigned to the handler before RegisterVoidLLMTools is called so
	// that the deps closures below can close over the handler and its fields.
	if cfg.Settings.MCP.CodeMode.IsEnabled() {
		codePool, poolErr := mcp.NewRuntimePool(
			cfg.Settings.MCP.CodeMode.PoolSize,
			cfg.Settings.MCP.CodeMode.MemoryLimitMB,
			cfg.Settings.MCP.CodeMode.Timeout,
		)
		if poolErr != nil {
			redisCancel()
			return nil, fmt.Errorf("create code mode pool: %w", poolErr)
		}
		adminHandler.CodePool = codePool
		adminHandler.CodeExecutor = mcp.NewExecutor(codePool)
		toolStore = &dbToolStore{db: database}
		httpFetcher := adminHandler.MakeToolFetcher()
		builtinServerID := builtinServer.ID
		adminHandler.ToolCache = mcp.NewPersistentToolCache(func(fetchCtx context.Context, serverID string) ([]mcp.Tool, error) {
			if serverID == builtinServerID && builtinMCPServer != nil {
				return builtinMCPServer.Tools(), nil
			}
			return httpFetcher(fetchCtx, serverID)
		}, time.Hour, toolStore)
		if loadErr := adminHandler.ToolCache.LoadFromStore(ctx); loadErr != nil {
			log.WarnContext(ctx, "failed to load cached tools from DB", slog.String("error", loadErr.Error()))
		}
		log.LogAttrs(ctx, slog.LevelInfo, "code mode enabled",
			slog.Int("pool_size", cfg.Settings.MCP.CodeMode.PoolSize),
			slog.Int("memory_limit_mb", cfg.Settings.MCP.CodeMode.MemoryLimitMB),
			slog.Duration("timeout", cfg.Settings.MCP.CodeMode.Timeout),
		)
	}

	// Wire the management MCP server with VoidLLM management tools. The server
	// is always created; the route is only registered when MCPServer is non-nil
	// (which it always is after this block). Tools that need DB access or RBAC
	// enforcement perform those checks inside the handler function.

	// Create Code Mode service — wires the three Code Mode VoidLLMDeps functions
	// and the OnToolsListHook as methods on a single struct so they share state
	// without ad-hoc closure captures.
	cmService := &codeModeService{
		executor:     adminHandler.CodeExecutor,
		toolCache:    adminHandler.ToolCache,
		callMCPTool:  adminHandler.CallMCPTool,
		db:           database,
		log:          log,
		maxToolCalls: cfg.Settings.MCP.CodeMode.MaxToolCalls,
		serverCache:  mcpServerCache,
		codePool:     adminHandler.CodePool,
	}

	mcpServer := mcp.NewServer("voidllm", apihealth.Version)
	voidllmDeps := mcp.VoidLLMDeps{
		ListModels: func(ctx context.Context) ([]map[string]any, error) {
			infos := registry.ListInfo()
			result := make([]map[string]any, len(infos))
			for i, info := range infos {
				result[i] = map[string]any{
					"name":               info.Name,
					"provider":           info.Provider,
					"type":               info.Type,
					"aliases":            info.Aliases,
					"max_context_tokens": info.MaxContextTokens,
					"strategy":           info.Strategy,
					"deployment_count":   info.DeploymentCount,
				}
			}
			return result, nil
		},
		ListAvailableModels: func(ctx context.Context) ([]map[string]any, error) {
			// Return only name + type for models accessible to the caller.
			// Uses the same access-cache logic as the /me/available-models endpoint
			// but scoped via the KeyIdentity carried in the MCP context.
			id := mcp.KeyIdentityFromCtx(ctx)
			infos := registry.ListInfo()
			result := make([]map[string]any, 0, len(infos))
			for _, info := range infos {
				if accessCache == nil || accessCache.Check(id.OrgID, "", id.KeyID, info.Name) {
					modelType := info.Type
					if modelType == "" {
						modelType = "chat"
					}
					result = append(result, map[string]any{
						"name": info.Name,
						"type": modelType,
					})
				}
			}
			return result, nil
		},
		GetAllHealth: func() []map[string]any {
			if healthChecker == nil {
				return nil
			}
			healths := healthChecker.GetAllHealth()
			result := make([]map[string]any, len(healths))
			for i, h := range healths {
				result[i] = map[string]any{
					"name":       h.ModelName,
					"status":     h.Status,
					"latency_ms": h.LatencyMs,
					"last_error": h.LastError,
				}
			}
			return result
		},
		GetHealth: func(key string) (map[string]any, bool) {
			if healthChecker == nil {
				return nil, false
			}
			h, ok := healthChecker.GetHealth(key)
			if !ok {
				return nil, false
			}
			return map[string]any{
				"name":          h.ModelName,
				"status":        h.Status,
				"latency_ms":    h.LatencyMs,
				"last_error":    h.LastError,
				"health_ok":     h.HealthOK,
				"models_ok":     h.ModelsOK,
				"functional_ok": h.FunctionalOK,
			}, true
		},
		GetUsage: func(ctx context.Context, from, to, groupBy, orgID, keyID string) (any, error) {
			return map[string]any{
				"message": "use the VoidLLM web UI or GET /api/v1/usage for detailed analytics",
			}, nil
		},
		ListKeys: func(ctx context.Context, orgID, role string) ([]map[string]any, error) {
			// Org admins and system admins see all non-session keys in the org.
			// Members see only their own keys via the userID filter.
			var userID string
			if role != auth.RoleOrgAdmin && role != auth.RoleSystemAdmin {
				id := mcp.KeyIdentityFromCtx(ctx)
				userID = id.UserID
			}
			keys, err := database.ListAPIKeys(ctx, orgID, userID, "", "", 200, false)
			if err != nil {
				return nil, fmt.Errorf("list api keys: %w", err)
			}
			result := make([]map[string]any, len(keys))
			for i, k := range keys {
				result[i] = map[string]any{
					"id":         k.ID,
					"key_hint":   k.KeyHint,
					"key_type":   k.KeyType,
					"name":       k.Name,
					"created_at": k.CreatedAt,
				}
			}
			return result, nil
		},
		CreateKey: func(ctx context.Context, orgID, userID, name string, expiresIn time.Duration) (map[string]any, error) {
			plaintextKey, err := keygen.Generate(keygen.KeyTypeUser)
			if err != nil {
				return nil, fmt.Errorf("generate key: %w", err)
			}
			keyHash := keygen.Hash(plaintextKey, hmacSecret)
			keyHint := keygen.Hint(plaintextKey)

			var expiresAt *string
			if expiresIn > 0 {
				t := time.Now().UTC().Add(expiresIn).Format(time.RFC3339)
				expiresAt = &t
			}

			apiKey, err := database.CreateAPIKey(ctx, db.CreateAPIKeyParams{
				KeyHash:   keyHash,
				KeyHint:   keyHint,
				KeyType:   keygen.KeyTypeUser,
				Name:      name,
				OrgID:     orgID,
				UserID:    &userID,
				ExpiresAt: expiresAt,
				CreatedBy: userID,
			})
			if err != nil {
				return nil, fmt.Errorf("create api key: %w", err)
			}

			// Resolve the user's role so the key cache entry is accurate.
			resolvedRole, roleErr := database.GetUserOrgRole(ctx, userID, orgID)
			if roleErr == nil && resolvedRole != "" {
				var expTime *time.Time
				if apiKey.ExpiresAt != nil {
					if t, parseErr := time.Parse(time.RFC3339, *apiKey.ExpiresAt); parseErr == nil {
						expTime = &t
					}
				}
				keyCache.Set(apiKey.KeyHash, auth.KeyInfo{
					ID:        apiKey.ID,
					KeyType:   apiKey.KeyType,
					Role:      resolvedRole,
					OrgID:     apiKey.OrgID,
					UserID:    userID,
					Name:      apiKey.Name,
					ExpiresAt: expTime,
				})
			}

			return map[string]any{
				"id":         apiKey.ID,
				"key":        plaintextKey,
				"key_hint":   apiKey.KeyHint,
				"name":       apiKey.Name,
				"expires_at": apiKey.ExpiresAt,
			}, nil
		},
		ListDeployments: func(ctx context.Context, modelID string) ([]map[string]any, error) {
			deps, err := database.ListDeployments(ctx, modelID)
			if err != nil {
				return nil, fmt.Errorf("list deployments: %w", err)
			}
			result := make([]map[string]any, len(deps))
			for i, d := range deps {
				result[i] = map[string]any{
					"id":        d.ID,
					"name":      d.Name,
					"provider":  d.Provider,
					"base_url":  d.BaseURL,
					"weight":    d.Weight,
					"priority":  d.Priority,
					"is_active": d.IsActive,
				}
			}
			return result, nil
		},
		ExecuteCode:              cmService.ExecuteCode,
		ListAccessibleMCPServers: cmService.ListAccessibleMCPServers,
		SearchMCPTools:           cmService.SearchMCPTools,
	}
	mcp.RegisterVoidLLMTools(mcpServer, voidllmDeps)
	adminHandler.MCPServer = mcpServer

	// Expose the built-in server's tools through the ToolCache so Code Mode
	// callers can discover and invoke them without an HTTP round-trip.
	builtinMCPServer = mcpServer
	if adminHandler.ToolCache != nil {
		builtinTools := mcpServer.Tools()
		adminHandler.ToolCache.SetTools(builtinServer.ID, builtinTools)
		if toolStore != nil {
			toolStore.Save(ctx, builtinServer.ID, builtinTools) //nolint:errcheck
		}
	}

	// Wire the Code Mode MCP server when Code Mode is enabled. It exposes only
	// the list_servers, search_tools, and execute_code tools and is served at
	// /api/v1/mcp (distinct from the management server at /api/v1/mcp/voidllm).
	if cfg.Settings.MCP.CodeMode.IsEnabled() {
		codeModeServer := mcp.NewServer("voidllm-code-mode", apihealth.Version)
		mcp.RegisterCodeModeTools(codeModeServer, mcp.VoidLLMDeps{
			ExecuteCode:              voidllmDeps.ExecuteCode,
			ListAccessibleMCPServers: voidllmDeps.ListAccessibleMCPServers,
			SearchMCPTools:           voidllmDeps.SearchMCPTools,
		})

		// Inject TypeScript type declarations into the execute_code tool
		// description so LLMs can generate correct tool calls without calling
		// search_tools first. The hook runs on every tools/list request so the
		// declarations stay current as the ToolCache is populated lazily.
		codeModeServer.SetOnToolsList(cmService.toolsListHook())

		adminHandler.CodeModeServer = codeModeServer
	}

	adminHandler.MCPCallTimeout = cfg.Settings.MCP.CallTimeout
	adminHandler.MCPAllowPrivateURLs = cfg.Settings.MCP.AllowPrivateURLs

	mcpLogger := usage.NewMCPLogger(database, 1000, log)
	adminHandler.MCPLogger = mcpLogger

	success = true
	return &Application{
		cfg:               cfg,
		log:               log,
		devMode:           devMode,
		licHolder:         licHolder,
		rawLicenseKey:     licenseKey,
		bootstrapResult:   bootstrapResult,
		database:          database,
		encKey:            encKey,
		hmacSecret:        hmacSecret,
		registry:          registry,
		keyCache:          keyCache,
		accessCache:       accessCache,
		aliasCache:        aliasCache,
		mcpServerCache:    mcpServerCache,
		mcpAccessCache:    mcpAccessCache,
		mcpTransportCache: mcpTransportCache,
		rateLimiter:       rateLimiter,
		tokenCounter:      tokenCounter,
		usageLogger:       usageLogger,
		mcpLogger:         mcpLogger,
		auditLogger:       auditLogger,
		healthChecker:     healthChecker,
		shutdownState:     shutdownState,
		proxyHandler:      proxyHandler,
		adminHandler:      adminHandler,
		redisClient:       redisClient,
		redisCancel:       redisCancel,
		otelShutdown:      otelShutdownFn,
	}, nil
}

// Start launches background goroutines (cache refresh tickers, pprof if dev
// mode) and begins listening on the configured port(s). Start must be called
// exactly once after New returns successfully.
//
// Listener errors are handled asynchronously; the error return is reserved for
// future synchronous startup checks and currently always returns nil.
func (a *Application) Start() error {
	// Cache refresh tickers. Stop functions are registered in LIFO order so
	// that the key refresh stops first on shutdown (matching startup order).
	a.stopFuncs = append(a.stopFuncs,
		auth.StartCacheRefresh(a.database, a.keyCache, a.cfg.Cache.KeyTTL, a.log),
		startTicker(a.cfg.Cache.ModelTTL, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			orgA, teamA, keyA, err := a.database.LoadAllModelAccess(ctx)
			if err != nil {
				a.log.LogAttrs(context.Background(), slog.LevelError, "access cache refresh failed",
					slog.String("error", err.Error()),
				)
				return
			}
			a.accessCache.Load(orgA, teamA, keyA)
		}),
		startTicker(a.cfg.Cache.AliasTTL, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			orgAliases, teamAliases, err := a.database.LoadAllModelAliases(ctx)
			if err != nil {
				a.log.LogAttrs(context.Background(), slog.LevelError, "alias cache refresh failed",
					slog.String("error", err.Error()),
				)
				return
			}
			a.aliasCache.Load(orgAliases, teamAliases)
		}),
		startTicker(5*time.Minute, func() {
			a.tokenCounter.EvictStale()
		}),
		startTicker(30*time.Second, func() {
			metrics.CacheSize.WithLabelValues("keys").Set(float64(a.keyCache.Len()))
			metrics.CacheSize.WithLabelValues("access").Set(float64(a.accessCache.Len()))
			metrics.CacheSize.WithLabelValues("aliases").Set(float64(a.aliasCache.Len()))
			metrics.CacheSize.WithLabelValues("mcp_servers").Set(float64(a.mcpServerCache.Len()))
			metrics.CacheSize.WithLabelValues("mcp_access").Set(float64(a.mcpAccessCache.Len()))
			metrics.CacheSize.WithLabelValues("mcp_transports").Set(float64(a.mcpTransportCache.Len()))
		}),
		startTicker(30*time.Second, func() {
			ctx1, cancel1 := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel1()
			if servers, err := a.database.LoadAllActiveMCPServers(ctx1); err == nil {
				a.mcpServerCache.LoadAll(servers)
				a.mcpTransportCache.LoadAll(servers)
			} else {
				a.log.LogAttrs(context.Background(), slog.LevelError, "mcp server cache refresh failed",
					slog.String("error", err.Error()),
				)
			}

			ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel2()
			if orgA, teamA, keyA, err := a.database.LoadAllMCPAccess(ctx2); err == nil {
				a.mcpAccessCache.Load(orgA, teamA, keyA)
			} else {
				a.log.LogAttrs(context.Background(), slog.LevelError, "mcp access cache refresh failed",
					slog.String("error", err.Error()),
				)
			}
		}),
	)

	// The in-memory rate limiter accumulates counter entries that must be
	// periodically evicted to reclaim memory. The Redis-backed checker uses
	// TTL-keyed counters that self-expire, so no eviction goroutine is needed.
	if memRL, ok := a.rateLimiter.(*ratelimit.RateLimiter); ok {
		a.stopFuncs = append(a.stopFuncs, startTicker(5*time.Minute, memRL.EvictStale))
	}

	// Start upstream model health monitoring when at least one probe is enabled.
	if a.healthChecker != nil {
		a.stopFuncs = append(a.stopFuncs, a.healthChecker.Start())
	}

	// Register Code Mode pool cleanup. Close must be called after all in-flight
	// executions complete; the LIFO stop order ensures this runs before the
	// admin server is stopped.
	if a.adminHandler.CodePool != nil {
		a.stopFuncs = append(a.stopFuncs, func() {
			a.adminHandler.CodePool.Close()
		})
	}

	// 24h background refresh of tool schemas from upstream MCP servers.
	if a.adminHandler.ToolCache != nil {
		a.stopFuncs = append(a.stopFuncs, startTicker(24*time.Hour, func() {
			refreshCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			if err := a.adminHandler.ToolCache.RefreshAll(refreshCtx); err != nil {
				a.log.WarnContext(refreshCtx, "periodic tool cache refresh", slog.String("error", err.Error()))
			}
		}))
	}

	// Start heartbeat if a license key was configured, even if it has expired.
	// An expired key falls back to community at startup, but the heartbeat can
	// recover by requesting a fresh JWT from the license server.
	if a.rawLicenseKey != "" && !a.devMode {
		stop := license.StartHeartbeat(a.licHolder, a.rawLicenseKey, license.HeartbeatConfig{
			ServerURL: license.DefaultServerURL,
			Interval:  license.DefaultInterval,
			Log:       a.log,
			DB:        a.database,
		})
		a.stopFuncs = append(a.stopFuncs, stop)
	}

	// pprof profiling is enabled in dev mode. Handlers are registered on a
	// dedicated ServeMux so they are never reachable unless dev mode is
	// explicitly enabled. Always bound to localhost — never exposed externally.
	if a.devMode {
		pprofMux := http.NewServeMux()
		pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
		pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		pprofServer := &http.Server{
			Addr:              "localhost:6060",
			Handler:           pprofMux,
			ReadHeaderTimeout: 10 * time.Second,
		}
		a.log.LogAttrs(context.Background(), slog.LevelInfo, "pprof enabled (dev mode)",
			slog.String("addr", "localhost:6060"),
		)
		go func() {
			if err := pprofServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				a.log.LogAttrs(context.Background(), slog.LevelError, "pprof server stopped",
					slog.String("error", err.Error()),
				)
			}
		}()
		a.stopFuncs = append(a.stopFuncs, func() {
			pprofServer.Close() //nolint:errcheck // best-effort on shutdown
		})
	}

	// Populate Swagger spec metadata. Host is intentionally empty so that the
	// Swagger UI uses the current origin — no hard-coded address required.
	docs.SwaggerInfo.Title = "VoidLLM API"
	docs.SwaggerInfo.Description = "Lightweight LLM proxy with org/team/user hierarchy"
	docs.SwaggerInfo.Version = "0.2.0"
	docs.SwaggerInfo.BasePath = "/api/v1"
	docs.SwaggerInfo.Host = ""
	docs.SwaggerInfo.Schemes = []string{"http", "https"}

	a.setupRoutes()
	a.startListening()
	return nil
}

// WaitForShutdown blocks until SIGINT or SIGTERM is received, then performs a
// phased graceful shutdown:
//
//  1. Begin drain — signals load balancers via /readyz to stop sending traffic.
//  2. Wait for in-flight requests to finish (up to DrainTimeout).
//  3. Force-cancel any remaining requests if the timeout expires.
//  4. Stop the Fiber server(s).
//  5. LIFO cleanup: stop tickers, flush usage/audit loggers, close Redis, close DB.
//  6. Zero sensitive key material from memory.
//
// A second signal received while draining triggers an immediate os.Exit(1).
// ctx is reserved for future use and may be context.Background().
func (a *Application) WaitForShutdown(ctx context.Context) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	sig := <-sigCh
	a.log.LogAttrs(ctx, slog.LevelInfo, "shutdown signal received, starting drain",
		slog.String("signal", sig.String()),
	)

	// A second signal bypasses the drain and exits immediately.
	go func() {
		sig := <-sigCh
		a.log.LogAttrs(ctx, slog.LevelWarn, "second signal received, forcing immediate exit (buffered usage events will be lost)",
			slog.String("signal", sig.String()),
		)
		os.Exit(1)
	}()

	// Phase 1: Begin drain — /readyz returns 503 from this point forward so
	// load balancers stop routing new requests to this instance.
	a.shutdownState.BeginDrain()

	// Phase 2: Wait for in-flight requests to finish, logging progress every
	// 5 seconds so operators can monitor drain progress.
	drainDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-drainDone:
				return
			case <-ticker.C:
				a.log.LogAttrs(ctx, slog.LevelInfo, "drain in progress",
					slog.Int64("in_flight", a.shutdownState.InFlight()),
				)
			}
		}
	}()

	drained := a.shutdownState.WaitForDrain(a.cfg.Server.Proxy.DrainTimeout)
	close(drainDone)

	if drained {
		a.log.LogAttrs(ctx, slog.LevelInfo, "all requests drained")
	} else {
		// Phase 3: Force-cancel remaining in-flight requests.
		a.log.LogAttrs(ctx, slog.LevelWarn, "drain timeout exceeded, canceling in-flight requests",
			slog.Int64("in_flight", a.shutdownState.InFlight()),
		)
		a.shutdownState.CancelInflight()
		time.Sleep(500 * time.Millisecond)
	}

	// Phase 4: Stop the Fiber server(s).
	if err := a.proxyApp.Shutdown(); err != nil {
		a.log.LogAttrs(ctx, slog.LevelError, "proxy shutdown error",
			slog.String("error", err.Error()),
		)
	}

	if a.adminApp != nil {
		if err := a.adminApp.Shutdown(); err != nil {
			a.log.LogAttrs(ctx, slog.LevelError, "admin shutdown error",
				slog.String("error", err.Error()),
			)
		}
	}

	// Phase 5: cleanup resources.
	a.cleanup(ctx)

	a.log.LogAttrs(ctx, slog.LevelInfo, "shutdown complete")
}

// PrintBootstrapCredentials writes the bootstrap credentials to stderr when a
// bootstrap was performed during startup. It must be called after Start so that
// the Fiber server banner has already been printed, preventing interleaving.
// If no bootstrap occurred this is a no-op.
//
// Intentional use of fmt.Fprintln instead of slog: the plaintext key and
// password must be shown to the operator on stderr exactly once but must NOT
// go through structured logging where they could be captured by log aggregation
// systems (ELK, Datadog, CloudWatch).
func (a *Application) PrintBootstrapCredentials() {
	if a.bootstrapResult == nil {
		return
	}
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "========================================")
	fmt.Fprintln(os.Stderr, " BOOTSTRAP COMPLETE — COPY THESE NOW")
	fmt.Fprintln(os.Stderr, "========================================")
	fmt.Fprintf(os.Stderr, "  API Key:    %s\n", a.bootstrapResult.APIKey)
	fmt.Fprintf(os.Stderr, "  Email:      %s\n", a.bootstrapResult.Email)
	fmt.Fprintf(os.Stderr, "  Password:   %s\n", a.bootstrapResult.Password)
	fmt.Fprintln(os.Stderr, "========================================")
	fmt.Fprintln(os.Stderr, "")
	a.bootstrapResult = nil
}

// deploymentAAD returns the additional authenticated data used when encrypting
// or decrypting a deployment API key. The AAD binds the ciphertext to the
// specific deployment row so that a ciphertext from one row cannot be replayed
// against a different row.
func deploymentAAD(id string) []byte {
	return []byte("deployment:" + id)
}

// isCodeModeTimeout reports whether a Code Mode execution error message
// indicates the script exceeded its wall-clock time limit.
func isCodeModeTimeout(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "interrupted") || strings.Contains(lower, "timeout")
}

// isCodeModeOOM reports whether a Code Mode execution error message indicates
// the script exceeded its memory limit inside the WASM sandbox.
func isCodeModeOOM(msg string) bool {
	lower := strings.ToLower(msg)
	return strings.Contains(lower, "out of memory") || strings.Contains(lower, "stack overflow")
}

// cleanup tears down resources in reverse startup order: Redis pub/sub,
// background tickers (LIFO), usage/audit loggers, Redis connection, database.
// Sensitive key material is zeroed last, after all components that might still
// need to decrypt data have been stopped. cleanup must be called exactly once,
// by WaitForShutdown.
func (a *Application) cleanup(ctx context.Context) {
	// Cancel Redis pub/sub subscriber before stopping the client.
	a.redisCancel()
	if a.redisClient != nil {
		if err := a.redisClient.Close(); err != nil {
			a.log.LogAttrs(ctx, slog.LevelError, "redis close error",
				slog.String("error", err.Error()),
			)
		}
	}

	// Stop background tickers in LIFO order.
	for i := len(a.stopFuncs) - 1; i >= 0; i-- {
		a.stopFuncs[i]()
	}

	// Flush buffered usage, MCP, and audit events.
	a.usageLogger.Stop()
	if a.mcpLogger != nil {
		a.mcpLogger.Stop()
	}
	if a.auditLogger != nil {
		a.auditLogger.Stop()
	}

	// Flush buffered OTel spans. Use a bounded context so a slow collector
	// cannot block shutdown indefinitely.
	if a.otelShutdown != nil {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := a.otelShutdown(shutdownCtx); err != nil {
			a.log.LogAttrs(ctx, slog.LevelWarn, "otel shutdown error",
				slog.String("error", err.Error()),
			)
		}
		shutdownCancel()
	}

	// Close all persistent MCP transports before closing the database.
	a.mcpTransportCache.Close()

	if err := a.database.Close(); err != nil {
		a.log.LogAttrs(ctx, slog.LevelError, "database close error",
			slog.String("error", err.Error()),
		)
	}

	// Zero sensitive key material after all components are stopped.
	crypto.ZeroKey(a.hmacSecret)
	crypto.ZeroKey(a.encKey)
}
