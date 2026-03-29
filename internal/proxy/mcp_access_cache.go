package proxy

import "sync"

// MCPAccessCache caches MCP server access control lists (org/team/key
// allowlists). Mirrors the ModelAccessCache pattern.
//
// Access policy (most-restrictive-wins, org → team → key):
//   - Org level is CLOSED-by-default: a missing or empty org entry denies access
//     to global MCP servers. The org must explicitly list a server ID to grant access.
//   - Team level is OPEN-by-default: a missing or empty team entry does not restrict
//     access further (inherits from org). A non-empty team set acts as an additional
//     restriction.
//   - Key level is OPEN-by-default: same inheritance rule as the team level.
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
//
// The org level is CLOSED-by-default: if the org has no configured allowlist,
// or serverID is not in it, access is denied. The team and key levels are
// OPEN-by-default: they only restrict access when a non-empty allowlist is
// present and serverID is absent from it.
//
// teamID may be empty for keys that are not scoped to a team.
func (c *MCPAccessCache) Check(orgID, teamID, keyID, serverID string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	// Org level: CLOSED-by-default — deny when unconfigured or not in the set.
	orgSet, orgConfigured := c.orgAllow[orgID]
	if !orgConfigured || len(orgSet) == 0 || !orgSet[serverID] {
		return false
	}

	// Team level: OPEN-by-default — only restrict when explicitly configured.
	if teamID != "" {
		if teamSet, ok := c.teamAllow[teamID]; ok && len(teamSet) > 0 {
			if !teamSet[serverID] {
				return false
			}
		}
	}

	// Key level: OPEN-by-default — only restrict when explicitly configured.
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
