package mcp

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"
)

// listenMinBackoff, listenMaxBackoff, listenUnsupportedRetry, and
// listenReconnectJitterMax are runListener's reconnect-policy timing
// constants — see that function's own doc for exactly what each governs.
// They are package-level vars, not consts, purely so export_test.go can
// override them for a test that needs to drive the full reconnect state
// machine without waiting out real production durations (in particular
// listenUnsupportedRetry's 1h and listenMaxBackoff's 5m); production code
// never mutates them after startup, and no ListenManager is ever
// constructed concurrently with a test overriding these, so a plain var
// (rather than an atomic) is sufficient here — the same discipline
// bindingProbeErrorTTL-adjacent test hooks elsewhere in this package already
// follow.
var (
	listenMinBackoff         = 1 * time.Second
	listenMaxBackoff         = 5 * time.Minute
	listenUnsupportedRetry   = 1 * time.Hour
	listenReconnectJitterMax = 1 * time.Second
)

// listenAckHookForTest, when non-nil, is called synchronously at the very
// end of runListener's own onAck closure — after the backoff reset and the
// conditional notifyToolsChanged call — so a test can be notified exactly
// when one acknowledgement's handling has fully completed, rather than only
// when the underlying HTTP request arrived (which says nothing about
// whether this package's own onAck-driven state changes, e.g. a
// notifyToolsChanged call, have already happened). Package-level and
// atomic, following the exact discipline SetSharedFetchFreshEntryHookForTest
// (tool_cache.go/export_test.go) already establishes for a single
// package-level test hook shared by every instance in the test binary: a
// test that sets it must always clear it again, and must not run
// concurrently with any other test that also sets it.
var listenAckHookForTest atomic.Pointer[func(serverID string)]

// listenExitDelayHookForTest, when non-nil, is called by runListener
// immediately before it returns because ctx has ended (via exitIfCtxDone),
// and may block for as long as it likes. In production this window between
// "ctx was cancelled" and "this goroutine has fully exited (its done
// channel closes)" is ordinarily sub-millisecond — nothing about
// Reconcile's own "wait for the old listener" contract (see start's own
// doc) depends on it being any longer — but that also makes the ordering
// itself hard to observe deterministically in a test using only realistic
// HTTP timing. This hook exists purely so a test can widen that window on
// demand, synchronizing on (and optionally delaying) the exact moment a
// superseded listener is about to exit, without changing production
// behavior at all when unset (nil). Same single-package-level-hook
// discipline as listenAckHookForTest.
var listenExitDelayHookForTest atomic.Pointer[func()]

// listenThrottleInterval bounds how often ListenManager invokes its own
// onToolsChanged callback for a single server: at most once per this
// duration — see listenThrottle's own doc for the coalescing behavior this
// enforces. A package-level var, not a const, purely so export_test.go can
// override it for a test that needs to observe several throttle windows
// without waiting out the real production interval — the identical
// reasoning listenMinBackoff and its siblings already document; no
// ListenManager is ever constructed concurrently with a test overriding it.
var listenThrottleInterval = 1 * time.Second

// ListenTarget names one upstream MCP server ListenManager should hold a
// subscriptions/listen stream open against, and the *HTTPTransport already
// resolved for it — the same one the proxy hot path uses (see
// proxy.MCPTransportCache), never one ListenManager constructs itself.
type ListenTarget struct {
	// ServerID is the stable database ID identifying this server across
	// Reconcile calls — the key ListenManager tracks its per-target
	// goroutine and reconnect state under.
	ServerID string
	// Transport is the auth-configured, persistent transport for this
	// server. A Reconcile call naming the same ServerID with a DIFFERENT
	// Transport pointer than the one currently running restarts that
	// target's listener against the new pointer — see Reconcile's own doc.
	Transport *HTTPTransport
}

// listenerHandle is ListenManager's per-target bookkeeping: the goroutine's
// own cancel function — so a superseded or removed target can be torn down
// independently of every other — the Transport pointer Reconcile most
// recently started it against, used to detect a changed pointer for the
// same ServerID, and done, closed exactly once the goroutine itself (and
// everything it owns — see start's own doc) has fully exited, which a
// REPLACEMENT listener for the same ServerID waits on before ever sending
// its own first request (see Reconcile's own doc, "transport change waits
// for the old listener").
type listenerHandle struct {
	cancel    context.CancelFunc
	transport *HTTPTransport
	done      <-chan struct{}
}

// ListenManager holds one subscriptions/listen stream open per modern-era
// upstream MCP server that has Code Mode's ToolCache enabled, reconnecting
// automatically per runListener's own reconnect policy, and invalidating
// ToolCache's cached listing for a server (via the onToolsChanged callback
// given to NewListenManager) whenever that upstream reports its tools
// changed, or whenever a reconnect makes it possible that such a change was
// missed while the listener was down — see runListener's own doc for the
// exact triggers. Every call to onToolsChanged for a given server passes
// through that server's own listenThrottle first (see start's own doc): at
// most one call per listenThrottleInterval reaches the caller-supplied
// callback, with a burst's LAST call always eventually delivered rather than
// dropped.
//
// Reconcile and Stop may be called from any goroutine, including
// concurrently with each other and with themselves: every mutation of this
// type's own state (the listeners map and stopped flag) happens while
// holding mu, which serializes them completely — see the race test
// alongside this file's own tests for a direct proof under -race. The only
// unsynchronized step either performs is the final wg.Wait() in Stop, which
// blocks (outside the lock) until every goroutine already recorded under the
// lock has exited; this is exactly the same "mutate under the lock, wait
// outside it" shape every other *Cache's LoadAll/Reconcile method in this
// codebase already follows.
//
// Every VoidLLM instance in a multi-instance deployment runs its own
// ListenManager and holds its own stream per upstream: there is no
// Redis-backed coordination to elect a single listener across instances
// (this repo's v0.1 single-instance-without-Redis constraint,
// docs/development.md, still applies — nothing here changes it). This is
// deliberately harmless, not merely tolerated: every instance's ToolCache is
// itself a purely local, in-memory cache, so each instance invalidating its
// OWN cache in response to its OWN copy of the upstream's notification
// stream is exactly the isolation this codebase already assumes for every
// other in-memory cache — an upstream simply answers as many concurrent
// subscriptions/listen streams as it has instances calling it, precisely as
// it would answer any other number of concurrent clients.
type ListenManager struct {
	onToolsChanged func(serverID string)

	mu        sync.Mutex
	listeners map[string]*listenerHandle
	stopped   bool
	wg        sync.WaitGroup
}

// NewListenManager returns a ready-to-use ListenManager with no listeners
// running yet — call Reconcile to start them. onToolsChanged is called
// synchronously, from whichever listener goroutine observed the change or
// reconnect, once per qualifying event (subject to that server's own
// listenThrottle — see ListenManager's own doc); it must not block for long
// (see HTTPTransport.Listen's identical constraint on its own
// onToolsChanged parameter).
func NewListenManager(onToolsChanged func(serverID string)) *ListenManager {
	return &ListenManager{
		onToolsChanged: onToolsChanged,
		listeners:      make(map[string]*listenerHandle),
	}
}

// Reconcile starts a listener goroutine for every target in targets that
// does not already have one running against the identical *HTTPTransport
// pointer, restarts (cancels the old goroutine, starts a new one) a
// listener whose target's Transport pointer has changed since the last
// Reconcile, and cancels every currently running listener whose ServerID no
// longer appears in targets at all. Each listener owns its own
// context.Background()-derived context, independent of any one
// *HTTPTransport's own lifetime — HTTPTransport.Close does not cancel a
// stream Listen is reading (see Forward's identical independence) — so only
// Reconcile (superseding or removing a target) or Stop ever ends one.
//
// A transport-pointer change does not merely cancel the old goroutine and
// immediately start a new one racing against its teardown: the new
// listener's very first connection attempt waits for the OLD goroutine to
// have fully exited (see start's own doc for exactly what that entails), so
// there is never a moment where two listeners for the same ServerID are
// both connected — or one is connecting while the other is still tearing
// down — at once. That wait itself respects the NEW listener's own context:
// if this same target is superseded or removed again before the old
// goroutine finishes exiting, the new listener abandons the wait (and so
// never sends a request at all) instead of blocking forever.
//
// A no-op once Stop has been called: Stop is a one-way shutdown, matching
// every other Stop method in this codebase (e.g. RuntimePool.Close).
func (m *ListenManager) Reconcile(targets []ListenTarget) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return
	}

	seen := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		seen[target.ServerID] = struct{}{}
		var waitFor <-chan struct{}
		if existing, ok := m.listeners[target.ServerID]; ok {
			if existing.transport == target.Transport {
				continue
			}
			existing.cancel()
			waitFor = existing.done
		}
		m.start(target, waitFor)
	}

	for serverID, existing := range m.listeners {
		if _, ok := seen[serverID]; !ok {
			existing.cancel()
			delete(m.listeners, serverID)
		}
	}
}

// start launches the listener goroutine for target and records its handle.
// waitFor, if non-nil, is the PREVIOUS listener's own done channel for this
// exact ServerID (see Reconcile's "transport change" doc): the new
// goroutine waits for it to close before ever calling transport.Listen, so a
// transport-pointer change never lets two listeners for the same server run
// concurrently.
//
// This goroutine's own done channel — the one a LATER Reconcile call may in
// turn hand to some THIRD listener as ITS waitFor — closes only once BOTH
// this goroutine's own work has finished AND (if waitFor is non-nil) waitFor
// itself has closed, never merely whichever of the two happens first. This
// matters specifically for a supersession CHAIN: if this listener (call it
// B, waiting on a predecessor A's done) is itself cancelled by a Reconcile
// that replaces it with a successor C before A has actually exited, B does
// not abandon the wait for A the instant its own ctx ends — it keeps waiting
// for A's done to close (never calling transport.Listen at all, since ctx is
// already done) before closing its OWN done. Only once that has happened
// does C's own wait on B's done unblock. Without this, B closing its done
// the instant its ctx was cancelled — before A had genuinely exited — would
// let C start connecting while A might still be connected, exactly the
// "two listeners for the same server at once" race Reconcile's own doc says
// can never happen. The wait for waitFor in this cancelled-early branch is
// bounded: A was already cancelled by the earlier Reconcile call that
// created B in the first place (see Reconcile's own doc), so A is already
// unwinding by the time B's own ctx could ever have been cancelled by a
// later Reconcile call superseding B.
//
// Every call to onToolsChanged this target's listener would otherwise make
// — both transport.Listen's own onToolsChanged callback and runListener's
// post-ack invalidation (see that function's own doc) — is routed through a
// single per-target listenThrottle instead, so both triggers share one
// coalescing window rather than each bypassing the other's throttle state.
// The throttle is stopped (releasing its own timer) as the goroutine exits,
// before done closes and before m.wg.Done is recorded — see listenThrottle's
// own Stop doc for why this ordering leaves no timer able to fire, and so no
// call to onToolsChanged able to happen, after that point.
//
// Callers must already hold m.mu.
func (m *ListenManager) start(target ListenTarget, waitFor <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	throttle := newListenThrottle(func() { m.onToolsChanged(target.ServerID) })
	m.listeners[target.ServerID] = &listenerHandle{cancel: cancel, transport: target.Transport, done: done}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer close(done)
		defer throttle.Stop()

		if waitFor != nil {
			select {
			case <-waitFor:
			case <-ctx.Done():
				// This listener was itself superseded or removed before its
				// own predecessor (waitFor) ever exited. Still wait for
				// waitFor — see this function's own doc for why closing done
				// any earlier would let a THIRD listener, chained onto this
				// one via a later Reconcile call, start before the ORIGINAL
				// predecessor has genuinely exited.
				<-waitFor
				return
			}
		}
		runListener(ctx, target.ServerID, target.Transport, throttle.Call)
	}()
}

// Stop cancels every currently running listener goroutine and blocks until
// all of them have returned. After Stop returns, Reconcile is a permanent
// no-op. Safe to call at most once — like every other one-way Stop in this
// codebase, a second call is not a supported usage.
func (m *ListenManager) Stop() {
	m.mu.Lock()
	m.stopped = true
	for _, existing := range m.listeners {
		existing.cancel()
	}
	m.listeners = make(map[string]*listenerHandle)
	m.mu.Unlock()

	m.wg.Wait()
}

// runListener drives target's subscriptions/listen stream for serverID
// until ctx is cancelled (by Stop, or by a Reconcile that superseded or
// removed this target), calling transport.Listen in a loop and applying the
// reconnect policy below between attempts. Every wait — including the 1h
// and jitter ones — selects on ctx, so a cancellation interrupts it
// immediately rather than only once it elapses. notifyToolsChanged is
// already bound to serverID and already routed through this target's own
// listenThrottle (see start's own doc) — every call site below simply
// invokes it directly, with no further per-call argument or throttling of
// its own.
//
// Acknowledgement handling: transport.Listen calls the onAck closure built
// below exactly once per connection attempt that reaches a valid
// acknowledgement (see Listen's own doc for exactly which of its return
// values that does and does not imply). onAck is what — not
// transport.Listen's return value — drives both of the "on ack" behaviors
// this function's own reconnect policy depends on, since the return value
// alone cannot distinguish "this attempt never acknowledged at all" from
// "this attempt acknowledged, then failed later" for any bucket other than
// the unambiguous nil/ErrListenIdle one (see the reconnect-policy doc
// below):
//
//   - Every acknowledgement resets the backoff state below to its floor
//     (listenMinBackoff), synchronously, the instant it happens — not only
//     once transport.Listen eventually returns. If the same connection
//     attempt then goes on to fail anyway (idle, or any other error), the
//     wait this function computes for THAT failure already starts from the
//     reset floor rather than from wherever backoff happened to be before
//     this attempt acknowledged.
//   - Every acknowledgement EXCEPT the very first one this goroutine ever
//     observes calls notifyToolsChanged: a change on the upstream could have
//     happened during the gap since this goroutine last held an
//     acknowledged stream open, and there is no way to know without asking
//     again — regardless of how that PREVIOUS acknowledged stream ended
//     (gracefully, by idle timeout, or by disconnecting mid-stream with
//     some other error) or how many further, never-acknowledged connection
//     attempts (a connection refused, a TLS failure, ...) came and went in
//     between. firstAck itself only ever flips on a genuine acknowledgement,
//     never on an attempt that failed before reaching one, so it tracks
//     exactly "has THIS goroutine ever actually held an acknowledged stream
//     before", which is the only thing that determines whether a gap with
//     something to miss could have existed at all.
//
// Reconnect policy, keyed on transport.Listen's own return contract (see
// Listen's own doc) for the WAIT applied before the next attempt — backbone
// state (notifyToolsChanged/backoff-reset) is already handled by onAck above,
// independent of this switch:
//
//   - nil (graceful end) or ErrListenIdle: reconnect after a random
//     0-listenReconnectJitterMax jitter — no backoff: neither outcome is a
//     failure needing one, it is simply time to ask again.
//   - ErrListenUnsupported or ErrListenNotHonored: this upstream does not
//     (or will not) speak subscriptions/listen the way VoidLLM needs at
//     all — retrying rapidly would only hammer a server that has already
//     answered this question. Wait listenUnsupportedRetry (1h) before
//     trying again, in case the upstream is redeployed with support in the
//     meantime, and leave the exponential backoff state below untouched —
//     this bucket is orthogonal to it.
//   - Any other error: wait with exponential backoff — starting at
//     listenMinBackoff, doubling each consecutive time this specific bucket
//     is hit, capped at listenMaxBackoff, with full jitter applied — before
//     retrying. An error in this bucket for an attempt that never
//     acknowledged at all keeps doubling the same backoff state a previous
//     such attempt already grew; one for an attempt that DID acknowledge
//     before failing starts this wait from the floor instead, since onAck
//     already reset it moments earlier — this is what "error returns
//     without a preceding ack keep exponential backoff [growth]" means in
//     practice: only a run of attempts that never once acknowledged
//     actually accumulates a growing wait.
//
// Logging is debug for an ordinary connect/ack/end cycle and warn for a
// failure, both carrying only server_id and — for a failure — a fixed,
// static reason string, never transport.Listen's own error text: that text
// may itself wrap an upstream-adjacent Go error (a TLS failure, a DNS
// failure), but this package's zero-knowledge-logging rule applies uniformly
// regardless of how unlikely any one specific wrapped error is to actually
// carry response content — the discipline is "never log it", not "log it
// unless a human reviewer is confident it is safe this time".
func runListener(ctx context.Context, serverID string, transport *HTTPTransport, notifyToolsChanged func()) {
	logger := slog.Default()
	backoff := listenMinBackoff
	firstAck := true

	onAck := func() {
		logger.LogAttrs(ctx, slog.LevelDebug, "mcp listen: acknowledged",
			slog.String("server_id", serverID))
		backoff = listenMinBackoff
		if !firstAck {
			notifyToolsChanged()
		}
		firstAck = false
		if hook := listenAckHookForTest.Load(); hook != nil {
			(*hook)(serverID)
		}
	}

	// exitIfCtxDone reports whether ctx has already ended, and if so, gives
	// listenExitDelayHookForTest a chance to run — see that hook's own doc —
	// before this function returns. Every one of runListener's own return
	// points reachable while ctx has ended goes through this, so a test
	// installing that hook can deterministically observe (and, if it
	// chooses, delay) the exact moment this goroutine is about to exit.
	exitIfCtxDone := func() bool {
		if ctx.Err() == nil {
			return false
		}
		if hook := listenExitDelayHookForTest.Load(); hook != nil {
			(*hook)()
		}
		return true
	}

	for {
		if exitIfCtxDone() {
			return
		}

		logger.LogAttrs(ctx, slog.LevelDebug, "mcp listen: connecting",
			slog.String("server_id", serverID))
		err := transport.Listen(ctx, onAck, notifyToolsChanged)

		if exitIfCtxDone() {
			return
		}

		switch {
		case err == nil || errors.Is(err, ErrListenIdle):
			logger.LogAttrs(ctx, slog.LevelDebug, "mcp listen: stream ended, reconnecting",
				slog.String("server_id", serverID))
			if !sleepListen(ctx, randDuration(listenReconnectJitterMax)) {
				return
			}
		case errors.Is(err, ErrListenUnsupported), errors.Is(err, ErrListenNotHonored):
			logger.LogAttrs(ctx, slog.LevelDebug, "mcp listen: upstream does not support or honor subscriptions/listen",
				slog.String("server_id", serverID))
			if !sleepListen(ctx, listenUnsupportedRetry) {
				return
			}
		default:
			logger.LogAttrs(ctx, slog.LevelWarn, "mcp listen: stream failed",
				slog.String("server_id", serverID),
				slog.String("reason", "transport error"))
			wait := randDuration(backoff)
			backoff = min(backoff*2, listenMaxBackoff)
			if !sleepListen(ctx, wait) {
				return
			}
		}
	}
}

// sleepListen waits for d, or until ctx is cancelled, whichever comes first,
// reporting false in the latter case so runListener's caller can return
// immediately instead of proceeding to another connection attempt.
func sleepListen(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// randDuration returns a random duration in [0, max) — the "full jitter"
// building block runListener applies both to its 0-1s reconnect wait and to
// its exponential backoff wait (where max is the current, already-capped
// backoff value) — unless a test has overridden the jitter source via
// SetListenJitterFuncForTest (export_test.go), in which case that function
// is called instead, with no further randomization: this is what lets a test
// assert an EXACT wait duration for a given backoff bound instead of only a
// looser "roughly grows" trend. max <= 0 always returns 0 (the override is
// never consulted in that case either), since rand.N panics for a
// non-positive bound.
func randDuration(maxD time.Duration) time.Duration {
	if maxD <= 0 {
		return 0
	}
	if fn := listenJitterFuncForTest.Load(); fn != nil {
		return (*fn)(maxD)
	}
	return time.Duration(rand.Int64N(int64(maxD)))
}

// listenJitterFuncForTest, when non-nil, replaces randDuration's own
// rand.Int64N draw — see randDuration's own doc. Package-level and atomic,
// following the exact single-hook discipline listenAckHookForTest's own doc
// documents.
var listenJitterFuncForTest atomic.Pointer[func(time.Duration) time.Duration]

// listenThrottle coalesces bursts of calls to fire into at most one call per
// listenThrottleInterval, per instance, with the LAST call in any burst
// always eventually delivered — a trailing-edge call — rather than dropped:
// a plain leading-edge-only throttle would silently lose whatever change
// arrived last in a burst that landed inside the current window, which is
// exactly the class of tools-changed notification ToolCache's own
// invalidation must not miss. Every ListenManager target owns exactly one
// listenThrottle (see start's own doc), shared by every trigger that would
// otherwise call onToolsChanged for that server — transport.Listen's own
// notification callback and runListener's post-ack invalidation alike.
//
// Safe for concurrent use: Call may run on the listener's own goroutine, and
// the trailing-edge fire it can schedule runs on a time.AfterFunc timer's
// own goroutine — both are serialized by mu.
type listenThrottle struct {
	fire func()

	mu      sync.Mutex
	pending bool
	timer   *time.Timer
	stopped bool
}

// newListenThrottle returns a listenThrottle that calls fire, subject to the
// throttle described in listenThrottle's own doc. fire must not block for
// long — the same constraint every onToolsChanged-adjacent callback in this
// package already documents.
func newListenThrottle(fire func()) *listenThrottle {
	return &listenThrottle{fire: fire}
}

// Call requests fire be invoked, subject to the throttle. If no window is
// currently open, fire runs synchronously, immediately, on the caller's own
// goroutine (a leading-edge call), and a new listenThrottleInterval window
// opens. If a window is already open, this call is coalesced: fire will run
// exactly once more, from the window's own closing timer, unless a further
// call arrives before then (in which case that one is what ends up
// delivered) or Stop is called first (in which case none of them are).
// A no-op after Stop.
func (lt *listenThrottle) Call() {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if lt.stopped {
		return
	}
	if lt.timer == nil {
		lt.fire()
		lt.timer = time.AfterFunc(listenThrottleInterval, lt.onWindowEnd)
		return
	}
	lt.pending = true
}

// onWindowEnd runs on the window-closing timer's own goroutine once
// listenThrottleInterval has elapsed since the window's most recent
// leading-edge (or trailing-edge) fire. If a call was coalesced during the
// window, fire runs once more (the trailing edge) and a new window opens
// immediately to keep coalescing whatever arrives next; otherwise the
// window simply closes, and the NEXT Call becomes a fresh leading edge.
func (lt *listenThrottle) onWindowEnd() {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	if lt.stopped {
		return
	}
	if lt.pending {
		lt.pending = false
		lt.fire()
		lt.timer = time.AfterFunc(listenThrottleInterval, lt.onWindowEnd)
		return
	}
	lt.timer = nil
}

// Stop permanently disables this listenThrottle: no further call to fire
// will ever happen, whether from Call's own leading edge or from a
// trailing-edge timer already scheduled. If onWindowEnd is concurrently
// executing fire when Stop is called, Stop blocks until that call returns
// (both hold the same mu) — so by the time Stop itself returns, fire is
// guaranteed to never run again, never merely "not running right now". Safe
// to call at most once, like every other one-way Stop in this codebase.
func (lt *listenThrottle) Stop() {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	lt.stopped = true
	if lt.timer != nil {
		lt.timer.Stop()
		lt.timer = nil
	}
}
