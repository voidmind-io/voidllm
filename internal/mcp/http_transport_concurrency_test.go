package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// TestCall_Legacy_ConcurrentExpiredSession_ReinitsOnceWithoutLooping is the
// concurrency counterpart to TestCall_Legacy_SessionExpired_ReinitsExactlyOnceAndRetries
// and the direct test of reinitAndRetry's TOCTOU fix (docs/mcp-v2.md §11.3
// Befund 2): reinitAndRetry now compares against the session ID actually
// placed on the wire for the call that just failed — read back from the
// header map Prepare produced via doCall's return value — rather than a
// snapshot of UpstreamState taken before doCall ran, which a concurrent Call
// in the same SessionScope could already have raced ahead of.
//
// Many goroutines call Call() at once against a transport whose session has
// already expired. Every one of them will see the SAME stale session and
// hit ErrSessionExpired at roughly the same time, but per reinitAndRetry's
// double-check only the FIRST to acquire the scoped state's mutex actually
// re-initializes; every other goroutine observes the session has already
// moved on (current != its own staleSession) and simply retries with it
// instead of re-initializing again. Run with -race: this also exercises
// UpstreamState's session read/write under genuine concurrent access.
func TestCall_Legacy_ConcurrentExpiredSession_ReinitsOnceWithoutLooping(t *testing.T) {
	t.Parallel()

	var initializeCount int32
	var validSession atomic.Value
	validSession.Store("session-1")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "initialize":
			n := atomic.AddInt32(&initializeCount, 1)
			newSession := fmt.Sprintf("session-%d", n)
			validSession.Store(newSession)
			w.Header().Set("Mcp-Session-Id", newSession)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "ping":
			if r.Header.Get("Mcp-Session-Id") != validSession.Load().(string) {
				http.NotFound(w, nil) // this session is no longer current
				return
			}
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")

	// Establish the initial session sequentially — this is the "already
	// expired" session every concurrent goroutine below will race against.
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("initial Call() error = %v, want nil", err)
	}
	if got := atomic.LoadInt32(&initializeCount); got != 1 {
		t.Fatalf("initialize count after setup = %d, want 1", got)
	}

	// Invalidate the established session from the upstream's point of view,
	// without touching the transport — every concurrent Call below will find
	// its cached session rejected on the very first attempt.
	validSession.Store("session-invalidated-for-test")

	const goroutines = 50
	var wg sync.WaitGroup
	wg.Add(goroutines)
	errs := make([]error, goroutines)

	for i := range goroutines {
		go func(i int) {
			defer wg.Done()
			_, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
			errs[i] = err
		}(i)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent Calls did not complete within 10s — reinitAndRetry may be looping")
	}

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Call() error = %v, want nil", i, err)
		}
	}

	// Exactly one reinit must have happened, no matter how many goroutines
	// raced into ErrSessionExpired at once: 1 for setup, 1 for the reinit
	// every concurrent goroutine converges on via the double-check in
	// reinitAndRetry.
	if got := atomic.LoadInt32(&initializeCount); got != 2 {
		t.Errorf("initialize count = %d, want exactly 2 (one setup, one reinit shared by all concurrent callers — not one per goroutine, and not a retry loop)", got)
	}
}
