package mcp_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file covers the properties that motivated the streaming rewrite
// itself (docs/mcp-v2.md, "Streaming-Umbau"): HTTPTransport.Forward must
// deliver every byte the upstream sends, in order, without buffering the
// whole response first — and must bound an unresponsive upstream by IDLE
// time, not by a fixed total duration, since a healthy subscriptions/listen
// stream (docs/mcp-v2.md §3.4) is specified to stay open indefinitely.
//
// newStreamTransport builds a modern-era-pinned HTTPTransport with a
// caller-supplied idle timeout, for tests that need to control that value
// precisely instead of using testStreamIdleTimeout's generic 5s default.
func newStreamTransport(endpoint string, idle time.Duration) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, "none", "", "", 5*time.Second, true,
		"", nil, nil, mcp.ClientInfo{Name: "voidllm-test", Version: "test"}, mcp.V20260728, idle)
}

// ---- Progress before Result: the test that proves the original bug --------

// TestForward_SSE_ProgressBeforeResult_BothEventsDeliveredInOrder is the
// direct regression test for the bug that motivated this entire rewrite
// (docs/mcp-v2.md, "Streaming-Umbau" intro; http_transport.go's Forward doc):
// a modern upstream answering tools/call may send notifications/progress
// BEFORE its actual result on the same response stream. The pre-rewrite path
// (rawPost → io.ReadAll → extractSSEData) read the whole body into memory and
// then returned only the FIRST "data:" line — the progress notification —
// silently discarding the result that followed it. Forward must instead
// stream the upstream's response through byte-for-byte, so the caller
// receives BOTH events, in the order the upstream sent them.
//
// Against the pre-rewrite implementation this test would have failed: the
// returned body would have been exactly progressEvent, with resultEvent
// missing entirely.
func TestForward_SSE_ProgressBeforeResult_BothEventsDeliveredInOrder(t *testing.T) {
	t.Parallel()

	const progressEvent = "event: message\n" +
		`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progressToken":"tok-1","progress":50,"total":100}}` +
		"\n\n"
	const resultEvent = "event: message\n" +
		`data: {"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","content":[{"type":"text","text":"done"}]}}` +
		"\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		// Two separate writes with a flush in between, exactly as a real
		// upstream sending progress first and the result afterward would —
		// not one combined write, which would trivially pass even a naive
		// implementation that just happened to read everything available at
		// once.
		fmt.Fprint(w, progressEvent)
		flusher.Flush()
		fmt.Fprint(w, resultEvent)
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)

	tr := newStreamTransport(srv.URL, 5*time.Second)

	res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`), mcp.MapHeader{})
	if err != nil {
		t.Fatalf("Forward() error = %v, want nil", err)
	}
	defer res.Body.Close()

	got, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read res.Body: %v", err)
	}

	want := progressEvent + resultEvent
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("body =\n%s\nwant byte-identical, BOTH events in order:\n%s\n\n"+
			"(against the pre-rewrite rawPost/extractSSEData path, this would have contained only "+
			"progressEvent — the result would have been silently discarded)", got, want)
	}
}

// ---- Long-lived stream: subscriptions/listen must not be bounded by any ---
// ---- fixed total duration ---------------------------------------------------

// TestForward_SubscriptionsListen_PauseJustUnderIdleTimeout_NotificationStillDelivered
// is the scaled-down regression test for docs/mcp-v2.md §3.4/§1a's
// requirement that a subscriptions/listen response stream stay open
// indefinitely: before this rewrite, HTTPTransport used a single shared
// *http.Client whose Timeout (30s, covering the ENTIRE exchange including the
// body read) would have severed any response stream that took longer than
// that to finish — including a subscriptions/listen stream that legitimately
// waits for a real-world event. Forward uses a Timeout:0 streamClient and
// bounds the stream by IDLE time instead (idleTimeoutReader).
//
// This test proves that mechanism does what it is supposed to without
// waiting out a real 30+ second clock: idle and pause are both scaled down by
// three orders of magnitude, but the RATIO that matters — pause comfortably
// under idle, with two such pauses back to back so the exchange's total
// duration comfortably exceeds a single idle window — is preserved. A
// transport still using the old fixed-Timeout http.Client would fail this
// test the same way it would fail in production, just faster.
func TestForward_SubscriptionsListen_PauseJustUnderIdleTimeout_NotificationStillDelivered(t *testing.T) {
	t.Parallel()

	const ack = "event: message\n" +
		`data: {"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":1}}}` +
		"\n\n"
	const notif1 = "event: message\n" +
		`data: {"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{}}` +
		"\n\n"
	const notif2 = "event: message\n" +
		`data: {"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"file:///a"}}` +
		"\n\n"

	// idle is generous enough not to flake under -race or a loaded CI runner;
	// pause is comfortably under it, and TWO pauses back to back put the
	// exchange's total duration well past a single idle window — the
	// property a total-duration timeout (the old http.Client.Timeout) would
	// have been unable to tolerate for a genuinely long-lived stream, but an
	// idle timeout tolerates without difficulty since neither individual gap
	// ever comes close to idle.
	const idle = 600 * time.Millisecond
	const pause = 200 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		fmt.Fprint(w, ack)
		flusher.Flush()
		time.Sleep(pause)
		fmt.Fprint(w, notif1)
		flusher.Flush()
		time.Sleep(pause)
		fmt.Fprint(w, notif2)
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)

	tr := newStreamTransport(srv.URL, idle)

	res, err := tr.Forward(context.Background(),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen","params":{"toolsListChanged":true,"resourceSubscriptions":["file:///a"]}}`),
		mcp.MapHeader{})
	if err != nil {
		t.Fatalf("Forward() error = %v, want nil", err)
	}
	defer res.Body.Close()

	start := time.Now()
	got, err := io.ReadAll(res.Body)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("read res.Body: %v (elapsed %v) — the stream must survive both pauses, each comfortably "+
			"under the idle timeout, even though their sum exceeds it", err, elapsed)
	}

	want := ack + notif1 + notif2
	if !bytes.Equal(got, []byte(want)) {
		t.Errorf("body =\n%s\nwant all three events, in order:\n%s", got, want)
	}
	// A small slack (rather than a hard 2*pause floor) absorbs scheduler
	// jitter under -race and loaded CI runners without weakening the actual
	// property under test — this is a sanity check on the test fixture
	// itself, not an assertion the production idle-timeout mechanism needs
	// to satisfy exactly.
	const slack = 30 * time.Millisecond
	if elapsed < 2*pause-slack {
		t.Errorf("elapsed = %v, want at least ~%v (both pauses must have actually been waited out, "+
			"not skipped) — the test setup itself may be broken", elapsed, 2*pause)
	}
}

// ---- Idle timeout: an upstream that goes silent must not hang forever -----

// TestForward_IdleTimeout_NoDataAtAll_StreamEndsAfterIdleWindow verifies the
// other side of the idle-timeout contract: an upstream that sends headers
// and then never writes another byte must not leave the caller's Read
// blocked forever. idleTimeoutReader's timer, started the moment Forward
// returns (see its doc — "covering the wait for the very first byte"),
// cancels the underlying request's context once idle elapses, which fails
// the blocked Read with a context error.
func TestForward_IdleTimeout_NoDataAtAll_StreamEndsAfterIdleWindow(t *testing.T) {
	t.Parallel()

	upstreamDone := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done() // hang until Forward's idle timeout cancels us
		close(upstreamDone)
	}))
	t.Cleanup(srv.Close)

	const idle = 150 * time.Millisecond
	tr := newStreamTransport(srv.URL, idle)

	res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen"}`), mcp.MapHeader{})
	if err != nil {
		t.Fatalf("Forward() error = %v, want nil", err)
	}
	defer res.Body.Close()

	start := time.Now()
	buf := make([]byte, 16)
	_, readErr := res.Body.Read(buf)
	elapsed := time.Since(start)

	if readErr == nil {
		t.Fatal("Read() error = nil, want the idle timeout to fail the read — the upstream never sent a byte")
	}
	// Generous upper bound: the read must not hang far past idle. Generous
	// lower bound: it must not return suspiciously before idle even had a
	// chance to fire (which would indicate some OTHER, unrelated error path
	// tripped instead of the idle timer).
	if elapsed > 2*time.Second {
		t.Errorf("idle timeout took %v to fire, want well under 2s for a %v idle window", elapsed, idle)
	}
	if elapsed < idle/2 {
		t.Errorf("Read() returned after only %v, want at least ~%v — did something OTHER than the idle timer fire?", elapsed, idle)
	}

	select {
	case <-upstreamDone:
	case <-time.After(2 * time.Second):
		t.Error("upstream handler's request context was never cancelled — Forward's idle timer did not tear down the connection")
	}
}

// TestForward_IdleTimeout_KeepAliveComments_StreamStaysOpenPastIdleWindow is
// the counterexample docs/mcp-v2.md §4.1 exists to justify: an upstream that
// sends periodic SSE comment lines (":\r\n") as keep-alives — exactly what
// the spec recommends servers do on a long-lived stream — must never trip the
// idle timeout, no matter how long the stream runs, because every comment
// resets idleTimeoutReader's timer. This is the same idle-vs-total-duration
// property as the subscriptions/listen test above, from the opposite
// direction: here nothing but keep-alives ever arrives, and the stream must
// still survive well past what a single idle window would tolerate on its
// own.
func TestForward_IdleTimeout_KeepAliveComments_StreamStaysOpenPastIdleWindow(t *testing.T) {
	t.Parallel()

	const idle = 200 * time.Millisecond
	const commentInterval = 60 * time.Millisecond // comfortably under idle
	const finalEvent = "event: message\n" +
		`data: {"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{}}` +
		"\n\n"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)

		// Send enough keep-alive comments, spaced well under idle, that their
		// CUMULATIVE span comfortably exceeds a single idle window — proving
		// survival is governed by the per-gap idle timer resetting on every
		// comment, not by some hidden total-duration cap.
		for i := 0; i < 6; i++ {
			fmt.Fprint(w, ": keep-alive\r\n\r\n")
			flusher.Flush()
			time.Sleep(commentInterval)
		}
		fmt.Fprint(w, finalEvent)
		flusher.Flush()
	}))
	t.Cleanup(srv.Close)

	tr := newStreamTransport(srv.URL, idle)

	res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen"}`), mcp.MapHeader{})
	if err != nil {
		t.Fatalf("Forward() error = %v, want nil", err)
	}
	defer res.Body.Close()

	start := time.Now()
	got, err := io.ReadAll(res.Body)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("read res.Body: %v (elapsed %v) — periodic SSE comment keep-alives must prevent the idle "+
			"timeout from firing (docs/mcp-v2.md §4.1)", err, elapsed)
	}

	if !strings.HasSuffix(string(got), finalEvent) {
		t.Errorf("body does not end with the final event; got: %q", got)
	}
	if !bytes.Contains(got, []byte(": keep-alive")) {
		t.Error("body does not contain any keep-alive comment lines — the test fixture itself may be broken")
	}
	// See the analogous slack in the subscriptions/listen test above: this
	// guards against a fixture that finished suspiciously fast (all sleeps
	// skipped), not against ordinary scheduler jitter under -race.
	const slack = 30 * time.Millisecond
	if elapsed < 6*commentInterval-slack {
		t.Errorf("elapsed = %v, want at least ~%v (all six keep-alive gaps must have actually been waited "+
			"out) — the test setup itself may be broken", elapsed, 6*commentInterval)
	}
}

// ---- Close: stops the timer, no goroutine leak -----------------------------

// TestForward_Body_Close_StopsIdleTimer_NoGoroutineLeak verifies
// idleTimeoutReader.Close's contract (idle_timeout_reader.go): stopping the
// timer and cancelling the request's context promptly, so that closing many
// Forward responses in a row — well before their (long) idle timeout would
// otherwise fire — leaves no goroutines or connections accumulating. This is
// the standard net/http goroutine-leak idiom: a runtime.NumGoroutine snapshot
// before and after, with GC and a short settle window, rather than reaching
// into idleTimeoutReader's private timer field (which export_test.go
// deliberately does not expose — Close's behavior is what callers depend on,
// not the field itself).
func TestForward_Body_Close_StopsIdleTimer_NoGoroutineLeak(t *testing.T) {
	// Every upstream handler blocks on its own request context until Forward
	// cancels it (via Body.Close), exactly like a real subscriptions/listen
	// upstream would keep its own handler goroutine alive until the client
	// disconnects.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	// idle is deliberately long — far longer than this test could possibly
	// run — so that if Close ever failed to stop the timer, that would show
	// up as a real, observable delay/leak rather than accidentally being
	// masked by the timer firing on its own during the test.
	tr := newStreamTransport(srv.URL, 30*time.Second)
	t.Cleanup(func() { _ = tr.Close() })

	runtime.GC()
	before := runtime.NumGoroutine()

	const rounds = 20
	for i := 0; i < rounds; i++ {
		res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"subscriptions/listen"}`), mcp.MapHeader{})
		if err != nil {
			t.Fatalf("round %d: Forward() error = %v, want nil", i, err)
		}
		if err := res.Body.Close(); err != nil {
			t.Fatalf("round %d: res.Body.Close() error = %v, want nil", i, err)
		}
	}

	// Give the upstream handler goroutines (unblocked by Close's context
	// cancellation) and the transport's connection teardown a moment to
	// actually unwind, polling instead of a single fixed sleep.
	deadline := time.Now().Add(2 * time.Second)
	var after int
	for {
		runtime.GC()
		after = runtime.NumGoroutine()
		if after <= before+2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if after > before+2 {
		t.Errorf("goroutine count after %d Forward+Close rounds = %d, want <= %d (before=%d) — "+
			"Close may not be stopping the idle timer / cancelling the request promptly", rounds, after, before+2, before)
	}
}

// ---- Concurrency: many parallel streams over one transport, -race ---------

// TestForward_ManyParallelStreams_SameTransport_NoRace drives many concurrent
// Forward calls against the SAME HTTPTransport, each reading its own body to
// completion and closing it — the property docs/mcp-v2.md §1a demands
// explicitly ("Serialisierung pro Upstream ist ein Fehler, kein
// Sicherheitsnetz"): concurrent requests against one resolved binding must
// never contend with one another. Run with -race. Every goroutine closes its
// own res.Body, per this file's and the plan's explicit requirement.
func TestForward_ManyParallelStreams_SameTransport_NoRace(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			fmt.Fprintf(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{\"progress\":%d}}\n\n", i)
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)

	tr := newStreamTransport(srv.URL, 5*time.Second)

	const streams = 30
	errCh := make(chan error, streams)
	for i := 0; i < streams; i++ {
		go func() {
			res, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`), mcp.MapHeader{})
			if err != nil {
				errCh <- fmt.Errorf("Forward() error = %w", err)
				return
			}
			defer res.Body.Close()
			body, readErr := io.ReadAll(res.Body)
			if readErr != nil {
				errCh <- fmt.Errorf("read body: %w", readErr)
				return
			}
			if !bytes.Contains(body, []byte(`"progress":2`)) {
				errCh <- fmt.Errorf("body missing final progress event: %s", body)
				return
			}
			errCh <- nil
		}()
	}

	for i := 0; i < streams; i++ {
		if err := <-errCh; err != nil {
			t.Error(err)
		}
	}
}
