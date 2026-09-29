package mcp_test

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file proves sseEventReader (the incremental, live-connection SSE
// parser HTTPTransport.Listen uses) and extractSSEResult (the fully-buffered
// parser rawPost uses, http_transport.go) dispatch IDENTICAL events for the
// same input bytes — in particular at every one of the EOF/termination
// boundaries Next's own doc documents as aligned with extractSSEResult's
// unconditional post-loop dispatch() call (see both functions' own docs).

// bufferedDispatchedEvents mirrors extractSSEResult's own dispatch loop
// exactly — blank lines dispatch whatever "data:" lines have accumulated
// since the last one, and a final, undispatched event is dispatched once
// more after the loop ends — using only the field-recognition primitives
// already exported for testing (SplitSSEBodyForTest, ClassifySSELineForTest).
// Unlike extractSSEResult itself, this collects EVERY dispatched event's
// Data regardless of its own JSON-RPC id: extractSSEResult only ever returns
// the ONE event matching a caller-supplied id, which is not the property
// this file's shared table needs — it needs "which events get dispatched at
// all, with what bytes", independent of the id-matching layered on top by
// extractSSEResult's own caller. This is test-only scaffolding, not
// production code under test in its own right.
func bufferedDispatchedEvents(body []byte) [][]byte {
	lines := mcp.SplitSSEBodyForTest(body)

	var dataLines [][]byte
	var dispatched [][]byte

	dispatch := func() {
		if len(dataLines) == 0 {
			return
		}
		dispatched = append(dispatched, bytes.Join(dataLines, []byte("\n")))
		dataLines = nil
	}

	for _, line := range lines {
		kind, value := mcp.ClassifySSELineForTest(line)
		switch kind {
		case mcp.SSELineKindBlank:
			dispatch()
		case mcp.SSELineKindData:
			dataLines = append(dataLines, value)
		default:
			// id:, event:, comment, or any other field — ignored, exactly as
			// extractSSEResult's own loop ignores everything but blank and
			// data lines.
		}
	}
	dispatch() // the same unconditional final dispatch extractSSEResult performs

	return dispatched
}

// incrementalDispatchedEvents drains sseEventReader via Next() until it
// returns an error, collecting every dispatched event's Data along the way,
// and returns that terminal error alongside them.
func incrementalDispatchedEvents(body []byte) (dispatched [][]byte, terminalErr error) {
	r := mcp.NewSSEEventReaderForTest(bytes.NewReader(body))
	for {
		ev, err := r.Next()
		if err != nil {
			return dispatched, err
		}
		dispatched = append(dispatched, ev.Data)
	}
}

// TestSSEParity_BufferedAndIncrementalDispatchIdenticalEvents runs the same
// raw SSE bytes through both parsers and asserts they dispatch the exact
// same sequence of events, for every EOF/termination shape either parser's
// own doc calls out: a proper trailing blank line, a body that ends right
// after a line's own terminator but with no SEPARATE blank line following
// (which bytes.Split's own trailing-empty-element behavior turns into one
// anyway — see splitSSEBody's own doc), and a body that ends mid-line with
// no terminator at all. The incremental reader's terminal error is always
// io.EOF here — none of these inputs exercise a genuine transport failure.
func TestSSEParity_BufferedAndIncrementalDispatchIdenticalEvents(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"empty_body", ""},
		{"only_comments_no_data", ":a\n:b\n\n"},
		{"single_event_trailing_blank_line", "data: one\n\n"},
		{"single_event_terminated_no_trailing_blank", "data: one\n"},
		{"single_event_unterminated_partial_line", "data: one"},
		{"multi_line_data_trailing_blank", "data: line one\ndata: line two\n\n"},
		{"multiple_events_trailing_blank", "data: one\n\ndata: two\n\n"},
		{"multiple_events_no_trailing_blank", "data: one\n\ndata: two"},
		{"multiple_events_second_terminated_no_trailing_blank", "data: one\n\ndata: two\n"},
		{"id_and_comment_lines_ignored", "id: 1\n:comment\ndata: one\n\n"},
		{"crlf_line_endings", "data: one\r\n\r\n"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			want := bufferedDispatchedEvents([]byte(tc.body))
			got, err := incrementalDispatchedEvents([]byte(tc.body))
			if !errors.Is(err, io.EOF) {
				t.Fatalf("incremental reader terminal error = %v, want io.EOF", err)
			}
			if len(got) != len(want) {
				t.Fatalf("dispatched %d events, want %d (buffered: %q, incremental: %q)",
					len(got), len(want), want, got)
			}
			for i := range want {
				if !bytes.Equal(got[i], want[i]) {
					t.Errorf("event %d Data = %q, want %q", i, got[i], want[i])
				}
			}
		})
	}
}

// parityWantID is the JSON-RPC id every fixture below's final event carries,
// and the id TestSSEParity_RealExtractSSEResult_MatchesIncrementalDispatch
// asks the REAL extractSSEResult (via mcp.ExtractSSEResult) to match against.
const parityWantID = "parity-id"

// parityWantIDRaw is parityWantID already marshaled as the JSON string value
// extractSSEResult's own wantID parameter expects — see that function's own
// doc (http_transport.go) for the exact byte-for-byte comparison rule this
// must satisfy.
var parityWantIDRaw = []byte(`"` + parityWantID + `"`)

// TestSSEParity_RealExtractSSEResult_MatchesIncrementalDispatch is the
// direct regression test for item 5's own guarantee: unlike
// TestSSEParity_BufferedAndIncrementalDispatchIdenticalEvents above — which
// proves the incremental reader agrees with bufferedDispatchedEvents, a
// hand-rolled REPLICA of extractSSEResult's own dispatch loop — this proves
// the incremental reader agrees with the REAL, production extractSSEResult
// (exposed for this test via mcp.ExtractSSEResult, export_test.go) for a
// caller that actually cares which ONE event a given id matches, not merely
// "which events get dispatched at all". A second, independent
// reimplementation of the same loop cannot rule out both containing the
// identical mistake — this closes that gap by driving the real
// implementation directly.
//
// Every fixture's FINAL dispatched event carries a JSON-RPC response naming
// parityWantID, across the same three EOF/termination shapes the sibling
// test above covers (a proper trailing blank line, a single terminating
// newline with no separate trailing blank line, and no terminator at all —
// see sseEventReader.Next's own doc for why the incremental reader dispatches
// identically in all three cases), plus a variant prefixing an unrelated
// notification event (no id at all) before the matching response, proving
// extractSSEResult's own id-matching skips it exactly as
// dispatchListenMessage's incremental equivalent does.
func TestSSEParity_RealExtractSSEResult_MatchesIncrementalDispatch(t *testing.T) {
	t.Parallel()

	const responseEvent = `data: {"jsonrpc":"2.0","id":"parity-id","result":{"ok":true}}`
	const unrelatedNotificationEvent = `data: {"jsonrpc":"2.0","method":"notifications/progress","params":{}}` + "\n\n"

	tests := []struct {
		name string
		body string
	}{
		{"trailing_blank_line", responseEvent + "\n\n"},
		{"terminated_no_trailing_blank", responseEvent + "\n"},
		{"unterminated_final_line_no_trailing_newline", responseEvent},
		{"preceded_by_unrelated_notification_trailing_blank", unrelatedNotificationEvent + responseEvent + "\n\n"},
		{"preceded_by_unrelated_notification_no_trailing_blank", unrelatedNotificationEvent + responseEvent},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			incremental, incErr := incrementalDispatchedEvents([]byte(tc.body))
			if !errors.Is(incErr, io.EOF) {
				t.Fatalf("incremental reader terminal error = %v, want io.EOF", incErr)
			}
			if len(incremental) == 0 {
				t.Fatal("incremental reader dispatched no events at all")
			}
			wantData := incremental[len(incremental)-1]

			got, err := mcp.ExtractSSEResult([]byte(tc.body), parityWantIDRaw)
			if err != nil {
				t.Fatalf("ExtractSSEResult() error = %v, want nil", err)
			}
			if !bytes.Equal(got, wantData) {
				t.Errorf("ExtractSSEResult() = %q, want %q (the incremental reader's own final dispatched event, matching id %q)",
					got, wantData, parityWantID)
			}
		})
	}
}
