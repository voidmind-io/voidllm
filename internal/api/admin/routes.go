package admin

import (
	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/audit"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/cache"
)

// RegisterRoutes mounts all admin API routes under /api/v1 on the given Fiber app.
// The login route at POST /api/v1/auth/login is public. All other routes require
// a valid Bearer API key via auth.Middleware. Individual routes apply additional
// role requirements where needed.
//
// When auditLogger is non-nil, the audit middleware is mounted on the
// authenticated /api/v1 group so that all successful mutations are recorded.
func RegisterRoutes(app *fiber.App, handler *Handler, keyCache *cache.Cache[string, auth.KeyInfo], hmacSecret []byte, auditLogger *audit.Logger) {
	// Public routes — no auth required.
	app.Post("/api/v1/auth/login", handler.Login)
	app.Get("/api/v1/auth/providers", handler.AuthProviders)
	app.Get("/api/v1/invites/peek", handler.PeekInvite)
	app.Post("/api/v1/invites/redeem", handler.RedeemInvite)

	// SSO / OIDC routes — public, only registered when an SSO provider is configured.
	if handler.SSOProvider != nil {
		app.Get("/api/v1/auth/oidc/login", handler.OIDCLogin)
		app.Get("/api/v1/auth/oidc/callback", handler.OIDCCallback)
	}

	var apiMiddlewares []any
	apiMiddlewares = append(apiMiddlewares, auth.Middleware(keyCache, hmacSecret))
	if auditLogger != nil {
		apiMiddlewares = append(apiMiddlewares, audit.Middleware(auditLogger))
	}

	api := app.Group("/api/v1", apiMiddlewares...)

	// Current user profile — no role restriction.
	api.Get("/me", handler.Me)
	api.Get("/me/available-models", handler.AvailableModels)

	// Own usage — no role restriction; any authenticated key sees its own data.
	api.Get("/usage/me", handler.MyUsage)

	// Dashboard stats — no role restriction.
	api.Get("/dashboard/stats", handler.DashboardStats)

	// Organizations
	api.Post("/orgs", auth.RequireRole(auth.RoleSystemAdmin), handler.CreateOrg)
	api.Get("/orgs", auth.RequireRole(auth.RoleOrgAdmin), handler.ListOrgs)
	api.Get("/orgs/:org_id", auth.RequireRole(auth.RoleOrgAdmin), handler.GetOrg)
	api.Patch("/orgs/:org_id", auth.RequireRole(auth.RoleOrgAdmin), handler.UpdateOrg)
	api.Delete("/orgs/:org_id", auth.RequireRole(auth.RoleSystemAdmin), handler.DeleteOrg)

	// Users
	api.Post("/users", auth.RequireRole(auth.RoleOrgAdmin), handler.CreateUser)
	api.Get("/users", auth.RequireRole(auth.RoleSystemAdmin), handler.ListUsers)
	api.Get("/users/:user_id", auth.RequireRole(auth.RoleOrgAdmin), handler.GetUser)
	api.Patch("/users/:user_id", auth.RequireRole(auth.RoleOrgAdmin), handler.UpdateUser)
	api.Delete("/users/:user_id", auth.RequireRole(auth.RoleSystemAdmin), handler.DeleteUser)

	// Org Memberships
	api.Post("/orgs/:org_id/members", auth.RequireRole(auth.RoleOrgAdmin), handler.CreateOrgMembership)
	api.Get("/orgs/:org_id/members", auth.RequireRole(auth.RoleOrgAdmin), handler.ListOrgMemberships)
	api.Patch("/orgs/:org_id/members/:membership_id", auth.RequireRole(auth.RoleOrgAdmin), handler.UpdateOrgMembership)
	api.Delete("/orgs/:org_id/members/:membership_id", auth.RequireRole(auth.RoleOrgAdmin), handler.DeleteOrgMembership)

	// Teams
	api.Post("/orgs/:org_id/teams", auth.RequireRole(auth.RoleOrgAdmin), handler.CreateTeam)
	api.Get("/orgs/:org_id/teams", auth.RequireRole(auth.RoleMember), handler.ListTeams)
	api.Get("/orgs/:org_id/teams/:team_id", auth.RequireRole(auth.RoleMember), handler.GetTeam)
	api.Patch("/orgs/:org_id/teams/:team_id", auth.RequireRole(auth.RoleTeamAdmin), handler.UpdateTeam)
	api.Delete("/orgs/:org_id/teams/:team_id", auth.RequireRole(auth.RoleOrgAdmin), handler.DeleteTeam)

	// Team Memberships
	api.Post("/orgs/:org_id/teams/:team_id/members", auth.RequireRole(auth.RoleTeamAdmin), handler.CreateTeamMembership)
	api.Get("/orgs/:org_id/teams/:team_id/members", auth.RequireRole(auth.RoleTeamAdmin), handler.ListTeamMemberships)
	api.Patch("/orgs/:org_id/teams/:team_id/members/:membership_id", auth.RequireRole(auth.RoleTeamAdmin), handler.UpdateTeamMembership)
	api.Delete("/orgs/:org_id/teams/:team_id/members/:membership_id", auth.RequireRole(auth.RoleTeamAdmin), handler.DeleteTeamMembership)

	// Service Accounts
	api.Post("/orgs/:org_id/service-accounts", auth.RequireRole(auth.RoleMember), handler.CreateServiceAccount)
	api.Get("/orgs/:org_id/service-accounts", auth.RequireRole(auth.RoleMember), handler.ListServiceAccounts)
	api.Get("/orgs/:org_id/service-accounts/:sa_id", auth.RequireRole(auth.RoleMember), handler.GetServiceAccount)
	api.Patch("/orgs/:org_id/service-accounts/:sa_id", auth.RequireRole(auth.RoleMember), handler.UpdateServiceAccount)
	api.Delete("/orgs/:org_id/service-accounts/:sa_id", auth.RequireRole(auth.RoleMember), handler.DeleteServiceAccount)

	// Invites
	api.Post("/orgs/:org_id/invites", auth.RequireRole(auth.RoleOrgAdmin), handler.CreateInvite)
	api.Get("/orgs/:org_id/invites", auth.RequireRole(auth.RoleOrgAdmin), handler.ListInvites)
	api.Delete("/orgs/:org_id/invites/:invite_id", auth.RequireRole(auth.RoleOrgAdmin), handler.RevokeInvite)

	// API Keys
	api.Post("/orgs/:org_id/keys", auth.RequireRole(auth.RoleMember), handler.CreateAPIKey)
	api.Get("/orgs/:org_id/keys", auth.RequireRole(auth.RoleMember), handler.ListAPIKeys)
	api.Get("/orgs/:org_id/keys/:key_id", auth.RequireRole(auth.RoleMember), handler.GetAPIKey)
	api.Patch("/orgs/:org_id/keys/:key_id", auth.RequireRole(auth.RoleMember), handler.UpdateAPIKey)
	api.Delete("/orgs/:org_id/keys/:key_id", auth.RequireRole(auth.RoleMember), handler.DeleteAPIKey)
	api.Post("/orgs/:org_id/keys/:key_id/rotate", auth.RequireRole(auth.RoleMember), handler.RotateAPIKey)

	// Models — global resources managed by system admins only.
	// An org_admin in a multi-org deployment must not be able to add or modify
	// models that are visible to all organisations.
	// Static sub-paths (health, test-connection) are registered before
	// /:model_id so Fiber does not treat them as model_id parameter values.
	api.Get("/models/health", auth.RequireRole(auth.RoleMember), handler.GetModelHealth)
	api.Post("/models/test-connection", auth.RequireRole(auth.RoleSystemAdmin), handler.TestModelConnection)
	api.Post("/models", auth.RequireRole(auth.RoleSystemAdmin), handler.CreateModel)
	api.Get("/models", auth.RequireRole(auth.RoleSystemAdmin), handler.ListModels)
	api.Get("/models/:model_id", auth.RequireRole(auth.RoleSystemAdmin), handler.GetModel)
	api.Patch("/models/:model_id", auth.RequireRole(auth.RoleSystemAdmin), handler.UpdateModel)
	api.Delete("/models/:model_id", auth.RequireRole(auth.RoleSystemAdmin), handler.DeleteModel)
	api.Patch("/models/:model_id/activate", auth.RequireRole(auth.RoleSystemAdmin), handler.ActivateModel)
	api.Patch("/models/:model_id/deactivate", auth.RequireRole(auth.RoleSystemAdmin), handler.DeactivateModel)

	// Model Deployments — sub-resources of a model, managed by system admins.
	api.Post("/models/:model_id/deployments", auth.RequireRole(auth.RoleSystemAdmin), handler.createDeployment)
	api.Get("/models/:model_id/deployments", auth.RequireRole(auth.RoleSystemAdmin), handler.listDeployments)
	api.Patch("/models/:model_id/deployments/:deployment_id", auth.RequireRole(auth.RoleSystemAdmin), handler.updateDeployment)
	api.Delete("/models/:model_id/deployments/:deployment_id", auth.RequireRole(auth.RoleSystemAdmin), handler.deleteDeployment)

	// Model Access Control
	api.Get("/orgs/:org_id/model-access", auth.RequireRole(auth.RoleOrgAdmin), handler.GetOrgModelAccess)
	api.Put("/orgs/:org_id/model-access", auth.RequireRole(auth.RoleOrgAdmin), handler.SetOrgModelAccess)
	api.Get("/orgs/:org_id/teams/:team_id/model-access", auth.RequireRole(auth.RoleTeamAdmin), handler.GetTeamModelAccess)
	api.Put("/orgs/:org_id/teams/:team_id/model-access", auth.RequireRole(auth.RoleTeamAdmin), handler.SetTeamModelAccess)
	api.Get("/orgs/:org_id/keys/:key_id/model-access", auth.RequireRole(auth.RoleOrgAdmin), handler.GetKeyModelAccess)
	api.Put("/orgs/:org_id/keys/:key_id/model-access", auth.RequireRole(auth.RoleOrgAdmin), handler.SetKeyModelAccess)

	// MCP Access Control
	api.Get("/orgs/:org_id/mcp-access", auth.RequireRole(auth.RoleOrgAdmin), handler.GetOrgMCPAccess)
	api.Put("/orgs/:org_id/mcp-access", auth.RequireRole(auth.RoleOrgAdmin), handler.SetOrgMCPAccess)
	api.Get("/orgs/:org_id/teams/:team_id/mcp-access", auth.RequireRole(auth.RoleTeamAdmin), handler.GetTeamMCPAccess)
	api.Put("/orgs/:org_id/teams/:team_id/mcp-access", auth.RequireRole(auth.RoleTeamAdmin), handler.SetTeamMCPAccess)
	api.Get("/orgs/:org_id/keys/:key_id/mcp-access", auth.RequireRole(auth.RoleOrgAdmin), handler.GetKeyMCPAccess)
	api.Put("/orgs/:org_id/keys/:key_id/mcp-access", auth.RequireRole(auth.RoleOrgAdmin), handler.SetKeyMCPAccess)
	api.Get("/orgs/:org_id/available-mcp-servers", auth.RequireRole(auth.RoleOrgAdmin), handler.ListAvailableGlobalMCPServers)

	// Model Aliases
	api.Post("/orgs/:org_id/model-aliases", auth.RequireRole(auth.RoleOrgAdmin), handler.CreateOrgAlias)
	api.Get("/orgs/:org_id/model-aliases", auth.RequireRole(auth.RoleOrgAdmin), handler.ListOrgAliases)
	api.Delete("/orgs/:org_id/model-aliases/:alias_id", auth.RequireRole(auth.RoleOrgAdmin), handler.DeleteOrgAlias)
	api.Post("/orgs/:org_id/teams/:team_id/model-aliases", auth.RequireRole(auth.RoleOrgAdmin), handler.CreateTeamAlias)
	api.Get("/orgs/:org_id/teams/:team_id/model-aliases", auth.RequireRole(auth.RoleOrgAdmin), handler.ListTeamAliases)
	api.Delete("/orgs/:org_id/teams/:team_id/model-aliases/:alias_id", auth.RequireRole(auth.RoleOrgAdmin), handler.DeleteTeamAlias)

	// Usage
	api.Get("/usage", auth.RequireRole(auth.RoleSystemAdmin), handler.SystemAdminUsage)
	api.Get("/orgs/:org_id/usage", auth.RequireRole(auth.RoleOrgAdmin), handler.GetOrgUsage)

	// Audit logs — org_admin and above; org_admin is scoped to own org in the handler.
	api.Get("/audit-logs", auth.RequireRole(auth.RoleOrgAdmin), handler.ListAuditLogs)

	// Org SSO configuration — org_admin and above; handler also enforces the org boundary.
	api.Get("/orgs/:org_id/sso", auth.RequireRole(auth.RoleOrgAdmin), handler.GetOrgSSOConfig)
	api.Put("/orgs/:org_id/sso", auth.RequireRole(auth.RoleOrgAdmin), handler.UpsertOrgSSOConfig)
	api.Delete("/orgs/:org_id/sso", auth.RequireRole(auth.RoleOrgAdmin), handler.DeleteOrgSSOConfig)
	api.Post("/orgs/:org_id/sso/test", auth.RequireRole(auth.RoleOrgAdmin), handler.TestSSOConnection)

	// Global SSO configuration — system admin only, read-only view of YAML config.
	api.Get("/settings/sso", auth.RequireRole(auth.RoleSystemAdmin), handler.GetGlobalSSOConfig)

	// License key management — system_admin only for writes; any member may read.
	api.Put("/settings/license", auth.RequireRole(auth.RoleSystemAdmin), handler.SetLicense)

	// License — any authenticated user may inspect the current license.
	api.Get("/license", auth.RequireRole(auth.RoleMember), handler.GetLicense)

	// Code Mode MCP server — aggregated code execution tools (list_servers,
	// search_tools, execute_code). These routes MUST be registered before the
	// /mcp/:alias routes so that Fiber does not treat the bare /mcp path as
	// alias="" on the parameterised route.
	if handler.CodeModeServer != nil {
		api.Post("/mcp", handler.HandleCodeModeMCP)
		api.Get("/mcp", handler.HandleCodeModeMCPSSE)
	}

	// MCP gateway — any authenticated caller may send MCP requests.
	// The :alias parameter routes to the built-in "voidllm" management server
	// or any registered external MCP server. Individual tools enforce their own
	// RBAC checks via the injected KeyIdentity.
	// GET opens a persistent SSE stream (legacy SSE transport for "voidllm");
	// POST handles JSON-RPC and responds with JSON or SSE per the Accept header.
	if handler.MCPServer != nil {
		api.Post("/mcp/:alias", handler.HandleMCPProxy)
		api.Get("/mcp/:alias", handler.HandleMCPProxySSE)
	}

	// MCP Servers — global resources (system_admin only for write; handler checks
	// scope permissions for shared read/mutate routes).
	// Static sub-paths (:server_id/test) are registered before /:server_id
	// so Fiber does not treat "test" as a server_id parameter value.
	api.Post("/mcp-servers", auth.RequireRole(auth.RoleSystemAdmin), handler.CreateMCPServer)
	api.Get("/mcp-servers", auth.RequireRole(auth.RoleSystemAdmin), handler.ListMCPServers)

	// Org-scoped MCP Servers.
	api.Post("/orgs/:org_id/mcp-servers", auth.RequireRole(auth.RoleOrgAdmin), handler.CreateOrgMCPServer)
	api.Get("/orgs/:org_id/mcp-servers", auth.RequireRole(auth.RoleMember), handler.ListOrgMCPServers)

	// Team-scoped MCP Servers.
	api.Post("/orgs/:org_id/teams/:team_id/mcp-servers", auth.RequireRole(auth.RoleTeamAdmin), handler.CreateTeamMCPServer)
	api.Get("/orgs/:org_id/teams/:team_id/mcp-servers", auth.RequireRole(auth.RoleMember), handler.ListTeamMCPServers)

	// Shared MCP server operations — handler enforces scope-based ownership.
	// Static sub-paths (activate, deactivate, test) are registered before
	// /:server_id so Fiber does not treat them as server_id parameter values.
	api.Get("/mcp-servers/:server_id", auth.RequireRole(auth.RoleMember), handler.GetMCPServer)
	api.Patch("/mcp-servers/:server_id", auth.RequireRole(auth.RoleMember), handler.UpdateMCPServer)
	api.Delete("/mcp-servers/:server_id", auth.RequireRole(auth.RoleMember), handler.DeleteMCPServer)
	api.Patch("/mcp-servers/:server_id/activate", auth.RequireRole(auth.RoleMember), handler.ActivateMCPServer)
	api.Patch("/mcp-servers/:server_id/deactivate", auth.RequireRole(auth.RoleMember), handler.DeactivateMCPServer)
	api.Post("/mcp-servers/:server_id/test", auth.RequireRole(auth.RoleMember), handler.TestMCPServerConnection)

	// MCP server tool blocklist — scope permissions enforced in handlers.
	api.Get("/mcp-servers/:server_id/blocklist", auth.RequireRole(auth.RoleMember), handler.ListMCPServerBlocklist)
	api.Post("/mcp-servers/:server_id/blocklist", auth.RequireRole(auth.RoleMember), handler.AddMCPServerBlocklist)
	api.Delete("/mcp-servers/:server_id/blocklist", auth.RequireRole(auth.RoleMember), handler.RemoveMCPServerBlocklist)

	// MCP server tool cache refresh — scope permissions enforced in handler.
	api.Post("/mcp-servers/:server_id/refresh-tools", auth.RequireRole(auth.RoleMember), handler.HandleRefreshMCPServerTools)

	// MCP server cached tools — returns cached schemas with blocked status.
	api.Get("/mcp-servers/:server_id/tools", auth.RequireRole(auth.RoleMember), handler.HandleListMCPServerTools)
}
