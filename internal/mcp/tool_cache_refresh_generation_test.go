package mcp_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// TestToolCache_RefreshServer_DiscardsResultSupersededByInvalidation drives
// the exact race RefreshServer's generation counter exists to close: a
// refresh that started fetching BEFORE an Invalidate — for example, one
// triggered by an admin rotating this server's credential — must not publish
// (or persist) whatever it fetched once that fetch finally returns, because
// it was fetched under state Invalidate has already superseded. Before the
// generation counter existed, RefreshServer wrote unconditionally on
// success, so this exact sequence resurrected the stale entry Invalidate had
// just deleted, silently undoing it.
func TestToolCache_RefreshServer_DiscardsResultSupersededByInvalidation(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	staleTools := []mcp.Tool{{Name: "stale_tool_under_old_credential"}}

	fetcher := func(_ context.Context, _ string) (*mcp.ToolListing, error) {
		close(started)
		<-release
		return &mcp.ToolListing{Tools: staleTools}, nil
	}

	cache := mcp.NewToolCache(fetcher, time.Hour)

	var wg sync.WaitGroup
	wg.Add(1)
	var refreshErr error
	go func() {
		defer wg.Done()
		refreshErr = cache.RefreshServer(context.Background(), "srv")
	}()

	// Wait until RefreshServer's fetch has actually started (and therefore
	// already captured its starting generation) before invalidating, so the
	// race this test drives is deterministic rather than depending on
	// goroutine scheduling luck.
	<-started

	// Simulates an admin rotating this server's credential (or any other
	// change that calls Invalidate/InvalidateWithStore) while the refresh
	// above is still in flight, fetching under the OLD state.
	cache.Invalidate("srv")

	// Let the stale fetch complete now that the invalidation has already
	// happened.
	close(release)
	wg.Wait()

	if refreshErr != nil {
		t.Fatalf("RefreshServer returned an error for a discarded-but-successful fetch: %v", refreshErr)
	}

	all := cache.GetAllTools()
	if _, present := all["srv"]; present {
		t.Errorf("cache still holds an entry for %q after Invalidate ran mid-fetch — the superseded "+
			"RefreshServer result was published anyway, undoing the invalidation", "srv")
	}
}

// TestToolCache_RefreshServer_DiscardsResultSupersededByInvalidateWithStore
// is the persistent-store counterpart of
// TestToolCache_RefreshServer_DiscardsResultSupersededByInvalidation: a
// refresh superseded by InvalidateWithStore must not persist its stale
// result to the store either, even though the fetch itself succeeded. Before
// the generation counter existed, persistListing ran unconditionally after
// every successful fetch, so this sequence re-wrote the very row
// InvalidateWithStore had just deleted from the store.
func TestToolCache_RefreshServer_DiscardsResultSupersededByInvalidateWithStore(t *testing.T) {
	t.Parallel()

	started := make(chan struct{})
	release := make(chan struct{})
	staleTools := []mcp.Tool{{Name: "stale_tool_under_old_credential"}}

	fetcher := func(_ context.Context, _ string) (*mcp.ToolListing, error) {
		close(started)
		<-release
		return &mcp.ToolListing{Tools: staleTools}, nil
	}

	store := &fakeToolStore{}
	cache := mcp.NewPersistentToolCache(fetcher, time.Hour, store)

	var wg sync.WaitGroup
	wg.Add(1)
	var refreshErr error
	go func() {
		defer wg.Done()
		refreshErr = cache.RefreshServer(context.Background(), "srv")
	}()

	<-started
	cache.InvalidateWithStore(context.Background(), "srv")
	close(release)
	wg.Wait()

	if refreshErr != nil {
		t.Fatalf("RefreshServer returned an error for a discarded-but-successful fetch: %v", refreshErr)
	}

	all := cache.GetAllTools()
	if _, present := all["srv"]; present {
		t.Errorf("cache still holds an entry for %q after InvalidateWithStore ran mid-fetch", "srv")
	}

	saves, _ := store.counts()
	if saves != 0 {
		t.Errorf("store.Save called %d times for a fetch superseded by InvalidateWithStore, want 0 — "+
			"the discarded result must never reach the store", saves)
	}
}
