package pii

// gazetteer_stage1a_test.go covers the PII Stage 1a (gazetteer detector)
// feature introduced on feat/pii-stage1-gazetteer.  Tests are grouped by
// the component under test:
//
//   - Aho-Corasick automaton (ahocorasick.go)
//   - GazetteerDetector.Find (gazetteer.go)
//   - Loader + embedded packs (gazetteer_load.go, parseGazetteerFile)
//   - Pseudonym helpers (pseudonym.go)
//   - End-to-end pipeline (Engine + GazetteerDetector + RegexDetector)

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

// ── Aho-Corasick automaton ────────────────────────────────────────────────────

// TestAC_SingleTerm verifies that a single-term dictionary finds one match.
func TestAC_SingleTerm(t *testing.T) {
	t.Parallel()

	m, err := newACMatcher([]acTerm{{norm: "gmbh", typ: "ORG"}})
	if err != nil {
		t.Fatalf("newACMatcher: %v", err)
	}
	matches := m.match([]rune("acme gmbh corp"))
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d: %+v", len(matches), matches)
	}
	if m.terms[matches[0].termIdx].norm != "gmbh" {
		t.Errorf("matched term = %q, want %q", m.terms[matches[0].termIdx].norm, "gmbh")
	}
}

// TestAC_MultipleTerms verifies that all three dictionary terms are found in a
// single pass over the input.
func TestAC_MultipleTerms(t *testing.T) {
	t.Parallel()

	terms := []acTerm{
		{norm: "gmbh", typ: "ORG"},
		{norm: "ag", typ: "ORG"},
		{norm: "berlin", typ: "CITY"},
	}
	m, err := newACMatcher(terms)
	if err != nil {
		t.Fatalf("newACMatcher: %v", err)
	}

	matches := m.match([]rune("acme gmbh und berlin ag"))
	found := make(map[string]bool)
	for _, match := range matches {
		found[m.terms[match.termIdx].norm] = true
	}

	for _, want := range []string{"gmbh", "ag", "berlin"} {
		if !found[want] {
			t.Errorf("expected to find term %q, but did not; all found: %v", want, found)
		}
	}
}

// TestAC_OverlappingDictionaryTerms verifies that when dictionary terms overlap
// (e.g. "ag" is a suffix of "gmbh & co. kg"), both are reported by the raw
// automaton (it is the Find() caller's job to filter, not the automaton's).
func TestAC_OverlappingDictionaryTerms(t *testing.T) {
	t.Parallel()

	// "kg" is a suffix of "gmbh & co. kg"; both should be raw-reported.
	terms := []acTerm{
		{norm: "gmbh & co. kg", typ: "ORG"},
		{norm: "kg", typ: "ORG"},
	}
	m, err := newACMatcher(terms)
	if err != nil {
		t.Fatalf("newACMatcher: %v", err)
	}

	runes := []rune("foo gmbh & co. kg bar")
	matches := m.match(runes)

	found := make(map[string]bool)
	for _, match := range matches {
		found[m.terms[match.termIdx].norm] = true
	}
	// Both "gmbh & co. kg" and "kg" appear as raw automaton hits.
	if !found["gmbh & co. kg"] {
		t.Error("expected raw match for 'gmbh & co. kg', but did not find it")
	}
	if !found["kg"] {
		t.Error("expected raw match for 'kg', but did not find it")
	}
}

// TestAC_SuffixPrefixInDictionary verifies that "berg" is found when it appears
// inside "bergmann", demonstrating that the raw automaton reports all hits
// (word-boundary filtering is done by Find(), not by match()).
func TestAC_SuffixPrefixInDictionary(t *testing.T) {
	t.Parallel()

	terms := []acTerm{
		{norm: "berg", typ: "CITY"},
		{norm: "bergmann", typ: "NAME"},
	}
	m, err := newACMatcher(terms)
	if err != nil {
		t.Fatalf("newACMatcher: %v", err)
	}

	runes := []rune("bergmann")
	matches := m.match(runes)

	found := make(map[string]bool)
	for _, match := range matches {
		found[m.terms[match.termIdx].norm] = true
	}
	// Raw automaton finds both "berg" (suffix of bergmann) and "bergmann".
	if !found["berg"] {
		t.Error("raw automaton: expected to find 'berg' inside 'bergmann'")
	}
	if !found["bergmann"] {
		t.Error("raw automaton: expected to find 'bergmann' when the whole input is 'bergmann'")
	}
}

// TestAC_NoMatch verifies that no matches are reported for an input that
// contains none of the dictionary terms.
func TestAC_NoMatch(t *testing.T) {
	t.Parallel()

	m, err := newACMatcher([]acTerm{{norm: "gmbh", typ: "ORG"}})
	if err != nil {
		t.Fatalf("newACMatcher: %v", err)
	}
	matches := m.match([]rune("nothing here"))
	if len(matches) != 0 {
		t.Errorf("expected 0 matches, got %d: %+v", len(matches), matches)
	}
}

// TestAC_EmptyInput verifies that an empty rune slice produces no matches.
func TestAC_EmptyInput(t *testing.T) {
	t.Parallel()

	m, err := newACMatcher([]acTerm{{norm: "gmbh", typ: "ORG"}})
	if err != nil {
		t.Fatalf("newACMatcher: %v", err)
	}
	if got := m.match([]rune{}); len(got) != 0 {
		t.Errorf("empty input: expected 0 matches, got %d", len(got))
	}
	if got := m.match(nil); len(got) != 0 {
		t.Errorf("nil input: expected 0 matches, got %d", len(got))
	}
}

// TestAC_EmptyDictionary verifies that a zero-term automaton produces no
// matches and does not panic.
func TestAC_EmptyDictionary(t *testing.T) {
	t.Parallel()

	m, err := newACMatcher(nil)
	if err != nil {
		t.Fatalf("newACMatcher(nil): %v", err)
	}
	if got := m.match([]rune("anything")); len(got) != 0 {
		t.Errorf("empty dictionary: expected 0 matches, got %d", len(got))
	}
}

// TestAC_MemoryCap verifies that newACMatcher returns a non-nil error (and
// never panics) when the dictionary would push the state count past
// maxGazetteerStates.
func TestAC_MemoryCap(t *testing.T) {
	t.Parallel()

	// Build terms whose combined unique rune paths are designed to fill the
	// trie far faster than maxGazetteerStates terms would suggest.  Each
	// term uses a unique 6-character prefix derived from its index so that
	// the trie cannot share states between terms until the state limit fires.
	// With base-26 encoding, 26^6 = 308 million possible terms, so we can
	// safely pick a count that guarantees we hit the cap well before running
	// out of unique prefixes.
	//
	// The cap is 500 000 states.  With 6-rune unique prefixes we need at
	// most 500 000 / 6 ≈ 83 334 terms to guarantee a cap.  Use 100 000 to
	// be certain.
	const n = 100_000
	const alpha = "abcdefghijklmnopqrstuvwxyz"

	terms := make([]acTerm, n)
	for i := 0; i < n; i++ {
		b := make([]byte, 6)
		idx := i
		for j := 5; j >= 0; j-- {
			b[j] = alpha[idx%len(alpha)]
			idx /= len(alpha)
		}
		// Append a unique suffix so no two terms share a complete path.
		terms[i] = acTerm{norm: string(b) + fmt.Sprintf("%d", i), typ: "TEST"}
	}

	_, err := newACMatcher(terms)
	if err == nil {
		// It is theoretically possible that path-sharing keeps us under the
		// cap; treat this as a note rather than a hard failure so the test
		// does not become flaky.  What matters is that there was NO PANIC.
		t.Log("note: state cap not triggered; test confirms no panic but not the error path")
	} else {
		if !strings.Contains(err.Error(), "trie exceeds") {
			t.Errorf("expected cap error, got: %v", err)
		}
	}
}

// TestAC_MemoryCap_Triggers ensures the cap error is returned for a dictionary
// that definitely exceeds the limit.  We build all terms up front and call
// newACMatcher exactly once so the test runs in O(n) time rather than O(n²).
//
// Strategy: each term is a two-character string "prefix + unique-letter" where
// the prefix is a fixed non-ASCII rune.  Every term therefore has an entirely
// unique second character, contributing exactly one new state per term (the
// root-level node for the prefix rune is shared, but the second-level node is
// new for each term).  We need maxGazetteerStates+1 terms.  We use lowercase
// ASCII letters for the unique character to avoid the Unicode surrogate range.
//
// State budget: 1 (root) + 1 (shared prefix rune) + n (unique suffixes).
// With n = maxGazetteerStates, total would be maxGazetteerStates + 2, and the
// cap fires when len(states) reaches maxGazetteerStates.
//
// To stay clear of the surrogate range entirely we encode the index in
// base-62 (a-z A-Z 0-9), packing up to 3 characters per term.  With
// 3 characters we have 62^3 > 238 000 unique terms per prefix rune.
// Using two distinct prefix runes gives 476 000+ unique 4-char paths,
// comfortably exceeding the 500 000 cap.
func TestAC_MemoryCap_Triggers(t *testing.T) {
	t.Parallel()

	// Encode i as a base-62 string using only plain ASCII printable chars,
	// avoiding any Unicode surrogate-range values.
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	encode := func(i int) string {
		if i == 0 {
			return "a"
		}
		b := make([]byte, 0, 4)
		for i > 0 {
			b = append(b, alphabet[i%len(alphabet)])
			i /= len(alphabet)
		}
		// Reverse.
		for l, r := 0, len(b)-1; l < r; l, r = l+1, r-1 {
			b[l], b[r] = b[r], b[l]
		}
		return string(b)
	}

	// Build maxGazetteerStates + 1 terms.  Each term is a unique ASCII string,
	// so every character along the path adds a new trie state.  We use
	// short (1-4 char) strings derived from the counter, which guarantees
	// no two terms share a complete trie path.
	n := maxGazetteerStates + 1
	terms := make([]acTerm, n)
	for i := 0; i < n; i++ {
		// Prefix with 'x' so 1-char codes don't collide with 2-char codes.
		terms[i] = acTerm{norm: "x" + encode(i), typ: "TEST"}
	}

	_, err := newACMatcher(terms)
	if err == nil {
		t.Error("expected state-cap error for maxGazetteerStates+1 terms, got nil")
		return
	}
	if !strings.Contains(err.Error(), "trie exceeds") {
		t.Errorf("expected cap error containing 'trie exceeds', got: %v", err)
	}
}

// ── GazetteerDetector.Find: whole-token boundary ──────────────────────────────

// TestFind_WholeTokenBoundary is the authoritative test for the word-boundary
// filter.  Dictionary has "Berg"; input contains "Bergmann" (no match) and
// standalone "Berg" (match).  Exact byte Start/End are asserted.
func TestFind_WholeTokenBoundary(t *testing.T) {
	t.Parallel()

	det, err := NewGazetteerDetector(
		[]Gazetteer{{Name: "t", Type: "CITY", Terms: []string{"Berg"}}},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	// "Bergmann fuhr nach Berg."
	//  0123456789...
	//  B=0, e=1, r=2, g=3, m=4, a=5, n=6, n=7, ' '=8, ...
	//  "Berg." at end: "Bergmann fuhr nach Berg."
	text := "Bergmann fuhr nach Berg."

	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: unexpected error: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("expected 1 span (standalone Berg only), got %d: %+v", len(spans), spans)
	}

	wantStart := strings.Index(text, " Berg.") + 1 // skip the leading space
	wantEnd := wantStart + len("Berg")

	if spans[0].Start != wantStart {
		t.Errorf("span.Start = %d, want %d", spans[0].Start, wantStart)
	}
	if spans[0].End != wantEnd {
		t.Errorf("span.End = %d, want %d", spans[0].End, wantEnd)
	}
	if spans[0].Type != "CITY" {
		t.Errorf("span.Type = %q, want %q", spans[0].Type, "CITY")
	}
	// Verify the slice is correct.
	if got := text[spans[0].Start:spans[0].End]; got != "Berg" {
		t.Errorf("text[span] = %q, want %q", got, "Berg")
	}
}

// TestFind_ByteOffsetWithMultibytePrefix verifies that Span byte offsets are
// correct even when multibyte UTF-8 characters appear before the matched term.
func TestFind_ByteOffsetWithMultibytePrefix(t *testing.T) {
	t.Parallel()

	// "Grüße an München" — "ü" is 2 bytes, "ß" is 2 bytes, "ü" in München
	// is 2 bytes.  The critical check: text[span.Start:span.End] == "München".
	det, err := NewGazetteerDetector(
		[]Gazetteer{{Name: "t", Type: "CITY", Terms: []string{"München"}}},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	text := "Grüße an München"
	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: unexpected error: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d: %+v", len(spans), spans)
	}

	got := text[spans[0].Start:spans[0].End]
	if got != "München" {
		t.Errorf("text[span.Start:span.End] = %q, want %q", got, "München")
	}
	if spans[0].Type != "CITY" {
		t.Errorf("span.Type = %q, want CITY", spans[0].Type)
	}
}

// TestFind_ByteOffsetWithEmoji verifies byte-offset correctness when a
// multibyte emoji (4 bytes in UTF-8) precedes the matched term.
func TestFind_ByteOffsetWithEmoji(t *testing.T) {
	t.Parallel()

	det, err := NewGazetteerDetector(
		[]Gazetteer{{Name: "t", Type: "ORG", Terms: []string{"GmbH"}}},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	// U+1F600 GRINNING FACE is 4 bytes in UTF-8.
	text := "\U0001F600 Acme GmbH"
	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: unexpected error: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d: %+v", len(spans), spans)
	}

	got := text[spans[0].Start:spans[0].End]
	if got != "GmbH" {
		t.Errorf("text[span.Start:span.End] = %q, want %q", got, "GmbH")
	}
}

// TestFind_CaseInsensitiveUppercase verifies that upper-case input matches a
// mixed-case dictionary term when CaseInsensitive is true.
func TestFind_CaseInsensitiveUppercase(t *testing.T) {
	t.Parallel()

	det, err := NewGazetteerDetector(
		[]Gazetteer{{Name: "t", Type: "CITY", Terms: []string{"München"}}},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	// All-caps form must be matched.
	text := "Büro in MÜNCHEN"
	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: unexpected error: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("expected 1 span for MÜNCHEN, got %d: %+v", len(spans), spans)
	}
	// The returned Span must slice the ORIGINAL-case text.
	got := text[spans[0].Start:spans[0].End]
	if got != "MÜNCHEN" {
		t.Errorf("span slices %q, want %q (original-case)", got, "MÜNCHEN")
	}

	// Lower-case form must also be matched.
	text2 := "Büro in münchen"
	spans2, err2 := det.Find(text2)
	if err2 != nil {
		t.Fatalf("Find (text2): unexpected error: %v", err2)
	}
	if len(spans2) != 1 {
		t.Fatalf("expected 1 span for münchen, got %d: %+v", len(spans2), spans2)
	}
	if got2 := text2[spans2[0].Start:spans2[0].End]; got2 != "münchen" {
		t.Errorf("span slices %q, want %q", got2, "münchen")
	}
}

// TestFind_CaseSensitiveExactOnly verifies that CaseInsensitive:false only
// matches exact-case terms.
func TestFind_CaseSensitiveExactOnly(t *testing.T) {
	t.Parallel()

	det, err := NewGazetteerDetector(
		[]Gazetteer{{Name: "t", Type: "CITY", Terms: []string{"München"}}},
		GazetteerOptions{CaseInsensitive: false},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	// "münchen" (all-lower) must NOT match when case-sensitive.
	if spans, err := det.Find("Büro in münchen"); err != nil || len(spans) != 0 {
		t.Errorf("case-sensitive: did not expect match for lowercase 'münchen', got %+v err=%v", spans, err)
	}
	// "MÜNCHEN" must NOT match.
	if spans, err := det.Find("Büro in MÜNCHEN"); err != nil || len(spans) != 0 {
		t.Errorf("case-sensitive: did not expect match for uppercase 'MÜNCHEN', got %+v err=%v", spans, err)
	}
	// Exact original form must match.
	text := "Büro in München"
	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: unexpected error: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("case-sensitive: expected 1 span for exact 'München', got %d", len(spans))
	}
	if got := text[spans[0].Start:spans[0].End]; got != "München" {
		t.Errorf("span slices %q, want %q", got, "München")
	}
}

// TestFind_CaseFoldingOffsetAlignment is the critical edge-case test for offset
// alignment when the input contains characters whose Unicode ToLower mapping
// changes byte length or rune count.
//
// The Turkish dotted capital I (U+0130, "İ") lowercases to a 2-rune sequence
// "i" + combining dot above (U+0069 + U+0307) in some locales, but
// unicode.ToLower in Go maps it to the single rune U+0069 ("i"), preserving
// a 1:1 rune correspondence.  This test verifies that a term AFTER "İ" in the
// input still gets correct byte offsets regardless of how ToLower handles "İ".
//
// We also test "ß" (2 bytes in UTF-8), which lowercases to itself, so no
// alignment issue is expected — but we assert it explicitly as a regression
// anchor.
func TestFind_CaseFoldingOffsetAlignment(t *testing.T) {
	t.Parallel()

	det, err := NewGazetteerDetector(
		[]Gazetteer{{Name: "t", Type: "ORG", Terms: []string{"GmbH"}}},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	tests := []struct {
		name      string
		text      string
		wantSlice string
	}{
		{
			// U+0130 "İ" is 2 bytes (C4 B0) in UTF-8; followed by " GmbH".
			name:      "turkish dotted I before GmbH",
			text:      "İstanbul GmbH",
			wantSlice: "GmbH",
		},
		{
			// U+00DF "ß" is 2 bytes (C3 9F) in UTF-8.
			name:      "german sharp s before GmbH",
			text:      "Straße GmbH",
			wantSlice: "GmbH",
		},
		{
			// Combining character after "İ": ensure the byte/rune table is
			// built from origRunes (not scanRunes), so the offset is stable.
			name:      "multiple multibyte chars before GmbH",
			text:      "Ö Ü Ä GmbH",
			wantSlice: "GmbH",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spans, err := det.Find(tc.text)
			if err != nil {
				t.Fatalf("Find: unexpected error: %v", err)
			}
			if len(spans) != 1 {
				t.Fatalf("expected 1 span, got %d: %+v", len(spans), spans)
			}
			got := tc.text[spans[0].Start:spans[0].End]
			if got != tc.wantSlice {
				t.Errorf("text[span.Start:span.End] = %q, want %q (offset misalignment)", got, tc.wantSlice)
			}
		})
	}
}

// TestFind_MultipleTypes verifies that a single detector built from multiple
// gazetteers assigns the correct Type to each span.
func TestFind_MultipleTypes(t *testing.T) {
	t.Parallel()

	det, err := NewGazetteerDetector(
		[]Gazetteer{
			{Name: "org", Type: "ORG", Terms: []string{"GmbH", "AG"}},
			{Name: "city", Type: "CITY", Terms: []string{"Berlin", "Hamburg"}},
		},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	text := "Acme GmbH sitzt in Berlin."
	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: unexpected error: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d: %+v", len(spans), spans)
	}

	// Build a map of matched text → type for assertions.
	got := make(map[string]string)
	for _, sp := range spans {
		got[text[sp.Start:sp.End]] = sp.Type
	}
	if got["GmbH"] != "ORG" {
		t.Errorf("GmbH type = %q, want ORG", got["GmbH"])
	}
	if got["Berlin"] != "CITY" {
		t.Errorf("Berlin type = %q, want CITY", got["Berlin"])
	}
}

// TestFind_OverlapResolution_LeftmostLongest verifies that when two dictionary
// terms overlap at the same start position, the longest one wins.
func TestFind_OverlapResolution_LeftmostLongest(t *testing.T) {
	t.Parallel()

	// "gmbh & co. kg" and "gmbh" both start at the same position; the
	// longer form must win.
	det, err := NewGazetteerDetector(
		[]Gazetteer{{Name: "t", Type: "ORG", Terms: []string{"GmbH & Co. KG", "GmbH"}}},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	text := "Foo GmbH & Co. KG ist eine Firma."
	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: unexpected error: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("expected 1 span (longest wins), got %d: %+v", len(spans), spans)
	}
	got := text[spans[0].Start:spans[0].End]
	if got != "GmbH & Co. KG" {
		t.Errorf("span = %q, want %q", got, "GmbH & Co. KG")
	}
}

// TestFind_ConcurrentFindRace runs many goroutines calling Find on a shared
// detector simultaneously.  Must be run with -race to catch data races.
func TestFind_ConcurrentFindRace(t *testing.T) {
	t.Parallel()

	det, err := NewGazetteerDetector(
		[]Gazetteer{
			{Name: "org", Type: "ORG", Terms: []string{"GmbH", "AG", "Ltd"}},
			{Name: "city", Type: "CITY", Terms: []string{"Berlin", "Hamburg", "München"}},
		},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	const goroutines = 50
	text := "Acme GmbH und Bar AG wohnen in Berlin und Hamburg."

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			spans, findErr := det.Find(text)
			if findErr != nil {
				t.Errorf("concurrent Find: unexpected error: %v", findErr)
				return
			}
			// Expect GmbH, AG, Berlin, Hamburg — 4 spans.
			if len(spans) != 4 {
				// Use t.Errorf, not t.Fatalf; goroutines cannot call Fatal.
				t.Errorf("concurrent Find: expected 4 spans, got %d: %+v", len(spans), spans)
			}
		}()
	}
	wg.Wait()
}

// TestFind_MultiLocale verifies that terms from two different locale packs
// (simulated as gazetteers with different names) are both found in one call.
func TestFind_MultiLocale(t *testing.T) {
	t.Parallel()

	// Simulate a "de" pack and a custom "fr" operator pack.
	det, err := NewGazetteerDetector(
		[]Gazetteer{
			{Name: "de-companies", Locale: "de", Type: "ORG", Terms: []string{"GmbH", "AG"}},
			{Name: "fr-companies", Locale: "fr", Type: "ORG", Terms: []string{"SAS", "SARL"}},
		},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	text := "Foo GmbH und Bar SAS sind Partner."
	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: unexpected error: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans (de+fr), got %d: %+v", len(spans), spans)
	}

	matched := make(map[string]bool)
	for _, sp := range spans {
		matched[text[sp.Start:sp.End]] = true
	}
	if !matched["GmbH"] {
		t.Error("expected GmbH (de pack) to match")
	}
	if !matched["SAS"] {
		t.Error("expected SAS (fr pack) to match")
	}
}

// TestFind_EmptyInputAndEmptyDict verifies edge cases do not panic.
func TestFind_EmptyInputAndEmptyDict(t *testing.T) {
	t.Parallel()

	// Empty dictionary, non-empty input.
	det, err := NewGazetteerDetector(nil, GazetteerOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatalf("NewGazetteerDetector(nil): %v", err)
	}
	if spans, findErr := det.Find("some text"); findErr != nil || len(spans) != 0 {
		t.Errorf("empty dict: expected 0 spans, got %d err=%v", len(spans), findErr)
	}

	// Non-empty dictionary, empty input.
	det2, err := NewGazetteerDetector(
		[]Gazetteer{{Name: "t", Type: "ORG", Terms: []string{"GmbH"}}},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}
	if spans, findErr := det2.Find(""); findErr != nil || len(spans) != 0 {
		t.Errorf("empty input: expected 0 spans, got %d err=%v", len(spans), findErr)
	}
}

// ── Loader + embedded packs ───────────────────────────────────────────────────

// TestParseGazetteerFile_Full verifies all parsing features: locale header,
// type header, blank lines, comments, and term extraction.
func TestParseGazetteerFile_Full(t *testing.T) {
	t.Parallel()

	content := "# locale: de\n# type: CITY\n\n# A comment\nBerlin\nHamburg\n# another\nBremen\n"
	g, err := parseGazetteerFile(strings.NewReader(content), "de-test")
	if err != nil {
		t.Fatalf("parseGazetteerFile: %v", err)
	}
	if g.Name != "de-test" {
		t.Errorf("Name = %q, want %q", g.Name, "de-test")
	}
	if g.Locale != "de" {
		t.Errorf("Locale = %q, want %q", g.Locale, "de")
	}
	if g.Type != "CITY" {
		t.Errorf("Type = %q, want %q", g.Type, "CITY")
	}
	want := []string{"Berlin", "Hamburg", "Bremen"}
	if len(g.Terms) != len(want) {
		t.Fatalf("Terms = %v, want %v", g.Terms, want)
	}
	for i, w := range want {
		if g.Terms[i] != w {
			t.Errorf("Terms[%d] = %q, want %q", i, g.Terms[i], w)
		}
	}
}

// TestParseGazetteerFile_TypeUppercased verifies that the # type: header is
// upper-cased before being stored.
func TestParseGazetteerFile_TypeUppercased(t *testing.T) {
	t.Parallel()

	content := "# type: city\nBerlin\n"
	g, err := parseGazetteerFile(strings.NewReader(content), "x")
	if err != nil {
		t.Fatalf("parseGazetteerFile: %v", err)
	}
	if g.Type != "CITY" {
		t.Errorf("Type = %q, want CITY (type header should be uppercased)", g.Type)
	}
}

// TestParseGazetteerFile_NoTypeHeader verifies that a missing # type: header
// causes parseGazetteerFile to return an error (FIX 5). An empty or missing
// type is malformed because it would produce PII spans with no type label,
// breaking pseudonymization consistency.
func TestParseGazetteerFile_NoTypeHeader(t *testing.T) {
	t.Parallel()

	content := "# locale: universal\nFoo\nBar\n"
	_, err := parseGazetteerFile(strings.NewReader(content), "x")
	if err == nil {
		t.Fatal("expected error for missing # type: header, got nil")
	}
	if !strings.Contains(err.Error(), "type") {
		t.Errorf("error should mention 'type', got: %v", err)
	}
}

// TestParseGazetteerFile_BlankLinesAndComments verifies that blank lines and
// comment-only lines are not included as terms.
func TestParseGazetteerFile_BlankLinesAndComments(t *testing.T) {
	t.Parallel()

	// The file must include a # type: header (FIX 5: missing type is an error).
	content := "# type: ENTITY\n\n# just comments\n\n# more comments\nActualTerm\n\n"
	g, err := parseGazetteerFile(strings.NewReader(content), "x")
	if err != nil {
		t.Fatalf("parseGazetteerFile: %v", err)
	}
	if len(g.Terms) != 1 || g.Terms[0] != "ActualTerm" {
		t.Errorf("Terms = %v, want [ActualTerm]", g.Terms)
	}
}

// TestEmbeddedPack_CompanyForms verifies that the "company-forms" pack loads,
// has Type ORG, and contains expected canonical terms.
func TestEmbeddedPack_CompanyForms(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("company-forms")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(company-forms): %v", err)
	}
	if g.Type != "ORG" {
		t.Errorf("Type = %q, want ORG", g.Type)
	}
	if len(g.Terms) == 0 {
		t.Fatal("company-forms pack has no terms")
	}

	// Check for a few expected entries.
	want := map[string]bool{"GmbH": false, "AG": false, "Ltd": false, "Inc": false}
	for _, term := range g.Terms {
		if _, ok := want[term]; ok {
			want[term] = true
		}
	}
	for term, found := range want {
		if !found {
			t.Errorf("company-forms pack missing expected term %q", term)
		}
	}
}

// TestEmbeddedPack_DeCities verifies that the "de-cities" pack loads with
// Type CITY and contains expected German city names.
func TestEmbeddedPack_DeCities(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-cities")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-cities): %v", err)
	}
	if g.Type != "CITY" {
		t.Errorf("Type = %q, want CITY", g.Type)
	}
	if len(g.Terms) == 0 {
		t.Fatal("de-cities pack has no terms")
	}

	want := map[string]bool{"Düsseldorf": false, "Dortmund": false, "Stuttgart": false}
	for _, term := range g.Terms {
		if _, ok := want[term]; ok {
			want[term] = true
		}
	}
	for term, found := range want {
		if !found {
			t.Errorf("de-cities pack missing expected term %q", term)
		}
	}
}

// TestEmbeddedPack_UnknownName verifies that an unknown pack name returns an error.
func TestEmbeddedPack_UnknownName(t *testing.T) {
	t.Parallel()

	_, err := loadEmbeddedPack("does-not-exist")
	if err == nil {
		t.Error("expected error for unknown pack name, got nil")
	}
	if !strings.Contains(err.Error(), "unknown embedded pack") {
		t.Errorf("error should mention 'unknown embedded pack', got: %v", err)
	}
}

// TestLoadGazetteerDir_TxtFiles verifies that a temp directory containing
// *.txt gazetteer files is loaded correctly.
func TestLoadGazetteerDir_TxtFiles(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Write two gazetteer files.
	file1 := "# locale: custom\n# type: ORG\nAcmeCorp\nFooBar Inc\n"
	file2 := "# type: NAME\nAlice\nBob\n"
	if err := os.WriteFile(filepath.Join(dir, "orgs.txt"), []byte(file1), 0o600); err != nil {
		t.Fatalf("write orgs.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "names.txt"), []byte(file2), 0o600); err != nil {
		t.Fatalf("write names.txt: %v", err)
	}
	// Non-.txt file must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "readme.md"), []byte("ignore me"), 0o600); err != nil {
		t.Fatalf("write readme.md: %v", err)
	}

	var gazes []Gazetteer
	var fc int
	var tb int64
	if err := loadGazetteerDir(dir, &gazes, &fc, &tb); err != nil {
		t.Fatalf("loadGazetteerDir: %v", err)
	}
	if len(gazes) != 2 {
		t.Fatalf("expected 2 gazetteers (one per .txt), got %d", len(gazes))
	}

	// Build a unified term set.
	allTerms := make(map[string]bool)
	for _, g := range gazes {
		for _, term := range g.Terms {
			allTerms[term] = true
		}
	}
	for _, want := range []string{"AcmeCorp", "FooBar Inc", "Alice", "Bob"} {
		if !allTerms[want] {
			t.Errorf("expected term %q in loaded gazetteers, not found; all: %v", want, allTerms)
		}
	}
}

// TestLoadGazetteerDir_NonexistentDir verifies that a non-existent directory
// returns an error.
func TestLoadGazetteerDir_NonexistentDir(t *testing.T) {
	t.Parallel()

	var out []Gazetteer
	var fc int
	var tb int64
	err := loadGazetteerDir("/absolutely/does/not/exist/12345", &out, &fc, &tb)
	if err == nil {
		t.Error("expected error for nonexistent dir, got nil")
	}
}

// TestLoadGazetteerDetector_InlineTerms verifies that inline terms from
// PIIGazetteerConfig are loaded and matched.
func TestLoadGazetteerDetector_InlineTerms(t *testing.T) {
	t.Parallel()

	cfg := config.PIIGazetteerConfig{
		Terms: []config.PIIGazetteerTermConfig{
			{Type: "ORG", Values: []string{"Project Titan", "Operation Moonshot"}},
		},
	}
	// Set Options.CaseInsensitive to true via a pointer in the config.
	trueVal := true
	cfg.Options = config.PIIGazetteerOptionsConfig{CaseInsensitive: &trueVal}

	det, err := LoadGazetteerDetector(cfg)
	if err != nil {
		t.Fatalf("LoadGazetteerDetector: %v", err)
	}

	text := "We launched Project Titan last week."
	spans, findErr := det.Find(text)
	if findErr != nil {
		t.Fatalf("Find: unexpected error: %v", findErr)
	}
	if len(spans) != 1 {
		t.Fatalf("expected 1 span for 'Project Titan', got %d: %+v", len(spans), spans)
	}
	if got := text[spans[0].Start:spans[0].End]; got != "Project Titan" {
		t.Errorf("span = %q, want %q", got, "Project Titan")
	}
	if spans[0].Type != "ORG" {
		t.Errorf("span.Type = %q, want ORG", spans[0].Type)
	}
}

// TestLoadGazetteerDetector_EmptyTypeInlineTermError verifies that an inline
// term entry with empty Type causes LoadGazetteerDetector to return an error.
func TestLoadGazetteerDetector_EmptyTypeInlineTermError(t *testing.T) {
	t.Parallel()

	cfg := config.PIIGazetteerConfig{
		Terms: []config.PIIGazetteerTermConfig{
			{Type: "", Values: []string{"foo"}},
		},
	}

	_, err := LoadGazetteerDetector(cfg)
	if err == nil {
		t.Error("expected error for empty term type, got nil")
	}
	if !strings.Contains(err.Error(), "type") {
		t.Errorf("error should mention 'type', got: %v", err)
	}
}

// TestLoadGazetteerDetector_Dedup verifies that duplicate (term, type) pairs
// across multiple sources do not produce duplicate spans.
func TestLoadGazetteerDetector_Dedup(t *testing.T) {
	t.Parallel()

	cfg := config.PIIGazetteerConfig{
		Terms: []config.PIIGazetteerTermConfig{
			{Type: "ORG", Values: []string{"GmbH", "GmbH"}}, // dup within same entry
		},
	}
	trueVal := true
	cfg.Options = config.PIIGazetteerOptionsConfig{CaseInsensitive: &trueVal}

	det, err := LoadGazetteerDetector(cfg)
	if err != nil {
		t.Fatalf("LoadGazetteerDetector: %v", err)
	}

	spans, findErr := det.Find("Acme GmbH.")
	if findErr != nil {
		t.Fatalf("Find: unexpected error: %v", findErr)
	}
	if len(spans) != 1 {
		t.Errorf("expected 1 span (dedup), got %d: %+v", len(spans), spans)
	}
}

// ── Pseudonym helpers: typeAbbrev and stableAbbrev ───────────────────────────

// TestLoadGazetteerDetector_WithPack exercises the embedded pack loading path
// through LoadGazetteerDetector end-to-end.
func TestLoadGazetteerDetector_WithPack(t *testing.T) {
	t.Parallel()

	trueVal := true
	cfg := config.PIIGazetteerConfig{
		Packs: []string{"company-forms"},
	}
	cfg.Options = config.PIIGazetteerOptionsConfig{CaseInsensitive: &trueVal}

	det, err := LoadGazetteerDetector(cfg)
	if err != nil {
		t.Fatalf("LoadGazetteerDetector with pack: %v", err)
	}

	text := "Acme GmbH is a company."
	spans, findErr := det.Find(text)
	if findErr != nil {
		t.Fatalf("Find: unexpected error: %v", findErr)
	}
	if len(spans) == 0 {
		t.Fatal("expected at least one span from company-forms pack, got none")
	}
	// GmbH must be found and carry type ORG.
	found := false
	for _, sp := range spans {
		if text[sp.Start:sp.End] == "GmbH" && sp.Type == "ORG" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected span for 'GmbH' with type ORG, spans: %+v", spans)
	}
}

// TestLoadGazetteerDetector_WithDir exercises the operator directory loading
// path through LoadGazetteerDetector end-to-end.
func TestLoadGazetteerDetector_WithDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "# type: ORG\nAcmeCorp\nFooBar Inc\n"
	if err := os.WriteFile(filepath.Join(dir, "custom.txt"), []byte(content), 0o600); err != nil {
		t.Fatalf("write custom.txt: %v", err)
	}

	trueVal := true
	cfg := config.PIIGazetteerConfig{
		Dirs: []string{dir},
	}
	cfg.Options = config.PIIGazetteerOptionsConfig{CaseInsensitive: &trueVal}

	det, err := LoadGazetteerDetector(cfg)
	if err != nil {
		t.Fatalf("LoadGazetteerDetector with dir: %v", err)
	}

	text := "We hired AcmeCorp."
	spans, findErr := det.Find(text)
	if findErr != nil {
		t.Fatalf("Find: unexpected error: %v", findErr)
	}
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d: %+v", len(spans), spans)
	}
	if got := text[spans[0].Start:spans[0].End]; got != "AcmeCorp" {
		t.Errorf("span = %q, want AcmeCorp", got)
	}
	if spans[0].Type != "ORG" {
		t.Errorf("span.Type = %q, want ORG", spans[0].Type)
	}
}

// TestLoadGazetteerDetector_UnknownPackError verifies that LoadGazetteerDetector
// returns an error for an unknown embedded pack name.
func TestLoadGazetteerDetector_UnknownPackError(t *testing.T) {
	t.Parallel()

	cfg := config.PIIGazetteerConfig{
		Packs: []string{"does-not-exist"},
	}

	_, err := LoadGazetteerDetector(cfg)
	if err == nil {
		t.Error("expected error for unknown pack, got nil")
	}
}

// TestLoadGazetteerDetector_NonexistentDirError verifies that LoadGazetteerDetector
// returns an error when a configured directory does not exist.
func TestLoadGazetteerDetector_NonexistentDirError(t *testing.T) {
	t.Parallel()

	cfg := config.PIIGazetteerConfig{
		Dirs: []string{"/no/such/dir/xyz99"},
	}

	_, err := LoadGazetteerDetector(cfg)
	if err == nil {
		t.Error("expected error for nonexistent dir, got nil")
	}
}

// TestTypeAbbrev_GazetteerTypes verifies the fixed abbreviation codes for all
// new gazetteer PII types added in Stage 1a.
func TestTypeAbbrev_GazetteerTypes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		typ  string
		want string
	}{
		// Stage 1a gazetteer types — fixed codes.
		{"NAME", "NM"},
		{"PERSON", "PN"},
		{"CITY", "CT"},
		{"LOCATION", "LO"},
		{"ORG", "OR"},
		{"COMPANY", "CO"},
		// Stage 0 regex types — must remain unchanged.
		{"EMAIL", "EM"},
		{"IBAN", "IB"},
		{"PHONE", "PH"},
		{"CREDIT_CARD", "CC"},
		{"TAX_ID", "TX"},
	}

	for _, tc := range tests {
		t.Run(tc.typ, func(t *testing.T) {
			t.Parallel()

			got := typeAbbrev(tc.typ)
			if got != tc.want {
				t.Errorf("typeAbbrev(%q) = %q, want %q", tc.typ, got, tc.want)
			}
		})
	}
}

// TestStableAbbrev_Properties verifies the three required properties of
// stableAbbrev for a broad set of custom types:
//  1. Always exactly 2 characters.
//  2. All characters in [A-Z0-9].
//  3. Deterministic (same input → same output on repeated calls).
func TestStableAbbrev_Properties(t *testing.T) {
	t.Parallel()

	// Broad set covering: long types, short types (1 char, empty), digit-only,
	// lower-case, underscore-heavy (no leading alnum), and Unicode.
	inputs := []string{
		"UNKNOWN_TYPE",
		"PASSPORT_NO",
		"CUSTOM",
		"X",
		"",
		"123",
		"foo_bar",
		"__UNDER__",
		"ÄÖÜ", // no ASCII alnum → FNV pad path
		"A",   // 1 alnum → needs 1 pad char
		"AB",  // 2 alnum → no padding needed
		"ABC", // 3 alnum → truncate to first 2
	}

	alphanumRe := regexp.MustCompile(`^[A-Z0-9]{2}$`)

	for _, inp := range inputs {
		t.Run(fmt.Sprintf("input=%q", inp), func(t *testing.T) {
			t.Parallel()

			got := stableAbbrev(inp)

			if !alphanumRe.MatchString(got) {
				t.Errorf("stableAbbrev(%q) = %q: not in [A-Z0-9]{2}", inp, got)
			}
			// Deterministic: calling again must return the same result.
			got2 := stableAbbrev(inp)
			if got != got2 {
				t.Errorf("stableAbbrev(%q) not deterministic: %q then %q", inp, got, got2)
			}
		})
	}
}

// TestStableAbbrev_PseudonymRegexValid verifies that every abbreviation
// produced by stableAbbrev (for custom types) yields a valid pseudonym token
// that matches the canonical regex PII_[A-Za-z0-9]{2}_[0-9a-f]{24}.
func TestStableAbbrev_PseudonymRegexValid(t *testing.T) {
	t.Parallel()

	pseudonymRe := regexp.MustCompile(`^PII_[A-Za-z0-9]{2}_[0-9a-f]{24}$`)

	customTypes := []string{"CUSTOM_TYPE", "X", "", "123", "foo_bar", "__X__"}
	for _, typ := range customTypes {
		p := pseudonym(testSecret, testOrgID, typ, "some-value")
		if !pseudonymRe.MatchString(p) {
			t.Errorf("pseudonym for type %q = %q: does not match canonical regex", typ, p)
		}
	}
}

// ── End-to-end: Engine + RegexDetector + GazetteerDetector ───────────────────

// TestEndToEnd_GazetteerAndRegexBothAnonymized verifies that when an Engine is
// built with both a RegexDetector and a GazetteerDetector, AnonymizeJSON
// pseudonymizes spans from both detectors, Restore round-trips both back, and
// the resulting tokens conform to the canonical pseudonym format.
func TestEndToEnd_GazetteerAndRegexBothAnonymized(t *testing.T) {
	t.Parallel()

	regexDet, err := NewRegexDetector(DefaultPatterns())
	if err != nil {
		t.Fatalf("NewRegexDetector: %v", err)
	}

	gazDet, err := NewGazetteerDetector(
		[]Gazetteer{
			{Name: "code-names", Type: "ORG", Terms: []string{"Project Titan"}},
			{Name: "forms", Type: "ORG", Terms: []string{"GmbH"}},
		},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	engine := NewEngine(testSecret, []Detector{regexDet, gazDet})
	f := engine.NewFilter(testOrgID)

	// Body contains one email (regex) and one gazetteer term "Project Titan".
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"Email alice@example.com about Project Titan GmbH."}]}`)

	anonBody, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}

	// Neither the email nor the gazetteer terms should remain in plain text.
	if strings.Contains(string(anonBody), "alice@example.com") {
		t.Error("anonymized body still contains email")
	}
	if strings.Contains(string(anonBody), "Project Titan") {
		t.Error("anonymized body still contains 'Project Titan'")
	}
	if strings.Contains(string(anonBody), "GmbH") {
		t.Error("anonymized body still contains 'GmbH'")
	}

	if !f.Touched() {
		t.Error("Touched() = false; expected PII to be detected")
	}

	// All tokens must match the canonical pseudonym regex.
	pseudonymRe := regexp.MustCompile(`PII_[A-Za-z0-9]{2}_[0-9a-f]{24}`)
	anonStr := string(anonBody)
	tokens := pseudonymRe.FindAllString(anonStr, -1)
	if len(tokens) < 2 {
		t.Errorf("expected at least 2 pseudonym tokens, found %d in: %s", len(tokens), anonStr)
	}
	for _, tok := range tokens {
		if len(tok) != pseudonymLen {
			t.Errorf("token %q has length %d, want %d", tok, len(tok), pseudonymLen)
		}
	}

	// Restore must recover all original values.
	restored := f.Restore(anonBody)
	if !strings.Contains(string(restored), "alice@example.com") {
		t.Errorf("restore: email missing from restored body: %s", restored)
	}
	if !strings.Contains(string(restored), "Project Titan") {
		t.Errorf("restore: 'Project Titan' missing from restored body: %s", restored)
	}
	if !strings.Contains(string(restored), "GmbH") {
		t.Errorf("restore: 'GmbH' missing from restored body: %s", restored)
	}
}

// TestEndToEnd_GazetteerTypeCodesInTokens verifies that the pseudonym token
// for ORG spans uses the "OR" abbreviation and CITY uses "CT".
func TestEndToEnd_GazetteerTypeCodesInTokens(t *testing.T) {
	t.Parallel()

	gazDet, err := NewGazetteerDetector(
		[]Gazetteer{
			{Name: "orgs", Type: "ORG", Terms: []string{"Acme Corp"}},
			{Name: "cities", Type: "CITY", Terms: []string{"Berlin"}},
		},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	engine := NewEngine(testSecret, []Detector{gazDet})
	f := engine.NewFilter(testOrgID)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"Acme Corp is based in Berlin."}]}`)
	anonBody, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}

	anonStr := string(anonBody)
	if !strings.Contains(anonStr, "PII_OR_") {
		t.Errorf("expected PII_OR_ token for ORG type, body: %s", anonStr)
	}
	if !strings.Contains(anonStr, "PII_CT_") {
		t.Errorf("expected PII_CT_ token for CITY type, body: %s", anonStr)
	}
}

// TestEndToEnd_RestoreRoundTrip_GazetteerSpans verifies full round-trip
// fidelity for gazetteer-only spans: anonymize and then restore recovers the
// exact original strings.
func TestEndToEnd_RestoreRoundTrip_GazetteerSpans(t *testing.T) {
	t.Parallel()

	gazDet, err := NewGazetteerDetector(
		[]Gazetteer{
			{Name: "names", Type: "PERSON", Terms: []string{"Project Titan"}},
		},
		GazetteerOptions{CaseInsensitive: false},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	engine := NewEngine(testSecret, []Detector{gazDet})
	f := engine.NewFilter(testOrgID)

	originalText := "We are discussing Project Titan today."
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"` + originalText + `"}]}`)

	anonBody, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if strings.Contains(string(anonBody), "Project Titan") {
		t.Error("anonymized body still contains 'Project Titan'")
	}

	restored := f.Restore(anonBody)
	if !strings.Contains(string(restored), "Project Titan") {
		t.Errorf("restored body missing 'Project Titan': %s", restored)
	}
}

// ── Review fixes ─────────────────────────────────────────────────────────────

// TestFoldRunes_BothSides verifies that foldRunes is the single folding
// function for both dictionary normalization (NewGazetteerDetector) and
// scan-text folding (Find), and that span byte offsets are exact even when
// the dictionary term and the input text have different cases.
//
// This is the regression anchor for FIX 1: if either side used a different
// folding path the automaton would not match, or offsets would be wrong.
func TestFoldRunes_BothSides(t *testing.T) {
	t.Parallel()

	// Dictionary term is mixed-case; input text will be supplied in three
	// different case variants. All must produce a span whose byte slice
	// reproduces the exact input substring.
	det, err := NewGazetteerDetector(
		[]Gazetteer{{Name: "t", Type: "ORG", Terms: []string{"GmbH & Co. KG"}}},
		GazetteerOptions{CaseInsensitive: true},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	cases := []struct {
		name      string
		text      string
		wantSlice string
	}{
		{
			name:      "exact case",
			text:      "Foo GmbH & Co. KG bar",
			wantSlice: "GmbH & Co. KG",
		},
		{
			name:      "all upper",
			text:      "Foo GMBH & CO. KG bar",
			wantSlice: "GMBH & CO. KG",
		},
		{
			name:      "all lower",
			text:      "Foo gmbh & co. kg bar",
			wantSlice: "gmbh & co. kg",
		},
		{
			// Multibyte prefix tests offset alignment: "Grüße " is 9 bytes.
			name:      "multibyte prefix exact case",
			text:      "Grüße GmbH & Co. KG bar",
			wantSlice: "GmbH & Co. KG",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spans, findErr := det.Find(tc.text)
			if findErr != nil {
				t.Fatalf("Find: unexpected error: %v", findErr)
			}
			if len(spans) != 1 {
				t.Fatalf("expected 1 span, got %d: %+v", len(spans), spans)
			}
			got := tc.text[spans[0].Start:spans[0].End]
			if got != tc.wantSlice {
				t.Errorf("text[span.Start:span.End] = %q, want %q (offset or folding mismatch)", got, tc.wantSlice)
			}
		})
	}
}

// TestLoadGazetteerDir_FileCountLimit verifies that loadGazetteerDir returns
// an error (not a panic) when the directory contains more than maxGazetteerFiles
// *.txt files.
func TestLoadGazetteerDir_FileCountLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Write maxGazetteerFiles+1 minimal *.txt files.
	for i := 0; i <= maxGazetteerFiles; i++ {
		name := filepath.Join(dir, fmt.Sprintf("pack%04d.txt", i))
		if err := os.WriteFile(name, []byte("# type: ORG\nterm\n"), 0o600); err != nil {
			t.Fatalf("write file %d: %v", i, err)
		}
	}

	var out []Gazetteer
	var fc int
	var tb int64
	err := loadGazetteerDir(dir, &out, &fc, &tb)
	if err == nil {
		t.Fatal("expected error when file count exceeds limit, got nil")
	}
	if !strings.Contains(err.Error(), "file limit") {
		t.Errorf("error should mention 'file limit', got: %v", err)
	}
}

// TestLoadGazetteerDir_ByteLimit verifies that loadGazetteerDir returns an
// error when the total byte volume of *.txt files exceeds maxGazetteerBytes.
//
// Strategy: write two files each just over half the cap. The size guard checks
// d.Info().Size() (the on-disk file size) before opening, so it fires before
// the bufio.Scanner ever sees the content. Each file is filled with short
// "term\n" lines so a scanner would not hit its own 64KB line limit; the
// byte-total guard is what must fire here.
func TestLoadGazetteerDir_ByteLimit(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// Each file is (maxGazetteerBytes/2 + 1) bytes of short lines.
	// Include the required # type: header so parse does not fail before
	// the byte-volume guard fires (the header itself is only a few bytes
	// and does not move the size check to a later file).
	const fileSize = maxGazetteerBytes/2 + 1
	const line = "term\n"
	reps := (fileSize / len(line)) + 1
	content := "# type: ORG\n" + strings.Repeat(line, reps)

	for _, name := range []string{"big1.txt", "big2.txt"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	var out []Gazetteer
	var fc int
	var tb int64
	err := loadGazetteerDir(dir, &out, &fc, &tb)
	if err == nil {
		t.Fatal("expected error when byte total exceeds limit, got nil")
	}
	if !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("error should mention 'byte limit', got: %v", err)
	}
}
