package mcp_test

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers ListenManager (listen_manager.go) — the per-instance
// supervisor that holds one subscriptions/listen stream open per modern-era
// upstream and reconnects per runListener's own policy.
//
// Every test in this file is deliberately NOT marked t.Parallel(): all of
// them override the package-level listenMinBackoff/listenMaxBackoff/
// listenUnsupportedRetry/listenReconnectJitterMax vars via
// withShrunkListenTimings below, and export_test.go's own doc for those
// setters is explicit that "no ListenManager is ever constructed
// concurrently with a test overriding these" is what makes a plain var
// (rather than an atomic) safe there — which this file's tests satisfy only
// by running strictly one at a time (Go's test runner never starts a
// non-parallel test's body concurrently with another non-parallel test, nor
// with any t.Parallel() test's body, which only ever run after every serial
// test in the binary has already finished).

// withShrunkListenTimings overrides every runListener reconnect-policy timing
// constant to a small value for the duration of t, restoring the real
// production values (matching listen_manager.go's own documented defaults)
// via t.Cleanup before the next (necessarily serial) test in this file runs.
func withShrunkListenTimings(t *testing.T) {
	t.Helper()
	mcp.SetListenMinBackoffForTest(3 * time.Millisecond)
	mcp.SetListenMaxBackoffForTest(300 * time.Millisecond)
	mcp.SetListenUnsupportedRetryForTest(80 * time.Millisecond)
	mcp.SetListenReconnectJitterMaxForTest(20 * time.Millisecond)
	t.Cleanup(func() {
		mcp.SetListenMinBackoffForTest(1 * time.Second)
		mcp.SetListenMaxBackoffForTest(5 * time.Minute)
		mcp.SetListenUnsupportedRetryForTest(1 * time.Hour)
		mcp.SetListenReconnectJitterMaxForTest(1 * time.Second)
	})
}

// newRecordingServer builds an httptest.Server whose handler sends the
// current time on connected (unbuffered sends never block callers here since
// every connected channel in this file is created with ample buffer) before
// running handle, so a test can observe exactly when — and how many times —
// an upstream was connected to, independent of what that connection's
// response actually contains.
func newRecordingServer(t *testing.T, connected chan<- time.Time, handle http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connected <- time.Now()
		handle(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ackThenBlock acknowledges the subscriptions/listen request (honoring
// toolsListChanged) and then blocks until the request's own context ends —
// exactly blockUntilCanceled's contract (listen_client_test.go, same
// package) — so the connection stays open until ListenManager itself tears
// it down (Reconcile removing/changing the target, or Stop).
func ackThenBlock(w http.ResponseWriter, r *http.Request) {
	blockUntilCanceled(w, r, ackEvent(true))
}

// ackThenGracefulEnd acknowledges the request, then immediately answers the
// MCP 2026-07-28 §3.4 "Graceful Closure" response (gracefulEndEvent) and lets
// the handler return, closing the connection normally — the shape that
// drives runListener's "nil (graceful end): reconnect after jitter, no
// backoff" bucket.
func ackThenGracefulEnd(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(ackEvent(true) + gracefulEndEvent))
	w.(http.Flusher).Flush()
}

// ackThenAbruptDrop acknowledges the request, then returns immediately
// without a graceful-end response — the connection closes, and the
// client-side Listen call observes this as an ordinary transport failure
// (an unexpected EOF), driving runListener's exponential-backoff "any other
// error" bucket exactly like ackThenGracefulEnd drives the jitter bucket.
func ackThenAbruptDrop(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(ackEvent(true)))
	w.(http.Flusher).Flush()
}

// neverAcksUnexpectedResponse answers every request with an HTTP 200, no
// body, and no text/event-stream content type — a shape Listen classifies as
// errListenUnexpectedResponse, never reaching an acknowledgement at all. This
// drives the same exponential-backoff bucket as ackThenAbruptDrop, but
// without ever calling onAck — used by the backoff-growth test, which needs
// every attempt in its measured run to fail BEFORE any ack resets the state.
func neverAcksUnexpectedResponse(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// alwaysUnsupported404 answers every request with HTTP 404 — ErrListenUnsupported.
func alwaysUnsupported404(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNotFound)
}

// recvWithin waits up to timeout for a value on ch, failing t if none
// arrives — the bounded "did (not) happen" idiom this package's own testing
// conventions require in place of a sleep.
func recvWithin[T any](t *testing.T, ch <-chan T, timeout time.Duration, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(timeout):
		t.Fatalf("timed out after %v waiting for %s", timeout, what)
		var zero T
		return zero
	}
}

// assertNoneWithin is the negative counterpart to recvWithin: a bounded
// "did not happen" assertion, not a synchronization sleep — it fails t only
// if a value DOES arrive within the window, which would disprove the
// property under test (e.g. "no retry before the unsupported-retry window
// elapses").
func assertNoneWithin[T any](t *testing.T, ch <-chan T, window time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("received %s within %v, want none", what, window)
	case <-time.After(window):
	}
}

// ---- Reconcile: one listener per target ------------------------------------

func TestListenManager_Reconcile_StartsOneListenerPerTarget(t *testing.T) {
	withShrunkListenTimings(t)

	connected1 := make(chan time.Time, 8)
	connected2 := make(chan time.Time, 8)
	srv1 := newRecordingServer(t, connected1, ackThenBlock)
	srv2 := newRecordingServer(t, connected2, ackThenBlock)

	manager := mcp.NewListenManager(func(string) {})
	t.Cleanup(manager.Stop)

	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(srv1.URL, "none", "", "")},
		{ServerID: "s2", Transport: newModernTransport(srv2.URL, "none", "", "")},
	})

	recvWithin(t, connected1, 2*time.Second, "srv1's first connection")
	recvWithin(t, connected2, 2*time.Second, "srv2's first connection")

	// Both upstreams ack and then block indefinitely: no reconnect should
	// ever happen while the manager's targets stay unchanged, so a second
	// connection attempt arriving promptly would indicate a stray extra
	// listener.
	assertNoneWithin(t, connected1, 100*time.Millisecond, "a second connection to srv1")
	assertNoneWithin(t, connected2, 100*time.Millisecond, "a second connection to srv2")
}

// ---- Reconcile: removing a target cancels its listener ---------------------

func TestListenManager_Reconcile_RemovingTargetCancelsIt_UpstreamSeesDisconnect(t *testing.T) {
	withShrunkListenTimings(t)

	connected := make(chan time.Time, 8)
	disconnected := make(chan struct{}, 1)
	srv := newRecordingServer(t, connected, func(w http.ResponseWriter, r *http.Request) {
		blockUntilCanceled(w, r, ackEvent(true))
		disconnected <- struct{}{}
	})

	manager := mcp.NewListenManager(func(string) {})
	t.Cleanup(manager.Stop)

	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(srv.URL, "none", "", "")},
	})
	recvWithin(t, connected, 2*time.Second, "the initial connection")

	manager.Reconcile(nil) // remove every target

	recvWithin(t, disconnected, 2*time.Second, "the upstream observing its connection torn down")
}

// ---- Reconcile: a changed Transport pointer restarts the listener ----------

func TestListenManager_Reconcile_ChangedTransportPointerRestarts(t *testing.T) {
	withShrunkListenTimings(t)

	oldConnected := make(chan time.Time, 8)
	oldDisconnected := make(chan struct{}, 1)
	oldSrv := newRecordingServer(t, oldConnected, func(w http.ResponseWriter, r *http.Request) {
		blockUntilCanceled(w, r, ackEvent(true))
		oldDisconnected <- struct{}{}
	})

	newConnected := make(chan time.Time, 8)
	newSrv := newRecordingServer(t, newConnected, ackThenBlock)

	manager := mcp.NewListenManager(func(string) {})
	t.Cleanup(manager.Stop)

	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(oldSrv.URL, "none", "", "")},
	})
	recvWithin(t, oldConnected, 2*time.Second, "the old transport's first connection")

	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(newSrv.URL, "none", "", "")},
	})

	recvWithin(t, oldDisconnected, 2*time.Second, "the old transport's listener being cancelled")
	recvWithin(t, newConnected, 2*time.Second, "the new transport's first connection")
}

// TestListenManager_Reconcile_TransportChange_NewListenerWaitsForOldGoroutineExit
// is the direct regression test for item 8's own guarantee: a
// transport-pointer change does not just cancel the old listener and race a
// new one against its teardown — the new listener's first connection
// attempt waits for the OLD goroutine to have FULLY exited. In production
// that window is sub-millisecond and so cannot be observed deterministically
// through realistic HTTP timing alone; SetListenExitDelayHookForTest widens
// it on demand, purely for this test, without changing any production
// behavior (see that hook's own doc).
func TestListenManager_Reconcile_TransportChange_NewListenerWaitsForOldGoroutineExit(t *testing.T) {
	withShrunkListenTimings(t)

	oldConnected := make(chan time.Time, 8)
	oldSrv := newRecordingServer(t, oldConnected, ackThenBlock)

	newConnected := make(chan time.Time, 8)
	newSrv := newRecordingServer(t, newConnected, ackThenBlock)

	manager := mcp.NewListenManager(func(string) {})
	// Registered BEFORE the hook-clearing cleanup below, so it runs AFTER
	// that one during t.Cleanup's LIFO unwind: Stop() also cancels the NEW
	// listener, which would otherwise re-enter the still-installed hook
	// (see hookStarted's own once-guard below for the belt-and-suspenders
	// half of this) after this test's own assertions no longer expect it.
	t.Cleanup(manager.Stop)

	const exitDelay = 200 * time.Millisecond
	hookStarted := make(chan struct{})
	var hookStartedOnce sync.Once
	mcp.SetListenExitDelayHookForTest(func() {
		hookStartedOnce.Do(func() { close(hookStarted) })
		time.Sleep(exitDelay)
	})
	t.Cleanup(func() { mcp.SetListenExitDelayHookForTest(nil) })

	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(oldSrv.URL, "none", "", "")},
	})
	recvWithin(t, oldConnected, 2*time.Second, "the old transport's first connection")

	reconcileAt := time.Now()
	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(newSrv.URL, "none", "", "")},
	})

	recvWithin(t, hookStarted, 2*time.Second, "the old listener's goroutine beginning its (delayed) exit")

	// While the old goroutine is still delayed inside the exit hook, the new
	// listener must not have connected yet.
	assertNoneWithin(t, newConnected, exitDelay/2, "the new transport's first connection, before the old goroutine finished exiting")

	newFirstConn := recvWithin(t, newConnected, 2*time.Second, "the new transport's first connection, after the old goroutine finished exiting")
	if elapsed := newFirstConn.Sub(reconcileAt); elapsed < exitDelay {
		t.Errorf("new listener connected %v after Reconcile, want at least %v — it must wait for the old goroutine to fully exit", elapsed, exitDelay)
	}
}

// TestListenManager_Reconcile_SupersessionChain_ThirdListenerWaitsForOriginal
// is the direct regression test for item 1's own guarantee: a supersession
// CHAIN — A superseded by B, B itself superseded by C before B ever got a
// chance to wait out A's exit — must still never let C connect before A has
// genuinely exited, even though B itself is cancelled (and so abandons its
// own wait on A) partway through. B never reaches transport.Listen at all in
// this scenario (its own ctx ends while it is still waiting on A's done), so
// SetListenExitDelayHookForTest — which only fires from runListener's own
// exitIfCtxDone — only ever fires for A here, exactly like the direct
// single-supersession regression test above, just one link further down the
// chain.
func TestListenManager_Reconcile_SupersessionChain_ThirdListenerWaitsForOriginal(t *testing.T) {
	withShrunkListenTimings(t)

	aConnected := make(chan time.Time, 8)
	aSrv := newRecordingServer(t, aConnected, ackThenBlock)

	cConnected := make(chan time.Time, 8)
	cSrv := newRecordingServer(t, cConnected, ackThenBlock)

	manager := mcp.NewListenManager(func(string) {})
	// Registered BEFORE the hook-clearing cleanup below, so it runs AFTER
	// that one during t.Cleanup's LIFO unwind — see the sibling
	// single-supersession test's own doc for why this ordering matters.
	t.Cleanup(manager.Stop)

	const exitDelay = 200 * time.Millisecond
	hookStarted := make(chan struct{})
	var hookStartedOnce sync.Once
	mcp.SetListenExitDelayHookForTest(func() {
		hookStartedOnce.Do(func() {
			close(hookStarted)
			time.Sleep(exitDelay)
		})
	})
	t.Cleanup(func() { mcp.SetListenExitDelayHookForTest(nil) })

	// A: connects and blocks indefinitely.
	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(aSrv.URL, "none", "", "")},
	})
	recvWithin(t, aConnected, 2*time.Second, "A's first connection")

	// Supersede A with B. B never actually connects to anything: it is
	// itself superseded (below) before A — held open by the delayed exit
	// hook above — ever exits, so B never gets past its own wait for A's
	// done. The endpoint is deliberately never dialed.
	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport("http://127.0.0.1:0", "none", "", "")},
	})

	// Supersede B with C, before A has exited (the hook is still holding A's
	// own exit open at this point) — the item 1 regression case: B is
	// cancelled while still waiting on A, then replaced by C.
	reconcileAt := time.Now()
	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(cSrv.URL, "none", "", "")},
	})

	recvWithin(t, hookStarted, 2*time.Second, "A's goroutine beginning its (delayed) exit")

	// While A is still delayed inside the exit hook, C must not have
	// connected yet — proving B's own done channel did not close (and so
	// unblock C's own wait) merely because B's ctx was cancelled.
	assertNoneWithin(t, cConnected, exitDelay/2, "C's first connection, before A finished exiting")

	cFirstConn := recvWithin(t, cConnected, 2*time.Second, "C's first connection, after A finished exiting")
	if elapsed := cFirstConn.Sub(reconcileAt); elapsed < exitDelay {
		t.Errorf("C connected %v after the B->C Reconcile, want at least %v — C must wait for the ORIGINAL predecessor (A), not just its immediate one (B), to fully exit", elapsed, exitDelay)
	}
}

// ---- Stop: cancels all, waits for goroutines, no leaks ----------------------

func TestListenManager_Stop_CancelsAll_WaitsForHandlersToExit_NoLeaks(t *testing.T) {
	withShrunkListenTimings(t)

	const n = 3
	connected := make(chan time.Time, n*4)
	done := make(chan struct{}, n*4)

	manager := mcp.NewListenManager(func(string) {})

	targets := make([]mcp.ListenTarget, 0, n)
	for i := 0; i < n; i++ {
		srv := newRecordingServer(t, connected, func(w http.ResponseWriter, r *http.Request) {
			blockUntilCanceled(w, r, ackEvent(true))
			done <- struct{}{}
		})
		targets = append(targets, mcp.ListenTarget{
			ServerID:  string(rune('a' + i)),
			Transport: newModernTransport(srv.URL, "none", "", ""),
		})
	}
	manager.Reconcile(targets)

	for i := 0; i < n; i++ {
		recvWithin(t, connected, 2*time.Second, "each target's initial connection")
	}

	manager.Stop() // must block until every runListener goroutine has returned

	for i := 0; i < n; i++ {
		recvWithin(t, done, 2*time.Second, "each upstream handler observing its connection torn down")
	}
}

// ---- Reconcile after Stop: permanent no-op ---------------------------------

func TestListenManager_Reconcile_AfterStop_NoOp(t *testing.T) {
	withShrunkListenTimings(t)

	connected := make(chan time.Time, 8)
	srv := newRecordingServer(t, connected, ackThenBlock)

	manager := mcp.NewListenManager(func(string) {})
	manager.Stop() // stop before ever starting anything

	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(srv.URL, "none", "", "")},
	})

	assertNoneWithin(t, connected, 200*time.Millisecond, "a connection from a Reconcile call after Stop")
}

// TestListenManager_ConcurrentReconcileAndStop_RaceSafe is the direct
// regression test for ListenManager's own corrected doc: Reconcile and Stop
// are safe to call concurrently, including with themselves, serialized
// entirely by ListenManager's own mutex. Several goroutines hammer Reconcile
// with a churning target list (adds, removals, and transport-pointer
// changes for the same ServerID) while another goroutine calls Stop exactly
// once, mid-run, with no synchronization between the two beyond what
// ListenManager itself provides. Run with -race, this proves the absence of
// a data race on ListenManager's own state under an ARBITRARY interleaving —
// not merely the absence of a panic or deadlock under one convenient,
// already-synchronized ordering.
func TestListenManager_ConcurrentReconcileAndStop_RaceSafe(t *testing.T) {
	withShrunkListenTimings(t)

	srv1 := newRecordingServer(t, make(chan time.Time, 256), ackThenBlock)
	srv2 := newRecordingServer(t, make(chan time.Time, 256), ackThenBlock)
	tr1 := newModernTransport(srv1.URL, "none", "", "")
	tr2 := newModernTransport(srv2.URL, "none", "", "")

	manager := mcp.NewListenManager(func(string) {})

	const reconcilers = 8
	const itersPerGoroutine = 50

	var wg sync.WaitGroup
	wg.Add(reconcilers)
	for g := 0; g < reconcilers; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < itersPerGoroutine; i++ {
				var targets []mcp.ListenTarget
				switch (g + i) % 3 {
				case 0:
					targets = []mcp.ListenTarget{{ServerID: "s1", Transport: tr1}}
				case 1:
					targets = []mcp.ListenTarget{{ServerID: "s1", Transport: tr2}}
				case 2:
					targets = nil
				}
				manager.Reconcile(targets)
			}
		}(g)
	}

	// Stop runs concurrently, NOT synchronized with the reconcilers above —
	// proving safety under an arbitrary interleaving is the whole point.
	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		manager.Stop()
	}()

	wg.Wait()
	recvWithin(t, stopDone, 5*time.Second, "Stop() to return")

	// Reconcile remains a permanent no-op once Stop has returned.
	connected := make(chan time.Time, 8)
	srv3 := newRecordingServer(t, connected, ackThenBlock)
	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(srv3.URL, "none", "", "")},
	})
	assertNoneWithin(t, connected, 200*time.Millisecond, "a connection from a Reconcile call after Stop")
}

// ---- Reconnect after graceful end ------------------------------------------

func TestListenManager_ReconnectAfterGracefulEnd(t *testing.T) {
	withShrunkListenTimings(t)

	connected := make(chan time.Time, 8)
	srv := newRecordingServer(t, connected, ackThenGracefulEnd)

	manager := mcp.NewListenManager(func(string) {})
	t.Cleanup(manager.Stop)

	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(srv.URL, "none", "", "")},
	})

	recvWithin(t, connected, 2*time.Second, "the first connection")
	recvWithin(t, connected, 2*time.Second, "a reconnect after the graceful end")
}

// ---- onToolsChanged: second ack fires it, first ack does not ---------------

func TestListenManager_SecondAckTriggersOnToolsChanged_FirstAckDoesNot(t *testing.T) {
	withShrunkListenTimings(t)

	// ackDone is signaled synchronously at the end of EACH ack's own onAck
	// handling (backoff reset, and the "must not fire on the first ack"
	// decision already made) — see listenAckHookForTest's own doc. Waiting
	// on it, rather than merely on "connected" (which only proves the HTTP
	// request arrived, before this package has processed anything about it),
	// is what makes the "nothing fired after ack #1" assertion below prove
	// something: an implementation that incorrectly fired on the FIRST ack
	// would already have delivered to toolsChanged by the time ackDone's
	// first value arrives, since both are signaled from the same onAck call.
	ackDone := make(chan string, 8)
	mcp.SetListenAckHookForTest(func(serverID string) { ackDone <- serverID })
	t.Cleanup(func() { mcp.SetListenAckHookForTest(nil) })

	connected := make(chan time.Time, 8)
	srv := newRecordingServer(t, connected, ackThenGracefulEnd)

	toolsChanged := make(chan string, 8)
	manager := mcp.NewListenManager(func(serverID string) { toolsChanged <- serverID })
	t.Cleanup(manager.Stop)

	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "srv-x", Transport: newModernTransport(srv.URL, "none", "", "")},
	})

	recvWithin(t, connected, 2*time.Second, "the first connection (first ack)")
	recvWithin(t, ackDone, 2*time.Second, "the first ack's own onAck handling to complete")

	select {
	case <-toolsChanged:
		t.Fatal("onToolsChanged fired after only the first ack")
	default:
	}

	recvWithin(t, connected, 2*time.Second, "the second connection (second ack)")
	recvWithin(t, ackDone, 2*time.Second, "the second ack's own onAck handling to complete")
	got := recvWithin(t, toolsChanged, 2*time.Second, "onToolsChanged after the second ack")
	if got != "srv-x" {
		t.Errorf("onToolsChanged serverID = %q, want %q", got, "srv-x")
	}

	manager.Stop()
	// Stop() only returns once every runListener goroutine has exited, so the
	// channel's contents are now final: exactly one call total.
	select {
	case extra := <-toolsChanged:
		t.Errorf("onToolsChanged fired again unexpectedly for %q", extra)
	default:
	}
}

// ---- Abrupt disconnect after ack: reconnect, onToolsChanged on next ack ----

func TestListenManager_AbruptDisconnectAfterAck_ReconnectsAndFiresOnNextAck(t *testing.T) {
	withShrunkListenTimings(t)

	// See TestListenManager_SecondAckTriggersOnToolsChanged_FirstAckDoesNot's
	// own doc for why waiting on ackDone (not just "connected") is what
	// makes the "nothing fired before the reconnect's ack" assertion below
	// actually prove something.
	ackDone := make(chan string, 8)
	mcp.SetListenAckHookForTest(func(serverID string) { ackDone <- serverID })
	t.Cleanup(func() { mcp.SetListenAckHookForTest(nil) })

	connected := make(chan time.Time, 8)
	var attempt atomic.Int32
	srv := newRecordingServer(t, connected, func(w http.ResponseWriter, r *http.Request) {
		if attempt.Add(1) == 1 {
			ackThenAbruptDrop(w, r)
			return
		}
		ackThenBlock(w, r)
	})

	toolsChanged := make(chan string, 8)
	manager := mcp.NewListenManager(func(serverID string) { toolsChanged <- serverID })
	t.Cleanup(manager.Stop)

	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "srv-y", Transport: newModernTransport(srv.URL, "none", "", "")},
	})

	recvWithin(t, connected, 2*time.Second, "the first connection (acks, then drops abruptly)")
	recvWithin(t, ackDone, 2*time.Second, "the first ack's own onAck handling to complete")

	select {
	case <-toolsChanged:
		t.Fatal("onToolsChanged fired before the reconnect's own ack")
	default:
	}

	recvWithin(t, connected, 2*time.Second, "the reconnect after the abrupt disconnect")
	recvWithin(t, ackDone, 2*time.Second, "the reconnect's own onAck handling to complete")
	got := recvWithin(t, toolsChanged, 2*time.Second, "onToolsChanged after the reconnect's ack")
	if got != "srv-y" {
		t.Errorf("onToolsChanged serverID = %q, want %q", got, "srv-y")
	}
}

// ---- Unsupported: no retry within the shrunken window, then retry after ----

func TestListenManager_Unsupported_NoRetryWithinWindow_ThenRetryAfter(t *testing.T) {
	withShrunkListenTimings(t)
	const unsupportedRetry = 80 * time.Millisecond // matches withShrunkListenTimings

	connected := make(chan time.Time, 8)
	srv := newRecordingServer(t, connected, alwaysUnsupported404)

	manager := mcp.NewListenManager(func(string) {})
	t.Cleanup(manager.Stop)

	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(srv.URL, "none", "", "")},
	})

	first := recvWithin(t, connected, 2*time.Second, "the first (unsupported) connection attempt")

	// Bounded "did not happen" window, comfortably under unsupportedRetry:
	// no retry should occur this quickly after an ErrListenUnsupported result.
	assertNoneWithin(t, connected, unsupportedRetry/2, "a retry before the unsupported-retry window elapsed")

	second := recvWithin(t, connected, 2*time.Second, "the retry after the unsupported-retry window")
	gap := second.Sub(first)
	// Full-jitter is not applied to this bucket (see runListener's own doc:
	// the unsupported-retry wait is fixed, not randomized), so the gap should
	// track unsupportedRetry itself, not a random fraction of it — a generous
	// lower bound (well under the configured value) absorbs scheduling
	// jitter without weakening the property under test: SOME real wait
	// happened, comparable to the configured retry duration, not an
	// immediate reconnect.
	if gap < unsupportedRetry/2 {
		t.Errorf("gap between unsupported attempts = %v, want at least ~%v", gap, unsupportedRetry/2)
	}
}

// ---- Backoff: grows without ack, resets after ack --------------------------

// TestListenManager_BackoffGrowsWithoutAck_ResetsAfterAck asserts the EXACT
// reconnect-gap sequence across a run of consecutive, never-acknowledged
// failures (each gap's underlying bound doubles per runListener's own
// policy, capped at listenMaxBackoff), then forces one acknowledgement and
// checks the very next gap collapses back down to listenMinBackoff — the
// reset property. This is made deterministic — rather than only asserting a
// looser "grows, then shrinks" trend — via SetListenJitterFuncForTest, which
// replaces randDuration's own random draw with a fixed function returning
// its input unchanged: every wait then equals its own underlying bound
// exactly (mod scheduling delay, absorbed by wantGap's own tolerance), so
// this test can assert precise expected values instead of only relative
// ordering.
func TestListenManager_BackoffGrowsWithoutAck_ResetsAfterAck(t *testing.T) {
	const (
		minBackoff = 20 * time.Millisecond
		maxBackoff = 200 * time.Millisecond
	)
	mcp.SetListenMinBackoffForTest(minBackoff)
	mcp.SetListenMaxBackoffForTest(maxBackoff)
	mcp.SetListenUnsupportedRetryForTest(80 * time.Millisecond)
	mcp.SetListenReconnectJitterMaxForTest(20 * time.Millisecond)
	t.Cleanup(func() {
		mcp.SetListenMinBackoffForTest(1 * time.Second)
		mcp.SetListenMaxBackoffForTest(5 * time.Minute)
		mcp.SetListenUnsupportedRetryForTest(1 * time.Hour)
		mcp.SetListenReconnectJitterMaxForTest(1 * time.Second)
	})
	mcp.SetListenJitterFuncForTest(func(max time.Duration) time.Duration { return max })
	t.Cleanup(func() { mcp.SetListenJitterFuncForTest(nil) })

	const preAckFailures = 6 // enough doublings for maxBackoff to be reached well before the ack

	connected := make(chan time.Time, preAckFailures+4)
	var attempt atomic.Int32
	srv := newRecordingServer(t, connected, func(w http.ResponseWriter, r *http.Request) {
		n := attempt.Add(1)
		switch {
		case n <= preAckFailures:
			neverAcksUnexpectedResponse(w, r)
		case n == preAckFailures+1:
			// The ack itself resets backoff synchronously (onAck), then this
			// same connection fails immediately afterward — the wait BEFORE
			// the next attempt is computed from the just-reset floor.
			ackThenAbruptDrop(w, r)
		default:
			ackThenBlock(w, r)
		}
	})

	manager := mcp.NewListenManager(func(string) {})
	t.Cleanup(manager.Stop)

	manager.Reconcile([]mcp.ListenTarget{
		{ServerID: "s1", Transport: newModernTransport(srv.URL, "none", "", "")},
	})

	timestamps := make([]time.Time, 0, preAckFailures+2)
	for i := 0; i < preAckFailures+2; i++ {
		timestamps = append(timestamps, recvWithin(t, connected, 3*time.Second, "the next connection attempt"))
	}

	gap := func(i int) time.Duration { return timestamps[i].Sub(timestamps[i-1]) }

	// wantGap asserts got is at least want (a timer can never fire early) and
	// no more than want plus a generous absolute allowance for scheduling
	// delay under -race/test-parallelism — far tighter than an unbounded
	// upper end would be, while still tolerant of a slow CI runner.
	wantGap := func(i int, want time.Duration) {
		t.Helper()
		got := gap(i)
		const tolerance = 150 * time.Millisecond
		if got < want || got > want+tolerance {
			t.Errorf("gap(%d) = %v, want in [%v, %v]", i, got, want, want+tolerance)
		}
	}

	// Pre-ack: exponential doubling from minBackoff, capped at maxBackoff.
	expected := minBackoff
	for i := 1; i <= preAckFailures; i++ {
		wantGap(i, expected)
		expected = min(expected*2, maxBackoff)
	}

	// Reset check: the gap immediately after the ack (index preAckFailures+1)
	// must be exactly minBackoff again — onAck resets backoff synchronously,
	// before this attempt's own failure computes its wait.
	wantGap(preAckFailures+1, minBackoff)
}
