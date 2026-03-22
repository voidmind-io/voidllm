// Package cache provides a generic, concurrent, thread-safe cache backed by
// a plain map protected by a sync.RWMutex. Read operations (Get, Range, Len)
// acquire a shared read lock and do not block each other. Write operations
// (Set, Delete, LoadAll, Clear) acquire an exclusive lock. LoadAll performs an
// atomic map swap under the lock, so revoked keys are never visible to
// concurrent readers after the swap completes.
package cache

import "sync"

// Cache is a typed, concurrent map safe for use by multiple goroutines.
// The zero value is not usable; construct with New.
type Cache[K comparable, V any] struct {
	mu sync.RWMutex
	m  map[K]V
}

// New returns a ready-to-use empty Cache.
func New[K comparable, V any]() *Cache[K, V] {
	return &Cache[K, V]{m: make(map[K]V)}
}

// Get returns the value associated with key and true if found.
// If the key does not exist, the zero value of V and false are returned.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.RLock()
	v, ok := c.m[key]
	c.mu.RUnlock()
	return v, ok
}

// Set stores value under key, overwriting any existing entry.
func (c *Cache[K, V]) Set(key K, value V) {
	c.mu.Lock()
	c.m[key] = value
	c.mu.Unlock()
}

// Delete removes the entry for key. It is a no-op if the key does not exist.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	delete(c.m, key)
	c.mu.Unlock()
}

// LoadAll atomically replaces the entire cache contents with entries.
// After LoadAll returns, only the keys present in entries are visible to
// concurrent readers — previously cached keys not in entries are gone.
func (c *Cache[K, V]) LoadAll(entries map[K]V) {
	c.mu.Lock()
	c.m = entries
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
