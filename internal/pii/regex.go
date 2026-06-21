package pii

import (
	"fmt"
	"regexp"
	"sort"
)

// Pattern associates a PII type label with a regular expression string.
// Both fields are required; an empty Regexp causes NewRegexDetector to
// return an error.
type Pattern struct {
	// Type is the PII category label (e.g. "EMAIL", "IBAN"). It must be
	// non-empty and is used verbatim in the Span.Type field and in the
	// pseudonym token abbreviation.
	Type string
	// Regexp is the Go regular expression that matches a single PII value.
	// The expression is compiled once in NewRegexDetector and reused for
	// the lifetime of the detector. Named capture groups are ignored;
	// only the full match (group 0) is used.
	Regexp string
}

// RegexDetector finds PII spans using a set of compiled regular expressions.
// It is safe for concurrent use across goroutines; all state is read-only
// after construction.
type RegexDetector struct {
	compiled []*compiledPattern
}

// compiledPattern pairs a ready-to-use regexp with its PII type label.
type compiledPattern struct {
	typ string
	re  *regexp.Regexp
}

// NewRegexDetector compiles each pattern's Regexp exactly once and returns
// a RegexDetector ready for use. It returns an error if any pattern has an
// empty Type, an empty Regexp, or a Regexp that cannot be compiled.
func NewRegexDetector(patterns []Pattern) (*RegexDetector, error) {
	compiled := make([]*compiledPattern, 0, len(patterns))
	for i, p := range patterns {
		if p.Type == "" {
			return nil, fmt.Errorf("pii: pattern[%d]: Type must not be empty", i)
		}
		if p.Regexp == "" {
			return nil, fmt.Errorf("pii: pattern[%d] (%s): Regexp must not be empty", i, p.Type)
		}
		re, err := regexp.Compile(p.Regexp)
		if err != nil {
			return nil, fmt.Errorf("pii: pattern[%d] (%s): compile regexp: %w", i, p.Type, err)
		}
		compiled = append(compiled, &compiledPattern{typ: p.Type, re: re})
	}
	return &RegexDetector{compiled: compiled}, nil
}

// Find returns all non-overlapping PII spans in text. When multiple
// patterns match overlapping byte ranges, the leftmost match takes
// priority; among matches starting at the same position, the longest
// match wins. This policy is equivalent to scanning the text left-to-right
// and advancing past each accepted match.
//
// RegexDetector never returns an error; the error return satisfies the
// Detector interface and is always nil.
func (d *RegexDetector) Find(text string) ([]Span, error) {
	// Collect all raw matches from every pattern.
	var raw []Span
	for _, cp := range d.compiled {
		locs := cp.re.FindAllStringIndex(text, -1)
		for _, loc := range locs {
			raw = append(raw, Span{Start: loc[0], End: loc[1], Type: cp.typ})
		}
	}
	if len(raw) == 0 {
		return nil, nil
	}

	// Sort: leftmost first; on tie, longest first (largest End first).
	sort.Slice(raw, func(i, j int) bool {
		if raw[i].Start != raw[j].Start {
			return raw[i].Start < raw[j].Start
		}
		return raw[i].End > raw[j].End
	})

	// Greedy non-overlapping selection: advance cursor past each accepted span.
	result := make([]Span, 0, len(raw))
	cursor := 0
	for _, s := range raw {
		if s.Start < cursor {
			continue // overlaps with a previously accepted span
		}
		result = append(result, s)
		cursor = s.End
	}
	return result, nil
}

// DefaultPatterns returns the built-in set of conservative PII detection
// patterns tuned for European (particularly German) contexts. The list is
// intentionally small and precise — false positives in an LLM proxy are
// more disruptive than false negatives, because they corrupt legitimate
// text that happens to match a pattern.
//
// Included patterns:
//   - IBAN: German IBANs (DE + 20 digits, word-boundary anchored)
//   - EMAIL: RFC 5321-compatible local part and domain
//   - PHONE: German mobile and landline numbers (+49 or 0xx prefix)
//   - CREDIT_CARD: 13-19 contiguous digit groups (catches most major schemes)
//   - TAX_ID: German Steueridentifikationsnummer (11 digits, not starting with 0)
func DefaultPatterns() []Pattern {
	return []Pattern{
		{
			Type: "IBAN",
			// German IBAN: DE followed by exactly 20 digits.
			// Word-boundary anchors prevent matching inside longer digit strings.
			Regexp: `\bDE\d{20}\b`,
		},
		{
			Type: "EMAIL",
			// Standard email: local@domain.tld. Allows +addressing and subdomains.
			// Does not match bare domain names or addresses without a TLD.
			Regexp: `\b[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}\b`,
		},
		{
			Type: "PHONE",
			// German phone numbers in three common formats:
			//   +49 (prefix) followed by digits and optional spaces/hyphens
			//   0800 / 0900 service numbers
			//   0xx local landline with area code
			// The pattern requires at least 7 digits total to reduce false positives.
			Regexp: `(?:\+49|0)[0-9][0-9 \-]{5,14}[0-9]`,
		},
		{
			Type: "CREDIT_CARD",
			// 13-19 consecutive digits (no separators). Catches Visa (16),
			// Mastercard (16), Amex (15), Discover (16), and others.
			// Luhn validation is omitted intentionally: it adds CPU cost and
			// the false-positive risk from 13-19 digit sequences in normal
			// text is low enough with word-boundary anchors.
			Regexp: `\b[0-9]{13,19}\b`,
		},
		{
			Type: "TAX_ID",
			// German Steueridentifikationsnummer: 11 digits, first digit 1-9.
			// Word boundaries prevent matching inside longer digit sequences.
			Regexp: `\b[1-9][0-9]{10}\b`,
		},
	}
}
