package mcp_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers ToolCache's singleflight-based fetch deduplication
// (tc.sf, see tool_cache.go's own doc) and entryFor's discard-and-retry loop
// when a concurrent Invalidate lands mid-fetch (fetchAndPublish's (nil, nil)
// return — see both methods' own docs).

// ---- A slow fetch for one server does not block another, unrelated server --

// TestToolCache_SlowFetchForOneServer_DoesNotBlockAnotherServer verifies the
// property tc.sf's per-serverID keying exists for: a fetch blocked
// indefinitely for server "slow" must not prevent GetTools for an entirely
// different, already-fresh server "fast" from returning immediately. Before
// entryFor's fetch-on-miss path stopped holding a full write Lock across the
// whole upstream round-trip, a slow fetch for ANY server serialized every
// other server's GetTools too.
func TestToolCache_SlowFetchForOneServer_DoesNotBlockAnotherServer(t *testing.T) {
	t.Parallel()

	slowStarted := make(chan struct{})
	slowRelease := make(chan struct{})

	fetcher := func(_ context.Context, serverID string) (*mcp.ToolListing, error) {
		if serverID == "slow" {
			close(slowStarted)
			<-slowRelease
			return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "slow_tool"}}}, nil
		}
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "fast_tool"}}}, nil
	}

	cache := mcp.NewToolCache(fetcher, time.Hour)

	// Pre-populate "fast" so its entry is already fresh — entryFor's
	// freshness check short-circuits before ever touching tc.sf, isolating
	// this test to the specific claim: a stale/missing fetch for one server
	// must not serialize behind an in-flight fetch for a DIFFERENT server.
	if _, err := cache.GetTools(context.Background(), "fast"); err != nil {
		t.Fatalf("GetTools (pre-populate fast): %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	var slowErr error
	go func() {
		defer wg.Done()
		_, slowErr = cache.GetTools(context.Background(), "slow")
	}()

	<-slowStarted // the slow fetch is now blocked inside the fetcher

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := cache.GetTools(context.Background(), "fast"); err != nil {
			t.Errorf("GetTools(fast) while slow is in flight: %v", err)
		}
	}()

	select {
	case <-done:
		// GetTools(fast) returned promptly — the property under test.
	case <-time.After(2 * time.Second):
		t.Fatal("GetTools(fast) did not return within 2s while GetTools(slow) was blocked — " +
			"a slow fetch for one server is blocking an unrelated, already-fresh server")
	}

	close(slowRelease)
	wg.Wait()
	if slowErr != nil {
		t.Errorf("GetTools(slow): %v", slowErr)
	}
}

// ---- Concurrent GetTools for the SAME server: fetcher called exactly once --

// TestToolCache_ConcurrentGetTools_SameServer_SingleflightDeduplicatesFetch
// drives many concurrent GetTools calls for the same stale/missing serverID
// and verifies the fetcher is called exactly once — the same property
// TestToolCache_GetTools_Concurrent already covers via a slow-fetcher race
// window, exercised again here directly against the singleflight-based
// entryFor implementation with a higher goroutine count.
//
// This used to need a barrier (every goroutine signaling "ready" before the
// fetcher was released) PLUS a bounded runtime.Gosched() spin PLUS a fixed
// real sleep on top of both, to make it likely enough that every one of the
// 50 goroutines had actually reached tc.sf.DoChan before release closed —
// otherwise a goroutine arriving late could find the leader's singleflight
// call already forgotten and start a second, independent fetch of its own.
// sharedFetch's own freshness recheck (see that method's own doc) removes
// the need for any of that entirely: a late goroutine that starts a fresh
// DoChan call now re-checks tc.entries under RLock as the very first thing
// its own callback does, finds the leader's just-published entry already
// fresh, and returns it without ever calling the fetcher again — so "fetcher
// called exactly once" now holds regardless of how many of the 50
// goroutines join the original in-flight call versus arrive after it has
// already completed. No synchronization beyond the ordinary <-started signal
// (proving at least the very first call reached the fetcher) is needed
// before closing release.
func TestToolCache_ConcurrentGetTools_SameServer_SingleflightDeduplicatesFetch(t *testing.T) {
	t.Parallel()

	var calls int64
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once

	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		once.Do(func() { close(started) })
		<-release
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "shared_tool"}}}, nil
	}

	cache := mcp.NewToolCache(fetcher, time.Hour)

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if _, err := cache.GetTools(context.Background(), "shared"); err != nil {
				t.Errorf("GetTools: %v", err)
			}
		}()
	}

	<-started
	close(release)
	wg.Wait()

	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("fetcher called %d times for %d concurrent GetTools callers, want exactly 1 (singleflight plus sharedFetch's own freshness recheck)", got, goroutines)
	}
}

// ---- entryFor: an Invalidate landing mid-fetch is never handed back --------

// TestToolCache_EntryFor_ConcurrentInvalidateMidFetch_RetriesAndReturnsFreshResult
// drives the real concurrent race entryFor's discard-and-retry loop exists
// for: one goroutine's GetTools triggers a fetch that blocks; while it is
// blocked, Invalidate runs (simulating, e.g., an admin rotating this
// server's credential); the blocked fetch is then allowed to complete and
// returns STALE tools fetched under the now-superseded state.
// fetchAndPublish's generation check discards that result (returns (nil,
// nil) internally), and entryFor must not hand the stale tools back to
// GetTools' caller — it must retry and get FRESH tools from a second fetch
// instead. Both the fetcher's call count and the actual tools returned are
// asserted.
func TestToolCache_EntryFor_ConcurrentInvalidateMidFetch_RetriesAndReturnsFreshResult(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	var calls int64

	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		n := atomic.AddInt64(&calls, 1)
		if n == 1 {
			close(started)
			<-release
			return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "stale_tool_under_old_credential"}}}, nil
		}
		// The retry: no blocking, so the test cannot deadlock even if
		// entryFor's loop needs more than one extra iteration.
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "fresh_tool_under_new_credential"}}}, nil
	}

	cache := mcp.NewToolCache(fetcher, time.Hour)

	var wg sync.WaitGroup
	wg.Add(1)
	var got []mcp.Tool
	var getErr error
	go func() {
		defer wg.Done()
		got, getErr = cache.GetTools(context.Background(), "srv")
	}()

	<-started
	cache.Invalidate("srv") // races the still-in-flight first fetch
	close(release)
	wg.Wait()

	if getErr != nil {
		t.Fatalf("GetTools: %v", getErr)
	}
	if len(got) != 1 || got[0].Name != "fresh_tool_under_new_credential" {
		t.Fatalf("GetTools = %+v, want exactly [fresh_tool_under_new_credential] — the caller must never "+
			"receive the stale, superseded result the first (discarded) fetch produced", got)
	}
	if n := atomic.LoadInt64(&calls); n != 2 {
		t.Errorf("fetcher called %d times, want exactly 2 (the discarded fetch, then entryFor's retry)", n)
	}
}

// ---- entryFor's retry loop terminates once invalidation stops racing ------

// TestToolCache_EntryFor_BoundedRetries_TerminatesOnceInvalidationStops is a
// deterministic (non-goroutine, single-threaded) reproduction of repeated
// Invalidate calls racing entryFor's retry loop: the fetcher itself calls
// Invalidate on its own first 3 invocations, before returning — landing
// squarely inside the window fetchAndPublish's generation check is guarding,
// on every one of those calls — and stops on the 4th. This proves entryFor's
// loop is not merely "eventually consistent" in theory but actually
// terminates, with the exact expected fetch count, once the racing stops.
// 3 is deliberately well under maxToolCacheEntryAttempts (5): this test is
// about termination once invalidation STOPS, not about the retry cap itself
// — see the adjacent TestToolCache_EntryFor_RetryCap_ReturnsErrorUnderContinuousInvalidation
// for invalidation that never stops.
func TestToolCache_EntryFor_BoundedRetries_TerminatesOnceInvalidationStops(t *testing.T) {
	t.Parallel()

	const invalidateForFirstNCalls = 3

	var cache *mcp.ToolCache
	var calls int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		n := atomic.AddInt64(&calls, 1)
		if n <= invalidateForFirstNCalls {
			// Lands inside fetchAndPublish's own post-fetch generation
			// check window (this call happens synchronously, before
			// fetchAndPublish's fetch-return code path re-reads
			// tc.generations), so every one of these calls' results is
			// discarded and entryFor must retry.
			cache.Invalidate("srv")
		}
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: fmt.Sprintf("generation_%d", n)}}}, nil
	}
	cache = mcp.NewToolCache(fetcher, time.Hour)

	got, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}

	wantCalls := int64(invalidateForFirstNCalls + 1)
	if n := atomic.LoadInt64(&calls); n != wantCalls {
		t.Errorf("fetcher called %d times, want exactly %d (3 discarded attempts, then the one that finally lands)", n, wantCalls)
	}
	wantName := fmt.Sprintf("generation_%d", wantCalls)
	if len(got) != 1 || got[0].Name != wantName {
		t.Errorf("GetTools = %+v, want exactly [%s] (the generation that was never invalidated)", got, wantName)
	}
}

// TestToolCache_EntryFor_RetryCap_ReturnsErrorUnderContinuousInvalidation is
// the counterpart to the "invalidation stops" test above: the fetcher
// invalidates "srv" on EVERY call, without limit, reproducing exactly the
// unbounded race entryFor's own doc says its retry cap
// (maxToolCacheEntryAttempts) exists to bound. GetTools must return an error
// after exactly 5 fetcher calls — not hang, and not loop indefinitely. A
// timeout guard (a background goroutine racing a fixed deadline) bounds the
// test itself in case the cap were ever regressed to "no cap at all".
func TestToolCache_EntryFor_RetryCap_ReturnsErrorUnderContinuousInvalidation(t *testing.T) {
	t.Parallel()

	var cache *mcp.ToolCache
	var calls int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		n := atomic.AddInt64(&calls, 1)
		// Invalidates on every single call — the race never stops.
		cache.Invalidate("srv")
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: fmt.Sprintf("generation_%d", n)}}}, nil
	}
	cache = mcp.NewToolCache(fetcher, time.Hour)

	type result struct {
		got []mcp.Tool
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := cache.GetTools(context.Background(), "srv")
		done <- result{got: got, err: err}
	}()

	select {
	case res := <-done:
		if res.err == nil {
			t.Fatalf("GetTools = %+v, err = nil, want an error once the retry cap is exhausted under continuous invalidation", res.got)
		}
		if !strings.Contains(res.err.Error(), "could not be resolved after repeated invalidation") {
			t.Errorf("error = %q, want it to name the retry-cap guard", res.err.Error())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("GetTools did not return within 5s — the retry cap did not bound the loop as expected")
	}

	const wantCalls = 5 // mirrors the unexported maxToolCacheEntryAttempts
	if n := atomic.LoadInt64(&calls); n != wantCalls {
		t.Errorf("fetcher called %d times, want exactly %d (the retry cap, not fewer or more)", n, wantCalls)
	}
}

// blockingToolStore is a ToolStore whose Save blocks until release is
// closed and, once released, always succeeds — letting a test land an
// Invalidate call inside the exact window fetchAndPublish's own doc
// describes: after a fetch has been published to tc.entries but before its
// own, separately-locked store write has completed. LoadAll and Delete are
// both no-ops; this fake exists purely to control the TIMING of Save.
type blockingToolStore struct {
	saveStarted chan struct{}
	release     chan struct{}
	once        sync.Once
	saves       int64

	// mu guards saved and deleted, which record the store's actual current
	// state — not merely a call count — so a test can assert what a
	// concurrent recheck-and-delete (fetchAndPublish's own post-Save
	// generation recheck) actually left on disk, not just how many times
	// Save or Delete were invoked.
	mu     sync.Mutex
	saved  []mcp.Tool
	exists bool
}

func (s *blockingToolStore) LoadAll(context.Context) (map[string][]mcp.Tool, error) {
	return nil, nil
}

func (s *blockingToolStore) Save(_ context.Context, _ string, tools []mcp.Tool) error {
	n := atomic.AddInt64(&s.saves, 1)
	if n == 1 {
		s.once.Do(func() { close(s.saveStarted) })
		<-s.release
	}
	s.mu.Lock()
	s.saved = tools
	s.exists = true
	s.mu.Unlock()
	return nil
}

func (s *blockingToolStore) Delete(context.Context, string) error {
	s.mu.Lock()
	s.saved = nil
	s.exists = false
	s.mu.Unlock()
	return nil
}

// state returns the store's current content for the one server ID this
// fake ever serves in these tests: (nil, false) if the store's last
// operation was a Delete (or nothing was ever saved), or the tools most
// recently saved otherwise.
func (s *blockingToolStore) state() (tools []mcp.Tool, exists bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved, s.exists
}

// TestToolCache_StaleAfterInvalidation_DuringStoreWrite_NeverReturnedToNewCaller
// reproduces the race cacheEntry.generation exists to close: fetchAndPublish
// publishes an entry to tc.entries, THEN performs its separately-locked store
// write (Save) — see fetchAndPublish's own doc for why those two steps are
// not atomic. This test blocks Save, lets an Invalidate land in exactly that
// window, and starts a brand-new GetTools call while the first fetch is
// still blocked inside Save. Both calls must end up with the tools from a
// FRESH fetch (a later fetcher generation) — never the stale generation the
// blocked fetch published before Invalidate ran.
//
// It also exercises fetchAndPublish's post-Save generation recheck (see that
// method's own doc): a plain Invalidate — unlike InvalidateWithStore — never
// touches tc.storeMu or the store at all, so it lands here WHILE the first
// fetch's Save call is still blocked, superseding the generation that Save
// call is about to write under. Once Save unblocks, the store must end up
// with NO trace of the stale generation_1 write the blocked Save produced —
// the recheck must have deleted it — while the later, legitimately-saved
// generation the second (or any subsequent, un-superseded) fetch produces
// must survive untouched: the recheck exists to delete exactly the
// superseded write, never anything saved afterward under a generation that
// was never itself invalidated.
func TestToolCache_StaleAfterInvalidation_DuringStoreWrite_NeverReturnedToNewCaller(t *testing.T) {
	t.Parallel()

	store := &blockingToolStore{
		saveStarted: make(chan struct{}),
		release:     make(chan struct{}),
	}

	var calls int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		n := atomic.AddInt64(&calls, 1)
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: fmt.Sprintf("generation_%d", n)}}}, nil
	}
	cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)

	type result struct {
		got []mcp.Tool
		err error
	}
	firstDone := make(chan result, 1)
	go func() {
		got, err := cache.GetTools(context.Background(), "srv")
		firstDone <- result{got: got, err: err}
	}()

	<-store.saveStarted // the first fetch published its entry and is now blocked in Save

	cache.Invalidate("srv") // races the still-in-flight store write

	secondDone := make(chan result, 1)
	go func() {
		got, err := cache.GetTools(context.Background(), "srv")
		secondDone <- result{got: got, err: err}
	}()

	close(store.release) // let the first fetch's store write complete

	first := <-firstDone
	second := <-secondDone

	if first.err != nil {
		t.Fatalf("first GetTools: %v", first.err)
	}
	if second.err != nil {
		t.Fatalf("second GetTools: %v", second.err)
	}
	if len(first.got) != 1 || first.got[0].Name == "generation_1" {
		t.Errorf("first GetTools = %+v, want the fresh (post-invalidation) generation, not the stale one Save was blocked publishing", first.got)
	}
	if len(second.got) != 1 || second.got[0].Name == "generation_1" {
		t.Errorf("second GetTools = %+v, want the fresh (post-invalidation) generation, never the stale one the blocked fetch produced", second.got)
	}
	calls2 := atomic.LoadInt64(&calls)
	if calls2 < 2 {
		t.Errorf("fetcher called %d times, want at least 2 (the discarded stale fetch, then a fresh one)", calls2)
	}

	// The store's final state: no trace of the stale generation_1 write the
	// blocked Save produced (the post-Save recheck must have deleted it),
	// and exactly the tools from whichever fetch's generation last actually
	// landed as current — never silently missing, and never clobbered back
	// to an empty or stale state by the recheck's own delete firing on a
	// save it should not have.
	wantName := fmt.Sprintf("generation_%d", calls2)
	tools, exists := store.state()
	if !exists {
		t.Fatalf("store ended up with nothing persisted, want the final generation (%s) to be persisted", wantName)
	}
	if len(tools) != 1 || tools[0].Name != wantName {
		t.Errorf("store ended up with %+v persisted, want exactly [%s] — the stale generation_1 write must have been "+
			"deleted once superseded by the concurrent Invalidate, and the later, legitimately-saved write must never "+
			"be touched by that deletion", tools, wantName)
	}
}

// ---- sharedFetch: a caller's own context, not the leader's, bounds it ------

// TestToolCache_GetTools_LeaderCtxCanceled_SecondCallerWithLiveCtxStillGetsResult
// drives sharedFetch's core contract: the leader — whichever caller's
// context happens to start the shared singleflight fetch — has that context
// stripped of cancellation before it is used to run the fetch itself (see
// sharedFetch's own doc). It cancels the LEADER's own context while the
// fetch is still blocked in the fetcher, and verifies two things: the leader
// itself returns promptly with an error wrapping context.Canceled (never
// waiting for the fetch to finish), and a SECOND caller — joining the same
// in-flight fetch with its own, live context — still receives the fetch's
// result once it completes. The fetcher is asserted to have run exactly
// once: the leader's cancellation must not have severed, restarted, or
// duplicated it.
func TestToolCache_GetTools_LeaderCtxCanceled_SecondCallerWithLiveCtxStillGetsResult(t *testing.T) {
	t.Parallel()

	var calls int64
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		once.Do(func() { close(started) })
		<-release
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "shared_tool"}}}, nil
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)

	leaderCtx, leaderCancel := context.WithCancel(context.Background())
	type result struct {
		got []mcp.Tool
		err error
	}
	leaderDone := make(chan result, 1)
	go func() {
		got, err := cache.GetTools(leaderCtx, "srv")
		leaderDone <- result{got: got, err: err}
	}()

	<-started // the leader's call started the shared fetch and is now blocked

	leaderCancel()

	select {
	case res := <-leaderDone:
		if res.err == nil {
			t.Fatalf("leader GetTools = %+v, err = nil, want an error wrapping context.Canceled", res.got)
		}
		if !errors.Is(res.err, context.Canceled) {
			t.Errorf("leader error = %v, want it to wrap context.Canceled", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("leader GetTools did not return promptly after its own context was canceled — " +
			"it must not wait for the shared fetch, which is still blocked")
	}

	// The second caller joins the same in-flight fetch with a live context —
	// or, if it happens to arrive after that fetch has already completed and
	// been forgotten by tc.sf, starts a fresh singleflight round whose own
	// callback immediately finds the just-published entry already fresh via
	// sharedFetch's own freshness recheck (see that method's own doc) and
	// returns it without ever calling the fetcher again. Either way the
	// fetcher is called exactly once and the second caller gets
	// "shared_tool" back — this used to need a barrier plus a bounded
	// runtime.Gosched() spin plus a fixed sleep to reliably exercise the
	// FIRST of those two paths specifically; the recheck makes which path is
	// actually taken irrelevant to the outcome, so no synchronization beyond
	// launching the goroutine is needed before closing release.
	secondDone := make(chan result, 1)
	go func() {
		got, err := cache.GetTools(context.Background(), "srv")
		secondDone <- result{got: got, err: err}
	}()

	close(release) // let the shared fetch complete

	select {
	case res := <-secondDone:
		if res.err != nil {
			t.Fatalf("second caller GetTools: %v", res.err)
		}
		if len(res.got) != 1 || res.got[0].Name != "shared_tool" {
			t.Errorf("second caller GetTools = %+v, want [shared_tool] — the shared fetch's own result", res.got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("second caller GetTools did not return after the shared fetch completed")
	}

	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("fetcher called %d times, want exactly 1 — the leader's cancellation must not have restarted or duplicated the shared fetch", got)
	}
}

// TestToolCache_GetTools_OwnCtxDeadlineExceeded_ReturnsPromptly_FetchContinues
// is the single-caller counterpart of the test above: a caller whose OWN
// context has a short deadline must get ctx.Err() back (wrapped) once that
// deadline passes, without waiting for the shared fetch — which, since it
// runs detached from any one caller's context (see sharedFetch's own doc),
// keeps running to completion regardless. A second, independent GetTools
// call made after the fetch completes confirms it was never severed: it
// reads the fresh, now-cached entry directly, with no further fetcher call.
func TestToolCache_GetTools_OwnCtxDeadlineExceeded_ReturnsPromptly_FetchContinues(t *testing.T) {
	t.Parallel()

	var calls int64
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		once.Do(func() { close(started) })
		<-release
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "shared_tool"}}}, nil
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)

	shortCtx, shortCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer shortCancel()

	type result struct {
		got []mcp.Tool
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := cache.GetTools(shortCtx, "srv")
		done <- result{got: got, err: err}
	}()

	<-started // the fetch has started and is now blocked

	select {
	case res := <-done:
		if res.err == nil {
			t.Fatalf("GetTools = %+v, err = nil, want an error once the caller's own deadline elapses", res.got)
		}
		if !errors.Is(res.err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetTools did not return promptly after its own deadline elapsed")
	}

	close(release) // let the still-running fetch complete

	// entryFor's own freshness check (against tc.entries) and its fetch-on-miss
	// call into sharedFetch (against tc.sf's singleflight map) are two
	// separate, non-atomic steps: this follow-up call can reach its OWN
	// freshness check while the abandoned fetch is still completing, observe
	// "not fresh yet", and only THEN reach sharedFetch — by which point the
	// abandoned call may already have finished and been forgotten by tc.sf,
	// so this call starts a fresh singleflight round of its own rather than
	// joining the old one. Before sharedFetch's own freshness recheck existed
	// (see that method's own doc), this was a real, narrow TOCTOU: a fresh
	// round's callback ran the fetcher again unconditionally, so this test
	// needed a bounded runtime.Gosched() spin plus a fixed sleep here to
	// reliably let the abandoned fetch finish publishing BEFORE this call's
	// own freshness check ran, and so observe the entry as already fresh
	// instead of triggering a second fetch. The recheck closes that gap in
	// the production code itself: even a freshly-started round's callback
	// now re-checks tc.entries as its very first action and returns the
	// already-published entry directly — so no synchronization is needed
	// here at all, regardless of how entryFor's own outer check and
	// sharedFetch happen to interleave with the abandoned fetch completing.
	got, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools (after fetch completes): %v", err)
	}
	if len(got) != 1 || got[0].Name != "shared_tool" {
		t.Errorf("GetTools = %+v, want [shared_tool] from the fetch that kept running after the first caller gave up", got)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("fetcher called %d times, want exactly 1 — the abandoned caller must not have caused a second fetch", got)
	}
}

// ---- RefreshServer shares tc.sf's key with entryFor's own fetch path -------

// TestToolCache_RefreshServer_ConcurrentWithGetTools_FetcherCalledOnce
// verifies RefreshServer and a concurrent GetTools cache miss for the same
// server ID go through the SAME tc.sf singleflight key (sharedFetch) and
// therefore never run two independent fetches concurrently: whichever call
// reaches tc.sf.DoChan first becomes the leader and the other simply joins
// its result — see RefreshServer's own doc for why joining is an acceptable
// substitute for the forced refetch it would otherwise start.
//
// Unlike the singleflight tests above, this one's synchronization is NOT
// working around the old entryFor/sharedFetch TOCTOU (see sharedFetch's own
// "freshness recheck" doc) — RefreshServer always passes force=true, which
// deliberately SKIPS that recheck (see sharedFetch's and RefreshServer's own
// docs: RefreshServer must still force a genuine fetch even when an entry is
// already fresh). So if RefreshServer's own goroutine were simply slow to
// reach tc.sf.DoChan — arriving only after GetTools' in-flight call has
// already completed and been forgotten — it would start a second, genuinely
// independent, forced fetch of its own, which this test's core assertion
// (fetcher called exactly once) is specifically about ruling out. Rather
// than give the RefreshServer goroutine a head start via a bounded
// runtime.Gosched() spin plus a fixed sleep and hope it lands in time, this
// test installs sharedFetchJoinedHookForTest (SetSharedFetchJoinedHookForTest,
// filtered to RefreshServer's own call via a ctx marker only it carries) and
// waits on a channel that hook closes: a structural proof RefreshServer's
// own tc.sf.DoChan call has already registered — as GetTools' round's joiner,
// since that round is still blocked inside the fetcher at this point — before
// release is ever closed.
func TestToolCache_RefreshServer_ConcurrentWithGetTools_FetcherCalledOnce(t *testing.T) {
	// Deliberately not t.Parallel(): installs the package-level
	// sharedFetchJoinedHookForTest test hook, which is shared across every
	// ToolCache in this test binary.

	var calls int64
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		once.Do(func() { close(started) })
		<-release
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "shared_tool"}}}, nil
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)

	getDone := make(chan error, 1)
	go func() {
		_, err := cache.GetTools(context.Background(), "srv")
		getDone <- err
	}()

	<-started // GetTools' cache miss became the singleflight leader

	// refreshCallKey marks the ctx this test passes to RefreshServer, so the
	// joined hook below can attribute a join specifically to RefreshServer's
	// own tc.sf.DoChan call — not GetTools' own (earlier) one, which the hook
	// would otherwise also fire for.
	type refreshCallKey struct{}
	joined := make(chan struct{})
	var joinedOnce sync.Once
	mcp.SetSharedFetchJoinedHookForTest(func(ctx context.Context) {
		if ctx.Value(refreshCallKey{}) == nil {
			return
		}
		joinedOnce.Do(func() { close(joined) })
	})
	defer mcp.SetSharedFetchJoinedHookForTest(nil)

	refreshCtx := context.WithValue(context.Background(), refreshCallKey{}, true)
	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- cache.RefreshServer(refreshCtx, "srv")
	}()

	select {
	case <-joined:
		// RefreshServer's own tc.sf.DoChan call has registered — it has
		// joined GetTools' still in-flight round for "srv" — so releasing
		// the fetcher now cannot race RefreshServer into starting an
		// independent forced fetch of its own.
	case <-time.After(2 * time.Second):
		t.Fatal("RefreshServer did not join the in-flight singleflight round within 2s")
	}
	close(release)

	if err := <-getDone; err != nil {
		t.Errorf("GetTools: %v", err)
	}
	if err := <-refreshDone; err != nil {
		t.Errorf("RefreshServer: %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("fetcher called %d times, want exactly 1 (RefreshServer joined GetTools' in-flight fetch)", got)
	}

	// A follow-up GetTools call must be served from the now-cached, fresh
	// entry both calls above just populated — no further fetcher call at
	// all, proving the shared result was actually published to tc.entries
	// (not merely handed back transiently to the two original callers).
	got, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("follow-up GetTools: %v", err)
	}
	if len(got) != 1 || got[0].Name != "shared_tool" {
		t.Errorf("follow-up GetTools = %+v, want [shared_tool] served from the cache", got)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("fetcher called %d times after the follow-up GetTools, want still exactly 1", got)
	}
}

// ---- sharedFetch's own internal freshness recheck (force=false) -----------

// TestToolCache_SharedFetch_ForceFalse_FreshnessRecheck_ReturnsWithoutFetching
// isolates sharedFetch's own INNER freshness recheck (see that method's own
// doc) from entryFor's separate, OUTER one: it calls
// SharedFetchToolsForTest(force=false) DIRECTLY — bypassing entryFor and its
// own freshness check entirely — against a cache whose entry was populated
// via SetTools moments earlier. If sharedFetch's singleflight callback did
// not perform its own recheck, this call would unconditionally invoke the
// fetcher (a fetcher that fails the test if ever called); since it is
// expected to notice the already-fresh entry as the very first thing its
// callback does, and return it directly, the fetcher must never run at all.
// This is the deterministic replacement for what previously needed a
// "settle" sleep across a real TOCTOU window (see the tests above) — no
// timing is involved here at all, only sharedFetch's own recheck.
func TestToolCache_SharedFetch_ForceFalse_FreshnessRecheck_ReturnsWithoutFetching(t *testing.T) {
	t.Parallel()

	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		return nil, errors.New("fetcher must not be called — sharedFetch's own freshness recheck must return the already-fresh entry directly")
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)
	cache.SetTools("srv", []mcp.Tool{{Name: "already_fresh"}})

	got, err := cache.SharedFetchToolsForTest(context.Background(), "srv", false)
	if err != nil {
		t.Fatalf("SharedFetchToolsForTest(force=false): %v", err)
	}
	if len(got) != 1 || got[0].Name != "already_fresh" {
		t.Errorf("SharedFetchToolsForTest(force=false) = %+v, want [already_fresh] returned via the recheck, without fetching", got)
	}
}

// TestToolCache_SharedFetch_ForceTrue_AlwaysFetchesEvenWhenEntryIsFresh is
// the force=true counterpart: RefreshServer's whole contract is "re-fetch
// regardless of freshness" (see its own doc), so sharedFetch's freshness
// recheck must never apply when force is true — even against the exact same
// already-fresh entry the test above proves force=false is satisfied by
// without ever calling the fetcher.
func TestToolCache_SharedFetch_ForceTrue_AlwaysFetchesEvenWhenEntryIsFresh(t *testing.T) {
	t.Parallel()

	var calls int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "forced_refetch"}}}, nil
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)
	cache.SetTools("srv", []mcp.Tool{{Name: "already_fresh"}})

	got, err := cache.SharedFetchToolsForTest(context.Background(), "srv", true)
	if err != nil {
		t.Fatalf("SharedFetchToolsForTest(force=true): %v", err)
	}
	if len(got) != 1 || got[0].Name != "forced_refetch" {
		t.Errorf("SharedFetchToolsForTest(force=true) = %+v, want [forced_refetch] — force must always fetch, "+
			"even when an entry is already fresh", got)
	}
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Errorf("fetcher called %d times, want exactly 1 (force=true must never be satisfied by the freshness recheck)", n)
	}
}

// TestToolCache_RefreshServer_ForcesFetchEvenWhenEntryIsFresh is the
// RefreshServer-level counterpart of the direct sharedFetch test above,
// confirming the force=true plumbing actually reaches sharedFetch through
// RefreshServer's own public entry point, not merely when called directly.
func TestToolCache_RefreshServer_ForcesFetchEvenWhenEntryIsFresh(t *testing.T) {
	t.Parallel()

	var calls int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "forced_refetch"}}}, nil
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)
	cache.SetTools("srv", []mcp.Tool{{Name: "already_fresh"}})

	if err := cache.RefreshServer(context.Background(), "srv"); err != nil {
		t.Fatalf("RefreshServer: %v", err)
	}
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Errorf("fetcher called %d times, want exactly 1 — RefreshServer must force a fetch even though SetTools left a fresh entry", n)
	}

	got, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools after RefreshServer: %v", err)
	}
	if len(got) != 1 || got[0].Name != "forced_refetch" {
		t.Errorf("GetTools after RefreshServer = %+v, want [forced_refetch]", got)
	}
}

// ---- Process-wide (per-ToolCache) fetch concurrency cap --------------------

// TestToolCache_FetchConcurrencyCap_AtMostMaxConcurrentFetches drives
// GetTools for mcp.MaxConcurrentToolsListFetches+1 distinct, never-cached
// server IDs concurrently, each blocked in its own fetcher call, and
// verifies at most mcp.MaxConcurrentToolsListFetches of them are ever
// running inside the fetcher at the same time — the (servers-1)th fetcher
// must remain blocked, waiting for a free tc.fetchSem slot, until one of the
// first mcp.MaxConcurrentToolsListFetches releases it.
//
// Proving the (servers)th fetcher has NOT entered within a bounded window is
// an assertion about an ABSENCE, which fundamentally needs some wait to be
// confident of — this is not a "settle" sleep papering over a production
// race (there is no race here: the semaphore's own blocking is exactly what
// is under test), it is the standard, unavoidable shape of a "did not
// happen" assertion in a concurrent test.
func TestToolCache_FetchConcurrencyCap_AtMostMaxConcurrentFetches(t *testing.T) {
	t.Parallel()

	const servers = mcp.MaxConcurrentToolsListFetches + 1

	var active int32
	var maxActive int32
	entered := make(chan struct{}, servers)
	release := make(chan struct{})

	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		n := atomic.AddInt32(&active, 1)
		for {
			cur := atomic.LoadInt32(&maxActive)
			if n <= cur {
				break
			}
			if atomic.CompareAndSwapInt32(&maxActive, cur, n) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		atomic.AddInt32(&active, -1)
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "t"}}}, nil
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)

	var wg sync.WaitGroup
	wg.Add(servers)
	for i := 0; i < servers; i++ {
		go func(i int) {
			defer wg.Done()
			if _, err := cache.GetTools(context.Background(), fmt.Sprintf("srv-%d", i)); err != nil {
				t.Errorf("GetTools(srv-%d): %v", i, err)
			}
		}(i)
	}

	for i := 0; i < mcp.MaxConcurrentToolsListFetches; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d expected fetchers entered within 2s — the concurrency cap allowed too few concurrent fetches", i, mcp.MaxConcurrentToolsListFetches)
		}
	}

	select {
	case <-entered:
		t.Fatalf("a %dth fetcher entered concurrently, want at most %d — the concurrency cap did not hold", mcp.MaxConcurrentToolsListFetches+1, mcp.MaxConcurrentToolsListFetches)
	case <-time.After(200 * time.Millisecond):
		// Expected: the extra server's fetch remains blocked on tc.fetchSem.
	}

	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&maxActive); got > int32(mcp.MaxConcurrentToolsListFetches) {
		t.Errorf("max concurrent fetches observed = %d, want at most %d", got, mcp.MaxConcurrentToolsListFetches)
	}
}

// ---- Fetch timeout is overridable and enforced -----------------------------

// TestToolCache_SharedFetch_TimeoutOverride_SlowFetchEndsWithDeadlineExceeded
// overrides ToolCache's fetchTimeout (SetFetchTimeoutForTest — production
// code defaults it to toolsListFetchTimeout, 2 minutes) to a short duration,
// drives a fetcher that respects ctx and blocks until ITS OWN ctx is done,
// and verifies GetTools ends with an error wrapping context.DeadlineExceeded
// once that shortened budget elapses — without the caller's own context
// (context.Background() here, with no deadline of its own) ever being what
// bounds it. This exercises the real 2-minute production timeout path
// without waiting out the real duration.
func TestToolCache_SharedFetch_TimeoutOverride_SlowFetchEndsWithDeadlineExceeded(t *testing.T) {
	t.Parallel()

	fetcher := func(ctx context.Context, _ string) (*mcp.ToolListing, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)
	cache.SetFetchTimeoutForTest(50 * time.Millisecond)

	type result struct {
		got []mcp.Tool
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := cache.GetTools(context.Background(), "srv")
		done <- result{got: got, err: err}
	}()

	select {
	case res := <-done:
		if res.err == nil {
			t.Fatalf("GetTools = %+v, err = nil, want an error once the overridden fetch timeout elapses", res.got)
		}
		if !errors.Is(res.err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", res.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GetTools did not return within 2s of the overridden 50ms fetch timeout elapsing")
	}
}

// ---- sharedFetch's fetched return value ------------------------------------

// TestToolCache_SharedFetch_FetchedReportsWhetherARealFetchHappened drives
// sharedFetch (via SharedFetchForTest) through all three combinations of
// force and entry freshness that determine its fetched return value — see
// sharedFetch's own doc: only a round that actually reaches
// fetchAndPublish, whether because force was true or because force was
// false against a missing/stale entry, ever reports fetched=true; the
// force=false shortcut against an already-fresh entry is the one case that
// does not.
func TestToolCache_SharedFetch_FetchedReportsWhetherARealFetchHappened(t *testing.T) {
	t.Parallel()

	var calls int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "fetched_tool"}}}, nil
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)

	// force=false against a missing entry: genuinely fetches.
	fetched, err := cache.SharedFetchForTest(context.Background(), "srv", false)
	if err != nil {
		t.Fatalf("SharedFetchForTest(force=false, missing entry): %v", err)
	}
	if !fetched {
		t.Error("SharedFetchForTest(force=false, missing entry) fetched = false, want true")
	}
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Fatalf("fetcher called %d times, want 1", n)
	}

	// force=false against the now-fresh entry: returns via the shortcut,
	// never fetching.
	fetched, err = cache.SharedFetchForTest(context.Background(), "srv", false)
	if err != nil {
		t.Fatalf("SharedFetchForTest(force=false, fresh entry): %v", err)
	}
	if fetched {
		t.Error("SharedFetchForTest(force=false, fresh entry) fetched = true, want false")
	}
	if n := atomic.LoadInt64(&calls); n != 1 {
		t.Fatalf("fetcher called %d times after the fresh-entry call, want still 1", n)
	}

	// force=true against the same fresh entry: always fetches regardless.
	fetched, err = cache.SharedFetchForTest(context.Background(), "srv", true)
	if err != nil {
		t.Fatalf("SharedFetchForTest(force=true, fresh entry): %v", err)
	}
	if !fetched {
		t.Error("SharedFetchForTest(force=true, fresh entry) fetched = false, want true")
	}
	if n := atomic.LoadInt64(&calls); n != 2 {
		t.Fatalf("fetcher called %d times after the forced call, want 2", n)
	}
}

// ---- RefreshServer retries once when it joins a non-fetching round --------

// TestToolCache_RefreshServer_JoinsNonFetchingRound_RetriesWithForce drives
// the scenario sharedFetch's fetched return value exists for (see its own
// doc and RefreshServer's own doc): RefreshServer(force=true) joins an
// ALREADY in-flight force=false singleflight round for the same serverID
// that is about to return an already-fresh entry via its own shortcut,
// without ever reaching fetchAndPublish. RefreshServer must notice that
// (fetched=false) and run a second, genuinely forced round of its own —
// proving RefreshServer's "re-fetch right now, regardless of what is
// already cached" contract holds even when the very first round it ends up
// part of did not honor it.
//
// The two rounds are pinned to a deterministic ordering via
// SetSharedFetchFreshEntryHookForTest (see that hook's own doc): without
// it, the force=false round's own shortcut — a single RLock/RUnlock —
// completes so close to instantly that no amount of goroutine scheduling
// could reliably land RefreshServer's own tc.sf.DoChan call while it is
// still in flight.
//
// Proving round 1 is merely non-fetching (fetched=false, asserted below) is
// not, on its own, proof that RefreshServer ever actually joined it: since
// SetTools left "srv" fresh, a RefreshServer whose own tc.sf.DoChan call
// simply arrived AFTER round 1 had already been released and forgotten by
// tc.sf would start its own, entirely independent force=true round — which
// also reaches fetchAndPublish exactly once, so fetches == 1 alone cannot
// distinguish "RefreshServer joined round 1, noticed fetched=false, and
// retried" from "RefreshServer never joined round 1 at all". Two test hooks
// close that gap: SetSharedFetchJoinedHookForTest proves, via a channel this
// test blocks on before ever closing releaseShortcut, that RefreshServer's
// own tc.sf.DoChan call registered while round 1 was still in flight (so it
// could only have joined it, never started an independent one); and
// SetSharedFetchResultHookForTest counts how many times RefreshServer itself
// (identified by a ctx marker only its own calls carry) observed
// fetched=false — which must be exactly 1, proving both that it joined round
// 1's non-fetching result and that its own retry (round 2) is what actually
// produced the one fetch fetches ends up counting.
func TestToolCache_RefreshServer_JoinsNonFetchingRound_RetriesWithForce(t *testing.T) {
	// Deliberately not t.Parallel(): installs package-level test hooks
	// shared across every ToolCache in this test binary.

	reachedShortcut := make(chan struct{})
	releaseShortcut := make(chan struct{})
	mcp.SetSharedFetchFreshEntryHookForTest(func() {
		close(reachedShortcut)
		<-releaseShortcut
	})
	defer mcp.SetSharedFetchFreshEntryHookForTest(nil)

	// refreshCallKey marks the ctx this test passes to RefreshServer, so the
	// joined and result hooks below can attribute what they observe
	// specifically to RefreshServer's own sharedFetch calls — not round 1's
	// own leader call (made with a plain context.Background()), which both
	// hooks would otherwise also fire for.
	type refreshCallKey struct{}
	joinedRound1 := make(chan struct{})
	var joinedOnce sync.Once
	mcp.SetSharedFetchJoinedHookForTest(func(ctx context.Context) {
		if ctx.Value(refreshCallKey{}) == nil {
			return
		}
		joinedOnce.Do(func() { close(joinedRound1) })
	})
	defer mcp.SetSharedFetchJoinedHookForTest(nil)

	var refreshFetchedFalse int64
	mcp.SetSharedFetchResultHookForTest(func(ctx context.Context, fetched bool) {
		if ctx.Value(refreshCallKey{}) == nil {
			return
		}
		if !fetched {
			atomic.AddInt64(&refreshFetchedFalse, 1)
		}
	})
	defer mcp.SetSharedFetchResultHookForTest(nil)

	var fetches int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		n := atomic.AddInt64(&fetches, 1)
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: fmt.Sprintf("refetch_%d", n)}}}, nil
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)
	cache.SetTools("srv", []mcp.Tool{{Name: "already_fresh"}})

	// Round 1: a force=false call that finds the entry fresh and blocks
	// inside the shortcut, still registered as the in-flight singleflight
	// leader for "srv".
	nonForcedDone := make(chan struct{})
	var nonForcedFetched bool
	var nonForcedErr error
	go func() {
		defer close(nonForcedDone)
		nonForcedFetched, nonForcedErr = cache.SharedFetchForTest(context.Background(), "srv", false)
	}()
	<-reachedShortcut

	// Round 2: RefreshServer(force=true), started while round 1 is still
	// blocked inside the hook — it can only join round 1's already
	// in-flight singleflight call for the same key, never start an
	// independent one of its own, per tc.sf's own per-key deduplication
	// (see its field doc). RefreshServer's first sharedFetch call therefore
	// blocks on round 1's own result channel and cannot reach its retry
	// branch until releaseShortcut is closed below.
	refreshCtx := context.WithValue(context.Background(), refreshCallKey{}, true)
	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- cache.RefreshServer(refreshCtx, "srv")
	}()

	select {
	case <-joinedRound1:
		// RefreshServer's own tc.sf.DoChan call has registered while round 1
		// is still blocked inside the shortcut — a structural guarantee it
		// joined round 1 rather than merely racing to start an independent
		// round of its own.
	case <-time.After(2 * time.Second):
		t.Fatal("RefreshServer did not join round 1's in-flight singleflight call within 2s")
	}
	close(releaseShortcut)

	<-nonForcedDone
	if nonForcedErr != nil {
		t.Fatalf("round 1 (force=false): %v", nonForcedErr)
	}
	if nonForcedFetched {
		t.Fatal("round 1 (force=false) fetched = true, want false — it must have taken the freshness shortcut for this test to exercise anything")
	}

	if err := <-refreshDone; err != nil {
		t.Fatalf("RefreshServer: %v", err)
	}

	// RefreshServer must have actually observed round 1's non-fetching
	// result exactly once — proving the retry path this test targets was
	// genuinely taken, not merely that some fetch happened to run.
	if n := atomic.LoadInt64(&refreshFetchedFalse); n != 1 {
		t.Errorf("RefreshServer observed fetched=false %d times, want exactly 1 (round 1's non-fetching result, which its own retry logic must react to)", n)
	}

	// RefreshServer must have driven a genuine fetch — round 1 alone never
	// reached the fetcher at all (fetched=false, asserted above), so the
	// only way fetches could be non-zero is RefreshServer's own retry.
	if n := atomic.LoadInt64(&fetches); n != 1 {
		t.Errorf("fetcher called %d times, want exactly 1 (RefreshServer's own retry after joining round 1's non-fetching result)", n)
	}

	got, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("follow-up GetTools: %v", err)
	}
	if len(got) != 1 || got[0].Name != "refetch_1" {
		t.Errorf("follow-up GetTools = %+v, want [refetch_1] — RefreshServer's own retried fetch must have been published", got)
	}
}

// ---- RefreshServer's retry loop is bounded and ctx-aware -------------------

// TestToolCache_RefreshServer_AlreadyCanceledContext_ReturnsPromptly_NeverFetches
// drives RefreshServer with a context that is already canceled before the
// call is even made, and asserts it returns an error wrapping the ctx error
// immediately — without ever invoking the fetcher — rather than spending an
// attempt on a round it can no longer use. This is RefreshServer's own loop
// re-checking ctx.Err() at the top of every iteration, the same discipline
// entryFor's own analogous retry loop already has (see both loops' own
// docs).
func TestToolCache_RefreshServer_AlreadyCanceledContext_ReturnsPromptly_NeverFetches(t *testing.T) {
	t.Parallel()

	var fetches int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&fetches, 1)
		return &mcp.ToolListing{}, nil
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := cache.RefreshServer(ctx, "srv")
	if err == nil {
		t.Fatal("RefreshServer(already-canceled ctx) error = nil, want a wrapped context.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("RefreshServer(already-canceled ctx) error = %v, want it to wrap context.Canceled", err)
	}
	if n := atomic.LoadInt64(&fetches); n != 0 {
		t.Errorf("fetcher called %d times, want 0 — an already-canceled context must never spend an attempt on the fetcher", n)
	}
}
