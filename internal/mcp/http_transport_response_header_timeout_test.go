package mcp_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers ResponseHeaderTimeout (http_transport.go's
// ssrfResponseHeaderTimeout, applied by NewSSRFSafeTransport): an upstream
// that accepts the TCP connection, reads the request, and then simply never
// answers must fail Forward promptly instead of hanging until some other,
// unrelated timeout eventually notices — see ssrfDialTimeout's doc
// (docs/mcp-v2.md, review finding B2) for why this exists separately from
// streamIdleTimeout, which only starts once Forward has already returned.
//
// The production ResponseHeaderTimeout is a fixed 30s
// (ssrfResponseHeaderTimeout), with no parameter on the exported
// NewHTTPTransport constructor that reaches it — see
// export_test.go's NewHTTPTransportWithResponseHeaderTimeout for the
// test-only constructor this file uses instead of waiting out a real 30s.

// TestForward_ResponseHeaderTimeout_UpstreamNeverSendsHeaders_FailsFast
// verifies the failure side: an upstream that accepts the connection, reads
// the full request body, and then goes silent WITHOUT writing any response
// headers must cause Forward to return an error once ResponseHeaderTimeout
// elapses, not block forever.
func TestForward_ResponseHeaderTimeout_UpstreamNeverSendsHeaders_FailsFast(t *testing.T) {
	t.Parallel()

	const responseHeaderTimeout = 150 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Read the request fully (as a real slow-to-respond upstream would
		// have already done) but never call WriteHeader or Write — no
		// response headers are ever sent. Block until the client gives up
		// and the request's own context is cancelled, so this handler
		// goroutine does not leak past the end of the test.
		_, _ = io.ReadAll(r.Body)
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransportWithResponseHeaderTimeout(srv.URL, responseHeaderTimeout, 5*time.Second)

	start := time.Now()
	res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), mcp.MapHeader{})
	elapsed := time.Since(start)

	if err == nil {
		if res != nil {
			res.Body.Close()
		}
		t.Fatal("Forward() error = nil, want a response-header-timeout error for an upstream that never sends headers")
	}
	if res != nil {
		t.Errorf("Forward() result = %+v, want nil alongside a non-nil error", res)
	}

	// Generous upper bound: must fail well before it would if this test
	// accidentally fell back to the real 30s production default (which would
	// indicate NewHTTPTransportWithResponseHeaderTimeout is not actually
	// wired the way NewHTTPTransport itself is). Generous lower bound: must
	// not return suspiciously before responseHeaderTimeout even had a chance
	// to fire, which would indicate some OTHER, unrelated error path tripped
	// instead.
	if elapsed > 5*time.Second {
		t.Errorf("Forward() took %v to fail, want well under 5s for a %v ResponseHeaderTimeout", elapsed, responseHeaderTimeout)
	}
	if elapsed < responseHeaderTimeout/2 {
		t.Errorf("Forward() failed after only %v, want at least ~%v — did something OTHER than ResponseHeaderTimeout fire?", elapsed, responseHeaderTimeout)
	}
}

// TestForward_ResponseHeaderTimeout_DoesNotApply_OnceHeadersHaveArrived is
// the counterexample that proves the split this rewrite depends on:
// ResponseHeaderTimeout bounds only the wait for the upstream's response
// headers, never the body-read phase that follows. An upstream that sends
// headers immediately and then streams slowly — comfortably longer than
// ResponseHeaderTimeout, but still well under the idle timeout — must not be
// cut off by ResponseHeaderTimeout at all.
func TestForward_ResponseHeaderTimeout_DoesNotApply_OnceHeadersHaveArrived(t *testing.T) {
	t.Parallel()

	const responseHeaderTimeout = 150 * time.Millisecond
	// pause is deliberately several multiples of responseHeaderTimeout: if
	// ResponseHeaderTimeout incorrectly applied to the body-read phase too,
	// this would fail well within the test's own timeout budget instead of
	// requiring a coincidental race.
	const pause = 5 * responseHeaderTimeout
	const idle = 5 * time.Second

	const event = "event: message\n" +
		`data: {"jsonrpc":"2.0","id":1,"result":{"tools":[]}}` +
		"\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(pause)
		fmt.Fprint(w, event)
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(srv.Close)

	tr := mcp.NewHTTPTransportWithResponseHeaderTimeout(srv.URL, responseHeaderTimeout, idle)

	res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`), mcp.MapHeader{})
	if err != nil {
		t.Fatalf("Forward() error = %v, want nil — headers arrived immediately, well within ResponseHeaderTimeout", err)
	}
	defer res.Body.Close()

	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read res.Body: %v — a slow BODY must not be cut off by ResponseHeaderTimeout, only a slow "+
			"set of HEADERS should be", err)
	}
	if string(got) != event {
		t.Errorf("body = %q, want %q", got, event)
	}
}
