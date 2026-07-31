package health_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/health"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// testStreamIdleTimeout only needs to be nonzero and generous enough to
// never fire during a fast, local httptest exchange.
const testStreamIdleTimeout = 5 * time.Second

// testClientInfo self-identifies these tests' transports the same way
// production code does, for parity with what a real upstream would see.
var testClientInfo = mcp.ClientInfo{Name: "voidllm-health-test", Version: "test"}

// newAutoProbeTransport builds an *mcp.HTTPTransport with era auto-detection
// enabled (no pinned version), exactly like the transports MCPTransportCache
// hands the health checker in production. allowPrivate is true so loopback
// httptest servers are reachable.
func newAutoProbeTransport(endpoint string) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, "none", "", "", 5*time.Second, true,
		"", nil, nil, testClientInfo, "", testStreamIdleTimeout)
}

// newPinnedModernTransport builds an *mcp.HTTPTransport pinned to V20260728,
// skipping era probing entirely. Used by tests that only care about the
// checker's own orchestration (cleanup, start/stop) and would otherwise pay
// an unnecessary probe round trip.
func newPinnedModernTransport(endpoint string) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, "none", "", "", 5*time.Second, true,
		"", nil, nil, testClientInfo, mcp.V20260728, testStreamIdleTimeout)
}

// newPinnedLegacyTransport builds an *mcp.HTTPTransport pinned to V20250326,
// skipping era probing and forcing the initialize/notifications/initialized
// handshake deterministically.
func newPinnedLegacyTransport(endpoint string) *mcp.HTTPTransport {
	return mcp.NewHTTPTransport(endpoint, "none", "", "", 5*time.Second, true,
		"", nil, nil, testClientInfo, mcp.V20250326, testStreamIdleTimeout)
}

// newMCPTarget builds a minimal MCPServerTarget. Endpoint details live on the
// transport returned by transportFor now, not on the target itself.
func newMCPTarget(id, name, alias string) health.MCPServerTarget {
	return health.MCPServerTarget{ID: id, Name: name, Alias: alias, Source: "api"}
}

// newMCPChecker builds an MCPHealthChecker with a long interval (so the
// ticker never fires during tests unless a test overrides it) wired to the
// given servers and transportFor callbacks.
func newMCPChecker(servers func() []health.MCPServerTarget, transportFor func(string) (*mcp.HTTPTransport, bool)) *health.MCPHealthChecker {
	return health.NewMCPHealthChecker(servers, transportFor, 24*time.Hour, newLogger())
}

// staticTransportFor returns a transportFor callback backed by a fixed map,
// safe for concurrent reads by the checker's bounded-concurrency probe pool
// since the map is never mutated after construction.
func staticTransportFor(m map[string]*mcp.HTTPTransport) func(string) (*mcp.HTTPTransport, bool) {
	return func(id string) (*mcp.HTTPTransport, bool) {
		t, ok := m[id]
		return t, ok
	}
}

// rpcMethod extracts the JSON-RPC "method" field from a raw request body,
// returning "" if the body does not parse.
func rpcMethod(body []byte) string {
	var req struct {
		Method string `json:"method"`
	}
	_ = json.Unmarshal(body, &req)
	return req.Method
}

// TestMCPHealthChecker_GetHealth_UnknownServerID verifies that GetHealth returns
// a zero-value MCPServerHealth with Status "unknown" for a server that has never
// been probed.
func TestMCPHealthChecker_GetHealth_UnknownServerID(t *testing.T) {
	t.Parallel()

	c := newMCPChecker(func() []health.MCPServerTarget { return nil }, staticTransportFor(nil))

	got := c.GetHealth("does-not-exist")
	if got.Status != "unknown" {
		t.Errorf("Status = %q, want %q", got.Status, "unknown")
	}
	if got.ServerID != "does-not-exist" {
		t.Errorf("ServerID = %q, want %q", got.ServerID, "does-not-exist")
	}
}

// TestMCPHealthChecker_GetAllHealth_NoneProbed verifies that GetAllHealth returns
// an empty slice when no probe cycle has run.
func TestMCPHealthChecker_GetAllHealth_NoneProbed(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	transports := map[string]*mcp.HTTPTransport{"s1": newPinnedModernTransport(srv.URL)}
	c := newMCPChecker(func() []health.MCPServerTarget {
		return []health.MCPServerTarget{newMCPTarget("s1", "Server 1", "s1")}
	}, staticTransportFor(transports))
	// Do not call Start(), so no probe runs.
	all := c.GetAllHealth()
	if len(all) != 0 {
		t.Errorf("GetAllHealth() len = %d, want 0 (no probe run)", len(all))
	}
}

// TestMCPHealthChecker_ModernSpecCompliantServer_AutoProbe_Healthy is the
// regression test for the bug this refactor fixes: a fake upstream that
// behaves exactly like a spec-conformant 2026-07-28 server — it rejects any
// tools/list request missing the required MCP-Protocol-Version/Mcp-Method
// headers or params._meta with JSON-RPC -32020 HeaderMismatch, per
// docs/mcp-v2.md §4.5. The old hand-rolled probe (raw
// {"jsonrpc":"2.0","id":1,"method":"tools/list"}, no headers, no _meta) would
// be rejected by this fake server and the checker would report "unhealthy"
// forever. Probing through transport.ListTools instead negotiates the era via
// server/discover and sends the required headers and _meta, so the checker
// must report "healthy" with the correct tool count.
func TestMCPHealthChecker_ModernSpecCompliantServer_AutoProbe_Healthy(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		switch rpcMethod(body) {
		case "server/discover":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":"probe-discover","result":{"resultType":"complete","supportedVersions":["2026-07-28"],"capabilities":{},"ttlMs":0,"cacheScope":"public"}}`)
		case "tools/list":
			if r.Header.Get("MCP-Protocol-Version") == "" || r.Header.Get("Mcp-Method") == "" ||
				!strings.Contains(string(body), `"io.modelcontextprotocol/protocolVersion"`) ||
				!strings.Contains(string(body), `"io.modelcontextprotocol/clientCapabilities"`) {
				w.WriteHeader(http.StatusBadRequest)
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32020,"message":"header mismatch"}}`)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"resultType":"complete","ttlMs":0,"cacheScope":"public","tools":[{"name":"a"},{"name":"b"},{"name":"c"}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	transports := map[string]*mcp.HTTPTransport{"modern-1": newAutoProbeTransport(srv.URL)}
	c := newMCPChecker(func() []health.MCPServerTarget {
		return []health.MCPServerTarget{newMCPTarget("modern-1", "Modern Server", "modern-1")}
	}, staticTransportFor(transports))
	stop := c.Start()
	t.Cleanup(stop)

	got := c.GetHealth("modern-1")
	if got.Status != "healthy" {
		t.Fatalf("Status = %q, want %q (LastError=%q)", got.Status, "healthy", got.LastError)
	}
	if got.ToolCount != 3 {
		t.Errorf("ToolCount = %d, want 3", got.ToolCount)
	}
	if got.LatencyMs <= 0 {
		t.Errorf("LatencyMs = %d, want > 0", got.LatencyMs)
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want empty", got.LastError)
	}
}

// TestMCPHealthChecker_LegacyServer_AutoProbe_Healthy verifies that a legacy
// (pre-2026-07-28) upstream is still probed successfully: era auto-detection
// falls back to the initialize handshake and ListTools reports the correct
// tool count.
func TestMCPHealthChecker_LegacyServer_AutoProbe_Healthy(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")

		switch rpcMethod(body) {
		case "server/discover":
			// A legacy server does not know this method; per JSON-RPC
			// convention it stays at HTTP 200 with a wire-level error, which
			// probeEra must fall through to the legacy handshake for.
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":"probe-discover","error":{"code":-32601,"message":"method not found"}}`)
		case "initialize":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":"probe-initialize","result":{"protocolVersion":"2025-03-26"}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"x"},{"name":"y"}]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	transports := map[string]*mcp.HTTPTransport{"legacy-1": newAutoProbeTransport(srv.URL)}
	c := newMCPChecker(func() []health.MCPServerTarget {
		return []health.MCPServerTarget{newMCPTarget("legacy-1", "Legacy Server", "legacy-1")}
	}, staticTransportFor(transports))
	stop := c.Start()
	t.Cleanup(stop)

	got := c.GetHealth("legacy-1")
	if got.Status != "healthy" {
		t.Fatalf("Status = %q, want %q (LastError=%q)", got.Status, "healthy", got.LastError)
	}
	if got.ToolCount != 2 {
		t.Errorf("ToolCount = %d, want 2", got.ToolCount)
	}
}

// TestMCPHealthChecker_UnreachableUpstream_Unhealthy_SanitizedError verifies
// that a connection failure marks the server unhealthy and that LastError
// carries only a sanitized, low-information message — never the raw
// transport error, which would embed the upstream URL.
func TestMCPHealthChecker_UnreachableUpstream_Unhealthy_SanitizedError(t *testing.T) {
	t.Parallel()

	// Port 1 refuses connections immediately.
	const unreachable = "http://127.0.0.1:1"
	transports := map[string]*mcp.HTTPTransport{"probe-unreach": newAutoProbeTransport(unreachable)}
	c := newMCPChecker(func() []health.MCPServerTarget {
		return []health.MCPServerTarget{newMCPTarget("probe-unreach", "Probe Unreach", "probe-unreach")}
	}, staticTransportFor(transports))
	stop := c.Start()
	t.Cleanup(stop)

	got := c.GetHealth("probe-unreach")
	if got.Status != "unhealthy" {
		t.Errorf("Status = %q, want %q", got.Status, "unhealthy")
	}
	if got.LastError == "" {
		t.Fatal("LastError is empty, want non-empty error message")
	}
	if strings.Contains(got.LastError, unreachable) || strings.Contains(got.LastError, "127.0.0.1") {
		t.Errorf("LastError = %q leaks the upstream URL, want a sanitized message", got.LastError)
	}
}

// TestMCPHealthChecker_UpstreamJSONRPCErrorMessage_NotLeakedIntoLastError
// verifies the specific privacy guarantee mcp.HTTPTransport.ListTools
// documents (docs/mcp-v2.md §11.2/§11.5): an upstream's free-form JSON-RPC
// error message is upstream-controlled, attacker-influenceable text and must
// never reach LastError — only the numeric error code may. This is distinct
// from TestMCPHealthChecker_UnreachableUpstream_Unhealthy_SanitizedError
// above, which only exercises a network-level failure (connection refused):
// that path goes through net.Dial and never touches a JSON-RPC error body at
// all, so it cannot prove anything about upstream-controlled MESSAGE text
// being stripped. This test makes the fake upstream respond successfully at
// the HTTP layer with a JSON-RPC error body whose message field carries a
// conspicuous, unmistakable sentinel, and asserts LastError contains neither
// that sentinel nor any other trace of the upstream's message text.
func TestMCPHealthChecker_UpstreamJSONRPCErrorMessage_NotLeakedIntoLastError(t *testing.T) {
	t.Parallel()

	const sentinel = "SENTINEL-UPSTREAM-INTERNAL-DEBUG-9f3a1c-db-password-hunter2"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rpcMethod(body) != "tools/list" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":%q}}`, sentinel)
	}))
	t.Cleanup(srv.Close)

	// Pinned modern transport: skips era probing so the fake upstream only
	// ever has to answer tools/list, keeping the sentinel isolated to
	// exactly the code path under test.
	transports := map[string]*mcp.HTTPTransport{"sentinel-1": newPinnedModernTransport(srv.URL)}
	c := newMCPChecker(func() []health.MCPServerTarget {
		return []health.MCPServerTarget{newMCPTarget("sentinel-1", "Sentinel Server", "sentinel-1")}
	}, staticTransportFor(transports))
	stop := c.Start()
	t.Cleanup(stop)

	got := c.GetHealth("sentinel-1")
	if got.Status != "unhealthy" {
		t.Fatalf("Status = %q, want %q", got.Status, "unhealthy")
	}
	if got.LastError == "" {
		t.Fatal("LastError is empty, want a non-empty sanitized message")
	}
	if strings.Contains(got.LastError, sentinel) {
		t.Errorf("LastError = %q leaks the upstream's JSON-RPC error message, want it stripped to the generic sanitized message", got.LastError)
	}
	// ListTools reduces a JSON-RPC error to its numeric code alone (see its
	// doc), and that reduced message ("tools/list error: code -32000") does
	// not match any of sanitizeError's recognized patterns, so it falls
	// through to the generic fallback. Pinning the exact value means any
	// future change that lets more of the message through has to be a
	// deliberate, reviewed decision, not a silent regression.
	if got.LastError != "probe failed" {
		t.Errorf("LastError = %q, want %q", got.LastError, "probe failed")
	}
}

// TestMCPHealthChecker_CacheMiss_StaysUnknown verifies that when transportFor
// reports no transport for a target — a cold start, or a server registered
// between two transport-cache refreshes — the probe cycle skips it and its
// status stays "unknown" rather than being marked "unhealthy". VoidLLM has
// not checked the server yet, which is a different claim than having checked
// it and found it broken.
func TestMCPHealthChecker_CacheMiss_StaysUnknown(t *testing.T) {
	t.Parallel()

	c := newMCPChecker(func() []health.MCPServerTarget {
		return []health.MCPServerTarget{newMCPTarget("cold-start", "Cold Start", "cold-start")}
	}, staticTransportFor(nil)) // empty map: transportFor always reports not-found
	stop := c.Start()
	t.Cleanup(stop)

	got := c.GetHealth("cold-start")
	if got.Status != "unknown" {
		t.Errorf("Status = %q, want %q", got.Status, "unknown")
	}
	all := c.GetAllHealth()
	if len(all) != 0 {
		t.Errorf("GetAllHealth() len = %d, want 0 (skipped target must not create a record)", len(all))
	}
}

// TestMCPHealthChecker_CacheMiss_PreservesExistingRecordAcrossTransientMiss
// verifies the other half of runOne's documented cache-miss contract: a
// server that WAS successfully probed before must keep exactly that last
// known health record — untouched, including LastCheck — across any later
// cycle where transportFor transiently reports no transport, rather than
// having that stale-but-real result overwritten with "unknown" (or anything
// else). TestMCPHealthChecker_CacheMiss_StaysUnknown only covers a server
// that has NEVER been probed; a server flickering in and out of the
// transport cache between LoadAll refreshes is a materially different and
// equally real scenario that a naive "skip means reset to unknown" fix could
// silently break.
func TestMCPHealthChecker_CacheMiss_PreservesExistingRecordAcrossTransientMiss(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if rpcMethod(body) != "tools/list" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"a"},{"name":"b"}]}}`)
	}))
	t.Cleanup(srv.Close)

	transport := newPinnedModernTransport(srv.URL)
	var haveTransport atomic.Bool
	haveTransport.Store(true)

	target := newMCPTarget("flaky-cache", "Flaky Cache", "flaky-cache")
	c := health.NewMCPHealthChecker(
		func() []health.MCPServerTarget { return []health.MCPServerTarget{target} },
		func(string) (*mcp.HTTPTransport, bool) {
			if !haveTransport.Load() {
				return nil, false
			}
			return transport, true
		},
		10*time.Millisecond, newLogger(),
	)
	stop := c.Start()
	t.Cleanup(stop)

	deadline := time.Now().Add(2 * time.Second)
	for c.GetHealth("flaky-cache").Status != "healthy" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	before := c.GetHealth("flaky-cache")
	if before.Status != "healthy" {
		t.Fatalf("Status = %q before simulated cache eviction, want %q", before.Status, "healthy")
	}
	if before.ToolCount != 2 {
		t.Fatalf("ToolCount = %d before simulated cache eviction, want 2", before.ToolCount)
	}

	// Simulate the transport cache momentarily losing this server's entry
	// (e.g. between two LoadAll refreshes) and let several probe cycles pass.
	haveTransport.Store(false)
	time.Sleep(50 * time.Millisecond)

	after := c.GetHealth("flaky-cache")
	if after.Status != "healthy" {
		t.Errorf("Status = %q after transient cache miss, want %q (a transient miss must not overwrite the last known health)", after.Status, "healthy")
	}
	if !after.LastCheck.Equal(before.LastCheck) {
		t.Errorf("LastCheck changed after transient cache miss (%v -> %v), want unchanged: a skipped probe cycle must not touch the stored record at all", before.LastCheck, after.LastCheck)
	}
	if after.ToolCount != 2 {
		t.Errorf("ToolCount = %d after transient cache miss, want unchanged 2", after.ToolCount)
	}
}

// TestMCPHealthChecker_BuiltinServer_AlwaysHealthy verifies that servers with
// Source "builtin" are marked healthy without ever calling transportFor.
func TestMCPHealthChecker_BuiltinServer_AlwaysHealthy(t *testing.T) {
	t.Parallel()

	target := health.MCPServerTarget{ID: "builtin-srv", Name: "Built-in", Alias: "builtin", Source: "builtin"}

	c := newMCPChecker(func() []health.MCPServerTarget { return []health.MCPServerTarget{target} },
		func(string) (*mcp.HTTPTransport, bool) {
			t.Fatal("transportFor must not be called for a builtin server")
			return nil, false
		})
	stop := c.Start()
	t.Cleanup(stop)

	got := c.GetHealth("builtin-srv")
	if got.Status != "healthy" {
		t.Errorf("Status = %q, want %q (builtin servers are always healthy)", got.Status, "healthy")
	}
}

// TestMCPHealthChecker_StaleEntryCleanup verifies that health records for
// servers removed from the server list are deleted on the next probe cycle.
func TestMCPHealthChecker_StaleEntryCleanup(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if rpcMethod(body) != "tools/list" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	}))
	t.Cleanup(srv.Close)

	transports := map[string]*mcp.HTTPTransport{
		"stale-a": newPinnedModernTransport(srv.URL),
		"stale-b": newPinnedModernTransport(srv.URL),
	}
	twoTargets := []health.MCPServerTarget{
		newMCPTarget("stale-a", "Stale A", "stale-a"),
		newMCPTarget("stale-b", "Stale B", "stale-b"),
	}
	oneTarget := []health.MCPServerTarget{
		newMCPTarget("stale-a", "Stale A", "stale-a"),
	}

	// Phase 0: probe both servers — use a fresh checker.
	c1 := newMCPChecker(func() []health.MCPServerTarget { return twoTargets }, staticTransportFor(transports))
	stop1 := c1.Start()
	t.Cleanup(stop1)

	all := c1.GetAllHealth()
	if len(all) != 2 {
		t.Fatalf("phase 0: GetAllHealth() len = %d, want 2", len(all))
	}

	// Phase 1: use a separate checker that only knows about stale-a. When
	// Start() runs its initial probe cycle it will remove stale-b's record.
	c2 := newMCPChecker(func() []health.MCPServerTarget { return oneTarget }, staticTransportFor(transports))
	stop2 := c2.Start()
	t.Cleanup(stop2)

	all2 := c2.GetAllHealth()
	if len(all2) != 1 {
		t.Fatalf("phase 1: GetAllHealth() len = %d, want 1 (stale-b must be removed)", len(all2))
	}
	if all2[0].ServerID != "stale-a" {
		t.Errorf("phase 1: remaining server ID = %q, want %q", all2[0].ServerID, "stale-a")
	}
}

// TestMCPHealthChecker_StartStop verifies that calling Start and immediately
// stopping does not panic or deadlock.
func TestMCPHealthChecker_StartStop(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	}))
	t.Cleanup(srv.Close)

	transports := map[string]*mcp.HTTPTransport{"start-stop": newPinnedModernTransport(srv.URL)}
	c := newMCPChecker(func() []health.MCPServerTarget {
		return []health.MCPServerTarget{newMCPTarget("start-stop", "Start Stop", "start-stop")}
	}, staticTransportFor(transports))

	stop := c.Start()
	stop() // must not panic, race, or deadlock
}

// TestMCPHealthChecker_LegacyWarmup_RunsOnceAcrossMultipleProbeCycles verifies
// the second documented side effect of probing through ListTools: health
// probing and tool discovery share the empty SessionScope, so a legacy
// upstream's initialize/notifications/initialized handshake — cached on the
// *mcp.HTTPTransport itself — runs at most once for that scope's lifetime,
// not once per probe cycle. The checker reuses the same transport instance
// across every tick (transportFor is backed by a fixed map here, exactly as
// MCPTransportCache.Get behaves between LoadAll refreshes), so this pins down
// that runOne does not defeat that caching by resolving a fresh session per
// cycle.
func TestMCPHealthChecker_LegacyWarmup_RunsOnceAcrossMultipleProbeCycles(t *testing.T) {
	t.Parallel()

	var initializeCount, toolsListCount atomic.Int64
	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")

		switch rpcMethod(body) {
		case "initialize":
			initializeCount.Add(1)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":"warmup-initialize","result":{"protocolVersion":"2025-03-26"}}`)
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			toolsListCount.Add(1)
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	transports := map[string]*mcp.HTTPTransport{"legacy-warmup": newPinnedLegacyTransport(srv.URL)}
	target := newMCPTarget("legacy-warmup", "Legacy Warmup", "legacy-warmup")
	c := health.NewMCPHealthChecker(func() []health.MCPServerTarget { return []health.MCPServerTarget{target} },
		staticTransportFor(transports), 10*time.Millisecond, newLogger())

	stop := c.Start()
	t.Cleanup(stop)

	deadline := time.Now().Add(2 * time.Second)
	for toolsListCount.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	if got := toolsListCount.Load(); got < 3 {
		t.Fatalf("tools/list calls = %d, want at least 3 probe cycles to have run", got)
	}
	if got := initializeCount.Load(); got != 1 {
		t.Errorf("initialize calls = %d, want exactly 1 (warmup must run once, not per probe cycle)", got)
	}
}
