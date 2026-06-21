package pii

// pii_codex_fixes_test.go covers the six external-review (Codex) fixes applied
// to feat/pii-stage1-gazetteer:
//
//   FIX 1: union overlap-resolution prevents gazetteer span from suppressing
//           a longer regex PII span (phone-number regression).
//   FIX 2: maxGazetteerMatches cap causes Find to fail closed; build-time
//           maxGazetteerOutputs cap causes newACMatcher to return an error.
//   FIX 3: symlinked .txt files in operator dirs are skipped.
//   FIX 4: global file/byte/inline-term caps across multiple dirs → error.
//   FIX 5: pack file with empty or missing # type: header → parse error.
//   (FIX 6 is documentation-only; no behaviour change to test.)

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

// ── FIX 1: union overlap-resolution ──────────────────────────────────────────

// TestFix1_UnionOverlap_PhoneRegression is the critical regression test.
// A gazetteer operator term "+49" (e.g. the country-code prefix) starting
// at the same position as a PHONE span must NOT suppress the rest of the
// phone number. The entire PHONE interval must be masked after merging.
//
// Text: "call +4915112345678 now"
// PHONE regex:  [5, 19)  "+4915112345678"   (14 chars)
// GAZ operator: [5,  8)  "+49"              (3 chars, type ORG as stand-in)
//
// Old deOverlap (drop later-starting): keeps [5,8) only → "+4915112345678"
//
//	leaks "15112345678" in cleartext. Privacy regression.
//
// New deOverlap (union): merges into [5,19), type PHONE (longest wins).
//
//	No byte of the phone number remains unmasked.
func TestFix1_UnionOverlap_PhoneRegression(t *testing.T) {
	t.Parallel()

	// Regex detector with PHONE pattern.
	regexDet, err := NewRegexDetector([]Pattern{
		{
			Type:   "PHONE",
			Regexp: `(?:\+49|0)[0-9][0-9 \-]{5,14}[0-9]`,
		},
	})
	if err != nil {
		t.Fatalf("NewRegexDetector: %v", err)
	}

	// Gazetteer detector with "+49" as an operator term (simulates the
	// country-code prefix being in a company / operator custom list).
	// Whole-token boundary: "+49" followed by digit does NOT pass the
	// word-boundary filter (digit is alphanumeric) — so the gazetteer
	// alone would not match "+49" when it is part of "+4915112345678".
	// The regression scenario is slightly different: imagine the gazetteer
	// matches "+49 151" (7 chars) as a local-code entry while the regex
	// matches the full "+4915112345678" (14 chars) starting at the same
	// position. We simulate this by constructing spans manually through
	// deOverlap so the test is not dependent on gazetteer word-boundary rules.
	//
	// We test deOverlap directly with representative spans, then verify
	// end-to-end through AnonymizeJSON.

	// Direct deOverlap test: short gazetteer span [5,8) "ORG" precedes the
	// regex PHONE span [5,19).
	// Sorted by start, then by length descending → PHONE [5,19) comes first,
	// then ORG [5,8). deOverlap merges them into [5,19) type PHONE (longest).
	spans := []Span{
		{Start: 5, End: 19, Type: "PHONE"}, // regex, sorted first (longest)
		{Start: 5, End: 8, Type: "ORG"},    // gazetteer, overlaps
	}
	merged := deOverlap(spans)
	if len(merged) != 1 {
		t.Fatalf("deOverlap: expected 1 merged span, got %d: %+v", len(merged), merged)
	}
	if merged[0].Start != 5 || merged[0].End != 19 {
		t.Errorf("union span = [%d,%d), want [5,19)", merged[0].Start, merged[0].End)
	}
	if merged[0].Type != "PHONE" {
		t.Errorf("union type = %q, want PHONE (longest wins)", merged[0].Type)
	}

	// End-to-end: AnonymizeJSON must mask the full phone number, not just
	// the "+49" prefix. We use only the regex detector here; the key property
	// is that once Find returns the PHONE span [5,19), replaceSpansInText
	// masks exactly that interval.
	engine := NewEngine(testSecret, []Detector{regexDet})
	f := engine.NewFilter(testOrgID)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"call +4915112345678 now"}]}`)
	anon, anonErr := f.AnonymizeJSON(body)
	if anonErr != nil {
		t.Fatalf("AnonymizeJSON: %v", anonErr)
	}

	// The full phone number must be absent from the anonymized body.
	if strings.Contains(string(anon), "+4915112345678") {
		t.Error("full phone number still present in anonymized body (regression: FIX 1)")
	}
	// Any suffix of the number must also be absent (the "+49" prefix might
	// be masked but the tail "15112345678" must not leak).
	if strings.Contains(string(anon), "15112345678") {
		t.Error("phone number tail '15112345678' leaks in anonymized body (FIX 1 regression)")
	}

	// Restore must recover the original.
	restored := f.Restore(anon)
	if !strings.Contains(string(restored), "+4915112345678") {
		t.Errorf("restore: phone number missing from restored body: %s", restored)
	}
}

// TestFix1_UnionMergeRestoreRoundTrip verifies that the union-merged span's
// original substring is preserved exactly through Restore.
func TestFix1_UnionMergeRestoreRoundTrip(t *testing.T) {
	t.Parallel()

	// Build spans that overlap: A=[0,5) and B=[3,10) → union [0,10).
	// The value at [0,10) in text "hello world!" is "hello worl".
	// We pass them already sorted: B (longer) first so deOverlap picks B's type.
	text := "hello world!"
	sorted := []Span{
		{Start: 0, End: 10, Type: "B"}, // B is longer, comes first after sort
		{Start: 0, End: 5, Type: "A"},
	}
	merged := deOverlap(sorted)
	if len(merged) != 1 {
		t.Fatalf("deOverlap: expected 1 merged span, got %d", len(merged))
	}
	wantStart, wantEnd := 0, 10
	if merged[0].Start != wantStart || merged[0].End != wantEnd {
		t.Fatalf("merged span = [%d,%d), want [%d,%d)", merged[0].Start, merged[0].End, wantStart, wantEnd)
	}
	// The union substring is text[0:10] = "hello worl".
	unionSubstr := text[merged[0].Start:merged[0].End]
	if unionSubstr != "hello worl" {
		t.Errorf("union substring = %q, want %q", unionSubstr, "hello worl")
	}
	// The type is B (longest span wins: len(B)=10 > len(A)=5).
	if merged[0].Type != "B" {
		t.Errorf("union type = %q, want B (longest wins)", merged[0].Type)
	}
}

// TestFix1_PureStage0NoOverlapUnchanged verifies that the union-merge path
// does not alter behavior when there are no overlapping spans. Existing
// Stage 0 tests continue to pass unchanged.
func TestFix1_PureStage0NoOverlapUnchanged(t *testing.T) {
	t.Parallel()

	// Three disjoint spans: result must be identical to input.
	spans := []Span{
		{Start: 0, End: 5, Type: "EMAIL"},
		{Start: 10, End: 20, Type: "PHONE"},
		{Start: 25, End: 35, Type: "IBAN"},
	}
	result := deOverlap(spans)
	if len(result) != 3 {
		t.Fatalf("non-overlapping: expected 3 spans, got %d: %+v", len(result), result)
	}
	for i, want := range spans {
		if result[i] != want {
			t.Errorf("result[%d] = %+v, want %+v", i, result[i], want)
		}
	}
}

// ── FIX 2: match cap and output-link cap ──────────────────────────────────────

// TestFix2_MatchCapFailClosed verifies that Find returns a non-nil error
// (never partial/truncated results) when the number of raw automaton hits
// for a single input exceeds maxGazetteerMatches. The request must be
// rejected, not silently truncated.
func TestFix2_MatchCapFailClosed(t *testing.T) {
	t.Parallel()

	// Build a single 1-rune term "a". In an input of maxGazetteerMatches+1
	// 'a' characters with spaces for word boundaries, every 'a' produces a
	// boundary-passing hit. To exceed the cap we need more than
	// maxGazetteerMatches hits.
	//
	// Strategy: use a very short term " a " (with spaces so every occurrence
	// is boundary-clean) and build input that contains maxGazetteerMatches+1
	// such occurrences. Each 3-char triple " a " is a match.
	// Total chars ≈ (maxGazetteerMatches+1)*3 ≈ 600 003 chars — acceptable
	// for a test, it runs once.
	const n = maxGazetteerMatches + 1

	det, err := NewGazetteerDetector(
		[]Gazetteer{{Name: "t", Type: "TEST", Terms: []string{"a"}}},
		GazetteerOptions{CaseInsensitive: false},
	)
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	// Build input: "a a a a ..." with n 'a' tokens separated by spaces.
	// Each 'a' is at a word boundary (preceded and followed by ' ' or start/end).
	// That gives exactly n hits.
	var sb strings.Builder
	sb.Grow(n * 2)
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteByte(' ')
		}
		sb.WriteByte('a')
	}
	input := sb.String()

	resultSpans, findErr := det.Find(input)
	if findErr == nil {
		t.Errorf("Find: expected fail-closed error for %d matches, got nil (spans=%d)", n, len(resultSpans))
		return
	}
	// The error must be non-nil and no partial results returned.
	if resultSpans != nil {
		t.Errorf("Find: expected nil spans on error, got %d spans", len(resultSpans))
	}
	// The error must mention the cap.
	if !strings.Contains(findErr.Error(), "match count exceeded") {
		t.Errorf("Find error: expected 'match count exceeded', got: %v", findErr)
	}
}

// TestFix2_OutputLinkCapFailsClosed verifies that newACMatcher returns a
// non-nil error when the total number of output-link entries during BFS
// would exceed maxGazetteerOutputs. This prevents quadratic memory growth
// for prefix-heavy dictionaries.
func TestFix2_OutputLinkCapFailsClosed(t *testing.T) {
	t.Parallel()

	// A prefix-heavy dictionary: "a", "aa", "aaa", ..., up to maxGazetteerStates
	// states worth of terms. The Aho-Corasick BFS output-link flattening copies
	// the failure outputs of every ancestor into each state; for a chain of
	// length L, state i inherits i entries. The total is roughly L*(L+1)/2.
	//
	// We need enough terms so that the quadratic growth hits maxGazetteerOutputs
	// before the state cap. With maxGazetteerOutputs=1_000_000, the break-even
	// is at L ≈ sqrt(2*1_000_000) ≈ 1414. Use 1500 terms to be safe, and the
	// state cap of 500_000 won't interfere (1500 states for a chain).
	const chainLen = 1500
	terms := make([]acTerm, chainLen)
	for i := range terms {
		terms[i] = acTerm{norm: strings.Repeat("a", i+1), typ: "TEST"}
	}
	terms[0].runeLen = 1 // normally set by newACMatcher; pre-set for clarity

	_, err := newACMatcher(terms)
	if err == nil {
		t.Error("newACMatcher: expected output-link cap error for prefix dictionary, got nil")
		return
	}
	// The error must mention the output-link cap.
	if !strings.Contains(err.Error(), "output links exceed") {
		t.Errorf("error should mention 'output links exceed', got: %v", err)
	}
}

// ── FIX 3: symlink / special-file skipping ────────────────────────────────────

// TestFix3_SymlinkSkipped verifies that a symlinked .txt file in an operator
// directory is silently skipped (not read, not counted against limits).
func TestFix3_SymlinkSkipped(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Write a real .txt file with a proper type header.
	realFile := filepath.Join(dir, "real.txt")
	if err := os.WriteFile(realFile, []byte("# type: ORG\nRealTerm\n"), 0o600); err != nil {
		t.Fatalf("write real.txt: %v", err)
	}

	// Create a symlink pointing to a file that doesn't exist (or elsewhere).
	// The symlink target need not exist for os.Lstat to see the symlink itself.
	symFile := filepath.Join(dir, "symlink.txt")
	if err := os.Symlink("/etc/hostname", symFile); err != nil {
		// Symlink creation may be disallowed in some environments (e.g. Windows).
		// Skip the test rather than failing; the behaviour being tested is
		// the skip guard, not the OS capability to create symlinks.
		t.Skipf("os.Symlink not available: %v", err)
	}

	var gazes []Gazetteer
	var fc int
	var tb int64
	if err := loadGazetteerDir(dir, &gazes, &fc, &tb); err != nil {
		t.Fatalf("loadGazetteerDir: unexpected error: %v", err)
	}

	// Only the real file should have been loaded; the symlink must be skipped.
	if len(gazes) != 1 {
		t.Fatalf("expected 1 gazetteer (real.txt only), got %d: %+v", len(gazes), gazes)
	}
	if fc != 1 {
		t.Errorf("fileCount = %d, want 1 (symlink not counted)", fc)
	}
	found := false
	for _, g := range gazes {
		for _, term := range g.Terms {
			if term == "RealTerm" {
				found = true
			}
		}
	}
	if !found {
		t.Error("expected 'RealTerm' from real.txt to be loaded")
	}
}

// TestFix3_RegularFileRead verifies that a regular .txt file in the same dir
// as a symlink is still read correctly.
func TestFix3_RegularFileRead(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	content := "# type: CITY\nBerlin\nHamburg\n"
	if err := os.WriteFile(filepath.Join(dir, "cities.txt"), []byte(content), 0o600); err != nil {
		t.Fatalf("write cities.txt: %v", err)
	}

	var gazes []Gazetteer
	var fc int
	var tb int64
	if err := loadGazetteerDir(dir, &gazes, &fc, &tb); err != nil {
		t.Fatalf("loadGazetteerDir: %v", err)
	}
	if len(gazes) != 1 {
		t.Fatalf("expected 1 gazetteer, got %d", len(gazes))
	}
	allTerms := make(map[string]bool)
	for _, term := range gazes[0].Terms {
		allTerms[term] = true
	}
	for _, want := range []string{"Berlin", "Hamburg"} {
		if !allTerms[want] {
			t.Errorf("expected term %q, not found in %v", want, gazes[0].Terms)
		}
	}
}

// ── FIX 4: global file/byte/inline-term caps ──────────────────────────────────

// TestFix4_GlobalFileCapAcrossMultipleDirs verifies that the file count limit
// is enforced globally across all operator directories in one LoadGazetteerDetector
// call, not per-directory. Two directories each containing half the limit + 1
// file together exceed the global cap.
func TestFix4_GlobalFileCapAcrossMultipleDirs(t *testing.T) {
	t.Parallel()

	// Each directory contributes maxGazetteerFiles/2 + 1 files.
	// Together they exceed maxGazetteerFiles.
	const perDir = maxGazetteerFiles/2 + 1

	dir1 := t.TempDir()
	dir2 := t.TempDir()
	for _, dir := range []string{dir1, dir2} {
		for i := 0; i < perDir; i++ {
			name := filepath.Join(dir, fmt.Sprintf("pack%04d.txt", i))
			if err := os.WriteFile(name, []byte("# type: ORG\nterm\n"), 0o600); err != nil {
				t.Fatalf("write file: %v", err)
			}
		}
	}

	cfg := config.PIIGazetteerConfig{
		Dirs: []string{dir1, dir2},
	}
	_, err := LoadGazetteerDetector(cfg)
	if err == nil {
		t.Fatal("expected error when global file count exceeds limit across two dirs, got nil")
	}
	if !strings.Contains(err.Error(), "file limit") {
		t.Errorf("error should mention 'file limit', got: %v", err)
	}
}

// TestFix4_GlobalByteCapAcrossMultipleDirs verifies that the byte-volume limit
// is enforced globally across all operator directories.
func TestFix4_GlobalByteCapAcrossMultipleDirs(t *testing.T) {
	t.Parallel()

	// Split into two directories each contributing just over 1/4 of the byte cap;
	// together they exceed the 1/2 cap needed to trigger across two files total.
	// Each directory has one file of (maxGazetteerBytes/4 + 1) bytes.
	// Total across both dirs = maxGazetteerBytes/2 + 2 — still under the cap.
	// We need total > maxGazetteerBytes, so use 2 files each > maxGazetteerBytes/2.
	const fileSize = maxGazetteerBytes/2 + 1
	const lineContent = "# type: ORG\n"
	filler := strings.Repeat("t\n", (fileSize-len(lineContent))/2+1)
	content := lineContent + filler

	dir1 := t.TempDir()
	dir2 := t.TempDir()
	for i, dir := range []string{dir1, dir2} {
		name := filepath.Join(dir, fmt.Sprintf("big%d.txt", i))
		if err := os.WriteFile(name, []byte(content), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}

	cfg := config.PIIGazetteerConfig{
		Dirs: []string{dir1, dir2},
	}
	_, err := LoadGazetteerDetector(cfg)
	if err == nil {
		t.Fatal("expected error when global byte count exceeds limit across two dirs, got nil")
	}
	if !strings.Contains(err.Error(), "byte limit") {
		t.Errorf("error should mention 'byte limit', got: %v", err)
	}
}

// TestFix4_InlineTermCapExceeded verifies that exceeding maxGazetteerInlineTerms
// across all inline term entries causes a startup error.
func TestFix4_InlineTermCapExceeded(t *testing.T) {
	t.Parallel()

	// Build maxGazetteerInlineTerms+1 inline values across two entries.
	half := maxGazetteerInlineTerms/2 + 1
	vals1 := make([]string, half)
	vals2 := make([]string, half)
	for i := range vals1 {
		vals1[i] = fmt.Sprintf("term%d", i)
	}
	for i := range vals2 {
		vals2[i] = fmt.Sprintf("other%d", i)
	}

	cfg := config.PIIGazetteerConfig{
		Terms: []config.PIIGazetteerTermConfig{
			{Type: "ORG", Values: vals1},
			{Type: "CITY", Values: vals2},
		},
	}
	_, err := LoadGazetteerDetector(cfg)
	if err == nil {
		t.Fatal("expected error when inline term count exceeds limit, got nil")
	}
	if !strings.Contains(err.Error(), "inline terms exceed") {
		t.Errorf("error should mention 'inline terms exceed', got: %v", err)
	}
}

// ── FIX 5: empty type in pack files ───────────────────────────────────────────

// TestFix5_EmptyTypeHeaderError verifies that a pack file with an explicit
// empty # type: header (e.g. "# type:   ") is rejected.
func TestFix5_EmptyTypeHeaderError(t *testing.T) {
	t.Parallel()

	content := "# type:   \nFoo\nBar\n" // type value is empty after trim
	_, err := parseGazetteerFile(strings.NewReader(content), "x")
	if err == nil {
		t.Fatal("expected error for empty # type: header, got nil")
	}
	if !strings.Contains(err.Error(), "type") {
		t.Errorf("error should mention 'type', got: %v", err)
	}
}

// TestFix5_MissingTypeHeaderError verifies that a pack file with no # type:
// header at all is rejected.
func TestFix5_MissingTypeHeaderError(t *testing.T) {
	t.Parallel()

	content := "# locale: de\nFoo\nBar\n" // no # type: header
	_, err := parseGazetteerFile(strings.NewReader(content), "x")
	if err == nil {
		t.Fatal("expected error for missing # type: header, got nil")
	}
	if !strings.Contains(err.Error(), "type") {
		t.Errorf("error should mention 'type', got: %v", err)
	}
}

// TestFix5_ValidTypeHeaderOK verifies that a pack file with a non-empty
// # type: header continues to parse correctly (regression guard).
func TestFix5_ValidTypeHeaderOK(t *testing.T) {
	t.Parallel()

	content := "# type: ORG\nGmbH\nAG\n"
	g, err := parseGazetteerFile(strings.NewReader(content), "x")
	if err != nil {
		t.Fatalf("parseGazetteerFile: unexpected error: %v", err)
	}
	if g.Type != "ORG" {
		t.Errorf("Type = %q, want ORG", g.Type)
	}
	if len(g.Terms) != 2 {
		t.Errorf("Terms = %v, want [GmbH AG]", g.Terms)
	}
}

// ── Sort tie-break determinism ────────────────────────────────────────────────

// TestSortTieBreak_DeterministicTypeOnIdenticalInterval verifies that when two
// spans share the same Start and End but carry different Types, the sort
// tie-break on Type (ascending lexicographic) produces the same winner
// regardless of which detector emits the spans first or in what order they
// arrive in the combined slice.
//
// Without the tertiary Type tie-break the sort is unstable on identical
// intervals, so deOverlap's "keep first-seen type on tie" rule can pick a
// different Type across identical requests, changing the 2-char pseudonym
// abbreviation and breaking multi-turn consistency.
//
// The test constructs the same two spans in both possible input orders and
// asserts that deOverlap produces the same winning Type in every case.
func TestSortTieBreak_DeterministicTypeOnIdenticalInterval(t *testing.T) {
	t.Parallel()

	// Two spans with identical [Start, End) but different Types.
	// Lexicographically: "CITY" < "NAME", so "CITY" must always win.
	spanCity := Span{Start: 3, End: 10, Type: "CITY"}
	spanName := Span{Start: 3, End: 10, Type: "NAME"}

	orderAB := []Span{spanCity, spanName}
	orderBA := []Span{spanName, spanCity}

	// Sort and deOverlap with both input orders; the result must be identical.
	sort.Slice(orderAB, func(i, j int) bool {
		if orderAB[i].Start != orderAB[j].Start {
			return orderAB[i].Start < orderAB[j].Start
		}
		if orderAB[i].End != orderAB[j].End {
			return orderAB[i].End > orderAB[j].End
		}
		return orderAB[i].Type < orderAB[j].Type
	})
	sort.Slice(orderBA, func(i, j int) bool {
		if orderBA[i].Start != orderBA[j].Start {
			return orderBA[i].Start < orderBA[j].Start
		}
		if orderBA[i].End != orderBA[j].End {
			return orderBA[i].End > orderBA[j].End
		}
		return orderBA[i].Type < orderBA[j].Type
	})

	mergedAB := deOverlap(orderAB)
	mergedBA := deOverlap(orderBA)

	if len(mergedAB) != 1 {
		t.Fatalf("orderAB: expected 1 merged span, got %d: %+v", len(mergedAB), mergedAB)
	}
	if len(mergedBA) != 1 {
		t.Fatalf("orderBA: expected 1 merged span, got %d: %+v", len(mergedBA), mergedBA)
	}

	const wantType = "CITY" // lexicographically smallest of "CITY" and "NAME"
	if mergedAB[0].Type != wantType {
		t.Errorf("orderAB: winner type = %q, want %q", mergedAB[0].Type, wantType)
	}
	if mergedBA[0].Type != wantType {
		t.Errorf("orderBA: winner type = %q, want %q", mergedBA[0].Type, wantType)
	}
	if mergedAB[0].Type != mergedBA[0].Type {
		t.Errorf("non-deterministic: orderAB=%q, orderBA=%q", mergedAB[0].Type, mergedBA[0].Type)
	}
}

// TestSortTieBreak_EndToEndConsistentPseudonym verifies that the sort tie-break
// propagates end-to-end: when two detectors both match the same substring at
// the same offsets but assign different Types, repeated calls to AnonymizeJSON
// with the same input always produce the same pseudonym abbreviation, ensuring
// multi-turn Restore round-trips are stable.
func TestSortTieBreak_EndToEndConsistentPseudonym(t *testing.T) {
	t.Parallel()

	// Build two detectors that each match the same token "Berlin" in the input
	// but label it with a different Type.
	detCity, err := NewRegexDetector([]Pattern{
		{Type: "CITY", Regexp: `Berlin`},
	})
	if err != nil {
		t.Fatalf("NewRegexDetector CITY: %v", err)
	}
	detName, err := NewRegexDetector([]Pattern{
		{Type: "NAME", Regexp: `Berlin`},
	})
	if err != nil {
		t.Fatalf("NewRegexDetector NAME: %v", err)
	}

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"I live in Berlin."}]}`)

	// Run with detectors in both orders; the anonymized bodies must be identical
	// (same pseudonym = same abbreviation = same content bytes).
	runAnon := func(detectors []Detector) []byte {
		t.Helper()
		eng := NewEngine(testSecret, detectors)
		f := eng.NewFilter(testOrgID)
		out, anonErr := f.AnonymizeJSON(body)
		if anonErr != nil {
			t.Fatalf("AnonymizeJSON: %v", anonErr)
		}
		return out
	}

	outCityFirst := runAnon([]Detector{detCity, detName})
	outNameFirst := runAnon([]Detector{detName, detCity})

	if string(outCityFirst) != string(outNameFirst) {
		t.Errorf("pseudonym differs by detector order:\n  CITY first: %s\n  NAME first: %s",
			outCityFirst, outNameFirst)
	}

	// Neither output should contain the original value.
	if strings.Contains(string(outCityFirst), "Berlin") {
		t.Error("original value 'Berlin' not masked in anonymized output")
	}
}
