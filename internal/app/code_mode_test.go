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
	"github.com/voidmind-io/voidllm/internal/db"
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

	ctx := ctxWithIdentity(mcp.KeyIdentity{KeyID: "key-4", Role: "system_admin"})
	got, err := svc.accessibleServers(ctx, false)
	if err != nil {
		t.Fatalf("accessibleServers() error = %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d servers, want 0 (access denied)", len(got))
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

	// Cache has 5 tools for "myserver".
	toolList := make([]mcp.Tool, 5)
	for i := range toolList {
		toolList[i] = mcp.Tool{Name: "tool_" + string(rune('a'+i))}
	}
	tc := newPreloadedCache(t, map[string][]mcp.Tool{"myserver": toolList})

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
	tc := newPreloadedCache(t, map[string][]mcp.Tool{"search-srv": tools})
	svc := &codeModeService{db: mock, toolCache: tc, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-s1", Role: "member"})
	result, err := svc.SearchMCPTools(ctx, "find", nil)
	if err != nil {
		t.Fatalf("SearchMCPTools() error = %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d results, want 1", len(result))
	}
	if result[0]["name"] != "find_documents" {
		t.Errorf("got tool %q, want %q", result[0]["name"], "find_documents")
	}
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
	tc := newPreloadedCache(t, map[string][]mcp.Tool{"desc-srv": tools})
	svc := &codeModeService{db: mock, toolCache: tc, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-d1", Role: "member"})
	result, err := svc.SearchMCPTools(ctx, "email", nil)
	if err != nil {
		t.Fatalf("SearchMCPTools() error = %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d results, want 1", len(result))
	}
	if result[0]["name"] != "beta" {
		t.Errorf("got tool %q, want %q", result[0]["name"], "beta")
	}
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
	tc := newPreloadedCache(t, map[string][]mcp.Tool{"ci-srv": tools})
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
			if len(result) != 1 {
				t.Errorf("query %q: got %d results, want 1", tc2.query, len(result))
			}
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
	tc := newPreloadedCache(t, map[string][]mcp.Tool{"blocked-srv": tools})
	svc := &codeModeService{db: mock, toolCache: tc, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-b1", Role: "member"})
	result, err := svc.SearchMCPTools(ctx, "tool", nil)
	if err != nil {
		t.Fatalf("SearchMCPTools() error = %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d results, want 1 (blocked tool must be excluded)", len(result))
	}
	if result[0]["name"] != "safe_tool" {
		t.Errorf("expected safe_tool, got %q", result[0]["name"])
	}
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
		"first":  {{Name: "lookup", Description: "Lookup records"}},
		"second": {{Name: "lookup", Description: "Lookup records"}},
	})
	svc := &codeModeService{db: mock, toolCache: tc, log: newDiscardLogger()}

	ctx := ctxWithIdentity(mcp.KeyIdentity{OrgID: orgID, KeyID: "key-sf", Role: "member"})

	// Restrict to "first" only — should get exactly 1 result even though
	// "second" also has a tool named "lookup".
	result, err := svc.SearchMCPTools(ctx, "lookup", []string{"first"})
	if err != nil {
		t.Fatalf("SearchMCPTools() error = %v", err)
	}
	if len(result) != 1 {
		t.Fatalf("got %d results, want 1", len(result))
	}
	if result[0]["server"] != "first" {
		t.Errorf("got server %q, want %q", result[0]["server"], "first")
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
		"exec-srv": {{Name: "noop", Description: "No-op tool"}},
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
	tc := newPreloadedCache(t, map[string][]mcp.Tool{"tc-srv": tools})
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
	tc := newPreloadedCache(t, map[string][]mcp.Tool{"btc-srv": tools})
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
		"server-a": {{Name: "tool_a"}},
		"server-b": {{Name: "tool_b"}},
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
