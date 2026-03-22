package circuitbreaker

import "sync"

// Registry manages per-model circuit breakers. A single Registry instance is
// shared across all proxy handler goroutines. Breakers are created lazily on
// first access and never removed — the set of active model names is bounded by
// the model registry, so unbounded growth is not a concern.
type Registry struct {
	mu       sync.RWMutex
	breakers map[string]*Breaker
	cfg      Config
}

// NewRegistry creates a Registry that constructs each Breaker with cfg as the
// default configuration.
func NewRegistry(cfg Config) *Registry {
	return &Registry{
		breakers: make(map[string]*Breaker),
		cfg:      cfg,
	}
}

// Get returns the Breaker for the given model name. If no Breaker exists for
// that model yet, one is created with the Registry's default configuration and
// stored for future calls. Get is safe for concurrent use.
func (r *Registry) Get(modelName string) *Breaker {
	r.mu.RLock()
	b, ok := r.breakers[modelName]
	r.mu.RUnlock()
	if ok {
		return b
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check: another goroutine may have inserted the entry between the
	// read-unlock above and acquiring the write lock.
	if b, ok = r.breakers[modelName]; ok {
		return b
	}
	b = NewBreaker(r.cfg)
	r.breakers[modelName] = b
	return b
}
