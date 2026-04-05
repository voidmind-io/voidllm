// Package config handles loading, environment variable interpolation, defaulting,
// and validation of the VoidLLM configuration file (voidllm.yaml).
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration structure for VoidLLM.
type Config struct {
	Server     ServerConfig      `yaml:"server"`
	Database   DatabaseConfig    `yaml:"database"`
	Cache      CacheConfig       `yaml:"cache"`
	Redis      RedisConfig       `yaml:"redis"`
	Models     []ModelConfig     `yaml:"models"`
	MCPServers []MCPServerConfig `yaml:"mcp_servers"`
	Settings   SettingsConfig    `yaml:"settings"`
	Logging    LoggingConfig     `yaml:"logging"`
}

// ServerConfig holds configuration for both the proxy and admin HTTP servers.
type ServerConfig struct {
	Proxy ProxyConfig `yaml:"proxy"`
	Admin AdminConfig `yaml:"admin"`
}

// ProxyConfig holds configuration for the proxy (hot path) HTTP server.
type ProxyConfig struct {
	Port              int           `yaml:"port"`
	ReadTimeout       time.Duration `yaml:"read_timeout"`
	WriteTimeout      time.Duration `yaml:"write_timeout"`
	IdleTimeout       time.Duration `yaml:"idle_timeout"`
	MaxRequestBody    int           `yaml:"max_request_body"`    // bytes, 0 = use default
	MaxResponseBody   int           `yaml:"max_response_body"`   // bytes, 0 = use default
	MaxStreamDuration time.Duration `yaml:"max_stream_duration"` // 0 = use default (5m)
	DrainTimeout      time.Duration `yaml:"drain_timeout"`       // graceful shutdown drain window; default 25s
}

// AdminConfig holds configuration for the admin HTTP server.
type AdminConfig struct {
	Port int       `yaml:"port"`
	TLS  TLSConfig `yaml:"tls"`
}

// TLSConfig holds TLS certificate configuration for the admin server.
type TLSConfig struct {
	Enabled bool   `yaml:"enabled"`
	Cert    string `yaml:"cert"`
	Key     string `yaml:"key"`
}

// DatabaseConfig holds configuration for the primary data store.
type DatabaseConfig struct {
	Driver          string        `yaml:"driver"`
	DSN             string        `yaml:"dsn"`
	MaxOpenConns    int           `yaml:"max_open_conns"`
	MaxIdleConns    int           `yaml:"max_idle_conns"`
	ConnMaxLifetime time.Duration `yaml:"conn_max_lifetime"`
}

// LogValue implements slog.LogValuer to prevent the DSN (which may contain
// credentials) from appearing in logs.
func (d DatabaseConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("driver", d.Driver),
		slog.String("dsn", "[REDACTED]"),
		slog.Int("max_open_conns", d.MaxOpenConns),
		slog.Int("max_idle_conns", d.MaxIdleConns),
	)
}

// CacheConfig holds TTL settings for the in-memory cache.
type CacheConfig struct {
	KeyTTL   time.Duration `yaml:"key_ttl"`
	ModelTTL time.Duration `yaml:"model_ttl"`
	AliasTTL time.Duration `yaml:"alias_ttl"`
}

// RedisConfig holds configuration for the optional Redis integration.
type RedisConfig struct {
	Enabled   bool   `yaml:"enabled"`
	URL       string `yaml:"url"`
	KeyPrefix string `yaml:"key_prefix"`
}

// DeploymentConfig defines a single deployment endpoint within a multi-deployment model.
// When a ModelConfig has one or more deployments, the proxy selects among them using
// the strategy defined on the parent ModelConfig.
type DeploymentConfig struct {
	// Name is a unique identifier for this deployment within the model (required).
	Name string `yaml:"name"`
	// Provider is the upstream provider for this deployment (required).
	Provider string `yaml:"provider"`
	// BaseURL is the base URL for this deployment's API endpoint (required).
	BaseURL string `yaml:"base_url"`
	// APIKey is the API key for this deployment. Redacted in logs.
	APIKey string `yaml:"api_key" json:"-"`
	// AzureDeployment is the Azure deployment name. Required when provider is "azure".
	AzureDeployment string `yaml:"azure_deployment"`
	// AzureAPIVersion is the Azure API version string used in request URLs.
	AzureAPIVersion string `yaml:"azure_api_version"`
	// GCPProject is the Google Cloud project ID. Required when provider is "vertex".
	GCPProject string `yaml:"gcp_project" json:"gcp_project,omitempty"`
	// GCPLocation is the Google Cloud region (e.g. "us-central1"). Required when provider is "vertex".
	GCPLocation string `yaml:"gcp_location" json:"gcp_location,omitempty"`
	// AWSRegion is the AWS region (e.g. "us-east-1"). Required when provider is "bedrock-converse".
	AWSRegion string `yaml:"aws_region" json:"aws_region,omitempty"`
	// AWSAccessKey is the AWS IAM access key ID. Required when provider is "bedrock-converse".
	AWSAccessKey string `yaml:"aws_access_key" json:"-"`
	// AWSSecretKey is the AWS IAM secret access key. Required when provider is "bedrock-converse".
	AWSSecretKey string `yaml:"aws_secret_key" json:"-"`
	// AWSSessionToken is an optional AWS STS session token for temporary credentials.
	AWSSessionToken string `yaml:"aws_session_token" json:"-"`
	// Weight is the relative routing weight for the "weighted" strategy.
	// A value of 0 means this deployment is only used as a fallback when all
	// weighted deployments are unavailable.
	Weight int `yaml:"weight"`
	// Priority is the preference rank for the "priority" strategy. Lower
	// values indicate higher priority.
	Priority int `yaml:"priority"`
}

// LogValue implements slog.LogValuer to prevent API keys and AWS credentials
// from appearing in logs.
func (d DeploymentConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", d.Name),
		slog.String("provider", d.Provider),
		slog.String("base_url", d.BaseURL),
		slog.String("api_key", "[REDACTED]"),
		slog.String("aws_access_key", "[REDACTED]"),
		slog.String("aws_secret_key", "[REDACTED]"),
		slog.String("aws_session_token", "[REDACTED]"),
	)
}

// ModelConfig defines a single model entry in the static model registry.
type ModelConfig struct {
	Name     string `yaml:"name"`
	Provider string `yaml:"provider"`
	// "completion", "image", "audio_transcription", or "tts". Defaults to "chat".
	Type             string        `yaml:"type"`
	BaseURL          string        `yaml:"base_url"`
	APIKey           string        `yaml:"api_key" json:"-"`
	Aliases          []string      `yaml:"aliases"`
	MaxContextTokens int           `yaml:"max_context_tokens"`
	Pricing          PricingConfig `yaml:"pricing"`
	AzureDeployment  string        `yaml:"azure_deployment"`
	AzureAPIVersion  string        `yaml:"azure_api_version"`
	// GCPProject is the Google Cloud project ID. Required when provider is "vertex".
	GCPProject string `yaml:"gcp_project" json:"gcp_project,omitempty"`
	// GCPLocation is the Google Cloud region (e.g. "us-central1"). Required when provider is "vertex".
	GCPLocation string `yaml:"gcp_location" json:"gcp_location,omitempty"`
	// AWSRegion is the AWS region (e.g. "us-east-1"). Required when provider is "bedrock-converse".
	AWSRegion string `yaml:"aws_region" json:"aws_region,omitempty"`
	// AWSAccessKey is the AWS IAM access key ID. Required when provider is "bedrock-converse".
	AWSAccessKey string `yaml:"aws_access_key" json:"-"`
	// AWSSecretKey is the AWS IAM secret access key. Required when provider is "bedrock-converse".
	AWSSecretKey string `yaml:"aws_secret_key" json:"-"`
	// AWSSessionToken is an optional AWS STS session token for temporary credentials.
	AWSSessionToken string `yaml:"aws_session_token" json:"-"`
	// Timeout is the per-model upstream timeout as a duration string (e.g. "30s",
	// "2m"). When non-empty it overrides the global stream/response timeout for
	// this model. Zero or empty means use the global default.
	Timeout string `yaml:"timeout"`
	// Strategy is the deployment selection strategy used when Deployments is
	// non-empty. Valid values: round-robin, least-latency, weighted, priority.
	Strategy string `yaml:"strategy"`
	// MaxRetries is the number of times the proxy will retry a failed upstream
	// request across the available deployments. Must be >= 0.
	MaxRetries int `yaml:"max_retries"`
	// Deployments is the list of backend endpoints for this model. When set,
	// the model-level Provider and BaseURL fields are ignored in favour of the
	// per-deployment values, and Strategy must be set.
	Deployments []DeploymentConfig `yaml:"deployments"`
}

// LogValue implements slog.LogValuer to prevent API keys and AWS credentials
// from appearing in logs.
func (m ModelConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", m.Name),
		slog.String("provider", m.Provider),
		slog.String("base_url", m.BaseURL),
		slog.String("api_key", "[REDACTED]"),
		slog.String("aws_access_key", "[REDACTED]"),
		slog.String("aws_secret_key", "[REDACTED]"),
		slog.String("aws_session_token", "[REDACTED]"),
	)
}

// PricingConfig holds per-million-token pricing for a model.
type PricingConfig struct {
	InputPer1M  float64 `yaml:"input_per_1m"`
	OutputPer1M float64 `yaml:"output_per_1m"`
}

// LoggingConfig controls log output level and format.
type LoggingConfig struct {
	// Level sets the minimum log level. Valid values: debug, info, warn, error.
	Level string `yaml:"level"`
	// Format sets the log output format. Valid values: json (default), text (local dev).
	Format string `yaml:"format"`
}

// BootstrapConfig controls the default org, user, and admin email created on
// first startup when settings.admin_key is set and the database is empty.
type BootstrapConfig struct {
	// OrgName is the display name of the default organization. Defaults to "Default".
	OrgName string `yaml:"org_name"`
	// OrgSlug is the URL-safe slug for the default organization. Defaults to a
	// slug derived from OrgName.
	OrgSlug string `yaml:"org_slug"`
	// AdminEmail is the email address of the initial system-admin user.
	// Defaults to "admin@voidllm.local".
	AdminEmail string `yaml:"admin_email"`
}

// AuditConfig controls the enterprise audit logging subsystem.
type AuditConfig struct {
	// Enabled controls whether audit events are recorded. Defaults to false.
	Enabled bool `yaml:"enabled"`
	// BufferSize sets the capacity of the async event channel. Defaults to 500.
	BufferSize int `yaml:"buffer_size"`
	// FlushInterval is how often buffered events are written to the database.
	// Defaults to 5 seconds.
	FlushInterval time.Duration `yaml:"flush_interval"`
}

// CircuitBreakerConfig holds per-model circuit breaker configuration.
type CircuitBreakerConfig struct {
	// Enabled activates circuit breaker functionality. Defaults to false.
	Enabled bool `yaml:"enabled"`
	// Threshold is the number of consecutive upstream failures required before
	// the circuit opens and starts rejecting requests. Defaults to 5.
	Threshold int `yaml:"threshold"`
	// Timeout is how long the circuit stays open before transitioning to
	// half-open to probe for recovery. Defaults to 30 seconds.
	Timeout time.Duration `yaml:"timeout"`
	// HalfOpenMax is the maximum number of concurrent probe requests allowed
	// while the circuit is in half-open state. Defaults to 1.
	HalfOpenMax int `yaml:"half_open_max"`
}

// OTelConfig holds OpenTelemetry tracing configuration. Tracing is an
// enterprise feature; the Enabled flag is ignored unless a valid enterprise
// license with the otel_tracing feature is present.
type OTelConfig struct {
	// Enabled activates OpenTelemetry trace export. Requires an enterprise
	// license with the otel_tracing feature.
	Enabled bool `yaml:"enabled"`
	// Endpoint is the OTLP/gRPC collector address (host:port). Defaults to
	// "localhost:4317".
	Endpoint string `yaml:"endpoint"`
	// Insecure disables TLS on the gRPC connection to the collector. Suitable
	// for local collectors running without TLS (e.g. Jaeger all-in-one).
	Insecure bool `yaml:"insecure"`
	// SampleRate is the fraction of traces to export, in the range [0.0, 1.0].
	// 1.0 exports all traces; 0.0 exports none. Defaults to 1.0.
	// A pointer is used so that an explicit 0.0 can be distinguished from
	// the zero value after unmarshalling.
	SampleRate *float64 `yaml:"sample_rate"`
}

// SSOConfig holds configuration for OIDC/OAuth2 single sign-on.
// SSO/OIDC is an enterprise feature gated by license.FeatureSSOOIDC.
type SSOConfig struct {
	// Enabled controls whether the OIDC login flow is active.
	Enabled bool `yaml:"enabled"`
	// Issuer is the OIDC provider's issuer URL used for Discovery (e.g. "https://accounts.google.com").
	Issuer string `yaml:"issuer"`
	// ClientID is the OAuth2 client identifier registered with the identity provider.
	ClientID string `yaml:"client_id"`
	// ClientSecret is the OAuth2 client secret. It is redacted in logs.
	ClientSecret string `yaml:"client_secret" json:"-"`
	// RedirectURL is the absolute callback URL registered with the identity provider
	// (e.g. "https://voidllm.example.com/api/v1/auth/oidc/callback").
	RedirectURL string `yaml:"redirect_url"`
	// Scopes is the list of OAuth2 scopes to request. Defaults to ["openid", "email", "profile"].
	Scopes []string `yaml:"scopes"`
	// AllowedDomains restricts login to email addresses belonging to these domains.
	// An empty slice allows any email domain.
	AllowedDomains []string `yaml:"allowed_domains"`
	// AutoProvision controls whether users without a matching DB record are created
	// automatically on first login. When false, unrecognized users are redirected to
	// /login?error=not_provisioned.
	AutoProvision bool `yaml:"auto_provision"`
	// DefaultRole is the RBAC role assigned to auto-provisioned users.
	// Defaults to "member".
	DefaultRole string `yaml:"default_role"`
	// DefaultOrgSlug is the slug of the organization that auto-provisioned users are
	// added to. When empty, the first active organization is used.
	DefaultOrgSlug string `yaml:"default_org_slug"`
	// GroupSync enables automatic team membership synchronization based on the
	// group claim in the ID token. When true, the user's team memberships are
	// updated to match the groups listed in the token on every login.
	GroupSync bool `yaml:"group_sync"`
	// GroupClaim is the ID token claim key that contains the user's group list.
	// Defaults to "groups".
	GroupClaim string `yaml:"group_claim"`
}

// LogValue implements slog.LogValuer to prevent the client secret from appearing in logs.
func (s SSOConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("enabled", s.Enabled),
		slog.String("issuer", s.Issuer),
		slog.String("client_id", s.ClientID),
		slog.String("client_secret", "[REDACTED]"),
		slog.String("redirect_url", s.RedirectURL),
	)
}

// MCPServerConfig defines a single MCP server entry in the static registry.
// Servers declared here are upserted into the database at startup with
// source="yaml". API-created servers (source="api") are never overwritten by
// YAML entries.
type MCPServerConfig struct {
	// Name is the human-readable display name of the MCP server (required).
	Name string `yaml:"name"`
	// Alias is the stable short identifier used in URLs and tool call logs (required).
	// Must be lowercase alphanumeric and hyphens, e.g. "github".
	Alias string `yaml:"alias"`
	// URL is the base endpoint of the MCP server (required). Must start with
	// http:// or https://.
	URL string `yaml:"url"`
	// AuthType controls how VoidLLM authenticates to the upstream server.
	// Valid values: "none", "bearer", "header", "oauth". Defaults to "none" when empty.
	AuthType string `yaml:"auth_type"`
	// AuthHeader is the HTTP header name used when AuthType is "header".
	AuthHeader string `yaml:"auth_header"`
	// AuthToken is the plaintext credential. It is encrypted with AES-256-GCM
	// before being written to the database and is redacted from all logs.
	AuthToken string `yaml:"auth_token" json:"-"`

	// OAuth Client Credentials Flow fields. Required when AuthType is "oauth".
	// OAuthClientSecret is encrypted with AES-256-GCM before storage.
	OAuthTokenURL     string `yaml:"oauth_token_url"`
	OAuthClientID     string `yaml:"oauth_client_id"`
	OAuthClientSecret string `yaml:"oauth_client_secret" json:"-"`
	OAuthScopes       string `yaml:"oauth_scopes"`
}

// LogValue implements slog.LogValuer to prevent the auth token and OAuth client
// secret from appearing in log output.
func (c MCPServerConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", c.Name),
		slog.String("alias", c.Alias),
		slog.String("url", c.URL),
		slog.String("auth_token", "[REDACTED]"),
		slog.String("oauth_client_secret", "[REDACTED]"),
	)
}

// CodeModeConfig controls the sandboxed JavaScript execution runtime used
// by Code Mode to orchestrate multiple MCP tool calls in a single script.
type CodeModeConfig struct {
	// Enabled is the master switch for Code Mode. When false, the execute_code,
	// list_servers, and search_tools MCP tools are not registered.
	// A nil pointer means "not configured" and defaults to false at startup —
	// Code Mode must be explicitly opted in to.
	Enabled *bool `yaml:"enabled"`
	// MemoryLimitMB is the maximum memory (in megabytes) a single Code Mode
	// execution may consume inside the WASM sandbox. Default: 16.
	MemoryLimitMB int `yaml:"memory_limit_mb"`
	// Timeout is the maximum wall-clock duration for a single Code Mode
	// execution, including all tool calls. Default: 30s.
	Timeout time.Duration `yaml:"timeout"`
	// PoolSize is the number of pre-warmed QuickJS runtimes kept in the pool.
	// Each runtime handles one concurrent execution. Default: 4.
	PoolSize int `yaml:"pool_size"`
	// MaxToolCalls is the maximum number of MCP tool calls allowed within a
	// single Code Mode execution. Prevents runaway scripts from making
	// unbounded upstream calls. Default: 50. Valid range: [1, 1000].
	MaxToolCalls int `yaml:"max_tool_calls"`
}

// IsEnabled returns true only when Code Mode has been explicitly enabled via
// configuration. A nil pointer (field absent from YAML) returns false so that
// Code Mode is opt-in rather than on by default.
func (c CodeModeConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return false
	}
	return *c.Enabled
}

// MCPHealthConfig holds configuration for the MCP server health monitoring subsystem.
type MCPHealthConfig struct {
	// Enabled controls whether periodic health probes are sent to registered
	// MCP servers. Defaults to true when not set. A pointer is used so that an
	// explicit "enabled: false" in YAML can be distinguished from the zero value
	// after unmarshalling.
	Enabled *bool `yaml:"enabled"`
	// Interval is how often each registered MCP server is probed via a
	// tools/list JSON-RPC request. Defaults to 60 seconds.
	Interval time.Duration `yaml:"interval"`
}

// MCPConfig holds configuration for the MCP Gateway subsystem.
type MCPConfig struct {
	// CallTimeout is the maximum duration for a single proxied MCP tool call.
	// Defaults to 30 seconds.
	CallTimeout time.Duration `yaml:"call_timeout"`
	// AllowPrivateURLs disables SSRF protection for MCP server URLs, permitting
	// localhost, private IPs, and link-local addresses. Enable this for internal
	// deployments where MCP servers run on the same network. Default: false.
	// This setting is only configurable via YAML/ENV — not via Admin API/UI.
	AllowPrivateURLs bool `yaml:"allow_private_urls"`
	// Health configures periodic health probing for registered MCP servers.
	Health MCPHealthConfig `yaml:"health"`
	// CodeMode holds configuration for the sandboxed JavaScript execution runtime.
	CodeMode CodeModeConfig `yaml:"code_mode"`
}

// HealthCheckConfig holds configuration for the upstream model health monitoring subsystem.
type HealthCheckConfig struct {
	// Health configures the lightweight GET / reachability probe.
	Health HealthProbeConfig `yaml:"health"`
	// Models configures the GET /models API availability probe.
	Models HealthProbeConfig `yaml:"models"`
	// Functional configures the POST /chat/completions end-to-end probe.
	Functional HealthProbeConfig `yaml:"functional"`
}

// HealthProbeConfig holds the enable flag and polling interval for a single
// health probe level.
type HealthProbeConfig struct {
	// Enabled controls whether this probe level is active.
	Enabled bool `yaml:"enabled"`
	// Interval is how often the probe is executed for each registered model.
	Interval time.Duration `yaml:"interval"`
}

// SettingsConfig holds application-level settings.
type SettingsConfig struct {
	AdminKey      string `yaml:"admin_key" json:"-"`
	EncryptionKey string `yaml:"encryption_key" json:"-"`
	License       string `yaml:"license" json:"-"`
	// LicenseFile is the path to a file containing a VoidLLM enterprise
	// license JWT. When set and License is empty, the file contents are read
	// at startup and used as the license key. ${ENV_VAR} interpolation is
	// applied to this field before the file is read.
	LicenseFile    string               `yaml:"license_file" json:"-"`
	Bootstrap      BootstrapConfig      `yaml:"bootstrap"`
	Usage          UsageConfig          `yaml:"usage"`
	Audit          AuditConfig          `yaml:"audit"`
	OTel           OTelConfig           `yaml:"otel"`
	SSO            SSOConfig            `yaml:"sso"`
	TokenCounting  TokenCountingConfig  `yaml:"token_counting"`
	CircuitBreaker CircuitBreakerConfig `yaml:"circuit_breaker"`
	HealthCheck    HealthCheckConfig    `yaml:"health_check"`
	MCP            MCPConfig            `yaml:"mcp"`
	// SoftLimitThreshold uses *float64 so that an explicit 0.0 can be
	// distinguished from the zero value after unmarshalling. Use
	// GetSoftLimitThreshold to read the value.
	SoftLimitThreshold *float64 `yaml:"soft_limit_threshold"`
}

// LogValue implements slog.LogValuer to prevent secrets from appearing in logs.
func (s SettingsConfig) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("admin_key", "[REDACTED]"),
		slog.String("encryption_key", "[REDACTED]"),
		slog.String("license", "[REDACTED]"),
	)
}

// GetSoftLimitThreshold returns the configured threshold, defaulting to 0.9 if not set.
func (s SettingsConfig) GetSoftLimitThreshold() float64 {
	if s.SoftLimitThreshold == nil {
		return 0.9
	}
	return *s.SoftLimitThreshold
}

// UsageConfig holds settings for the async usage logging subsystem.
type UsageConfig struct {
	BufferSize    int           `yaml:"buffer_size"`
	FlushInterval time.Duration `yaml:"flush_interval"`
	// DropOnFull defaults to true. A *bool is used so that an explicit false
	// can be distinguished from the zero value after unmarshalling.
	DropOnFull *bool `yaml:"drop_on_full"`
}

// ShouldDropOnFull returns true when the field is nil (not set) or explicitly true.
func (u UsageConfig) ShouldDropOnFull() bool {
	if u.DropOnFull == nil {
		return true
	}
	return *u.DropOnFull
}

// TokenCountingConfig holds settings for the token counting pre-check subsystem.
type TokenCountingConfig struct {
	// Enabled defaults to true. A *bool is used so that an explicit false
	// can be distinguished from the zero value after unmarshalling.
	Enabled *bool `yaml:"enabled"`
}

// IsEnabled returns true when the field is nil (not set) or explicitly true.
func (t TokenCountingConfig) IsEnabled() bool {
	if t.Enabled == nil {
		return true
	}
	return *t.Enabled
}

// Load reads the configuration file at path, applies environment variable
// interpolation, unmarshals the YAML, applies defaults, and validates the result.
// If path is empty, Load calls findConfigFile to locate the file automatically.
// The second return value is true when no config file was found and defaults
// were used; callers should log this after their logger is initialised.
func Load(path string) (*Config, bool, error) {
	if path == "" {
		var err error
		path, err = findConfigFile()
		if err != nil {
			cfg, defErr := loadDefaults()
			if defErr != nil {
				return nil, false, defErr
			}
			return cfg, true, nil
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false, fmt.Errorf("config: read file %q: %w", path, err)
	}

	raw = interpolateEnv(raw)

	var cfg Config
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, false, fmt.Errorf("config: unmarshal yaml: %w", err)
	}

	cfg.setDefaults()

	// If license_file is set and the inline license key is empty, read the
	// JWT from the file. This allows secrets to be mounted as files (e.g.
	// Kubernetes secrets) without embedding sensitive values in the YAML.
	if cfg.Settings.LicenseFile != "" && cfg.Settings.License == "" {
		licenseBytes, readErr := os.ReadFile(cfg.Settings.LicenseFile)
		if readErr != nil {
			return nil, false, fmt.Errorf("config: read license_file %q: %w", cfg.Settings.LicenseFile, readErr)
		}
		cfg.Settings.License = strings.TrimSpace(string(licenseBytes))
	}

	if err := cfg.validate(); err != nil {
		return nil, false, fmt.Errorf("config: %w", err)
	}

	return &cfg, false, nil
}

// findConfigFile returns the path to the configuration file by checking, in order:
//  1. The VOIDLLM_CONFIG environment variable
//  2. ./voidllm.yaml in the current working directory
//  3. /etc/voidllm/voidllm.yaml
func findConfigFile() (string, error) {
	if v := os.Getenv("VOIDLLM_CONFIG"); v != "" {
		return v, nil
	}

	candidates := []string{
		"./voidllm.yaml",
		"/etc/voidllm/voidllm.yaml",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}

	return "", fmt.Errorf("no config file found; set VOIDLLM_CONFIG or place voidllm.yaml in the current directory")
}

// loadDefaults returns a Config populated entirely from environment
// variables and built-in defaults. It is used when no configuration
// file is found.
func loadDefaults() (*Config, error) {
	var cfg Config
	cfg.Settings.AdminKey = os.Getenv("VOIDLLM_ADMIN_KEY")
	cfg.Settings.EncryptionKey = os.Getenv("VOIDLLM_ENCRYPTION_KEY")
	cfg.Settings.License = os.Getenv("VOIDLLM_LICENSE")
	cfg.Database.DSN = os.Getenv("VOIDLLM_DATABASE_DSN")
	cfg.Database.Driver = os.Getenv("VOIDLLM_DATABASE_DRIVER")
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

// setDefaults populates zero-value fields with their documented defaults.
func (c *Config) setDefaults() {
	// Server proxy
	if c.Server.Proxy.Port == 0 {
		c.Server.Proxy.Port = 8080
	}
	if c.Server.Proxy.ReadTimeout == 0 {
		c.Server.Proxy.ReadTimeout = 30 * time.Second
	}
	if c.Server.Proxy.WriteTimeout == 0 {
		c.Server.Proxy.WriteTimeout = 120 * time.Second
	}
	if c.Server.Proxy.IdleTimeout == 0 {
		c.Server.Proxy.IdleTimeout = 60 * time.Second
	}
	if c.Server.Proxy.MaxRequestBody <= 0 {
		c.Server.Proxy.MaxRequestBody = 20 * 1024 * 1024 // 20 MB
	}
	if c.Server.Proxy.MaxResponseBody <= 0 {
		c.Server.Proxy.MaxResponseBody = 50 * 1024 * 1024 // 50 MB
	}
	if c.Server.Proxy.MaxStreamDuration <= 0 {
		c.Server.Proxy.MaxStreamDuration = 5 * time.Minute
	}
	if c.Server.Proxy.DrainTimeout <= 0 {
		c.Server.Proxy.DrainTimeout = 25 * time.Second
	}

	// Database
	if c.Database.Driver == "" {
		c.Database.Driver = "sqlite"
	}
	if c.Database.DSN == "" && c.Database.Driver == "sqlite" {
		c.Database.DSN = "voidllm.db"
	}
	if c.Database.Driver == "postgres" {
		if c.Database.MaxOpenConns == 0 {
			c.Database.MaxOpenConns = 25
		}
		if c.Database.MaxIdleConns == 0 {
			c.Database.MaxIdleConns = 5
		}
		if c.Database.ConnMaxLifetime == 0 {
			c.Database.ConnMaxLifetime = 5 * time.Minute
		}
	}

	// Cache
	if c.Cache.KeyTTL == 0 {
		c.Cache.KeyTTL = 30 * time.Second
	}
	if c.Cache.ModelTTL == 0 {
		c.Cache.ModelTTL = 60 * time.Second
	}
	if c.Cache.AliasTTL == 0 {
		c.Cache.AliasTTL = 60 * time.Second
	}

	// Redis
	if c.Redis.KeyPrefix == "" {
		c.Redis.KeyPrefix = "voidllm:"
	}

	// Settings usage
	if c.Settings.Usage.BufferSize == 0 {
		c.Settings.Usage.BufferSize = 1000
	}
	if c.Settings.Usage.FlushInterval == 0 {
		c.Settings.Usage.FlushInterval = 5 * time.Second
	}

	// Settings audit
	if c.Settings.Audit.BufferSize == 0 {
		c.Settings.Audit.BufferSize = 500
	}
	if c.Settings.Audit.FlushInterval == 0 {
		c.Settings.Audit.FlushInterval = 5 * time.Second
	}

	// Bootstrap
	if c.Settings.Bootstrap.OrgName == "" {
		c.Settings.Bootstrap.OrgName = "Default"
	}
	if c.Settings.Bootstrap.OrgSlug == "" {
		c.Settings.Bootstrap.OrgSlug = deriveSlug(c.Settings.Bootstrap.OrgName)
	}
	if c.Settings.Bootstrap.AdminEmail == "" {
		c.Settings.Bootstrap.AdminEmail = "admin@voidllm.local"
	}

	// OTel
	if c.Settings.OTel.Endpoint == "" {
		c.Settings.OTel.Endpoint = "localhost:4317"
	}
	if c.Settings.OTel.SampleRate == nil {
		v := 1.0
		c.Settings.OTel.SampleRate = &v
	}

	// Circuit breaker
	if c.Settings.CircuitBreaker.Threshold == 0 {
		c.Settings.CircuitBreaker.Threshold = 5
	}
	if c.Settings.CircuitBreaker.Timeout == 0 {
		c.Settings.CircuitBreaker.Timeout = 30 * time.Second
	}
	if c.Settings.CircuitBreaker.HalfOpenMax == 0 {
		c.Settings.CircuitBreaker.HalfOpenMax = 1
	}

	// SSO defaults
	if len(c.Settings.SSO.Scopes) == 0 {
		c.Settings.SSO.Scopes = []string{"openid", "email", "profile"}
	}
	if c.Settings.SSO.DefaultRole == "" {
		c.Settings.SSO.DefaultRole = "member"
	}
	if c.Settings.SSO.GroupClaim == "" {
		c.Settings.SSO.GroupClaim = "groups"
	}

	// Health check — only set interval defaults when the probe is explicitly
	// enabled; never auto-enable a probe that the user has not opted into.
	if c.Settings.HealthCheck.Health.Enabled && c.Settings.HealthCheck.Health.Interval == 0 {
		c.Settings.HealthCheck.Health.Interval = 30 * time.Second
	}
	if c.Settings.HealthCheck.Models.Enabled && c.Settings.HealthCheck.Models.Interval == 0 {
		c.Settings.HealthCheck.Models.Interval = 60 * time.Second
	}
	if c.Settings.HealthCheck.Functional.Enabled && c.Settings.HealthCheck.Functional.Interval == 0 {
		c.Settings.HealthCheck.Functional.Interval = 5 * time.Minute
	}

	// Enforce minimum polling intervals to prevent accidental DoS of upstreams.
	if c.Settings.HealthCheck.Health.Enabled && c.Settings.HealthCheck.Health.Interval < 10*time.Second {
		c.Settings.HealthCheck.Health.Interval = 10 * time.Second
	}
	if c.Settings.HealthCheck.Models.Enabled && c.Settings.HealthCheck.Models.Interval < 10*time.Second {
		c.Settings.HealthCheck.Models.Interval = 10 * time.Second
	}
	if c.Settings.HealthCheck.Functional.Enabled && c.Settings.HealthCheck.Functional.Interval < 60*time.Second {
		c.Settings.HealthCheck.Functional.Interval = 60 * time.Second
	}

	// MCP Gateway
	if c.Settings.MCP.CallTimeout == 0 {
		c.Settings.MCP.CallTimeout = 30 * time.Second
	}

	// MCP Health — default on with a 60-second probe interval. The pointer
	// allows "enabled: false" in YAML to be respected; a nil pointer means the
	// field was not set, which defaults to enabled.
	if c.Settings.MCP.Health.Enabled == nil {
		v := true
		c.Settings.MCP.Health.Enabled = &v
	}
	if c.Settings.MCP.Health.Interval == 0 {
		c.Settings.MCP.Health.Interval = 60 * time.Second
	}

	// MCP Code Mode — only apply sub-field defaults when Code Mode is explicitly
	// enabled. IsEnabled returns false for a nil pointer so Code Mode is opt-in.
	if c.Settings.MCP.CodeMode.IsEnabled() {
		if c.Settings.MCP.CodeMode.MemoryLimitMB == 0 {
			c.Settings.MCP.CodeMode.MemoryLimitMB = 16
		}
		if c.Settings.MCP.CodeMode.Timeout == 0 {
			c.Settings.MCP.CodeMode.Timeout = 30 * time.Second
		}
		if c.Settings.MCP.CodeMode.PoolSize == 0 {
			c.Settings.MCP.CodeMode.PoolSize = 8
		}
		if c.Settings.MCP.CodeMode.MaxToolCalls == 0 {
			c.Settings.MCP.CodeMode.MaxToolCalls = 50
		}
	}

	// Logging
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
	if c.Logging.Format == "" {
		c.Logging.Format = "json"
	}
}

// deriveSlug converts a display name to a URL-safe slug. It lowercases the
// input, replaces spaces with hyphens, strips any character that is not
// a-z, 0-9, or a hyphen, and trims leading and trailing hyphens. If the
// result is empty, "default" is returned.
func deriveSlug(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "-")
	var buf strings.Builder
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			buf.WriteRune(c)
		}
	}
	slug := strings.Trim(buf.String(), "-")
	if slug == "" {
		return "default"
	}
	return slug
}
