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
// while a stream that genuinely goes silent for idle does. On expiry, onIdle
// is invoked, which tears down the underlying HTTP request's context; the
// transport then fails the blocked Read with a context error and the
// caller's copy loop ends.
//
// idle is stored on the value rather than baked into a package constant
// specifically so a test can construct one with a short duration instead of
// waiting out the production default (config.MCPConfig.StreamIdleTimeout,
// 120s by default).
type idleTimeoutReader struct {
	r           io.ReadCloser
	closeCancel context.CancelFunc
	idle        time.Duration
	timer       *time.Timer
}

// newIdleTimeoutReader returns an idleTimeoutReader wrapping r. The idle
// timer starts immediately, covering the wait for the very first byte —
// matching the wait for any byte after it.
//
// onIdle and closeCancel are two DISTINCT context.CancelFuncs, deliberately
// never conflated even though both must tear down the same underlying
// request: onIdle is invoked from the timer's own goroutine, exactly once,
// if and only if idle genuinely elapses before Close is ever called — this
// is the ONLY thing that ever invokes it. closeCancel is invoked by Close,
// always, regardless of whether the idle timer ever fired, and must never
// carry any "this was an idle timeout" side effect of its own — Close is
// exactly as often the normal, successful end of a stream (fully read, or
// simply no longer needed by its caller) as it is a teardown following an
// idle timeout, and a caller distinguishing the two (HTTPTransport.Listen's
// own idleFired, for instance — see its own doc) must be able to trust that
// onIdle firing is the ONE and ONLY signal "idle" ever means, never also a
// side effect of an ordinary Close. A caller with no such distinction to
// draw (HTTPTransport.Forward) simply passes the same CancelFunc for both —
// calling a context's own CancelFunc more than once is documented as safe
// and a no-op after the first call, so nothing is lost by doing so.
func newIdleTimeoutReader(r io.ReadCloser, idle time.Duration, onIdle, closeCancel context.CancelFunc) *idleTimeoutReader {
	return &idleTimeoutReader{
		r:           r,
		closeCancel: closeCancel,
		idle:        idle,
		timer:       time.AfterFunc(idle, onIdle),
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

// Close stops the idle timer, cancels the streaming request's context via
// the plain closeCancel (see newIdleTimeoutReader's own doc for why this is
// never the same invocation as onIdle firing), and closes the underlying
// body, in that order. This is the single cleanup operation a Forward caller
// needs to perform — see ForwardResult's doc. Safe to call exactly once, as
// io.ReadCloser requires.
func (i *idleTimeoutReader) Close() error {
	i.timer.Stop()
	i.closeCancel()
	return i.r.Close()
}
