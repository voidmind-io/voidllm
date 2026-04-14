package app

// Black-box tests for codeModeService.
// The package is "app" (not "app_test") so we can construct the unexported
// codeModeService directly. All tests use a mock that satisfies codeModeDB and
// an in-process mcp.Executor / mcp.ToolCache — no real database, no real HTTP.

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/config"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/mcp"
)

// ---------------------------------------------------------------------------
// Mock DB
// ---------------------------------------------------------------------------

// mockCodeModeDB is a minimal codeModeDB implementation for unit tests.
type mockCodeModeDB struct {
	// servers is returned by ListMCPServers (global scope).
	servers []db.MCPServer
	// orgServers is returned by ListMCPServersByOrg.
	orgServers map[string][]db.MCPServer // orgID → servers
	// teamServers is returned by ListMCPServersByTeam.
	teamServers map[string][]db.MCPServer // teamID → servers
	// accessAllowed maps serverID → bool for CheckMCPAccess.
	accessAllowed map[string]bool
	// blockedTools maps serverID → tool names for ListBlockedToolNames.
	blockedTools map[string][]string
	// listErr is returned by all List* methods when non-nil.
	listErr error
	// outputSchemasByServerID maps serverID → (toolName → schema JSON) for
	// GetAllOutputSchemas. When nil the method returns nil, nil (no schemas).
	outputSchemasByServerID map[string]map[string]jsonx.RawMessage
}

func (m *mockCodeModeDB) ListMCPServers(_ context.Context) ([]db.MCPServer, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return append([]db.MCPServer(nil), m.servers...), nil
}

func (m *mockCodeModeDB) ListMCPServersByOrg(_ context.Context, orgID string) ([]db.MCPServer, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return append([]db.MCPServer(nil), m.orgServers[orgID]...), nil
}

func (m *mockCodeModeDB) ListMCPServersByTeam(_ context.Context, teamID, _ string) ([]db.MCPServer, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return append([]db.MCPServer(nil), m.teamServers[teamID]...), nil
}

func (m *mockCodeModeDB) CheckMCPAccess(_ context.Context, _, _, _, serverID string) (bool, error) {
	if m.accessAllowed == nil {
		return false, nil
	}
	return m.accessAllowed[serverID], nil
}

func (m *mockCodeModeDB) ListBlockedToolNames(_ context.Context, serverID string) ([]string, error) {
	if m.blockedTools == nil {
		return nil, nil
	}
	return append([]string(nil), m.blockedTools[serverID]...), nil
}

func (m *mockCodeModeDB) SaveOutputSchema(_ context.Context, _, _ string, _ jsonx.RawMessage) error {
	return nil
}

func (m *mockCodeModeDB) GetAllOutputSchemas(_ context.Context, serverID string, _ time.Duration) (map[string]jsonx.RawMessage, error) {
	if m.outputSchemasByServerID == nil {
		return nil, nil
	}
	return m.outputSchemasByServerID[serverID], nil
}

func (m *mockCodeModeDB) IsOutputSchemaStale(_ context.Context, _, _ string, _ time.Duration) (bool, error) {
	return true, nil
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// ptrStr returns a pointer to s, used to populate *string fields.
func ptrStr(s string) *string { return &s }

// newDiscardLogger returns a slog.Logger that silently discards all output.
func newDiscardLogger() *slog.Logger {
	return slog.New(slog.DiscardHandler)
}

// newTestPool creates a RuntimePool suitable for unit tests (small, short timeout).
func newTestPool(t *testing.T) *mcp.RuntimePool {
	t.Helper()
	pool, err := mcp.NewRuntimePool(2, 32, 5*time.Second)
	if err != nil {
		t.Fatalf("NewRuntimePool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTestExecutor creates an Executor backed by a small test pool.
func newTestExecutor(t *testing.T) *mcp.Executor {
	t.Helper()
	return mcp.NewExecutor(newTestPool(t))
}

// staticFetcher returns a ToolFetcher that serves pre-loaded tool lists keyed
// by server alias.
func staticFetcher(tools map[string][]mcp.Tool) mcp.ToolFetcher {
	return func(_ context.Context, alias string) ([]mcp.Tool, error) {
		if list, ok := tools[alias]; ok {
			return list, nil
		}
		return nil, errors.New("unknown server: " + alias)
	}
}

// newPreloadedCache creates a ToolCache and manually injects the given tools
// without triggering an upstream fetch. It does this by using a static fetcher
// and calling GetTools once per alias so the cache is warm before the test runs.
func newPreloadedCache(t *testing.T, tools map[string][]mcp.Tool) *mcp.ToolCache {
	t.Helper()
	tc := mcp.NewToolCache(staticFetcher(tools), 10*time.Minute)
	ctx := context.Background()
	for alias := range tools {
		if _, err := tc.GetTools(ctx, alias); err != nil {
			t.Fatalf("warm tool cache for %q: %v", alias, err)
		}
	}
	return tc
}

// ctxWithIdentity injects a KeyIdentity into a background context.
func ctxWithIdentity(ki mcp.KeyIdentity) context.Context {
	return mcp.WithKeyIdentity(context.Background(), ki)
}

// ---------------------------------------------------------------------------
// accessibleServers tests
// ---------------------------------------------------------------------------

func TestAccessibleServers_GlobalServers(t *testing.T) {
	t.Parallel()

	globalSv := db.MCPServer{
		ID:              "sv-global",
		Alias:           "global",
		Name:            "Global Server",
		CodeModeEnabled: true,
	}

	mock := &mockCodeModeDB{
		servers:       []db.MCPServer{globalSv},
		accessAllowed: map[string]bool{"sv-global": true},
	}
	svc := &codeModeService{db: mock, log: newDiscardLogger()}

	// A system_admin has no OrgID or TeamID — ListMCPServers is called.
	ctx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-1", Role: "system_admin"})
	got, err := svc.accessibleServers(ctx, false)
	if err != nil {
		t.Fatalf("accessibleServers() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d servers, want 1", len(got))
	}
	if got[0].ID != "sv-global" {
		t.Errorf("got server ID %q, want %q", got[0].ID, "sv-global")
	}
}

func TestAccessibleServers_OrgScope(t *testing.T) {
	t.Parallel()

	orgSv := db.MCPServer{
		ID:              "sv-org",
		Alias:           "org-server",
		Name:            "Org Server",
		OrgID:           ptrStr("org-1"),
		CodeModeEnabled: true,
	}
	globalSv := db.MCPServer{
		ID:              "sv-global",
		Alias:           "global-server",
		Name:            "Global Server",
		CodeModeEnabled: true,
	}

	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{
			"org-1": {orgSv, globalSv},
		},
		accessAllowed: map[string]bool{"sv-global": true},
	}
	svc := &codeModeService{db: mock, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-2", Role: "org_admin"})
	got, err := svc.accessibleServers(ctx, false)
	if err != nil {
		t.Fatalf("accessibleServers() error = %v", err)
	}
	// org-scoped server is returned directly; global server passes access check.
	if len(got) != 2 {
		t.Fatalf("got %d servers, want 2", len(got))
	}
}

func TestAccessibleServers_TeamScope(t *testing.T) {
	t.Parallel()

	teamSv := db.MCPServer{
		ID:              "sv-team",
		Alias:           "team-server",
		Name:            "Team Server",
		TeamID:          ptrStr("team-1"),
		OrgID:           ptrStr("org-1"),
		CodeModeEnabled: true,
	}
	orgSv := db.MCPServer{
		ID:              "sv-org",
		Alias:           "org-server",
		Name:            "Org Server",
		OrgID:           ptrStr("org-1"),
		CodeModeEnabled: true,
	}
	globalSv := db.MCPServer{
		ID:              "sv-global",
		Alias:           "global-server",
		Name:            "Global Server",
		CodeModeEnabled: true,
	}

	mock := &mockCodeModeDB{
		teamServers: map[string][]db.MCPServer{
			"team-1": {teamSv, orgSv, globalSv},
		},
		accessAllowed: map[string]bool{"sv-global": true},
	}
	svc := &codeModeService{db: mock, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-1", TeamID: "team-1", KeyID: "key-3", Role: "member"})
	got, err := svc.accessibleServers(ctx, false)
	if err != nil {
		t.Fatalf("accessibleServers() error = %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d servers, want 3", len(got))
	}
}

func TestAccessibleServers_MCPAccessFilter(t *testing.T) {
	t.Parallel()

	// A global server the org has NOT been granted access to.
	globalSv := db.MCPServer{
		ID:              "sv-no-access",
		Alias:           "blocked-global",
		Name:            "Blocked Global",
		CodeModeEnabled: true,
	}

	mock := &mockCodeModeDB{
		servers:       []db.MCPServer{globalSv},
		accessAllowed: map[string]bool{
			// "sv-no-access" is absent — defaults to false.
		},
	}
	svc := &codeModeService{db: mock, log: newDiscardLogger()}

	// Non-admin caller: access denied for global server without explicit grant.
	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-4", Role: "member"})
	got, err := svc.accessibleServers(ctx, false)
	if err != nil {
		t.Fatalf("accessibleServers() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d servers, want 0 (access denied)", len(got))
	}

	// System admin: bypasses access check, sees all global servers.
	adminCtx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-5", Role: "system_admin"})
	adminGot, adminErr := svc.accessibleServers(adminCtx, false)
	if adminErr != nil {
		t.Fatalf("accessibleServers(system_admin) error = %v", adminErr)
	}
	if len(adminGot) != 1 {
		t.Errorf("system_admin: got %d servers, want 1 (bypass)", len(adminGot))
	}
}

// TestAccessibleServers_BuiltinAlwaysIncluded verifies that a global server
// with source="builtin" is included without requiring a MCP access entry.
func TestAccessibleServers_BuiltinAlwaysIncluded(t *testing.T) {
	t.Parallel()

	builtinSv := db.MCPServer{
		ID:              "sv-builtin",
		Alias:           "voidllm",
		Name:            "VoidLLM",
		Source:          "builtin",
		CodeModeEnabled: true,
	}
	// A regular global server that requires explicit access — denied here.
	regularSv := db.MCPServer{
		ID:              "sv-regular",
		Alias:           "other",
		Name:            "Other Server",
		Source:          "api",
		CodeModeEnabled: true,
	}

	mock := &mockCodeModeDB{
		servers:       []db.MCPServer{builtinSv, regularSv},
		accessAllowed: map[string]bool{
			// Neither server has explicit access; builtin must bypass this.
		},
	}
	svc := &codeModeService{db: mock, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-builtin-test", Role: "member"})
	got, err := svc.accessibleServers(ctx, false)
	if err != nil {
		t.Fatalf("accessibleServers() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d servers, want 1 (only builtin)", len(got))
	}
	if got[0].Source != "builtin" {
		t.Errorf("got server Source = %q, want %q", got[0].Source, "builtin")
	}
	if got[0].Alias != "voidllm" {
		t.Errorf("got server Alias = %q, want %q", got[0].Alias, "voidllm")
	}
}

// TestAccessibleServers_BuiltinRespectsCodeModeFilter verifies that a builtin
// server with CodeModeEnabled=false is excluded when codeModeOnly=true.
func TestAccessibleServers_BuiltinRespectsCodeModeFilter(t *testing.T) {
	t.Parallel()

	builtinDisabled := db.MCPServer{
		ID:              "sv-builtin-disabled",
		Alias:           "voidllm",
		Name:            "VoidLLM",
		Source:          "builtin",
		CodeModeEnabled: false,
	}

	mock := &mockCodeModeDB{
		servers: []db.MCPServer{builtinDisabled},
	}
	svc := &codeModeService{db: mock, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-builtin-cm", Role: "member"})

	// codeModeOnly=true — the disabled builtin server must be excluded.
	got, err := svc.accessibleServers(ctx, true)
	if err != nil {
		t.Fatalf("accessibleServers(codeModeOnly=true) error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d servers with codeModeOnly=true, want 0 (builtin CodeModeEnabled=false)", len(got))
	}

	// codeModeOnly=false — the disabled builtin server should appear.
	got2, err := svc.accessibleServers(ctx, false)
	if err != nil {
		t.Fatalf("accessibleServers(codeModeOnly=false) error = %v", err)
	}
	if len(got2) != 1 {
		t.Errorf("got %d servers with codeModeOnly=false, want 1", len(got2))
	}
}

func TestAccessibleServers_CodeModeDisabled(t *testing.T) {
	t.Parallel()

	enabledSv := db.MCPServer{
		ID:              "sv-enabled",
		Alias:           "enabled",
		Name:            "Enabled Server",
		OrgID:           ptrStr("org-1"),
		CodeModeEnabled: true,
	}
	disabledSv := db.MCPServer{
		ID:              "sv-disabled",
		Alias:           "disabled",
		Name:            "Disabled Server",
		OrgID:           ptrStr("org-1"),
		CodeModeEnabled: false,
	}

	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{
			"org-1": {enabledSv, disabledSv},
		},
	}
	svc := &codeModeService{db: mock, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-1", KeyID: "key-5", Role: "org_admin"})

	t.Run("codeModeOnly=true filters disabled", func(t *testing.T) {
		t.Parallel()
		got, err := svc.accessibleServers(ctx, true)
		if err != nil {
			t.Fatalf("accessibleServers() error = %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("got %d servers, want 1", len(got))
		}
		if got[0].ID != "sv-enabled" {
			t.Errorf("got %q, want %q", got[0].ID, "sv-enabled")
		}
	})

	t.Run("codeModeOnly=false returns both", func(t *testing.T) {
		t.Parallel()
		got, err := svc.accessibleServers(ctx, false)
		if err != nil {
			t.Fatalf("accessibleServers() error = %v", err)
		}
		if len(got) != 2 {
			t.Errorf("got %d servers, want 2", len(got))
		}
	})
}

// ---------------------------------------------------------------------------
// ListAccessibleMCPServers tests
// ---------------------------------------------------------------------------

func TestListAccessibleMCPServers_ToolCount(t *testing.T) {
	t.Parallel()

	orgID := "org-list"
	sv := db.MCPServer{
		ID:              "sv-list-1",
		Alias:           "myserver",
		Name:            "My Server",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}

	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{
			orgID: {sv},
		},
		// Two tools blocked.
		blockedTools: map[string][]string{
			"sv-list-1": {"danger_tool", "secret_tool"},
		},
	}

	// Cache has 5 tools for sv (keyed by server ID).
	toolList := make([]mcp.Tool, 5)
	for i := range toolList {
		toolList[i] = mcp.Tool{Name: "tool_" + string(rune('a'+i))}
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{sv.ID: toolList})

	svc := &codeModeService{
		db:        mock,
		toolCache: tc,
		log:       newDiscardLogger(),
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-list", Role: "member"})
	result, err := svc.ListAccessibleMCPServers(ctx, false)
	if err != nil {
		t.Fatalf("ListAccessibleMCPServers() error = %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d entries, want 1", len(result))
	}

	entry := result[0]
	if entry["alias"] != "myserver" {
		t.Errorf("alias = %q, want %q", entry["alias"], "myserver")
	}
	// 5 tools cached - 2 blocked = 3.
	toolCount, _ := entry["tool_count"].(int)
	if toolCount != 3 {
		t.Errorf("tool_count = %d, want 3", toolCount)
	}
}

func TestListAccessibleMCPServers_Empty(t *testing.T) {
	t.Parallel()

	mock := &mockCodeModeDB{
		servers: nil,
		// No org or team entries.
	}
	tc := mcp.NewToolCache(staticFetcher(nil), 10*time.Minute)

	svc := &codeModeService{
		db:        mock,
		toolCache: tc,
		log:       newDiscardLogger(),
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-empty", Role: "member"})
	result, err := svc.ListAccessibleMCPServers(ctx, false)
	if err != nil {
		t.Fatalf("ListAccessibleMCPServers() error = %v", err)
	}
	if len(result) != 0 {
		t.Errorf("got %d entries, want 0", len(result))
	}
}

// ---------------------------------------------------------------------------
// accessibleServers: closed-by-default for global servers
// ---------------------------------------------------------------------------

// TestAccessibleServers_GlobalDeniedWithoutAccess verifies that a global server
// is excluded from the result when the org has no MCP access entries
// (closed-by-default policy). The mock returns false for CheckMCPAccess when
// accessAllowed is nil — matching the behaviour of DB.CheckMCPAccess for an org
// with an empty org_mcp_access table.
func TestAccessibleServers_GlobalDeniedWithoutAccess(t *testing.T) {
	t.Parallel()

	globalSv := db.MCPServer{
		ID:              "sv-global-denied",
		Alias:           "global-denied",
		Name:            "Global Denied",
		Source:          "api",
		CodeModeEnabled: true,
	}

	mock := &mockCodeModeDB{
		servers: []db.MCPServer{globalSv},
		// accessAllowed is nil — CheckMCPAccess returns false for all servers.
	}
	svc := &codeModeService{db: mock, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-gd", Role: "member"})
	got, err := svc.accessibleServers(ctx, false)
	if err != nil {
		t.Fatalf("accessibleServers() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d servers, want 0 (org has no MCP access entries)", len(got))
	}
}

// TestAccessibleServers_GlobalAllowedWithAccess verifies that a global server
// IS included when CheckMCPAccess returns true for the server ID — i.e., the
// org has an org_mcp_access entry granting access to it.
//
// A system_admin identity (no OrgID/TeamID) is used because it triggers
// ListMCPServers, which is the code path for global servers. The mock's
// accessAllowed map stands in for the DB's org_mcp_access check.
func TestAccessibleServers_GlobalAllowedWithAccess(t *testing.T) {
	t.Parallel()

	globalSv := db.MCPServer{
		ID:              "sv-global-allowed",
		Alias:           "global-allowed",
		Name:            "Global Allowed",
		Source:          "api",
		CodeModeEnabled: true,
	}

	mock := &mockCodeModeDB{
		servers: []db.MCPServer{globalSv},
		accessAllowed: map[string]bool{
			"sv-global-allowed": true,
		},
	}
	svc := &codeModeService{db: mock, log: newDiscardLogger()}

	// system_admin has no OrgID/TeamID — routes to ListMCPServers for global servers.
	ctx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-ga", Role: "system_admin"})
	got, err := svc.accessibleServers(ctx, false)
	if err != nil {
		t.Fatalf("accessibleServers() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d servers, want 1", len(got))
	}
	if got[0].ID != "sv-global-allowed" {
		t.Errorf("got server ID %q, want %q", got[0].ID, "sv-global-allowed")
	}
}

func TestListAccessibleMCPServers_NilCache(t *testing.T) {
	t.Parallel()

	svc := &codeModeService{
		db:        &mockCodeModeDB{},
		toolCache: nil, // disabled
		log:       newDiscardLogger(),
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-nil", Role: "member"})
	result, err := svc.ListAccessibleMCPServers(ctx, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result when cache is nil, got %v", result)
	}
}

// ---------------------------------------------------------------------------
// SearchMCPTools helpers
// ---------------------------------------------------------------------------

func assertContainsSearch(t *testing.T, got, substr string) {
	t.Helper()
	if !strings.Contains(got, substr) {
		t.Errorf("search result missing %q\ngot: %s", substr, got)
	}
}

func assertNotContainsSearch(t *testing.T, got, substr string) {
	t.Helper()
	if strings.Contains(got, substr) {
		t.Errorf("search result should not contain %q\ngot: %s", substr, got)
	}
}

// ---------------------------------------------------------------------------
// SearchMCPTools tests
// ---------------------------------------------------------------------------

func TestSearchMCPTools_KeywordMatch(t *testing.T) {
	t.Parallel()

	orgID := "org-search"
	sv := db.MCPServer{
		ID:              "sv-search",
		Alias:           "search-srv",
		Name:            "Search Server",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}
	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {sv}},
	}
	tools := []mcp.Tool{
		{Name: "find_documents", Description: "Search the document store"},
		{Name: "create_report", Description: "Generate a PDF report"},
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{sv.ID: tools})
	svc := &codeModeService{db: mock, toolCache: tc, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-s1", Role: "member"})
	result, err := svc.SearchMCPTools(ctx, "find", nil)
	if err != nil {
		t.Fatalf("SearchMCPTools() error = %v", err)
	}
	assertContainsSearch(t, result, `Found `)
	assertContainsSearch(t, result, `"find"`)
	assertContainsSearch(t, result, `function find_documents(`)
	assertContainsSearch(t, result, `declare namespace tools_search_srv`)
}

func TestSearchMCPTools_DescriptionMatch(t *testing.T) {
	t.Parallel()

	orgID := "org-desc"
	sv := db.MCPServer{
		ID:              "sv-desc",
		Alias:           "desc-srv",
		Name:            "Desc Server",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}
	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {sv}},
	}
	tools := []mcp.Tool{
		{Name: "alpha", Description: "Converts units of measurement"},
		{Name: "beta", Description: "Sends email notifications"},
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{sv.ID: tools})
	svc := &codeModeService{db: mock, toolCache: tc, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-d1", Role: "member"})
	result, err := svc.SearchMCPTools(ctx, "email", nil)
	if err != nil {
		t.Fatalf("SearchMCPTools() error = %v", err)
	}
	// "beta" matched via description; "alpha" must not appear.
	assertContainsSearch(t, result, `function beta(`)
	assertNotContainsSearch(t, result, `function alpha(`)
}

func TestSearchMCPTools_CaseInsensitive(t *testing.T) {
	t.Parallel()

	orgID := "org-ci"
	sv := db.MCPServer{
		ID:              "sv-ci",
		Alias:           "ci-srv",
		Name:            "CI Server",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}
	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {sv}},
	}
	tools := []mcp.Tool{
		{Name: "ReadFile", Description: "Reads a file from disk"},
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{sv.ID: tools})
	svc := &codeModeService{db: mock, toolCache: tc, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-ci", Role: "member"})

	tests := []struct {
		query string
	}{
		{"readfile"},
		{"READFILE"},
		{"ReadFile"},
		{"reads a file"},
		{"READS A FILE"},
	}

	for _, tc2 := range tests {
		t.Run(tc2.query, func(t *testing.T) {
			t.Parallel()
			result, err := svc.SearchMCPTools(ctx, tc2.query, nil)
			if err != nil {
				t.Fatalf("SearchMCPTools(%q) error = %v", tc2.query, err)
			}
			// Original casing of the tool name must be preserved in output.
			assertContainsSearch(t, result, `function ReadFile(`)
		})
	}
}

func TestSearchMCPTools_FiltersBlocked(t *testing.T) {
	t.Parallel()

	orgID := "org-blocked"
	sv := db.MCPServer{
		ID:              "sv-blocked",
		Alias:           "blocked-srv",
		Name:            "Blocked Server",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}
	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {sv}},
		blockedTools: map[string][]string{
			"sv-blocked": {"danger_tool"},
		},
	}
	tools := []mcp.Tool{
		{Name: "safe_tool", Description: "Completely safe"},
		{Name: "danger_tool", Description: "Dangerous operation"},
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{sv.ID: tools})
	svc := &codeModeService{db: mock, toolCache: tc, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-b1", Role: "member"})
	result, err := svc.SearchMCPTools(ctx, "tool", nil)
	if err != nil {
		t.Fatalf("SearchMCPTools() error = %v", err)
	}
	assertContainsSearch(t, result, `function safe_tool(`)
	assertNotContainsSearch(t, result, `function danger_tool(`)
}

func TestSearchMCPTools_ServerFilter(t *testing.T) {
	t.Parallel()

	orgID := "org-sf"
	sv1 := db.MCPServer{
		ID:              "sv-sf-1",
		Alias:           "first",
		Name:            "First Server",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}
	sv2 := db.MCPServer{
		ID:              "sv-sf-2",
		Alias:           "second",
		Name:            "Second Server",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}
	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {sv1, sv2}},
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{
		sv1.ID: {{Name: "lookup", Description: "Lookup records"}},
		sv2.ID: {{Name: "lookup", Description: "Lookup records"}},
	})
	svc := &codeModeService{db: mock, toolCache: tc, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-sf", Role: "member"})

	// Restrict to "first" only — should include server_a namespace and exclude server_b.
	result, err := svc.SearchMCPTools(ctx, "lookup", []string{"first"})
	if err != nil {
		t.Fatalf("SearchMCPTools() error = %v", err)
	}
	assertContainsSearch(t, result, `declare namespace tools.first`)
	assertNotContainsSearch(t, result, `declare namespace tools.second`)
}

func TestSearchMCPTools_SurfacesInferredTypes(t *testing.T) {
	t.Parallel()

	orgID := "org-infer-search"
	sv := db.MCPServer{
		ID:              "sv-infer-search",
		Alias:           "infer-srv",
		Name:            "Infer Server",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}

	// Output schema for "my_infer_tool" — simple object with id (number) and name (string).
	schemaJSON := jsonx.RawMessage(`{"type":"object","properties":{"id":{"type":"number"},"name":{"type":"string"}}}`)

	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {sv}},
		outputSchemasByServerID: map[string]map[string]jsonx.RawMessage{
			sv.ID: {"my_infer_tool": schemaJSON},
		},
	}
	tools := []mcp.Tool{
		{Name: "my_infer_tool", Description: "A tool with inferred output schema"},
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{sv.ID: tools})

	serverCache := &staticServerCache{
		byID: map[string]*db.MCPServer{sv.ID: &sv},
	}

	svc := &codeModeService{
		db:          mock,
		toolCache:   tc,
		serverCache: serverCache,
		log:         newDiscardLogger(),
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-infer-s", Role: "member"})
	result, err := svc.SearchMCPTools(ctx, "my_infer_tool", nil)
	if err != nil {
		t.Fatalf("SearchMCPTools() error = %v", err)
	}

	assertContainsSearch(t, result, `Promise<{ id: number; name: string }>`)
	assertContainsSearch(t, result, `// inferred - could depend on previous query`)
	// Ensure the inferred type replaced Promise<any> for this tool.
	assertNotContainsSearch(t, result, `Promise<any>`)
}

func TestSearchMCPTools_NoResults(t *testing.T) {
	t.Parallel()

	orgID := "org-noresults"
	sv := db.MCPServer{
		ID:              "sv-noresults",
		Alias:           "noresults-srv",
		Name:            "No Results Server",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}
	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {sv}},
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{
		sv.ID: {{Name: "some_tool", Description: "Does something"}},
	})
	svc := &codeModeService{db: mock, toolCache: tc, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-nr", Role: "member"})
	result, err := svc.SearchMCPTools(ctx, "xyzzyfooBarNonExistent", nil)
	if err != nil {
		t.Fatalf("SearchMCPTools() error = %v", err)
	}
	const want = `No tools found matching "xyzzyfooBarNonExistent".`
	if result != want {
		t.Errorf("SearchMCPTools() = %q, want %q", result, want)
	}
}

// ---------------------------------------------------------------------------
// ExecuteCode tests
// ---------------------------------------------------------------------------

func TestExecuteCode_SimpleScript(t *testing.T) {
	t.Parallel()

	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{
			"org-exec": {
				{
					ID:              "sv-exec",
					Alias:           "exec-srv",
					Name:            "Exec Server",
					OrgID:           ptrStr("org-exec"),
					CodeModeEnabled: true,
				},
			},
		},
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{
		"sv-exec": {{Name: "noop", Description: "No-op tool"}},
	})
	executor := newTestExecutor(t)

	noopCaller := func(_ context.Context, _ *auth.KeyInfo, _, _ string, _ json.RawMessage, _ bool, _ string) (json.RawMessage, error) {
		return json.RawMessage(`"noop-result"`), nil
	}

	svc := &codeModeService{
		executor:     executor,
		toolCache:    tc,
		callMCPTool:  noopCaller,
		db:           mock,
		log:          newDiscardLogger(),
		maxToolCalls: 10,
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: "org-exec", KeyID: "key-exec", Role: "member"})

	tests := []struct {
		name       string
		code       string
		wantResult string
	}{
		{
			name:       "return number",
			code:       `42`,
			wantResult: `42`,
		},
		{
			name:       "return string via JSON.stringify",
			code:       `JSON.stringify("hello")`,
			wantResult: `"hello"`,
		},
		{
			name:       "arithmetic",
			code:       `1 + 2 + 3`,
			wantResult: `6`,
		},
	}

	for _, tc2 := range tests {
		t.Run(tc2.name, func(t *testing.T) {
			t.Parallel()
			result, err := svc.ExecuteCode(ctx, tc2.code, nil)
			if err != nil {
				t.Fatalf("ExecuteCode() error = %v", err)
			}
			if result.Error != "" {
				t.Fatalf("ExecuteCode() result.Error = %q", result.Error)
			}
			if string(result.Result) != tc2.wantResult {
				t.Errorf("Result = %s, want %s", result.Result, tc2.wantResult)
			}
		})
	}
}

func TestExecuteCode_NilExecutor(t *testing.T) {
	t.Parallel()

	svc := &codeModeService{
		executor: nil, // Code Mode disabled
		db:       &mockCodeModeDB{},
		log:      newDiscardLogger(),
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-nil-exec", Role: "member"})
	result, err := svc.ExecuteCode(ctx, `42`, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result != nil {
		t.Errorf("expected nil result when executor is nil, got %+v", result)
	}
}

func TestExecuteCode_WithToolCall(t *testing.T) {
	t.Parallel()

	orgID := "org-tool-call"
	sv := db.MCPServer{
		ID:              "sv-tc",
		Alias:           "tc-srv",
		Name:            "TC Server",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}
	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {sv}},
	}
	tools := []mcp.Tool{
		{
			Name:        "get_value",
			Description: "Returns a value",
			InputSchema: mcp.InputSchema{Type: "object"},
		},
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{sv.ID: tools})
	executor := newTestExecutor(t)

	callCount := 0
	caller := func(_ context.Context, _ *auth.KeyInfo, _, toolName string, _ json.RawMessage, _ bool, _ string) (json.RawMessage, error) {
		callCount++
		if toolName == "get_value" {
			return json.RawMessage(`{"value":99}`), nil
		}
		return nil, errors.New("unexpected tool: " + toolName)
	}

	svc := &codeModeService{
		executor:     executor,
		toolCache:    tc,
		callMCPTool:  caller,
		db:           mock,
		log:          newDiscardLogger(),
		maxToolCalls: 10,
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-tc", Role: "member"})
	result, err := svc.ExecuteCode(ctx, `
		async function run() {
			const r = await tools["tc-srv"].get_value({});
			return JSON.stringify(r);
		}
		await run();
	`, nil)
	if err != nil {
		t.Fatalf("ExecuteCode() error = %v", err)
	}
	if result.Error != "" {
		t.Fatalf("ExecuteCode() result.Error = %q", result.Error)
	}
	if len(result.ToolCalls) != 1 {
		t.Errorf("ToolCalls = %d, want 1", len(result.ToolCalls))
	}
	if result.ToolCalls[0].Tool != "get_value" {
		t.Errorf("ToolCalls[0].Tool = %q, want %q", result.ToolCalls[0].Tool, "get_value")
	}
	if result.ToolCalls[0].Status != "success" {
		t.Errorf("ToolCalls[0].Status = %q, want %q", result.ToolCalls[0].Status, "success")
	}
}

func TestExecuteCode_BlockedToolRejected(t *testing.T) {
	t.Parallel()

	orgID := "org-block-tc"
	sv := db.MCPServer{
		ID:              "sv-btc",
		Alias:           "btc-srv",
		Name:            "BTC Server",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}
	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {sv}},
		// blocked_tool is listed as blocked on the server.
		blockedTools: map[string][]string{
			"sv-btc": {"blocked_tool"},
		},
	}
	tools := []mcp.Tool{
		{Name: "safe_tool", Description: "Safe"},
		{Name: "blocked_tool", Description: "Should be blocked"},
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{sv.ID: tools})
	executor := newTestExecutor(t)

	caller := func(_ context.Context, _ *auth.KeyInfo, _, toolName string, _ json.RawMessage, _ bool, _ string) (json.RawMessage, error) {
		return json.RawMessage(`"ok"`), nil
	}

	svc := &codeModeService{
		executor:     executor,
		toolCache:    tc,
		callMCPTool:  caller,
		db:           mock,
		log:          newDiscardLogger(),
		maxToolCalls: 10,
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-btc", Role: "member"})
	// blocked_tool is filtered out of serverTools at build time, so the JS
	// Proxy will reject the call as "unknown tool".
	result, err := svc.ExecuteCode(ctx, `
		async function run() {
			const r = await tools["btc-srv"].blocked_tool({});
			return JSON.stringify(r);
		}
		await run();
	`, nil)
	if err != nil {
		t.Fatalf("ExecuteCode() unexpected error = %v", err)
	}
	// The script should fail because blocked_tool was stripped from serverTools.
	if result.Error == "" {
		t.Error("expected result.Error to be non-empty (blocked tool call should fail), got empty")
	}
}

func TestExecuteCode_ServerAliasFilter(t *testing.T) {
	t.Parallel()

	orgID := "org-alias-filter"
	sv1 := db.MCPServer{
		ID:              "sv-af-1",
		Alias:           "server-a",
		Name:            "Server A",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}
	sv2 := db.MCPServer{
		ID:              "sv-af-2",
		Alias:           "server-b",
		Name:            "Server B",
		OrgID:           ptrStr(orgID),
		CodeModeEnabled: true,
	}
	mock := &mockCodeModeDB{
		orgServers: map[string][]db.MCPServer{orgID: {sv1, sv2}},
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{
		sv1.ID: {{Name: "tool_a"}},
		sv2.ID: {{Name: "tool_b"}},
	})
	executor := newTestExecutor(t)

	caller := func(_ context.Context, _ *auth.KeyInfo, serverAlias, _ string, _ json.RawMessage, _ bool, _ string) (json.RawMessage, error) {
		return json.RawMessage(`"ok"`), nil
	}

	svc := &codeModeService{
		executor:     executor,
		toolCache:    tc,
		callMCPTool:  caller,
		db:           mock,
		log:          newDiscardLogger(),
		maxToolCalls: 10,
	}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-af", Role: "member"})

	// Restrict to "server-a" — calling server-b's tool should fail as unknown.
	result, err := svc.ExecuteCode(ctx, `
		async function run() {
			const r = await tools["server-b"].tool_b({});
			return JSON.stringify(r);
		}
		await run();
	`, []string{"server-a"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Error == "" {
		t.Error("expected error when calling tool from excluded server")
	}
}

// ---------------------------------------------------------------------------
// toolsListHook tests
// ---------------------------------------------------------------------------

func TestToolsListHook_InjectsTypes(t *testing.T) {
	t.Parallel()

	// Pre-populate the cache so GetAllTools returns something.
	tc := newPreloadedCache(t, map[string][]mcp.Tool{
		"myserver": {
			{
				Name:        "my_tool",
				Description: "Does something useful",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]mcp.Property{
						"query": {Type: "string", Description: "search query"},
					},
					Required: []string{"query"},
				},
			},
		},
	})

	svc := &codeModeService{
		db:        &mockCodeModeDB{},
		toolCache: tc,
		log:       newDiscardLogger(),
	}

	hook := svc.toolsListHook()

	// Build a minimal list of tools that includes execute_code.
	inputTools := []mcp.Tool{
		{
			Name:        "execute_code",
			Description: "original description",
		},
		{
			Name:        "other_tool",
			Description: "untouched",
		},
	}

	got := hook(inputTools)

	if len(got) != len(inputTools) {
		t.Fatalf("hook changed tool count: got %d, want %d", len(got), len(inputTools))
	}

	// Find the execute_code tool in the result.
	var execCodeDesc string
	for _, tool := range got {
		if tool.Name == "execute_code" {
			execCodeDesc = tool.Description
			break
		}
	}

	if execCodeDesc == "original description" {
		t.Error("execute_code description was not updated by the hook")
	}
	if !strings.Contains(execCodeDesc, "Available Tools") {
		t.Errorf("execute_code description missing 'Available Tools' section; got: %s", execCodeDesc)
	}
	if !strings.Contains(execCodeDesc, "my_tool") {
		t.Errorf("execute_code description missing type def for my_tool; got: %s", execCodeDesc)
	}

	// Ensure the unrelated tool was not modified.
	for _, tool := range got {
		if tool.Name == "other_tool" && tool.Description != "untouched" {
			t.Errorf("other_tool description was unexpectedly changed to %q", tool.Description)
		}
	}
}

func TestToolsListHook_EmptyCache(t *testing.T) {
	t.Parallel()

	// Cache with no entries — GetAllTools returns empty map.
	tc := mcp.NewToolCache(staticFetcher(nil), 10*time.Minute)

	svc := &codeModeService{
		db:        &mockCodeModeDB{},
		toolCache: tc,
		log:       newDiscardLogger(),
	}

	hook := svc.toolsListHook()

	inputTools := []mcp.Tool{
		{Name: "execute_code", Description: "original"},
	}

	got := hook(inputTools)

	if len(got) != 1 {
		t.Fatalf("got %d tools, want 1", len(got))
	}
	// With empty cache the description must remain unchanged.
	if got[0].Description != "original" {
		t.Errorf("description changed to %q, want %q", got[0].Description, "original")
	}
}

func TestToolsListHook_NoExecuteCodeTool(t *testing.T) {
	t.Parallel()

	tc := newPreloadedCache(t, map[string][]mcp.Tool{
		"srv": {{Name: "some_tool", Description: "desc"}},
	})

	svc := &codeModeService{
		db:        &mockCodeModeDB{},
		toolCache: tc,
		log:       newDiscardLogger(),
	}

	hook := svc.toolsListHook()

	inputTools := []mcp.Tool{
		{Name: "list_models", Description: "lists models"},
	}

	got := hook(inputTools)

	if len(got) != 1 {
		t.Fatalf("got %d tools, want 1", len(got))
	}
	// execute_code is not in the list — other tools must be unchanged.
	if got[0].Description != "lists models" {
		t.Errorf("description changed to %q", got[0].Description)
	}
}

// ---------------------------------------------------------------------------
// Schema inference integration tests — real SQLite DB, real Executor
// ---------------------------------------------------------------------------

// staticServerCache is a minimal mcpServerByIDer that serves a fixed map of
// server ID → MCPServer for use in schema-inference tests.
type staticServerCache struct {
	byID map[string]*db.MCPServer
}

func (s *staticServerCache) GetByID(serverID string) (*db.MCPServer, bool) {
	sv, ok := s.byID[serverID]
	return sv, ok
}

// openTestDBForSchemaTests opens an isolated in-memory SQLite DB, runs all
// migrations, and registers cleanup. The dsn suffix must be unique per test.
func openTestDBForSchemaTests(t *testing.T) *db.DB {
	t.Helper()
	// Use test name sanitised to a safe URI filename component.
	safeName := strings.NewReplacer("/", "_", " ", "_", "#", "_", "=", "_").Replace(t.Name())
	cfg := config.DatabaseConfig{
		Driver:          "sqlite",
		DSN:             "file:" + safeName + "?mode=memory&cache=private",
		MaxOpenConns:    1,
		MaxIdleConns:    1,
		ConnMaxLifetime: time.Minute,
	}
	ctx := context.Background()
	d, err := db.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if err := db.RunMigrations(ctx, d.SQL(), d.Dialect(), slog.New(slog.DiscardHandler)); err != nil {
		t.Fatalf("RunMigrations: %v", err)
	}
	return d
}

// createTestMCPServer inserts a global builtin MCP server and returns it.
// Using source="builtin" avoids the need for an org row (no FK) and bypasses
// the MCP access check so any identity can access the server.
func createTestMCPServer(t *testing.T, database *db.DB, alias string) *db.MCPServer {
	t.Helper()
	enabled := true
	sv, err := database.CreateMCPServer(context.Background(), db.CreateMCPServerParams{
		Name:            "Test " + alias,
		Alias:           alias,
		URL:             "https://mcp.test/" + alias,
		AuthType:        "none",
		Source:          "builtin",
		CodeModeEnabled: &enabled,
	})
	if err != nil {
		t.Fatalf("CreateMCPServer(%q): %v", alias, err)
	}
	return sv
}

// pollUntilSchemaInferred polls the DB until a schema row exists for
// (serverID, toolName) or the deadline passes. It fails the test if no row
// appears within the timeout.
func pollUntilSchemaInferred(t *testing.T, database *db.DB, serverID, toolName string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		stale, err := database.IsOutputSchemaStale(context.Background(), serverID, toolName, time.Hour)
		if err != nil {
			t.Fatalf("IsOutputSchemaStale: %v", err)
		}
		if !stale {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("schema for (%s, %s) was not inferred within %s", serverID, toolName, timeout)
}

// readInferredAt returns the raw inferred_at string for a (serverID, toolName)
// row directly from the underlying sql.DB. It fails the test if the row is missing.
func readInferredAt(t *testing.T, database *db.DB, serverID, toolName string) string {
	t.Helper()
	var inferredAt string
	err := database.SQL().QueryRowContext(
		context.Background(),
		"SELECT inferred_at FROM output_schemas WHERE server_id = ? AND tool_name = ?",
		serverID, toolName,
	).Scan(&inferredAt)
	if err != nil {
		t.Fatalf("readInferredAt(%s, %s): %v", serverID, toolName, err)
	}
	return inferredAt
}

// setupSchemaInferenceTest builds a codeModeService wired to a real SQLite DB
// and a mock callMCPTool that returns the given JSON payload.
// The MCP server is created as a global builtin so no org FK is required.
// Returns the service, the database, and the MCPServer that was created.
func setupSchemaInferenceTest(t *testing.T, alias string, schemaTTL time.Duration, toolPayload json.RawMessage) (*codeModeService, *db.DB, *db.MCPServer) {
	t.Helper()

	database := openTestDBForSchemaTests(t)
	sv := createTestMCPServer(t, database, alias)

	// Tool cache keyed by server ID — must match what ExecuteCode uses.
	tc := newPreloadedCache(t, map[string][]mcp.Tool{
		sv.ID: {{Name: "my_tool", Description: "A tool"}},
	})

	executor := newTestExecutor(t)

	caller := func(_ context.Context, _ *auth.KeyInfo, _, _ string, _ json.RawMessage, _ bool, _ string) (json.RawMessage, error) {
		return toolPayload, nil
	}

	serverCache := &staticServerCache{
		byID: map[string]*db.MCPServer{sv.ID: sv},
	}

	svc := &codeModeService{
		executor:     executor,
		toolCache:    tc,
		callMCPTool:  caller,
		db:           database,
		log:          newDiscardLogger(),
		maxToolCalls: 10,
		schemaTTL:    schemaTTL,
		serverCache:  serverCache,
	}
	return svc, database, sv
}

// TestCodeMode_InfersSchemaOnFirstCall verifies that calling a tool whose
// response is {"users":[{"id":1,"name":"a"}]} causes the schema to be
// persisted and then surfaced as a typed Promise in toolsListHook output.
func TestCodeMode_InfersSchemaOnFirstCall(t *testing.T) {
	t.Parallel()

	const alias = "testsrv"

	toolPayload := json.RawMessage(`{"users":[{"id":1,"name":"a"}]}`)

	svc, database, sv := setupSchemaInferenceTest(t, alias, time.Hour, toolPayload)

	// system_admin with no OrgID/TeamID triggers ListMCPServers; builtin servers
	// bypass the MCP access check so no access record is needed.
	ctx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-infer", Role: "system_admin"})

	// Execute code that calls the tool.
	result, err := svc.ExecuteCode(ctx, `
		async function run() {
			const r = await tools["`+alias+`"].my_tool({});
			return JSON.stringify(r);
		}
		await run();
	`, nil)
	if err != nil {
		t.Fatalf("ExecuteCode: %v", err)
	}
	if result.Error != "" {
		t.Fatalf("ExecuteCode result.Error = %q", result.Error)
	}

	// Wait for the async OnToolResult goroutine to write the schema.
	pollUntilSchemaInferred(t, database, sv.ID, "my_tool", 500*time.Millisecond)

	// Now call toolsListHook and verify the execute_code description contains
	// the inferred TypeScript return type.
	hook := svc.toolsListHook()
	inputTools := []mcp.Tool{
		{Name: "execute_code", Description: "original"},
	}
	got := hook(inputTools)

	if len(got) != 1 {
		t.Fatalf("hook returned %d tools, want 1", len(got))
	}
	desc := got[0].Description

	// {"users":[{"id":1,"name":"a"}]} infers:
	//   object with property "users" of type Array<{ id: number; name: string }>
	// schemaToTypeScript produces: { users: Array<{ id: number; name: string }> }
	const wantType = "Promise<{ users: Array<{ id: number; name: string }> }>"
	if !strings.Contains(desc, wantType) {
		t.Errorf("description does not contain %q\ngot: %s", wantType, desc)
	}

	const wantComment = "// inferred - could depend on previous query"
	if !strings.Contains(desc, wantComment) {
		t.Errorf("description does not contain %q\ngot: %s", wantComment, desc)
	}
}

// TestCodeMode_TTLPreventsReinference verifies that a second tool call within
// the schema TTL does not update the inferred_at timestamp.
func TestCodeMode_TTLPreventsReinference(t *testing.T) {
	t.Parallel()

	const alias = "ttlsrv"

	toolPayload := json.RawMessage(`{"value":1}`)

	svc, database, sv := setupSchemaInferenceTest(t, alias, time.Hour, toolPayload)

	ctx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-ttl", Role: "system_admin"})

	callScript := `
		async function run() {
			const r = await tools["` + alias + `"].my_tool({});
			return JSON.stringify(r);
		}
		await run();
	`

	// First call — schema should be inferred and persisted.
	if _, err := svc.ExecuteCode(ctx, callScript, nil); err != nil {
		t.Fatalf("first ExecuteCode: %v", err)
	}
	pollUntilSchemaInferred(t, database, sv.ID, "my_tool", 500*time.Millisecond)
	firstInferredAt := readInferredAt(t, database, sv.ID, "my_tool")

	// Second call — within the 1h TTL so schema is not stale; inferred_at must be unchanged.
	if _, err := svc.ExecuteCode(ctx, callScript, nil); err != nil {
		t.Fatalf("second ExecuteCode: %v", err)
	}
	// Brief pause to let the goroutine run if it was (incorrectly) triggered.
	time.Sleep(100 * time.Millisecond)

	secondInferredAt := readInferredAt(t, database, sv.ID, "my_tool")
	if firstInferredAt != secondInferredAt {
		t.Errorf("inferred_at changed on second call within TTL:\n  first  = %s\n  second = %s", firstInferredAt, secondInferredAt)
	}
}

// TestCodeMode_ZeroTTLAllowsIndefiniteCache verifies that schemaTTL=0 means
// "never stale" — a row that already exists is never overwritten on re-calls.
func TestCodeMode_ZeroTTLAllowsIndefiniteCache(t *testing.T) {
	t.Parallel()

	const alias = "zerosrv"

	toolPayload := json.RawMessage(`{"ok":true}`)

	svc, database, sv := setupSchemaInferenceTest(t, alias, 0, toolPayload)

	ctx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-zero", Role: "system_admin"})

	callScript := `
		async function run() {
			const r = await tools["` + alias + `"].my_tool({});
			return JSON.stringify(r);
		}
		await run();
	`

	// First call — schema is written because no row exists yet (missing = stale).
	if _, err := svc.ExecuteCode(ctx, callScript, nil); err != nil {
		t.Fatalf("first ExecuteCode: %v", err)
	}
	pollUntilSchemaInferred(t, database, sv.ID, "my_tool", 500*time.Millisecond)
	firstInferredAt := readInferredAt(t, database, sv.ID, "my_tool")

	// Second call — schemaTTL=0 means the existing row is never stale; no update.
	if _, err := svc.ExecuteCode(ctx, callScript, nil); err != nil {
		t.Fatalf("second ExecuteCode: %v", err)
	}
	time.Sleep(100 * time.Millisecond)

	secondInferredAt := readInferredAt(t, database, sv.ID, "my_tool")
	if firstInferredAt != secondInferredAt {
		t.Errorf("inferred_at changed with schemaTTL=0 (should never re-infer):\n  first  = %s\n  second = %s", firstInferredAt, secondInferredAt)
	}
}

// ---------------------------------------------------------------------------
// toolsListHook description-content regression test (L-013)
// ---------------------------------------------------------------------------

// TestToolsListHook_InjectsCodeModePreference verifies that the dynamic hook
// appends the full "## Available Tools" section to the execute_code description
// and that the result retains all STRONG PREFERENCE / PATTERNS / NOTE guidance
// from the static CodeModeDescription(). It also asserts that the old trailing
// "Call tools via `await tools.serverAlias.toolName(args)`" sentence, which
// was removed during L-013, is no longer present.
func TestToolsListHook_InjectsCodeModePreference(t *testing.T) {
	t.Parallel()

	// Seed the tool cache with one server that has one tool with a required
	// string parameter — GenerateToolTypeDefs will emit a Promise<any> signature.
	tc := newPreloadedCache(t, map[string][]mcp.Tool{
		"myserver": {
			{
				Name:        "fetch_data",
				Description: "Fetches data by ID",
				InputSchema: mcp.InputSchema{
					Type: "object",
					Properties: map[string]mcp.Property{
						"id": {Type: "string", Description: "Record ID"},
					},
					Required: []string{"id"},
				},
			},
		},
	})

	svc := &codeModeService{
		db:        &mockCodeModeDB{},
		toolCache: tc,
		log:       newDiscardLogger(),
	}

	hook := svc.toolsListHook()

	// The hook receives the static description that RegisterCodeModeTools wires.
	inputTools := []mcp.Tool{
		{
			Name:        "execute_code",
			Description: mcp.CodeModeDescription(),
		},
	}

	got := hook(inputTools)
	if len(got) != 1 {
		t.Fatalf("hook changed tool count: got %d, want 1", len(got))
	}

	desc := got[0].Description

	// Static guidance must survive the hook — if STRONG PREFERENCE disappears the
	// LLM loses critical workflow instructions.
	if !strings.Contains(desc, "STRONG PREFERENCE") {
		t.Errorf("execute_code description after hook missing STRONG PREFERENCE;\ngot: %s", desc)
	}

	// Dynamic section must be appended with blank-line padding so the markdown
	// heading renders correctly for the LLM.
	if !strings.Contains(desc, "\n\n## Available Tools\n\n") {
		t.Errorf("execute_code description after hook missing '\\n\\n## Available Tools\\n\\n' (with surrounding blank lines);\ngot: %s", desc)
	}

	// TypeScript type definitions must appear (Promise<any> is the default for a
	// tool whose output schema has not yet been inferred).
	if !strings.Contains(desc, "Promise<") {
		t.Errorf("execute_code description after hook missing TypeScript Promise type;\ngot: %s", desc)
	}

	// The specific tool name injected by the cache must appear.
	if !strings.Contains(desc, "fetch_data") {
		t.Errorf("execute_code description after hook missing tool name 'fetch_data';\ngot: %s", desc)
	}

	// The old trailing call-syntax sentence was dropped in L-013.  If it comes
	// back a refactor has reintroduced it — that would be a regression.
	const droppedSentence = "Call tools via `await tools.serverAlias.toolName(args)`"
	if strings.Contains(desc, droppedSentence) {
		t.Errorf("execute_code description contains dropped sentence %q — was it re-added by mistake?", droppedSentence)
	}
}
