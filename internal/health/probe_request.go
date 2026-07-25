package health

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

// ErrProbeNotApplicable is returned by BuildProbeRequest when intent has no
// meaningful upstream equivalent for the target provider — for example a
// models-list probe against Azure, Vertex, or Gemini, or an embeddings probe
// against Anthropic or Gemini. Callers must treat this as "skip", not as a
// failed probe: it must never be recorded as a successful check.
var ErrProbeNotApplicable = errors.New("probe not applicable for this provider")

// ProbeIntent identifies which kind of synthetic upstream request
// BuildProbeRequest should construct.
type ProbeIntent int

const (
	// IntentChat requests a minimal chat/completion-shaped call.
	IntentChat ProbeIntent = iota
	// IntentEmbeddings requests a minimal embeddings-shaped call.
	IntentEmbeddings
	// IntentModelsList requests the provider's model-listing endpoint.
	IntentModelsList
)

// ProbeTarget carries the provider and model identity needed to build a
// synthetic probe request via BuildProbeRequest. It is exported so that both
// the periodic Checker (internal/health) and the on-demand connection tester
// (internal/api/admin) can construct probe requests through the same
// provider-aware logic; neither package re-implements provider URL, body, or
// header rules.
type ProbeTarget struct {
	// ModelName is the canonical model name. Required for providers whose
	// synthetic request must reference a model by name in the URL or body
	// (anthropic, gemini, vertex); ignored by IntentModelsList.
	ModelName string
	// Provider selects the adapter: "" (or an OpenAI-compatible value such as
	// "openai", "vllm", "ollama", "custom"), "anthropic", "azure", "gemini",
	// or "vertex".
	Provider string
	// BaseURL is the upstream provider's base URL.
	BaseURL string
	// APIKey is the upstream provider's plaintext API key. Empty means no
	// authentication header is set (aside from whatever the adapter itself
	// always sets, e.g. anthropic-version).
	APIKey string
	// AzureDeployment is the Azure OpenAI deployment name. Required when
	// Provider is "azure".
	AzureDeployment string
	// AzureAPIVersion overrides the default Azure OpenAI API version. Only
	// meaningful when Provider is "azure".
	AzureAPIVersion string
	// GCPProject is the Google Cloud project ID. Required when Provider is
	// "vertex".
	GCPProject string
	// GCPLocation is the Google Cloud region (e.g. "us-central1"). Required
	// when Provider is "vertex".
	GCPLocation string
}

// intentApplicable reports whether a probe of the given intent has a
// meaningful upstream equivalent for provider. IntentChat always applies:
// every provider adapter accepts a chat-shaped request.
func intentApplicable(intent ProbeIntent, provider string) bool {
	switch intent {
	case IntentModelsList:
		// Azure requires a per-deployment URL, Gemini/Vertex's TransformURL
		// unconditionally builds a generateContent URL — neither has a
		// generic model-listing endpoint reachable this way.
		switch provider {
		case "azure", "vertex", "gemini":
			return false
		}
		return true
	case IntentEmbeddings:
		// Anthropic and Gemini/Vertex have no OpenAI-compatible embeddings
		// endpoint. Azure does (it is OpenAI-compatible per deployment).
		switch provider {
		case "anthropic", "gemini", "vertex":
			return false
		}
		return true
	case IntentChat:
		return true
	default:
		return false
	}
}

// upstreamPathForIntent returns the OpenAI-shaped path passed to
// Adapter.TransformURL (and used directly for nil-adapter providers).
func upstreamPathForIntent(intent ProbeIntent) string {
	switch intent {
	case IntentChat:
		return "chat/completions"
	case IntentEmbeddings:
		return "embeddings"
	case IntentModelsList:
		return "models"
	default:
		return ""
	}
}

// probeModel builds the proxy.Model literal that adapters need for URL,
// body, and header construction from a ProbeTarget.
func probeModel(target ProbeTarget) proxy.Model {
	return proxy.Model{
		Name:            target.ModelName,
		Provider:        target.Provider,
		BaseURL:         target.BaseURL,
		APIKey:          target.APIKey,
		AzureDeployment: target.AzureDeployment,
		AzureAPIVersion: target.AzureAPIVersion,
		GCPProject:      target.GCPProject,
		GCPLocation:     target.GCPLocation,
	}
}

// probeRequestBody builds the fixed, synthetic OpenAI-shaped payload for
// intent and runs it through adapter.TransformRequest when an adapter is
// present. The payload never carries caller-supplied content — only static
// placeholder text — so no prompt or response content is ever part of a
// probe (zero-knowledge).
//
// For IntentChat against Azure, the deployment name is sent as the body's
// model field rather than the canonical model name. This mirrors the
// deployment-name substitution the real proxy path performs for Azure
// requests and predates this function; it is preserved here unchanged.
func probeRequestBody(intent ProbeIntent, target ProbeTarget, adapter proxy.Adapter, model proxy.Model) ([]byte, error) {
	var payload map[string]any
	switch intent {
	case IntentChat:
		upstreamModel := target.ModelName
		if target.Provider == "azure" && target.AzureDeployment != "" {
			upstreamModel = target.AzureDeployment
		}
		payload = map[string]any{
			"model":      upstreamModel,
			"messages":   []map[string]string{{"role": "user", "content": "hi"}},
			"max_tokens": 1,
		}
	case IntentEmbeddings:
		payload = map[string]any{
			"model": target.ModelName,
			"input": "test",
		}
	default:
		return nil, fmt.Errorf("probe request body: intent %d has no synthetic payload", intent)
	}

	body, err := jsonx.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal probe payload: %w", err)
	}

	if adapter == nil {
		return body, nil
	}

	transformed, err := adapter.TransformRequest(body, model)
	if err != nil {
		return nil, fmt.Errorf("adapter transform request: %w", err)
	}
	return transformed, nil
}

// BuildProbeRequest constructs the *http.Request for probing target
// according to intent, reusing the exact same provider adapters
// (proxy.GetAdapter, Adapter.TransformURL, Adapter.TransformRequest,
// Adapter.SetHeaders) that the proxy hot path uses for real traffic. This
// guarantees a probe talks to each provider the same way a real request
// would: Anthropic gets a Messages-API request at /v1/messages, Gemini gets
// a generateContent request with x-goog-api-key, Azure gets a
// deployment-scoped URL with api-key, and Vertex gets a project/location-
// scoped URL with a Bearer token.
//
// It returns ErrProbeNotApplicable when intent has no meaningful upstream
// equivalent for target.Provider; callers must treat that as "skip this
// probe", never as a failed or successful check.
//
// The request body, when present, is a fixed synthetic payload (a one-word
// user message, or a one-word embeddings input) — it never carries caller
// content, and BuildProbeRequest never logs the body it constructs.
func BuildProbeRequest(ctx context.Context, intent ProbeIntent, target ProbeTarget) (*http.Request, error) {
	if !intentApplicable(intent, target.Provider) {
		return nil, ErrProbeNotApplicable
	}

	adapter := proxy.GetAdapter(target.Provider)
	model := probeModel(target)
	upstreamPath := upstreamPathForIntent(intent)

	var method string
	var body []byte
	if intent == IntentModelsList {
		method = http.MethodGet
	} else {
		method = http.MethodPost
		var err error
		body, err = probeRequestBody(intent, target, adapter, model)
		if err != nil {
			return nil, err
		}
	}

	var rawURL string
	if adapter != nil {
		rawURL = adapter.TransformURL(target.BaseURL, upstreamPath, model)
	} else {
		rawURL = strings.TrimRight(target.BaseURL, "/") + "/" + upstreamPath
	}

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("build probe request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	// Mirror setUpstreamHeaders' default: set a Bearer Authorization header
	// before the adapter gets a chance to adjust it. This matters most for
	// Vertex, whose GeminiAdapter.SetHeaders intentionally leaves the
	// Authorization header untouched — the real (non-probe) request path
	// relies on this default having already been set upstream of the
	// adapter. Every other adapter that needs a different scheme removes
	// this header itself in SetHeaders.
	if target.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+target.APIKey)
	}

	if adapter != nil {
		adapter.SetHeaders(req, model)
	}

	return req, nil
}
