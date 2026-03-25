// Package proxy implements the LLM proxy core: model registry, request forwarding,
// streaming, and provider adapters.
package proxy

import (
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
)

// ErrModelNotFound is returned when a model name or alias cannot be resolved.
var ErrModelNotFound = errors.New("model not found")

// Deployment holds endpoint-specific configuration for one deployment
// within a multi-deployment model.
type Deployment struct {
	Name            string
	Provider        string
	BaseURL         string
	APIKey          string
	AzureDeployment string
	AzureAPIVersion string
	Weight          int
	Priority        int
}

// LogValue implements slog.LogValuer to prevent the upstream API key from
// appearing in structured log output.
func (d Deployment) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", d.Name),
		slog.String("provider", d.Provider),
		slog.String("base_url", d.BaseURL),
		slog.String("api_key", "[REDACTED]"),
	)
}

// Model holds a fully resolved model configuration ready for proxying.
type Model struct {
	Name     string
	Provider string // "vllm" | "openai" | "anthropic" | "azure" | "ollama" | "custom"
	// "completion", "image", "audio_transcription", or "tts". Defaults to "chat".
	Type             string
	BaseURL          string
	APIKey           string // upstream provider's API key (plaintext, in-memory)
	Aliases          []string
	MaxContextTokens int
	Pricing          config.PricingConfig
	AzureDeployment  string
	AzureAPIVersion  string
	// Timeout is the per-model upstream timeout. When non-zero it overrides the
	// global MaxStreamDuration and the context deadline used for non-streaming
	// requests. Zero means use the global default.
	Timeout time.Duration
	// Strategy is the deployment selection strategy used when Deployments is
	// non-empty. Valid values: round-robin, least-latency, weighted, priority.
	Strategy string
	// MaxRetries is the number of times the proxy will retry a failed upstream
	// request across the available deployments. Must be >= 0.
	MaxRetries int
	// Deployments is the list of backend endpoints for this model. When
	// non-empty, the model-level Provider, BaseURL, and APIKey are ignored
	// in favour of the per-deployment values.
	Deployments []Deployment
}

// LogValue implements slog.LogValuer to prevent the upstream API key from
// appearing in structured log output.
func (m Model) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("name", m.Name),
		slog.String("provider", m.Provider),
		slog.String("base_url", m.BaseURL),
		slog.String("api_key", "[REDACTED]"),
	)
}

// Registry holds the in-memory model registry and resolves model names/aliases
// for proxy requests. All methods are safe for concurrent use.
type Registry struct {
	mu      sync.RWMutex
	models  map[string]*Model // canonical name → model
	aliases map[string]string // alias → canonical name
	sorted  []*Model          // pre-sorted by name for List()
}

// NewRegistry builds a Registry from a slice of ModelConfig values. It returns
// an error if any model name or alias is duplicated, or if an alias collides
// with any model name (including those defined later in the slice).
func NewRegistry(models []config.ModelConfig) (*Registry, error) {
	r := &Registry{
		models:  make(map[string]*Model, len(models)),
		aliases: make(map[string]string),
	}

	// Pass 1: register all canonical names.
	for i := range models {
		mc := &models[i]

		if _, exists := r.models[mc.Name]; exists {
			return nil, fmt.Errorf("duplicate model name %q", mc.Name)
		}

		aliases := make([]string, len(mc.Aliases))
		copy(aliases, mc.Aliases)

		var timeout time.Duration
		if mc.Timeout != "" {
			if d, err := time.ParseDuration(mc.Timeout); err == nil {
				timeout = d
			} else {
				slog.Warn("model: invalid timeout string, ignoring",
					slog.String("model", mc.Name),
					slog.String("timeout", mc.Timeout),
					slog.String("error", err.Error()),
				)
			}
		}

		modelType := mc.Type
		if modelType == "" {
			modelType = "chat"
		}

		deployments := make([]Deployment, len(mc.Deployments))
		for i, d := range mc.Deployments {
			deployments[i] = Deployment{
				Name:            d.Name,
				Provider:        d.Provider,
				BaseURL:         d.BaseURL,
				APIKey:          d.APIKey,
				AzureDeployment: d.AzureDeployment,
				AzureAPIVersion: d.AzureAPIVersion,
				Weight:          d.Weight,
				Priority:        d.Priority,
			}
		}

		m := &Model{
			Name:             mc.Name,
			Provider:         mc.Provider,
			Type:             modelType,
			BaseURL:          mc.BaseURL,
			APIKey:           mc.APIKey,
			Aliases:          aliases,
			MaxContextTokens: mc.MaxContextTokens,
			Pricing:          mc.Pricing,
			AzureDeployment:  mc.AzureDeployment,
			AzureAPIVersion:  mc.AzureAPIVersion,
			Timeout:          timeout,
			Strategy:         mc.Strategy,
			MaxRetries:       mc.MaxRetries,
			Deployments:      deployments,
		}
		r.models[mc.Name] = m
	}

	// Pass 2: register all aliases now that every canonical name is known.
	for i := range models {
		mc := &models[i]

		for _, alias := range mc.Aliases {
			if _, exists := r.aliases[alias]; exists {
				return nil, fmt.Errorf("duplicate alias %q", alias)
			}
			if _, exists := r.models[alias]; exists {
				return nil, fmt.Errorf("alias %q collides with model name", alias)
			}
			r.aliases[alias] = mc.Name
		}
	}

	r.rebuildSorted()

	return r, nil
}

// Resolve looks up a model by its canonical name or by an alias. It returns a
// copy of the Model so callers cannot mutate the registry's internal state.
// ErrModelNotFound (wrapped) is returned when no match exists.
func (r *Registry) Resolve(nameOrAlias string) (Model, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if m, ok := r.models[nameOrAlias]; ok {
		return copyModel(m), nil
	}
	if canonical, ok := r.aliases[nameOrAlias]; ok {
		return copyModel(r.models[canonical]), nil
	}
	return Model{}, fmt.Errorf("resolve %q: %w", nameOrAlias, ErrModelNotFound)
}

// List returns all registered models sorted by name. Each element is a copy;
// callers may not mutate the registry's internal state through the returned slice.
// Use ListInfo when only public metadata is needed; List is for internal use only.
func (r *Registry) List() []Model {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]Model, len(r.sorted))
	for i, m := range r.sorted {
		result[i] = copyModel(m)
	}
	return result
}

// ModelInfo holds model metadata safe for external exposure.
// It omits sensitive fields like APIKey and BaseURL. Deployment details
// (including API keys and endpoint URLs) are never included.
type ModelInfo struct {
	Name             string
	Provider         string
	Type             string `json:"type"`
	Aliases          []string
	MaxContextTokens int
	// Strategy is the deployment selection strategy. Empty for single-deployment models.
	Strategy string `json:"strategy,omitempty"`
	// DeploymentCount is the number of configured deployments. Zero for
	// single-deployment models.
	DeploymentCount int `json:"deployment_count,omitempty"`
}

// ListInfo returns metadata for all registered models, omitting sensitive fields.
// The returned slice is sorted by name. Use this instead of List() wherever
// the caller does not need BaseURL or APIKey (e.g., the /v1/models endpoint).
func (r *Registry) ListInfo() []ModelInfo {
	r.mu.RLock()
	defer r.mu.RUnlock()

	result := make([]ModelInfo, len(r.sorted))
	for i, m := range r.sorted {
		aliases := make([]string, len(m.Aliases))
		copy(aliases, m.Aliases)
		result[i] = ModelInfo{
			Name:             m.Name,
			Provider:         m.Provider,
			Type:             m.Type,
			Aliases:          aliases,
			MaxContextTokens: m.MaxContextTokens,
			Strategy:         m.Strategy,
			DeploymentCount:  len(m.Deployments),
		}
	}
	return result
}

// AddModel adds or replaces a model in the registry and updates aliases and the
// sorted list. If a model with the same name already existed, its old aliases
// are removed before the new ones are registered.
func (r *Registry) AddModel(m Model) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Remove aliases belonging to the existing model with this name, if any.
	if old, exists := r.models[m.Name]; exists {
		for _, alias := range old.Aliases {
			delete(r.aliases, alias)
		}
	}

	aliases := make([]string, len(m.Aliases))
	copy(aliases, m.Aliases)

	deployments := make([]Deployment, len(m.Deployments))
	copy(deployments, m.Deployments)

	entry := &Model{
		Name:             m.Name,
		Provider:         m.Provider,
		Type:             m.Type,
		BaseURL:          m.BaseURL,
		APIKey:           m.APIKey,
		Aliases:          aliases,
		MaxContextTokens: m.MaxContextTokens,
		Pricing:          m.Pricing,
		AzureDeployment:  m.AzureDeployment,
		AzureAPIVersion:  m.AzureAPIVersion,
		Timeout:          m.Timeout,
		Strategy:         m.Strategy,
		MaxRetries:       m.MaxRetries,
		Deployments:      deployments,
	}
	r.models[m.Name] = entry

	for _, alias := range aliases {
		r.aliases[alias] = m.Name
	}

	r.rebuildSorted()
}

// RemoveModel removes a model by name and all of its aliases from the registry.
// If the model does not exist, RemoveModel is a no-op.
func (r *Registry) RemoveModel(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	m, exists := r.models[name]
	if !exists {
		return
	}

	for _, alias := range m.Aliases {
		delete(r.aliases, alias)
	}
	delete(r.models, name)

	r.rebuildSorted()
}

// copyModel returns a deep copy of m so callers cannot mutate the registry's
// internal state. Slices (Aliases, Deployments) are copied into new backing arrays.
func copyModel(m *Model) Model {
	result := *m
	result.Aliases = make([]string, len(m.Aliases))
	copy(result.Aliases, m.Aliases)
	result.Deployments = make([]Deployment, len(m.Deployments))
	copy(result.Deployments, m.Deployments)
	return result
}

// rebuildSorted rebuilds the pre-sorted slice of model pointers from the models map.
// It must be called with r.mu held for writing.
func (r *Registry) rebuildSorted() {
	r.sorted = make([]*Model, 0, len(r.models))
	for _, m := range r.models {
		r.sorted = append(r.sorted, m)
	}
	sort.Slice(r.sorted, func(i, j int) bool {
		return r.sorted[i].Name < r.sorted[j].Name
	})
}
