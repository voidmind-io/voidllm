package mcp_test

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers listenThrottle (listen_manager.go) directly — the
// per-server invalidation throttle ListenManager applies to onToolsChanged —
// independent of a full ListenManager/runListener/HTTP round trip.
//
// Every test here overrides listenThrottleInterval via
// SetListenThrottleIntervalForTest, following the same discipline
// listen_manager_test.go's own withShrunkListenTimings documents: not
// t.Parallel(), so no other test in this package can observe a different
// value mid-run. mcp_test as a whole DOES run other files' t.Parallel()
// tests, but per Go's own test scheduling, those only start once every
// non-parallel test (including this file's) has already finished.

// TestListenThrottle_LeadingEdge_FiresImmediately verifies the first Call
// after construction (or after a window has fully closed) fires
// synchronously, on the caller's own goroutine, with no wait at all.
func TestListenThrottle_LeadingEdge_FiresImmediately(t *testing.T) {
	mcp.SetListenThrottleIntervalForTest(1 * time.Hour) // never lets a window close during this test
	t.Cleanup(func() { mcp.SetListenThrottleIntervalForTest(1 * time.Second) })

	var fired atomic.Bool
	lt := mcp.NewListenThrottleForTest(func() { fired.Store(true) })
	t.Cleanup(lt.Stop)

	lt.Call()
	if !fired.Load() {
		t.Error("Call() did not fire synchronously on the leading edge")
	}
}

// TestListenThrottle_BurstWithinWindow_CoalescedToAtMostTwoCalls_DeliversLast
// is the direct regression test for item 6's own requirement: a burst of
// calls arriving faster than listenThrottleInterval must never be delivered
// one-for-one (that would defeat the point of throttling at all), but the
// LAST call in the burst must still eventually be reflected — never
// silently dropped. Since Call itself carries no payload identifying which
// change triggered it, "the last one" is proven by having fire observe an
// external sequence counter the test itself advances immediately before
// each Call(): the trailing (window-closing) fire, if it happens after
// every Call in the burst has already returned, must observe that counter
// at its FINAL value — proving it ran after the whole burst, not merely
// after some early prefix of it.
func TestListenThrottle_BurstWithinWindow_CoalescedToAtMostTwoCalls_DeliversLast(t *testing.T) {
	const window = 40 * time.Millisecond
	mcp.SetListenThrottleIntervalForTest(window)
	t.Cleanup(func() { mcp.SetListenThrottleIntervalForTest(1 * time.Second) })

	var (
		mu          sync.Mutex
		fireCount   int
		lastSeenSeq int64
	)
	var seq atomic.Int64

	lt := mcp.NewListenThrottleForTest(func() {
		mu.Lock()
		fireCount++
		lastSeenSeq = seq.Load()
		mu.Unlock()
	})
	t.Cleanup(lt.Stop)

	const burst = 50
	for i := 0; i < burst; i++ {
		seq.Store(int64(i))
		lt.Call()
	}

	// Give the trailing-edge timer, if one is pending, ample time to fire —
	// several multiples of the window, comfortably bounded (not a tight
	// synchronization primitive, just an upper wait; recvWithin-style
	// helpers are not needed here since there is nothing to "arrive" beyond
	// this one deterministic window).
	time.Sleep(5 * window)

	mu.Lock()
	defer mu.Unlock()
	if fireCount == 0 || fireCount > 2 {
		t.Fatalf("fireCount = %d after a %d-call burst, want 1 or 2 (leading, and optionally one coalesced trailing call)", fireCount, burst)
	}
	if lastSeenSeq != burst-1 {
		t.Errorf("lastSeenSeq = %d, want %d (the burst's own last call, not merely an early one) — the last change must not be dropped", lastSeenSeq, burst-1)
	}
}

// TestListenThrottle_NoCallAfterStop verifies that once Stop returns, no
// further call to fire ever happens — neither from a Call made after Stop,
// nor from a trailing-edge timer that was already pending when Stop was
// called.
func TestListenThrottle_NoCallAfterStop(t *testing.T) {
	const window = 30 * time.Millisecond
	mcp.SetListenThrottleIntervalForTest(window)
	t.Cleanup(func() { mcp.SetListenThrottleIntervalForTest(1 * time.Second) })

	var fireCount atomic.Int32
	lt := mcp.NewListenThrottleForTest(func() { fireCount.Add(1) })

	lt.Call() // leading edge: fires immediately, opens a window
	lt.Call() // coalesced: would otherwise fire at the trailing edge
	if got := fireCount.Load(); got != 1 {
		t.Fatalf("fireCount after the leading call = %d, want 1", got)
	}

	lt.Stop() // must prevent the pending trailing-edge fire above

	time.Sleep(5 * window) // comfortably past the window the pending trailing fire would have used

	if got := fireCount.Load(); got != 1 {
		t.Errorf("fireCount after Stop and waiting past the window = %d, want 1 (no call after Stop returns)", got)
	}

	lt.Call() // a Call after Stop must also be a no-op
	if got := fireCount.Load(); got != 1 {
		t.Errorf("fireCount after a Call following Stop = %d, want 1 (Stop is permanent)", got)
	}
}
