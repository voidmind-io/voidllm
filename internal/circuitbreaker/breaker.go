// Package circuitbreaker provides a per-model circuit breaker that prevents
// cascading failures when an upstream LLM provider becomes unhealthy. Each
// model endpoint gets its own independent Breaker instance, managed by the
// Registry. The implementation follows the classic three-state machine:
// Closed → Open → HalfOpen → Closed.
package circuitbreaker

import (
	"sync"
	"time"
)

// State represents the operating state of a circuit breaker.
type State int

const (
	// Closed is the normal operating state. All requests pass through.
	Closed State = iota
	// Open means the circuit is tripped. Requests are rejected immediately
	// without contacting the upstream to prevent cascading failures.
	Open
	// HalfOpen allows a limited number of probe requests to test whether the
	// upstream has recovered. A successful probe closes the circuit; a failed
	// probe reopens it immediately.
	HalfOpen
)

// String returns a human-readable name for the state.
func (s State) String() string {
	switch s {
	case Closed:
		return "closed"
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Config holds circuit breaker configuration parameters. A zero-valued Config
// is valid but disables the feature (Enabled defaults to false).
type Config struct {
	// Enabled activates circuit breaker functionality. When false, the Registry
	// still exists but all Breaker instances always allow requests through.
	Enabled bool `yaml:"enabled"`
	// Threshold is the number of consecutive failures required before the
	// circuit transitions from Closed to Open. Defaults to 5.
	Threshold int `yaml:"threshold"`
	// Timeout is how long the circuit stays Open before transitioning to
	// HalfOpen to probe for recovery. Defaults to 30 seconds.
	Timeout time.Duration `yaml:"timeout"`
	// HalfOpenMax is the maximum number of concurrent probe requests allowed
	// while the circuit is in HalfOpen state. Defaults to 1.
	HalfOpenMax int `yaml:"half_open_max"`
}

// Breaker tracks the health of a single upstream model/provider endpoint.
// All state transitions are guarded by a mutex so the Breaker is safe for
// concurrent use by multiple goroutines on the hot path.
type Breaker struct {
	mu             sync.Mutex
	state          State
	failures       int
	lastFailure    time.Time
	threshold      int
	timeout        time.Duration
	halfOpenMax    int
	halfOpenActive int
}

// NewBreaker creates a Breaker with the given configuration. The circuit starts
// in the Closed state (normal operation).
func NewBreaker(cfg Config) *Breaker {
	return &Breaker{
		state:       Closed,
		threshold:   cfg.Threshold,
		timeout:     cfg.Timeout,
		halfOpenMax: cfg.HalfOpenMax,
	}
}

// Allow reports whether a request should be allowed to proceed to the upstream.
//
//   - Closed: always returns true.
//   - Open: checks if the recovery timeout has elapsed; if so, transitions to
//     HalfOpen and counts this request against halfOpenMax, returning true.
//     Otherwise returns false (circuit still tripped).
//   - HalfOpen: allows up to halfOpenMax concurrent probe requests; additional
//     requests return false until an in-flight probe completes.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		return true

	case Open:
		if time.Since(b.lastFailure) < b.timeout {
			return false
		}
		// Timeout has elapsed — transition to HalfOpen and admit this request
		// as the first probe.
		b.state = HalfOpen
		b.halfOpenActive = 1
		return true

	case HalfOpen:
		if b.halfOpenActive >= b.halfOpenMax {
			return false
		}
		b.halfOpenActive++
		return true

	default:
		return true
	}
}

// RecordSuccess records a successful upstream call. In HalfOpen state this
// transitions the circuit back to Closed and resets the failure counter.
// In Closed state it decrements the in-flight counter (no-op for failures).
func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case HalfOpen:
		b.state = Closed
		b.failures = 0
		if b.halfOpenActive > 0 {
			b.halfOpenActive--
		}
	case Closed:
		b.failures = 0
	}
}

// RecordFailure records a failed upstream call and advances the state machine
// as appropriate:
//
//   - Closed: increments the consecutive failure counter; if failures reaches
//     threshold, transitions to Open and records the trip timestamp.
//   - HalfOpen: transitions back to Open immediately, regardless of threshold,
//     and records a new trip timestamp so the recovery timer restarts.
//   - Open: no-op — the original trip timestamp is preserved so the recovery
//     timer is not pushed forward by late-arriving failures.
func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case Closed:
		b.failures++
		if b.failures >= b.threshold {
			b.state = Open
			b.lastFailure = time.Now()
		}

	case HalfOpen:
		b.state = Open
		b.lastFailure = time.Now()
		if b.halfOpenActive > 0 {
			b.halfOpenActive--
		}

	case Open:
		// No-op: let the original trip timestamp stand so the recovery timer
		// is not pushed forward by late-arriving failures.
	}
}

// CurrentState returns the current circuit state. It is safe to call from
// multiple goroutines.
func (b *Breaker) CurrentState() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}
