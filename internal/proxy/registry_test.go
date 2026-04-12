package proxy

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

// modelConfigs is a helper that returns a minimal valid slice of ModelConfig
// values to avoid repetition across tests.
func modelConfigs(mcs ...config.ModelConfig) []config.ModelConfig {
	return mcs
}

func mc(name, provider, baseURL, apiKey string, aliases ...string) config.ModelConfig {
	return config.ModelConfig{
		Name:     name,
		Provider: provider,
		BaseURL:  baseURL,
		APIKey:   apiKey,
		Aliases:  aliases,
	}
}

// TestNewRegistry_Valid verifies that a well-formed set of model configs
// produces a registry without error.
func TestNewRegistry_Valid(t *testing.T) {
	cfgs := modelConfigs(
		mc("gpt-4o", "openai", "https://api.openai.com", "sk-test", "gpt4o", "gpt-4o-latest"),
		mc("claude-3-5-sonnet", "anthropic", "https://api.anthropic.com", "ant-test"),
	)

	r, err := NewRegistry(cfgs)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r == nil {
		t.Fatal("registry is nil")
	}
}

// TestNewRegistry_DuplicateName verifies that registering two models with the
// same name returns an error that mentions "duplicate model name".
func TestNewRegistry_DuplicateName(t *testing.T) {
	cfgs := modelConfigs(
		mc("gpt-4o", "openai", "https://api.openai.com", "sk-1"),
		mc("gpt-4o", "openai", "https://api.openai.com", "sk-2"),
	)

	_, err := NewRegistry(cfgs)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if want := "duplicate model name"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err.Error(), want)
	}
}

// TestNewRegistry_DuplicateAlias verifies that the same alias appearing twice
// across different models returns an error that mentions "duplicate alias".
func TestNewRegistry_DuplicateAlias(t *testing.T) {
	cfgs := modelConfigs(
		mc("gpt-4o", "openai", "https://api.openai.com", "sk-1", "latest"),
		mc("gpt-4o-mini", "openai", "https://api.openai.com", "sk-2", "latest"),
	)

	_, err := NewRegistry(cfgs)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if want := "duplicate alias"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err.Error(), want)
	}
}

// TestNewRegistry_AliasCollidesWithName verifies that an alias matching an
// existing model's canonical name returns an error mentioning "collides with model name".
func TestNewRegistry_AliasCollidesWithName(t *testing.T) {
	cfgs := modelConfigs(
		mc("gpt-4o", "openai", "https://api.openai.com", "sk-1"),
		mc("gpt-4o-mini", "openai", "https://api.openai.com", "sk-2", "gpt-4o"),
	)

	_, err := NewRegistry(cfgs)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if want := "collides with model name"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err.Error(), want)
	}
}

// TestNewRegistry_AliasCollidesWithLaterName verifies that an alias colliding
// with a model name defined LATER in the slice is also detected. This exercises
// the two-pass registration logic.
func TestNewRegistry_AliasCollidesWithLaterName(t *testing.T) {
	// Model A has alias "clash"; model B is named "clash". In a single-pass
	// implementation the collision would be missed because "clash" is not yet in
	// r.models when model A's aliases are processed.
	cfgs := modelConfigs(
		mc("model-a", "openai", "https://api.openai.com", "sk-1", "clash"),
		mc("clash", "openai", "https://api.openai.com", "sk-2"),
	)

	_, err := NewRegistry(cfgs)
	if err == nil {
		t.Fatal("expected error for alias colliding with later model name, got nil")
	}
	if want := "collides with model name"; !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not contain %q", err.Error(), want)
	}
}

// TestResolve_ByCanonicalName verifies that Resolve returns the correct model
// when queried by its canonical name.
func TestResolve_ByCanonicalName(t *testing.T) {
	cfgs := modelConfigs(
		mc("gpt-4o", "openai", "https://api.openai.com/v1", "sk-test"),
	)
	r, err := NewRegistry(cfgs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	m, err := r.Resolve("gpt-4o")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if m.Name != "gpt-4o" {
		t.Errorf("Name = %q, want %q", m.Name, "gpt-4o")
	}
	if m.Provider != "openai" {
		t.Errorf("Provider = %q, want %q", m.Provider, "openai")
	}
	if m.BaseURL != "https://api.openai.com/v1" {
		t.Errorf("BaseURL = %q, want %q", m.BaseURL, "https://api.openai.com/v1")
	}
}

// TestResolve_ByAlias verifies that Resolve returns equivalent model values
// whether queried by canonical name or by an alias.
func TestResolve_ByAlias(t *testing.T) {
	cfgs := modelConfigs(
		mc("gpt-4o", "openai", "https://api.openai.com/v1", "sk-test", "gpt4o", "gpt-4o-latest"),
	)
	r, err := NewRegistry(cfgs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	canonical, err := r.Resolve("gpt-4o")
	if err != nil {
		t.Fatalf("Resolve canonical: %v", err)
	}

	for _, alias := range []string{"gpt4o", "gpt-4o-latest"} {
		byAlias, err := r.Resolve(alias)
		if err != nil {
			t.Fatalf("Resolve %q: %v", alias, err)
		}
		if byAlias.Name != canonical.Name {
			t.Errorf("alias %q resolved to Name %q, want %q", alias, byAlias.Name, canonical.Name)
		}
		if byAlias.Provider != canonical.Provider {
			t.Errorf("alias %q resolved to Provider %q, want %q", alias, byAlias.Provider, canonical.Provider)
		}
		if byAlias.BaseURL != canonical.BaseURL {
			t.Errorf("alias %q resolved to BaseURL %q, want %q", alias, byAlias.BaseURL, canonical.BaseURL)
		}
	}
}

// TestResolve_ReturnsCopy verifies that mutating the Aliases slice on a
// resolved Model does not affect subsequent Resolve calls.
func TestResolve_ReturnsCopy(t *testing.T) {
	cfgs := modelConfigs(
		mc("gpt-4o", "openai", "https://api.openai.com/v1", "sk-test", "gpt4o"),
	)
	r, err := NewRegistry(cfgs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	first, err := r.Resolve("gpt-4o")
	if err != nil {
		t.Fatalf("Resolve first: %v", err)
	}
	first.Aliases[0] = "mutated"

	second, err := r.Resolve("gpt-4o")
	if err != nil {
		t.Fatalf("Resolve second: %v", err)
	}
	if len(second.Aliases) > 0 && second.Aliases[0] == "mutated" {
		t.Error("Resolve() returned a shared Aliases slice; mutation affected the registry")
	}
}

// TestResolve_Unknown verifies that resolving an unregistered name wraps
// ErrModelNotFound.
func TestResolve_Unknown(t *testing.T) {
	r, err := NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	_, err = r.Resolve("does-not-exist")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("errors.Is(err, ErrModelNotFound) = false; err = %v", err)
	}
}

// TestList_SortedByName verifies that List returns all models ordered
// lexicographically by name.
func TestList_SortedByName(t *testing.T) {
	cfgs := modelConfigs(
		mc("zmodel", "openai", "https://api.openai.com/v1", "sk-z"),
		mc("amodel", "openai", "https://api.openai.com/v1", "sk-a"),
		mc("mmodel", "openai", "https://api.openai.com/v1", "sk-m"),
	)
	r, err := NewRegistry(cfgs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	list := r.List()
	if len(list) != 3 {
		t.Fatalf("List() len = %d, want 3", len(list))
	}

	wantOrder := []string{"amodel", "mmodel", "zmodel"}
	for i, m := range list {
		if m.Name != wantOrder[i] {
			t.Errorf("List()[%d].Name = %q, want %q", i, m.Name, wantOrder[i])
		}
	}
}

// TestList_ReturnsCopy verifies that mutating the slice returned by List does
// not affect subsequent calls to List.
func TestList_ReturnsCopy(t *testing.T) {
	cfgs := modelConfigs(
		mc("gpt-4o", "openai", "https://api.openai.com/v1", "sk-test"),
	)
	r, err := NewRegistry(cfgs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	first := r.List()
	first[0].Name = "mutated" // mutate the copy

	second := r.List()
	if second[0].Name == "mutated" {
		t.Error("List() returned elements that share state with the registry; mutation affected subsequent calls")
	}
}

// TestList_AliasesCopied verifies that mutating the Aliases slice of a model
// returned by List does not affect subsequent calls.
func TestList_AliasesCopied(t *testing.T) {
	cfgs := modelConfigs(
		mc("gpt-4o", "openai", "https://api.openai.com/v1", "sk-test", "gpt4o"),
	)
	r, err := NewRegistry(cfgs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	first := r.List()
	first[0].Aliases[0] = "mutated"

	second := r.List()
	if len(second[0].Aliases) > 0 && second[0].Aliases[0] == "mutated" {
		t.Error("List() returned a shared Aliases slice; mutation affected the registry")
	}
}

// TestEmptyRegistry verifies that a registry built from nil or an empty slice
// returns ErrModelNotFound on Resolve and an empty (non-nil) slice from List.
func TestEmptyRegistry(t *testing.T) {
	r, err := NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry(nil): %v", err)
	}

	_, err = r.Resolve("anything")
	if !errors.Is(err, ErrModelNotFound) {
		t.Errorf("Resolve on empty registry: errors.Is(err, ErrModelNotFound) = false; err = %v", err)
	}

	list := r.List()
	if list == nil {
		t.Error("List() returned nil, want empty non-nil slice")
	}
	if len(list) != 0 {
		t.Errorf("List() len = %d, want 0", len(list))
	}
}

// TestAzureFields verifies that AzureDeployment and AzureAPIVersion are
// preserved faithfully when building a registry entry.
func TestAzureFields(t *testing.T) {
	cfgs := []config.ModelConfig{
		{
			Name:            "gpt-4o-azure",
			Provider:        "azure",
			BaseURL:         "https://my-resource.openai.azure.com",
			APIKey:          "azure-key",
			AzureDeployment: "gpt4o-deployment",
			AzureAPIVersion: "2024-02-01",
		},
	}
	r, err := NewRegistry(cfgs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	m, err := r.Resolve("gpt-4o-azure")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if m.AzureDeployment != "gpt4o-deployment" {
		t.Errorf("AzureDeployment = %q, want %q", m.AzureDeployment, "gpt4o-deployment")
	}
	if m.AzureAPIVersion != "2024-02-01" {
		t.Errorf("AzureAPIVersion = %q, want %q", m.AzureAPIVersion, "2024-02-01")
	}
}

// TestPricingField verifies that Pricing is copied from ModelConfig into the
// resolved Model.
func TestPricingField(t *testing.T) {
	cfgs := []config.ModelConfig{
		{
			Name:     "gpt-4o",
			Provider: "openai",
			BaseURL:  "https://api.openai.com/v1",
			APIKey:   "sk-test",
			Pricing: config.PricingConfig{
				InputPer1M:  5.00,
				OutputPer1M: 15.00,
			},
		},
	}
	r, err := NewRegistry(cfgs)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	m, err := r.Resolve("gpt-4o")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if m.Pricing.InputPer1M != 5.00 {
		t.Errorf("Pricing.InputPer1M = %v, want 5.00", m.Pricing.InputPer1M)
	}
	if m.Pricing.OutputPer1M != 15.00 {
		t.Errorf("Pricing.OutputPer1M = %v, want 15.00", m.Pricing.OutputPer1M)
	}
}

// TestModelLogValue verifies that the slog.LogValuer implementation on Model
// redacts the API key and that the api_key attribute is present.
func TestModelLogValue(t *testing.T) {
	m := Model{
		Name:     "gpt-4o",
		Provider: "openai",
		BaseURL:  "https://api.openai.com/v1",
		APIKey:   "super-secret-key",
	}

	val := m.LogValue()
	found := false
	for _, attr := range val.Group() {
		if attr.Key == "api_key" {
			found = true
			if attr.Value.String() != "[REDACTED]" {
				t.Errorf("api_key in LogValue = %q, want \"[REDACTED]\"", attr.Value.String())
			}
		}
	}
	if !found {
		t.Error("LogValue() missing api_key attribute")
	}
}

// mcWithFallback returns a ModelConfig with a fallback model set.
func mcWithFallback(name, provider, fallback string) config.ModelConfig {
	return config.ModelConfig{
		Name:     name,
		Provider: provider,
		BaseURL:  "http://" + name,
		APIKey:   "k",
		Fallback: fallback,
	}
}

// TestRegistry_StripAllFallbacks verifies the direct unit contract of
// StripAllFallbacks: count returned equals models that had a fallback,
// all FallbackModelName fields are cleared, and a second call returns 0.
func TestRegistry_StripAllFallbacks(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry([]config.ModelConfig{
		mcWithFallback("a", "openai", "b"),
		mcWithFallback("b", "openai", "c"),
		{Name: "c", Provider: "openai", BaseURL: "http://c", APIKey: "k"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	// Sanity: all three models are registered.
	if got := len(reg.AllModels()); got != 3 {
		t.Fatalf("AllModels() len = %d, want 3", got)
	}

	// First strip: two models have a fallback.
	stripped := reg.StripAllFallbacks()
	if stripped != 2 {
		t.Errorf("StripAllFallbacks() = %d, want 2", stripped)
	}

	// All fallbacks must now be empty.
	for _, m := range reg.AllModels() {
		if m.FallbackModelName != "" {
			t.Errorf("model %q still has FallbackModelName=%q after strip", m.Name, m.FallbackModelName)
		}
	}

	// Idempotent: second call should clear nothing.
	stripped2 := reg.StripAllFallbacks()
	if stripped2 != 0 {
		t.Errorf("StripAllFallbacks() second call = %d, want 0", stripped2)
	}
}

// TestRegistry_StripAllFallbacks_EmptyRegistry verifies that StripAllFallbacks
// on an empty registry returns 0 without panicking.
func TestRegistry_StripAllFallbacks_EmptyRegistry(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry(nil): %v", err)
	}

	if got := reg.StripAllFallbacks(); got != 0 {
		t.Errorf("StripAllFallbacks() on empty registry = %d, want 0", got)
	}
}

// TestRegistry_StripAllFallbacks_NoFallbacks verifies that StripAllFallbacks
// returns 0 when no model has a fallback configured.
func TestRegistry_StripAllFallbacks_NoFallbacks(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry([]config.ModelConfig{
		mc("x", "openai", "http://x", "k"),
		mc("y", "openai", "http://y", "k"),
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	if got := reg.StripAllFallbacks(); got != 0 {
		t.Errorf("StripAllFallbacks() with no fallbacks = %d, want 0", got)
	}
}

// TestRegistry_StripAllFallbacks_Concurrent verifies that concurrent reads
// (AllModels, FallbackFor) and writes (StripAllFallbacks) do not produce data
// races. Correctness of specific counts is not asserted — the criterion is
// that the race detector reports no violations.
func TestRegistry_StripAllFallbacks_Concurrent(t *testing.T) {
	t.Parallel()

	reg, err := NewRegistry([]config.ModelConfig{
		mcWithFallback("m1", "openai", "m2"),
		mcWithFallback("m2", "openai", "m3"),
		{Name: "m3", Provider: "openai", BaseURL: "http://m3", APIKey: "k"},
		{Name: "m4", Provider: "openai", BaseURL: "http://m4", APIKey: "k"},
	})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	const goroutines = 20
	const iterations = 50

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := range goroutines {
		go func(id int) {
			defer wg.Done()
			for range iterations {
				switch id % 3 {
				case 0:
					// Writer: strip fallbacks.
					_ = reg.StripAllFallbacks()
				case 1:
					// Reader: iterate all models.
					_ = reg.AllModels()
				case 2:
					// Reader: resolve fallback for a named model.
					visited := map[string]bool{}
					_, _ = reg.FallbackFor("m1", visited)
				}
			}
		}(i)
	}

	wg.Wait()
	// Reaching here without the race detector firing is the success criterion.
}
