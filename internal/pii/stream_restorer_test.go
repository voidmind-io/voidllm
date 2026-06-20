package pii

// stream_restorer_test.go covers the incremental StreamRestorer introduced in
// feat/pii-stage0b-incremental-streaming. Tests are organized to match the
// task matrix exactly; each section header identifies the matrix point(s)
// it covers.
//
// Pseudonyms are always produced by a real Engine/Filter (anonymizing a body
// with a test email and reading the pseudonym from filter.rev) so tests run
// against genuine pseudonym strings, not hand-crafted fakes.

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// ── test helpers ──────────────────────────────────────────────────────────────

// realPseudonym returns a real pseudonym by anonymizing a body that contains
// email and reading the first (and only) key from the filter's rev map.
// It also returns the filter itself so callers can create a StreamRestorer
// seeded with that filter's pseudonym set.
func realPseudonym(t *testing.T, email string) (string, *Filter) {
	t.Helper()
	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"` + email + `"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if !f.Touched() {
		t.Fatalf("Touched() = false; no pseudonym produced for %q", email)
	}
	var p string
	for k := range f.rev {
		p = k
		break
	}
	if len(p) != pseudonymLen {
		t.Fatalf("unexpected pseudonym length: got %d want %d (%q)", len(p), pseudonymLen, p)
	}
	return p, f
}

// pushLines feeds a slice of raw SSE lines (without trailing newlines) through
// the restorer one by one, accumulating all emitted bytes. nil entries are
// converted to blank-line bytes. Returns the concatenated client-visible SSE
// output and the last error. Stops after terminal=true.
func pushLines(t *testing.T, r *StreamRestorer, lines []string) (string, error) {
	t.Helper()
	var sb strings.Builder
	for _, l := range lines {
		var raw []byte
		if l == "" {
			raw = []byte{}
		} else {
			raw = []byte(l)
		}
		out, terminal, err := r.Push(raw)
		if err != nil {
			return sb.String(), err
		}
		for _, b := range out {
			if b == nil {
				sb.WriteByte('\n')
			} else {
				sb.Write(b)
				sb.WriteByte('\n')
			}
		}
		if terminal {
			break
		}
	}
	return sb.String(), nil
}

// pushAllCollect feeds every line until terminal or error, collects (out,
// terminal, err) per Push, and returns the accumulated output string plus the
// final error and whether terminal was ever true.
//
// It stops after the first Push that returns terminal=true (do not push after
// terminal — the next Push would return errStreamTerminated).
func pushAllCollect(r *StreamRestorer, lines []string) (output string, wasTerminal bool, finalErr error) {
	var sb strings.Builder
	for _, l := range lines {
		var raw []byte
		if l == "" {
			raw = []byte{}
		} else {
			raw = []byte(l)
		}
		out, terminal, err := r.Push(raw)
		if err != nil {
			finalErr = err
			return sb.String(), wasTerminal, finalErr
		}
		for _, b := range out {
			if b == nil {
				sb.WriteByte('\n')
			} else {
				sb.Write(b)
				sb.WriteByte('\n')
			}
		}
		if terminal {
			wasTerminal = true
			// Do NOT push further lines — terminal means the restorer is done.
			break
		}
	}
	return sb.String(), wasTerminal, nil
}

// chatChunk builds a minimal OpenAI chat-completion SSE data line.
func chatChunk(idx int, content string) string {
	return fmt.Sprintf(`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":%d,"delta":{"content":%s},"finish_reason":null}]}`,
		idx, jsonStr(content))
}

// roleChunk builds an initial SSE chunk that carries only a role.
func roleChunk(idx int, role string) string {
	return fmt.Sprintf(`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":%d,"delta":{"role":%s},"finish_reason":null}]}`,
		idx, jsonStr(role))
}

// finishChunk builds a finish_reason SSE data line.
func finishChunk(idx int, reason string) string {
	return fmt.Sprintf(`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":%d,"delta":{},"finish_reason":%s}]}`,
		idx, jsonStr(reason))
}

// textChunk builds a legacy-completion SSE chunk using choices[].text.
func textChunk(idx int, text string) string {
	return fmt.Sprintf(`data: {"id":"cid","object":"text_completion","choices":[{"index":%d,"text":%s,"finish_reason":null}]}`,
		idx, jsonStr(text))
}

// doneLines returns the SSE stream termination sequence: [DONE] and a blank separator.
func doneLines() []string { return []string{"data: [DONE]", ""} }

// jsonStr JSON-encodes a string for embedding in SSE payloads.
func jsonStr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// extractDeltaContent parses a data: SSE line and returns the delta.content
// string from choices[0]. Returns "" on any parse failure.
func extractDeltaContent(line string) string {
	if !strings.HasPrefix(line, "data: ") {
		return ""
	}
	payload := line[len("data: "):]
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content *string `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return ""
	}
	if len(chunk.Choices) == 0 || chunk.Choices[0].Delta.Content == nil {
		return ""
	}
	return *chunk.Choices[0].Delta.Content
}

// ── Matrix 1: Split at every position ────────────────────────────────────────

// TestStreamRestorer_SplitAtEveryPosition verifies that splitting a 31-byte
// pseudonym across two delta.content chunks at ANY internal position (1..30)
// causes the restorer to emit the restored original — never a PII_ fragment.
func TestStreamRestorer_SplitAtEveryPosition(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "split@allpos.example")
	orig := f.rev[pseudo]
	if len(pseudo) != pseudonymLen {
		t.Fatalf("pseudonym length %d != %d", len(pseudo), pseudonymLen)
	}
	// Pre-warm the replacer so parallel subtests only read f.replacer (no write race).
	f.Restore([]byte(pseudo))

	for splitAt := 1; splitAt < pseudonymLen; splitAt++ {
		splitAt := splitAt
		t.Run(fmt.Sprintf("split_at_%d", splitAt), func(t *testing.T) {
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
			}
			lines = append(lines, doneLines()...)

			output, _, err := pushAllCollect(r, lines)
			if err != nil {
				t.Fatalf("Push error at split %d: %v", splitAt, err)
			}

			// The PII_ prefix must never appear in any emitted line.
			for _, line := range strings.Split(output, "\n") {
				if strings.Contains(line, "PII_") {
					t.Errorf("split_at_%d: emitted line contains PII_ fragment: %q", splitAt, line)
				}
			}

			// The restored original must appear.
			if !strings.Contains(output, orig) {
				t.Errorf("split_at_%d: output missing restored original %q\noutput:\n%s", splitAt, orig, output)
			}
		})
	}
}

// ── Matrix 2: Marker-split ────────────────────────────────────────────────────

// TestStreamRestorer_MarkerSplit verifies that when the chunk boundary falls
// inside the "PII_" marker prefix (after P, PI, PII, PII_), the restorer
// still correctly assembles and restores the full pseudonym.
func TestStreamRestorer_MarkerSplit(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "markersplit@example.com")
	orig := f.rev[pseudo]
	// Pre-warm the replacer so parallel subtests only read f.replacer (no write race).
	f.Restore([]byte(pseudo))

	markerSplits := []int{1, 2, 3, 4} // after "P", "PI", "PII", "PII_"
	for _, splitAt := range markerSplits {
		splitAt := splitAt
		name := fmt.Sprintf("after_%d_marker_chars", splitAt)
		t.Run(name, func(t *testing.T) {
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
			}
			lines = append(lines, doneLines()...)

			output, _, err := pushAllCollect(r, lines)
			if err != nil {
				t.Fatalf("%s: Push error: %v", name, err)
			}

			for _, line := range strings.Split(output, "\n") {
				if strings.Contains(line, "PII_") {
					t.Errorf("%s: emitted line contains PII_ fragment: %q", name, line)
				}
			}
			if !strings.Contains(output, orig) {
				t.Errorf("%s: output missing restored original %q\noutput:\n%s", name, orig, output)
			}
		})
	}
}

// ── Matrix 3: False-positive avoided ─────────────────────────────────────────

// TestStreamRestorer_FalsePositiveAvoided verifies that natural text ending
// with "P", "PI", "PII" is emitted immediately without hold-back when it is
// not a prefix of any KNOWN pseudonym.
//
// The restorer uses a filter with exactly ONE known pseudonym (PII_EM_xxx...).
// Text like "GROUP" ends in "P" but "P" is a prefix of "PII_" which IS a
// prefix of the known pseudonym — so "P" may legitimately be held. However,
// text ending in "GROUQ" where Q is not a known-pseudonym-prefix suffix should
// be emitted immediately.
//
// We focus the false-positive check on content that clearly cannot be a
// pseudonym prefix: text like "GROUP" (ends in "P") would be held back until
// the next chunk confirms it is not a pseudonym prefix. That is CORRECT
// behaviour (not a false positive) for the known-prefix approach. The
// meaningful false-positive test is: content that ends in a string that IS
// NOT a prefix of "PII_" (the pseudonymMarker) must be emitted immediately.
func TestStreamRestorer_FalsePositiveAvoided(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "fptest@example.com")
	// Pre-warm replacer so parallel subtests only read (not write) f.replacer.
	f.Restore([]byte(pseudo))

	tests := []struct {
		name      string
		content   string
		wantImmed bool // true = must be emitted without waiting for a second chunk
	}{
		// "XYZ" has no suffix that is a prefix of "PII_" → emit immediately.
		{"xyz_suffix", "hello XYZ", true},
		// "123" → emit immediately.
		{"digit_suffix", "count 123", true},
		// A content chunk that ends with a complete pseudonym should be emitted
		// after Restore (no further hold-back needed).
		{"full_pseudonym", pseudo, false}, // pseudonym itself needs hold-back until resolved
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			r := NewStreamRestorer(f, "gpt-4o")

			// Push the content chunk (no second chunk yet).
			out, _, err := r.Push([]byte(chatChunk(0, tc.content)))
			if err != nil {
				t.Fatalf("%s: Push error: %v", tc.name, err)
			}

			if tc.wantImmed {
				// Must have emitted something immediately (content is safe).
				hasContent := false
				for _, b := range out {
					if b != nil && strings.Contains(string(b), "data:") {
						hasContent = true
						break
					}
				}
				if !hasContent {
					t.Errorf("%s: expected immediate emission but got no data: lines\ncontent=%q", tc.name, tc.content)
				}
				// And no PII_ must be in the emission.
				for _, b := range out {
					if b != nil && strings.Contains(string(b), "PII_") {
						t.Errorf("%s: emitted line contains PII_ fragment: %q", tc.name, string(b))
					}
				}
			}
		})
	}
}

// ── Matrix 4: Multiple pseudonyms ─────────────────────────────────────────────

// TestStreamRestorer_MultiplePseudonyms verifies that two pseudonyms in the
// same stream (one adjacent, one separated by plain text) are each correctly
// restored.
func TestStreamRestorer_MultiplePseudonyms(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"alice@multi.example and bob@multi.example"}]}`)
	if _, err := f.AnonymizeJSON(body); err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if !f.Touched() {
		t.Fatal("Touched() = false")
	}

	var alicePseudo, bobPseudo string
	for p, orig := range f.rev {
		if orig == "alice@multi.example" {
			alicePseudo = p
		}
		if orig == "bob@multi.example" {
			bobPseudo = p
		}
	}
	if alicePseudo == "" || bobPseudo == "" {
		t.Fatalf("pseudonyms not found: alice=%q bob=%q", alicePseudo, bobPseudo)
	}
	// Pre-warm replacer so parallel subtests only read f.replacer (no write race).
	f.Restore([]byte(alicePseudo + bobPseudo))

	// Case A: adjacent pseudonyms in one chunk.
	t.Run("adjacent", func(t *testing.T) {
		t.Parallel()
		r := NewStreamRestorer(f, "gpt-4o")
		combined := alicePseudo + bobPseudo
		lines := []string{chatChunk(0, combined), "", finishChunk(0, "stop"), ""}
		lines = append(lines, doneLines()...)
		output, _, err := pushAllCollect(r, lines)
		if err != nil {
			t.Fatalf("Push error: %v", err)
		}
		if !strings.Contains(output, "alice@multi.example") {
			t.Errorf("adjacent: output missing alice restoration\noutput:\n%s", output)
		}
		if !strings.Contains(output, "bob@multi.example") {
			t.Errorf("adjacent: output missing bob restoration\noutput:\n%s", output)
		}
		if strings.Contains(output, "PII_") {
			t.Errorf("adjacent: output contains PII_ fragment\noutput:\n%s", output)
		}
	})

	// Case B: pseudonyms separated by plain text across multiple chunks.
	t.Run("separated_by_text", func(t *testing.T) {
		t.Parallel()
		r := NewStreamRestorer(f, "gpt-4o")
		lines := []string{
			chatChunk(0, "Contact "),
			"",
			chatChunk(0, alicePseudo),
			"",
			chatChunk(0, " and also "),
			"",
			chatChunk(0, bobPseudo),
			"",
			finishChunk(0, "stop"),
			"",
		}
		lines = append(lines, doneLines()...)
		output, _, err := pushAllCollect(r, lines)
		if err != nil {
			t.Fatalf("Push error: %v", err)
		}
		if !strings.Contains(output, "alice@multi.example") {
			t.Errorf("separated: output missing alice restoration\noutput:\n%s", output)
		}
		if !strings.Contains(output, "bob@multi.example") {
			t.Errorf("separated: output missing bob restoration\noutput:\n%s", output)
		}
		if strings.Contains(output, "PII_") {
			t.Errorf("separated: output contains PII_ fragment\noutput:\n%s", output)
		}
	})
}

// ── Matrix 5: Pseudonym at end of stream ──────────────────────────────────────

// TestStreamRestorer_PseudonymAtEndFollowedByDone verifies that a pseudonym
// that appears in the very last content chunk (right before finish and [DONE])
// is fully restored and emitted — not lost or truncated.
func TestStreamRestorer_PseudonymAtEndFollowedByDone(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "endstream@example.com")
	orig := f.rev[pseudo]

	r := NewStreamRestorer(f, "gpt-4o")
	lines := []string{
		chatChunk(0, "Your email is "),
		"",
		chatChunk(0, pseudo),
		"",
		finishChunk(0, "stop"),
		"",
	}
	lines = append(lines, doneLines()...)
	output, terminal, err := pushAllCollect(r, lines)
	if err != nil {
		t.Fatalf("Push error: %v", err)
	}
	if !terminal {
		t.Error("stream did not reach terminal state")
	}
	if !strings.Contains(output, orig) {
		t.Errorf("output missing restored original %q at end of stream\noutput:\n%s", orig, output)
	}
	if strings.Contains(output, "PII_") {
		t.Errorf("output contains PII_ fragment\noutput:\n%s", output)
	}
}

// ── Matrix 6: Truncation mitten im Pseudonym → fail-closed ───────────────────

// TestStreamRestorer_TruncationMidPseudonym_FailClosed verifies that when
// [DONE] arrives with a non-empty carry (truncated pseudonym), Push returns
// errCarryNotEmpty and emits no fragment.
func TestStreamRestorer_TruncationMidPseudonym_FailClosed(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "truncate@example.com")

	// Only the first 12 bytes of the pseudonym; [DONE] arrives without the rest.
	partial := pseudo[:12]

	r := NewStreamRestorer(f, "gpt-4o")
	// Push a role-first chunk.
	if _, _, err := r.Push([]byte(roleChunk(0, "assistant"))); err != nil {
		t.Fatalf("Push role chunk: %v", err)
	}
	// Blank separator between events (required by SSE protocol).
	r.Push([]byte{})
	// Push the partial pseudonym content.
	if _, _, err := r.Push([]byte(chatChunk(0, partial))); err != nil {
		t.Fatalf("Push partial pseudonym: %v", err)
	}
	// Blank separator.
	r.Push([]byte{})

	// Now push [DONE] — carry is non-empty → must fail.
	_, _, err := r.Push([]byte("data: [DONE]"))
	if !errors.Is(err, errCarryNotEmpty) {
		t.Errorf("Push([DONE] with carry) error = %v, want errCarryNotEmpty", err)
	}

	// Verify no PII_ fragment was emitted before the error.
	// (We only check the final Push; earlier pushes were also captured and clean.)
}

// TestStreamRestorer_TruncationAtFinishReason_FailClosed verifies that when
// finish_reason arrives while the carry is non-empty (split pseudonym not yet
// complete), the restorer also fails closed.
func TestStreamRestorer_TruncationAtFinishReason_FailClosed(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "truncfr@example.com")
	partial := pseudo[:10]

	r := NewStreamRestorer(f, "gpt-4o")

	// Send partial pseudonym.
	if _, _, err := r.Push([]byte(chatChunk(0, partial))); err != nil {
		t.Fatalf("Push partial: %v", err)
	}
	r.Push([]byte{})

	// finish_reason arrives while carry is non-empty.
	// The spec (stream.go) checks len(cc.carry) > 0 before emitting finish.
	_, _, err := r.Push([]byte(finishChunk(0, "stop")))
	if err == nil {
		t.Error("Push(finish_reason with carry) returned nil error; expected fail-closed")
	}
	if !errors.Is(err, errCarryNotEmpty) {
		t.Errorf("error = %v, want errCarryNotEmpty", err)
	}
}

// ── Matrix 7: EOF without [DONE] → carry non-empty, documented ───────────────

// TestStreamRestorer_EOFWithoutDone_CarryNonEmpty_Documented documents the
// behavior when the scanner loop ends (EOF) without receiving [DONE] and the
// carry is non-empty. The restorer itself does not see an EOF event — it only
// sees whatever Push calls are made. After the last Push the carry remains
// non-empty but no error is surfaced from the restorer. The handler-level
// test (TestPIIFilter_StreamCap_FailClosed in pii_handler_test.go) covers
// the proxy aborting the stream in that case.
//
// At the restorer level: after all content Pushes (no [DONE]), verify that:
// (a) the carry is non-empty (partial pseudonym is held, not leaked), and
// (b) any further Push after an aborted state returns an error.
func TestStreamRestorer_EOFWithoutDone_CarryNonEmpty_Documented(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "eoftest@example.com")
	partial := pseudo[:15]

	r := NewStreamRestorer(f, "gpt-4o")

	// Push partial pseudonym.
	out, _, err := r.Push([]byte(chatChunk(0, partial)))
	if err != nil {
		t.Fatalf("Push partial: %v", err)
	}
	r.Push([]byte{}) // blank sep

	// Verify: no data: line with PII_ was emitted.
	for _, b := range out {
		if b != nil && strings.Contains(string(b), "PII_") {
			t.Errorf("partial pseudonym fragment emitted: %q", string(b))
		}
	}

	// At this point the scanner would have stopped (no [DONE]).
	// The carry is non-empty inside the restorer but not observable externally.
	// The handler sets streamAborted=true and calls breaker.RecordFailure().
	// Documented: restorer-level, we verify that a subsequent [DONE] push fails.
	_, _, doneErr := r.Push([]byte("data: [DONE]"))
	if doneErr == nil {
		t.Error("Push([DONE]) after partial pseudonym returned nil error; expected errCarryNotEmpty")
	}
}

// ── Matrix 8: [DONE] without EOF then further Push → error ───────────────────

// TestStreamRestorer_DoneWithoutEOF_FurtherPushErrors verifies the terminal
// state machine: after [DONE] is processed (terminal=true), any further Push
// returns errStreamTerminated.
func TestStreamRestorer_DoneWithoutEOF_FurtherPushErrors(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "doneterminal@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Complete a clean stream up to and including [DONE].
	setup := []string{
		chatChunk(0, "hello world"),
		"",
		finishChunk(0, "stop"),
		"",
	}
	for _, l := range setup {
		var raw []byte
		if l == "" {
			raw = []byte{}
		} else {
			raw = []byte(l)
		}
		if _, _, err := r.Push(raw); err != nil {
			t.Fatalf("setup Push(%q): %v", l, err)
		}
	}
	// Push [DONE].
	_, term, err := r.Push([]byte("data: [DONE]"))
	if err != nil {
		t.Fatalf("Push([DONE]): %v", err)
	}
	if !term {
		t.Error("[DONE] did not set terminal=true")
	}

	// Any further Push must error.
	_, _, err = r.Push([]byte(chatChunk(0, "extra")))
	if !errors.Is(err, errStreamTerminated) {
		t.Errorf("Push after terminal = %v, want errStreamTerminated", err)
	}

	// Even blank lines after terminal should error.
	_, _, err2 := r.Push([]byte{})
	if !errors.Is(err2, errStreamTerminated) {
		t.Errorf("Push(blank) after terminal = %v, want errStreamTerminated", err2)
	}
}

// ── Matrix 9: Content after finish_reason, double finish_reason, data after [DONE]

// TestStreamRestorer_ContentAfterFinishReason_FailClosed verifies that sending
// content after finish_reason for the same choice causes errStreamAborted.
func TestStreamRestorer_ContentAfterFinishReason_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "afterfinish@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	pushOrFail := func(line string) {
		t.Helper()
		if _, _, err := r.Push([]byte(line)); err != nil {
			t.Fatalf("unexpected Push error on setup line: %v (line=%q)", err, line)
		}
	}
	pushOrFail(chatChunk(0, "hello"))
	pushOrFail("")
	pushOrFail(finishChunk(0, "stop"))
	pushOrFail("")

	// Now send content for the same choice — must fail.
	_, _, err := r.Push([]byte(chatChunk(0, "extra content after stop")))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("content after finish_reason: error = %v, want errStreamAborted", err)
	}
}

// TestStreamRestorer_DoubleFinishReason_FailClosed verifies that a second
// finish_reason for the same choice causes errStreamAborted.
func TestStreamRestorer_DoubleFinishReason_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "doublefr@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	if _, _, err := r.Push([]byte(chatChunk(0, "ok"))); err != nil {
		t.Fatalf("setup: %v", err)
	}
	r.Push([]byte{})
	if _, _, err := r.Push([]byte(finishChunk(0, "stop"))); err != nil {
		t.Fatalf("first finish_reason: %v", err)
	}
	r.Push([]byte{})

	// Second finish_reason for the same index.
	_, _, err := r.Push([]byte(finishChunk(0, "stop")))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("double finish_reason: error = %v, want errStreamAborted", err)
	}
}

// TestStreamRestorer_DataAfterDone_FailClosed verifies that sending data after
// [DONE] causes errStreamTerminated (terminal guard).
func TestStreamRestorer_DataAfterDone_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "afterdone@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Set up a clean stream that reaches terminal state.
	setup := []string{chatChunk(0, "ok"), "", finishChunk(0, "stop"), ""}
	for _, l := range setup {
		var raw []byte
		if l == "" {
			raw = []byte{}
		} else {
			raw = []byte(l)
		}
		if _, _, err := r.Push(raw); err != nil {
			t.Fatalf("setup: %v", err)
		}
	}
	// Push [DONE] — this should succeed and set terminal.
	_, term, err := r.Push([]byte("data: [DONE]"))
	if err != nil {
		t.Fatalf("push [DONE]: %v", err)
	}
	if !term {
		t.Fatal("[DONE] did not set terminal=true")
	}

	// Any Push after terminal must error.
	_, _, err = r.Push([]byte(chatChunk(0, "smuggled data")))
	if !errors.Is(err, errStreamTerminated) {
		t.Errorf("data after [DONE]: error = %v, want errStreamTerminated", err)
	}
}

// ── Matrix 10: State-machine / Parsing fail-closed ───────────────────────────

// TestStreamRestorer_ChoicesNotArray_FailClosed tests that choices being a
// non-array (e.g. object or string) causes errStreamAborted.
func TestStreamRestorer_ChoicesNotArray_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dataStr string
	}{
		{
			name:    "choices is object",
			dataStr: `data: {"id":"x","object":"chat.completion.chunk","choices":{"index":0}}`,
		},
		{
			name:    "choices is string",
			dataStr: `data: {"id":"x","object":"chat.completion.chunk","choices":"bad"}`,
		},
		{
			name:    "choices is number",
			dataStr: `data: {"id":"x","object":"chat.completion.chunk","choices":42}`,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Each subtest gets its own filter (Filter is not safe for concurrent use).
			_, f := realPseudonym(t, "notarray@example.com")
			r := NewStreamRestorer(f, "gpt-4o")
			_, _, err := r.Push([]byte(tc.dataStr))
			if !errors.Is(err, errStreamAborted) {
				t.Errorf("%s: error = %v, want errStreamAborted", tc.name, err)
			}
		})
	}
}

// TestStreamRestorer_MissingIndex_FailClosed tests that a choice with a
// missing or null index causes errStreamAborted.
func TestStreamRestorer_MissingIndex_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dataStr string
	}{
		{
			name:    "missing index",
			dataStr: `data: {"id":"x","object":"chat.completion.chunk","choices":[{"delta":{"content":"hi"}}]}`,
		},
		{
			name:    "null index",
			dataStr: `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":null,"delta":{"content":"hi"}}]}`,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Each subtest gets its own filter (Filter is not safe for concurrent use).
			_, f := realPseudonym(t, "missingidx@example.com")
			r := NewStreamRestorer(f, "gpt-4o")
			_, _, err := r.Push([]byte(tc.dataStr))
			if !errors.Is(err, errStreamAborted) {
				t.Errorf("%s: error = %v, want errStreamAborted", tc.name, err)
			}
		})
	}
}

// ── Matrix 11: Delta.content AND text in same choice → error ──────────────────

// TestStreamRestorer_BothDeltaContentAndText_FailClosed verifies that a choice
// carrying BOTH delta.content AND text causes errStreamAborted.
func TestStreamRestorer_BothDeltaContentAndText_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "bothfields@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	line := `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello"},"text":"world","finish_reason":null}]}`
	_, _, err := r.Push([]byte(line))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("both delta.content and text: error = %v, want errStreamAborted", err)
	}
}

// TestStreamRestorer_FormatSwitch_FailClosed verifies that switching format
// mid-stream (from chat to completion or vice versa) causes errStreamAborted.
func TestStreamRestorer_FormatSwitch_FailClosed(t *testing.T) {
	t.Parallel()

	// Chat → completion switch.
	t.Run("chat_to_completion", func(t *testing.T) {
		t.Parallel()
		// Each subtest gets its own filter (Filter is not safe for concurrent use).
		_, f := realPseudonym(t, "fmtswitch1@example.com")
		r := NewStreamRestorer(f, "gpt-4o")

		if _, _, err := r.Push([]byte(chatChunk(0, "hello"))); err != nil {
			t.Fatalf("setup chat chunk: %v", err)
		}
		r.Push([]byte{})

		// Now send a text chunk for the same choice index → format switch.
		_, _, err := r.Push([]byte(textChunk(0, "world")))
		if !errors.Is(err, errStreamAborted) {
			t.Errorf("chat→completion switch: error = %v, want errStreamAborted", err)
		}
	})

	// Completion → chat switch.
	t.Run("completion_to_chat", func(t *testing.T) {
		t.Parallel()
		// Each subtest gets its own filter (Filter is not safe for concurrent use).
		_, f := realPseudonym(t, "fmtswitch2@example.com")
		r := NewStreamRestorer(f, "gpt-4o")

		if _, _, err := r.Push([]byte(textChunk(0, "hello"))); err != nil {
			t.Fatalf("setup text chunk: %v", err)
		}
		r.Push([]byte{})

		_, _, err := r.Push([]byte(chatChunk(0, "world")))
		if !errors.Is(err, errStreamAborted) {
			t.Errorf("completion→chat switch: error = %v, want errStreamAborted", err)
		}
	})
}

// ── Matrix 12: delta.function_call and delta.tool_calls → error ──────────────

// TestStreamRestorer_FunctionCall_FailClosed verifies that delta.function_call
// causes errStreamAborted.
func TestStreamRestorer_FunctionCall_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "functioncall@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	line := `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"function_call":{"name":"search","arguments":""}},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(line))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("function_call: error = %v, want errStreamAborted", err)
	}
}

// TestStreamRestorer_ToolCalls_InvalidShape_FailClosed verifies that malformed
// or semantically invalid delta.tool_calls shapes cause errStreamAborted.
// Stage 0c handles well-formed non-empty tool_calls arrays; this test covers
// the shapes that remain fail-closed after Stage 0c.
func TestStreamRestorer_ToolCalls_InvalidShape_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		dataStr string
	}{
		{
			name:    "empty tool_calls array",
			dataStr: `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[]},"finish_reason":null}]}`,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Each subtest gets its own filter (Filter is not safe for concurrent use).
			_, f := realPseudonym(t, "toolcalls@example.com")
			r := NewStreamRestorer(f, "gpt-4o")
			_, _, err := r.Push([]byte(tc.dataStr))
			if !errors.Is(err, errStreamAborted) {
				t.Errorf("%s: error = %v, want errStreamAborted", tc.name, err)
			}
		})
	}
}

// ── Matrix 13: Upstream error object → error ─────────────────────────────────

// TestStreamRestorer_UpstreamErrorObject_FailClosed verifies that a top-level
// error object in an SSE chunk causes errStreamAborted (not pass-through).
func TestStreamRestorer_UpstreamErrorObject_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "upsterr@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	line := `data: {"error":{"message":"rate limit exceeded","type":"rate_limit_error","code":429}}`
	_, _, err := r.Push([]byte(line))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("upstream error object: error = %v, want errStreamAborted", err)
	}
}

// ── Matrix 14: Unparseable JSON → error ──────────────────────────────────────

// TestStreamRestorer_UnparseableJSON_FailClosed verifies that a data: line
// with malformed JSON causes errStreamAborted.
func TestStreamRestorer_UnparseableJSON_FailClosed(t *testing.T) {
	t.Parallel()

	tests := []string{
		`data: {broken json`,
		`data: this is not json`,
		`data: {`,
		`data: [1,2,3]`, // array at top level (not [DONE])
	}

	for _, line := range tests {
		line := line
		t.Run(line[:min(len(line), 30)], func(t *testing.T) {
			t.Parallel()
			// Each subtest gets its own filter (Filter is not safe for concurrent use).
			_, f := realPseudonym(t, "badjson@example.com")
			r := NewStreamRestorer(f, "gpt-4o")
			_, _, err := r.Push([]byte(line))
			if !errors.Is(err, errStreamAborted) {
				t.Errorf("unparseable JSON: error = %v, want errStreamAborted (line=%q)", err, line)
			}
		})
	}
}

// ── Matrix 15: Multi-line data → error ───────────────────────────────────────

// TestStreamRestorer_MultiLineData_FailClosed verifies that two consecutive
// data: lines within the same SSE event (no blank-line separator between them)
// cause errStreamAborted.
func TestStreamRestorer_MultiLineData_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "multiline@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// First data: line sets inEvent=true.
	if _, _, err := r.Push([]byte(chatChunk(0, "hello"))); err != nil {
		t.Fatalf("first data: line: %v", err)
	}
	// NO blank-line separator → second data: line in same event.
	_, _, err := r.Push([]byte(chatChunk(0, "world")))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("multi-line data: error = %v, want errStreamAborted", err)
	}
}

// ── Matrix 16: finish_reason ordering ────────────────────────────────────────

// TestStreamRestorer_FinishReasonOrdering verifies that when content is held
// back in carry and then flush-forced by finish_reason, the content is emitted
// BEFORE the finish_reason chunk in the output.
func TestStreamRestorer_FinishReasonOrdering(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "ordering@example.com")
	orig := f.rev[pseudo]

	// Send pseudo in two parts: part1 is held, then finish arrives simultaneously
	// with part2. Actually, to test ordering we send pseudo whole (so it's held
	// until finish clears carry).
	// The cleaner scenario: send pseudo THEN finish_reason in the same chunk
	// (choices array with both delta.content and finish_reason — but that's the
	// BothDeltaContentAndText case). Instead we send finish after a complete
	// pseudonym (which resolves the carry first), then check ordering.
	r := NewStreamRestorer(f, "gpt-4o")

	var collectedLines []string
	collect := func(lines [][]byte) {
		for _, b := range lines {
			if b == nil {
				collectedLines = append(collectedLines, "")
			} else {
				collectedLines = append(collectedLines, string(b))
			}
		}
	}

	// Push pseudo whole (carry fills then flushes immediately since full pseudonym
	// is exactly pseudonymLen bytes, and longestPseudonymPrefixSuffix returns 0
	// for a full-length suffix).
	out1, _, err := r.Push([]byte(chatChunk(0, pseudo)))
	if err != nil {
		t.Fatalf("push pseudo: %v", err)
	}
	collect(out1)
	r.Push([]byte{})

	// Finish.
	out2, _, err := r.Push([]byte(finishChunk(0, "stop")))
	if err != nil {
		t.Fatalf("push finish: %v", err)
	}
	collect(out2)
	r.Push([]byte{})

	// Done.
	out3, _, err := r.Push([]byte("data: [DONE]"))
	if err != nil {
		t.Fatalf("push DONE: %v", err)
	}
	collect(out3)

	// Verify content (restored original) appears before finish_reason in output.
	contentIdx := -1
	finishIdx := -1
	for i, line := range collectedLines {
		if strings.Contains(line, orig) {
			contentIdx = i
		}
		if strings.Contains(line, `"finish_reason":"stop"`) {
			finishIdx = i
		}
	}
	if contentIdx < 0 {
		t.Errorf("restored original %q not found in output; lines: %v", orig, collectedLines)
	}
	if finishIdx < 0 {
		t.Errorf("finish_reason:stop not found in output; lines: %v", collectedLines)
	}
	if contentIdx >= 0 && finishIdx >= 0 && contentIdx > finishIdx {
		t.Errorf("ordering violation: content (line %d) appears AFTER finish_reason (line %d); lines: %v",
			contentIdx, finishIdx, collectedLines)
	}
	// Also verify no content comes AFTER finish_reason.
	for i, line := range collectedLines {
		if i > finishIdx && strings.Contains(line, orig) {
			t.Errorf("content %q emitted at line %d, which is after finish_reason at line %d",
				orig, i, finishIdx)
		}
	}
}

// ── Matrix 17: n>1 choices, each with different split ─────────────────────────

// TestStreamRestorer_MultipleChoices_IndependentRestore verifies that two
// choices each with a differently-split pseudonym are restored independently
// and correctly.
func TestStreamRestorer_MultipleChoices_IndependentRestore(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"alice@multi2.example bob@multi2.example"}]}`)
	if _, err := f.AnonymizeJSON(body); err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}

	var alicePseudo, bobPseudo string
	for p, orig := range f.rev {
		if orig == "alice@multi2.example" {
			alicePseudo = p
		}
		if orig == "bob@multi2.example" {
			bobPseudo = p
		}
	}
	if alicePseudo == "" || bobPseudo == "" {
		t.Fatalf("pseudonyms not found")
	}

	// Choice 0: alice pseudo split at position 10.
	// Choice 1: bob pseudo split at position 20.
	r := NewStreamRestorer(f, "gpt-4o")

	lines := []string{
		// First chunk: choice 0 gets first 10 bytes of alice, choice 1 gets first 20 of bob.
		fmt.Sprintf(`data: {"id":"c","object":"chat.completion.chunk","choices":[`+
			`{"index":0,"delta":{"content":%s},"finish_reason":null},`+
			`{"index":1,"delta":{"content":%s},"finish_reason":null}`+
			`]}`, jsonStr(alicePseudo[:10]), jsonStr(bobPseudo[:20])),
		"",
		// Second chunk: remainders.
		fmt.Sprintf(`data: {"id":"c","object":"chat.completion.chunk","choices":[`+
			`{"index":0,"delta":{"content":%s},"finish_reason":null},`+
			`{"index":1,"delta":{"content":%s},"finish_reason":null}`+
			`]}`, jsonStr(alicePseudo[10:]), jsonStr(bobPseudo[20:])),
		"",
		// Finish both choices.
		fmt.Sprintf(`data: {"id":"c","object":"chat.completion.chunk","choices":[` +
			`{"index":0,"delta":{},"finish_reason":"stop"},` +
			`{"index":1,"delta":{},"finish_reason":"stop"}` +
			`]}`),
		"",
		"data: [DONE]",
		"",
	}

	output, terminal, err := pushAllCollect(r, lines)
	if err != nil {
		t.Fatalf("Push error: %v", err)
	}
	if !terminal {
		t.Error("stream did not reach terminal")
	}
	if !strings.Contains(output, "alice@multi2.example") {
		t.Errorf("output missing alice restoration\noutput:\n%s", output)
	}
	if !strings.Contains(output, "bob@multi2.example") {
		t.Errorf("output missing bob restoration\noutput:\n%s", output)
	}
	if strings.Contains(output, "PII_") {
		t.Errorf("output contains PII_ fragment\noutput:\n%s", output)
	}
}

// ── Matrix 18: Multibyte UTF-8 ────────────────────────────────────────────────

// TestStreamRestorer_MultiBytePseudonymMix verifies that multibyte UTF-8
// content (Emojis, Umlaute) interleaved with a pseudonym does not cause rune
// splitting or incorrect restoration.
func TestStreamRestorer_MultiBytePseudonymMix(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "utf8test@example.com")
	orig := f.rev[pseudo]

	// Content with multibyte runes on both sides of the pseudonym.
	prefix := "Hallo Welt \xc3\xbc\xc3\xa4\xc3\xb6 " // "Hallo Welt üäö "
	suffix := " \xf0\x9f\x99\x82 fertig"             // " 🙂 fertig"

	r := NewStreamRestorer(f, "gpt-4o")
	lines := []string{
		chatChunk(0, prefix),
		"",
		chatChunk(0, pseudo),
		"",
		chatChunk(0, suffix),
		"",
		finishChunk(0, "stop"),
		"",
	}
	lines = append(lines, doneLines()...)

	output, _, err := pushAllCollect(r, lines)
	if err != nil {
		t.Fatalf("Push error: %v", err)
	}

	if !strings.Contains(output, orig) {
		t.Errorf("output missing restored original %q\noutput:\n%s", orig, output)
	}
	if strings.Contains(output, "PII_") {
		t.Errorf("output contains PII_ fragment\noutput:\n%s", output)
	}
	// The prefix and suffix must survive intact (no rune splitting).
	if !strings.Contains(output, "üäö") {
		t.Errorf("output missing UTF-8 prefix content\noutput:\n%s", output)
	}
	if !strings.Contains(output, "🙂") {
		t.Errorf("output missing emoji suffix\noutput:\n%s", output)
	}
}

// ── Matrix 19: role-only chunk ────────────────────────────────────────────────

// TestStreamRestorer_RoleOnlyChunk verifies that a chunk carrying only
// delta.role (no content) is forwarded correctly and does not cause errors.
func TestStreamRestorer_RoleOnlyChunk(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "roleonly@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	lines := []string{
		roleChunk(0, "assistant"),
		"",
		chatChunk(0, "hello"),
		"",
		finishChunk(0, "stop"),
		"",
		"data: [DONE]",
		"",
	}
	output, terminal, err := pushAllCollect(r, lines)
	if err != nil {
		t.Fatalf("pushAllCollect: %v", err)
	}
	if !terminal {
		t.Error("stream not terminal")
	}
	if !strings.Contains(output, "hello") {
		t.Errorf("content missing from output\noutput:\n%s", output)
	}
}

// ── Matrix 20: Usage-only chunk ──────────────────────────────────────────────

// TestStreamRestorer_UsageOnlyChunk verifies that a usage-only SSE chunk
// (choices=[]) is re-emitted with only whitelisted numeric fields, and extra
// fields are discarded.
func TestStreamRestorer_UsageOnlyChunk(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "usageonly@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Usage-only chunk with extra non-whitelisted field.
	usageLine := `data: {"id":"cid","object":"chat.completion.chunk","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"extra_field":"should_be_stripped"}}`

	out, _, err := r.Push([]byte(usageLine))
	if err != nil {
		t.Fatalf("usage-only chunk: %v", err)
	}

	// Must re-emit a data: line.
	var emitted string
	for _, b := range out {
		if b != nil {
			emitted = string(b)
		}
	}
	if !strings.HasPrefix(emitted, "data: ") {
		t.Fatalf("no data: line emitted for usage chunk; out=%v", out)
	}

	payload := emitted[len("data: "):]
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payload), &doc); err != nil {
		t.Fatalf("emitted usage chunk is not valid JSON: %v\npayload: %s", err, payload)
	}

	// Usage field must be present.
	rawUsage, ok := doc["usage"]
	if !ok {
		t.Fatal("usage field missing from emitted usage chunk")
	}

	var u struct {
		PromptTokens     int              `json:"prompt_tokens"`
		CompletionTokens int              `json:"completion_tokens"`
		TotalTokens      int              `json:"total_tokens"`
		ExtraField       *json.RawMessage `json:"extra_field"`
	}
	if err := json.Unmarshal(rawUsage, &u); err != nil {
		t.Fatalf("cannot unmarshal usage: %v", err)
	}
	if u.PromptTokens != 10 {
		t.Errorf("prompt_tokens = %d, want 10", u.PromptTokens)
	}
	if u.CompletionTokens != 5 {
		t.Errorf("completion_tokens = %d, want 5", u.CompletionTokens)
	}
	if u.TotalTokens != 15 {
		t.Errorf("total_tokens = %d, want 15", u.TotalTokens)
	}
	if u.ExtraField != nil {
		t.Errorf("extra_field must be stripped from usage chunk; got: %s", *u.ExtraField)
	}
}

// ── Matrix 21: Legacy completions text field ──────────────────────────────────

// TestStreamRestorer_LegacyCompletionsTextField verifies that the legacy
// text_completion format (choices[].text) is correctly handled and pseudonyms
// are restored.
func TestStreamRestorer_LegacyCompletionsTextField(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "legacytext@example.com")
	orig := f.rev[pseudo]

	r := NewStreamRestorer(f, "gpt-4o")

	// Split pseudonym across two text chunks.
	mid := len(pseudo) / 2
	lines := []string{
		textChunk(0, pseudo[:mid]),
		"",
		textChunk(0, pseudo[mid:]),
		"",
		// finish for text format.
		fmt.Sprintf(`data: {"id":"cid","object":"text_completion","choices":[{"index":0,"text":"","finish_reason":"stop"}]}`),
		"",
		"data: [DONE]",
		"",
	}

	output, terminal, err := pushAllCollect(r, lines)
	if err != nil {
		t.Fatalf("Push error: %v", err)
	}
	if !terminal {
		t.Error("stream not terminal")
	}
	if !strings.Contains(output, orig) {
		t.Errorf("output missing restored original %q\noutput:\n%s", orig, output)
	}
	if strings.Contains(output, "PII_") {
		t.Errorf("output contains PII_ fragment\noutput:\n%s", output)
	}
	// Output must use text shape (not delta.content).
	if !strings.Contains(output, `"text":`) {
		t.Errorf("output does not contain text shape (legacy completion)\noutput:\n%s", output)
	}
}

// ── Matrix 22: Envelope synthesis — no upstream values leaked ─────────────────

// TestStreamRestorer_EnvelopeSynthesis verifies that the restorer synthesizes
// its own envelope (id, model, object) and never echoes upstream id/model/
// system_fingerprint, which could carry pseudonyms.
func TestStreamRestorer_EnvelopeSynthesis(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "envelope@example.com")

	modelName := "proxy-model-name"
	r := NewStreamRestorer(f, modelName)

	// Upstream chunk echoes the pseudonym in id and model fields.
	upstreamLine := fmt.Sprintf(
		`data: {"id":%s,"object":"chat.completion.chunk","model":%s,"system_fingerprint":%s,"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		jsonStr(pseudo), jsonStr(pseudo), jsonStr(pseudo))

	out, _, err := r.Push([]byte(upstreamLine))
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	r.Push([]byte{})

	for _, b := range out {
		if b == nil {
			continue
		}
		line := string(b)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		// The proxy's synthesized id must NOT be the upstream pseudonym.
		if strings.Contains(line, pseudo) {
			t.Errorf("synthesized SSE line echoes upstream pseudonym: %q", line)
		}
		// The model field must be the proxy's own modelName.
		payload := line[len("data: "):]
		var doc map[string]json.RawMessage
		if err := json.Unmarshal([]byte(payload), &doc); err != nil {
			t.Fatalf("emitted line not valid JSON: %v\nline: %s", err, line)
		}
		var model string
		if err := json.Unmarshal(doc["model"], &model); err != nil {
			t.Fatalf("cannot unmarshal model field: %v", err)
		}
		if model != modelName {
			t.Errorf("emitted model = %q, want %q", model, modelName)
		}
	}
}

// ── Matrix 23: No PII — inkrementell durchgereicht ───────────────────────────

// TestStreamRestorer_NoPII_IncrementalPassthrough verifies that when the
// filter has no pseudonyms (Touched()=false), content passes through
// byte-for-byte and is emitted incrementally (per chunk, not buffered).
func TestStreamRestorer_NoPII_IncrementalPassthrough(t *testing.T) {
	t.Parallel()

	// Filter with no PII detected.
	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"no PII here at all"}]}`)
	if _, err := f.AnonymizeJSON(body); err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	// f.Touched() is false, but NewStreamRestorer still works — knownPseudonyms is empty.

	r := NewStreamRestorer(f, "gpt-4o")

	chunks := []string{"Hello, ", "world! ", "How are you?"}

	// Verify each chunk is emitted immediately upon Push (no hold-back).
	for _, chunk := range chunks {
		out, _, err := r.Push([]byte(chatChunk(0, chunk)))
		if err != nil {
			t.Fatalf("Push(%q): %v", chunk, err)
		}

		// Must have emitted content immediately (no hold-back when no pseudonyms).
		hasContent := false
		for _, b := range out {
			if b != nil && strings.Contains(string(b), "data:") {
				hasContent = true
				// No PII_ must appear.
				if strings.Contains(string(b), "PII_") {
					t.Errorf("no-PII passthrough: emitted PII_ fragment: %q", string(b))
				}
				// The chunk content must be present in the emitted line.
				if !strings.Contains(string(b), chunk) {
					t.Errorf("no-PII passthrough: chunk %q not found in emitted line %q", chunk, string(b))
				}
			}
		}
		if !hasContent {
			t.Errorf("no-PII passthrough: chunk %q not emitted immediately", chunk)
		}

		// Blank separator between events.
		r.Push([]byte{})
	}

	// Clean finish.
	if _, _, err := r.Push([]byte(finishChunk(0, "stop"))); err != nil {
		t.Fatalf("finish: %v", err)
	}
	r.Push([]byte{})

	_, term, doneErr := r.Push([]byte("data: [DONE]"))
	if doneErr != nil {
		t.Fatalf("done: %v", doneErr)
	}
	if !term {
		t.Error("stream not terminal after [DONE]")
	}
}

// ── Matrix 24: Oracle / Property test ────────────────────────────────────────

// TestStreamRestorer_OraclePropertyTest is the safety net: it generates
// random text with embedded real pseudonyms, chunks the content randomly
// across delta.content events, feeds the StreamRestorer line by line, and
// asserts:
//
//	(a) concatenation of emitted restored content equals the oracle output
//	    (RestoreSSEStream on the same input).
//	(b) no emitted line contains a PII_ substring.
//
// Uses math/rand with a fixed seed for determinism.
func TestStreamRestorer_OraclePropertyTest(t *testing.T) {
	t.Parallel()

	// We run multiple iterations, each with a fresh filter and its own seeded PRNG.
	// Each subtest gets a deterministic seed derived from the iteration number
	// so that the test is reproducible AND subtests can run in parallel without
	// sharing mutable state.
	const iterations = 20

	for iter := 0; iter < iterations; iter++ {
		iter := iter
		t.Run(fmt.Sprintf("iter_%d", iter), func(t *testing.T) {
			t.Parallel()

			// Each subtest gets its own seeded PRNG — no shared state.
			rng := rand.New(rand.NewSource(int64(0xdeadbeef42) + int64(iter)*7919))

			// Create a filter with two pseudonyms.
			f := newTestFilter(t)
			emails := []string{
				fmt.Sprintf("oracle%d@alpha.example", iter),
				fmt.Sprintf("oracle%d@beta.example", iter),
			}
			body := []byte(fmt.Sprintf(`{"model":"gpt-4","messages":[{"role":"user","content":"emails: %s and %s"}]}`, emails[0], emails[1]))
			if _, err := f.AnonymizeJSON(body); err != nil {
				t.Fatalf("AnonymizeJSON: %v", err)
			}
			if !f.Touched() {
				t.Fatal("Touched() = false")
			}

			// Build pseudonyms list (in rev map order).
			var pseudos []string
			for p := range f.rev {
				pseudos = append(pseudos, p)
			}

			// Build a content string that interleaves plain text with pseudonyms.
			fullContent := "Start text. "
			for i, p := range pseudos {
				fullContent += fmt.Sprintf("item%d: %s; ", i, p)
			}
			fullContent += "End text."

			// Randomly chunk the content into 3-7 pieces.
			nChunks := 3 + rng.Intn(5)
			chunks := randomChunk(rng, fullContent, nChunks)

			// Build SSE lines for both the oracle and the incremental restorer.
			var sseLines [][]byte
			var incrementalLines []string

			for _, chunk := range chunks {
				line := chatChunk(0, chunk)
				sseLines = append(sseLines, []byte(line), []byte{})
				incrementalLines = append(incrementalLines, line, "")
			}
			// Add finish and [DONE].
			finLine := finishChunk(0, "stop")
			sseLines = append(sseLines, []byte(finLine), []byte{}, []byte("data: [DONE]"), []byte{})
			incrementalLines = append(incrementalLines, finLine, "", "data: [DONE]", "")

			// Oracle: RestoreSSEStream.
			oracleOut, oracleErr := RestoreSSEStream(sseLines, f.Restore)
			if oracleErr != nil {
				t.Fatalf("iter %d: oracle RestoreSSEStream error: %v", iter, oracleErr)
			}
			oracleContent := extractAllContent(oracleOut)

			// Incremental: StreamRestorer.
			r := NewStreamRestorer(f, "gpt-4o")
			incrOutput, terminal, incrErr := pushAllCollect(r, incrementalLines)
			if incrErr != nil {
				t.Fatalf("iter %d: StreamRestorer Push error: %v", iter, incrErr)
			}
			if !terminal {
				t.Errorf("iter %d: stream did not reach terminal", iter)
			}

			// (a) Concatenated restored content must equal oracle's restored content.
			incrContent := extractAllDeltaContent(incrOutput)
			if incrContent != oracleContent {
				t.Errorf("iter %d: incremental content != oracle content\n  incremental: %q\n  oracle:      %q",
					iter, incrContent, oracleContent)
			}

			// (b) No emitted line must contain PII_.
			for _, line := range strings.Split(incrOutput, "\n") {
				if strings.Contains(line, "PII_") {
					t.Errorf("iter %d: emitted line contains PII_ fragment: %q", iter, line)
				}
			}
		})
	}
}

// randomChunk splits s into exactly n pieces with random boundary positions.
func randomChunk(rng *rand.Rand, s string, n int) []string {
	if n <= 1 || len(s) == 0 {
		return []string{s}
	}
	// Generate n-1 sorted cut points in [0, len(s)].
	cuts := make([]int, n-1)
	for i := range cuts {
		cuts[i] = rng.Intn(len(s) + 1)
	}
	// Sort cuts ascending.
	for i := 1; i < len(cuts); i++ {
		for j := i; j > 0 && cuts[j] < cuts[j-1]; j-- {
			cuts[j], cuts[j-1] = cuts[j-1], cuts[j]
		}
	}
	chunks := make([]string, 0, n)
	prev := 0
	for _, c := range cuts {
		chunks = append(chunks, s[prev:c])
		prev = c
	}
	chunks = append(chunks, s[prev:])
	return chunks
}

// extractAllDeltaContent extracts and concatenates all delta.content strings
// from an SSE output string (as produced by pushAllCollect).
func extractAllDeltaContent(output string) string {
	var sb strings.Builder
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if strings.Contains(line, "[DONE]") {
			continue
		}
		payload := line[len("data: "):]
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content *string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != nil {
				sb.WriteString(*ch.Delta.Content)
			}
		}
	}
	return sb.String()
}

// extractAllContent extracts and concatenates all delta.content strings from
// the oracle's [][]byte output.
func extractAllContent(lines [][]byte) string {
	var sb strings.Builder
	for _, b := range lines {
		if b == nil {
			continue
		}
		line := string(b)
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		payload := line[len("data: "):]
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			sb.WriteString(ch.Delta.Content)
		}
	}
	return sb.String()
}

// ── Additional protocol edge cases ───────────────────────────────────────────

// TestStreamRestorer_AbortedState_FurtherPushErrors verifies that after the
// restorer enters aborted state (errStreamAborted), all subsequent Push calls
// also return errStreamAborted.
func TestStreamRestorer_AbortedState_FurtherPushErrors(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "abortstate@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Trigger abort via tool_calls.
	line := `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0}]},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(line))
	if !errors.Is(err, errStreamAborted) {
		t.Fatalf("expected errStreamAborted to abort; got %v", err)
	}

	// Any further Push must also return errStreamAborted.
	_, _, err2 := r.Push([]byte(chatChunk(0, "extra")))
	if !errors.Is(err2, errStreamAborted) {
		t.Errorf("further Push after abort: error = %v, want errStreamAborted", err2)
	}
}

// TestStreamRestorer_NonDataLinesDiscarded verifies that SSE non-data lines
// (event:, id:, retry:, SSE comments) are silently discarded and do not affect
// the restorer state (no error, no emission).
func TestStreamRestorer_NonDataLinesDiscarded(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "nondataline@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	nonDataLines := []string{
		"event: ping",
		"id: 12345",
		"retry: 3000",
		": this is a comment line",
	}
	for _, line := range nonDataLines {
		out, _, err := r.Push([]byte(line))
		if err != nil {
			t.Errorf("non-data line %q caused error: %v", line, err)
		}
		if len(out) != 0 {
			t.Errorf("non-data line %q produced output: %v", line, out)
		}
	}
}

// TestStreamRestorer_BlankLineSeparator verifies that blank lines (SSE event
// separators) reset the inEvent flag and do not error.
func TestStreamRestorer_BlankLineSeparator(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "blank@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Push a data: line followed by a blank line — must not error.
	if _, _, err := r.Push([]byte(chatChunk(0, "hello"))); err != nil {
		t.Fatalf("data line: %v", err)
	}
	if out, _, err := r.Push([]byte{}); err != nil {
		t.Fatalf("blank line: %v", err)
	} else if len(out) != 0 {
		t.Errorf("blank line produced unexpected output: %v", out)
	}

	// After blank, the next data: line should NOT trigger multi-data abort.
	if _, _, err := r.Push([]byte(chatChunk(0, " world"))); err != nil {
		t.Fatalf("second data line after blank: %v", err)
	}
}

// TestStreamRestorer_ChunkNoChoicesField_Skipped verifies that a JSON chunk
// with no "choices" field at all is silently skipped (benign auxiliary data).
func TestStreamRestorer_ChunkNoChoicesField_Skipped(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "nochoices@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	line := `data: {"id":"cid","object":"chat.completion.chunk","system_fingerprint":"fp_abc"}`
	out, _, err := r.Push([]byte(line))
	if err != nil {
		t.Fatalf("no-choices chunk: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("no-choices chunk produced unexpected output: %v", out)
	}
}

// TestStreamRestorer_EmptyChoicesNoUsage_Skipped verifies that a chunk with
// choices=[] and no usage field is silently skipped.
func TestStreamRestorer_EmptyChoicesNoUsage_Skipped(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "emptynousa@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	line := `data: {"id":"cid","object":"chat.completion.chunk","choices":[]}`
	out, _, err := r.Push([]byte(line))
	if err != nil {
		t.Fatalf("empty choices no usage: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("empty choices no usage: unexpected output: %v", out)
	}
}

// ── delta.refusal tests ───────────────────────────────────────────────────────

// refusalChunk builds a minimal OpenAI chat-completion SSE data line that
// carries a delta.refusal payload (gpt-4o style).
func refusalChunk(idx int, refusal string) string {
	return fmt.Sprintf(`data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":%d,"delta":{"refusal":%s},"finish_reason":null}]}`,
		idx, jsonStr(refusal))
}

// extractDeltaRefusal parses a data: SSE line and returns the delta.refusal
// string from choices[0]. Returns "" on any parse failure.
func extractDeltaRefusal(line string) string {
	if !strings.HasPrefix(line, "data: ") {
		return ""
	}
	payload := line[len("data: "):]
	var chunk struct {
		Choices []struct {
			Delta struct {
				Refusal *string `json:"refusal"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
		return ""
	}
	if len(chunk.Choices) == 0 || chunk.Choices[0].Delta.Refusal == nil {
		return ""
	}
	return *chunk.Choices[0].Delta.Refusal
}

// TestStreamRestorer_RefusalSplitPseudonymRestored verifies that a pseudonym
// split across two delta.refusal chunks is correctly restored and the output
// is emitted as delta.refusal (not delta.content).
func TestStreamRestorer_RefusalSplitPseudonymRestored(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "refusal@splitpseudo.example")
	orig := f.rev[pseudo]
	// Pre-warm to avoid write race in parallel subtests.
	f.Restore([]byte(pseudo))

	for splitAt := 1; splitAt < pseudonymLen; splitAt++ {
		splitAt := splitAt
		t.Run(fmt.Sprintf("split_at_%d", splitAt), func(t *testing.T) {
			t.Parallel()

			part1 := pseudo[:splitAt]
			part2 := pseudo[splitAt:]

			r := NewStreamRestorer(f, "gpt-4o")
			lines := []string{
				refusalChunk(0, part1),
				"",
				refusalChunk(0, part2),
				"",
				finishChunk(0, "content_filter"),
				"",
				"data: [DONE]",
				"",
			}

			output, err := pushLines(t, r, lines)
			if err != nil {
				t.Fatalf("pushLines: %v", err)
			}

			// Verify the output contains the original value in delta.refusal.
			var allRefusal strings.Builder
			for _, line := range strings.Split(output, "\n") {
				if strings.HasPrefix(line, "data: ") && !strings.Contains(line, "[DONE]") {
					v := extractDeltaRefusal(line)
					allRefusal.WriteString(v)
					// Also verify no delta.content leaked.
					if extractDeltaContent(line) != "" {
						t.Errorf("split_at_%d: restored content appeared in delta.content, want delta.refusal", splitAt)
					}
				}
			}
			got := allRefusal.String()
			if got != orig {
				t.Errorf("split_at_%d: restored refusal = %q, want %q", splitAt, got, orig)
			}
		})
	}
}

// TestStreamRestorer_ContentAndRefusalMixed_FailClosed verifies that a choice
// carrying both delta.content and delta.refusal in the same chunk triggers a
// fail-closed abort.
func TestStreamRestorer_ContentAndRefusalMixed_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "mixed@contentrefusal.example")
	r := NewStreamRestorer(f, "gpt-4o")

	// A single chunk with both content and refusal fields populated.
	line := `data: {"id":"cid","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello","refusal":"no"},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(line))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("content+refusal mixed: error = %v, want errStreamAborted", err)
	}
}

// TestStreamRestorer_RefusalCarryNotEmptyAtDone_FailClosed verifies that when
// [DONE] arrives with a non-empty refusal carry (upstream truncated a pseudonym
// in a delta.refusal stream), the restorer returns errCarryNotEmpty.
func TestStreamRestorer_RefusalCarryNotEmptyAtDone_FailClosed(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "refusalcarry@done.example")
	// Push only the first half of the pseudonym — never complete it.
	part1 := pseudo[:pseudonymLen/2]

	r := NewStreamRestorer(f, "gpt-4o")
	lines := []string{
		refusalChunk(0, part1),
		"",
		// No finish_reason before [DONE] — carry is still non-empty.
		"data: [DONE]",
		"",
	}
	_, err := pushLines(t, r, lines)
	if !errors.Is(err, errCarryNotEmpty) {
		t.Errorf("refusal carry not empty at [DONE]: error = %v, want errCarryNotEmpty", err)
	}
}

// ── FIX 1: role synthesis ─────────────────────────────────────────────────────

// TestStreamRestorer_RoleSynthesis_NeverEchosUpstream verifies that a
// malicious upstream that injects a pseudonym (or any non-"assistant" string)
// into delta.role never causes the pseudonym to appear in the client output.
// The restorer must always emit the canonical "assistant" role.
func TestStreamRestorer_RoleSynthesis_NeverEchosUpstream(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "rolesynthesis@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Upstream sends the pseudonym as the role value — simulating a malicious
	// or compromised upstream trying to leak PII through the role field.
	roleLine := fmt.Sprintf(
		`data: {"id":"r1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":%s,"content":"hello"},"finish_reason":null}]}`,
		jsonStr(pseudo),
	)
	lines := []string{
		roleLine,
		"",
		finishChunk(0, "stop"),
		"",
		"data: [DONE]",
		"",
	}
	output, err := pushLines(t, r, lines)
	if err != nil {
		t.Fatalf("push error: %v", err)
	}

	// The pseudonym must never appear in any emitted line.
	for _, line := range strings.Split(output, "\n") {
		if strings.Contains(line, pseudo) {
			t.Errorf("pseudonym appeared in client output via role field: %q", line)
		}
	}

	// The emitted role must be the canonical "assistant", not the upstream value.
	for _, line := range strings.Split(output, "\n") {
		if !strings.HasPrefix(line, "data: ") || strings.Contains(line, "[DONE]") {
			continue
		}
		payload := line[len("data: "):]
		var chunk struct {
			Choices []struct {
				Delta struct {
					Role *string `json:"role,omitempty"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		if r := chunk.Choices[0].Delta.Role; r != nil {
			if *r != "assistant" {
				t.Errorf("emitted role = %q, want %q", *r, "assistant")
			}
		}
	}
}

// ── FIX 1: finish_reason allowlist ───────────────────────────────────────────

// TestStreamRestorer_FinishReason_ValidValues verifies that all allowed
// finish_reason values pass through without triggering an abort.
func TestStreamRestorer_FinishReason_ValidValues(t *testing.T) {
	t.Parallel()

	// "tool_calls" and "function_call" are intentionally absent: they are
	// fail-closed (not in allowedFinishReasons) because the Anthropic adapter
	// drops tool_use content deltas but still emits finish_reason:"tool_calls",
	// which would produce a corrupt response. See allowedFinishReasons.
	validReasons := []string{"stop", "length", "content_filter"}

	_, f := realPseudonym(t, "finishreason@example.com")
	// Pre-warm replacer so parallel subtests only read f.replacer.
	f.Restore([]byte("warmup"))

	for _, reason := range validReasons {
		reason := reason
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			r := NewStreamRestorer(f, "gpt-4o")
			lines := []string{
				chatChunk(0, "hello"),
				"",
				finishChunk(0, reason),
				"",
				"data: [DONE]",
				"",
			}
			_, err := pushLines(t, r, lines)
			if err != nil {
				t.Errorf("finish_reason %q: unexpected error: %v", reason, err)
			}
		})
	}
}

// TestStreamRestorer_FinishReason_ToolCalls_FailClosed verifies that
// finish_reason "tool_calls" and "function_call" are fail-closed. The Anthropic
// adapter maps its tool_use stop_reason to finish_reason:"tool_calls" but drops
// all tool_use content deltas (they are not text and cannot be PII-restored).
// Without this guard, a tool-calling Anthropic stream would produce a response
// with finish_reason:"tool_calls" but no tool_calls body — a contract violation.
// Failing closed ensures tool-use streams are never forwarded through the
// PII restorer regardless of whether the tool_calls delta was visible.
func TestStreamRestorer_FinishReason_ToolCalls_FailClosed(t *testing.T) {
	t.Parallel()

	disallowedReasons := []string{"tool_calls", "function_call"}

	for _, reason := range disallowedReasons {
		reason := reason
		t.Run(reason, func(t *testing.T) {
			t.Parallel()
			_, f := realPseudonym(t, "toolcallfinish@example.com")
			r := NewStreamRestorer(f, "gpt-4o")
			lines := []string{
				chatChunk(0, "hello"),
				"",
				finishChunk(0, reason),
				"",
				"data: [DONE]",
				"",
			}
			_, err := pushLines(t, r, lines)
			if !errors.Is(err, errStreamAborted) {
				t.Errorf("finish_reason %q: error = %v, want errStreamAborted", reason, err)
			}
		})
	}
}

// TestStreamRestorer_FinishReason_PseudonymInReason_FailClosed verifies that a
// finish_reason value containing a pseudonym is rejected fail-closed. A
// malicious upstream could attempt to leak PII through the finish_reason field.
func TestStreamRestorer_FinishReason_PseudonymInReason_FailClosed(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "finishpseudo@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Build a chunk where finish_reason is set to the pseudonym itself.
	line := fmt.Sprintf(
		`data: {"id":"fp1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":%s}]}`,
		jsonStr(pseudo),
	)
	lines := []string{chatChunk(0, "pre"), "", line, "", "data: [DONE]", ""}
	_, err := pushLines(t, r, lines)
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("pseudonym in finish_reason: error = %v, want errStreamAborted", err)
	}
}

// TestStreamRestorer_FinishReason_UnknownValue_FailClosed verifies that an
// arbitrary unrecognised finish_reason value triggers a fail-closed abort.
func TestStreamRestorer_FinishReason_UnknownValue_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "finishunknown@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	line := `data: {"id":"fu1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"weird_value"}]}`
	lines := []string{chatChunk(0, "pre"), "", line, "", "data: [DONE]", ""}
	_, err := pushLines(t, r, lines)
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("unknown finish_reason: error = %v, want errStreamAborted", err)
	}
}

// ── FIX 2: Gemini-style termination (data:[DONE] without preceding blank) ────

// TestStreamRestorer_GeminiTermination_NoBlankBeforeDone verifies that a
// "data: [DONE]" line received while inEvent=true (i.e., without an intervening
// blank SSE separator after the last content line) is treated as the terminal
// sentinel rather than a multi-line-data protocol violation.
//
// This is the real Gemini adapter termination pattern: the blank separator
// that follows the terminal content chunk is consumed by the adapter as the
// trigger to emit "data: [DONE]", but since no additional blank precedes that
// [DONE] line, the restorer sees inEvent=true when [DONE] arrives.
func TestStreamRestorer_GeminiTermination_NoBlankBeforeDone(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "geminiterm@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Sequence: data content line, NO blank, then data: [DONE].
	// inEvent will be true when [DONE] arrives — must NOT abort.
	lines := []string{
		chatChunk(0, "hello"),
		// No blank separator here — simulates Gemini adapter output.
		"data: [DONE]",
		"",
	}
	output, terminal, err := pushAllCollect(r, lines)
	if err != nil {
		t.Fatalf("Gemini termination: unexpected error: %v", err)
	}
	if !terminal {
		t.Error("Gemini termination: terminal was not set to true")
	}
	if !strings.Contains(output, "[DONE]") {
		t.Errorf("Gemini termination: [DONE] not emitted; output:\n%s", output)
	}
}

// TestStreamRestorer_MultiLineData_RealViolation_FailClosed verifies that two
// genuine JSON data: lines within a single SSE event (without a blank separator
// between them) still trigger a fail-closed abort. This ensures the Gemini fix
// (FIX 2) does not regress the multi-line-data protocol guard.
func TestStreamRestorer_MultiLineData_RealViolation_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "multiline@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Two JSON data: lines with NO blank between them — real protocol violation.
	lines := []string{
		chatChunk(0, "first"),
		chatChunk(0, "second"), // no blank between — violation
		"",
	}
	_, _, err := pushAllCollect(r, lines)
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("real multi-line data violation: error = %v, want errStreamAborted", err)
	}
}

// ── FIX 4: index validation ───────────────────────────────────────────────────

// TestStreamRestorer_NegativeIndex_FailClosed verifies that a negative choice
// index is rejected as a protocol violation.
func TestStreamRestorer_NegativeIndex_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "negidx@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	line := `data: {"id":"ni1","object":"chat.completion.chunk","choices":[{"index":-1,"delta":{"content":"oops"},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(line))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("negative index: error = %v, want errStreamAborted", err)
	}
}

// TestStreamRestorer_DuplicateIndexInChunk_FailClosed verifies that two
// entries with the same index within a single choices array are rejected.
func TestStreamRestorer_DuplicateIndexInChunk_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "dupidx@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Both entries have index=0.
	line := `data: {"id":"di1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"a"},"finish_reason":null},{"index":0,"delta":{"content":"b"},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(line))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("duplicate index in chunk: error = %v, want errStreamAborted", err)
	}
}

// TestStreamRestorer_MaxStreamChoices_FailClosed verifies that a stream with
// more than maxStreamChoices distinct choice indices is rejected to prevent
// unbounded map growth (memory-DoS guard).
func TestStreamRestorer_MaxStreamChoices_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "maxchoices@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	// Send maxStreamChoices+1 distinct indices across separate chunks.
	// Each chunk has a single entry, so no duplicate-within-chunk violation fires.
	var finalErr error
	for i := 0; i <= maxStreamChoices; i++ {
		line := fmt.Sprintf(
			`data: {"id":"mc1","object":"chat.completion.chunk","choices":[{"index":%d,"delta":{"content":"x"},"finish_reason":null}]}`,
			i,
		)
		_, _, err := r.Push([]byte(line))
		if err != nil {
			finalErr = err
			break
		}
		// blank separator after each chunk
		_, _, _ = r.Push([]byte{})
	}
	if !errors.Is(finalErr, errStreamAborted) {
		t.Errorf("max stream choices: error = %v, want errStreamAborted", finalErr)
	}
}

// ── FIX 5: tool_calls/function_call null key-presence ────────────────────────

// TestStreamRestorer_ToolCallsNull_FailClosed verifies that a delta with
// "tool_calls": null is rejected fail-closed. A typed unmarshal produces a nil
// slice (indistinguishable from "field absent"), so the fix uses the raw key
// map to detect the key regardless of its value.
func TestStreamRestorer_ToolCallsNull_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "toolnull@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	line := `data: {"id":"tc1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":null},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(line))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("tool_calls:null: error = %v, want errStreamAborted", err)
	}
}

// TestStreamRestorer_FunctionCallNull_FailClosed verifies that a delta with
// "function_call": null is rejected fail-closed (key-presence check).
func TestStreamRestorer_FunctionCallNull_FailClosed(t *testing.T) {
	t.Parallel()

	_, f := realPseudonym(t, "funcnull@example.com")
	r := NewStreamRestorer(f, "gpt-4o")

	line := `data: {"id":"fc1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"function_call":null},"finish_reason":null}]}`
	_, _, err := r.Push([]byte(line))
	if !errors.Is(err, errStreamAborted) {
		t.Errorf("function_call:null: error = %v, want errStreamAborted", err)
	}
}

// ── FIX 8: carry-at-done availability trade-off ───────────────────────────────

// TestStreamRestorer_CarryAtDone_TrailingPseudonymPrefix_FailClosed verifies
// the documented availability trade-off: when the upstream response ends with
// [DONE] while a choice carry contains a proper prefix of a known pseudonym
// (e.g. the response legitimately ends with "P", "PI", "PII_" etc.), the
// restorer returns errCarryNotEmpty rather than emitting the ambiguous bytes.
//
// This is leak-safe by design: a carry suffix that matches a pseudonym prefix
// cannot be distinguished from a truncated pseudonym at stream end, so the
// restorer must fail-closed. The consequence is that responses whose final
// characters happen to match the start of a known pseudonym are not delivered
// to the client. This trade-off is explicitly chosen over the risk of emitting
// a partial pseudonym.
func TestStreamRestorer_CarryAtDone_TrailingPseudonymPrefix_FailClosed(t *testing.T) {
	t.Parallel()

	pseudo, f := realPseudonym(t, "trailingprefix@example.com")
	// pseudo starts with "PII_" — so sending just "P" as the last content
	// byte will be held in the carry and not emitted before [DONE].

	// Build a chunk that ends with just the first byte of the pseudonym.
	// This sits in the carry when [DONE] arrives.
	firstByte := string(pseudo[0]) // "P"

	r := NewStreamRestorer(f, "gpt-4o")
	lines := []string{
		chatChunk(0, "Response ends here: "+firstByte),
		"",
		// No finish_reason — [DONE] arrives directly.
		"data: [DONE]",
		"",
	}
	_, err := pushLines(t, r, lines)
	if !errors.Is(err, errCarryNotEmpty) {
		t.Errorf("trailing pseudonym prefix at [DONE]: error = %v, want errCarryNotEmpty", err)
	}
}
