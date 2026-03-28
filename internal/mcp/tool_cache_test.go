package mcp_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// makeStaticFetcher returns a ToolFetcher that always returns the given tools
// for any alias.
func makeStaticFetcher(tools []mcp.Tool) mcp.ToolFetcher {
	return func(_ context.Context, _ string) ([]mcp.Tool, error) {
		return tools, nil
	}
}

// makeCountingFetcher returns a ToolFetcher that counts how many times it has
// been called per alias and returns the configured tools.
func makeCountingFetcher(tools []mcp.Tool) (mcp.ToolFetcher, *sync.Map) {
	counts := &sync.Map{}
	fetcher := func(_ context.Context, alias string) ([]mcp.Tool, error) {
		v, _ := counts.LoadOrStore(alias, new(int64))
		counter := v.(*int64)
		atomic.AddInt64(counter, 1)
		return tools, nil
	}
	return fetcher, counts
}

// fetchCount returns the number of times the counting fetcher was called for
// alias.
func fetchCount(counts *sync.Map, alias string) int64 {
	v, ok := counts.Load(alias)
	if !ok {
		return 0
	}
	return atomic.LoadInt64(v.(*int64))
}

// ---- GetTools fresh ----------------------------------------------------------

func TestToolCache_GetTools_Fresh(t *testing.T) {
	t.Parallel()

	tools := []mcp.Tool{
		{Name: "tool_one", Description: "First tool"},
		{Name: "tool_two", Description: "Second tool"},
	}

	fetcher, counts := makeCountingFetcher(tools)
	cache := mcp.NewToolCache(fetcher, time.Hour)

	// First call — triggers fetch.
	got, err := cache.GetTools(context.Background(), "myserver")
	if err != nil {
		t.Fatalf("GetTools (first): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len(tools) = %d, want 2", len(got))
	}
	if fetchCount(counts, "myserver") != 1 {
		t.Errorf("fetcher called %d times, want 1", fetchCount(counts, "myserver"))
	}

	// Second call — should return cached.
	got2, err := cache.GetTools(context.Background(), "myserver")
	if err != nil {
		t.Fatalf("GetTools (second): %v", err)
	}
	if len(got2) != 2 {
		t.Errorf("len(tools) cached = %d, want 2", len(got2))
	}
	if fetchCount(counts, "myserver") != 1 {
		t.Errorf("fetcher called %d times after cache hit, want still 1", fetchCount(counts, "myserver"))
	}
}

// ---- GetTools stale ----------------------------------------------------------

func TestToolCache_GetTools_Stale(t *testing.T) {
	t.Parallel()

	tools := []mcp.Tool{{Name: "tool_one"}}
	fetcher, counts := makeCountingFetcher(tools)

	// Very short maxAge so the entry expires almost immediately.
	cache := mcp.NewToolCache(fetcher, time.Millisecond)

	// First fetch.
	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools first: %v", err)
	}

	// Wait for the entry to expire.
	time.Sleep(5 * time.Millisecond)

	// Second fetch — entry is stale, so fetcher must be called again.
	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools second: %v", err)
	}
	if fetchCount(counts, "srv") < 2 {
		t.Errorf("fetcher called %d times, want >= 2 (stale re-fetch)", fetchCount(counts, "srv"))
	}
}

// ---- GetTools maxAge=0 never expires ----------------------------------------

func TestToolCache_GetTools_ZeroMaxAge_NeverExpires(t *testing.T) {
	t.Parallel()

	tools := []mcp.Tool{{Name: "forever"}}
	fetcher, counts := makeCountingFetcher(tools)
	cache := mcp.NewToolCache(fetcher, 0) // 0 = never expire

	for range 5 {
		if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
			t.Fatalf("GetTools: %v", err)
		}
	}
	if fetchCount(counts, "srv") != 1 {
		t.Errorf("fetcher called %d times with 0 maxAge, want 1", fetchCount(counts, "srv"))
	}
}

// ---- GetTools concurrent — double-check pattern prevents multiple fetches ---

func TestToolCache_GetTools_Concurrent(t *testing.T) {
	t.Parallel()

	tools := []mcp.Tool{{Name: "concurrent_tool"}}

	var fetchCallCount int64
	fetcher := func(_ context.Context, _ string) ([]mcp.Tool, error) {
		// Simulate a slightly slow fetch to maximise race window.
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt64(&fetchCallCount, 1)
		return tools, nil
	}

	cache := mcp.NewToolCache(fetcher, time.Hour)

	const goroutines = 20
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

	wg.Wait()

	// The double-check pattern should ensure only one fetch is made.
	if n := atomic.LoadInt64(&fetchCallCount); n != 1 {
		t.Errorf("fetcher called %d times concurrently, want 1 (double-check pattern)", n)
	}
}

// ---- GetTools error propagation ----------------------------------------------

func TestToolCache_GetTools_FetchError(t *testing.T) {
	t.Parallel()

	fetchErr := errors.New("upstream unavailable")
	fetcher := func(_ context.Context, _ string) ([]mcp.Tool, error) {
		return nil, fetchErr
	}

	cache := mcp.NewToolCache(fetcher, time.Hour)
	_, err := cache.GetTools(context.Background(), "broken")
	if !errors.Is(err, fetchErr) {
		t.Errorf("GetTools err = %v, want %v", err, fetchErr)
	}
}

// ---- RefreshServer -----------------------------------------------------------

func TestToolCache_RefreshServer(t *testing.T) {
	t.Parallel()

	callCount := 0
	fetcher := func(_ context.Context, _ string) ([]mcp.Tool, error) {
		callCount++
		return []mcp.Tool{{Name: fmt.Sprintf("tool_v%d", callCount)}}, nil
	}

	cache := mcp.NewToolCache(fetcher, time.Hour)

	// Populate cache.
	got, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	if len(got) != 1 || got[0].Name != "tool_v1" {
		t.Fatalf("initial GetTools = %+v, want tool_v1", got)
	}

	// Force refresh.
	if err := cache.RefreshServer(context.Background(), "srv"); err != nil {
		t.Fatalf("RefreshServer: %v", err)
	}

	// GetTools should now return the refreshed version.
	got2, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools after RefreshServer: %v", err)
	}
	if len(got2) != 1 || got2[0].Name != "tool_v2" {
		t.Errorf("after refresh GetTools = %+v, want tool_v2", got2)
	}
}

// ---- RefreshServer error — old cache preserved --------------------------------

func TestToolCache_RefreshServer_Error_PreservesCache(t *testing.T) {
	t.Parallel()

	originalTools := []mcp.Tool{{Name: "original"}}
	callCount := 0
	fetcher := func(_ context.Context, _ string) ([]mcp.Tool, error) {
		callCount++
		if callCount == 1 {
			return originalTools, nil
		}
		return nil, errors.New("network error")
	}

	cache := mcp.NewToolCache(fetcher, time.Hour)

	// Initial population.
	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools: %v", err)
	}

	// Refresh fails.
	if err := cache.RefreshServer(context.Background(), "srv"); err == nil {
		t.Fatal("RefreshServer expected to return error, got nil")
	}

	// Old cache entry must still be present and serve the original tools.
	got, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools after failed refresh: %v", err)
	}
	if len(got) != 1 || got[0].Name != "original" {
		t.Errorf("GetTools = %+v, want original tools to be preserved", got)
	}
}

// ---- RefreshAll --------------------------------------------------------------

func TestToolCache_RefreshAll(t *testing.T) {
	t.Parallel()

	// Populate two entries.
	fetchCounts := map[string]int{}
	var mu sync.Mutex
	fetcher := func(_ context.Context, alias string) ([]mcp.Tool, error) {
		mu.Lock()
		fetchCounts[alias]++
		mu.Unlock()
		return []mcp.Tool{{Name: alias + "_tool"}}, nil
	}

	cache := mcp.NewToolCache(fetcher, time.Hour)

	for _, alias := range []string{"alpha", "beta", "gamma"} {
		if _, err := cache.GetTools(context.Background(), alias); err != nil {
			t.Fatalf("GetTools(%q): %v", alias, err)
		}
	}

	// Each alias fetched once so far.
	for _, alias := range []string{"alpha", "beta", "gamma"} {
		mu.Lock()
		c := fetchCounts[alias]
		mu.Unlock()
		if c != 1 {
			t.Errorf("fetchCounts[%q] = %d, want 1", alias, c)
		}
	}

	// RefreshAll.
	if err := cache.RefreshAll(context.Background()); err != nil {
		t.Fatalf("RefreshAll: %v", err)
	}

	// Each alias must have been fetched a second time.
	for _, alias := range []string{"alpha", "beta", "gamma"} {
		mu.Lock()
		c := fetchCounts[alias]
		mu.Unlock()
		if c != 2 {
			t.Errorf("fetchCounts[%q] = %d after RefreshAll, want 2", alias, c)
		}
	}
}

func TestToolCache_RefreshAll_PartialError(t *testing.T) {
	t.Parallel()

	// Track per-alias call counts separately to avoid a shared counter race.
	var (
		mu         sync.Mutex
		callCounts = map[string]int{}
	)

	fetcher := func(_ context.Context, alias string) ([]mcp.Tool, error) {
		mu.Lock()
		callCounts[alias]++
		n := callCounts[alias]
		mu.Unlock()
		// "broken" always fails on the second and subsequent calls.
		if alias == "broken" && n > 1 {
			return nil, errors.New("fetch failed")
		}
		return []mcp.Tool{{Name: alias + "_tool"}}, nil
	}

	cache := mcp.NewToolCache(fetcher, time.Hour)

	// Populate both aliases (first call for each succeeds).
	for _, alias := range []string{"ok", "broken"} {
		if _, err := cache.GetTools(context.Background(), alias); err != nil {
			t.Fatalf("GetTools(%q): %v", alias, err)
		}
	}

	// RefreshAll triggers a second fetch for each alias.
	// "broken"'s second fetch fails; "ok"'s second fetch succeeds.
	err := cache.RefreshAll(context.Background())
	if err == nil {
		t.Error("RefreshAll expected non-nil error when one fetch fails")
	}
	if !containsErrMsg(err, "fetch failed") {
		t.Errorf("RefreshAll err = %v, want it to mention 'fetch failed'", err)
	}
}

// ---- Invalidate --------------------------------------------------------------

func TestToolCache_Invalidate(t *testing.T) {
	t.Parallel()

	fetcher := makeStaticFetcher([]mcp.Tool{{Name: "t1"}})
	cache := mcp.NewToolCache(fetcher, time.Hour)

	// Populate.
	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools: %v", err)
	}

	all := cache.GetAllTools()
	if _, ok := all["srv"]; !ok {
		t.Fatal("expected 'srv' in GetAllTools before Invalidate")
	}

	// Invalidate.
	cache.Invalidate("srv")

	all2 := cache.GetAllTools()
	if _, ok := all2["srv"]; ok {
		t.Error("expected 'srv' to be absent from GetAllTools after Invalidate")
	}
}

func TestToolCache_Invalidate_Unknown(t *testing.T) {
	t.Parallel()

	cache := mcp.NewToolCache(makeStaticFetcher(nil), time.Hour)
	// Invalidating an alias that was never fetched must not panic.
	cache.Invalidate("nonexistent")
}

// ---- ToolCount ---------------------------------------------------------------

func TestToolCache_ToolCount(t *testing.T) {
	t.Parallel()

	tools := []mcp.Tool{
		{Name: "t1"},
		{Name: "t2"},
		{Name: "t3"},
	}
	cache := mcp.NewToolCache(makeStaticFetcher(tools), time.Hour)

	// Before fetch — must return 0.
	if n := cache.ToolCount("srv"); n != 0 {
		t.Errorf("ToolCount before fetch = %d, want 0", n)
	}

	// Populate.
	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools: %v", err)
	}

	if n := cache.ToolCount("srv"); n != 3 {
		t.Errorf("ToolCount after fetch = %d, want 3", n)
	}

	// Unknown alias.
	if n := cache.ToolCount("unknown"); n != 0 {
		t.Errorf("ToolCount(unknown) = %d, want 0", n)
	}
}

// ---- GetAllTools is a snapshot -----------------------------------------------

func TestToolCache_GetAllTools_Snapshot(t *testing.T) {
	t.Parallel()

	tools := []mcp.Tool{{Name: "snap1"}, {Name: "snap2"}}
	cache := mcp.NewToolCache(makeStaticFetcher(tools), time.Hour)

	// Populate two aliases.
	for _, alias := range []string{"a", "b"} {
		if _, err := cache.GetTools(context.Background(), alias); err != nil {
			t.Fatalf("GetTools(%q): %v", alias, err)
		}
	}

	snapshot := cache.GetAllTools()

	// Mutate the snapshot — cache internals must not be affected.
	snapshot["a"] = append(snapshot["a"], mcp.Tool{Name: "injected"})
	delete(snapshot, "b")

	// Original cache must be intact.
	all := cache.GetAllTools()
	if len(all) != 2 {
		t.Errorf("GetAllTools() len = %d after snapshot mutation, want 2", len(all))
	}
	if len(all["a"]) != 2 {
		t.Errorf("GetAllTools()[\"a\"] len = %d, want 2 (snapshot mutation leaked)", len(all["a"]))
	}
}

func TestToolCache_GetAllTools_Empty(t *testing.T) {
	t.Parallel()

	cache := mcp.NewToolCache(makeStaticFetcher(nil), time.Hour)
	all := cache.GetAllTools()
	if len(all) != 0 {
		t.Errorf("GetAllTools() on empty cache = %v, want empty map", all)
	}
}

// ---- Multiple aliases independent -------------------------------------------

func TestToolCache_MultipleAliasesIndependent(t *testing.T) {
	t.Parallel()

	fetcher := func(_ context.Context, alias string) ([]mcp.Tool, error) {
		return []mcp.Tool{{Name: alias + "_specific"}}, nil
	}

	cache := mcp.NewToolCache(fetcher, time.Hour)

	toolsA, err := cache.GetTools(context.Background(), "server_a")
	if err != nil {
		t.Fatalf("GetTools(server_a): %v", err)
	}
	toolsB, err := cache.GetTools(context.Background(), "server_b")
	if err != nil {
		t.Fatalf("GetTools(server_b): %v", err)
	}

	if len(toolsA) != 1 || toolsA[0].Name != "server_a_specific" {
		t.Errorf("toolsA = %+v, want server_a_specific", toolsA)
	}
	if len(toolsB) != 1 || toolsB[0].Name != "server_b_specific" {
		t.Errorf("toolsB = %+v, want server_b_specific", toolsB)
	}
}

// ---- copyTools returns independent copy -------------------------------------

func TestToolCache_GetTools_ReturnsCopy(t *testing.T) {
	t.Parallel()

	original := []mcp.Tool{{Name: "original_name"}}
	cache := mcp.NewToolCache(makeStaticFetcher(original), time.Hour)

	got1, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}

	// Mutate the returned slice.
	got1[0].Name = "mutated"

	// Second call should still return the original name.
	got2, err := cache.GetTools(context.Background(), "srv")
	if err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	if got2[0].Name != "original_name" {
		t.Errorf("GetTools after mutation = %q, want %q (mutation should not affect cache)", got2[0].Name, "original_name")
	}
}

// ---- helpers -----------------------------------------------------------------

// containsErrMsg checks whether err's message contains substr.
func containsErrMsg(err error, substr string) bool {
	if err == nil {
		return false
	}
	return len(err.Error()) > 0 && (err.Error() == substr || len(err.Error()) >= len(substr) &&
		stringContains(err.Error(), substr))
}

func stringContains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(s) < len(sub) {
		return false
	}
	for i := range len(s) - len(sub) + 1 {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
