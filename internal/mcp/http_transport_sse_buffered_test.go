package mcp_test

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file reproduces a known, not-yet-fixed bug in extractSSEData
// (internal/mcp/http_transport.go, ~line 530):
//
//	func extractSSEData(body []byte) []byte {
//		for _, line := range bytes.Split(body, []byte("\n")) {
//			if bytes.HasPrefix(line, []byte("data: ")) {
//				return bytes.TrimPrefix(line, []byte("data: "))
//			}
//			...
//
// It returns the FIRST "data:" line of an SSE response body and stops. This
// function sits on the BUFFERED client path — rawPost, used by both Call and
// (through it) ListTools — never on Forward, which streams bytes through
// untouched and is unaffected (see forward_streaming_test.go's
// TestForward_SSE_ProgressBeforeResult_BothEventsDeliveredInOrder, the
// already-green counterpart to this file on the streaming path).
//
// docs/mcp-v2.md is explicit that a request-scoped SSE response may contain
// more than the final result:
//
//   - §4.1 (Streamable HTTP, line 329f): "Request → Server antwortet entweder
//     mit `application/json` (ein Objekt) oder `text/event-stream` (Stream).
//     Der Client **MUSS** beides können."
//   - §3.4 (line 222-224): "Request-bezogene Notifications
//     (`notifications/progress`, `notifications/message`) laufen **nicht**
//     über diesen Stream [gemeint: den subscriptions/listen-Stream], sondern
//     nur über den Response-Stream des Requests, zu dem sie gehören."
//
// Read together: an upstream MAY answer any request — including a plain
// tools/list or tools/call VoidLLM sends through Call — with
// text/event-stream, and MAY send any number of notifications/progress (or
// notifications/message) events on that same stream before the final
// JSON-RPC result event. §11.3 Befund 3 of the same document names this
// directly as VoidLLM's own gap: "`Call` liest den kompletten Body per
// `io.ReadAll` und `extractSSEData` zieht nur die erste `data:`-Zeile
// heraus. Für `subscriptions/listen` und für Progress-Notifications vor dem
// finalen Result reicht das nicht."
//
// newSSEModernTransport builds a modern-era-pinned (V20260728) HTTPTransport,
// so every test below drives the fast, no-handshake path (dialect2026Client)
// directly through Call and ListTools — mirroring newModernTransport in
// http_transport_test.go, duplicated here because that helper is unexported
// to its own file.
func newSSEModernTransport(endpoint string) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, testStreamIdleTimeout)
}

// sseEvent formats a single SSE event carrying dataJSON as its one-line
// "data:" field, exactly matching the event framing already used by
// forward_streaming_test.go's progressEvent/resultEvent constants, so the
// buffered path under test here and the already-green streaming path stay
// byte-for-byte comparable.
func sseEvent(dataJSON string) string {
	return "event: message\n" + "data: " + dataJSON + "\n\n"
}

// TestListTools_SSEUpstream_SkipsProgressNotificationBeforeResult is the
// most important test in this file: a modern upstream answers tools/list
// with text/event-stream, sending one notifications/progress event before
// the real tools/list result event. ListTools must still return the real
// tool list.
//
// Today it does not: extractSSEData returns only the first "data:" line —
// the progress notification, which has no "result" field at all — so
// ListTools's own decode of result.Body into {Result.Tools, Error} finds
// neither a populated Tools slice nor an Error, and silently returns an
// EMPTY tool listing with a nil error. This is exactly the "leer" (empty,
// not erroring) failure mode the task describes, not a loud one.
func TestListTools_SSEUpstream_SkipsProgressNotificationBeforeResult(t *testing.T) {
	t.Parallel()

	const progressEvent = `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"tok-1","progress":50,"total":100}}`
	const toolsListResult = `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","description":"Search the web","inputSchema":{"type":"object"}}]}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sseEvent(progressEvent))
		fmt.Fprint(w, sseEvent(toolsListResult))
	}))
	t.Cleanup(srv.Close)

	tr := newSSEModernTransport(srv.URL)
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
	if len(listing.Tools) != 1 {
		t.Fatalf("ListTools() returned %d tools, want 1 (the real tools/list result, not the progress notification that preceded it on the SSE stream)", len(listing.Tools))
	}
	if listing.Tools[0].Name != "search" {
		t.Errorf("ListTools() tool name = %q, want %q", listing.Tools[0].Name, "search")
	}
}

// TestCall_SSEUpstream_ToolsCall_SkipsProgressNotificationBeforeResult is
// the Call counterpart to the ListTools test above, using a real tools/call
// request. Expectation: the returned *CallResult's Body is the real
// JSON-RPC response to the tools/call, not the notifications/progress event
// that preceded it.
func TestCall_SSEUpstream_ToolsCall_SkipsProgressNotificationBeforeResult(t *testing.T) {
	t.Parallel()

	const progressEvent = `{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"tok-1","progress":10,"total":100}}`
	const callResult = `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[{"type":"text","text":"deployment-complete-marker"}]}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sseEvent(progressEvent))
		fmt.Fprint(w, sseEvent(callResult))
	}))
	t.Cleanup(srv.Close)

	tr := newSSEModernTransport(srv.URL)
	req := &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy","arguments":{}}}`)}
	got, err := tr.Call(context.Background(), req, "")
	if err != nil {
		t.Fatalf("Call() error = %v, want nil", err)
	}
	if !bytes.Contains(got.Body, []byte("deployment-complete-marker")) {
		t.Errorf("Call() body = %q, want it to contain the real tools/call result (%q), not the notifications/progress event that preceded it on the SSE stream", got.Body, callResult)
	}
	if bytes.Contains(got.Body, []byte("notifications/progress")) {
		t.Errorf("Call() body = %q, want it NOT to be the progress notification extractSSEData incorrectly returns as if it were the result", got.Body)
	}
}

// TestListTools_SSEUpstream_MultipleNotificationsBeforeResult drives several
// notifications/progress events in a row before the final result, per
// docs/mcp-v2.md §3.4: nothing in the spec bounds how many request-scoped
// notifications an upstream may send on a single request's response stream
// before its result. Expectation: unchanged from the single-notification
// case above — the real tool list, regardless of how many notifications
// preceded it.
func TestListTools_SSEUpstream_MultipleNotificationsBeforeResult(t *testing.T) {
	t.Parallel()

	const toolsListResult = `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"weather","description":"Get the weather","inputSchema":{"type":"object"}}]}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i := 0; i < 5; i++ {
			fmt.Fprint(w, sseEvent(fmt.Sprintf(
				`{"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"tok-1","progress":%d,"total":100}}`, i*20)))
		}
		fmt.Fprint(w, sseEvent(toolsListResult))
	}))
	t.Cleanup(srv.Close)

	tr := newSSEModernTransport(srv.URL)
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
	if len(listing.Tools) != 1 || listing.Tools[0].Name != "weather" {
		t.Errorf("ListTools() = %+v, want [{weather ...}] — the real result must survive any number of preceding progress notifications", listing.Tools)
	}
}

// TestListTools_SSEUpstream_MultiLineDataEvent_SingleEvent covers a shape
// extractSSEData mishandles independently of the notification-skipping bug
// above: the WHATWG "Server-Sent Events" interpretation algorithm
// (https://html.spec.whatwg.org/multipage/server-sent-events.html#event-stream-interpretation,
// not itself part of docs/mcp-v2.md, which assumes but does not restate the
// underlying SSE framing) explicitly allows a single event's data field to
// be split across several consecutive "data:" lines, which the client MUST
// reassemble by joining them with "\n" before parsing. This event carries
// ONLY the JSON-RPC result — no notification precedes it — split across two
// "data:" lines purely as valid SSE framing of one event.
//
// Today extractSSEData takes only the FIRST "data:" line of the body
// (`{"jsonrpc":"2.0",`), which is not even valid JSON on its own, so
// ListTools fails to decode it — a different symptom (a loud decode error)
// from the empty-result symptom above, but the same root cause.
func TestListTools_SSEUpstream_MultiLineDataEvent_SingleEvent(t *testing.T) {
	t.Parallel()

	// One SSE event, two "data:" lines, that reassemble (joined by "\n")
	// into a single valid JSON-RPC tools/list result.
	const multiLineEvent = "event: message\n" +
		`data: {"jsonrpc":"2.0",` + "\n" +
		`data: "id":1,"result":{"tools":[{"name":"calendar","description":"Manage calendar","inputSchema":{"type":"object"}}]}}` +
		"\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, multiLineEvent)
	}))
	t.Cleanup(srv.Close)

	tr := newSSEModernTransport(srv.URL)
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil (the two \"data:\" lines should reassemble into one valid JSON-RPC result)", err)
	}
	if len(listing.Tools) != 1 || listing.Tools[0].Name != "calendar" {
		t.Errorf("ListTools() = %+v, want [{calendar ...}]", listing.Tools)
	}
}

// TestListTools_SSEUpstream_SingleEventNoNotification_AlreadyWorks is the
// deliberate counter-proof the task asks for: a modern upstream that
// declares Content-Type: text/event-stream but sends exactly ONE event,
// with a single-line "data:" field, and no notification ahead of it. This
// is the shape extractSSEData was originally written for, and it already
// works correctly today — this test is expected to be GREEN. It exists so
// that a future fix to extractSSEData (or to whatever replaces it) can be
// checked against this test too: if fixing the multi-event/multi-line cases
// above ever broke this simplest case, this test would catch it.
func TestListTools_SSEUpstream_SingleEventNoNotification_AlreadyWorks(t *testing.T) {
	t.Parallel()

	const toolsListResult = `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"echo","description":"","inputSchema":{"type":"object"}}]}}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, sseEvent(toolsListResult))
	}))
	t.Cleanup(srv.Close)

	tr := newSSEModernTransport(srv.URL)
	listing, err := tr.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools() error = %v, want nil", err)
	}
	if len(listing.Tools) != 1 || listing.Tools[0].Name != "echo" {
		t.Errorf("ListTools() = %+v, want [{echo ...}]", listing.Tools)
	}
}
