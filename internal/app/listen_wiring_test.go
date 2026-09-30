package app

// White-box tests (package app, not app_test) for reconcileMCPListenTargets
// and the Code Mode / mcpListenManager wiring in app.go — see code_mode_test.go's
// own doc for why this package's tests already construct an Application (or,
// as here, its unexported collaborators) directly rather than going through
// the heavy app.New() startup path.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
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

	manager := mcp.NewListenManager(func(context.Context, string) {})
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

	manager := mcp.NewListenManager(func(context.Context, string) {})
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
//
// These four tests are the end-to-end wiring tests for the Code Mode /
// ListenManager / ToolCache triangle app.go's New wires together, mirroring
// that wiring's own callback exactly: on a tools-changed notification it
// calls ToolCache.RefreshServer — never Invalidate. See app.go's own comment
// at that wiring for why: Code Mode's toolsListHook (code_mode.go) reads
// ToolCache.GetAllTools, a pure snapshot that never triggers a fetch of its
// own, so a plain Invalidate would leave a changed server missing from every
// tools/list response — not merely stale — until some UNRELATED caller
// happened to call GetTools for it first.

// newWiringToolCacheTarget builds the db.MCPServer/MCPServerCache/
// MCPTransportCache triple every test below needs to give serverID's
// listenWiringServer a subscriptions/listen connection via
// reconcileMCPListenTargets, returning the Application ready for that call.
func newWiringToolCacheTarget(t *testing.T, serverID, url string, manager *mcp.ListenManager) *Application {
	t.Helper()
	servers := []db.MCPServer{
		{ID: serverID, Alias: serverID, URL: url, AuthType: "none", IsActive: true, ProtocolVersion: "2026-07-28"},
	}
	serverCache := proxy.NewMCPServerCache()
	serverCache.LoadAll(servers)
	transportCache := newWiringTransportCache()
	t.Cleanup(transportCache.Close)
	transportCache.LoadAll(servers)

	return &Application{
		mcpServerCache:    serverCache,
		mcpTransportCache: transportCache,
		mcpListenManager:  manager,
	}
}

// TestReconcileMCPListenTargets_ListChanged_RefreshesToolCacheEagerly is the
// direct regression test for the bug this wiring used to have: a
// notifications/tools/list_changed event must EAGERLY re-fetch and publish
// serverID's tool listing — proven here by reading ToolCache.GetAllTools
// (the exact snapshot toolsListHook itself reads) without this test, or
// anyone else, ever calling GetTools again. The old Invalidate-based wiring
// would have left GetAllTools reporting nothing at all for serverID at that
// point instead.
func TestReconcileMCPListenTargets_ListChanged_RefreshesToolCacheEagerly(t *testing.T) {
	t.Parallel()

	const serverID = "wired-server"

	connected := make(chan struct{}, 4)
	srv := listenWiringServer(t, connected, listenWiringAckEvent+listenWiringListChangedEvent)

	var fetchCount atomic.Int32
	fetcher := func(_ context.Context, _ string) (*mcp.ToolListing, error) {
		fetchCount.Add(1)
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: fmt.Sprintf("tool-%d", fetchCount.Load())}}}, nil
	}
	toolCache := mcp.NewToolCache(fetcher, time.Hour) // long maxAge: only RefreshServer should force a refetch

	if _, err := toolCache.GetTools(context.Background(), serverID); err != nil {
		t.Fatalf("initial GetTools() error = %v", err)
	}
	if got := fetchCount.Load(); got != 1 {
		t.Fatalf("fetchCount after initial GetTools() = %d, want 1", got)
	}

	refreshed := make(chan error, 4)
	manager := mcp.NewListenManager(func(ctx context.Context, id string) {
		refreshed <- toolCache.RefreshServer(ctx, id)
	})
	t.Cleanup(manager.Stop)

	a := newWiringToolCacheTarget(t, serverID, srv.URL, manager)
	a.reconcileMCPListenTargets()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("the server never received its subscriptions/listen connection")
	}

	select {
	case err := <-refreshed:
		if err != nil {
			t.Fatalf("RefreshServer triggered by list_changed returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ToolCache.RefreshServer was never called after the list_changed notification")
	}

	// No GetTools call happens between the wait above and this read: the
	// refreshed listing must already be published by the time RefreshServer
	// itself returned.
	tools := toolCache.GetAllTools()[serverID]
	if len(tools) != 1 || tools[0].Name != "tool-2" {
		t.Fatalf("GetAllTools()[%q] = %v, want a single tool-2 entry published by the eager refresh", serverID, tools)
	}
	if got := fetchCount.Load(); got != 2 {
		t.Errorf("fetchCount after the list_changed refresh = %d, want 2", got)
	}
}

// TestReconcileMCPListenTargets_ListChanged_FailedRefreshKeepsOldListing
// verifies that when the upstream fetch RefreshServer triggers fails, the
// previously cached listing is left untouched — never cleared — matching
// RefreshServer's own documented contract ("on fetch failure the existing
// cache entry is preserved").
func TestReconcileMCPListenTargets_ListChanged_FailedRefreshKeepsOldListing(t *testing.T) {
	t.Parallel()

	const serverID = "wired-server-fail"

	connected := make(chan struct{}, 4)
	srv := listenWiringServer(t, connected, listenWiringAckEvent+listenWiringListChangedEvent)

	var fetchCount atomic.Int32
	fetcher := func(_ context.Context, _ string) (*mcp.ToolListing, error) {
		if fetchCount.Add(1) == 1 {
			return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "tool-1"}}}, nil
		}
		return nil, errors.New("upstream unavailable")
	}
	toolCache := mcp.NewToolCache(fetcher, time.Hour)

	if _, err := toolCache.GetTools(context.Background(), serverID); err != nil {
		t.Fatalf("initial GetTools() error = %v", err)
	}

	refreshed := make(chan error, 4)
	manager := mcp.NewListenManager(func(ctx context.Context, id string) {
		refreshed <- toolCache.RefreshServer(ctx, id)
	})
	t.Cleanup(manager.Stop)

	a := newWiringToolCacheTarget(t, serverID, srv.URL, manager)
	a.reconcileMCPListenTargets()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("the server never received its subscriptions/listen connection")
	}

	select {
	case err := <-refreshed:
		if err == nil {
			t.Fatal("expected the list_changed-triggered RefreshServer to fail, got nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ToolCache.RefreshServer was never called after the list_changed notification")
	}

	tools := toolCache.GetAllTools()[serverID]
	if len(tools) != 1 || tools[0].Name != "tool-1" {
		t.Fatalf("GetAllTools()[%q] = %v, want the pre-refresh tool-1 entry preserved after a failed refresh", serverID, tools)
	}
}

// TestReconcileMCPListenTargets_ListChanged_OnChangeFiresOnlyAfterPublish
// verifies the ordering app.go's own "Trigger 1" wiring comment documents:
// ToolCache.SetOnChange's hook — which production wires to
// codeModeServer.NotifyToolsListChanged — must never fire before the
// refreshed listing it is announcing has actually been published; by the
// time the hook installed here runs, GetAllTools must already report the new
// listing, not the old one.
func TestReconcileMCPListenTargets_ListChanged_OnChangeFiresOnlyAfterPublish(t *testing.T) {
	t.Parallel()

	const serverID = "wired-server-notify"

	connected := make(chan struct{}, 4)
	srv := listenWiringServer(t, connected, listenWiringAckEvent+listenWiringListChangedEvent)

	var fetchCount atomic.Int32
	fetcher := func(_ context.Context, _ string) (*mcp.ToolListing, error) {
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: fmt.Sprintf("tool-%d", fetchCount.Add(1))}}}, nil
	}
	toolCache := mcp.NewToolCache(fetcher, time.Hour)

	if _, err := toolCache.GetTools(context.Background(), serverID); err != nil {
		t.Fatalf("initial GetTools() error = %v", err)
	}

	publishedBeforeNotify := make(chan bool, 4)
	toolCache.SetOnChange(func(id string) {
		tools := toolCache.GetAllTools()[id]
		publishedBeforeNotify <- len(tools) == 1 && tools[0].Name == "tool-2"
	})

	manager := mcp.NewListenManager(func(ctx context.Context, id string) {
		_ = toolCache.RefreshServer(ctx, id)
	})
	t.Cleanup(manager.Stop)

	a := newWiringToolCacheTarget(t, serverID, srv.URL, manager)
	a.reconcileMCPListenTargets()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("the server never received its subscriptions/listen connection")
	}

	select {
	case ok := <-publishedBeforeNotify:
		if !ok {
			t.Fatal("SetOnChange fired before the refreshed listing was published")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ToolCache.SetOnChange was never called after the list_changed notification")
	}
}

// TestListenManager_Stop_DoesNotBlockOnStuckToolsChangedRefresh_NoLeak is the
// direct regression test for spawnToolsChangedRefresh's own "no leak" claim
// (listen_manager.go): the goroutine ListenManager itself spawns to run a
// tools-changed refresh (tracked under m.wg) must not be held open by Stop
// past the moment Stop cancels this target's own listener context, even when
// the upstream fetch that refresh triggered never returns at all.
//
// This deliberately does NOT assert that the underlying upstream fetch
// itself gets cancelled — it does not, by ToolCache's own design: sharedFetch
// (tool_cache.go) runs the actual fetch against a context "fully detached
// from every caller's context" specifically so one caller's cancellation can
// never sever a shared fetch other callers are still joined to, and only
// races the CALLER's own ctx against that detached fetch to decide when to
// return to THAT caller. What ends the goroutine ListenManager itself owns —
// and so what this test proves — is that RefreshServer (via sharedFetch's
// same ctx-losing-race path) returns to spawnToolsChangedRefresh's goroutine
// promptly once ctx ends, letting it call m.wg.Done() and so let Stop's own
// wg.Wait() return, regardless of how long the now-orphaned upstream fetch
// keeps running on its own, separately bounded by tc.fetchTimeout.
func TestListenManager_Stop_DoesNotBlockOnStuckToolsChangedRefresh_NoLeak(t *testing.T) {
	t.Parallel()

	const serverID = "wired-server-stop"

	connected := make(chan struct{}, 4)
	srv := listenWiringServer(t, connected, listenWiringAckEvent+listenWiringListChangedEvent)

	fetcherEntered := make(chan struct{})
	// release is deliberately never closed within the test body itself — this
	// fetcher stands in for an upstream that has stopped answering entirely,
	// ignoring ctx exactly as an upstream naturally would (a real HTTP round
	// trip only ends because the underlying net.Conn errors or the resolved
	// fetchCtx's own timeout eventually fires, not because SOME OTHER
	// caller's ctx ended). t.Cleanup unblocks it after the test's own
	// assertions are done, purely so this goroutine does not outlive the test
	// binary itself.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var fetcherEnteredOnce sync.Once
	fetcher := func(_ context.Context, _ string) (*mcp.ToolListing, error) {
		fetcherEnteredOnce.Do(func() { close(fetcherEntered) })
		<-release
		return nil, errors.New("unreachable: release is only closed by t.Cleanup, after every assertion below")
	}
	toolCache := mcp.NewToolCache(fetcher, time.Hour)

	manager := mcp.NewListenManager(func(ctx context.Context, id string) {
		_ = toolCache.RefreshServer(ctx, id)
	})

	a := newWiringToolCacheTarget(t, serverID, srv.URL, manager)
	a.reconcileMCPListenTargets()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("the server never received its subscriptions/listen connection")
	}

	select {
	case <-fetcherEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("the list_changed-triggered refresh never reached the (blocking) fetcher")
	}

	stopDone := make(chan struct{})
	go func() {
		defer close(stopDone)
		manager.Stop()
	}()

	select {
	case <-stopDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() blocked on a tools-changed refresh whose upstream fetch never returns — spawnToolsChangedRefresh's own goroutine leaked past Stop")
	}
}
