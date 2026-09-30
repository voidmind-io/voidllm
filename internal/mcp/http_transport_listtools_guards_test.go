package mcp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers ListTools' pagination guards: bounds on page count, total
// tool count, aggregate response bytes, and cursor length, plus the
// repeated-cursor and duplicate-tool cycle detectors — see http_transport.go's
// own doc block for maxToolsListPages/maxToolsListTools/
// maxToolsListTotalBytes/maxToolsListCursorLen and the err* vars each guard
// returns. Every guard must fail the WHOLE fetch (a nil *ToolListing), never
// a partial one, and never embed the upstream-controlled cursor or tool-name
// bytes that tripped it — errToolsListDuplicateTool is a bare, static
// sentinel that never names the colliding tool, exactly like every other
// guard here.

// ---- Page limit -------------------------------------------------------------

// TestListTools_PageLimit_ExceededReturnsError_NoPartialList drives an
// upstream that always hands back a fresh, never-repeated nextCursor (so
// only the page-count guard, not the repeated-cursor one, can trip) and
// verifies ListTools stops after exactly 100 requests — the hardcoded
// mirror of the unexported maxToolsListPages constant; if that constant is
// ever retuned, this test's hit-count assertion must be updated alongside
// it — with an error, and no tools at all.
func TestListTools_PageLimit_ExceededReturnsError_NoPartialList(t *testing.T) {
	t.Parallel()

	const wantHits = 100 // mirrors the unexported maxToolsListPages
	const sentinel = "SENTINEL-PAGE-LIMIT-CURSOR-DO-NOT-LEAK"

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[],"nextCursor":"%s-%d"}}`, sentinel, n)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error for an upstream that never stops paginating")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil (no partial list on a guard failure)", listing)
	}
	if !strings.Contains(err.Error(), "exceeded the maximum number of pages") {
		t.Errorf("error = %q, want it to name the page-limit guard", err.Error())
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("error = %q, leaks the upstream-controlled cursor bytes", err.Error())
	}

	got := atomic.LoadInt64(&hits)
	if got != wantHits {
		t.Errorf("upstream received %d requests, want exactly %d (the guard must stop the fetch, not merely error eventually)", got, wantHits)
	}
}

// ---- Total tool count limit --------------------------------------------------

// TestListTools_ToolCountLimit_ExceededReturnsError_NoPartialList drives an
// upstream that hands back 200 uniquely-named tools per page — comfortably
// exceeding the 10,000-tool ceiling (maxToolsListTools) well before the
// 100-page ceiling could ever trip instead, so this test is unambiguously
// about the tool-count guard.
func TestListTools_ToolCountLimit_ExceededReturnsError_NoPartialList(t *testing.T) {
	t.Parallel()

	const toolsPerPage = 200
	// 51 pages * 200 tools/page = 10,200 > 10,000 (maxToolsListTools), while
	// 51 is well under the 100-page ceiling — isolates the tool-count guard.

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)

		var b strings.Builder
		b.WriteString(`{"jsonrpc":"2.0","id":1,"result":{"tools":[`)
		for i := 0; i < toolsPerPage; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"name":"p%d_t%d","inputSchema":{"type":"object"}}`, n, i)
		}
		fmt.Fprintf(&b, `],"nextCursor":"page-%d"}}`, n)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, b.String())
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error once the aggregated tool count exceeds the ceiling")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil (no partial list on a guard failure)", listing)
	}
	if !strings.Contains(err.Error(), "exceeded the maximum number of tools") {
		t.Errorf("error = %q, want it to name the tool-count guard", err.Error())
	}

	got := atomic.LoadInt64(&hits)
	if got > 60 {
		t.Errorf("upstream received %d requests, want the fetch to have stopped shortly after crossing 10,000 tools (around page 51), not run away", got)
	}
}

// TestListTools_ToolCountLimit_SinglePageOverCeiling_RejectedBeforeSecondRequest
// drives a single FIRST page whose own tool count (10,001) already exceeds
// maxToolsListTools by itself, with a nextCursor that would otherwise invite
// a second request. This isolates the ordering finding: the tool-count check
// must compare len(allTools)+len(page tools) BEFORE any of the page's tools
// are appended to allTools or inserted into seenNames, so a single
// over-the-ceiling page is rejected outright — never partially ingested up
// to the ceiling — and, since the fetch fails before ever inspecting
// nextCursor, the upstream must never receive a second request.
func TestListTools_ToolCountLimit_SinglePageOverCeiling_RejectedBeforeSecondRequest(t *testing.T) {
	t.Parallel()

	const toolCount = 10_001

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)

		var b strings.Builder
		b.WriteString(`{"jsonrpc":"2.0","id":1,"result":{"tools":[`)
		for i := 0; i < toolCount; i++ {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"name":"t%d","inputSchema":{"type":"object"}}`, i)
		}
		b.WriteString(`],"nextCursor":"would-be-page-2"}}`)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, b.String())
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error for a single page whose own tool count exceeds the ceiling")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil", listing)
	}
	if !strings.Contains(err.Error(), "exceeded the maximum number of tools") {
		t.Errorf("error = %q, want it to name the tool-count guard", err.Error())
	}

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("upstream received %d requests, want exactly 1 — the over-ceiling page must be rejected before ever following its nextCursor", got)
	}
}

// ---- Aggregate response byte bound -------------------------------------------

// TestListTools_TotalBytesLimit_ExceededReturnsError_NoPartialList drives an
// upstream that hands back ~5 MiB of tool schema padding per page —
// comfortably under any single page's own rawPostMaxBodyBytes ceiling (10
// MiB) and well under maxToolsListTools, so only maxToolsListTotalBytes (32
// MiB, summed across pages) can trip. 7 pages * ~5 MiB = ~35 MiB, crossing
// the ceiling on the 7th page.
func TestListTools_TotalBytesLimit_ExceededReturnsError_NoPartialList(t *testing.T) {
	t.Parallel()

	const padPerPage = 5 << 20 // 5 MiB of description padding per page

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)

		var b strings.Builder
		fmt.Fprintf(&b, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"p%d_t","description":"`, n)
		b.WriteString(strings.Repeat("x", padPerPage))
		fmt.Fprintf(&b, `","inputSchema":{"type":"object"}}],"nextCursor":"page-%d"}}`, n)

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, b.String())
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error once the aggregate response bytes exceed the ceiling")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil (no partial list on a guard failure)", listing)
	}
	if !strings.Contains(err.Error(), "exceeded the maximum aggregate response bytes") {
		t.Errorf("error = %q, want it to name the aggregate-byte guard", err.Error())
	}

	if got := atomic.LoadInt64(&hits); got > 8 {
		t.Errorf("upstream received %d requests, want the fetch to have stopped shortly after crossing 32 MiB (around page 7), not run away", got)
	}
}

// ---- Nil response body on a page after the first -----------------------------

// TestListTools_NilBodyOnFirstPage_EndsFetchAsEmptyListing verifies the
// unchanged half of errToolsListNilBodyMidFetch's contract: a nil body
// (HTTP 202 with no payload) on the very FIRST page ends the fetch with an
// empty, non-error listing, exactly as it did before pagination existed —
// see the adjacent TestListTools_NilBodyOnSecondPage_ReturnsError for the
// later-page case, which is NOT treated the same way.
func TestListTools_NilBodyOnFirstPage_EndsFetchAsEmptyListing(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted) // no body
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil for a bodyless first page", err)
	}
	if listing == nil || len(listing.Tools) != 0 {
		t.Errorf("ListTools() listing = %+v, want an empty, non-nil listing", listing)
	}
	// A bodyless first page never reaches the CacheHint-aggregation switch
	// at the bottom of ListTools at all (see errToolsListNilBodyMidFetch's
	// own doc): that switch has no case for "zero pages ever contributed a
	// flag" and would otherwise fall through to its default branch, which
	// claims TTLMsSet true, Scope CacheScopePublic for a listing that in
	// fact reflects no upstream response whatsoever. listing.Cache must be
	// the exact zero CacheHint here, not that fabricated default.
	if listing != nil && listing.Cache != (mcp.CacheHint{}) {
		t.Errorf("ListTools() listing.Cache = %+v, want the zero CacheHint for a bodyless first page", listing.Cache)
	}
}

// TestListTools_NilBodyOnSecondPage_ReturnsError verifies a nil response body
// (HTTP 202 with no payload) on any page AFTER the first fails the whole
// fetch instead of silently ending it with whatever pages were already
// read — the no-partial-listing contract errToolsListNilBodyMidFetch exists
// to enforce.
func TestListTools_NilBodyOnSecondPage_ReturnsError(t *testing.T) {
	t.Parallel()

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		switch n {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t1","inputSchema":{"type":"object"}}],"nextCursor":"next"}}`)
		case 2:
			w.WriteHeader(http.StatusAccepted) // no body
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error for a nil body on a page after the first")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil (no partial list on a guard failure)", listing)
	}
	if !strings.Contains(err.Error(), "follow-up page returned no response body") {
		t.Errorf("error = %q, want it to name the nil-body-mid-fetch guard", err.Error())
	}
}

// ---- ctx cancellation between pages -------------------------------------------

// TestListTools_CtxCanceledBetweenPages_StopsFetch verifies ListTools checks
// ctx at the top of every page's iteration, so a caller's own context —
// independent of maxToolsListPages or of ToolCache's singleflight fetch
// bound (toolsListFetchTimeout) — bounds a direct call to ListTools, such as
// the health checker or the admin API's test-connection endpoint, both of
// which call it directly rather than through ToolCache. The upstream's own
// nextCursor never runs out on its own here; only ctx cancellation, landing
// between the first and second page, stops the fetch.
func TestListTools_CtxCanceledBetweenPages_StopsFetch(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t%d","inputSchema":{"type":"object"}}],"nextCursor":"next-%d"}}`, n, n)
		if f, ok := w.(http.Flusher); ok {
			// Push page 1's response onto the wire BEFORE cancelling, so the
			// client's already-in-flight read of it is not the thing that
			// observes the cancellation — cancel() below must be caught at
			// the top of the SECOND iteration, not mid-page-1.
			f.Flush()
		}
		if n == 1 {
			cancel()
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(ctx)
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error once ctx is canceled between pages")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil (no partial list on a guard failure)", listing)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("upstream received %d requests, want exactly 1 — the second page must never be requested once ctx was already canceled", got)
	}
}

// ---- Cursor length limit -----------------------------------------------------

// TestListTools_CursorTooLong_ReturnsError verifies a single page whose
// nextCursor exceeds the 4096-byte ceiling (maxToolsListCursorLen) fails the
// fetch immediately, and that the error names only the byte count — never
// the cursor's own content, which is embedded here as a distinctive sentinel
// specifically so its absence from the error text can be asserted.
func TestListTools_CursorTooLong_ReturnsError(t *testing.T) {
	t.Parallel()

	const sentinel = "SENTINEL-CURSOR-TOO-LONG-DO-NOT-LEAK-"
	oversizedCursor := sentinel + strings.Repeat("x", 4096-len(sentinel)+1) // 4097 bytes total

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t","inputSchema":{"type":"object"}}],"nextCursor":%q}}`, oversizedCursor)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error for a nextCursor exceeding 4096 bytes")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil", listing)
	}
	if !strings.Contains(err.Error(), "exceeds the maximum length") {
		t.Errorf("error = %q, want it to name the cursor-length guard", err.Error())
	}
	if !strings.Contains(err.Error(), "4097 bytes") {
		t.Errorf("error = %q, want it to report the exact byte count (4097)", err.Error())
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("error = %q, leaks the oversized cursor's own content", err.Error())
	}
}

// ---- Repeated cursor (A -> B -> A) -------------------------------------------

// TestListTools_RepeatedCursor_ReturnsError drives a three-request cycle —
// cursor A, then B, then A again — and verifies ListTools detects the cycle
// on the third page rather than looping forever, stops at exactly 3
// upstream requests, and never embeds either cursor's content in the error
// (errToolsListRepeatedCursor is a bare sentinel with no formatting at all).
func TestListTools_RepeatedCursor_ReturnsError(t *testing.T) {
	t.Parallel()

	const cursorA = "SENTINEL-CYCLE-CURSOR-A"
	const cursorB = "SENTINEL-CYCLE-CURSOR-B"

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		body, _ := io.ReadAll(r.Body)
		req := decodeToolsListRequest(t, body)

		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			if req.Params != nil && req.Params.Cursor != nil {
				t.Errorf("request 1 unexpectedly carries a cursor: %+v", req.Params)
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t1","inputSchema":{"type":"object"}}],"nextCursor":%q}}`, cursorA)
		case 2:
			if req.Params == nil || req.Params.Cursor == nil || *req.Params.Cursor != cursorA {
				t.Errorf("request 2 params.cursor = %+v, want %q", req.Params, cursorA)
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t2","inputSchema":{"type":"object"}}],"nextCursor":%q}}`, cursorB)
		case 3:
			if req.Params == nil || req.Params.Cursor == nil || *req.Params.Cursor != cursorB {
				t.Errorf("request 3 params.cursor = %+v, want %q", req.Params, cursorB)
			}
			// Repeats cursor A — a cycle.
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t3","inputSchema":{"type":"object"}}],"nextCursor":%q}}`, cursorA)
		default:
			t.Errorf("unexpected request #%d, want the cycle guard to have stopped the fetch by now", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error for a repeated nextCursor")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil", listing)
	}
	if !strings.Contains(err.Error(), "repeated an earlier page's cursor") {
		t.Errorf("error = %q, want it to name the repeated-cursor guard", err.Error())
	}
	if strings.Contains(err.Error(), cursorA) || strings.Contains(err.Error(), cursorB) {
		t.Errorf("error = %q, leaks a cursor's own content", err.Error())
	}

	got := atomic.LoadInt64(&hits)
	if got != 3 {
		t.Errorf("upstream received %d requests, want exactly 3 (the cycle must be caught after the 3rd page, not looped further)", got)
	}
}

// ---- Repeated cursor: an empty-string cursor is a real cursor too ----------

// TestListTools_RepeatedEmptyCursor_ReturnsError exercises the seenCursors
// dedup guard for the one cursor value that is easy to special-case wrong:
// "" (empty string), which ListTools' own doc distinguishes from a MISSING
// nextCursor — a nil pointer means "no more pages", while a non-nil pointer
// to "" is "a real, if unusual, cursor" that must be followed exactly once.
// The first page hands back nextCursor:"" (not null/absent), so the second
// request correctly carries params.cursor:"". If the SECOND page also hands
// back nextCursor:"", that repeats a cursor value ("") already recorded in
// seenCursors from the first page — the identical cycle-detection guard
// errToolsListRepeatedCursor exists for, just with the one cursor value a
// naive "cursor is empty" check might mistake for "no cursor at all".
func TestListTools_RepeatedEmptyCursor_ReturnsError(t *testing.T) {
	t.Parallel()

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		body, _ := io.ReadAll(r.Body)
		req := decodeToolsListRequest(t, body)

		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			if req.Params != nil && req.Params.Cursor != nil {
				t.Errorf("request 1 unexpectedly carries a cursor: %+v", req.Params)
			}
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t1","inputSchema":{"type":"object"}}],"nextCursor":""}}`)
		case 2:
			if req.Params == nil || req.Params.Cursor == nil || *req.Params.Cursor != "" {
				t.Errorf("request 2 params.cursor = %+v, want a non-nil pointer to \"\"", req.Params)
			}
			// Repeats the empty-string cursor already seen after page 1.
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"t2","inputSchema":{"type":"object"}}],"nextCursor":""}}`)
		default:
			t.Errorf("unexpected request #%d, want the repeated-cursor guard to have stopped the fetch by now", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error for a repeated empty-string nextCursor")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil", listing)
	}
	if !strings.Contains(err.Error(), "repeated an earlier page's cursor") {
		t.Errorf("error = %q, want it to name the repeated-cursor guard", err.Error())
	}

	if got := atomic.LoadInt64(&hits); got != 2 {
		t.Errorf("upstream received %d requests, want exactly 2 (the repeat is caught on the 2nd page)", got)
	}
}

// ---- Duplicate tool name WITHIN a single page --------------------------------

// TestListTools_DuplicateToolName_SamePage_ReturnsError verifies the
// duplicate-tool guard also fires for two tools sharing a name on the SAME
// page, not only across pages (see the adjacent
// TestListTools_DuplicateToolName_ReturnsError for the across-pages case):
// the seenNames map ListTools' per-tool loop checks and populates is shared
// across every page of the whole fetch, so a single page naming the same
// tool twice trips the identical guard on its own second occurrence, before
// ever requesting a second page at all.
func TestListTools_DuplicateToolName_SamePage_ReturnsError(t *testing.T) {
	t.Parallel()

	const dupName = "same_page_duplicate"

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":%q,"inputSchema":{"type":"object"}},{"name":%q,"inputSchema":{"type":"object"}}]}}`, dupName, dupName)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error for a tool name repeated within a single page")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil (no partial list on a guard failure)", listing)
	}
	if !strings.Contains(err.Error(), "same tool name on more than one page") {
		t.Errorf("error = %q, want it to name the duplicate-tool guard", err.Error())
	}
	if strings.Contains(err.Error(), dupName) {
		t.Errorf("error = %q, must never embed the colliding tool name", err.Error())
	}

	if got := atomic.LoadInt64(&hits); got != 1 {
		t.Errorf("upstream received %d requests, want exactly 1 — the guard must fire within the first page, never requesting a second", got)
	}
}

// ---- Duplicate tool name across pages ----------------------------------------

// TestListTools_DuplicateToolName_ReturnsError verifies two pages naming the
// same tool fail the whole fetch with a bare, static error — the duplicate
// tool name itself, upstream-controlled content, is never embedded at all,
// unlike some of this package's other error paths (e.g.
// FilterHeaderParamTools' bounded annotation-name logging).
func TestListTools_DuplicateToolName_ReturnsError(t *testing.T) {
	t.Parallel()

	const dupName = "duplicate_tool"

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":%q,"inputSchema":{"type":"object"}}],"nextCursor":"next"}}`, dupName)
		case 2:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":%q,"inputSchema":{"type":"object"}}]}}`, dupName)
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	listing, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error for a tool name repeated across pages")
	}
	if listing != nil {
		t.Errorf("ListTools() listing = %v, want nil", listing)
	}
	if !strings.Contains(err.Error(), "same tool name on more than one page") {
		t.Errorf("error = %q, want it to name the duplicate-tool guard", err.Error())
	}
	if strings.Contains(err.Error(), dupName) {
		t.Errorf("error = %q, must never embed the colliding tool name — errToolsListDuplicateTool is a bare, static sentinel", err.Error())
	}
}

// TestListTools_DuplicateToolName_ErrorBoundedToMaxLength verifies that even
// a very long upstream-controlled tool name colliding across pages never
// makes it into the error at all — errToolsListDuplicateTool carries no
// per-collision content, so its error text is the same fixed length
// regardless of how long (or short) the colliding name was. A distinctive
// marker is placed inside the long name specifically so its absence from the
// error can be asserted directly, alongside the length check.
func TestListTools_DuplicateToolName_ErrorBoundedToMaxLength(t *testing.T) {
	t.Parallel()

	const beyondBoundMarker = "SENTINEL-BEYOND-TRUNCATION-BOUND"
	longDupName := strings.Repeat("n", 200) + beyondBoundMarker

	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := atomic.AddInt64(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		switch n {
		case 1:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":%q,"inputSchema":{"type":"object"}}],"nextCursor":"next"}}`, longDupName)
		case 2:
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":%q,"inputSchema":{"type":"object"}}]}}`, longDupName)
		default:
			t.Errorf("unexpected request #%d", n)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	_, err := tr.ListTools(context.Background())
	if err == nil {
		t.Fatal("ListTools() error = nil, want an error for a tool name repeated across pages")
	}
	if strings.Contains(err.Error(), beyondBoundMarker) {
		t.Errorf("error = %q, contains a marker embedded from the colliding tool name — want no part of it embedded at all", err.Error())
	}
	if strings.Contains(err.Error(), longDupName) {
		t.Errorf("error = %q, contains the colliding tool name — want it entirely absent", err.Error())
	}
	if len(err.Error()) >= len(longDupName) {
		t.Errorf("error length (%d) is not shorter than the long tool name (%d), want a short, fixed-size static error", len(err.Error()), len(longDupName))
	}
}
