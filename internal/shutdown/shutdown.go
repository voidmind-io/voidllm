// Package shutdown coordinates graceful shutdown of the VoidLLM server by
// tracking in-flight requests and providing an orderly drain sequence before
// the process exits.
package shutdown

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// State coordinates graceful shutdown with in-flight request tracking.
// The zero value is not usable; construct one with New.
type State struct {
	draining atomic.Bool
	inflight atomic.Int64
	wg       sync.WaitGroup
	parent   context.Context
	cancel   context.CancelFunc
}

// New creates a State whose parent context is derived from context.Background.
// Upstream request contexts should be derived from ParentCtx so that
// CancelInflight can abort them all at once during a forced shutdown.
func New() *State {
	ctx, cancel := context.WithCancel(context.Background())
	return &State{
		parent: ctx,
		cancel: cancel,
	}
}

// TrackStart records that a new request has started. Every call to TrackStart
// must be paired with exactly one call to TrackDone when the request finishes.
func (s *State) TrackStart() {
	s.wg.Add(1)
	s.inflight.Add(1)
}

// TrackDone records that a tracked request has finished. It must be called
// exactly once for each prior call to TrackStart.
func (s *State) TrackDone() {
	s.inflight.Add(-1)
	s.wg.Done()
}

// ParentCtx returns the parent context from which upstream request contexts
// should be derived. Calling CancelInflight cancels this context, which
// propagates cancellation to all derived contexts.
func (s *State) ParentCtx() context.Context {
	return s.parent
}

// BeginDrain signals that the server is entering the drain phase. Once set,
// the draining flag is never cleared. Callers such as readiness probes can
// observe this to stop routing new requests to the instance.
func (s *State) BeginDrain() {
	s.draining.Store(true)
}

// Draining reports whether BeginDrain has been called.
func (s *State) Draining() bool {
	return s.draining.Load()
}

// InFlight returns the number of requests currently being tracked.
func (s *State) InFlight() int64 {
	return s.inflight.Load()
}

// WaitForDrain waits for all tracked requests to finish or for timeout to
// elapse, whichever comes first. It returns true if all requests drained
// before the timeout and false if the timeout was reached with requests still
// in flight.
func (s *State) WaitForDrain(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// CancelInflight cancels the parent context, propagating cancellation to every
// upstream request context derived from ParentCtx. This is called as a last
// resort when WaitForDrain times out.
func (s *State) CancelInflight() {
	s.cancel()
}
