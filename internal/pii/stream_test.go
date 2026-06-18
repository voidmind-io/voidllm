package pii

// stream_test.go is in package pii (white-box) so it can construct a *Filter
// directly via newTestFilter and call RestoreSSEStream with filter.Restore.

import (
	"bytes"
	"strings"
	"testing"
)

// ── helpers ───────────────────────────────────────────────────────────────────

// sseLines converts a variadic list of raw SSE line strings into the [][]byte
// slice expected by RestoreSSEStream. Blank separator lines are represented as
// empty strings and converted to nil (matching the output convention of
// RestoreSSEStream but accepting them as input too — RestoreSSEStream treats
// non-data: lines as nonDataLines).
func makeSSELines(lines ...string) [][]byte {
	out := make([][]byte, 0, len(lines))
	for _, l := range lines {
		if l == "" {
			out = append(out, []byte{}) // blank line (non-data)
		} else {
			out = append(out, []byte(l))
		}
	}
	return out
}

// outputStr joins the output lines of RestoreSSEStream into a single string
// for assertion. nil elements (blank separators) become a bare newline.
func outputStr(lines [][]byte) string {
	var b strings.Builder
	for _, l := range lines {
		if l == nil {
			b.WriteByte('\n')
		} else {
			b.Write(l)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// ── Core bug: split pseudonym across two SSE events ──────────────────────────

// TestRestoreSSEStream_SplitPseudonym is the principal regression test for the
// streaming-leak bug described in the issue. It constructs an upstream SSE
// response in which a deterministic pseudonym (produced by the same filter used
// on the request) has been split across two consecutive SSE events by the LLM
// tokenizer. The first event carries the first half as delta.content and the
// second event carries the second half.
//
// The Stage-0a raw-byte-join approach fails here: when the two JSON objects are
// concatenated as raw bytes the pseudonym appears as
//
//	...{"content":"PII_EM_1bd7"}...{"content":"5caa..."}...
//
// which never contains the full token as a contiguous substring, so
// strings.Replacer cannot find it.
//
// RestoreSSEStream must reassemble the content per choice index before calling
// Restore, at which point the complete pseudonym appears as a contiguous string
// and the replacement succeeds.
func TestRestoreSSEStream_SplitPseudonym(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	// Anonymize a body containing a known email so we can derive the pseudonym.
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"email: test@split.example"}]}`)
	anonBody, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if !f.Touched() {
		t.Fatal("Touched() = false; filter did not detect PII in test body")
	}

	// Extract the pseudonym that was assigned to the email.
	pseudo := ""
	for p, orig := range f.rev {
		if orig == "test@split.example" {
			pseudo = p
			break
		}
	}
	if pseudo == "" {
		t.Fatalf("could not find pseudonym in filter rev map; anonBody: %s", anonBody)
	}

	// Verify the pseudonym is at least two characters so we can split it.
	if len(pseudo) < 2 {
		t.Fatalf("pseudonym too short to split: %q", pseudo)
	}

	// Split the pseudonym at the midpoint.
	mid := len(pseudo) / 2
	part1 := pseudo[:mid]
	part2 := pseudo[mid:]

	// Build upstream SSE lines: the pseudonym is split across event 1 and event 2.
	// This exactly reproduces the bug: the LLM tokenizes the pseudonym token and
	// emits the first half in one chunk and the second half in the next.
	event1 := `data: {"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + part1 + `"},"finish_reason":null}]}`
	event2 := `data: {"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + part2 + `"},"finish_reason":null}]}`
	eventDone := `data: {"id":"chatcmpl-x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

	sseInput := makeSSELines(event1, "", event2, "", eventDone, "", "data: [DONE]", "")

	restoredLines, err := RestoreSSEStream(sseInput, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: unexpected error: %v", err)
	}

	output := outputStr(restoredLines)

	// Assert 1: the output contains the original email (pseudonym was restored).
	if !strings.Contains(output, "test@split.example") {
		t.Errorf("split-pseudonym restore failed: output does not contain original email %q\noutput:\n%s", "test@split.example", output)
	}

	// Assert 2: the output does NOT contain the raw pseudonym or either half.
	if strings.Contains(output, pseudo) {
		t.Errorf("output still contains full raw pseudonym %q (should have been restored)\noutput:\n%s", pseudo, output)
	}
	if strings.Contains(output, part1) || strings.Contains(output, part2) {
		t.Errorf("output still contains pseudonym fragment (part1=%q part2=%q)\noutput:\n%s", part1, part2, output)
	}

	// Assert 3: output is valid SSE (contains data: and [DONE]).
	if !strings.Contains(output, "data: ") {
		t.Errorf("output missing SSE data: prefix\noutput:\n%s", output)
	}
	if !strings.Contains(output, "[DONE]") {
		t.Errorf("output missing [DONE] sentinel\noutput:\n%s", output)
	}
}

// TestRestoreSSEStream_SingleChunkPseudonym verifies the straightforward case
// where the pseudonym appears in a single SSE event (no split). RestoreSSEStream
// must restore it correctly in the output.
func TestRestoreSSEStream_SingleChunkPseudonym(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"IBAN DE12345678901234567890"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if !f.Touched() {
		t.Fatal("Touched() = false; expected IBAN to be detected")
	}

	pseudo := ""
	orig := ""
	for p, o := range f.rev {
		pseudo = p
		orig = o
		break
	}

	event := `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"Your IBAN is ` + pseudo + ` confirmed"},"finish_reason":null}]}`
	done := `data: [DONE]`

	lines := makeSSELines(event, "", done, "")

	out, err := RestoreSSEStream(lines, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: %v", err)
	}

	output := outputStr(out)

	if !strings.Contains(output, orig) {
		t.Errorf("output missing original value %q\noutput:\n%s", orig, output)
	}
	if strings.Contains(output, pseudo) {
		t.Errorf("output still contains pseudonym %q\noutput:\n%s", pseudo, output)
	}
}

// TestRestoreSSEStream_NoPII verifies that when filter.Restore is a no-op
// (no PII was anonymized), RestoreSSEStream returns valid SSE output with the
// original content unchanged.
func TestRestoreSSEStream_NoPII(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	// No AnonymizeJSON call — rev map is empty; Restore is a no-op.

	event := `data: {"id":"y","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hello world"},"finish_reason":null}]}`
	done := `data: [DONE]`

	lines := makeSSELines(event, "", done, "")

	out, err := RestoreSSEStream(lines, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: %v", err)
	}

	output := outputStr(out)

	if !strings.Contains(output, "hello world") {
		t.Errorf("content missing from output\noutput:\n%s", output)
	}
	if !strings.Contains(output, "[DONE]") {
		t.Errorf("missing [DONE]\noutput:\n%s", output)
	}
}

// TestRestoreSSEStream_MultipleChoices verifies that n>1 choices are each
// restored independently. Each choice's content is assembled separately and
// restore is applied per-choice.
func TestRestoreSSEStream_MultipleChoices(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"contact alice@example.com and bob@example.com"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if !f.Touched() {
		t.Fatal("Touched() = false")
	}

	// Find the pseudonyms for alice and bob.
	alicePseudo := ""
	bobPseudo := ""
	for p, o := range f.rev {
		if o == "alice@example.com" {
			alicePseudo = p
		}
		if o == "bob@example.com" {
			bobPseudo = p
		}
	}
	if alicePseudo == "" || bobPseudo == "" {
		t.Fatalf("pseudonyms not found: alice=%q bob=%q", alicePseudo, bobPseudo)
	}

	// Two choices in the same SSE event, each with a different pseudonym.
	event := `data: {"id":"z","object":"chat.completion.chunk","choices":[` +
		`{"index":0,"delta":{"content":"` + alicePseudo + `"},"finish_reason":null},` +
		`{"index":1,"delta":{"content":"` + bobPseudo + `"},"finish_reason":null}]}`

	lines := makeSSELines(event, "", "data: [DONE]", "")

	out, err := RestoreSSEStream(lines, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: %v", err)
	}

	output := outputStr(out)

	if !strings.Contains(output, "alice@example.com") {
		t.Errorf("output missing alice@example.com\noutput:\n%s", output)
	}
	if !strings.Contains(output, "bob@example.com") {
		t.Errorf("output missing bob@example.com\noutput:\n%s", output)
	}
	if strings.Contains(output, alicePseudo) {
		t.Errorf("output still contains alice pseudonym %q\noutput:\n%s", alicePseudo, output)
	}
	if strings.Contains(output, bobPseudo) {
		t.Errorf("output still contains bob pseudonym %q\noutput:\n%s", bobPseudo, output)
	}
}

// TestRestoreSSEStream_ToolCallsFailClosed verifies the fail-closed contract
// for tool_calls streaming: if any choice carries tool_calls deltas,
// RestoreSSEStream must return an error rather than risk leaking un-restored
// content (split tool-call argument JSON cannot be safely reassembled).
func TestRestoreSSEStream_ToolCallsFailClosed(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	// No PII setup needed — we're testing structural fail-closed behaviour.

	event := `data: {"id":"tc1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"search","arguments":""}}]},"finish_reason":null}]}`

	lines := makeSSELines(event, "", "data: [DONE]", "")

	_, err := RestoreSSEStream(lines, f.Restore)
	if err == nil {
		t.Error("RestoreSSEStream returned nil error for tool_calls stream; expected fail-closed error")
	}
}

// TestRestoreSSEStream_InvalidJSON verifies fail-closed behaviour when a data:
// line carries a non-[DONE] payload that is not valid JSON.
func TestRestoreSSEStream_InvalidJSON(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	lines := makeSSELines(`data: {broken json`, "")

	_, err := RestoreSSEStream(lines, f.Restore)
	if err == nil {
		t.Error("RestoreSSEStream returned nil error for invalid JSON payload; expected fail-closed error")
	}
}

// TestRestoreSSEStream_EmptyStream verifies that an SSE stream with no data
// chunks (only a [DONE] line) is handled gracefully and emits a valid [DONE].
func TestRestoreSSEStream_EmptyStream(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	lines := makeSSELines("data: [DONE]", "")

	out, err := RestoreSSEStream(lines, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: %v", err)
	}

	output := outputStr(out)
	if !strings.Contains(output, "[DONE]") {
		t.Errorf("output missing [DONE]\noutput:\n%s", output)
	}
}

// TestRestoreSSEStream_FinishReasonPreserved verifies that the finish_reason
// field seen in the upstream stream is preserved in the synthesized output.
func TestRestoreSSEStream_FinishReasonPreserved(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	// Minimal setup: no PII, just checking finish_reason is forwarded.
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"email: x@example.com"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}

	pseudo := ""
	for p := range f.rev {
		pseudo = p
		break
	}
	if pseudo == "" {
		t.Fatal("no pseudonym found")
	}

	contentEvent := `data: {"id":"fr1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + pseudo + `"},"finish_reason":null}]}`
	stopEvent := `data: {"id":"fr1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

	lines := makeSSELines(contentEvent, "", stopEvent, "", "data: [DONE]", "")

	out, err := RestoreSSEStream(lines, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: %v", err)
	}

	output := outputStr(out)

	if !strings.Contains(output, `"finish_reason":"stop"`) {
		t.Errorf("finish_reason:stop missing from output\noutput:\n%s", output)
	}
}

// TestRestoreSSEStream_SplitPseudonymRawJoinFails demonstrates WHY the old
// raw-byte-join approach fails on a split pseudonym. This test runs the old
// approach and asserts that it produces incorrect output (raw pseudonym fragments
// visible), confirming the bug that RestoreSSEStream fixes. If this test ever
// starts passing it means the old approach happened to work by accident (e.g.
// because the pseudonym was emitted whole) — in that case the test conditions
// must be re-verified.
func TestRestoreSSEStream_SplitPseudonymRawJoinFails(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"email: raw@joinbug.example"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if !f.Touched() {
		t.Fatal("Touched() = false")
	}

	pseudo := ""
	for p, o := range f.rev {
		if o == "raw@joinbug.example" {
			pseudo = p
			break
		}
	}
	if pseudo == "" {
		t.Fatal("no pseudonym found")
	}
	if len(pseudo) < 2 {
		t.Skipf("pseudonym too short to split: %q", pseudo)
	}

	mid := len(pseudo) / 2
	part1 := pseudo[:mid]
	part2 := pseudo[mid:]

	event1 := `data: {"choices":[{"index":0,"delta":{"content":"` + part1 + `"}}]}`
	event2 := `data: {"choices":[{"index":0,"delta":{"content":"` + part2 + `"}}]}`

	lines := [][]byte{[]byte(event1), []byte(event2)}

	// Simulate the old raw-byte-join approach.
	var joined []byte
	for _, l := range lines {
		joined = append(joined, l...)
		joined = append(joined, '\n')
	}
	rawRestored := f.Restore(joined)

	// The old approach FAILS: the full pseudonym never appears as a contiguous
	// substring in the joined buffer (it is interrupted by JSON field closing
	// and opening), so Restore cannot find it, and fragments remain.
	//
	// If this assertion fails it means the pseudonym happened to survive the
	// join intact (e.g. the test data changed) — re-split more carefully.
	if !bytes.Contains(rawRestored, []byte(part1)) && !bytes.Contains(rawRestored, []byte(part2)) {
		t.Skip("raw-join happened to restore correctly for this pseudonym; test conditions need re-verification")
	}
	// At least one fragment must remain visible, OR the full pseudonym must
	// still be present (not replaced), demonstrating the limitation.
	if !bytes.Contains(rawRestored, []byte(pseudo)) &&
		!bytes.Contains(rawRestored, []byte(part1)) &&
		!bytes.Contains(rawRestored, []byte(part2)) {
		t.Skip("no fragment found; raw-join may have accidentally succeeded")
	}

	// Now confirm RestoreSSEStream correctly restores the split pseudonym.
	sseInput := makeSSELines(event1, "", event2, "", "data: [DONE]", "")
	out, err := RestoreSSEStream(sseInput, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: %v", err)
	}
	output := outputStr(out)

	if !strings.Contains(output, "raw@joinbug.example") {
		t.Errorf("RestoreSSEStream did not restore split pseudonym; output:\n%s", output)
	}
	if strings.Contains(output, pseudo) || strings.Contains(output, part1) || strings.Contains(output, part2) {
		t.Errorf("RestoreSSEStream output still contains pseudonym or fragment\noutput:\n%s", output)
	}
}

// TestRestoreSSEStream_CompletionsTextField verifies that the legacy completions
// streaming format (choices[i].text instead of choices[i].delta.content) is
// handled correctly.
func TestRestoreSSEStream_CompletionsTextField(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-3.5-turbo-instruct","prompt":"email: comp@example.com"}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if !f.Touched() {
		t.Fatal("Touched() = false")
	}

	pseudo := ""
	for p, o := range f.rev {
		if o == "comp@example.com" {
			pseudo = p
			break
		}
	}
	if pseudo == "" {
		t.Fatal("no pseudonym found")
	}

	// Completions streaming: text at choice level, not inside delta.
	event := `data: {"id":"comp1","object":"text_completion.chunk","choices":[{"index":0,"text":"` + pseudo + `","finish_reason":null}]}`

	lines := makeSSELines(event, "", "data: [DONE]", "")

	out, err := RestoreSSEStream(lines, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: %v", err)
	}

	output := outputStr(out)

	if !strings.Contains(output, "comp@example.com") {
		t.Errorf("output missing restored email\noutput:\n%s", output)
	}
	if strings.Contains(output, pseudo) {
		t.Errorf("output still contains pseudonym %q\noutput:\n%s", pseudo, output)
	}
}
