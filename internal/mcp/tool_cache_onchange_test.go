package mcp_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// onChangeRecorder collects every serverID SetOnChange's installed callback
// was invoked with, safe for concurrent use — RefreshServer/GetTools may call
// the hook from whichever goroutine happens to be singleflight's leader.
type onChangeRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (r *onChangeRecorder) record(serverID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, serverID)
}

func (r *onChangeRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *onChangeRecorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = nil
}

// ---- fires on Invalidate / InvalidateWithStore -------------------------------

// TestToolCache_OnChange_FiresOnInvalidate verifies SetOnChange's own doc: a
// plain Invalidate call fires the hook, synchronously, with the invalidated
// server's own ID.
func TestToolCache_OnChange_FiresOnInvalidate(t *testing.T) {
	t.Parallel()

	cache := mcp.NewToolCache(makeStaticFetcher([]mcp.Tool{{Name: "t1"}}), time.Hour)
	rec := &onChangeRecorder{}
	cache.SetOnChange(rec.record)

	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	rec.reset() // the initial publish (oldEntry == nil) always fires — not under test here.

	cache.Invalidate("srv")

	if got := rec.snapshot(); len(got) != 1 || got[0] != "srv" {
		t.Errorf("onChange calls = %v, want exactly one call for %q", got, "srv")
	}
}

// TestToolCache_OnChange_FiresOnInvalidateWithStore verifies the same
// contract for InvalidateWithStore.
func TestToolCache_OnChange_FiresOnInvalidateWithStore(t *testing.T) {
	t.Parallel()

	store := &fakeToolStore{}
	cache := mcp.NewPersistentToolCache(makeStaticFetcher([]mcp.Tool{{Name: "t1"}}), time.Hour, store)
	rec := &onChangeRecorder{}
	cache.SetOnChange(rec.record)

	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	rec.reset()

	cache.InvalidateWithStore(context.Background(), "srv")

	if got := rec.snapshot(); len(got) != 1 || got[0] != "srv" {
		t.Errorf("onChange calls = %v, want exactly one call for %q", got, "srv")
	}
}

// TestToolCache_OnChange_NotFiredWhenNil verifies a nil callback (never
// installed, or installed and then cleared via SetOnChange(nil)) is simply
// never invoked — Invalidate must not panic or behave differently.
func TestToolCache_OnChange_NotFiredWhenNil(t *testing.T) {
	t.Parallel()

	cache := mcp.NewToolCache(makeStaticFetcher([]mcp.Tool{{Name: "t1"}}), time.Hour)
	rec := &onChangeRecorder{}
	cache.SetOnChange(rec.record)
	cache.SetOnChange(nil) // clears it again — must disable the callback entirely.

	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	cache.Invalidate("srv")

	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("onChange calls = %v, want none after SetOnChange(nil)", got)
	}
}

// ---- fires on a differing refetch, not on an identical one -------------------

// toggleFetcher returns tools[0] on its first call for a given serverID and
// tools[1] on every subsequent call — deterministic, not counter-racy across
// distinct serverIDs, since each serverID gets its own call count.
func toggleFetcher(first, second []mcp.Tool) mcp.ToolFetcher {
	var mu sync.Mutex
	calls := make(map[string]int)
	return func(_ context.Context, serverID string) (*mcp.ToolListing, error) {
		mu.Lock()
		n := calls[serverID]
		calls[serverID]++
		mu.Unlock()
		if n == 0 {
			return &mcp.ToolListing{Tools: first}, nil
		}
		return &mcp.ToolListing{Tools: second}, nil
	}
}

// TestToolCache_OnChange_FiresOnDifferingRefetch_NameAdded verifies
// toolsListingChanged's tool-name-set comparison: a forced refetch
// (RefreshServer) that adds a tool the previous listing did not have fires
// the hook.
func TestToolCache_OnChange_FiresOnDifferingRefetch_NameAdded(t *testing.T) {
	t.Parallel()

	fetcher := toggleFetcher(
		[]mcp.Tool{{Name: "t1", InputSchema: mcp.ObjectSchema(nil)}},
		[]mcp.Tool{{Name: "t1", InputSchema: mcp.ObjectSchema(nil)}, {Name: "t2", InputSchema: mcp.ObjectSchema(nil)}},
	)
	cache := mcp.NewToolCache(fetcher, time.Hour)
	rec := &onChangeRecorder{}
	cache.SetOnChange(rec.record)

	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("initial GetTools: %v", err)
	}
	rec.reset()

	if err := cache.RefreshServer(context.Background(), "srv"); err != nil {
		t.Fatalf("RefreshServer: %v", err)
	}

	if got := rec.snapshot(); len(got) != 1 || got[0] != "srv" {
		t.Errorf("onChange calls = %v, want exactly one call for %q after a listing with an added tool", got, "srv")
	}
}

// TestToolCache_OnChange_FiresOnDifferingRefetch_SchemaBytesChanged verifies
// toolsListingChanged's InputSchema-bytes comparison: a forced refetch whose
// tool NAMES are identical but whose InputSchema bytes differ for one of them
// still fires the hook.
func TestToolCache_OnChange_FiresOnDifferingRefetch_SchemaBytesChanged(t *testing.T) {
	t.Parallel()

	fetcher := toggleFetcher(
		[]mcp.Tool{{Name: "t1", InputSchema: mcp.ObjectSchema(map[string]mcp.SchemaProp{"a": {Type: "string"}})}},
		[]mcp.Tool{{Name: "t1", InputSchema: mcp.ObjectSchema(map[string]mcp.SchemaProp{"a": {Type: "number"}})}},
	)
	cache := mcp.NewToolCache(fetcher, time.Hour)
	rec := &onChangeRecorder{}
	cache.SetOnChange(rec.record)

	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("initial GetTools: %v", err)
	}
	rec.reset()

	if err := cache.RefreshServer(context.Background(), "srv"); err != nil {
		t.Fatalf("RefreshServer: %v", err)
	}

	if got := rec.snapshot(); len(got) != 1 || got[0] != "srv" {
		t.Errorf("onChange calls = %v, want exactly one call for %q after a schema-only change", got, "srv")
	}
}

// TestToolCache_OnChange_NotFiredOnIdenticalRefetch verifies the negative:
// most upstreams' tools rarely change, and an ordinary TTL/force-driven
// refetch that returns byte-for-byte the same listing must NOT fire the hook
// — firing here would turn every routine refresh into subscriptions/listen
// noise.
func TestToolCache_OnChange_NotFiredOnIdenticalRefetch(t *testing.T) {
	t.Parallel()

	tools := []mcp.Tool{{Name: "t1", InputSchema: mcp.ObjectSchema(map[string]mcp.SchemaProp{"a": {Type: "string"}})}}
	cache := mcp.NewToolCache(makeStaticFetcher(tools), time.Hour)
	rec := &onChangeRecorder{}
	cache.SetOnChange(rec.record)

	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("initial GetTools: %v", err)
	}
	rec.reset()

	if err := cache.RefreshServer(context.Background(), "srv"); err != nil {
		t.Fatalf("RefreshServer: %v", err)
	}

	if got := rec.snapshot(); len(got) != 0 {
		t.Errorf("onChange calls = %v, want none for a byte-for-byte identical refetch", got)
	}
}

// ---- callback never invoked while ToolCache's own locks are held -------------

// TestToolCache_OnChange_CallbackNotInvokedUnderLock_CanCallGetTools verifies
// SetOnChange's own doc: fn always runs after every lock this cache holds
// (tc.mu, and where applicable tc.storeMu) has already been released, so it
// is free to call back into the SAME ToolCache — here, GetTools for a
// DIFFERENT server ID, run from inside the onChange callback itself — without
// deadlocking. Driven through a background goroutine with a bounded wait
// (not a sleep loop): if fireOnChange ran while a lock were held, GetTools
// would block forever on that same lock and the done channel would never
// close, so the bound below turns a would-be permanent hang into a fast,
// deterministic test failure instead.
func TestToolCache_OnChange_CallbackNotInvokedUnderLock_CanCallGetTools(t *testing.T) {
	t.Parallel()

	otherTools := []mcp.Tool{{Name: "other-tool"}}
	fetcher := func(_ context.Context, serverID string) (*mcp.ToolListing, error) {
		if serverID == "other-srv" {
			return &mcp.ToolListing{Tools: otherTools}, nil
		}
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "t1"}}}, nil
	}
	cache := mcp.NewToolCache(fetcher, time.Hour)

	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("initial GetTools: %v", err)
	}

	// Installed only AFTER the initial populate above: that first fetch's own
	// oldEntry-is-nil publish is unconditionally "changed" (toolsListingChanged's
	// own doc) and would otherwise fire this hook once before Invalidate is
	// ever called, filling reentered's buffer-of-1 with a send nothing then
	// drains and deadlocking the second (Invalidate-triggered) send below —
	// a test-fixture-ordering hazard, not the re-entrancy property under test.
	reentered := make(chan error, 1)
	cache.SetOnChange(func(serverID string) {
		if serverID != "srv" {
			return
		}
		// Recursive call into the SAME cache from inside the callback — this
		// is exactly the scenario that would deadlock if fireOnChange ran
		// while tc.mu were still held.
		_, err := cache.GetTools(context.Background(), "other-srv")
		reentered <- err
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.Invalidate("srv")
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Invalidate did not return within the bound — the onChange callback likely deadlocked " +
			"trying to re-enter the cache while a lock was still held")
	}

	select {
	case err := <-reentered:
		if err != nil {
			t.Errorf("re-entrant GetTools from inside onChange callback failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onChange callback's re-entrant GetTools never completed within the bound")
	}
}
