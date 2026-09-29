package mcp

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// maxSSEEventDataBytes bounds how many bytes of a single SSE event
// sseEventReader will accumulate, across EVERY line belonging to that event —
// not only "data:" lines, but also "id:", "event:", comment (":") lines, and
// any other recognized-but-ignored field, each counted by its own raw,
// already-terminator-stripped length (see Next's own doc for exactly how and
// why) — before failing closed with errSSEEventTooLarge. Unlike
// maxSSEEventsSkipped (http_transport.go), which bounds how many events
// extractSSEResult examines within a single, already fully-buffered response
// of at most rawPostMaxBodyBytes, nothing else in this package bounds the
// TOTAL size of a long-lived subscriptions/listen stream at all — it is
// specified to stay open indefinitely (docs/mcp-v2.md §3.4/§1a) — so without
// a per-event ceiling here, a single malicious or misbehaving upstream event
// could otherwise grow without limit before HTTPTransport.Listen's own JSON
// decode of it even begins. Counting every line kind, not only "data:" lines
// with their prefix already stripped, closes the gap the previous
// data-value-only accounting left open: an upstream sending an unbounded run
// of empty "data:" lines (each contributing a zero-length VALUE) or an
// unbounded run of comment lines (never counted at all) between blank lines
// could otherwise never trip this cap, no matter how long the connection
// stayed open accumulating them.
const maxSSEEventDataBytes = 1 << 20 // 1 MiB

// sseLineOverheadBytes is a fixed, per-line cost charged against
// maxSSEEventDataBytes IN ADDITION TO a line's own raw byte length — see
// Next's own doc for exactly where this is applied. Every line Next reads,
// regardless of kind, costs at least this much bookkeeping overhead
// regardless of its own length: the []byte slice header (and, for a "data:"
// line, the copy appended to dataLines) Go allocates per line, the
// dataLines slice's own amortized growth as more elements are appended to
// it, and the general per-iteration accounting this reader performs. Without
// this floor, a line that is short on the wire but still costs real heap
// memory per occurrence — most notably an unbounded run of empty "data:"
// lines, each contributing only its few-byte "data:" prefix to the
// raw-length-only accounting maxSSEEventDataBytes alone would perform — could
// still accumulate far more actual memory (one slice-header-sized allocation
// per line, however small) than the 1 MiB nominal budget suggests, well
// before that budget's own byte count is exhausted. Charging this overhead
// per line, not per byte, closes that amplification gap: the number of
// lines a single event can ever accumulate before tripping
// errSSEEventTooLarge is now itself bounded to roughly
// maxSSEEventDataBytes/sseLineOverheadBytes, regardless of how short each
// individual line's own raw bytes are.
const sseLineOverheadBytes = 32

// maxSSELineBytes bounds how many bytes of a single line (as returned by
// readLine, terminator already stripped) sseEventReader will buffer at all,
// checked incrementally as each byte is read — see readLine's own doc — so a
// single pathological line can never grow past this size regardless of which
// SSE field it turns out to name once classified, and so Next never has to
// copy an oversized line's bytes (into dataLines, or anywhere else) before
// discovering it is oversized. Deliberately the same ceiling as
// maxSSEEventDataBytes: no single line can legitimately need to contribute
// more than the entire per-event budget, so a separate, tighter constant
// would only be a second tuning knob for the same limit. Because the two
// constants are equal, a single line at exactly this size — once
// sseLineOverheadBytes is added on top, per Next's own per-event budget
// check — necessarily EXCEEDS that budget rather than merely saturating it;
// this is not an off-by-one hazard, since readLine's own cap and Next's own
// cap are two independent guards (a line can fail either one first) and both
// fail closed identically, with distinct sentinels (errSSELineTooLarge vs
// errSSEEventTooLarge) a caller never needs to tell apart.
const maxSSELineBytes = maxSSEEventDataBytes

// errSSELineTooLarge is returned by sseEventReader.readLine when a single
// line's accumulated bytes exceed maxSSELineBytes. A bare, static sentinel:
// the line's own content is upstream-controlled, and this package's
// zero-knowledge-logging rule keeps it out of error text throughout this
// file, exactly as it does everywhere else in this package.
var errSSELineTooLarge = errors.New("mcp: sse line exceeds the maximum size of 1 MiB")

// errSSEEventTooLarge is returned by sseEventReader.Next when a single
// event's accumulated line bytes (see maxSSEEventDataBytes's own doc for
// exactly what is counted) exceed maxSSEEventDataBytes. It is a bare, static
// sentinel: the event's own content is upstream-controlled, and this
// package's zero-knowledge-logging rule keeps it out of error text
// throughout this file, exactly as it does everywhere else in this package.
var errSSEEventTooLarge = errors.New("mcp: sse event data exceeds the maximum size of 1 MiB")

// sseEvent is a single parsed SSE event, per the WHATWG "Server-Sent Events"
// interpretation algorithm (see extractSSEResult's own doc, http_transport.go,
// for the citation this package already relies on for SSE framing). ID and
// Event are this event's own "id:" and "event:" field values — ID is sticky
// and persists across events that do not set a new one, and Event resets to
// "" after every dispatched event, exactly as the algorithm specifies — and
// Data is every "data:" line's value joined by "\n", in the order they
// appeared.
type sseEvent struct {
	ID    string
	Event string
	Data  []byte
}

// sseLineKind classifies a single already-line-terminator-stripped SSE
// stream line, per classifySSELine's own doc.
type sseLineKind int

const (
	sseLineOther sseLineKind = iota
	sseLineBlank
	sseLineComment
	sseLineData
	sseLineID
	sseLineEvent
)

// classifySSELine classifies line — one line of an SSE stream, with its line
// terminator already removed by the caller — into one of the three fields
// this package ever acts on ("data:", "id:", "event:"), a comment, a blank
// (event-dispatching) line, or any other field name, which is recognized
// only enough to be ignored, never acted on — exactly as extractSSEResult's
// own doc already documents for "event:", "id:", "retry:", or anything else
// on the path that predates this function. value is the field's value with
// a single optional leading space already stripped (the "field-name: value"
// convention's sentinel space; "field-name:value" without one is equally
// valid) for the three recognized kinds; nil for every other kind.
//
// This is the single implementation of that field-recognition rule shared by
// extractSSEResult — the fully-buffered path, operating on an
// already-normalized []byte split into lines by splitSSEBody — and
// sseEventReader — the incremental, live-connection path used by
// HTTPTransport.Listen. Only the LINE-SPLITTING step differs between the
// two (a whole-body bytes.Split versus reading one line at a time off a live
// io.Reader); the classification of each resulting line is now identical
// code for both, rather than two independently maintained copies of the same
// framing rules.
func classifySSELine(line []byte) (kind sseLineKind, value []byte) {
	switch {
	case len(line) == 0:
		return sseLineBlank, nil
	case bytes.HasPrefix(line, []byte(":")):
		return sseLineComment, nil
	case bytes.HasPrefix(line, []byte("data: ")):
		return sseLineData, line[len("data: "):]
	case bytes.HasPrefix(line, []byte("data:")):
		return sseLineData, line[len("data:"):]
	case bytes.HasPrefix(line, []byte("id: ")):
		return sseLineID, line[len("id: "):]
	case bytes.HasPrefix(line, []byte("id:")):
		return sseLineID, line[len("id:"):]
	case bytes.HasPrefix(line, []byte("event: ")):
		return sseLineEvent, line[len("event: "):]
	case bytes.HasPrefix(line, []byte("event:")):
		return sseLineEvent, line[len("event:"):]
	default:
		return sseLineOther, nil
	}
}

// splitSSEBody normalizes body's line endings — \r\n and bare \r both become
// \n, tolerating an upstream that does not use bare LF — and splits it into
// lines with their terminators removed. This is the exact normalization
// extractSSEResult performed inline before this helper existed, extracted so
// it has one implementation rather than being reproduced at every
// fully-buffered SSE caller.
func splitSSEBody(body []byte) [][]byte {
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	normalized = bytes.ReplaceAll(normalized, []byte("\r"), []byte("\n"))
	return bytes.Split(normalized, []byte("\n"))
}

// sseEventReader incrementally parses SSE events off of a live io.Reader —
// typically an upstream response body wrapped in an idle-timeout reader
// (idleTimeoutReader) — one event at a time, in contrast to
// extractSSEResult, which requires the ENTIRE body already buffered in
// memory before it can even begin. This is what lets HTTPTransport.Listen
// react to each notification on a long-lived subscriptions/listen stream
// (docs/mcp-v2.md §3.4) as it arrives, rather than only once the connection
// eventually closes. Not safe for concurrent use — like idleTimeoutReader and
// every other single-stream reader in this package, exactly one goroutine
// calls Next at a time.
type sseEventReader struct {
	r *bufio.Reader

	lastID     string
	eventType  string
	dataLines  [][]byte
	eventBytes int

	// err is sticky: once readLine observes a failure from the underlying
	// reader (io.EOF or anything else), every subsequent readLine call
	// returns the SAME error immediately, without touching s.r again. This
	// both guarantees Next's own "dispatch the final pending event, then
	// report the error on the NEXT call" contract (see Next's own doc) always
	// terminates after exactly one extra dispatch, and avoids depending on
	// the underlying reader's own EOF being safely repeatable.
	err error
}

// newSSEEventReader returns an sseEventReader reading from r.
func newSSEEventReader(r io.Reader) *sseEventReader {
	return &sseEventReader{r: bufio.NewReaderSize(r, 4096)}
}

// Next blocks until a complete SSE event is available and returns it, or
// returns a non-nil error — from the underlying reader (including io.EOF
// once the stream ends, which HTTPTransport.Listen treats as an unexpected
// disconnect — see its own doc), errSSELineTooLarge once a single line
// exceeds maxSSELineBytes (see readLine's own doc), or errSSEEventTooLarge
// once a single event's accumulated line bytes exceed maxSSEEventDataBytes.
// An event with no "data:" lines at all — for example a run of keep-alive
// comment lines followed by a blank line — is never dispatched, per the
// WHATWG algorithm ("if the data buffer is an empty string ... return"):
// Next simply keeps reading past it instead of returning a spurious empty
// event.
//
// Every line Next reads — regardless of kind: "data:" (including an empty
// one), "id:", "event:", a comment, or any other recognized-but-ignored
// field — counts against the current event's share of maxSSEEventDataBytes,
// checked BEFORE the line is acted on (appended to dataLines, or otherwise),
// so a line that would push the running total over the cap is rejected
// without first copying its bytes anywhere. Each line's own cost is its raw
// byte length PLUS the fixed sseLineOverheadBytes floor (see that constant's
// own doc) — deliberately broader than "sum of data: VALUES" (the previous
// accounting) in two ways: an upstream sending an unbounded run of empty
// "data:" lines, or an unbounded run of comment lines, between two blank
// lines could otherwise never trip this cap at all, no matter how long it
// kept the connection open doing so, and even counting each such line's raw
// bytes alone (without the fixed floor) would still let the ACTUAL memory
// such a run consumes — one slice-header-sized allocation per line,
// regardless of how short — grow well past what the nominal byte budget
// suggests before the cap ever triggers. The budget resets to zero at every
// event boundary (see dispatchPending's own doc) — the blank line that
// dispatches (or discards) one event, or the artificial boundary EOF
// imposes, whichever comes first.
//
// EOF behavior, aligned with extractSSEResult (the fully-buffered
// equivalent, http_transport.go — see that function's own doc for exactly
// why): reaching the end of the stream (any error from readLine) with lines
// already accumulated for the CURRENT event — whether they ended in a proper
// blank line elsewhere on the wire or not, and regardless of whether the
// very last one was itself newline-terminated (see readLine's own doc for
// why an unterminated trailing line is folded in as an ordinary line rather
// than discarded) — dispatches that pending event exactly once, deferring
// the underlying error to the NEXT call to Next, which then returns it
// directly (s.err is sticky — see its own field doc). Reaching the end of
// the stream with nothing pending returns the underlying error immediately,
// with no event. This exactly mirrors extractSSEResult's own unconditional
// dispatch() call after its line-processing loop ends, which fires under the
// identical condition (dataLines non-empty) regardless of how the body
// itself ended.
func (s *sseEventReader) Next() (sseEvent, error) {
	for {
		line, err := s.readLine()
		if err != nil {
			if ev, ok := s.dispatchPending(); ok {
				return ev, nil
			}
			return sseEvent{}, err
		}

		lineCost := len(line) + sseLineOverheadBytes
		if s.eventBytes+lineCost > maxSSEEventDataBytes {
			return sseEvent{}, errSSEEventTooLarge
		}
		s.eventBytes += lineCost

		kind, value := classifySSELine(line)
		switch kind {
		case sseLineBlank:
			if ev, ok := s.dispatchPending(); ok {
				return ev, nil
			}
			// No data accumulated since the last dispatch (or since Next
			// started reading) — the WHATWG algorithm resets the event-type
			// buffer here and dispatches nothing. This is also an event
			// boundary for the size budget above, even though nothing is
			// dispatched: the NEXT event starts its own count from zero.
			s.eventType = ""
			s.eventBytes = 0
		case sseLineComment:
			// Ignored — but already counted against the event's budget above.
		case sseLineData:
			cp := append([]byte(nil), value...)
			s.dataLines = append(s.dataLines, cp)
		case sseLineID:
			// The last event ID buffer is sticky per the WHATWG algorithm: set
			// by any "id:" line, including an explicitly empty one, and
			// otherwise persisting across dispatched events until a later
			// "id:" line changes it again.
			s.lastID = string(value)
		case sseLineEvent:
			s.eventType = string(value)
		case sseLineOther:
			// Ignored — see classifySSELine's own doc.
		}
	}
}

// dispatchPending builds and returns the sseEvent for whatever "data:" lines
// are currently accumulated, resetting all per-event state (dataLines,
// eventType, and the size budget — see Next's own doc) for the next one, or
// reports ok == false without resetting anything if no "data:" line has been
// seen since the last event boundary — the WHATWG algorithm's "if the data
// buffer is an empty string ... return" rule, shared by both of Next's own
// call sites: the ordinary blank-line boundary, and the end-of-stream
// fallback (see Next's own EOF doc).
func (s *sseEventReader) dispatchPending() (sseEvent, bool) {
	if len(s.dataLines) == 0 {
		return sseEvent{}, false
	}
	ev := sseEvent{
		ID:    s.lastID,
		Event: s.eventType,
		Data:  bytes.Join(s.dataLines, []byte("\n")),
	}
	s.dataLines = nil
	s.eventType = ""
	s.eventBytes = 0
	return ev, true
}

// readLine reads and returns the next line from s.r, with its line
// terminator removed, recognizing \n, \r\n, and a bare \r as line endings —
// the same CR/CRLF/LF tolerance extractSSEResult's own normalization applies
// to a fully-buffered body (splitSSEBody), detected byte-by-byte here since
// an incremental reader cannot normalize the whole stream up front.
//
// Once s.r itself fails (io.EOF once the stream ends, or any other error —
// including a context cancellation propagated up from an
// idleTimeoutReader-wrapped body), readLine's behavior depends on whether
// any bytes had already been read into the current line before the failure:
//
//   - None yet (a failure at a line boundary): the error is returned
//     immediately, with a nil line, and is remembered (s.err) so every
//     subsequent call returns the same error without reading from s.r again.
//   - Some bytes already read, but no terminator ever arrived (a final,
//     unterminated partial line at end-of-stream): those bytes ARE returned
//     as an ordinary line, with a nil error — the failure itself is still
//     remembered (s.err) for the NEXT call. This matches splitSSEBody's own
//     treatment of a fully-buffered body's trailing, non-newline-terminated
//     chunk: bytes.Split always includes it as a real final element to
//     classify, never discards it — so treating it any other way here would
//     make the incremental and buffered parsers disagree about which events
//     end up dispatched for the exact same bytes. See Next's own EOF doc for
//     how the caller turns this into a final dispatched event when
//     appropriate.
//
// A single line's own bytes are bounded independently of maxSSEEventDataBytes
// (see maxSSELineBytes's own doc): once len(buf) would exceed
// maxSSELineBytes, readLine fails closed with errSSELineTooLarge — checked
// before the byte that would exceed it is ever appended, so a single
// pathological line is never grown, let alone copied elsewhere, past that
// size.
func (s *sseEventReader) readLine() ([]byte, error) {
	if s.err != nil {
		return nil, s.err
	}
	var buf []byte
	for {
		b, err := s.r.ReadByte()
		if err != nil {
			s.err = err
			if len(buf) > 0 {
				return buf, nil
			}
			return nil, err
		}
		switch b {
		case '\n':
			return buf, nil
		case '\r':
			if next, peekErr := s.r.Peek(1); peekErr == nil && len(next) == 1 && next[0] == '\n' {
				_, _ = s.r.ReadByte() // consume the paired \n of a \r\n terminator
			}
			return buf, nil
		default:
			if len(buf) >= maxSSELineBytes {
				s.err = errSSELineTooLarge
				return nil, errSSELineTooLarge
			}
			buf = append(buf, b)
		}
	}
}
