package app

// White-box tests for the tenant scoping the app.go wiring gives Code Mode's
// subscriptions/listen support: a Code Mode *mcp.Server whose AccessChecker
// is codeModeAccessChecker (code_mode.go), driven the same way app.go wires
// it — ToolCache.SetOnChange forwarding to Server.NotifyToolsListChanged
// scoped by ServerID, and a direct NotifyScope{OrgID} call standing in for
// AfterMCPAccessRefresh's own wiring (app.go, Trigger 2) — without needing a
// real database, HTTP server, or admin.Handler.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

// notifyScopingListenBody builds a minimal, well-formed modern-era
// subscriptions/listen request body requesting toolsListChanged — the two
// MUST _meta fields (docs/mcp-v2.md §3.2) plus the notifications filter,
// hand-built here since modernRequestBody (internal/mcp's own test fixture)
// lives in that package's _test.go files and is not visible from here.
func notifyScopingListenBody(id int) string {
	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "subscriptions/listen",
		"params": map[string]any{
			"notifications": map[string]any{"toolsListChanged": true},
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	}
	b, err := json.Marshal(req)
	if err != nil {
		panic(err) // static, JSON-marshalable test fixture data
	}
	return string(b)
}

// notifyScopingModernHeader negotiates the 2026-07-28 era, matching
// notifyScopingListenBody's own _meta.protocolVersion.
func notifyScopingModernHeader() mcp.Header {
	return mcp.MapHeader{mcp.HeaderProtocolVersion: "2026-07-28"}
}

// registerNotifyScopingListener registers one subscriptions/listen subscriber
// against s for identity, failing the test unless registration honored
// toolsListChanged (requires s.SetToolsListChangedSource(true) to have
// already been called).
func registerNotifyScopingListener(t *testing.T, s *mcp.Server, id mcp.KeyIdentity, reqID int) *mcp.Subscriber {
	t.Helper()
	ctx := mcp.WithKeyIdentity(context.Background(), id)
	result := s.Handle(ctx, []byte(notifyScopingListenBody(reqID)), notifyScopingModernHeader())
	if result.Listen == nil {
		t.Fatalf("registration for %+v failed: %s", id, result.Body)
	}
	if !result.Listen.Sub.Honored().ToolsListChanged {
		t.Fatalf("registration for %+v did not honor toolsListChanged — fixture bug", id)
	}
	t.Cleanup(result.Listen.Sub.Unregister)
	return result.Listen.Sub
}

// assertScopingEventPending and assertScopingNoEvent read directly from
// sub.Events(): every delivery path exercised in this file
// (subscriberRegistry.notify → Subscriber.deliver, and ToolCache.fireOnChange
// calling straight into it) is synchronous, single-goroutine work — by the
// time Invalidate/NotifyToolsListChanged returns, delivery (or its absence)
// has already happened, so no wait is needed to observe it, only a bounded
// non-blocking check.
func assertScopingEventPending(t *testing.T, sub *mcp.Subscriber) {
	t.Helper()
	select {
	case <-sub.Events():
	default:
		t.Error("no pending event, want one")
	}
}

func assertScopingNoEvent(t *testing.T, sub *mcp.Subscriber) {
	t.Helper()
	select {
	case body := <-sub.Events():
		t.Errorf("unexpected pending event: %s", body)
	default:
	}
}

// newNotifyScopingFixture builds the same triangle app.go wires together for
// Code Mode's subscriptions/listen support (New, internal/app/app.go): a
// Code Mode-style *mcp.Server with SetToolsListChangedSource(true) and
// codeModeAccessChecker installed, plus a *mcp.ToolCache whose SetOnChange
// forwards straight into NotifyToolsListChanged scoped by ServerID — Trigger
// 1 in app.go's own wiring comment. serverCache is pre-loaded with two
// org-scoped servers, "sv-a" (org-a) and "sv-b" (org-b), each with one static
// tool so an initial GetTools populates the cache without itself being the
// interesting event under test.
func newNotifyScopingFixture(t *testing.T) (codeModeServer *mcp.Server, toolCache *mcp.ToolCache) {
	t.Helper()

	svA := db.MCPServer{ID: "sv-a", OrgID: ptrStr("org-a")}
	svB := db.MCPServer{ID: "sv-b", OrgID: ptrStr("org-b")}
	serverCache := &staticServerCache{byID: map[string]*db.MCPServer{
		"sv-a": &svA,
		"sv-b": &svB,
	}}

	codeModeServer = mcp.NewServer("code-mode", "test")
	codeModeServer.SetToolsListChangedSource(true)
	codeModeServer.SetAccessChecker(codeModeAccessChecker(serverCache, (*proxy.MCPAccessCache)(nil)))

	fetcher := func(_ context.Context, serverID string) (*mcp.ToolListing, error) {
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "tool-" + serverID}}}, nil
	}
	toolCache = mcp.NewToolCache(fetcher, time.Hour)
	toolCache.SetOnChange(func(serverID string) {
		codeModeServer.NotifyToolsListChanged(mcp.NotifyScope{ServerID: serverID})
	})

	// Warm both entries so the interesting Invalidate below is a genuine
	// "listing changed on an already-published entry" event, not the
	// always-changed first publish (toolsListingChanged's own doc).
	if _, err := toolCache.GetTools(context.Background(), "sv-a"); err != nil {
		t.Fatalf("warm sv-a: %v", err)
	}
	if _, err := toolCache.GetTools(context.Background(), "sv-b"); err != nil {
		t.Fatalf("warm sv-b: %v", err)
	}

	return codeModeServer, toolCache
}

// TestMCPNotifyScoping_ToolCacheInvalidate_OnlyNotifiesSubscribersWithAccess
// is the full triangle test: a ToolCache.Invalidate for an org-scoped server
// of org A — exactly what a ListenManager relay or an admin mutation would
// trigger — reaches only org A's own subscriber, never org B's, even though
// both are registered on the SAME Code Mode server instance and both honor
// toolsListChanged.
func TestMCPNotifyScoping_ToolCacheInvalidate_OnlyNotifiesSubscribersWithAccess(t *testing.T) {
	t.Parallel()

	codeModeServer, toolCache := newNotifyScopingFixture(t)

	subA := registerNotifyScopingListener(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-a", KeyID: "key-a"}, 1)
	subB := registerNotifyScopingListener(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-b", KeyID: "key-b"}, 2)

	toolCache.Invalidate("sv-a")

	assertScopingEventPending(t, subA)
	assertScopingNoEvent(t, subB)
}

// TestMCPNotifyScoping_ToolCacheInvalidate_TeamScopedServer_OnlyThatTeam
// verifies the same triangle for a team-scoped server: only a subscriber
// whose own KeyIdentity.TeamID matches exactly is notified — an org sibling
// on a DIFFERENT team within the SAME org must not be.
func TestMCPNotifyScoping_ToolCacheInvalidate_TeamScopedServer_OnlyThatTeam(t *testing.T) {
	t.Parallel()

	teamServer := db.MCPServer{ID: "sv-team", OrgID: ptrStr("org-1"), TeamID: ptrStr("team-1")}
	serverCache := &staticServerCache{byID: map[string]*db.MCPServer{"sv-team": &teamServer}}

	codeModeServer := mcp.NewServer("code-mode", "test")
	codeModeServer.SetToolsListChangedSource(true)
	codeModeServer.SetAccessChecker(codeModeAccessChecker(serverCache, (*proxy.MCPAccessCache)(nil)))

	fetcher := func(_ context.Context, serverID string) (*mcp.ToolListing, error) {
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "tool-" + serverID}}}, nil
	}
	toolCache := mcp.NewToolCache(fetcher, time.Hour)
	toolCache.SetOnChange(func(serverID string) {
		codeModeServer.NotifyToolsListChanged(mcp.NotifyScope{ServerID: serverID})
	})
	if _, err := toolCache.GetTools(context.Background(), "sv-team"); err != nil {
		t.Fatalf("warm sv-team: %v", err)
	}

	sameTeam := registerNotifyScopingListener(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-1", TeamID: "team-1", KeyID: "key-same-team"}, 1)
	otherTeam := registerNotifyScopingListener(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-1", TeamID: "team-2", KeyID: "key-other-team"}, 2)

	toolCache.Invalidate("sv-team")

	assertScopingEventPending(t, sameTeam)
	assertScopingNoEvent(t, otherTeam)
}

// TestMCPNotifyScoping_ToolCacheInvalidate_GlobalServer_RespectsMCPAccessCache
// verifies the fourth branch of codeModeAccessChecker: a global (non-builtin)
// server falls back to proxy.MCPAccessCache — a subscriber whose org is not
// in that cache's allowlist is denied even though nothing else restricts a
// global server.
func TestMCPNotifyScoping_ToolCacheInvalidate_GlobalServer_RespectsMCPAccessCache(t *testing.T) {
	t.Parallel()

	globalServer := db.MCPServer{ID: "sv-global"}
	serverCache := &staticServerCache{byID: map[string]*db.MCPServer{"sv-global": &globalServer}}

	accessCache := proxy.NewMCPAccessCache()
	accessCache.Load(map[string][]string{"org-allowed": {"sv-global"}}, nil, nil)

	codeModeServer := mcp.NewServer("code-mode", "test")
	codeModeServer.SetToolsListChangedSource(true)
	codeModeServer.SetAccessChecker(codeModeAccessChecker(serverCache, accessCache))

	fetcher := func(_ context.Context, serverID string) (*mcp.ToolListing, error) {
		return &mcp.ToolListing{Tools: []mcp.Tool{{Name: "tool-" + serverID}}}, nil
	}
	toolCache := mcp.NewToolCache(fetcher, time.Hour)
	toolCache.SetOnChange(func(serverID string) {
		codeModeServer.NotifyToolsListChanged(mcp.NotifyScope{ServerID: serverID})
	})
	if _, err := toolCache.GetTools(context.Background(), "sv-global"); err != nil {
		t.Fatalf("warm sv-global: %v", err)
	}

	allowed := registerNotifyScopingListener(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-allowed", KeyID: "key-allowed"}, 1)
	denied := registerNotifyScopingListener(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-denied", KeyID: "key-denied"}, 2)

	toolCache.Invalidate("sv-global")

	assertScopingEventPending(t, allowed)
	assertScopingNoEvent(t, denied)
}

// TestMCPNotifyScoping_SystemAdmin_SeesEveryOrgsEvent verifies
// codeModeAccessChecker's system-admin bypass holds inside the full
// ToolCache → NotifyToolsListChanged triangle too, not only in isolation
// (TestCodeModeAccessChecker): a system_admin subscriber receives a
// notification for an org-scoped server belonging to an org it does not
// itself belong to.
func TestMCPNotifyScoping_SystemAdmin_SeesEveryOrgsEvent(t *testing.T) {
	t.Parallel()

	codeModeServer, toolCache := newNotifyScopingFixture(t)

	admin := registerNotifyScopingListener(t, codeModeServer,
		mcp.KeyIdentity{OrgID: "org-unrelated", KeyID: "key-admin", Role: "system_admin"}, 1)

	toolCache.Invalidate("sv-a")

	assertScopingEventPending(t, admin)
}

// TestMCPNotifyScoping_OrgIDScope_DirectCall mirrors AfterMCPAccessRefresh's
// own wiring in app.go (Trigger 2: an org's MCP access allowlist changed) —
// NotifyToolsListChanged(NotifyScope{OrgID: ...}) called directly, not via
// ToolCache — and verifies it reaches only that org's own subscribers.
func TestMCPNotifyScoping_OrgIDScope_DirectCall(t *testing.T) {
	t.Parallel()

	codeModeServer, _ := newNotifyScopingFixture(t)

	orgA := registerNotifyScopingListener(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-a", KeyID: "key-a"}, 1)
	orgB := registerNotifyScopingListener(t, codeModeServer, mcp.KeyIdentity{OrgID: "org-b", KeyID: "key-b"}, 2)

	codeModeServer.NotifyToolsListChanged(mcp.NotifyScope{OrgID: "org-a"})

	assertScopingEventPending(t, orgA)
	assertScopingNoEvent(t, orgB)
}
