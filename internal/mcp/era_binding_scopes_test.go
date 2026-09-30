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

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// This file is the unit-level regression test suite for eraBinding.scopes'
// own bound, maxScopesPerBinding (http_transport.go): before it existed, the
// Call path's per-SessionScope legacy session bookkeeping
// (eraBinding.stateFor) grew without limit for the lifetime of a single
// *mcp.HTTPTransport — one entry per distinct organization that had ever
// called CallMCPTool against a shared global server, never removed even
// after that organization stopped existing. maxScopesPerBinding bounds and
// LRU-evicts it exactly like mcp.SessionRegistry's own
// maxOrgsPerServer/maxKeysPerOrg one layer up.

// legacyWarmupCountingHandler returns an httptest handler that implements the
// legacy initialize/notifications/initialized handshake for ANY scope Call
// warms up through, incrementing initializeCount on every single "initialize"
// it answers (Warmup's own I/O, distinct from the "ping" Call itself sends
// afterward) — the observable signal this file's tests use to tell whether a
// given scope's *scopedState was still warm (no new initialize) or had been
// evicted and warmed up again from scratch (a new initialize).
func legacyWarmupCountingHandler(initializeCount *atomic.Int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var rpc struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &rpc)

		w.Header().Set("Content-Type", "application/json")
		switch rpc.Method {
		case "initialize":
			initializeCount.Add(1)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		default:
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		}
	})
}

// TestEraBinding_MaxScopesPerBinding_LRUEvictsLeastRecentlyUsedScope is the
// single, comprehensive regression test for maxScopesPerBinding: it covers
// all three properties docs/mcp-v2.md's fix description calls for in one
// pass, exactly the way this package's own maxSessionsPerScope/
// maxOrgsPerServer tests do — the cap is reached, the LEAST recently used
// scope is evicted (not merely the oldest by creation order, and a Call
// counts as use exactly like the scope's first creation), and a subsequently
// evicted scope's next Call re-runs Warmup cleanly rather than failing.
func TestEraBinding_MaxScopesPerBinding_LRUEvictsLeastRecentlyUsedScope(t *testing.T) {
	var initializeCount atomic.Int64
	srv := httptest.NewServer(legacyWarmupCountingHandler(&initializeCount))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")
	t.Cleanup(func() { _ = tr.Close() })

	const capScopes = 4096 // maxScopesPerBinding
	scopeAt := func(i int) mcp.SessionScope {
		return mcp.SessionScope(fmt.Sprintf("org-%04d", i))
	}
	const pingBody = `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	// Fill the binding to exactly the cap: org-0000 .. org-4095, each
	// warming up its own *scopedState in order — org-0000 is therefore the
	// least recently used scope once the loop finishes.
	for i := 0; i < capScopes; i++ {
		if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(pingBody)}, scopeAt(i)); err != nil {
			t.Fatalf("Call() for scope #%d: %v", i, err)
		}
	}
	if got := tr.ScopedStateCount(); got != capScopes {
		t.Fatalf("ScopedStateCount() after filling to the cap = %d, want %d", got, capScopes)
	}
	if got := initializeCount.Load(); got != capScopes {
		t.Fatalf("initialize count after filling to the cap = %d, want %d (one Warmup per distinct scope)", got, capScopes)
	}

	// Call org-0000 again — no new initialize (still warm), but this DOES
	// count as use and must move it to the front of the LRU, protecting it
	// from the eviction about to happen.
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(pingBody)}, scopeAt(0)); err != nil {
		t.Fatalf("Call() re-touching scope #0: %v", err)
	}
	if got := initializeCount.Load(); got != capScopes {
		t.Fatalf("initialize count after re-touching scope #0 = %d, want %d (no new Warmup for an already-warm scope)", got, capScopes)
	}

	// One brand new scope overflows the cap, evicting the LEAST recently
	// used scope — org-0001, since org-0000 was just refreshed above — not
	// org-0000.
	overflowScope := mcp.SessionScope("org-4096-the-overflow")
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(pingBody)}, overflowScope); err != nil {
		t.Fatalf("Call() for the overflow scope: %v", err)
	}
	if got := tr.ScopedStateCount(); got != capScopes {
		t.Errorf("ScopedStateCount() after the overflow = %d, want %d (still bounded, one evicted for the one added)", got, capScopes)
	}
	if got := initializeCount.Load(); got != capScopes+1 {
		t.Fatalf("initialize count after the overflow scope's own Call = %d, want %d", got, capScopes+1)
	}

	// org-0000 must still be warm: no new initialize for it.
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(pingBody)}, scopeAt(0)); err != nil {
		t.Fatalf("Call() re-touching scope #0 after the overflow: %v", err)
	}
	if got := initializeCount.Load(); got != capScopes+1 {
		t.Errorf("initialize count after re-touching scope #0 post-overflow = %d, want %d (org-0000 must have survived the eviction)", got, capScopes+1)
	}

	// org-0001, the least recently used scope, must have been evicted: its
	// next Call is NOT an error — it transparently re-runs Warmup, exactly
	// as if this were the very first Call ever made in that scope.
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(pingBody)}, scopeAt(1)); err != nil {
		t.Fatalf("Call() for the evicted scope #1: %v, want a clean re-Warmup, not an error", err)
	}
	if got := initializeCount.Load(); got != capScopes+2 {
		t.Errorf("initialize count after calling the evicted scope #1 = %d, want %d (org-0001 must have re-run Warmup from scratch)", got, capScopes+2)
	}

	// An unrelated, untouched scope well away from either the eviction or
	// the LRU refreshes above (org-2048, comfortably in the middle) must be
	// completely unaffected: still warm, no new initialize.
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(pingBody)}, scopeAt(2048)); err != nil {
		t.Fatalf("Call() re-touching the unrelated mid scope #2048: %v", err)
	}
	if got := initializeCount.Load(); got != capScopes+2 {
		t.Errorf("initialize count after re-touching the unrelated mid scope #2048 = %d, want %d (it must still be warm)", got, capScopes+2)
	}
}

// TestEraBinding_MaxScopesPerBinding_RegularlyUsedScopeSurvivesChurn is the
// smaller, focused counterpart of the comprehensive test above: a single
// scope that is repeatedly re-used WHILE many other, one-off scopes churn
// through and overflow the cap around it must never itself be evicted — a
// direct regression test for "a scope in active use is never the one
// evicted" in isolation from the exact LRU-ordering mechanics the
// comprehensive test above already covers.
func TestEraBinding_MaxScopesPerBinding_RegularlyUsedScopeSurvivesChurn(t *testing.T) {
	var initializeCount atomic.Int64
	srv := httptest.NewServer(legacyWarmupCountingHandler(&initializeCount))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")
	t.Cleanup(func() { _ = tr.Close() })

	const pingBody = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	const survivorScope = mcp.SessionScope("survivor-org")

	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(pingBody)}, survivorScope); err != nil {
		t.Fatalf("Call() for the survivor scope: %v", err)
	}

	// touchSurvivor re-calls the survivor scope and reports whether THAT
	// specific call triggered a new Warmup (initializeCount incrementing),
	// isolated from the churn scopes' own initializes around it — the global
	// counter keeps climbing throughout this test regardless of the
	// survivor's own fate, since every churn scope warms up too.
	touchSurvivor := func() (triggeredNewWarmup bool) {
		before := initializeCount.Load()
		if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(pingBody)}, survivorScope); err != nil {
			t.Fatalf("Call() re-touching the survivor scope: %v", err)
		}
		return initializeCount.Load() != before
	}

	// Churn through more than maxScopesPerBinding (4096) one-off scopes,
	// touching the survivor scope every 100 calls so it never becomes the
	// least recently used entry.
	const churnScopes = 4096 + 500
	for i := 0; i < churnScopes; i++ {
		scope := mcp.SessionScope(fmt.Sprintf("churn-org-%05d", i))
		if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(pingBody)}, scope); err != nil {
			t.Fatalf("Call() for churn scope #%d: %v", i, err)
		}
		if i%100 == 0 && touchSurvivor() {
			t.Fatalf("the regularly re-used survivor scope triggered a new Warmup at churn #%d, want it to never be "+
				"evicted while in active use", i)
		}
	}

	if touchSurvivor() {
		t.Error("the regularly re-used survivor scope triggered a new Warmup on the final touch after churn, want it " +
			"to never be evicted while in active use")
	}
}

// TestEraBinding_MaxScopesPerBinding_ConcurrentCalls_NoRace drives concurrent
// Call()s across many distinct scopes, several of them landing on the SAME
// scope from more than one goroutine at once — the ordinary shape of MCP
// traffic against a shared upstream (docs/mcp-v2.md §1a) — through a single
// *mcp.HTTPTransport, well past maxScopesPerBinding, verified under -race.
func TestEraBinding_MaxScopesPerBinding_ConcurrentCalls_NoRace(t *testing.T) {
	var initializeCount atomic.Int64
	srv := httptest.NewServer(legacyWarmupCountingHandler(&initializeCount))
	t.Cleanup(srv.Close)

	tr := newLegacyTransport(srv.URL, "none", "", "")
	t.Cleanup(func() { _ = tr.Close() })

	const pingBody = `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	const scopes = 50
	const goroutinesPerScope = 4
	const roundsPerGoroutine = 20

	var wg sync.WaitGroup
	for s := 0; s < scopes; s++ {
		scope := mcp.SessionScope(fmt.Sprintf("concurrent-org-%03d", s))
		for g := 0; g < goroutinesPerScope; g++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for r := 0; r < roundsPerGoroutine; r++ {
					if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(pingBody)}, scope); err != nil {
						t.Errorf("Call() for scope %q: %v", scope, err)
					}
				}
			}()
		}
	}
	wg.Wait()
}
