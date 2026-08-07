package proxy

import (
	"log/slog"
	"net/http"
	"sync"
	"time"

	apihealth "github.com/voidmind-io/voidllm/internal/api/health"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/pkg/crypto"
)

// mcpClientInfo self-identifies VoidLLM to upstream MCP servers in both
// protocol eras: the legacy initialize handshake's clientInfo field and the
// modern era's params._meta["io.modelcontextprotocol/clientInfo"].
var mcpClientInfo = mcp.ClientInfo{Name: "voidllm", Version: apihealth.Version}

// resolvedMCPServer holds a pre-resolved, ready-to-use MCP server configuration
// with a persistent HTTP transport.
type resolvedMCPServer struct {
	ID                   string
	URL                  string
	AuthType             string
	AuthHeader           string
	AuthTokenEnc         string // original encrypted value, used for change detection
	OAuthClientSecretEnc string // original encrypted value, used for change detection
	ProtocolVersion      string // db.MCPServer.ProtocolVersion, used for change detection
	// OAuthTokenURL, OAuthClientID, and OAuthScopes are used for change
	// detection only, exactly like the fields above — LoadAll must rebuild
	// (not reuse) the transport when any of them changes, since all three
	// feed the mcp.OAuthConfig a reused *mcp.HTTPTransport would otherwise go
	// on using unchanged (docs/mcp-v2.md Fund 8). Unlike AuthTokenEnc and
	// OAuthClientSecretEnc these are never encrypted — the plain values are
	// exactly what mcp.OAuthConfig itself holds — so no decryption or extra
	// storage is involved in tracking them here.
	OAuthTokenURL string
	OAuthClientID string
	OAuthScopes   string
	Transport     *mcp.HTTPTransport // persistent, reusable; closed on eviction
}

// MCPTransportCache caches persistent HTTP transports and decrypted auth tokens
// for MCP servers. It eliminates per-request transport creation and AES-256-GCM
// decryption overhead from the proxy hot path. The cache is keyed by server ID.
//
// LoadAll is called at startup and on every periodic cache refresh tick. It
// closes transports for removed or changed servers and opens new ones, so the
// hot-path Get never allocates.
type MCPTransportCache struct {
	mu                sync.RWMutex
	servers           map[string]*resolvedMCPServer // keyed by serverID
	stale             []*mcp.HTTPTransport          // closed at the start of the next LoadAll
	encKey            []byte
	callTimeout       time.Duration
	streamIdleTimeout time.Duration
	allowPrivate      bool
	log               *slog.Logger
	oauthManager      *mcp.OAuthTokenManager // shared OAuth token manager; always non-nil
}

// NewMCPTransportCache returns an empty, ready-to-use MCPTransportCache.
// encKey is the AES-256-GCM key used to decrypt stored auth tokens and OAuth
// client secrets. callTimeout is passed to mcp.NewHTTPTransport for each
// persistent transport as the buffered-path timeout (Call, probeEra,
// Warmup); a zero value falls back to 30 seconds. streamIdleTimeout is
// passed as the idle timeout for the streaming path (Forward); a zero value
// falls back to 120 seconds — see config.MCPConfig.StreamIdleTimeout.
// allowPrivate disables SSRF protection.
func NewMCPTransportCache(encKey []byte, allowPrivate bool, callTimeout, streamIdleTimeout time.Duration, log *slog.Logger) *MCPTransportCache {
	if callTimeout == 0 {
		callTimeout = 30 * time.Second
	}
	if streamIdleTimeout == 0 {
		streamIdleTimeout = 120 * time.Second
	}
	return &MCPTransportCache{
		servers:           make(map[string]*resolvedMCPServer),
		encKey:            encKey,
		callTimeout:       callTimeout,
		streamIdleTimeout: streamIdleTimeout,
		allowPrivate:      allowPrivate,
		log:               log,
		oauthManager: mcp.NewOAuthTokenManager(&http.Client{
			Timeout:   30 * time.Second,
			Transport: mcp.NewSSRFSafeTransport(allowPrivate),
		}),
	}
}

// Get returns the cached resolved server for the given serverID. Returns the
// resolved server and true when found, or nil and false when the server is not
// in the cache. The returned pointer is owned by the cache — callers must not
// modify it.
func (c *MCPTransportCache) Get(serverID string) (*resolvedMCPServer, bool) {
	c.mu.RLock()
	rs, ok := c.servers[serverID]
	c.mu.RUnlock()
	return rs, ok
}

// LoadAll atomically replaces the cache contents with the supplied server
// slice. For each server:
//   - If an existing entry has the same URL, AuthType, AuthHeader, encrypted
//     token, encrypted OAuth client secret, OAuthTokenURL, OAuthClientID,
//     OAuthScopes, and ProtocolVersion it is kept unchanged (the transport is
//     reused). All of these are the fields that feed either the transport's
//     wire target or its outbound credential — see resolvedMCPServer's own
//     doc for why the three OAuth fields joined this comparison alongside
//     the ones that were already here (docs/mcp-v2.md Fund 8): changing only
//     one of them (say, the token issuer URL) without also changing
//     AuthType/AuthHeader/the encrypted secrets previously left the OLD
//     transport, and its already-cached OAuth token from the OLD issuer,
//     silently in place.
//   - If anything changed the old transport is closed and a new resolved
//     server with a fresh transport is created — UNLESS decrypting either
//     encrypted credential fails, in which case no transport is built for
//     this server at all this cycle (see the decrypt-failure handling
//     below).
//   - Servers present in the old cache but absent from servers have their
//     transports closed and are evicted.
//
// A server whose stored auth token or OAuth client secret cannot be
// decrypted (e.g. after an encryption key rotation) is skipped entirely
// rather than given a transport with an empty credential: Get(id) then
// simply misses for that server, exactly as it would for a server not yet
// loaded into the cache at all, and callers (HandleMCPProxy, CallMCPTool,
// MakeToolFetcher) already fall back to buildAdHocTransport on a cache miss
// (mcp_proxy.go) — which decrypts the SAME stored value and fails the SAME
// way, returning a definite internal-error response to the caller instead of
// silently sending a credential-carrying header with no credential in it
// (docs/mcp-v2.md Fund 3; mirrors buildAdHocTransport's own already-correct
// abort-on-decrypt-failure behavior). Any previously cached transport for
// this server is still evicted via c.stale in this case: continuing to serve
// requests against a transport built from a credential that may since have
// rotated or been revoked would not be an improvement over failing loudly.
//
// Servers without a URL (builtin) are skipped. LoadAll is safe to call
// concurrently with Get and with itself (serialised by the write lock).
func (c *MCPTransportCache) LoadAll(servers []db.MCPServer) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Close transports from the previous eviction cycle. The grace period
	// equals one cache refresh interval, giving in-flight requests time
	// to complete before the transport is torn down.
	for _, t := range c.stale {
		t.Close() //nolint:errcheck
	}
	c.stale = c.stale[:0]

	next := make(map[string]*resolvedMCPServer, len(servers))

	for i := range servers {
		s := &servers[i]

		// Builtin servers have no URL to proxy to.
		if s.URL == "" {
			continue
		}

		encToken := ""
		if s.AuthTokenEnc != nil {
			encToken = *s.AuthTokenEnc
		}
		encOAuthSecret := ""
		if s.OAuthClientSecretEnc != nil {
			encOAuthSecret = *s.OAuthClientSecretEnc
		}

		existing := c.servers[s.ID]

		// Reuse existing entry when nothing that affects transport or auth changed.
		if existing != nil &&
			existing.URL == s.URL &&
			existing.AuthType == s.AuthType &&
			existing.AuthHeader == s.AuthHeader &&
			existing.AuthTokenEnc == encToken &&
			existing.OAuthClientSecretEnc == encOAuthSecret &&
			existing.OAuthTokenURL == s.OAuthTokenURL &&
			existing.OAuthClientID == s.OAuthClientID &&
			existing.OAuthScopes == s.OAuthScopes &&
			existing.ProtocolVersion == s.ProtocolVersion {
			next[s.ID] = existing
			continue
		}

		// Config changed — defer close of the old transport until next LoadAll and
		// evict any cached OAuth token so the next request gets a fresh grant.
		if existing != nil && existing.Transport != nil {
			c.stale = append(c.stale, existing.Transport)
		}
		c.oauthManager.Evict(s.ID)

		// Decrypt the auth token once. A decrypt failure fails this server's
		// entire rebuild closed (see LoadAll's own doc) rather than leaving
		// plainToken at its zero value and proceeding: an empty token would
		// otherwise still be used to build a usable-looking transport that
		// sends an empty credential (docs/mcp-v2.md Fund 3).
		var plainToken string
		if encToken != "" {
			decrypted, err := crypto.DecryptString(encToken, c.encKey, []byte("mcp_server:"+s.ID))
			if err != nil {
				c.log.Error("mcp transport cache: decrypt auth token, server has no usable transport this cycle",
					slog.String("server_id", s.ID),
					slog.String("error", err.Error()))
				continue
			}
			plainToken = decrypted
		}

		// Build optional OAuth config for "oauth" auth type. As with the auth
		// token above, a decrypt failure here fails the whole server closed —
		// never falls back to a nil oauthCfg, which errOAuthNotConfigured is
		// reserved for a genuine "oauth selected but no client secret
		// configured" misconfiguration, not a decrypt failure on a secret
		// that WAS configured.
		var oauthCfg *mcp.OAuthConfig
		if s.AuthType == "oauth" && encOAuthSecret != "" {
			plainSecret, decErr := crypto.DecryptString(encOAuthSecret, c.encKey, []byte("mcp_server:"+s.ID))
			if decErr != nil {
				c.log.Error("mcp transport cache: decrypt oauth client secret, server has no usable transport this cycle",
					slog.String("server_id", s.ID),
					slog.String("error", decErr.Error()))
				continue
			}
			oauthCfg = &mcp.OAuthConfig{
				TokenURL:     s.OAuthTokenURL,
				ServerURL:    s.URL,
				ClientID:     s.OAuthClientID,
				ClientSecret: plainSecret,
				Scopes:       s.OAuthScopes,
			}
		}

		next[s.ID] = &resolvedMCPServer{
			ID:                   s.ID,
			URL:                  s.URL,
			AuthType:             s.AuthType,
			AuthHeader:           s.AuthHeader,
			AuthTokenEnc:         encToken,
			OAuthClientSecretEnc: encOAuthSecret,
			OAuthTokenURL:        s.OAuthTokenURL,
			OAuthClientID:        s.OAuthClientID,
			OAuthScopes:          s.OAuthScopes,
			ProtocolVersion:      s.ProtocolVersion,
			Transport: mcp.NewHTTPTransport(s.URL, s.AuthType, s.AuthHeader, plainToken, c.callTimeout, c.allowPrivate,
				s.ID, c.oauthManager, oauthCfg, mcpClientInfo, mcp.ResolvePinnedVersion(s.ProtocolVersion), c.streamIdleTimeout),
		}
	}

	// Defer close of transports for servers that are no longer present.
	for id, old := range c.servers {
		if _, kept := next[id]; !kept && old.Transport != nil {
			c.stale = append(c.stale, old.Transport)
		}
	}

	c.servers = next
}

// Invalidate moves the transport for serverID to the stale list (closed on the
// next LoadAll or Close call) and removes the entry from the cache. It is a
// no-op when serverID is not cached. Callers must ensure LoadAll runs within a
// bounded time after Invalidate to reclaim the stale transport.
func (c *MCPTransportCache) Invalidate(serverID string) {
	c.mu.Lock()
	if rs, ok := c.servers[serverID]; ok {
		if rs.Transport != nil {
			c.stale = append(c.stale, rs.Transport)
		}
		delete(c.servers, serverID)
	}
	c.mu.Unlock()
	// Evict cached OAuth token outside the lock — Evict takes its own lock.
	c.oauthManager.Evict(serverID)
}

// Close closes all cached and stale transports and empties the cache. It must
// be called once on application shutdown to release idle HTTP connections.
// Callers should stop routing new requests before calling Close — goroutines
// that obtained a transport pointer via Get before Close may encounter
// connection errors on in-flight calls.
func (c *MCPTransportCache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.stale {
		t.Close() //nolint:errcheck
	}
	c.stale = nil
	for _, rs := range c.servers {
		if rs.Transport != nil {
			rs.Transport.Close() //nolint:errcheck
		}
	}
	c.servers = make(map[string]*resolvedMCPServer)
}

// Len returns the number of server entries currently held in the cache. It
// acquires a read lock and is safe to call concurrently with all other methods.
func (c *MCPTransportCache) Len() int {
	c.mu.RLock()
	n := len(c.servers)
	c.mu.RUnlock()
	return n
}

// OAuthManager returns the shared OAuthTokenManager owned by this cache.
// Callers such as the MCP health checker may reuse it to avoid redundant
// token grants for the same server.
func (c *MCPTransportCache) OAuthManager() *mcp.OAuthTokenManager {
	return c.oauthManager
}
