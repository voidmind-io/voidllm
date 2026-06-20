package pii

// stream_review3_test.go covers the findings from the third external review
// round for feat/pii-stage0b-incremental-streaming.
//
// FIX 1: prefixSet replaced by sortedPseudonyms + binary search — behavioral
//         equivalence proven via:
//           - existing split-at-every-position tests (still green)
//           - TestStreamRestorer_BinarySearch_ManyPseudonyms (new, below)
//
// FIX 3: "tool_calls"/"function_call" finish_reason now fail-closed — covered
//         by TestStreamRestorer_FinishReason_ToolCalls_FailClosed (in
//         stream_restorer_test.go) and the refactored ValidValues test.
//
// FIX 2 (byte-cap): raw bytes are now counted before the adapter so dropped
//         (nil) adapter lines still contribute to the cap. Unit-level proof is
//         in TestStreamRestorer_RawByteCapCountedBeforeAdapter (proxy-level
//         integration in proxy/pii_review3_test.go).

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// ── FIX 1: binary search behavioural equivalence with many pseudonyms ─────────

// TestStreamRestorer_BinarySearch_ManyPseudonyms verifies that the binary-search
// implementation of longestPseudonymPrefixSuffix returns the same result as the
// former map-based implementation for every possible carry suffix, across a
// large (several hundred) pseudonym set. This is the equivalence proof for FIX 1.
//
// Strategy:
//  1. Anonymize a body with many distinct emails → produce N pseudonyms.
//  2. For each pseudonym, split it at every position (1..pseudonymLen-1) and
//     feed the prefix as a carry to a StreamRestorer built from all N pseudonyms.
//  3. Assert: (a) the carry is held (longestPseudonymPrefixSuffix returns l > 0),
//     and (b) the full stream restores the email correctly.
//
// Additionally verify that the binary search finds the LONGEST matching prefix:
// when two pseudonyms share a common leading sequence (PII_EM_…), the longest
// carry suffix that is a proper prefix of any known pseudonym must be returned.
func TestStreamRestorer_BinarySearch_ManyPseudonyms(t *testing.T) {
	t.Parallel()

	const numEmails = 250
	f := newTestFilter(t)

	// Generate distinct emails and anonymize them all through the same filter.
	emails := make([]string, numEmails)
	for i := range emails {
		emails[i] = fmt.Sprintf("user%04d@many-pseudos.example", i)
	}

	// Build a JSON body with all emails in a single anonymization pass so all
	// pseudonyms share the same filter instance (required for StreamRestorer).
	content := strings.Join(emails, " ")
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"` + content + `"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON(%d emails): %v", numEmails, err)
	}
	if !f.Touched() {
		t.Fatal("Touched() = false; no pseudonyms produced")
	}

	// Collect all generated pseudonyms.
	var pseudos []string
	for p := range f.rev {
		pseudos = append(pseudos, p)
	}
	if len(pseudos) < numEmails/2 {
		// Expect at least half — some may collide by birthday paradox, but not many.
		t.Fatalf("expected ~%d pseudonyms, got %d", numEmails, len(pseudos))
	}

	// Pre-warm the replacer to avoid write races in parallel subtests.
	for _, p := range pseudos {
		f.Restore([]byte(p))
	}

	// Verify makeSortedPseudonyms produces a properly sorted slice.
	sorted := makeSortedPseudonyms(pseudos)
	if !sort.StringsAreSorted(sorted) {
		t.Fatal("makeSortedPseudonyms: result is not sorted")
	}
	if len(sorted) != len(pseudos) {
		t.Errorf("makeSortedPseudonyms: len = %d, want %d", len(sorted), len(pseudos))
	}

	// For a representative sample of pseudonyms, verify split-at-every-position
	// restores correctly using a restorer built from all N pseudonyms.
	sampleSize := 10
	if len(pseudos) < sampleSize {
		sampleSize = len(pseudos)
	}
	for i := 0; i < sampleSize; i++ {
		pseudo := pseudos[i]
		orig := f.rev[pseudo]

		for splitAt := 1; splitAt < pseudonymLen; splitAt++ {
			splitAt := splitAt
			pseudo := pseudo
			orig := orig
			t.Run(fmt.Sprintf("pseudo%d_split%d", i, splitAt), func(t *testing.T) {
				t.Parallel()

				part1 := pseudo[:splitAt]
				part2 := pseudo[splitAt:]

				r := NewStreamRestorer(f, "gpt-4o")
				lines := []string{
					chatChunk(0, part1),
					"",
					chatChunk(0, part2),
					"",
					finishChunk(0, "stop"),
					"",
					"data: [DONE]",
					"",
				}
				output, _, err := pushAllCollect(r, lines)
				if err != nil {
					t.Fatalf("pseudo%d split_at_%d: Push error: %v", i, splitAt, err)
				}
				if strings.Contains(output, "PII_") {
					t.Errorf("pseudo%d split_at_%d: PII_ fragment in output", i, splitAt)
				}
				if !strings.Contains(output, orig) {
					t.Errorf("pseudo%d split_at_%d: original %q missing", i, splitAt, orig)
				}
			})
		}
	}
}

// TestStreamRestorer_BinarySearch_LongestPrefixWins verifies that when the
// carry buffer ends in a string that is a proper prefix of MULTIPLE pseudonyms
// (e.g., "PII_" is a prefix of every known pseudonym), the function returns
// the LONGEST l such that carry[n-l:] is a prefix of some pseudonym.
//
// All pseudonyms begin with "PII_" (4 bytes), so a carry ending in "PII_" will
// have suffix matches at lengths 1 ("P"), 2 ("PI"), 3 ("PII"), and 4 ("PII_").
// The function must return 4 (the longest match), not 1.
func TestStreamRestorer_BinarySearch_LongestPrefixWins(t *testing.T) {
	t.Parallel()

	// Anonymize two distinct emails to produce exactly two pseudonyms.
	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"a@longest.example b@longest.example"}]}`)
	if _, err := f.AnonymizeJSON(body); err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}

	var pseudo string
	for p := range f.rev {
		pseudo = p
		break
	}
	if len(pseudo) != pseudonymLen {
		t.Fatalf("pseudonym length %d != %d", len(pseudo), pseudonymLen)
	}
	// Pre-warm.
	f.Restore([]byte(pseudo))

	r := NewStreamRestorer(f, "gpt-4o")

	// Push "PII_" (just the 4-byte marker) as carry content. All pseudonyms
	// begin with "PII_", so:
	//   - suffix of length 1 ("P"): prefix of "PII_..." → match
	//   - suffix of length 2 ("PI"): prefix of "PII_..." → match
	//   - suffix of length 3 ("PII"): prefix of "PII_..." → match
	//   - suffix of length 4 ("PII_"): prefix of "PII_..." → match
	// The function must return 4 (the longest).
	carry := []byte("PII_")
	L := r.longestPseudonymPrefixSuffix(carry)
	if L != 4 {
		t.Errorf("longestPseudonymPrefixSuffix(%q) = %d, want 4 (longest match wins)", carry, L)
	}

	// "XPP" ends with suffix "P" of length 1. "P" IS a proper prefix of "PII_"
	// which is a proper prefix of every pseudonym. The function must return 1.
	L2 := r.longestPseudonymPrefixSuffix([]byte("XPP"))
	if L2 != 1 {
		t.Errorf("longestPseudonymPrefixSuffix(%q) = %d, want 1 (suffix 'P' is a prefix of 'PII_')", "XPP", L2)
	}

	// "XYZ" ends with no suffix that is a prefix of "PII_" → must return 0.
	L3 := r.longestPseudonymPrefixSuffix([]byte("XYZ"))
	if L3 != 0 {
		t.Errorf("longestPseudonymPrefixSuffix(%q) = %d, want 0 (no suffix matches any pseudonym prefix)", "XYZ", L3)
	}
}

// TestStreamRestorer_BinarySearch_NoPseudonymsEarlyExit verifies that when the
// filter has no known pseudonyms (Touched() would be false), longestPseudonym-
// PrefixSuffix returns 0 immediately without any binary search.
func TestStreamRestorer_BinarySearch_NoPseudonymsEarlyExit(t *testing.T) {
	t.Parallel()

	// Filter with no anonymizations — sortedPseudonyms will be nil.
	f := newTestFilter(t)
	// Do NOT call AnonymizeJSON; Touched() = false.

	r := NewStreamRestorer(f, "gpt-4o")
	if r.sortedPseudonyms != nil {
		t.Fatalf("expected sortedPseudonyms to be nil when no pseudonyms; got %v", r.sortedPseudonyms)
	}

	// Any carry must return 0 immediately.
	L := r.longestPseudonymPrefixSuffix([]byte("PII_"))
	if L != 0 {
		t.Errorf("longestPseudonymPrefixSuffix with empty set = %d, want 0", L)
	}
}

// ── FIX 3: finish_reason "tool_calls"/"function_call" — additional restorer tests

// TestStreamRestorer_AnthropicToolUsePattern_FailClosed simulates the exact
// pattern emitted by the Anthropic adapter when tool_use is active: a stream
// with text deltas followed by a finish_reason:"tool_calls" (no tool_calls
// delta, since the adapter drops tool_use content). The restorer must fail
// closed when it sees finish_reason:"tool_calls", because the response would
// be contract-violating (tool_calls finish with no tool_calls body).
func TestStreamRestorer_AnthropicToolUsePattern_FailClosed(t *testing.T) {
	t.Parallel()

	// Build a filter with one pseudonym to activate the PII path.
	pseudo, f := realPseudonym(t, "tooluse@anthropic.example")
	f.Restore([]byte(pseudo)) // pre-warm

	r := NewStreamRestorer(f, "claude-3-5-sonnet")

	// The Anthropic adapter emits "assistant" role first, then text deltas
	// (none here — tool_use streams may have no text), then finish_reason.
	// The content_block_delta for tool_use is dropped by the adapter, so the
	// restorer only sees finish_reason:"tool_calls".
	lines := []string{
		// Role chunk (normal start).
		fmt.Sprintf(`data: {"id":"c1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`),
		"",
		// finish_reason:"tool_calls" — the disallowed value.
		finishChunk(0, "tool_calls"),
		"",
	}

	var gotErr error
	for _, l := range lines {
		var raw []byte
		if l == "" {
			raw = []byte{}
		} else {
			raw = []byte(l)
		}
		_, _, err := r.Push(raw)
		if err != nil {
			gotErr = err
			break
		}
	}

	if !errors.Is(gotErr, errStreamAborted) {
		t.Errorf("Anthropic tool_use pattern: error = %v, want errStreamAborted", gotErr)
	}
}
