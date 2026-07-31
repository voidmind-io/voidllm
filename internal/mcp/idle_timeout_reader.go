package mcp

import (
	"context"
	"io"
	"time"
)

// idleTimeoutReader wraps an upstream response body for HTTPTransport.Forward,
// enforcing an IDLE timeout instead of a total-duration one. The timer is
// reset on every read that returns without error, so a long-lived stream
// that keeps producing bytes — including the periodic SSE keep-alive comment
// lines MCP Streamable HTTP recommends servers send on a long-lived
// subscriptions/listen response (docs/mcp-v2.md §3.4/§4.1) — never trips it,
// while a stream that genuinely goes silent for idle does. On expiry, cancel
// is invoked, which tears down the underlying HTTP request's context; the
// transport then fails the blocked Read with a context error and the
// caller's copy loop ends.
//
// idle is stored on the value rather than baked into a package constant
// specifically so a test can construct one with a short duration instead of
// waiting out the production default (config.MCPConfig.StreamIdleTimeout,
// 120s by default).
type idleTimeoutReader struct {
	r      io.ReadCloser
	cancel context.CancelFunc
	idle   time.Duration
	timer  *time.Timer
}

// newIdleTimeoutReader returns an idleTimeoutReader wrapping r. The idle
// timer starts immediately, covering the wait for the very first byte —
// matching the wait for any byte after it. cancel is invoked from the
// timer's own goroutine if idle elapses before Close is called; it must tear
// down whatever request produced r.
func newIdleTimeoutReader(r io.ReadCloser, idle time.Duration, cancel context.CancelFunc) *idleTimeoutReader {
	return &idleTimeoutReader{
		r:      r,
		cancel: cancel,
		idle:   idle,
		timer:  time.AfterFunc(idle, cancel),
	}
}

// Read implements io.Reader. A read that returns at least one byte without
// error resets the idle timer; a zero-byte, no-error read — permitted by
// io.Reader's contract — leaves the timer alone, since no actual progress
// was made and resetting would let a upstream that returns (0, nil) forever
// defeat the idle timeout entirely. A read that returns an error (including
// a context cancellation caused by the timer itself firing) likewise leaves
// the timer alone — there is nothing left to reset for, and Close will stop
// it shortly.
func (i *idleTimeoutReader) Read(p []byte) (int, error) {
	n, err := i.r.Read(p)
	if err == nil && n > 0 {
		i.timer.Reset(i.idle)
	}
	return n, err
}

// Close stops the idle timer, cancels the streaming request's context, and
// closes the underlying body, in that order. This is the single cleanup
// operation a Forward caller needs to perform — see ForwardResult's doc.
// Safe to call exactly once, as io.ReadCloser requires.
func (i *idleTimeoutReader) Close() error {
	i.timer.Stop()
	i.cancel()
	return i.r.Close()
}
