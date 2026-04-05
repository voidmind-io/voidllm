// Package router selects upstream deployment candidates for a model based on
// routing strategy, health status, and circuit breaker state. It is used by
// the proxy handler to build the ordered list of endpoints to attempt for a
// given request.
package router

import (
	"math/rand/v2"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/voidmind-io/voidllm/internal/circuitbreaker"
	"github.com/voidmind-io/voidllm/internal/health"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

// Router selects deployments for a model based on routing strategy,
// health status, and circuit breaker state.
type Router struct {
	healthChecker   *health.Checker
	circuitBreakers *circuitbreaker.Registry
	counters        sync.Map // model name → *atomic.Uint64 (round-robin)
}

// NewRouter creates a Router. Both dependencies are optional (nil-safe).
func NewRouter(hc *health.Checker, cb *circuitbreaker.Registry) *Router {
	return &Router{
		healthChecker:   hc,
		circuitBreakers: cb,
	}
}

// DeploymentKey returns the key used for circuit breakers and health lookups.
// For single-deployment models, returns the model name.
// For multi-deployment models, returns "model/deployment".
func DeploymentKey(modelName, deploymentName string) string {
	if deploymentName == modelName {
		return modelName
	}
	return modelName + "/" + deploymentName
}

// Pick returns an ordered list of deployment candidates for the given model.
// The caller should try each candidate in order until one succeeds.
// For single-deployment models (no Deployments slice), Pick synthesizes
// a single-element slice from the model's own fields.
func (r *Router) Pick(model proxy.Model) []proxy.Deployment {
	// Single-deployment fast path: synthesize from model-level fields.
	if len(model.Deployments) == 0 {
		return []proxy.Deployment{{
			Name:            model.Name,
			Provider:        model.Provider,
			BaseURL:         model.BaseURL,
			APIKey:          model.APIKey,
			AzureDeployment: model.AzureDeployment,
			AzureAPIVersion: model.AzureAPIVersion,
			GCPProject:      model.GCPProject,
			GCPLocation:     model.GCPLocation,
			AWSRegion:       model.AWSRegion,
			AWSAccessKey:    model.AWSAccessKey,
			AWSSecretKey:    model.AWSSecretKey,
			AWSSessionToken: model.AWSSessionToken,
			Weight:          1,
			Priority:        0,
		}}
	}

	// Filter deployments by circuit breaker and health state.
	available := r.filterAvailable(model)

	// Last-resort fallback: if every deployment was filtered out, use all of
	// them so the caller at least gets to try — failing visibly is better than
	// silently dropping the request.
	candidates := available
	if len(candidates) == 0 {
		candidates = make([]proxy.Deployment, len(model.Deployments))
		copy(candidates, model.Deployments)
	}

	// Order candidates according to the model's routing strategy.
	var ordered []proxy.Deployment
	switch model.Strategy {
	case "least-latency":
		ordered = r.leastLatency(model.Name, candidates)
	case "weighted":
		ordered = r.weighted(candidates)
	case "priority":
		ordered = r.priority(candidates)
	default: // "round-robin" or ""
		ordered = r.roundRobin(model.Name, candidates)
	}

	// Limit result length to maxRetries+1 so the caller does not attempt more
	// deployments than the model policy allows. Zero means try all candidates.
	limit := len(ordered)
	if model.MaxRetries > 0 && model.MaxRetries+1 < limit {
		limit = model.MaxRetries + 1
	}
	return ordered[:limit]
}

// filterAvailable returns the subset of deployments whose circuit breaker
// allows requests AND whose health status (if known) is not "unhealthy".
func (r *Router) filterAvailable(model proxy.Model) []proxy.Deployment {
	result := make([]proxy.Deployment, 0, len(model.Deployments))
	for _, d := range model.Deployments {
		key := DeploymentKey(model.Name, d.Name)

		// Circuit breaker check — nil registry means feature is disabled.
		if r.circuitBreakers != nil {
			if !r.circuitBreakers.Get(key).Allow() {
				continue
			}
		}

		// Health check — nil checker means health monitoring is disabled.
		if r.healthChecker != nil {
			if mh, ok := r.healthChecker.GetHealth(key); ok && mh.Status == "unhealthy" {
				continue
			}
		}

		result = append(result, d)
	}
	return result
}

// roundRobin rotates through candidates using a per-model atomic counter so
// successive calls distribute load evenly across all available deployments.
func (r *Router) roundRobin(modelName string, candidates []proxy.Deployment) []proxy.Deployment {
	counter := r.getCounter(modelName)
	idx := int(counter.Add(1)-1) % len(candidates)
	return rotate(candidates, idx)
}

// leastLatency sorts candidates by ascending health-checker latency. If the
// health checker is not configured, or no latency data exists for any
// candidate, it falls back to round-robin.
func (r *Router) leastLatency(modelName string, candidates []proxy.Deployment) []proxy.Deployment {
	if r.healthChecker == nil {
		return r.roundRobin(modelName, candidates)
	}

	// Build a copy so we do not sort the caller's slice.
	ordered := make([]proxy.Deployment, len(candidates))
	copy(ordered, candidates)

	// Collect latency values keyed by deployment name for O(1) access during
	// the sort comparison. Zero means no data yet.
	latency := make(map[string]int64, len(candidates))
	hasData := false
	for _, d := range candidates {
		key := DeploymentKey(modelName, d.Name)
		if mh, ok := r.healthChecker.GetHealth(key); ok && mh.LatencyMs > 0 {
			latency[d.Name] = mh.LatencyMs
			hasData = true
		}
	}

	if !hasData {
		return r.roundRobin(modelName, candidates)
	}

	sort.SliceStable(ordered, func(i, j int) bool {
		li, lj := latency[ordered[i].Name], latency[ordered[j].Name]
		if li == 0 {
			return false
		}
		if lj == 0 {
			return true
		}
		return li < lj
	})
	return ordered
}

// weighted performs a weighted-random selection of the first candidate, then
// returns the remaining deployments in a shuffled order for fallback attempts.
func (r *Router) weighted(candidates []proxy.Deployment) []proxy.Deployment {
	// Build cumulative weight array.
	total := 0
	for _, d := range candidates {
		w := d.Weight
		if w <= 0 {
			w = 1
		}
		total += w
	}

	// Pick a random number in [0, total) and find the corresponding deployment.
	pick := rand.IntN(total) //nolint:gosec // non-cryptographic selection is intentional
	cumulative := 0
	chosen := 0
	for i, d := range candidates {
		w := d.Weight
		if w <= 0 {
			w = 1
		}
		cumulative += w
		if pick < cumulative {
			chosen = i
			break
		}
	}

	// Build the result: chosen deployment first, then the rest shuffled.
	result := make([]proxy.Deployment, 0, len(candidates))
	result = append(result, candidates[chosen])

	rest := make([]proxy.Deployment, 0, len(candidates)-1)
	for i, d := range candidates {
		if i != chosen {
			rest = append(rest, d)
		}
	}
	rand.Shuffle(len(rest), func(i, j int) { rest[i], rest[j] = rest[j], rest[i] })
	return append(result, rest...)
}

// priority sorts candidates by Priority ascending so the lowest numeric value
// (highest priority) is tried first. A stable sort preserves the original
// ordering among deployments with equal priority values.
func (r *Router) priority(candidates []proxy.Deployment) []proxy.Deployment {
	ordered := make([]proxy.Deployment, len(candidates))
	copy(ordered, candidates)
	sort.SliceStable(ordered, func(i, j int) bool {
		return ordered[i].Priority < ordered[j].Priority
	})
	return ordered
}

// getCounter returns the atomic counter for modelName, creating it on first use.
func (r *Router) getCounter(modelName string) *atomic.Uint64 {
	v, _ := r.counters.LoadOrStore(modelName, new(atomic.Uint64))
	return v.(*atomic.Uint64)
}

// rotate returns a new slice that starts at index start and wraps around. It
// does not modify the original slice.
func rotate(s []proxy.Deployment, start int) []proxy.Deployment {
	n := len(s)
	if n == 0 {
		return nil
	}
	result := make([]proxy.Deployment, n)
	for i := range s {
		result[i] = s[(start+i)%n]
	}
	return result
}
