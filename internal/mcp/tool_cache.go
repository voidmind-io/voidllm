package mcp

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ToolFetcher retrieves the list of tools from an MCP server identified by
// alias. Implementations typically create an HTTPTransport, send initialize
// + tools/list, and return the parsed tool array.
type ToolFetcher func(ctx context.Context, alias string) ([]Tool, error)

// cacheEntry holds the cached tools for a single MCP server.
type cacheEntry struct {
	tools     []Tool
	fetchedAt time.Time
}

// ToolCache maintains a thread-safe cache of tool schemas from upstream MCP
// servers. Entries are populated lazily on first access and automatically
// refreshed when older than maxAge.
type ToolCache struct {
	mu      sync.RWMutex
	entries map[string]*cacheEntry // keyed by server alias
	fetcher ToolFetcher
	maxAge  time.Duration
}

// NewToolCache creates a ToolCache that uses fetcher to retrieve tool schemas
// and considers entries stale after maxAge. A maxAge of zero means entries
// never expire automatically.
func NewToolCache(fetcher ToolFetcher, maxAge time.Duration) *ToolCache {
	return &ToolCache{
		entries: make(map[string]*cacheEntry),
		fetcher: fetcher,
		maxAge:  maxAge,
	}
}

// isFresh reports whether e is still within maxAge. When maxAge is zero,
// entries never expire and are always considered fresh.
func (tc *ToolCache) isFresh(e *cacheEntry) bool {
	if tc.maxAge == 0 {
		return true
	}
	return time.Since(e.fetchedAt) < tc.maxAge
}

// copyTools returns a shallow copy of the given slice so callers cannot
// mutate the cache's internal state.
func copyTools(src []Tool) []Tool {
	if src == nil {
		return nil
	}
	dst := make([]Tool, len(src))
	copy(dst, src)
	return dst
}

// GetTools returns the cached tools for alias, fetching them from upstream if
// the entry is missing or stale. A double-check pattern ensures that at most
// one fetch per alias is in flight when multiple goroutines request the same
// stale entry concurrently.
func (tc *ToolCache) GetTools(ctx context.Context, alias string) ([]Tool, error) {
	tc.mu.RLock()
	e, ok := tc.entries[alias]
	if ok && tc.isFresh(e) {
		tools := copyTools(e.tools)
		tc.mu.RUnlock()
		return tools, nil
	}
	tc.mu.RUnlock()

	// Entry is missing or stale — upgrade to write lock.
	tc.mu.Lock()
	defer tc.mu.Unlock()

	// Double-check: another goroutine may have fetched while we waited.
	e, ok = tc.entries[alias]
	if ok && tc.isFresh(e) {
		return copyTools(e.tools), nil
	}

	tools, err := tc.fetcher(ctx, alias)
	if err != nil {
		return nil, err
	}
	tc.entries[alias] = &cacheEntry{
		tools:     tools,
		fetchedAt: time.Now(),
	}
	return copyTools(tools), nil
}

// GetAllTools returns a snapshot of all currently cached tool lists keyed by
// server alias. Only entries that are already in the cache are included; no
// upstream fetches are performed.
func (tc *ToolCache) GetAllTools() map[string][]Tool {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	snapshot := make(map[string][]Tool, len(tc.entries))
	for alias, e := range tc.entries {
		snapshot[alias] = copyTools(e.tools)
	}
	return snapshot
}

// RefreshServer forces a re-fetch of the tool list for alias regardless of
// whether the cached entry is still fresh. On fetch failure the existing cache
// entry is preserved and the error is returned.
func (tc *ToolCache) RefreshServer(ctx context.Context, alias string) error {
	tools, err := tc.fetcher(ctx, alias)
	if err != nil {
		return err
	}

	tc.mu.Lock()
	tc.entries[alias] = &cacheEntry{
		tools:     tools,
		fetchedAt: time.Now(),
	}
	tc.mu.Unlock()
	return nil
}

// RefreshAll forces a re-fetch for every alias currently in the cache.
// All aliases are refreshed even when individual fetches fail. The first
// error encountered is returned; subsequent errors are joined with it.
func (tc *ToolCache) RefreshAll(ctx context.Context) error {
	tc.mu.RLock()
	aliases := make([]string, 0, len(tc.entries))
	for alias := range tc.entries {
		aliases = append(aliases, alias)
	}
	tc.mu.RUnlock()

	var firstErr error
	for _, alias := range aliases {
		if err := tc.RefreshServer(ctx, alias); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
	}
	return firstErr
}

// Invalidate removes the cached entry for alias. Subsequent calls to GetTools
// for that alias will trigger a fresh upstream fetch.
func (tc *ToolCache) Invalidate(alias string) {
	tc.mu.Lock()
	delete(tc.entries, alias)
	tc.mu.Unlock()
}

// ToolCount returns the number of tools cached for alias. It returns 0 if the
// alias is not present in the cache; no upstream fetch is performed.
func (tc *ToolCache) ToolCount(alias string) int {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	e, ok := tc.entries[alias]
	if !ok {
		return 0
	}
	return len(e.tools)
}
