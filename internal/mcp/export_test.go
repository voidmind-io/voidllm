package mcp

import (
	"context"
	"io"
	"net/http"
	"time"
)

// ErrSessionExpired exposes the unexported errSessionExpired sentinel for
// white-box testing from the mcp_test package — see that error's own doc for
// why it is unexported in production code: it never reaches any caller
// outside this package, so nothing outside it should be able to errors.Is
// against it directly, but tests still need to construct and recognize it to
// verify that contract.
var ErrSessionExpired = errSessionExpired

// ErrOAuthNotConfigured exposes the unexported errOAuthNotConfigured sentinel
// for white-box testing from the mcp_test package — see that error's own doc
// in http_transport.go for why rawPost and Forward must fail closed (never
// send a request with no Authorization header) when authType is "oauth" but
// the transport was built without a usable oauth manager and config. Tests
// need this to assert the failure via errors.Is without being able to
// construct errOAuthNotConfigured themselves.
var ErrOAuthNotConfigured = errOAuthNotConfigured

// UnwrapToolResult exposes the internal unwrapToolResult function for
// white-box testing from the mcp_test package.
var UnwrapToolResult = unwrapToolResult

// SchemaToTypeScript exposes the internal schemaToTypeScript function for
// white-box testing from the mcp_test package.
var SchemaToTypeScript = schemaToTypeScript

// NewLegacyDialect exposes newLegacyDialect for testing the legacy
// ServerDialect's Decode/Encode behavior directly, without going through
// Negotiate and Server.Handle.
var NewLegacyDialect = newLegacyDialect

// NewDialect2026 exposes newDialect2026 for testing the 2026-07-28
// ServerDialect's Decode/Encode behavior directly, without going through
// Negotiate and Server.Handle.
var NewDialect2026 = newDialect2026

// HintForError exposes hintForError for testing the JSON-RPC error code →
// StatusHint mapping (MCP Streamable HTTP §4.5/§4.6, docs/mcp-v2.md §4.5,
// §4.6) directly and exhaustively, without needing a full Handle call for
// every (code, era) combination.
var HintForError = hintForError

// NewLegacyClientDialect exposes newLegacyClientDialect for testing the
// legacy ClientDialect's Warmup/Prepare/Parse behavior directly, without
// going through HTTPTransport's era resolution and caching.
var NewLegacyClientDialect = newLegacyClientDialect

// NewDialect2026Client exposes newDialect2026Client for testing the
// 2026-07-28 ClientDialect's Warmup/Prepare/Parse behavior directly, without
// going through HTTPTransport's era resolution and caching.
var NewDialect2026Client = newDialect2026Client

// ProbeEra exposes HTTPTransport.probeEra for direct testing of the
// docs/mcp-v2.md §4.6 era-classification table against a fake upstream,
// independent of Call's binding cache, Warmup, and session-retry behavior —
// none of which probeEra itself performs.
func (t *HTTPTransport) ProbeEra(ctx context.Context) (Version, error) {
	return t.probeEra(ctx)
}

// InvalidateBinding exposes an unconditional reset of HTTPTransport's
// published bindingResolution, for direct testing of the "next call
// re-probes" contract (see PostURL) without needing to drive a full
// reinitAndRetry failure through Call. Unlike the production
// invalidateBinding — which only discards a resolution via compare-and-swap
// against the exact *eraBinding the caller captured (see its doc) — this
// test helper does not need that precision: a test driving it directly
// always wants the reset to happen regardless of what is currently
// published.
func (t *HTTPTransport) InvalidateBinding() {
	t.resolution.Store(nil)
}

// PostURL exposes the postURL of HTTPTransport's currently resolved
// *eraBinding, read from the same atomically published bindingResolution the
// production code reads, for testing that invalidateBinding clears it —
// leaving it set to a POST target resolved for an era that just proved wrong
// would misroute every request the next resolveBinding prepares for a
// possibly different era. Returns "" when no binding is currently resolved,
// exactly as postTarget falls back to the base endpoint in that case.
func (t *HTTPTransport) PostURL() string {
	res := t.resolution.Load()
	if res == nil || res.binding == nil {
		return ""
	}
	return res.binding.postURL
}

// SetBindingErrAt overrides the deadline resolveBinding compares against to
// decide whether a cached probe failure is still valid, letting tests
// simulate bindingProbeErrorTTL elapsing without a real sleep. at is
// interpreted exactly as the pre-snapshot implementation's bindingErrAt
// field was: the moment the failure was recorded, with the TTL window
// itself added on top when computing the new errUntil deadline, so a caller
// backdating at by more than bindingProbeErrorTTL reliably simulates an
// already-elapsed window. It has no effect unless a probe failure is
// already cached (i.e. a prior resolveBinding call — reached via Call or
// ListTools — already failed); this only backdates or postdates that cached
// failure's deadline, it never fabricates one.
func (t *HTTPTransport) SetBindingErrAt(at time.Time) {
	cur := t.resolution.Load()
	if cur == nil || cur.binding != nil {
		return
	}
	t.resolution.Store(&bindingResolution{err: cur.err, errUntil: at.Add(bindingProbeErrorTTL).UnixNano()})
}

// BindingProbeErrorTTL exposes bindingProbeErrorTTL so tests can compute
// timestamps relative to the real production TTL (e.g. via SetBindingErrAt)
// instead of duplicating the literal duration and risking silent drift if
// the constant is ever tuned.
var BindingProbeErrorTTL = bindingProbeErrorTTL

// CurrentBindingGeneration returns an opaque handle to the *eraBinding
// currently published by resolveBinding (nil if none has been resolved yet),
// for testing invalidateBinding's compare-and-swap directly — see
// InvalidateBindingIfMatches. Two calls made before and after a
// re-resolution return handles that compare unequal via ==, exactly as the
// underlying *eraBinding pointers do; this is what lets a test capture "the
// binding generation in effect right now" and later assert whether a stale
// one was, or was not, allowed to discard a newer one.
func (t *HTTPTransport) CurrentBindingGeneration() any {
	res := t.resolution.Load()
	if res == nil {
		return nil
	}
	return res.binding
}

// InvalidateBindingIfMatches calls the real, production invalidateBinding —
// which only discards the published resolution via compare-and-swap against
// the exact *eraBinding the caller captured (see its doc) — with expected, a
// handle previously obtained from CurrentBindingGeneration. Unlike
// InvalidateBinding's unconditional reset, this exercises the precision the
// production code depends on: a late-returning caller that captured an old
// generation must never be able to discard a generation some other,
// concurrent caller has already re-resolved in the meantime. expected may be
// nil (matching a not-yet-resolved transport) or a value of any other
// dynamic type (which simply never matches, so the call is a no-op) —
// callers of this test helper are expected to only ever pass what
// CurrentBindingGeneration returned.
func (t *HTTPTransport) InvalidateBindingIfMatches(expected any) {
	b, _ := expected.(*eraBinding)
	t.invalidateBinding(b)
}

// NewHTTPTransportWithResponseHeaderTimeout builds an HTTPTransport wired
// exactly like NewHTTPTransport, except the underlying *http.Transport's
// ResponseHeaderTimeout is responseHeaderTimeout instead of the fixed
// production value (ssrfResponseHeaderTimeout, 30s) NewSSRFSafeTransport
// always applies.
//
// There is no way to construct a short-ResponseHeaderTimeout HTTPTransport
// through the public NewHTTPTransport constructor: ssrfResponseHeaderTimeout
// is an unexported package constant baked into NewSSRFSafeTransport, with no
// parameter anywhere in the exported API that reaches it. This is a genuine
// testability gap in the production code — see this test-only constructor's
// callers (TestForward_ResponseHeaderTimeout_*) for what it exists to prove
// without waiting out a real 30 seconds; if that gap is ever closed by
// threading a configurable ResponseHeaderTimeout through NewHTTPTransport
// itself, this helper should be removed in favor of the real constructor.
func NewHTTPTransportWithResponseHeaderTimeout(endpoint string, responseHeaderTimeout, streamIdleTimeout time.Duration) *HTTPTransport {
	transport := NewSSRFSafeTransport(true)
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	noRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &HTTPTransport{
		endpoint:      endpoint,
		authType:      "none",
		clientInfo:    ClientInfo{Name: "voidllm-test", Version: "test"},
		pinnedVersion: V20260728,
		client: &http.Client{
			Timeout:       5 * time.Second,
			Transport:     transport,
			CheckRedirect: noRedirect,
		},
		streamClient: &http.Client{
			// No Timeout — see streamClient's own field doc: it must not be
			// severed by elapsed wall-clock time, only by the idle timeout.
			Transport:     transport,
			CheckRedirect: noRedirect,
		},
		streamIdleTimeout: streamIdleTimeout,
	}
}

// DecodeClientSessionScope exposes decodeClientSessionScope for direct
// testing of the length-prefixed encoding NewClientSessionScope produces —
// see that function's own doc, and decodeClientSessionScope's, for why this
// is purely an LRU-partitioning concern for SessionRegistry's two-level
// (organization, API key) split, never the thing Record/Known actually key
// their session bookkeeping by.
func DecodeClientSessionScope(scope SessionScope) (orgID, apiKeyID string, ok bool) {
	return decodeClientSessionScope(scope)
}

// NewIdleTimeoutReader exposes newIdleTimeoutReader for direct unit testing
// of idle_timeout_reader.go's timer-reset semantics — in particular that a
// zero-byte, no-error Read must NOT reset the idle timer (see Read's doc) —
// without needing a full HTTPTransport.Forward round trip.
func NewIdleTimeoutReader(r io.ReadCloser, idle time.Duration, cancel context.CancelFunc) io.ReadCloser {
	return newIdleTimeoutReader(r, idle, cancel)
}

// ParseCacheHint exposes parseCacheHint for direct testing of the
// CacheableResult ttlMs/cacheScope interpretation (MCP 2026-07-28 §5,
// docs/mcp-v2.md §5) without needing a full dialect2026Client.Parse call for
// every (ttlMs, cacheScope) shape.
var ParseCacheHint = parseCacheHint

// MaxUpstreamToolTTL exposes maxUpstreamToolTTL so tests can assert the exact
// ceiling ToolCache.resolveTTL clamps an upstream-supplied ttlMs to, instead
// of duplicating the literal duration and risking silent drift if the
// constant is ever tuned.
const MaxUpstreamToolTTL = maxUpstreamToolTTL

// MinToolFetchInterval exposes minToolFetchInterval so tests can assert the
// exact floor ToolCache.resolveTTL clamps an upstream-supplied ttlMs to, and
// compute real-time waits relative to the production value rather than a
// hardcoded literal.
const MinToolFetchInterval = minToolFetchInterval

// ResolveTTL exposes ToolCache.resolveTTL for direct testing of the
// CacheHint -> (ttl, neverExpires) interpretation (see that method's own doc)
// without needing to drive a fetch through GetTools/RefreshServer for every
// hint shape under test.
func (tc *ToolCache) ResolveTTL(hint CacheHint) (ttl time.Duration, neverExpires bool) {
	return tc.resolveTTL(hint)
}

// ScopedStateCount returns the number of distinct SessionScope entries the
// currently resolved *eraBinding has ever created a scopedState for (see
// eraBinding.stateFor), for testing that EraModern's Call genuinely skips
// stateFor and ensureWarm entirely (see Call's doc) rather than merely
// producing the same visible I/O (no session header, no initialize) that a
// no-op Warmup would produce even if stateFor HAD been called. Returns 0 if
// no binding is resolved yet.
func (t *HTTPTransport) ScopedStateCount() int {
	res := t.resolution.Load()
	if res == nil || res.binding == nil {
		return 0
	}
	res.binding.scopesMu.Lock()
	defer res.binding.scopesMu.Unlock()
	return len(res.binding.scopes)
}
