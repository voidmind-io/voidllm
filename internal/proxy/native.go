package proxy

import (
	"bufio"
	"errors"
	"fmt"
	"strings"
)

// apiFormat identifies the wire format of a proxied endpoint. It is derived
// once from the request path and decides whether a request is translated by a
// provider adapter (chat) or forwarded natively (messages, responses).
type apiFormat int

const (
	// formatOther covers every endpoint that is neither chat nor native, such as
	// embeddings, models, rerank, score, images and audio.
	formatOther apiFormat = iota
	// formatChat covers the OpenAI chat/completions and completions endpoints.
	formatChat
	// formatMessages covers the Anthropic Messages endpoints (messages and
	// messages/count_tokens), forwarded natively to Anthropic.
	formatMessages
	// formatResponses covers the OpenAI Responses endpoint, forwarded natively to
	// OpenAI-compatible providers.
	formatResponses
)

// messagesCountTokensPath is the upstream path of the Anthropic token counting
// endpoint. It carries no generation and produces no usage event.
const messagesCountTokensPath = "messages/count_tokens"

// formatForPath derives the apiFormat from a cleaned upstream path (the request
// path without the /v1/ prefix).
func formatForPath(upstreamPath string) apiFormat {
	switch upstreamPath {
	case "chat/completions", "completions":
		return formatChat
	case "messages", messagesCountTokensPath:
		return formatMessages
	case "responses":
		return formatResponses
	}
	return formatOther
}

// native reports whether f is forwarded without adapter translation.
func (f apiFormat) native() bool {
	return f == formatMessages || f == formatResponses
}

// isUnsupportedNativeSubpath reports whether upstreamPath addresses a
// sub-resource of a native endpoint that VoidLLM does not proxy (for example
// GET or DELETE /v1/responses/{id}).
func isUnsupportedNativeSubpath(upstreamPath string) bool {
	return strings.HasPrefix(upstreamPath, "responses/") ||
		(strings.HasPrefix(upstreamPath, "messages/") && upstreamPath != messagesCountTokensPath)
}

// providerSupportsFormat reports whether a deployment with the given provider
// can serve format. Non-native formats are always supported here; their
// per-provider handling lives in the adapters.
func providerSupportsFormat(providerName string, f apiFormat) bool {
	switch f {
	case formatMessages:
		return providerName == "anthropic"
	case formatResponses:
		switch providerName {
		case "openai", "custom", "vllm", "ollama":
			return true
		}
		return false
	}
	return true
}

// errNativeIncompatible is returned by buildUpstreamRequest when a native
// endpoint is requested for a deployment whose provider cannot serve it. No
// response has been written; tryModel skips the deployment.
var errNativeIncompatible = errors.New("endpoint not supported for provider")

// nativeBlock explains why a model cannot serve a native endpoint.
type nativeBlock int

const (
	// nativeUsable means the model has at least one deployment that can serve the
	// native endpoint.
	nativeUsable nativeBlock = iota
	// nativeProviderBlocked means no deployment has a compatible provider.
	nativeProviderBlocked
	// nativePIIBlocked means compatible deployments exist but PII anonymization
	// applies to all of them, which native endpoints cannot honor.
	nativePIIBlocked
)

// piiAppliesTo reports whether PII anonymization must be applied for the given
// deployment, assuming the PII engine is active. The decision follows this
// priority order:
//
//  1. dep.PIIFilter != nil  -> use *dep.PIIFilter (explicit per-deployment)
//  2. m.PIIFilter != nil    -> use *m.PIIFilter (explicit per-model)
//  3. default               -> anonymize when the destination is NOT private
//
// An explicit pii_filter: true on a private destination forces anonymization;
// an explicit pii_filter: false on a public destination suppresses it.
func piiAppliesTo(dep Deployment, m Model) bool {
	if dep.PIIFilter != nil {
		return *dep.PIIFilter
	}
	if m.PIIFilter != nil {
		return *m.PIIFilter
	}
	return !dep.destPrivate
}

// nativeUsableModel narrows m to the deployments that can serve the native
// endpoint f: deployments with an incompatible provider are dropped, and so are
// deployments for which PII anonymization applies (the PII walkers only
// understand chat completions, so a native request is never forwarded to a
// destination that requires scanning). The returned model is a copy; the
// registry's own state is never modified. When nothing is usable the reason is
// reported and the model is returned unchanged.
func (p *ProxyHandler) nativeUsableModel(m Model, f apiFormat) (Model, nativeBlock) {
	piiOn := p.PIIEngine != nil

	if p.Router == nil || len(m.Deployments) == 0 {
		// tryModel synthesizes one candidate from the model's own fields here.
		d := Deployment{Provider: m.Provider, destPrivate: m.destPrivate, PIIFilter: m.PIIFilter}
		if !providerSupportsFormat(d.Provider, f) {
			return m, nativeProviderBlocked
		}
		if piiOn && piiAppliesTo(d, m) {
			return m, nativePIIBlocked
		}
		return m, nativeUsable
	}

	kept := make([]Deployment, 0, len(m.Deployments))
	providerOK := false
	for _, d := range m.Deployments {
		if !providerSupportsFormat(d.Provider, f) {
			continue
		}
		providerOK = true
		if piiOn && piiAppliesTo(d, m) {
			continue
		}
		kept = append(kept, d)
	}
	if len(kept) == 0 {
		if providerOK {
			return m, nativePIIBlocked
		}
		return m, nativeProviderBlocked
	}
	m.Deployments = kept
	return m, nativeUsable
}

// nextFallback returns the next fallback model for currentName. For native
// endpoints, fallback targets without a usable deployment are skipped (and
// marked visited) rather than ending the chain, and the returned model is
// narrowed to its usable deployments.
func (p *ProxyHandler) nextFallback(currentName string, visited map[string]bool, f apiFormat) (Model, bool) {
	for {
		next, ok := p.Registry.FallbackFor(currentName, visited)
		if !ok {
			return Model{}, false
		}
		if !f.native() {
			return next, true
		}
		usable, block := p.nativeUsableModel(next, f)
		if block == nativeUsable {
			return usable, true
		}
		visited[next.Name] = true
		currentName = next.Name
	}
}

// nativeUpstreamURL builds the upstream URL of a native endpoint. Anthropic
// Messages live under {base}/v1/; Responses under {base}/ like every other
// OpenAI-compatible endpoint.
func nativeUpstreamURL(baseURL, upstreamPath string, f apiFormat) string {
	base := strings.TrimRight(baseURL, "/")
	if f == formatMessages {
		return base + "/v1/" + upstreamPath
	}
	return base + "/" + upstreamPath
}

// writeFormatAbortEvent writes the content-free abort event matching the wire
// format of f and flushes. Chat and other formats use the OpenAI-shaped event
// of writeStreamAbortEvent. Messages streams get an Anthropic "error" event and
// Responses streams a Responses "error" event. code must be a static string
// constant. It returns whether the event was fully written and flushed.
func writeFormatAbortEvent(w *bufio.Writer, f apiFormat, code string) bool {
	var err error
	switch f {
	case formatMessages:
		_, err = w.WriteString("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"api_error\",\"message\":\"stream aborted\"}}\n\n")
	case formatResponses:
		_, err = fmt.Fprintf(w, "event: error\ndata: {\"type\":\"error\",\"code\":%q,\"message\":\"stream aborted\"}\n\n", code)
	default:
		return writeStreamAbortEvent(w, code)
	}
	if err != nil {
		return false
	}
	return w.Flush() == nil
}
