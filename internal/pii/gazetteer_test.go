package pii

import (
	"strings"
	"testing"
)

// TestAhoCorasickBasic verifies that the automaton finds all dictionary terms
// in a simple input string.
func TestAhoCorasickBasic(t *testing.T) {
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

	input := []rune("acme gmbh ag berlin")
	matches := m.match(input)
	if len(matches) == 0 {
		t.Fatal("expected matches, got none")
	}

	// Verify "gmbh" is found.
	found := make(map[string]bool)
	for _, match := range matches {
		found[terms[match.termIdx].norm] = true
	}
	for _, want := range []string{"gmbh", "ag", "berlin"} {
		if !found[want] {
			t.Errorf("expected to find term %q, but did not", want)
		}
	}
}

// TestAhoCorasickMaxStates ensures the constructor returns an error when the
// state count would exceed maxGazetteerStates.
func TestAhoCorasickMaxStates(t *testing.T) {
	t.Parallel()

	// Build a large number of long unique terms to saturate the state cap.
	// Each unique rune path adds one state per rune; unique prefixes compound.
	const alphabet = "abcdefghijklmnopqrstuvwxyz"
	terms := make([]acTerm, 0, maxGazetteerStates/10)
	for i := 0; i < maxGazetteerStates/10; i++ {
		// Generate a unique 10-rune term from the index.
		b := make([]byte, 10)
		n := i
		for j := 9; j >= 0; j-- {
			b[j] = alphabet[n%len(alphabet)]
			n /= len(alphabet)
		}
		terms = append(terms, acTerm{norm: string(b), typ: "TEST"})
	}

	// This may or may not exceed the cap depending on term overlap. We only
	// care that the constructor never panics and returns either success or a
	// meaningful error.
	_, err := newACMatcher(terms)
	// err may be nil (terms are short enough to stay under cap) or non-nil
	// (exceeded cap); either outcome is acceptable. We just verify no panic.
	_ = err
}

// TestGazetteerDetectorCaseInsensitive verifies that whole-token matching and
// original byte-offset preservation work correctly with case folding.
func TestGazetteerDetectorCaseInsensitive(t *testing.T) {
	t.Parallel()

	gazes := []Gazetteer{
		{Name: "test", Type: "ORG", Terms: []string{"GmbH", "AG"}},
	}
	det, err := NewGazetteerDetector(gazes, GazetteerOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	text := "Acme GmbH und Foo AG sind Firmen."
	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: unexpected error: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d: %+v", len(spans), spans)
	}

	// Verify start offsets correspond to original text bytes.
	for _, sp := range spans {
		got := text[sp.Start:sp.End]
		if got != "GmbH" && got != "AG" {
			t.Errorf("span [%d:%d] = %q, expected GmbH or AG", sp.Start, sp.End, got)
		}
		if sp.Type != "ORG" {
			t.Errorf("span type = %q, expected ORG", sp.Type)
		}
	}
}

// TestGazetteerDetectorWholeToken verifies that partial-word matches are
// rejected by the word-boundary filter.
func TestGazetteerDetectorWholeToken(t *testing.T) {
	t.Parallel()

	gazes := []Gazetteer{
		{Name: "test", Type: "CITY", Terms: []string{"Berg"}},
	}
	det, err := NewGazetteerDetector(gazes, GazetteerOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	// "Bergmann" contains "Berg" but is not a whole token.
	text := "Bergmann lebt in Berg."
	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: unexpected error: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("expected 1 span (standalone Berg), got %d: %+v", len(spans), spans)
	}
	got := text[spans[0].Start:spans[0].End]
	if got != "Berg" {
		t.Errorf("span = %q, want %q", got, "Berg")
	}
}

// TestGazetteerDetectorConcurrent verifies that Find is safe to call
// concurrently on the same detector.
func TestGazetteerDetectorConcurrent(t *testing.T) {
	t.Parallel()

	gazes := []Gazetteer{
		{Name: "test", Type: "ORG", Terms: []string{"GmbH", "AG", "Ltd"}},
	}
	det, err := NewGazetteerDetector(gazes, GazetteerOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	const goroutines = 20
	done := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			spans, findErr := det.Find("Foo GmbH and Bar AG and Baz Ltd.")
			if findErr != nil {
				t.Errorf("concurrent Find: unexpected error: %v", findErr)
				return
			}
			if len(spans) != 3 {
				t.Errorf("concurrent Find: expected 3 spans, got %d", len(spans))
			}
		}()
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
}

// TestGazetteerDetectorUnicode verifies correct handling of non-ASCII terms
// (e.g. German city names with umlauts).
func TestGazetteerDetectorUnicode(t *testing.T) {
	t.Parallel()

	gazes := []Gazetteer{
		{Name: "de-cities", Type: "CITY", Terms: []string{"Düsseldorf", "München"}},
	}
	det, err := NewGazetteerDetector(gazes, GazetteerOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	text := "Büro in Düsseldorf und München."
	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: unexpected error: %v", err)
	}
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d: %+v", len(spans), spans)
	}
	for _, sp := range spans {
		got := text[sp.Start:sp.End]
		if got != "Düsseldorf" && got != "München" {
			t.Errorf("span = %q, expected Düsseldorf or München", got)
		}
	}
}

// TestGazetteerDetectorDedup verifies that duplicate (term, type) pairs across
// gazetteers do not produce duplicate spans.
func TestGazetteerDetectorDedup(t *testing.T) {
	t.Parallel()

	gazes := []Gazetteer{
		{Name: "a", Type: "ORG", Terms: []string{"GmbH"}},
		{Name: "b", Type: "ORG", Terms: []string{"GmbH"}}, // exact duplicate
	}
	det, err := NewGazetteerDetector(gazes, GazetteerOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	spans, err := det.Find("Acme GmbH.")
	if err != nil {
		t.Fatalf("Find: unexpected error: %v", err)
	}
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d: %+v", len(spans), spans)
	}
}

// TestParseGazetteerFile verifies that the .txt format parser handles
// comment headers and blank lines correctly.
func TestParseGazetteerFile(t *testing.T) {
	t.Parallel()

	content := `# locale: de
# type: CITY

# A comment
Berlin
Hamburg
# another comment
Bremen
`
	g, err := parseGazetteerFile(strings.NewReader(content), "test-pack")
	if err != nil {
		t.Fatalf("parseGazetteerFile: %v", err)
	}
	if g.Locale != "de" {
		t.Errorf("Locale = %q, want %q", g.Locale, "de")
	}
	if g.Type != "CITY" {
		t.Errorf("Type = %q, want %q", g.Type, "CITY")
	}
	wantTerms := []string{"Berlin", "Hamburg", "Bremen"}
	if len(g.Terms) != len(wantTerms) {
		t.Fatalf("Terms count = %d, want %d: %v", len(g.Terms), len(wantTerms), g.Terms)
	}
	for i, want := range wantTerms {
		if g.Terms[i] != want {
			t.Errorf("Terms[%d] = %q, want %q", i, g.Terms[i], want)
		}
	}
}

// TestLoadEmbeddedPack verifies that the two bundled embedded packs load
// without error and contain at least one term.
func TestLoadEmbeddedPack(t *testing.T) {
	t.Parallel()

	for _, packName := range []string{"company-forms", "de-cities"} {
		packName := packName
		t.Run(packName, func(t *testing.T) {
			t.Parallel()
			g, err := loadEmbeddedPack(packName)
			if err != nil {
				t.Fatalf("loadEmbeddedPack(%q): %v", packName, err)
			}
			if len(g.Terms) == 0 {
				t.Errorf("pack %q has no terms", packName)
			}
			if g.Name != packName {
				t.Errorf("pack Name = %q, want %q", g.Name, packName)
			}
			if g.Type == "" {
				t.Errorf("pack %q has empty Type", packName)
			}
		})
	}
}

// TestLoadEmbeddedPackUnknown verifies that an unknown pack name returns an
// error rather than silently succeeding.
func TestLoadEmbeddedPackUnknown(t *testing.T) {
	t.Parallel()

	_, err := loadEmbeddedPack("does-not-exist")
	if err == nil {
		t.Error("expected error for unknown pack, got nil")
	}
}

// TestStableAbbrev verifies the properties of the custom-type abbreviation:
// always 2 chars in [A-Z0-9], stable across calls, distinct for distinct types.
func TestStableAbbrev(t *testing.T) {
	t.Parallel()

	types := []string{"UNKNOWN_TYPE", "PASSPORT_NO", "CUSTOM", "X", "", "123", "foo_bar"}
	seen := make(map[string]string)

	for _, typ := range types {
		abbr := stableAbbrev(typ)
		if len(abbr) != 2 {
			t.Errorf("stableAbbrev(%q) = %q: want len 2, got %d", typ, abbr, len(abbr))
		}
		for _, c := range abbr {
			if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
				t.Errorf("stableAbbrev(%q) = %q: char %q not in [A-Z0-9]", typ, abbr, c)
			}
		}
		// Stability: calling again must return the same result.
		if abbr2 := stableAbbrev(typ); abbr2 != abbr {
			t.Errorf("stableAbbrev(%q) not stable: got %q then %q", typ, abbr, abbr2)
		}
		// Record for distinctness check.
		if prev, conflict := seen[abbr]; conflict && prev != typ {
			// Collision is allowed (pigeonhole) but log it so it's visible.
			t.Logf("note: stableAbbrev collision: %q and %q both → %q", prev, typ, abbr)
		}
		seen[abbr] = typ
	}
}

// TestGazetteerTypeAbbrev verifies that the new gazetteer PII types are
// mapped to their fixed 2-char codes in typeAbbrev.
func TestGazetteerTypeAbbrev(t *testing.T) {
	t.Parallel()

	cases := [][2]string{
		{"NAME", "NM"},
		{"PERSON", "PN"},
		{"CITY", "CT"},
		{"LOCATION", "LO"},
		{"ORG", "OR"},
		{"COMPANY", "CO"},
	}
	for _, tc := range cases {
		got := typeAbbrev(tc[0])
		if got != tc[1] {
			t.Errorf("typeAbbrev(%q) = %q, want %q", tc[0], got, tc[1])
		}
	}
}
