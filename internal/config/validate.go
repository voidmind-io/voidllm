package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/voidmind-io/voidllm/internal/provider"
)

// mcpAliasRe matches a valid MCP server alias: lowercase alphanumeric
// characters and hyphens, starting with an alphanumeric character.
var mcpAliasRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// validMCPAuthTypes is the set of accepted MCP server auth type values.
var validMCPAuthTypes = map[string]bool{
	"none":   true,
	"bearer": true,
	"header": true,
	"oauth":  true,
}

// validModelTypes is the set of accepted model type values in the YAML config.
// An empty string is also valid and resolves to "chat" at sync time.
var validModelTypes = map[string]bool{
	"":                    true,
	"chat":                true,
	"embedding":           true,
	"reranking":           true,
	"completion":          true,
	"image":               true,
	"audio_transcription": true,
	"tts":                 true,
}

// validStrategies is the set of accepted multi-deployment routing strategies.
var validStrategies = map[string]bool{
	"round-robin":   true,
	"least-latency": true,
	"weighted":      true,
	"priority":      true,
}

// validate checks all fields in the configuration for correctness. All
// validation errors are collected and returned as a single joined error so
// the caller can see every problem at once.
func (c *Config) validate() error {
	var errs []error

	// --- server.proxy.port ---
	if c.Server.Proxy.Port < 1 || c.Server.Proxy.Port > 65535 {
		errs = append(errs, fmt.Errorf("server.proxy.port: must be between 1 and 65535, got %d", c.Server.Proxy.Port))
	}

	// --- server.proxy.max_request_body ---
	if c.Server.Proxy.MaxRequestBody > 100*1024*1024 {
		errs = append(errs, fmt.Errorf("server.proxy.max_request_body: must not exceed 100 MB"))
	}

	// --- server.proxy.max_response_body ---
	if c.Server.Proxy.MaxResponseBody > 500*1024*1024 {
		errs = append(errs, fmt.Errorf("server.proxy.max_response_body: must not exceed 500 MB"))
	}

	// --- server.proxy.max_stream_duration ---
	if c.Server.Proxy.MaxStreamDuration > time.Hour {
		errs = append(errs, fmt.Errorf("server.proxy.max_stream_duration: must not exceed 1 hour"))
	}
	if c.Server.Proxy.MaxStreamDuration > 0 && c.Server.Proxy.MaxStreamDuration < 10*time.Second {
		errs = append(errs, fmt.Errorf("server.proxy.max_stream_duration: must be at least 10 seconds"))
	}

	// --- server.proxy.drain_timeout ---
	if c.Server.Proxy.DrainTimeout > 120*time.Second {
		errs = append(errs, fmt.Errorf("server.proxy.drain_timeout: must not exceed 120s"))
	}
	if c.Server.Proxy.DrainTimeout > 0 && c.Server.Proxy.DrainTimeout < 5*time.Second {
		errs = append(errs, fmt.Errorf("server.proxy.drain_timeout: must be at least 5s"))
	}

	// --- server.admin.port ---
	if c.Server.Admin.Port != 0 && (c.Server.Admin.Port < 1 || c.Server.Admin.Port > 65535) {
		errs = append(errs, fmt.Errorf("server.admin.port: must be 0 or between 1 and 65535, got %d", c.Server.Admin.Port))
	}

	// --- server.admin.tls ---
	if c.Server.Admin.TLS.Enabled {
		if c.Server.Admin.TLS.Cert == "" {
			errs = append(errs, fmt.Errorf("server.admin.tls.cert: must not be empty when tls is enabled"))
		}
		if c.Server.Admin.TLS.Key == "" {
			errs = append(errs, fmt.Errorf("server.admin.tls.key: must not be empty when tls is enabled"))
		}
	}

	// --- database.driver ---
	if c.Database.Driver != "sqlite" && c.Database.Driver != "postgres" {
		errs = append(errs, fmt.Errorf("database.driver: must be \"sqlite\" or \"postgres\", got %q", c.Database.Driver))
	}

	// --- database.dsn ---
	// SQLite gets a default DSN ("voidllm.db") in setDefaults, so an empty DSN
	// here only happens for postgres where the caller must supply a value.
	if c.Database.Driver == "postgres" && c.Database.DSN == "" {
		errs = append(errs, fmt.Errorf("database.dsn: required for postgres driver"))
	}

	// --- models ---
	seenNames := make(map[string]bool)
	seenAliases := make(map[string]string) // alias → model name

	for i, m := range c.Models {
		prefix := fmt.Sprintf("models[%d]", i)

		if m.Name == "" {
			errs = append(errs, fmt.Errorf("%s.name: must not be empty", prefix))
		} else {
			if seenNames[m.Name] {
				errs = append(errs, fmt.Errorf("%s.name: duplicate model name %q", prefix, m.Name))
			}
			seenNames[m.Name] = true
		}

		if !validModelTypes[m.Type] {
			errs = append(errs, fmt.Errorf("%s.type: must be one of chat, embedding, reranking, completion, image, audio_transcription, tts; got %q", prefix, m.Type))
		}

		if m.MaxRetries < 0 {
			errs = append(errs, fmt.Errorf("%s.max_retries: must be >= 0, got %d", prefix, m.MaxRetries))
		}

		if len(m.Deployments) == 0 {
			// Single-deployment model: provider, base_url, and azure checks apply directly.
			if !provider.ValidProviders[m.Provider] {
				errs = append(errs, fmt.Errorf("%s.provider: must be one of %v, got %q", prefix, provider.Names(), m.Provider))
			}

			if m.BaseURL == "" {
				errs = append(errs, fmt.Errorf("%s.base_url: must not be empty", prefix))
			} else if !strings.HasPrefix(m.BaseURL, "http://") && !strings.HasPrefix(m.BaseURL, "https://") {
				errs = append(errs, fmt.Errorf("%s.base_url: must start with http:// or https://", prefix))
			}

			if m.Provider == "azure" && m.AzureDeployment == "" {
				errs = append(errs, fmt.Errorf("%s.azure_deployment: must not be empty for azure provider", prefix))
			}

			if m.Provider == "vertex" {
				if m.GCPProject == "" {
					errs = append(errs, fmt.Errorf("%s.gcp_project: must not be empty for vertex provider", prefix))
				}
				if m.GCPLocation == "" {
					errs = append(errs, fmt.Errorf("%s.gcp_location: must not be empty for vertex provider", prefix))
				}
			}

			if m.Strategy != "" {
				errs = append(errs, fmt.Errorf("%s.strategy: must be empty when deployments is not set", prefix))
			}
		} else {
			// Multi-deployment model: strategy is required; per-deployment fields are validated below.
			if !validStrategies[m.Strategy] {
				errs = append(errs, fmt.Errorf("%s.strategy: must be one of round-robin, least-latency, weighted, priority; got %q", prefix, m.Strategy))
			}

			seenDeploymentNames := make(map[string]bool)
			for j, d := range m.Deployments {
				dprefix := fmt.Sprintf("%s.deployments[%d]", prefix, j)

				if d.Name == "" {
					errs = append(errs, fmt.Errorf("%s.name: must not be empty", dprefix))
				} else {
					if seenDeploymentNames[d.Name] {
						errs = append(errs, fmt.Errorf("%s.name: duplicate deployment name %q within model %q", dprefix, d.Name, m.Name))
					}
					seenDeploymentNames[d.Name] = true
				}

				if !provider.ValidProviders[d.Provider] {
					errs = append(errs, fmt.Errorf("%s.provider: must be one of %v, got %q", dprefix, provider.Names(), d.Provider))
				}

				if d.BaseURL == "" {
					errs = append(errs, fmt.Errorf("%s.base_url: must not be empty", dprefix))
				} else if !strings.HasPrefix(d.BaseURL, "http://") && !strings.HasPrefix(d.BaseURL, "https://") {
					errs = append(errs, fmt.Errorf("%s.base_url: must start with http:// or https://", dprefix))
				}

				if d.Provider == "azure" && d.AzureDeployment == "" {
					errs = append(errs, fmt.Errorf("%s.azure_deployment: must not be empty for azure provider", dprefix))
				}

				if d.Provider == "vertex" {
					if d.GCPProject == "" {
						errs = append(errs, fmt.Errorf("%s.gcp_project: must not be empty for vertex provider", dprefix))
					}
					if d.GCPLocation == "" {
						errs = append(errs, fmt.Errorf("%s.gcp_location: must not be empty for vertex provider", dprefix))
					}
				}

				if d.Weight < 0 {
					errs = append(errs, fmt.Errorf("%s.weight: must be >= 0, got %d", dprefix, d.Weight))
				}

				if d.Priority < 0 {
					errs = append(errs, fmt.Errorf("%s.priority: must be >= 0, got %d", dprefix, d.Priority))
				}
			}
		}

		for _, alias := range m.Aliases {
			if _, nameExists := seenNames[alias]; nameExists {
				errs = append(errs, fmt.Errorf("%s.aliases: alias %q collides with model name", prefix, alias))
			} else if owner, exists := seenAliases[alias]; exists {
				errs = append(errs, fmt.Errorf("%s.aliases: duplicate alias %q already used by model %q", prefix, alias, owner))
			} else {
				seenAliases[alias] = m.Name
			}
		}
	}

	// --- mcp_servers ---
	seenMCPAliases := make(map[string]bool)
	for i, s := range c.MCPServers {
		prefix := fmt.Sprintf("mcp_servers[%d]", i)

		if s.Name == "" {
			errs = append(errs, fmt.Errorf("%s.name: must not be empty", prefix))
		}

		if s.Alias == "" {
			errs = append(errs, fmt.Errorf("%s.alias: must not be empty", prefix))
		} else if s.Alias == "voidllm" {
			errs = append(errs, fmt.Errorf(`%s.alias: "voidllm" is reserved`, prefix))
		} else if !mcpAliasRe.MatchString(s.Alias) {
			errs = append(errs, fmt.Errorf("%s.alias: must contain only lowercase alphanumeric characters and hyphens, and must start with an alphanumeric character", prefix))
		} else if seenMCPAliases[s.Alias] {
			errs = append(errs, fmt.Errorf("%s.alias: duplicate alias %q", prefix, s.Alias))
		} else {
			seenMCPAliases[s.Alias] = true
		}

		if s.URL == "" {
			errs = append(errs, fmt.Errorf("%s.url: must not be empty", prefix))
		} else if !strings.HasPrefix(s.URL, "http://") && !strings.HasPrefix(s.URL, "https://") {
			errs = append(errs, fmt.Errorf("%s.url: must start with http:// or https://", prefix))
		}

		authType := s.AuthType
		if authType == "" {
			authType = "none"
		}
		if !validMCPAuthTypes[authType] {
			errs = append(errs, fmt.Errorf(`%s.auth_type: must be one of "none", "bearer", "header", "oauth"; got %q`, prefix, s.AuthType))
		}
		if authType == "header" && s.AuthHeader == "" {
			errs = append(errs, fmt.Errorf(`%s.auth_header: must not be empty when auth_type is "header"`, prefix))
		}
		if authType == "oauth" {
			if s.OAuthTokenURL != "" && !strings.HasPrefix(s.OAuthTokenURL, "https://") {
				errs = append(errs, fmt.Errorf(`%s.oauth_token_url: must use HTTPS`, prefix))
			}
			if s.OAuthClientID == "" {
				errs = append(errs, fmt.Errorf(`%s.oauth_client_id: must not be empty when auth_type is "oauth"`, prefix))
			}
			if s.OAuthClientSecret == "" {
				errs = append(errs, fmt.Errorf(`%s.oauth_client_secret: must not be empty when auth_type is "oauth"`, prefix))
			}
		}
	}

	// --- settings.mcp.code_mode ---
	if c.Settings.MCP.CodeMode.IsEnabled() {
		if c.Settings.MCP.CodeMode.MemoryLimitMB < 1 || c.Settings.MCP.CodeMode.MemoryLimitMB > 128 {
			errs = append(errs, fmt.Errorf("settings.mcp.code_mode.memory_limit_mb: must be between 1 and 128, got %d", c.Settings.MCP.CodeMode.MemoryLimitMB))
		}
		if c.Settings.MCP.CodeMode.Timeout < time.Second || c.Settings.MCP.CodeMode.Timeout > 120*time.Second {
			errs = append(errs, fmt.Errorf("settings.mcp.code_mode.timeout: must be between 1s and 120s, got %s", c.Settings.MCP.CodeMode.Timeout))
		}
		if c.Settings.MCP.CodeMode.PoolSize < 1 || c.Settings.MCP.CodeMode.PoolSize > 32 {
			errs = append(errs, fmt.Errorf("settings.mcp.code_mode.pool_size: must be between 1 and 32, got %d", c.Settings.MCP.CodeMode.PoolSize))
		}
		if c.Settings.MCP.CodeMode.MaxToolCalls < 1 || c.Settings.MCP.CodeMode.MaxToolCalls > 1000 {
			errs = append(errs, fmt.Errorf("settings.mcp.code_mode.max_tool_calls: must be between 1 and 1000, got %d", c.Settings.MCP.CodeMode.MaxToolCalls))
		}
	}

	// --- settings.bootstrap.admin_email ---
	if c.Settings.Bootstrap.AdminEmail != "" && !strings.Contains(c.Settings.Bootstrap.AdminEmail, "@") {
		errs = append(errs, fmt.Errorf("settings.bootstrap.admin_email: invalid email format"))
	}

	// --- settings.encryption_key ---
	if c.Settings.EncryptionKey == "" {
		errs = append(errs, fmt.Errorf("settings.encryption_key: must not be empty"))
	}

	// --- settings.usage.buffer_size ---
	if c.Settings.Usage.BufferSize <= 0 {
		errs = append(errs, fmt.Errorf("settings.usage.buffer_size: must be greater than 0, got %d", c.Settings.Usage.BufferSize))
	}

	// --- settings.soft_limit_threshold ---
	if t := c.Settings.GetSoftLimitThreshold(); t < 0.0 || t > 1.0 {
		errs = append(errs, fmt.Errorf("settings.soft_limit_threshold: must be between 0.0 and 1.0, got %g", t))
	}

	// --- settings.fallback_max_depth ---
	// 0 means "disabled" (no fallback hops); positive values in [1, 10] set the
	// maximum chain depth. Negative values are promoted to 3 in setDefaults, so
	// validation only needs to reject values that slipped through unchanged.
	if c.Settings.FallbackMaxDepth < 0 || c.Settings.FallbackMaxDepth > 10 {
		errs = append(errs, errors.New("settings.fallback_max_depth must be in [0, 10] (0 = disabled)"))
	}

	// Per-model fallback validation: target exists, no self-loop, no cycles
	modelByName := make(map[string]*ModelConfig, len(c.Models))
	for i := range c.Models {
		modelByName[c.Models[i].Name] = &c.Models[i]
		for _, alias := range c.Models[i].Aliases {
			modelByName[alias] = &c.Models[i]
		}
	}
	for i := range c.Models {
		m := &c.Models[i]
		if m.Fallback == "" {
			continue
		}
		if m.Fallback == m.Name {
			errs = append(errs, fmt.Errorf("model %q: fallback cannot reference itself", m.Name))
			continue
		}
		target, ok := modelByName[m.Fallback]
		if !ok {
			errs = append(errs, fmt.Errorf("model %q: fallback target %q not found", m.Name, m.Fallback))
			continue
		}
		// Walk the chain looking for a cycle back to m
		visited := map[string]bool{m.Name: true}
		curr := target
		for curr != nil {
			if visited[curr.Name] {
				errs = append(errs, fmt.Errorf("model %q: fallback chain forms a cycle through %q", m.Name, curr.Name))
				break
			}
			visited[curr.Name] = true
			if curr.Fallback == "" {
				break
			}
			next, ok := modelByName[curr.Fallback]
			if !ok {
				break // unreachable target gets caught on its own iteration
			}
			curr = next
		}
	}

	// --- settings.retention ---
	const maxRetention = 10 * 365 * 24 * time.Hour // 10 years
	if c.Settings.Retention.UsageEvents < 0 {
		errs = append(errs, errors.New("settings.retention.usage_events must be >= 0"))
	}
	if c.Settings.Retention.AuditLogs < 0 {
		errs = append(errs, errors.New("settings.retention.audit_logs must be >= 0"))
	}
	if c.Settings.Retention.UsageEvents > maxRetention {
		errs = append(errs, errors.New("settings.retention.usage_events exceeds maximum (10 years)"))
	}
	if c.Settings.Retention.AuditLogs > maxRetention {
		errs = append(errs, errors.New("settings.retention.audit_logs exceeds maximum (10 years)"))
	}
	const minRetentionInterval = 1 * time.Minute
	if c.Settings.Retention.Enabled() && c.Settings.Retention.Interval < minRetentionInterval {
		errs = append(errs, fmt.Errorf("settings.retention.interval must be >= %s when retention is enabled", minRetentionInterval))
	}

	// --- settings.sso.default_role ---
	if c.Settings.SSO.Enabled {
		switch c.Settings.SSO.DefaultRole {
		case "member", "team_admin":
			// allowed for SSO auto-provisioning
		default:
			errs = append(errs, fmt.Errorf("sso.default_role must be 'member' or 'team_admin', got %q", c.Settings.SSO.DefaultRole))
		}
	}

	// --- logging.level ---
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[c.Logging.Level] {
		errs = append(errs, fmt.Errorf("logging.level: must be one of debug|info|warn|error, got %q", c.Logging.Level))
	}

	// --- logging.format ---
	validFormats := map[string]bool{"json": true, "text": true}
	if !validFormats[c.Logging.Format] {
		errs = append(errs, fmt.Errorf("logging.format: must be json or text, got %q", c.Logging.Format))
	}

	return errors.Join(errs...)
}
