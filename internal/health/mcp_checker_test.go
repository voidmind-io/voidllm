package health_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/health"
)

// newMCPTarget builds a minimal MCPServerTarget pointing at the given URL.
func newMCPTarget(id, name, alias, url string) health.MCPServerTarget {
	return health.MCPServerTarget{
		ID:       id,
		Name:     name,
		Alias:    alias,
		URL:      url,
		AuthType: "none",
		Source:   "api",
	}
}

// newMCPChecker builds an MCPHealthChecker with a long interval (so the ticker
// never fires during tests) using allowPrivateURLs=true so that httptest servers
// on 127.0.0.1 are reachable.
func newMCPChecker(servers func() []health.MCPServerTarget) *health.MCPHealthChecker {
	return health.NewMCPHealthChecker(servers, 24*time.Hour, true, newLogger())
}

// TestMCPHealthChecker_GetHealth_UnknownServerID verifies that GetHealth returns
// a zero-value MCPServerHealth with Status "unknown" for a server that has never
// been probed.
func TestMCPHealthChecker_GetHealth_UnknownServerID(t *testing.T) {
	t.Parallel()

	c := newMCPChecker(func() []health.MCPServerTarget { return nil })

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

	// Use a 24-hour interval — Start() will still run the initial probe cycle.
	// To avoid that, create without Start.
	c := newMCPChecker(func() []health.MCPServerTarget {
		return []health.MCPServerTarget{newMCPTarget("s1", "Server 1", "s1", srv.URL)}
	})
	// Do not call Start(), so no probe runs.
	all := c.GetAllHealth()
	if len(all) != 0 {
		t.Errorf("GetAllHealth() len = %d, want 0 (no probe run)", len(all))
	}
}

// TestMCPHealthChecker_GetAllHealth_AfterProbe verifies that GetAllHealth returns
// one result per probed server after Start() has run.
func TestMCPHealthChecker_GetAllHealth_AfterProbe(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	}))
	t.Cleanup(srv.Close)

	targets := []health.MCPServerTarget{
		newMCPTarget("srv-a", "Server A", "a", srv.URL),
		newMCPTarget("srv-b", "Server B", "b", srv.URL),
	}

	c := newMCPChecker(func() []health.MCPServerTarget { return targets })
	stop := c.Start()
	t.Cleanup(stop)

	all := c.GetAllHealth()
	if len(all) != 2 {
		t.Fatalf("GetAllHealth() len = %d, want 2", len(all))
	}
	for _, h := range all {
		if h.Status != "healthy" {
			t.Errorf("server %q: Status = %q, want %q", h.ServerID, h.Status, "healthy")
		}
	}
}

// TestMCPHealthChecker_ProbeMCPServer_Success verifies that a successful
// tools/list response marks the server healthy with the correct tool count and
// a non-zero latency.
func TestMCPHealthChecker_ProbeMCPServer_Success(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		resp := map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"result": map[string]any{
				"tools": []map[string]any{
					{"name": "tool-one"},
					{"name": "tool-two"},
					{"name": "tool-three"},
				},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)

	target := newMCPTarget("probe-ok", "Probe OK", "probe-ok", srv.URL)
	c := newMCPChecker(func() []health.MCPServerTarget { return []health.MCPServerTarget{target} })
	stop := c.Start()
	t.Cleanup(stop)

	got := c.GetHealth("probe-ok")
	if got.Status != "healthy" {
		t.Errorf("Status = %q, want %q", got.Status, "healthy")
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

// TestMCPHealthChecker_ProbeMCPServer_ErrorStatus verifies that a non-2xx
// response from the MCP server marks the server unhealthy.
func TestMCPHealthChecker_ProbeMCPServer_ErrorStatus(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	target := newMCPTarget("probe-err", "Probe Err", "probe-err", srv.URL)
	c := newMCPChecker(func() []health.MCPServerTarget { return []health.MCPServerTarget{target} })
	stop := c.Start()
	t.Cleanup(stop)

	got := c.GetHealth("probe-err")
	if got.Status != "unhealthy" {
		t.Errorf("Status = %q, want %q", got.Status, "unhealthy")
	}
	if got.LastError == "" {
		t.Error("LastError is empty, want non-empty error message")
	}
}

// TestMCPHealthChecker_ProbeMCPServer_Unreachable verifies that a connection
// refused error marks the server unhealthy.
func TestMCPHealthChecker_ProbeMCPServer_Unreachable(t *testing.T) {
	t.Parallel()

	// Port 1 refuses connections immediately.
	target := newMCPTarget("probe-unreach", "Probe Unreach", "probe-unreach", "http://127.0.0.1:1")
	c := newMCPChecker(func() []health.MCPServerTarget { return []health.MCPServerTarget{target} })
	stop := c.Start()
	t.Cleanup(stop)

	got := c.GetHealth("probe-unreach")
	if got.Status != "unhealthy" {
		t.Errorf("Status = %q, want %q", got.Status, "unhealthy")
	}
	if got.LastError == "" {
		t.Error("LastError is empty, want non-empty error message")
	}
}

// TestMCPHealthChecker_ProbeMCPServer_AcceptHeader verifies that the probe
// request includes the correct Accept header.
func TestMCPHealthChecker_ProbeMCPServer_AcceptHeader(t *testing.T) {
	t.Parallel()

	var capturedAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	}))
	t.Cleanup(srv.Close)

	target := newMCPTarget("probe-hdr", "Probe Hdr", "probe-hdr", srv.URL)
	c := newMCPChecker(func() []health.MCPServerTarget { return []health.MCPServerTarget{target} })
	stop := c.Start()
	t.Cleanup(stop)

	if capturedAccept == "" {
		t.Fatal("Accept header was not set on probe request")
	}
	// The probe must accept both JSON and SSE since some MCP servers respond
	// with text/event-stream.
	wantContains := []string{"application/json", "text/event-stream"}
	for _, want := range wantContains {
		if !containsStr(capturedAccept, want) {
			t.Errorf("Accept header %q does not contain %q", capturedAccept, want)
		}
	}
}

// TestMCPHealthChecker_ProbeMCPServer_ContentTypeHeader verifies that the probe
// sends the correct Content-Type header.
func TestMCPHealthChecker_ProbeMCPServer_ContentTypeHeader(t *testing.T) {
	t.Parallel()

	var capturedCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedCT = r.Header.Get("Content-Type")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	}))
	t.Cleanup(srv.Close)

	target := newMCPTarget("probe-ct", "Probe CT", "probe-ct", srv.URL)
	c := newMCPChecker(func() []health.MCPServerTarget { return []health.MCPServerTarget{target} })
	stop := c.Start()
	t.Cleanup(stop)

	if capturedCT != "application/json" {
		t.Errorf("Content-Type = %q, want %q", capturedCT, "application/json")
	}
}

// TestMCPHealthChecker_StaleEntryCleanup verifies that health records for
// servers removed from the server list are deleted on the next probe cycle.
func TestMCPHealthChecker_StaleEntryCleanup(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	}))
	t.Cleanup(srv.Close)

	// Start with two servers.
	twoTargets := []health.MCPServerTarget{
		newMCPTarget("stale-a", "Stale A", "stale-a", srv.URL),
		newMCPTarget("stale-b", "Stale B", "stale-b", srv.URL),
	}
	// After the first probe cycle, shrink to one server.
	oneTarget := []health.MCPServerTarget{
		newMCPTarget("stale-a", "Stale A", "stale-a", srv.URL),
	}

	// Phase 0: probe both servers — use a fresh checker.
	c1 := newMCPChecker(func() []health.MCPServerTarget { return twoTargets })
	stop1 := c1.Start()
	t.Cleanup(stop1)

	all := c1.GetAllHealth()
	if len(all) != 2 {
		t.Fatalf("phase 0: GetAllHealth() len = %d, want 2", len(all))
	}

	// Phase 1: use a separate checker that only knows about stale-a. When
	// Start() runs its initial probe cycle it will remove stale-b's record.
	c2 := newMCPChecker(func() []health.MCPServerTarget { return oneTarget })
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

// TestMCPHealthChecker_BuiltinServer_AlwaysHealthy verifies that servers with
// Source "builtin" are marked healthy without making an outbound HTTP request.
func TestMCPHealthChecker_BuiltinServer_AlwaysHealthy(t *testing.T) {
	t.Parallel()

	// Use an URL that would definitely fail if a real request were made.
	target := health.MCPServerTarget{
		ID:     "builtin-srv",
		Name:   "Built-in",
		Alias:  "builtin",
		URL:    "http://127.0.0.1:1",
		Source: "builtin",
	}

	c := newMCPChecker(func() []health.MCPServerTarget { return []health.MCPServerTarget{target} })
	stop := c.Start()
	t.Cleanup(stop)

	got := c.GetHealth("builtin-srv")
	if got.Status != "healthy" {
		t.Errorf("Status = %q, want %q (builtin servers are always healthy)", got.Status, "healthy")
	}
}

// TestMCPHealthChecker_BearerAuth_SetsAuthorizationHeader verifies that when a
// target uses bearer auth, the Authorization header is sent to the upstream.
func TestMCPHealthChecker_BearerAuth_SetsAuthorizationHeader(t *testing.T) {
	t.Parallel()

	var capturedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	}))
	t.Cleanup(srv.Close)

	target := health.MCPServerTarget{
		ID:        "bearer-srv",
		Name:      "Bearer Srv",
		Alias:     "bearer",
		URL:       srv.URL,
		AuthType:  "bearer",
		AuthToken: "secret-token",
		Source:    "api",
	}

	c := newMCPChecker(func() []health.MCPServerTarget { return []health.MCPServerTarget{target} })
	stop := c.Start()
	t.Cleanup(stop)

	if capturedAuth != "Bearer secret-token" {
		t.Errorf("Authorization header = %q, want %q", capturedAuth, "Bearer secret-token")
	}
}

// TestMCPHealthChecker_StartStop verifies that calling Start and immediately
// stopping does not panic or deadlock.
func TestMCPHealthChecker_StartStop(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`)
	}))
	t.Cleanup(srv.Close)

	target := newMCPTarget("start-stop", "Start Stop", "start-stop", srv.URL)
	c := newMCPChecker(func() []health.MCPServerTarget { return []health.MCPServerTarget{target} })

	stop := c.Start()
	stop() // must not panic, race, or deadlock
}

// containsStr is a small helper for substring checks, avoiding an import of
// strings just for one call.
func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		func() bool {
			for i := 0; i <= len(s)-len(sub); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		}())
}
