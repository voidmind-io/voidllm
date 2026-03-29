package proxy

import "sync"

// MCPAccessCache caches MCP server access control lists (org/team/key
// allowlists). Mirrors the ModelAccessCache pattern.
//
// A nil map entry for a given ID means that scope is unconfigured, which is
// semantically equivalent to "all servers allowed at this level". A non-nil
// but empty map means the allowlist was explicitly set to empty (same result).
// A non-nil, non-empty map means only the listed servers are allowed.
type MCPAccessCache struct {
	mu        sync.RWMutex
	orgAllow  map[string]map[string]bool // orgID  → serverID → true
	teamAllow map[string]map[string]bool // teamID → serverID → true
	keyAllow  map[string]map[string]bool // keyID  → serverID → true
}

// NewMCPAccessCache returns an empty, ready-to-use MCPAccessCache.
func NewMCPAccessCache() *MCPAccessCache {
	return &MCPAccessCache{
		orgAllow:  make(map[string]map[string]bool),
		teamAllow: make(map[string]map[string]bool),
		keyAllow:  make(map[string]map[string]bool),
	}
}

// Load atomically replaces all cached allowlists with the provided data.
// The maps passed in are the raw string-slice form returned by
// DB.LoadAllMCPAccess: a nil or empty slice means "unconfigured".
// Load is safe to call from any goroutine but must not be called concurrently
// with itself.
func (c *MCPAccessCache) Load(
	orgAccess map[string][]string,
	teamAccess map[string][]string,
	keyAccess map[string][]string,
) {
	org := toSetMap(orgAccess)
	team := toSetMap(teamAccess)
	key := toSetMap(keyAccess)

	c.mu.Lock()
	c.orgAllow = org
	c.teamAllow = team
	c.keyAllow = key
	c.mu.Unlock()
}

// Check reports whether serverID is accessible given the org, team, and key
// identifiers. The most-restrictive-wins rule is applied: org → team → key.
// A scope that has no entry (or an empty allowlist) is treated as unconfigured
// and passes all servers through at that level. teamID may be empty for keys
// that are not scoped to a team.
func (c *MCPAccessCache) Check(orgID, teamID, keyID, serverID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if orgSet, ok := c.orgAllow[orgID]; ok && len(orgSet) > 0 {
		if !orgSet[serverID] {
			return false
		}
	}

	if teamID != "" {
		if teamSet, ok := c.teamAllow[teamID]; ok && len(teamSet) > 0 {
			if !teamSet[serverID] {
				return false
			}
		}
	}

	if keySet, ok := c.keyAllow[keyID]; ok && len(keySet) > 0 {
		if !keySet[serverID] {
			return false
		}
	}

	return true
}

// Len returns the total number of scoped allowlist entries across org, team,
// and key dimensions. It acquires a read lock so it is safe to call
// concurrently with Load and Check.
func (c *MCPAccessCache) Len() int {
	c.mu.RLock()
	n := len(c.orgAllow) + len(c.teamAllow) + len(c.keyAllow)
	c.mu.RUnlock()
	return n
}
