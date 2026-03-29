package proxy

import (
	"log/slog"
	"sync"
	"time"

	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/pkg/crypto"
)

// resolvedMCPServer holds a pre-resolved, ready-to-use MCP server configuration
// with a persistent HTTP transport.
type resolvedMCPServer struct {
	ID           string
	URL          string
	AuthType     string
	AuthHeader   string
	AuthTokenEnc string             // original encrypted value, used for change detection
	Transport    *mcp.HTTPTransport // persistent, reusable; closed on eviction
}

// MCPTransportCache caches persistent HTTP transports and decrypted auth tokens
// for MCP servers. It eliminates per-request transport creation and AES-256-GCM
// decryption overhead from the proxy hot path. The cache is keyed by server ID.
//
// LoadAll is called at startup and on every periodic cache refresh tick. It
// closes transports for removed or changed servers and opens new ones, so the
// hot-path Get never allocates.
type MCPTransportCache struct {
	mu           sync.RWMutex
	servers      map[string]*resolvedMCPServer // keyed by serverID
	stale        []*mcp.HTTPTransport          // closed at the start of the next LoadAll
	encKey       []byte
	callTimeout  time.Duration
	allowPrivate bool
	log          *slog.Logger
}

// NewMCPTransportCache returns an empty, ready-to-use MCPTransportCache.
// encKey is the AES-256-GCM key used to decrypt stored auth tokens.
// callTimeout is passed to mcp.NewHTTPTransport for each persistent transport;
// a zero value falls back to 30 seconds. allowPrivate disables SSRF protection.
func NewMCPTransportCache(encKey []byte, allowPrivate bool, callTimeout time.Duration, log *slog.Logger) *MCPTransportCache {
	if callTimeout == 0 {
		callTimeout = 30 * time.Second
	}
	return &MCPTransportCache{
		servers:      make(map[string]*resolvedMCPServer),
		encKey:       encKey,
		callTimeout:  callTimeout,
		allowPrivate: allowPrivate,
		log:          log,
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
//   - If an existing entry has the same URL, AuthType, AuthHeader, and
//     encrypted token it is kept unchanged (the transport is reused).
//   - If anything changed the old transport is closed and a new resolved
//     server with a fresh transport is created.
//   - Servers present in the old cache but absent from servers have their
//     transports closed and are evicted.
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

		existing := c.servers[s.ID]

		// Reuse existing entry when nothing that affects transport or auth changed.
		if existing != nil &&
			existing.URL == s.URL &&
			existing.AuthType == s.AuthType &&
			existing.AuthHeader == s.AuthHeader &&
			existing.AuthTokenEnc == encToken {
			next[s.ID] = existing
			continue
		}

		// Config changed — defer close of the old transport until next LoadAll.
		if existing != nil && existing.Transport != nil {
			c.stale = append(c.stale, existing.Transport)
		}

		// Decrypt the auth token once.
		var plainToken string
		if encToken != "" {
			decrypted, err := crypto.DecryptString(encToken, c.encKey, []byte("mcp_server:"+s.ID))
			if err == nil {
				plainToken = decrypted
			} else {
				c.log.Error("mcp transport cache: decrypt auth token",
					slog.String("server_id", s.ID),
					slog.String("error", err.Error()))
			}
		}

		next[s.ID] = &resolvedMCPServer{
			ID:           s.ID,
			URL:          s.URL,
			AuthType:     s.AuthType,
			AuthHeader:   s.AuthHeader,
			AuthTokenEnc: encToken,
			Transport:    mcp.NewHTTPTransport(s.URL, s.AuthType, s.AuthHeader, plainToken, c.callTimeout, c.allowPrivate),
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
	defer c.mu.Unlock()
	if rs, ok := c.servers[serverID]; ok {
		if rs.Transport != nil {
			c.stale = append(c.stale, rs.Transport)
		}
		delete(c.servers, serverID)
	}
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
