package app

// White-box tests (package app, not app_test) for reconcileMCPListenTargets
// and the Code Mode / mcpListenManager wiring in app.go — see code_mode_test.go's
// own doc for why this package's tests already construct an Application (or,
// as here, its unexported collaborators) directly rather than going through
// the heavy app.New() startup path.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

const listenWiringAckEvent = "event: message\n" +
	`data: {"jsonrpc":"2.0","method":"notifications/subscriptions/acknowledged","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"voidllm-listen"},"notifications":{"toolsListChanged":true}}}` +
	"\n\n"

const listenWiringListChangedEvent = "event: message\n" +
	`data: {"jsonrpc":"2.0","method":"notifications/tools/list_changed","params":{"_meta":{"io.modelcontextprotocol/subscriptionId":"voidllm-listen"}}}` +
	"\n\n"

// listenWiringServer builds an httptest.Server whose handler records each
// connection's arrival on connected, writes body as its subscriptions/listen
// SSE response, and then blocks until the request's own context ends (torn
// down by ListenManager, exactly as every listener test in internal/mcp
// itself already relies on).
func listenWiringServer(t *testing.T, connected chan<- struct{}, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connected <- struct{}{}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newWiringTransportCache builds a real, empty *proxy.MCPTransportCache with
// SSRF protection disabled (loopback httptest servers) and short call/stream
// timeouts, ready for LoadAll.
func newWiringTransportCache() *proxy.MCPTransportCache {
	return proxy.NewMCPTransportCache(make([]byte, 32), true, 5*time.Second, 5*time.Second, slog.Default())
}

// ---- reconcileMCPListenTargets: Code Mode disabled -------------------------

// TestReconcileMCPListenTargets_NilManager_NoOp is the direct regression test
// for the "Code Mode disabled" wiring: New only ever constructs a
// *mcp.ListenManager inside the Code Mode Enabled block (app.go), leaving
// a.mcpListenManager nil otherwise — reconcileMCPListenTargets' own doc
// promises this is then a no-op. This must hold even with every other
// collaborator (mcpServerCache, mcpTransportCache) also left at its zero
// value, exactly as they are on an Application built with Code Mode disabled
// before either cache is ever populated.
func TestReconcileMCPListenTargets_NilManager_NoOp(t *testing.T) {
	t.Parallel()

	a := &Application{}
	a.reconcileMCPListenTargets() // must not panic
}

// ---- reconcileMCPListenTargets: filters inactive servers -------------------

// TestReconcileMCPListenTargets_SkipsInactiveServers verifies that only
// IS_active servers with a resolved transport are handed to
// ListenManager.Reconcile — an inactive server's upstream must never see a
// subscriptions/listen connection at all.
func TestReconcileMCPListenTargets_SkipsInactiveServers(t *testing.T) {
	t.Parallel()

	activeConnected := make(chan struct{}, 4)
	inactiveConnected := make(chan struct{}, 4)
	activeSrv := listenWiringServer(t, activeConnected, listenWiringAckEvent)
	inactiveSrv := listenWiringServer(t, inactiveConnected, listenWiringAckEvent)

	servers := []db.MCPServer{
		{ID: "active-1", Alias: "active-1", URL: activeSrv.URL, AuthType: "none", IsActive: true, ProtocolVersion: "2026-07-28"},
		{ID: "inactive-1", Alias: "inactive-1", URL: inactiveSrv.URL, AuthType: "none", IsActive: false, ProtocolVersion: "2026-07-28"},
	}

	serverCache := proxy.NewMCPServerCache()
	serverCache.LoadAll(servers)
	transportCache := newWiringTransportCache()
	t.Cleanup(transportCache.Close)
	transportCache.LoadAll(servers)

	manager := mcp.NewListenManager(func(string) {})
	t.Cleanup(manager.Stop)

	a := &Application{
		mcpServerCache:    serverCache,
		mcpTransportCache: transportCache,
		mcpListenManager:  manager,
	}
	a.reconcileMCPListenTargets()

	select {
	case <-activeConnected:
	case <-time.After(2 * time.Second):
		t.Fatal("the active server never received a subscriptions/listen connection")
	}

	select {
	case <-inactiveConnected:
		t.Fatal("the inactive server received a subscriptions/listen connection, want none")
	case <-time.After(200 * time.Millisecond):
	}
}

// ---- reconcileMCPListenTargets: skips servers pinned to a legacy revision --

// TestReconcileMCPListenTargets_SkipsPinnedLegacyServer verifies that a
// server whose protocol_version column pins it to a pre-2026-07-28 revision
// never gets a subscriptions/listen connection at all — that is already
// known from configuration (mcp.ResolvePinnedVersion + Version.Era), with no
// need to probe the upstream to find out. A server pinned to the MODERN
// revision, alongside it in the same Reconcile call, still gets one — the
// skip is specific to a legacy pin, not a blanket "never listen unless
// unpinned" rule.
func TestReconcileMCPListenTargets_SkipsPinnedLegacyServer(t *testing.T) {
	t.Parallel()

	legacyConnected := make(chan struct{}, 4)
	modernConnected := make(chan struct{}, 4)
	legacySrv := listenWiringServer(t, legacyConnected, listenWiringAckEvent)
	modernSrv := listenWiringServer(t, modernConnected, listenWiringAckEvent)

	servers := []db.MCPServer{
		{ID: "legacy-1", Alias: "legacy-1", URL: legacySrv.URL, AuthType: "none", IsActive: true, ProtocolVersion: "2025-06-18"},
		{ID: "modern-1", Alias: "modern-1", URL: modernSrv.URL, AuthType: "none", IsActive: true, ProtocolVersion: "2026-07-28"},
	}

	serverCache := proxy.NewMCPServerCache()
	serverCache.LoadAll(servers)
	transportCache := newWiringTransportCache()
	t.Cleanup(transportCache.Close)
	transportCache.LoadAll(servers)

	manager := mcp.NewListenManager(func(string) {})
	t.Cleanup(manager.Stop)

	a := &Application{
		mcpServerCache:    serverCache,
		mcpTransportCache: transportCache,
		mcpListenManager:  manager,
	}
	a.reconcileMCPListenTargets()

	select {
	case <-modernConnected:
	case <-time.After(2 * time.Second):
		t.Fatal("the modern (pinned) server never received a subscriptions/listen connection")
	}

	select {
	case <-legacyConnected:
		t.Fatal("the legacy-pinned server received a subscriptions/listen connection, want none")
	case <-time.After(200 * time.Millisecond):
	}
}

// ---- reconcileMCPListenTargets + real ListenManager + real ToolCache ------

// TestReconcileMCPListenTargets_ListChanged_InvalidatesToolCache_LazyRefetch
// is the end-to-end wiring test for the Code Mode / ListenManager /
// ToolCache triangle app.go's New wires together: a
// notifications/tools/list_changed event on a reconciled target's stream
// must invalidate ToolCache's cached listing for that exact server, so the
// NEXT GetTools call refetches from upstream instead of returning the
// already-cached listing.
func TestReconcileMCPListenTargets_ListChanged_InvalidatesToolCache_LazyRefetch(t *testing.T) {
	t.Parallel()

	const serverID = "wired-server"

	connected := make(chan struct{}, 4)
	srv := listenWiringServer(t, connected, listenWiringAckEvent+listenWiringListChangedEvent)

	var fetchCount atomic.Int32
	fetcher := func(_ context.Context, _ string) (*mcp.ToolListing, error) {
		fetchCount.Add(1)
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: fmt.Sprintf("tool-%d", fetchCount.Load())}}}, nil
	}
	toolCache := mcp.NewToolCache(fetcher, time.Hour) // long maxAge: only Invalidate should force a refetch

	if _, err := toolCache.GetTools(context.Background(), serverID); err != nil {
		t.Fatalf("initial GetTools() error = %v", err)
	}
	if got := fetchCount.Load(); got != 1 {
		t.Fatalf("fetchCount after initial GetTools() = %d, want 1", got)
	}

	invalidated := make(chan string, 4)
	manager := mcp.NewListenManager(func(id string) {
		toolCache.Invalidate(id)
		invalidated <- id
	})
	t.Cleanup(manager.Stop)

	servers := []db.MCPServer{
		{ID: serverID, Alias: serverID, URL: srv.URL, AuthType: "none", IsActive: true, ProtocolVersion: "2026-07-28"},
	}
	serverCache := proxy.NewMCPServerCache()
	serverCache.LoadAll(servers)
	transportCache := newWiringTransportCache()
	t.Cleanup(transportCache.Close)
	transportCache.LoadAll(servers)

	a := &Application{
		mcpServerCache:    serverCache,
		mcpTransportCache: transportCache,
		mcpListenManager:  manager,
	}
	a.reconcileMCPListenTargets()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("the server never received its subscriptions/listen connection")
	}

	select {
	case got := <-invalidated:
		if got != serverID {
			t.Errorf("invalidated serverID = %q, want %q", got, serverID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ToolCache.Invalidate was never called after the list_changed notification")
	}

	if _, err := toolCache.GetTools(context.Background(), serverID); err != nil {
		t.Fatalf("post-invalidation GetTools() error = %v", err)
	}
	if got := fetchCount.Load(); got != 2 {
		t.Errorf("fetchCount after post-invalidation GetTools() = %d, want 2 (a lazy refetch triggered by the invalidation)", got)
	}
}
