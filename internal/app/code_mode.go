package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/internal/metrics"
	"github.com/voidmind-io/voidllm/internal/proxy"
)

// codeModeDB is the subset of database methods needed by Code Mode.
// Using an interface instead of *db.DB allows unit testing with mocks.
type codeModeDB interface {
	ListMCPServers(ctx context.Context) ([]db.MCPServer, error)
	ListMCPServersByOrg(ctx context.Context, orgID string) ([]db.MCPServer, error)
	ListMCPServersByTeam(ctx context.Context, teamID, orgID string) ([]db.MCPServer, error)
	CheckMCPAccess(ctx context.Context, orgID, teamID, keyID, serverID string) (bool, error)
	ListBlockedToolNames(ctx context.Context, serverID string) ([]string, error)
	SaveOutputSchema(ctx context.Context, serverID, toolName string, schema jsonx.RawMessage) error
	GetOutputSchema(ctx context.Context, serverID, toolName string) (jsonx.RawMessage, error)
	GetAllOutputSchemas(ctx context.Context, serverID string, maxAge time.Duration) (map[string]jsonx.RawMessage, error)
	IsOutputSchemaStale(ctx context.Context, serverID, toolName string, maxAge time.Duration) (bool, error)
}

// mcpServerByIDer is the subset of proxy.MCPServerCache used by codeModeService
// to resolve a server database ID to its alias for TypeScript type generation.
type mcpServerByIDer interface {
	GetByID(serverID string) (*db.MCPServer, bool)
}

// mcpServerCacheReader is the subset of proxy.MCPServerCache used by
// codeModeAccessChecker to mirror codeModeService.accessibleServers' own
// visibility decision from in-memory state only — GetByID for a
// Server-scoped lookup by the notification's own trigger (unused by the
// checker itself, kept here so every mcpServerByIDer caller can share one
// interface) and List to build the same scoped, alias-deduplicated view
// accessibleServers derives from a DB query (see visibleMCPServersForCaller
// and aliasWinnerForCaller).
type mcpServerCacheReader interface {
	mcpServerByIDer
	List() []db.MCPServer
}

// searchToolsLimit is the maximum number of matched tools returned by
// SearchMCPTools. Matches VoidMCP's hard limit to keep responses tractable.
const searchToolsLimit = 50

// toolsListHookDeadline bounds the total wall-clock time toolsListHook may
// spend on every DB read it performs — accessibleServers, the per-server
// blocklist reads, and the per-server output-schema reads — combined. It is
// derived from the incoming request context so cancellation of the request
// still takes effect immediately; if it elapses before the hook finishes
// rendering the dynamic tool list, the hook fails closed and returns the
// static description instead of a partially-built one.
const toolsListHookDeadline = 3 * time.Second

// codeModeService holds the dependencies for the three Code Mode VoidLLMDeps
// closures (ExecuteCode, ListAccessibleMCPServers, SearchMCPTools) and the
// OnToolsListHook. It is constructed once in app.go and its methods are wired
// directly as function values into mcp.VoidLLMDeps.
type codeModeService struct {
	executor     *mcp.Executor
	toolCache    *mcp.ToolCache
	callMCPTool  func(ctx context.Context, ki *auth.KeyInfo, alias, tool string, args jsonx.RawMessage, codeMode bool, execID string) (jsonx.RawMessage, error)
	db           codeModeDB
	log          *slog.Logger
	maxToolCalls int
	// schemaTTL is the TTL for inferred output schemas (from config).
	schemaTTL time.Duration
	// serverCache resolves server IDs to aliases for TypeScript type generation.
	serverCache mcpServerByIDer
	// codePool is optional; when non-nil the pool's Available count is recorded
	// in the CodeModePoolAvailable metric after each execution.
	codePool interface{ Available() int }
	// notifyServerChanged, when non-nil, is called with a server's own ID
	// whenever this service writes data the Code Mode subscriptions/listen
	// hook (toolsListHook) renders for that server, outside of ToolCache's
	// own SetOnChange trigger — today, only ExecuteCode's own OnToolResult
	// callback, the first time a tool's inferred output schema is saved or
	// actually changes (item 3). Wired in internal/app to
	// codeModeServer.NotifyToolsListChanged(mcp.NotifyScope{ServerID:
	// serverID}), the exact same call ToolCache.SetOnChange's own hook makes
	// — see app.go's wiring comment for why this is a SEPARATE hook rather
	// than reusing ToolCache.SetOnChange itself (output schemas are not
	// tracked by ToolCache at all). nil is a perfectly ordinary configuration
	// (Code Mode disabled, or a test that never exercises this path) and
	// every call site nil-checks before invoking.
	notifyServerChanged func(serverID string)
}

// accessibleServers returns the MCP servers visible to the caller identified by
// the KeyIdentity stored in ctx. Global servers (OrgID == nil, TeamID == nil)
// are included only when CheckMCPAccess grants the caller explicit access. When
// codeModeOnly is true, servers with CodeModeEnabled == false are excluded from
// the returned slice.
func (s *codeModeService) accessibleServers(ctx context.Context, codeModeOnly bool) ([]db.MCPServer, error) {
	ki := mcp.KeyIdentityFromCtx(ctx)

	var servers []db.MCPServer
	var listErr error
	if ki.TeamID != "" {
		servers, listErr = s.db.ListMCPServersByTeam(ctx, ki.TeamID, ki.OrgID)
	} else if ki.OrgID != "" {
		servers, listErr = s.db.ListMCPServersByOrg(ctx, ki.OrgID)
	} else {
		servers, listErr = s.db.ListMCPServers(ctx)
	}
	if listErr != nil {
		return nil, listErr
	}

	// Multiple servers may share the same alias across scopes — e.g. an
	// org-scoped and a global server both registered under alias "foo". Every
	// caller of accessibleServers keys its output by alias (tool lists,
	// search results, execution dispatch), so resolve ties here, first, among
	// every scope-visible active server — before any per-server exclusion
	// below (code-mode-enabled filter, global access grant). This mirrors the
	// team > org > global priority applied at execution time by
	// proxy.MCPServerCache.Get and db.GetMCPServerByAliasScoped, which
	// resolve purely on scope and never consider CodeModeEnabled or the
	// global access grant. Resolving winners on the filtered subset instead
	// would let a lower-priority server "win" an alias here whenever the
	// true (higher-priority) winner is excluded by one of those filters,
	// even though the execution path still dispatches every call for that
	// alias to the true winner — advertising tools and schemas the execution
	// path can never actually reach. Deduping first means a winner that
	// turns out to be unusable (see the per-winner checks below) simply
	// drops the alias entirely; there is no loser left to fall back to.
	winners := resolveServersByAlias(servers)

	// Filter global servers (OrgID == nil && TeamID == nil) to only those
	// explicitly allowed via org/team/key access tables. Org- and team-scoped
	// servers are implicitly accessible to members of that org/team.
	// System admins bypass the access check — they have unrestricted access.
	isSystemAdmin := ki.Role == auth.RoleSystemAdmin
	accessible := make([]db.MCPServer, 0, len(winners))
	for _, sv := range winners {
		if sv.OrgID != nil || sv.TeamID != nil {
			if !codeModeOnly || sv.CodeModeEnabled {
				accessible = append(accessible, sv)
			}
			continue
		}
		// Built-in server is always accessible — no explicit MCP access entry needed.
		if sv.Source == "builtin" {
			if !codeModeOnly || sv.CodeModeEnabled {
				accessible = append(accessible, sv)
			}
			continue
		}
		if isSystemAdmin {
			if !codeModeOnly || sv.CodeModeEnabled {
				accessible = append(accessible, sv)
			}
			continue
		}
		allowed, accessErr := s.db.CheckMCPAccess(ctx, ki.OrgID, ki.TeamID, ki.KeyID, sv.ID)
		if accessErr != nil {
			continue
		}
		if allowed {
			if !codeModeOnly || sv.CodeModeEnabled {
				accessible = append(accessible, sv)
			}
		}
	}

	return accessible, nil
}

// resolveServersByAlias deduplicates servers by alias, keeping only the
// highest-priority server for each alias (see serverScopePriority). The
// order of first appearance of each alias in servers is preserved in the
// returned slice.
func resolveServersByAlias(servers []db.MCPServer) []db.MCPServer {
	type winner struct {
		server   db.MCPServer
		priority int
	}
	winners := make(map[string]winner, len(servers))
	order := make([]string, 0, len(servers))
	for _, sv := range servers {
		priority := serverScopePriority(sv)
		w, ok := winners[sv.Alias]
		if !ok {
			winners[sv.Alias] = winner{server: sv, priority: priority}
			order = append(order, sv.Alias)
			continue
		}
		if priority < w.priority {
			winners[sv.Alias] = winner{server: sv, priority: priority}
		}
	}
	resolved := make([]db.MCPServer, 0, len(order))
	for _, alias := range order {
		resolved = append(resolved, winners[alias].server)
	}
	return resolved
}

// serverScopePriority returns the alias-resolution priority for sv: 1
// (highest, team-scoped), 2 (org-scoped), or 3 (global, lowest). Lower values
// win ties in resolveServersByAlias. This mirrors the CASE ordering in
// db.GetMCPServerByAliasScoped and the team > org > global priority applied
// by proxy.MCPServerCache.Get.
func serverScopePriority(sv db.MCPServer) int {
	switch {
	case sv.TeamID != nil:
		return 1
	case sv.OrgID != nil:
		return 2
	default:
		return 3
	}
}

// ExecuteCode runs JavaScript code in the Code Mode sandbox with MCP tools
// from accessible servers injected as async functions. It returns nil when Code
// Mode is disabled (executor == nil). serverAliases restricts which servers'
// tools are available; nil means all accessible servers.
func (s *codeModeService) ExecuteCode(ctx context.Context, code string, serverAliases []string) (*mcp.ExecuteResult, error) {
	if s.executor == nil {
		return nil, nil
	}

	// List MCP servers accessible to this caller with code_mode_enabled.
	servers, listErr := s.accessibleServers(ctx, true)
	if listErr != nil {
		return nil, fmt.Errorf("execute code: list servers: %w", listErr)
	}

	ki := mcp.KeyIdentityFromCtx(ctx)

	// Build a set of requested aliases for fast lookup (nil = all).
	wantSet := make(map[string]bool, len(serverAliases))
	for _, a := range serverAliases {
		wantSet[a] = true
	}

	// Build the blocklist map (alias → set of blocked tool names) and the
	// per-alias tool list in a single pass. blockedByServer is used both to
	// filter the tool list and as a second defense inside the ToolCaller
	// closure below. excludedAliases holds the aliases of servers whose
	// blocklist could not be read at all: such a server is excluded from
	// every Code Mode surface for this execution — no tools listed, no calls
	// permitted — rather than treated as having an empty blocklist. A
	// blocklist read failure must never silently widen what an execution is
	// allowed to reach.
	blockedByServer := make(map[string]map[string]bool)
	excludedAliases := make(map[string]bool)
	serverTools := make(map[string][]mcp.Tool)
	aliasToServerID := make(map[string]string, len(servers))
	for _, sv := range servers {
		if len(wantSet) > 0 && !wantSet[sv.Alias] {
			continue
		}

		blocked, blockErr := s.db.ListBlockedToolNames(ctx, sv.ID)
		if blockErr != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn,
				"code mode: list blocked tools failed, excluding server",
				slog.String("server_id", sv.ID))
			excludedAliases[sv.Alias] = true
			continue
		}
		if len(blocked) > 0 {
			set := make(map[string]bool, len(blocked))
			for _, name := range blocked {
				set[name] = true
			}
			blockedByServer[sv.Alias] = set
		}

		aliasToServerID[sv.Alias] = sv.ID

		tools, toolErr := s.toolCache.GetTools(ctx, sv.ID)
		if toolErr != nil {
			// A single server failure does not abort the whole execution.
			s.log.LogAttrs(ctx, slog.LevelWarn, "code mode: get tools",
				slog.String("server", sv.Alias),
				slog.String("error", toolErr.Error()),
			)
			continue
		}
		if bs := blockedByServer[sv.Alias]; len(bs) > 0 {
			filtered := make([]mcp.Tool, 0, len(tools))
			for _, t := range tools {
				if !bs[t.Name] {
					filtered = append(filtered, t)
				}
			}
			serverTools[sv.Alias] = filtered
		} else {
			serverTools[sv.Alias] = tools
		}
	}

	// Build auth.KeyInfo from mcp.KeyIdentity so callMCPTool can enforce access.
	kiAuth := &auth.KeyInfo{
		ID:     ki.KeyID,
		OrgID:  ki.OrgID,
		TeamID: ki.TeamID,
		UserID: ki.UserID,
		Role:   ki.Role,
	}

	executionUUID, uuidErr := uuid.NewV7()
	if uuidErr != nil {
		return nil, fmt.Errorf("execute code: generate execution id: %w", uuidErr)
	}
	executionID := executionUUID.String()

	callTool := mcp.ToolCaller(func(callCtx context.Context, serverAlias, toolName string, args jsonx.RawMessage) (jsonx.RawMessage, error) {
		if excludedAliases[serverAlias] {
			return nil, fmt.Errorf("tool %q is unavailable on server %q", toolName, serverAlias)
		}
		if bs, ok := blockedByServer[serverAlias]; ok && bs[toolName] {
			return nil, fmt.Errorf("tool %q is blocked on server %q", toolName, serverAlias)
		}
		return s.callMCPTool(callCtx, kiAuth, serverAlias, toolName, args, true, executionID)
	})

	start := time.Now()
	result, execErr := s.executor.Execute(ctx, mcp.ExecuteParams{
		Code:         code,
		ServerTools:  serverTools,
		CallTool:     callTool,
		MaxToolCalls: s.maxToolCalls,
		ExecutionID:  executionID,
		OnToolResult: func(alias, toolName string, result jsonx.RawMessage) {
			serverID, ok := aliasToServerID[alias]
			if !ok {
				return
			}
			// Use a 5-second timeout: the hook runs in a goroutine that
			// outlives the request, but must not pin the goroutine
			// indefinitely on a slow or contended DB write.
			hctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			stale, err := s.db.IsOutputSchemaStale(hctx, serverID, toolName, s.schemaTTL)
			if err != nil || !stale {
				return
			}
			schema := mcp.InferSchema(result)
			if schema == nil {
				return
			}
			// previous is read BEFORE the save below so it reflects the
			// state a subscriber's own last tools/list render was built
			// from — schemaChanged compares it against the newly inferred
			// schema so the notification below (item 3) fires only when
			// this write actually changes what toolsListHook renders next,
			// never on every tool call whose output happens to be schema-
			// identical to what was already stored (including the routine
			// re-infer-on-staleness case, which very often IS identical).
			previous, prevErr := s.db.GetOutputSchema(hctx, serverID, toolName)
			if prevErr != nil && !errors.Is(prevErr, db.ErrNotFound) {
				s.log.LogAttrs(hctx, slog.LevelWarn, "schema inference: read previous schema failed",
					slog.String("server_id", serverID),
					slog.String("tool", toolName),
					slog.String("error", prevErr.Error()))
			}
			if saveErr := s.db.SaveOutputSchema(hctx, serverID, toolName, schema); saveErr != nil {
				s.log.LogAttrs(hctx, slog.LevelWarn, "schema inference: save failed",
					slog.String("server_id", serverID),
					slog.String("tool", toolName),
					slog.String("error", saveErr.Error()))
				return
			}
			if s.notifyServerChanged != nil && !bytes.Equal(previous, schema) {
				s.notifyServerChanged(serverID)
			}
		},
	})
	duration := time.Since(start)

	if execErr != nil {
		metrics.CodeModeExecutionsTotal.WithLabelValues("error").Inc()
		return nil, fmt.Errorf("execute code: %w", execErr)
	}

	execStatus := "success"
	if result.Error != "" {
		execStatus = "error"
		if isCodeModeTimeout(result.Error) {
			execStatus = "timeout"
		} else if isCodeModeOOM(result.Error) {
			execStatus = "oom"
		}
	}
	metrics.CodeModeExecutionsTotal.WithLabelValues(execStatus).Inc()
	metrics.CodeModeExecutionDurationSeconds.Observe(duration.Seconds())
	metrics.CodeModeToolCallsPerExecution.Observe(float64(len(result.ToolCalls)))
	if s.codePool != nil {
		metrics.CodeModePoolAvailable.Set(float64(s.codePool.Available()))
	}

	return result, nil
}

// ListAccessibleMCPServers returns a JSON-serializable summary of MCP servers
// the caller can access. When codeModeOnly is true only servers with
// code_mode_enabled are included. Returns nil when the tool cache is nil
// (Code Mode disabled).
func (s *codeModeService) ListAccessibleMCPServers(ctx context.Context, codeModeOnly bool) ([]map[string]any, error) {
	if s.toolCache == nil {
		return nil, nil
	}

	servers, listErr := s.accessibleServers(ctx, codeModeOnly)
	if listErr != nil {
		return nil, fmt.Errorf("list accessible mcp servers: %w", listErr)
	}

	result := make([]map[string]any, 0, len(servers))
	for _, sv := range servers {
		blocked, blockErr := s.db.ListBlockedToolNames(ctx, sv.ID)
		if blockErr != nil {
			// Fail closed: a server whose blocklist cannot be read is
			// excluded from the result entirely rather than reported with an
			// unadjusted (too-high) tool_count.
			s.log.LogAttrs(ctx, slog.LevelWarn,
				"list servers: list blocked tools failed, excluding server",
				slog.String("server_id", sv.ID))
			continue
		}
		toolCount := s.toolCache.ToolCount(sv.ID)
		toolCount -= len(blocked)
		if toolCount < 0 {
			toolCount = 0
		}
		entry := map[string]any{
			"alias":             sv.Alias,
			"name":              sv.Name,
			"code_mode_enabled": sv.CodeModeEnabled,
			"tool_count":        toolCount,
		}
		result = append(result, entry)
	}
	return result, nil
}

// SearchMCPTools searches tool schemas across accessible MCP servers by
// keyword and returns a TypeScript text block ready for LLM consumption.
// query is matched case-insensitively against tool name and description.
// serverAliases restricts the search scope when non-empty (nil = all).
// Returns an empty string when the tool cache is nil (Code Mode disabled).
// At most searchToolsLimit tools are returned; when query == "*" and results
// were truncated a notice is appended.
func (s *codeModeService) SearchMCPTools(ctx context.Context, query string, serverAliases []string) (string, error) {
	if s.toolCache == nil {
		return "", nil
	}

	servers, listErr := s.accessibleServers(ctx, true)
	if listErr != nil {
		return "", fmt.Errorf("search mcp tools: list servers: %w", listErr)
	}

	wantSet := make(map[string]bool, len(serverAliases))
	for _, a := range serverAliases {
		wantSet[a] = true
	}

	queryLower := strings.ToLower(query)
	matched := make(map[string][]mcp.Tool)
	matchedServerIDs := make(map[string]struct{})
	matchCount := 0
	totalAvailable := 0

	for _, sv := range servers {
		if len(wantSet) > 0 && !wantSet[sv.Alias] {
			continue
		}
		tools, toolErr := s.toolCache.GetTools(ctx, sv.ID)
		if toolErr != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn, "search mcp tools: get tools",
				slog.String("server", sv.Alias),
				slog.String("error", toolErr.Error()),
			)
			continue
		}
		blocked, blockErr := s.db.ListBlockedToolNames(ctx, sv.ID)
		if blockErr != nil {
			// Fail closed: a server whose blocklist cannot be read is
			// excluded from search results entirely rather than searched with
			// an assumed-empty blocklist.
			s.log.LogAttrs(ctx, slog.LevelWarn,
				"search mcp tools: list blocked tools failed, excluding server",
				slog.String("server_id", sv.ID))
			continue
		}
		blockedSet := make(map[string]bool, len(blocked))
		for _, name := range blocked {
			blockedSet[name] = true
		}
		for _, t := range tools {
			if blockedSet[t.Name] {
				continue
			}
			if !strings.Contains(strings.ToLower(t.Name), queryLower) &&
				!strings.Contains(strings.ToLower(t.Description), queryLower) {
				continue
			}
			totalAvailable++
			if matchCount < searchToolsLimit {
				matched[sv.Alias] = append(matched[sv.Alias], t)
				matchedServerIDs[sv.ID] = struct{}{}
				matchCount++
			}
		}
	}

	if matchCount == 0 {
		return fmt.Sprintf("No tools found matching %q.", query), nil
	}

	outputSchemas := make(map[string]map[string]jsonx.RawMessage, len(matchedServerIDs))
	for serverID := range matchedServerIDs {
		if s.serverCache == nil {
			continue
		}
		sv, ok := s.serverCache.GetByID(serverID)
		if !ok {
			continue
		}
		schemas, schemaErr := s.db.GetAllOutputSchemas(ctx, serverID, s.schemaTTL)
		if schemaErr != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn, "search mcp tools: get output schemas",
				slog.String("server", sv.Alias),
				slog.String("error", schemaErr.Error()))
			continue
		}
		if len(schemas) > 0 {
			outputSchemas[sv.Alias] = schemas
		}
	}

	types := mcp.GenerateToolTypeDefs(matched, outputSchemas)

	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d tool(s) matching %q:\n\n", matchCount, query)
	sb.WriteString(types)
	if totalAvailable > matchCount {
		fmt.Fprintf(&sb, "\n(showing %d of %d tools)\n", matchCount, totalAvailable)
	}
	return sb.String(), nil
}

// visibleMCPServersForCaller filters all — a full, unscoped snapshot of
// every active MCP server this process knows about (proxy.MCPServerCache.List)
// — down to exactly the subset one of the three DB queries
// codeModeService.accessibleServers itself would have issued for id would
// have returned, reimplementing each query's own WHERE clause in Go rather
// than re-issuing it against the database:
//
//   - id.TeamID != "": mirrors db.DB.ListMCPServersByTeam(teamID, orgID) —
//     (team_id = teamID AND org_id = orgID) OR (team_id IS NULL AND org_id =
//     orgID) OR (team_id IS NULL AND org_id IS NULL).
//   - id.TeamID == "" && id.OrgID != "": mirrors db.DB.ListMCPServersByOrg(orgID)
//     — (org_id = orgID OR org_id IS NULL) AND team_id IS NULL.
//   - neither set: mirrors db.DB.ListMCPServers() — org_id IS NULL AND
//     team_id IS NULL.
//
// This is the visibility half of accessibleServers' own decision — the DB
// query it issues before ever reaching its own per-server loop — kept here
// as its own function so aliasWinnerForCaller can apply resolveServersByAlias
// to exactly this same scoped subset, matching accessibleServers' own
// "dedupe first, then filter" ordering (see resolveServersByAlias' own doc
// for why that order matters).
func visibleMCPServersForCaller(all []db.MCPServer, id mcp.KeyIdentity) []db.MCPServer {
	visible := make([]db.MCPServer, 0, len(all))
	for _, sv := range all {
		switch {
		case id.TeamID != "":
			teamMatch := sv.TeamID != nil && *sv.TeamID == id.TeamID && sv.OrgID != nil && *sv.OrgID == id.OrgID
			orgMatch := sv.TeamID == nil && sv.OrgID != nil && *sv.OrgID == id.OrgID
			globalMatch := sv.TeamID == nil && sv.OrgID == nil
			if teamMatch || orgMatch || globalMatch {
				visible = append(visible, sv)
			}
		case id.OrgID != "":
			if sv.TeamID == nil && (sv.OrgID == nil || *sv.OrgID == id.OrgID) {
				visible = append(visible, sv)
			}
		default:
			if sv.OrgID == nil && sv.TeamID == nil {
				visible = append(visible, sv)
			}
		}
	}
	return visible
}

// aliasWinnerForCaller resolves serverID against the alias-deduplicated view
// of all a caller with identity id would see — visibleMCPServersForCaller
// followed by resolveServersByAlias, the identical two-step
// codeModeService.accessibleServers itself performs — and reports the
// resulting winning db.MCPServer, or false if serverID is not visible to id
// at all, OR is visible but lost its own alias to a higher-priority sibling
// (e.g. a team-scoped server of the SAME alias the caller's team also
// happens to see) — exactly as accessibleServers itself would silently drop
// a loser from its own returned slice.
func aliasWinnerForCaller(all []db.MCPServer, id mcp.KeyIdentity, serverID string) (db.MCPServer, bool) {
	winners := resolveServersByAlias(visibleMCPServersForCaller(all, id))
	for _, sv := range winners {
		if sv.ID == serverID {
			return sv, true
		}
	}
	return db.MCPServer{}, false
}

// accessDecisionForWinner applies the remaining accessibleServers per-server
// branches, in order, to winner — an already alias-resolved db.MCPServer row,
// either the LIVE winner aliasWinnerForCaller returned for a serverID lookup,
// or the synthetic row snapshotCandidate built for a NotifiedServerScope —
// exactly mirroring codeModeService.accessibleServers' own per-server loop:
//
//   - CodeModeEnabled must be true (accessibleServers' own codeModeOnly=true
//     filter, applied identically regardless of which branch below grants
//     access).
//   - A team- or org-scoped server is accessible unconditionally once it is
//     the alias winner in id's own visible set — visibleMCPServersForCaller
//     already established that id's own org/team matches, so no further
//     check is needed here (matching accessibleServers' own unconditional
//     append for this branch).
//   - A builtin server is always accessible.
//   - Otherwise (a global, non-builtin server) falls back to
//     mcpAccessCache.Check — the SAME access decision the transparent MCP
//     proxy's own hot path (mcp_proxy.go) applies for this identical case.
//     Unlike accessibleServers itself, this branch does NOT special-case
//     auth.RoleSystemAdmin: accessibleServers' own isSystemAdmin bypass only
//     ever applies within this same global-server branch too (a system
//     admin gains no wider organizational reach through Code Mode's
//     tools/list than the very DB query that already scoped `all` to id's
//     own org/team would allow), so this applies it identically here, not as
//     a blanket bypass of every branch above.
func accessDecisionForWinner(winner db.MCPServer, id mcp.KeyIdentity, mcpAccessCache *proxy.MCPAccessCache) bool {
	if !winner.CodeModeEnabled {
		return false
	}
	if winner.TeamID != nil || winner.OrgID != nil {
		return true
	}
	if winner.Source == "builtin" {
		return true
	}
	if id.Role == auth.RoleSystemAdmin {
		return true
	}
	if mcpAccessCache == nil {
		return false
	}
	return mcpAccessCache.Check(id.OrgID, id.TeamID, id.KeyID, winner.ID)
}

// excludeServerID returns all with every server whose ID equals serverID
// removed — used by snapshotAccessible to keep a stale, LIVE entry for the
// very server a NotifiedServerScope snapshot describes from shadowing the
// snapshot's own, pre-mutation row in the alias-winner resolution below (see
// that function's own doc for why this matters specifically for an
// UpdateMCPServer rescope, where the live cache already reflects the NEW
// row under the SAME ID by the time the snapshot-scoped notification fires).
func excludeServerID(all []db.MCPServer, serverID string) []db.MCPServer {
	out := make([]db.MCPServer, 0, len(all))
	for _, sv := range all {
		if sv.ID != serverID {
			out = append(out, sv)
		}
	}
	return out
}

// snapshotCandidate reconstructs the db.MCPServer row a NotifiedServerScope
// snapshot describes — exactly the fields aliasWinnerForCaller and
// accessDecisionForWinner need (ID, Alias, OrgID, TeamID, Source,
// CodeModeEnabled) — so snapshotAccessible can run the identical
// alias-resolution and access-decision code the live serverID path uses,
// rather than a second, hand-rolled copy of those rules.
func snapshotCandidate(snapshot mcp.NotifiedServerScope) db.MCPServer {
	return db.MCPServer{
		ID:              snapshot.ID,
		Alias:           snapshot.Alias,
		OrgID:           snapshot.OrgID,
		TeamID:          snapshot.TeamID,
		Source:          snapshot.Source,
		CodeModeEnabled: snapshot.CodeModeEnabled,
	}
}

// snapshotAccessible reports whether id could have seen the server snapshot
// describes, applying the exact same alias-winner rule and access decision
// (accessDecisionForWinner) the live serverID path applies — see item 2's own
// requirement that snapshot evaluation must never drift from the live
// checker's rules. snapshot.Active false is checked first and short-circuits
// to false unconditionally: every DB query accessibleServers itself issues
// filters WHERE is_active = 1, so an inactive row was never a candidate for
// any caller's own visible set to begin with (see NotifiedServerScope.Active's
// own doc).
//
// The alias-winner resolution runs against serverCache.List() with the
// snapshot's own (possibly stale) ID excluded (excludeServerID) plus the
// reconstructed snapshot row appended (snapshotCandidate) — representing
// "the world as it was immediately before this one mutation", since every
// OTHER live sibling is unaffected by a mutation to this single server.
func snapshotAccessible(serverCache mcpServerCacheReader, mcpAccessCache *proxy.MCPAccessCache, id mcp.KeyIdentity, snapshot mcp.NotifiedServerScope) bool {
	if !snapshot.Active {
		return false
	}
	candidate := snapshotCandidate(snapshot)
	siblings := excludeServerID(serverCache.List(), candidate.ID)
	all := append(siblings, candidate)
	winner, ok := aliasWinnerForCaller(all, id, candidate.ID)
	if !ok {
		return false
	}
	return accessDecisionForWinner(winner, id, mcpAccessCache)
}

// codeModeAccessChecker returns an mcp.AccessChecker that mirrors
// codeModeService.accessibleServers(ctx, true)'s own access decision (see
// that method's own doc) for a single server — either LIVE (snapshot == nil,
// serverID names it) or a pre-mutation NotifiedServerScope (serverID == "",
// snapshot != nil; see NotifyScope's own doc for when each is used) — built
// entirely from in-memory state — never a database call — so it is safe to
// invoke once per subscriber from mcp.Server.NotifyToolsListChanged's own
// delivery loop.
//
// For the live case, serverCache.List stands in for whichever of
// ListMCPServersByTeam/ByOrg/ListMCPServers accessibleServers itself would
// have queried for this exact identity (see visibleMCPServersForCaller), and
// aliasWinnerForCaller applies the identical alias-deduplication
// accessibleServers performs before its own per-server loop — a server that
// is visible in scope but lost its alias to a higher-priority sibling is
// correctly reported as inaccessible, since accessibleServers' own returned
// slice would never have included it either. For the snapshot case,
// snapshotAccessible applies the exact same two steps against a
// reconstructed, pre-mutation row — see that function's own doc. Both cases
// converge on the same accessDecisionForWinner once a winning row is
// resolved, so the two paths can never drift on what "accessible" means, only
// on which row they resolve it for.
func codeModeAccessChecker(serverCache mcpServerCacheReader, mcpAccessCache *proxy.MCPAccessCache) mcp.AccessChecker {
	return func(id mcp.KeyIdentity, serverID string, snapshot *mcp.NotifiedServerScope) bool {
		if snapshot != nil {
			return snapshotAccessible(serverCache, mcpAccessCache, id, *snapshot)
		}
		winner, ok := aliasWinnerForCaller(serverCache.List(), id, serverID)
		if !ok {
			return false
		}
		return accessDecisionForWinner(winner, id, mcpAccessCache)
	}
}

// toolsListHook returns an mcp.OnToolsListHook that injects TypeScript type
// declarations for the tools of MCP servers accessible to the caller into the
// execute_code tool description. This keeps the LLM-visible schema current as
// the ToolCache is populated lazily, and — critically — scoped to what the
// caller identified by ctx may actually see: the server list comes only from
// accessibleServers, tools are read from the cache snapshot (no upstream
// fetch), and each server's blocklist is applied exactly as ExecuteCode and
// SearchMCPTools do.
//
// The whole hook — accessibleServers, every per-server blocklist read, and
// every per-server output-schema read — is bounded by a single overall
// deadline, toolsListHookDeadline, derived from ctx (see that constant's
// doc). If the deadline elapses before rendering finishes, the hook discards
// whatever it had built and returns the static description unchanged; it
// never returns a partially-rendered tool list.
//
// If ctx carries no caller identity, or accessibleServers fails, the hook
// fails closed: it returns tools unchanged (static description only, no
// upstream tool types are rendered). A server whose blocklist cannot be read
// is excluded from the rendered list entirely — see the ListBlockedToolNames
// error branch below — rather than rendered with an assumed-empty blocklist.
func (s *codeModeService) toolsListHook() mcp.OnToolsListHook {
	return func(ctx context.Context, tools []mcp.Tool) []mcp.Tool {
		if !mcp.KeyIdentityPresent(ctx) {
			s.log.LogAttrs(ctx, slog.LevelWarn,
				"tools/list: no caller identity in context, omitting upstream tool types")
			return tools
		}

		// hctx bounds every DB read this hook performs. It cascades from ctx,
		// so request cancellation still takes effect immediately.
		hctx, cancel := context.WithTimeout(ctx, toolsListHookDeadline)
		defer cancel()

		servers, listErr := s.accessibleServers(hctx, true)
		if listErr != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn, "tools/list: list accessible servers failed",
				slog.String("error", listErr.Error()))
			return tools
		}
		if len(servers) == 0 {
			return tools
		}

		// Snapshot only — no upstream fetch is triggered for tools/list.
		allCached := s.toolCache.GetAllTools() // map[serverID][]Tool

		byAlias := make(map[string][]mcp.Tool, len(servers))
		outputSchemas := make(map[string]map[string]jsonx.RawMessage, len(servers))
		for _, sv := range servers {
			cachedTools, ok := allCached[sv.ID]
			if !ok || len(cachedTools) == 0 {
				continue
			}

			blocked, blockErr := s.db.ListBlockedToolNames(hctx, sv.ID)
			if blockErr != nil {
				// Fail closed: exclude this server entirely rather than
				// render it with an assumed-empty blocklist.
				s.log.LogAttrs(ctx, slog.LevelWarn,
					"tools/list: list blocked tools failed, excluding server",
					slog.String("server_id", sv.ID))
				continue
			}

			filtered := cachedTools
			if len(blocked) > 0 {
				blockedSet := make(map[string]bool, len(blocked))
				for _, name := range blocked {
					blockedSet[name] = true
				}
				filtered = make([]mcp.Tool, 0, len(cachedTools))
				for _, t := range cachedTools {
					if !blockedSet[t.Name] {
						filtered = append(filtered, t)
					}
				}
			}
			if len(filtered) == 0 {
				continue
			}
			byAlias[sv.Alias] = filtered

			schemas, schemaErr := s.db.GetAllOutputSchemas(hctx, sv.ID, s.schemaTTL)
			if schemaErr == nil && len(schemas) > 0 {
				outputSchemas[sv.Alias] = schemas
			}
		}

		// The overall deadline covers every read above. If it expired before
		// rendering finished, discard whatever was built — including servers
		// that were fully processed before the deadline hit — and return the
		// static description rather than a partial list.
		if hctx.Err() != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn,
				"tools/list: deadline exceeded, returning static description")
			return tools
		}

		if len(byAlias) == 0 {
			return tools
		}

		types := mcp.GenerateToolTypeDefs(byAlias, outputSchemas)
		if types == "" {
			return tools
		}
		desc := mcp.CodeModeDescription() + "\n\n## Available Tools\n\n" + types

		return s.finalizeToolsList(ctx, hctx, tools, desc)
	}
}

// finalizeToolsList installs desc as the execute_code tool's description,
// unless hctx's overall deadline has elapsed since the last check performed
// in toolsListHook. Generating desc — mcp.GenerateToolTypeDefs plus the
// description concatenation — is not free, so the "no partial list" rule
// covers that work too: re-checking here, immediately before the result is
// returned, closes the window between "every DB read succeeded" and "the
// rendered description is actually handed back" during which the deadline
// could still elapse. On expiry it discards desc and returns tools unchanged
// (the static description), exactly like every earlier fail-closed check in
// toolsListHook.
func (s *codeModeService) finalizeToolsList(ctx, hctx context.Context, tools []mcp.Tool, desc string) []mcp.Tool {
	if hctx.Err() != nil {
		s.log.LogAttrs(ctx, slog.LevelWarn,
			"tools/list: deadline exceeded after rendering, returning static description")
		return tools
	}
	for i := range tools {
		if tools[i].Name == "execute_code" {
			tools[i].Description = desc
			break
		}
	}
	return tools
}
