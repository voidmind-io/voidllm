package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers ListTools' pre-decode tool-count bound
// (countToolsListPageTools, http_transport.go): a page whose own tools array
// holds far more elements than maxToolsListTools must be rejected by a cheap
// streaming token walk BEFORE the expensive jsonx.Unmarshal into []Tool ever
// runs — see that function's own doc for why maxToolsListTotalBytes and
// rawPostMaxBodyBytes alone do not already close this gap.

// hugeMinimalToolsPage builds a tools/list response body carrying n minimal
// {} tool elements — no name, no schema, nothing but the two-byte object
// itself — comfortably under both rawPostMaxBodyBytes (10 MiB, a single
// page's own ceiling) and maxToolsListTotalBytes (32 MiB, the aggregate
// ceiling) even at n in the low millions, while still exceeding
// maxToolsListTools by a wide margin once n is large enough.
func hugeMinimalToolsPage(n int) []byte {
	var b strings.Builder
	b.Grow(n*3 + 64)
	b.WriteString(`{"jsonrpc":"2.0","id":1,"result":{"tools":[`)
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("{}")
	}
	b.WriteString(`]}}`)
	return []byte(b.String())
}

// TestListTools_HugeMinimalToolsPage_RejectedByPreDecodeCount drives a
// single page of 2,000,000 minimal {} tool elements (~6 MB, well under
// rawPostMaxBodyBytes' 10 MiB single-page ceiling) through a real ListTools
// call and verifies it is rejected by the tool-count guard rather than
// accepted — proving the pre-decode count, not merely the post-decode
// len(allTools) check, is what a single oversized page actually hits first
// end to end.
func TestListTools_HugeMinimalToolsPage_RejectedByPreDecodeCount(t *testing.T) {
	t.Parallel()

	const toolCount = 2_000_000
	body := hugeMinimalToolsPage(toolCount)

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error for a page whose own tool count vastly exceeds the ceiling")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil (no partial list on a guard failure)", listing)
	}
	if !strings.Contains(err.Error(), "exceeded the maximum number of tools") {
		t.Errorf("error = %q, want it to name the tool-count guard", err.Error())
	}

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("upstream received %d requests, want exactly 1 — the single over-ceiling page must be rejected before ever following a nextCursor", got)
	}
}

// heapBytesAllocated runs fn once and returns how many bytes it caused the
// Go heap allocator to allocate, measured as the delta of runtime.MemStats'
// own TotalAlloc — a monotonically increasing counter of every byte ever
// allocated by the heap allocator, unlike HeapAlloc (which a GC run between
// the before/after snapshots could shrink back down and understate the true
// cost). runtime.GC() is called before each snapshot purely so any garbage
// left over from an EARLIER call does not inflate a LATER one's own delta;
// it has no effect on TotalAlloc, which never decreases.
func heapBytesAllocated(fn func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	fn()
	runtime.GC()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestCountToolsListPageTools_LargeArray_ExceededWithBoundedAllocs drives
// countToolsListPageTools directly (via the export_test.go hook) against the
// same shape of pathological page, isolated from every allocation net/http
// itself would otherwise add, and asserts two things: it correctly reports
// the array as exceeding a small limit, and it does so allocating FAR fewer
// BYTES than a full decode of the identical body into a typed Go slice
// would — the entire reason this pre-decode count exists instead of just
// running the real decode and rejecting afterward. Bytes, not allocation
// COUNT (testing.AllocsPerRun), is the metric that actually distinguishes
// the two here: encoding/json's slice-growth strategy means both approaches
// make a similar, small NUMBER of allocation calls (geometric slice growth
// is O(log n) calls, not one call per element) — but the full decode's calls
// are for an ever-larger backing array of real Tool-shaped elements, while
// the pre-decode count's calls are for small, fixed-size decoder bookkeeping
// regardless of how many elements it walks past.
func TestCountToolsListPageTools_LargeArray_ExceededWithBoundedAllocs(t *testing.T) {
	// Deliberately not t.Parallel(): this test wants an isolated, otherwise
	// idle heap for its runtime.MemStats snapshots.

	const toolCount = 2_000_000
	const limit = 10_000 // mirrors maxToolsListTools
	body := hugeMinimalToolsPage(toolCount)

	count, exceeded, err := mcp.CountToolsListPageTools(body, limit)
	if err != nil {
		t.Fatalf("CountToolsListPageTools(...) error = %v, want nil", err)
	}
	if !exceeded {
		t.Fatalf("CountToolsListPageTools(...) exceeded = false, want true (count=%d, limit=%d)", count, limit)
	}
	if count <= limit {
		t.Errorf("CountToolsListPageTools(...) count = %d, want it to have counted past limit (%d) before giving up", count, limit)
	}

	preDecodeBytes := heapBytesAllocated(func() {
		_, exceeded, err := mcp.CountToolsListPageTools(body, limit)
		if err != nil {
			t.Fatalf("CountToolsListPageTools(...) error = %v inside heapBytesAllocated, want nil", err)
		}
		if !exceeded {
			t.Fatal("CountToolsListPageTools(...) exceeded = false inside heapBytesAllocated, want true")
		}
	})

	// The full-decode comparison: unmarshaling the identical body into the
	// actual response shape ListTools itself decodes into allocates a
	// backing array sized for every one of the 2,000,000 tools (plus each
	// element's own fields) — the exact memory cost the pre-decode count
	// exists to avoid paying before ever rejecting the page.
	var decodedCount int
	fullDecodeBytes := heapBytesAllocated(func() {
		var rpcResp struct {
			Result struct {
				Tools []struct {
					Name        string          `json:"name"`
					InputSchema json.RawMessage `json:"inputSchema"`
					Description string          `json:"description"`
				} `json:"tools"`
				NextCursor *string `json:"nextCursor"`
			} `json:"result"`
		}
		if err := json.Unmarshal(body, &rpcResp); err != nil {
			t.Fatalf("json.Unmarshal comparison decode: %v", err)
		}
		decodedCount = len(rpcResp.Result.Tools)
	})
	if decodedCount != toolCount {
		t.Fatalf("comparison decode produced %d tools, want %d", decodedCount, toolCount)
	}

	t.Logf("pre-decode count: %d bytes allocated; full decode into []Tool-shaped struct: %d bytes allocated", preDecodeBytes, fullDecodeBytes)

	// A generous, non-flaky bound: the pre-decode count must stay well under
	// 1 MiB regardless of the 2,000,000 elements walked, while the full
	// decode — by construction — allocates a backing array with at least
	// one struct-sized slot per element (tens of MB at this element count).
	// This is not a knife's-edge assertion on an exact byte count (which
	// even a stdlib version bump could shift); it is a wide, qualitative gap
	// the pre-decode approach exists to guarantee.
	const preDecodeByteBound = 1 << 20 // 1 MiB
	if preDecodeBytes > preDecodeByteBound {
		t.Errorf("pre-decode count allocated %d bytes, want at most %d regardless of the %d elements walked",
			preDecodeBytes, preDecodeByteBound, toolCount)
	}
	if fullDecodeBytes <= preDecodeBytes*10 {
		t.Errorf("full decode allocated only %d bytes vs. the pre-decode count's %d — want the full decode to cost at least an order of magnitude more, proving the pre-decode count actually avoids that cost",
			fullDecodeBytes, preDecodeBytes)
	}
}
