package mcp

import (
	"bytes"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// errSessionExpired is returned internally by doCall when a legacy upstream
// MCP server responds with HTTP 404, indicating the session ID is no longer
// valid. Call handles it by re-initializing exactly once (see
// reinitAndRetry) and never returns it to its own caller — so, unlike
// ErrSSENotSupported, it is unexported: a sentinel a caller can never
// actually observe via errors.Is must not invite one to try.
var errSessionExpired = errors.New("mcp session expired")

// errOAuthNotConfigured is returned by rawPost and Forward when authType is
// "oauth" but the transport was built without a usable oauth manager and
// config. This happens when MCPTransportCache.LoadAll could not decrypt the
// stored OAuth client secret (e.g. after an encryption key rotation) and
// logged the failure while still constructing the transport with a nil
// oauthConfig. Both call sites must fail closed here rather than silently
// sending the request without an Authorization header, so this sentinel is
// exported package-internally to let a test assert the failure via
// errors.Is.
var errOAuthNotConfigured = errors.New("oauth not configured")

// errEmptyAuthCredential is returned by rawPost and Forward when authType is
// "bearer" or "header" but the credential that would be sent is empty — an
// empty authToken for either type, or an empty authHeader name for "header".
// This is the same fail-open class errOAuthNotConfigured already closes for
// authType "oauth". Before this sentinel existed, neither branch checked its
// value at all — only "header" checked that authHeader (the header NAME) was
// non-empty, never that authToken (the header VALUE) was — so a transport
// built with an empty authToken still sent "Authorization: Bearer " (or an
// equally empty configured header) instead of failing the call. The two
// callers that construct an HTTPTransport from a stored, encrypted credential
// (MCPTransportCache.LoadAll and buildAdHocTransport, mcp_proxy.go) now both
// already fail closed BEFORE ever reaching here — a decrypt failure aborts
// the transport's construction entirely rather than leaving authToken at its
// zero value — so this check is the second, independent layer of the same
// defense: it also catches an authToken that was simply never configured to
// begin with (no decrypt failure involved at all), and it remains the last
// line of defense against any future call site that constructs an
// HTTPTransport some other way. Both authType switches must fail closed here
// regardless, rather than silently sending a credential-carrying header with
// no credential in it, so this sentinel is exported package-internally to
// let a test assert the failure via errors.Is. The message never names which
// of the two (missing header name vs. missing token value) applied, nor the
// configured header name itself — both are operator configuration, not
// upstream or caller content, but the message stays generic anyway to keep
// this sentinel's text stable regardless of which branch produced it.
var errEmptyAuthCredential = errors.New("auth credential is empty")

// ErrSSENotSupported is returned by probeEra when the upstream MCP server
// uses the deprecated SSE transport (pre 2025-03-26 spec). SSE requires
// a persistent bidirectional connection that VoidLLM does not yet support.
var ErrSSENotSupported = errors.New("server uses deprecated SSE transport (not supported, use Streamable HTTP)")

// errToolsListDecodeFailed is wrapped, never the raw jsonx.Unmarshal error
// itself, into ListTools' returned error when an upstream's tools/list
// response fails to decode as JSON-RPC — see that call site's own comment
// for why: internal/jsonx's decoder quotes a window of the source bytes
// around a syntax error, and those bytes are upstream-controlled response
// content this repo's zero-knowledge logging contract must never let reach
// a log line via err.Error().
var errToolsListDecodeFailed = errors.New("mcp: tools/list response decode failed")

// Pagination guards for ListTools (MCP tools/list cursor pagination):
// nextCursor is an opaque string entirely under the upstream's control, so
// nothing about its shape structurally bounds how long ListTools could keep
// following it. An upstream that never lets nextCursor go absent, or that
// echoes a cursor it already handed out, would otherwise make ListTools loop
// forever, accumulate an unbounded tool list in memory, or repeat the exact
// same request indefinitely. Each guard below fails the WHOLE fetch, never a
// subset of the pages already read — see ListTools' own doc for why a
// partial listing must never be returned to a caller instead.
const (
	// maxToolsListPages bounds how many tools/list requests ListTools will
	// send for one fetch — the initial request plus every nextCursor-driven
	// follow-up.
	maxToolsListPages = 100
	// maxToolsListTools bounds the total number of tools ListTools will
	// accumulate across every page of one fetch.
	maxToolsListTools = 10_000
	// maxToolsListCursorLen bounds the byte length of a single nextCursor
	// ListTools will echo back as the following request's params.cursor.
	maxToolsListCursorLen = 4096
	// maxToolsListTotalBytes bounds the sum of len(result.Body) across every
	// page of one fetch. This is distinct from both rawPostMaxBodyBytes,
	// which only ever bounds a SINGLE page's own response body, and
	// maxToolsListTools, which counts tools rather than wire bytes: an
	// upstream could stay under the ceiling on every individual page — and
	// even under maxToolsListTools, by repeating few tools with very large
	// schemas — while still driving ListTools to accumulate an unbounded
	// number of response bytes in memory across enough pages before any
	// other guard would ever trip.
	maxToolsListTotalBytes = 32 << 20 // 32 MiB
)

// errToolsListTooManyPages, errToolsListTooManyTools, errToolsListCursorTooLong,
// and errToolsListTooManyBytes each guard one of the const bounds above; see
// that block's own doc for what each protects against and why. All four
// carry only counts or lengths in their wrapping fmt.Errorf calls — never
// the cursor value itself, which is upstream-controlled and therefore
// subject to the same zero-knowledge-logging rule as any other upstream
// response content.
var (
	errToolsListTooManyPages  = errors.New("mcp: tools/list exceeded the maximum number of pages")
	errToolsListTooManyTools  = errors.New("mcp: tools/list exceeded the maximum number of tools")
	errToolsListCursorTooLong = errors.New("mcp: tools/list nextCursor exceeds the maximum length")
	errToolsListTooManyBytes  = errors.New("mcp: tools/list exceeded the maximum aggregate response bytes")
)

// errToolsListRepeatedCursor is returned when an upstream hands back a
// nextCursor ListTools has already used to request an earlier page in this
// same fetch — a cycle that would otherwise make ListTools loop forever
// re-requesting the same page.
var errToolsListRepeatedCursor = errors.New("mcp: tools/list nextCursor repeated an earlier page's cursor")

// errToolsListNilBodyMidFetch is returned when a page AFTER the first
// carries a nil response body (a *CallResult with Body == nil, signaling an
// HTTP 202 Accepted with no payload — see CallResult's own doc). The FIRST
// page alone keeps the pre-pagination behavior of ending the fetch right
// there with whatever was read as an empty listing, since a bodyless first
// response cannot possibly have started a multi-page fetch to begin with. A
// nil body on any LATER page means pages were already accumulated and must
// not be silently truncated to whatever was read so far — this method's own
// no-partial-listing contract (see ListTools' own doc) requires failing the
// whole fetch instead.
var errToolsListNilBodyMidFetch = errors.New("mcp: tools/list follow-up page returned no response body")

// errToolsListDuplicateTool is returned when two pages of the same fetch —
// whether adjacent or not — name the same tool. Without this check, the
// duplicate would reach FilterHeaderParamTools' HeaderParams map (silently
// overwriting the first occurrence's x-mcp-header bindings) and then
// db.UpsertServerTools' UNIQUE(server_id, name) constraint, whose failure
// tool_cache.go's persistListing already treats as fire-and-forget and
// swallows — by the time either of those runs it is too late to reject the
// fetch, so the check belongs here, before either is ever reached.
var errToolsListDuplicateTool = errors.New("mcp: tools/list returned the same tool name on more than one page")

// errToolsListPreScanFailed is returned by ListTools when
// countToolsListPageTools' pre-decode tool-count walk over a page's body
// fails for any reason — see that function's own doc for its fail-closed
// contract and the exact class of bypass (a page shaped so an EARLIER,
// unrelated key's value trips the walk's own bounded-depth guard while the
// real decode's own, more permissive depth tolerance would have sailed
// past it and gone on to fully unmarshal a later, enormous "tools" array)
// this guards against. ListTools fails the whole fetch on this error
// WITHOUT ever reaching jsonx.Unmarshal for the page in question — the one
// call this pre-decode walk exists to keep from ever running unbounded. A
// bare, static sentinel: the underlying walk error is upstream-controlled
// content (or, for errJSONSkipMaxDepthExceeded specifically, the mere fact
// that some upstream-controlled value nested unusually deep) this package's
// zero-knowledge-logging rule keeps out of error text everywhere else in
// this file, and this guard is no exception.
var errToolsListPreScanFailed = errors.New("mcp: tools/list pre-decode tool-count scan failed")

// cloudMetadataIP is the well-known link-local address used by cloud provider
// instance metadata services (AWS, GCP, Azure, DigitalOcean, etc.).
var cloudMetadataIP = net.ParseIP("169.254.169.254")

// ssrfDialTimeout, ssrfTLSHandshakeTimeout, and ssrfResponseHeaderTimeout
// bound only the connection-establishment phase of a request made through
// NewSSRFSafeTransport's transport: TCP dial, TLS handshake, and the wait for
// the upstream's response headers to start arriving. None of them bound the
// response BODY read — that is deliberate. HTTPTransport's buffered client
// additionally sets http.Client.Timeout (the stricter, total-duration bound
// for Call/ListTools/Warmup); its streamClient sets Timeout: 0 specifically
// because Forward's long-lived responses (in particular subscriptions/listen,
// docs/mcp-v2.md §3.4) must never be severed by elapsed wall-clock time.
// Before these were added, an upstream that accepted the TCP connection, read
// the request, and then simply never answered would block Forward
// indefinitely: streamIdleTimeout only starts once Do returns, and
// http.Client.Timeout on the buffered client only fires this late anyway
// with Timeout large enough to also cover slow-but-healthy bodies (see
// docs/mcp-v2.md, review finding B2). ResponseHeaderTimeout closes that gap
// for both clients without touching the body-read phase either one relies on.
const (
	ssrfDialTimeout           = 10 * time.Second
	ssrfTLSHandshakeTimeout   = 10 * time.Second
	ssrfResponseHeaderTimeout = 30 * time.Second
)

// NewSSRFSafeTransport returns an http.Transport that, when allowPrivate is
// false, refuses TCP connections to loopback, private, link-local, and cloud
// metadata addresses at dial time. This defends against DNS rebinding attacks:
// even if a hostname resolved to a public IP at registration time, a malicious
// DNS update cannot redirect traffic to an internal address at call time.
// When allowPrivate is true the transport is unrestricted (for self-hosted
// vLLM deployments on private networks).
//
// The returned transport also bounds the dial, TLS handshake, and
// response-header wait phases — see ssrfDialTimeout's doc for why these
// three, and only these three, are set here rather than on the body-read
// phase.
func NewSSRFSafeTransport(allowPrivate bool) *http.Transport {
	dialer := &net.Dialer{Timeout: ssrfDialTimeout}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				// address is already a bare host when no port is present; use as-is.
				host = address
			}
			ip := net.ParseIP(host)
			if ip == nil {
				// Not an IP address — DNS was already resolved by the dialer;
				// if we reach here with a hostname it is safe to pass through.
				return nil
			}
			if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				return fmt.Errorf("connection to internal address blocked: %s", host)
			}
			if ip.Equal(cloudMetadataIP) {
				return fmt.Errorf("connection to cloud metadata service blocked: %s", host)
			}
			return nil
		}
	}
	return &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   ssrfTLSHandshakeTimeout,
		ResponseHeaderTimeout: ssrfResponseHeaderTimeout,
	}
}

// eraBinding pairs a resolved ClientDialect with one UpstreamState per
// SessionScope that has called through it. Every Call captures one
// *eraBinding at the start and operates only on that local value from then
// on — never re-reading HTTPTransport.resolution — so a concurrent
// invalidateBinding (which only ever compare-and-swaps the published
// bindingResolution for the NEXT resolveBinding call, see its doc) can never
// mutate state a call already holds.
//
// postURL is the POST target this binding resolved to: the base endpoint
// for a modern-era binding, or empty — meaning "use the base endpoint" — for
// a legacy-era binding, since a legacy session never redirects POST traffic
// anywhere else. It is set exactly once, at binding construction inside
// resolveBinding, and never mutated afterward. Like dialect, it is a
// property of the resolved binding, not of the transport: every Call that
// captured this *eraBinding sees a postURL that cannot change out from under
// it, so there is nothing left for a concurrent invalidateBinding to race
// with — discarding the binding (see invalidateBinding) discards postURL
// along with it, automatically, without needing separate treatment.
//
// scopes holds one *scopedState for every distinct SessionScope this binding
// is currently remembering (see SessionScope's doc for why legacy session
// state must never cross that boundary), bounded at maxScopesPerBinding and
// LRU-evicted via scopesLRU (front = most recently used) once a new scope
// would exceed it.
//
// This was previously left unbounded, on the reasoning that the
// global-server-shared-by-many-orgs case grows this map to at most one entry
// per distinct org that has ever called through this server — bounded by
// org count, not by request volume — so no eviction was needed. That
// reasoning assumed a fixed set of organizations; this repo has no such
// fixed set. Organizations are created through the ordinary Admin API for
// the entire lifetime of the process, and every one of them that ever calls
// CallMCPTool against this server mints its own entry here (see stateFor)
// that nothing then removed — an unbounded map growing for the life of the
// process regardless of how many of those organizations are still active.
// Bounding and LRU-evicting it here mirrors the same graceful degradation
// SessionRegistry's own maxOrgsPerServer/maxKeysPerOrg already provide one
// layer up (see that type's doc): an evicted scope simply has its
// remembered legacy session forgotten, and the next Call made in it re-runs
// Warmup as if it were the first ever made in this scope.
type eraBinding struct {
	dialect ClientDialect
	postURL string

	scopesMu  sync.Mutex
	scopesLRU *list.List // *scopedState values; front = most recently used scope
	scopes    map[SessionScope]*list.Element
}

// maxScopesPerBinding bounds how many distinct SessionScope entries a single
// eraBinding retains a *scopedState for, evicting the least-recently-used
// scope first (via eraBinding.scopesLRU) once a new one would exceed it.
// eraBinding.scopes is keyed by organization alone — Call always passes
// mcp.SessionScope(ki.OrgID), see CallMCPTool — never by (organization, API
// key) the way SessionRegistry's scopes are, so this single flat bound is
// already the org-count equivalent of that registry's maxOrgsPerServer, with
// no second, per-key axis needed: every key belonging to one organization
// shares that organization's single scope here, so no one key can inflate
// this map on its own. 4096 comfortably covers every organization that has
// ever called CallMCPTool against a single global server, for any
// deployment sized within this project's single-instance-without-Redis
// constraint, while keeping each binding's worst case a small, fixed amount
// of memory (each entry is a few bytes of legacy session state) regardless
// of how many organizations have existed over the process' lifetime — see
// eraBinding.scopes' own doc for why that property, not org count alone, is
// what needed bounding here.
const maxScopesPerBinding = 4096

// scopedState is one eraBinding's state for a single SessionScope: the
// UpstreamState (holding the legacy session, if any) established for that
// scope, plus whether Warmup has already run for it. It is populated and
// read only by Call — Forward, the transparent legacy proxy path
// (HandleMCPProxy), owns no session of its own and never touches
// eraBinding.scopes or scopedState at all; the caller-supplied-session
// tracking that once lived here has moved to SessionRegistry, which is
// independent of any one eraBinding's lifetime (see that type's doc for
// why).
//
// scope is this entry's own key, recorded so stateFor can find and remove
// it from eraBinding.scopes again once maxScopesPerBinding evicts it.
type scopedState struct {
	scope SessionScope
	state *UpstreamState

	mu     sync.Mutex
	warmed bool
}

// stateFor returns the scopedState for scope, creating it on first use —
// evicting the least-recently-used scope first (via eraBinding.scopesLRU) if
// this creation would exceed maxScopesPerBinding — and moving it to the
// front of scopesLRU either way. Creation only allocates zero-value state —
// it performs no I/O — so it is safe to hold scopesMu for the whole
// operation; the (potentially slow) Warmup call happens later, under the
// returned scopedState's own mu, once scopesMu has already been released.
func (b *eraBinding) stateFor(scope SessionScope) *scopedState {
	b.scopesMu.Lock()
	defer b.scopesMu.Unlock()
	if b.scopes == nil {
		b.scopes = make(map[SessionScope]*list.Element)
		b.scopesLRU = list.New()
	}
	if el, ok := b.scopes[scope]; ok {
		b.scopesLRU.MoveToFront(el)
		return el.Value.(*scopedState)
	}
	if len(b.scopes) >= maxScopesPerBinding {
		if oldest := b.scopesLRU.Back(); oldest != nil {
			b.scopesLRU.Remove(oldest)
			delete(b.scopes, oldest.Value.(*scopedState).scope)
		}
	}
	ss := &scopedState{scope: scope, state: &UpstreamState{}}
	b.scopes[scope] = b.scopesLRU.PushFront(ss)
	return ss
}

// bindingResolution is the immutable, atomically published outcome of
// resolving which protocol era an upstream speaks: either a resolved
// *eraBinding, or a cached probe failure valid until errUntil elapses.
// Publishing binding, err, and errUntil together as a single value — rather
// than as three independently-guarded fields, the previous design — is what
// lets resolveBinding's fast path read the current outcome with one
// lock-free Load instead of an exclusive Mutex: there is no window in which
// a reader could observe, say, a freshly resolved binding paired with a
// stale errUntil left over from the failure it just superseded, because the
// two are never updated independently of one another.
type bindingResolution struct {
	binding *eraBinding
	err     error
	// errUntil is the UnixNano deadline until which err is still considered
	// a valid cached failure (see bindingProbeErrorTTL). Zero when binding
	// is non-nil: a resolved binding carries no cached failure to expire.
	errUntil int64
}

// snapshot reports whether res already answers a resolveBinding call without
// a fresh probe. A resolved binding always does. A cached failure does only
// until errUntil elapses; once it has, ok is false and resolveBinding must
// probe the upstream again.
func (res *bindingResolution) snapshot() (b *eraBinding, err error, ok bool) {
	if res.binding != nil {
		return res.binding, nil, true
	}
	if time.Now().UnixNano() < res.errUntil {
		return nil, res.err, true
	}
	return nil, nil, false
}

// HTTPTransport proxies JSON-RPC requests to a remote MCP server over HTTP.
// It is not safe to use concurrently with Close.
type HTTPTransport struct {
	endpoint   string
	authType   string // "none", "bearer", "header", or "oauth"
	authHeader string // header name used when authType is "header"
	authToken  string // decrypted token value
	client     *http.Client

	// streamClient is used exclusively by Forward — the transparent streaming
	// proxy path. It shares client's SSRF-hardened *http.Transport (same
	// NewSSRFSafeTransport instance, same CheckRedirect prohibition) but sets
	// Timeout: 0: http.Client.Timeout bounds the ENTIRE exchange including the
	// body read, and would silently sever any stream — in particular a
	// long-lived subscriptions/listen response (docs/mcp-v2.md §3.4) — after
	// that duration regardless of how much healthy traffic is still flowing.
	// streamIdleTimeout (below) is what actually bounds a stream on this
	// path, and it does so as an idle timeout, not a total-duration one.
	streamClient *http.Client
	// streamIdleTimeout is the idle timeout Forward applies to the streaming
	// response body via idleTimeoutReader: the request's context is cancelled
	// if this much time passes with no bytes read from the upstream. See
	// streamClient's doc for why this replaces http.Client.Timeout on this
	// path, and config.MCPConfig.StreamIdleTimeout for the operator-facing
	// setting this is populated from.
	streamIdleTimeout time.Duration

	// OAuth fields — populated only when authType is "oauth".
	serverID     string             // stable server ID used as OAuthTokenManager cache key
	oauthManager *OAuthTokenManager // shared manager; nil for non-OAuth transports
	oauthConfig  *OAuthConfig       // OAuth Client Credentials Flow configuration

	// clientInfo self-identifies VoidLLM to the upstream server in both eras:
	// the legacy initialize handshake's clientInfo field and the modern
	// era's params._meta["io.modelcontextprotocol/clientInfo"].
	clientInfo ClientInfo
	// pinnedVersion overrides era auto-detection when non-empty (from
	// mcp_servers.protocol_version via ResolvePinnedVersion); "" means probe
	// via probeEra.
	pinnedVersion Version

	// resolution is the atomically published outcome of resolving which
	// protocol era this upstream speaks — a property of the SERVER, not of
	// any one request (docs/mcp-v2.md §4.7). The fast path — every call once
	// the era is known — is a single lock-free Load; no mutex is taken on
	// this path at all. The spec expects many requests in flight
	// concurrently against the same upstream (docs/mcp-v2.md §1a); a mutex
	// taken on every call just to read an already-resolved, immutable
	// pointer would serialize exactly the traffic §1a says must not be
	// serialized.
	resolution atomic.Pointer[bindingResolution]
	// resolveMu serializes only the SLOW path: the very first resolution
	// attempt, a re-probe once a cached failure's bindingProbeErrorTTL has
	// elapsed, and a re-probe after invalidateBinding. It is never held
	// while a request is in flight against an already-resolved binding.
	resolveMu sync.Mutex
}

// bindingProbeErrorTTL bounds how long resolveBinding treats a cached probe
// failure as still valid before retrying. Caching it forever — the previous
// behavior — meant a probe failure on the very FIRST call through a fresh
// HTTPTransport (a transient DNS blip, an upstream still finishing its own
// boot sequence and answering one 500) wedged a freshly registered server
// unusable until process restart or a config change: invalidateBinding only
// ever runs from reinitAndRetry, which is never reached when resolution
// itself never succeeded in the first place (docs/mcp-v2.md, FIX 5).
//
// Not caching the failure at all (a zero TTL, re-probing on every call)
// would also fix that, but turns a genuinely, durably dead upstream into a
// source of load: every proxied request and every ListTools/CallMCPTool call
// would re-run the full probeEra round trip — a server/discover POST and,
// for anything that does not answer cleanly as modern, a legacy initialize
// fallback — against a server that has already proven unreachable. A short
// TTL gets both properties at once: within the window, concurrent and
// back-to-back calls reuse the same cached failure instead of hammering a
// server that just failed; once it elapses, the very next call gets a fresh
// attempt, so a transient outage self-heals with no operator action needed.
// Five seconds is short enough that a real recovery is noticed well within
// any reasonable request cadence or external health-check interval, and long
// enough that a genuinely dead upstream is not re-probed on every request
// during a burst.
const bindingProbeErrorTTL = 5 * time.Second

// NewHTTPTransport creates a transport for the given endpoint with the
// supplied authentication configuration and per-call timeout.
// authType must be one of "none", "bearer", "header", or "oauth".
// When authType is "bearer", authToken is sent as a Bearer token.
// When authType is "header", authToken is sent under the authHeader header name.
// When authType is "oauth", serverID and oauthMgr and oauthCfg must be non-nil;
// the manager fetches and caches access tokens via the Client Credentials Flow.
// When allowPrivate is false, the underlying TCP dialer refuses connections to
// loopback, private-range, link-local, and cloud metadata addresses, preventing
// DNS rebinding SSRF attacks even after the URL has been registered.
// Pass empty serverID, nil oauthMgr, and nil oauthCfg for non-OAuth servers.
// clientInfo self-identifies VoidLLM to the upstream in both protocol eras.
// pinnedVersion, when non-empty, skips era probing entirely (see
// ResolvePinnedVersion); pass "" to auto-detect via probeEra.
// timeout bounds Call, probeEra, and Warmup (the buffered paths); it never
// applies to Forward — see streamIdleTimeout's doc. streamIdleTimeout bounds
// Forward's streaming path as an idle timeout instead.
func NewHTTPTransport(endpoint, authType, authHeader, authToken string,
	timeout time.Duration, allowPrivate bool,
	serverID string, oauthMgr *OAuthTokenManager, oauthCfg *OAuthConfig,
	clientInfo ClientInfo, pinnedVersion Version, streamIdleTimeout time.Duration) *HTTPTransport {
	t := NewSSRFSafeTransport(allowPrivate)
	// Never follow redirects on either client — the remote MCP server should
	// not redirect POST requests and doing so could silently drop the
	// request body.
	noRedirect := func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &HTTPTransport{
		endpoint:      endpoint,
		authType:      authType,
		authHeader:    authHeader,
		authToken:     authToken,
		serverID:      serverID,
		oauthManager:  oauthMgr,
		oauthConfig:   oauthCfg,
		clientInfo:    clientInfo,
		pinnedVersion: pinnedVersion,
		client: &http.Client{
			Timeout:       timeout,
			Transport:     t,
			CheckRedirect: noRedirect,
		},
		streamClient: &http.Client{
			// No Timeout — see the streamClient field doc.
			Transport:     t,
			CheckRedirect: noRedirect,
		},
		streamIdleTimeout: streamIdleTimeout,
	}
}

// httpResult is the outcome of a single raw HTTP POST to the upstream
// server: the response body (already SSE-unwrapped and size-limited), the
// response headers, and the HTTP status code. It carries no interpretation
// of the JSON-RPC payload — that is doCall's and probeEra's job.
type httpResult struct {
	status int
	body   []byte
	header http.Header
}

// protectedAuthHeaderName returns the exact header name this transport's own
// authentication just set on a request — "Authorization" for authType
// "bearer" and "oauth" (both send a bearer token that way), t.authHeader for
// authType "header", or "" for authType "none" or an authHeader-less
// "header" config (neither sets a credential-carrying header at all, so
// there is nothing to protect). This is the one name applyExtraHeaders must
// never let an hdr entry overwrite, on both rawPost and Forward: whichever
// of those two functions built hdr, and for whatever reason, an entry named
// exactly this — case-insensitively, since HTTP header names are
// case-insensitive — is the request's own outbound credential, not a value
// either function's hdr parameter is ever entitled to replace.
func (t *HTTPTransport) protectedAuthHeaderName() string {
	switch t.authType {
	case "bearer", "oauth":
		return "Authorization"
	case "header":
		return t.authHeader
	default:
		return ""
	}
}

// applyExtraHeaders merges hdr onto req, skipping any entry whose name
// matches protect case-insensitively. It is the single place both rawPost
// and Forward apply hdr to the outbound request, specifically so the
// protection against overwriting this transport's own credential lives in
// exactly one place rather than being re-derived, possibly inconsistently,
// by every function that happens to build an hdr map (docs/mcp-v2.md,
// review finding C4). protect is normally the result of
// protectedAuthHeaderName, called immediately before this at each call
// site — see that method's own doc for what it protects and why. protect ==
// "" matches nothing, so every entry in hdr is applied unconditionally when
// this transport sets no credential-carrying header to begin with.
func applyExtraHeaders(req *http.Request, hdr MapHeader, protect string) {
	for k, v := range hdr {
		if protect != "" && strings.EqualFold(k, protect) {
			continue
		}
		req.Header.Set(k, v)
	}
}

// rawPost sends raw as a JSON-RPC POST to target with hdr merged in as extra
// request headers on top of Content-Type, Accept, and whatever
// authentication this transport is configured for. target is supplied by
// the caller rather than read from transport state: probeEra and
// probeLegacy pass t.endpoint directly, since no binding exists yet at probe
// time, while doCall and the RoundTripper Warmup depends on pass the
// already-resolved eraBinding's postURL (see postTarget). rawPost performs
// no interpretation of the response body's JSON-RPC shape, but does unwrap a
// text/event-stream response down to the one event that answers raw's own
// JSON-RPC id (see extractSSEResult), since every caller needs the same JSON
// payload regardless of which content type the upstream chose to answer
// with.
func (t *HTTPTransport) rawPost(ctx context.Context, target string, raw []byte, hdr MapHeader) (*httpResult, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	// Authentication is applied BEFORE hdr below — so that hdr, which may
	// carry MCP standard request headers this transport does not interpret,
	// can still win an ordinary name collision — but the merge itself
	// (applyExtraHeaders) refuses to let any entry in hdr overwrite whichever
	// header this switch just set to carry the request's own credential (see
	// protectedAuthHeaderName). That protection is no longer merely defense
	// in depth on this path: hdr here is whatever the resolved ClientDialect's
	// Prepare produced (see roundTripper/doCall), and dialect2026Client.Prepare
	// mirrors x-mcp-header annotations from the UPSTREAM's own tool schema
	// onto exactly this map (docs/mcp-v2.md §4.3) — so an upstream schema
	// that happens to annotate a property "Mcp-Param-Token" (or any other
	// name colliding with authType "header"'s configured auth_header) could
	// otherwise overwrite VoidLLM's own outbound credential with a
	// tool-argument value. Server registration rejects a NEW auth_header
	// equal to any reserved MCP header name, this prefix included
	// (internal/api/admin's isReservedMCPHeader), but a server registered
	// before that validation existed could still have one configured — so
	// this guard, not registration-time validation, is what must hold
	// (docs/mcp-v2.md, review finding C4).
	switch t.authType {
	case "bearer":
		if t.authToken == "" {
			return nil, errEmptyAuthCredential
		}
		req.Header.Set("Authorization", "Bearer "+t.authToken)
	case "header":
		if t.authHeader == "" || t.authToken == "" {
			return nil, errEmptyAuthCredential
		}
		req.Header.Set(t.authHeader, t.authToken)
	case "oauth":
		if t.oauthManager == nil || t.oauthConfig == nil {
			return nil, errOAuthNotConfigured
		}
		oauthToken, oauthErr := t.oauthManager.GetToken(ctx, t.serverID, *t.oauthConfig)
		if oauthErr != nil {
			return nil, fmt.Errorf("oauth token: %w", oauthErr)
		}
		req.Header.Set("Authorization", "Bearer "+oauthToken)
	}

	applyExtraHeaders(req, hdr, t.protectedAuthHeaderName())

	resp, err := t.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()

	// The limit reader is deliberately given rawPostMaxBodyBytes+1, one byte
	// more than the ceiling this function actually enforces — see
	// legacySSEWrapReadCap's identical +1 trick (internal/api/admin/mcp_proxy.go)
	// for the full reasoning: reading exactly rawPostMaxBodyBytes would make a
	// response that fits EXACTLY at the limit indistinguishable from one that
	// was truncated at it — both come back as a len(body) == rawPostMaxBodyBytes
	// read with no error. The extra byte lets the length check below tell the
	// two apart: len == rawPostMaxBodyBytes+1 only happens when the upstream
	// body was actually longer than the limit. Before this fix, an oversized
	// body was silently truncated at exactly 10 MiB and the truncated bytes
	// handed to the JSON-RPC decoder as if they were the complete response,
	// rather than rejected outright.
	body, err := io.ReadAll(io.LimitReader(resp.Body, rawPostMaxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if int64(len(body)) > rawPostMaxBodyBytes {
		return nil, fmt.Errorf("response body exceeds %d byte limit", rawPostMaxBodyBytes)
	}

	// len(body) > 0 guards against a response that labels itself
	// text/event-stream but carries no body at all — most notably an HTTP 202
	// Accepted acknowledging a notification (MCP Streamable HTTP §4.1: "202
	// Accepted ohne Body"), which some upstream could still send with a
	// stray/copy-pasted Content-Type header despite there being nothing to
	// parse. Without this guard extractSSEResult would see an empty body,
	// find no event of any kind, and fail with errSSENoResultEvent for a
	// response doCall's own status switch would otherwise have handled
	// harmlessly (StatusAccepted never inspects the body at all). An empty
	// body is left as empty either way — doCall's separate
	// len(res.body) == 0 check further down handles it identically for every
	// era regardless of Content-Type.
	if len(body) > 0 && isSSEContentType(resp.Header.Get("Content-Type")) {
		matched, sseErr := extractSSEResult(body, outboundRequestID(raw))
		if sseErr != nil {
			return nil, fmt.Errorf("sse response: %w", sseErr)
		}
		body = matched
	}

	return &httpResult{status: resp.StatusCode, body: body, header: resp.Header}, nil
}

// rawPostMaxBodyBytes bounds how much of an upstream response body rawPost
// will read, to prevent OOM on a misbehaving upstream server. It is enforced
// as a hard reject, not a silent truncation — see the +1-byte LimitReader
// trick at rawPost's own read site.
const rawPostMaxBodyBytes int64 = 10 << 20 // 10 MiB

// isSSEContentType reports whether ct — a response's raw Content-Type header
// value — names the "text/event-stream" media type, ignoring any parameters
// (e.g. "; charset=utf-8") and case. rawPost previously decided this with a
// plain strings.HasPrefix(ct, "text/event-stream") check, which happens to
// tolerate a trailing "; charset=..." parameter by accident — HasPrefix stops
// comparing at the end of the literal regardless of what follows — but is
// not robust to anything RFC 9110's media-type grammar itself allows and a
// prefix comparison cannot: a different case ("Text/Event-Stream", which a
// prefix comparison would reject as not-SSE and hand to the plain-JSON
// decode path instead, corrupting a genuine SSE response), or leading
// whitespace inside a value net/http did not already trim. mime.ParseMediaType
// parses the value per that grammar and returns the bare type in lowercase, so
// this compares it exactly rather than by prefix. A ct that fails to parse at
// all — a malformed header — is reported as not SSE: the previous prefix
// check would already have rejected most malformed values (they rarely start
// with the literal "text/event-stream" by accident), so this preserves that
// same safe default rather than guessing.
func isSSEContentType(ct string) bool {
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "text/event-stream")
}

// maxSSEEventsSkipped bounds how many SSE events extractSSEResult examines
// and discards — because they carry no JSON-RPC id at all (a notification,
// MCP 2026-07-28 §3.4) or an id that does not match the request this
// response belongs to — before giving up on this response ever carrying a
// matching one, at which point it fails with errSSETooManyEvents rather than
// scanning indefinitely.
//
// rawPostMaxBodyBytes already bounds the SAME response by total wire bytes
// (Fund 7), and for events of realistic size that bound is the tighter of
// the two in practice: a genuine notifications/progress event runs to tens
// or a few hundred bytes, so rawPostMaxBodyBytes alone already caps the
// count reachable within one response to a five-digit number at most. This
// separate, size-independent cap exists for the pathological case
// rawPostMaxBodyBytes cannot catch on its own: a flood of minimal-size
// events — a bare `data:{}` costs under ten bytes on the wire — could
// otherwise reach into the hundreds of thousands of iterations before
// rawPostMaxBodyBytes ever triggers. In that scenario this cap, not
// rawPostMaxBodyBytes, is the one that actually stops the scan.
const maxSSEEventsSkipped = 1000

// errSSENoResultEvent is returned by extractSSEResult when the response body
// contains no SSE event this function can treat as the JSON-RPC response to
// the outbound request — every event either carried no "id" field
// (notifications/progress, notifications/message, or any other JSON-RPC
// notification, MCP 2026-07-28 §3.4) or one that did not match the outbound
// request's own id, and the body was exhausted before a match was found.
// Never returned silently as an empty result — see this package's own review
// finding (docs/mcp-v2.md §11.3 Befund 3) for why a caller reading an empty
// []Tool with a nil error, the previous behavior in this exact case, is the
// failure mode this sentinel exists to replace with a loud one.
var errSSENoResultEvent = errors.New("sse response contained no event matching the request")

// errSSETooManyEvents is returned by extractSSEResult once
// maxSSEEventsSkipped is exceeded — see that constant's own doc for why this
// bound exists independently of rawPostMaxBodyBytes.
var errSSETooManyEvents = errors.New("sse response exceeded the maximum number of events examined")

// outboundRequestID extracts the top-level JSON-RPC "id" field from raw — a
// single already-serialized outbound request, exactly as every caller of
// rawPost builds it (Streamable HTTP §4.1: "Body ist genau ein Request oder
// eine Notification") — so extractSSEResult can recognize which SSE event on
// a per-request response stream actually answers THIS request, as opposed to
// a notifications/progress or notifications/message event the same stream
// may carry ahead of it (MCP 2026-07-28 §3.4). A genuine JSON-RPC
// notification never reaches this code path with a body to parse at all: it
// is acknowledged with HTTP 202 and no body (see doCall's status switch), so
// raw always names an id whenever this function's return value actually
// matters.
//
// Returns nil — not an error — when raw does not parse as a JSON-RPC request
// or carries no id at all: extractSSEResult's fallback for a nil wantID is
// to accept the first event carrying ANY id, rather than failing the whole
// call over an id VoidLLM itself failed to parse back out of bytes it just
// built.
func outboundRequestID(raw []byte) jsonx.RawMessage {
	var req Request
	if err := jsonx.Unmarshal(raw, &req); err != nil {
		return nil
	}
	if req.IsNotification() {
		return nil
	}
	return bytes.TrimSpace(req.ID)
}

// extractSSEResult parses body as an SSE event stream and returns the raw
// JSON-RPC message carried by the one event that answers wantID, or an error
// if none does.
//
// Event framing follows the WHATWG "Server-Sent Events" interpretation
// algorithm (https://html.spec.whatwg.org/multipage/server-sent-events.html#event-stream-interpretation,
// referenced but not restated by docs/mcp-v2.md, which assumes the
// underlying SSE framing): events are separated by a blank line; a single
// event's "data:" field may itself be split across several consecutive
// "data:" lines, which are reassembled by joining them with "\n" in the
// order they appeared; a line beginning with ":" is a comment and is
// skipped; every other field ("event:", "id:", "retry:", or anything else)
// is recognized only enough to be ignored, never acted on — this package's
// use of SSE is confined to the JSON-RPC payload inside "data:", nothing
// else in the framing carries information VoidLLM interprets. \r\n and bare
// \r line endings are normalized to \n before splitting, tolerating an
// upstream that does not use bare LF.
//
// For each reassembled event, the joined data is treated as a candidate
// JSON-RPC message and its own top-level "id" field is inspected:
//
//   - No "id" field at all (or an explicit JSON null) means the event is a
//     JSON-RPC notification (MCP 2026-07-28 §3.4: notifications/progress and
//     notifications/message may precede a request's own result on its
//     response stream) — never a candidate, always skipped.
//   - wantID non-empty and the event's id does not match it, byte-for-byte
//     after trimming whitespace: not the response to THIS request — skipped.
//     wantID is always VoidLLM's own previously-marshaled id (see
//     outboundRequestID), so a plain byte comparison is exact; no numeric or
//     string-vs-number normalization is needed.
//   - wantID empty (outboundRequestID could not determine it) and the event
//     carries any id at all: accepted. This is the most defensible fallback
//     available without restructuring every rawPost caller to thread a
//     request id through independently of raw — see outboundRequestID's own
//     doc for why wantID is expected to be non-empty on every real call
//     rawPost ever makes, making this branch a safety net rather than the
//     common case.
//   - The event's data does not even parse as a JSON object with a
//     "id"-shaped top level: treated as "not a match" rather than a fatal
//     parse error — an unparsable event is exactly as unusable as one that
//     is a known notification shape, and rejecting the whole call over one
//     upstream field VoidLLM was never going to read anyway would be more
//     fragile than simply continuing to look for the real result.
//
// Every event examined and rejected counts against maxSSEEventsSkipped;
// exceeding it fails with errSSETooManyEvents instead of scanning
// indefinitely (see that constant's own doc for how it relates to
// rawPostMaxBodyBytes, which already bounds this same body by total wire
// bytes). Reaching the end of body with no event ever accepted fails with
// errSSENoResultEvent — this function never falls back to returning body,
// or any part of it, unexamined: MCP 2026-07-28 §11.3 Befund 3 (see
// docs/mcp-v2.md) is explicit that a silent, "leer, nicht fehlerhaft"
// (empty, not erroring) result is the failure mode this replaces.
func extractSSEResult(body []byte, wantID jsonx.RawMessage) ([]byte, error) {
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	normalized = bytes.ReplaceAll(normalized, []byte("\r"), []byte("\n"))
	lines := bytes.Split(normalized, []byte("\n"))

	var dataLines [][]byte
	skipped := 0

	dispatch := func() ([]byte, bool, error) {
		if len(dataLines) == 0 {
			return nil, false, nil
		}
		data := bytes.Join(dataLines, []byte("\n"))
		dataLines = dataLines[:0]

		if sseEventMatches(data, wantID) {
			return data, true, nil
		}
		skipped++
		if skipped > maxSSEEventsSkipped {
			return nil, false, fmt.Errorf("%w: examined more than %d events", errSSETooManyEvents, maxSSEEventsSkipped)
		}
		return nil, false, nil
	}

	for _, line := range lines {
		switch {
		case len(line) == 0:
			data, matched, err := dispatch()
			if err != nil {
				return nil, err
			}
			if matched {
				return data, nil
			}
		case bytes.HasPrefix(line, []byte(":")):
			// Comment line — ignored.
		case bytes.HasPrefix(line, []byte("data: ")):
			dataLines = append(dataLines, line[len("data: "):])
		case bytes.HasPrefix(line, []byte("data:")):
			dataLines = append(dataLines, line[len("data:"):])
		default:
			// Some other SSE field ("event:", "id:", "retry:", or an
			// unrecognized one) — ignored, see the function's own doc.
		}
	}
	// A final event with no terminating blank line at end-of-body still gets
	// dispatched here — every fixture in this package's own tests happens to
	// end with a blank line, but a well-formed upstream that omits the
	// trailing one must not have its final (and, in the single-event case,
	// only) event silently dropped.
	data, matched, err := dispatch()
	if err != nil {
		return nil, err
	}
	if matched {
		return data, nil
	}

	return nil, errSSENoResultEvent
}

// sseEventMatches reports whether data — one SSE event's already-joined
// "data:" payload — is the JSON-RPC response wantID names. See
// extractSSEResult's own doc for the full matching rule this implements.
func sseEventMatches(data []byte, wantID jsonx.RawMessage) bool {
	var probe struct {
		ID jsonx.RawMessage `json:"id"`
	}
	if err := jsonx.Unmarshal(data, &probe); err != nil {
		return false
	}
	id := bytes.TrimSpace(probe.ID)
	if len(id) == 0 || string(id) == "null" {
		return false
	}
	if len(wantID) == 0 {
		return true
	}
	return bytes.Equal(id, wantID)
}

// httpHeaderAdapter adapts net/http's Header to the mcp.Header interface so
// ClientDialect.Warmup implementations can read response headers (e.g.
// Mcp-Session-Id) without depending on net/http directly.
type httpHeaderAdapter http.Header

// Get implements Header.
func (h httpHeaderAdapter) Get(name string) string {
	return http.Header(h).Get(name)
}

// httpRoundTripper adapts HTTPTransport.rawPost to the narrow RoundTripper
// interface a ClientDialect's Warmup depends on, so legacyClientDialect can
// drive its own initialize handshake without importing HTTPTransport itself.
// target is fixed at construction (see roundTripper) to the POST target of
// the *eraBinding this handshake is warming up.
type httpRoundTripper struct {
	t      *HTTPTransport
	target string
}

// RoundTrip implements RoundTripper.
func (r httpRoundTripper) RoundTrip(ctx context.Context, raw []byte, hdr MapHeader) (*RoundTripResult, error) {
	res, err := r.t.rawPost(ctx, r.target, raw, hdr)
	if err != nil {
		return nil, err
	}
	return &RoundTripResult{Status: res.status, Body: res.body, Header: httpHeaderAdapter(res.header)}, nil
}

// roundTripper returns the RoundTripper b's dialect uses to drive its own
// handshake, posting to b's resolved POST target (see postTarget).
func (t *HTTPTransport) roundTripper(b *eraBinding) RoundTripper {
	return httpRoundTripper{t: t, target: t.postTarget(b)}
}

// postTarget returns the POST target for b: b.postURL when the binding
// resolved one (the modern era), or the transport's base endpoint otherwise
// (the legacy era, which never redirects POST traffic elsewhere).
func (t *HTTPTransport) postTarget(b *eraBinding) string {
	if b.postURL != "" {
		return b.postURL
	}
	return t.endpoint
}

// dialectFor returns the ClientDialect for version v, dispatching on its Era.
func (t *HTTPTransport) dialectFor(v Version) ClientDialect {
	if v.Era() == EraModern {
		return newDialect2026Client(v, t.clientInfo)
	}
	return newLegacyClientDialect(v, t.clientInfo)
}

// resolveBinding returns the ClientDialect/UpstreamState pair for this
// upstream, resolving it via pinnedVersion or probeEra on first use and
// caching the result for the lifetime of this HTTPTransport (docs/mcp-v2.md
// §4.7: era is a property of the server). A probe failure is cached too, but
// only for bindingProbeErrorTTL (see its doc for why not forever and not at
// all) — once that window elapses, the next caller gets a fresh probe
// attempt instead of reusing a possibly stale failure.
//
// A probe failure caused by ctx itself — context.Canceled or
// context.DeadlineExceeded — is the one kind of probeEra error NEVER
// published to the shared snapshot: it describes THIS caller's context, not
// the upstream's reachability, so it carries no information any other
// caller's resolveBinding should reuse. Caching it anyway would mean the
// first request against a fresh HTTPTransport happening to be cancelled or
// to time out (a slow client disconnecting, a short per-request deadline)
// wedges bindingProbeErrorTTL's worth of the SAME context error onto every
// other caller — including ones from other organizations sharing this
// upstream, with their own, perfectly healthy contexts — even though the
// upstream itself was never actually determined to be unreachable
// (docs/mcp-v2.md, FIX 2). Returning it straight to the caller without
// publishing it leaves the shared resolution exactly as it was, so the very
// next call — from this caller with a fresh context, or from any other
// caller — probes again instead of reusing a verdict that was never about
// the upstream in the first place. errors.Is is used rather than a string or
// sentinel-value comparison so this still recognizes both errors however
// deeply probeEra's own error wrapping nests them.
//
// The fast path — resolution already published and still valid — is a
// single atomic Load with no lock at all, so concurrent Calls against an
// already-resolved upstream never contend with one another (docs/mcp-v2.md
// §1a). resolveMu is taken only on the slow path: first resolution, a
// re-probe once a cached failure's TTL has elapsed, or a re-probe after
// invalidateBinding — each guarded by a double-check against a fresh Load,
// so a goroutine that lost the race to acquire resolveMu reuses whatever the
// winner just published instead of probing the upstream a second time.
func (t *HTTPTransport) resolveBinding(ctx context.Context) (*eraBinding, error) {
	if res := t.resolution.Load(); res != nil {
		if b, err, ok := res.snapshot(); ok {
			return b, err
		}
	}

	t.resolveMu.Lock()
	defer t.resolveMu.Unlock()

	if res := t.resolution.Load(); res != nil {
		if b, err, ok := res.snapshot(); ok {
			return b, err
		}
	}

	version := t.pinnedVersion
	if version == "" || !version.Valid() {
		v, err := t.probeEra(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				// Caller-specific, not upstream-specific — see this function's
				// doc. Leave the shared resolution untouched so the next call,
				// from any caller, gets a genuine probe attempt.
				return nil, err
			}
			t.resolution.Store(&bindingResolution{
				err:      err,
				errUntil: time.Now().Add(bindingProbeErrorTTL).UnixNano(),
			})
			return nil, err
		}
		version = v
	}

	b := &eraBinding{dialect: t.dialectFor(version)}
	if version.Era() == EraModern {
		// Modern requests are served directly by the base endpoint; no
		// separate discovery step changes the POST target.
		b.postURL = t.endpoint
	}

	t.resolution.Store(&bindingResolution{binding: b})
	return b, nil
}

// invalidateBinding discards the cached era binding so the next call to
// resolveBinding re-probes the upstream from scratch, but ONLY if the
// currently published resolution still points at expected — the exact
// *eraBinding the caller (reinitAndRetry) captured before its Warmup failed.
// This compare-and-swap is what keeps a late-returning call from clobbering
// a generation some other, concurrent caller has already re-resolved in the
// meantime: without it, a call still holding a stale binding could discard a
// binding that has already proven itself good, because of a failure that
// actually happened to an earlier one. It is used when re-establishing a
// legacy session fails outright, which suggests the cached era assumption no
// longer holds for this upstream — the era can change if, for example, the
// upstream is redeployed behind the same URL.
//
// It never affects a binding a Call already captured locally: eraBinding is
// immutable once constructed (see its doc), so a Call already in flight
// against it is unaffected regardless of what resolution publishes next.
func (t *HTTPTransport) invalidateBinding(expected *eraBinding) {
	for {
		cur := t.resolution.Load()
		if cur == nil || cur.binding != expected {
			// Already invalidated, or superseded by a newer generation this
			// call has no business discarding.
			return
		}
		if t.resolution.CompareAndSwap(cur, nil) {
			return
		}
		// Lost a race with a concurrent update between Load and
		// CompareAndSwap; reload and re-check rather than assume success.
	}
}

// ensureWarm runs b's dialect's Warmup exactly once for ss's lifetime,
// guarded by ss's own mutex so concurrent callers in the same SessionScope
// racing to use a freshly resolved binding block on the first Warmup rather
// than each running their own. Callers in a different SessionScope hold a
// different ss and never contend with this call at all.
func (t *HTTPTransport) ensureWarm(ctx context.Context, b *eraBinding, ss *scopedState) error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.warmed {
		return nil
	}
	if err := b.dialect.Warmup(ctx, t.roundTripper(b), ss.state); err != nil {
		return err
	}
	ss.warmed = true
	return nil
}

// CallResult is the outcome of a single successful Call: the upstream's
// response body, plus whatever CacheableResult freshness hint (MCP
// 2026-07-28 §5) the resolved era's Parse extracted from it. A non-nil
// *CallResult is Call's entire success contract — see Call's own doc for how
// to read a nil Body within it (HTTP 202 Accepted).
type CallResult struct {
	// Body is the raw response body bytes Call resolved for this request.
	Body []byte
	// Cache is the CacheableResult hint parsed from a modern-era response —
	// see CacheHint.TTLMsSet for how to distinguish an explicit hint from no
	// hint at all. It is always the zero value for a legacy-era response
	// (see legacyClientDialect.Parse's doc) and for a modern-era response
	// whose resultType was not "complete".
	Cache CacheHint
}

// Call sends req to the remote MCP server for whichever protocol era it was
// found to speak, and returns the response as a *CallResult. The era is
// resolved once per HTTPTransport (resolveBinding, cached) and its
// ClientDialect drives everything era-specific: the legacy initialize
// handshake and Mcp-Session-Id header live entirely inside
// legacyClientDialect and this transport's UpstreamState — nothing about a
// session is visible in this signature beyond scope, and nothing about a
// session is visible to any caller at all.
//
// scope isolates EraLegacy's Mcp-Session-Id between tenants sharing this
// upstream (see SessionScope); EraModern dialects never touch it.
//
// Returns a *CallResult with a nil Body for HTTP 202 Accepted (a
// notification with no response expected). Returns a non-nil error for any
// transport failure, for a non-2xx/202 status the era's retry logic could
// not recover from, or when the response carries a modern-era resultType of
// "input_required" — VoidLLM does not yet implement Multi Round-Trip
// Requests (docs/mcp-v2.md §3.7) — rather than forward a shape callers
// cannot act on, Call fails loudly instead of silently returning something
// wrong. The returned *CallResult is always non-nil when error is nil.
//
// For EraModern, Call skips stateFor and ensureWarm entirely: the
// 2026-07-28 revision has no session and no handshake (docs/mcp-v2.md §1a,
// §2), so there is nothing to isolate per SessionScope and nothing to warm
// up. Skipping them avoids scopesMu — an exclusive lock — on every modern
// hot-path call; only EraLegacy calls ever take it.
func (t *HTTPTransport) Call(ctx context.Context, req *CallRequest, scope SessionScope) (*CallResult, error) {
	b, err := t.resolveBinding(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve protocol era: %w", err)
	}

	if b.dialect.Version().Era() == EraModern {
		result, _, callErr := t.doCall(ctx, b, nil, req)
		return result, callErr
	}

	ss := b.stateFor(scope)
	if err := t.ensureWarm(ctx, b, ss); err != nil {
		return nil, fmt.Errorf("warmup: %w", err)
	}

	result, usedSession, callErr := t.doCall(ctx, b, ss.state, req)
	if errors.Is(callErr, errSessionExpired) {
		result, callErr = t.reinitAndRetry(ctx, b, ss, req, usedSession)
	}
	if errors.Is(callErr, errSessionExpired) {
		// reinitAndRetry retries doCall exactly once after re-initializing;
		// a second errSessionExpired means the upstream invalidated even the
		// freshly re-established session. errSessionExpired is a legacy-era
		// implementation detail of this transport (see its doc) and must
		// never reach Call's own caller — wrap it into an opaque error so
		// errors.Is(err, errSessionExpired) can no longer match outside this
		// function, while still leaving a diagnosable message.
		callErr = fmt.Errorf("session could not be re-established after retry: %v", callErr)
	}
	return result, callErr
}

// ForwardResult is the outcome of a single Forward call: the upstream's raw
// response, with no interpretation of its JSON-RPC content. Status and
// Header are the upstream response's HTTP status and headers, unmodified, so
// a caller can mirror upstream-owned state — in particular Mcp-Session-Id —
// back to its own client (see Forward's doc). Header is http.Header, not the
// package's own Get-only Header interface, because the response header
// allowlist a caller applies (internal/api/admin's headers.go) must be able
// to forward multi-valued headers (e.g. several X-RateLimit-* lines), which
// a Get-only interface cannot express.
//
// Body is the upstream response body, completely uninterpreted and streamed
// through rather than buffered: this is what makes a long-lived
// subscriptions/listen response and a tools/call response that sends
// notifications/progress before its result (docs/mcp-v2.md §3.4, §1a) work
// at all. The caller MUST call Body.Close exactly once, and MUST do so from
// whatever goroutine actually finishes reading it — never deferred at
// Handle scope on a Fiber handler's streaming path, since that goroutine
// outlives the handler's own return (see HandleMCPProxy). Body.Close is the
// only cleanup a caller needs to perform: it stops Forward's internal idle
// timer and cancels the streaming request's context before closing the
// underlying connection, so there is no separate cancel function or timer
// for the caller to track.
type ForwardResult struct {
	// Status is the HTTP status code the upstream responded with.
	Status int
	// Header carries the upstream response's headers, unmodified.
	Header http.Header
	// Body is the upstream response body. See the type doc for ownership.
	Body io.ReadCloser
}

// Forward sends raw JSON-RPC bytes to the remote MCP server exactly as
// received and returns the upstream's response, streamed through
// byte-for-byte and unbuffered, for the transparent-intermediary proxy path
// (HandleMCPProxy) where VoidLLM is not an MCP client of its own — the
// CALLER is, and the caller's own request body is that caller's own
// handshake (or, under the modern era, that caller's own per-request
// identity and capabilities). Forward differs from Call in exactly the ways
// that role requires:
//
//   - No buffering, no SSE unwrapping, no total-duration timeout: unlike
//     Call (via rawPost), Forward never reads the response into memory and
//     never uses this transport's http.Client, whose Timeout would sever
//     any stream — including a long-lived subscriptions/listen response —
//     after a fixed duration regardless of how much healthy traffic is
//     still flowing. Forward uses streamClient (Timeout: 0, but the same
//     SSRF-hardened Transport and the same redirect prohibition as the
//     buffered client) and wraps the response body in an idle timeout
//     instead (idleTimeoutReader, streamIdleTimeout) — see ForwardResult's
//     doc for how a caller releases it.
//   - No Warmup: Forward never sends its own initialize. A legacy caller's
//     "initialize", proxied straight through, reaches the upstream as the
//     ONLY handshake on the wire — restoring the pre-Phase-2 property that
//     Call's own warmup broke for HandleMCPProxy, where an upstream that
//     rejects re-initialization inside an existing session would otherwise
//     see [caller's initialize, caller's notifications/initialized,
//     VoidLLM's own initialize] and lose every legacy client proxied through
//     it (docs/mcp-v2.md, FIX 1/FIX 3).
//   - No response parsing and no MRTR rejection: Call's doCall feeds every
//     modern-era 200 body through the dialect's Parse to detect
//     resultType:"input_required" and turns it into an error, because Call's
//     caller (ListTools, CallMCPTool) cannot itself act on that shape. On
//     this path the real MCP client on the other side of the proxy CAN act
//     on it — it is the one obligated by the spec to retry with
//     inputResponses (docs/mcp-v2.md §3.7) — so Forward must let that shape
//     (and any other upstream response, including ordinary JSON-RPC errors
//     and non-2xx/202 HTTP statuses) through untouched rather than mapping
//     it to a Go error.
//   - No session OF ITS OWN, and, as of docs/mcp-v2.md's session-binding
//     fix, genuinely session-blind too: VoidLLM never mints, holds, or
//     re-initializes a legacy session on this path — hdr's Mcp-Session-Id,
//     if any, is the CALLER's own, not VoidLLM's, and Forward writes no
//     UpstreamState for it, checks it against nothing, and records nothing
//     from the response either. Forward relays hdr exactly as given and
//     mirrors back exactly what the upstream answered with, unconditionally,
//     regardless of era. An earlier revision of this fix had Forward itself
//     check an inbound Mcp-Session-Id against a per-scope record of what the
//     upstream had actually issued, gated on the resolved binding's era —
//     but that gate was wrong: a dual-era upstream that answers
//     server/discover as modern (so its binding resolves to EraModern) may
//     still accept headerless legacy traffic at the very same endpoint, and
//     a caller doing exactly that sailed straight through the era-gated
//     check unexamined, because the check never ran for a binding that
//     merely LOOKS modern to the one caller who happened to probe it first.
//     The check itself was also anchored to eraBinding — which does not
//     outlive a single ad-hoc transport built for a cache-miss request
//     (buildAdHocTransport), so a caller's own legitimate session could be
//     forgotten before its own follow-up request arrived. Both problems are
//     fixed by moving the entire mechanism out of Forward and up to
//     HandleMCPProxy, which checks and records via a *SessionRegistry it
//     holds independently of any one HTTPTransport, and which gates the
//     check on whether hdr carries an Mcp-Session-Id at all rather than on
//     what era anything resolved to — a modern request has no reason to
//     carry that header in the first place (docs/mcp-v2.md §2, §3.1), so
//     "the header is present" already implies "this needs checking" without
//     consulting the binding at all. See HandleMCPProxy and SessionRegistry
//     for the full contract; Forward itself now has nothing left to do here
//     beyond relaying hdr byte-for-byte, exactly like every other header
//     forwardHeaders (mcp_proxy.go) puts in it.
//
// resolveBinding is still used — Forward needs the resolved binding's POST
// target (postTarget), which is a property of the era per docs/mcp-v2.md
// §4.7 — but ensureWarm never runs, and, since the session-binding fix
// above, neither does anything else that reads b.dialect.Version().Era() on
// this path.
//
// A connection failure or a non-2xx/202 response that never got past
// building or sending the request is returned as a Go error, exactly as
// before: the caller has sent no bytes to its own client yet at that point
// and can still answer with an ordinary error response. Once Forward has
// returned a *ForwardResult successfully, the upstream is reachable and
// headers have arrived; if the body stream then breaks partway through,
// that is no longer Forward's concern to report — see HandleMCPProxy's
// mid-stream error handling (docs/mcp-v2.md §3.9: the caller MUST retry as
// an entirely new request, so nothing is invented or recovered here).
func (t *HTTPTransport) Forward(ctx context.Context, raw []byte, hdr MapHeader) (*ForwardResult, error) {
	b, err := t.resolveBinding(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve protocol era: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.postTarget(b), bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	// Authentication is applied BEFORE hdr below — see rawPost's identical
	// ordering and its doc for why the merge (applyExtraHeaders) must still
	// refuse to let hdr overwrite whichever header this switch just set
	// (docs/mcp-v2.md, review finding C4). On this path hdr is
	// forwardHeaders(c) (mcp_proxy.go): the caller's own already-validated
	// MCP-Protocol-Version/Mcp-Method/Mcp-Name/Mcp-Session-Id, plus whatever
	// Mcp-Param-{Name} headers the caller itself sent, collected via
	// collectMCPParamHeaders — which already drops a candidate colliding
	// with this server's own auth_header (its rule 5) before hdr is even
	// built. applyExtraHeaders is still applied here, unconditionally,
	// rather than trusted to be redundant: it is the ONLY guard on the
	// rawPost/Call path (dialect2026Client.Prepare has no equivalent
	// collision check of its own — see rawPost's doc), and duplicating this
	// protection at the one place hdr is actually merged onto req, instead
	// of in every function that builds an hdr, is what keeps the two paths
	// from silently drifting apart again.
	switch t.authType {
	case "bearer":
		if t.authToken == "" {
			return nil, errEmptyAuthCredential
		}
		req.Header.Set("Authorization", "Bearer "+t.authToken)
	case "header":
		if t.authHeader == "" || t.authToken == "" {
			return nil, errEmptyAuthCredential
		}
		req.Header.Set(t.authHeader, t.authToken)
	case "oauth":
		if t.oauthManager == nil || t.oauthConfig == nil {
			return nil, errOAuthNotConfigured
		}
		oauthToken, oauthErr := t.oauthManager.GetToken(ctx, t.serverID, *t.oauthConfig)
		if oauthErr != nil {
			return nil, fmt.Errorf("oauth token: %w", oauthErr)
		}
		req.Header.Set("Authorization", "Bearer "+oauthToken)
	}

	applyExtraHeaders(req, hdr, t.protectedAuthHeaderName())

	// streamCtx, not ctx: the idle timeout must be able to tear down THIS
	// request specifically without depending on — or affecting — whatever
	// deadline or cancellation ctx itself carries. Ownership of streamCancel
	// passes to the idleTimeoutReader constructed below, which is in turn
	// owned by the caller via ForwardResult.Body — see that type's doc.
	streamCtx, streamCancel := context.WithCancel(ctx)
	req = req.WithContext(streamCtx)

	resp, err := t.streamClient.Do(req)
	if err != nil {
		streamCancel()
		return nil, fmt.Errorf("transport: %w", err)
	}

	// Any Content-Encoding still present here is unsolicited. Neither rawPost
	// nor Forward ever set Accept-Encoding themselves, which leaves Go's own
	// http.Transport free to add "Accept-Encoding: gzip" on its own — and,
	// for a response actually encoded that way, to transparently decompress
	// the body and strip this header before RoundTrip returns (see
	// net/http.Transport's DisableCompression doc): that is why a gzip
	// response never reaches this check, and why DisableCompression must
	// stay false — setting it would also turn off that transparent
	// decompression and break every upstream that compresses by default.
	// Anything that survives is an encoding VoidLLM never asked for (br,
	// zstd, deflate, ...). Forwarding it byte-for-byte through this
	// function's caller-facing pass-through (HandleMCPProxy) would let a
	// malicious or misconfigured upstream turn settings.mcp.stream_max_bytes
	// — which counts WIRE bytes — into a multi-gigabyte decompression bomb
	// for the real MCP client on the other end of the proxy, who negotiated
	// no such encoding either (docs/mcp-v2.md, review finding B; see also
	// internal/proxy/headers.go's sibling reasoning for the LLM proxy's own
	// Accept-Encoding handling). Treated exactly like any other failure
	// Forward's own caller never gets to see response bytes for: the body is
	// closed, the stream's own idle-timeout context is torn down, and a
	// plain error is returned — never the upstream's own Content-Encoding
	// value, which is upstream-controlled text with no legitimate reason to
	// end up in a VoidLLM log line.
	if resp.Header.Get("Content-Encoding") != "" {
		resp.Body.Close() //nolint:errcheck // best-effort close; the body was never read
		streamCancel()
		return nil, errors.New("transport: upstream sent an unsolicited response Content-Encoding")
	}

	// The upstream's own Mcp-Session-Id response header, if any, is recorded
	// by HandleMCPProxy against its *SessionRegistry after Forward returns —
	// not here. Forward itself no longer reads or interprets this response
	// header at all; it is mirrored back to the caller unmodified as part of
	// resp.Header below, exactly like every other response header.
	body := newIdleTimeoutReader(resp.Body, t.streamIdleTimeout, streamCancel)

	return &ForwardResult{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// reinitAndRetry re-establishes a legacy session exactly once after the
// upstream reports it expired, then retries the original request exactly
// once more — it does not loop. If another concurrent Call in the same
// SessionScope already refreshed the session (detected by comparing against
// staleSession) it reuses that session instead of re-initializing again,
// mirroring the double-check the previous mcpReInitMu-guarded sync.Map
// implementation performed. staleSession is the session ID actually sent on
// the wire for the call that just failed — read back from the header map
// Prepare produced, via doCall's return value, rather than re-read from
// ss.state, which a concurrent Call could have already replaced between
// doCall capturing it and this function running (docs/mcp-v2.md §11.3
// Befund 2: comparing against a re-read snapshot that was never actually
// sent could mistake a fresh, unrelated session for a successful renewal of
// this call's own). If re-initialization itself fails to establish a usable
// session, the cached era binding is discarded so the NEXT call re-probes
// this upstream from scratch (see invalidateBinding) instead of every future
// call being wedged against a guess that just proved wrong.
func (t *HTTPTransport) reinitAndRetry(ctx context.Context, b *eraBinding, ss *scopedState, req *CallRequest, staleSession string) (*CallResult, error) {
	ss.mu.Lock()
	if current := ss.state.SessionID(); current != "" && current != staleSession {
		ss.mu.Unlock()
		result, _, err := t.doCall(ctx, b, ss.state, req)
		return result, err
	}
	ss.state.ClearSessionID()
	warmErr := b.dialect.Warmup(ctx, t.roundTripper(b), ss.state)
	ss.mu.Unlock()
	if warmErr != nil {
		t.invalidateBinding(b)
		return nil, fmt.Errorf("re-initialize after session expiry: %w", warmErr)
	}
	result, _, err := t.doCall(ctx, b, ss.state, req)
	return result, err
}

// doCall prepares req via b's dialect against st, sends it, and interprets
// the HTTP status and (for the modern era only) the JSON-RPC result shape —
// including, for a resultType:"complete" response, the CacheableResult hint
// Parse extracted, which flows into the returned *CallResult's Cache field.
// See Call's doc for the overall contract; this is the part that runs once
// per attempt, without any retry logic of its own. The returned session
// string is the Mcp-Session-Id actually placed on the wire for this
// specific attempt — read back from the header map b.dialect.Prepare
// produced, not from st, which may already have moved on by the time the
// caller inspects it — and is empty for the modern era, which never sets
// that header. st may be nil when b's dialect is EraModern: the modern
// ClientDialect's Prepare never dereferences it, and this function's only
// other read of st is inside the EraLegacy-only branch below. The returned
// *CallResult is always non-nil when error is nil.
func (t *HTTPTransport) doCall(ctx context.Context, b *eraBinding, st *UpstreamState, req *CallRequest) (*CallResult, string, error) {
	prepared, hdr, err := b.dialect.Prepare(req, st)
	if err != nil {
		return nil, "", fmt.Errorf("prepare request: %w", err)
	}
	usedSession := hdr.Get(HeaderSessionID)

	res, err := t.rawPost(ctx, t.postTarget(b), prepared, hdr)
	if err != nil {
		return nil, usedSession, err
	}

	if b.dialect.Version().Era() == EraLegacy {
		// Legacy servers may refresh the session on any response, not only
		// on initialize; mirror that here exactly as the pre-Phase-2
		// implementation did for every Call.
		if sid := res.header.Get(HeaderSessionID); sid != "" {
			st.SetSessionID(sid)
		}
	}

	switch res.status {
	case http.StatusNotFound:
		if b.dialect.Version().Era() == EraLegacy {
			return nil, usedSession, errSessionExpired
		}
		return nil, usedSession, fmt.Errorf("upstream returned HTTP %d", res.status)
	case http.StatusAccepted:
		// Notification acknowledged — no response body expected per the MCP spec.
		return &CallResult{}, usedSession, nil
	case http.StatusOK:
		// fall through to result handling below
	default:
		return nil, usedSession, fmt.Errorf("upstream returned HTTP %d", res.status)
	}

	if len(res.body) == 0 {
		return &CallResult{Body: res.body}, usedSession, nil
	}

	if b.dialect.Version().Era() == EraModern {
		parsed, perr := b.dialect.Parse(res.body)
		if perr != nil {
			return nil, usedSession, fmt.Errorf("mcp: %s", perr.Message)
		}
		var cache CacheHint
		if parsed != nil {
			cache = parsed.Cache
		}
		return &CallResult{Body: res.body, Cache: cache}, usedSession, nil
	}

	return &CallResult{Body: res.body}, usedSession, nil
}

// maxJSONSkipDepth bounds how many nested '{'/'[' containers skipJSONValue
// (via skipContainerBody) and countArrayElements' own object-skip branch
// will follow into before giving up with errJSONSkipMaxDepthExceeded. It is
// deliberately far smaller than either encoding/json's or sonic's own
// built-in nesting limits (on the order of 10,000): this guard exists so a
// pathological page — one whose "tools" key's value, or any OTHER key's
// value this walk has to skip past, nests tens or hundreds of thousands of
// levels deep, a shape that costs an attacker only a couple of bytes per
// level to construct — is rejected by an explicit, bounded depth counter
// this package owns and controls, rather than by however deep whichever
// JSON library happens to be running underneath decides to tolerate before
// erroring on its own. 512 comfortably exceeds any nesting depth a
// legitimate tool schema could plausibly need (MCP tool input schemas are
// typically a handful of levels deep at most) while still failing fast, long
// before either library's own much larger internal limit would even be
// reached.
const maxJSONSkipDepth = 512

// errJSONSkipMaxDepthExceeded is returned by skipJSONValue and
// countArrayElements (via skipContainerBody) when a value being skipped
// nests more than maxJSONSkipDepth containers deep. It is a bare, static
// sentinel — never wrapped with any detail about the value itself, which is
// upstream-controlled content this package's zero-knowledge-logging rule
// already keeps out of error text everywhere else in this file.
var errJSONSkipMaxDepthExceeded = errors.New("mcp: json value nests deeper than the maximum depth this walk will follow")

// errJSONObjectKeyNotString is returned by countResultTools and
// countToolsListPageTools when *json.Decoder's Token() call, invoked right
// after dec.More() reported another object member was present, returns a
// token that is not a Go string. Per encoding/json's own documented
// contract, Token() always returns object member names as strings — this
// branch is therefore unreachable given the decoder's own invariants and
// exists purely as defense in depth against a future change to that
// contract (or a decoder swap) rather than any input this package has ever
// observed trigger it. It is a bare, static sentinel for the same reason
// every other guard in this file is: whatever token actually came back is
// upstream-controlled content this package's zero-knowledge-logging rule
// keeps out of error text.
var errJSONObjectKeyNotString = errors.New("mcp: json decoder returned a non-string object member name")

// skipJSONValue consumes exactly one JSON value from dec — a scalar
// (string, number, bool, or null; already fully consumed by the single
// Token() call that reads it), or a full object or array, including every
// value nested inside it — without ever unmarshaling into any typed Go
// value beyond the tokens themselves. This is the "skip a value neither of
// us has any interest in the CONTENTS of, only in getting past it"
// primitive countToolsListPageTools' own object-key loop and
// countArrayElements' own array-element loop both use.
//
// It is iterative, not recursive: skipContainerBody below tracks how many
// containers are currently open with a single explicit depth counter,
// incremented on every '{'/'[' and decremented on every matching '}'/']',
// rather than calling itself once per nesting level the way an earlier
// version of this function did. Every value this function is ever asked to
// skip is upstream-controlled content; a naive recursive walker over it
// would grow this goroutine's own call stack by one frame per nesting
// level, which an upstream sending a value nested tens or hundreds of
// thousands of levels deep — trivially cheap to construct, one open
// bracket per level — could drive arbitrarily high. The explicit counter
// bounds that cost to a small, fixed amount of stack regardless of how deep
// the value actually nests, and maxJSONSkipDepth (see its own doc) turns
// "arbitrarily deep" into a fast, bounded rejection instead.
func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		// A scalar — string, number, bool, or null — Token() already
		// consumed it in full; nothing more to skip.
		return nil
	}
	if delim == '{' || delim == '[' {
		return skipContainerBody(dec, 1)
	}
	return nil
}

// skipContainerBody consumes tokens from dec until depth currently-open
// containers have all been closed. depth starts above zero because the
// caller has already consumed the outermost container's own opening '{' or
// '[' before calling this function — skipJSONValue's own first Token()
// call, or countArrayElements' peek at a "tools" value that turned out to
// be an object rather than an array (see that function's own doc) — so this
// function itself never reads an opening delimiter it did not already know
// about via depth's initial value.
//
// Every '{' or '[' token encountered increments depth (bounded by
// maxJSONSkipDepth — see that constant's own doc); every '}' or ']' token
// decrements it; every other token — an object key, or a scalar value at
// any level — is read and discarded without affecting depth at all. See
// skipJSONValue's own doc for why this explicit counter, rather than one
// recursive call per nesting level, is what keeps this bounded regardless
// of how deep the container actually nests.
func skipContainerBody(dec *json.Decoder, depth int) error {
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			continue
		}
		switch delim {
		case '{', '[':
			depth++
			if depth > maxJSONSkipDepth {
				return errJSONSkipMaxDepthExceeded
			}
		case '}', ']':
			depth--
		}
	}
	return nil
}

// countArrayElements reads exactly one JSON value from dec — the value
// immediately following a "tools"-matching key, dec positioned right before
// it — and, if that value is a JSON array, counts its elements without
// unmarshaling into any of them, stopping the instant the count exceeds
// budget rather than draining (or even fully skipping) whatever elements
// remain past that point: a caller that has already established "this
// array alone pushes the page over its ceiling" has no further use for the
// array's own exact remaining length, mirroring countToolsListPageTools'
// own early-exit contract for the page as a whole (see its own doc). budget
// may already be negative — the running total is already at or past the
// ceiling before this array is even considered — in which case the very
// first element, if any, already exceeds it.
//
// If the value is anything other than a JSON array — an object, string,
// number, bool, or null — it is fully skipped (contributing 0 elements) so
// dec is left correctly positioned for whatever key follows, per this
// package's "treat a non-array tools value as 0" contract: an upstream is
// free to name some OTHER, unrelated field "tools", "Tools", or any other
// case variant that is not itself a tool list at all, and this pre-check
// must neither crash on it nor mistake it for one.
func countArrayElements(dec *json.Decoder, budget int) (n int, err error) {
	tok, err := dec.Token()
	if err != nil {
		return 0, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		// A scalar value — already fully consumed above, not an array:
		// contributes 0.
		return 0, nil
	}
	if delim != '[' {
		// '{' — an object, not an array: contributes 0, but its contents
		// still need to be skipped — its own opening '{' was already
		// consumed by the Token() call above, hence depth starting at 1.
		return 0, skipContainerBody(dec, 1)
	}

	for dec.More() {
		if err := skipJSONValue(dec); err != nil {
			return n, err
		}
		n++
		if n > budget {
			// Over budget: stop here, leaving dec positioned mid-array on
			// purpose. countToolsListPageTools returns immediately once it
			// sees the running total exceed limit and never reads from dec
			// again — see its own doc.
			return n, nil
		}
	}
	// Under budget: unlike the early return above, this array's own caller
	// (countResultTools) keeps using dec afterward — to look for another
	// "tools"-matching key, and eventually to consume the enclosing
	// result object's own closing '}' — so, unlike the early return above,
	// the array's closing ']' must be consumed here before returning.
	if _, err := dec.Token(); err != nil {
		return n, err
	}
	return n, nil
}

// countResultTools reads exactly one JSON value from dec — a top-level
// "result"-matching key's value, dec positioned right before it — and, if
// that value is a JSON object, sums the element counts of every key inside
// it that equals "tools" case-insensitively (strings.EqualFold — catching
// "Tools", "TOOLS", or any other case variant, not only an exact lowercase
// match), stopping the instant the running sum exceeds limit rather than
// continuing to walk the rest of the object. See countToolsListPageTools'
// own doc for why summing every match, rather than trying to pick the one
// key the real decode would resolve "tools" to, is the conservative choice
// this walk deliberately makes. If the value is not a JSON object at all —
// a scalar or an array — it is fully skipped, contributing 0: a "result"
// that carries no object at all has no "tools" field to sum, exactly as
// countToolsListPageTools' own doc for a non-object result describes.
func countResultTools(dec *json.Decoder, limit int) (count int, exceeded bool, err error) {
	tok, err := dec.Token()
	if err != nil {
		return 0, false, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return 0, false, nil // scalar result value — no tools field to sum.
	}
	if delim != '{' {
		// '[' — result is an array, not an object: no tools field, but its
		// contents still need to be skipped (its own opening '[' was
		// already consumed above, hence depth starting at 1).
		return 0, false, skipContainerBody(dec, 1)
	}

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return count, false, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return count, false, errJSONObjectKeyNotString
		}
		if !strings.EqualFold(key, "tools") {
			if err := skipJSONValue(dec); err != nil {
				return count, false, err
			}
			continue
		}

		n, err := countArrayElements(dec, limit-count)
		if err != nil {
			return count, false, err
		}
		count += n
		if count > limit {
			return count, true, nil
		}
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return count, false, err
	}
	return count, false, nil
}

// countToolsListPageTools performs a streaming token walk over body — one
// tools/list page's raw JSON-RPC response — to count how many elements
// result.tools carries, without ever unmarshaling a single element into a
// Tool (or any other allocating shape). It exists so ListTools' running
// len(allTools)+count bound (maxToolsListTools) can be enforced BEFORE the
// much more expensive jsonx.Unmarshal(body, &rpcResp) call at its own call
// site ever runs: a page whose tools array holds millions of minimal {}
// elements can stay comfortably under maxToolsListTotalBytes (32 MiB is
// roughly 3 bytes per element at that count) and even under a single page's
// own rawPostMaxBodyBytes (10 MiB) ceiling, while still costing an enormous
// amount of memory once each element is unmarshaled into a full Tool struct
// (InputSchema bytes, HeaderParams validation, name/description strings,
// ...).
//
// The whole walk — both the top-level "result" key and, inside it, every
// "tools"-matching key — runs over a single encoding/json *Decoder token
// stream, deliberately never through jsonx.Unmarshal (sonic): an earlier
// version of this function routed the outer "result" field through
// jsonx.Unmarshal specifically to reuse its exact duplicate/case-variant
// key resolution, but on a build where sonic falls back to its own
// generic-value machinery (see internal/jsonx's own package doc) that
// single call alone allocates many times more than decoding the SAME bytes
// straight into a typed []Tool slice would — the opposite of what a
// pre-decode bound exists to guarantee, and the reason a dedicated,
// allocation-bounded test exists for this function at all
// (TestCountToolsListPageTools_LargeArray_ExceededWithBoundedAllocs).
//
// A single sequential *json.Decoder walk turns out to reproduce
// jsonx.Unmarshal's own resolution exactly anyway, not merely approximate
// it: encoding/json (and sonic's ConfigStd, built to mimic it) decodes an
// object into a struct by processing its keys strictly in document order
// and, for each one that maps to a given field — case-insensitively when no
// exact match exists, as ordinary for that field alone — OVERWRITING
// whatever that field already held from an earlier key. Since the struct
// the real decode (ListTools' own jsonx.Unmarshal(body, &rpcResp) call)
// targets has exactly one field mapped from "result", every key anywhere in
// body that equals "result" case-insensitively maps to that SAME field —
// there is no second, competing field for "exact match" preference to ever
// have to arbitrate between — so the real decode's own outcome is already
// exactly "whichever result-matching key appears LAST in body, verbatim".
// The top-level loop below reproduces that identically: it walks every key
// of body's own top-level object in order, and every time it sees one
// matching "result" case-insensitively, it computes that key's own
// "tools"-sum via countResultTools and OVERWRITES this function's own
// running count with it, discarding whatever an earlier "result" key
// produced — the same "keep processing in order, last write wins" rule,
// applied identically. Only when a given "result" candidate's own count
// would exceed limit does this function deviate from strict fidelity and
// return exceeded immediately without first confirming that candidate is
// the actual final one: this is deliberately still the conservative
// direction (rejecting a body the real decode might have accepted, never
// the reverse) — see countResultTools' own doc for the identical reasoning
// applied to "tools" duplicates one level down.
//
// limit is maxToolsListTools minus however many tools ListTools has already
// accumulated across earlier pages of this same fetch. A non-positive limit
// (the running total already at or past the ceiling before this page's own
// tools are even considered) is reported as exceeded immediately, without
// reading any further into body at all.
//
// Fail-closed contract: err is non-nil whenever this walk stopped before it
// had fully accounted for the top-level object's own "result" key (or
// confirmed none exists), for ANY reason — a genuine JSON syntax error, an
// I/O failure, an object member name that was not a string
// (errJSONObjectKeyNotString — see its own doc for why this is defense in
// depth against an unreachable case, not a real input this package has ever
// observed), or a value nested deeper than maxJSONSkipDepth
// (errJSONSkipMaxDepthExceeded). ListTools treats any such error as reason
// to fail the WHOLE fetch via errToolsListPreScanFailed, WITHOUT ever
// reaching jsonx.Unmarshal — see that call site's own comment for why. This
// used to instead be reported as the same (0, false) "not exceeded" outcome
// a clean, tools-free page produces, which was a fail-OPEN bug: this walk
// runs over a plain *encoding/json.Decoder (via jsonx.NewDecoder — see that
// function's own "falls back to stdlib" doc), a DIFFERENT implementation
// from the real decode's jsonx.Unmarshal (sonic's ConfigStd), and this
// file's own doc elsewhere already documents that sonic's own generic
// fallback machinery can diverge from encoding/json's behavior on the same
// bytes. maxJSONSkipDepth (512) in particular is deliberately far smaller
// than either library's own much larger internal nesting limit (see that
// constant's own doc) specifically so this walk fails fast on a
// pathologically deep value — which means a body shaped like
// {"junk": <513 levels deep>, "result": {"tools": [...millions...]}} trips
// this walk's depth guard on the UNRELATED "junk" key, strictly before ever
// reaching "result", while the real decode's own, much more permissive
// depth tolerance sails straight past that same "junk" key and goes on to
// fully unmarshal the enormous "tools" array into typed Tool structs — the
// exact per-element allocation cost this whole pre-decode bound exists to
// avoid ever paying for an unbounded page. Any OTHER decode error uncovered
// mid-walk carries the identical risk for the identical reason: this
// function cannot prove, from the error alone, that the real decode would
// have failed at the same point rather than tolerating it and continuing on
// to a later, well-formed "tools" array this walk never got to see — so
// every one of them is treated the same way, fail closed, rather than
// trying to classify which specific errors are "safe" to let through.
//
// Two, and only two, outcomes remain (0, false, nil) — no error — despite
// not having walked the entire body, because both are provably safe
// regardless of any behavioral difference between this walk's decoder and
// the real decode's: they are decided by the FIRST token of body alone,
// before this function has consumed anything a "result" key's value could
// ever have followed.
//
//   - body's very first token is a valid, complete JSON value that is not an
//     object at all (a JSON array, string, number, bool, or null) — no
//     RFC 8259-compliant parser, whichever library implements it, can ever
//     resolve a top-level "result" key out of a value that structurally
//     is not an object; this holds independent of any parser-specific
//     leniency elsewhere. The real decode's own outcome for this shape is
//     bounded the same way this walk's is: encoding/json (and sonic's
//     ConfigStd) responds to a top-level type mismatch against a struct
//     target by skipping the mismatched value at the token level, the same
//     class of cheap, non-allocating walk this package's own
//     skipJSONValue/skipContainerBody perform, never by decoding it into
//     any typed Tool slice.
//   - dec.More() reports no further top-level members, and no key seen so
//     far matched "result" — a genuine, complete scan of every top-level
//     key that turned up no "result" at all. The real decode leaves
//     rpcResp.Result at its zero value for the identical reason.
//
// Every OTHER path — in particular a decode error encountered ANYWHERE
// after the first token has confirmed the top level IS an object — fails
// closed, per the contract above.
//
// Trailing bytes after the top-level object's own closing '}' are never
// inspected by this walk at all: it stops as soon as dec.More() reports no
// further top-level members, without reading (or needing to read) the
// closing '}' itself. This is safe without an explicit check: JSON's own
// brace-matching rules mean nothing that could ever appear AFTER a
// well-formed top-level object's closing '}' can retroactively change what
// that object's own "result"/"tools" keys already resolved to — trailing
// bytes cannot smuggle a bigger "tools" array into a page whose properly
// nested top-level object this walk already counted correctly. Separately,
// this repo's jsonx.Unmarshal (sonic's ConfigStd) has been confirmed to
// still reject a body carrying trailing non-whitespace bytes after its
// top-level value (a non-nil error, matching encoding/json's own
// json.Unmarshal contract), so ListTools' own decode-error path still fails
// the fetch for such a body rather than silently accepting it — this walk
// simply never needed to duplicate that check itself.
func countToolsListPageTools(body []byte, limit int) (count int, exceeded bool, err error) {
	if limit < 0 {
		return 0, true, nil
	}

	dec := jsonx.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return 0, false, err
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		// Not an object at the top level — provably safe, no error; see
		// this function's own doc.
		return 0, false, nil
	}

	found := false
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return count, false, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return count, false, errJSONObjectKeyNotString
		}
		if !strings.EqualFold(key, "result") {
			if err := skipJSONValue(dec); err != nil {
				return count, false, err
			}
			continue
		}

		n, resultExceeded, err := countResultTools(dec, limit)
		if err != nil {
			return count, false, err
		}
		if resultExceeded {
			return n, true, nil
		}
		count = n
		found = true
	}
	if !found {
		return 0, false, nil
	}
	return count, false, nil
}

// ListTools sends tools/list to the remote server, follows every nextCursor
// page it returns, parses and aggregates the tool definitions across all of
// them, and applies FilterHeaderParamTools to the aggregated list before
// returning. Session/handshake management, if the resolved era needs any,
// happens transparently inside Call — in the modern era this sends only
// tools/list, with no initialize and no notifications/initialized, matching
// the stateless core (docs/mcp-v2.md §2). ListTools is tool discovery, not a
// call on behalf of any one caller's organization, so it always uses the
// empty SessionScope, isolated from every real org's legacy session on this
// same server.
//
// Pagination: the first request carries no params, exactly as it did before
// pagination existed. Whenever a page's response carries a nextCursor, the
// following request's params.cursor is set to that value, and ListTools
// loops; an absent (or explicit JSON null) nextCursor ends the fetch — MCP's
// pagination contract treats a MISSING nextCursor as "no more pages", not an
// empty one: nextCursor is decoded as *string specifically so an upstream
// that returns cursor: "" is followed one more time (a nil pointer is "no
// more"; a non-nil pointer to "" is a real, if unusual, cursor). Both
// ClientDialect implementations already forward params.cursor unmodified —
// legacyClientDialect.Prepare returns req.Raw byte-for-byte, and
// dialect2026Client.Prepare only ever adds or replaces the top-level "_meta"
// key inside params, leaving every other key (cursor included) exactly as
// this method set it — so no dialect-level change was needed to carry it.
//
// This method never returns a partial listing: any of the guards below
// (see their own docs — errToolsListTooManyPages, errToolsListTooManyTools,
// errToolsListTooManyBytes, errToolsListCursorTooLong,
// errToolsListRepeatedCursor, errToolsListDuplicateTool,
// errToolsListNilBodyMidFetch) or a page-level fetch/decode/protocol error
// fails the WHOLE fetch, discarding every page already read, rather than
// returning what was accumulated so far. The one exception, unchanged from
// before pagination existed, is a nil response body on the very FIRST page —
// see errToolsListNilBodyMidFetch's own doc for why only the first page gets
// this treatment.
//
// ctx is checked for cancellation or deadline expiry at the top of every
// page's iteration, in addition to whatever bounds this method's own caller
// applies via ToolCache's singleflight fetch (see ToolCache.sharedFetch):
// this is what bounds a caller of ListTools directly — the health checker
// and the admin API's test-connection endpoint, neither of which goes
// through ToolCache at all — by its own context, rather than only by
// maxToolsListPages.
//
// errToolsListDuplicateTool is a bare, static sentinel: unlike
// errToolsListCursorTooLong or the others above, it deliberately never
// embeds the colliding tool name, upstream-controlled content that this
// package's zero-knowledge-logging rule already keeps out of every other
// guard's error text here.
//
// CacheHint aggregation (MCP 2026-07-28 §5), per page's own result.Cache.
// Every page falls into exactly one of three hint categories — CacheHint.
// Scope has no fourth value (parseCacheHint's own doc), so these three are
// exhaustive:
//
//   - "no hint": TTLMsSet false (the common case today, since CacheableResult
//     is opt-in and most upstreams, including every legacy-era one, never
//     set it).
//   - "public hint": TTLMsSet true, Scope == CacheScopePublic.
//   - "private hint": TTLMsSet true, Scope == CacheScopePrivate.
//   - "unknown-scope hint": TTLMsSet true, Scope neither public nor private
//     (parseCacheHint leaves Scope at its zero value "" when cacheScope was
//     absent or unrecognized within an otherwise-present hint).
//
// The aggregate is decided by which of these categories appear across every
// page of the fetch, in this priority order — persistListing is the sole
// reader of the resulting Scope/TTLMsSet/TTLMs, and its own doc defines
// exactly what each combination does (save, delete, or neither):
//
//  1. ANY page is "private hint": the aggregate's Scope is forced to
//     CacheScopePrivate unconditionally — regardless of what any OTHER page
//     said, including a page that offered no hint at all, which would
//     otherwise mask this page's explicit privacy claim entirely. This is
//     checked FIRST, ahead of every other rule below, because "fail closed
//     to delete-only" is the safest possible outcome for a listing any page
//     ever claimed contains caller-specific data — persistListing's own
//     Scope == CacheScopePrivate branch reads Scope alone, so nothing else
//     computed below can override it once this rule applies. TTLMsSet/TTLMs
//     are still populated from every hint-carrying page (rule 4) purely so
//     ToolCache.resolveTTL — which reads Cache regardless of what
//     persistListing will do with it — has an accurate freshness window for
//     the in-memory entry; persistListing itself never consults them once
//     Scope is private.
//  2. Else, ANY page is "unknown-scope hint": the aggregate is neither
//     public nor private — TTLMsSet true, Scope left at "" — exactly the
//     shape persistListing already treats as "unproven: neither save nor
//     delete" for a single-page fetch with that Scope. This applies
//     EVEN IF other pages carried no hint at all: an unproven scope claim is
//     not something a hint-less page can dilute back into "safe to persist"
//     — persisting on the strength of the OTHER pages' silence would still
//     hand every caller a listing this one page never actually vouched for.
//     TTLMs is the minimum across every page that carried ANY hint
//     (public or unknown-scope alike — see rule 4), so the aggregate still
//     carries a sensible freshness window rather than none at all.
//  3. Else (no page was private or unknown-scope — every page was either
//     "no hint" or "public hint"): the aggregate MAY be persisted.
//     - If every page actually carried a hint (all of them "public hint",
//     since neither of the other two categories is present here), the
//     aggregate carries that agreement through: TTLMsSet true, Scope
//     CacheScopePublic, TTLMs the minimum across every page.
//     - If at least one page offered no hint at all, the aggregate
//     collapses to "no hint" (the zero CacheHint) — the same "if ANY page
//     carried no hint at all, the whole aggregated listing gets no hint"
//     behavior this rule always had, now scoped to apply only once
//     private/unknown-scope pages are already ruled out. persistListing's
//     own no-hint branch still persists it (mirroring a legacy upstream
//     that never implements CacheableResult at all), it simply carries no
//     TTL opinion of its own.
//  4. TTLMs/TTLMsSet, independent of the Scope decision above: TTLMsSet is
//     true, and TTLMs is the minimum ttlMs across every page whose own hint
//     was present (TTLMsSet true), whenever at least one page carried a
//     hint at all — regardless of category, so a private or unknown-scope
//     page still contributes to (and can lower) the minimum. This is what
//     ToolCache.resolveTTL reads to size the in-memory entry's freshness
//     window even when persistListing goes on to neither save nor delete
//     (rule 2) or to delete (rule 1) — the in-memory cache and the backing
//     store answer two different questions and are sized independently.
//     Only when NO page carried any hint at all does TTLMsSet stay false
//     (rule 3's "no hint" collapse).
//
// A page's Scope disagreeing with another page's — e.g. one "public", one
// "unknown-scope" — is logged once at Warn with only the server ID, never a
// cursor or scope value (both would still be diagnosable from the server ID
// alone via the upstream's own logs), whenever more than one of
// {private, unknown-scope, public} actually appears among the fetch's pages.
//
// The x-mcp-header filtering happens here, once, on the fully aggregated and
// deduplicated tool list, and only here, because this is the sole place a
// tools/list response becomes VoidLLM's own []Tool shape — MCP 2026-07-28
// §4.3 requires the exclusion to happen against the tools/list RESULT, not
// at some later consumer of it, and running it once on the aggregate (rather
// than per page) is what lets it see every tool at once. FilterHeaderParamTools
// is given t.serverID (the stable server ID this transport was constructed
// with — empty only for the handful of ad-hoc, non-persistent probes that
// pass "" to NewHTTPTransport, e.g. the startup SSE probe in cmd/voidllm's
// app wiring) so an excluded tool's warning log line can identify which
// server it came from.
//
// The returned *ToolListing's Cache field carries the aggregated
// CacheableResult hint (MCP 2026-07-28 §5) computed above. It is not merely
// carried along on ToolListing for a later change to wire up:
// ToolCache.resolveTTL reads it to decide the ttl and neverExpires a fetched
// entry is cached under, and ToolCache.persistListing reads its Scope to
// decide whether the fetch is written through to the backing store at all —
// see both methods' own docs.
//
// toolsListRPCErrorCode is the minimal shape ListTools decodes a tools/list
// response's top-level "error" field into: only the numeric JSON-RPC code.
// It deliberately has no Message or Data field at all — unlike this
// package's general-purpose Error type (protocol.go), which carries both —
// so that decoding rpcResp below can never populate either with
// upstream-controlled, free-form content in the first place. Before this
// existed, rpcResp.Error was typed *Error, so a malicious "data" payload
// (Error.Data is typed any) was fully decoded into memory even though
// nothing downstream of this decode ever reads it — ListTools' own error
// return already only ever names rpcResp.Error.Code (see below). This is
// the same "don't retain what nothing reads" principle
// dialect2026Client.Parse's own doc already documents for resp.Result.
type toolsListRPCErrorCode struct {
	Code int `json:"code"`
}

func (t *HTTPTransport) ListTools(ctx context.Context) (*ToolListing, error) {
	var (
		allTools    []Tool
		seenNames   = make(map[string]struct{})
		seenCursors = make(map[string]struct{})
		cursor      *string
		totalBytes  int64

		// aggTTLMs/aggTTLSet track the minimum ttlMs across every page whose
		// own hint was present, regardless of which of the three hint
		// categories below it fell into — see the CacheHint aggregation
		// rule table above, rule 4.
		aggTTLMs  int64
		aggTTLSet bool
		// aggNoHint, aggAnyPublicHint, aggAnyUnknownScope, and aggAnyPrivate
		// each record whether at least one page fell into that hint
		// category — see the rule table above for how the four combine.
		// A page contributes to at most one of the latter three (Scope has
		// no fourth value), so these four flags alone fully characterize
		// every page seen without needing to track each page's Scope
		// individually.
		aggNoHint          bool
		aggAnyPublicHint   bool
		aggAnyUnknownScope bool
		aggAnyPrivate      bool
	)

	for page := 0; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("mcp: tools/list: %w", err)
		}
		if page >= maxToolsListPages {
			return nil, fmt.Errorf("%w: stopped after %d requests", errToolsListTooManyPages, page)
		}

		rpcReq := Request{
			JSONRPC: "2.0",
			ID:      jsonx.RawMessage(`1`),
			Method:  "tools/list",
		}
		if cursor != nil {
			cursorRaw, err := jsonx.Marshal(*cursor)
			if err != nil {
				return nil, fmt.Errorf("marshal tools/list cursor: %w", err)
			}
			paramsRaw, err := jsonx.Marshal(map[string]jsonx.RawMessage{"cursor": cursorRaw})
			if err != nil {
				return nil, fmt.Errorf("marshal tools/list params: %w", err)
			}
			rpcReq.Params = paramsRaw
		}
		raw, err := jsonx.Marshal(rpcReq)
		if err != nil {
			return nil, fmt.Errorf("marshal tools/list: %w", err)
		}

		result, err := t.Call(ctx, &CallRequest{Raw: raw}, "")
		if err != nil {
			return nil, err
		}
		if result.Body == nil {
			// No body at all (e.g. an HTTP 202 with no payload). On the very
			// first page this mirrors the pre-pagination behavior of ending
			// the fetch with an empty listing; on any later page it means a
			// fetch already under way abruptly lost its body, and the
			// no-partial-listing contract requires failing the whole fetch
			// instead of silently truncating it — see
			// errToolsListNilBodyMidFetch's own doc.
			//
			// Returning &ToolListing{} directly here — rather than breaking
			// out of the loop into the CacheHint aggregation switch below —
			// matters beyond convenience: that switch has no case at all for
			// "zero pages ever contributed a flag", since every one of
			// aggAnyPrivate/aggAnyUnknownScope/aggNoHint stays false when no
			// page's Cache was ever inspected, so it falls through to the
			// switch's default case — the "every page was a public hint"
			// branch — and would incorrectly claim TTLMsSet true, Scope
			// CacheScopePublic for a listing that in fact carried no hint
			// from anywhere, since no upstream response was ever read at all.
			// A bare zero-value ToolListing is also exactly what the
			// pre-pagination version of this method returned for this same
			// case: an empty Tools/HeaderParams pair, not the output of
			// FilterHeaderParamTools run over an empty slice.
			if page == 0 {
				return &ToolListing{}, nil
			}
			return nil, errToolsListNilBodyMidFetch
		}

		totalBytes += int64(len(result.Body))
		if totalBytes > maxToolsListTotalBytes {
			return nil, fmt.Errorf("%w: %d bytes", errToolsListTooManyBytes, totalBytes)
		}

		// Pre-decode bound, BEFORE the far more expensive jsonx.Unmarshal into
		// []Tool below ever runs — see countToolsListPageTools' own doc for
		// why maxToolsListTotalBytes/rawPostMaxBodyBytes alone do not already
		// close this gap: a page of millions of minimal {} tool elements can
		// stay comfortably under both byte ceilings while still costing an
		// enormous amount of memory once each element is unmarshaled into a
		// full Tool struct. The post-decode len(allTools)+len(...) check
		// below still runs unchanged as defense in depth.
		//
		// A non-nil err here means the walk itself failed — for any reason,
		// including this file's own known bypass class (see
		// countToolsListPageTools' and errToolsListPreScanFailed's own docs)
		// — and the whole fetch is rejected right here, WITHOUT ever reaching
		// jsonx.Unmarshal below for this page: that unmarshal is exactly the
		// expensive, unbounded call this pre-scan exists to gate, so a pre-scan
		// that could not finish must never be treated as "go ahead anyway".
		if _, exceeded, err := countToolsListPageTools(result.Body, maxToolsListTools-len(allTools)); err != nil {
			return nil, errToolsListPreScanFailed
		} else if exceeded {
			return nil, fmt.Errorf("%w: accumulated more than %d tools", errToolsListTooManyTools, maxToolsListTools)
		}

		var rpcResp struct {
			Result struct {
				Tools      []Tool  `json:"tools"`
				NextCursor *string `json:"nextCursor"`
			} `json:"result"`
			// Error only decodes the numeric JSON-RPC code — never Message or
			// Data, both upstream-controlled, free-form content this
			// package's zero-knowledge-logging rule keeps out of decoded
			// memory entirely, not merely out of the error text built from it
			// below (see toolsListRPCErrorCode's own doc).
			Error *toolsListRPCErrorCode `json:"error"`
		}
		if err := jsonx.Unmarshal(result.Body, &rpcResp); err != nil {
			// err's own message is deliberately never embedded here: this
			// package's JSON decoder (internal/jsonx, backed by sonic) reports a
			// syntax error by quoting a window of the SOURCE bytes around the
			// failure position — an upstream that returns deliberately malformed
			// JSON with embedded content would otherwise put those bytes into
			// whatever log line a caller builds from err.Error() (docs/mcp-v2.md
			// review round, Fund 4; the same class ToolHeaderParams' own decode
			// error already closed — see that function's identical comment). The
			// fixed message plus errToolsListDecodeFailed is diagnosis enough:
			// this response did not even parse as the expected JSON-RPC shape.
			return nil, fmt.Errorf("%w: tools/list response is not valid JSON-RPC", errToolsListDecodeFailed)
		}
		if rpcResp.Error != nil {
			// An upstream-controlled, free-form "message" (and any "data") is
			// never embedded in a Go error, which callers log via err.Error()
			// (see docs/mcp-v2.md §11.2/§11.5, formerly tracked here as
			// L-004) — rpcResp.Error's own type (toolsListRPCErrorCode) only
			// ever decodes "code" in the first place, so neither is even
			// present here to embed by mistake. The numeric JSON-RPC code is
			// enough to diagnose from VoidLLM's side.
			return nil, fmt.Errorf("tools/list error: code %d", rpcResp.Error.Code)
		}

		// Checked BEFORE any of this page's tools are appended to allTools or
		// inserted into seenNames: a single page whose own tool count would
		// push the running total above the ceiling must be rejected outright,
		// not partially ingested up to the ceiling and then rejected.
		if len(allTools)+len(rpcResp.Result.Tools) > maxToolsListTools {
			return nil, fmt.Errorf("%w: accumulated %d tools", errToolsListTooManyTools, len(allTools)+len(rpcResp.Result.Tools))
		}
		for _, tool := range rpcResp.Result.Tools {
			if _, dup := seenNames[tool.Name]; dup {
				return nil, errToolsListDuplicateTool
			}
			seenNames[tool.Name] = struct{}{}
			allTools = append(allTools, tool)
		}

		if !result.Cache.TTLMsSet {
			aggNoHint = true
		} else {
			if !aggTTLSet || result.Cache.TTLMs < aggTTLMs {
				aggTTLMs = result.Cache.TTLMs
				aggTTLSet = true
			}
			switch result.Cache.Scope {
			case CacheScopePrivate:
				aggAnyPrivate = true
			case CacheScopePublic:
				aggAnyPublicHint = true
			default:
				// Present hint (TTLMsSet true) but a Scope that is neither
				// public nor private — parseCacheHint's own doc.
				aggAnyUnknownScope = true
			}
		}

		next := rpcResp.Result.NextCursor
		if next == nil {
			break
		}
		if len(*next) > maxToolsListCursorLen {
			return nil, fmt.Errorf("%w: %d bytes", errToolsListCursorTooLong, len(*next))
		}
		if _, dup := seenCursors[*next]; dup {
			return nil, errToolsListRepeatedCursor
		}
		seenCursors[*next] = struct{}{}
		cursor = next
	}

	// See this method's own "CacheHint aggregation" doc above for the full
	// rule table this implements; aggAnyPrivate, aggAnyUnknownScope, and
	// aggAnyPublicHint are mutually exclusive per page (CacheHint.Scope has
	// no fourth value), so at most one of the first two switch cases below
	// ever applies, and the third only when neither did.
	if categories := boolCount(aggAnyPrivate, aggAnyUnknownScope, aggAnyPublicHint); categories > 1 {
		slog.Default().LogAttrs(ctx, slog.LevelWarn,
			"mcp: tools/list pages disagreed on cacheScope, resolving to the most restrictive aggregate",
			slog.String("server_id", t.serverID))
	}

	var cache CacheHint
	switch {
	case aggAnyPrivate:
		// Rule 1: fail closed unconditionally, regardless of anything else —
		// see the rule table's own doc for why this is checked first. TTL is
		// still populated (rule 4) purely for ToolCache.resolveTTL's benefit;
		// persistListing never reads it once Scope is private.
		cache.Scope = CacheScopePrivate
		if aggTTLSet {
			cache.TTLMsSet = true
			cache.TTLMs = aggTTLMs
		}
	case aggAnyUnknownScope:
		// Rule 2: unproven — neither save nor delete — even if another page
		// carried no hint at all. aggTTLSet is guaranteed true here, since
		// this page's own hint is what set aggAnyUnknownScope.
		cache.TTLMsSet = true
		cache.TTLMs = aggTTLMs
		// cache.Scope stays "" — neither public nor private, exactly the
		// shape persistListing already treats as "unproven".
	case aggNoHint:
		// Rule 3, no-hint branch: every page was "no hint" or "public hint",
		// and at least one was "no hint" — the aggregate collapses to no
		// hint at all, still persisted by persistListing's own fallback
		// branch, but with no TTL opinion of its own.
	default:
		// Rule 3, all-hinted branch: every page carried a hint and none of
		// them was private or unknown-scope, so — Scope having no fourth
		// value — every one of them must have been "public hint".
		cache.TTLMsSet = true
		cache.TTLMs = aggTTLMs
		cache.Scope = CacheScopePublic
	}

	kept, headerParams := FilterHeaderParamTools(ctx, t.serverID, allTools)
	return &ToolListing{Tools: kept, HeaderParams: headerParams, Cache: cache}, nil
}

// boolCount returns how many of vs are true, for ListTools' own diagnostic
// log of disagreeing per-page cacheScope categories (see its "CacheHint
// aggregation" doc) — a tiny local helper rather than a one-off inline
// three-way sum, so the log condition reads as "how many categories
// actually appeared" rather than an opaque boolean expression.
func boolCount(vs ...bool) int {
	n := 0
	for _, v := range vs {
		if v {
			n++
		}
	}
	return n
}

// Close releases idle connections held by both underlying HTTP clients —
// client (the buffered Call/ListTools/Warmup path) and streamClient (the
// Forward path). Both currently share the same *http.Transport, so closing
// one's idle connections happens to also affect the other today, but that is
// an implementation detail Close's own contract must not depend on: closing
// both explicitly keeps this correct even if the two clients are ever given
// independent transports.
func (t *HTTPTransport) Close() error {
	t.client.CloseIdleConnections()
	t.streamClient.CloseIdleConnections()
	return nil
}
