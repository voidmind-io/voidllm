package mcp_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers the fail-closed contract countToolsListPageTools' own doc
// now requires (http_transport.go) end to end, through a real ListTools call
// against a real httptest server: a page shaped so this pre-decode walk's
// own bounded-depth guard (maxJSONSkipDepth) trips on SOME value it has to
// walk PAST before it could ever reach — or fully sum — a later, enormous
// "tools" array must reject the WHOLE page via errToolsListPreScanFailed,
// and — the property that actually matters — must never reach the far more
// permissive real decode (jsonx.Unmarshal) that would otherwise happily
// unmarshal that enormous array into millions of typed Tool structs. Each
// test below proves the second half of that with a MemStats allocation
// bound: if ListTools ever fell through to the real decode for one of these
// bodies, the resulting allocation would be in the hundreds of megabytes
// (TestCountToolsListPageTools_LargeArray_ExceededWithBoundedAllocs measured
// roughly 600 MB for an identical 2,000,000-element array decoded into
// []Tool) — a gap no amount of ordinary net/http or JSON-request-building
// overhead could plausibly be mistaken for.

// deepArrayLiteral returns the literal JSON text of an array nested depth
// levels deep — depth opening '[' immediately followed by depth closing ']'
// — cheap for an attacker to construct (one byte per level) but, at
// depth > 512 (maxJSONSkipDepth), far beyond what this package's own bounded
// skip will tolerate while still comfortably under encoding/json's and
// sonic's own much larger internal nesting limits, so the real decode never
// trips on it.
func deepArrayLiteral(depth int) string {
	var b strings.Builder
	b.Grow(depth * 2)
	for i := 0; i < depth; i++ {
		b.WriteByte('[')
	}
	for i := 0; i < depth; i++ {
		b.WriteByte(']')
	}
	return b.String()
}

// listToolsPreScanBytesAllocated drives a real HTTPTransport.ListTools call
// against a single-page httptest server serving body, and returns both the
// call's own outcome and how many bytes the call caused the heap allocator
// to allocate (see heapBytesAllocated's own doc for the TotalAlloc-delta
// methodology) — the same measurement
// TestCountToolsListPageTools_LargeArray_ExceededWithBoundedAllocs uses,
// applied here to the full ListTools call rather than to
// countToolsListPageTools in isolation, so a fall-through to the real,
// per-element-allocating decode shows up exactly the same way it would in
// production.
func listToolsPreScanBytesAllocated(t *testing.T, body []byte) (listing *mcp.ToolListing, err error, allocated uint64) {
	t.Helper()

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")

	allocated = heapBytesAllocated(func() {
		listing, err = tr.ListTools(context.Background())
	})

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("upstream received %d requests, want exactly 1 — a pre-scan rejection must fail the fetch before ever following a nextCursor", got)
	}
	return listing, err, allocated
}

// preScanFailClosedBound is a generous, non-flaky ceiling for how much a
// single ListTools call against one of this file's pathological pages may
// allocate while still being rejected by the pre-decode walk alone. The ~6
// MB response body itself is copied a handful of times end to end —
// httptest.Server's own buffering, the client's io.ReadAll, this file's own
// string-builder construction of the body — measured at roughly 75-80 MB in
// practice, well under this bound; a full decode of the same body's
// 2,000,000-element "tools" array, by contrast, allocates on the order of
// 600 MB (see this file's own package doc above and
// TestCountToolsListPageTools_LargeArray_ExceededWithBoundedAllocs). This is
// not a knife's-edge assertion on an exact byte count — it is a wide,
// qualitative gap between "a few copies of one page's own bytes" and "one
// allocation per tool element across millions of elements".
const preScanFailClosedBound = 200 << 20 // 200 MiB

// assertPreScanFailedClosed asserts the shared outcome every test in this
// file requires: a non-nil error wrapping errToolsListPreScanFailed (never a
// partial or nil listing), and a bounded allocation proving the real decode
// was never reached.
func assertPreScanFailedClosed(t *testing.T, listing *mcp.ToolListing, err error, allocated uint64) {
	t.Helper()

	if err == nil {
		t.Fatal("ListTools(...) error = nil, want errToolsListPreScanFailed")
	}
	if !errors.Is(err, mcp.ErrToolsListPreScanFailed) {
		t.Errorf("ListTools(...) error = %v, want it to wrap ErrToolsListPreScanFailed", err)
	}
	if listing != nil {
		t.Errorf("ListTools(...) listing = %v, want nil on a pre-scan rejection", listing)
	}
	if allocated > preScanFailClosedBound {
		t.Errorf("ListTools(...) allocated %d bytes, want at most %d — the real per-element decode must never have run", allocated, preScanFailClosedBound)
	}
}

// TestListTools_DeepJunkBeforeResult_FailsClosed_NoFullDecode is the
// integration-level counterpart of the core fail-open bug this whole round
// of changes closes: a top-level key ("junk") entirely unrelated to
// "result" nests deeper than maxJSONSkipDepth, BEFORE "result" itself ever
// appears in document order. The pre-scan's own bounded skip trips on
// "junk" and never even reaches "result" — countToolsListPageTools now
// reports that as an error rather than the old fail-open (0, false)
// "nothing to see here" outcome, so ListTools rejects the whole page instead
// of falling through to jsonx.Unmarshal, which — thanks to its own much more
// permissive depth tolerance — would have sailed past "junk" and fully
// decoded the enormous "tools" array that follows it.
func TestListTools_DeepJunkBeforeResult_FailsClosed_NoFullDecode(t *testing.T) {
	// Deliberately not t.Parallel(): wants an isolated heap for its
	// runtime.MemStats snapshot, mirroring
	// TestCountToolsListPageTools_LargeArray_ExceededWithBoundedAllocs.

	const toolCount = 2_000_000
	const depth = 600 // > maxJSONSkipDepth (512)

	body := []byte(`{"jsonrpc":"2.0","id":1,"junk":` + deepArrayLiteral(depth) +
		`,"result":{"tools":` + hugeToolsArrayLiteral(toolCount) + `}}`)

	listing, err, allocated := listToolsPreScanBytesAllocated(t, body)
	assertPreScanFailedClosed(t, listing, err, allocated)
}

// TestListTools_DeepJunkInsideResultBeforeTools_FailsClosed_NoFullDecode is
// the same shape one level down: the over-deep key ("junk") is INSIDE the
// "result" object, before "tools" itself, rather than before "result" at
// the top level.
func TestListTools_DeepJunkInsideResultBeforeTools_FailsClosed_NoFullDecode(t *testing.T) {
	// Deliberately not t.Parallel(): see the test above.

	const toolCount = 2_000_000
	const depth = 600

	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"junk":` + deepArrayLiteral(depth) +
		`,"tools":` + hugeToolsArrayLiteral(toolCount) + `}}`)

	listing, err, allocated := listToolsPreScanBytesAllocated(t, body)
	assertPreScanFailedClosed(t, listing, err, allocated)
}

// TestListTools_DuplicateResultKey_DeepFirstValue_FailsClosed_NoFullDecode
// combines this package's duplicate-"result"-key handling with the depth
// guard: the FIRST "result" key's own value nests deeper than
// maxJSONSkipDepth; the SECOND "result" key — the one the real decode's own
// last-write-wins resolution would actually use, per countToolsListPageTools'
// own doc — carries the enormous "tools" array. The pre-scan never gets far
// enough to see the second "result" at all: it fails while skipping past the
// FIRST one's over-deep value.
func TestListTools_DuplicateResultKey_DeepFirstValue_FailsClosed_NoFullDecode(t *testing.T) {
	// Deliberately not t.Parallel(): see the test above.

	const toolCount = 2_000_000
	const depth = 600

	body := []byte(`{"jsonrpc":"2.0","id":1,"result":` + deepArrayLiteral(depth) +
		`,"result":{"tools":` + hugeToolsArrayLiteral(toolCount) + `}}`)

	listing, err, allocated := listToolsPreScanBytesAllocated(t, body)
	assertPreScanFailedClosed(t, listing, err, allocated)
}

// TestListTools_DeepJunkInFirstToolsValue_HugeSecondTools_FailsClosed_NoFullDecode
// is the "tools" analog one level further down: the FIRST "tools" key's own
// value nests deeper than maxJSONSkipDepth (rather than being a huge array
// itself); the SECOND "tools" key carries the enormous array
// countResultTools' own sum-every-match strategy would otherwise still need
// to add in. The pre-scan fails while walking the FIRST "tools" value's
// own excessive nesting, never reaching the second, huge one.
func TestListTools_DeepJunkInFirstToolsValue_HugeSecondTools_FailsClosed_NoFullDecode(t *testing.T) {
	// Deliberately not t.Parallel(): see the test above.

	const toolCount = 2_000_000
	const depth = 600

	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":` + deepArrayLiteral(depth) +
		`,"tools":` + hugeToolsArrayLiteral(toolCount) + `}}`)

	listing, err, allocated := listToolsPreScanBytesAllocated(t, body)
	assertPreScanFailedClosed(t, listing, err, allocated)
}
