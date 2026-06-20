package pii

// pii_test.go is in package pii (white-box) so it can reach unexported
// helpers: pseudonym, normalizeValue, typeAbbrev, replaceSpansInText, deOverlap.

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

// ── helpers ──────────────────────────────────────────────────────────────────

// testSecret is a fixed 32-byte secret used by all pseudonym tests. It must
// not be a real installation secret — it exists only to make tests
// deterministic.
var testSecret = []byte("test-secret-32-bytes-0000000000!")

// testOrgID is the fixed organisation ID used in tests.
const testOrgID = "org-test-001"

// newTestEngine creates an Engine backed by the DefaultPatterns detector.
func newTestEngine(t *testing.T) *Engine {
	t.Helper()
	d, err := NewRegexDetector(DefaultPatterns())
	if err != nil {
		t.Fatalf("NewRegexDetector(DefaultPatterns()): %v", err)
	}
	return NewEngine(testSecret, []Detector{d})
}

// newTestFilter is shorthand for engine.NewFilter in tests.
func newTestFilter(t *testing.T) *Filter {
	t.Helper()
	return newTestEngine(t).NewFilter(testOrgID)
}

// mustAnonymize calls AnonymizeJSON and fails the test on error.
func mustAnonymize(t *testing.T, f *Filter, body []byte) []byte {
	t.Helper()
	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: unexpected error: %v", err)
	}
	return out
}

// ── RegexDetector / DefaultPatterns ──────────────────────────────────────────

func TestRegexDetector_DefaultPatterns_Recognition(t *testing.T) {
	t.Parallel()

	d, err := NewRegexDetector(DefaultPatterns())
	if err != nil {
		t.Fatalf("NewRegexDetector: %v", err)
	}

	tests := []struct {
		name      string
		text      string
		wantType  string
		wantStart int
		wantEnd   int
	}{
		{
			name:      "german iban",
			text:      "IBAN: DE12345678901234567890",
			wantType:  "IBAN",
			wantStart: 6,
			wantEnd:   28,
		},
		{
			name:      "email address",
			text:      "contact max.mustermann+test@example.de please",
			wantType:  "EMAIL",
			wantStart: 8,
			wantEnd:   38,
		},
		{
			// "+4915112345678" is 14 characters starting at offset 5 → end is 19.
			name:      "german phone +49 prefix",
			text:      "call +4915112345678 now",
			wantType:  "PHONE",
			wantStart: 5,
			wantEnd:   19,
		},
		{
			name:      "credit card number",
			text:      "card 4111111111111111 expired",
			wantType:  "CREDIT_CARD",
			wantStart: 5,
			wantEnd:   21,
		},
		{
			name:      "german tax id",
			text:      "steuer-id: 12345678901 fertig",
			wantType:  "TAX_ID",
			wantStart: 11,
			wantEnd:   22,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spans := d.Find(tc.text)
			if len(spans) == 0 {
				t.Fatalf("Find(%q): no spans, want at least one", tc.text)
			}
			// Find the expected span.
			found := false
			for _, s := range spans {
				if s.Type == tc.wantType && s.Start == tc.wantStart && s.End == tc.wantEnd {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("Find(%q) = %+v, want span {Type:%q, Start:%d, End:%d}",
					tc.text, spans, tc.wantType, tc.wantStart, tc.wantEnd)
			}
		})
	}
}

func TestRegexDetector_NoMatchInCleanText(t *testing.T) {
	t.Parallel()

	d, err := NewRegexDetector(DefaultPatterns())
	if err != nil {
		t.Fatalf("NewRegexDetector: %v", err)
	}

	text := "the quick brown fox jumps over the lazy dog"
	spans := d.Find(text)
	if len(spans) != 0 {
		t.Errorf("Find(%q) = %+v, want no spans", text, spans)
	}
}

func TestRegexDetector_NonOverlappingSpans(t *testing.T) {
	t.Parallel()

	// Use a single custom pattern to generate two adjacent matches.
	d, err := NewRegexDetector([]Pattern{
		{Type: "EMAIL", Regexp: `\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`},
	})
	if err != nil {
		t.Fatalf("NewRegexDetector: %v", err)
	}

	text := "send to alice@example.com and bob@example.com today"
	spans := d.Find(text)
	if len(spans) != 2 {
		t.Fatalf("Find(%q) = %+v, want 2 non-overlapping spans", text, spans)
	}
	// Verify no overlaps.
	for i := 1; i < len(spans); i++ {
		if spans[i].Start < spans[i-1].End {
			t.Errorf("span[%d] (start=%d) overlaps span[%d] (end=%d)",
				i, spans[i].Start, i-1, spans[i-1].End)
		}
	}
	// Verify correct positions.
	if spans[0].Start != strings.Index(text, "alice@example.com") {
		t.Errorf("span[0].Start = %d, want %d", spans[0].Start, strings.Index(text, "alice@example.com"))
	}
	if spans[1].Start != strings.Index(text, "bob@example.com") {
		t.Errorf("span[1].Start = %d, want %d", spans[1].Start, strings.Index(text, "bob@example.com"))
	}
}

func TestNewRegexDetector_InvalidRegexp(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		patterns []Pattern
	}{
		{
			name: "empty type returns error",
			patterns: []Pattern{
				{Type: "", Regexp: `\d+`},
			},
		},
		{
			name: "empty regexp returns error",
			patterns: []Pattern{
				{Type: "CUSTOM", Regexp: ""},
			},
		},
		{
			name: "invalid regexp syntax returns error",
			patterns: []Pattern{
				{Type: "CUSTOM", Regexp: `[invalid`},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := NewRegexDetector(tc.patterns)
			if err == nil {
				t.Errorf("NewRegexDetector(%v): expected error, got nil", tc.patterns)
			}
		})
	}
}

// ── pseudonym ────────────────────────────────────────────────────────────────

func TestPseudonym_Deterministic(t *testing.T) {
	t.Parallel()

	secret := testSecret
	typ := "EMAIL"
	value := "alice@example.com"

	first := pseudonym(secret, testOrgID, typ, value)
	second := pseudonym(secret, testOrgID, typ, value)
	third := pseudonym(secret, testOrgID, typ, value)

	if first != second || second != third {
		t.Errorf("pseudonym is not deterministic: %q, %q, %q", first, second, third)
	}
}

func TestPseudonym_DifferentValues(t *testing.T) {
	t.Parallel()

	secret := testSecret
	typ := "EMAIL"

	p1 := pseudonym(secret, testOrgID, typ, "alice@example.com")
	p2 := pseudonym(secret, testOrgID, typ, "bob@example.com")

	if p1 == p2 {
		t.Errorf("different values produced the same pseudonym %q", p1)
	}
}

func TestPseudonym_DifferentSecrets(t *testing.T) {
	t.Parallel()

	value := "alice@example.com"
	typ := "EMAIL"

	secret1 := []byte("secret-aaaaaaaaaaaaaaaaaaaaaaaaaaa")
	secret2 := []byte("secret-bbbbbbbbbbbbbbbbbbbbbbbbbbb")

	p1 := pseudonym(secret1, testOrgID, typ, value)
	p2 := pseudonym(secret2, testOrgID, typ, value)

	if p1 == p2 {
		t.Errorf("different secrets produced the same pseudonym %q", p1)
	}
}

func TestPseudonym_DifferentOrgs(t *testing.T) {
	t.Parallel()

	// Same secret + type + value but different org → different pseudonym.
	// This prevents cross-tenant correlation.
	value := "alice@example.com"
	typ := "EMAIL"

	p1 := pseudonym(testSecret, "org-a", typ, value)
	p2 := pseudonym(testSecret, "org-b", typ, value)

	if p1 == p2 {
		t.Errorf("different orgs produced the same pseudonym %q; cross-tenant isolation broken", p1)
	}
}

func TestPseudonym_DifferentTypes(t *testing.T) {
	t.Parallel()

	// Same value under two types must produce different pseudonyms.
	// Prevents cross-type correlation for values that match multiple patterns.
	value := "12345678901"

	p1 := pseudonym(testSecret, testOrgID, "TAX_ID", value)
	p2 := pseudonym(testSecret, testOrgID, "PHONE", value)

	if p1 == p2 {
		t.Errorf("different types produced the same pseudonym %q; cross-type isolation broken", p1)
	}
}

// TestPseudonymConstants verifies that the package-level pseudonymLen and
// pseudonymMarker constants match pseudonyms actually produced by Engine.NewFilter.
// This test is the authoritative cross-check between the constants and the
// pseudonym() implementation — if either changes without the other, this test
// will catch the divergence.
func TestPseudonymConstants(t *testing.T) {
	t.Parallel()

	eng := newTestEngine(t)
	f := eng.NewFilter(testOrgID)

	// Anonymize a body that will produce at least one pseudonym.
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"email: const@example.com and IBAN DE12345678901234567890"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if !f.Touched() {
		t.Fatal("Touched() = false; no pseudonyms produced")
	}

	for p := range f.rev {
		// Every pseudonym must be exactly pseudonymLen bytes.
		if len(p) != pseudonymLen {
			t.Errorf("pseudonym %q: len = %d, want pseudonymLen = %d", p, len(p), pseudonymLen)
		}
		// Every pseudonym must begin with pseudonymMarker.
		if len(p) < len(pseudonymMarker) || p[:len(pseudonymMarker)] != pseudonymMarker {
			t.Errorf("pseudonym %q does not start with pseudonymMarker %q", p, pseudonymMarker)
		}
	}
}

func TestPseudonym_FixedLengthPerType(t *testing.T) {
	t.Parallel()

	// All pseudonyms for a given type must have the same length.
	// Format: "PII_<2-char abbr>_<24 hex chars>" = 4 + 2 + 1 + 24 = 31 chars.
	expectedLen := pseudonymLen

	types := []struct {
		typ    string
		values []string
	}{
		{"EMAIL", []string{"a@b.com", "test.user+alias@domain.co.uk", "x@y.z"}},
		{"IBAN", []string{"DE12345678901234567890", "DE00000000000000000000"}},
		{"PHONE", []string{"+4915112345678", "030123456", "0800123456"}},
		{"CREDIT_CARD", []string{"4111111111111111", "5500005555555559"}},
		{"TAX_ID", []string{"12345678901", "99999999999"}},
	}

	for _, tc := range types {
		t.Run(tc.typ, func(t *testing.T) {
			t.Parallel()

			var lengths []int
			for _, v := range tc.values {
				p := pseudonym(testSecret, testOrgID, tc.typ, v)
				lengths = append(lengths, len(p))
			}
			for i, l := range lengths {
				if l != expectedLen {
					t.Errorf("pseudonym(%q, %q): len = %d, want %d",
						tc.typ, tc.values[i], l, expectedLen)
				}
			}
			// All same type must have the same length.
			for i := 1; i < len(lengths); i++ {
				if lengths[i] != lengths[0] {
					t.Errorf("pseudonym length inconsistency for type %q: %d != %d",
						tc.typ, lengths[i], lengths[0])
				}
			}
		})
	}
}

func TestPseudonym_AlphanumericFormat(t *testing.T) {
	t.Parallel()

	// Pseudonyms must only contain [A-Za-z0-9_] characters.
	validChars := regexp.MustCompile(`^[A-Za-z0-9_]+$`)

	types := []string{"EMAIL", "IBAN", "PHONE", "CREDIT_CARD", "TAX_ID", "UNKNOWN"}
	values := []string{"test@example.com", "DE12345678901234567890", "+4915112345678", "4111111111111111", "12345678901", "anything"}

	for i, typ := range types {
		p := pseudonym(testSecret, testOrgID, typ, values[i])
		if !validChars.MatchString(p) {
			t.Errorf("pseudonym(%q, %q) = %q contains characters outside [A-Za-z0-9_]", typ, values[i], p)
		}
	}
}

func TestPseudonym_EmailNormalization(t *testing.T) {
	t.Parallel()

	// EMAIL normalization: lowercase + trim. Same canonical form → same pseudonym.
	tests := []struct {
		a, b string
		same bool
	}{
		// These normalize to the same value.
		{"Max@Example.COM", "max@example.com", true},
		{"  alice@test.org  ", "alice@test.org", true},
		{"User+Tag@DOMAIN.NET", "user+tag@domain.net", true},
		// These normalize to different values.
		{"alice@example.com", "bob@example.com", false},
	}

	for _, tc := range tests {
		t.Run(tc.a+"_vs_"+tc.b, func(t *testing.T) {
			t.Parallel()

			pA := pseudonym(testSecret, testOrgID, "EMAIL", tc.a)
			pB := pseudonym(testSecret, testOrgID, "EMAIL", tc.b)

			if tc.same && pA != pB {
				t.Errorf("pseudonym(EMAIL, %q) = %q, pseudonym(EMAIL, %q) = %q, want equal",
					tc.a, pA, tc.b, pB)
			}
			if !tc.same && pA == pB {
				t.Errorf("pseudonym(EMAIL, %q) = pseudonym(EMAIL, %q) = %q, want different",
					tc.a, tc.b, pA)
			}
		})
	}
}

func TestNormalizeValue_NonEmailTrimOnly(t *testing.T) {
	t.Parallel()

	// Non-EMAIL types: only whitespace trimming, no case folding.
	tests := []struct {
		typ   string
		input string
		want  string
	}{
		{"IBAN", "  DE12345678901234567890  ", "DE12345678901234567890"},
		{"IBAN", "DE12345678901234567890", "DE12345678901234567890"},
		{"PHONE", " +4915112345678 ", "+4915112345678"},
		// Case is preserved for non-EMAIL types.
		{"CUSTOM", "UPPERCASE-VALUE", "UPPERCASE-VALUE"},
	}

	for _, tc := range tests {
		t.Run(tc.typ+"_"+tc.input, func(t *testing.T) {
			t.Parallel()

			got := normalizeValue(tc.typ, tc.input)
			if got != tc.want {
				t.Errorf("normalizeValue(%q, %q) = %q, want %q", tc.typ, tc.input, got, tc.want)
			}
		})
	}
}

// ── Filter: AnonymizeJSON / Restore round-trip ────────────────────────────────

func TestFilter_RoundTrip_StringContent(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"Please contact max.mustermann@example.com about IBAN DE12345678901234567890."}]}`)
	anonBody := mustAnonymize(t, f, body)

	// Anonymized body must not contain the original PII.
	if strings.Contains(string(anonBody), "max.mustermann@example.com") {
		t.Error("anonymized body still contains original email")
	}
	if strings.Contains(string(anonBody), "DE12345678901234567890") {
		t.Error("anonymized body still contains original IBAN")
	}

	// Restore must recover original PII.
	restored := f.Restore(anonBody)
	if !strings.Contains(string(restored), "max.mustermann@example.com") {
		t.Errorf("restored body does not contain original email; got: %s", restored)
	}
	if !strings.Contains(string(restored), "DE12345678901234567890") {
		t.Errorf("restored body does not contain original IBAN; got: %s", restored)
	}
}

func TestFilter_NonContentFieldsUntouched(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	// Put a PII-like value in a non-covered field (here: model name).
	body := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hello world"}]}`)
	anonBody := mustAnonymize(t, f, body)

	// The model field must remain unchanged.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(anonBody, &doc); err != nil {
		t.Fatalf("anonBody is not valid JSON: %v\nbody: %s", err, anonBody)
	}
	var model string
	if err := json.Unmarshal(doc["model"], &model); err != nil {
		t.Fatalf("cannot unmarshal model field: %v", err)
	}
	if model != "gpt-4o" {
		t.Errorf("model field = %q, want %q", model, "gpt-4o")
	}

	// Content was PII-free, so structure stays valid and touched is false.
	if f.Touched() {
		t.Error("Touched() = true, want false (no PII in content)")
	}
}

func TestFilter_JSONStructureIntact(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"system","content":"You are helpful."},{"role":"user","content":"My email is user@example.com."}]}`)
	anonBody := mustAnonymize(t, f, body)

	// Result must be valid JSON.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(anonBody, &doc); err != nil {
		t.Fatalf("anonBody is not valid JSON: %v\nbody: %s", err, anonBody)
	}

	// Messages array must be parseable.
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(doc["messages"], &messages); err != nil {
		t.Fatalf("messages is not valid JSON array: %v", err)
	}
	if len(messages) != 2 {
		t.Fatalf("messages length = %d, want 2", len(messages))
	}

	// role fields must be intact.
	var role0, role1 string
	_ = json.Unmarshal(messages[0]["role"], &role0)
	_ = json.Unmarshal(messages[1]["role"], &role1)
	if role0 != "system" {
		t.Errorf("messages[0].role = %q, want %q", role0, "system")
	}
	if role1 != "user" {
		t.Errorf("messages[1].role = %q, want %q", role1, "user")
	}
}

func TestFilter_ArrayContentParts(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4-vision","messages":[{"role":"user","content":[{"type":"text","text":"send to alice@example.com"},{"type":"image_url","image_url":{"url":"https://example.com/img.png"}}]}]}`)
	anonBody := mustAnonymize(t, f, body)

	// The text part must have been anonymized.
	if strings.Contains(string(anonBody), "alice@example.com") {
		t.Error("anonymized body still contains original email in text part")
	}

	// The image_url part must be structurally unchanged (URL is not PII-scanned).
	if !strings.Contains(string(anonBody), "image_url") {
		t.Error("image_url part was removed from array content")
	}
	if !strings.Contains(string(anonBody), "https://example.com/img.png") {
		t.Error("image URL was modified or removed")
	}

	// Restore must put the email back.
	restored := f.Restore(anonBody)
	if !strings.Contains(string(restored), "alice@example.com") {
		t.Errorf("restored body does not contain original email; got: %s", restored)
	}
}

func TestFilter_Touched_FalseWhenNoPII(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"What is the capital of France?"}]}`)
	_ = mustAnonymize(t, f, body)

	if f.Touched() {
		t.Error("Touched() = true, want false when content contains no PII")
	}
}

func TestFilter_Touched_TrueWhenPIIReplaced(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"My email is anon@example.com."}]}`)
	_ = mustAnonymize(t, f, body)

	if !f.Touched() {
		t.Error("Touched() = false, want true when email PII was detected")
	}
}

func TestFilter_ConsistencyWithinRequest(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	// Same email appears twice: must get the same pseudonym both times.
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"First: alice@example.com. Second: alice@example.com."}]}`)
	anonBody := mustAnonymize(t, f, body)

	// Count how many distinct PII_ tokens appear.
	content := string(anonBody)
	// Find all PII_EM_ tokens (24 hex chars now).
	re := regexp.MustCompile(`PII_EM_[0-9a-f]{24}`)
	matches := re.FindAllString(content, -1)
	if len(matches) < 2 {
		t.Fatalf("expected at least 2 pseudonym occurrences, got: %v\nbody: %s", matches, content)
	}
	// All must be identical.
	for i, m := range matches {
		if m != matches[0] {
			t.Errorf("match[%d] = %q, want same as match[0] = %q", i, m, matches[0])
		}
	}
}

func TestFilter_FailClosed_UnparseableBody(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`not json at all { broken`)
	_, err := f.AnonymizeJSON(body)

	// Must return an error (fail-closed), not the original body.
	if err == nil {
		t.Error("AnonymizeJSON(invalid JSON) returned nil error; expected fail-closed error")
	}
	// Error message must be content-free.
	if strings.Contains(err.Error(), "not json") || strings.Contains(err.Error(), "broken") {
		t.Errorf("error message contains body content: %q", err.Error())
	}

	if f.Touched() {
		t.Error("Touched() = true after fail-closed path, want false")
	}
}

func TestFilter_AnonymizeJSON_NoMessagesKey(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	// Valid JSON but no "messages" key — treated as completion-style body.
	// The body contains no covered fields with PII, so it should succeed
	// and return a copy of the original.
	body := []byte(`{"model":"gpt-4","other":"hello"}`)
	out, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON(no messages key): unexpected error: %v", err)
	}
	if string(out) != string(body) {
		t.Errorf("AnonymizeJSON(no PII) returned different body: %q vs %q", out, body)
	}
	if f.Touched() {
		t.Error("Touched() = true, want false when no PII fields present")
	}
}

func TestFilter_AnonymizeJSON_CompletionPromptString(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-3.5-turbo-instruct","prompt":"My email is alice@example.com"}`)
	anonBody, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if strings.Contains(string(anonBody), "alice@example.com") {
		t.Error("anonymized body still contains original email in prompt string")
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in prompt, want true")
	}
	restored := f.Restore(anonBody)
	if !strings.Contains(string(restored), "alice@example.com") {
		t.Errorf("restored body missing original email: %s", restored)
	}
}

func TestFilter_AnonymizeJSON_CompletionPromptArray(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-3.5-turbo-instruct","prompt":["Hello","My IBAN is DE12345678901234567890"]}`)
	anonBody, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if strings.Contains(string(anonBody), "DE12345678901234567890") {
		t.Error("anonymized body still contains IBAN in prompt array")
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in prompt array, want true")
	}
}

// TestFilter_AnonymizeJSON_CompletionPromptTokenArray verifies that integer
// token arrays in the "prompt" field are passed through unchanged. Token IDs
// are PII-free by definition and must never be rejected or modified.
func TestFilter_AnonymizeJSON_CompletionPromptTokenArray(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	// int[] prompt — a flat token-ID array.
	body := []byte(`{"model":"gpt-3.5-turbo-instruct","prompt":[1234,5678,91011]}`)
	anonBody, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON(int[] prompt): unexpected error: %v", err)
	}
	if string(anonBody) != string(body) {
		t.Errorf("AnonymizeJSON(int[] prompt) mutated body: got %q, want %q", anonBody, body)
	}
	if f.Touched() {
		t.Error("Touched() = true after int[] prompt, want false")
	}

	f2 := newTestFilter(t)

	// Mixed prompt: string elements are scanned, int elements pass through.
	mixed := []byte(`{"model":"gpt-3.5-turbo-instruct","prompt":["hello alice@example.com",42,99]}`)
	anonMixed, err := f2.AnonymizeJSON(mixed)
	if err != nil {
		t.Fatalf("AnonymizeJSON(mixed prompt): unexpected error: %v", err)
	}
	if strings.Contains(string(anonMixed), "alice@example.com") {
		t.Error("AnonymizeJSON(mixed prompt): string element still contains PII")
	}
	// Integer elements must be preserved verbatim.
	if !strings.Contains(string(anonMixed), "42") || !strings.Contains(string(anonMixed), "99") {
		t.Errorf("AnonymizeJSON(mixed prompt): integer elements missing: %s", anonMixed)
	}
	if !f2.Touched() {
		t.Error("Touched() = false after PII in mixed prompt string element, want true")
	}
}

func TestFilter_AnonymizeJSON_UserField(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}],"user":"alice@example.com"}`)
	anonBody, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if strings.Contains(string(anonBody), "alice@example.com") {
		t.Error("anonymized body still contains original email in user field")
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in user field, want true")
	}
}

func TestFilter_AnonymizeJSON_MessageName(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","name":"alice@example.com","content":"hello"}]}`)
	anonBody, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if strings.Contains(string(anonBody), "alice@example.com") {
		t.Error("anonymized body still contains original email in messages[].name")
	}
}

func TestFilter_AnonymizeJSON_ToolCallArguments(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"send_email","arguments":"{\"to\":\"alice@example.com\",\"body\":\"hello\"}"}}]}]}`)
	anonBody, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if strings.Contains(string(anonBody), "alice@example.com") {
		t.Error("anonymized body still contains original email in tool_calls[].function.arguments")
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in tool_calls arguments, want true")
	}
}

func TestFilter_AnonymizeJSON_ToolDescription(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"contact","description":"Send email to alice@example.com"}}]}`)
	anonBody, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if strings.Contains(string(anonBody), "alice@example.com") {
		t.Error("anonymized body still contains original email in tools[].function.description")
	}
	if !f.Touched() {
		t.Error("Touched() = false after PII in tool description, want true")
	}
}

func TestFilter_Restore_UnknownPseudonymUnchanged(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	// Call AnonymizeJSON on PII-free content so rev map remains empty.
	_ = mustAnonymize(t, f, []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`))

	// A pseudonym-shaped string that was NOT produced by this filter.
	foreign := []byte(`response containing PII_EM_aabbccddeeff001122334455 which is unknown`)
	result := f.Restore(foreign)

	if string(result) != string(foreign) {
		t.Errorf("Restore() modified an unknown pseudonym: got %q, want %q", result, foreign)
	}
}

func TestFilter_Restore_NoopWhenNotTouched(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	// No AnonymizeJSON call at all — rev map is empty.
	text := []byte("some response with no pseudonyms")
	result := f.Restore(text)

	// When rev map is empty, Restore returns the original slice without allocation.
	if string(result) != string(text) {
		t.Errorf("Restore() changed content unexpectedly: %q", result)
	}
}

func TestFilter_Restore_CachesReplacer(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"email: x@example.com"}]}`)
	anonBody := mustAnonymize(t, f, body)
	if !f.Touched() {
		t.Skip("no PII detected, cannot test replacer caching")
	}

	// Call Restore twice with the same filter: second call should reuse the
	// cached replacer and produce the same output.
	first := f.Restore(anonBody)
	second := f.Restore(anonBody)
	if string(first) != string(second) {
		t.Errorf("Restore() produced different results on repeated calls: %q vs %q", first, second)
	}
}

func TestFilter_MappingCapExceeded(t *testing.T) {
	t.Parallel()

	eng := newTestEngine(t)
	f := eng.NewFilter(testOrgID)

	// Fill the mapping table to exactly maxPIIMappings entries by calling
	// pseudonymFor directly with unique synthetic values. Using fmt.Sprintf
	// would pull in an extra import; instead we build unique keys via the
	// lookup key format the filter itself uses (type+NUL+value). We bypass
	// the detector here because we are testing the cap logic in pseudonymFor,
	// not the detection path.
	for i := 0; i < maxPIIMappings; i++ {
		// Generate a unique value string that looks like "value-0000000001",
		// "value-0000000002", etc. We zero-pad so that short integers do not
		// accidentally alias each other through normalization.
		val := "value-" + strings.Repeat("0", 10-len(itoa(i))) + itoa(i)
		_, err := f.pseudonymFor("EMAIL", val)
		if err != nil {
			t.Fatalf("unexpected error at mapping %d (before cap of %d): %v", i, maxPIIMappings, err)
		}
	}
	// The next new unique value must exceed the cap.
	_, err := f.pseudonymFor("EMAIL", "cap-exceeded-sentinel")
	if err == nil {
		t.Error("pseudonymFor returned nil error when mapping cap was exceeded; expected fail-closed error")
	}
	// Error message must never contain the PII value.
	if strings.Contains(err.Error(), "cap-exceeded-sentinel") {
		t.Errorf("error message leaks PII value: %q", err.Error())
	}
}

// itoa is a minimal int-to-string helper for test use only (avoids importing strconv).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := make([]byte, 0, 10)
	for n > 0 {
		digits = append(digits, byte('0'+n%10))
		n /= 10
	}
	// Reverse.
	for i, j := 0, len(digits)-1; i < j; i, j = i+1, j-1 {
		digits[i], digits[j] = digits[j], digits[i]
	}
	return string(digits)
}

// ── Engine / NewFilter ────────────────────────────────────────────────────────

func TestEngine_NewFilterIsolation(t *testing.T) {
	t.Parallel()

	eng := newTestEngine(t)

	f1 := eng.NewFilter(testOrgID)
	f2 := eng.NewFilter(testOrgID)

	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"email: x@example.com"}]}`)
	_ = mustAnonymize(t, f1, body)

	// f2 must have its own clean state — Touched must be false.
	if f2.Touched() {
		t.Error("f2.Touched() = true after f1 anonymized; filters must be independent")
	}
}

func TestEngine_SecretIsCopied(t *testing.T) {
	t.Parallel()

	secret := []byte("mutable-secret-32-bytes-00000000")

	// Build a detector so the engine can actually find PII.
	d, err := NewRegexDetector(DefaultPatterns())
	if err != nil {
		t.Fatalf("NewRegexDetector: %v", err)
	}
	eng := NewEngine(secret, []Detector{d})

	// Compute the expected pseudonym BEFORE mutating the secret slice.
	origSecret := []byte("mutable-secret-32-bytes-00000000")
	want := pseudonym(origSecret, testOrgID, "EMAIL", "test@example.com")

	// Mutate the caller's slice — the engine must have taken a copy.
	secret[0] = 'X'

	f := eng.NewFilter(testOrgID)
	// Trigger pseudonymFor via AnonymizeJSON on a body with that email.
	anonBody := mustAnonymize(t, f, []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"test@example.com"}]}`))

	re := regexp.MustCompile(`PII_EM_[0-9a-f]{24}`)
	got := re.FindString(string(anonBody))
	if got != want {
		t.Errorf("pseudonym after secret mutation = %q, want %q (engine should hold a copy of secret)", got, want)
	}
}

func TestEngine_Close_ZerosSecret(t *testing.T) {
	t.Parallel()

	d, err := NewRegexDetector(DefaultPatterns())
	if err != nil {
		t.Fatalf("NewRegexDetector: %v", err)
	}
	eng := NewEngine(testSecret, []Detector{d})

	// Compute a pseudonym before Close.
	before := pseudonym(eng.secret, testOrgID, "EMAIL", "test@example.com")

	eng.Close()

	// After Close the internal secret slice must be all zeros.
	for i, b := range eng.secret {
		if b != 0 {
			t.Errorf("secret[%d] = %d after Close, want 0", i, b)
		}
	}

	// A filter created after Close will derive pseudonyms from an all-zero
	// key, which will differ from pre-Close pseudonyms.
	after := pseudonym(eng.secret, testOrgID, "EMAIL", "test@example.com")
	if before == after && len(testSecret) > 0 {
		t.Error("pseudonym after Close equals pre-Close pseudonym; secret was not zeroed")
	}
}

// ── typeAbbrev ────────────────────────────────────────────────────────────────

func TestTypeAbbrev(t *testing.T) {
	t.Parallel()

	tests := []struct {
		typ  string
		want string
	}{
		{"EMAIL", "EM"},
		{"IBAN", "IB"},
		{"PHONE", "PH"},
		{"CREDIT_CARD", "CC"},
		{"TAX_ID", "TX"},
		{"UNKNOWN_TYPE", "XX"},
		{"", "XX"},
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

// ── replaceSpansInText / deOverlap (package-level helpers) ───────────────────

func TestReplaceSpansInText_Empty(t *testing.T) {
	t.Parallel()

	text, touched, err := replaceSpansInText("hello world", nil, func(_, v string) (string, error) { return v, nil })
	if err != nil {
		t.Fatalf("replaceSpansInText with nil spans: unexpected error: %v", err)
	}
	if touched {
		t.Error("replaceSpansInText with nil spans: touched = true, want false")
	}
	if text != "hello world" {
		t.Errorf("replaceSpansInText with nil spans: text = %q, want %q", text, "hello world")
	}
}

func TestReplaceSpansInText_Substitution(t *testing.T) {
	t.Parallel()

	text := "call 123 or 456"
	spans := []Span{
		{Start: 5, End: 8, Type: "NUM"},
		{Start: 12, End: 15, Type: "NUM"},
	}
	replace := func(_, v string) (string, error) { return "[" + v + "]", nil }

	got, touched, err := replaceSpansInText(text, spans, replace)
	if err != nil {
		t.Fatalf("replaceSpansInText: unexpected error: %v", err)
	}
	if !touched {
		t.Error("touched = false, want true")
	}
	want := "call [123] or [456]"
	if got != want {
		t.Errorf("replaceSpansInText = %q, want %q", got, want)
	}
}

func TestDeOverlap(t *testing.T) {
	t.Parallel()

	spans := []Span{
		{Start: 0, End: 10, Type: "A"},
		{Start: 5, End: 15, Type: "B"},  // overlaps A
		{Start: 15, End: 20, Type: "C"}, // non-overlapping
	}
	result := deOverlap(spans)
	if len(result) != 2 {
		t.Fatalf("deOverlap: got %d spans, want 2: %+v", len(result), result)
	}
	if result[0] != spans[0] {
		t.Errorf("result[0] = %+v, want %+v", result[0], spans[0])
	}
	if result[1] != spans[2] {
		t.Errorf("result[1] = %+v, want %+v", result[1], spans[2])
	}
}
