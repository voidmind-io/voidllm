package pii

// gazetteer_stage1b_test.go covers the PII Stage 1b (data packs) feature.
// Tests are grouped by concern:
//
//   - de-firstnames pack: loading, metadata, expected terms, term count
//   - de-cities pack (expanded): loading, metadata, expected terms, exclusion of "Essen"
//   - GazetteerDetector behaviour with de-firstnames: whole-token, no partial match
//   - config validation: de-firstnames accepted; unknown pack rejected
//   - End-to-end: building a detector from all three packs stays under the state cap

import (
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/config"
)

// ── de-firstnames pack ───────────────────────────────────────────────────────

// TestEmbeddedPack_DeFirstnames_Loads verifies that loadEmbeddedPack returns
// without error for the "de-firstnames" pack and that the pack has the correct
// metadata fields.
func TestEmbeddedPack_DeFirstnames_Loads(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-firstnames")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-firstnames): %v", err)
	}
	if g.Name != "de-firstnames" {
		t.Errorf("Name = %q, want %q", g.Name, "de-firstnames")
	}
	if g.Locale != "de" {
		t.Errorf("Locale = %q, want %q", g.Locale, "de")
	}
	if g.Type != "NAME" {
		t.Errorf("Type = %q, want NAME (maps to NM via typeAbbrev)", g.Type)
	}
}

// TestEmbeddedPack_DeFirstnames_TermCount verifies that the pack contains a
// non-trivial number of names. The conservative curation target is 200-300
// terms; we assert at least 150 to allow for future pruning while still
// confirming the list is substantive.
func TestEmbeddedPack_DeFirstnames_TermCount(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-firstnames")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-firstnames): %v", err)
	}
	if len(g.Terms) < 150 {
		t.Errorf("de-firstnames has %d terms, want at least 150 (conservative but substantive)", len(g.Terms))
	}
}

// TestEmbeddedPack_DeFirstnames_ContainsExpectedNames verifies that a
// representative sample of well-known German given names is present.
func TestEmbeddedPack_DeFirstnames_ContainsExpectedNames(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-firstnames")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-firstnames): %v", err)
	}

	termSet := make(map[string]bool, len(g.Terms))
	for _, term := range g.Terms {
		termSet[term] = true
	}

	expected := []string{
		"Peter", "Hans", "Thomas", "Andreas", "Stefan", "Michael",
		"Anna", "Maria", "Julia", "Sabine",
	}
	for _, name := range expected {
		if !termSet[name] {
			t.Errorf("de-firstnames missing expected name %q", name)
		}
	}
}

// TestEmbeddedPack_DeFirstnames_TypeAbbrev verifies that the NAME type code
// produces the "NM" pseudonym abbreviation as documented in Stage 1a.
func TestEmbeddedPack_DeFirstnames_TypeAbbrev(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-firstnames")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-firstnames): %v", err)
	}
	if got := typeAbbrev(g.Type); got != "NM" {
		t.Errorf("typeAbbrev(%q) = %q, want NM", g.Type, got)
	}
}

// ── de-cities pack (expanded) ────────────────────────────────────────────────

// TestEmbeddedPack_DeCities_Expanded verifies that the expanded de-cities pack
// loads, has correct metadata, and contains a representative set of major
// German cities including ones not in the original minimal template.
func TestEmbeddedPack_DeCities_Expanded(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-cities")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-cities): %v", err)
	}
	if g.Type != "CITY" {
		t.Errorf("Type = %q, want CITY (maps to CT via typeAbbrev)", g.Type)
	}
	if g.Locale != "de" {
		t.Errorf("Locale = %q, want de", g.Locale)
	}

	termSet := make(map[string]bool, len(g.Terms))
	for _, term := range g.Terms {
		termSet[term] = true
	}

	// Core unambiguous German cities that must be present.
	required := []string{
		"Berlin", "Hamburg", "München", "Köln", "Frankfurt am Main",
		"Stuttgart", "Düsseldorf", "Dortmund", "Leipzig", "Dresden",
		"Hannover", "Nürnberg", "Bremen",
	}
	for _, city := range required {
		if !termSet[city] {
			t.Errorf("de-cities missing required city %q", city)
		}
	}
}

// TestEmbeddedPack_DeCities_EssenExcluded documents and asserts the
// conservative curation decision: "Essen" is excluded because "essen" (to eat)
// is one of the most common German verbs, making standalone "Essen" highly
// ambiguous in natural text and a significant source of false positives.
func TestEmbeddedPack_DeCities_EssenExcluded(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-cities")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-cities): %v", err)
	}

	for _, term := range g.Terms {
		if strings.EqualFold(term, "essen") {
			t.Errorf("de-cities should NOT contain %q (essen = to eat; excluded for conservative FP avoidance)", term)
		}
	}
}

// TestEmbeddedPack_DeCities_SiegenExcluded asserts that "Siegen" is excluded
// because "siegen" (to win/prevail) is a common German verb; whole-word
// case-insensitive matching would mask the verb in normal prose.
func TestEmbeddedPack_DeCities_SiegenExcluded(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-cities")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-cities): %v", err)
	}

	for _, term := range g.Terms {
		if strings.EqualFold(term, "siegen") {
			t.Errorf("de-cities should NOT contain %q (siegen = to win; excluded for FP avoidance)", term)
		}
	}
}

// TestEmbeddedPack_DeCities_SingenExcluded asserts that "Singen" is excluded
// because "singen" (to sing) is a common German verb; whole-word
// case-insensitive matching would mask the verb in normal prose.
func TestEmbeddedPack_DeCities_SingenExcluded(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-cities")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-cities): %v", err)
	}

	for _, term := range g.Terms {
		if strings.EqualFold(term, "singen") {
			t.Errorf("de-cities should NOT contain %q (singen = to sing; excluded for FP avoidance)", term)
		}
	}
}

// TestEmbeddedPack_DeFirstnames_ChristianExcluded asserts that "Christian" is
// absent from the de-firstnames pack. It is listed in the file's own exclusion
// header as a common English adjective and would produce false positives in
// mixed-language technical text.
func TestEmbeddedPack_DeFirstnames_ChristianExcluded(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-firstnames")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-firstnames): %v", err)
	}

	for _, term := range g.Terms {
		if strings.EqualFold(term, "christian") {
			t.Errorf("de-firstnames should NOT contain %q (common English adjective; excluded per file header)", term)
		}
	}
}

// TestEmbeddedPack_DeFirstnames_MiaExcluded asserts that "Mia" is absent from
// the de-firstnames pack. As a 3-letter token it collides with the "MIA"
// missing-in-action acronym under case-insensitive whole-word matching.
func TestEmbeddedPack_DeFirstnames_MiaExcluded(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-firstnames")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-firstnames): %v", err)
	}

	for _, term := range g.Terms {
		if strings.EqualFold(term, "mia") {
			t.Errorf("de-firstnames should NOT contain %q (collides with MIA acronym; excluded for FP avoidance)", term)
		}
	}
}

// TestEmbeddedPack_DeCities_TypeAbbrev verifies that the CITY type code
// produces the "CT" pseudonym abbreviation as documented in Stage 1a.
func TestEmbeddedPack_DeCities_TypeAbbrev(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-cities")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-cities): %v", err)
	}
	if got := typeAbbrev(g.Type); got != "CT" {
		t.Errorf("typeAbbrev(%q) = %q, want CT", g.Type, got)
	}
}

// TestEmbeddedPack_DeCities_TermCount verifies the expanded pack is
// substantially larger than the original minimal template (which had ~35
// entries). We assert at least 60 entries to confirm the expansion landed.
func TestEmbeddedPack_DeCities_TermCount(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-cities")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-cities): %v", err)
	}
	if len(g.Terms) < 60 {
		t.Errorf("de-cities (expanded) has %d terms, want at least 60", len(g.Terms))
	}
}

// ── GazetteerDetector with de-firstnames ────────────────────────────────────

// TestDeFirstnames_WholeTokenMatch verifies that a name from the pack is
// matched as a whole token in a sentence and that its byte offsets are
// correct in the returned span.
func TestDeFirstnames_WholeTokenMatch(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-firstnames")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-firstnames): %v", err)
	}

	det, err := NewGazetteerDetector([]Gazetteer{g}, GazetteerOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	text := "Bitte leiten Sie das an Peter weiter."
	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	var found bool
	for _, sp := range spans {
		if text[sp.Start:sp.End] == "Peter" {
			found = true
			if sp.Type != "NAME" {
				t.Errorf("span for Peter: Type = %q, want NAME", sp.Type)
			}
		}
	}
	if !found {
		t.Errorf("expected span for 'Peter' in %q, got spans: %+v", text, spans)
	}
}

// TestDeFirstnames_NoPartialMatch verifies the whole-token boundary filter:
// a name that appears as a prefix of a longer word must NOT be matched.
// "Andreas" must not match inside "Andreaskirche" (a church name compound).
func TestDeFirstnames_NoPartialMatch(t *testing.T) {
	t.Parallel()

	g, err := loadEmbeddedPack("de-firstnames")
	if err != nil {
		t.Fatalf("loadEmbeddedPack(de-firstnames): %v", err)
	}

	det, err := NewGazetteerDetector([]Gazetteer{g}, GazetteerOptions{CaseInsensitive: true})
	if err != nil {
		t.Fatalf("NewGazetteerDetector: %v", err)
	}

	// "Andreaskirche" contains "Andreas" as a prefix — must not match.
	// "Andreas" standalone must match.
	text := "Die Andreaskirche liegt nahe Andreas."
	spans, err := det.Find(text)
	if err != nil {
		t.Fatalf("Find: %v", err)
	}

	for _, sp := range spans {
		slice := text[sp.Start:sp.End]
		if slice == "Andreas" {
			// Verify it is the standalone occurrence, not inside "Andreaskirche".
			// The standalone "Andreas" starts after "nahe " and is at the end.
			// "Andreaskirche" starts at 0, so the match must not start at 0.
			if sp.Start == 0 {
				t.Errorf("matched 'Andreas' at offset 0 (inside Andreaskirche): %+v", sp)
			}
		}
	}

	// Confirm no span covers byte offset 0 (start of "Andreaskirche").
	for _, sp := range spans {
		if sp.Start == 0 {
			t.Errorf("a span starts at offset 0, which would be inside Andreaskirche: %q", text[sp.Start:sp.End])
		}
	}
}

// ── Config validation ────────────────────────────────────────────────────────

// TestConfig_DeFirstnamesPackValidates verifies that including "de-firstnames"
// in settings.pii.gazetteer.packs passes config validation without error.
func TestConfig_DeFirstnamesPackValidates(t *testing.T) {
	t.Parallel()

	trueVal := true
	gaz := config.PIIGazetteerConfig{
		Packs:   []string{"de-firstnames"},
		Options: config.PIIGazetteerOptionsConfig{CaseInsensitive: &trueVal},
	}

	// LoadGazetteerDetector performs the same pack-name lookup as validate.go.
	// A successful load confirms the name is known and the file parses cleanly.
	_, err := LoadGazetteerDetector(gaz)
	if err != nil {
		t.Errorf("LoadGazetteerDetector with pack 'de-firstnames' failed: %v", err)
	}
}

// TestConfig_UnknownPackStillErrors verifies that an unknown pack name still
// causes LoadGazetteerDetector to return a descriptive error, confirming the
// registry gating is still enforced after adding de-firstnames.
func TestConfig_UnknownPackStillErrors(t *testing.T) {
	t.Parallel()

	cfg := config.PIIGazetteerConfig{
		Packs: []string{"de-surnames"}, // plausible but not registered
	}

	_, err := LoadGazetteerDetector(cfg)
	if err == nil {
		t.Error("expected error for unknown pack 'de-surnames', got nil")
	}
	if !strings.Contains(err.Error(), "unknown embedded pack") {
		t.Errorf("error should mention 'unknown embedded pack', got: %v", err)
	}
}

// ── End-to-end: all three packs combined ────────────────────────────────────

// TestEndToEnd_AllThreePacksUnderStateCap verifies that building a
// GazetteerDetector from the three embedded packs (de-firstnames, de-cities,
// company-forms) succeeds without hitting the trie state cap. This exercises
// the combined deduplication and confirms the aggregate term set is well within
// the maxGazetteerStates limit.
func TestEndToEnd_AllThreePacksUnderStateCap(t *testing.T) {
	t.Parallel()

	trueVal := true
	cfg := config.PIIGazetteerConfig{
		Packs:   []string{"de-firstnames", "de-cities", "company-forms"},
		Options: config.PIIGazetteerOptionsConfig{CaseInsensitive: &trueVal},
	}

	det, err := LoadGazetteerDetector(cfg)
	if err != nil {
		t.Fatalf("LoadGazetteerDetector with all three packs: %v", err)
	}

	// Sanity-check the combined detector with a sentence that exercises all
	// three pack types: a person name, a city, and a company suffix.
	text := "Peter wohnt in München und arbeitet bei Acme GmbH."
	spans, findErr := det.Find(text)
	if findErr != nil {
		t.Fatalf("Find: %v", findErr)
	}

	typesSeen := make(map[string]bool)
	for _, sp := range spans {
		typesSeen[sp.Type] = true
	}

	if !typesSeen["NAME"] {
		t.Errorf("expected NAME span (from de-firstnames) in %q; spans: %+v", text, spans)
	}
	if !typesSeen["CITY"] {
		t.Errorf("expected CITY span (from de-cities) in %q; spans: %+v", text, spans)
	}
	if !typesSeen["ORG"] {
		t.Errorf("expected ORG span (from company-forms) in %q; spans: %+v", text, spans)
	}
}

// TestEndToEnd_PacksAreOptIn confirms that NOT listing a pack in cfg.Packs
// means its terms do not appear in the detector — the pack is strictly opt-in.
// We build a detector with only company-forms and assert that a German first
// name from de-firstnames produces no span.
func TestEndToEnd_PacksAreOptIn(t *testing.T) {
	t.Parallel()

	trueVal := true
	cfg := config.PIIGazetteerConfig{
		Packs:   []string{"company-forms"}, // de-firstnames deliberately absent
		Options: config.PIIGazetteerOptionsConfig{CaseInsensitive: &trueVal},
	}

	det, err := LoadGazetteerDetector(cfg)
	if err != nil {
		t.Fatalf("LoadGazetteerDetector: %v", err)
	}

	// "Peter" is in de-firstnames; it must NOT be matched when only
	// company-forms is loaded.
	text := "Bitte fragen Sie Peter."
	spans, findErr := det.Find(text)
	if findErr != nil {
		t.Fatalf("Find: %v", findErr)
	}

	for _, sp := range spans {
		if text[sp.Start:sp.End] == "Peter" {
			t.Errorf("'Peter' was matched even though de-firstnames is not loaded; span: %+v", sp)
		}
	}
}
