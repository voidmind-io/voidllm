package proxy

import "sync"

// ModelAccessCache is a concurrency-safe in-memory store of org/team/key model
// allowlists. It is the authoritative source for model access decisions on the
// hot path — no database queries are made during a proxy request.
//
// A nil map entry for a given ID means that scope is unconfigured, which is
// semantically equivalent to "all models allowed at this level". A non-nil but
// empty map means the allowlist was explicitly set to empty (same result).
// A non-nil, non-empty map means only the listed models are allowed.
type ModelAccessCache struct {
	mu        sync.RWMutex
	orgAllow  map[string]map[string]bool // orgID  → set of allowed model names
	teamAllow map[string]map[string]bool // teamID → set of allowed model names
	keyAllow  map[string]map[string]bool // keyID  → set of allowed model names
}

// NewModelAccessCache returns an empty, ready-to-use ModelAccessCache.
func NewModelAccessCache() *ModelAccessCache {
	return &ModelAccessCache{
		orgAllow:  make(map[string]map[string]bool),
		teamAllow: make(map[string]map[string]bool),
		keyAllow:  make(map[string]map[string]bool),
	}
}

// Load atomically replaces all cached allowlists with the provided data.
// The maps passed in are the raw string-slice form returned by
// DB.LoadAllModelAccess: a nil or empty slice means "unconfigured".
// Load is safe to call from any goroutine but must not be called concurrently
// with itself.
func (c *ModelAccessCache) Load(
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

// Check reports whether modelName is accessible given the org, team, and key
// identifiers. The most-restrictive-wins rule is applied: org → team → key.
// A scope that has no entry (or an empty allowlist) is treated as unconfigured
// and passes all models through at that level. teamID may be empty for keys
// that are not scoped to a team.
func (c *ModelAccessCache) Check(orgID, teamID, keyID, modelName string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if orgSet, ok := c.orgAllow[orgID]; ok && len(orgSet) > 0 {
		if !orgSet[modelName] {
			return false
		}
	}

	if teamID != "" {
		if teamSet, ok := c.teamAllow[teamID]; ok && len(teamSet) > 0 {
			if !teamSet[modelName] {
				return false
			}
		}
	}

	if keySet, ok := c.keyAllow[keyID]; ok && len(keySet) > 0 {
		if !keySet[modelName] {
			return false
		}
	}

	return true
}

// Len returns the total number of scoped allowlist entries across org, team,
// and key dimensions. It acquires a read lock so it is safe to call
// concurrently with Load and Check.
func (c *ModelAccessCache) Len() int {
	c.mu.RLock()
	n := len(c.orgAllow) + len(c.teamAllow) + len(c.keyAllow)
	c.mu.RUnlock()
	return n
}

// toSetMap converts a map of string slices into a map of string sets (bool
// maps). IDs whose slices are nil or empty are stored as nil entries so that
// Check can distinguish between "unconfigured" and "explicitly empty allowlist"
// — both are treated identically (pass-through), but storing nil avoids
// allocating an empty map per unconfigured entity.
func toSetMap(in map[string][]string) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(in))
	for id, names := range in {
		if len(names) == 0 {
			out[id] = nil
			continue
		}
		set := make(map[string]bool, len(names))
		for _, n := range names {
			set[n] = true
		}
		out[id] = set
	}
	return out
}
