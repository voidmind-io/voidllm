package mcp_test

import (
	"bytes"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers sseEventReader (sse_reader.go) — the incremental,
// live-connection SSE parser HTTPTransport.Listen uses for a long-lived
// subscriptions/listen stream — independent of extractSSEResult's own
// fully-buffered path (already covered elsewhere in this package). Every
// test here drives mcp.NewSSEEventReaderForTest directly against a plain
// io.Reader, with no HTTP transport involved at all.

// ---- classifySSELine -------------------------------------------------------

func TestClassifySSELine(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		line      string
		wantKind  int
		wantValue string
	}{
		{"blank", "", mcp.SSELineKindBlank, ""},
		{"comment", ": keep-alive", mcp.SSELineKindComment, ""},
		{"comment_empty", ":", mcp.SSELineKindComment, ""},
		{"data_with_space", "data: hello", mcp.SSELineKindData, "hello"},
		{"data_without_space", "data:hello", mcp.SSELineKindData, "hello"},
		{"data_empty", "data:", mcp.SSELineKindData, ""},
		{"data_empty_with_space", "data: ", mcp.SSELineKindData, ""},
		{"id_with_space", "id: 42", mcp.SSELineKindID, "42"},
		{"id_without_space", "id:42", mcp.SSELineKindID, "42"},
		{"event_with_space", "event: message", mcp.SSELineKindEvent, "message"},
		{"event_without_space", "event:message", mcp.SSELineKindEvent, "message"},
		{"retry_ignored", "retry: 3000", mcp.SSELineKindOther, ""},
		{"unrecognized_field", "foo: bar", mcp.SSELineKindOther, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			kind, value := mcp.ClassifySSELineForTest([]byte(tc.line))
			if kind != tc.wantKind {
				t.Errorf("classifySSELine(%q) kind = %d, want %d", tc.line, kind, tc.wantKind)
			}
			if string(value) != tc.wantValue {
				t.Errorf("classifySSELine(%q) value = %q, want %q", tc.line, value, tc.wantValue)
			}
		})
	}
}

// ---- splitSSEBody: CRLF/CR/LF normalization --------------------------------

func TestSplitSSEBody_LineEndingNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
		want []string
	}{
		{"lf", "a\nb\nc", []string{"a", "b", "c"}},
		{"crlf", "a\r\nb\r\nc", []string{"a", "b", "c"}},
		{"bare_cr", "a\rb\rc", []string{"a", "b", "c"}},
		{"mixed", "a\r\nb\rc\nd", []string{"a", "b", "c", "d"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := mcp.SplitSSEBodyForTest([]byte(tc.body))
			if len(got) != len(tc.want) {
				t.Fatalf("splitSSEBody(%q) = %q (len %d), want %q (len %d)", tc.body, got, len(got), tc.want, len(tc.want))
			}
			for i, line := range got {
				if string(line) != tc.want[i] {
					t.Errorf("splitSSEBody(%q)[%d] = %q, want %q", tc.body, i, line, tc.want[i])
				}
			}
		})
	}
}

// ---- sseEventReader.Next: framing -------------------------------------------

// TestSSEEventReader_MultiLineData_JoinedWithNewline covers the WHATWG rule
// (see extractSSEResult's own doc for the citation this package relies on)
// that several consecutive "data:" lines within one event are reassembled by
// joining them with "\n" before the event is dispatched.
func TestSSEEventReader_MultiLineData_JoinedWithNewline(t *testing.T) {
	t.Parallel()

	const stream = "data: line one\n" +
		"data: line two\n" +
		"data: line three\n" +
		"\n"

	r := mcp.NewSSEEventReaderForTest(strings.NewReader(stream))
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next() error = %v, want nil", err)
	}
	want := "line one\nline two\nline three"
	if string(ev.Data) != want {
		t.Errorf("Data = %q, want %q", ev.Data, want)
	}
}

// TestSSEEventReader_CommentLinesIgnored verifies that ":"-prefixed
// keep-alive comment lines never contribute to an event's data and never
// cause a spurious dispatch on their own (WHATWG: "if the data buffer is an
// empty string, ... return").
func TestSSEEventReader_CommentLinesIgnored(t *testing.T) {
	t.Parallel()

	const stream = ": keep-alive\n" +
		":\n" +
		"data: real payload\n" +
		"\n"

	r := mcp.NewSSEEventReaderForTest(strings.NewReader(stream))
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next() error = %v, want nil", err)
	}
	if string(ev.Data) != "real payload" {
		t.Errorf("Data = %q, want %q", ev.Data, "real payload")
	}
}

// TestSSEEventReader_LineEndings_CRLF_CR_LF_AllRecognized drives one event
// per line-ending style through the same reader instance, proving readLine
// tolerates \n, \r\n, and a bare \r interchangeably, exactly like
// splitSSEBody's fully-buffered counterpart.
func TestSSEEventReader_LineEndings_CRLF_CR_LF_AllRecognized(t *testing.T) {
	t.Parallel()

	const stream = "data: lf-event\n\n" +
		"data: crlf-event\r\n\r\n" +
		"data: cr-event\r\r"

	r := mcp.NewSSEEventReaderForTest(strings.NewReader(stream))

	for _, want := range []string{"lf-event", "crlf-event", "cr-event"} {
		ev, err := r.Next()
		if err != nil {
			t.Fatalf("Next() error = %v, want nil (event %q)", err, want)
		}
		if string(ev.Data) != want {
			t.Errorf("Data = %q, want %q", ev.Data, want)
		}
	}
}

// TestSSEEventReader_IDAndEventFieldsTracked verifies the WHATWG-specified
// stickiness of "id:" (persists across events until a later "id:" line
// changes it) and reset of "event:" (defaults to "" again after every
// dispatch unless the next event sets its own).
func TestSSEEventReader_IDAndEventFieldsTracked(t *testing.T) {
	t.Parallel()

	const stream = "id: 1\n" +
		"event: custom\n" +
		"data: first\n" +
		"\n" +
		"data: second\n" +
		"\n" +
		"id: 2\n" +
		"data: third\n" +
		"\n"

	r := mcp.NewSSEEventReaderForTest(strings.NewReader(stream))

	ev1, err := r.Next()
	if err != nil {
		t.Fatalf("Next() #1 error = %v", err)
	}
	if ev1.ID != "1" || ev1.Event != "custom" || string(ev1.Data) != "first" {
		t.Errorf("ev1 = %+v, want ID=1 Event=custom Data=first", ev1)
	}

	ev2, err := r.Next()
	if err != nil {
		t.Fatalf("Next() #2 error = %v", err)
	}
	// id sticky (still "1"), event reset to "" since this event set none.
	if ev2.ID != "1" || ev2.Event != "" || string(ev2.Data) != "second" {
		t.Errorf("ev2 = %+v, want ID=1 Event=\"\" Data=second", ev2)
	}

	ev3, err := r.Next()
	if err != nil {
		t.Fatalf("Next() #3 error = %v", err)
	}
	if ev3.ID != "2" || string(ev3.Data) != "third" {
		t.Errorf("ev3 = %+v, want ID=2 Data=third", ev3)
	}
}

// ---- sseEventReader.Next: split across separate underlying writes ---------

// TestSSEEventReader_EventSplitAcrossWrites_MidLine is the direct regression
// test for the incremental parser's whole reason to exist: a single SSE
// line's bytes arriving in two separate underlying Read calls (as they would
// over a real TCP connection whose write is flushed mid-line) must still be
// reassembled into one line, and one event, exactly as if they had arrived
// in a single Read. io.Pipe provides the synchronization: Write blocks until
// a Read has consumed the bytes, so the second Write below is only ever
// attempted once the first chunk has genuinely been handed to the reader —
// no sleep involved.
func TestSSEEventReader_EventSplitAcrossWrites_MidLine(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	r := mcp.NewSSEEventReaderForTest(pr)

	evCh := make(chan struct {
		data []byte
		err  error
	}, 1)
	go func() {
		ev, err := r.Next()
		evCh <- struct {
			data []byte
			err  error
		}{ev.Data, err}
	}()

	// First chunk: a partial "data:" line, deliberately cut mid-word, with no
	// terminator at all yet.
	if _, err := pw.Write([]byte("data: hel")); err != nil {
		t.Fatalf("pw.Write #1: %v", err)
	}
	// Second chunk: the rest of the line, its terminator, and the blank line
	// that dispatches the event — written only now that the pipe's
	// synchronous rendezvous guarantees the first chunk was already read.
	if _, err := pw.Write([]byte("lo\n\n")); err != nil {
		t.Fatalf("pw.Write #2: %v", err)
	}

	got := <-evCh
	if got.err != nil {
		t.Fatalf("Next() error = %v, want nil", got.err)
	}
	if string(got.data) != "hello" {
		t.Errorf("Data = %q, want %q (the line must be reassembled across the split write)", got.data, "hello")
	}
}

// TestSSEEventReader_EventSplitAcrossWrites_MidTerminator further stresses
// the split boundary by cutting exactly between a "\r" and its paired "\n"
// — readLine's own \r\n-pairing peek must survive the second byte arriving
// in a later Read.
func TestSSEEventReader_EventSplitAcrossWrites_MidTerminator(t *testing.T) {
	t.Parallel()

	pr, pw := io.Pipe()
	r := mcp.NewSSEEventReaderForTest(pr)

	evCh := make(chan struct {
		data []byte
		err  error
	}, 1)
	go func() {
		ev, err := r.Next()
		evCh <- struct {
			data []byte
			err  error
		}{ev.Data, err}
	}()

	if _, err := pw.Write([]byte("data: split\r")); err != nil {
		t.Fatalf("pw.Write #1: %v", err)
	}
	if _, err := pw.Write([]byte("\n\r\n")); err != nil {
		t.Fatalf("pw.Write #2: %v", err)
	}

	got := <-evCh
	if got.err != nil {
		t.Fatalf("Next() error = %v, want nil", got.err)
	}
	if string(got.data) != "split" {
		t.Errorf("Data = %q, want %q", got.data, "split")
	}
}

// ---- sseEventReader.readLine: per-line size cap ----------------------------

// TestSSEEventReader_PerLineCap_ErrorsClosed verifies that a single line
// whose own raw bytes exceed maxSSELineBytesForTest fails closed with
// errSSELineTooLarge, checked incrementally in readLine rather than only
// after copying the whole oversized line somewhere (Next's own per-event
// budget would eventually catch it too, but only after such a copy).
func TestSSEEventReader_PerLineCap_ErrorsClosed(t *testing.T) {
	t.Parallel()

	oversized := bytes.Repeat([]byte("a"), mcp.MaxSSELineBytesForTest+1)
	var buf bytes.Buffer
	buf.WriteString("data: ")
	buf.Write(oversized)
	buf.WriteString("\n\n")

	r := mcp.NewSSEEventReaderForTest(&buf)
	_, err := r.Next()
	if !errors.Is(err, mcp.ErrSSELineTooLargeForTest) {
		t.Fatalf("Next() error = %v, want errSSELineTooLarge", err)
	}
}

// ---- sseEventReader.Next: per-event size cap -------------------------------

// TestSSEEventReader_PerEventSizeCap_ErrorsClosed verifies that a single
// event whose accumulated line bytes (across two separate "data:" lines,
// each individually well under maxSSELineBytesForTest so only the aggregate
// per-event budget check below is what actually rejects this input) exceed
// maxSSEEventDataBytes fails closed with errSSEEventTooLarge, rather than
// growing without bound — the property that matters for a
// subscriptions/listen stream, which is specified to stay open indefinitely
// and so is never otherwise bounded by total size.
func TestSSEEventReader_PerEventSizeCap_ErrorsClosed(t *testing.T) {
	t.Parallel()

	// Two "data:" lines, each individually under the per-line cap, whose
	// combined raw bytes exceed the per-event budget — the cap is defined as
	// the SUM across every line of one event, not any single line in
	// isolation.
	first := bytes.Repeat([]byte("a"), mcp.MaxSSEEventDataBytesForTest-10)
	second := bytes.Repeat([]byte("b"), 20)
	var buf bytes.Buffer
	buf.WriteString("data: ")
	buf.Write(first)
	buf.WriteString("\ndata: ")
	buf.Write(second)
	buf.WriteString("\n\n")

	r := mcp.NewSSEEventReaderForTest(&buf)
	_, err := r.Next()
	if !errors.Is(err, mcp.ErrSSEEventTooLargeForTest) {
		t.Fatalf("Next() error = %v, want errSSEEventTooLarge", err)
	}
}

// TestSSEEventReader_JustUnderSizeCap_Succeeds is the boundary companion to
// the test above: an event whose total accumulated line cost sits exactly at
// the cap must still succeed — the guard must not be off-by-one in the
// strict direction. Two lines count against the budget before the event is
// dispatched: the "data:" line itself, and the terminating blank line that
// triggers dispatch (its own cost — 0 raw bytes plus the fixed
// sseLineOverheadBytes floor — is still charged before the dispatch happens,
// see Next's own doc), so value is sized so
// len("data: "+value)+2*sseLineOverheadBytes (one overhead charge per line)
// lands exactly at maxSSEEventDataBytes.
func TestSSEEventReader_JustUnderSizeCap_Succeeds(t *testing.T) {
	t.Parallel()

	value := bytes.Repeat([]byte("a"), mcp.MaxSSEEventDataBytesForTest-len("data: ")-2*mcp.SSELineOverheadBytesForTest)
	var buf bytes.Buffer
	buf.WriteString("data: ")
	buf.Write(value)
	buf.WriteString("\n\n")

	r := mcp.NewSSEEventReaderForTest(&buf)
	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next() error = %v, want nil (event exactly at the cap)", err)
	}
	if len(ev.Data) != len(value) {
		t.Errorf("len(Data) = %d, want %d", len(ev.Data), len(value))
	}
}

// TestSSEEventReader_UnlimitedEmptyDataLines_Rejected verifies that a run of
// empty "data:" lines (each contributing a zero-length VALUE, the shape the
// previous data-value-only accounting never counted at all) with no blank
// line to ever dispatch them is still bounded and rejected, rather than
// buffering — or simply looping over — an unbounded number of them.
func TestSSEEventReader_UnlimitedEmptyDataLines_Rejected(t *testing.T) {
	t.Parallel()

	const line = "data:\n" // 5 raw bytes ("data:") once the terminator is stripped
	repeats := mcp.MaxSSEEventDataBytesForTest/len("data:") + 10

	var buf bytes.Buffer
	for i := 0; i < repeats; i++ {
		buf.WriteString(line)
	}

	r := mcp.NewSSEEventReaderForTest(&buf)
	_, err := r.Next()
	if !errors.Is(err, mcp.ErrSSEEventTooLargeForTest) {
		t.Fatalf("Next() error = %v, want errSSEEventTooLarge", err)
	}
}

// TestSSEEventReader_UnlimitedCommentLines_Rejected is the same property for
// an unbounded run of keep-alive comment lines between blank lines — never
// counted at all under the previous accounting, and never dispatched as data
// either way, so nothing but this budget check would ever stop the scan.
func TestSSEEventReader_UnlimitedCommentLines_Rejected(t *testing.T) {
	t.Parallel()

	const line = ":ka\n" // 3 raw bytes (":ka") once the terminator is stripped
	repeats := mcp.MaxSSEEventDataBytesForTest/len(":ka") + 10

	var buf bytes.Buffer
	for i := 0; i < repeats; i++ {
		buf.WriteString(line)
	}

	r := mcp.NewSSEEventReaderForTest(&buf)
	_, err := r.Next()
	if !errors.Is(err, mcp.ErrSSEEventTooLargeForTest) {
		t.Fatalf("Next() error = %v, want errSSEEventTooLarge", err)
	}
}

// TestSSEEventReader_UnlimitedEmptyDataLines_MemoryBounded is the direct
// regression test for item 3's own guarantee: sseLineOverheadBytes must bound
// the ACTUAL memory a run of minimal "data:" lines costs, not merely the raw
// wire-byte count Next accumulates before rejecting the event. Each such
// line's own raw bytes ("data:", 5 bytes) are cheap, but every one still
// costs a real []byte slice-header-sized allocation once appended to
// dataLines — without the fixed per-line floor, the number of lines
// reachable before the nominal 1 MiB budget trips (roughly
// maxSSEEventDataBytes/len("data:") ≈ 209,715 of them) allocates roughly an
// order of magnitude more than the nominal budget suggests; with the floor,
// the line COUNT itself is bounded tightly enough (roughly
// maxSSEEventDataBytes/(len("data:")+sseLineOverheadBytes) ≈ 28,300) that
// total allocation stays within a small, bounded multiple of the nominal
// budget instead. This measures runtime.MemStats.TotalAlloc — CUMULATIVE
// bytes allocated, monotonically increasing and unaffected by what the GC
// has or hasn't yet reclaimed — rather than HeapAlloc (live bytes in use),
// which this exact shape (a large, mostly-already-drained bytes.Buffer
// still reachable alongside a slice of many small elements) makes an
// unreliable signal: a GC between the "before" and "after" snapshots can
// reclaim unrelated garbage (e.g. bytes.Buffer's own internal growth
// history) and make HeapAlloc appear to SHRINK across the very call this
// test means to measure, masking the property under test entirely.
// Deliberately not t.Parallel(): a quiesce-then-measure test needs a
// sibling-free allocation profile between its two snapshots, which a
// concurrently running test would pollute.
func TestSSEEventReader_UnlimitedEmptyDataLines_MemoryBounded(t *testing.T) {
	const numLines = 250_000
	const line = "data:\n"

	var buf bytes.Buffer
	buf.Grow(numLines * len(line))
	for i := 0; i < numLines; i++ {
		buf.WriteString(line)
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	r := mcp.NewSSEEventReaderForTest(&buf)
	_, err := r.Next()
	if !errors.Is(err, mcp.ErrSSEEventTooLargeForTest) {
		t.Fatalf("Next() error = %v, want errSSEEventTooLarge", err)
	}

	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// A generous multiple of the nominal per-event byte budget: comfortably
	// above what the sseLineOverheadBytes-bounded line count above actually
	// allocates (including the several intermediate reallocations dataLines'
	// own amortized growth performs along the way), while comfortably below
	// what the pre-fix, raw-line-bytes-only accounting's roughly 209,715
	// lines would allocate for the identical input.
	const bound = 8 * mcp.MaxSSEEventDataBytesForTest
	if grew := after.TotalAlloc - before.TotalAlloc; grew > bound {
		t.Errorf("Next() allocated %d bytes handling %d empty \"data:\" lines, want at most %d — sseLineOverheadBytes must bound the number of lines accumulated (and so the actual memory they cost), not just their raw byte sum", grew, numLines, bound)
	}
}

// ---- sseEventReader.Next: EOF mid-event -------------------------------------

// TestSSEEventReader_EOFMidEvent_NoTrailingBlankLine_DispatchesThenEOF
// verifies Next's EOF alignment with extractSSEResult (see both functions'
// own docs): a stream that ends (EOF) after accumulating "data:" lines but
// before the blank line that would ordinarily dispatch them still dispatches
// that pending event exactly once — matching splitSSEBody's own treatment of
// a fully-buffered body's trailing, unterminated chunk as a real final line,
// and extractSSEResult's own unconditional dispatch() call once its loop
// ends — with the underlying io.EOF deferred to the NEXT call.
func TestSSEEventReader_EOFMidEvent_NoTrailingBlankLine_DispatchesThenEOF(t *testing.T) {
	t.Parallel()

	const stream = "data: never dispatched"
	r := mcp.NewSSEEventReaderForTest(strings.NewReader(stream))

	ev, err := r.Next()
	if err != nil {
		t.Fatalf("Next() #1 error = %v, want nil (the final, unterminated line must still be dispatched)", err)
	}
	if string(ev.Data) != "never dispatched" {
		t.Errorf("Data = %q, want %q", ev.Data, "never dispatched")
	}

	_, err = r.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Next() #2 error = %v, want io.EOF", err)
	}
}

// TestSSEEventReader_EOFMidEvent_AfterCompleteFirstEvent verifies the same
// property when the EOF-truncated event follows a complete, successfully
// dispatched one: the first Next() call dispatches "complete", the second
// dispatches the truncated "truncated" event (see the sibling test above for
// why), and only the third call observes io.EOF.
func TestSSEEventReader_EOFMidEvent_AfterCompleteFirstEvent(t *testing.T) {
	t.Parallel()

	const stream = "data: complete\n\n" + "data: truncated"
	r := mcp.NewSSEEventReaderForTest(strings.NewReader(stream))

	ev1, err := r.Next()
	if err != nil {
		t.Fatalf("Next() #1 error = %v, want nil", err)
	}
	if string(ev1.Data) != "complete" {
		t.Errorf("Data #1 = %q, want %q", ev1.Data, "complete")
	}

	ev2, err := r.Next()
	if err != nil {
		t.Fatalf("Next() #2 error = %v, want nil (the trailing unterminated line must still be dispatched)", err)
	}
	if string(ev2.Data) != "truncated" {
		t.Errorf("Data #2 = %q, want %q", ev2.Data, "truncated")
	}

	_, err = r.Next()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("Next() #3 error = %v, want io.EOF", err)
	}
}
