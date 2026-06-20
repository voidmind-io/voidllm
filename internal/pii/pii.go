// Package pii provides in-memory PII detection and pseudonymization for
// LLM proxy requests. It intercepts outbound request bodies, replaces
// personally-identifiable values with deterministic pseudonyms, and
// restores the originals in the response — all without persisting or
// logging any sensitive content.
//
// Zero-knowledge guarantee: no PII value, original or pseudonymized, is
// ever written to logs, the database, or any usage event. The mapping
// lives exclusively in the per-request Filter and is garbage-collected
// when the request finishes.
package pii

import (
	"errors"
	"strings"
)

// maxPIIMappings is the maximum number of unique PII-to-pseudonym mappings
// permitted within a single request. A request body of 20 MB could
// theoretically contain up to ~1 million short values; bounding the map
// prevents unbounded memory growth while still covering every realistic
// document. When the cap is reached, AnonymizeJSON returns an error and
// the proxy rejects the request (fail-closed).
const maxPIIMappings = 10_000

// Span describes a single PII match within a string. Start and End are
// byte offsets (half-open interval [Start, End)) into the original text.
// Type is the detector-assigned label (e.g. "EMAIL", "IBAN").
type Span struct {
	// Start is the inclusive start byte offset of the matched value.
	Start int
	// End is the exclusive end byte offset of the matched value.
	End int
	// Type is the PII category label assigned by the detector.
	Type string
}

// Detector finds PII spans within a plain-text string. Implementations
// must be safe for concurrent reuse across requests — they may not carry
// per-request state. All expensive initialisation (regexp compilation,
// model loading) must happen in the constructor, not in Find.
type Detector interface {
	// Find returns all non-overlapping PII spans found in text.
	// The returned slice may be nil or empty when no PII is detected.
	// Overlapping matches are resolved according to each implementation's
	// documented policy (typically: longest match or leftmost wins).
	Find(text string) []Span
}

// Engine is the shared, request-independent factory for PII filters. It
// holds the compiled detectors and the per-installation pseudonym secret
// derived from the main encryption key. Create one Engine at startup and
// call NewFilter once per proxy request.
//
// Engine must not be used after Close has been called.
type Engine struct {
	secret    []byte
	detectors []Detector
}

// NewEngine constructs an Engine from a pre-derived pseudonym secret and
// a list of detectors. The secret should be derived from the installation
// encryption key using HKDF with a stable info string so that the same
// value always produces the same pseudonym across requests (within a
// single installation lifetime). The caller owns the secret slice; Engine
// makes an internal copy.
func NewEngine(secret []byte, detectors []Detector) *Engine {
	s := make([]byte, len(secret))
	copy(s, secret)
	return &Engine{
		secret:    s,
		detectors: detectors,
	}
}

// NewFilter creates a fresh, empty Filter scoped to orgID for a single
// proxy request. The orgID is incorporated into every pseudonym derivation
// so that the same PII value maps to different tokens across organisations,
// preventing cross-tenant correlation.
//
// The returned Filter is not safe for concurrent use; each request must
// create its own instance via this method.
func (e *Engine) NewFilter(orgID string) *Filter {
	return &Filter{
		secret:    e.secret,
		orgID:     orgID,
		detectors: e.detectors,
		fwd:       make(map[string]string),
		rev:       make(map[string]string),
	}
}

// Close zeros the Engine's internal secret material. After Close,
// NewFilter produces Filters whose pseudonyms are derived from a zeroed
// key and will not match any previously issued pseudonyms. Call Close
// exactly once at application shutdown, after all in-flight requests have
// completed.
func (e *Engine) Close() {
	for i := range e.secret {
		e.secret[i] = 0
	}
}

// Filter is a per-request PII anonymization context. It holds the
// forward map (original -> pseudonym) used to anonymize outbound content
// and the reverse map (pseudonym -> original) used to restore values in
// the response. A Filter must not be shared between goroutines or reused
// across requests.
type Filter struct {
	secret    []byte
	orgID     string
	detectors []Detector
	// fwd maps (type + NUL + NORMALIZED value) to their pseudonyms, ensuring
	// that semantically equivalent values (e.g. email addresses differing only
	// in case) always map to the same pseudonym. Using the normalized form as
	// the key guarantees that "User@x.com" and "user@x.com" share one fwd entry
	// and one rev entry. The rev entry is set to the FIRST-SEEN original form
	// (see pseudonymFor) and is never overwritten, so restore always returns the
	// original form as first encountered in the request.
	fwd map[string]string
	// rev maps pseudonyms back to the original values for response restore.
	// Each pseudonym maps to the first-seen original form; subsequent occurrences
	// of a semantically equivalent value (same type + normalized form) reuse the
	// same fwd entry and do not overwrite rev.
	rev map[string]string
	// replacer is built lazily the first time Restore is called and cached
	// for subsequent calls. Streaming restore calls Restore once per SSE
	// chunk, so avoiding repeated construction matters on the hot path.
	replacer *strings.Replacer
}

// AnonymizeJSON replaces PII found in PII-bearing fields of an
// OpenAI-shaped request body with deterministic pseudonyms. The returned
// slice is a new allocation; the input is never modified.
//
// Fail-closed: when the body cannot be parsed, reassembled, or when the
// unique-mapping cap is exceeded, an error is returned. The caller must
// reject the request rather than forward the original body, because a
// later routing hop may be external and would receive raw PII.
// Error messages never contain body content or PII values.
func (f *Filter) AnonymizeJSON(body []byte) ([]byte, error) {
	out, err := anonymizeWithDetectors(body, f.detectors, func(typ, value string) (string, error) {
		return f.pseudonymFor(typ, value)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Restore replaces all known pseudonyms in text with their original
// values. It is safe to call on any byte slice, not just JSON — the
// replacement is a plain string substitution over the raw bytes. When
// nothing was anonymized (Touched returns false), Restore is a no-op and
// returns the original slice without allocating.
//
// The internal strings.Replacer is built once on the first call and
// cached for subsequent calls (relevant for streaming restore, where
// Restore may be called once per SSE chunk).
func (f *Filter) Restore(text []byte) []byte {
	if len(f.rev) == 0 {
		return text
	}
	if f.replacer == nil {
		pairs := make([]string, 0, len(f.rev)*2)
		for pseudo, orig := range f.rev {
			pairs = append(pairs, pseudo, orig)
		}
		f.replacer = strings.NewReplacer(pairs...)
	}
	return []byte(f.replacer.Replace(string(text)))
}

// Touched reports whether any PII was detected and replaced during
// AnonymizeJSON. Callers can skip the Restore step when this returns
// false, avoiding an unnecessary allocation on the response path.
func (f *Filter) Touched() bool {
	return len(f.rev) > 0
}

// pseudonymFor returns the stable pseudonym for (typ, value), creating
// and recording the mapping on first call. Subsequent calls with the same
// arguments return the cached pseudonym so that repeated occurrences of
// the same PII value are replaced consistently throughout the request.
//
// Case-normalization semantics: the fwd map is keyed on (type, normalizedValue)
// so that semantically equivalent values map to the same pseudonym. For example,
// "User@x.com" and "user@x.com" both normalize to "user@x.com" (EMAIL type),
// share one fwd entry, and produce the same pseudonym. The rev entry maps the
// pseudonym to the FIRST-SEEN original form and is never overwritten — restore
// always returns the first form encountered in the request (deterministic,
// no data loss). Subsequent case variants of the same canonical value use the
// existing fwd entry and leave rev unchanged.
//
// Returns an error when the mapping cap (maxPIIMappings) would be exceeded.
func (f *Filter) pseudonymFor(typ, value string) (string, error) {
	// Key on the normalized form so that case variants share one mapping.
	norm := normalizeValue(typ, value)
	key := typ + "\x00" + norm
	if p, ok := f.fwd[key]; ok {
		return p, nil
	}
	if len(f.fwd) >= maxPIIMappings {
		return "", errors.New("pii: request exceeds maximum unique PII mapping count")
	}
	p := pseudonym(f.secret, f.orgID, typ, value) // pseudonym also normalizes internally
	f.fwd[key] = p
	// Set rev only on first occurrence: subsequent case variants of the same
	// canonical value reuse the fwd entry (above) and never reach here.
	f.rev[p] = value
	return p, nil
}
