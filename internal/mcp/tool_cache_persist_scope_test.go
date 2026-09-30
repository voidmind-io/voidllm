package mcp_test

import (
	"context"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers docs/mcp-v2.md Fund 10 (persistListing, tool_cache.go),
// specifically the one branch TestToolCache_PersistListing_ScopeGovernsSaveVsDelete
// (tool_cache_cachehint_test.go) does not exercise at all: a CacheableResult
// hint that WAS offered (CacheHint.TTLMsSet true) but whose Scope names
// neither "public" nor "private" — an upstream that implements
// CacheableResult at all, but sent no usable grant. persistListing must
// neither Save (no basis to treat the listing as safe to share with every
// caller) nor Delete (this is not the "explicitly private" case — nothing
// here says a previously persisted copy has gone stale).
//
// That sibling test's own "no hint at all" case only ever constructs
// mcp.CacheHint{Scope: scope} with TTLMsSet left at its Go zero value
// (false) — so it already correctly exercises "no hint", but never
// constructs a CacheHint with TTLMsSet true and an unusable Scope at all.
// Conflating the two is exactly the mistake persistListing's own doc warns
// against: treating "no hint" (legacy upstream, Save) like "hint present but
// unusable" (neither) would evict every legacy server's cache from the store
// on every single fetch. This file's table makes both cases share nothing
// but a table row, so a future change accidentally merging their branches
// cannot pass both silently.
//
// fakeToolStore and its counts helper are defined once, in
// tool_cache_cachehint_test.go, and reused here (same package, mcp_test).

func TestToolCache_PersistListing_HintPresentButUnusableScope_NeitherSavedNorDeleted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		cache mcp.CacheHint
	}{
		{
			name:  "hint present, Scope is an unrecognized string",
			cache: mcp.CacheHint{TTLMsSet: true, TTLMs: 60_000, Scope: "not-a-recognized-scope"},
		},
		{
			name:  "hint present (TTLMsSet true), Scope left empty entirely",
			cache: mcp.CacheHint{TTLMsSet: true, TTLMs: 60_000, Scope: ""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			store := &fakeToolStore{}
			fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
				return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "a-tool"}}, Cache: tc.cache}, nil
			}
			cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)

			if err := cache.RefreshServer(context.Background(), "server-1"); err != nil {
				t.Fatalf("RefreshServer() error = %v, want nil", err)
			}

			saves, deletes := store.counts()
			if saves != 0 {
				t.Errorf("Save() called %d time(s), want 0 — a hint that was offered but grants nothing gives no "+
					"basis to persist and share this listing with every caller", saves)
			}
			if deletes != 0 {
				t.Errorf("Delete() called %d time(s), want 0 — this is not the \"explicitly private\" case, so a "+
					"previously persisted copy must be left alone, not evicted", deletes)
			}
		})
	}
}

// TestToolCache_PersistListing_NoHintAtAll_IsNotTreatedLikeUnusableHint is
// the direct falsifiable counterpart of the test above: with NO hint at all
// (CacheHint{}, TTLMsSet false — the shape every legacy upstream and every
// modern one that has not implemented CacheableResult yet produces), the
// listing MUST be saved. If this case were ever handled by the same branch
// as "hint present but unusable" above, this assertion — and only this one —
// would flip to a failure.
func TestToolCache_PersistListing_NoHintAtAll_IsNotTreatedLikeUnusableHint(t *testing.T) {
	t.Parallel()

	store := &fakeToolStore{}
	fetcher := func(context.Context, string) (*mcp.ToolListing, error) {
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "a-tool"}}, Cache: mcp.CacheHint{}}, nil
	}
	cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)

	if err := cache.RefreshServer(context.Background(), "server-1"); err != nil {
		t.Fatalf("RefreshServer() error = %v, want nil", err)
	}

	saves, deletes := store.counts()
	if saves != 1 {
		t.Errorf("Save() called %d time(s), want exactly 1 — no hint at all must persist exactly like a legacy "+
			"upstream always did, or every legacy server would be evicted from the store on every fetch", saves)
	}
	if deletes != 0 {
		t.Errorf("Delete() called %d time(s), want 0", deletes)
	}
}
