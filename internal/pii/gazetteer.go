package pii

import (
	"bufio"
	"embed"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

//go:embed gazetteer_packs/*.txt
var embeddedPacks embed.FS

// maxGazetteerMatches is the maximum number of raw automaton hits (before
// word-boundary filtering) allowed for a single Find call. Exceeding this
// cap causes Find to return an error so the request is rejected fail-closed
// rather than silently truncated. Truncation would leave PII unmasked;
// rejection is always safer. 200 000 is well above any realistic match
// density for legitimate input and low enough to bound per-request work.
const maxGazetteerMatches = 200_000

// Gazetteer describes a named collection of terms that identify a single PII
// type (e.g. city names, company suffixes). The Locale field is selection
// metadata only; matching itself is language-blind.
type Gazetteer struct {
	// Name is the unique identifier used to select this pack in config
	// (e.g. "company-forms", "de-cities").
	Name string
	// Locale is an informational BCP-47 locale tag (e.g. "de", "universal").
	// It has no effect on matching behaviour.
	Locale string
	// Type is the PII type label assigned to spans matched by this gazetteer
	// (e.g. "ORG", "CITY").
	Type string
	// Terms is the list of terms to match. Empty terms are ignored.
	Terms []string
}

// GazetteerOptions controls matching behaviour of a GazetteerDetector.
type GazetteerOptions struct {
	// CaseInsensitive folds the case of both dictionary terms and the scanned
	// text before matching. Original byte offsets are always preserved in the
	// returned Spans. Default: true.
	CaseInsensitive bool
}

// GazetteerDetector finds PII spans using a set of gazetteer term lists.
// It is built once and safe for concurrent Find calls; all state is
// read-only after construction.
//
// Case folding: when GazetteerOptions.CaseInsensitive is true, matching
// uses simple per-rune unicode.ToLower folding (see foldRunes). This does
// not perform Unicode full case folding, so variants such as "ß" vs "ss",
// Greek final sigma, and NFC/NFD-different accented forms are not matched.
// Operators should add spelling and case variants to their gazetteer lists
// for full coverage.
type GazetteerDetector struct {
	matcher *acMatcher
	opts    GazetteerOptions
}

// foldRunes lowercases s one rune at a time using unicode.ToLower, guaranteeing
// a strict 1:1 rune correspondence between the input and output strings.
// Both the dictionary term normalization and the scan-text folding in Find use
// this function so the two sides are always folded identically — a requirement
// for correct byte-offset alignment in the returned Spans.
//
// Documented limitation (FIX 6): foldRunes uses simple per-rune unicode.ToLower,
// not Unicode full case folding (golang.org/x/text/unicode/norm or cases.Fold).
// As a result, the following variants are NOT matched by CaseInsensitive mode:
//
//   - "ß" (U+00DF) vs "ss": unicode.ToLower('ß') == 'ß'; it does NOT expand to
//     "ss", so Straße and STRASSE are treated as distinct strings. Operators
//     must add both spelling variants to their gazetteer lists.
//   - Greek final sigma (ς / σ): treated as distinct runes by unicode.ToLower.
//   - NFC/NFD-different forms of accented characters (e.g. "é" as U+00E9 vs
//     "e" + U+0301): not normalized; operators should use a single canonical form.
//
// Full Unicode case folding or NFC normalization would require changing rune
// count or byte length, which would break the security-critical 1:1
// rune-index-to-byte-offset mapping. Accepting a slightly weaker matching model
// is the correct tradeoff for a privacy-critical anonymizer where incorrect
// byte offsets could silently mis-mask a different text region.
func foldRunes(s string) string {
	runes := []rune(s)
	for i, r := range runes {
		runes[i] = unicode.ToLower(r)
	}
	return string(runes)
}

// NewGazetteerDetector builds a GazetteerDetector from the given gazetteers.
// One Aho-Corasick automaton is compiled from all terms across all gazetteers;
// each term carries its gazetteer's Type.
//
// When opts.CaseInsensitive is true (the default), terms are folded to
// lowercase for the automaton dictionary, and the input text is also folded
// before scanning. Matches are then mapped back to original byte offsets in the
// returned Spans.
//
// Returns an error if the total number of automaton states would exceed
// maxGazetteerStates (see ahocorasick.go).
func NewGazetteerDetector(gazes []Gazetteer, opts GazetteerOptions) (*GazetteerDetector, error) {
	// Collect all (term, type) pairs, deduplicating identical (norm, type) pairs.
	type termKey struct {
		norm string
		typ  string
	}
	seen := make(map[termKey]struct{})
	var terms []acTerm

	for _, g := range gazes {
		for _, raw := range g.Terms {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			norm := raw
			if opts.CaseInsensitive {
				norm = foldRunes(raw)
			}
			k := termKey{norm: norm, typ: g.Type}
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			terms = append(terms, acTerm{norm: norm, typ: g.Type})
		}
	}

	matcher, err := newACMatcher(terms)
	if err != nil {
		return nil, err
	}

	return &GazetteerDetector{matcher: matcher, opts: opts}, nil
}

// Find returns all non-overlapping PII spans in text where a gazetteer term
// matches at a whole-token boundary. Whole-token filtering: a match at
// [start, end) is accepted only when the rune before start and the rune after
// end are NOT Unicode letters or digits (or the match is at the start/end of
// the string). This prevents partial-word matches such as "Berg" matching
// inside "Bergmann".
//
// When multiple spans overlap, the leftmost match wins; ties are broken by
// longest match (same policy as RegexDetector). The returned Spans use byte
// offsets into the original text, regardless of whether case folding was used.
//
// Find returns an error when the number of raw automaton hits before boundary
// filtering exceeds maxGazetteerMatches. The caller must treat this as a
// hard failure (fail-closed): an over-cap condition means the input is
// pathologically term-dense and cannot be safely processed — partial results
// would risk leaving PII unmasked. The request must be rejected rather than
// forwarded with incomplete anonymization.
func (d *GazetteerDetector) Find(text string) ([]Span, error) {
	if len(text) == 0 || len(d.matcher.states) == 0 {
		return nil, nil
	}

	// Build rune slice of the original text and a parallel slice for scanning.
	// When case-insensitive, the scan slice is lowercased via foldRunes — the
	// same function used for dictionary normalization — guaranteeing a strict
	// 1:1 rune correspondence between origRunes and scanRunes so that rune
	// indices computed from automaton output map correctly back to origRunes.
	//
	// Note: Find assumes valid UTF-8 input. The proxy guarantees this via
	// Sonic ConfigStd JSON parsing before detection. If the input contains
	// invalid UTF-8, []rune yields U+FFFD replacement characters whose byte
	// lengths differ from the original sequences, causing runeByteStart
	// offsets to be incorrect for the affected positions.
	origRunes := []rune(text)
	scanRunes := origRunes
	if d.opts.CaseInsensitive {
		scanRunes = []rune(foldRunes(text))
	}

	// Build a rune-index → byte-offset mapping so matches can be converted
	// back to original byte offsets. runeByteStart[i] is the byte offset of
	// origRunes[i] in text.
	runeByteStart := make([]int, len(origRunes)+1)
	byteOff := 0
	for i, r := range origRunes {
		runeByteStart[i] = byteOff
		n := utf8.RuneLen(r)
		if n < 0 {
			// origRunes comes from []rune(text) so all runes are valid;
			// treat the impossible case as 1 to avoid corrupting byteOff.
			n = 1
		}
		byteOff += n
	}
	runeByteStart[len(origRunes)] = len(text)

	// Run the automaton over the (possibly folded) rune slice, applying the
	// word-boundary filter inline to avoid materializing all raw hits before
	// filtering. The cap is checked against the raw hit count (before
	// boundary filtering) so that a dictionary attack via a term-dense input
	// is detected even when most hits are rejected by the boundary filter.
	var filtered []acMatch
	cur := int32(0)
	rawCount := 0
	states := d.matcher.states
	terms := d.matcher.terms

	for pos, r := range scanRunes {
		// Follow failure links until we find a valid goto or reach root.
		for cur != 0 {
			if _, ok := states[cur].children[r]; ok {
				break
			}
			cur = states[cur].fail
		}
		if next, ok := states[cur].children[r]; ok {
			cur = next
		}

		// Collect all outputs at this state (term matches ending at pos+1).
		for _, tIdx := range states[cur].output {
			rawCount++
			if rawCount > maxGazetteerMatches {
				return nil, fmt.Errorf("pii: gazetteer: match count exceeded %d for a single input (request rejected)", maxGazetteerMatches)
			}
			t := terms[tIdx]
			startRune := pos + 1 - t.runeLen
			// Apply word-boundary filter inline so only whole-token hits
			// are kept; this avoids a separate allocation for all raw hits.
			if isWordBoundary(origRunes, startRune, pos+1) {
				filtered = append(filtered, acMatch{
					startRune: startRune,
					endRune:   pos + 1,
					termIdx:   int(tIdx),
				})
			}
		}
	}

	if len(filtered) == 0 {
		return nil, nil
	}

	// Sort: leftmost first; on tie, longest first (largest endRune first).
	sort.Slice(filtered, func(i, j int) bool {
		a, b := filtered[i], filtered[j]
		if a.startRune != b.startRune {
			return a.startRune < b.startRune
		}
		return a.endRune > b.endRune
	})

	// Greedy non-overlapping selection, advancing cursor past each accepted match.
	result := make([]Span, 0, len(filtered))
	cursor := 0
	for _, m := range filtered {
		if m.startRune < cursor {
			continue
		}
		result = append(result, Span{
			Start: runeByteStart[m.startRune],
			End:   runeByteStart[m.endRune],
			Type:  d.matcher.terms[m.termIdx].typ,
		})
		cursor = m.endRune
	}
	return result, nil
}

// isWordBoundary reports whether a match at [startRune, endRune) in runes is
// at a whole-token boundary. The rune before startRune and the rune after
// endRune-1 must not be a Unicode letter or digit.
func isWordBoundary(runes []rune, startRune, endRune int) bool {
	if startRune > 0 {
		prev := runes[startRune-1]
		if unicode.IsLetter(prev) || unicode.IsDigit(prev) {
			return false
		}
	}
	if endRune < len(runes) {
		next := runes[endRune]
		if unicode.IsLetter(next) || unicode.IsDigit(next) {
			return false
		}
	}
	return true
}

// embeddedPackRegistry maps pack names to their embedded Gazetteer loaded from
// the embedded file system. Packs are loaded lazily by loadEmbeddedPack and
// cached here.
var embeddedPackRegistry = map[string]string{
	"company-forms": "gazetteer_packs/company-forms.txt",
	"de-cities":     "gazetteer_packs/de-cities.txt",
	"de-firstnames": "gazetteer_packs/de-firstnames.txt",
}

// loadEmbeddedPack loads a Gazetteer from an embedded pack file by name.
// Returns an error for unknown pack names.
func loadEmbeddedPack(name string) (Gazetteer, error) {
	path, ok := embeddedPackRegistry[name]
	if !ok {
		return Gazetteer{}, fmt.Errorf("pii: gazetteer: unknown embedded pack %q", name)
	}
	f, err := embeddedPacks.Open(path)
	if err != nil {
		return Gazetteer{}, fmt.Errorf("pii: gazetteer: open embedded pack %q: %w", name, err)
	}
	defer f.Close()
	return parseGazetteerFile(f, name)
}

// maxGazetteerFiles is the maximum total number of *.txt files read across
// all loadGazetteerDir calls in one LoadGazetteerDetector invocation.
// Exceeding this limit causes an error at startup so that a misconfigured
// set of directories (e.g. pointing at /usr/share) fails fast rather than
// reading thousands of files before the trie cap fires.
const maxGazetteerFiles = 256

// maxGazetteerBytes is the maximum total byte volume read across all files
// in all loadGazetteerDir calls in one LoadGazetteerDetector invocation
// (64 MiB). Together with maxGazetteerFiles this bounds the startup I/O
// surface for operator-supplied gazetteer directories.
const maxGazetteerBytes = 64 << 20 // 64 MiB

// loadGazetteerDir reads every *.txt regular file in dir and appends the
// resulting Gazetteers to *out. The shared counters *fileCount and
// *totalBytes are incremented as files are processed; both limits are
// enforced globally across all directories in a single loader run.
//
// Returns an error if dir does not exist, cannot be read, the cumulative
// number of *.txt files across all dirs exceeds maxGazetteerFiles, or the
// cumulative byte volume exceeds maxGazetteerBytes.
func loadGazetteerDir(dir string, out *[]Gazetteer, fileCount *int, totalBytes *int64) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("pii: gazetteer: dir %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("pii: gazetteer: %q is not a directory", dir)
	}

	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		// FIX 3: skip symlinks and any non-regular file (FIFOs, devices,
		// sockets). Symlinks are excluded explicitly to prevent an operator
		// from pointing a symlink into a sensitive path that bypasses the
		// byte-volume cap. FIFOs and device files would stall os.Open
		// indefinitely at startup.
		//
		// d.Type() returns the file-mode type bits. A regular file has no
		// type bits set (Type() == 0); a symlink has ModeSymlink set;
		// FIFOs, sockets, and devices have their own bits. We use
		// d.Type().IsRegular() (i.e. Type() == 0) as the affirmative check
		// so that any future special-file type is also skipped by default.
		if !d.Type().IsRegular() {
			return nil
		}

		if !strings.HasSuffix(d.Name(), ".txt") {
			return nil
		}

		*fileCount++
		if *fileCount > maxGazetteerFiles {
			return fmt.Errorf("pii: gazetteer: directory exceeds %d file limit", maxGazetteerFiles)
		}

		// Use d.Info() for the file size. Since we confirmed the entry is a
		// regular file above, Info() reflects the file itself, not a symlink
		// target, and will not block on a FIFO.
		fi, statErr := d.Info()
		if statErr != nil {
			return fmt.Errorf("pii: gazetteer: stat %q: %w", path, statErr)
		}
		*totalBytes += fi.Size()
		if *totalBytes > maxGazetteerBytes {
			return fmt.Errorf("pii: gazetteer: directory exceeds %d byte limit", maxGazetteerBytes)
		}

		f, openErr := os.Open(path)
		if openErr != nil {
			return fmt.Errorf("pii: gazetteer: open %q: %w", path, openErr)
		}
		defer f.Close()

		name := strings.TrimSuffix(d.Name(), ".txt")
		g, parseErr := parseGazetteerFile(f, name)
		if parseErr != nil {
			return fmt.Errorf("pii: gazetteer: parse %q: %w", path, parseErr)
		}
		*out = append(*out, g)
		return nil
	})
}

// parseGazetteerFile reads a gazetteer text file from r and returns a Gazetteer.
// The file format is:
//
//	# locale: <locale>   (optional comment header)
//	# type: <TYPE>       (required comment header; sets the PII type)
//	<term>               (one term per line)
//	# <comment>          (ignored)
//	                     (blank lines ignored)
//
// The name parameter sets Gazetteer.Name. The type parsed from a "# type:"
// header (uppercased) is required — a file with an empty or missing "# type:"
// header is considered malformed and returns an error. This is consistent with
// inline-term validation in LoadGazetteerDetector, which also rejects empty types.
func parseGazetteerFile(r io.Reader, name string) (Gazetteer, error) {
	g := Gazetteer{
		Name: name,
	}

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			// Parse metadata headers.
			rest := strings.TrimSpace(line[1:])
			if strings.HasPrefix(rest, "locale:") {
				g.Locale = strings.TrimSpace(rest[len("locale:"):])
			} else if strings.HasPrefix(rest, "type:") {
				g.Type = strings.TrimSpace(strings.ToUpper(rest[len("type:"):]))
			}
			continue
		}
		g.Terms = append(g.Terms, line)
	}
	if err := scanner.Err(); err != nil {
		return Gazetteer{}, fmt.Errorf("pii: gazetteer: scan: %w", err)
	}
	// FIX 5: reject files with an empty or missing # type: header. An empty
	// type would create anonymous spans whose pseudonyms collide across types
	// and whose meaning is undefined. Consistent with inline-term validation
	// which also rejects empty types.
	if g.Type == "" {
		return Gazetteer{}, fmt.Errorf("pii: gazetteer: file %q: missing or empty '# type:' header", name)
	}
	return g, nil
}
