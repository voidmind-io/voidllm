// Package cooldown tracks upstream deployments that are temporarily
// rate-limited (HTTP 429) so that the router can prefer other deployments for
// a short window without treating the rate limit as a circuit-breaker
// failure. It intentionally imports neither internal/proxy nor
// internal/router so it can be used by both — the same constraint that
// shapes internal/circuitbreaker.
package cooldown

import (
	"sync"
	"time"
)

// Registry tracks, per deployment key, the time until which a deployment is
// considered "cooling" (temporarily rate-limited by its upstream). A single
// Registry instance is shared across all proxy handler and router goroutines.
// All methods are nil-receiver safe so callers never need to nil-check a
// *Registry before use.
type Registry struct {
	mu    sync.RWMutex
	until map[string]time.Time
	// now returns the current time. Defaults to time.Now; overridden in tests.
	now func() time.Time
}

// NewRegistry creates an empty Registry ready for use.
func NewRegistry() *Registry {
	return &Registry{
		until: make(map[string]time.Time),
		now:   time.Now,
	}
}

// Mark records that the deployment identified by key is cooling down for the
// duration d, i.e. Cooling(key) reports true until d has elapsed. A
// non-positive d is a no-op — it never shortens or clears an existing
// cooldown. Mark is safe for concurrent use and safe to call on a nil
// Registry (no-op).
func (r *Registry) Mark(key string, d time.Duration) {
	if r == nil || d <= 0 {
		return
	}
	until := r.now().Add(d)

	r.mu.Lock()
	defer r.mu.Unlock()
	r.until[key] = until
}

// Cooling reports whether the deployment identified by key is currently
// within its cooldown window. It performs a pure time comparison under a
// read lock and never allocates, so it is safe to call on the hot path once
// per candidate per request. Cooling returns false on a nil Registry and for
// any key that was never marked or whose cooldown has expired.
func (r *Registry) Cooling(key string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	until, ok := r.until[key]
	r.mu.RUnlock()
	if !ok {
		return false
	}
	return r.now().Before(until)
}
