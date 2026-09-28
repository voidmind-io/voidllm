package mcp_test

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---- resolveTTL: direct, no wall clock --------------------------------------

// TestToolCache_ResolveTTL_Table drives ToolCache.resolveTTL directly for
// every CacheHint shape MCP 2026-07-28 §5 defines, plus the two production
// ambiguities CacheHint.TTLMsSet and cacheEntry.neverExpires exist to
// resolve. Testing resolveTTL directly, rather than only observing its
// effect through GetTools over real wall-clock time, shows the exact same
// property without any sleep at all — the guidance this suite follows
// throughout for anything not tied to the one deliberate one-second floor.
func TestToolCache_ResolveTTL_Table(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		maxAge           time.Duration
		hint             mcp.CacheHint
		wantTTL          time.Duration
		wantNeverExpires bool
	}{
		{
			name:             "no hint at all, maxAge > 0: falls back to maxAge, never-expires stays false",
			maxAge:           time.Hour,
			hint:             mcp.CacheHint{},
			wantTTL:          time.Hour,
			wantNeverExpires: false,
		},
		{
			name:             "no hint at all, maxAge == 0: the pre-existing \"never expires\" semantics",
			maxAge:           0,
			hint:             mcp.CacheHint{},
			wantTTL:          0,
			wantNeverExpires: true,
		},
		{
			name:             "an explicit hint with maxAge == 0 DOES expire — this is the ambiguity TTLMsSet exists to resolve",
			maxAge:           0,
			hint:             mcp.CacheHint{TTLMs: 5000, TTLMsSet: true},
			wantTTL:          5 * time.Second,
			wantNeverExpires: false,
		},
		{
			name:             "an ordinary hint well within [floor, ceiling] is honored exactly",
			maxAge:           time.Hour,
			hint:             mcp.CacheHint{TTLMs: 5000, TTLMsSet: true},
			wantTTL:          5 * time.Second,
			wantNeverExpires: false,
		},
		{
			name:             "a hint below minToolFetchInterval is floored to it",
			maxAge:           time.Hour,
			hint:             mcp.CacheHint{TTLMs: 50, TTLMsSet: true},
			wantTTL:          mcp.MinToolFetchInterval,
			wantNeverExpires: false,
		},
		{
			name:             "ttlMs: 0 is floored to minToolFetchInterval too — the documented spec deviation",
			maxAge:           time.Hour,
			hint:             mcp.CacheHint{TTLMs: 0, TTLMsSet: true},
			wantTTL:          mcp.MinToolFetchInterval,
			wantNeverExpires: false,
		},
		{
			name:             "a hint far above maxUpstreamToolTTL is capped to it",
			maxAge:           time.Hour,
			hint:             mcp.CacheHint{TTLMs: int64(30 * 24 * time.Hour / time.Millisecond), TTLMsSet: true},
			wantTTL:          mcp.MaxUpstreamToolTTL,
			wantNeverExpires: false,
		},
		{
			name:             "ttlMs near math.MaxInt64 does not overflow: capped to maxUpstreamToolTTL, stays positive",
			maxAge:           time.Hour,
			hint:             mcp.CacheHint{TTLMs: math.MaxInt64, TTLMsSet: true},
			wantTTL:          mcp.MaxUpstreamToolTTL,
			wantNeverExpires: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cache := mcp.NewToolCache(func(context.Context, string) (*mcp.ToolListing, error) {
				return nil, errors.New("resolveTTL must never invoke the fetcher")
			}, tc.maxAge)

			ttl, neverExpires := cache.ResolveTTL(tc.hint)
			if ttl != tc.wantTTL {
				t.Errorf("ttl = %s, want %s", ttl, tc.wantTTL)
			}
			if neverExpires != tc.wantNeverExpires {
				t.Errorf("neverExpires = %v, want %v", neverExpires, tc.wantNeverExpires)
			}
			if ttl < 0 {
				t.Errorf("ttl = %s, want a non-negative duration (overflow safety)", ttl)
			}
		})
	}
}

// ---- The one-second floor: a real, generous wait across the boundary ------

// TestToolCache_GetTools_TTLFloor_RealTimeBoundary is the wall-clock
// counterpart of the "below-floor" resolveTTL cases above: it proves the
// floor actually governs entryFor's fresh/stale decision, not merely
// resolveTTL's return value in isolation. minToolFetchInterval is exactly
// one second, so this test cannot avoid a real sleep across that boundary —
// per this suite's own guidance, the wait here is generous (floor plus 300ms
// margin), not a knife's-edge sleep that would flake under scheduler jitter.
//
// Two sub-cases share the exact same wait so the test shows both directions
// at once: TTLMsSet=true with TTLMs=50 floors to the 1s window (so refetches
// after the wait), while TTLMsSet=false with maxAge=1h falls back to the
// 1-hour window (so the SAME wait must NOT trigger a refetch). A single
// sub-case could not distinguish "the floor genuinely takes effect" from
// "GetTools just always refetches after any wait".
func TestToolCache_GetTools_TTLFloor_RealTimeBoundary(t *testing.T) {
	t.Parallel()

	wait := mcp.MinToolFetchInterval + 300*time.Millisecond

	t.Run("TTLMsSet true, floored below maxAge: refetches after the floor elapses", func(t *testing.T) {
		t.Parallel()

		var calls int64
		fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
			atomic.AddInt64(&calls, 1)
			return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "t"}}, Cache: mcp.CacheHint{TTLMs: 50, TTLMsSet: true}}, nil
		}
		cache := mcp.NewToolCache(fetcher, time.Hour)

		if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
			t.Fatalf("GetTools (first): %v", err)
		}
		time.Sleep(wait)
		if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
			t.Fatalf("GetTools (after floor): %v", err)
		}

		if got := atomic.LoadInt64(&calls); got != 2 {
			t.Errorf("fetcher called %d times, want 2 (the floored 1s window must have elapsed)", got)
		}
	})

	t.Run("TTLMsSet false: falls back to maxAge=1h, same wait does not trigger a refetch", func(t *testing.T) {
		t.Parallel()

		var calls int64
		fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
			atomic.AddInt64(&calls, 1)
			return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "t"}}}, nil
		}
		cache := mcp.NewToolCache(fetcher, time.Hour)

		if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
			t.Fatalf("GetTools (first): %v", err)
		}
		time.Sleep(wait)
		if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
			t.Fatalf("GetTools (after wait): %v", err)
		}

		if got := atomic.LoadInt64(&calls); got != 1 {
			t.Errorf("fetcher called %d times, want 1 (no upstream hint: falls back to the 1h maxAge, unaffected by the floor)", got)
		}
	})
}

// TestToolCache_GetTools_ZeroTTLHint_FloorAppliesAcrossRapidCalls verifies
// the documented, deliberate spec deviation: MCP 2026-07-28 §5 says
// ttlMs: 0 means "immediately stale", but ToolCache floors it to
// minToolFetchInterval so ten GetTools calls made back-to-back right after
// the first fetch still collapse to a single upstream request. After the
// floor genuinely elapses (the same generous real wait as above), a second
// fetch does occur — proving this is a floor, not "never expires".
func TestToolCache_GetTools_ZeroTTLHint_FloorAppliesAcrossRapidCalls(t *testing.T) {
	t.Parallel()

	var calls int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "t"}}, Cache: mcp.CacheHint{TTLMs: 0, TTLMsSet: true}}, nil
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)

	for i := 0; i < 10; i++ {
		if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
			t.Fatalf("GetTools (rapid call %d): %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("fetcher called %d times across 10 rapid calls, want 1 (ttlMs:0 must be floored, not honored literally)", got)
	}

	time.Sleep(mcp.MinToolFetchInterval + 300*time.Millisecond)
	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools (after floor elapses): %v", err)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Errorf("fetcher called %d times after the floor elapsed, want 2", got)
	}
}

// ---- persistListing: scope governs Save vs. Delete -------------------------

// fakeToolStoreCall records one Save or Delete invocation a fakeToolStore
// received, in order.
type fakeToolStoreCall struct {
	op       string // "save" or "delete"
	serverID string
	toolsLen int
}

// fakeToolStore is an in-memory ToolStore that logs every Save/Delete call it
// receives, for asserting exactly how persistListing reacts to each
// CacheHint.Scope value — this test suite's storage layer is otherwise real
// SQLite; this is the one deliberate exception, because the task at hand is
// asserting the SEQUENCE of Save/Delete calls a scope transition produces,
// which a real store's eventual on-disk state cannot distinguish from
// (e.g.) "saved twice" vs. "saved once, then left alone".
type fakeToolStore struct {
	mu      sync.Mutex
	loadAll map[string][]mcp.Tool
	calls   []fakeToolStoreCall
	// saveErr, when non-nil, is returned by every Save call instead of nil —
	// used to drive persistListing's own Save-failure branch (see its own
	// doc): saved must be reported false, and fetchAndPublish's post-Save
	// cleanup-Delete block must never run, for a Save that failed.
	saveErr error
}

func (f *fakeToolStore) LoadAll(context.Context) (map[string][]mcp.Tool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loadAll, nil
}

func (f *fakeToolStore) Save(_ context.Context, serverID string, tools []mcp.Tool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeToolStoreCall{op: "save", serverID: serverID, toolsLen: len(tools)})
	return f.saveErr
}

func (f *fakeToolStore) Delete(_ context.Context, serverID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeToolStoreCall{op: "delete", serverID: serverID})
	return nil
}

func (f *fakeToolStore) counts() (saves, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c.op == "save" {
			saves++
		} else {
			deletes++
		}
	}
	return saves, deletes
}

// TestToolCache_PersistListing_ScopeGovernsSaveVsDelete drives three
// successive fetches for the same server ID through RefreshServer, each with
// a different CacheHint.Scope, and asserts the exact Save/Delete count after
// each one:
//
//  1. "public" -> one Save, zero Deletes.
//  2. "private" (simulating an upstream that newly starts reporting private)
//     -> one MORE Delete, Save count unchanged.
//  3. "" (no hint at all — every legacy upstream today) -> one MORE Save,
//     Delete count unchanged — proving "no hint" is treated like "public",
//     not "private" (which would otherwise evict every legacy server's cache
//     from the store on every single fetch).
func TestToolCache_PersistListing_ScopeGovernsSaveVsDelete(t *testing.T) {
	t.Parallel()

	store := &fakeToolStore{}
	var scope string
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "t"}}, Cache: mcp.CacheHint{Scope: scope}}, nil
	}
	cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)

	scope = mcp.CacheScopePublic
	if err := cache.RefreshServer(context.Background(), "srv"); err != nil {
		t.Fatalf("RefreshServer (public): %v", err)
	}
	if saves, deletes := store.counts(); saves != 1 || deletes != 0 {
		t.Fatalf("after public fetch: saves=%d deletes=%d, want saves=1 deletes=0", saves, deletes)
	}

	scope = mcp.CacheScopePrivate
	if err := cache.RefreshServer(context.Background(), "srv"); err != nil {
		t.Fatalf("RefreshServer (private): %v", err)
	}
	if saves, deletes := store.counts(); saves != 1 || deletes != 1 {
		t.Fatalf("after private fetch: saves=%d deletes=%d, want saves=1 deletes=1 (no new Save, exactly one new Delete)", saves, deletes)
	}

	scope = ""
	if err := cache.RefreshServer(context.Background(), "srv"); err != nil {
		t.Fatalf("RefreshServer (no hint): %v", err)
	}
	if saves, deletes := store.counts(); saves != 2 || deletes != 1 {
		t.Fatalf("after no-hint fetch: saves=%d deletes=%d, want saves=2 deletes=1 (\"no hint\" must behave like public, not private)", saves, deletes)
	}
}

// TestToolCache_GetTools_AfterPrivateFetch_ServedFromMemory verifies the
// deliberate design point: a private-scoped listing is NEVER persisted, but
// it IS kept in the in-memory cache — "weiter im Speicher, nie in die DB",
// not "gar nicht cachen". A GetTools call made while the private entry is
// still fresh must return the tools without triggering another upstream
// fetch and without ever calling Save for this server.
func TestToolCache_GetTools_AfterPrivateFetch_ServedFromMemory(t *testing.T) {
	t.Parallel()

	store := &fakeToolStore{}
	var calls int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "private_tool"}}, Cache: mcp.CacheHint{Scope: mcp.CacheScopePrivate}}, nil
	}
	cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)

	got1, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools (first): %v", err)
	}
	if len(got1) != 1 || got1[0].Name != "private_tool" {
		t.Fatalf("GetTools (first) = %+v, want [private_tool]", got1)
	}

	got2, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools (second, still fresh): %v", err)
	}
	if len(got2) != 1 || got2[0].Name != "private_tool" {
		t.Errorf("GetTools (second) = %+v, want it served from memory, unchanged", got2)
	}

	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("fetcher called %d times, want 1 (second GetTools must be served from the still-fresh in-memory entry)", got)
	}
	if saves, _ := store.counts(); saves != 0 {
		t.Errorf("store.Save called %d times, want 0 (a private listing must never be persisted)", saves)
	}
}

// ---- LoadFromStore: unchanged regression coverage ---------------------------

// TestToolCache_LoadFromStore_MaxAgeNonZero_TriggersRefetch is a pure
// regression guard for pre-existing behavior (see LoadFromStore's own doc):
// with maxAge > 0, an entry loaded from the store is considered stale on
// first access (fetchedAt is the zero time), so the first GetTools call
// after LoadFromStore triggers exactly one upstream refetch.
func TestToolCache_LoadFromStore_MaxAgeNonZero_TriggersRefetch(t *testing.T) {
	t.Parallel()

	store := &fakeToolStore{loadAll: map[string][]mcp.Tool{"srv": {{Name: "from_store"}}}}
	var calls int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "from_upstream"}}}, nil
	}
	cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)

	if err := cache.LoadFromStore(context.Background()); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}

	got, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	if len(got) != 1 || got[0].Name != "from_upstream" {
		t.Errorf("GetTools = %+v, want a refetch to have replaced the DB-loaded entry", got)
	}
	if c := atomic.LoadInt64(&calls); c != 1 {
		t.Errorf("fetcher called %d times, want 1", c)
	}
}

// TestToolCache_LoadFromStore_MaxAgeZero_StillTriggersRefetch is the
// maxAge=0 half of the same regression guard, and used to be named
// TestToolCache_LoadFromStore_MaxAgeZero_NoRefetch: it asserted that a
// DB-loaded entry was immediately fresh (neverExpires) when maxAge == 0, so
// GetTools served it forever without ever calling the fetcher. That was the
// bug in review FUND 1 — a DB-loaded entry carries neither headerParams nor
// a CacheableResult hint (ToolStore persists only []Tool), so treating it as
// neverExpires let a tool's x-mcp-header binding (MCP 2026-07-28 §4.3) go
// missing permanently whenever tool_cache_ttl was configured as 0. A
// DB-loaded entry must always be refetched on first access, regardless of
// maxAge, so this test now asserts the opposite: the fetcher IS called
// exactly once, and its result — not the DB placeholder — is what GetTools
// returns.
func TestToolCache_LoadFromStore_MaxAgeZero_StillTriggersRefetch(t *testing.T) {
	t.Parallel()

	store := &fakeToolStore{loadAll: map[string][]mcp.Tool{"srv": {{Name: "from_store"}}}}
	var calls int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "from_upstream"}}}, nil
	}
	cache := mcp.NewPersistentToolCache(fetcher, 0, store)

	if err := cache.LoadFromStore(context.Background()); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}

	got, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	if len(got) != 1 || got[0].Name != "from_upstream" {
		t.Errorf("GetTools = %+v, want a refetch to have replaced the DB-loaded entry even with maxAge == 0", got)
	}
	if c := atomic.LoadInt64(&calls); c != 1 {
		t.Errorf("fetcher called %d times, want 1", c)
	}
}

// ---- SetTools: stays fresh after the per-entry TTL refactor ----------------

// TestSetTools_StaysFreshAfterPerEntryTTLRefactor is a targeted regression
// test for a bug the per-entry-freshness refactor introduced and then fixed
// (see SetTools' own doc): after moving ttl/neverExpires onto each
// cacheEntry individually (to support upstream ttlMs hints), a SetTools-
// populated entry must still resolve to "fresh until maxAge elapses" via the
// same resolveTTL(CacheHint{}) fallback every other no-hint entry uses. A
// fetcher that always errors proves GetTools never falls through to a fetch
// for an entry SetTools just populated.
func TestSetTools_StaysFreshAfterPerEntryTTLRefactor(t *testing.T) {
	t.Parallel()

	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		return nil, errors.New("fetcher must not be called for a SetTools-populated, still-fresh entry")
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)

	cache.SetTools("voidllm", []mcp.Tool{{Name: "builtin_tool"}})

	for i := 0; i < 5; i++ {
		got, err := cache.GetTools(context.Background(), "voidllm")
		if err != nil {
			t.Fatalf("GetTools (call %d): %v", i, err)
		}
		if len(got) != 1 || got[0].Name != "builtin_tool" {
			t.Fatalf("GetTools (call %d) = %+v, want [builtin_tool]", i, got)
		}
	}
}

// ---- A nil-first-page *ToolListing (zero CacheHint) through resolveTTL ------

// TestToolCache_ZeroCacheHintListing_MaxAgeZero_NeverExpires drives a
// fetcher returning exactly what ListTools now returns for a bodyless first
// page (see errToolsListNilBodyMidFetch's own doc and the ListTools test
// covering it directly): a &ToolListing{} whose Cache field is the zero
// CacheHint, TTLMsSet false. Through resolveTTL's own no-hint fallback (see
// its doc), TTLMsSet false with maxAge == 0 must resolve to neverExpires —
// exactly the same fallback any other no-hint upstream (in practice, every
// legacy MCP server today) already gets. A fetcher that fails the test if
// called more than once proves the entry this listing produced is treated
// as never expiring, not as the erroneous "public, TTLMsSet true" default a
// prior bug in ListTools' own CacheHint aggregation would have produced for
// this exact shape (see ListTools' nil-first-page handling).
func TestToolCache_ZeroCacheHintListing_MaxAgeZero_NeverExpires(t *testing.T) {
	t.Parallel()

	var calls int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		if n := atomic.AddInt64(&calls, 1); n > 1 {
			return nil, errors.New("fetcher must not be called again for a neverExpires entry")
		}
		return &mcp.ToolListing{}, nil
	}
	cache := mcp.NewToolCache(fetcher, 0)

	for i := 0; i < 5; i++ {
		got, err := cache.GetTools(context.Background(), "srv")
		if err != nil {
			t.Fatalf("GetTools (call %d): %v", i, err)
		}
		if len(got) != 0 {
			t.Fatalf("GetTools (call %d) = %+v, want an empty listing", i, got)
		}
	}
	if c := atomic.LoadInt64(&calls); c != 1 {
		t.Errorf("fetcher called %d times, want exactly 1 (maxAge == 0 with a zero CacheHint must never expire)", c)
	}
}

// TestToolCache_ZeroCacheHintListing_MaxAgeSet_UsesMaxAge is the maxAge != 0
// counterpart: the SAME &ToolListing{} (zero CacheHint) shape must instead
// resolve to ttl == maxAge, neverExpires false — a second GetTools call made
// after maxAge has elapsed must trigger a genuine refetch, not be served
// from an entry that incorrectly latched onto "never expires" for this
// shape.
func TestToolCache_ZeroCacheHintListing_MaxAgeSet_UsesMaxAge(t *testing.T) {
	t.Parallel()

	const maxAge = 30 * time.Millisecond

	var calls int64
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		return &mcp.ToolListing{}, nil
	}
	cache := mcp.NewToolCache(fetcher, maxAge)

	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools (first call): %v", err)
	}
	if c := atomic.LoadInt64(&calls); c != 1 {
		t.Fatalf("fetcher called %d times after the first call, want 1", c)
	}

	// Immediately afterward the entry is still fresh: no refetch yet.
	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools (immediate second call): %v", err)
	}
	if c := atomic.LoadInt64(&calls); c != 1 {
		t.Fatalf("fetcher called %d times after the immediate second call, want still 1 (entry should still be fresh)", c)
	}

	// >= 5x maxAge — a wide, non-flaky margin: 2x left this test vulnerable
	// to spurious failure under scheduler contention or a slow CI runner,
	// where wall-clock time between the sleep starting and the entry's own
	// fetchedAt-relative freshness check running could plausibly eat into a
	// 2x-only margin.
	time.Sleep(6 * maxAge)

	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools (after maxAge elapsed): %v", err)
	}
	if c := atomic.LoadInt64(&calls); c != 2 {
		t.Errorf("fetcher called %d times after maxAge elapsed, want 2 (maxAge, not neverExpires, must govern this entry's freshness)", c)
	}
}

// ---- persistListing: saved reflects Save's own outcome, not merely "took the Save branch" --

// TestToolCache_PersistListing_SaveError_NoCleanupDeleteAttempted drives a
// store whose Save always fails and verifies two things: the failed Save is
// still attempted (persistListing's Scope-based decision to try saving at
// all is unaffected by whether it then succeeds), and — the actual
// regression this test guards — fetchAndPublish's post-Save cleanup-Delete
// block never runs for it. Before persistListing reported saved=true purely
// because it took the Save branch (a store.Save error was discarded
// entirely), a superseded generation landing after a FAILED Save could still
// trigger that cleanup block and issue a store.Delete for a row Save itself
// never actually wrote.
//
// No concurrent Invalidate is needed to prove this: fetchAndPublish's
// cleanup block is nested entirely inside `if tc.persistListing(...)`, so a
// false return already skips it unconditionally, regardless of whether a
// supersede would otherwise have been detected — see
// TestToolCache_FetchAndPublish_CleanupDelete_UsesOwnContext_NotExpiredFetchCtx
// below for the counterpart that DOES drive a real supersede, against a
// store whose Save succeeds, to reach that same block from the other side.
func TestToolCache_PersistListing_SaveError_NoCleanupDeleteAttempted(t *testing.T) {
	t.Parallel()

	store := &fakeToolStore{saveErr: errors.New("store unavailable")}
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "t"}}}, nil
	}
	cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)

	// A failed store write must not fail the fetch itself: the entry is
	// still served from memory exactly as it would be without a store at
	// all (persistListing's own Save-failure doc).
	got, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	if len(got) != 1 || got[0].Name != "t" {
		t.Errorf("GetTools = %+v, want [t] served from memory despite the store Save failing", got)
	}

	saves, deletes := store.counts()
	if saves != 1 {
		t.Errorf("store.Save called %d times, want exactly 1 (the attempt itself must still happen)", saves)
	}
	if deletes != 0 {
		t.Errorf("store.Delete called %d times, want 0 — no cleanup Delete may be attempted for a Save that failed", deletes)
	}
}

// blockingSaveStore is a ToolStore whose Save blocks until released,
// ignoring the context it is given entirely (deliberately: this fixture
// exists to prove WHICH context fetchAndPublish's post-Save cleanup Delete
// call is given, by having Delete record ctx.Err() at the moment it is
// called — a store that itself respected ctx cancellation would make that
// observation impossible to attribute to one specific ctx). LoadAll is
// unused by every test that uses this fixture.
type blockingSaveStore struct {
	saveStarted chan struct{}
	saveRelease chan struct{}

	mu            sync.Mutex
	deleteCalls   int
	deleteCtxErrs []error
}

func (s *blockingSaveStore) LoadAll(context.Context) (map[string][]mcp.Tool, error) {
	return nil, nil
}

func (s *blockingSaveStore) Save(context.Context, string, []mcp.Tool) error {
	close(s.saveStarted)
	<-s.saveRelease
	return nil
}

func (s *blockingSaveStore) Delete(ctx context.Context, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteCalls++
	s.deleteCtxErrs = append(s.deleteCtxErrs, ctx.Err())
	return nil
}

// TestToolCache_FetchAndPublish_CleanupDelete_UsesOwnContext_NotExpiredFetchCtx
// drives the exact race fetchAndPublish's post-Save recheck-and-delete
// exists to close (see that method's own doc): a concurrent Invalidate
// landing WHILE store.Save is still in flight, discovered only once Save
// returns. It additionally proves the cleanup Delete call itself uses its
// own dedicated context.WithTimeout(context.Background(),
// toolCacheCleanupDeleteTimeout) — never the fetch's own (by then already
// expired) context — by configuring an unusually short fetch timeout, then
// deliberately letting it elapse WHILE Save is still blocked, before the
// invalidation and release that let Save return.
func TestToolCache_FetchAndPublish_CleanupDelete_UsesOwnContext_NotExpiredFetchCtx(t *testing.T) {
	t.Parallel()

	store := &blockingSaveStore{
		saveStarted: make(chan struct{}),
		saveRelease: make(chan struct{}),
	}
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "t"}}}, nil
	}
	cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)
	cache.SetFetchTimeoutForTest(20 * time.Millisecond)

	refreshDone := make(chan error, 1)
	go func() {
		refreshDone <- cache.RefreshServer(context.Background(), "srv")
	}()

	// The fetch itself has already succeeded and the entry already
	// published in memory (fetchAndPublish's own FIRST generation check,
	// before Save, has already passed) by the time Save blocks here.
	<-store.saveStarted

	// Let the fetch's own 20ms budget elapse WHILE Save is still in
	// flight, then invalidate — landing exactly in the window
	// fetchAndPublish's own doc calls out: after the pre-Save generation
	// check, during persistListing's own Save call.
	time.Sleep(100 * time.Millisecond)
	cache.Invalidate("srv")
	close(store.saveRelease)

	if err := <-refreshDone; err != nil {
		t.Fatalf("RefreshServer: %v", err)
	}

	store.mu.Lock()
	deleteCalls := store.deleteCalls
	var deleteCtxErr error
	if len(store.deleteCtxErrs) > 0 {
		deleteCtxErr = store.deleteCtxErrs[0]
	}
	store.mu.Unlock()

	if deleteCalls != 1 {
		t.Fatalf("store.Delete called %d times, want exactly 1 — cleanup must run even though the fetch context already expired", deleteCalls)
	}
	if deleteCtxErr != nil {
		t.Errorf("ctx passed to Delete had Err() = %v, want nil — the cleanup Delete must use its own dedicated, freshly-timed context, never the (by now expired) fetch context", deleteCtxErr)
	}
}
