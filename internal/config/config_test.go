package config_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
)

// minimalValidYAML returns a YAML string that satisfies all validation
// rules. Individual tests override specific fields to trigger errors.
func minimalValidYAML() string {
	return `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: aaaaaaaaaaaaaaaa
  usage:
    buffer_size: 100
`
}

// writeTemp writes content to a file inside t.TempDir() and returns its path.
func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeTemp: %v", err)
	}
	return path
}

// ---- interpolateEnv --------------------------------------------------------
// interpolateEnv is unexported so we exercise it through Load.
// We embed env-var references inside the api_key field of a model entry
// (which has no validation constraint) so the YAML remains otherwise valid.

func modelWithAPIKey(expr string) string {
	return fmt.Sprintf(`
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: aaaaaaaaaaaaaaaa
  usage:
    buffer_size: 100
models:
  - name: gpt-4o
    provider: openai
    base_url: https://api.openai.com
    api_key: "%s"
`, expr)
}

func TestInterpolateEnv(t *testing.T) {
	// t.Setenv is used in subtests; parallel is not allowed with t.Setenv.

	tests := []struct {
		name    string
		expr    string // the expression placed in api_key
		envKey  string // env var to set (empty = don't set)
		envVal  string // value to set
		wantKey string // expected resolved api_key value
	}{
		{
			name:    "set var replaced",
			expr:    "${MY_API_KEY}",
			envKey:  "MY_API_KEY",
			envVal:  "sk-live-1234",
			wantKey: "sk-live-1234",
		},
		{
			name:    "unset var replaced with empty string",
			expr:    "${UNSET_VAR_XYZ}",
			envKey:  "",
			wantKey: "",
		},
		{
			name:    "fallback syntax with set var uses var value",
			expr:    "${MY_KEY2:-defaultval}",
			envKey:  "MY_KEY2",
			envVal:  "actual",
			wantKey: "actual",
		},
		{
			name:    "fallback syntax with unset var uses fallback",
			expr:    "${MISSING_KEY_ABC:-fallback}",
			envKey:  "",
			wantKey: "fallback",
		},
		{
			name:    "fallback syntax with empty var uses fallback",
			expr:    "${EMPTY_KEY_ABC:-fallback}",
			envKey:  "EMPTY_KEY_ABC",
			envVal:  "",
			wantKey: "fallback",
		},
		{
			name:    "no vars in string unchanged",
			expr:    "literal-key-value",
			envKey:  "",
			wantKey: "literal-key-value",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Cannot use t.Parallel() here because t.Setenv requires sequential execution.
			if tc.envKey != "" {
				t.Setenv(tc.envKey, tc.envVal)
			}

			path := writeTemp(t, "voidllm.yaml", modelWithAPIKey(tc.expr))
			cfg, _, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if len(cfg.Models) == 0 {
				t.Fatal("expected at least one model")
			}
			if got := cfg.Models[0].APIKey; got != tc.wantKey {
				t.Errorf("api_key = %q, want %q", got, tc.wantKey)
			}
		})
	}
}

func TestInterpolateEnvMultipleVars(t *testing.T) {
	// t.Setenv requires sequential execution; no t.Parallel() here.
	t.Setenv("HOST_PART", "api.openai.com")
	t.Setenv("SCHEME_PART", "https")

	yaml := `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: aaaaaaaaaaaaaaaa
  usage:
    buffer_size: 100
models:
  - name: gpt-4o
    provider: openai
    base_url: "${SCHEME_PART}://${HOST_PART}"
`
	path := writeTemp(t, "voidllm.yaml", yaml)
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	want := "https://api.openai.com"
	if got := cfg.Models[0].BaseURL; got != want {
		t.Errorf("base_url = %q, want %q", got, want)
	}
}

// ---- setDefaults (exercised via Load with minimal config) ------------------

func TestSetDefaults(t *testing.T) {
	t.Parallel()

	// Load a config that sets nothing except the required fields so that
	// every default-filling branch is exercised.
	path := writeTemp(t, "voidllm.yaml", minimalValidYAML())
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	tests := []struct {
		name string
		got  any
		want any
	}{
		{"proxy port", cfg.Server.Proxy.Port, 8080},
		{"proxy read timeout", cfg.Server.Proxy.ReadTimeout, 30 * time.Second},
		{"proxy write timeout", cfg.Server.Proxy.WriteTimeout, 120 * time.Second},
		{"proxy idle timeout", cfg.Server.Proxy.IdleTimeout, 60 * time.Second},
		{"database driver", cfg.Database.Driver, "sqlite"},
		{"database dsn", cfg.Database.DSN, "voidllm.db"},
		// Pool defaults are only applied for postgres; sqlite pins to 1 conn in db.Open.
		{"database max open conns", cfg.Database.MaxOpenConns, 0},
		{"database max idle conns", cfg.Database.MaxIdleConns, 0},
		{"database conn max lifetime", cfg.Database.ConnMaxLifetime, time.Duration(0)},
		{"cache key ttl", cfg.Cache.KeyTTL, 30 * time.Second},
		{"cache model ttl", cfg.Cache.ModelTTL, 60 * time.Second},
		{"cache alias ttl", cfg.Cache.AliasTTL, 60 * time.Second},
		{"redis key prefix", cfg.Redis.KeyPrefix, "voidllm:"},
		{"usage buffer size", cfg.Settings.Usage.BufferSize, 100}, // set explicitly in minimal YAML
		{"usage flush interval", cfg.Settings.Usage.FlushInterval, 5 * time.Second},
		{"soft limit threshold", cfg.Settings.GetSoftLimitThreshold(), 0.9},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.got != tc.want {
				t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
			}
		})
	}
}

func TestSetDefaults_DropOnFullNilIsTrue(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, "voidllm.yaml", minimalValidYAML())
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if !cfg.Settings.Usage.ShouldDropOnFull() {
		t.Error("ShouldDropOnFull() = false when drop_on_full not set, want true")
	}
}

func TestSetDefaults_TokenCountingNilIsEnabled(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, "voidllm.yaml", minimalValidYAML())
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if !cfg.Settings.TokenCounting.IsEnabled() {
		t.Error("IsEnabled() = false when enabled not set, want true")
	}
}

func TestSetDefaults_ExplicitDropOnFullFalse(t *testing.T) {
	t.Parallel()

	yaml := `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: aaaaaaaaaaaaaaaa
  usage:
    buffer_size: 50
    drop_on_full: false
`
	path := writeTemp(t, "voidllm.yaml", yaml)
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.Settings.Usage.ShouldDropOnFull() {
		t.Error("ShouldDropOnFull() = true, want false when drop_on_full: false is set")
	}
}

// ---- validate --------------------------------------------------------------

func TestValidate(t *testing.T) {
	t.Parallel()

	// baseModel is a valid model entry reused across cases.
	validModel := `
  - name: gpt-4o
    provider: openai
    base_url: https://api.openai.com
`

	tests := []struct {
		name        string
		yaml        string
		wantErr     bool
		errContains string
	}{
		{
			name:    "valid config no error",
			yaml:    minimalValidYAML(),
			wantErr: false,
		},
		{
			// Port 0 is filled by setDefaults to 8080 before validation runs,
			// so it never produces an error via Load. A negative port (-1)
			// bypasses the default (only 0 is defaulted) and triggers the error.
			name: "proxy port negative error",
			yaml: `
server:
  proxy:
    port: -1
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
`,
			wantErr:     true,
			errContains: "server.proxy.port",
		},
		{
			name: "proxy port 65536 error",
			yaml: `
server:
  proxy:
    port: 65536
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
`,
			wantErr:     true,
			errContains: "server.proxy.port",
		},
		{
			name: "admin port 65536 error",
			yaml: `
server:
  proxy:
    port: 8080
  admin:
    port: 65536
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
`,
			wantErr:     true,
			errContains: "server.admin.port",
		},
		{
			name: "admin port 0 is ok",
			yaml: `
server:
  proxy:
    port: 8080
  admin:
    port: 0
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
`,
			wantErr: false,
		},
		{
			name: "TLS enabled empty cert error",
			yaml: `
server:
  proxy:
    port: 8080
  admin:
    tls:
      enabled: true
      key: /path/to/key.pem
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
`,
			wantErr:     true,
			errContains: "server.admin.tls.cert",
		},
		{
			name: "TLS enabled empty key error",
			yaml: `
server:
  proxy:
    port: 8080
  admin:
    tls:
      enabled: true
      cert: /path/to/cert.pem
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
`,
			wantErr:     true,
			errContains: "server.admin.tls.key",
		},
		{
			name: "database driver mysql error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: mysql
  dsn: root@tcp(localhost)/voidllm
settings:
  encryption_key: key
  usage:
    buffer_size: 100
`,
			wantErr:     true,
			errContains: "database.driver",
		},
		{
			// An empty DSN in YAML is filled by setDefaults to "voidllm.db"
			// before validation runs, so the empty-DSN validation branch is
			// unreachable via Load(). Verify that the default is applied and
			// no error is returned.
			name: "database dsn empty gets defaulted no error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: ""
settings:
  encryption_key: key
  usage:
    buffer_size: 100
`,
			wantErr: false,
		},
		{
			name: "model with empty name error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
models:
  - name: ""
    provider: openai
    base_url: https://api.openai.com
`,
			wantErr:     true,
			errContains: "models[0].name",
		},
		{
			name: "model with empty base_url error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
models:
  - name: gpt-4o
    provider: openai
    base_url: ""
`,
			wantErr:     true,
			errContains: "models[0].base_url",
		},
		{
			name: "model with non-http base_url error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
models:
  - name: gpt-4o
    provider: openai
    base_url: ftp://api.openai.com
`,
			wantErr:     true,
			errContains: "models[0].base_url",
		},
		{
			name: "model with invalid provider error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
models:
  - name: gpt-4o
    provider: bedrock
    base_url: https://api.openai.com
`,
			wantErr:     true,
			errContains: "models[0].provider",
		},
		{
			name: "azure model without azure_deployment error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
models:
  - name: gpt-4
    provider: azure
    base_url: https://myresource.openai.azure.com
`,
			wantErr:     true,
			errContains: "models[0].azure_deployment",
		},
		{
			name: "duplicate model names error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
models:
  - name: gpt-4o
    provider: openai
    base_url: https://api.openai.com
  - name: gpt-4o
    provider: openai
    base_url: https://api.openai.com
`,
			wantErr:     true,
			errContains: "models[1].name",
		},
		{
			name: "duplicate aliases across models error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 100
models:
  - name: gpt-4o
    provider: openai
    base_url: https://api.openai.com
    aliases: [smart]
  - name: claude-3
    provider: anthropic
    base_url: https://api.anthropic.com
    aliases: [smart]
`,
			wantErr:     true,
			errContains: "models[1].aliases",
		},
		{
			name: "encryption_key empty error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: ""
  usage:
    buffer_size: 100
`,
			wantErr:     true,
			errContains: "settings.encryption_key",
		},
		{
			name: "buffer_size 0 error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: 0
`,
			// setDefaults sets buffer_size to 1000 when 0, so we must write a
			// negative value to trigger the error.
			wantErr: false, // 0 becomes 1000 via setDefaults; no error
		},
		{
			name: "buffer_size negative error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  usage:
    buffer_size: -1
`,
			wantErr:     true,
			errContains: "settings.usage.buffer_size",
		},
		{
			name: "soft_limit_threshold 1.1 error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  soft_limit_threshold: 1.1
  usage:
    buffer_size: 100
`,
			wantErr:     true,
			errContains: "settings.soft_limit_threshold",
		},
		{
			name: "soft_limit_threshold negative error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: key
  soft_limit_threshold: -0.1
  usage:
    buffer_size: 100
`,
			wantErr:     true,
			errContains: "settings.soft_limit_threshold",
		},
		{
			name: "valid config with models no error",
			yaml: `
server:
  proxy:
    port: 8080
database:
  driver: sqlite
  dsn: voidllm.db
settings:
  encryption_key: supersecretkey
  usage:
    buffer_size: 200
models:
` + validModel,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := writeTemp(t, "voidllm.yaml", tc.yaml)
			_, _, err := config.Load(path)

			if tc.wantErr {
				if err == nil {
					t.Fatal("Load() expected error, got nil")
				}
				if tc.errContains != "" && !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err.Error(), tc.errContains)
				}
			} else {
				if err != nil {
					t.Errorf("Load() unexpected error: %v", err)
				}
			}
		})
	}
}

func TestValidate_AllErrorsCollected(t *testing.T) {
	t.Parallel()

	// This config has multiple distinct validation errors simultaneously.
	yaml := `
server:
  proxy:
    port: 99999
  admin:
    port: 70000
    tls:
      enabled: true
database:
  driver: mysql
  dsn: ""
settings:
  encryption_key: ""
  usage:
    buffer_size: -5
  soft_limit_threshold: 2.0
models:
  - name: ""
    provider: bedrock
    base_url: ""
`
	path := writeTemp(t, "voidllm.yaml", yaml)
	_, _, err := config.Load(path)
	if err == nil {
		t.Fatal("Load() expected error, got nil")
	}

	errStr := err.Error()
	expected := []string{
		"server.proxy.port",
		"server.admin.port",
		"server.admin.tls.cert",
		"server.admin.tls.key",
		"database.driver",
		"settings.encryption_key",
		"settings.usage.buffer_size",
		"settings.soft_limit_threshold",
		"models[0].name",
		"models[0].provider",
		"models[0].base_url",
	}

	for _, fragment := range expected {
		if !strings.Contains(errStr, fragment) {
			t.Errorf("error string missing %q\nfull error: %s", fragment, errStr)
		}
	}
}

// ---- Load — file finding ---------------------------------------------------

func TestLoad_ExplicitPath(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, "voidllm.yaml", minimalValidYAML())
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load(%q) unexpected error: %v", path, err)
	}
	if cfg == nil {
		t.Fatal("Load() returned nil config")
	}
}

func TestLoad_NonexistentExplicitPath(t *testing.T) {
	t.Parallel()

	_, _, err := config.Load("/nonexistent/path/voidllm.yaml")
	if err == nil {
		t.Fatal("Load() expected error for nonexistent path, got nil")
	}
}

func TestLoad_VoidllmConfigEnvVar(t *testing.T) {
	// t.Setenv requires sequential execution; no t.Parallel() here.
	path := writeTemp(t, "custom.yaml", minimalValidYAML())
	t.Setenv("VOIDLLM_CONFIG", path)

	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatalf("Load(\"\") with VOIDLLM_CONFIG set: unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load() returned nil config")
	}
}

// isolateFromFilesystem changes into a fresh temp directory so that no
// ./voidllm.yaml is present, clears VOIDLLM_CONFIG, and also clears the two
// env-based secrets so each sub-test starts from a known baseline.
// It restores everything via t.Cleanup.
func isolateFromFilesystem(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	original, err := os.Getwd()
	if err != nil {
		t.Fatalf("isolateFromFilesystem: Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("isolateFromFilesystem: Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	t.Setenv("VOIDLLM_CONFIG", "")
	t.Setenv("VOIDLLM_ENCRYPTION_KEY", "")
	t.Setenv("VOIDLLM_ADMIN_KEY", "")
}

// TestLoad_NoPathNoEnvVarNoFile tests the new loadDefaults() fallback path.
// When no config file is found Load("") now calls loadDefaults(), which reads
// secrets from environment variables and runs validate(). Without an encryption
// key the validation error must mention "settings.encryption_key".
func TestLoad_NoPathNoEnvVarNoFile(t *testing.T) {
	// t.Setenv and os.Chdir require sequential execution; no t.Parallel() here.
	isolateFromFilesystem(t)

	_, _, err := config.Load("")
	if err == nil {
		t.Fatal("Load(\"\") with no encryption key: expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "settings.encryption_key") {
		t.Errorf("Load(\"\") error = %q, want it to mention %q", err.Error(), "settings.encryption_key")
	}
}

// TestLoad_FallbackToDefaultsWithEncryptionKey verifies that Load("") succeeds
// and returns a fully-defaulted Config (proxy port 8080) when no config file
// exists but VOIDLLM_ENCRYPTION_KEY is present in the environment.
func TestLoad_FallbackToDefaultsWithEncryptionKey(t *testing.T) {
	// t.Setenv and os.Chdir require sequential execution; no t.Parallel() here.
	isolateFromFilesystem(t)
	t.Setenv("VOIDLLM_ENCRYPTION_KEY", "test-encryption-key-32chars-long!")

	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatalf("Load(\"\") with VOIDLLM_ENCRYPTION_KEY set: unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load(\"\") returned nil config")
	}
	if cfg.Server.Proxy.Port != 8080 {
		t.Errorf("Server.Proxy.Port = %d, want 8080", cfg.Server.Proxy.Port)
	}
	if cfg.Settings.EncryptionKey != "test-encryption-key-32chars-long!" {
		t.Errorf("Settings.EncryptionKey = %q, want %q", cfg.Settings.EncryptionKey, "test-encryption-key-32chars-long!")
	}
}

// TestLoad_FallbackToDefaultsPicksUpAdminKey verifies that Load("") populates
// both Settings.EncryptionKey and Settings.AdminKey from the environment when
// no config file is present.
func TestLoad_FallbackToDefaultsPicksUpAdminKey(t *testing.T) {
	// t.Setenv and os.Chdir require sequential execution; no t.Parallel() here.
	isolateFromFilesystem(t)
	t.Setenv("VOIDLLM_ENCRYPTION_KEY", "test-encryption-key-32chars-long!")
	t.Setenv("VOIDLLM_ADMIN_KEY", "vl_sa_testsecretadminkey")

	cfg, _, err := config.Load("")
	if err != nil {
		t.Fatalf("Load(\"\") with both env keys set: unexpected error: %v", err)
	}
	if cfg == nil {
		t.Fatal("Load(\"\") returned nil config")
	}
	if cfg.Settings.EncryptionKey != "test-encryption-key-32chars-long!" {
		t.Errorf("Settings.EncryptionKey = %q, want %q", cfg.Settings.EncryptionKey, "test-encryption-key-32chars-long!")
	}
	if cfg.Settings.AdminKey != "vl_sa_testsecretadminkey" {
		t.Errorf("Settings.AdminKey = %q, want %q", cfg.Settings.AdminKey, "vl_sa_testsecretadminkey")
	}
}

// ---- Load — full integration -----------------------------------------------

func TestLoad_FullConfig(t *testing.T) {
	// t.Setenv requires sequential execution; no t.Parallel() here.
	t.Setenv("OPENAI_API_KEY", "sk-test-openai")

	yaml := `
server:
  proxy:
    port: 9090
  admin:
    port: 9091
database:
  driver: postgres
  dsn: postgres://user:pass@localhost/voidllm
settings:
  encryption_key: supersecurekey123
  usage:
    buffer_size: 500
    flush_interval: 10s
  soft_limit_threshold: 0.8
models:
  - name: gpt-4o
    provider: openai
    base_url: https://api.openai.com
    api_key: "${OPENAI_API_KEY}"
    aliases:
      - smart
      - gpt4
    max_context_tokens: 128000
    pricing:
      input_per_1m: 2.50
      output_per_1m: 10.00
  - name: azure-gpt-4
    provider: azure
    base_url: https://myresource.openai.azure.com
    azure_deployment: my-gpt4-deployment
    azure_api_version: "2024-02-01"
    aliases:
      - fast
`
	path := writeTemp(t, "voidllm.yaml", yaml)
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	// Server
	if cfg.Server.Proxy.Port != 9090 {
		t.Errorf("proxy port = %d, want 9090", cfg.Server.Proxy.Port)
	}
	if cfg.Server.Admin.Port != 9091 {
		t.Errorf("admin port = %d, want 9091", cfg.Server.Admin.Port)
	}

	// Models
	if len(cfg.Models) != 2 {
		t.Fatalf("models count = %d, want 2", len(cfg.Models))
	}

	gpt4o := cfg.Models[0]
	if gpt4o.Name != "gpt-4o" {
		t.Errorf("model[0].Name = %q, want %q", gpt4o.Name, "gpt-4o")
	}
	if gpt4o.APIKey != "sk-test-openai" {
		t.Errorf("model[0].APIKey = %q, want %q", gpt4o.APIKey, "sk-test-openai")
	}
	if len(gpt4o.Aliases) != 2 || gpt4o.Aliases[0] != "smart" || gpt4o.Aliases[1] != "gpt4" {
		t.Errorf("model[0].Aliases = %v, want [smart gpt4]", gpt4o.Aliases)
	}
	if gpt4o.MaxContextTokens != 128000 {
		t.Errorf("model[0].MaxContextTokens = %d, want 128000", gpt4o.MaxContextTokens)
	}
	if gpt4o.Pricing.InputPer1M != 2.50 {
		t.Errorf("model[0].Pricing.InputPer1M = %g, want 2.50", gpt4o.Pricing.InputPer1M)
	}
	if gpt4o.Pricing.OutputPer1M != 10.00 {
		t.Errorf("model[0].Pricing.OutputPer1M = %g, want 10.00", gpt4o.Pricing.OutputPer1M)
	}

	azureModel := cfg.Models[1]
	if azureModel.Name != "azure-gpt-4" {
		t.Errorf("model[1].Name = %q, want %q", azureModel.Name, "azure-gpt-4")
	}
	if azureModel.AzureDeployment != "my-gpt4-deployment" {
		t.Errorf("model[1].AzureDeployment = %q, want %q", azureModel.AzureDeployment, "my-gpt4-deployment")
	}
	if azureModel.AzureAPIVersion != "2024-02-01" {
		t.Errorf("model[1].AzureAPIVersion = %q, want %q", azureModel.AzureAPIVersion, "2024-02-01")
	}
	if len(azureModel.Aliases) != 1 || azureModel.Aliases[0] != "fast" {
		t.Errorf("model[1].Aliases = %v, want [fast]", azureModel.Aliases)
	}

	// Settings
	if cfg.Settings.GetSoftLimitThreshold() != 0.8 {
		t.Errorf("GetSoftLimitThreshold() = %g, want 0.8", cfg.Settings.GetSoftLimitThreshold())
	}
	if cfg.Settings.Usage.BufferSize != 500 {
		t.Errorf("Usage.BufferSize = %d, want 500", cfg.Settings.Usage.BufferSize)
	}
	if cfg.Settings.Usage.FlushInterval != 10*time.Second {
		t.Errorf("Usage.FlushInterval = %v, want 10s", cfg.Settings.Usage.FlushInterval)
	}
}
