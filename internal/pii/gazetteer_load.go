package pii

import (
	"fmt"

	"github.com/voidmind-io/voidllm/internal/config"
)

// maxGazetteerInlineTerms is the maximum total number of term values that may
// be provided across all inline config entries in a single LoadGazetteerDetector
// call. Exceeding this cap causes a startup error (fail-closed) so that a
// misconfigured inline section cannot exhaust memory before the trie state cap
// fires.
const maxGazetteerInlineTerms = 100_000

// LoadGazetteerDetector constructs a GazetteerDetector from the three sources
// described in the config: embedded packs, operator directories, and inline
// terms. All sources are merged into a single Aho-Corasick automaton.
//
// Source priority: all sources contribute equally; deduplication is applied
// across all of them by (normalized-term, type) pair inside
// NewGazetteerDetector.
//
// Global load limits (FIX 4): the file count and byte volume caps are enforced
// across ALL operator directories in a single call, not per-directory. The
// inline term count is also bounded globally.
//
// Returns an error on any of:
//   - unknown embedded pack name
//   - nonexistent or unreadable directory
//   - empty or missing type in pack files or inline terms
//   - too many files or bytes across all operator directories
//   - too many inline terms
//   - too many trie states (exceeds maxGazetteerStates)
//   - too many output-link entries (exceeds maxGazetteerOutputs)
func LoadGazetteerDetector(cfg config.PIIGazetteerConfig) (*GazetteerDetector, error) {
	var gazes []Gazetteer

	// 1. Embedded packs selected by name.
	for _, packName := range cfg.Packs {
		g, err := loadEmbeddedPack(packName)
		if err != nil {
			return nil, fmt.Errorf("pii: gazetteer: load pack %q: %w", packName, err)
		}
		gazes = append(gazes, g)
	}

	// 2. Operator directories — global counters threaded across all dirs.
	var (
		globalFileCount  int
		globalTotalBytes int64
	)
	for _, dir := range cfg.Dirs {
		if err := loadGazetteerDir(dir, &gazes, &globalFileCount, &globalTotalBytes); err != nil {
			return nil, fmt.Errorf("pii: gazetteer: load dir %q: %w", dir, err)
		}
	}

	// 3. Inline config terms — count total values across all entries.
	inlineTermCount := 0
	for i, entry := range cfg.Terms {
		if entry.Type == "" {
			return nil, fmt.Errorf("pii: gazetteer: settings.pii.gazetteer.terms[%d].type: must not be empty", i)
		}
		if len(entry.Values) == 0 {
			continue
		}
		inlineTermCount += len(entry.Values)
		if inlineTermCount > maxGazetteerInlineTerms {
			return nil, fmt.Errorf("pii: gazetteer: inline terms exceed %d entry limit (reduce settings.pii.gazetteer.terms)", maxGazetteerInlineTerms)
		}
		gazes = append(gazes, Gazetteer{
			Name:  fmt.Sprintf("inline[%d]", i),
			Type:  entry.Type,
			Terms: entry.Values,
		})
	}

	opts := GazetteerOptions{
		CaseInsensitive: cfg.Options.IsCaseInsensitive(),
	}

	return NewGazetteerDetector(gazes, opts)
}
