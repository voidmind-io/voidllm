package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofiber/fiber/v3"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/circuitbreaker"
	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/metrics"
	"github.com/voidmind-io/voidllm/internal/ratelimit"
	"github.com/voidmind-io/voidllm/internal/shutdown"
	"github.com/voidmind-io/voidllm/internal/usage"
)

// DeploymentPicker selects an ordered list of deployment candidates for a model.
// router.Router implements this interface; the indirection avoids an import
// cycle between the proxy and router packages (router already imports proxy).
type DeploymentPicker interface {
	Pick(model Model) []Deployment
}

// ProxyHandler forwards OpenAI-compatible requests to upstream LLM providers.
// It resolves model names via the Registry, rewrites the Authorization header
// with the upstream API key, and streams responses without buffering.
type ProxyHandler struct {
	Registry          *Registry
	AccessCache       *ModelAccessCache        // in-memory model access control; nil disables access checks
	AliasCache        *AliasCache              // in-memory scoped alias resolution; nil disables alias lookup
	CircuitBreakers   *circuitbreaker.Registry // per-model circuit breaker registry; nil disables circuit breaking
	Router            DeploymentPicker         // deployment selector; nil falls back to single-deployment behavior
	HTTPClient        *http.Client
	UsageLogger       *usage.Logger           // nil disables usage logging
	RateLimiter       ratelimit.Checker       // nil disables rate limiting
	TokenCounter      *ratelimit.TokenCounter // nil disables token budget enforcement
	ShutdownState     *shutdown.State         // nil disables in-flight tracking and graceful drain
	Tracer            trace.Tracer            // nil disables distributed tracing
	Log               *slog.Logger
	MaxRequestBody    int           // maximum allowed request body size in bytes
	MaxResponseBody   int           // maximum allowed non-streaming response body size in bytes
	MaxStreamDuration time.Duration // maximum duration for a streaming response
}

// NewProxyHandler constructs a ProxyHandler with a pre-configured HTTP client.
// The client follows no redirects (SSRF prevention). Client.Timeout is not set
// because it would cancel streaming reads mid-flight; instead, transport-level
// timeouts cap the connection and header phases only.
func NewProxyHandler(registry *Registry, log *slog.Logger) *ProxyHandler {
	httpClient := &http.Client{
		// No Timeout here — Client.Timeout kills streaming reads mid-flight.
		// Use Transport-level timeouts instead.
		Transport: &http.Transport{
			MaxIdleConnsPerHost:   100,
			IdleConnTimeout:       120 * time.Second,
			ResponseHeaderTimeout: 600 * time.Second, // wait up to 10min for upstream to start responding
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ForceAttemptHTTP2:     true, // enables HTTP/2 on custom Transport
			DisableCompression:    true, // prevents gzip-encoded SSE streams
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	return &ProxyHandler{
		Registry:   registry,
		HTTPClient: httpClient,
		Log:        log,
	}
}

// requestEnvelope extracts the fields from an incoming LLM API request that the
// proxy needs without fully parsing the body.
type requestEnvelope struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// errResponseSent is a package-private sentinel returned by helper methods that
// have already written a complete error response to the Fiber context. Handle
// maps this sentinel to nil before returning so that Fiber treats the response
// as already sent. No other package ever sees this value.
var errResponseSent = errors.New("response sent")

// defaultMaxRequestBody, defaultMaxResponseBody, and defaultMaxStreamDuration
// are the fallback limits used when ProxyHandler fields are zero (e.g. in
// tests that do not configure limits).
const (
	defaultMaxRequestBody    = 20 * 1024 * 1024  // 20 MB
	defaultMaxResponseBody   = 50 * 1024 * 1024  // 50 MB
	defaultMaxStreamDuration = 300 * time.Second // 5 minutes
)

// streamUsageExtractor observes OpenAI-format SSE lines and records the last
// usage object seen. Passthrough providers (vllm, custom) emit usage only on
// the final data chunk when stream_options.include_usage is set.
type streamUsageExtractor struct {
	lastUsage UsageInfo
}

// observe parses a single SSE line. Lines that are not JSON data lines or that
// carry no usage field are ignored without error.
func (s *streamUsageExtractor) observe(line []byte) {
	if !bytes.HasPrefix(line, []byte("data: {")) {
		return
	}
	data := line[len("data: "):]
	var chunk struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if jsonx.Unmarshal(data, &chunk) == nil && chunk.Usage != nil {
		s.lastUsage = UsageInfo{
			PromptTokens:     chunk.Usage.PromptTokens,
			CompletionTokens: chunk.Usage.CompletionTokens,
			TotalTokens:      chunk.Usage.TotalTokens,
		}
	}
}

// Handle is the hot-path proxy handler. It resolves the requested model,
// rewrites the body with the canonical model name, forwards the request to
// the upstream provider, and streams or buffers the response back to the client.
func (p *ProxyHandler) Handle(c fiber.Ctx) error {
	startTime := time.Now()

	// Root span covers the full proxy lifecycle for this request. The span is
	// started before the drain check so that even rejected requests are visible
	// in traces. When Tracer is nil, all span operations below are no-ops via
	// the otel.GetTextMapPropagator() and trace.SpanFromContext() no-op impls.
	if p.Tracer != nil {
		ctx, span := p.Tracer.Start(c.Context(), "proxy.handle")
		defer span.End()
		c.SetContext(ctx)
	}

	// Reject new requests immediately during graceful drain so the load
	// balancer can route them elsewhere before in-flight requests finish.
	if p.ShutdownState != nil && p.ShutdownState.Draining() {
		return apierror.Send(c, fiber.StatusServiceUnavailable,
			"service_unavailable", "server is shutting down")
	}

	trackDone := p.initShutdownTracking()
	defer trackDone()

	maxReqBody, maxRespBody, maxStreamDur := p.resolveEffectiveLimits()

	body, envelope, err := p.readAndValidateBody(c, maxReqBody)
	if err != nil {
		if errors.Is(err, errResponseSent) {
			return nil
		}
		return err
	}

	if p.Tracer != nil {
		trace.SpanFromContext(c.Context()).SetAttributes(
			attribute.String("model.requested", envelope.Model),
		)
	}

	keyInfo := auth.KeyInfoFromCtx(c)
	requestID := apierror.RequestIDFromCtx(c)

	if err := p.checkLimits(c, keyInfo); err != nil {
		if errors.Is(err, errResponseSent) {
			return nil
		}
		return err
	}

	model, err := p.resolveModel(c, keyInfo, envelope.Model)
	if err != nil {
		if errors.Is(err, errResponseSent) {
			return nil
		}
		return err
	}

	if p.Tracer != nil {
		trace.SpanFromContext(c.Context()).SetAttributes(
			attribute.String("model.canonical", model.Name),
			attribute.String("model.provider", model.Provider),
		)
	}

	// Build the ordered list of deployment candidates. When Router is nil or
	// the model has no multi-deployment configuration, synthesize a single
	// candidate from the model's own fields so the retry loop is uniform.
	var candidates []Deployment
	if p.Router != nil && len(model.Deployments) > 0 {
		candidates = p.Router.Pick(model)
	} else {
		candidates = []Deployment{{
			Name:            model.Name,
			Provider:        model.Provider,
			BaseURL:         model.BaseURL,
			APIKey:          model.APIKey,
			AzureDeployment: model.AzureDeployment,
			AzureAPIVersion: model.AzureAPIVersion,
			GCPProject:      model.GCPProject,
			GCPLocation:     model.GCPLocation,
			Weight:          1,
		}}
	}

	// Per-model timeout overrides the global stream duration limit when set.
	effectiveStreamDur := maxStreamDur
	if model.Timeout > 0 {
		effectiveStreamDur = model.Timeout
	}

	// req and cancelUpstream are set on the last successful buildUpstreamRequest
	// call. They are used below after the loop exits to route into the response
	// handlers. Both are nil if every candidate was skipped or failed during
	// request construction.
	var (
		req            *http.Request
		cancelUpstream context.CancelFunc
		adapter        Adapter
		resp           *http.Response
		lastErr        error
		usedDep        Deployment
	)

	for i, dep := range candidates {
		depKey := deploymentKey(model.Name, dep.Name)

		// Per-deployment circuit breaker check. The router's filterAvailable
		// already excludes open breakers when Router is non-nil, so we only
		// guard the synthesized single-candidate ourselves when Router is nil.
		if p.CircuitBreakers != nil && p.Router == nil {
			breaker := p.CircuitBreakers.Get(depKey)
			if !breaker.Allow() {
				metrics.CircuitBreakerRejectionsTotal.WithLabelValues(depKey).Inc()
				// Continue to the next candidate; if this is the only one,
				// the loop exits and we return the service-unavailable error
				// below.
				continue
			}
		}

		// Overlay the deployment's endpoint fields onto a copy of the resolved
		// model so buildUpstreamRequest uses the correct provider/URL/key.
		m := model
		applyDeployment(&m, dep)

		var buildErr error
		req, cancelUpstream, adapter, buildErr = p.buildUpstreamRequest(c, m, body, envelope)
		if buildErr != nil {
			if errors.Is(buildErr, errResponseSent) {
				return nil
			}
			return buildErr
		}

		// Send the request to the upstream. The upstream span measures
		// time-to-first-byte; Do() returns once response headers arrive.
		var doErr error
		if p.Tracer != nil {
			_, upstreamSpan := p.Tracer.Start(req.Context(), "proxy.upstream",
				trace.WithAttributes(
					attribute.String("http.request.method", req.Method),
					attribute.String("url.full", req.URL.String()),
				),
			)
			otel.GetTextMapPropagator().Inject(req.Context(), propagation.HeaderCarrier(req.Header))
			resp, doErr = p.HTTPClient.Do(req)
			if doErr != nil {
				upstreamSpan.RecordError(doErr)
				upstreamSpan.SetStatus(codes.Error, doErr.Error())
			} else {
				upstreamSpan.SetAttributes(attribute.Int("http.response.status_code", resp.StatusCode))
			}
			upstreamSpan.End()
		} else {
			resp, doErr = p.HTTPClient.Do(req)
		}

		if doErr != nil {
			// Connection-level error: transport failure, DNS, timeout.
			cancelUpstream()
			if p.CircuitBreakers != nil {
				p.CircuitBreakers.Get(depKey).RecordFailure()
			}
			metrics.UpstreamErrorsTotal.WithLabelValues(m.Name, m.Provider).Inc()
			p.Log.LogAttrs(c.Context(), slog.LevelWarn, "upstream request failed, retrying next deployment",
				slog.String("model", m.Name),
				slog.String("deployment", dep.Name),
				slog.String("provider", m.Provider),
				slog.Int("candidate", i),
				slog.String("error", doErr.Error()),
			)
			lastErr = doErr
			req = nil
			cancelUpstream = nil
			resp = nil
			metrics.RoutingRetriesTotal.WithLabelValues(model.Name, model.Strategy).Inc()
			continue
		}

		metrics.UpstreamRequestsTotal.WithLabelValues(m.Name, m.Provider, strconv.Itoa(resp.StatusCode)).Inc()

		if isRetryable(resp.StatusCode) && i < len(candidates)-1 {
			// 5xx response from upstream — try the next deployment. Drain
			// and close the body before moving on so the connection is
			// returned to the pool.
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			cancelUpstream()
			if p.CircuitBreakers != nil {
				p.CircuitBreakers.Get(depKey).RecordFailure()
			}
			metrics.UpstreamErrorsTotal.WithLabelValues(m.Name, m.Provider).Inc()
			p.Log.LogAttrs(c.Context(), slog.LevelWarn, "upstream returned retryable error, retrying next deployment",
				slog.String("model", m.Name),
				slog.String("deployment", dep.Name),
				slog.String("provider", m.Provider),
				slog.Int("candidate", i),
				slog.Int("status", resp.StatusCode),
			)
			lastErr = errors.New("upstream returned " + strconv.Itoa(resp.StatusCode))
			req = nil
			cancelUpstream = nil
			resp = nil
			metrics.RoutingRetriesTotal.WithLabelValues(model.Name, model.Strategy).Inc()
			continue
		}

		// Success or non-retryable status (4xx, or last retryable with no
		// more candidates). Record circuit breaker outcome for non-streaming
		// responses immediately; streaming outcome is recorded inside the
		// goroutine once the stream completes.
		if p.CircuitBreakers != nil && !isStreamingResponse(resp) {
			breaker := p.CircuitBreakers.Get(depKey)
			if resp.StatusCode >= 500 {
				breaker.RecordFailure()
			} else {
				breaker.RecordSuccess()
			}
		}

		usedDep = dep
		model = m // use the deployment-overlaid model for response handling
		break
	}

	// All candidates were exhausted without a usable response.
	if resp == nil {
		if lastErr != nil {
			return apierror.Send(c, fiber.StatusBadGateway, "upstream_unavailable", "upstream provider is unavailable")
		}
		// Every candidate was blocked by its circuit breaker.
		metrics.CircuitBreakerRejectionsTotal.WithLabelValues(model.Name).Inc()
		return apierror.Send(c, fiber.StatusServiceUnavailable,
			"circuit_open", "upstream temporarily unavailable")
	}

	_ = usedDep // deployment name available for future usage event enrichment

	if isStreamingResponse(resp) {
		var breaker *circuitbreaker.Breaker
		if p.CircuitBreakers != nil {
			breaker = p.CircuitBreakers.Get(deploymentKey(model.Name, usedDep.Name))
		}
		return p.handleStreamingResponse(c, resp, cancelUpstream, model,
			keyInfo, adapter, startTime, requestID, effectiveStreamDur, trackDone, breaker)
	}

	defer cancelUpstream()
	return p.handleBufferedResponse(c, resp, model, keyInfo, adapter,
		startTime, requestID, maxRespBody)
}

// resolveEffectiveLimits returns the effective request body, response body, and
// stream duration limits, substituting package defaults for any zero-valued fields.
func (p *ProxyHandler) resolveEffectiveLimits() (maxRequestBody, maxResponseBody int, maxStreamDuration time.Duration) {
	maxRequestBody = p.MaxRequestBody
	if maxRequestBody <= 0 {
		maxRequestBody = defaultMaxRequestBody
	}
	maxResponseBody = p.MaxResponseBody
	if maxResponseBody <= 0 {
		maxResponseBody = defaultMaxResponseBody
	}
	maxStreamDuration = p.MaxStreamDuration
	if maxStreamDuration <= 0 {
		maxStreamDuration = defaultMaxStreamDuration
	}
	return maxRequestBody, maxResponseBody, maxStreamDuration
}

// initShutdownTracking registers the request with ShutdownState and returns a
// trackDone callback that must be called exactly once when the request finishes.
// If ShutdownState is nil, a no-op function is returned.
// The returned callback is safe to call multiple times — a sync.Once inside
// ensures TrackDone is only forwarded once regardless of how many callers fire.
func (p *ProxyHandler) initShutdownTracking() func() {
	var trackOnce sync.Once
	trackDone := func() {}
	if p.ShutdownState != nil {
		p.ShutdownState.TrackStart()
		trackDone = func() {
			trackOnce.Do(p.ShutdownState.TrackDone)
		}
	}
	return trackDone
}

// readAndValidateBody reads the request body, enforces the size limit, and
// unmarshals the envelope fields needed by the proxy. On any error it sends
// an appropriate API error response and returns that error so Handle can
// return it immediately.
func (p *ProxyHandler) readAndValidateBody(c fiber.Ctx, maxRequestBody int) ([]byte, requestEnvelope, error) {
	body := c.Body()

	if len(body) > maxRequestBody {
		if err := apierror.Send(c, fiber.StatusRequestEntityTooLarge,
			"request_too_large", "request body exceeds size limit"); err != nil {
			return nil, requestEnvelope{}, err
		}
		return nil, requestEnvelope{}, errResponseSent
	}

	var envelope requestEnvelope
	if err := jsonx.Unmarshal(body, &envelope); err != nil || envelope.Model == "" {
		if err := apierror.Send(c, fiber.StatusBadRequest, "bad_request", "model field is required"); err != nil {
			return nil, requestEnvelope{}, err
		}
		return nil, requestEnvelope{}, errResponseSent
	}

	return body, envelope, nil
}

// checkLimits evaluates rate limits and token budgets for the authenticated key.
// It builds the three-tier Limits structs from keyInfo and delegates to the
// RateLimiter and TokenCounter. If either check rejects the request, an API
// error response is sent and the error is returned. Nil-safe for both
// RateLimiter and TokenCounter; a nil keyInfo is also safe and skips all checks.
func (p *ProxyHandler) checkLimits(c fiber.Ctx, keyInfo *auth.KeyInfo) error {
	if keyInfo == nil {
		return nil
	}

	keyLimits := ratelimit.Limits{
		RequestsPerMinute: keyInfo.RequestsPerMinute,
		RequestsPerDay:    keyInfo.RequestsPerDay,
		DailyTokenLimit:   keyInfo.DailyTokenLimit,
		MonthlyTokenLimit: keyInfo.MonthlyTokenLimit,
	}
	teamLimits := ratelimit.Limits{
		RequestsPerMinute: keyInfo.TeamRequestsPerMinute,
		RequestsPerDay:    keyInfo.TeamRequestsPerDay,
		DailyTokenLimit:   keyInfo.TeamDailyTokenLimit,
		MonthlyTokenLimit: keyInfo.TeamMonthlyTokenLimit,
	}
	orgLimits := ratelimit.Limits{
		RequestsPerMinute: keyInfo.OrgRequestsPerMinute,
		RequestsPerDay:    keyInfo.OrgRequestsPerDay,
		DailyTokenLimit:   keyInfo.OrgDailyTokenLimit,
		MonthlyTokenLimit: keyInfo.OrgMonthlyTokenLimit,
	}

	if p.RateLimiter != nil {
		if err := p.RateLimiter.CheckRate(keyInfo.ID, keyInfo.TeamID, keyInfo.OrgID, keyLimits, teamLimits, orgLimits); err != nil {
			metrics.RateLimitRejectionsTotal.WithLabelValues("request").Inc()
			p.Log.LogAttrs(c.Context(), slog.LevelWarn, "rate limit exceeded",
				slog.String("key_id", keyInfo.ID),
				slog.String("org_id", keyInfo.OrgID),
			)
			if err := apierror.Send(c, fiber.StatusTooManyRequests, "rate_limit_exceeded", "rate limit exceeded"); err != nil {
				return err
			}
			return errResponseSent
		}
	}

	if p.TokenCounter != nil {
		if err := p.TokenCounter.CheckTokens(keyInfo.ID, keyInfo.TeamID, keyInfo.OrgID, keyLimits, teamLimits, orgLimits); err != nil {
			metrics.RateLimitRejectionsTotal.WithLabelValues("token").Inc()
			p.Log.LogAttrs(c.Context(), slog.LevelWarn, "token budget exceeded",
				slog.String("key_id", keyInfo.ID),
				slog.String("org_id", keyInfo.OrgID),
			)
			if err := apierror.Send(c, fiber.StatusTooManyRequests, "token_limit_exceeded", "token budget exceeded"); err != nil {
				return err
			}
			return errResponseSent
		}
	}

	return nil
}

// resolveModel performs scoped alias resolution followed by registry lookup and
// access control. It returns the resolved Model or sends an API error response
// and returns the error for Handle to propagate.
func (p *ProxyHandler) resolveModel(c fiber.Ctx, keyInfo *auth.KeyInfo, modelName string) (Model, error) {
	// Scoped alias resolution: team → org (before global YAML aliases).
	if p.AliasCache != nil && keyInfo != nil {
		if canonical, ok := p.AliasCache.Resolve(keyInfo.OrgID, keyInfo.TeamID, modelName); ok {
			modelName = canonical
		}
	}

	model, err := p.Registry.Resolve(modelName)
	if err != nil {
		if errors.Is(err, ErrModelNotFound) {
			if err := apierror.Send(c, fiber.StatusNotFound, "model_not_found",
				"the requested model was not found"); err != nil {
				return Model{}, err
			}
			return Model{}, errResponseSent
		}
		p.Log.LogAttrs(c.Context(), slog.LevelError, "registry resolve error",
			slog.String("model", modelName),
			slog.String("error", err.Error()),
		)
		if err := apierror.Send(c, fiber.StatusInternalServerError, "internal_error", "failed to resolve model"); err != nil {
			return Model{}, err
		}
		return Model{}, errResponseSent
	}

	if p.AccessCache != nil && keyInfo != nil {
		if !p.AccessCache.Check(keyInfo.OrgID, keyInfo.TeamID, keyInfo.ID, model.Name) {
			if err := apierror.Send(c, fiber.StatusForbidden, "model_access_denied", "model access denied"); err != nil {
				return Model{}, err
			}
			return Model{}, errResponseSent
		}
	}

	return model, nil
}

// buildUpstreamRequest constructs the outbound HTTP request for the upstream
// provider. It validates the path, selects and applies the provider adapter,
// transforms the body, builds the URL, creates the request with a dedicated
// context, and sets all required headers. It returns the ready-to-send request,
// a cancel function for its context, the adapter (needed later for response
// transformation), or an API error response and error for Handle to propagate.
func (p *ProxyHandler) buildUpstreamRequest(c fiber.Ctx, model Model, body []byte, envelope requestEnvelope) (*http.Request, context.CancelFunc, Adapter, error) {
	upstreamPath := path.Clean(strings.TrimPrefix(c.Path(), "/v1/"))

	if !isAllowedPath(upstreamPath) {
		if err := apierror.Send(c, fiber.StatusBadRequest,
			"bad_request", "unsupported API endpoint"); err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, nil, errResponseSent
	}

	adapter := GetAdapter(model.Provider)

	// Determine if body needs mutation.
	needsModelReplace := envelope.Model != model.Name
	needsStreamOpts := envelope.Stream && (adapter == nil || isAzureAdapter(adapter))

	var modifiedBody []byte
	if needsModelReplace || needsStreamOpts {
		modifiedBody = mutateRequestBody(body, model.Name, needsStreamOpts)
	} else {
		// No JSON parse/serialize needed — model name is already canonical
		// and no stream_options injection required. A defensive copy is still
		// made because c.Body() is backed by fasthttp's arena which is recycled
		// after Handle returns.
		modifiedBody = append([]byte(nil), body...)
	}

	if adapter != nil {
		var transformErr error
		modifiedBody, transformErr = adapter.TransformRequest(modifiedBody, model)
		if transformErr != nil {
			p.Log.LogAttrs(c.Context(), slog.LevelWarn, "adapter transform request failed",
				slog.String("error", transformErr.Error()),
			)
			if err := apierror.Send(c, fiber.StatusBadRequest, "bad_request", "failed to transform request for provider"); err != nil {
				return nil, nil, nil, err
			}
			return nil, nil, nil, errResponseSent
		}
	}

	var upstreamURL string
	if adapter != nil {
		upstreamURL = adapter.TransformURL(model.BaseURL, upstreamPath, model)
	} else {
		upstreamURL = strings.TrimRight(model.BaseURL, "/") + "/" + upstreamPath
	}

	// upstreamCtx is a dedicated context for the upstream HTTP request. We never
	// use c.Context() (the fasthttp RequestCtx) because fasthttp recycles it
	// after Handle returns — which happens before a streaming goroutine finishes.
	// Derive from ShutdownState.ParentCtx so that CancelInflight aborts all
	// in-flight upstream requests during a forced shutdown.
	//
	// When the model has a per-model timeout configured, use WithTimeout so the
	// upstream request is automatically cancelled after that duration. This caps
	// both the connection phase and non-streaming read phase. For streaming
	// responses the timeout is also enforced via time.AfterFunc in
	// handleStreamingResponse; using WithTimeout here additionally cancels the
	// context on non-streaming reads without requiring a separate timer.
	var parentCtx context.Context
	if p.ShutdownState != nil {
		parentCtx = p.ShutdownState.ParentCtx()
	} else {
		parentCtx = context.Background()
	}

	// For non-streaming requests, apply a hard deadline via WithTimeout so the
	// upstream call is automatically cancelled after the per-model timeout.
	// For streaming requests, the timeout is enforced by the time.AfterFunc in
	// handleStreamingResponse (using effectiveStreamDur); applying WithTimeout
	// here as well would fire redundantly at the same instant and add no value.
	var upstreamCtx context.Context
	var upstreamCancel context.CancelFunc
	if model.Timeout > 0 && !envelope.Stream {
		upstreamCtx, upstreamCancel = context.WithTimeout(parentCtx, model.Timeout)
	} else {
		upstreamCtx, upstreamCancel = context.WithCancel(parentCtx)
	}

	// Graft the active OTel span from the Fiber context onto the upstream
	// context so child spans maintain the correct trace hierarchy. The
	// upstream context is derived from ShutdownState.ParentCtx which does
	// not carry the root span; without this graft, proxy.upstream becomes
	// an orphaned root span in the collector.
	if p.Tracer != nil {
		if span := trace.SpanFromContext(c.Context()); span.SpanContext().IsValid() {
			upstreamCtx = trace.ContextWithSpan(upstreamCtx, span)
		}
	}

	req, err := http.NewRequestWithContext(upstreamCtx, c.Method(), upstreamURL, bytes.NewReader(modifiedBody))
	if err != nil {
		upstreamCancel()
		p.Log.LogAttrs(c.Context(), slog.LevelError, "failed to build upstream request",
			slog.String("url", upstreamURL),
			slog.String("error", err.Error()),
		)
		if err := apierror.Send(c, fiber.StatusInternalServerError, "internal_error", "failed to build upstream request"); err != nil {
			return nil, nil, nil, err
		}
		return nil, nil, nil, errResponseSent
	}

	setUpstreamHeaders(req, c, model)

	if adapter != nil {
		adapter.SetHeaders(req, model)
	}

	return req, upstreamCancel, adapter, nil
}

// handleStreamingResponse sets the SSE response headers and launches the
// SendStreamWriter goroutine that pipes the upstream event stream to the client.
// The goroutine owns cancelUpstream, resp.Body.Close, and the trackDone call —
// none of these must be deferred at Handle scope on the streaming path.
// breaker may be nil when circuit breaking is disabled; when non-nil, the
// goroutine records success or failure after the stream completes.
func (p *ProxyHandler) handleStreamingResponse(c fiber.Ctx, resp *http.Response, cancelUpstream context.CancelFunc, model Model, keyInfo *auth.KeyInfo, adapter Adapter, startTime time.Time, requestID string, maxStreamDuration time.Duration, trackDone func(), breaker *circuitbreaker.Breaker) error {
	copyResponseHeaders(c, resp)
	c.Set("Content-Type", "text/event-stream")
	c.Set("Cache-Control", "no-cache")
	c.Set("X-Accel-Buffering", "no")
	c.Status(resp.StatusCode)

	// respStatusCode is captured here before Handle returns, because the Fiber
	// context is recycled by fasthttp after the handler exits and must not be
	// accessed inside the SendStreamWriter closure.
	respStatusCode := resp.StatusCode

	// upstreamCancel, resp.Body.Close, and the drain tracking call are all
	// handled inside the closure. SendStreamWriter's goroutine outlives
	// Handle's return, so none of them must be deferred at Handle scope —
	// that would fire before the goroutine has finished reading the body.
	// trackDone is safe to pass directly: sync.Once ensures it fires exactly
	// once whether the top-level defer or this goroutine runs first.
	return c.SendStreamWriter(func(w *bufio.Writer) {
		metrics.ActiveStreams.Inc()
		defer metrics.ActiveStreams.Dec()
		defer trackDone()
		defer cancelUpstream()
		defer resp.Body.Close()

		// Stream timeout: after maxStreamDuration, cancel the upstream
		// request context. This causes the transport to tear down the
		// connection, scanner.Scan() fails, and the loop exits cleanly.
		// On normal completion, streamTimer.Stop() prevents the callback
		// from firing. Either way, a single defer resp.Body.Close() above
		// handles cleanup — no concurrent Close+Read race.
		streamTimer := time.AfterFunc(maxStreamDuration, func() {
			p.Log.LogAttrs(context.Background(), slog.LevelWarn,
				"stream timeout exceeded, aborting upstream connection")
			cancelUpstream()
		})
		defer streamTimer.Stop()

		extractor := &streamUsageExtractor{}
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024) // up to 4MB per SSE line
		var ttftMS *int
		firstChunk := true
		for scanner.Scan() {
			line := scanner.Bytes()
			if firstChunk && bytes.HasPrefix(line, []byte("data: ")) {
				t := int(time.Since(startTime).Milliseconds())
				ttftMS = &t
				firstChunk = false
				metrics.ProxyTTFTSeconds.WithLabelValues(model.Name).Observe(float64(t) / 1000)
			}
			if adapter != nil {
				line = adapter.TransformStreamLine(line)
				if line == nil {
					continue // adapter says skip this line
				}
			}
			// Always observe the (possibly transformed) line. Transformed
			// lines are OpenAI-shaped (Azure passthrough, Anthropic → OpenAI),
			// and raw passthrough lines are already OpenAI-shaped, so the
			// extractor can parse usage from all of them.
			extractor.observe(line)
			if _, err := w.Write(line); err != nil {
				break // client disconnected
			}
			if err := w.WriteByte('\n'); err != nil {
				break // client disconnected
			}
			if err := w.Flush(); err != nil {
				break // client disconnected
			}
		}
		scanErr := scanner.Err()
		if scanErr != nil {
			p.Log.LogAttrs(context.Background(), slog.LevelWarn,
				"streaming scan error",
				slog.String("error", scanErr.Error()),
			)
		}

		// Record the circuit breaker outcome now that we know whether the
		// stream completed successfully. A scan error (transport failure,
		// context cancellation) counts as a failure; a clean EOF does not.
		if breaker != nil {
			if scanErr != nil {
				breaker.RecordFailure()
			} else {
				breaker.RecordSuccess()
			}
		}

		if p.UsageLogger != nil {
			var streamUI UsageInfo
			if adapter != nil {
				streamUI = adapter.StreamUsage()
			}
			// Fall back to the extractor when the adapter reports zero tokens.
			// AzureAdapter.StreamUsage always returns zero because Azure uses
			// the OpenAI SSE format and the extractor handles usage directly.
			if streamUI.PromptTokens == 0 && streamUI.CompletionTokens == 0 {
				streamUI = extractor.lastUsage
			}
			durationMS := int(time.Since(startTime).Milliseconds())
			p.logUsageEvent(keyInfo, model, streamUI, durationMS, ttftMS, respStatusCode, requestID)
		}

		metrics.ProxyDurationSeconds.WithLabelValues(model.Name, "true").Observe(time.Since(startTime).Seconds())
	})
}

// handleBufferedResponse reads the full upstream response body, validates its
// size, applies any adapter transformation, then sends the status, headers, and
// body to the client. Usage is logged asynchronously on success.
func (p *ProxyHandler) handleBufferedResponse(c fiber.Ctx, resp *http.Response, model Model, keyInfo *auth.KeyInfo, adapter Adapter, startTime time.Time, requestID string, maxResponseBody int) error {
	// Content-Length pre-check: fast-reject optimization to avoid allocating
	// memory for obviously oversized responses. Not the security boundary —
	// io.LimitReader on the next line handles chunked/unknown-length responses.
	if resp.ContentLength > 0 && resp.ContentLength > int64(maxResponseBody) {
		_ = resp.Body.Close() // body unread; error irrelevant on early reject
		return apierror.Send(c, fiber.StatusBadGateway,
			"upstream_response_too_large", "upstream response exceeds size limit")
	}

	// Read the entire response body up to limit+1 bytes. Reading one byte
	// beyond the limit lets us distinguish "exactly at limit" from "over limit"
	// without needing to know the Content-Length in advance.
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, int64(maxResponseBody)+1))
	_ = resp.Body.Close() // body fully consumed; close error is irrelevant
	if err != nil {
		p.Log.LogAttrs(c.Context(), slog.LevelWarn, "failed to read upstream response",
			slog.String("error", err.Error()),
		)
		return apierror.Send(c, fiber.StatusBadGateway,
			"upstream_unavailable", "failed to read upstream response")
	}

	if len(responseBody) > maxResponseBody {
		return apierror.Send(c, fiber.StatusBadGateway,
			"upstream_response_too_large", "upstream response exceeds size limit")
	}

	// Set status and copy headers after body validation so we never send a
	// 200 OK followed by a truncated or oversized body.
	c.Status(resp.StatusCode)
	copyResponseHeaders(c, resp)

	// Transform the body if an adapter is present and the response is
	// successful. Error responses (4xx/5xx) are forwarded as-is so that
	// provider-specific error details reach the client unchanged.
	//
	// usageBody tracks which body to pass to extractUsage. Anthropic (and
	// other non-OpenAI adapters) use provider-specific field names
	// (e.g. input_tokens/output_tokens) that only become OpenAI-shaped
	// (prompt_tokens/completion_tokens) AFTER TransformResponse. For adapter
	// paths we therefore extract usage from finalBody (post-transform).
	// For passthrough providers the raw upstream body is already OpenAI-shaped,
	// so usageBody stays as responseBody.
	var finalBody []byte
	var usageBody []byte
	if adapter != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		var transformErr error
		finalBody, transformErr = adapter.TransformResponse(responseBody)
		if transformErr != nil {
			p.Log.LogAttrs(c.Context(), slog.LevelWarn, "adapter transform response failed",
				slog.String("error", transformErr.Error()),
			)
			return apierror.Send(c, fiber.StatusBadGateway, "upstream_error", "failed to transform response from provider")
		}
		usageBody = finalBody // post-transform = OpenAI-shaped
	} else {
		finalBody = responseBody
		usageBody = responseBody // already OpenAI-shaped
	}

	if err := c.Send(finalBody); err != nil {
		return err
	}

	if p.UsageLogger != nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		ui := extractUsage(usageBody)
		durationMS := int(time.Since(startTime).Milliseconds())
		// For non-streaming responses TTFT equals total duration: the entire
		// response body is the first (and only) "token delivery".
		ttftMS := durationMS
		p.logUsageEvent(keyInfo, model, ui, durationMS, &ttftMS, resp.StatusCode, requestID)
	}

	metrics.ProxyDurationSeconds.WithLabelValues(model.Name, "false").Observe(time.Since(startTime).Seconds())

	return nil
}

// logUsageEvent builds and enqueues a usage.Event. It is a no-op when keyInfo
// is nil (unauthenticated request, or auth middleware not wired).
// ttftMS is the time-to-first-token in milliseconds; nil for non-streaming paths
// where the whole response arrives at once.
// requestID is the per-request trace ID from the request ID middleware; it must
// be captured from the Fiber context before Handle returns because the context
// is recycled by fasthttp after the handler exits.
func (p *ProxyHandler) logUsageEvent(keyInfo *auth.KeyInfo, model Model, ui UsageInfo, durationMS int, ttftMS *int, statusCode int, requestID string) {
	if keyInfo == nil {
		return
	}

	var cost *float64
	if model.Pricing.InputPer1M > 0 || model.Pricing.OutputPer1M > 0 {
		c := float64(ui.PromptTokens)/1_000_000*model.Pricing.InputPer1M +
			float64(ui.CompletionTokens)/1_000_000*model.Pricing.OutputPer1M
		cost = &c
	}

	var tps *float64
	if durationMS > 0 && ui.CompletionTokens > 0 {
		t := float64(ui.CompletionTokens) / (float64(durationMS) / 1000.0)
		tps = &t
	}

	p.UsageLogger.Log(usage.Event{
		KeyID:            keyInfo.ID,
		KeyType:          keyInfo.KeyType,
		OrgID:            keyInfo.OrgID,
		TeamID:           keyInfo.TeamID,
		UserID:           keyInfo.UserID,
		ServiceAccountID: keyInfo.ServiceAccountID,
		ModelName:        model.Name,
		PromptTokens:     ui.PromptTokens,
		CompletionTokens: ui.CompletionTokens,
		TotalTokens:      ui.TotalTokens,
		CostEstimate:     cost,
		DurationMS:       durationMS,
		TTFT_MS:          ttftMS,
		TokensPerSecond:  tps,
		StatusCode:       statusCode,
		RequestID:        requestID,
	})

	metrics.TokensTotal.WithLabelValues(model.Name, "prompt").Add(float64(ui.PromptTokens))
	metrics.TokensTotal.WithLabelValues(model.Name, "completion").Add(float64(ui.CompletionTokens))
}

// deploymentKey returns the circuit breaker / health-checker lookup key for a
// deployment. It mirrors router.DeploymentKey; the duplication avoids the
// import cycle that arises from proxy ↔ router mutual imports.
func deploymentKey(modelName, deploymentName string) string {
	if deploymentName == modelName {
		return modelName
	}
	return modelName + "/" + deploymentName
}

// applyDeployment overlays the endpoint fields from dep onto model in-place.
// It is safe to call on a copy returned by resolveModel because that copy has
// its own backing arrays and no pointer aliasing with the registry's internal
// state.
func applyDeployment(model *Model, dep Deployment) {
	model.Provider = dep.Provider
	model.BaseURL = dep.BaseURL
	model.APIKey = dep.APIKey
	model.AzureDeployment = dep.AzureDeployment
	model.AzureAPIVersion = dep.AzureAPIVersion
	model.GCPProject = dep.GCPProject
	model.GCPLocation = dep.GCPLocation
}

// isRetryable reports whether an HTTP status code from an upstream response
// should cause the proxy to attempt the next deployment candidate. 5xx errors
// indicate a server-side problem that a different backend may not share.
// 4xx errors are client errors that will recur regardless of which deployment
// is used, so they are not retried.
func isRetryable(statusCode int) bool {
	return statusCode == http.StatusInternalServerError ||
		statusCode == http.StatusBadGateway ||
		statusCode == http.StatusServiceUnavailable ||
		statusCode == http.StatusGatewayTimeout
}

// extractUsage parses token counts from a non-streaming OpenAI-format response
// body. Returns a zero UsageInfo when the body cannot be parsed or carries no
// usage field.
func extractUsage(body []byte) UsageInfo {
	var resp struct {
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if jsonx.Unmarshal(body, &resp) != nil || resp.Usage == nil {
		return UsageInfo{}
	}
	return UsageInfo{
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
		TotalTokens:      resp.Usage.TotalTokens,
	}
}

// mutateRequestBody applies model name replacement and optional stream_options
// injection in a single JSON parse/serialize pass. If the body cannot be parsed
// or re-serialized, the original bytes are returned unchanged.
func mutateRequestBody(body []byte, canonicalModel string, injectUsage bool) []byte {
	var doc map[string]jsonx.RawMessage
	if jsonx.Unmarshal(body, &doc) != nil {
		return body
	}
	if nameJSON, err := jsonx.Marshal(canonicalModel); err == nil {
		doc["model"] = jsonx.RawMessage(nameJSON)
	}
	if injectUsage {
		doc["stream_options"] = jsonx.RawMessage(`{"include_usage":true}`)
	}
	if out, err := jsonx.Marshal(doc); err == nil {
		return out
	}
	return body
}

// isAzureAdapter reports whether the given adapter is an Azure OpenAI adapter.
func isAzureAdapter(a Adapter) bool {
	_, ok := a.(*AzureAdapter)
	return ok
}

// isAllowedPath checks whether the upstream path is a known LLM API endpoint.
// Exact matches are used for single-resource paths; prefix matches are used
// only for paths that have legitimate sub-routes (images/, audio/, models/).
func isAllowedPath(p string) bool {
	switch p {
	case "chat/completions", "completions", "embeddings", "models":
		return true
	}
	return strings.HasPrefix(p, "images/") ||
		strings.HasPrefix(p, "audio/") ||
		strings.HasPrefix(p, "models/")
}

// isStreamingResponse reports whether the upstream response is a server-sent
// event stream by inspecting the Content-Type header.
func isStreamingResponse(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.Contains(ct, "text/event-stream")
}
