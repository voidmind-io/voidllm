package admin_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

// This file covers the ad-hoc transport lifecycle (mcp_proxy.go's
// transportClosedAsync field and its doc, review finding B1): on a
// cache miss, HandleMCPProxy builds a one-off *mcp.HTTPTransport via
// buildAdHocTransport. Its Close must not fire while the SendStreamWriter
// goroutine is still reading the response body — HandleMCPProxy's own return
// (and any defer on it) can happen well before that goroutine finishes — but
// it must fire exactly once after the goroutine completes. None of the tests
// in this package configure Handler.MCPTransportCache, so every proxied call
// through setupMCPProxyApp already goes through this ad-hoc path; this file
// adds the dedicated, repeated-round regression coverage for its lifecycle
// specifically.
//
// HTTPTransport.Close (internal/mcp/http_transport.go) only calls
// CloseIdleConnections on its two *http.Client values — an idempotent,
// panic-free operation — so a premature or duplicate Close cannot be
// observed as a synchronous error from these tests. What IS observable, and
// what these tests assert instead:
//
//  1. A stream over an ad-hoc transport runs to completion and delivers
//     every byte, even when reading resumes well after HandleMCPProxy's own
//     handler function has already returned (proxyPost already returns once
//     headers arrive — see the other streaming tests in this package for
//     that same property) — proving whatever cleanup happens on the early
//     return path does not interfere with the in-flight read.
//  2. Repeated rounds through each of the three lifecycle paths (genuine
//     pass-through streaming, 202 notification, and a pre-header transport
//     error) leave no growing number of goroutines or open connections
//     behind — the same idiom forward_streaming_test.go's own
//     TestForward_Body_Close_StopsIdleTimer_NoGoroutineLeak already uses at
//     the HTTPTransport level, applied here through the full HTTP proxy
//     stack — which would catch both "never closed" (a leak) and a Close
//     that panics or otherwise breaks the request.

// TestMCPProxy_AdHocTransport_SlowStream_CompletesAfterHandlerReturns proves
// that a stream over an ad-hoc transport (the only kind this package's test
// harness ever builds — no MCPTransportCache is configured) delivers all of
// its bytes even when the client only starts reading well after
// HandleMCPProxy's own handler function must already have returned.
func TestMCPProxy_AdHocTransport_SlowStream_CompletesAfterHandlerReturns(t *testing.T) {
	t.Parallel()

	const firstChunk = "event: message\n" +
		`data: {"jsonrpc":"2.0","method":"notifications/progress","params":{"progress":1}}` + "\n\n"
	const secondChunk = "event: message\n" +
		`data: {"jsonrpc":"2.0","id":1,"result":{"resultType":"complete"}}` + "\n\n"
	const pause = 200 * time.Millisecond

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, firstChunk)
		flusher.Flush()
		time.Sleep(pause)
		fmt.Fprint(w, secondChunk)
		flusher.Flush()
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_AdHocTransport_SlowStream?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "adhoc-slow-stream")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "adhoc-slow-stream-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
	defer resp.Body.Close()

	if resp.StatusCode != fiber.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, raw)
	}

	// proxyPost already returned — HandleMCPProxy's own handler function, and
	// any defer on it (including the ad-hoc transport's conditional Close),
	// has already run by this point. Only NOW does this test start reading —
	// deliberately delayed, so that the read below spans well past whatever
	// HandleMCPProxy's own return already did.
	time.Sleep(pause / 2)

	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v — a slow, ad-hoc-transport-backed stream must complete even when reading "+
			"resumes after the handler has already returned", err)
	}

	want := firstChunk + secondChunk
	if string(got) != want {
		t.Errorf("body = %q, want both chunks in order: %q", got, want)
	}
}

// mcpProxyAdHocRounds is shared by the three no-leak tests below: enough
// rounds to make a real leak (one goroutine or connection per round, say)
// obviously visible against scheduler noise, without making the test slow.
const mcpProxyAdHocRounds = 40

// TestMCPProxy_AdHocTransport_PassThroughStreaming_NoLeakAcrossManyRounds
// drives many rounds of the genuine pass-through streaming path — each
// building and, via the SendStreamWriter goroutine, closing its own ad-hoc
// transport — and asserts the goroutine count settles back down rather than
// growing round over round.
func TestMCPProxy_AdHocTransport_PassThroughStreaming_NoLeakAcrossManyRounds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message\n"+`data: {"jsonrpc":"2.0","id":1,"result":{}}`+"\n\n")
		w.(http.Flusher).Flush()
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_AdHocTransport_PassThroughStreaming_NoLeak?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "adhoc-noleak-stream")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "adhoc-noleak-stream-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	for i := 0; i < mcpProxyAdHocRounds; i++ {
		resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"deploy"}}`)
		if resp.StatusCode != fiber.StatusOK {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("round %d: status = %d, want 200; body: %s", i, resp.StatusCode, raw)
		}
		if _, err := io.ReadAll(resp.Body); err != nil {
			resp.Body.Close()
			t.Fatalf("round %d: read body: %v", i, err)
		}
		resp.Body.Close()
	}

	assertGoroutineCountSettles(t, before)
}

// TestMCPProxy_AdHocTransport_NotificationPath_NoLeakAcrossManyRounds drives
// many rounds of the 202 Accepted (notification) path — which never enters
// the SendStreamWriter goroutine at all, closing its ad-hoc transport
// synchronously via HandleMCPProxy's own defer instead (see
// transportClosedAsync's doc: it is never set on this path) — and asserts no
// growing leak.
func TestMCPProxy_AdHocTransport_NotificationPath_NoLeakAcrossManyRounds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(upstream.Close)

	dsn := "file:TestMCPProxy_AdHocTransport_NotificationPath_NoLeak?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "adhoc-noleak-notif")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "adhoc-noleak-notif-server"
	s := createExternalMCPServerPinned(t, database, alias, upstream.URL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	for i := 0; i < mcpProxyAdHocRounds; i++ {
		resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
		if resp.StatusCode != fiber.StatusAccepted {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("round %d: status = %d, want 202; body: %s", i, resp.StatusCode, raw)
		}
		resp.Body.Close()
	}

	assertGoroutineCountSettles(t, before)
}

// TestMCPProxy_AdHocTransport_TransportErrorPath_NoLeakAcrossManyRounds
// drives many rounds of the pre-header transport-error path (connection
// refused) — closed synchronously via HandleMCPProxy's own defer, exactly
// like the notification path above — and asserts no growing leak.
func TestMCPProxy_AdHocTransport_TransportErrorPath_NoLeakAcrossManyRounds(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unreachableURL := upstream.URL
	upstream.Close() // closed before any request reaches it: connection refused

	dsn := "file:TestMCPProxy_AdHocTransport_TransportErrorPath_NoLeak?mode=memory&cache=private"
	app, database, keyCache := setupMCPProxyApp(t, dsn)
	org := mustCreateTestOrg(t, database, "adhoc-noleak-error")
	memberKey := addMCPTestKey(t, keyCache, org.ID)

	const alias = "adhoc-noleak-error-server"
	s := createExternalMCPServerPinned(t, database, alias, unreachableURL, "2026-07-28")
	if err := database.SetOrgMCPAccess(context.Background(), org.ID, []string{s}); err != nil {
		t.Fatalf("SetOrgMCPAccess: %v", err)
	}

	runtime.GC()
	before := runtime.NumGoroutine()

	for i := 0; i < mcpProxyAdHocRounds; i++ {
		resp := proxyPost(t, app, alias, memberKey, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
		if resp.StatusCode != fiber.StatusBadGateway {
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			t.Fatalf("round %d: status = %d, want 502; body: %s", i, resp.StatusCode, raw)
		}
		resp.Body.Close()
	}

	assertGoroutineCountSettles(t, before)
}

// assertGoroutineCountSettles polls runtime.NumGoroutine, GC'ing between
// samples, until it settles back near before or a deadline elapses — mirrors
// forward_streaming_test.go's TestForward_Body_Close_StopsIdleTimer_NoGoroutineLeak.
func assertGoroutineCountSettles(t *testing.T, before int) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
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
		t.Errorf("goroutine count after %d rounds = %d, want <= %d (before=%d) — the ad-hoc transport may not "+
			"be closing (a leak) or something on its lifecycle path is misbehaving", mcpProxyAdHocRounds, after, before+2, before)
	}
}
