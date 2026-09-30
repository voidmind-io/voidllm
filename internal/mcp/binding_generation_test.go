package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---- The decisive test: a late-returning stale invalidation must never ----
// ---- discard a newer, already re-resolved binding generation --------------

// TestHTTPTransport_InvalidateBinding_StaleGenerationNeverDiscardsNewerOne is
// the direct test of invalidateBinding's compare-and-swap contract
// (docs/mcp-v2.md §1a; http_transport.go's invalidateBinding doc): a call
// that captured an OLD *eraBinding generation and only decides to invalidate
// it after some other, concurrent caller has already re-resolved a NEWER
// generation must be a no-op — it must never discard a generation it did not
// itself observe failing.
//
// This is constructed deterministically, without any goroutines, exactly as
// asked: resolve a binding, capture that generation, invalidate it, resolve a
// NEW generation, and only THEN invalidate again using the STALE, first
// generation — the new one must survive untouched.
func TestHTTPTransport_InvalidateBinding_StaleGenerationNeverDiscardsNewerOne(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")

	// Resolve the first binding generation.
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("Call() #1 error = %v, want nil", err)
	}
	oldGen := tr.CurrentBindingGeneration()
	if oldGen == nil {
		t.Fatal("CurrentBindingGeneration() = nil after a successful Call, want a resolved binding")
	}

	// Invalidate it (as if a caller holding this generation had just detected
	// it needs re-resolving) and resolve a NEW generation in its place.
	tr.InvalidateBindingIfMatches(oldGen)
	if got := tr.CurrentBindingGeneration(); got != nil {
		t.Fatalf("CurrentBindingGeneration() = %v immediately after invalidation, want nil", got)
	}
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("Call() #2 (re-resolve) error = %v, want nil", err)
	}
	newGen := tr.CurrentBindingGeneration()
	if newGen == nil {
		t.Fatal("CurrentBindingGeneration() = nil after re-resolution, want a resolved binding")
	}
	if newGen == oldGen {
		t.Fatal("newGen == oldGen, want a distinct generation after re-resolution — the test setup is broken")
	}

	// The decisive step: a late-returning call still holding the OLD
	// generation only now gets around to invalidating it.
	tr.InvalidateBindingIfMatches(oldGen)

	// The newer generation, published in the meantime, must survive
	// completely untouched.
	if got := tr.CurrentBindingGeneration(); got != newGen {
		t.Errorf("CurrentBindingGeneration() = %v after invalidating with a STALE generation, want it to "+
			"remain %v — a late-returning old call must never discard a newer, already-published binding",
			got, newGen)
	}

	// And the transport must still be fully usable.
	if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, ""); err != nil {
		t.Fatalf("Call() #3 error = %v, want nil", err)
	}
}

// ---- Many parallel Call and Forward against the same transport, -race -----

// TestHTTPTransport_ConcurrentCallAndForward_WithRepeatedInvalidation_NoRace
// drives many concurrent Call and Forward goroutines against the same
// HTTPTransport while other goroutines repeatedly invalidate the binding, all
// at once. Run with -race: the property under test is the absence of a data
// race between Forward (which never took part in the original
// TestHTTPTransport_Call_ConcurrentWithInvalidateBinding_NoRace regression
// coverage) and Call sharing one resolveBinding/invalidateBinding pair,
// consistent with docs/mcp-v2.md §1a's requirement that a gateway serialize
// only around state resolution, never around the request path itself.
func TestHTTPTransport_ConcurrentCallAndForward_WithRepeatedInvalidation_NoRace(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
	}))
	t.Cleanup(srv.Close)

	tr := newModernTransport(srv.URL, "none", "", "")

	const callers = 8
	const forwarders = 8
	const invalidators = 4
	const iterations = 150

	var wg sync.WaitGroup
	wg.Add(callers + forwarders + invalidators)

	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// A transport-level or era-resolution error is an acceptable
				// outcome here (the binding might be invalidated mid-flight)
				// — the property under test is the absence of a data race,
				// not that every single call succeeds.
				_, _ = tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
			}
		}()
	}
	for i := 0; i < forwarders; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				// A transport-level or era-resolution error is an acceptable
				// outcome (see the callers loop above); when Forward DOES
				// succeed, its ForwardResult.Body MUST be closed here — it is
				// never read to EOF by anything else in this stress test, and
				// an unclosed body leaks the underlying connection, which
				// would make -race's outcome depend on how many idle
				// connections happen to still be live when the race detector
				// samples goroutine state.
				res, ferr := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), mcp.MapHeader{})
				if ferr == nil {
					res.Body.Close()
				}
			}
		}()
	}
	for i := 0; i < invalidators; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < iterations; j++ {
				tr.InvalidateBinding()
			}
		}()
	}

	wg.Wait()

	// Sanity check: the transport must still be fully usable afterward.
	got, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, "")
	if err != nil {
		t.Fatalf("Call() after the concurrent storm error = %v, want nil", err)
	}
	if len(got.Body) == 0 {
		t.Error("Call() after the concurrent storm returned an empty body, want the server's response")
	}
	fwd, err := tr.Forward(context.Background(), []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), mcp.MapHeader{})
	if err != nil {
		t.Fatalf("Forward() after the concurrent storm error = %v, want nil", err)
	}
	fwdBody, err := io.ReadAll(fwd.Body)
	fwd.Body.Close()
	if err != nil {
		t.Fatalf("read fwd.Body: %v", err)
	}
	if len(fwdBody) == 0 {
		t.Error("Forward() after the concurrent storm returned an empty body, want the server's response")
	}
}

// ---- Modern era: stateFor is skipped entirely, regardless of scope --------

// TestCall_Modern_SkipsStateForEntirely_AcrossScopes verifies docs/mcp-v2.md
// §1a/§2's stateless-core property at the MECHANISM level, not merely its
// visible symptom: TestCall_Modern_NoSessionHeaderNoInitialize
// (http_transport_test.go) already shows no Mcp-Session-Id header and no
// initialize appear on the wire, but that alone is also what a no-op
// EraModern Warmup would produce even if stateFor HAD been called for every
// scope. ScopedStateCount (export_test.go) inspects eraBinding.scopes
// directly — populated only by stateFor — to prove stateFor itself is never
// invoked under the modern era, no matter how many distinct SessionScope
// values Call is given.
//
// The legacy subtest alongside it is a sanity check on ScopedStateCount
// itself: it proves the hook reflects real internal state (growing by
// exactly one entry per distinct scope) rather than a hook that would read
// zero unconditionally and make the modern subtest vacuous.
func TestCall_Modern_SkipsStateForEntirely_AcrossScopes(t *testing.T) {
	t.Parallel()

	scopes := []mcp.SessionScope{"", "org-a", "org-b", "org-c"}

	t.Run("modern era: eraBinding.scopes is never populated for any scope", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{}}`)
		}))
		t.Cleanup(srv.Close)

		tr := newModernTransport(srv.URL, "none", "", "")
		for _, scope := range scopes {
			if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, scope); err != nil {
				t.Fatalf("Call() scope=%q error = %v, want nil", scope, err)
			}
		}

		if got := tr.ScopedStateCount(); got != 0 {
			t.Errorf("ScopedStateCount() = %d, want 0 — EraModern's Call must skip stateFor entirely, for every scope", got)
		}
	})

	t.Run("legacy era counterpart: stateFor DOES populate one entry per distinct scope (sanity check)", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var rpc struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(body, &rpc)

			w.Header().Set("Content-Type", "application/json")
			switch rpc.Method {
			case "initialize":
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":0,"result":{"protocolVersion":"2025-03-26","capabilities":{}}}`)
			case "notifications/initialized":
				w.WriteHeader(http.StatusAccepted)
			default:
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"status":"ok"}}`)
			}
		}))
		t.Cleanup(srv.Close)

		tr := newLegacyTransport(srv.URL, "none", "", "")
		for _, scope := range scopes {
			if _, err := tr.Call(context.Background(), &mcp.CallRequest{Raw: []byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)}, scope); err != nil {
				t.Fatalf("Call() scope=%q error = %v, want nil", scope, err)
			}
		}

		if got := tr.ScopedStateCount(); got != len(scopes) {
			t.Errorf("ScopedStateCount() = %d, want %d (one scopedState per distinct SessionScope) — "+
				"this sanity-checks that ScopedStateCount reflects real internal state, not a hook that "+
				"always reads zero regardless of era", got, len(scopes))
		}
	})
}
