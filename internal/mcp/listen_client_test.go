package mcp_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers HTTPTransport.Listen (listen_client.go) — the client side
// of subscriptions/listen (MCP 2026-07-28 §3.4) — against real httptest
// upstreams. newModernTransport, newLegacyTransport, and testStreamIdleTimeout
// (http_transport_test.go, same package) are reused throughout, exactly as
// every other file in this package already does, so this file's transports
// carry the identical auth-configuration plumbing (bearer/header/oauth) that
// rawPost and Forward are already tested against.

// listenNewIdleTransport builds a modern-era-pinned HTTPTransport with a
// caller-supplied idle timeout, for the idle-timeout tests below that need to
// control it precisely instead of testStreamIdleTimeout's generous default.
func listenNewIdleTransport(endpoint string, idle time.Duration) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, idle)
}

// blockUntilCanceled is the standard shape every upstream handler below that
// needs to stay "connected" until the client tears it down uses: write
// whatever bytes the test needs, flush, then block on the request's own
// context until Listen (via ctx cancellation or its idle timer) ends it —
// exactly like forward_streaming_test.go's identical idiom for Forward.
func blockUntilCanceled(w http.ResponseWriter, r *http.Request, body string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	if body != "" {
		fmt.Fprint(w, body)
	}
	w.(http.Flusher).Flush()
	<-r.Context().Done()
}

// ---- SSE event fixtures -----------------------------------------------------
//
// listenRequestID (listen_client.go) is the fixed string "voidllm-listen"
// every request Listen sends carries as its own JSON-RPC id, and every
// subsequent event's params._meta subscriptionId (or a graceful-end/error
// response's own id) must echo to be recognized. These fixtures hardcode
// that exact value rather than importing it, since it is not itself exported
// — the request-shape test below independently verifies the outbound
// request actually carries it.

const listenSubID = "voidllm-listen"

func ackEvent(honored bool) string {
	return "event: message\n" +
		fmt.Sprintf(`data: {"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":%q},"notifications":{"toolsListChanged":%t}}}`, listenSubID, honored) +
		"\n\n"
}

const ackEventMissingNotifications = "event: message\n" +
	`data: {"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"voidllm-listen"},"notifications":{}}}` +
	"\n\n"

const ackEventWrongSubID = "event: message\n" +
	`data: {"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"some-other-id"},"notifications":{"toolsListChanged":true}}}` +
	"\n\n"

const ackEventWrongMethod = "event: message\n" +
	`data: {"jsonrpc":"2.0","method":"ping","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"voidllm-listen"},"notifications":{"toolsListChanged":true}}}` +
	"\n\n"

const listChangedEventCorrectSubID = "event: message\n" +
	`data: {"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"voidllm-listen"}}}` +
	"\n\n"

const listChangedEventWrongSubID = "event: message\n" +
	`data: {"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"someone-elses-subscription"}}}` +
	"\n\n"

const listChangedEventMissingSubID = "event: message\n" +
	`data: {"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{}}` +
	"\n\n"

const otherMethodEvent = "event: message\n" +
	`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"tok","progress":1}}` +
	"\n\n"

const gracefulEndEvent = "event: message\n" +
	`data: {"jsonrpc":"2.0","id":"voidllm-listen","result":{}}` +
	"\n\n"

// listenMarkerText is embedded in an upstream-controlled error/message field
// in several tests below, so the "errors/logs contain no upstream marker
// strings" assertions have something distinctive to look for.
const listenMarkerText = "SECRET-UPSTREAM-MARKER-9f3c1b2a"

func errorEndEventWithMarker() string {
	return "event: message\n" +
		fmt.Sprintf(`data: {"jsonrpc":"2.0","id":"voidllm-listen","error":{"code":-32000,"message":%q}}`, listenMarkerText) +
		"\n\n"
}

const malformedEventNotJSON = "event: message\n" +
	"data: this is not json at all\n\n"

// ---- Request shape: method, params, _meta, headers, auth -------------------

// TestListen_RequestShape_MethodParamsMetaHeadersAuth verifies the exact
// outbound request Listen builds: JSON-RPC method subscriptions/listen,
// params.notifications.toolsListChanged == true, a modern dialect's
// params._meta (protocolVersion/clientCapabilities/clientInfo), the standard
// MCP-Protocol-Version and Mcp-Method request headers, and the transport's
// own configured Authorization header — reusing newModernTransport's
// "bearer" auth exactly as rawPost/Forward's own equivalent tests do.
func TestListen_RequestShape_MethodParamsMetaHeadersAuth(t *testing.T) {
	t.Parallel()

	const bearerToken = "listen-shape-token"

	var (
		mu      sync.Mutex
		gotBody map[string]any
		gotHdr  http.Header
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(raw, &decoded)
		mu.Lock()
		gotBody = decoded
		gotHdr = r.Header.Clone()
		mu.Unlock()

		blockUntilCanceled(w, r, ackEvent(true))
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "bearer", "", bearerToken)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := tr.Listen(ctx, cancel, func() {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Listen() error = %v, want context.Canceled (onAck cancels ctx to end the test cleanly)", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if gotBody["method"] != "subscriptions/listen" {
		t.Errorf("method = %v, want %q", gotBody["method"], "subscriptions/listen")
	}
	params, _ := gotBody["params"].(map[string]any)
	if params == nil {
		t.Fatal("params missing from request body")
	}
	notifications, _ := params["notifications"].(map[string]any)
	if notifications["toolsListChanged"] != true {
		t.Errorf("params.notifications.toolsListChanged = %v, want true", notifications["toolsListChanged"])
	}
	meta, _ := params["_meta"].(map[string]any)
	if meta == nil {
		t.Fatal("params._meta missing from request body")
	}
	for _, key := range []string{
		"io.modelcontextprotocol/protocolVersion",
		"io.modelcontextprotocol/clientCapabilities",
		"io.modelcontextprotocol/clientInfo",
	} {
		if _, ok := meta[key]; !ok {
			t.Errorf("params._meta missing key %q; got %v", key, meta)
		}
	}

	if got := gotHdr.Get("MCP-Protocol-Version"); got != "2026-07-28" {
		t.Errorf("MCP-Protocol-Version header = %q, want %q", got, "2026-07-28")
	}
	if got := gotHdr.Get("Mcp-Method"); got != "subscriptions/listen" {
		t.Errorf("Mcp-Method header = %q, want %q", got, "subscriptions/listen")
	}
	if got := gotHdr.Get("Authorization"); got != "Bearer "+bearerToken {
		t.Errorf("Authorization header = %q, want %q", got, "Bearer "+bearerToken)
	}
}

// TestListen_RequestShape_HeaderAuthType covers the authType "header"
// configuration (a custom header name carrying the credential), the other
// half of rawPost/Forward's own auth matrix this transport supports.
func TestListen_RequestShape_HeaderAuthType(t *testing.T) {
	t.Parallel()

	const headerName = "X-Api-Key"
	const token = "listen-header-token"

	authHdr := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHdr <- r.Header.Get(headerName)
		blockUntilCanceled(w, r, ackEvent(true))
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "header", headerName, token)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := tr.Listen(ctx, cancel, func() {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Listen() error = %v, want context.Canceled", err)
	}

	select {
	case got := <-authHdr:
		if got != token {
			t.Errorf("%s header = %q, want %q", headerName, got, token)
		}
	default:
		t.Fatal("upstream never received a request")
	}
}

// ---- Ack -> tools/list_changed --------------------------------------------

// TestListen_Ack_ThenListChanged_OnToolsChangedCalledAfterOnAck verifies the
// core notification-dispatch behavior: onAck runs exactly once, synchronously,
// before onToolsChanged is ever invoked for a subsequent
// notifications/tools/list_changed event carrying the same subscriptionId.
func TestListen_Ack_ThenListChanged_OnToolsChangedCalledAfterOnAck(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockUntilCanceled(w, r, ackEvent(true)+listChangedEventCorrectSubID)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu               sync.Mutex
		ackCalled        bool
		toolsChangedCall int
		ackBeforeChange  bool
	)

	err := tr.Listen(ctx,
		func() {
			mu.Lock()
			ackCalled = true
			mu.Unlock()
		},
		func() {
			mu.Lock()
			toolsChangedCall++
			ackBeforeChange = ackCalled
			mu.Unlock()
			cancel()
		})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Listen() error = %v, want context.Canceled", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if !ackCalled {
		t.Error("onAck was never called")
	}
	if toolsChangedCall != 1 {
		t.Errorf("onToolsChanged called %d times, want 1", toolsChangedCall)
	}
	if !ackBeforeChange {
		t.Error("onToolsChanged fired before onAck had been called")
	}
}

// TestListen_NotificationWrongSubscriptionID_Ignored verifies that a
// notifications/tools/list_changed event whose subscriptionId does not match
// this call's own is silently ignored — never triggers onToolsChanged, never
// ends Listen with an error.
func TestListen_NotificationWrongSubscriptionID_Ignored(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockUntilCanceled(w, r, ackEvent(true)+listChangedEventWrongSubID+listChangedEventCorrectSubID)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	err := tr.Listen(ctx, func() {}, func() {
		calls.Add(1)
		cancel()
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Listen() error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("onToolsChanged called %d times, want exactly 1 (the wrong-subscriptionId event must be ignored)", got)
	}
}

// TestListen_NotificationMissingSubscriptionID_Ignored is the same property
// for an event whose params._meta carries no subscriptionId key at all,
// rather than a mismatched one.
func TestListen_NotificationMissingSubscriptionID_Ignored(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockUntilCanceled(w, r, ackEvent(true)+listChangedEventMissingSubID+listChangedEventCorrectSubID)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	err := tr.Listen(ctx, func() {}, func() {
		calls.Add(1)
		cancel()
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Listen() error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("onToolsChanged called %d times, want exactly 1", got)
	}
}

// TestListen_OtherMethodEvent_Ignored verifies that an event whose method is
// neither notifications/tools/list_changed nor the graceful-end response's
// own id is simply skipped, without error and without triggering either
// callback.
func TestListen_OtherMethodEvent_Ignored(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockUntilCanceled(w, r, ackEvent(true)+otherMethodEvent+listChangedEventCorrectSubID)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls atomic.Int32
	err := tr.Listen(ctx, func() {}, func() {
		calls.Add(1)
		cancel()
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Listen() error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("onToolsChanged called %d times, want exactly 1 (the notifications/progress event must be ignored)", got)
	}
}

// ---- Graceful end / upstream error -----------------------------------------

// TestListen_GracefulEnd_ReturnsNil verifies MCP 2026-07-28 §3.4's "Graceful
// Closure": a JSON-RPC response whose id matches the subscriptions/listen
// request's own id, carrying a result rather than an error, ends Listen with
// a nil error — after onAck has already run.
func TestListen_GracefulEnd_ReturnsNil(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, ackEvent(true))
		fmt.Fprint(w, gracefulEndEvent)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")

	var ackCalled bool
	err := tr.Listen(context.Background(), func() { ackCalled = true }, func() {})
	if err != nil {
		t.Fatalf("Listen() error = %v, want nil (graceful end)", err)
	}
	if !ackCalled {
		t.Error("onAck was never called before the graceful end")
	}
}

// TestListen_ErrorResponseWithOurID_ReturnsError verifies that a JSON-RPC
// response whose id matches but carries an error (not a result) ends Listen
// with a non-nil error distinct from the graceful-end nil case, and that the
// upstream's own error message never appears in the returned error's text
// (this package's zero-knowledge-logging rule).
func TestListen_ErrorResponseWithOurID_ReturnsError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, ackEvent(true))
		fmt.Fprint(w, errorEndEventWithMarker())
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")

	err := tr.Listen(context.Background(), func() {}, func() {})
	if err == nil {
		t.Fatal("Listen() error = nil, want a non-nil error for an upstream-carried JSON-RPC error response")
	}
	if strings.Contains(err.Error(), listenMarkerText) {
		t.Errorf("error text %q contains the upstream's own error message marker — zero-knowledge-logging violation", err.Error())
	}
}

// ---- Non-ack first event ----------------------------------------------------

// TestListen_NonAckFirstEvent_ErrorAndOnAckNeverCalled covers three shapes of
// a first event that is not a valid acknowledgement: wrong method, wrong
// subscriptionId, and outright malformed (non-JSON) data. In every case
// Listen must return a non-nil error and onAck must never run.
func TestListen_NonAckFirstEvent_ErrorAndOnAckNeverCalled(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		firstEvent string
	}{
		{"wrong_method", ackEventWrongMethod},
		{"wrong_subscription_id", ackEventWrongSubID},
		{"malformed_json", malformedEventNotJSON},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				blockUntilCanceled(w, r, tc.firstEvent)
			}))
			t.Cleanup(srv.Close)

			tr := newModernTransport(srv.URL, "none", "", "")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			var ackCalled bool
			done := make(chan error, 1)
			go func() {
				done <- tr.Listen(ctx, func() { ackCalled = true }, func() {})
			}()

			select {
			case err := <-done:
				if !errors.Is(err, mcp.ErrListenMalformedEventForTest) {
					t.Errorf("Listen() error = %v, want errListenMalformedEvent", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Listen() did not return within 5s")
			}
			if ackCalled {
				t.Error("onAck was called for a non-ack first event")
			}
		})
	}
}

// ---- Ack without honoring toolsListChanged ---------------------------------

// TestListen_AckWithoutToolsListChanged_ErrListenNotHonored verifies
// ErrListenNotHonored for both an explicit "toolsListChanged": false and an
// ack whose notifications object omits the key entirely — either way, onAck
// must never be called.
func TestListen_AckWithoutToolsListChanged_ErrListenNotHonored(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		ack  string
	}{
		{"explicit_false", ackEvent(false)},
		{"key_omitted", ackEventMissingNotifications},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				blockUntilCanceled(w, r, tc.ack)
			}))
			t.Cleanup(srv.Close)

			tr := newModernTransport(srv.URL, "none", "", "")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			var ackCalled bool
			err := tr.Listen(ctx, func() { ackCalled = true }, func() {})
			if !errors.Is(err, mcp.ErrListenNotHonored) {
				t.Fatalf("Listen() error = %v, want ErrListenNotHonored", err)
			}
			if ackCalled {
				t.Error("onAck was called even though the ack did not honor toolsListChanged")
			}
		})
	}
}

// ---- Unsupported upstream: 404, 405, JSON-RPC -32601 -----------------------

// TestListen_UnsupportedUpstream_404_405_MethodNotFound covers every shape
// ErrListenUnsupported's own doc names: an HTTP 404 or 405 response, and an
// HTTP 200 JSON-RPC error response carrying CodeMethodNotFound.
func TestListen_UnsupportedUpstream_404_405_MethodNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"http_404", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		}},
		{"http_405", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}},
		{"json_rpc_method_not_found", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"voidllm-listen","error":{"code":%d,"message":"method not found"}}`, mcp.CodeMethodNotFound)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			tr := newModernTransport(srv.URL, "none", "", "")
			err := tr.Listen(context.Background(), func() {}, func() {})
			if !errors.Is(err, mcp.ErrListenUnsupported) {
				t.Errorf("Listen() error = %v, want ErrListenUnsupported", err)
			}
		})
	}
}

// TestListen_LegacyTransport_ErrListenUnsupported_ZeroRequests verifies that
// a transport whose resolved binding is legacy era returns
// ErrListenUnsupported immediately, before sending anything at all — the
// request counter proves it, not just the returned error.
func TestListen_LegacyTransport_ErrListenUnsupported_ZeroRequests(t *testing.T) {
	t.Parallel()

	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")
	err := tr.Listen(context.Background(), func() {}, func() {})
	if !errors.Is(err, mcp.ErrListenUnsupported) {
		t.Fatalf("Listen() error = %v, want ErrListenUnsupported", err)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("upstream received %d requests, want 0 — a legacy binding must never send subscriptions/listen at all", got)
	}
}

// ---- Other unexpected responses --------------------------------------------

// TestListen_OtherJSONBody_ErrListenUnexpectedResponse covers an HTTP 200
// JSON-RPC error response whose code is NOT CodeMethodNotFound — a shape
// that is neither a valid acknowledgement nor "this upstream doesn't support
// the method", and must not be silently reinterpreted as either.
func TestListen_OtherJSONBody_ErrListenUnexpectedResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"voidllm-listen","error":{"code":-32000,"message":%q}}`, listenMarkerText)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	err := tr.Listen(context.Background(), func() {}, func() {})
	if !errors.Is(err, mcp.ErrListenUnexpectedResponseForTest) {
		t.Fatalf("Listen() error = %v, want errListenUnexpectedResponse", err)
	}
	if strings.Contains(err.Error(), listenMarkerText) {
		t.Errorf("error text %q contains the upstream's own error message marker", err.Error())
	}
}

// TestListen_500WithSSEContentType_ErrListenUnexpectedResponse covers a
// non-2xx status paired with a genuine text/event-stream content type — the
// other branch of the same guard.
func TestListen_500WithSSEContentType_ErrListenUnexpectedResponse(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	err := tr.Listen(context.Background(), func() {}, func() {})
	if !errors.Is(err, mcp.ErrListenUnexpectedResponseForTest) {
		t.Fatalf("Listen() error = %v, want errListenUnexpectedResponse", err)
	}
}

// ---- Content-Encoding -------------------------------------------------------

// TestListen_UnsupportedContentEncoding_Rejected verifies that a response
// carrying an encoding Go's net/http.Transport does NOT transparently
// decompress (see hasUnsolicitedContentEncoding's own doc: only a literal
// "gzip" response is ever auto-handled) is rejected with
// errUnsolicitedContentEncoding, and that the upstream's own header value
// never appears in the returned error's text.
func TestListen_UnsupportedContentEncoding_Rejected(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, ackEvent(true))
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	err := tr.Listen(context.Background(), func() {}, func() {})
	if !errors.Is(err, mcp.ErrUnsolicitedContentEncodingForTest) {
		t.Fatalf("Listen() error = %v, want errUnsolicitedContentEncoding", err)
	}
	if strings.Contains(err.Error(), "br") {
		t.Errorf("error text %q contains the upstream's own Content-Encoding value", err.Error())
	}
}

// TestListen_GzipContentEncoding_TransparentlyDecompressed_NotRejected
// documents the flip side of the same guard, per hasUnsolicitedContentEncoding's
// own doc: Go's net/http.Transport itself adds "Accept-Encoding: gzip" on an
// outbound request that sets none (both Forward and Listen set none), and
// for a response whose Content-Encoding is exactly "gzip" it transparently
// decompresses the body and deletes the response header before RoundTrip
// even returns — so a literal gzip-encoded response is never seen by
// hasUnsolicitedContentEncoding at all and must NOT be rejected. This is the
// reason the sibling test above uses "br", not "gzip", to demonstrate
// rejection.
func TestListen_GzipContentEncoding_TransparentlyDecompressed_NotRejected(t *testing.T) {
	t.Parallel()

	var gzipped bytes.Buffer
	gz := gzip.NewWriter(&gzipped)
	if _, err := gz.Write([]byte(ackEvent(true))); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(gzipped.Bytes())
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	// A short idle timeout, not ctx cancellation, ends this test: what matters
	// here is only that the ack was reached at all (proving the gzip body was
	// transparently decompressed into a valid event) and that Listen never
	// classified it as errUnsolicitedContentEncoding — not how quickly the
	// stream itself ends afterward.
	const idle = 150 * time.Millisecond
	tr := listenNewIdleTransport(srv.URL, idle)

	var ackCalled bool
	err := tr.Listen(context.Background(), func() { ackCalled = true }, func() {})
	if errors.Is(err, mcp.ErrUnsolicitedContentEncodingForTest) {
		t.Fatalf("Listen() error = %v, want anything but errUnsolicitedContentEncoding (a literal gzip response must be transparently decompressed, not rejected)", err)
	}
	if !ackCalled {
		t.Error("onAck was never called — the gzip-compressed ack event was not successfully decoded")
	}
}

// ---- Malformed later event ---------------------------------------------------

// TestListen_MalformedLaterEvent_ErrorAfterAck verifies that a malformed
// event occurring AFTER a valid acknowledgement (as opposed to as the first
// event, covered above) also ends Listen with an error, with onAck already
// having run.
func TestListen_MalformedLaterEvent_ErrorAfterAck(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockUntilCanceled(w, r, ackEvent(true)+malformedEventNotJSON)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	var ackCalled bool
	err := tr.Listen(ctx, func() { ackCalled = true }, func() {})
	if !errors.Is(err, mcp.ErrListenMalformedEventForTest) {
		t.Fatalf("Listen() error = %v, want errListenMalformedEvent", err)
	}
	if !ackCalled {
		t.Error("onAck should have been called before the malformed event was reached")
	}
}

// ---- Idle timeout: before and after ack ------------------------------------

// TestListen_IdleTimeout_BeforeAck verifies ErrListenIdle fires when the
// upstream sends headers (and flushes) but never writes a single event —
// onAck must never be called.
func TestListen_IdleTimeout_BeforeAck(t *testing.T) {
	t.Parallel()

	upstreamDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamDone)
	}))
	t.Cleanup(srv.Close)

	const idle = 150 * time.Millisecond
	tr := listenNewIdleTransport(srv.URL, idle)

	var ackCalled bool
	err := tr.Listen(context.Background(), func() { ackCalled = true }, func() {})
	if !errors.Is(err, mcp.ErrListenIdle) {
		t.Fatalf("Listen() error = %v, want ErrListenIdle", err)
	}
	if ackCalled {
		t.Error("onAck was called even though the upstream never sent an acknowledgement")
	}

	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Error("upstream handler's request context was never cancelled — the idle timer did not tear down the connection")
	}
}

// TestListen_IdleTimeout_AfterAck verifies ErrListenIdle also fires once the
// upstream has acknowledged and then gone silent — onAck must have been
// called exactly once by the time Listen returns.
func TestListen_IdleTimeout_AfterAck(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockUntilCanceled(w, r, ackEvent(true))
	}))
	t.Cleanup(srv.Close)

	const idle = 150 * time.Millisecond
	tr := listenNewIdleTransport(srv.URL, idle)

	var ackCount atomic.Int32
	err := tr.Listen(context.Background(), func() { ackCount.Add(1) }, func() {})
	if !errors.Is(err, mcp.ErrListenIdle) {
		t.Fatalf("Listen() error = %v, want ErrListenIdle", err)
	}
	if got := ackCount.Load(); got != 1 {
		t.Errorf("onAck called %d times, want exactly 1", got)
	}
}

// ---- Context cancellation ----------------------------------------------------

// TestListen_CtxCancel_ReturnsCtxError verifies that cancelling ctx from an
// independent goroutine — synchronized via a channel the onAck callback
// closes, never a sleep — ends Listen with ctx.Err(), distinguished from
// ErrListenIdle.
func TestListen_CtxCancel_ReturnsCtxError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blockUntilCanceled(w, r, ackEvent(true))
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	ctx, cancel := context.WithCancel(context.Background())

	acked := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- tr.Listen(ctx, func() { close(acked) }, func() {})
	}()

	select {
	case <-acked:
	case <-time.After(5 * time.Second):
		t.Fatal("onAck was never called")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Listen() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Listen() did not return within 5s of ctx cancellation")
	}
}

// ---- Zero-knowledge logging: no upstream content in errors -----------------

// TestListen_ErrorTexts_NeverContainUpstreamMarker sweeps every error-return
// path this file otherwise exercises individually and re-checks the same
// property in one place: none of Listen's returned errors ever embed
// upstream-controlled content, using a single distinctive marker planted in
// several different upstream-controlled fields.
func TestListen_ErrorTexts_NeverContainUpstreamMarker(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"error_response_message", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, ackEvent(true))
			fmt.Fprint(w, errorEndEventWithMarker())
			w.(http.Flusher).Flush()
		}},
		{"malformed_first_event_with_marker", func(w http.ResponseWriter, r *http.Request) {
			blockUntilCanceled(w, r, "event: message\ndata: "+listenMarkerText+" not json\n\n")
		}},
		{"unexpected_json_body_with_marker", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"voidllm-listen","error":{"code":-32000,"message":%q}}`, listenMarkerText)
		}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(tc.handler)
			t.Cleanup(srv.Close)

			tr := newModernTransport(srv.URL, "none", "", "")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			done := make(chan error, 1)
			go func() { done <- tr.Listen(ctx, func() {}, func() {}) }()

			select {
			case err := <-done:
				if err == nil {
					t.Fatal("Listen() error = nil, want a non-nil error")
				}
				if strings.Contains(err.Error(), listenMarkerText) {
					t.Errorf("error text %q contains the upstream marker", err.Error())
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Listen() did not return within 5s")
			}
		})
	}
}

// ---- Idle timeout on the non-SSE (JSON) response branch --------------------

// TestListen_NonSSEResponse_StallsAfterHeaders_HitsIdleTimeout verifies that
// an upstream answering with a non-streaming Content-Type, but then never
// completing its body (headers sent and flushed, then silence), is bounded
// by the same idle timeout the SSE branch already enforces — not left to
// block forever on ctx alone, which may carry no deadline of its own (see
// runListener's context.Background()-derived ctx, listen_manager.go).
func TestListen_NonSSEResponse_StallsAfterHeaders_HitsIdleTimeout(t *testing.T) {
	t.Parallel()

	upstreamDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamDone)
	}))
	t.Cleanup(srv.Close)

	const idle = 150 * time.Millisecond
	tr := listenNewIdleTransport(srv.URL, idle)

	err := tr.Listen(context.Background(), func() {}, func() {})
	if !errors.Is(err, mcp.ErrListenIdle) {
		t.Fatalf("Listen() error = %v, want ErrListenIdle", err)
	}

	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Error("upstream handler's request context was never cancelled — the idle timer did not bound the non-SSE read")
	}
}

// TestListen_NonSSEResponse_DripFeed_HitsIdleTimeout is the "drips slowly"
// counterpart to the stall test above: the upstream DOES make forward
// progress (one byte at a time), just too slowly to ever complete the body
// before the idle timeout — proving the bound is a genuine per-read idle
// timer, not merely "give up if nothing at all ever arrives".
func TestListen_NonSSEResponse_DripFeed_HitsIdleTimeout(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		body := `{"jsonrpc":"2.0"}`
		for i := 0; i < len(body); i++ {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(30 * time.Millisecond):
			}
			_, _ = w.Write([]byte{body[i]})
			flusher.Flush()
		}
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	// idle shorter than the drip interval: the connection never goes truly
	// idle relative to a GENEROUS timeout, but must still trip THIS one,
	// since it is tighter than the drip rate itself.
	const idle = 10 * time.Millisecond
	tr := listenNewIdleTransport(srv.URL, idle)

	err := tr.Listen(context.Background(), func() {}, func() {})
	if !errors.Is(err, mcp.ErrListenIdle) {
		t.Fatalf("Listen() error = %v, want ErrListenIdle", err)
	}
}

// ---- idleFired: only the timer, never a normal Close ------------------------

// TestListen_ClosePaths_NeverMisreportIdle_SSEAndNonSSE is the black-box,
// Listen-level companion to TestIdleTimeoutReader_Close_NeverInvokesOnIdle
// (idle_timeout_reader_test.go, the direct white-box proof of item 4's own
// structural separation): every return path exercised here runs Close (via
// the SSE branch's deferred body.Close(), or the non-SSE branch's explicit
// ones) well after a SUCCESSFUL read that never involved the idle timer at
// all, and neither ends Listen with ErrListenIdle — a regression test for
// the observable surface of the guarantee, complementing (not replacing) the
// white-box proof that Close structurally cannot invoke onIdle at all.
func TestListen_ClosePaths_NeverMisreportIdle_SSEAndNonSSE(t *testing.T) {
	t.Parallel()

	t.Run("sse_branch_malformed_later_event_after_close", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			blockUntilCanceled(w, r, ackEvent(true)+malformedEventNotJSON)
		}))
		t.Cleanup(srv.Close)

		tr := newModernTransport(srv.URL, "none", "", "")
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		err := tr.Listen(ctx, func() {}, func() {})
		if !errors.Is(err, mcp.ErrListenMalformedEventForTest) {
			t.Fatalf("Listen() error = %v, want errListenMalformedEvent", err)
		}
		if errors.Is(err, mcp.ErrListenIdle) {
			t.Error("Listen() misreported ErrListenIdle for an ordinary malformed-event failure — the deferred Close must never itself mark idle")
		}
	})

	t.Run("non_sse_branch_unsupported_classification_after_close", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"voidllm-listen","error":{"code":%d,"message":"nope"}}`, mcp.CodeMethodNotFound)
		}))
		t.Cleanup(srv.Close)

		tr := newModernTransport(srv.URL, "none", "", "")
		err := tr.Listen(context.Background(), func() {}, func() {})
		if !errors.Is(err, mcp.ErrListenUnsupported) {
			t.Fatalf("Listen() error = %v, want ErrListenUnsupported", err)
		}
		if errors.Is(err, mcp.ErrListenIdle) {
			t.Error("Listen() misreported ErrListenIdle for a successful unsupported classification — Close must never itself mark idle")
		}
	})
}

// ---- Unsupported classification: status and content-type both matter ------

// TestListen_500WithMethodNotFoundBody_NotUnsupported verifies that a
// non-2xx status carrying CodeMethodNotFound in its body is NOT classified
// as ErrListenUnsupported — only a genuine 2xx JSON-RPC error response
// carrying that code is (see ErrListenUnsupported's own doc). Conflating the
// two would route an ordinary upstream failure into runListener's
// hour-long, dont-retry-for-an-hour bucket instead of its ordinary
// exponential-backoff one.
func TestListen_500WithMethodNotFoundBody_NotUnsupported(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"voidllm-listen","error":{"code":%d,"message":"nope"}}`, mcp.CodeMethodNotFound)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	err := tr.Listen(context.Background(), func() {}, func() {})
	if errors.Is(err, mcp.ErrListenUnsupported) {
		t.Fatalf("Listen() error = %v, want anything but ErrListenUnsupported (a 500 is an ordinary failure, not proof of unsupported)", err)
	}
	if err == nil {
		t.Fatal("Listen() error = nil, want a non-nil error")
	}
}

// TestListen_200NonJSONContentType_MethodNotFoundBody_NotUnsupported covers
// the other half of the same guard: a 2xx status whose Content-Type is NOT
// application/json must not be classified as unsupported either, even if its
// body happens to parse as a -32601 JSON-RPC error.
func TestListen_200NonJSONContentType_MethodNotFoundBody_NotUnsupported(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":"voidllm-listen","error":{"code":%d,"message":"nope"}}`, mcp.CodeMethodNotFound)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")
	err := tr.Listen(context.Background(), func() {}, func() {})
	if errors.Is(err, mcp.ErrListenUnsupported) {
		t.Fatalf("Listen() error = %v, want anything but ErrListenUnsupported (Content-Type is not application/json)", err)
	}
	if err == nil {
		t.Fatal("Listen() error = nil, want a non-nil error")
	}
}

// TestListen_UnsupportedClassification_StrictJSONRPC_NotUnsupported covers
// every shape of a 2xx application/json response body that carries
// CodeMethodNotFound (-32601) — otherwise exactly the shape
// TestListen_UnsupportedUpstream_404_405_MethodNotFound's own
// json_rpc_method_not_found case classifies as ErrListenUnsupported — but
// fails isJSONRPCMethodNotFound's own strict JSON-RPC 2.0 response
// validation for some OTHER reason: a wrong jsonrpc version, an id that does
// not match this call's own listenRequestID, a body carrying BOTH result and
// error at once, and an explicit "id": null. Per isJSONRPCMethodNotFound's
// own doc, only a FULLY valid JSON-RPC 2.0 response counts as unsupported —
// every one of these near-misses must instead end Listen with an ordinary
// unexpected-response error, never ErrListenUnsupported and never nil.
func TestListen_UnsupportedClassification_StrictJSONRPC_NotUnsupported(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body string
	}{
		{"wrong_jsonrpc_version", fmt.Sprintf(`{"jsonrpc":"1.0","id":"voidllm-listen","error":{"code":%d,"message":"nope"}}`, mcp.CodeMethodNotFound)},
		{"wrong_id", fmt.Sprintf(`{"jsonrpc":"2.0","id":"some-other-request","error":{"code":%d,"message":"nope"}}`, mcp.CodeMethodNotFound)},
		{"result_and_error_both_present", fmt.Sprintf(`{"jsonrpc":"2.0","id":"voidllm-listen","result":{},"error":{"code":%d,"message":"nope"}}`, mcp.CodeMethodNotFound)},
		{"id_null", fmt.Sprintf(`{"jsonrpc":"2.0","id":null,"error":{"code":%d,"message":"nope"}}`, mcp.CodeMethodNotFound)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				fmt.Fprint(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			tr := newModernTransport(srv.URL, "none", "", "")
			err := tr.Listen(context.Background(), func() {}, func() {})
			if errors.Is(err, mcp.ErrListenUnsupported) {
				t.Fatalf("Listen() error = %v, want anything but ErrListenUnsupported", err)
			}
			if !errors.Is(err, mcp.ErrListenUnexpectedResponseForTest) {
				t.Fatalf("Listen() error = %v, want errListenUnexpectedResponse", err)
			}
		})
	}
}

// ---- Strict JSON-RPC validation --------------------------------------------

const ackEventWrongJSONRPCVersion = "event: message\n" +
	`data: {"jsonrpc":"1.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"voidllm-listen"},"notifications":{"toolsListChanged":true}}}` +
	"\n\n"

const ackEventCarriesID = "event: message\n" +
	`data: {"jsonrpc":"2.0","id":1,"method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"voidllm-listen"},"notifications":{"toolsListChanged":true}}}` +
	"\n\n"

// ackEventIDNull is the specific "carries an id" shape that hasJSONValue
// alone would have missed before hasIDMember existed: an EXPLICIT JSON null
// id member is still a present member — JSON-RPC 2.0 defines a notification
// by the id member's total absence, never by its value — so this must be
// rejected exactly like ackEventCarriesID's non-null id, not silently
// accepted as "no real id here".
const ackEventIDNull = "event: message\n" +
	`data: {"jsonrpc":"2.0","id":null,"method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"voidllm-listen"},"notifications":{"toolsListChanged":true}}}` +
	"\n\n"

const listChangedEventWrongJSONRPCVersion = "event: message\n" +
	`data: {"jsonrpc":"1.0","method":"notifications/tools/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"voidllm-listen"}}}` +
	"\n\n"

// listChangedEventIDNull is the "id" member's null-value case for a LATER
// notification, the sibling of ackEventIDNull above for Listen's own
// dispatch loop rather than the first-event-only validateListenAck.
const listChangedEventIDNull = "event: message\n" +
	`data: {"jsonrpc":"2.0","id":null,"method":"notifications/tools/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"voidllm-listen"}}}` +
	"\n\n"

// unknownNotificationEventWrongJSONRPCVersion is the direct regression
// fixture for item 2's own first rule: jsonrpc must be checked for EVERY
// message before any method-based dispatch or skipping. Before that fix,
// this event's method (notifications/progress, not
// methodToolsListChanged) would have taken the "ignored" branch before the
// jsonrpc version was ever inspected — silently overlooking the version
// violation rather than failing closed on it.
const unknownNotificationEventWrongJSONRPCVersion = "event: message\n" +
	`data: {"jsonrpc":"1.0","method":"notifications/progress","params":{"progressToken":"tok","progress":1}}` +
	"\n\n"

// responseEventWithMethod is a message carrying BOTH an id (this call's own)
// and a method — neither a well-formed notification (which carries no id at
// all) nor a well-formed response (which carries no method) per JSON-RPC
// 2.0's own structural definition of the two shapes.
const responseEventWithMethod = "event: message\n" +
	`data: {"jsonrpc":"2.0","id":"voidllm-listen","method":"notifications/tools/list_changed","result":{}}` +
	"\n\n"

const gracefulEndEventWrongJSONRPCVersion = "event: message\n" +
	`data: {"jsonrpc":"1.0","id":"voidllm-listen","result":{}}` +
	"\n\n"

const gracefulEndEventNeitherResultNorError = "event: message\n" +
	`data: {"jsonrpc":"2.0","id":"voidllm-listen"}` +
	"\n\n"

const gracefulEndEventBothResultAndError = "event: message\n" +
	`data: {"jsonrpc":"2.0","id":"voidllm-listen","result":{},"error":{"code":-32000,"message":"x"}}` +
	"\n\n"

const responseEventWithForeignID = "event: message\n" +
	`data: {"jsonrpc":"2.0","id":"some-other-request","result":{}}` +
	"\n\n"

// TestListen_Ack_StrictValidation_Rejected covers every shape of a
// first event that fails validateListenAck's own strict checks (jsonrpc
// version, and no "id" member at all — including an explicit null, which is
// still a present member — since an acknowledgement is a JSON-RPC
// notification, which never carries one): every case ends Listen with
// errListenMalformedEvent, and onAck must never run.
func TestListen_Ack_StrictValidation_Rejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		firstEvent string
	}{
		{"wrong_jsonrpc_version", ackEventWrongJSONRPCVersion},
		{"carries_an_id", ackEventCarriesID},
		{"carries_an_id_null", ackEventIDNull},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				blockUntilCanceled(w, r, tc.firstEvent)
			}))
			t.Cleanup(srv.Close)

			tr := newModernTransport(srv.URL, "none", "", "")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			var ackCalled bool
			done := make(chan error, 1)
			go func() {
				done <- tr.Listen(ctx, func() { ackCalled = true }, func() {})
			}()

			select {
			case err := <-done:
				if !errors.Is(err, mcp.ErrListenMalformedEventForTest) {
					t.Errorf("Listen() error = %v, want errListenMalformedEvent", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Listen() did not return within 5s")
			}
			if ackCalled {
				t.Error("onAck was called for a malformed ack event")
			}
		})
	}
}

// TestListen_LaterEvent_StrictValidation_Rejected covers every shape of a
// LATER event (after a valid ack) that fails Listen's own strict validation:
// a notification with the wrong jsonrpc version (both a known method,
// notifications/tools/list_changed, and an unknown one,
// notifications/progress — see unknownNotificationEventWrongJSONRPCVersion's
// own doc for why the unknown-method case specifically proves jsonrpc is
// checked before any method-based skip), a notification whose "id" member is
// an explicit null, a graceful-end-shaped response with the wrong jsonrpc
// version, one with neither result nor error, one with BOTH, one whose id
// does not match this call's own, and one that carries BOTH an id and a
// method. Every case ends Listen with errListenMalformedEvent, with onAck
// already having run.
func TestListen_LaterEvent_StrictValidation_Rejected(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		secondEvent string
	}{
		{"notification_wrong_jsonrpc_version", listChangedEventWrongJSONRPCVersion},
		{"notification_id_null", listChangedEventIDNull},
		{"unknown_notification_wrong_jsonrpc_version", unknownNotificationEventWrongJSONRPCVersion},
		{"response_wrong_jsonrpc_version", gracefulEndEventWrongJSONRPCVersion},
		{"response_neither_result_nor_error", gracefulEndEventNeitherResultNorError},
		{"response_both_result_and_error", gracefulEndEventBothResultAndError},
		{"response_foreign_id", responseEventWithForeignID},
		{"response_with_method", responseEventWithMethod},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				blockUntilCanceled(w, r, ackEvent(true)+tc.secondEvent)
			}))
			t.Cleanup(srv.Close)

			tr := newModernTransport(srv.URL, "none", "", "")
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)

			var ackCalled bool
			err := tr.Listen(ctx, func() { ackCalled = true }, func() {})
			if !errors.Is(err, mcp.ErrListenMalformedEventForTest) {
				t.Fatalf("Listen() error = %v, want errListenMalformedEvent", err)
			}
			if !ackCalled {
				t.Error("onAck should have been called before the malformed later event was reached")
			}
		})
	}
}
