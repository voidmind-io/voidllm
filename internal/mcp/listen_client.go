package mcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// ErrListenUnsupported is returned by Listen when the upstream does not
// implement subscriptions/listen at all: an HTTP 404 or 405 answering the
// request, or a 2xx JSON-RPC response carrying an application/json body
// whose top-level error code is CodeMethodNotFound (-32601). Any OTHER
// status paired with that same error code (a 500 carrying -32601 in its
// body, for instance) is deliberately NOT classified as unsupported — see
// isListenUnsupportedResponse's own doc for why conflating the two would
// misroute a genuine upstream failure into runListener's dont-retry-for-an-hour
// bucket instead of its ordinary exponential-backoff one. It is also
// returned immediately, before any request is ever sent, when the resolved
// binding is EraLegacy — a pre-2026-07-28 upstream has no subscriptions/listen
// method to call at all (docs/mcp-v2.md §3.4, §1a).
var ErrListenUnsupported = errors.New("mcp: upstream does not support subscriptions/listen")

// ErrListenNotHonored is returned by Listen when the upstream acknowledges
// the subscriptions/listen request but its acknowledgement's own
// params.notifications does not report the requested filter (toolsListChanged)
// as honored — MCP 2026-07-28 §3.4: "Das notifications-Feld im
// Acknowledgement spiegelt die Teilmenge, die der Server tatsächlich
// honoriert." A server that supports the method in principle but declines
// this particular filter is, from Listen's caller's point of view, exactly
// as unusable as one that does not support the method at all.
var ErrListenNotHonored = errors.New("mcp: upstream acknowledged subscriptions/listen but did not honor the requested notification filter")

// ErrListenIdle is returned by Listen when the stream's idle timeout
// (HTTPTransport's own streamIdleTimeout, exactly as Forward applies it —
// see newIdleTimeoutReader) elapses before the upstream sends another byte.
// This is distinguished from an ordinary transport error so a caller such as
// ListenManager can apply its own, more patient reconnect policy for "the
// upstream simply went quiet" rather than treating it the same as a genuine
// failure — see ListenManager's own reconnect-policy doc.
var ErrListenIdle = errors.New("mcp: subscriptions/listen stream idle timeout elapsed")

// errListenMalformedEvent is returned when an event on the subscriptions/listen
// stream cannot be interpreted as a JSON-RPC message this function
// recognizes — the ack was not the required first event, its subscriptionId
// did not match this request's own id, or a later event's data failed to
// parse as JSON-RPC at all. A bare, static sentinel: the event's own content
// is upstream-controlled and this package's zero-knowledge-logging rule
// keeps it out of error text, as it does everywhere else in this package.
var errListenMalformedEvent = errors.New("mcp: subscriptions/listen received a malformed or unexpected event")

// errListenUpstreamError is returned when the upstream's own graceful-end
// JSON-RPC response (MCP 2026-07-28 §3.4: id equal to the subscriptions/listen
// request's own id) carries a JSON-RPC error instead of a result. The
// error's own code/message/data are never embedded — upstream-controlled
// content this package's zero-knowledge-logging rule keeps out of error
// text throughout this file.
var errListenUpstreamError = errors.New("mcp: subscriptions/listen upstream response carried a json-rpc error")

// errListenUnexpectedResponse is returned when the upstream answers the
// subscriptions/listen request with a status or content type this function
// does not recognize as either "unsupported" (see ErrListenUnsupported) or a
// genuine streamed acknowledgement: any other JSON body, or any content type
// other than application/json or text/event-stream.
var errListenUnexpectedResponse = errors.New("mcp: subscriptions/listen received an unexpected response")

// listenRequestID is the fixed JSON-RPC id Listen sends on every
// subscriptions/listen request it makes. A string, not a number, so it can
// never collide with whatever numbering scheme a caller of Call happens to
// use for its own requests against the same upstream (Listen and Call share
// nothing else — Listen never goes through Call/doCall at all — but the ids
// still travel to the same upstream, which is free to reject a request
// carrying an id it has already seen).
const listenRequestID = "voidllm-listen"

// listenIDRaw is listenRequestID already marshaled as a JSON string value —
// the exact bytes Listen compares every subscriptionId and graceful-end
// response id against, per MCP 2026-07-28 §3.4 ("Der Wert ist die
// JSON-RPC-ID des subscriptions/listen-Requests"). Comparisons are done as
// JSON values (trimmed byte-for-byte equality against this literal), never
// by re-marshaling or re-parsing listenRequestID on every comparison.
var listenIDRaw = jsonx.RawMessage(`"` + listenRequestID + `"`)

// Listen opens a subscriptions/listen stream against the upstream this
// transport is configured for and blocks until it ends, calling onAck
// exactly once, synchronously, immediately after a valid acknowledgement is
// observed — one that names this call's own subscriptionId and honors the
// requested toolsListChanged filter — and calling onToolsChanged
// synchronously for every subsequent notifications/tools/list_changed event
// that carries that same subscriptionId (MCP 2026-07-28 §3.4). Neither
// callback must block for long: both run on Listen's own calling goroutine,
// between reads of the stream, so a slow callback delays every read behind
// it. onAck is never called more than once per Listen call, and never at
// all if Listen returns ErrListenUnsupported or ErrListenNotHonored — see
// the return contract below for exactly which outcomes guarantee onAck ran
// and which guarantee it did not.
//
// Listen is only ever meaningful against a modern-era (EraModern) upstream —
// the legacy revisions have no subscriptions/listen method at all — so it
// resolves this transport's binding (exactly like Call) and returns
// ErrListenUnsupported immediately, without sending anything, when the
// resolved era is EraLegacy.
//
// The request is built with the modern client dialect
// (dialect2026Client.Prepare), so its params._meta and standard request
// headers (MCP-Protocol-Version, Mcp-Method) are exactly as for any other
// modern-era call this transport makes — see buildAuthedRequest for the
// shared authentication/header path Listen sends it through, identical to
// rawPost and Forward. Unlike rawPost, Listen sends the request on
// streamClient (no overall http.Client.Timeout) and wraps the response body
// in the same idle-timeout reader Forward uses (newIdleTimeoutReader, this
// transport's own streamIdleTimeout) — a subscriptions/listen stream is
// specified to stay open indefinitely (docs/mcp-v2.md §3.4/§1a), so nothing
// here ever bounds it by total duration, only by idle time or by ctx.
//
// Return contract:
//
//   - ErrListenUnsupported: the resolved era is legacy, the upstream
//     answered with HTTP 404/405, or its response was a 2xx application/json
//     JSON-RPC error carrying CodeMethodNotFound. onAck was NOT called.
//   - ErrListenNotHonored: the upstream acknowledged the request but did not
//     report toolsListChanged as an honored filter. onAck was NOT called —
//     an acknowledgement that does not honor what was asked for is not the
//     "valid acknowledgement" onAck's own contract requires.
//   - nil: the upstream ended the subscription gracefully — a JSON-RPC
//     response whose id matches this call's own, carrying a result rather
//     than an error (MCP 2026-07-28 §3.4's "Graceful Closure"). onAck was
//     called exactly once — reaching a graceful end is only possible after
//     a valid acknowledgement was already observed and acted on.
//   - An error satisfying errors.Is(err, ctx.Err()): ctx itself was
//     cancelled or its deadline exceeded. Checked ahead of every other
//     classification below, since it reflects the CALLER's own decision to
//     stop, not anything about the upstream. onAck may or may not have been
//     called, depending on how far the stream got before ctx ended.
//   - ErrListenIdle: the idle timeout elapsed with no further bytes from the
//     upstream. The idle timer starts the instant the response body begins
//     being read — covering the wait for the very first byte exactly like
//     Forward's identical use of the same idleTimeoutReader (see its own
//     doc) — so this can happen either before the ack itself ever arrived
//     (onAck NOT called) or after a valid acknowledgement was already
//     observed and acted on, then the upstream went quiet (onAck called).
//   - Any other error: a transport-level failure (connection refused, TLS
//     error, an unsolicited response Content-Encoding, an unexpected HTTP
//     status/content-type, a stream that closed without a graceful-end
//     response, or an unparseable event) — none of these carry upstream
//     response content in their text, per this package's
//     zero-knowledge-logging rule. onAck may or may not have been called:
//     this bucket covers both a failure before any acknowledgement was ever
//     reached, and a mid-stream failure well after one was.
//
// Listen never returns a nil error paired with a notifications/tools/list_changed
// event having been skipped — every such event that matches this call's own
// subscriptionId is delivered, in order, before Listen's own final return,
// since both happen on the same goroutine reading the same stream.
func (t *HTTPTransport) Listen(ctx context.Context, onAck func(), onToolsChanged func()) error {
	b, err := t.resolveBinding(ctx)
	if err != nil {
		return fmt.Errorf("subscriptions/listen: resolve protocol era: %w", err)
	}
	if b.dialect.Version().Era() != EraModern {
		return ErrListenUnsupported
	}

	reqRaw, err := jsonx.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      listenRequestID,
		"method":  "subscriptions/listen",
		"params": map[string]any{
			"notifications": map[string]any{
				"toolsListChanged": true,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("subscriptions/listen: marshal request: %w", err)
	}

	prepared, hdr, err := b.dialect.Prepare(&CallRequest{Raw: reqRaw}, nil)
	if err != nil {
		return fmt.Errorf("subscriptions/listen: prepare request: %w", err)
	}

	req, err := t.buildAuthedRequest(ctx, t.postTarget(b), prepared, hdr)
	if err != nil {
		return err
	}

	// streamCtx, not ctx: exactly like Forward, the idle timeout below must be
	// able to tear down THIS request specifically, without depending on — or
	// affecting — whatever deadline or cancellation ctx itself carries.
	streamCtx, streamCancel := context.WithCancel(ctx)
	req = req.WithContext(streamCtx)

	// idleFired distinguishes "the idle timer tore this stream down" from
	// "ctx itself was cancelled" for classifyErr below: ctx cancellation
	// propagates to streamCtx automatically as a WithCancel child and never
	// calls cancelForIdle. cancelForIdle is passed to newIdleTimeoutReader
	// below as its onIdle argument ONLY, never as closeCancel — see that
	// constructor's own doc — so this is now a STRUCTURAL guarantee, not one
	// that merely happens to hold because of this function's own return
	// order: idleTimeoutReader's Close (invoked for every return path via the
	// deferred body.Close() below, or explicitly in the non-SSE branch) calls
	// a separate, plain streamCancel instead, which never touches idleFired
	// at all. An atomic.Bool, not a plain bool: cancelForIdle runs on the
	// idle timer's own goroutine (time.AfterFunc — see newIdleTimeoutReader),
	// which can race with classifyErr reading it from whichever goroutine is
	// executing Listen itself.
	var idleFired atomic.Bool
	cancelForIdle := func() {
		idleFired.Store(true)
		streamCancel()
	}
	classifyErr := func(err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if idleFired.Load() {
			return ErrListenIdle
		}
		return fmt.Errorf("subscriptions/listen: stream ended unexpectedly: %w", err)
	}

	resp, err := t.streamClient.Do(req)
	if err != nil {
		streamCancel()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return fmt.Errorf("subscriptions/listen: transport: %w", err)
	}

	// Checked before anything else about the response, exactly like
	// Forward's identical call site (http_transport.go) — see
	// hasUnsolicitedContentEncoding's own doc for why this rejection applies
	// to Listen too, even though Listen never relays bytes to a downstream
	// client the way Forward does.
	if hasUnsolicitedContentEncoding(resp.Header) {
		resp.Body.Close() //nolint:errcheck // best-effort close; the body was never read
		streamCancel()
		return errUnsolicitedContentEncoding
	}

	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		resp.Body.Close() //nolint:errcheck // best-effort close; the body was never read
		streamCancel()
		return ErrListenUnsupported
	}

	contentType := resp.Header.Get("Content-Type")
	switch {
	case !isSSEContentType(contentType):
		// Wrapped in the same idle-timeout reader as the genuine SSE branch
		// below, and bounded exactly like rawPost's own ceiling (a
		// non-streaming response is never expected to be large): without the
		// idle wrapper, an upstream that sends headers with a non-streaming
		// Content-Type and then stalls or drips could pin this goroutine open
		// forever — ctx itself may carry no deadline at all here (see
		// runListener's own context.Background()-derived ctx, listen_manager.go).
		// closeCancel is streamCancel, not cancelForIdle: the body.Close()
		// calls below (both the readErr and success paths) must never mark
		// idleFired themselves — only the timer's own onIdle firing may (see
		// idleFired's own doc above).
		body := newIdleTimeoutReader(resp.Body, t.streamIdleTimeout, cancelForIdle, streamCancel)
		raw, readErr := io.ReadAll(io.LimitReader(body, rawPostMaxBodyBytes+1))
		if readErr != nil {
			err := classifyErr(readErr)
			body.Close() //nolint:errcheck // Close's own error carries no actionable information here; err above already reflects the read failure
			return err
		}
		body.Close() //nolint:errcheck // Close's own error carries no actionable information here; the body has already been fully consumed
		if int64(len(raw)) > rawPostMaxBodyBytes {
			return fmt.Errorf("subscriptions/listen: response body exceeds %d byte limit", rawPostMaxBodyBytes)
		}
		if isListenUnsupportedResponse(resp.StatusCode, contentType, raw) {
			return ErrListenUnsupported
		}
		return errListenUnexpectedResponse
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		resp.Body.Close() //nolint:errcheck // best-effort close; the body was never read
		streamCancel()
		return errListenUnexpectedResponse
	}

	// From here on the response is a genuine 2xx text/event-stream: Listen
	// owns reading it to completion (or to idle/ctx/failure) before
	// returning — unlike Forward, nothing is ever handed back to a caller to
	// read incrementally itself. closeCancel is streamCancel, not
	// cancelForIdle: the deferred body.Close() below runs on EVERY return
	// path, including a perfectly ordinary one (a graceful end, an
	// ErrListenNotHonored ack, ...), and must never mark idleFired itself —
	// see idleFired's own doc above.
	body := newIdleTimeoutReader(resp.Body, t.streamIdleTimeout, cancelForIdle, streamCancel)
	defer body.Close() //nolint:errcheck // Close's own error carries no actionable information here

	reader := newSSEEventReader(body)

	first, err := reader.Next()
	if err != nil {
		return classifyErr(err)
	}
	ack, err := parseListenMessage(first.Data)
	if err != nil {
		return errListenMalformedEvent
	}
	if !validateListenAck(ack) {
		return errListenMalformedEvent
	}
	if !ackHonorsToolsListChanged(ack.Params.Notifications) {
		return ErrListenNotHonored
	}
	onAck()

	for {
		ev, err := reader.Next()
		if err != nil {
			return classifyErr(err)
		}

		msg, err := parseListenMessage(ev.Data)
		if err != nil {
			return errListenMalformedEvent
		}
		// Checked before ANY method-based dispatch or skipping below,
		// including for a notification this loop would otherwise simply
		// ignore (e.g. an unrecognized method name): a wrong jsonrpc version
		// is itself already a protocol violation this package fails closed
		// on, never silently overlooked just because the message also
		// happened to name a method this loop does not act on.
		if msg.JSONRPC != jsonRPCVersion {
			return errListenMalformedEvent
		}

		switch classifyListenMessage(msg) {
		case listenMessageResponse:
			// This stream only ever carries THIS call's own
			// subscriptions/listen request-response pair, so any id other
			// than listenRequestID is already a protocol violation, not
			// merely "some other, harmless message to skip" — fail closed
			// rather than silently accept an upstream sending responses to
			// requests VoidLLM never made on this connection.
			if !subscriptionIDMatches(msg.ID) {
				return errListenMalformedEvent
			}
			if hasJSONValue(msg.Result) {
				// The graceful end (MCP 2026-07-28 §3.4) — regardless of
				// result's own content, see listenMessage's own doc for why
				// it is never read beyond presence.
				return nil
			}
			return errListenUpstreamError

		case listenMessageNotification:
			if msg.Method != methodToolsListChanged {
				// Some other notification (e.g. notifications/progress) or
				// an unrecognized method name — ignored, exactly like any
				// other field this package does not act on.
				continue
			}
			if !subscriptionIDMatches(msg.Params.Meta[metaSubscriptionID]) {
				continue
			}
			onToolsChanged()

		default:
			// Neither a well-formed notification nor a well-formed
			// response — both an id and a method present, neither present,
			// a notification carrying a result or error, or a response
			// carrying neither or both of result/error. Fails closed rather
			// than guessing which of the two shapes was intended.
			return errListenMalformedEvent
		}
	}
}

// jsonRPCVersion is the only "jsonrpc" field value Listen ever accepts, on
// the acknowledgement, every notification, and the graceful-end response
// alike (MCP 2026-07-28 §3.4, JSON-RPC 2.0 itself). Any other value —
// including the field's absence, which decodes to the zero value "" — fails
// closed with errListenMalformedEvent.
const jsonRPCVersion = "2.0"

// methodSubscriptionsAcknowledged is the JSON-RPC method name the first
// event on a subscriptions/listen stream MUST carry (MCP 2026-07-28 §3.4).
const methodSubscriptionsAcknowledged = "notifications/subscriptions/acknowledged"

// methodToolsListChanged is the JSON-RPC method name of the notification
// Listen's own dispatch loop acts on (MCP 2026-07-28 §3.4); any other method
// name is ignored (see Listen's own loop).
const methodToolsListChanged = "notifications/tools/list_changed"

// listenMessage is the minimal shape Listen decodes every event's "data:"
// payload into, via parseListenMessage. Result and Error are decoded as
// jsonx.RawMessage — never into typed fields — since Listen only ever
// checks their PRESENCE (see hasJSONValue), exactly like
// dialect2026Client.Parse's identical treatment of a modern-era response's
// own error field: this package's zero-knowledge-logging rule means neither
// field's actual content is ever retained or embedded in an error message
// here.
type listenMessage struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      jsonx.RawMessage `json:"id"`
	Method  string           `json:"method"`
	Params  struct {
		Meta          map[string]jsonx.RawMessage `json:"_meta"`
		Notifications map[string]jsonx.RawMessage `json:"notifications"`
	} `json:"params"`
	Result jsonx.RawMessage `json:"result"`
	Error  jsonx.RawMessage `json:"error"`
}

// parseListenMessage decodes data — one SSE event's "data:" payload — into
// its full listenMessage shape. This performs only syntactic JSON decoding;
// every field-level validation (jsonrpc version, id presence/absence,
// result/error exclusivity, subscriptionId matching, ...) is Listen's own
// responsibility (validateListenAck below, and Listen's own dispatch loop),
// since what counts as valid differs between the first (acknowledgement)
// event and every later one.
func parseListenMessage(data []byte) (listenMessage, error) {
	var msg listenMessage
	if err := jsonx.Unmarshal(data, &msg); err != nil {
		return listenMessage{}, err
	}
	return msg, nil
}

// validateListenAck reports whether msg is a well-formed subscriptions/listen
// acknowledgement (MCP 2026-07-28 §3.4): jsonrpc exactly "2.0", a well-formed
// JSON-RPC NOTIFICATION shape (see classifyListenMessage — in particular, no
// "id" member at all, not even an explicit null: JSON-RPC 2.0 never lets a
// notification carry one), method notifications/subscriptions/acknowledged,
// and its own params._meta.subscriptionId matching this call's own request
// id. Callers separately check ackHonorsToolsListChanged — an ack that fails
// THAT check is still well-formed, just not usable (see ErrListenNotHonored's
// own doc), which is why it is not folded into this function.
func validateListenAck(msg listenMessage) bool {
	if msg.JSONRPC != jsonRPCVersion {
		return false
	}
	if classifyListenMessage(msg) != listenMessageNotification {
		return false
	}
	if msg.Method != methodSubscriptionsAcknowledged {
		return false
	}
	return subscriptionIDMatches(msg.Params.Meta[metaSubscriptionID])
}

// listenMessageKind classifies an already jsonrpc-version-checked
// listenMessage as one of the two shapes JSON-RPC 2.0 defines, per
// classifyListenMessage's own doc.
type listenMessageKind int

const (
	// listenMessageInvalid is neither a well-formed notification nor a
	// well-formed response — see classifyListenMessage's own doc for every
	// shape that falls here.
	listenMessageInvalid listenMessageKind = iota
	// listenMessageNotification is a well-formed JSON-RPC 2.0 notification:
	// a "method" member, no "id" member at all, and neither "result" nor
	// "error".
	listenMessageNotification
	// listenMessageResponse is a well-formed JSON-RPC 2.0 response: an "id"
	// member (including an explicit null), no "method" member, and exactly
	// one of "result"/"error".
	listenMessageResponse
)

// classifyListenMessage reports whether msg — already confirmed to carry
// jsonrpc exactly "2.0" by every caller before this function is ever reached
// (see Listen's own loop and validateListenAck, both of which check that
// first) — is a well-formed JSON-RPC 2.0 notification, a well-formed
// response, or neither, per JSON-RPC 2.0's own structural definition of the
// two message shapes a subscriptions/listen stream ever carries (MCP
// 2026-07-28 §3.4):
//
//   - A notification has a "method" member, no "id" member AT ALL — not even
//     an explicit JSON null, which JSON-RPC 2.0 treats identically to any
//     other id value for THIS purpose: a notification is defined by the
//     member's total absence, never by its value (see hasIDMember's own doc
//     for how this is distinguished from an absent member) — and neither
//     "result" nor "error".
//   - A response has an "id" member (including an explicit null), no
//     "method" member, and EXACTLY ONE of "result"/"error" (hasJSONValue,
//     which treats an explicit null result/error identically to an absent
//     one, per this package's own existing convention — see that function's
//     doc).
//   - Anything else — both an id and a method present, neither present, a
//     notification-shaped message that also carries a result or error, or a
//     response-shaped message carrying neither or both of result/error — is
//     listenMessageInvalid: this package fails closed rather than guessing
//     which of the two shapes an ambiguous message was meant to be.
func classifyListenMessage(msg listenMessage) listenMessageKind {
	hasID := hasIDMember(msg.ID)
	hasMethod := msg.Method != ""
	hasResult := hasJSONValue(msg.Result)
	hasError := hasJSONValue(msg.Error)

	switch {
	case hasMethod && !hasID && !hasResult && !hasError:
		return listenMessageNotification
	case hasID && !hasMethod && hasResult != hasError:
		return listenMessageResponse
	default:
		return listenMessageInvalid
	}
}

// subscriptionIDMatches reports whether raw — a decoded event's own
// params._meta["io.modelcontextprotocol/subscriptionId"], or nil if the key
// was absent — equals listenIDRaw as a JSON value: byte-for-byte after
// trimming whitespace, exactly as sseEventMatches (http_transport.go)
// already compares a JSON-RPC id elsewhere in this package. listenRequestID
// is always sent as the same fixed JSON string, so a byte comparison against
// the literal listenIDRaw is exact; no numeric or string-vs-number
// normalization is needed.
func subscriptionIDMatches(raw jsonx.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), listenIDRaw)
}

// hasJSONValue reports whether raw is present and is not the JSON literal
// null — the same presence check dialect2026Client.Parse uses for a
// modern-era response's own error field (see that method's doc for why
// presence, not content, is what a caller of this package ever needs to
// know about an upstream-controlled error/result value).
func hasJSONValue(raw jsonx.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null"))
}

// hasIDMember reports whether raw represents a JSON object member that was
// actually PRESENT in the source document — including an explicit JSON
// null — as opposed to a member that was entirely absent. This is distinct
// from, and deliberately more permissive than, hasJSONValue: jsonx.Unmarshal
// (backed by encoding/json-compatible semantics — RawMessage's own
// UnmarshalJSON is invoked for a present member regardless of its value,
// including the literal null, but never at all when the member is
// syntactically absent) leaves raw at its nil zero value only when the "id"
// key never appeared at all, and sets it to the 4 bytes "null" when the key
// appeared with a null value — so raw != nil correctly distinguishes the
// two. classifyListenMessage relies on exactly this distinction for its own
// "id member present at all" rule (JSON-RPC 2.0 defines a notification by
// the id MEMBER's total absence, never by its value — unlike result/error,
// where an explicit null is conventionally treated the same as absent, per
// hasJSONValue's own doc).
func hasIDMember(raw jsonx.RawMessage) bool {
	return raw != nil
}

// ackHonorsToolsListChanged reports whether notifications — the ack's own
// params.notifications map — reports toolsListChanged as an honored filter:
// present and decodes to the JSON boolean true. Any other shape (absent,
// false, or a value that is not a JSON boolean at all) means the upstream
// did not honor VoidLLM's request for that specific notification type (MCP
// 2026-07-28 §3.4: "Nicht unterstützte Typen werden weggelassen").
func ackHonorsToolsListChanged(notifications map[string]jsonx.RawMessage) bool {
	raw, ok := notifications["toolsListChanged"]
	if !ok {
		return false
	}
	var honored bool
	if err := jsonx.Unmarshal(raw, &honored); err != nil {
		return false
	}
	return honored
}

// jsonRPCErrorCode is the minimal shape isJSONRPCMethodNotFound decodes a
// JSON-RPC error response's own "error" member into — only the numeric
// code, mirroring toolsListRPCErrorCode's identical reasoning
// (http_transport.go): neither Message nor Data, both upstream-controlled
// free-form content, is ever decoded into memory in the first place.
type jsonRPCErrorCode struct {
	Code int `json:"code"`
}

// isJSONRPCMethodNotFound reports whether body decodes as a FULLY VALID
// JSON-RPC 2.0 response — jsonrpc exactly "2.0", a well-formed response
// shape per classifyListenMessage (an "id" member, no "method", exactly one
// of "result"/"error"), that id equal to this call's own listenRequestID,
// and an "error" (never "result") whose code is CodeMethodNotFound (-32601)
// — the shape a server that does not implement subscriptions/listen answers
// with, per this method's own doc and MCP Streamable HTTP's ordinary
// "unknown method" convention. Any other shape — a wrong jsonrpc version, an
// id that does not match this call's own, a result present instead of an
// error, a message that is not even a well-formed response at all (both id
// and method present, or neither), or a body that fails to parse — reports
// false: see isListenUnsupportedResponse, this function's only caller, for
// why every one of these conditions must hold before a non-2xx-adjacent
// classification this permissive is ever trusted. This performs no
// status-code or content-type check of its own — those are
// isListenUnsupportedResponse's own responsibility, checked separately
// before this function's result is ever trusted.
func isJSONRPCMethodNotFound(body []byte) bool {
	msg, err := parseListenMessage(body)
	if err != nil {
		return false
	}
	if msg.JSONRPC != jsonRPCVersion {
		return false
	}
	if classifyListenMessage(msg) != listenMessageResponse {
		return false
	}
	if !subscriptionIDMatches(msg.ID) {
		return false
	}
	if !hasJSONValue(msg.Error) {
		// A well-formed response with hasResult != hasError already
		// guaranteed by listenMessageResponse — this branch is the "result
		// present, not error" half of that pair.
		return false
	}
	var errBody jsonRPCErrorCode
	if err := jsonx.Unmarshal(msg.Error, &errBody); err != nil {
		return false
	}
	return errBody.Code == CodeMethodNotFound
}

// isListenUnsupportedResponse reports whether a non-SSE subscriptions/listen
// response should be classified as ErrListenUnsupported: status must be 2xx,
// contentType must name application/json (ignoring parameters and case, like
// isSSEContentType), and body must be a well-formed JSON-RPC error response
// carrying CodeMethodNotFound (see isJSONRPCMethodNotFound). Every one of
// these three conditions is required — status and content type are checked
// here, never left to isJSONRPCMethodNotFound's own body-only decode — so
// that a 500 (or any other non-2xx status) whose body happens to carry
// -32601 is NOT misclassified as "this upstream doesn't support
// subscriptions/listen" (runListener's hour-long dont-retry bucket) when it
// is really an ordinary upstream failure (runListener's exponential-backoff
// bucket): reusing the retry-later bucket for a genuine 500 would mean a
// transient failure gets treated as a permanent capability gap, silencing
// reconnect attempts for an hour instead of retrying promptly with backoff.
func isListenUnsupportedResponse(statusCode int, contentType string, body []byte) bool {
	if statusCode < 200 || statusCode >= 300 {
		return false
	}
	if !isJSONContentType(contentType) {
		return false
	}
	return isJSONRPCMethodNotFound(body)
}

// isJSONContentType reports whether ct — a response's raw Content-Type
// header value — names the "application/json" media type, ignoring any
// parameters (e.g. "; charset=utf-8") and case — the same
// mime.ParseMediaType-based comparison isSSEContentType (http_transport.go)
// applies to "text/event-stream", for the identical reasons documented
// there. A ct that fails to parse at all reports false.
func isJSONContentType(ct string) bool {
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return strings.EqualFold(mediaType, "application/json")
}
