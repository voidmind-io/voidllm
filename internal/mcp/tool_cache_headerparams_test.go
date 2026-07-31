package mcp_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// makeHeaderParamFetcher returns a ToolFetcher that always returns tools
// together with headerParams for a fixed serverID, and a pointer to a call
// counter the test can inspect.
func makeHeaderParamFetcher(tools []mcp.Tool, headerParams map[string][]mcp.HeaderParam) (mcp.ToolFetcher, *int64) {
	var calls int64
	fetcher := func(_ context.Context, _ string) (*mcp.ToolListing, error) {
		atomic.AddInt64(&calls, 1)
		return &mcp.ToolListing{Tools: tools, HeaderParams: headerParams}, nil
	}
	return fetcher, &calls
}

// TestToolCache_HeaderParams_ReturnsDeepCopy verifies HeaderParams hands back
// an independent copy: mutating the returned []HeaderParam's Path slice in
// place must never reach the cache's own internal entry. Rot the moment
// copyHeaderParams is replaced with a shallow `append([]HeaderParam(nil),
// src...)` (which copies the outer slice header but still aliases each
// HeaderParam's own Path backing array) — the second HeaderParams call below
// would then observe the mutated Path.
func TestToolCache_HeaderParams_ReturnsDeepCopy(t *testing.T) {
	t.Parallel()

	tools := []mcp.Tool{{Name: "lookup"}}
	headerParams := map[string][]mcp.HeaderParam{
		"lookup": {{Name: "Region", Path: []string{"outer", "region"}, Kind: mcp.HeaderParamKindString}},
	}
	fetcher, _ := makeHeaderParamFetcher(tools, headerParams)
	cache := mcp.NewToolCache(fetcher, time.Hour)

	got1, err := cache.HeaderParams(context.Background(), "srv", "lookup")
	if err != nil {
		t.Fatalf("HeaderParams (first): %v", err)
	}
	if len(got1) != 1 || len(got1[0].Path) != 2 {
		t.Fatalf("HeaderParams (first) = %+v, want a single binding with a 2-element Path", got1)
	}

	// Mutate the returned Path IN PLACE.
	got1[0].Path[0] = "MUTATED"
	got1[0].Path[1] = "MUTATED"

	got2, err := cache.HeaderParams(context.Background(), "srv", "lookup")
	if err != nil {
		t.Fatalf("HeaderParams (second): %v", err)
	}
	if len(got2) != 1 || len(got2[0].Path) != 2 {
		t.Fatalf("HeaderParams (second) = %+v, want a single binding with a 2-element Path", got2)
	}
	if got2[0].Path[0] != "outer" || got2[0].Path[1] != "region" {
		t.Errorf("HeaderParams (second) Path = %v, want [\"outer\" \"region\"] unaffected by the first call's mutation", got2[0].Path)
	}
}

// TestToolCache_HeaderParams_UnknownTool_NilNilNoError verifies HeaderParams
// returns (nil, nil) — not an error — for a tool name absent from the
// server's header-params map, per its own documented contract (either no
// such tool, or the tool's schema violated §4.3 and was already excluded by
// FilterHeaderParamTools).
func TestToolCache_HeaderParams_UnknownTool_NilNilNoError(t *testing.T) {
	t.Parallel()

	tools := []mcp.Tool{{Name: "lookup"}}
	fetcher, _ := makeHeaderParamFetcher(tools, nil)
	cache := mcp.NewToolCache(fetcher, time.Hour)

	got, err := cache.HeaderParams(context.Background(), "srv", "does_not_exist")
	if err != nil {
		t.Fatalf("HeaderParams() error = %v, want nil", err)
	}
	if got != nil {
		t.Errorf("HeaderParams() = %+v, want nil", got)
	}
}

// TestToolCache_HeaderParams_SharesFetchWithGetTools verifies the Code Mode
// hot-path assertion documented on HeaderParams itself: a GetTools call
// followed by several HeaderParams calls for the SAME server ID all share a
// single underlying upstream fetch via entryFor's fresh/stale logic. Rot the
// moment HeaderParams starts driving its own fetch instead of sharing
// entryFor with GetTools — the call count below would then read 4 (one for
// GetTools, three more for each HeaderParams call) instead of 1.
func TestToolCache_HeaderParams_SharesFetchWithGetTools(t *testing.T) {
	t.Parallel()

	tools := []mcp.Tool{{Name: "lookup"}}
	headerParams := map[string][]mcp.HeaderParam{
		"lookup": {{Name: "Region", Path: []string{"region"}, Kind: mcp.HeaderParamKindString}},
	}
	fetcher, calls := makeHeaderParamFetcher(tools, headerParams)
	cache := mcp.NewToolCache(fetcher, time.Hour)

	if _, err := cache.GetTools(context.Background(), "srv"); err != nil {
		t.Fatalf("GetTools: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := cache.HeaderParams(context.Background(), "srv", "lookup"); err != nil {
			t.Fatalf("HeaderParams (call %d): %v", i, err)
		}
	}

	if got := atomic.LoadInt64(calls); got != 1 {
		t.Errorf("fetcher called %d times across 1 GetTools + 3 HeaderParams calls, want 1 (shared entryFor)", got)
	}
}
