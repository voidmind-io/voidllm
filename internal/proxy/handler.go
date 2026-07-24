package proxy

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
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
	"github.com/voidmind-io/voidllm/internal/cooldown"
	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/metrics"
	"github.com/voidmind-io/voidllm/internal/pii"
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
	Cooldowns         *cooldown.Registry       // per-deployment rate-limit cooldown tracker; nil disables cooldown tracking
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
	// FallbackMaxDepth is the maximum number of fallback hops allowed per
	// request. Zero or negative disables fallback chaining entirely.
	// Set from config.SettingsConfig.FallbackMaxDepth at startup.
	FallbackMaxDepth int
	// PIIEngine performs in-memory PII anonymization on outbound request
	// bodies and restores original values in responses. A nil value
	// disables PII anonymization entirely with zero overhead on the hot path.
	PIIEngine *pii.Engine
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

	// Set tracing attributes only after successful model resolution so that the
	// raw, client-supplied model string (envelope.Model) never reaches the
	// tracing backend. A client that embeds PII in the model field would
	// otherwise persist that PII in spans — a Zero-Knowledge violation.
	// model.Name is the canonical, registry-validated name; model.Provider is
	// set by the registry loader and is never derived from client input.
	if p.Tracer != nil {
		trace.SpanFromContext(c.Context()).SetAttributes(
			attribute.String("model.canonical", model.Name),
			attribute.String("model.provider", model.Provider),
		)
	}

	// requestedModelName is the canonical name of the model originally requested
	// by the client. It is preserved across fallback hops so usage events can
	// record both the originally-requested model and the one that actually served
	// the request.
	requestedModelName := model.Name

	// visited tracks the canonical names of all models attempted in this
	// request's fallback chain so that runtime cycles are detected and broken.
	visited := make(map[string]bool)

	// currentModel may be replaced on each fallback iteration.
	currentModel := model

	// PII anonymization: create a per-request filter and pre-anonymize the
	// body ONCE before the fallback loop. We hold both the original body
	// and an anonymized body in parallel. On each hop we choose which body
	// to send based on the ACTUAL provider resolved for that hop (after
	// applyDeployment). This is the only correct approach: the initial model
	// provider may differ from the deployment or fallback provider, and a
	// later hop could route to an external provider even when the primary
	// was internal.
	//
	// Design: the filter is created with the authenticated key's OrgID so
	// that pseudonyms are tenant-scoped. The filter lives until the response
	// is fully handled (including inside the streaming goroutine, which
	// captures it by value on the heap). The filter is never accessed from
	// multiple goroutines.
	//
	// Fail-closed: if anonymization fails (parse error, mapping cap exceeded),
	// the request is rejected immediately. We never forward the original body
	// when the engine is enabled and anonymization cannot be guaranteed.
	var piiFilter *pii.Filter
	var anonBody []byte // anonymized body; nil when PII engine is disabled
	if p.PIIEngine != nil {
		// OrgID is always non-empty for authenticated requests because the auth
		// middleware (internal/auth) validates the API key and rejects requests
		// with no matching key. The only path with a nil keyInfo is test code or
		// an unauthenticated bypass — neither is possible on the production hot
		// path (auth middleware is always wired before Handle). If keyInfo is nil
		// here, orgID defaults to "" which still produces valid pseudonyms scoped
		// to the empty-string tenant; anonymization is still applied.
		orgID := ""
		if keyInfo != nil {
			orgID = keyInfo.OrgID
		}
		piiFilter = p.PIIEngine.NewFilter(orgID)
		var anonErr error
		anonBody, anonErr = piiFilter.AnonymizeJSON(body)
		if anonErr != nil {
			// Fail-closed: reject the request rather than risk sending PII
			// to an external provider on any hop in the fallback chain.
			return apierror.Send(c, fiber.StatusUnprocessableEntity,
				"pii_policy", "request rejected by PII policy")
		}
	}

	// shouldAnonymize reports whether PII anonymization must be applied for the
	// given deployment. The decision follows this priority order:
	//
	//  1. dep.PIIFilter != nil  → use *dep.PIIFilter (explicit per-deployment)
	//  2. model.PIIFilter != nil → use *model.PIIFilter (explicit per-model)
	//  3. default               → anonymize when the destination is NOT private
	//                             (!dep.destPrivate), pass through when private
	//
	// An explicit pii_filter: true on a private destination forces anonymization;
	// an explicit pii_filter: false on a public destination suppresses it (trusted
	// public endpoint). The default (nil flag) uses the network-based heuristic
	// so that self-hosted models on loopback/private IPs are never anonymized by
	// default, while cloud-provider endpoints are always anonymized by default.
	//
	// The model captured here is the ORIGINAL resolved model (before any fallback
	// hop replaces currentModel). Within a single hop, currentModel is passed into
	// tryModel which passes it as the model arg; the closure below captures the
	// outer currentModel by reference, so it always reflects the model for the
	// current hop. This is correct: each hop's shouldAnonymize reads the model
	// for that hop, not the original requested model.
	shouldAnonymize := func(dep Deployment, m Model) bool {
		if dep.PIIFilter != nil {
			return *dep.PIIFilter
		}
		if m.PIIFilter != nil {
			return *m.PIIFilter
		}
		return !dep.destPrivate
	}

	// pickBody returns the body that should be sent to a given deployment.
	// It delegates to shouldAnonymize and selects the anonymized body when
	// the PII engine is active and anonymization is required. The model
	// argument is the hop-local model (after applyDeployment in tryModel).
	//
	// The anonymous function signature matches what tryModel expects: only
	// a Deployment is passed as the first argument. We capture currentModel
	// by reference so the closure always sees the active hop's model.
	pickBody := func(dep Deployment) []byte {
		if piiFilter != nil && shouldAnonymize(dep, currentModel) {
			return anonBody
		}
		return body
	}

	// Per-model timeout overrides the global stream duration limit when set.
	// Recomputed on each iteration in case the fallback model has a different timeout.
	effectiveStreamDur := p.effectiveStreamDuration(c, maxStreamDur, currentModel)

	maxDepth := p.FallbackMaxDepth
	if maxDepth <= 0 {
		maxDepth = 0 // no fallback
	}

	var (
		resp           *http.Response
		cancelUpstream context.CancelFunc
		adapter        Adapter
		usedDep        Deployment
		lastErr        error
		lastStatus     int
	)

	// Chain wall time is bounded by the sum of per-model Timeout values
	// across the hops actually attempted. Each tryModel call enforces its
	// own timeout via the upstream HTTP client. No chain-level deadline
	// is imposed here: any in-process work between hops completes in
	// microseconds and cannot pathologically extend the request.
	for depth := 0; depth <= maxDepth; depth++ {
		visited[currentModel.Name] = true

		var tryErr error
		// pickBody is passed as a closure so tryModel can evaluate the correct
		// body variant per deployment, after applyDeployment has resolved the
		// effective provider. This is the fix for VULN-001: the body selection
		// must happen inside the deployment loop, not at the loop's call site.
		resp, cancelUpstream, adapter, usedDep, lastErr, lastStatus, tryErr = p.tryModel(c, currentModel, pickBody, envelope)
		if tryErr != nil {
			// tryModel wrote an error response to c (errResponseSent) or
			// returned a framework error. Either way, stop immediately.
			if errors.Is(tryErr, errResponseSent) {
				return nil
			}
			return tryErr
		}

		if resp != nil && !isFallbackEligible(lastStatus, nil) {
			// We have a usable response (success or non-retriable 4xx). Done.
			break
		}

		// resp is nil (all deployments exhausted) or resp carries a 5xx that
		// should trigger a fallback. Decide whether to try the next model in
		// the chain.
		if !isFallbackEligible(lastStatus, lastErr) {
			break
		}

		next, hasFallback := p.Registry.FallbackFor(currentModel.Name, visited)
		if !hasFallback {
			// No fallback model configured — keep resp as-is so it can be
			// forwarded to the client (even if it is a 5xx).
			break
		}
		if depth >= maxDepth {
			p.Log.LogAttrs(c.Context(), slog.LevelWarn, "fallback chain depth limit reached",
				slog.String("model", requestedModelName),
				slog.Int("max_depth", maxDepth),
			)
			break
		}

		// Access control check for the fallback target. This must happen before
		// committing to the hop so that a key without access to the fallback model
		// cannot exploit the chain to bypass access policy. The check mirrors the
		// one in resolveModel. We never surface a "forbidden" error to the client
		// here — instead we silently stop the chain and preserve the primary's
		// error, so the existence of the fallback target is not disclosed.
		if p.AccessCache != nil && keyInfo != nil {
			if !p.AccessCache.Check(keyInfo.OrgID, keyInfo.TeamID, keyInfo.ID, next.Name) {
				p.Log.LogAttrs(c.Context(), slog.LevelInfo, "fallback target not permitted by access policy",
					slog.String("requested", requestedModelName),
					slog.String("target", next.Name),
				)
				// Preserve lastErr from the failed primary; do not leak
				// "forbidden" to the client.
				break
			}
		}

		// Rewrite the model field in both the original and anonymized bodies
		// so that the next hop uses the correct model name regardless of
		// which body variant is ultimately sent. The rewrite only modifies
		// the "model" field, not any content field.
		newBody, berr := rewriteModelInBody(body, next.Name)
		if berr != nil {
			p.Log.LogAttrs(c.Context(), slog.LevelWarn, "fallback: cannot rewrite request body; stopping chain",
				slog.String("from", currentModel.Name),
				slog.String("to", next.Name),
				slog.String("error", berr.Error()),
			)
			// Preserve the primary's error; do not leak the body-rewrite error.
			break
		}
		body = newBody
		if anonBody != nil {
			newAnonBody, anonBerr := rewriteModelInBody(anonBody, next.Name)
			if anonBerr != nil {
				p.Log.LogAttrs(c.Context(), slog.LevelWarn, "fallback: cannot rewrite anonymized request body; stopping chain",
					slog.String("from", currentModel.Name),
					slog.String("to", next.Name),
					slog.String("error", anonBerr.Error()),
				)
				break
			}
			anonBody = newAnonBody
		}

		// We have a fallback target, access is permitted, and the body has been
		// rewritten. Log the hop and commit to using the fallback model.
		// This log fires only after all checks pass, so it never fires unless the
		// hop is actually going to happen.
		p.Log.LogAttrs(c.Context(), slog.LevelInfo, "falling back to next model",
			slog.String("from", requestedModelName),
			slog.String("to", next.Name),
			slog.Int("depth", depth+1),
		)

		// Drain and discard the current 5xx response before moving on so
		// the upstream connection is returned to the pool cleanly.
		if resp != nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			cancelUpstream()
			resp = nil
			cancelUpstream = nil
		}

		currentModel = next
		// The correct body variant (original vs anonymized) for the next hop
		// is selected inside tryModel via the pickBody closure, after
		// applyDeployment resolves the effective provider for each deployment.
		// No per-hop body pre-selection is needed here.
		effectiveStreamDur = p.effectiveStreamDuration(c, maxStreamDur, currentModel)
	}

	// All candidates (and fallback chain) exhausted without a usable response.
	if resp == nil {
		if lastErr != nil {
			return apierror.Send(c, fiber.StatusBadGateway, "upstream_unavailable", "upstream provider is unavailable")
		}
		// Every candidate was blocked by its circuit breaker.
		metrics.CircuitBreakerRejectionsTotal.WithLabelValues(currentModel.Name).Inc()
		return apierror.Send(c, fiber.StatusServiceUnavailable,
			"circuit_open", "upstream temporarily unavailable")
	}

	if isStreamingResponse(resp) {
		var breaker *circuitbreaker.Breaker
		if p.CircuitBreakers != nil {
			breaker = p.CircuitBreakers.Get(deploymentKey(currentModel.Name, usedDep.Name))
		}
		return p.handleStreamingResponse(c, resp, cancelUpstream, currentModel,
			keyInfo, adapter, startTime, requestID, requestedModelName, effectiveStreamDur, maxRespBody, trackDone, breaker, piiFilter)
	}

	defer cancelUpstream()
	return p.handleBufferedResponse(c, resp, currentModel, keyInfo, adapter,
		startTime, requestID, requestedModelName, maxRespBody, piiFilter)
}

// tryModel attempts to forward the request to the given model using its
// configured deployment candidates. It selects candidates via the Router (or
// synthesises a single candidate), iterates them in order, and returns as soon
// as one succeeds or returns a non-retryable status.
//
// pickBody is called once per deployment with the fully resolved Deployment
// value (after applyDeployment). It returns the correct body variant (original
// or anonymized) based on the deployment's pre-computed TrustedInternal flag.
// This is the only correct place to perform this selection: the model-level
// provider may differ from the deployment provider, and a deployment on a
// public-internet host must receive the anonymized body regardless of the
// model-level provider field. When PII anonymization is disabled, pickBody
// always returns the original body.
//
// Return values:
//   - resp: the upstream response, or nil if all candidates failed.
//   - cancel: the cancel func for the upstream request context. Non-nil only
//     when resp is non-nil. Must be called by the caller when done.
//   - adapter: the provider adapter selected for this model. May be nil.
//   - usedDep: the deployment that produced the response. Valid only when resp != nil.
//   - lastErr: the last transport-level error seen, or nil.
//   - lastStatus: the HTTP status of the last response seen (0 for transport errors).
//   - err: a framework-level error (including errResponseSent). When non-nil the
//     caller must propagate it immediately without inspecting the other values.
func (p *ProxyHandler) tryModel(
	c fiber.Ctx,
	model Model,
	pickBody func(dep Deployment) []byte,
	envelope requestEnvelope,
) (*http.Response, context.CancelFunc, Adapter, Deployment, error, int, error) {
	// Build the ordered list of deployment candidates. When Router is nil or
	// the model has no multi-deployment configuration, synthesize a single
	// candidate from the model's own fields so the retry loop is uniform.
	var candidates []Deployment
	if p.Router != nil && len(model.Deployments) > 0 {
		candidates = p.Router.Pick(model)
	} else {
		// Synthesize a single deployment from the model's own fields.
		// destPrivate and PIIFilter are copied from the model's pre-computed
		// fields so that single-deployment models receive the same PII decision
		// as multi-deployment models. Both fields are immutable after registry
		// load; the pointer copy for PIIFilter is safe.
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
			destPrivate:     model.destPrivate,
			PIIFilter:       model.PIIFilter,
		}}
	}

	var (
		req            *http.Request
		cancelUpstream context.CancelFunc
		currentAdapter Adapter
		currentResp    *http.Response
		dep            Deployment
		lastErr        error
	)

	for i, d := range candidates {
		depKey := deploymentKey(model.Name, d.Name)

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
		applyDeployment(&m, d)

		// Select the correct body variant for this specific deployment.
		// The security boundary is the deployment's TrustedInternal flag, which
		// was pre-computed from the BaseURL host at registry load time. We pass
		// the full Deployment so pickBody can read the flag directly. This is
		// evaluated AFTER applyDeployment so the per-deployment BaseURL is used,
		// not the model-level BaseURL which may differ.
		effectiveBody := pickBody(d)

		var buildErr error
		req, cancelUpstream, currentAdapter, buildErr = p.buildUpstreamRequest(c, m, effectiveBody, envelope)
		if buildErr != nil {
			return nil, nil, nil, Deployment{}, nil, 0, buildErr
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
			currentResp, doErr = p.HTTPClient.Do(req)
			if doErr != nil {
				upstreamSpan.RecordError(doErr)
				upstreamSpan.SetStatus(codes.Error, doErr.Error())
			} else {
				upstreamSpan.SetAttributes(attribute.Int("http.response.status_code", currentResp.StatusCode))
			}
			upstreamSpan.End()
		} else {
			currentResp, doErr = p.HTTPClient.Do(req)
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
				slog.String("deployment", d.Name),
				slog.String("provider", m.Provider),
				slog.Int("candidate", i),
				slog.String("error", doErr.Error()),
			)
			lastErr = doErr
			req = nil
			cancelUpstream = nil
			currentResp = nil
			metrics.RoutingRetriesTotal.WithLabelValues(model.Name, model.Strategy).Inc()
			continue
		}

		metrics.UpstreamRequestsTotal.WithLabelValues(m.Name, m.Provider, strconv.Itoa(currentResp.StatusCode)).Inc()

		if (isRetryable(currentResp.StatusCode) || isRateLimited(currentResp.StatusCode)) && i < len(candidates)-1 {
			// 5xx or 429 response from upstream — try the next deployment.
			// Drain and close the body before moving on so the connection is
			// returned to the pool.
			rateLimited := isRateLimited(currentResp.StatusCode)
			retryAfter := retryAfterOrDefault(currentResp)
			_, _ = io.Copy(io.Discard, currentResp.Body)
			_ = currentResp.Body.Close()
			cancelUpstream()
			if rateLimited {
				// A rate-limited upstream is not broken — recording it as a
				// circuit-breaker failure would erase real accumulated
				// failures on RecordSuccess and mis-trip the breaker on
				// RecordFailure. Instead, park the deployment in the
				// cooldown registry so the router deprioritizes it for a
				// short window.
				p.Cooldowns.Mark(depKey, retryAfter)
				metrics.RateLimitFailoversTotal.WithLabelValues(model.Name, depKey).Inc()
				p.Log.LogAttrs(c.Context(), slog.LevelWarn, "upstream rate limited, retrying next deployment",
					slog.String("model", m.Name),
					slog.String("deployment", d.Name),
					slog.String("provider", m.Provider),
					slog.Int("candidate", i),
					slog.Duration("cooldown", retryAfter),
				)
			} else {
				if p.CircuitBreakers != nil {
					p.CircuitBreakers.Get(depKey).RecordFailure()
				}
				metrics.UpstreamErrorsTotal.WithLabelValues(m.Name, m.Provider).Inc()
				p.Log.LogAttrs(c.Context(), slog.LevelWarn, "upstream returned retryable error, retrying next deployment",
					slog.String("model", m.Name),
					slog.String("deployment", d.Name),
					slog.String("provider", m.Provider),
					slog.Int("candidate", i),
					slog.Int("status", currentResp.StatusCode),
				)
			}
			lastErr = errors.New("upstream returned " + strconv.Itoa(currentResp.StatusCode))
			req = nil
			cancelUpstream = nil
			currentResp = nil
			metrics.RoutingRetriesTotal.WithLabelValues(model.Name, model.Strategy).Inc()
			continue
		}

		// Success or non-retryable status (4xx, or last retryable/rate-limited
		// with no more candidates). Record circuit breaker outcome for
		// non-streaming responses immediately; streaming outcome is recorded
		// inside the goroutine once the stream completes. A 429 is NEUTRAL —
		// it is neither a success (RecordSuccess would erase real accumulated
		// failures) nor a breaker failure (a rate-limited upstream is not
		// broken) — but the deployment is still marked cooling so the next
		// request does not try it first again.
		if isRateLimited(currentResp.StatusCode) {
			p.Cooldowns.Mark(depKey, retryAfterOrDefault(currentResp))
		} else if p.CircuitBreakers != nil && !isStreamingResponse(currentResp) {
			breaker := p.CircuitBreakers.Get(depKey)
			if currentResp.StatusCode >= 500 {
				breaker.RecordFailure()
			} else {
				breaker.RecordSuccess()
			}
		}

		dep = d
		model = m // use the deployment-overlaid model for response handling
		break
	}

	if currentResp != nil {
		return currentResp, cancelUpstream, currentAdapter, dep, lastErr, currentResp.StatusCode, nil
	}
	return nil, nil, nil, Deployment{}, lastErr, 0, nil
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

// effectiveStreamDuration returns the stream duration budget for a single hop:
// model.Timeout when configured, otherwise maxStreamDur. When c carries a
// tunnel stream-duration cap (set via SetTunnelStreamCap), the result is
// further capped at that value — never extended — so that a request tunneled
// through an in-process proxy such as the admin app's playground tunnel is
// always torn down by the proxy's own stream timer (clean upstream
// cancellation plus a terminal abort event) before any shorter, unrefreshed
// socket write deadline further down the stack can kill the connection
// instead. Requests that never call SetTunnelStreamCap take this codepath
// with zero behavioral change: tunnelStreamCapFromCtx reports ok=false and
// the model/global duration is returned unmodified.
func (p *ProxyHandler) effectiveStreamDuration(c fiber.Ctx, maxStreamDur time.Duration, model Model) time.Duration {
	d := maxStreamDur
	if model.Timeout > 0 {
		d = model.Timeout
	}
	if tunnelCap, ok := tunnelStreamCapFromCtx(c); ok && tunnelCap > 0 && tunnelCap < d {
		d = tunnelCap
	}
	return d
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

// doneLine is the exact SSE data line OpenAI-compatible upstreams (and our
// own provider adapters — see AnthropicAdapter's message_stop handling and
// GeminiAdapter's doneSent logic) emit to signal a clean end of stream. The
// plain (non-PII) streaming path compares each line it actually writes to
// the client against this value to detect the terminal event — see
// handleStreamingResponse's post-loop classification block.
var doneLine = []byte("data: [DONE]")

// writeStreamAbortEvent writes a single content-free OpenAI-shaped SSE error
// event to w and flushes. The event carries the provided code as both
// "type" and "code" so that clients can distinguish abort reasons without any
// upstream content appearing in the wire format. No trailing [DONE] is emitted;
// the absence of [DONE] signals to well-behaved clients that the stream did not
// complete successfully.
//
// It returns whether the event was fully written and flushed. The function
// never logs and never retries — by the time it is called the only objective
// is a best-effort notification and the stream is already being torn down —
// but callers on the plain streaming path use the reported outcome to tell a
// genuine delivery from a client that turned out to already be gone (a write
// failure here means there is nobody left to receive the notification, i.e.
// a disconnect, not a truncated response the client was successfully told
// about). Callers that do not need this distinction may ignore the result.
func writeStreamAbortEvent(w *bufio.Writer, code string) bool {
	// The message field is a static, content-free string. The code value is
	// caller-controlled and must always be a static string constant (never
	// derived from upstream content or user input) to ensure zero-knowledge.
	msg := "stream aborted"
	if _, err := fmt.Fprintf(w, "data: {\"error\":{\"message\":%q,\"type\":%q,\"code\":%q}}\n\n", msg, code, code); err != nil {
		return false
	}
	return w.Flush() == nil
}

// handleStreamingResponse sets the SSE response headers and launches the
// SendStreamWriter goroutine that pipes the upstream event stream to the client.
// The goroutine owns cancelUpstream, resp.Body.Close, and the trackDone call —
// none of these must be deferred at Handle scope on the streaming path.
// breaker may be nil when circuit breaking is disabled; when non-nil, the
// goroutine records success or failure after the stream completes.
// requestedModelName is the canonical name the client originally asked for;
// it may differ from model.Name when a fallback was activated.
// maxRespBody is the aggregate byte cap for the PII-buffered streaming path;
// it is the same limit used for non-streaming responses.
// filter may be nil when PII anonymization is disabled.
func (p *ProxyHandler) handleStreamingResponse(c fiber.Ctx, resp *http.Response, cancelUpstream context.CancelFunc, model Model, keyInfo *auth.KeyInfo, adapter Adapter, startTime time.Time, requestID string, requestedModelName string, maxStreamDuration time.Duration, maxRespBody int, trackDone func(), breaker *circuitbreaker.Breaker, filter *pii.Filter) error {
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

		// needsContentRestore is true when PII was detected in the request: the
		// response must be processed through pii.StreamRestorer so that pseudonyms
		// are restored before content reaches the client.
		//
		// StreamRestorer operates incrementally: it is pushed one raw upstream SSE
		// line at a time and emits restored content as soon as the per-choice carry
		// buffer cannot be the start of any known pseudonym (known-pseudonym-prefix
		// hold-back). This delivers token-by-token streaming to the client while
		// guaranteeing that a pseudonym split across SSE event boundaries is never
		// emitted in pieces.
		//
		// Fail-closed contract: any protocol violation (tool_calls in delta,
		// function_call, duplicate data: in one event, content after finish_reason,
		// upstream error object, non-empty carry at [DONE]) sets the restorer to
		// aborted state. The scan loop breaks immediately and no further content is
		// written to the client.
		//
		// When false (no PII detected or filter is nil), the original per-line
		// passthrough is used with zero overhead.
		needsContentRestore := filter != nil && filter.Touched()

		var ttftMS *int
		firstChunk := true
		// streamIncomplete is true when the streaming loop ended without a clean
		// [DONE] sentinel (abort or truncation — not a client disconnect). The usage
		// event for an incomplete stream is logged with http.StatusBadGateway so
		// that the record accurately reflects the stream was not successful, while
		// still capturing the partial token counts that the upstream consumed.
		streamIncomplete := false

		if needsContentRestore {
			// Incremental PII restore path (Stage 0b).
			//
			// StreamRestorer maintains a per-choice rolling carry buffer of at
			// most pseudonymLen-1 bytes. Content is emitted incrementally as
			// soon as the carry cannot be the prefix of any known pseudonym.
			// This delivers token-by-token streaming to the client rather than
			// buffering the full response, while preserving the fail-closed
			// guarantee: a pseudonym that is split across SSE event boundaries
			// by the LLM tokenizer is never emitted in pieces.
			//
			// The adapter runs BEFORE the restorer on each line, so the restorer
			// always sees OpenAI-shaped SSE regardless of the upstream provider.
			//
			// Fail-closed contract:
			//   - Protocol violations (tool_calls, duplicate data: in one event,
			//     content after finish_reason, upstream error object) → abort,
			//     nothing more emitted.
			//   - [DONE] with non-empty carry (truncated pseudonym) → abort.
			//   - scanner.Err() non-nil → abort (no partial restore emitted).
			//   - Aggregate input byte cap exceeded → abort (memory exhaustion guard).
			//
			// On terminal=true (clean [DONE] received), the loop is broken via
			// break (not early return) so that the post-stream usage-logging and
			// metrics code below still runs.
			restorer := pii.NewStreamRestorer(filter, model.Name)
			// streamAborted is true when a genuine upstream or protocol error
			// occurred — triggers breaker.RecordFailure().
			streamAborted := false
			// adapterAborted is true when the adapter returned errStreamTransformAborted
			// and the abort event has already been emitted to the client. The
			// post-loop error-event block must not emit a second event in this case.
			adapterAborted := false
			// clientDisconnected is true when a write to the client failed (the
			// client hung up mid-stream). This is not an upstream fault and must
			// NOT be recorded as a circuit-breaker failure.
			clientDisconnected := false
			// piiTotalInputBytes tracks the total raw bytes read from the upstream
			// on the PII path. This guards against a malicious or malfunctioning
			// upstream that sends an unbounded stream, exhausting proxy heap.
			// We use maxRespBody as the cap (same limit as non-streaming buffered path).
			var piiTotalInputBytes int

			writeSSELines := func(lines [][]byte) bool {
				for _, b := range lines {
					if b == nil {
						if err := w.WriteByte('\n'); err != nil {
							return false
						}
						continue
					}
					if _, err := w.Write(b); err != nil {
						return false
					}
					if err := w.WriteByte('\n'); err != nil {
						return false
					}
					if err := w.Flush(); err != nil {
						// Flush failure means the client disconnected.
						return false
					}
				}
				return true
			}

			// terminalSeen is true when Push returns terminal=true (clean [DONE]
			// received from the upstream). A stream that ends via EOF without
			// ever seeing [DONE] is a truncated/incomplete response and must NOT
			// be recorded as a circuit-breaker success.
			terminalSeen := false

		piiScanLoop:
			for scanner.Scan() {
				line := scanner.Bytes()

				if firstChunk && bytes.HasPrefix(line, []byte("data:")) {
					t := int(time.Since(startTime).Milliseconds())
					ttftMS = &t
					firstChunk = false
					metrics.ProxyTTFTSeconds.WithLabelValues(model.Name).Observe(float64(t) / 1000)
				}

				// Aggregate input byte cap on RAW scanner bytes (+1 for the
				// stripped newline), BEFORE the adapter runs. Adapter-dropped
				// lines (nil return) still count against the cap: a provider that
				// sends unbounded keepalive/ping lines would otherwise bypass the
				// cap entirely and stream forever until the upstream timeout fires.
				piiTotalInputBytes += len(line) + 1
				if piiTotalInputBytes > maxRespBody {
					p.Log.LogAttrs(context.Background(), slog.LevelWarn,
						"pii: aggregate input stream size limit exceeded; stream aborted to prevent memory exhaustion")
					streamAborted = true
					break piiScanLoop
				}

				if adapter != nil {
					adaptedLines, terr := adapter.TransformStreamLine(line)
					if terr != nil {
						p.Log.LogAttrs(context.Background(), slog.LevelWarn,
							"pii: adapter stream transform error; stream aborted fail-closed")
						streamAborted = true
						adapterAborted = true
						if !clientDisconnected {
							writeStreamAbortEvent(w, "stream_transform_aborted")
						}
						break piiScanLoop
					}
					if len(adaptedLines) == 0 {
						continue // adapter says skip this line
					}
					for i, al := range adaptedLines {
						extractor.observe(al)

						// Inject a blank SSE event separator into the restorer before
						// each adapter output line after the first. An adapter may return
						// multiple data: lines from a single upstream line (e.g. Gemini
						// mixed text+functionCall chunk → text line + tool_calls line).
						// The StreamRestorer treats a second data: line without a preceding
						// blank separator as a protocol violation and aborts fail-closed.
						// Push of an empty slice always returns (nil, false, nil) — no
						// error handling is needed here.
						if i > 0 && len(al) > 0 {
							_, _, _ = restorer.Push([]byte{})
						}

						// Copy: scanner.Bytes() is reused; al may alias line.
						alCopy := make([]byte, len(al))
						copy(alCopy, al)

						outLines, terminal, pushErr := restorer.Push(alCopy)
						if pushErr != nil {
							p.Log.LogAttrs(context.Background(), slog.LevelWarn,
								"pii: incremental stream restore error; stream aborted to prevent pseudonym leak",
								slog.String("error", pushErr.Error()),
							)
							streamAborted = true
							break piiScanLoop
						}
						if len(outLines) > 0 {
							if !writeSSELines(outLines) {
								clientDisconnected = true
								break piiScanLoop
							}
						}
						if terminal {
							terminalSeen = true
							break piiScanLoop
						}
					}
					continue
				}
				extractor.observe(line)

				// Copy: scanner.Bytes() is reused on the next Scan call.
				lineCopy := make([]byte, len(line))
				copy(lineCopy, line)

				outLines, terminal, pushErr := restorer.Push(lineCopy)
				if pushErr != nil {
					p.Log.LogAttrs(context.Background(), slog.LevelWarn,
						"pii: incremental stream restore error; stream aborted to prevent pseudonym leak",
						slog.String("error", pushErr.Error()),
					)
					streamAborted = true
					break piiScanLoop
				}
				if len(outLines) > 0 {
					if !writeSSELines(outLines) {
						// The client disconnected mid-stream (write or flush
						// error). This is not an upstream fault — do not
						// penalise the circuit breaker.
						clientDisconnected = true
						break piiScanLoop
					}
				}
				if terminal {
					terminalSeen = true
					// Clean [DONE] received. Break — do NOT early-return —
					// so post-stream usage/metrics code below still executes.
					break piiScanLoop
				}
			}

			scanErr := scanner.Err()
			if scanErr != nil {
				p.Log.LogAttrs(context.Background(), slog.LevelWarn,
					"pii: stream scan error; stream aborted to prevent pseudonym leak",
					slog.String("error", scanErr.Error()),
				)
				streamAborted = true
			}

			// Mark the stream as incomplete when it ended without a clean [DONE]
			// sentinel (abort or truncation, excluding client disconnects). This
			// propagates to the usage event status code so truncated streams are
			// not recorded as successful 200 events.
			if (streamAborted || !terminalSeen) && !clientDisconnected {
				streamIncomplete = true
			}

			// Synthesize a content-free SSE error event when the stream is
			// aborted by a PII policy violation, scanner error, timeout, or
			// EOF-without-[DONE] — but NOT after client disconnect (the client
			// is already gone), NOT after a write failure (the client is also
			// gone), and NOT when the adapter already emitted its own abort
			// event (adapterAborted). The error event is emitted WITHOUT a
			// trailing [DONE] so that clients that ignore the error object read
			// an incomplete stream (correct signal) rather than a successful one.
			// Never flush carry bytes before or alongside the error event.
			if !clientDisconnected && !adapterAborted && streamIncomplete {
				const piiErrorEvent = "data: {\"error\":{\"message\":\"response withheld by PII policy\",\"type\":\"pii_restore_aborted\",\"code\":\"pii_restore_aborted\"}}"
				_, _ = w.WriteString(piiErrorEvent)
				_ = w.WriteByte('\n')
				_ = w.WriteByte('\n')
				_ = w.Flush()
			}

			if breaker != nil {
				switch {
				case streamAborted:
					breaker.RecordFailure()
				case clientDisconnected:
					// Client hung up — neither success nor failure on the upstream side.
				case !terminalSeen:
					// Upstream closed the connection without sending [DONE].
					// This is a truncated response — treat as upstream failure.
					p.Log.LogAttrs(context.Background(), slog.LevelWarn,
						"pii: upstream stream ended without [DONE] sentinel; treating as failure")
					breaker.RecordFailure()
				default:
					breaker.RecordSuccess()
				}
			}
			_ = w.Flush()
		} else {
			// Non-buffered path: per-line passthrough with no PII overhead.
			//
			// plainAborted tracks adapter-abort: the abort branch below
			// already writes exactly one abort event and records the
			// breaker failure inline, so the post-loop classification must
			// not touch either again.
			//
			// clientDisconnected tracks a write/newline/flush failure — the
			// client is already gone. It is checked FIRST in the
			// classification below because a write failure can race with a
			// pending scanner error (the upstream tearing down and the
			// client vanishing can surface as concurrent failures) and must
			// win: there is nobody left to receive an abort event either way.
			// It can also be set RETROACTIVELY, after the loop, when a
			// scanner error (or an EOF without [DONE]) leads the
			// classification below to attempt delivering the abort event and
			// that delivery itself fails — that outcome means the client was
			// already gone, so it is reclassified as a disconnect rather than
			// left as an "incomplete stream the client was notified about."
			//
			// terminalSeen mirrors the PII path's field of the same name. In the
			// adapter branch it becomes true after a synthesized [DONE] event is
			// written; in raw passthrough it becomes true only after the complete
			// event containing the sole "data: [DONE]" field and its separator are
			// flushed. A write failure while delivering either is a disconnect,
			// not a completion. The loop then stops so later upstream activity
			// cannot downgrade an already-completed stream.
			plainAborted := false
			clientDisconnected := false
			terminalSeen := false
			rawEventOpen := false
			rawEventDataLines := 0
			rawEventHasDoneLine := false

		plainScanLoop:
			for scanner.Scan() {
				line := scanner.Bytes()
				if firstChunk && bytes.HasPrefix(line, []byte("data: ")) {
					t := int(time.Since(startTime).Milliseconds())
					ttftMS = &t
					firstChunk = false
					metrics.ProxyTTFTSeconds.WithLabelValues(model.Name).Observe(float64(t) / 1000)
				}

				if adapter != nil {
					adaptedLines, terr := adapter.TransformStreamLine(line)
					if terr != nil {
						p.Log.LogAttrs(context.Background(), slog.LevelWarn,
							"streaming: adapter stream transform error; stream aborted fail-closed")
						if writeStreamAbortEvent(w, "stream_transform_aborted") {
							plainAborted = true
							streamIncomplete = true
							if breaker != nil {
								breaker.RecordFailure()
							}
						} else {
							// The abort event itself failed to reach the client —
							// it is already gone. Reclassify as a disconnect so
							// the post-loop switch's precedence (disconnect wins
							// over everything) applies, mirroring case 4 below.
							clientDisconnected = true
						}
						break plainScanLoop
					}
					if len(adaptedLines) == 0 {
						continue plainScanLoop
					}
					// Each adapted output line is written as a self-contained SSE
					// event: the line itself followed by "\n\n" (the SSE event
					// terminator). Forwarded upstream blank lines (event separators)
					// are skipped — they are now redundant because every event
					// self-terminates.
					//
					// This model is byte-identical for the normal single-line case:
					//   upstream: "data:{a}\n" + "\n"
					//   old:      write "data:{a}\n" then forward blank as "\n"  → "data:{a}\n\n"
					//   new:      write "data:{a}\n\n", skip blank               → "data:{a}\n\n"
					//
					// It also correctly terminates multi-output lines (Gemini
					// text+functionCall → two lines), [DONE] from a blank-input
					// adapter transform (Gemini doneSent), and the abort event
					// injected by writeStreamAbortEvent (which already ends in \n\n).
					for _, al := range adaptedLines {
						if len(al) == 0 {
							// Forwarded upstream blank line is the old event
							// delimiter — skip it; the preceding event already
							// self-terminated with \n\n.
							continue
						}
						extractor.observe(al)
						if _, err := w.Write(al); err != nil {
							clientDisconnected = true
							break plainScanLoop
						}
						if _, err := w.WriteString("\n\n"); err != nil {
							clientDisconnected = true
							break plainScanLoop
						}
						if err := w.Flush(); err != nil {
							clientDisconnected = true
							break plainScanLoop
						}
						// The line and its "\n\n" event terminator were fully
						// written and flushed — only now is it safe to record
						// the terminal. Nothing after [DONE] may be processed
						// or written, so stop scanning immediately.
						if bytes.Equal(al, doneLine) {
							terminalSeen = true
							break plainScanLoop
						}
					}
					continue plainScanLoop
				}
				// Raw passthrough (no adapter transform): preserve the upstream
				// event structure line-for-line. In particular, several data fields,
				// metadata fields, and comments may all belong to one event and must
				// not be separated by proxy-inserted blank lines. With ScanLines,
				// LF-framed streams are forwarded byte-for-byte, CRLF is normalized
				// to LF, and lone-CR framing is not supported.
				if len(line) > 0 {
					rawEventOpen = true
					fieldName, _, _ := bytes.Cut(line, []byte(":"))
					if bytes.Equal(fieldName, []byte("data")) {
						rawEventDataLines++
						if bytes.Equal(line, doneLine) {
							rawEventHasDoneLine = true
						}
					}
					extractor.observe(line)
				}

				if _, err := w.Write(line); err != nil {
					clientDisconnected = true
					break plainScanLoop
				}
				if err := w.WriteByte('\n'); err != nil {
					clientDisconnected = true
					break plainScanLoop
				}
				if err := w.Flush(); err != nil {
					clientDisconnected = true
					break plainScanLoop
				}

				if len(line) == 0 {
					isTerminalEvent := rawEventOpen && rawEventDataLines == 1 && rawEventHasDoneLine
					rawEventOpen = false
					rawEventDataLines = 0
					rawEventHasDoneLine = false
					if isTerminalEvent {
						terminalSeen = true
						break plainScanLoop
					}
				}
			}

			// EOF or a scanner error can arrive after a non-blank line. Close
			// that event before classification so any injected abort event starts
			// at a fresh boundary. A lone [DONE] event becomes terminal only after
			// this successfully writes and flushes its missing separator.
			if adapter == nil && !clientDisconnected && !terminalSeen && rawEventOpen {
				if err := w.WriteByte('\n'); err != nil {
					clientDisconnected = true
				} else if err := w.Flush(); err != nil {
					clientDisconnected = true
				} else if rawEventDataLines == 1 && rawEventHasDoneLine {
					terminalSeen = true
				}
			}

			// scanErr is logged unconditionally for observability whenever
			// non-nil, independent of how it is ultimately classified below.
			scanErr := scanner.Err()
			if scanErr != nil {
				p.Log.LogAttrs(context.Background(), slog.LevelWarn,
					"streaming scan error",
					slog.String("error", scanErr.Error()),
				)
			}

			// Classify how the scan loop ended, in strict precedence order.
			// Exactly one case applies; see the field comments above the loop
			// for the reasoning behind the ordering.
			switch {
			case clientDisconnected:
				// Case 1: the client is already gone (a write, newline, or
				// flush failed). This takes precedence over everything else,
				// including a pending scanner error, because a write failure
				// racing with an upstream read error is still fundamentally a
				// disconnect — there is nobody left to receive an abort
				// event. No abort event is written; streamIncomplete is left
				// untouched here (a disconnect has its own usage semantics,
				// handled by the breaker switch below, not the incomplete/502
				// path).

			case plainAborted:
				// Case 2: the adapter transform reported
				// errStreamTransformAborted. The abort branch above already
				// wrote exactly one abort event, set streamIncomplete, and
				// recorded the breaker failure — nothing more to do here.

			case terminalSeen:
				// Case 3: a clean "data: [DONE]" line was actually written to
				// the client. The stream completed successfully. The scan
				// loop broke out the instant [DONE] was flushed, so nothing
				// the upstream does afterward (e.g. tearing the connection
				// down immediately after its final event) is ever observed
				// here: no abort event, no downgrade.

			default:
				// Case 4: the loop ended without ever writing [DONE] to the
				// client — either scanner.Err() reports a genuine error
				// (stream-duration timeout firing and cancelling the upstream
				// context — see streamTimer above — upstream reset, or
				// malformed framing) or the upstream closed the connection
				// cleanly without ever sending [DONE] (a silently truncated
				// response; bufio.Scanner treats EOF as terminal but never
				// reports it via Err, so scanErr is nil in that sub-case too).
				// Both are incomplete from the client's perspective, so we
				// attempt to deliver the same content-free abort event the
				// PII path already emits for this failure class.
				//
				// Framing invariant: upstream blank separators are forwarded, and
				// an event still open at EOF/error is explicitly closed above. The
				// abort event therefore always starts at a fresh SSE event boundary.
				if writeStreamAbortEvent(w, "stream_incomplete") {
					streamIncomplete = true
				} else {
					// The client turned out to already be gone by the time we
					// tried to deliver the abort event — e.g. it disconnected
					// at the same moment the scanner hit its own error. This
					// is a disconnect, not a truncated response the client
					// was actually told about: reclassify as case 1 so the
					// breaker switch below records neither success nor
					// failure, and the stream is not marked incomplete for a
					// notification nobody received.
					//
					// This reclassification is best-effort by nature: a write to
					// an already-departed client frequently still succeeds
					// because the bytes only reach a socket buffer, so the
					// failure is not observable here. Such a request is then
					// recorded as incomplete (502) with a breaker failure even
					// though nobody was listening. That mislabels a rare race in
					// metrics only - no content is affected - and detecting it
					// reliably is not possible at this layer, which is why no
					// test covers that particular interleaving.
					clientDisconnected = true
				}
			}

			if breaker != nil && !plainAborted {
				switch {
				case clientDisconnected:
					// Client hung up — neither success nor failure on the
					// upstream side.
				case terminalSeen:
					breaker.RecordSuccess()
				default:
					// Scanner error before [DONE], or a clean EOF that never
					// reached [DONE] — treat as an upstream failure.
					breaker.RecordFailure()
				}
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
			// Use the upstream HTTP status for successful, complete streams. For
			// aborted or truncated streams (no clean [DONE]) log StatusBadGateway
			// so the usage record reflects that the stream did not complete
			// successfully. Token counts are preserved: the upstream consumed those
			// tokens regardless of whether the response reached the client intact.
			eventStatusCode := respStatusCode
			if streamIncomplete {
				eventStatusCode = http.StatusBadGateway
			}
			p.logUsageEvent(keyInfo, model, streamUI, durationMS, ttftMS, eventStatusCode, requestID, requestedModelName)
		}

		metrics.ProxyDurationSeconds.WithLabelValues(model.Name, "true").Observe(time.Since(startTime).Seconds())
	})
}

// handleBufferedResponse reads the full upstream response body, validates its
// size, applies any adapter transformation, then sends the status, headers, and
// body to the client. Usage is logged asynchronously on success.
// requestedModelName is the canonical name the client originally asked for;
// it may differ from model.Name when a fallback was activated.
// filter may be nil when PII anonymization is disabled.
func (p *ProxyHandler) handleBufferedResponse(c fiber.Ctx, resp *http.Response, model Model, keyInfo *auth.KeyInfo, adapter Adapter, startTime time.Time, requestID string, requestedModelName string, maxResponseBody int, filter *pii.Filter) error {
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

	// PII restore: replace pseudonyms with original values on all responses,
	// including 4xx and 5xx. Provider error messages sometimes echo back
	// parts of the request (e.g. the model name or request fields); if a
	// pseudonym appears in an error body, the client must see the original
	// value. Restoring on non-2xx is a no-op when the provider did not echo
	// any pseudonym — and when it did, restoring is the correct behaviour.
	// filter.Touched() guards against the cost of building the Replacer on
	// requests where no PII was detected.
	if filter != nil && filter.Touched() {
		finalBody = filter.Restore(finalBody)
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
		p.logUsageEvent(keyInfo, model, ui, durationMS, &ttftMS, resp.StatusCode, requestID, requestedModelName)
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
// requestedModelName is the canonical name the client originally asked for; equal
// to model.Name when no fallback occurred.
func (p *ProxyHandler) logUsageEvent(keyInfo *auth.KeyInfo, model Model, ui UsageInfo, durationMS int, ttftMS *int, statusCode int, requestID string, requestedModelName string) {
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
		KeyID:              keyInfo.ID,
		KeyType:            keyInfo.KeyType,
		OrgID:              keyInfo.OrgID,
		TeamID:             keyInfo.TeamID,
		UserID:             keyInfo.UserID,
		ServiceAccountID:   keyInfo.ServiceAccountID,
		ModelName:          model.Name,
		RequestedModelName: requestedModelName,
		PromptTokens:       ui.PromptTokens,
		CompletionTokens:   ui.CompletionTokens,
		TotalTokens:        ui.TotalTokens,
		CostEstimate:       cost,
		DurationMS:         durationMS,
		TTFT_MS:            ttftMS,
		TokensPerSecond:    tps,
		StatusCode:         statusCode,
		RequestID:          requestID,
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
//
// isRetryable is deliberately kept separate from isRateLimited even though
// callers usually OR the two together to decide whether to try the next
// candidate: the two carry different circuit-breaker semantics. A 5xx means
// the deployment itself is broken and must count as a circuit-breaker
// failure; a 429 (see isRateLimited) means the deployment is healthy but
// temporarily throttled and must never trip or reset the circuit breaker —
// it is tracked separately via the cooldown registry instead.
func isRetryable(statusCode int) bool {
	return statusCode == http.StatusInternalServerError ||
		statusCode == http.StatusBadGateway ||
		statusCode == http.StatusServiceUnavailable ||
		statusCode == http.StatusGatewayTimeout
}

// isRateLimited reports whether an HTTP status code from an upstream response
// is a rate-limit rejection (429). See isRetryable's godoc for why this is a
// distinct predicate rather than folded into isRetryable.
func isRateLimited(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests
}

// defaultRateLimitCooldown and maxRateLimitCooldown govern how long a
// deployment is parked in the cooldown registry after returning HTTP 429.
// Neither is configurable in v1.
const (
	// defaultRateLimitCooldown is applied when the upstream response carries
	// no usable Retry-After / retry-after-ms hint. A few seconds is enough to
	// skip the deployment for the next handful of requests without assuming a
	// specific rate-limit window when the upstream did not say one.
	defaultRateLimitCooldown = 3 * time.Second

	// maxRateLimitCooldown caps the cooldown even when the upstream requests
	// a much longer wait (e.g. "Retry-After: 3600"). A single 429 response
	// must never park a deployment for anywhere close to that long; one
	// minute is a generous upper bound that still meaningfully deprioritizes
	// the deployment without contributing to an hour of reduced capacity.
	maxRateLimitCooldown = 60 * time.Second
)

// retryAfterOrDefault returns the cooldown duration to apply for a 429
// response. It reads the standard Retry-After header (RFC 7231), supporting
// both the delta-seconds form ("120") and the HTTP-date form ("Fri, 31 Dec
// 1999 23:59:59 GMT"), and the OpenAI-specific "retry-after-ms" header
// (milliseconds) as a higher-resolution alternative, preferring the latter
// when both are present. When neither header is present or parseable,
// defaultRateLimitCooldown is used. The result is always clamped to
// [0, maxRateLimitCooldown].
func retryAfterOrDefault(resp *http.Response) time.Duration {
	if resp == nil {
		return defaultRateLimitCooldown
	}

	if ms := resp.Header.Get("retry-after-ms"); ms != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(ms)); err == nil && n >= 0 {
			return clampCooldown(time.Duration(n) * time.Millisecond)
		}
	}

	ra := resp.Header.Get("Retry-After")
	if ra == "" {
		return defaultRateLimitCooldown
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs >= 0 {
		return clampCooldown(time.Duration(secs) * time.Second)
	}
	if when, err := http.ParseTime(ra); err == nil {
		return clampCooldown(time.Until(when))
	}

	return defaultRateLimitCooldown
}

// clampCooldown bounds d to [0, maxRateLimitCooldown]. A negative input
// (e.g. an HTTP-date Retry-After value already in the past) clamps to zero,
// which makes the subsequent Registry.Mark call a no-op.
func clampCooldown(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > maxRateLimitCooldown {
		return maxRateLimitCooldown
	}
	return d
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

// isFallbackEligible reports whether a failure on one model should trigger a
// fallback to the next model in the chain.
//
// Network / DNS / dial / timeout errors are eligible. Context cancellation is
// NOT eligible — the client went away and there is no point retrying. 5xx
// responses are eligible, as is 429 (rate limited — a different model may
// have separate quota); other 4xx are not (bad request or auth failure will
// recur on any backend).
func isFallbackEligible(statusCode int, err error) bool {
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false
		}
		return true
	}
	return (statusCode >= 500 && statusCode < 600) || statusCode == http.StatusTooManyRequests
}

// rewriteModelInBody replaces the "model" field inside a JSON request body
// with newModel. It unmarshals into a map, updates the field, and re-marshals.
// If the body is not valid JSON an error is returned — a non-JSON body cannot
// be safely forwarded to a fallback model and the chain should stop.
func rewriteModelInBody(body []byte, newModel string) ([]byte, error) {
	var doc map[string]jsonx.RawMessage
	if err := jsonx.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("fallback body rewrite: body is not JSON: %w", err)
	}
	nameJSON, err := jsonx.Marshal(newModel)
	if err != nil {
		return nil, fmt.Errorf("fallback body rewrite: marshal model name: %w", err)
	}
	doc["model"] = jsonx.RawMessage(nameJSON)
	out, err := jsonx.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("fallback body rewrite: marshal document: %w", err)
	}
	return out, nil
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
