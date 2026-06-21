package pii

import (
	"fmt"
)

// maxGazetteerStates is the maximum number of trie states the Aho-Corasick
// automaton may allocate. Each state corresponds to one rune in the trie.
// At ~40 bytes per state (IDs + map overhead) this caps memory at roughly
// 20 MB before terms are even matched. Exceeding the cap causes the
// constructor to return an error rather than silently exhausting heap.
const maxGazetteerStates = 500_000

// maxGazetteerOutputs is the maximum total number of output-link entries
// summed across all trie states. Without this bound, BFS output-link
// flattening for prefix-heavy dictionaries (e.g. "a", "aa", "aaa", …)
// copies failure outputs into every descendant, growing quadratically in
// the number of states. Exceeding this cap causes the constructor to return
// an error (fail-closed at startup) rather than exhausting heap.
const maxGazetteerOutputs = 1_000_000

// acState is a single node in the Aho-Corasick trie.
type acState struct {
	// children maps a rune to the next state ID on that edge.
	children map[rune]int32
	// fail is the failure-link state ID (BFS-computed).
	fail int32
	// output holds the term payloads that match at this state (via goto or
	// output links). Each element is an index into acMatcher.terms.
	output []int32
}

// acTerm pairs a normalized term with the PII type it represents.
type acTerm struct {
	norm    string // case-folded (or original if case-sensitive)
	typ     string
	runeLen int // precomputed len([]rune(norm)); set once in newACMatcher
}

// acMatcher is a compiled, immutable Aho-Corasick automaton over runes.
// It is built once in newACMatcher and reused concurrently across Find calls.
type acMatcher struct {
	states []acState
	terms  []acTerm
}

// newACMatcher builds an Aho-Corasick automaton from the given (normalized
// term, type) pairs. It returns an error if the number of trie states would
// exceed maxGazetteerStates, or if the total number of output-link entries
// across all states would exceed maxGazetteerOutputs.
//
// The output-link bound prevents quadratic memory growth for prefix-heavy
// dictionaries (e.g. "a", "aa", "aaa", …) where BFS output-link flattening
// copies failure outputs into every descendant, potentially producing
// O(states^2) total entries.
//
// The caller is responsible for deduplication; duplicate terms produce
// redundant but harmless output entries.
func newACMatcher(terms []acTerm) (*acMatcher, error) {
	if len(terms) == 0 {
		return &acMatcher{states: []acState{{}}}, nil
	}

	m := &acMatcher{
		states: make([]acState, 1, min(len(terms)*4, maxGazetteerStates)),
		terms:  terms,
	}
	// State 0 is the root.
	m.states[0] = acState{children: make(map[rune]int32)}

	// Phase 1: insert all terms into the trie.
	totalOutputs := 0
	for termIdx, t := range terms {
		// Precompute rune length once; avoids repeated allocation in match().
		m.terms[termIdx].runeLen = len([]rune(t.norm))
		cur := int32(0)
		for _, r := range t.norm {
			if _, ok := m.states[cur].children[r]; !ok {
				if len(m.states) >= maxGazetteerStates {
					return nil, fmt.Errorf("pii: gazetteer: trie exceeds %d states (reduce term count)", maxGazetteerStates)
				}
				newID := int32(len(m.states))
				m.states = append(m.states, acState{children: make(map[rune]int32)})
				m.states[cur].children[r] = newID
			}
			cur = m.states[cur].children[r]
		}
		// Mark this state as an output for termIdx.
		m.states[cur].output = append(m.states[cur].output, int32(termIdx))
		totalOutputs++
	}

	// Phase 2: compute failure links and output links via BFS.
	// Failure links use a FIFO queue of state IDs.
	queue := make([]int32, 0, len(m.states))

	// Seed: depth-1 nodes fail to root.
	for _, childID := range m.states[0].children {
		m.states[childID].fail = 0
		queue = append(queue, childID)
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]

		for r, childID := range m.states[cur].children {
			// Compute failure link for childID.
			f := m.states[cur].fail
			for f != 0 {
				if _, ok := m.states[f].children[r]; ok {
					break
				}
				f = m.states[f].fail
			}
			if next, ok := m.states[f].children[r]; ok && next != childID {
				m.states[childID].fail = next
			} else {
				m.states[childID].fail = 0
			}

			// Merge output links: childID inherits the outputs of its fail state.
			// Count the new entries before merging to enforce the output cap.
			failOutputs := m.states[m.states[childID].fail].output
			if len(failOutputs) > 0 {
				added := len(failOutputs)
				totalOutputs += added
				if totalOutputs > maxGazetteerOutputs {
					return nil, fmt.Errorf("pii: gazetteer: output links exceed %d entries (reduce term count or remove prefix dictionaries)", maxGazetteerOutputs)
				}
				merged := make([]int32, len(m.states[childID].output)+len(failOutputs))
				copy(merged, m.states[childID].output)
				copy(merged[len(m.states[childID].output):], failOutputs)
				m.states[childID].output = merged
			}

			queue = append(queue, childID)
		}
	}

	return m, nil
}

// acMatch is a single occurrence found by the automaton scan.
type acMatch struct {
	startRune int // inclusive start rune index in the scanned text
	endRune   int // exclusive end rune index
	termIdx   int // index into acMatcher.terms
}

// match scans text (as a rune slice) using the automaton and returns every
// occurrence of every dictionary term. Overlapping occurrences are all
// reported; the caller resolves overlaps.
func (m *acMatcher) match(runes []rune) []acMatch {
	if len(m.states) == 0 || len(runes) == 0 {
		return nil
	}

	var results []acMatch
	cur := int32(0)

	for pos, r := range runes {
		// Follow failure links until we find a valid goto or reach root.
		for cur != 0 {
			if _, ok := m.states[cur].children[r]; ok {
				break
			}
			cur = m.states[cur].fail
		}
		if next, ok := m.states[cur].children[r]; ok {
			cur = next
		}
		// cur now holds the state after consuming runes[pos].

		// Collect all outputs at this state (term matches ending at pos+1).
		for _, tIdx := range m.states[cur].output {
			term := m.terms[tIdx]
			startRune := pos + 1 - term.runeLen
			results = append(results, acMatch{
				startRune: startRune,
				endRune:   pos + 1,
				termIdx:   int(tIdx),
			})
		}
	}

	return results
}
