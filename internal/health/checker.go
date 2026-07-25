// Package health implements periodic upstream model health monitoring.
// It probes registered models at three configurable levels and exposes the
// results via GetHealth / GetAllHealth for the admin API and Prometheus metrics.
package health

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/metrics"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

// probeLevel identifies which probe produced a result so that the correct
// field on ModelHealth can be updated.
type probeLevel int

const (
	levelHealth probeLevel = iota
	levelModels
	levelFunctional
)

// probeTimeout is the per-request context deadline for all probe types.
const probeTimeout = 10 * time.Second

// probeTarget holds the endpoint-specific fields needed to execute a single
// probe. For single-deployment models key equals the model name; for
// multi-deployment models key is "modelName/deploymentName".
type probeTarget struct {
	// key is the sync.Map key and the Prometheus label value. It equals
	// modelName for single-deployment models or "modelName/deploymentName"
	// for each deployment within a multi-deployment model.
	key             string
	modelName       string
	provider        string
	baseURL         string
	apiKey          string
	modelType       string
	azureDeployment string
	azureAPIVersion string
	// gcpProject is the Google Cloud project ID. Non-empty only for provider "vertex".
	gcpProject string
	// gcpLocation is the Google Cloud region. Non-empty only for provider "vertex".
	gcpLocation string
}

// asProbeTarget converts t to the exported ProbeTarget shape that
// BuildProbeRequest accepts. key and modelType are Checker-internal
// dispatch/labelling fields with no bearing on request construction and are
// intentionally not carried over.
func (t probeTarget) asProbeTarget() ProbeTarget {
	return ProbeTarget{
		ModelName:       t.modelName,
		Provider:        t.provider,
		BaseURL:         t.baseURL,
		APIKey:          t.apiKey,
		AzureDeployment: t.azureDeployment,
		AzureAPIVersion: t.azureAPIVersion,
		GCPProject:      t.gcpProject,
		GCPLocation:     t.gcpLocation,
	}
}

// ModelHealth holds the most recent health state for a single upstream model.
type ModelHealth struct {
	// ModelName is the canonical registry name of the model.
	ModelName string `json:"name"`
	// Status is the overall health classification derived from all enabled
	// probe results: "healthy", "degraded", "unhealthy", or "unknown".
	Status string `json:"status"`
	// LastCheck is the UTC timestamp of the most recent probe cycle.
	LastCheck time.Time `json:"last_check"`
	// LastError is derived from HealthError, ModelsError, and FunctionalError
	// using the same health > models > functional priority as Status, for
	// consumers that display a single error value. It is empty when every
	// enabled and applicable probe is currently passing. Prefer HealthError,
	// ModelsError, and FunctionalError when it matters which probe produced
	// the message.
	LastError string `json:"last_error,omitempty"`
	// LatencyMs is the round-trip time of the most recent successful probe
	// in milliseconds. Zero when no probe has succeeded yet.
	LatencyMs int64 `json:"latency_ms"`
	// HealthOK is nil when the health probe is disabled; otherwise it reflects
	// whether the last GET / probe could reach the server.
	HealthOK *bool `json:"health_ok"`
	// ModelsOK is nil when the models probe is disabled; otherwise it reflects
	// whether the last GET /models probe returned a 2xx response.
	ModelsOK *bool `json:"models_ok"`
	// FunctionalOK is nil when the functional probe is disabled; otherwise it
	// reflects whether the last POST /chat/completions probe returned a 2xx
	// response.
	FunctionalOK *bool `json:"functional_ok"`
	// HealthError holds the sanitized error from the most recent health
	// probe. It is empty when the probe is disabled, has not yet run, or
	// last succeeded.
	HealthError string `json:"health_error,omitempty"`
	// ModelsError holds the sanitized error from the most recent models
	// probe. It is empty when the probe is disabled, not applicable to the
	// target's provider, has not yet run, or last succeeded.
	ModelsError string `json:"models_error,omitempty"`
	// FunctionalError holds the sanitized error from the most recent
	// functional probe. It is empty when the probe is disabled, not
	// applicable to the target's model type, has not yet run, or last
	// succeeded.
	FunctionalError string `json:"functional_error,omitempty"`
}

// Checker periodically probes all models registered in the proxy.Registry at
// up to three configurable levels and stores the results in memory. All methods
// are safe for concurrent use.
//
// results is a sync.Map so that GetHealth and GetAllHealth stay lock-free on
// the request hot path. mu protects only the load-copy-mutate-store sequence
// in runOne: up to three probe levels (health, models, functional) run on
// independent tickers and can race to update the same key, and without
// serialization one level's Store can silently overwrite a concurrent
// update from another level (a lost update, not a data race — copying the
// struct before mutating already prevents the latter). Readers never take
// mu; they only ever observe a fully-formed *ModelHealth that runOne
// published after releasing it.
type Checker struct {
	registry *proxy.Registry
	results  sync.Map // map[string]*ModelHealth — keyed by probeTarget.key, replaced atomically
	// mu serializes the read-modify-write sequence in runOne across
	// concurrently running probe levels. It is never held during network
	// I/O (execProbe) and is never taken by readers (GetHealth,
	// GetAllHealth) — see the Checker doc comment.
	mu     sync.Mutex
	cfg    config.HealthCheckConfig
	client *http.Client
	log    *slog.Logger
}

// NewChecker constructs a Checker that will probe the models in registry
// according to cfg. The http.Client used for probes relies on per-request
// context timeouts (probeTimeout) and does not follow redirects.
func NewChecker(registry *proxy.Registry, cfg config.HealthCheckConfig, log *slog.Logger) *Checker {
	return &Checker{
		registry: registry,
		cfg:      cfg,
		client: &http.Client{
			Transport: &http.Transport{
				TLSHandshakeTimeout: 10 * time.Second,
				IdleConnTimeout:     90 * time.Second,
			},
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		log: log,
	}
}

// GetHealth returns the most recent health state for the given key. For
// single-deployment models key is the model name; for a specific deployment
// within a multi-deployment model key is "modelName/deploymentName". It
// returns nil and false when the target has not yet been probed.
// The returned pointer is safe to read without further synchronization —
// stored values are never mutated after being placed in the map. This holds
// even though writers serialize on Checker.mu: GetHealth intentionally does
// not acquire it, since the map only ever holds fully-formed snapshots.
func (c *Checker) GetHealth(key string) (*ModelHealth, bool) {
	v, ok := c.results.Load(key)
	if !ok {
		return nil, false
	}
	return v.(*ModelHealth), true
}

// GetAllHealth returns a snapshot of health state for every probe target that
// has been probed at least once. For models with multiple deployments each
// deployment appears as a separate entry. The returned slice is unordered.
func (c *Checker) GetAllHealth() []ModelHealth {
	var result []ModelHealth
	c.results.Range(func(_, v any) bool {
		mh := v.(*ModelHealth)
		result = append(result, *mh)
		return true
	})
	return result
}

// Start launches the enabled probe tickers. It immediately runs a first probe
// cycle for all models before waiting for the first tick interval. The returned
// function stops all tickers and waits for their goroutines to exit.
func (c *Checker) Start() func() {
	var stopFuncs []func()

	if c.cfg.Health.Enabled {
		c.runAll(levelHealth)
		stopFuncs = append(stopFuncs, c.startTicker(c.cfg.Health.Interval, func() {
			c.runAll(levelHealth)
		}))
	}

	if c.cfg.Models.Enabled {
		c.runAll(levelModels)
		stopFuncs = append(stopFuncs, c.startTicker(c.cfg.Models.Interval, func() {
			c.runAll(levelModels)
		}))
	}

	if c.cfg.Functional.Enabled {
		c.runAll(levelFunctional)
		stopFuncs = append(stopFuncs, c.startTicker(c.cfg.Functional.Interval, func() {
			c.runAll(levelFunctional)
		}))
	}

	return func() {
		for _, stop := range stopFuncs {
			stop()
		}
	}
}

// deploymentKey returns the sync.Map and Prometheus label key for a probe
// target. It mirrors router.DeploymentKey to avoid an import cycle: health
// imports proxy, router imports health, so health must not import router.
// For single-deployment models the key is the model name. For a deployment
// within a multi-deployment model the key is "modelName/deploymentName".
func deploymentKey(modelName, deploymentName string) string {
	if deploymentName == modelName {
		return modelName
	}
	return modelName + "/" + deploymentName
}

// runAll executes the probe identified by level for every probe target derived
// from the registry. Single-deployment models produce one target keyed by the
// model name; multi-deployment models produce one target per deployment keyed
// by "modelName/deploymentName".
func (c *Checker) runAll(level probeLevel) {
	models := c.registry.List()
	for _, m := range models {
		if len(m.Deployments) == 0 {
			// Single-deployment: probe using model-level fields.
			t := probeTarget{
				key:             m.Name,
				modelName:       m.Name,
				provider:        m.Provider,
				baseURL:         m.BaseURL,
				apiKey:          m.APIKey,
				modelType:       m.Type,
				azureDeployment: m.AzureDeployment,
				azureAPIVersion: m.AzureAPIVersion,
				gcpProject:      m.GCPProject,
				gcpLocation:     m.GCPLocation,
			}
			c.runOne(t, level)
			continue
		}
		// Multi-deployment: one goroutine per deployment.
		for _, d := range m.Deployments {
			t := probeTarget{
				key:             deploymentKey(m.Name, d.Name),
				modelName:       m.Name,
				provider:        d.Provider,
				baseURL:         d.BaseURL,
				apiKey:          d.APIKey,
				modelType:       m.Type,
				azureDeployment: d.AzureDeployment,
				azureAPIVersion: d.AzureAPIVersion,
				gcpProject:      d.GCPProject,
				gcpLocation:     d.GCPLocation,
			}
			c.runOne(t, level)
		}
	}
}

// runOne executes a single probe for the given target at the given level and
// atomically replaces the stored ModelHealth using copy-on-write.
func (c *Checker) runOne(t probeTarget, level probeLevel) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	latencyMs, err := execProbe(ctx, c.client, t, level)

	// From here on we only touch in-memory state — no network I/O — so hold
	// mu for the remainder of the function. Health, models, and functional
	// probes run on independent tickers and can reach this point for the
	// same key concurrently; without the lock, two levels could both load
	// the same old snapshot and the second Store would silently discard the
	// first level's update (a lost update). Copy-on-write alone prevents a
	// data race on the struct fields, but not this lost-update race between
	// levels — mu is what serializes the load-copy-mutate-store sequence.
	c.mu.Lock()
	defer c.mu.Unlock()

	// Load existing or create a zero value to copy from.
	existing, _ := c.results.LoadOrStore(t.key, &ModelHealth{ModelName: t.key, Status: "unknown"})
	old := existing.(*ModelHealth)

	// Copy-on-write: mutate the copy, then store atomically, so a concurrent
	// reader that already holds the old pointer (see GetHealth) never
	// observes a partially-updated struct.
	updated := *old
	updated.LastCheck = time.Now().UTC()

	if errors.Is(err, ErrProbeNotApplicable) {
		// This probe has no meaningful equivalent for the target's provider
		// or model type (e.g. a models-list probe against Gemini, or a
		// functional probe against an image model). Leave the level's field
		// nil — deriveStatus already treats nil as "not checked" — rather
		// than recording a success that never actually ran, or a failure
		// that would wrongly drag the status to degraded/unhealthy. Also
		// clear the level's own error field: a probe that just became
		// not-applicable (e.g. after a model type edit) must not keep
		// displaying a stale failure from when it was still applicable.
		//
		// levelHealth never appears here: probeHealth pings the bare server
		// root directly and never routes through BuildProbeRequest, so it
		// can never return ErrProbeNotApplicable.
		switch level {
		case levelModels:
			updated.ModelsOK = nil
			updated.ModelsError = ""
		case levelFunctional:
			updated.FunctionalOK = nil
			updated.FunctionalError = ""
		}
		updated.LastError = deriveLastError(&updated)
		updated.Status = deriveStatus(&updated)
		c.results.Store(t.key, &updated)
		updateMetrics(t.key, &updated)
		return
	}

	ok := err == nil
	var sanitized string
	if ok {
		updated.LatencyMs = latencyMs
	} else {
		sanitized = sanitizeError(err)
		c.log.LogAttrs(ctx, slog.LevelDebug, "health probe failed",
			slog.String("key", t.key),
			slog.String("error", sanitized),
		)
	}

	// Each level owns its own error field. Assigning sanitized (empty on
	// success) here, rather than sharing one field across levels, ensures a
	// successful run of one probe never wipes a genuine failure recorded by
	// another.
	switch level {
	case levelHealth:
		updated.HealthOK = &ok
		updated.HealthError = sanitized
	case levelModels:
		updated.ModelsOK = &ok
		updated.ModelsError = sanitized
	case levelFunctional:
		updated.FunctionalOK = &ok
		updated.FunctionalError = sanitized
	}

	updated.LastError = deriveLastError(&updated)
	updated.Status = deriveStatus(&updated)
	c.results.Store(t.key, &updated)

	updateMetrics(t.key, &updated)
}

// execProbe dispatches to the appropriate probe function for the given level.
func execProbe(ctx context.Context, client *http.Client, t probeTarget, level probeLevel) (int64, error) {
	switch level {
	case levelHealth:
		return probeHealth(ctx, client, t)
	case levelModels:
		return probeModels(ctx, client, t)
	case levelFunctional:
		return probeFunctional(ctx, client, t)
	default:
		return 0, fmt.Errorf("unknown probe level %d", level)
	}
}

// sanitizeError converts a raw error message to a safe, low-information string
// that does not expose internal URLs, IP addresses, or stack details.
func sanitizeError(err error) string {
	msg := err.Error()
	if strings.Contains(msg, "connection refused") {
		return "connection refused"
	}
	if strings.Contains(msg, "connection reset") {
		return "connection reset"
	}
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline exceeded") {
		return "request timeout"
	}
	if strings.Contains(msg, "no such host") {
		return "dns resolution failed"
	}
	if strings.Contains(msg, "tls") || strings.Contains(msg, "certificate") {
		return "tls error"
	}
	if strings.HasPrefix(msg, "http ") {
		// e.g. "http 401" — safe to surface, contains no internal details.
		return msg
	}
	return "probe failed"
}

// serverRoot extracts the scheme + host from a base URL, stripping any path
// like "/v1". This gives us the actual server root for health pings.
func serverRoot(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return strings.TrimRight(baseURL, "/")
	}
	return u.Scheme + "://" + u.Host
}

// probeHealth performs a GET to the target's server root. Any HTTP response —
// even 4xx or 5xx — is treated as success because receiving a response means
// the server is reachable. Only connection-level errors (refused, timeout,
// DNS failure) indicate an unhealthy host.
func probeHealth(ctx context.Context, client *http.Client, t probeTarget) (int64, error) {
	rawURL := serverRoot(t.baseURL)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}

	setAuthHeaders(req, t)

	start := time.Now()
	resp, err := client.Do(req)
	latencyMs := time.Since(start).Milliseconds()
	if latencyMs == 0 {
		latencyMs = 1 // sub-millisecond response, show as 1ms rather than 0
	}
	if err != nil {
		return 0, fmt.Errorf("connection failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	// Any HTTP response means the server is reachable — healthy.
	return latencyMs, nil
}

// probeModels performs a GET to the provider's model-listing endpoint (built
// by BuildProbeRequest, provider-aware) and returns success on any 2xx HTTP
// response. It returns ErrProbeNotApplicable for providers whose adapter has
// no meaningful model-listing endpoint (azure, vertex, gemini) — see
// BuildProbeRequest's doc for the full rationale.
func probeModels(ctx context.Context, client *http.Client, t probeTarget) (int64, error) {
	return probeWithIntent(ctx, client, IntentModelsList, t)
}

// probeFunctional dispatches to the appropriate functional probe based on the
// target's model type. Image, audio_transcription, and tts models are skipped
// (ErrProbeNotApplicable) because they are too expensive or require special
// binary input to probe meaningfully.
func probeFunctional(ctx context.Context, client *http.Client, t probeTarget) (int64, error) {
	switch t.modelType {
	case "embedding":
		return probeWithIntent(ctx, client, IntentEmbeddings, t)
	case "reranking", "image", "audio_transcription", "tts":
		// Skip — incompatible endpoint or too expensive to probe.
		return 0, ErrProbeNotApplicable
	default: // "chat", "completion", ""
		return probeWithIntent(ctx, client, IntentChat, t)
	}
}

// probeWithIntent builds a provider-aware probe request for intent via
// BuildProbeRequest and executes it, returning ErrProbeNotApplicable
// unchanged so callers (probeFunctional, runOne) can distinguish "skipped"
// from "failed".
func probeWithIntent(ctx context.Context, client *http.Client, intent ProbeIntent, t probeTarget) (int64, error) {
	req, err := BuildProbeRequest(ctx, intent, t.asProbeTarget())
	if err != nil {
		if errors.Is(err, ErrProbeNotApplicable) {
			return 0, err
		}
		return 0, fmt.Errorf("build request: %w", err)
	}
	return doProbeRequest(client, req)
}

// doProbeRequest executes req and returns success on any 2xx HTTP response.
// It is shared by every probe that treats "2xx" as the pass/fail boundary
// (models-list, functional chat, functional embeddings).
func doProbeRequest(client *http.Client, req *http.Request) (int64, error) {
	start := time.Now()
	resp, err := client.Do(req)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		return 0, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, fmt.Errorf("http %d", resp.StatusCode)
	}
	return latencyMs, nil
}

// setAuthHeaders adds the appropriate authentication headers to req based on
// the target's provider. It is used only by probeHealth: probeModels and
// probeFunctional build their requests via BuildProbeRequest, which sets
// provider-correct headers through the same proxy.Adapter.SetHeaders logic
// the real proxy hot path uses. probeHealth intentionally stays independent
// of BuildProbeRequest because it hits the bare server root, not a
// provider-shaped endpoint, and treats any HTTP response as success — the
// exact header scheme used barely matters for that check, but Anthropic's
// x-api-key scheme is still applied for parity with a normal request.
func setAuthHeaders(req *http.Request, t probeTarget) {
	if t.apiKey == "" {
		return
	}
	if t.provider == "anthropic" {
		req.Header.Set("x-api-key", t.apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+t.apiKey)
	}
}

// deriveLastError picks the single error message to expose as LastError for
// consumers that show one value. It follows the same precedence as
// deriveStatus: a failed reachability probe outranks a failed models probe,
// which outranks a failed functional probe, so the reported message always
// describes the most severe current failure. It returns an empty string when
// every enabled and applicable probe is passing.
func deriveLastError(h *ModelHealth) string {
	if h.HealthError != "" {
		return h.HealthError
	}
	if h.ModelsError != "" {
		return h.ModelsError
	}
	return h.FunctionalError
}

// deriveStatus computes the overall status from the individual probe results.
func deriveStatus(h *ModelHealth) string {
	// If the health (reachability) probe was checked and failed → unhealthy.
	if h.HealthOK != nil && !*h.HealthOK {
		return "unhealthy"
	}
	// If any checked probe failed → degraded.
	if (h.ModelsOK != nil && !*h.ModelsOK) || (h.FunctionalOK != nil && !*h.FunctionalOK) {
		return "degraded"
	}
	// If at least one probe was checked and all passed → healthy.
	if h.HealthOK != nil || h.ModelsOK != nil || h.FunctionalOK != nil {
		return "healthy"
	}
	return "unknown"
}

// updateMetrics refreshes the Prometheus gauges for the given key after a
// probe cycle completes. All status label values are reset to zero before
// setting the current status to avoid stale time series.
func updateMetrics(key string, mh *ModelHealth) {
	// Reset all status labels to avoid stale series from previous status values.
	for _, s := range []string{"healthy", "degraded", "unhealthy", "unknown"} {
		metrics.ModelHealthStatus.WithLabelValues(key, s).Set(0)
	}

	var val float64
	switch mh.Status {
	case "healthy":
		val = 1
	case "degraded":
		val = 0.5
	default: // "unhealthy" or "unknown"
		val = 0
	}
	metrics.ModelHealthStatus.WithLabelValues(key, mh.Status).Set(val)

	if mh.LatencyMs > 0 {
		metrics.ModelHealthLatencySeconds.WithLabelValues(key).Set(float64(mh.LatencyMs) / 1000)
	}
}

// startTicker runs fn on the given interval and returns a stop function that
// signals the goroutine to exit and waits for it to finish.
func (c *Checker) startTicker(interval time.Duration, fn func()) func() {
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				fn()
			case <-done:
				return
			}
		}
	}()
	return func() { close(done); wg.Wait() }
}
