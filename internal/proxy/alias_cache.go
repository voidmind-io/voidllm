package proxy

import "sync"

// AliasCache is a concurrency-safe in-memory store of org- and team-scoped model aliases.
// It is the authoritative source for alias resolution on the hot path — no database
// queries are made during a proxy request.
//
// Resolution order is team first, then org. If neither scope defines the alias the
// caller receives ("", false) and must treat the name as a literal canonical model name.
type AliasCache struct {
	mu          sync.RWMutex
	orgAliases  map[string]map[string]string // orgID  → alias → canonical model name
	teamAliases map[string]map[string]string // teamID → alias → canonical model name
}

// NewAliasCache returns an empty, ready-to-use AliasCache.
func NewAliasCache() *AliasCache {
	return &AliasCache{
		orgAliases:  make(map[string]map[string]string),
		teamAliases: make(map[string]map[string]string),
	}
}

// Resolve looks up alias within the given org and team scope and returns the
// canonical model name. Team aliases are checked before org aliases so that a
// team-level override takes precedence. Returns ("", false) if the alias is not
// defined in either scope.
func (c *AliasCache) Resolve(orgID, teamID, alias string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if teamID != "" {
		if aliases, ok := c.teamAliases[teamID]; ok {
			if canonical, ok := aliases[alias]; ok {
				return canonical, true
			}
		}
	}

	if aliases, ok := c.orgAliases[orgID]; ok {
		if canonical, ok := aliases[alias]; ok {
			return canonical, true
		}
	}

	return "", false
}

// Len returns the total number of alias mappings across org and team scopes.
// It counts the individual alias entries within each scope map, not the number
// of scopes. It acquires a read lock so it is safe to call concurrently with
// Load and Resolve.
func (c *AliasCache) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	n := 0
	for _, aliases := range c.orgAliases {
		n += len(aliases)
	}
	for _, aliases := range c.teamAliases {
		n += len(aliases)
	}
	return n
}

// Load atomically replaces all cached aliases with the provided data.
// The maps are the nested form returned by DB.LoadAllModelAliases:
// orgAliases is orgID → alias → canonical name, teamAliases is teamID → alias → canonical name.
// Load is safe to call from any goroutine but must not be called concurrently with itself.
func (c *AliasCache) Load(orgAliases, teamAliases map[string]map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.orgAliases = orgAliases
	c.teamAliases = teamAliases
}
