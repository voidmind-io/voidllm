package mcp

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
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

// ErrSSENotSupported is returned by probeEra when the upstream MCP server
// uses the deprecated SSE transport (pre 2025-03-26 spec). SSE requires
// a persistent bidirectional connection that VoidLLM does not yet support.
var ErrSSENotSupported = errors.New("server uses deprecated SSE transport (not supported, use Streamable HTTP)")

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
// text/event-stream response down to its first "data:" line, since every
// caller needs the same JSON payload regardless of which content type the
// upstream chose to answer with.
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
		req.Header.Set("Authorization", "Bearer "+t.authToken)
	case "header":
		if t.authHeader != "" {
			req.Header.Set(t.authHeader, t.authToken)
		}
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

	// Limit body reads to 10 MiB to prevent OOM on misbehaving upstream servers.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "text/event-stream") {
		body = extractSSEData(body)
	}

	return &httpResult{status: resp.StatusCode, body: body, header: resp.Header}, nil
}

// extractSSEData pulls the first "data:" line from an SSE response body.
func extractSSEData(body []byte) []byte {
	for _, line := range bytes.Split(body, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data: ")) {
			return bytes.TrimPrefix(line, []byte("data: "))
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			return bytes.TrimPrefix(line, []byte("data:"))
		}
	}
	return body // fallback: return as-is
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
		req.Header.Set("Authorization", "Bearer "+t.authToken)
	case "header":
		if t.authHeader != "" {
			req.Header.Set(t.authHeader, t.authToken)
		}
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

// ListTools sends tools/list to the remote server, parses the returned tool
// definitions, and applies FilterHeaderParamTools to them before returning.
// Session/handshake management, if the resolved era needs any, happens
// transparently inside Call — in the modern era this sends only tools/list,
// with no initialize and no notifications/initialized, matching the
// stateless core (docs/mcp-v2.md §2). ListTools is tool discovery, not a
// call on behalf of any one caller's organization, so it always uses the
// empty SessionScope, isolated from every real org's legacy session on this
// same server.
//
// The x-mcp-header filtering happens here, and only here, because this is
// the sole place a tools/list response becomes VoidLLM's own []Tool shape —
// MCP 2026-07-28 §4.3 requires the exclusion to happen against the
// tools/list RESULT, not at some later consumer of it.
// FilterHeaderParamTools is given t.serverID (the stable server ID this
// transport was constructed with — empty only for the handful of ad-hoc,
// non-persistent probes that pass "" to NewHTTPTransport, e.g. the startup
// SSE probe in cmd/voidllm's app wiring) so an excluded tool's warning log
// line can identify which server it came from.
//
// The returned *ToolListing's Cache field carries the CacheableResult hint
// (MCP 2026-07-28 §5) Call's own Parse already extracted — see CallResult's
// doc. It is not merely carried along on ToolListing for a later change to
// wire up: ToolCache.resolveTTL reads it to decide the ttl and neverExpires
// a fetched entry is cached under, and ToolCache.persistListing reads its
// Scope to decide whether the fetch is written through to the backing store
// at all — see both methods' own docs.
func (t *HTTPTransport) ListTools(ctx context.Context) (*ToolListing, error) {
	rpcReq := Request{
		JSONRPC: "2.0",
		ID:      jsonx.RawMessage(`1`),
		Method:  "tools/list",
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
		return &ToolListing{}, nil
	}

	var rpcResp struct {
		Result struct {
			Tools []Tool `json:"tools"`
		} `json:"result"`
		Error *Error `json:"error"`
	}
	if err := jsonx.Unmarshal(result.Body, &rpcResp); err != nil {
		return nil, fmt.Errorf("decode tools/list response: %w", err)
	}
	if rpcResp.Error != nil {
		// rpcResp.Error.Message is upstream-controlled, free-form text — never
		// embed it in a Go error, which callers log via err.Error() (see
		// docs/mcp-v2.md §11.2/§11.5, formerly tracked here as L-004). The
		// numeric JSON-RPC code is enough to diagnose from VoidLLM's side.
		return nil, fmt.Errorf("tools/list error: code %d", rpcResp.Error.Code)
	}

	kept, headerParams := FilterHeaderParamTools(ctx, t.serverID, rpcResp.Result.Tools)
	return &ToolListing{Tools: kept, HeaderParams: headerParams, Cache: result.Cache}, nil
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
