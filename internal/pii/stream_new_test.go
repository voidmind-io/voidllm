package pii

// stream_new_test.go covers the additional SSE parsing cases required by the
// feat/pii-anonymization-stage0a test task:
//
//  5. SSE "data:" without space (bare colon) is parsed correctly; mixed with/without
//  6. Unparseable data line → fail-closed

import (
	"strings"
	"testing"
)

// ── 5. SSE "data:" without space ──────────────────────────────────────────────

// TestRestoreSSEStream_DataPrefixNoSpace verifies that a data: line without
// the optional space (i.e., "data:{...}" rather than "data: {...}") is parsed
// and restored correctly, not passed through verbatim.
//
// Per RFC 6202 §4.2, the space after "data:" is optional. The stream.go
// implementation explicitly strips the optional space via:
//
//	payload := line[len(dataLinePrefix):]
//	if len(payload) > 0 && payload[0] == ' ' { payload = payload[1:] }
//
// A line that begins with "data:" (no space) must be treated as a data line,
// not a non-data line.
func TestRestoreSSEStream_DataPrefixNoSpace(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"email: nospace@example.com"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if !f.Touched() {
		t.Fatal("Touched() = false; expected email to be detected")
	}

	pseudo := ""
	for p, o := range f.rev {
		if o == "nospace@example.com" {
			pseudo = p
			break
		}
	}
	if pseudo == "" {
		t.Fatal("pseudonym not found in rev map")
	}

	// Construct a data: line WITHOUT the space after the colon.
	// The payload is a valid JSON SSE chunk with the pseudonym in delta.content.
	eventNoSpace := `data:{"id":"ns1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + pseudo + `"},"finish_reason":null}]}`

	lines := makeSSELines(eventNoSpace, "", "data: [DONE]", "")

	out, err := RestoreSSEStream(lines, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: unexpected error: %v", err)
	}

	output := outputStr(out)

	// The pseudonym must NOT appear in the output (it was restored).
	if strings.Contains(output, pseudo) {
		t.Errorf("pseudonym %q still visible in output; expected it to be restored\noutput:\n%s", pseudo, output)
	}

	// The original email must appear in the output.
	if !strings.Contains(output, "nospace@example.com") {
		t.Errorf("original email missing from output\noutput:\n%s", output)
	}

	// Output must contain valid SSE structure.
	if !strings.Contains(output, "data: ") {
		t.Errorf("output missing data: prefix\noutput:\n%s", output)
	}
	if !strings.Contains(output, "[DONE]") {
		t.Errorf("output missing [DONE]\noutput:\n%s", output)
	}
}

// TestRestoreSSEStream_MixedDataPrefixSpacing verifies that a stream containing
// a mix of "data: " (with space) and "data:" (without space) lines is processed
// correctly. Each data line must be parsed as a data event regardless of whether
// the optional space is present.
func TestRestoreSSEStream_MixedDataPrefixSpacing(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"contact mix@example.com"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if !f.Touched() {
		t.Fatal("Touched() = false; expected email to be detected")
	}

	pseudo := ""
	origEmail := ""
	for p, o := range f.rev {
		pseudo = p
		origEmail = o
		break
	}
	if pseudo == "" {
		t.Fatal("no pseudonym found")
	}

	// Build a stream where the pseudonym is split across two chunks:
	// one emitted with "data: " (with space) and one with "data:" (no space).
	// This exercises the path where both variants coexist and must both be
	// parsed correctly for the assembled-content restore to succeed.
	mid := len(pseudo) / 2
	part1 := pseudo[:mid]
	part2 := pseudo[mid:]

	// Chunk 1: "data: " with space (standard).
	eventWithSpace := `data: {"id":"mix1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + part1 + `"},"finish_reason":null}]}`
	// Chunk 2: "data:" without space.
	eventNoSpace := `data:{"id":"mix1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + part2 + `"},"finish_reason":null}]}`
	done := `data: [DONE]`

	lines := makeSSELines(eventWithSpace, "", eventNoSpace, "", done, "")

	out, err := RestoreSSEStream(lines, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: unexpected error on mixed-spacing stream: %v", err)
	}

	output := outputStr(out)

	// The original email must appear in the output.
	if !strings.Contains(output, origEmail) {
		t.Errorf("original email %q missing from mixed-spacing output\noutput:\n%s", origEmail, output)
	}

	// Neither the full pseudonym nor either fragment must remain.
	if strings.Contains(output, pseudo) {
		t.Errorf("full pseudonym still visible\noutput:\n%s", output)
	}
	if strings.Contains(output, part1) || strings.Contains(output, part2) {
		t.Errorf("pseudonym fragment still visible\noutput:\n%s", output)
	}
}

// ── 6. Unparseable data line → fail-closed ────────────────────────────────────

// TestRestoreSSEStream_UnparseableDataLine_FailClosed verifies that a data: line
// whose payload is not [DONE] and not valid JSON causes RestoreSSEStream to
// return an error rather than emitting any content to the client.
//
// This covers the "fail-closed on unparseable data" contract described in the
// RestoreSSEStream documentation.
func TestRestoreSSEStream_UnparseableDataLine_FailClosed(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	tests := []struct {
		name string
		line string
	}{
		{
			name: "broken json with space prefix",
			line: `data: {broken json`,
		},
		{
			name: "broken json without space prefix",
			line: `data:{broken json`,
		},
		{
			name: "data line is plain text not json",
			line: `data: this is not json at all`,
		},
		{
			name: "data line is empty object fragment",
			line: `data: {`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lines := makeSSELines(tc.line, "")
			_, err := RestoreSSEStream(lines, f.Restore)
			if err == nil {
				t.Errorf("[%s] expected fail-closed error for unparseable data line, got nil", tc.name)
			}
		})
	}
}

// TestRestoreSSEStream_UnparseableDataLine_NoContentEmitted verifies that when
// RestoreSSEStream returns an error due to an unparseable line, no content is
// returned in the output slice (the returned slice should be nil/empty).
func TestRestoreSSEStream_UnparseableDataLine_NoContentEmitted(t *testing.T) {
	t.Parallel()

	// A stream with a valid first chunk followed by a broken chunk.
	// The broken chunk must cause the entire stream processing to fail.
	f2 := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"email: fail@closed.example"}]}`)
	_, err := f2.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	pseudo := ""
	for p := range f2.rev {
		pseudo = p
		break
	}

	// Valid JSON chunk, then broken chunk.
	validLine := `data: {"id":"fc1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + pseudo + `"},"finish_reason":null}]}`
	brokenLine := `data: {not json`

	lines := makeSSELines(validLine, "", brokenLine, "")
	result, err := RestoreSSEStream(lines, f2.Restore)
	if err == nil {
		t.Error("expected fail-closed error when stream contains broken JSON, got nil")
	}
	// When err != nil, result must be nil (no partial content emitted).
	if result != nil {
		t.Errorf("result must be nil when err != nil; got %d lines", len(result))
	}
}

// TestRestoreSSEStream_DataNoSpaceOnlyDone verifies that a minimal stream
// containing only a "data:[DONE]" sentinel (no space) is handled gracefully
// and the [DONE] sentinel is preserved in the output.
func TestRestoreSSEStream_DataNoSpaceOnlyDone(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)

	// "data:[DONE]" without space — the payload is "[DONE]".
	// This must be treated as the stream sentinel, not as broken JSON.
	lines := [][]byte{[]byte("data:[DONE]"), {}}

	out, err := RestoreSSEStream(lines, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: unexpected error on bare data:[DONE]: %v", err)
	}
	output := outputStr(out)
	if !strings.Contains(output, "[DONE]") {
		t.Errorf("output missing [DONE] sentinel\noutput:\n%s", output)
	}
}

// ── FIX 3: envelope and non-data lines restored ───────────────────────────────

// TestRestoreSSEStream_PseudonymInEnvelopeField verifies that a pseudonym
// echo-ed by the upstream in an envelope field (e.g. "model" or
// "system_fingerprint") is removed from the synthesized output. This covers the
// belt-and-suspenders final restore pass added to RestoreSSEStream.
func TestRestoreSSEStream_PseudonymInEnvelopeField(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	// Anonymize a body to get a deterministic pseudonym for an email.
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"id: env@example.com"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if !f.Touched() {
		t.Fatal("Touched() = false; expected email to be detected")
	}

	pseudo := ""
	origEmail := ""
	for p, o := range f.rev {
		pseudo = p
		origEmail = o
		break
	}
	if pseudo == "" {
		t.Fatal("no pseudonym found")
	}

	// Upstream echoes the pseudonym in the "model" envelope field.
	// The content delta carries clean text (no pseudonym in content).
	eventWithPseudoInModel := `data: {"id":"ep1","object":"chat.completion.chunk","model":"` + pseudo + `","choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}`

	lines := makeSSELines(eventWithPseudoInModel, "", "data: [DONE]", "")

	out, err := RestoreSSEStream(lines, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: unexpected error: %v", err)
	}

	output := outputStr(out)

	// The raw pseudonym must NOT appear anywhere in the output (belt-and-suspenders).
	if strings.Contains(output, pseudo) {
		t.Errorf("FIX 3: pseudonym %q still visible in output after envelope restore\noutput:\n%s",
			pseudo, output)
	}

	// The original email must appear in the output (restored from envelope).
	if !strings.Contains(output, origEmail) {
		t.Errorf("FIX 3: original email %q missing from output\noutput:\n%s", origEmail, output)
	}

	// The clean content must still be present.
	if !strings.Contains(output, "hello") {
		t.Errorf("FIX 3: clean content 'hello' missing from output\noutput:\n%s", output)
	}
}

// TestRestoreSSEStream_PseudonymInNonDataLine verifies that a pseudonym in a
// non-data SSE line (e.g. an SSE comment line starting with ":") is removed
// from the output by the restore pass applied to non-data lines.
func TestRestoreSSEStream_PseudonymInNonDataLine(t *testing.T) {
	t.Parallel()

	f := newTestFilter(t)
	body := []byte(`{"model":"gpt-4","messages":[{"role":"user","content":"id: nd@example.com"}]}`)
	_, err := f.AnonymizeJSON(body)
	if err != nil {
		t.Fatalf("AnonymizeJSON: %v", err)
	}
	if !f.Touched() {
		t.Fatal("Touched() = false")
	}

	pseudo := ""
	origEmail := ""
	for p, o := range f.rev {
		pseudo = p
		origEmail = o
		break
	}
	if pseudo == "" {
		t.Fatal("no pseudonym found")
	}

	// An SSE comment line carrying the pseudonym (e.g. upstream debug info).
	commentLine := ": debug " + pseudo
	contentEvent := `data: {"id":"nd1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`

	lines := makeSSELines(commentLine, contentEvent, "", "data: [DONE]", "")

	out, err := RestoreSSEStream(lines, f.Restore)
	if err != nil {
		t.Fatalf("RestoreSSEStream: unexpected error: %v", err)
	}

	output := outputStr(out)

	// Pseudonym must not appear anywhere in the output.
	if strings.Contains(output, pseudo) {
		t.Errorf("FIX 3: pseudonym %q still visible in non-data line output\noutput:\n%s",
			pseudo, output)
	}

	// Original email must appear (restored from the comment line).
	if !strings.Contains(output, origEmail) {
		t.Errorf("FIX 3: original email %q missing from output\noutput:\n%s", origEmail, output)
	}
}
