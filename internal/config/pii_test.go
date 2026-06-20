package config_test

import (
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

// minimalValidYAMLWithPII returns a valid base config with the provided
// settings.pii block inlined. This avoids repeating all required fields
// in every PII-specific test case.
func minimalValidYAMLWithPII(piiBlock string) string {
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
  pii:
` + piiBlock
}

// TestValidate_PII covers settings.pii validation and default handling.
func TestValidate_PII(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		yaml        string
		wantErr     bool
		errContains string
	}{
		// ── valid configurations ────────────────────────────────────────────
		{
			name: "pii disabled by default, no pii block at all",
			yaml: minimalValidYAML(),
		},
		{
			name: "pii explicitly disabled, no validation of pii fields",
			yaml: minimalValidYAMLWithPII(`
    enabled: false
    action: "block"
`),
			// action "block" would be invalid when enabled, but must be ignored when disabled.
		},
		{
			name: "pii enabled with action pseudonymize",
			yaml: minimalValidYAMLWithPII(`
    enabled: true
    action: "pseudonymize"
`),
		},
		{
			name: "pii enabled with valid custom pattern",
			yaml: minimalValidYAMLWithPII(`
    enabled: true
    action: "pseudonymize"
    patterns:
      - type: "PASSPORT_NO"
        regexp: '\b[A-Z]{1,2}[0-9]{6,9}\b'
`),
		},
		{
			name: "pii enabled with multiple valid custom patterns",
			yaml: minimalValidYAMLWithPII(`
    enabled: true
    action: "pseudonymize"
    patterns:
      - type: "PASSPORT_NO"
        regexp: '\b[A-Z]{1,2}[0-9]{6,9}\b'
      - type: "EMPLOYEE_ID"
        regexp: '\bEMP-[0-9]{5}\b'
`),
		},

		// ── invalid configurations ──────────────────────────────────────────
		{
			name: "pii enabled with action block returns error",
			yaml: minimalValidYAMLWithPII(`
    enabled: true
    action: "block"
`),
			wantErr:     true,
			errContains: "settings.pii.action",
		},
		{
			name: "pii enabled with action redact returns error",
			yaml: minimalValidYAMLWithPII(`
    enabled: true
    action: "redact"
`),
			wantErr:     true,
			errContains: "settings.pii.action",
		},
		{
			name: "pii enabled with empty action string gets default pseudonymize",
			// setDefaults fills in "pseudonymize" when action is empty, so this is valid.
			yaml: minimalValidYAMLWithPII(`
    enabled: true
`),
		},
		{
			name: "pii enabled with invalid custom pattern regexp returns error",
			yaml: minimalValidYAMLWithPII(`
    enabled: true
    action: "pseudonymize"
    patterns:
      - type: "BROKEN"
        regexp: '[invalid'
`),
			wantErr:     true,
			errContains: "settings.pii.patterns[0].regexp",
		},
		{
			name: "pii enabled with empty pattern type returns error",
			yaml: minimalValidYAMLWithPII(`
    enabled: true
    action: "pseudonymize"
    patterns:
      - type: ""
        regexp: '\d+'
`),
			wantErr:     true,
			errContains: "settings.pii.patterns[0].type",
		},
		{
			name: "pii enabled with empty pattern regexp returns error",
			yaml: minimalValidYAMLWithPII(`
    enabled: true
    action: "pseudonymize"
    patterns:
      - type: "CUSTOM"
        regexp: ""
`),
			wantErr:     true,
			errContains: "settings.pii.patterns[0].regexp",
		},
		{
			name: "pii enabled invalid pattern in second entry uses correct index",
			yaml: minimalValidYAMLWithPII(`
    enabled: true
    action: "pseudonymize"
    patterns:
      - type: "VALID"
        regexp: '\d+'
      - type: "BROKEN"
        regexp: '(?P<bad'
`),
			wantErr:     true,
			errContains: "settings.pii.patterns[1].regexp",
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

// TestPIIDefaults verifies the default values for PIIConfig after setDefaults
// runs on a minimal config that does not set any pii fields.
func TestPIIDefaults(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, "voidllm.yaml", minimalValidYAML())
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	t.Run("enabled defaults to false", func(t *testing.T) {
		t.Parallel()

		if cfg.Settings.PII.IsEnabled() {
			t.Error("PII.IsEnabled() = true, want false (disabled by default)")
		}
		// The pointer must not be nil after setDefaults — it is set to &false.
		if cfg.Settings.PII.Enabled == nil {
			t.Error("PII.Enabled pointer is nil after setDefaults, want &false")
		}
		if *cfg.Settings.PII.Enabled != false {
			t.Errorf("*PII.Enabled = %v, want false", *cfg.Settings.PII.Enabled)
		}
	})

	t.Run("action defaults to pseudonymize", func(t *testing.T) {
		t.Parallel()

		if cfg.Settings.PII.Action != "pseudonymize" {
			t.Errorf("PII.Action = %q, want %q", cfg.Settings.PII.Action, "pseudonymize")
		}
	})

	t.Run("patterns defaults to nil/empty", func(t *testing.T) {
		t.Parallel()

		if len(cfg.Settings.PII.Patterns) != 0 {
			t.Errorf("PII.Patterns = %v, want empty", cfg.Settings.PII.Patterns)
		}
	})
}

// TestPIIIsEnabled_ExplicitTrue verifies that IsEnabled returns true when
// explicitly set to true in the YAML.
func TestPIIIsEnabled_ExplicitTrue(t *testing.T) {
	t.Parallel()

	yaml := minimalValidYAMLWithPII(`
    enabled: true
    action: "pseudonymize"
`)
	path := writeTemp(t, "voidllm.yaml", yaml)
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if !cfg.Settings.PII.IsEnabled() {
		t.Error("PII.IsEnabled() = false, want true when enabled: true is set")
	}
}

// TestPIIIsEnabled_ExplicitFalse verifies that IsEnabled returns false when
// explicitly set to false in the YAML (pointer semantics: not nil, value false).
func TestPIIIsEnabled_ExplicitFalse(t *testing.T) {
	t.Parallel()

	yaml := minimalValidYAMLWithPII(`
    enabled: false
`)
	path := writeTemp(t, "voidllm.yaml", yaml)
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.Settings.PII.IsEnabled() {
		t.Error("PII.IsEnabled() = true, want false when enabled: false is set")
	}
}
