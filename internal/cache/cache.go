// Package cache provides a generic, concurrent, thread-safe cache backed by
// a plain map protected by a sync.RWMutex. Read operations (Get, Range, Len)
// acquire a shared read lock and do not block each other. Write operations
// (Set, Delete, LoadAll, Clear, BeginLoad, CommitLoad, AbortLoad) acquire an
// exclusive lock. LoadAll and CommitLoad perform an atomic map swap under the
// lock, so removed keys are never visible to concurrent readers after the swap
// completes.
//
// # Full loads and concurrent point writes
//
// A full load reads a snapshot from a slow source (the database) and then
// replaces the cache contents. A Set or Delete that happens between the read
// and the swap is newer than the snapshot and must win. BeginLoad, CommitLoad
// and AbortLoad implement that: the cache keeps a monotonic generation that is
// advanced by every Set and Delete, a load remembers the generation at which
// it began, and while at least one load is in flight every Set and Delete is
// recorded per key together with its generation. CommitLoad then drops from the
// snapshot every key deleted after the load began and keeps the cached value
// of every key set after the load began.
package cache

import (
	"sync"
	"sync/atomic"
)

// mutation is the most recent point write to a key while a load was in flight.
type mutation struct {
	gen     uint64 // generation assigned to the write
	deleted bool   // true for Delete, false for Set
}

// LoadToken identifies one full load started with BeginLoad. It must be passed
// to exactly one of CommitLoad or AbortLoad; further calls with the same token
// are no-ops, so deferring AbortLoad after a successful CommitLoad is safe.
//
// A token is bound to the cache that issued it: passing it to another cache's
// CommitLoad or AbortLoad is a no-op.
type LoadToken struct {
	owner uint64
	id    uint64
	start uint64
}

// nextCacheID hands out the unique identity bound into every LoadToken.
var nextCacheID atomic.Uint64

// Cache is a typed, concurrent map safe for use by multiple goroutines.
// The zero value is not usable; construct with New.
type Cache[K comparable, V any] struct {
	mu sync.RWMutex
	m  map[K]V

	// id uniquely identifies this cache instance; it is immutable.
	id uint64

	// gen is advanced by every Set and Delete. Guarded by mu.
	gen uint64
	// mutations holds, per key, the latest Set or Delete performed while a
	// load was in flight. It is empty whenever no load is in flight and is
	// pruned as loads finish. Guarded by mu.
	mutations map[K]mutation
	// active maps the ID of every load in flight to the generation at which
	// it began. Guarded by mu.
	active map[uint64]uint64
	// nextLoadID is the ID handed to the next BeginLoad. Guarded by mu.
	nextLoadID uint64
	// committed reports whether any load has been committed, and
	// committedID is the ID of the most recently begun load among them. A
	// load begun earlier than that is superseded and must not be installed.
	// Guarded by mu.
	committed   bool
	committedID uint64
}

// New returns a ready-to-use empty Cache.
func New[K comparable, V any]() *Cache[K, V] {
	return &Cache[K, V]{
		id:        nextCacheID.Add(1),
		m:         make(map[K]V),
		mutations: make(map[K]mutation),
		active:    make(map[uint64]uint64),
	}
}

// Get returns the value associated with key and true if found.
// If the key does not exist, the zero value of V and false are returned.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.RLock()
	v, ok := c.m[key]
	c.mu.RUnlock()
	return v, ok
}

// Set stores value under key, overwriting any existing entry. A load that is
// in flight when Set is called keeps this value instead of its own snapshot.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	c.m[key] = value
	c.record(key, false)
	c.mu.Unlock()
}

// Delete removes the entry for key. It is a no-op if the key does not exist.
// A load that is in flight when Delete is called does not bring the key back.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	delete(c.m, key)
	c.record(key, true)
	c.mu.Unlock()
}

// record advances the generation and, while a load is in flight, remembers the
// write so CommitLoad can apply it on top of the load's snapshot. With no load
// in flight nothing is stored: a load that begins later reads its source after
// this write. The caller must hold c.mu.
func (c *Cache[K, V]) record(key K, deleted bool) {
	c.gen++
	if len(c.active) > 0 {
		c.mutations[key] = mutation{gen: c.gen, deleted: deleted}
	}
}

// BeginLoad starts a full load and must be called before the source is read.
// The returned token must be passed to CommitLoad (to install the snapshot) or
// AbortLoad (when the read failed); until then every Set and Delete is
// recorded, so a load that is never finished leaks one record per written key.
func (c *Cache[K, V]) BeginLoad() LoadToken {
	c.mu.Lock()
	c.nextLoadID++
	t := LoadToken{owner: c.id, id: c.nextLoadID, start: c.gen}
	c.active[t.id] = t.start
	c.mu.Unlock()
	return t
}

// CommitLoad atomically replaces the cache contents with entries, a snapshot
// read after BeginLoad returned t, merged with the point writes made since:
// a key deleted after the load began is not installed, and a key set after the
// load began keeps its cached value. The cache takes ownership of entries.
//
// CommitLoad reports whether the snapshot was installed. It is not installed
// when t was already committed or aborted, or when a load that began after t
// has been committed in the meantime (that snapshot is newer).
func (c *Cache[K, V]) CommitLoad(t LoadToken, entries map[K]V) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if t.owner != c.id {
		return false
	}
	if _, ok := c.active[t.id]; !ok {
		return false
	}
	if c.committed && t.id <= c.committedID {
		c.finish(t)
		return false
	}

	if entries == nil {
		entries = make(map[K]V)
	}
	for k, mut := range c.mutations {
		if mut.gen <= t.start {
			continue
		}
		if mut.deleted {
			delete(entries, k)
			continue
		}
		if v, ok := c.m[k]; ok {
			entries[k] = v
		}
	}
	c.m = entries
	c.committed = true
	c.committedID = t.id
	c.finish(t)
	return true
}

// AbortLoad ends a load without changing the cache contents. It is a no-op
// when t was already committed or aborted.
func (c *Cache[K, V]) AbortLoad(t LoadToken) {
	c.mu.Lock()
	if _, ok := c.active[t.id]; ok && t.owner == c.id {
		c.finish(t)
	}
	c.mu.Unlock()
}

// finish removes t from the loads in flight and prunes the recorded writes
// that no remaining load can still need: all of them when no load is left,
// otherwise those not newer than the oldest remaining load's start. The caller
// must hold c.mu.
func (c *Cache[K, V]) finish(t LoadToken) {
	delete(c.active, t.id)
	if len(c.active) == 0 {
		clear(c.mutations)
		return
	}
	oldest := c.gen
	for _, start := range c.active {
		if start < oldest {
			oldest = start
		}
	}
	for k, mut := range c.mutations {
		if mut.gen <= oldest {
			delete(c.mutations, k)
		}
	}
}

// LoadAll atomically replaces the entire cache contents with entries.
// After LoadAll returns, only the keys present in entries are visible to
// concurrent readers — previously cached keys not in entries are gone.
//
// LoadAll installs entries unconditionally. When entries is a snapshot read
// from a slow source while other goroutines Set or Delete keys, use BeginLoad
// and CommitLoad instead so those writes are not undone.
//
// LoadAll counts as the latest commit: every load in flight (begun before
// LoadAll) is superseded and its CommitLoad returns false, so an older snapshot
// can never override the entries installed here.
func (c *Cache[K, V]) LoadAll(entries map[K]V) {
	c.mu.Lock()
	c.m = entries
	c.gen++
	c.committed = true
	c.committedID = c.nextLoadID
	c.mu.Unlock()
}

// Len returns the number of entries currently held in the cache.
func (c *Cache[K, V]) Len() int {
	c.mu.RLock()
	n := len(c.m)
	c.mu.RUnlock()
	return n
}

// Clear removes all entries from the cache.
func (c *Cache[K, V]) Clear() {
	c.mu.Lock()
	c.m = make(map[K]V)
	c.mu.Unlock()
}

// Range calls fn for each key-value pair in the cache in no particular order.
// Iteration stops if fn returns false. The read lock is held for the duration
// of the iteration; fn must not call any Cache methods.
func (c *Cache[K, V]) Range(fn func(key K, value V) bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for k, v := range c.m {
		if !fn(k, v) {
			break
		}
	}
}
