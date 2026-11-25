package config_test

import (
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

// TestSettingsCurrency covers the settings.currency default and validation.
func TestSettingsCurrency(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		line    string
		want    string
		wantErr bool
	}{
		{name: "defaults to USD", line: "", want: "USD"},
		{name: "EUR", line: "  currency: EUR\n", want: "EUR"},
		{name: "lowercase rejected", line: "  currency: eur\n", wantErr: true},
		{name: "two letters rejected", line: "  currency: US\n", wantErr: true},
		{name: "four letters rejected", line: "  currency: USDT\n", wantErr: true},
		{name: "digits rejected", line: "  currency: U5D\n", wantErr: true},
		{name: "symbol rejected", line: "  currency: \"$\"\n", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := writeTemp(t, "voidllm.yaml", minimalValidYAML()+tc.line)
			cfg, _, err := config.Load(path)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "settings.currency") {
					t.Fatalf("Load() error = %v, want one naming settings.currency", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.Settings.Currency != tc.want {
				t.Errorf("Currency = %q, want %q", cfg.Settings.Currency, tc.want)
			}
		})
	}
}
