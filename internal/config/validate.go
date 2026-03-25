package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/voidmind-io/voidllm/internal/provider"
)

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

		if !provider.ValidProviders[m.Provider] {
			errs = append(errs, fmt.Errorf("%s.provider: must be one of %v, got %q", prefix, provider.Names(), m.Provider))
		}

		if !validModelTypes[m.Type] {
			errs = append(errs, fmt.Errorf("%s.type: must be one of chat, embedding, reranking, completion, image, audio_transcription, tts; got %q", prefix, m.Type))
		}

		if m.BaseURL == "" {
			errs = append(errs, fmt.Errorf("%s.base_url: must not be empty", prefix))
		} else if !strings.HasPrefix(m.BaseURL, "http://") && !strings.HasPrefix(m.BaseURL, "https://") {
			errs = append(errs, fmt.Errorf("%s.base_url: must start with http:// or https://", prefix))
		}

		if m.Provider == "azure" && m.AzureDeployment == "" {
			errs = append(errs, fmt.Errorf("%s.azure_deployment: must not be empty for azure provider", prefix))
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
