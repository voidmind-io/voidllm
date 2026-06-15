package auth

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// ErrTooManyAttempts is returned by LoginThrottle.Allow when a request is
// blocked by either the per-IP rate limit or the per-account lockout.
var ErrTooManyAttempts = errors.New("too many login attempts")

// Package-level constants govern the throttle behaviour. They are intentionally
// not configurable so that operators cannot accidentally weaken brute-force
// protection.
const (
	// loginMaxAttemptsPerIPPerMinute is the maximum number of login attempts
	// allowed from a single client IP within a one-minute fixed window.
	loginMaxAttemptsPerIPPerMinute = 10

	// loginLockoutThreshold is the number of consecutive failed attempts for a
	// single email address that triggers a lockout.
	loginLockoutThreshold = 5

	// loginLockoutDuration is how long an email address remains locked out after
	// reaching loginLockoutThreshold consecutive failures.
	loginLockoutDuration = 15 * time.Minute

	// loginAccountIdleEviction is the minimum time since the last recorded
	// failure before an account entry without an active lockout is removed by
	// EvictStale. Must be >= loginLockoutDuration so that entries whose lockout
	// window has not yet expired are never evicted prematurely.
	loginAccountIdleEviction = 30 * time.Minute
)

// ipEntry tracks login attempts from a single client IP within a fixed
// one-minute window.
type ipEntry struct {
	mu          sync.Mutex
	count       int
	windowStart time.Time
}

// accountEntry tracks consecutive failures for a single email address and the
// time at which a lockout (if any) expires.
type accountEntry struct {
	mu          sync.Mutex
	failures    int
	lockedUntil time.Time
	lastFailure time.Time // zero if no failure has ever been recorded
}

// LoginThrottle protects the login endpoint against brute-force attacks using
// two in-memory mechanisms: a per-IP attempt limit and a per-account lockout
// after consecutive failures. Safe for concurrent use.
type LoginThrottle struct {
	ipCounters sync.Map // normalised IP string → *ipEntry
	accounts   sync.Map // normalised email string → *accountEntry
	// now returns the current time. Defaults to time.Now; overridden in tests.
	now func() time.Time
}

// NewLoginThrottle constructs a LoginThrottle ready for use.
func NewLoginThrottle() *LoginThrottle {
	return &LoginThrottle{now: time.Now}
}

// Allow checks whether a login attempt from ip for email should be permitted.
// It must be called before credential verification. Every call — whether the
// credentials turn out to be valid or not — counts toward the per-IP limit.
//
// Returns ErrTooManyAttempts if the IP has exceeded its per-minute budget or if
// the account is currently locked out. The returned error never reveals whether
// the IP limit or the account lockout triggered the rejection.
func (t *LoginThrottle) Allow(ip, email string) error {
	now := t.now()
	email = normaliseEmail(email)

	// Per-IP check: increment attempt counter for this IP in the current window.
	// Aborted attempts (bad JSON body, missing fields) are counted by the caller
	// if Allow is invoked; callers may choose not to invoke Allow for those cases
	// to keep the early-return path cheap, but the Login handler calls Allow
	// after validating required fields.
	ipE := t.loadOrCreateIP(ip)
	ipE.mu.Lock()
	windowStart := now.Truncate(time.Minute)
	if ipE.windowStart != windowStart {
		// New minute window — reset.
		ipE.windowStart = windowStart
		ipE.count = 0
	}
	ipE.count++
	ipOver := ipE.count > loginMaxAttemptsPerIPPerMinute
	ipE.mu.Unlock()

	if ipOver {
		return ErrTooManyAttempts
	}

	// Per-account check: is the account currently locked out?
	if email == "" {
		return nil
	}
	accE := t.loadOrCreateAccount(email)
	accE.mu.Lock()
	locked := !accE.lockedUntil.IsZero() && now.Before(accE.lockedUntil)
	// Lazy eviction: clear expired lockout so memory is reclaimed over time.
	if !accE.lockedUntil.IsZero() && !locked {
		accE.lockedUntil = time.Time{}
		accE.failures = 0
	}
	accE.mu.Unlock()

	if locked {
		return ErrTooManyAttempts
	}
	return nil
}

// RecordFailure increments the consecutive-failure counter for email. If the
// counter reaches loginLockoutThreshold the account is locked out for
// loginLockoutDuration. Unknown email addresses are tracked the same way as
// known ones to prevent probing via observable timing differences.
func (t *LoginThrottle) RecordFailure(email string) {
	email = normaliseEmail(email)
	if email == "" {
		return
	}
	accE := t.loadOrCreateAccount(email)
	accE.mu.Lock()
	defer accE.mu.Unlock()

	now := t.now()
	accE.failures++
	accE.lastFailure = now
	if accE.failures >= loginLockoutThreshold {
		accE.lockedUntil = now.Add(loginLockoutDuration)
	}
}

// RecordSuccess resets the consecutive-failure counter for email after a
// successful authentication. This ensures that a legitimately authenticated
// user is not locked out by prior failed attempts.
func (t *LoginThrottle) RecordSuccess(email string) {
	email = normaliseEmail(email)
	if email == "" {
		return
	}
	accE := t.loadOrCreateAccount(email)
	accE.mu.Lock()
	defer accE.mu.Unlock()

	accE.failures = 0
	accE.lockedUntil = time.Time{}
}

// EvictStale removes IP window entries whose minute window has expired and
// account entries that are neither locked nor have any recorded failures. It
// should be called periodically (e.g. every 5 minutes via the application
// ticker) to bound memory usage. Entries that are currently being mutated by
// concurrent Allow / RecordFailure calls are not removed.
func (t *LoginThrottle) EvictStale() {
	now := t.now()
	currentWindow := now.Truncate(time.Minute)

	t.ipCounters.Range(func(key, value any) bool {
		e := value.(*ipEntry)
		e.mu.Lock()
		stale := e.windowStart.Before(currentWindow)
		e.mu.Unlock()
		if stale {
			t.ipCounters.Delete(key)
		}
		return true
	})

	t.accounts.Range(func(key, value any) bool {
		e := value.(*accountEntry)
		e.mu.Lock()
		noLock := e.lockedUntil.IsZero() || now.After(e.lockedUntil)
		noFails := e.failures == 0
		// Also evict entries that have partial failures but no active lockout and
		// whose last failure is older than loginAccountIdleEviction. Without this,
		// accounts with 1–4 failures from random probes accumulate indefinitely.
		idle := !e.lastFailure.IsZero() && now.Sub(e.lastFailure) > loginAccountIdleEviction
		e.mu.Unlock()
		if noLock && (noFails || idle) {
			t.accounts.Delete(key)
		}
		return true
	})
}

// loadOrCreateIP retrieves the ipEntry for ip, creating one if absent.
func (t *LoginThrottle) loadOrCreateIP(ip string) *ipEntry {
	if v, ok := t.ipCounters.Load(ip); ok {
		return v.(*ipEntry)
	}
	e := &ipEntry{}
	actual, _ := t.ipCounters.LoadOrStore(ip, e)
	return actual.(*ipEntry)
}

// loadOrCreateAccount retrieves the accountEntry for email, creating one if absent.
func (t *LoginThrottle) loadOrCreateAccount(email string) *accountEntry {
	if v, ok := t.accounts.Load(email); ok {
		return v.(*accountEntry)
	}
	e := &accountEntry{}
	actual, _ := t.accounts.LoadOrStore(email, e)
	return actual.(*accountEntry)
}

// normaliseEmail returns the email in lowercase with surrounding whitespace
// removed. This ensures that "User@Example.com" and "user@example.com" map to
// the same throttle bucket.
func normaliseEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}
