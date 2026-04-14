package app

import (
	"context"
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
	GetAllOutputSchemas(ctx context.Context, serverID string, maxAge time.Duration) (map[string]jsonx.RawMessage, error)
	IsOutputSchemaStale(ctx context.Context, serverID, toolName string, maxAge time.Duration) (bool, error)
}

// mcpServerByIDer is the subset of proxy.MCPServerCache used by codeModeService
// to resolve a server database ID to its alias for TypeScript type generation.
type mcpServerByIDer interface {
	GetByID(serverID string) (*db.MCPServer, bool)
}

// searchToolsLimit is the maximum number of matched tools returned by
// SearchMCPTools. Matches VoidMCP's hard limit to keep responses tractable.
const searchToolsLimit = 50

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

	// Filter global servers (OrgID == nil && TeamID == nil) to only those
	// explicitly allowed via org/team/key access tables. Org- and team-scoped
	// servers are implicitly accessible to members of that org/team.
	// System admins bypass the access check — they have unrestricted access.
	isSystemAdmin := ki.Role == auth.RoleSystemAdmin
	accessible := make([]db.MCPServer, 0, len(servers))
	for _, sv := range servers {
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

	// Build a blocklist map (alias → set of blocked tool names) once so
	// it can be used both for filtering the tool list and as a second
	// defense inside the ToolCaller closure.
	blockedByServer := make(map[string]map[string]bool)
	for _, sv := range servers {
		if len(wantSet) > 0 && !wantSet[sv.Alias] {
			continue
		}
		blocked, blockErr := s.db.ListBlockedToolNames(ctx, sv.ID)
		if blockErr != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn, "code mode: list blocked tools",
				slog.String("server", sv.Alias),
				slog.String("error", blockErr.Error()),
			)
			// Continue with an empty blocklist for this server rather than
			// aborting; the ToolCache fetch below will still run.
		}
		if len(blocked) > 0 {
			set := make(map[string]bool, len(blocked))
			for _, name := range blocked {
				set[name] = true
			}
			blockedByServer[sv.Alias] = set
		}
	}

	serverTools := make(map[string][]mcp.Tool)
	for _, sv := range servers {
		if len(wantSet) > 0 && !wantSet[sv.Alias] {
			continue
		}
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
		if bs, ok := blockedByServer[serverAlias]; ok && bs[toolName] {
			return nil, fmt.Errorf("tool %q is blocked on server %q", toolName, serverAlias)
		}
		return s.callMCPTool(callCtx, kiAuth, serverAlias, toolName, args, true, executionID)
	})

	aliasToServerID := make(map[string]string, len(servers))
	for _, sv := range servers {
		if len(wantSet) > 0 && !wantSet[sv.Alias] {
			continue
		}
		aliasToServerID[sv.Alias] = sv.ID
	}

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
			_ = s.db.SaveOutputSchema(hctx, serverID, toolName, schema)
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
		toolCount := s.toolCache.ToolCount(sv.ID)
		blocked, blockErr := s.db.ListBlockedToolNames(ctx, sv.ID)
		if blockErr != nil {
			s.log.LogAttrs(ctx, slog.LevelWarn, "list servers: list blocked tools",
				slog.String("server", sv.Alias),
				slog.String("error", blockErr.Error()))
		}
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
	capped := false

	for _, sv := range servers {
		if capped {
			break
		}
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
			s.log.LogAttrs(ctx, slog.LevelWarn, "search mcp tools: list blocked tools",
				slog.String("server", sv.Alias),
				slog.String("error", blockErr.Error()))
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
			if matchCount >= searchToolsLimit {
				capped = true
				break
			}
			matched[sv.Alias] = append(matched[sv.Alias], t)
			matchedServerIDs[sv.ID] = struct{}{}
			matchCount++
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
	if query == "*" && matchCount < totalAvailable {
		fmt.Fprintf(&sb, "\n(showing %d of %d tools)\n", matchCount, totalAvailable)
	}
	return sb.String(), nil
}

// toolsListHook returns an mcp.OnToolsListHook that injects TypeScript type
// declarations for all currently-cached tools into the execute_code tool
// description. This keeps the LLM-visible schema current as the ToolCache is
// populated lazily.
func (s *codeModeService) toolsListHook() mcp.OnToolsListHook {
	return func(tools []mcp.Tool) []mcp.Tool {
		allCached := s.toolCache.GetAllTools() // map[serverID][]Tool
		if len(allCached) == 0 {
			return tools
		}

		// Convert server ID keys to alias keys so GenerateToolTypeDefs produces
		// TypeScript namespaces that match the JS `await tools.alias.toolName()`
		// calling convention. Entries whose server ID cannot be resolved in the
		// cache are skipped rather than blocking the entire hook.
		byAlias := make(map[string][]mcp.Tool, len(allCached))
		for serverID, serverToolList := range allCached {
			if s.serverCache != nil {
				if server, ok := s.serverCache.GetByID(serverID); ok {
					byAlias[server.Alias] = serverToolList
					continue
				}
			}
			// Fallback: use serverID as key so tools are not silently dropped
			// when the cache is unavailable (e.g. in unit tests).
			byAlias[serverID] = serverToolList
		}

		// The hook runs on every tools/list request. Bound the per-server schema
		// reads so a slow or stuck DB cannot pin the handler.
		outputSchemas := make(map[string]map[string]jsonx.RawMessage, len(allCached))
		hctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		for serverID := range allCached {
			if s.serverCache != nil {
				if server, ok := s.serverCache.GetByID(serverID); ok {
					schemas, err := s.db.GetAllOutputSchemas(hctx, serverID, s.schemaTTL)
					if err == nil && len(schemas) > 0 {
						outputSchemas[server.Alias] = schemas
					}
				}
			}
		}
		types := mcp.GenerateToolTypeDefs(byAlias, outputSchemas)
		if types == "" {
			return tools
		}
		desc := mcp.CodeModeDescription() + "\n\n## Available Tools\n\n" + types
		for i := range tools {
			if tools[i].Name == "execute_code" {
				tools[i].Description = desc
				break
			}
		}
		return tools
	}
}
