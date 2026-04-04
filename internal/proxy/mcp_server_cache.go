package proxy

import (
	"sync"

	"github.com/voidmind-io/voidllm/internal/db"
)

// MCPServerCache caches active MCP server records to avoid DB lookups on
// every proxy request. The cache is refreshed periodically via LoadAll.
//
// Keys are formatted as "alias:orgID:teamID" where orgID and teamID are empty
// strings for absent scopes. Get applies scope priority matching:
// team-scoped > org-scoped > global.
type MCPServerCache struct {
	mu      sync.RWMutex
	servers map[string]*db.MCPServer // keyed by "alias:orgID:teamID"
}

// NewMCPServerCache returns an empty, ready-to-use MCPServerCache.
func NewMCPServerCache() *MCPServerCache {
	return &MCPServerCache{
		servers: make(map[string]*db.MCPServer),
	}
}

// Get resolves an active MCP server by alias using scope priority:
// team-scoped (highest) → org-scoped → global (lowest). orgID and teamID
// may each be an empty string to indicate absence of that scope.
// It returns the server and true if found, or nil and false otherwise.
func (c *MCPServerCache) Get(alias, orgID, teamID string) (*db.MCPServer, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// 1. Try team-scoped first (highest priority).
	if teamID != "" && orgID != "" {
		if s, ok := c.servers[alias+":"+orgID+":"+teamID]; ok {
			return s, true
		}
	}

	// 2. Try org-scoped.
	if orgID != "" {
		if s, ok := c.servers[alias+":"+orgID+":"]; ok {
			return s, true
		}
	}

	// 3. Try global (no org, no team).
	if s, ok := c.servers[alias+"::"]; ok {
		return s, true
	}

	return nil, false
}

// LoadAll atomically replaces all cached server records with the provided
// slice. It is safe to call from any goroutine but must not be called
// concurrently with itself.
func (c *MCPServerCache) LoadAll(servers []db.MCPServer) {
	next := make(map[string]*db.MCPServer, len(servers))
	for i := range servers {
		s := &servers[i]
		key := mcpServerCacheKey(s)
		next[key] = s
	}

	c.mu.Lock()
	c.servers = next
	c.mu.Unlock()
}

// GetByID looks up a cached server by its database ID. It iterates all entries
// under a read lock. The cache is small (typically fewer than 20 entries) so
// linear scan is acceptable. Returns the server and true if found.
func (c *MCPServerCache) GetByID(serverID string) (*db.MCPServer, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, s := range c.servers {
		if s.ID == serverID {
			cpy := *s
			return &cpy, true
		}
	}
	return nil, false
}

// Len returns the number of server records currently held in the cache.
// It acquires a read lock so it is safe to call concurrently with LoadAll and Get.
func (c *MCPServerCache) Len() int {
	c.mu.RLock()
	n := len(c.servers)
	c.mu.RUnlock()
	return n
}

// List returns a snapshot of all server records currently held in the cache.
// The returned slice contains value copies; callers may safely read the entries
// without holding any lock. The order of entries is unspecified.
func (c *MCPServerCache) List() []db.MCPServer {
	c.mu.RLock()
	out := make([]db.MCPServer, 0, len(c.servers))
	for _, s := range c.servers {
		out = append(out, *s)
	}
	c.mu.RUnlock()
	return out
}

// mcpServerCacheKey builds the lookup key for an MCPServer record.
// Key format is "alias:orgID:teamID" with empty strings for absent scopes.
func mcpServerCacheKey(s *db.MCPServer) string {
	orgID := ""
	if s.OrgID != nil {
		orgID = *s.OrgID
	}
	teamID := ""
	if s.TeamID != nil {
		teamID = *s.TeamID
	}
	return s.Alias + ":" + orgID + ":" + teamID
}
