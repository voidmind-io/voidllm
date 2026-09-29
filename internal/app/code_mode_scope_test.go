package app

// Tests covering the caller-scoped metadata exposed by toolsListHook (and, by
// extension, the mcp.Server tools/list JSON-RPC handler it is wired into).
// The fixture models a small multi-tenant topology — two orgs, two teams
// within one of them, a global server whose access grant differs per org, a
// builtin server, a code-mode-disabled server, and a blocked tool — so a
// single table can assert that each caller sees exactly the servers/tools it
// is entitled to and nothing more.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// toolsListScopeFixture holds the servers, tools, and mock DB used by the
// caller-scoping tests below. All server IDs and tool names are unique and
// descriptive so failures are easy to attribute to a specific server.
type toolsListScopeFixture struct {
	svc *codeModeService

	builtinSv  db.MCPServer // global, source=builtin — always visible
	globalSv   db.MCPServer // global, source=api — access varies per org
	orgASv     db.MCPServer // org-scoped to org-a
	orgBSv     db.MCPServer // org-scoped to org-b
	teamA1Sv   db.MCPServer // team-scoped to org-a/team-a1
	teamA2Sv   db.MCPServer // team-scoped to org-a/team-a2
	disabledSv db.MCPServer // org-scoped to org-a, CodeModeEnabled=false
}

// newToolsListScopeFixture builds the fixture. The mock DB's List* methods
// mirror the real SQL semantics documented on db.DB.ListMCPServers /
// ListMCPServersByOrg / ListMCPServersByTeam (see internal/db/mcp_servers.go):
// the "org" query includes org-scoped-for-that-org servers plus every truly
// global server (team_id IS NULL); the "team" query additionally includes
// servers scoped to that specific team.
func newToolsListScopeFixture(t *testing.T) *toolsListScopeFixture {
	t.Helper()

	f := &toolsListScopeFixture{
		builtinSv: db.MCPServer{
			ID: "sv-builtin", Alias: "builtin-srv", Name: "Builtin",
			Source: "builtin", CodeModeEnabled: true,
		},
		globalSv: db.MCPServer{
			ID: "sv-global", Alias: "global-srv", Name: "Global",
			Source: "api", CodeModeEnabled: true,
		},
		orgASv: db.MCPServer{
			ID: "sv-org-a", Alias: "org-a-srv", Name: "Org A",
			OrgID: ptrStr("org-a"), CodeModeEnabled: true,
		},
		orgBSv: db.MCPServer{
			ID: "sv-org-b", Alias: "org-b-srv", Name: "Org B",
			OrgID: ptrStr("org-b"), CodeModeEnabled: true,
		},
		teamA1Sv: db.MCPServer{
			ID: "sv-team-a1", Alias: "team-a1-srv", Name: "Team A1",
			OrgID: ptrStr("org-a"), TeamID: ptrStr("team-a1"), CodeModeEnabled: true,
		},
		teamA2Sv: db.MCPServer{
			ID: "sv-team-a2", Alias: "team-a2-srv", Name: "Team A2",
			OrgID: ptrStr("org-a"), TeamID: ptrStr("team-a2"), CodeModeEnabled: true,
		},
		disabledSv: db.MCPServer{
			ID: "sv-disabled", Alias: "disabled-srv", Name: "Disabled",
			OrgID: ptrStr("org-a"), CodeModeEnabled: false,
		},
	}

	mock := &mockCodeModeDB{
		// ListMCPServers(): org_id IS NULL AND team_id IS NULL.
		servers: []db.MCPServer{f.builtinSv, f.globalSv},
		// ListMCPServersByOrg(orgID): (org_id = orgID OR org_id IS NULL) AND team_id IS NULL.
		orgServers: map[string][]db.MCPServer{
			"org-a": {f.builtinSv, f.globalSv, f.orgASv, f.disabledSv},
			"org-b": {f.builtinSv, f.globalSv, f.orgBSv},
		},
		// ListMCPServersByTeam(teamID, orgID): team match, OR org-scoped-for-org, OR global.
		teamServers: map[string][]db.MCPServer{
			"team-a1": {f.builtinSv, f.globalSv, f.orgASv, f.disabledSv, f.teamA1Sv},
			"team-a2": {f.builtinSv, f.globalSv, f.orgASv, f.disabledSv, f.teamA2Sv},
		},
		// globalSv is the only non-builtin global server, so it is the only
		// one gated by CheckMCPAccess. Open for org-a, closed for org-b (and
		// for the empty-org system_admin identity, though that caller bypasses
		// the check entirely).
		accessAllowedByOrg: map[string]map[string]bool{
			"org-a": {"sv-global": true},
			"org-b": {"sv-global": false},
		},
		blockedTools: map[string][]string{
			"sv-team-a1": {"team_a1_secret_tool"},
		},
		outputSchemasByServerID: map[string]map[string]jsonx.RawMessage{
			"sv-org-a": {"org_a_tool": jsonx.RawMessage(`{"type":"object","properties":{"id":{"type":"number"}}}`)},
		},
	}

	tools := map[string][]mcp.Tool{
		f.builtinSv.ID:  {{Name: "builtin_tool", Description: "builtin"}},
		f.globalSv.ID:   {{Name: "global_tool", Description: "global"}},
		f.orgASv.ID:     {{Name: "org_a_tool", Description: "org a"}},
		f.orgBSv.ID:     {{Name: "org_b_tool", Description: "org b"}},
		f.teamA1Sv.ID:   {{Name: "team_a1_tool", Description: "team a1"}, {Name: "team_a1_secret_tool", Description: "blocked"}},
		f.teamA2Sv.ID:   {{Name: "team_a2_tool", Description: "team a2"}},
		f.disabledSv.ID: {{Name: "disabled_tool", Description: "disabled"}},
	}
	// newPreloadedCache keys the ToolCache by whatever string is passed in its
	// map keys; toolsListHook and ExecuteCode both read the cache by server
	// ID (tc.GetTools(ctx, sv.ID)), so tools is already keyed correctly.
	tc := newPreloadedCache(t, tools)

	f.svc = &codeModeService{
		db:        mock,
		toolCache: tc,
		log:       newDiscardLogger(),
		schemaTTL: time.Hour,
	}
	return f
}

// allToolNames is every tool name present anywhere in the fixture, used to
// compute the "must NOT appear" set for each caller by subtracting the
// caller's expected visible set.
func (f *toolsListScopeFixture) allToolNames() []string {
	return []string{
		"builtin_tool", "global_tool", "org_a_tool", "org_b_tool",
		"team_a1_tool", "team_a1_secret_tool", "team_a2_tool", "disabled_tool",
	}
}

// runHook invokes the toolsListHook with a static execute_code tool and
// returns the resulting description.
func (f *toolsListScopeFixture) runHook(ctx context.Context) string {
	hook := f.svc.toolsListHook()
	inputTools := []mcp.Tool{
		{Name: "execute_code", Description: "original description"},
	}
	got := hook(ctx, inputTools)
	for _, tool := range got {
		if tool.Name == "execute_code" {
			return tool.Description
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Per-caller visibility matrix
// ---------------------------------------------------------------------------

func TestToolsListHook_CallerScoping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		ctx     func(f *toolsListScopeFixture) context.Context
		want    []string // tool names that must appear
		wantNot []string // tool names that must NOT appear; nil = complement of want
	}{
		{
			name: "member org A no team",
			ctx: func(f *toolsListScopeFixture) context.Context {
				return ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-a", KeyID: "key-a-member", Role: "member"})
			},
			want: []string{"builtin_tool", "global_tool", "org_a_tool"},
		},
		{
			name: "member team A1",
			ctx: func(f *toolsListScopeFixture) context.Context {
				return ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-a", TeamID: "team-a1", KeyID: "key-a1-member", Role: "member"})
			},
			want: []string{"builtin_tool", "global_tool", "org_a_tool", "team_a1_tool"},
		},
		{
			name: "member team A2",
			ctx: func(f *toolsListScopeFixture) context.Context {
				return ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-a", TeamID: "team-a2", KeyID: "key-a2-member", Role: "member"})
			},
			want: []string{"builtin_tool", "global_tool", "org_a_tool", "team_a2_tool"},
		},
		{
			name: "member org B",
			ctx: func(f *toolsListScopeFixture) context.Context {
				return ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-b", KeyID: "key-b-member", Role: "member"})
			},
			// org-b has no grant for the global server — it must not appear.
			want: []string{"builtin_tool", "org_b_tool"},
		},
		{
			name: "system admin (no org/team identity)",
			ctx: func(f *toolsListScopeFixture) context.Context {
				return ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-sysadmin", Role: "system_admin"})
			},
			// No OrgID/TeamID on the identity routes to ListMCPServers, i.e.
			// only the truly-global servers — org/team-scoped servers require
			// an org/team context even for a system_admin caller.
			want: []string{"builtin_tool", "global_tool"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newToolsListScopeFixture(t)
			ctx := tc.ctx(f)
			desc := f.runHook(ctx)

			if desc == "original description" {
				t.Fatalf("hook did not modify description at all; want dynamic tool list injected")
			}
			if !strings.Contains(desc, "## Available Tools") {
				t.Fatalf("description missing '## Available Tools' section;\ngot: %s", desc)
			}

			for _, want := range tc.want {
				if !strings.Contains(desc, want) {
					t.Errorf("description missing expected tool %q\ngot: %s", want, desc)
				}
			}

			wantNot := tc.wantNot
			if wantNot == nil {
				wantSet := make(map[string]bool, len(tc.want))
				for _, w := range tc.want {
					wantSet[w] = true
				}
				for _, name := range f.allToolNames() {
					if !wantSet[name] {
						wantNot = append(wantNot, name)
					}
				}
			}
			for _, notWant := range wantNot {
				if strings.Contains(desc, notWant) {
					t.Errorf("description contains tool %q which this caller must not see\ngot: %s", notWant, desc)
				}
			}
		})
	}
}

// TestToolsListHook_OutputSchemaOnlyForAccessibleServer verifies that the
// inferred output-schema type for org_a_tool (registered only on sv-org-a) is
// rendered for callers who can see that server and is entirely absent — not
// even the raw schema shape — for callers who cannot.
func TestToolsListHook_OutputSchemaOnlyForAccessibleServer(t *testing.T) {
	t.Parallel()

	f := newToolsListScopeFixture(t)

	orgACtx := ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-a", KeyID: "key-a-schema", Role: "member"})
	orgADesc := f.runHook(orgACtx)
	const wantType = "Promise<{ id: number }>"
	if !strings.Contains(orgADesc, wantType) {
		t.Errorf("org A caller: description missing inferred type %q\ngot: %s", wantType, orgADesc)
	}

	f2 := newToolsListScopeFixture(t)
	orgBCtx := ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-b", KeyID: "key-b-schema", Role: "member"})
	orgBDesc := f2.runHook(orgBCtx)
	if strings.Contains(orgBDesc, "org_a_tool") {
		t.Errorf("org B caller must not see org_a_tool at all\ngot: %s", orgBDesc)
	}
	if strings.Contains(orgBDesc, wantType) {
		t.Errorf("org B caller must not see org A's inferred output type\ngot: %s", orgBDesc)
	}
}

// TestToolsListHook_NoIdentity_StaticDescriptionOnly verifies the fail-closed
// path: a context that never went through mcp.WithKeyIdentity (e.g. a
// transport bug that dispatches to Server.Handle without authenticating the
// caller first) must not leak any tool metadata — the hook must return the
// static description unchanged.
func TestToolsListHook_NoIdentity_StaticDescriptionOnly(t *testing.T) {
	t.Parallel()

	f := newToolsListScopeFixture(t)

	desc := f.runHook(context.Background())
	if desc != "original description" {
		t.Errorf("description changed with no caller identity in ctx, want unchanged;\ngot: %s", desc)
	}
	for _, name := range f.allToolNames() {
		if strings.Contains(desc, name) {
			t.Errorf("description leaked tool %q with no caller identity in ctx", name)
		}
	}
}

// TestToolsListHook_AccessibleServersError_StaticDescriptionOnly verifies that
// when accessibleServers fails (e.g. a DB error listing servers) the hook
// fails closed rather than surfacing a stale or partial tool list.
func TestToolsListHook_AccessibleServersError_StaticDescriptionOnly(t *testing.T) {
	t.Parallel()

	f := newToolsListScopeFixture(t)
	f.svc.db.(*mockCodeModeDB).listErr = errTestListFailed

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-a", KeyID: "key-a-err", Role: "member"})
	desc := f.runHook(ctx)
	if desc != "original description" {
		t.Errorf("description changed despite accessibleServers error, want unchanged;\ngot: %s", desc)
	}
}

// ---------------------------------------------------------------------------
// Context cancellation propagation
// ---------------------------------------------------------------------------

// ctxAwareSchemaDB wraps mockCodeModeDB and records whether GetAllOutputSchemas
// observed a context whose Err() was already non-nil, so the test can assert
// that the 2-second bound in toolsListHook is derived from the request ctx
// (context.WithTimeout(ctx, ...)) rather than context.Background().
type ctxAwareSchemaDB struct {
	*mockCodeModeDB
	sawCancelledCtx bool
}

func (d *ctxAwareSchemaDB) GetAllOutputSchemas(ctx context.Context, serverID string, maxAge time.Duration) (map[string]jsonx.RawMessage, error) {
	if ctx.Err() != nil {
		d.sawCancelledCtx = true
		return nil, ctx.Err()
	}
	return d.mockCodeModeDB.GetAllOutputSchemas(ctx, serverID, maxAge)
}

// TestToolsListHook_RequestCancellation_StaticDescriptionOnly verifies that
// an already-cancelled request context makes the hook fail closed entirely.
// The overall deadline (toolsListHookDeadline) is derived from ctx via
// context.WithTimeout, so a caller-cancelled ctx cascades into hctx
// immediately — the "no partial list" rule then applies exactly as it would
// for a deadline that elapsed mid-render: the hook must discard whatever it
// had already built (including tool names collected before the read that
// observed the cancellation) and return the static description unchanged.
func TestToolsListHook_RequestCancellation_StaticDescriptionOnly(t *testing.T) {
	t.Parallel()

	f := newToolsListScopeFixture(t)
	wrapped := &ctxAwareSchemaDB{mockCodeModeDB: f.svc.db.(*mockCodeModeDB)}
	f.svc.db = wrapped

	base := ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-a", KeyID: "key-a-cancel", Role: "member"})
	cancelCtx, cancel := context.WithCancel(base)
	cancel() // already cancelled before the hook ever runs

	desc := f.runHook(cancelCtx)

	if !wrapped.sawCancelledCtx {
		t.Error("GetAllOutputSchemas was not invoked with a context derived from the (already-cancelled) request ctx")
	}
	if desc != "original description" {
		t.Errorf("description changed despite a cancelled request ctx, want unchanged (fail closed, no partial list);\ngot: %s", desc)
	}
	for _, name := range f.allToolNames() {
		if strings.Contains(desc, name) {
			t.Errorf("description leaked tool %q despite a cancelled request ctx", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Blocklist read failures fail closed (exclude the whole server)
// ---------------------------------------------------------------------------

// TestBlocklistReadFailure_ExcludesServerEverywhere verifies that when
// ListBlockedToolNames errors for one server, that server is excluded
// entirely from every Code Mode surface — tools/list, search,
// ListAccessibleMCPServers, and execute_code's tool dispatch — rather than
// treated as having an empty blocklist. An unrelated, healthy server remains
// fully visible and callable throughout.
func TestBlocklistReadFailure_ExcludesServerEverywhere(t *testing.T) {
	t.Parallel()

	const orgID = "org-blocklist-failure"

	brokenSv := db.MCPServer{
		ID: "sv-broken", Alias: "broken-srv", Name: "Broken Server",
		OrgID: ptrStr(orgID), CodeModeEnabled: true,
	}
	healthySv := db.MCPServer{
		ID: "sv-healthy", Alias: "healthy-srv", Name: "Healthy Server",
		OrgID: ptrStr(orgID), CodeModeEnabled: true,
	}

	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {brokenSv, healthySv}},
		blockedToolsErr: map[string]error{
			brokenSv.ID: errors.New("simulated blocklist read failure"),
		},
	}
	tools := map[string][]mcp.Tool{
		brokenSv.ID:  {{Name: "broken_tool", Description: "should never be reachable"}},
		healthySv.ID: {{Name: "healthy_tool", Description: "always reachable"}},
	}
	tc := newPreloadedCache(t, tools)

	caller := func(_ context.Context, _ *auth.KeyInfo, _, _ string, _ jsonx.RawMessage, _ bool, _ string) (jsonx.RawMessage, error) {
		return jsonx.RawMessage(`"ok"`), nil
	}

	svc := &codeModeService{
		executor:     newTestExecutor(t),
		toolCache:    tc,
		callMCPTool:  caller,
		db:           mock,
		log:          newDiscardLogger(),
		maxToolCalls: 10,
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-blocklist-failure", Role: "member"})

	t.Run("toolsListHook", func(t *testing.T) {
		hook := svc.toolsListHook()
		got := hook(ctx, []mcp.Tool{{Name: "execute_code", Description: "original description"}})
		var desc string
		for _, tool := range got {
			if tool.Name == "execute_code" {
				desc = tool.Description
			}
		}
		if strings.Contains(desc, "broken_tool") {
			t.Errorf("description must not contain broken_tool (blocklist read failed);\ngot: %s", desc)
		}
		if !strings.Contains(desc, "healthy_tool") {
			t.Errorf("description missing healthy_tool (unrelated server must be unaffected);\ngot: %s", desc)
		}
	})

	t.Run("SearchMCPTools", func(t *testing.T) {
		result, err := svc.SearchMCPTools(ctx, "tool", nil)
		if err != nil {
			t.Fatalf("SearchMCPTools() error = %v", err)
		}
		if strings.Contains(result, "broken_tool") {
			t.Errorf("search result must not contain broken_tool;\ngot: %s", result)
		}
		if !strings.Contains(result, "healthy_tool") {
			t.Errorf("search result missing healthy_tool;\ngot: %s", result)
		}
	})

	t.Run("ListAccessibleMCPServers", func(t *testing.T) {
		result, err := svc.ListAccessibleMCPServers(ctx, true)
		if err != nil {
			t.Fatalf("ListAccessibleMCPServers() error = %v", err)
		}
		var sawBroken, sawHealthy bool
		for _, entry := range result {
			switch entry["alias"] {
			case "broken-srv":
				sawBroken = true
			case "healthy-srv":
				sawHealthy = true
			}
		}
		if sawBroken {
			t.Error("ListAccessibleMCPServers must not include the server whose blocklist read failed")
		}
		if !sawHealthy {
			t.Error("ListAccessibleMCPServers must still include the unaffected healthy server")
		}
	})

	t.Run("ExecuteCode", func(t *testing.T) {
		result, err := svc.ExecuteCode(ctx, `
			async function run() {
				const r = await tools["broken-srv"].broken_tool({});
				return JSON.stringify(r);
			}
			await run();
		`, nil)
		if err != nil {
			t.Fatalf("ExecuteCode() error = %v", err)
		}
		if result.Error == "" {
			t.Error("expected an error calling a tool on the server whose blocklist read failed, got none")
		}

		result2, err2 := svc.ExecuteCode(ctx, `
			async function run() {
				const r = await tools["healthy-srv"].healthy_tool({});
				return JSON.stringify(r);
			}
			await run();
		`, nil)
		if err2 != nil {
			t.Fatalf("ExecuteCode() error = %v", err2)
		}
		if result2.Error != "" {
			t.Errorf("ExecuteCode() result.Error = %q, want empty (healthy server must remain callable)", result2.Error)
		}
	})
}

// ---------------------------------------------------------------------------
// Overall deadline (toolsListHookDeadline) covers every phase of the hook
// ---------------------------------------------------------------------------

// slowToolsListDB wraps mockCodeModeDB to observe and, optionally, stall past
// the deadline set on the context passed to accessibleServers' underlying
// List* call, ListBlockedToolNames, and GetAllOutputSchemas. It records the
// Deadline() seen on every call so a test can assert all three phases share
// exactly one bounded context, and — when sleepOnBlockedTools is set —
// sleeps just past that deadline before returning a normal (non-error)
// result, simulating a DB call that completes successfully but only after
// the overall budget for the whole hook is spent.
type slowToolsListDB struct {
	*mockCodeModeDB
	sleepOnBlockedTools bool

	mu               sync.Mutex
	orgDeadlines     []time.Time
	blockedDeadlines []time.Time
	schemaDeadlines  []time.Time
}

func (d *slowToolsListDB) recordDeadline(ctx context.Context, dst *[]time.Time) {
	dl, ok := ctx.Deadline()
	if !ok {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	*dst = append(*dst, dl)
}

func (d *slowToolsListDB) ListMCPServersByOrg(ctx context.Context, orgID string) ([]db.MCPServer, error) {
	d.recordDeadline(ctx, &d.orgDeadlines)
	return d.mockCodeModeDB.ListMCPServersByOrg(ctx, orgID)
}

func (d *slowToolsListDB) ListBlockedToolNames(ctx context.Context, serverID string) ([]string, error) {
	d.recordDeadline(ctx, &d.blockedDeadlines)
	if d.sleepOnBlockedTools {
		if dl, ok := ctx.Deadline(); ok {
			if wait := time.Until(dl) + 100*time.Millisecond; wait > 0 {
				time.Sleep(wait)
			}
		}
	}
	return d.mockCodeModeDB.ListBlockedToolNames(ctx, serverID)
}

func (d *slowToolsListDB) GetAllOutputSchemas(ctx context.Context, serverID string, maxAge time.Duration) (map[string]jsonx.RawMessage, error) {
	d.recordDeadline(ctx, &d.schemaDeadlines)
	return d.mockCodeModeDB.GetAllOutputSchemas(ctx, serverID, maxAge)
}

// TestToolsListHook_SingleDeadlineCoversAllPhases verifies that
// accessibleServers, every ListBlockedToolNames call, and every
// GetAllOutputSchemas call all observe the exact same deadline — proving
// they share one context.WithTimeout(ctx, toolsListHookDeadline) rather than
// each phase (or none) getting its own, independent bound.
func TestToolsListHook_SingleDeadlineCoversAllPhases(t *testing.T) {
	t.Parallel()

	f := newToolsListScopeFixture(t)
	wrapped := &slowToolsListDB{mockCodeModeDB: f.svc.db.(*mockCodeModeDB)}
	f.svc.db = wrapped

	before := time.Now()
	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-a", KeyID: "key-a-deadline", Role: "member"})
	desc := f.runHook(ctx)
	after := time.Now()

	if desc == "original description" {
		t.Fatal("hook did not render a dynamic description; cannot verify deadline propagation")
	}
	if len(wrapped.orgDeadlines) == 0 {
		t.Fatal("accessibleServers did not observe a bounded context")
	}
	if len(wrapped.blockedDeadlines) == 0 {
		t.Fatal("ListBlockedToolNames did not observe a bounded context")
	}
	if len(wrapped.schemaDeadlines) == 0 {
		t.Fatal("GetAllOutputSchemas did not observe a bounded context")
	}

	want := wrapped.orgDeadlines[0]
	for _, dl := range wrapped.blockedDeadlines {
		if !dl.Equal(want) {
			t.Errorf("ListBlockedToolNames deadline %v != accessibleServers deadline %v; hook is not sharing one context", dl, want)
		}
	}
	for _, dl := range wrapped.schemaDeadlines {
		if !dl.Equal(want) {
			t.Errorf("GetAllOutputSchemas deadline %v != accessibleServers deadline %v; hook is not sharing one context", dl, want)
		}
	}

	// The shared deadline must be toolsListHookDeadline out from roughly when
	// the hook ran — not unbounded, and not some unrelated duration.
	if want.Before(before.Add(toolsListHookDeadline-500*time.Millisecond)) ||
		want.After(after.Add(toolsListHookDeadline+500*time.Millisecond)) {
		t.Errorf("shared deadline %v is not within tolerance of the hook's call window + %s (before=%v after=%v)",
			want, toolsListHookDeadline, before, after)
	}
}

// TestToolsListHook_DeadlineExceededDuringRendering_StaticDescriptionOnly
// verifies the "no partial list" rule for a genuine timeout: every
// individual DB call the double makes succeeds (no error is ever returned),
// but the first ListBlockedToolNames call stalls until just past the shared
// deadline. Even though every server the fixture would show this caller was
// processed without error, the hook must detect that its overall budget was
// exceeded and discard everything, returning the static description.
func TestToolsListHook_DeadlineExceededDuringRendering_StaticDescriptionOnly(t *testing.T) {
	t.Parallel()

	f := newToolsListScopeFixture(t)
	wrapped := &slowToolsListDB{mockCodeModeDB: f.svc.db.(*mockCodeModeDB), sleepOnBlockedTools: true}
	f.svc.db = wrapped

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-a", KeyID: "key-a-slow", Role: "member"})
	desc := f.runHook(ctx)

	if desc != "original description" {
		t.Errorf("hook returned a rendered description despite the overall deadline elapsing mid-render, want static description unchanged;\ngot: %s", desc)
	}
	for _, name := range f.allToolNames() {
		if strings.Contains(desc, name) {
			t.Errorf("description leaked tool %q despite the overall deadline elapsing mid-render", name)
		}
	}
}

// ---------------------------------------------------------------------------
// Alias resolution priority: team > org > global
// ---------------------------------------------------------------------------

// TestAliasResolution_OrgWinsOverGlobal verifies that when an org-scoped and
// a global server share the same alias, every Code Mode surface —
// accessibleServers, toolsListHook, SearchMCPTools, and ExecuteCode's actual
// tool dispatch — resolves to the org-scoped server (the higher-priority
// scope), never the global one, and the alias appears exactly once rather
// than twice. This mirrors the team > org > global priority applied at
// execution time by proxy.MCPServerCache.Get and db.GetMCPServerByAliasScoped.
func TestAliasResolution_OrgWinsOverGlobal(t *testing.T) {
	t.Parallel()

	const orgID = "org-alias-collision"
	const sharedAlias = "shared"

	orgSv := db.MCPServer{
		ID: "sv-alias-org", Alias: sharedAlias, Name: "Org Winner",
		OrgID: ptrStr(orgID), CodeModeEnabled: true,
	}
	globalSv := db.MCPServer{
		ID: "sv-alias-global", Alias: sharedAlias, Name: "Global Loser",
		Source: "api", CodeModeEnabled: true,
	}

	mock := &mockCodeModeDB{
		// Order deliberately puts the lower-priority (global) server first —
		// resolution must not depend on list order.
		orgServers: map[string][]db.MCPServer{orgID: {globalSv, orgSv}},
		accessAllowedByOrg: map[string]map[string]bool{
			orgID: {"sv-alias-global": true},
		},
	}
	tools := map[string][]mcp.Tool{
		orgSv.ID:    {{Name: "org_tool", Description: "the org tool"}},
		globalSv.ID: {{Name: "global_tool", Description: "the global tool"}},
	}
	tc := newPreloadedCache(t, tools)

	var calledAlias, calledTool string
	caller := func(_ context.Context, _ *auth.KeyInfo, alias, toolName string, _ jsonx.RawMessage, _ bool, _ string) (jsonx.RawMessage, error) {
		calledAlias, calledTool = alias, toolName
		return jsonx.RawMessage(`"ok"`), nil
	}

	svc := &codeModeService{
		executor:     newTestExecutor(t),
		toolCache:    tc,
		callMCPTool:  caller,
		db:           mock,
		log:          newDiscardLogger(),
		maxToolCalls: 10,
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-alias-collision", Role: "member"})

	t.Run("accessibleServers resolves a single winner", func(t *testing.T) {
		got, err := svc.accessibleServers(ctx, true)
		if err != nil {
			t.Fatalf("accessibleServers() error = %v", err)
		}
		var seen int
		for _, sv := range got {
			if sv.Alias != sharedAlias {
				continue
			}
			seen++
			if sv.ID != orgSv.ID {
				t.Errorf("resolved server for alias %q = %q, want org server %q", sharedAlias, sv.ID, orgSv.ID)
			}
		}
		if seen != 1 {
			t.Errorf("alias %q appeared %d times, want exactly 1", sharedAlias, seen)
		}
	})

	t.Run("toolsListHook renders only the org tool", func(t *testing.T) {
		hook := svc.toolsListHook()
		got := hook(ctx, []mcp.Tool{{Name: "execute_code", Description: "original description"}})
		var desc string
		for _, tool := range got {
			if tool.Name == "execute_code" {
				desc = tool.Description
			}
		}
		if !strings.Contains(desc, "org_tool") {
			t.Errorf("description missing org_tool;\ngot: %s", desc)
		}
		if strings.Contains(desc, "global_tool") {
			t.Errorf("description must not contain global_tool (lower-priority server for the same alias);\ngot: %s", desc)
		}
	})

	t.Run("SearchMCPTools returns only the org tool", func(t *testing.T) {
		result, err := svc.SearchMCPTools(ctx, "tool", nil)
		if err != nil {
			t.Fatalf("SearchMCPTools() error = %v", err)
		}
		if !strings.Contains(result, "org_tool") {
			t.Errorf("search result missing org_tool;\ngot: %s", result)
		}
		if strings.Contains(result, "global_tool") {
			t.Errorf("search result must not contain global_tool;\ngot: %s", result)
		}
	})

	t.Run("execute_code dispatches to the org tool and rejects the global one", func(t *testing.T) {
		result, err := svc.ExecuteCode(ctx, `
			async function run() {
				const r = await tools["`+sharedAlias+`"].org_tool({});
				return JSON.stringify(r);
			}
			await run();
		`, nil)
		if err != nil {
			t.Fatalf("ExecuteCode() error = %v", err)
		}
		if result.Error != "" {
			t.Fatalf("ExecuteCode() result.Error = %q, want empty (org_tool must be reachable)", result.Error)
		}
		if calledAlias != sharedAlias || calledTool != "org_tool" {
			t.Errorf("callMCPTool invoked with (%q, %q), want (%q, %q)", calledAlias, calledTool, sharedAlias, "org_tool")
		}

		result2, err2 := svc.ExecuteCode(ctx, `
			async function run() {
				const r = await tools["`+sharedAlias+`"].global_tool({});
				return JSON.stringify(r);
			}
			await run();
		`, nil)
		if err2 != nil {
			t.Fatalf("ExecuteCode() error = %v", err2)
		}
		if result2.Error == "" {
			t.Error("expected an error calling global_tool (excluded — lower-priority server for this alias), got none")
		}
	})
}

// TestAliasResolution_UnusableWinnerDropsAliasEntirely verifies the core
// regression this fix addresses: when the higher-priority server for an
// alias is excluded because it is not code-mode-enabled, the alias must be
// dropped entirely from every Code Mode surface for a codeModeOnly caller —
// never falling back to a lower-priority server sharing the alias. Before
// the fix, resolveServersByAlias ran only on the already-filtered
// "accessible" slice, so the org server's exclusion (CodeModeEnabled=false)
// let the global server "win" the alias here even though the real execution
// path (proxy.MCPServerCache.Get / db.GetMCPServerByAliasScoped) resolves
// purely on scope and always dispatches "foo" to the org server regardless
// of CodeModeEnabled — a mismatch between what Code Mode advertised and what
// it could actually execute.
func TestAliasResolution_UnusableWinnerDropsAliasEntirely(t *testing.T) {
	t.Parallel()

	const orgID = "org-cme-collision"
	const sharedAlias = "foo"

	orgFoo := db.MCPServer{
		ID: "sv-foo-org", Alias: sharedAlias, Name: "Org Foo",
		OrgID: ptrStr(orgID), CodeModeEnabled: false,
	}
	globalFoo := db.MCPServer{
		ID: "sv-foo-global", Alias: sharedAlias, Name: "Global Foo",
		Source: "api", CodeModeEnabled: true,
	}

	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {orgFoo, globalFoo}},
		accessAllowedByOrg: map[string]map[string]bool{
			orgID: {"sv-foo-global": true},
		},
	}
	tools := map[string][]mcp.Tool{
		orgFoo.ID:    {{Name: "org_foo_tool", Description: "the org foo tool"}},
		globalFoo.ID: {{Name: "global_foo_tool", Description: "the global foo tool"}},
	}
	tc := newPreloadedCache(t, tools)

	caller := func(_ context.Context, _ *auth.KeyInfo, alias, toolName string, _ jsonx.RawMessage, _ bool, _ string) (jsonx.RawMessage, error) {
		return jsonx.RawMessage(`"ok"`), nil
	}

	svc := &codeModeService{
		executor:     newTestExecutor(t),
		toolCache:    tc,
		callMCPTool:  caller,
		db:           mock,
		log:          newDiscardLogger(),
		maxToolCalls: 10,
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-cme-collision", Role: "member"})

	t.Run("accessibleServers drops the alias entirely (codeModeOnly)", func(t *testing.T) {
		got, err := svc.accessibleServers(ctx, true)
		if err != nil {
			t.Fatalf("accessibleServers() error = %v", err)
		}
		for _, sv := range got {
			if sv.Alias == sharedAlias {
				t.Errorf("alias %q present in accessibleServers(codeModeOnly=true) = %+v, want dropped entirely (winner org server is CodeModeEnabled=false)", sharedAlias, sv)
			}
		}
	})

	t.Run("accessibleServers keeps the org winner (codeModeOnly=false)", func(t *testing.T) {
		got, err := svc.accessibleServers(ctx, false)
		if err != nil {
			t.Fatalf("accessibleServers() error = %v", err)
		}
		var seen int
		for _, sv := range got {
			if sv.Alias != sharedAlias {
				continue
			}
			seen++
			if sv.ID != orgFoo.ID {
				t.Errorf("resolved server for alias %q = %q, want org server %q even with codeModeOnly=false", sharedAlias, sv.ID, orgFoo.ID)
			}
		}
		if seen != 1 {
			t.Errorf("alias %q appeared %d times with codeModeOnly=false, want exactly 1", sharedAlias, seen)
		}
	})

	t.Run("toolsListHook omits the alias", func(t *testing.T) {
		hook := svc.toolsListHook()
		got := hook(ctx, []mcp.Tool{{Name: "execute_code", Description: "original description"}})
		var desc string
		for _, tool := range got {
			if tool.Name == "execute_code" {
				desc = tool.Description
			}
		}
		if strings.Contains(desc, "org_foo_tool") || strings.Contains(desc, "global_foo_tool") {
			t.Errorf("description must not mention either server for alias %q;\ngot: %s", sharedAlias, desc)
		}
	})

	t.Run("SearchMCPTools omits the alias", func(t *testing.T) {
		result, err := svc.SearchMCPTools(ctx, "foo", nil)
		if err != nil {
			t.Fatalf("SearchMCPTools() error = %v", err)
		}
		if strings.Contains(result, "org_foo_tool") || strings.Contains(result, "global_foo_tool") {
			t.Errorf("search result must not mention either server for alias %q;\ngot: %s", sharedAlias, result)
		}
	})

	t.Run("ListAccessibleMCPServers omits the alias", func(t *testing.T) {
		result, err := svc.ListAccessibleMCPServers(ctx, true)
		if err != nil {
			t.Fatalf("ListAccessibleMCPServers() error = %v", err)
		}
		for _, entry := range result {
			if entry["alias"] == sharedAlias {
				t.Errorf("ListAccessibleMCPServers(codeModeOnly=true) included alias %q, want dropped entirely", sharedAlias)
			}
		}
	})

	t.Run("execute_code rejects both servers for the alias", func(t *testing.T) {
		result, err := svc.ExecuteCode(ctx, `
			async function run() {
				const r = await tools["`+sharedAlias+`"].org_foo_tool({});
				return JSON.stringify(r);
			}
			await run();
		`, nil)
		if err != nil {
			t.Fatalf("ExecuteCode() error = %v", err)
		}
		if result.Error == "" {
			t.Error("expected an error calling org_foo_tool (alias dropped entirely), got none")
		}

		result2, err2 := svc.ExecuteCode(ctx, `
			async function run() {
				const r = await tools["`+sharedAlias+`"].global_foo_tool({});
				return JSON.stringify(r);
			}
			await run();
		`, nil)
		if err2 != nil {
			t.Fatalf("ExecuteCode() error = %v", err2)
		}
		if result2.Error == "" {
			t.Error("expected an error calling global_foo_tool (alias dropped entirely, no fallback to the global server), got none")
		}
	})
}

// TestAliasResolution_OrgWinnerUsedDespiteGlobalAccessDenied verifies that
// when an org-scoped and a global server share an alias, the org-scoped
// server (the higher-priority scope) is used as the winner even when the
// global server's access grant for this org is explicitly denied — access
// control for the global server is irrelevant once it has already lost the
// alias to a higher-priority scope. A team-scoped server sharing the same
// alias could exist elsewhere in the system (rounding out the full
// "team/org/global" topology), but it would never be visible to this
// org-level caller in the first place — db.ListMCPServersByOrg excludes
// team-scoped rows entirely — so it plays no part in resolution here.
func TestAliasResolution_OrgWinnerUsedDespiteGlobalAccessDenied(t *testing.T) {
	t.Parallel()

	const orgID = "org-denied-collision"
	const sharedAlias = "shared-denied"

	orgSv := db.MCPServer{
		ID: "sv-denied-org", Alias: sharedAlias, Name: "Org Winner",
		OrgID: ptrStr(orgID), CodeModeEnabled: true,
	}
	globalSv := db.MCPServer{
		ID: "sv-denied-global", Alias: sharedAlias, Name: "Global Loser",
		Source: "api", CodeModeEnabled: true,
	}

	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {globalSv, orgSv}},
		// The global server is explicitly denied for this org — it must not
		// matter, since the org server outranks it for this alias regardless.
		accessAllowedByOrg: map[string]map[string]bool{
			orgID: {"sv-denied-global": false},
		},
	}
	tools := map[string][]mcp.Tool{
		orgSv.ID:    {{Name: "org_denied_tool", Description: "the org tool"}},
		globalSv.ID: {{Name: "global_denied_tool", Description: "the global tool"}},
	}
	tc := newPreloadedCache(t, tools)

	var calledAlias, calledTool string
	caller := func(_ context.Context, _ *auth.KeyInfo, alias, toolName string, _ jsonx.RawMessage, _ bool, _ string) (jsonx.RawMessage, error) {
		calledAlias, calledTool = alias, toolName
		return jsonx.RawMessage(`"ok"`), nil
	}

	svc := &codeModeService{
		executor:     newTestExecutor(t),
		toolCache:    tc,
		callMCPTool:  caller,
		db:           mock,
		log:          newDiscardLogger(),
		maxToolCalls: 10,
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-denied-collision", Role: "member"})

	got, err := svc.accessibleServers(ctx, true)
	if err != nil {
		t.Fatalf("accessibleServers() error = %v", err)
	}
	var seen int
	for _, sv := range got {
		if sv.Alias != sharedAlias {
			continue
		}
		seen++
		if sv.ID != orgSv.ID {
			t.Errorf("resolved server for alias %q = %q, want org server %q (global access denial is irrelevant once outranked)", sharedAlias, sv.ID, orgSv.ID)
		}
	}
	if seen != 1 {
		t.Errorf("alias %q appeared %d times, want exactly 1", sharedAlias, seen)
	}

	result, err := svc.ExecuteCode(ctx, `
		async function run() {
			const r = await tools["`+sharedAlias+`"].org_denied_tool({});
			return JSON.stringify(r);
		}
		await run();
	`, nil)
	if err != nil {
		t.Fatalf("ExecuteCode() error = %v", err)
	}
	if result.Error != "" {
		t.Fatalf("ExecuteCode() result.Error = %q, want empty (org tool must be reachable)", result.Error)
	}
	if calledAlias != sharedAlias || calledTool != "org_denied_tool" {
		t.Errorf("callMCPTool invoked with (%q, %q), want (%q, %q)", calledAlias, calledTool, sharedAlias, "org_denied_tool")
	}
}

// errTestListFailed is a sentinel error used to force accessibleServers'
// List* calls to fail in tests.
var errTestListFailed = errors.New("simulated list servers failure")

// ---------------------------------------------------------------------------
// Through the full JSON-RPC surface (mcp.Server.Handle)
// ---------------------------------------------------------------------------

// buildToolsListServer wires the fixture's toolsListHook into a real mcp.Server
// alongside a static execute_code tool, exactly as RegisterCodeModeTools +
// app.go wiring do in production.
func buildToolsListServer(f *toolsListScopeFixture) *mcp.Server {
	s := mcp.NewServer("test", "0.0.0")
	s.RegisterTool(mcp.Tool{Name: "execute_code", Description: "original description"}, func(_ context.Context, _ jsonx.RawMessage) (*mcp.ToolResult, error) {
		return mcp.TextResult("unused"), nil
	})
	s.SetOnToolsList(f.svc.toolsListHook())
	return s
}

// toolsListRPCRequest is a minimal tools/list JSON-RPC 2.0 request body.
const toolsListRPCRequest = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`

// decodeExecuteCodeDescription parses a tools/list JSON-RPC response and
// returns the execute_code tool's description field.
func decodeExecuteCodeDescription(t *testing.T, raw []byte) string {
	t.Helper()
	var resp struct {
		Result struct {
			Tools []mcp.Tool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode tools/list response: %v\nraw: %s", err, raw)
	}
	for _, tool := range resp.Result.Tools {
		if tool.Name == "execute_code" {
			return tool.Description
		}
	}
	t.Fatalf("execute_code tool missing from tools/list response\nraw: %s", raw)
	return ""
}

// TestServerHandle_ToolsList_CallerScoping exercises the same scoping rules
// as TestToolsListHook_CallerScoping but through the full JSON-RPC surface
// (mcp.Server.Handle), confirming the hook is correctly wired end to end.
func TestServerHandle_ToolsList_CallerScoping(t *testing.T) {
	t.Parallel()

	t.Run("member org A no team sees only its own scope", func(t *testing.T) {
		t.Parallel()
		f := newToolsListScopeFixture(t)
		s := buildToolsListServer(f)
		ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-a", KeyID: "key-a-rpc", Role: "member"})

		raw := s.Handle(ctx, []byte(toolsListRPCRequest))
		desc := decodeExecuteCodeDescription(t, raw)

		for _, want := range []string{"builtin_tool", "global_tool", "org_a_tool"} {
			if !strings.Contains(desc, want) {
				t.Errorf("missing %q in Handle()-derived description\ngot: %s", want, desc)
			}
		}
		for _, notWant := range []string{"org_b_tool", "team_a1_tool", "team_a1_secret_tool", "team_a2_tool", "disabled_tool"} {
			if strings.Contains(desc, notWant) {
				t.Errorf("unexpected %q in Handle()-derived description\ngot: %s", notWant, desc)
			}
		}
	})

	t.Run("no identity in ctx yields static description via Handle", func(t *testing.T) {
		t.Parallel()
		f := newToolsListScopeFixture(t)
		s := buildToolsListServer(f)

		raw := s.Handle(context.Background(), []byte(toolsListRPCRequest))
		desc := decodeExecuteCodeDescription(t, raw)

		if desc != "original description" {
			t.Errorf("Handle() with no identity in ctx changed description, want unchanged;\ngot: %s", desc)
		}
	})
}
