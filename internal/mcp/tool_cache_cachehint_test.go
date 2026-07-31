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
	return nil
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

// TestToolCache_LoadFromStore_MaxAgeZero_NoRefetch is the maxAge=0 half of
// the same regression guard: a DB-loaded entry is immediately fresh
// (neverExpires), so GetTools must serve it without ever calling the
// fetcher.
func TestToolCache_LoadFromStore_MaxAgeZero_NoRefetch(t *testing.T) {
	t.Parallel()

	store := &fakeToolStore{loadAll: map[string][]mcp.Tool{"srv": {{Name: "from_store"}}}}
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		return nil, errors.New("fetcher must not be called when maxAge is 0")
	}
	cache := mcp.NewPersistentToolCache(fetcher, 0, store)

	if err := cache.LoadFromStore(context.Background()); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}

	got, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	if len(got) != 1 || got[0].Name != "from_store" {
		t.Errorf("GetTools = %+v, want the DB-loaded entry served without any refetch", got)
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
