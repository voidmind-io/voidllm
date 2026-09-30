package mcp_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers the specific bypass shapes countToolsListPageTools'
// duplicate/case-insensitive-key handling and its iterative depth guard
// (see http_transport.go's own doc on countToolsListPageTools,
// countResultTools, and skipJSONValue) exist to close: a page constructed so
// a naive, case-sensitive, first-match-wins object walk resolves "result" or
// "tools" to a DIFFERENT key than jsonx.Unmarshal itself would resolve it to
// when the real decode runs afterward, and a value nested so deep a
// recursive skip would grow this goroutine's own call stack without bound.
// Every test here mirrors TestCountToolsListPageTools_LargeArray_ExceededWithBoundedAllocs's
// own MemStats-bounded shape: driving countToolsListPageTools directly
// against a body carrying millions of minimal {} elements and asserting
// both that it is rejected AND that rejecting it cost a small, fixed amount
// of memory regardless of how many elements the bypass attempt tried to
// hide behind a duplicate or case-variant key.

// jsonUnicodeEscape returns the six-character JSON unicode escape sequence
// for r — a literal backslash, 'u', and r's code point as four lowercase
// hex digits (e.g. r == 't' yields the six bytes backslash, u, 0, 0, 7, 4).
// Built via string(rune(0x5C)) and fmt.Sprintf rather than typed directly as
// a literal escape sequence in this file's own source text, purely so the
// tests below that need this exact six-byte sequence to survive verbatim
// into their JSON test bodies are not at the mercy of any tooling that
// might otherwise treat a literal run of source characters spelling out a
// JSON unicode escape as something to normalize away before this file is
// even compiled.
func jsonUnicodeEscape(r rune) string {
	return string(rune(0x5C)) + fmt.Sprintf("u%04x", r)
}

// hugeToolsArrayLiteral returns the literal JSON text of an array holding n
// minimal {} elements — the same shape hugeMinimalToolsPage's own tools
// array uses, factored out here so the tests in this file can embed it at
// whatever key (or however many times) each individual test needs.
func hugeToolsArrayLiteral(n int) string {
	var b strings.Builder
	b.Grow(n*3 + 2)
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("{}")
	}
	b.WriteByte(']')
	return b.String()
}

// ---- Duplicate "result" keys: last one wins, matching the real decode -----

// TestCountToolsListPageTools_DuplicateResultKey_LastWins_MatchesRealDecode
// drives a body with TWO top-level "result" keys: the FIRST carries a small,
// harmless tools array; the SECOND — the one jsonx.Unmarshal itself resolves
// "result" to when the real decode runs, since encoding/json (and sonic's
// ConfigStd, built to mimic it) processes an object's keys in order and lets
// each later match overwrite an earlier one — carries millions of minimal {}
// elements. A case-sensitive, first-match-wins walk (the previous
// findObjectField approach) would stop at the first "result" and report a
// harmless count, exactly the disagreement countResultTools' own doc exists
// to close: this test proves the SECOND, real-decode-winning "result" is
// what actually gets counted, while still costing only a small, bounded
// amount of memory to reject.
func TestCountToolsListPageTools_DuplicateResultKey_LastWins_MatchesRealDecode(t *testing.T) {
	// Deliberately not t.Parallel(): wants an isolated heap for its
	// runtime.MemStats snapshot, mirroring
	// TestCountToolsListPageTools_LargeArray_ExceededWithBoundedAllocs.

	const toolCount = 2_000_000
	const limit = 10_000

	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"harmless"}]},"result":{"tools":` +
		hugeToolsArrayLiteral(toolCount) + `}}`)

	count, exceeded, err := mcp.CountToolsListPageTools(body, limit)
	if err != nil {
		t.Fatalf("CountToolsListPageTools(...) error = %v, want nil", err)
	}
	if !exceeded {
		t.Fatalf("CountToolsListPageTools(...) exceeded = false, want true — the SECOND (real-decode-winning) \"result\" carries %d tools, count=%d, limit=%d", toolCount, count, limit)
	}

	allocated := heapBytesAllocated(func() {
		_, exceeded, err := mcp.CountToolsListPageTools(body, limit)
		if err != nil {
			t.Fatalf("CountToolsListPageTools(...) error = %v inside heapBytesAllocated, want nil", err)
		}
		if !exceeded {
			t.Fatal("CountToolsListPageTools(...) exceeded = false inside heapBytesAllocated, want true")
		}
	})
	const bound = 1 << 20 // 1 MiB, mirrors the existing bounded-alloc test's own bound.
	if allocated > bound {
		t.Errorf("pre-decode count allocated %d bytes for a duplicate-\"result\"-key bypass attempt, want at most %d", allocated, bound)
	}
}

// ---- Duplicate "tools" keys: summed conservatively -------------------------

// TestCountToolsListPageTools_DuplicateToolsKey_SummedConservatively drives
// a single "result" object with TWO "tools" keys: the FIRST empty, the
// SECOND carrying millions of minimal {} elements. A case-sensitive,
// first-match-wins walk would stop at the first (empty) "tools" and report
// zero, even though jsonx.Unmarshal itself resolves "tools" to the SECOND,
// non-empty one when the real decode runs. countResultTools' own
// sum-every-match strategy catches this regardless of which one the real
// decode would actually pick, by construction (see its own doc).
func TestCountToolsListPageTools_DuplicateToolsKey_SummedConservatively(t *testing.T) {
	// Deliberately not t.Parallel(): see the test above.

	const toolCount = 2_000_000
	const limit = 10_000

	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[],"tools":` +
		hugeToolsArrayLiteral(toolCount) + `}}`)

	count, exceeded, err := mcp.CountToolsListPageTools(body, limit)
	if err != nil {
		t.Fatalf("CountToolsListPageTools(...) error = %v, want nil", err)
	}
	if !exceeded {
		t.Fatalf("CountToolsListPageTools(...) exceeded = false, want true — the SECOND \"tools\" key carries %d tools, count=%d, limit=%d", toolCount, count, limit)
	}

	allocated := heapBytesAllocated(func() {
		_, exceeded, err := mcp.CountToolsListPageTools(body, limit)
		if err != nil {
			t.Fatalf("CountToolsListPageTools(...) error = %v inside heapBytesAllocated, want nil", err)
		}
		if !exceeded {
			t.Fatal("CountToolsListPageTools(...) exceeded = false inside heapBytesAllocated, want true")
		}
	})
	const bound = 1 << 20
	if allocated > bound {
		t.Errorf("pre-decode count allocated %d bytes for a duplicate-\"tools\"-key bypass attempt, want at most %d", allocated, bound)
	}
}

// ---- "TOOLS"/"Tools" case variants -----------------------------------------

// TestCountToolsListPageTools_ToolsCaseVariants_SummedConservatively drives
// a "result" object whose harmless, exact-case "tools": [] key comes FIRST,
// followed by "Tools" and "TOOLS" case variants — one of which carries the
// pathological array. A case-SENSITIVE first-match-wins walk would stop at
// the exact-case "tools" key and never even look at "Tools"/"TOOLS" at all,
// even though jsonx.Unmarshal's own case-insensitive field matching resolves
// every one of them to the SAME destination field (all three map to the one
// Go field tagged "tools" — there is no competing field for exact-match
// preference to arbitrate between), with whichever appears last in document
// order winning. strings.EqualFold-based matching here recognizes every
// variant regardless of position.
func TestCountToolsListPageTools_ToolsCaseVariants_SummedConservatively(t *testing.T) {
	// Deliberately not t.Parallel(): see the tests above.

	const toolCount = 2_000_000
	const limit = 10_000

	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[],"Tools":[],"TOOLS":` +
		hugeToolsArrayLiteral(toolCount) + `}}`)

	count, exceeded, err := mcp.CountToolsListPageTools(body, limit)
	if err != nil {
		t.Fatalf("CountToolsListPageTools(...) error = %v, want nil", err)
	}
	if !exceeded {
		t.Fatalf("CountToolsListPageTools(...) exceeded = false, want true — \"TOOLS\" carries %d tools, count=%d, limit=%d", toolCount, count, limit)
	}

	allocated := heapBytesAllocated(func() {
		_, exceeded, err := mcp.CountToolsListPageTools(body, limit)
		if err != nil {
			t.Fatalf("CountToolsListPageTools(...) error = %v inside heapBytesAllocated, want nil", err)
		}
		if !exceeded {
			t.Fatal("CountToolsListPageTools(...) exceeded = false inside heapBytesAllocated, want true")
		}
	})
	const bound = 1 << 20
	if allocated > bound {
		t.Errorf("pre-decode count allocated %d bytes for a \"TOOLS\"/\"Tools\" case-variant bypass attempt, want at most %d", allocated, bound)
	}
}

// ---- Escaped key -------------------------------------------------------------

// TestCountToolsListPageTools_EscapedToolsKey_Recognized drives a "tools"
// key spelled with a real JSON six-character unicode escape for its leading
// letter (backslash, u, 0, 0, 7, 4 — the code point for 't') carrying the
// pathological array, so the SOURCE bytes never contain the literal
// substring "tools" at all. The escape is built via jsonUnicodeEscape (see
// its own doc) rather than typed directly in this file's own source, purely
// so it survives byte-for-byte rather than risk being pre-expanded by any
// tooling that treats a literal backslash-u-four-hex-digits run in Go
// source text as something to normalize. encoding/json's own Token() call
// already returns the fully-UNescaped Go string for a JSON string token
// regardless of how the source spelled it, so this proves strings.EqualFold
// is compared against that already-decoded value, not against the source
// bytes verbatim — a naive substring search over the raw body would miss
// this key entirely.
func TestCountToolsListPageTools_EscapedToolsKey_Recognized(t *testing.T) {
	// Deliberately not t.Parallel(): see the tests above.

	const toolCount = 2_000_000
	const limit = 10_000

	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"` + jsonUnicodeEscape('t') + `ools":` +
		hugeToolsArrayLiteral(toolCount) + `}}`)
	if strings.Contains(string(body), `"tools"`) {
		t.Fatalf("test body accidentally contains the literal substring \"tools\": %s", body)
	}

	count, exceeded, err := mcp.CountToolsListPageTools(body, limit)
	if err != nil {
		t.Fatalf("CountToolsListPageTools(...) error = %v, want nil", err)
	}
	if !exceeded {
		t.Fatalf("CountToolsListPageTools(...) exceeded = false, want true — the escaped \"tools\" key carries %d tools, count=%d, limit=%d", toolCount, count, limit)
	}

	allocated := heapBytesAllocated(func() {
		_, exceeded, err := mcp.CountToolsListPageTools(body, limit)
		if err != nil {
			t.Fatalf("CountToolsListPageTools(...) error = %v inside heapBytesAllocated, want nil", err)
		}
		if !exceeded {
			t.Fatal("CountToolsListPageTools(...) exceeded = false inside heapBytesAllocated, want true")
		}
	})
	const bound = 1 << 20
	if allocated > bound {
		t.Errorf("pre-decode count allocated %d bytes for an escaped-\"tools\"-key bypass attempt, want at most %d", allocated, bound)
	}
}

// TestCountToolsListPageTools_EscapedResultKey_Recognized is the top-level
// counterpart: the "result" key itself is spelled with a real JSON unicode
// escape for its leading letter (the code point for 'r'), built the same
// way via jsonUnicodeEscape, so the SOURCE bytes never contain the literal
// substring "result" either, exactly mirroring the escaped-"tools" test
// above one level up.
func TestCountToolsListPageTools_EscapedResultKey_Recognized(t *testing.T) {
	// Deliberately not t.Parallel(): see the tests above.

	const toolCount = 2_000_000
	const limit = 10_000

	body := []byte(`{"jsonrpc":"2.0","id":1,"` + jsonUnicodeEscape('r') + `esult":{"tools":` +
		hugeToolsArrayLiteral(toolCount) + `}}`)
	if strings.Contains(string(body), `"result"`) {
		t.Fatalf("test body accidentally contains the literal substring \"result\": %s", body)
	}

	count, exceeded, err := mcp.CountToolsListPageTools(body, limit)
	if err != nil {
		t.Fatalf("CountToolsListPageTools(...) error = %v, want nil", err)
	}
	if !exceeded {
		t.Fatalf("CountToolsListPageTools(...) exceeded = false, want true — the escaped \"result\" key carries %d tools, count=%d, limit=%d", toolCount, count, limit)
	}

	allocated := heapBytesAllocated(func() {
		_, exceeded, err := mcp.CountToolsListPageTools(body, limit)
		if err != nil {
			t.Fatalf("CountToolsListPageTools(...) error = %v inside heapBytesAllocated, want nil", err)
		}
		if !exceeded {
			t.Fatal("CountToolsListPageTools(...) exceeded = false inside heapBytesAllocated, want true")
		}
	})
	const bound = 1 << 20
	if allocated > bound {
		t.Errorf("pre-decode count allocated %d bytes for an escaped-\"result\"-key bypass attempt, want at most %d", allocated, bound)
	}
}

// ---- Deep nesting: iterative, bounded, no stack growth ---------------------

// TestSkipJSONValueForTest_DeepNesting_RejectedWithMaxDepthError drives
// skipJSONValue (via SkipJSONValueForTest) directly against a value nested
// 100,000 '[' deep — cheap for an attacker to construct (one byte per
// level) but far beyond maxJSONSkipDepth (512). A recursive walk (the
// previous implementation) would grow this goroutine's own call stack by
// one frame per level; skipJSONValue's iterative, explicit depth counter
// (see its own doc) instead rejects it with errJSONSkipMaxDepthExceeded as
// soon as the counter crosses maxJSONSkipDepth, without ever recursing —
// this test asserts the specific sentinel error via errors.Is in isolation;
// TestCountToolsListPageTools_DeepNestingUnderTools_HandledGracefully below
// asserts the same errors.Is outcome through countToolsListPageTools' own
// public boundary, where it now surfaces as a genuine error (fail closed)
// rather than being swallowed into a bare (0, false) outcome.
func TestSkipJSONValueForTest_DeepNesting_RejectedWithMaxDepthError(t *testing.T) {
	t.Parallel()

	const depth = 100_000
	var b strings.Builder
	b.Grow(depth*2 + 8)
	for i := 0; i < depth; i++ {
		b.WriteByte('[')
	}
	for i := 0; i < depth; i++ {
		b.WriteByte(']')
	}

	dec := json.NewDecoder(bytes.NewReader([]byte(b.String())))
	err := mcp.SkipJSONValueForTest(dec)
	if err == nil {
		t.Fatal("SkipJSONValueForTest(...) error = nil, want errJSONSkipMaxDepthExceeded for a value nested 100,000 levels deep")
	}
	if !errors.Is(err, mcp.ErrJSONSkipMaxDepthExceeded) {
		t.Errorf("SkipJSONValueForTest(...) error = %v, want it to wrap ErrJSONSkipMaxDepthExceeded", err)
	}
}

// TestCountToolsListPageTools_DeepNestingUnderTools_HandledGracefully is the
// integration-level counterpart: a "tools" array whose single element nests
// 100,000 levels deep must not panic or hang countToolsListPageTools — it is
// caught by the SAME depth guard the direct skipJSONValue test above
// exercises in isolation, and reported as a genuine error
// (errJSONSkipMaxDepthExceeded), never as (0, false, nil) — the fail-closed
// contract countToolsListPageTools' own doc now requires: a depth-guard
// failure must never be mistaken for "this page carries no tools", since
// ListTools treats any counter error as reason to reject the whole page
// (errToolsListPreScanFailed) rather than falling through to the real,
// far-more-permissive jsonx.Unmarshal decode.
func TestCountToolsListPageTools_DeepNestingUnderTools_HandledGracefully(t *testing.T) {
	t.Parallel()

	const depth = 100_000
	var nested strings.Builder
	nested.Grow(depth*2 + 8)
	for i := 0; i < depth; i++ {
		nested.WriteByte('[')
	}
	for i := 0; i < depth; i++ {
		nested.WriteByte(']')
	}

	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[` + nested.String() + `]}}`)

	done := make(chan struct{})
	var count int
	var exceeded bool
	var err error
	go func() {
		defer close(done)
		count, exceeded, err = mcp.CountToolsListPageTools(body, 10_000)
	}()
	select {
	case <-done:
		if err == nil {
			t.Fatalf("CountToolsListPageTools(...) error = nil, want errJSONSkipMaxDepthExceeded for a value nested 100,000 levels deep (count=%d, exceeded=%v)", count, exceeded)
		}
		if !errors.Is(err, mcp.ErrJSONSkipMaxDepthExceeded) {
			t.Errorf("CountToolsListPageTools(...) error = %v, want it to wrap ErrJSONSkipMaxDepthExceeded", err)
		}
		if exceeded {
			t.Errorf("CountToolsListPageTools(...) exceeded = true, want false — a depth-guard failure is reported via err, not the exceeded-count guard")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CountToolsListPageTools(...) did not return promptly for a 100,000-level-deep value — want a fast, bounded rejection, not unbounded recursion")
	}
}
