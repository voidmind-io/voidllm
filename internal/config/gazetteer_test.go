package config_test

// gazetteer_test.go covers settings.pii.gazetteer configuration: YAML parsing,
// default values, and validation rules (pack names, dirs, inline terms).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

// minimalValidYAMLWithGazetteer inlines the provided gazetteer YAML block
// inside a minimal valid config that already has pii.enabled: true so that the
// gazetteer block itself is validated.
func minimalValidYAMLWithGazetteer(gazBlock string) string {
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
    enabled: true
    action: pseudonymize
    gazetteer:
` + gazBlock
}

// TestGazetteerConfig_Parse verifies that a full gazetteer block is parsed
// and stored correctly in the loaded Config struct.
func TestGazetteerConfig_Parse(t *testing.T) {
	t.Parallel()

	// Create a real temp dir so dir validation passes.
	dir := t.TempDir()

	yaml := minimalValidYAMLWithGazetteer(`
      enabled: true
      packs:
        - company-forms
        - de-cities
      dirs:
        - ` + dir + `
      terms:
        - type: ORG
          values:
            - Project Titan
            - Operation Moonshot
      options:
        case_insensitive: true
`)

	path := writeTemp(t, "voidllm.yaml", yaml)
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	gaz := cfg.Settings.PII.Gazetteer

	if !gaz.IsEnabled() {
		t.Error("Gazetteer.IsEnabled() = false, want true")
	}

	if len(gaz.Packs) != 2 {
		t.Fatalf("Packs = %v, want [company-forms, de-cities]", gaz.Packs)
	}
	if gaz.Packs[0] != "company-forms" {
		t.Errorf("Packs[0] = %q, want company-forms", gaz.Packs[0])
	}
	if gaz.Packs[1] != "de-cities" {
		t.Errorf("Packs[1] = %q, want de-cities", gaz.Packs[1])
	}

	if len(gaz.Dirs) != 1 || gaz.Dirs[0] != dir {
		t.Errorf("Dirs = %v, want [%q]", gaz.Dirs, dir)
	}

	if len(gaz.Terms) != 1 {
		t.Fatalf("Terms = %v, want 1 entry", gaz.Terms)
	}
	if gaz.Terms[0].Type != "ORG" {
		t.Errorf("Terms[0].Type = %q, want ORG", gaz.Terms[0].Type)
	}
	if len(gaz.Terms[0].Values) != 2 {
		t.Errorf("Terms[0].Values = %v, want 2 values", gaz.Terms[0].Values)
	}

	if !gaz.Options.IsCaseInsensitive() {
		t.Error("Options.IsCaseInsensitive() = false, want true")
	}
}

// TestGazetteerConfig_Defaults verifies the default values applied by
// setDefaults when no gazetteer block is present in the YAML.
func TestGazetteerConfig_Defaults(t *testing.T) {
	t.Parallel()

	path := writeTemp(t, "voidllm.yaml", minimalValidYAML())
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	gaz := cfg.Settings.PII.Gazetteer

	t.Run("enabled defaults to false", func(t *testing.T) {
		t.Parallel()
		if gaz.IsEnabled() {
			t.Error("Gazetteer.IsEnabled() = true, want false (disabled by default)")
		}
		if gaz.Enabled == nil {
			t.Error("Gazetteer.Enabled is nil after setDefaults, want &false")
		}
	})

	t.Run("case_insensitive defaults to true", func(t *testing.T) {
		t.Parallel()
		if !gaz.Options.IsCaseInsensitive() {
			t.Error("Options.IsCaseInsensitive() = false, want true (default)")
		}
		if gaz.Options.CaseInsensitive == nil {
			t.Error("Options.CaseInsensitive is nil after setDefaults, want &true")
		}
	})
}

// TestGazetteerConfig_CaseInsensitiveFalse verifies that explicitly setting
// case_insensitive: false is preserved and not overwritten to the default.
func TestGazetteerConfig_CaseInsensitiveFalse(t *testing.T) {
	t.Parallel()

	yaml := minimalValidYAMLWithGazetteer(`
      enabled: true
      packs:
        - company-forms
      options:
        case_insensitive: false
`)

	path := writeTemp(t, "voidllm.yaml", yaml)
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	if cfg.Settings.PII.Gazetteer.Options.IsCaseInsensitive() {
		t.Error("IsCaseInsensitive() = true, want false when explicitly set to false")
	}
}

// TestGazetteerConfig_EnabledNilMeansDisabled verifies that a nil Enabled
// pointer (field absent from YAML) means the detector is disabled.
func TestGazetteerConfig_EnabledNilMeansDisabled(t *testing.T) {
	t.Parallel()

	// Build a PIIGazetteerConfig directly without going through YAML/Load
	// to test the IsEnabled() method with a nil pointer.
	var gaz config.PIIGazetteerConfig
	if gaz.IsEnabled() {
		t.Error("IsEnabled() on zero-value config = true, want false")
	}
}

// TestGazetteerConfig_CaseInsensitiveNilMeansTrue verifies that a nil
// CaseInsensitive pointer means case-insensitive is on.
func TestGazetteerConfig_CaseInsensitiveNilMeansTrue(t *testing.T) {
	t.Parallel()

	var opts config.PIIGazetteerOptionsConfig
	if !opts.IsCaseInsensitive() {
		t.Error("IsCaseInsensitive() on nil pointer = false, want true (default on)")
	}
}

// TestGazetteerValidation covers all validation error paths for the
// settings.pii.gazetteer block.
func TestGazetteerValidation(t *testing.T) {
	t.Parallel()

	// We need a real temp dir for the "valid dir" cases.
	existingDir := t.TempDir()

	tests := []struct {
		name        string
		yaml        string
		wantErr     bool
		errContains string
	}{
		// ── valid ────────────────────────────────────────────────────────────
		{
			name: "gazetteer disabled, pack list not validated",
			yaml: minimalValidYAMLWithGazetteer(`
      enabled: false
      packs:
        - nonexistent-pack
`),
			// Pack name is invalid but gazetteer is disabled, so no error.
		},
		{
			name: "gazetteer enabled with known packs",
			yaml: minimalValidYAMLWithGazetteer(`
      enabled: true
      packs:
        - company-forms
        - de-cities
`),
		},
		{
			name: "gazetteer enabled with valid existing dir",
			yaml: minimalValidYAMLWithGazetteer(`
      enabled: true
      dirs:
        - ` + existingDir + `
`),
		},
		{
			name: "gazetteer enabled with inline terms with type",
			yaml: minimalValidYAMLWithGazetteer(`
      enabled: true
      terms:
        - type: ORG
          values:
            - Acme Corp
`),
		},
		{
			name: "gazetteer enabled with no packs or terms (empty detector)",
			yaml: minimalValidYAMLWithGazetteer(`
      enabled: true
`),
		},
		// ── invalid ──────────────────────────────────────────────────────────
		{
			name: "unknown pack name returns error",
			yaml: minimalValidYAMLWithGazetteer(`
      enabled: true
      packs:
        - company-forms
        - unknown-pack-xyz
`),
			wantErr:     true,
			errContains: "settings.pii.gazetteer.packs",
		},
		{
			name: "only unknown pack returns error",
			yaml: minimalValidYAMLWithGazetteer(`
      enabled: true
      packs:
        - totally-unknown
`),
			wantErr:     true,
			errContains: "settings.pii.gazetteer.packs",
		},
		{
			name: "nonexistent dir returns error",
			yaml: minimalValidYAMLWithGazetteer(`
      enabled: true
      dirs:
        - /no/such/dir/xyz123
`),
			wantErr:     true,
			errContains: "settings.pii.gazetteer.dirs",
		},
		{
			name: "inline term with empty type returns error",
			yaml: minimalValidYAMLWithGazetteer(`
      enabled: true
      terms:
        - type: ""
          values:
            - foo
`),
			wantErr:     true,
			errContains: "settings.pii.gazetteer.terms[0].type",
		},
		{
			name: "second inline term with empty type uses correct index",
			yaml: minimalValidYAMLWithGazetteer(`
      enabled: true
      terms:
        - type: ORG
          values:
            - GmbH
        - type: ""
          values:
            - bar
`),
			wantErr:     true,
			errContains: "settings.pii.gazetteer.terms[1].type",
		},
		{
			name: "dir that is a file not a directory returns error",
			// Write a temp file then use its path as a dir.
			// We build this inline using a file path approach in the subtest below.
		},
	}

	for _, tc := range tests {
		if tc.yaml == "" {
			continue // skip the "dir is a file" case handled below
		}
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

	// Special case: path that points to a file (not a directory).
	t.Run("dir path pointing to a file returns error", func(t *testing.T) {
		t.Parallel()

		// Write a plain file, then use its path as a "dir".
		fileAsDir := filepath.Join(t.TempDir(), "not-a-dir.txt")
		if err := os.WriteFile(fileAsDir, []byte("content"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		yaml := minimalValidYAMLWithGazetteer(`
      enabled: true
      dirs:
        - ` + fileAsDir + `
`)
		path := writeTemp(t, "voidllm.yaml", yaml)
		_, _, err := config.Load(path)
		if err == nil {
			t.Error("expected error when dir path is a file, got nil")
		}
		if err != nil && !strings.Contains(err.Error(), "settings.pii.gazetteer.dirs") {
			t.Errorf("error %q should mention settings.pii.gazetteer.dirs", err.Error())
		}
	})
}

// TestGazetteerConfig_MultipleInlineTermGroups verifies that multiple inline
// term entries with different types are all stored correctly.
func TestGazetteerConfig_MultipleInlineTermGroups(t *testing.T) {
	t.Parallel()

	yaml := minimalValidYAMLWithGazetteer(`
      enabled: true
      terms:
        - type: ORG
          values:
            - Acme Corp
            - Foo Inc
        - type: PERSON
          values:
            - Alice Smith
`)

	path := writeTemp(t, "voidllm.yaml", yaml)
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}

	terms := cfg.Settings.PII.Gazetteer.Terms
	if len(terms) != 2 {
		t.Fatalf("Terms length = %d, want 2", len(terms))
	}
	if terms[0].Type != "ORG" {
		t.Errorf("Terms[0].Type = %q, want ORG", terms[0].Type)
	}
	if len(terms[0].Values) != 2 {
		t.Errorf("Terms[0].Values = %v, want 2 entries", terms[0].Values)
	}
	if terms[1].Type != "PERSON" {
		t.Errorf("Terms[1].Type = %q, want PERSON", terms[1].Type)
	}
	if len(terms[1].Values) != 1 || terms[1].Values[0] != "Alice Smith" {
		t.Errorf("Terms[1].Values = %v, want [Alice Smith]", terms[1].Values)
	}
}

// TestGazetteerConfig_DisabledPackValidationSkipped verifies that when
// gazetteer.enabled is false the pack-name validation is not applied, allowing
// the config to load even with a fake pack name.  This lets operators disable
// the feature temporarily without removing their config entries.
func TestGazetteerConfig_DisabledPackValidationSkipped(t *testing.T) {
	t.Parallel()

	yaml := minimalValidYAMLWithGazetteer(`
      enabled: false
      packs:
        - not-a-real-pack
      terms:
        - type: ""
          values:
            - ignored
`)
	// Even though type is empty and pack name is invalid, no error because disabled.
	path := writeTemp(t, "voidllm.yaml", yaml)
	_, _, err := config.Load(path)
	if err != nil {
		t.Errorf("Load() unexpected error when gazetteer disabled: %v", err)
	}
}
