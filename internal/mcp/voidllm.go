package mcp

import (
	"context"
	"fmt"
	"time"

	"github.com/voidmind-io/voidllm/internal/jsonx"
)

// mcpContextKey is a package-private type for context value keys, preventing
// collisions with other packages that use context.WithValue.
type mcpContextKey int

const (
	// keyInfoContextKey is the context key for the authenticated caller's identity,
	// stored as a KeyIdentity value by the MCP transport handler.
	keyInfoContextKey mcpContextKey = iota
)

// KeyIdentity holds the caller identity extracted from the authenticated request.
// It is stored in the context by the MCP transport handler and read by tool
// handlers via VoidLLMDeps.GetKeyInfo.
type KeyIdentity struct {
	// OrgID is the organization the caller belongs to.
	OrgID string
	// TeamID is the team this key is scoped to. Empty if not team-scoped.
	TeamID string
	// KeyID is the unique identifier of the API key used to authenticate.
	KeyID string
	// UserID is the user associated with the key, if any.
	UserID string
	// Role is the RBAC role of the caller.
	Role string
}

// WithKeyIdentity returns a new context carrying the given KeyIdentity.
// Call this in the MCP transport handler before invoking Server.Handle.
func WithKeyIdentity(ctx context.Context, id KeyIdentity) context.Context {
	return context.WithValue(ctx, keyInfoContextKey, id)
}

// keyIdentityFromCtx retrieves the KeyIdentity stored in ctx by WithKeyIdentity.
// Returns a zero-value KeyIdentity if none was set.
func keyIdentityFromCtx(ctx context.Context) KeyIdentity {
	if v, ok := ctx.Value(keyInfoContextKey).(KeyIdentity); ok {
		return v
	}
	return KeyIdentity{}
}

// KeyIdentityFromCtx retrieves the KeyIdentity stored in ctx by WithKeyIdentity.
// Returns a zero-value KeyIdentity if none was set. This exported variant allows
// dependency closures wired in other packages to read caller identity from the
// context without importing fiber or auth packages.
func KeyIdentityFromCtx(ctx context.Context) KeyIdentity {
	return keyIdentityFromCtx(ctx)
}

// VoidLLMDeps holds the injectable dependencies for the built-in VoidLLM MCP
// tools. Each field is a function so the mcp package has no compile-time
// dependency on VoidLLM internal packages. All fields must be non-nil when
// RegisterVoidLLMTools is called.
type VoidLLMDeps struct {
	// ListModels returns metadata for all registered models as JSON-serializable
	// maps. The maps must not include sensitive fields such as API keys or
	// base URLs. It is called for system_admin and org_admin callers.
	ListModels func(ctx context.Context) ([]map[string]any, error)

	// ListAvailableModels returns models accessible to the caller (name and type
	// only). It is called for member and team_admin callers so that strategy,
	// deployment details, and provider information are not exposed.
	ListAvailableModels func(ctx context.Context) ([]map[string]any, error)

	// GetAllHealth returns health state for all probe targets. Each map must
	// contain at least a "name" key (string) and a "status" key (string).
	GetAllHealth func() []map[string]any

	// GetHealth returns health state for a single model or deployment key.
	// key is the canonical model name for single-deployment models, or
	// "modelName/deploymentName" for a specific deployment.
	GetHealth func(key string) (map[string]any, bool)

	// GetUsage returns usage statistics aggregated according to the supplied
	// filter parameters. from and to are optional RFC 3339 timestamps.
	// groupBy is an optional aggregation dimension (e.g. "model", "key").
	// orgID and keyID scope the query to the caller's context.
	GetUsage func(ctx context.Context, from, to, groupBy, orgID, keyID string) (any, error)

	// ListKeys returns API keys visible to the caller, scoped by org and role.
	// Each map must not include the key hash or plaintext.
	ListKeys func(ctx context.Context, orgID, role string) ([]map[string]any, error)

	// CreateKey creates a temporary API key on behalf of the caller and returns
	// a map that includes the plaintext key under the "key" field. expiresIn of
	// zero means no expiry.
	CreateKey func(ctx context.Context, orgID, userID, name string, expiresIn time.Duration) (map[string]any, error)

	// ListDeployments returns the deployment records for the given model ID.
	// Sensitive fields such as API keys must be omitted from the returned maps.
	ListDeployments func(ctx context.Context, modelID string) ([]map[string]any, error)

	// ExecuteCode runs JavaScript in the Code Mode sandbox with MCP tools
	// injected as async functions. code is the JS source, serverAliases
	// optionally restricts which servers' tools are available (nil = all
	// accessible). Returns nil when Code Mode is disabled.
	ExecuteCode func(ctx context.Context, code string, serverAliases []string) (*ExecuteResult, error)

	// ListAccessibleMCPServers returns MCP servers the caller can access.
	// When codeModeOnly is true, only servers with code_mode_enabled are
	// returned. Returns nil when Code Mode is disabled.
	ListAccessibleMCPServers func(ctx context.Context, codeModeOnly bool) ([]map[string]any, error)

	// SearchMCPTools searches tool schemas across accessible servers by
	// keyword. query is matched case-insensitively against tool names and
	// descriptions. serverAliases optionally restricts the search scope
	// (nil = all accessible). Returns nil when Code Mode is disabled.
	SearchMCPTools func(ctx context.Context, query string, serverAliases []string) ([]map[string]any, error)
}

// RegisterVoidLLMTools registers the VoidLLM management tools on the given MCP
// server. The tools cover model listing, health inspection, usage statistics,
// key management, and deployment inspection.
//
// Dependencies are injected via deps so the mcp package remains decoupled from
// VoidLLM internals. All function fields in deps must be non-nil.
//
// Caller identity is read from the request context via WithKeyIdentity; the
// MCP transport handler is responsible for populating the context before
// invoking Server.Handle.
func RegisterVoidLLMTools(s *Server, deps VoidLLMDeps) {
	s.RegisterTool(Tool{
		Name:        "list_models",
		Description: "List all registered models with their metadata and current health status.",
		InputSchema: InputSchema{
			Type: "object",
		},
	}, makeListModels(deps))

	s.RegisterTool(Tool{
		Name:        "get_model_health",
		Description: "Get the current health state for a specific model or deployment. Use \"modelName/deploymentName\" to target a specific deployment within a multi-deployment model.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"model": {
					Type:        "string",
					Description: "Canonical model name, or \"modelName/deploymentName\" for a specific deployment.",
				},
			},
			Required: []string{"model"},
		},
	}, makeGetModelHealth(deps))

	s.RegisterTool(Tool{
		Name:        "get_usage",
		Description: "Get usage statistics for the caller's organization. Results can be filtered by time range and grouped by a dimension.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"from": {
					Type:        "string",
					Description: "Start of the time range as an RFC 3339 timestamp. Defaults to the start of the current day.",
				},
				"to": {
					Type:        "string",
					Description: "End of the time range as an RFC 3339 timestamp. Defaults to now.",
				},
				"group_by": {
					Type:        "string",
					Description: "Aggregation dimension, e.g. \"model\" or \"key\".",
				},
			},
		},
	}, makeGetUsage(deps))

	s.RegisterTool(Tool{
		Name:        "list_keys",
		Description: "List API keys visible to the caller. Org admins and above see all keys in the org; members see only their own keys.",
		InputSchema: InputSchema{
			Type: "object",
		},
	}, makeListKeys(deps))

	s.RegisterTool(Tool{
		Name:        "create_key",
		Description: "Create a temporary API key in the caller's organization. The plaintext key is returned once and cannot be retrieved again.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"name": {
					Type:        "string",
					Description: "Human-readable label for the key.",
				},
				"expires_in": {
					Type:        "string",
					Description: "Go duration until expiry, e.g. \"24h\" or \"168h\". Omit for no expiry.",
				},
			},
			Required: []string{"name"},
		},
	}, makeCreateKey(deps))

	s.RegisterTool(Tool{
		Name:        "list_deployments",
		Description: "List the backend deployments configured for a model. Requires system_admin role.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"model_id": {
					Type:        "string",
					Description: "UUID of the model whose deployments should be listed.",
				},
			},
			Required: []string{"model_id"},
		},
	}, makeListDeployments(deps))
}

// RegisterCodeModeTools registers the Code Mode tools (list_servers,
// search_tools, execute_code) on the given MCP server. These tools are
// only registered when deps.ExecuteCode is non-nil (Code Mode enabled).
func RegisterCodeModeTools(s *Server, deps VoidLLMDeps) {
	if deps.ExecuteCode == nil {
		return
	}

	s.RegisterTool(Tool{
		Name:        "list_servers",
		Description: "List MCP servers available for Code Mode execution. Shows server names, aliases, and tool counts.",
		InputSchema: InputSchema{
			Type: "object",
		},
	}, makeListServers(deps))

	s.RegisterTool(Tool{
		Name:        "search_tools",
		Description: "Search for MCP tools across accessible servers by keyword. Returns matching tool names, descriptions, and input schemas for writing Code Mode scripts.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"query": {
					Type:        "string",
					Description: "Search keyword to match against tool names and descriptions.",
				},
				"server": {
					Type:        "string",
					Description: "Optional server alias to restrict search scope.",
				},
			},
			Required: []string{"query"},
		},
	}, makeSearchTools(deps))

	s.RegisterTool(Tool{
		Name: "execute_code",
		Description: "Execute JavaScript code in a sandboxed WASM runtime with MCP tools available as async functions. " +
			"Tools are accessible via tools.serverAlias.toolName(args). Use await for tool calls. " +
			"Return a value to send it back as the result.",
		InputSchema: InputSchema{
			Type: "object",
			Properties: map[string]Property{
				"code": {
					Type:        "string",
					Description: "JavaScript code to execute. MCP tools are available as async functions under tools.serverAlias.toolName(args).",
				},
				"servers": {
					Type:        "array",
					Description: "Optional list of server aliases to include. Omit for all accessible servers.",
				},
			},
			Required: []string{"code"},
		},
	}, makeExecuteCode(deps))
}

// makeListModels returns the handler for the list_models tool. Callers with
// the system_admin or org_admin role receive full model metadata (name,
// provider, type, aliases, max_context_tokens, strategy, deployment_count)
// merged with live health data. All other callers (team_admin, member) receive
// only name and type for the models accessible to them.
func makeListModels(deps VoidLLMDeps) ToolHandler {
	return func(ctx context.Context, _ jsonx.RawMessage) (*ToolResult, error) {
		id := keyIdentityFromCtx(ctx)
		if id.Role == "system_admin" || id.Role == "org_admin" {
			models, err := deps.ListModels(ctx)
			if err != nil {
				return nil, fmt.Errorf("list models: %w", err)
			}

			healthSlice := deps.GetAllHealth()
			healthByName := make(map[string]map[string]any, len(healthSlice))
			for _, h := range healthSlice {
				if name, ok := h["name"].(string); ok {
					healthByName[name] = h
				}
			}
			for i, m := range models {
				name, _ := m["name"].(string)
				if h, ok := healthByName[name]; ok {
					models[i]["health"] = h["status"]
					models[i]["latency_ms"] = h["latency_ms"]
				}
			}

			out, _ := jsonx.MarshalIndent(models, "", "  ")
			return TextResult(string(out)), nil
		}

		// team_admin and member: return only name + type for accessible models.
		models, err := deps.ListAvailableModels(ctx)
		if err != nil {
			return nil, fmt.Errorf("list available models: %w", err)
		}
		out, _ := jsonx.MarshalIndent(models, "", "  ")
		return TextResult(string(out)), nil
	}
}

// makeGetModelHealth returns the handler for the get_model_health tool.
func makeGetModelHealth(deps VoidLLMDeps) ToolHandler {
	return func(_ context.Context, args jsonx.RawMessage) (*ToolResult, error) {
		var input struct {
			Model string `json:"model"`
		}
		if err := jsonx.Unmarshal(args, &input); err != nil || input.Model == "" {
			return ErrorResult("model parameter is required"), nil
		}

		h, ok := deps.GetHealth(input.Model)
		if !ok {
			return ErrorResult(fmt.Sprintf("no health data for model %q", input.Model)), nil
		}

		out, _ := jsonx.MarshalIndent(h, "", "  ")
		return TextResult(string(out)), nil
	}
}

// makeGetUsage returns the handler for the get_usage tool. All parameters are
// optional; the caller's org and key IDs are appended automatically from the
// request context so results are always scoped to the caller's organization.
func makeGetUsage(deps VoidLLMDeps) ToolHandler {
	return func(ctx context.Context, args jsonx.RawMessage) (*ToolResult, error) {
		var input struct {
			From    string `json:"from"`
			To      string `json:"to"`
			GroupBy string `json:"group_by"`
		}
		// All fields are optional; ignore unmarshal error for empty/null args.
		_ = jsonx.Unmarshal(args, &input)

		id := keyIdentityFromCtx(ctx)
		data, err := deps.GetUsage(ctx, input.From, input.To, input.GroupBy, id.OrgID, id.KeyID)
		if err != nil {
			return nil, fmt.Errorf("get usage: %w", err)
		}

		out, _ := jsonx.MarshalIndent(data, "", "  ")
		return TextResult(string(out)), nil
	}
}

// makeListKeys returns the handler for the list_keys tool.
func makeListKeys(deps VoidLLMDeps) ToolHandler {
	return func(ctx context.Context, _ jsonx.RawMessage) (*ToolResult, error) {
		id := keyIdentityFromCtx(ctx)
		keys, err := deps.ListKeys(ctx, id.OrgID, id.Role)
		if err != nil {
			return nil, fmt.Errorf("list keys: %w", err)
		}

		out, _ := jsonx.MarshalIndent(keys, "", "  ")
		return TextResult(string(out)), nil
	}
}

// makeCreateKey returns the handler for the create_key tool.
func makeCreateKey(deps VoidLLMDeps) ToolHandler {
	return func(ctx context.Context, args jsonx.RawMessage) (*ToolResult, error) {
		var input struct {
			Name      string `json:"name"`
			ExpiresIn string `json:"expires_in"`
		}
		if err := jsonx.Unmarshal(args, &input); err != nil || input.Name == "" {
			return ErrorResult("name parameter is required"), nil
		}

		var expiry time.Duration
		if input.ExpiresIn != "" {
			d, err := time.ParseDuration(input.ExpiresIn)
			if err != nil {
				return ErrorResult(fmt.Sprintf("invalid expires_in %q: %v", input.ExpiresIn, err)), nil
			}
			expiry = d
		}

		id := keyIdentityFromCtx(ctx)
		result, err := deps.CreateKey(ctx, id.OrgID, id.UserID, input.Name, expiry)
		if err != nil {
			return nil, fmt.Errorf("create key: %w", err)
		}

		out, _ := jsonx.MarshalIndent(result, "", "  ")
		return TextResult(string(out)), nil
	}
}

// makeListDeployments returns the handler for the list_deployments tool.
// Access is restricted to callers with the system_admin role.
func makeListDeployments(deps VoidLLMDeps) ToolHandler {
	return func(ctx context.Context, args jsonx.RawMessage) (*ToolResult, error) {
		id := keyIdentityFromCtx(ctx)
		if id.Role != "system_admin" {
			return ErrorResult("system_admin role required"), nil
		}

		var input struct {
			ModelID string `json:"model_id"`
		}
		if err := jsonx.Unmarshal(args, &input); err != nil || input.ModelID == "" {
			return ErrorResult("model_id parameter is required"), nil
		}

		deployments, err := deps.ListDeployments(ctx, input.ModelID)
		if err != nil {
			return nil, fmt.Errorf("list deployments: %w", err)
		}

		out, _ := jsonx.MarshalIndent(deployments, "", "  ")
		return TextResult(string(out)), nil
	}
}

// makeListServers returns the handler for the list_servers tool. It returns
// only servers with Code Mode enabled, as seen by the authenticated caller.
func makeListServers(deps VoidLLMDeps) ToolHandler {
	return func(ctx context.Context, _ jsonx.RawMessage) (*ToolResult, error) {
		servers, err := deps.ListAccessibleMCPServers(ctx, true)
		if err != nil {
			return nil, fmt.Errorf("list accessible mcp servers: %w", err)
		}

		out, _ := jsonx.MarshalIndent(servers, "", "  ")
		return TextResult(string(out)), nil
	}
}

// makeSearchTools returns the handler for the search_tools tool. It searches
// tool schemas across accessible MCP servers by a caller-supplied keyword.
func makeSearchTools(deps VoidLLMDeps) ToolHandler {
	return func(ctx context.Context, args jsonx.RawMessage) (*ToolResult, error) {
		var input struct {
			Query  string `json:"query"`
			Server string `json:"server"`
		}
		if err := jsonx.Unmarshal(args, &input); err != nil || input.Query == "" {
			return ErrorResult("query parameter is required"), nil
		}

		var serverAliases []string
		if input.Server != "" {
			serverAliases = []string{input.Server}
		}

		tools, err := deps.SearchMCPTools(ctx, input.Query, serverAliases)
		if err != nil {
			return nil, fmt.Errorf("search mcp tools: %w", err)
		}

		out, _ := jsonx.MarshalIndent(tools, "", "  ")
		return TextResult(string(out)), nil
	}
}

// makeExecuteCode returns the handler for the execute_code tool. It runs
// caller-supplied JavaScript in the Code Mode WASM sandbox with the requested
// MCP servers' tools injected as async functions.
// maxCodeSize is the maximum allowed length of JavaScript code submitted to
// execute_code. This prevents resource exhaustion during string concatenation
// and WASM compilation before the sandbox memory limit takes effect.
const maxCodeSize = 256 * 1024 // 256 KB

func makeExecuteCode(deps VoidLLMDeps) ToolHandler {
	return func(ctx context.Context, args jsonx.RawMessage) (*ToolResult, error) {
		var input struct {
			Code    string   `json:"code"`
			Servers []string `json:"servers"`
		}
		if err := jsonx.Unmarshal(args, &input); err != nil || input.Code == "" {
			return ErrorResult("code parameter is required"), nil
		}
		if len(input.Code) > maxCodeSize {
			return ErrorResult("code exceeds maximum size (256 KB)"), nil
		}

		result, err := deps.ExecuteCode(ctx, input.Code, input.Servers)
		if err != nil {
			return nil, fmt.Errorf("execute code: %w", err)
		}

		if result.Error != "" {
			return ErrorResult(result.Error), nil
		}

		out, _ := jsonx.MarshalIndent(result, "", "  ")
		return TextResult(string(out)), nil
	}
}
