package ratelimit

import (
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// ErrRateLimitExceeded is returned by CheckRate when a rate limit is exceeded.
var ErrRateLimitExceeded = errors.New("rate limit exceeded")

// counterEntry holds an atomic request counter and the start of its current
// sliding window, both stored as Unix seconds. mu is held only during the
// window rollover path; the common case (same window) is entirely lock-free.
type counterEntry struct {
	count       atomic.Int64
	windowStart atomic.Int64
	mu          sync.Mutex
}

// Compile-time assertion: RateLimiter must implement Checker.
var _ Checker = (*RateLimiter)(nil)

// RateLimiter enforces requests-per-minute and requests-per-day limits using
// in-memory atomic counters. It never calls the database.
type RateLimiter struct {
	minuteCounters sync.Map // scope string → *counterEntry
	dayCounters    sync.Map // scope string → *counterEntry
}

// NewRateLimiter constructs a RateLimiter ready for use.
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{}
}

// CheckRate verifies rate limits for the key, team (if non-empty), and org scopes.
// Effective limits are computed as the most-restrictive across all three scopes.
// Counters are incremented atomically on success via a CAS loop; if any scope
// limit is exceeded the request is rejected. Scopes that were already incremented
// when a later scope fails are not rolled back — the over-count by 1 is
// self-correcting at the next window reset.
// Returns ErrRateLimitExceeded if any limit is exceeded.
func (r *RateLimiter) CheckRate(keyID, teamID, orgID string, keyLimits, teamLimits, orgLimits Limits) error {
	effective := EffectiveLimits(keyLimits, teamLimits, orgLimits)

	now := time.Now().UTC()
	minuteWindow := now.Truncate(time.Minute).Unix()
	dayWindow := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()

	// Collect scope identifiers. Team scope is only included when the key is
	// actually team-scoped so that per-team counters do not accumulate for keys
	// that have no team.
	scopes := make([]string, 0, 3)
	scopes = append(scopes, "key:"+keyID)
	if teamID != "" {
		scopes = append(scopes, "team:"+teamID)
	}
	scopes = append(scopes, "org:"+orgID)

	if effective.RequestsPerMinute > 0 {
		limit := int64(effective.RequestsPerMinute)
		for _, scope := range scopes {
			entry := r.loadOrCreate(&r.minuteCounters, scope)
			if !r.tryIncrement(entry, minuteWindow, limit) {
				return ErrRateLimitExceeded
			}
		}
	}

	if effective.RequestsPerDay > 0 {
		limit := int64(effective.RequestsPerDay)
		for _, scope := range scopes {
			entry := r.loadOrCreate(&r.dayCounters, scope)
			if !r.tryIncrement(entry, dayWindow, limit) {
				return ErrRateLimitExceeded
			}
		}
	}

	return nil
}

// EvictStale removes counter entries whose window has expired. It should be
// called periodically (e.g. every 5 minutes) to reclaim memory for keys that
// are no longer active.
func (r *RateLimiter) EvictStale() {
	now := time.Now().UTC()
	minuteWindow := now.Truncate(time.Minute).Unix()
	dayWindow := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Unix()

	r.minuteCounters.Range(func(key, value any) bool {
		if value.(*counterEntry).windowStart.Load() < minuteWindow {
			r.minuteCounters.Delete(key)
		}
		return true
	})
	r.dayCounters.Range(func(key, value any) bool {
		if value.(*counterEntry).windowStart.Load() < dayWindow {
			r.dayCounters.Delete(key)
		}
		return true
	})
}

// loadOrCreate retrieves an existing counterEntry for the given scope key, or
// stores and returns a newly allocated one. The sync.Map value is always a
// *counterEntry.
func (r *RateLimiter) loadOrCreate(m *sync.Map, scope string) *counterEntry {
	if v, ok := m.Load(scope); ok {
		return v.(*counterEntry)
	}
	entry := &counterEntry{}
	actual, _ := m.LoadOrStore(scope, entry)
	return actual.(*counterEntry)
}

// tryIncrement atomically checks the limit and increments the counter using a
// CAS loop. Returns true if the request is allowed, false if the limit is
// exceeded.
//
// The common path (same window) is entirely lock-free: it reads the current
// count and CAS-increments it. The rollover path takes entry.mu to ensure that
// windowStart and count are updated atomically with respect to other rollovers
// — without the lock a goroutine that wins the windowStart CAS can have its
// count.Store(1) overwritten by a second goroutine that wins the count CAS
// before the Store completes.
func (r *RateLimiter) tryIncrement(entry *counterEntry, currentWindow int64, limit int64) bool {
	for {
		stored := entry.windowStart.Load()
		if stored == currentWindow {
			// Same window — lock-free CAS increment.
			cur := entry.count.Load()
			if cur >= limit {
				return false
			}
			if entry.count.CompareAndSwap(cur, cur+1) {
				return true
			}
			// A concurrent increment raced with us; retry.
			continue
		}

		// Window has rolled over — take the lock so that windowStart and the
		// reset count are updated as a single visible transition.
		entry.mu.Lock()
		if entry.windowStart.Load() != currentWindow {
			// We are still the first to roll over inside the lock.
			entry.windowStart.Store(currentWindow)
			entry.count.Store(1)
			entry.mu.Unlock()
			return true
		}
		// Another goroutine already rolled the window while we were acquiring
		// the lock. Fall through to the same-window path.
		entry.mu.Unlock()
	}
}
