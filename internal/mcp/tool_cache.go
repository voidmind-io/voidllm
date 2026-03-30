package mcp

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ToolStore persists and retrieves tool schemas from a backing store (typically
// a database). When non-nil, the ToolCache writes through on every fetch and
// loads from the store at startup for zero-HTTP-call warm starts.
type ToolStore interface {
	// LoadAll returns all cached tool schemas grouped by server ID.
	LoadAll(ctx context.Context) (map[string][]Tool, error)
	// Save persists the tool schemas for a server, replacing any previous entry.
	// serverID is the database ID of the MCP server.
	Save(ctx context.Context, serverID string, tools []Tool) error
	// Delete removes all cached tool schemas for a server. serverID is the
	// database ID, used because alias-based lookups fail after soft-delete.
	Delete(ctx context.Context, serverID string) error
}

// ToolFetcher retrieves the list of tools from an MCP server identified by
// serverID. Implementations typically create an HTTPTransport, send initialize
// + tools/list, and return the parsed tool array.
type ToolFetcher func(ctx context.Context, serverID string) ([]Tool, error)

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
	entries map[string]*cacheEntry // keyed by server ID
	fetcher ToolFetcher
	maxAge  time.Duration
	store   ToolStore // optional, nil for pure in-memory
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

// NewPersistentToolCache creates a ToolCache backed by a persistent store.
// Tools are written through to the store on every fetch and can be loaded
// from the store at startup via LoadFromStore.
func NewPersistentToolCache(fetcher ToolFetcher, maxAge time.Duration, store ToolStore) *ToolCache {
	return &ToolCache{
		entries: make(map[string]*cacheEntry),
		fetcher: fetcher,
		maxAge:  maxAge,
		store:   store,
	}
}

// LoadFromStore populates the in-memory cache from the backing store.
// Call once at startup before serving requests. Returns nil if the store
// is nil or empty.
func (tc *ToolCache) LoadFromStore(ctx context.Context) error {
	if tc.store == nil {
		return nil
	}
	all, err := tc.store.LoadAll(ctx)
	if err != nil {
		return err
	}
	tc.mu.Lock()
	defer tc.mu.Unlock()
	for serverID, tools := range all {
		// Set fetchedAt to zero so entries loaded from the DB are considered
		// stale on first access. This ensures tools are refreshed from upstream
		// within maxAge of startup, while still providing immediate availability
		// for TypeScript type generation and list_servers tool counts.
		tc.entries[serverID] = &cacheEntry{
			tools:     tools,
			fetchedAt: time.Time{},
		}
	}
	return nil
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

// GetTools returns the cached tools for serverID, fetching them from upstream
// if the entry is missing or stale. A double-check pattern ensures that at most
// one fetch per serverID is in flight when multiple goroutines request the same
// stale entry concurrently.
func (tc *ToolCache) GetTools(ctx context.Context, serverID string) ([]Tool, error) {
	tc.mu.RLock()
	e, ok := tc.entries[serverID]
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
	e, ok = tc.entries[serverID]
	if ok && tc.isFresh(e) {
		return copyTools(e.tools), nil
	}

	tools, err := tc.fetcher(ctx, serverID)
	if err != nil {
		return nil, err
	}
	tc.entries[serverID] = &cacheEntry{
		tools:     tools,
		fetchedAt: time.Now(),
	}
	if tc.store != nil {
		_ = tc.store.Save(ctx, serverID, tools) //nolint:errcheck
	}
	return copyTools(tools), nil
}

// GetAllTools returns a snapshot of all currently cached tool lists keyed by
// server ID. Only entries that are already in the cache are included; no
// upstream fetches are performed.
func (tc *ToolCache) GetAllTools() map[string][]Tool {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	snapshot := make(map[string][]Tool, len(tc.entries))
	for serverID, e := range tc.entries {
		snapshot[serverID] = copyTools(e.tools)
	}
	return snapshot
}

// RefreshServer forces a re-fetch of the tool list for serverID regardless of
// whether the cached entry is still fresh. On fetch failure the existing cache
// entry is preserved and the error is returned.
func (tc *ToolCache) RefreshServer(ctx context.Context, serverID string) error {
	tools, err := tc.fetcher(ctx, serverID)
	if err != nil {
		return err
	}

	tc.mu.Lock()
	tc.entries[serverID] = &cacheEntry{
		tools:     tools,
		fetchedAt: time.Now(),
	}
	tc.mu.Unlock()

	if tc.store != nil {
		_ = tc.store.Save(ctx, serverID, tools) //nolint:errcheck
	}
	return nil
}

// RefreshAll forces a re-fetch for every server ID currently in the cache.
// All entries are refreshed even when individual fetches fail. The first
// error encountered is returned; subsequent errors are joined with it.
func (tc *ToolCache) RefreshAll(ctx context.Context) error {
	tc.mu.RLock()
	serverIDs := make([]string, 0, len(tc.entries))
	for serverID := range tc.entries {
		serverIDs = append(serverIDs, serverID)
	}
	tc.mu.RUnlock()

	var firstErr error
	for _, serverID := range serverIDs {
		if err := tc.RefreshServer(ctx, serverID); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
	}
	return firstErr
}

// SetTools manually populates the cache for serverID with the given tools.
// Used for the built-in server whose tools come from memory, not HTTP.
// The entry is marked fresh at the current time; it will not be re-fetched
// from upstream until maxAge has elapsed.
func (tc *ToolCache) SetTools(serverID string, tools []Tool) {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	tc.entries[serverID] = &cacheEntry{
		tools:     tools,
		fetchedAt: time.Now(),
	}
}

// Invalidate removes the cached entry for serverID. Subsequent calls to
// GetTools for that serverID will trigger a fresh upstream fetch.
func (tc *ToolCache) Invalidate(serverID string) {
	tc.mu.Lock()
	delete(tc.entries, serverID)
	tc.mu.Unlock()
}

// InvalidateWithStore removes a server from the cache and deletes its
// persisted tools from the backing store. serverID is the database ID used
// both as the cache key and to address the store deletion.
func (tc *ToolCache) InvalidateWithStore(ctx context.Context, serverID string) {
	tc.mu.Lock()
	delete(tc.entries, serverID)
	tc.mu.Unlock()
	if tc.store != nil {
		_ = tc.store.Delete(ctx, serverID) //nolint:errcheck
	}
}

// FreshFor returns how long the cache entry for serverID has been fresh,
// measured from the time the entry was last fetched. It returns -1 if the
// serverID is not present in the cache. The caller can use this to enforce a
// cooldown between forced refreshes.
func (tc *ToolCache) FreshFor(serverID string) time.Duration {
	tc.mu.RLock()
	defer tc.mu.RUnlock()
	entry, ok := tc.entries[serverID]
	if !ok {
		return -1
	}
	return time.Since(entry.fetchedAt)
}

// ToolCount returns the number of tools cached for serverID. It returns 0 if
// serverID is not present in the cache; no upstream fetch is performed.
func (tc *ToolCache) ToolCount(serverID string) int {
	tc.mu.RLock()
	defer tc.mu.RUnlock()

	e, ok := tc.entries[serverID]
	if !ok {
		return 0
	}
	return len(e.tools)
}
