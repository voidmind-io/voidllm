// Package health implements periodic upstream model health monitoring.
// It probes registered models at three configurable levels and exposes the
// results via GetHealth / GetAllHealth for the admin API and Prometheus metrics.
package health

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/jsonx"
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
	// LastError holds the error message from the most recently failed probe,
	// or is empty when all probes passed.
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
}

// Checker periodically probes all models registered in the proxy.Registry at
// up to three configurable levels and stores the results in memory. All methods
// are safe for concurrent use.
type Checker struct {
	registry *proxy.Registry
	results  sync.Map // map[string]*ModelHealth — keyed by probeTarget.key, replaced atomically
	cfg      config.HealthCheckConfig
	client   *http.Client
	log      *slog.Logger
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
// stored values are never mutated after being placed in the map.
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
			}
			c.runOne(t, level)
		}
	}
}

// runOne executes a single probe for the given target at the given level and
// atomically replaces the stored ModelHealth using copy-on-write to avoid data
// races.
func (c *Checker) runOne(t probeTarget, level probeLevel) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	latencyMs, err := execProbe(ctx, c.client, t, level)

	// Load existing or create a zero value to copy from.
	existing, _ := c.results.LoadOrStore(t.key, &ModelHealth{ModelName: t.key, Status: "unknown"})
	old := existing.(*ModelHealth)

	// Copy-on-write: mutate the copy, then store atomically. This eliminates
	// the data race that would occur if multiple probe-level goroutines
	// mutated the same *ModelHealth in place.
	updated := *old
	updated.LastCheck = time.Now().UTC()

	ok := err == nil
	if ok {
		updated.LatencyMs = latencyMs
		updated.LastError = ""
	} else {
		updated.LastError = sanitizeError(err)
		c.log.LogAttrs(ctx, slog.LevelDebug, "health probe failed",
			slog.String("key", t.key),
			slog.String("error", updated.LastError),
		)
	}

	switch level {
	case levelHealth:
		updated.HealthOK = &ok
		if ok {
			updated.LatencyMs = latencyMs
		}
	case levelModels:
		updated.ModelsOK = &ok
	case levelFunctional:
		updated.FunctionalOK = &ok
	}

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

// probeModels performs a GET to <base_url>/models and returns success on any
// 2xx HTTP response.
func probeModels(ctx context.Context, client *http.Client, t probeTarget) (int64, error) {
	rawURL := strings.TrimRight(t.baseURL, "/") + "/models"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}

	setAuthHeaders(req, t)

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

// probeFunctional dispatches to the appropriate functional probe based on the
// target's model type. Image, audio_transcription, and tts models are skipped
// because they are too expensive or require special binary input to probe
// meaningfully.
func probeFunctional(ctx context.Context, client *http.Client, t probeTarget) (int64, error) {
	switch t.modelType {
	case "embedding":
		return probeEmbedding(ctx, client, t)
	case "reranking", "image", "audio_transcription", "tts":
		// Skip — incompatible endpoint or too expensive to probe.
		return 0, nil
	default: // "chat", "completion", ""
		return probeFunctionalChat(ctx, client, t)
	}
}

// probeFunctionalChat performs a minimal POST /chat/completions request with a
// single-token max to verify end-to-end functionality of the upstream model.
// For Azure targets the deployment name is used as the model identifier.
func probeFunctionalChat(ctx context.Context, client *http.Client, t probeTarget) (int64, error) {
	rawURL := strings.TrimRight(t.baseURL, "/") + "/chat/completions"

	upstreamModel := t.modelName
	if t.provider == "azure" && t.azureDeployment != "" {
		upstreamModel = t.azureDeployment
	}

	payload := map[string]any{
		"model":      upstreamModel,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
	}
	body, err := jsonx.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	setAuthHeaders(req, t)

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

// probeEmbedding performs a minimal POST /embeddings request with a single
// short input string to verify end-to-end functionality of an embedding model.
func probeEmbedding(ctx context.Context, client *http.Client, t probeTarget) (int64, error) {
	rawURL := strings.TrimRight(t.baseURL, "/") + "/embeddings"

	payload := map[string]any{
		"model": t.modelName,
		"input": "test",
	}
	body, err := jsonx.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	setAuthHeaders(req, t)

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
// the target's provider. Anthropic uses the x-api-key header scheme; all other
// providers use Bearer token authorization.
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
