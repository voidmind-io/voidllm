package mcp

import (
	"context"
	"encoding/json"
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

// ErrEmptyAuthCredential exposes the unexported errEmptyAuthCredential
// sentinel for white-box testing from the mcp_test package — see that
// error's own doc in http_transport.go for why rawPost and Forward must fail
// closed (never send a request carrying an empty Authorization/custom-header
// credential) when authType is "bearer" with an empty token, or "header"
// with a configured header name but an empty token value. Tests need this to
// assert the failure via errors.Is without being able to construct
// errEmptyAuthCredential themselves.
var ErrEmptyAuthCredential = errEmptyAuthCredential

// OAuthResponseMaxBytes exposes oauthResponseMaxBytes so tests can construct
// a response body exactly at, or one byte over, the ceiling
// OAuthTokenManager.fetchToken and discoverTokenURL enforce, instead of
// duplicating the literal byte count and risking silent drift if the
// constant is ever tuned.
const OAuthResponseMaxBytes = oauthResponseMaxBytes

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
// zero-byte, no-error Read must NOT reset the idle timer (see Read's doc),
// and that Close invokes only closeCancel, never onIdle (see Close's own
// doc) — without needing a full HTTPTransport.Forward round trip. A test
// that does not care about the onIdle/closeCancel distinction (most of this
// package's own idle_timeout_reader_test.go) may simply pass the same
// CancelFunc for both, exactly as HTTPTransport.Forward's own production
// call site does.
func NewIdleTimeoutReader(r io.ReadCloser, idle time.Duration, onIdle, closeCancel context.CancelFunc) io.ReadCloser {
	return newIdleTimeoutReader(r, idle, onIdle, closeCancel)
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

// MaxHeaderParamNameInError exposes maxHeaderParamNameInError so tests can
// assert the exact bound truncateForError enforces — reused by server.go's
// dispatchLegacy/dispatchModern/handleToolsCall for method-name and
// tool-name error text (docs/mcp-v2.md review round, Fund 7) — instead of
// duplicating the literal and risking silent drift if the constant is ever
// tuned.
const MaxHeaderParamNameInError = maxHeaderParamNameInError

// RegisterToolUnsafe registers tool on s exactly like the exported
// RegisterTool, but WITHOUT the x-mcp-header schema validation that method
// now performs at registration time (ToolHeaderParams; see RegisterTool's
// own doc, docs/mcp-v2.md review round Fund 6). It exists purely so
// TestServer_EncodingFailure_* can still force a genuinely malformed schema
// into the registry, to exercise Handle's encoding-failure fallback path —
// something the production RegisterTool now refuses to register at all,
// since a schema that fails to even parse as JSON can never satisfy §4.3's
// constraints either (see ToolHeaderParams' own doc). Production code must
// never call this: it is reachable only from the mcp_test package.
func (s *Server) RegisterToolUnsafe(tool Tool, handler ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools = append(s.tools, tool)
	s.handlers[tool.Name] = registeredTool{handler: handler}
}

// CountToolsListPageTools exposes countToolsListPageTools for direct testing
// of ListTools' pre-decode tool-count bound — in particular for measuring
// its allocation profile against a pathological page (millions of minimal
// {} tool elements) without the unrelated allocation noise a full HTTP round
// trip through net/http would add.
var CountToolsListPageTools = countToolsListPageTools

// ErrJSONSkipMaxDepthExceeded exposes the unexported errJSONSkipMaxDepthExceeded
// sentinel so a test can assert skipJSONValue's own depth guard failed via
// errors.Is, rather than only observing it indirectly through
// countToolsListPageTools' own fail-closed error return (see that
// function's own doc).
var ErrJSONSkipMaxDepthExceeded = errJSONSkipMaxDepthExceeded

// ErrToolCacheRefreshRetriesExhausted exposes the unexported
// errToolCacheRefreshRetriesExhausted sentinel so a test can assert, via
// errors.Is, that RefreshServer gave up after maxToolCacheEntryAttempts
// consecutive non-fetching rounds — see that sentinel's own doc.
var ErrToolCacheRefreshRetriesExhausted = errToolCacheRefreshRetriesExhausted

// ErrToolsListPreScanFailed exposes the unexported errToolsListPreScanFailed
// sentinel so a test can assert, via errors.Is, that ListTools rejected a
// page because countToolsListPageTools' own pre-decode walk failed — rather
// than for any other reason a ListTools call can fail — without depending
// on matching its error text.
var ErrToolsListPreScanFailed = errToolsListPreScanFailed

// SkipJSONValueForTest exposes skipJSONValue for direct testing of its own
// iterative, bounded-depth walk — in particular for proving a value nested
// far deeper than maxJSONSkipDepth is rejected with
// errJSONSkipMaxDepthExceeded (via ErrJSONSkipMaxDepthExceeded above)
// rather than growing this goroutine's own call stack, something only a
// test constructing a pathologically deep value and driving skipJSONValue
// against it directly — not any higher-level, allocation-shaped test — can
// actually observe.
func SkipJSONValueForTest(dec *json.Decoder) error {
	return skipJSONValue(dec)
}

// MaxConcurrentToolsListFetches exposes maxConcurrentToolsListFetches so
// tests can assert the exact concurrency cap ToolCache.fetchSem enforces,
// instead of duplicating the literal and risking silent drift if the
// constant is ever tuned.
const MaxConcurrentToolsListFetches = maxConcurrentToolsListFetches

// SetFetchTimeoutForTest overrides tc's fetchTimeout — the production
// toolsListFetchTimeout by default (see that constant's own doc) — letting a
// test exercise sharedFetch's 2-minute fetch budget (and, since
// maxConcurrentToolsListFetches' semaphore is acquired with the same bounded
// context, its concurrency-slot wait too) without actually waiting out the
// real production duration.
func (tc *ToolCache) SetFetchTimeoutForTest(d time.Duration) {
	tc.fetchTimeout.Store(int64(d))
}

// SharedFetchToolsForTest calls the production sharedFetch directly,
// bypassing entryFor's own OUTER freshness check entirely, for testing
// sharedFetch's own INNER freshness recheck (see that method's own doc) in
// isolation: a test that pre-populates a fresh entry (e.g. via SetTools) and
// then calls this directly proves the recheck inside sharedFetch's
// singleflight callback — not entryFor's separate, outer one — is what
// avoids the redundant fetch, something a call to the exported GetTools
// could never isolate on its own, since GetTools' own entryFor call would
// already short-circuit before ever reaching sharedFetch.
func (tc *ToolCache) SharedFetchToolsForTest(ctx context.Context, serverID string, force bool) ([]Tool, error) {
	e, _, err := tc.sharedFetch(ctx, serverID, force)
	if err != nil {
		return nil, err
	}
	return copyTools(e.tools), nil
}

// SharedFetchForTest calls the production sharedFetch directly and also
// returns its fetched result — whether this singleflight round actually
// reached fetchAndPublish, as opposed to returning early via the
// force-false freshness recheck (see sharedFetch's own doc) — which
// SharedFetchToolsForTest's narrower []Tool-only return cannot expose. Used
// by tests exercising RefreshServer's own retry-once-if-not-fetched
// behavior (see RefreshServer's own doc).
func (tc *ToolCache) SharedFetchForTest(ctx context.Context, serverID string, force bool) (fetched bool, err error) {
	_, fetched, err = tc.sharedFetch(ctx, serverID, force)
	return fetched, err
}

// SetSharedFetchFreshEntryHookForTest installs fn (or, when fn is nil,
// clears the previously installed hook) as sharedFetch's package-level
// force=false freshness-shortcut test hook — see
// sharedFetchFreshEntryHookForTest's own doc. It is a single package-level
// atomic.Pointer[func()] shared by every ToolCache instance in the test
// binary, so a test that sets it must always clear it again before
// returning (defer SetSharedFetchFreshEntryHookForTest(nil)) and must not
// run in parallel with any other test that also sets it — the atomic makes
// the SET/CLEAR itself race-free, it does not make two tests' hooks
// coexist.
func SetSharedFetchFreshEntryHookForTest(fn func()) {
	if fn == nil {
		sharedFetchFreshEntryHookForTest.Store(nil)
		return
	}
	sharedFetchFreshEntryHookForTest.Store(&fn)
}

// SetSharedFetchJoinedHookForTest installs fn (or, when fn is nil, clears the
// previously installed hook) as sharedFetch's package-level per-caller
// joined-round test hook — see sharedFetchJoinedHookForTest's own doc. Same
// single-package-level-pointer discipline as
// SetSharedFetchFreshEntryHookForTest: a test that sets it must always clear
// it again (defer SetSharedFetchJoinedHookForTest(nil)) and must not run in
// parallel with any other test that also sets a shared-fetch test hook.
func SetSharedFetchJoinedHookForTest(fn func(ctx context.Context)) {
	if fn == nil {
		sharedFetchJoinedHookForTest.Store(nil)
		return
	}
	sharedFetchJoinedHookForTest.Store(&fn)
}

// SetSharedFetchResultHookForTest installs fn (or, when fn is nil, clears the
// previously installed hook) as sharedFetch's package-level per-caller
// result-observation test hook — see sharedFetchResultHookForTest's own doc.
// Same single-package-level-pointer discipline as
// SetSharedFetchFreshEntryHookForTest: a test that sets it must always clear
// it again (defer SetSharedFetchResultHookForTest(nil)) and must not run in
// parallel with any other test that also sets a shared-fetch test hook.
func SetSharedFetchResultHookForTest(fn func(ctx context.Context, fetched bool)) {
	if fn == nil {
		sharedFetchResultHookForTest.Store(nil)
		return
	}
	sharedFetchResultHookForTest.Store(&fn)
}

// SetListenMinBackoffForTest overrides listenMinBackoff — runListener's
// exponential-backoff floor and per-attempt doubling base — for a test that
// needs to observe several backoff cycles without waiting out the real
// production floor.
func SetListenMinBackoffForTest(d time.Duration) { listenMinBackoff = d }

// SetListenMaxBackoffForTest overrides listenMaxBackoff — runListener's
// exponential-backoff ceiling (5 minutes in production) — for a test that
// needs to observe backoff actually hitting its cap.
func SetListenMaxBackoffForTest(d time.Duration) { listenMaxBackoff = d }

// SetListenUnsupportedRetryForTest overrides listenUnsupportedRetry —
// runListener's fixed wait after ErrListenUnsupported/ErrListenNotHonored (1
// hour in production) — for a test that needs to observe a retry in that
// bucket without waiting out the real production duration.
func SetListenUnsupportedRetryForTest(d time.Duration) { listenUnsupportedRetry = d }

// SetListenReconnectJitterMaxForTest overrides listenReconnectJitterMax —
// the upper bound of runListener's post-graceful-end/idle reconnect jitter
// (1 second in production) — for a test that needs a tight, deterministic
// upper bound on how long that reconnect can take.
func SetListenReconnectJitterMaxForTest(d time.Duration) { listenReconnectJitterMax = d }

// SetListenThrottleIntervalForTest overrides listenThrottleInterval —
// listenThrottle's own coalescing window (1 second in production) — for a
// test that needs to observe several throttle windows without waiting out
// the real production interval.
func SetListenThrottleIntervalForTest(d time.Duration) { listenThrottleInterval = d }

// SetListenAckHookForTest installs fn (or, when fn is nil, clears the
// previously installed hook) as runListener's own onAck completion test
// hook — see listenAckHookForTest's own doc (listen_manager.go). A test that
// sets it must always clear it again (defer SetListenAckHookForTest(nil))
// and must not run concurrently with any other test that also sets it —
// the same single-package-level-hook discipline
// SetSharedFetchFreshEntryHookForTest already documents.
func SetListenAckHookForTest(fn func(serverID string)) {
	if fn == nil {
		listenAckHookForTest.Store(nil)
		return
	}
	listenAckHookForTest.Store(&fn)
}

// SetListenExitDelayHookForTest installs fn (or, when fn is nil, clears the
// previously installed hook) as runListener's own pre-exit test hook — see
// listenExitDelayHookForTest's own doc (listen_manager.go). Same
// single-package-level-hook discipline as SetListenAckHookForTest.
func SetListenExitDelayHookForTest(fn func()) {
	if fn == nil {
		listenExitDelayHookForTest.Store(nil)
		return
	}
	listenExitDelayHookForTest.Store(&fn)
}

// NewListenThrottleForTest exposes newListenThrottle (listen_manager.go) for
// direct testing of its own leading-edge/trailing-edge coalescing and Stop
// semantics, independent of a full ListenManager/runListener/HTTP round
// trip. The returned value's type is unexported, but its Call and Stop
// methods are exported, so a caller in another package can still use it via
// :=, exactly as NewSSEEventReaderForTest's identical pattern already
// establishes elsewhere in this file.
func NewListenThrottleForTest(fire func()) *listenThrottle {
	return newListenThrottle(fire)
}

// SetListenJitterFuncForTest installs fn (or, when fn is nil, clears the
// previously installed override) as randDuration's own jitter-source test
// hook — see randDuration's own doc (listen_manager.go) — letting a test
// assert an EXACT reconnect/backoff wait for a given bound instead of only a
// looser randomized-within-bound one. Same single-package-level-hook
// discipline as SetListenAckHookForTest.
func SetListenJitterFuncForTest(fn func(max time.Duration) time.Duration) {
	if fn == nil {
		listenJitterFuncForTest.Store(nil)
		return
	}
	listenJitterFuncForTest.Store(&fn)
}

// NewSSEEventReaderForTest exposes newSSEEventReader (sse_reader.go) so a
// black-box test can drive HTTPTransport.Listen's own incremental,
// live-connection SSE parser directly — multi-line data, comments, CR/CRLF/LF
// line endings, an event split across separate underlying Reads, the
// per-event size cap, and EOF mid-event — independent of any HTTP transport
// plumbing. The returned value's type is unexported, but its Next method
// (and the sseEvent fields it returns) are exported, so a caller in another
// package can still use it via :=, exactly as every other ForTest
// constructor in this file already does for its own unexported return type.
func NewSSEEventReaderForTest(r io.Reader) *sseEventReader {
	return newSSEEventReader(r)
}

// ErrSSEEventTooLargeForTest exposes errSSEEventTooLarge for a black-box test
// to assert sseEventReader.Next's per-event size cap via errors.Is.
var ErrSSEEventTooLargeForTest = errSSEEventTooLarge

// ErrSSELineTooLargeForTest exposes errSSELineTooLarge for a black-box test
// to assert sseEventReader.readLine's per-line size cap via errors.Is.
var ErrSSELineTooLargeForTest = errSSELineTooLarge

// MaxSSELineBytesForTest exposes maxSSELineBytes so a test can size its
// fixture relative to the real production per-line cap instead of
// hardcoding a second copy of the same limit.
const MaxSSELineBytesForTest = maxSSELineBytes

// MaxSSEEventDataBytesForTest exposes maxSSEEventDataBytes so a test can size
// its fixture relative to the real production per-event cap instead of
// hardcoding a second copy of the same limit.
const MaxSSEEventDataBytesForTest = maxSSEEventDataBytes

// SSELineOverheadBytesForTest exposes sseLineOverheadBytes so a test can
// compute the exact per-line cost Next's own per-event budget check charges
// (raw line length plus this fixed floor) instead of hardcoding a second
// copy of the same constant.
const SSELineOverheadBytesForTest = sseLineOverheadBytes

// ClassifySSELineForTest exposes classifySSELine — the single field-
// recognition implementation shared by extractSSEResult (the fully-buffered
// path) and sseEventReader (the incremental path) — for a black-box test to
// verify directly. The returned kind is one of the SSELineKindXxx constants
// below, since sseLineKind itself is unexported.
func ClassifySSELineForTest(line []byte) (kind int, value []byte) {
	k, v := classifySSELine(line)
	return int(k), v
}

// SSELineKindOther, SSELineKindBlank, SSELineKindComment, SSELineKindData,
// SSELineKindID, and SSELineKindEvent expose the sseLineKind enum's own
// values (sse_reader.go), in the same order, for ClassifySSELineForTest's
// caller to compare against.
const (
	SSELineKindOther   = int(sseLineOther)
	SSELineKindBlank   = int(sseLineBlank)
	SSELineKindComment = int(sseLineComment)
	SSELineKindData    = int(sseLineData)
	SSELineKindID      = int(sseLineID)
	SSELineKindEvent   = int(sseLineEvent)
)

// SplitSSEBodyForTest exposes splitSSEBody — the CRLF/CR/LF line-ending
// normalization extractSSEResult relies on — for direct testing.
func SplitSSEBodyForTest(body []byte) [][]byte {
	return splitSSEBody(body)
}

// ExtractSSEResult exposes the real, production extractSSEResult
// (http_transport.go) — the fully-buffered SSE parser rawPost/Call use — for
// a black-box test to run directly against the same input bytes it drives
// through sseEventReader (via NewSSEEventReaderForTest), proving the two
// parsers dispatch identical event data for a matching id rather than merely
// a hand-rolled replica of extractSSEResult's own dispatch loop (see
// sse_reader_parity_test.go's own doc for why a second, independent
// implementation of the same loop cannot rule out both containing the
// identical mistake).
func ExtractSSEResult(body []byte, wantID []byte) ([]byte, error) {
	return extractSSEResult(body, wantID)
}

// ErrListenMalformedEventForTest, ErrListenUpstreamErrorForTest,
// ErrListenUnexpectedResponseForTest, and ErrUnsolicitedContentEncodingForTest
// expose Listen's own unexported error sentinels (listen_client.go,
// http_transport.go) for a black-box test to assert via errors.Is — the same
// need every other ForTest error alias in this file already serves.
var (
	ErrListenMalformedEventForTest       = errListenMalformedEvent
	ErrListenUpstreamErrorForTest        = errListenUpstreamError
	ErrListenUnexpectedResponseForTest   = errListenUnexpectedResponse
	ErrUnsolicitedContentEncodingForTest = errUnsolicitedContentEncoding
)

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
