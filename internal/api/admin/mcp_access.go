package admin

import (
	"errors"
	"log/slog"
	"slices"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// errMCPAccessResponseWritten is returned by validation helpers that have
// already written a complete error response to the Fiber context. Callers
// map this to nil so Fiber treats the response as sent.
var errMCPAccessResponseWritten = errors.New("mcp access: response written")

// mcpAccessRequest is the JSON body accepted by the Set*MCPAccess handlers.
type mcpAccessRequest struct {
	Servers []string `json:"servers"`
}

// mcpAccessResponse is the JSON body returned by all MCP access handlers.
type mcpAccessResponse struct {
	Servers []string `json:"servers"`
}

// GetOrgMCPAccess handles GET /api/v1/orgs/:org_id/mcp-access.
// Returns the list of MCP server IDs allowed for the org.
// An empty list means "not configured" — no MCP servers are accessible at the org level.
//
// @Summary      Get org MCP access
// @Description  Returns the list of MCP server IDs in the org's allowlist. An empty list means no MCP servers are permitted.
// @Tags         mcp-access
// @Produce      json
// @Param        org_id  path      string  true  "Organization ID"
// @Success      200     {object}  mcpAccessResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/mcp-access [get]
func (h *Handler) GetOrgMCPAccess(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	servers, err := h.DB.GetOrgMCPAccess(c.Context(), orgID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "get org mcp access", slog.String("org_id", orgID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get org mcp access")
	}

	if servers == nil {
		servers = []string{}
	}
	return c.JSON(mcpAccessResponse{Servers: servers})
}

// SetOrgMCPAccess handles PUT /api/v1/orgs/:org_id/mcp-access.
// Replaces the org's MCP server allowlist atomically.
// An empty servers array clears the list (no MCP servers accessible).
//
// @Summary      Set org MCP access
// @Description  Atomically replaces the org's MCP server allowlist. Pass an empty array to deny all MCP servers.
// @Tags         mcp-access
// @Accept       json
// @Produce      json
// @Param        org_id  path      string            true  "Organization ID"
// @Param        body    body      mcpAccessRequest  true  "MCP server allowlist"
// @Success      200     {object}  mcpAccessResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/mcp-access [put]
func (h *Handler) SetOrgMCPAccess(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	var req mcpAccessRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if err := h.validateMCPServerIDs(c, req.Servers); err != nil {
		return nil // response already written
	}

	if err := h.DB.SetOrgMCPAccess(c.Context(), orgID, req.Servers); err != nil {
		h.Log.ErrorContext(c.Context(), "set org mcp access", slog.String("org_id", orgID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to set org mcp access")
	}

	h.refreshMCPAccessCache(c.Context())
	h.publishAccessInvalidation(c.Context())

	return c.JSON(mcpAccessResponse{Servers: req.Servers})
}

// GetTeamMCPAccess handles GET /api/v1/orgs/:org_id/teams/:team_id/mcp-access.
// Returns the list of MCP server IDs allowed for the team.
// An empty list means the team inherits the org's MCP allowlist (no further restriction).
// Team admins may only access teams they are a member of.
//
// @Summary      Get team MCP access
// @Description  Returns the list of MCP server IDs in the team's allowlist. An empty list means the team inherits from parent scope (no further restriction).
// @Tags         mcp-access
// @Produce      json
// @Param        org_id   path      string  true  "Organization ID"
// @Param        team_id  path      string  true  "Team ID"
// @Success      200      {object}  mcpAccessResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/mcp-access [get]
func (h *Handler) GetTeamMCPAccess(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	teamID := c.Params("team_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	if err := h.requireTeamBelongsToOrg(c, teamID, orgID); err != nil {
		return err
	}

	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		isMember, err := h.DB.IsTeamMember(c.Context(), keyInfo.UserID, teamID)
		if err != nil {
			h.Log.ErrorContext(c.Context(), "get team mcp access: check membership", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to verify team membership")
		}
		if !isMember {
			return apierror.NotFound(c, "team not found")
		}
	}

	servers, err := h.DB.GetTeamMCPAccess(c.Context(), teamID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "get team mcp access", slog.String("team_id", teamID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get team mcp access")
	}

	if servers == nil {
		servers = []string{}
	}
	return c.JSON(mcpAccessResponse{Servers: servers})
}

// SetTeamMCPAccess handles PUT /api/v1/orgs/:org_id/teams/:team_id/mcp-access.
// Replaces the team's MCP server allowlist atomically.
// All requested servers must be present in the org's allowlist (if the org has one configured).
// An empty servers array clears the list — the team inherits from parent scope (no further restriction).
// Team admins may only manage access for teams they are a member of.
//
// @Summary      Set team MCP access
// @Description  Atomically replaces the team's MCP server allowlist. All servers must be allowed at the org level. Pass an empty array to inherit from parent scope (no further restriction).
// @Tags         mcp-access
// @Accept       json
// @Produce      json
// @Param        org_id   path      string            true  "Organization ID"
// @Param        team_id  path      string            true  "Team ID"
// @Param        body     body      mcpAccessRequest  true  "MCP server allowlist"
// @Success      200      {object}  mcpAccessResponse
// @Failure      400      {object}  swaggerErrorResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/mcp-access [put]
func (h *Handler) SetTeamMCPAccess(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	teamID := c.Params("team_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	if err := h.requireTeamBelongsToOrg(c, teamID, orgID); err != nil {
		return err
	}

	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		isMember, err := h.DB.IsTeamMember(c.Context(), keyInfo.UserID, teamID)
		if err != nil {
			h.Log.ErrorContext(c.Context(), "set team mcp access: check membership", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to verify team membership")
		}
		if !isMember {
			return apierror.NotFound(c, "team not found")
		}
	}

	var req mcpAccessRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if err := h.validateMCPServerIDs(c, req.Servers); err != nil {
		return nil // response already written
	}

	if len(req.Servers) > 0 {
		if err := h.requireSubsetOfOrgMCPServers(c, orgID, req.Servers); err != nil {
			return nil // response already written
		}
	}

	if err := h.DB.SetTeamMCPAccess(c.Context(), teamID, req.Servers); err != nil {
		h.Log.ErrorContext(c.Context(), "set team mcp access", slog.String("team_id", teamID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to set team mcp access")
	}

	h.refreshMCPAccessCache(c.Context())
	h.publishAccessInvalidation(c.Context())

	return c.JSON(mcpAccessResponse{Servers: req.Servers})
}

// GetKeyMCPAccess handles GET /api/v1/orgs/:org_id/keys/:key_id/mcp-access.
// Returns the list of MCP server IDs allowed for the API key.
// An empty list means the key inherits from parent scope (no further restriction).
//
// @Summary      Get key MCP access
// @Description  Returns the list of MCP server IDs in the API key's allowlist. An empty list means the key inherits from parent scope (no further restriction).
// @Tags         mcp-access
// @Produce      json
// @Param        org_id  path      string  true  "Organization ID"
// @Param        key_id  path      string  true  "API key ID"
// @Success      200     {object}  mcpAccessResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/keys/{key_id}/mcp-access [get]
func (h *Handler) GetKeyMCPAccess(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	keyID := c.Params("key_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	if err := h.requireKeyBelongsToOrg(c, keyID, orgID); err != nil {
		return err
	}

	servers, err := h.DB.GetKeyMCPAccess(c.Context(), keyID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "get key mcp access", slog.String("key_id", keyID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get key mcp access")
	}

	if servers == nil {
		servers = []string{}
	}
	return c.JSON(mcpAccessResponse{Servers: servers})
}

// SetKeyMCPAccess handles PUT /api/v1/orgs/:org_id/keys/:key_id/mcp-access.
// Replaces the API key's MCP server allowlist atomically.
// An empty servers array clears the list — the key inherits from parent scope (no further restriction).
//
// @Summary      Set key MCP access
// @Description  Atomically replaces the API key's MCP server allowlist. All servers must be allowed at the team and org level. Pass an empty array to inherit from parent scope (no further restriction).
// @Tags         mcp-access
// @Accept       json
// @Produce      json
// @Param        org_id  path      string            true  "Organization ID"
// @Param        key_id  path      string            true  "API key ID"
// @Param        body    body      mcpAccessRequest  true  "MCP server allowlist"
// @Success      200     {object}  mcpAccessResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/keys/{key_id}/mcp-access [put]
func (h *Handler) SetKeyMCPAccess(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	keyID := c.Params("key_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	if err := h.requireKeyBelongsToOrg(c, keyID, orgID); err != nil {
		return err
	}

	var req mcpAccessRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if err := h.validateMCPServerIDs(c, req.Servers); err != nil {
		return nil // response already written
	}

	if len(req.Servers) > 0 {
		if err := h.requireSubsetOfKeyEffectiveMCPServers(c, orgID, keyID, req.Servers); err != nil {
			return nil // response already written
		}
	}

	if err := h.DB.SetKeyMCPAccess(c.Context(), keyID, req.Servers); err != nil {
		h.Log.ErrorContext(c.Context(), "set key mcp access", slog.String("key_id", keyID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to set key mcp access")
	}

	h.refreshMCPAccessCache(c.Context())
	h.publishAccessInvalidation(c.Context())

	return c.JSON(mcpAccessResponse{Servers: req.Servers})
}

// availableMCPServer is the minimal, safe representation of a global MCP server
// returned by ListAvailableGlobalMCPServers. It intentionally omits URLs and
// auth configuration to prevent sensitive data exposure to org-admin callers.
type availableMCPServer struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Alias string `json:"alias"`
}

// ListAvailableGlobalMCPServers handles GET /api/v1/orgs/:org_id/available-mcp-servers.
// Returns a list of active global MCP servers (name, alias, ID only) that the
// org could be granted access to. This is a lightweight read-only endpoint for
// the MCP Access management UI — it does not expose server URLs or auth config.
func (h *Handler) ListAvailableGlobalMCPServers(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	servers, err := h.DB.ListMCPServers(c.Context())
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list available global mcp servers",
			slog.String("org_id", orgID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list available MCP servers")
	}

	result := make([]availableMCPServer, len(servers))
	for i, s := range servers {
		result[i] = availableMCPServer{
			ID:    s.ID,
			Name:  s.Name,
			Alias: s.Alias,
		}
	}
	return c.JSON(result)
}

// validateMCPServerIDs checks that each ID in servers references a known, active,
// global MCP server and that there are no duplicates. It writes a 400 or 500
// response and returns a non-nil error on the first violation found.
func (h *Handler) validateMCPServerIDs(c fiber.Ctx, serverIDs []string) error {
	seen := make(map[string]struct{}, len(serverIDs))
	for _, id := range serverIDs {
		if _, dup := seen[id]; dup {
			apierror.BadRequest(c, "duplicate server: "+id)
			return errMCPAccessResponseWritten
		}
		seen[id] = struct{}{}

		s, err := h.DB.GetMCPServer(c.Context(), id)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				apierror.BadRequest(c, "unknown MCP server: "+id)
				return errMCPAccessResponseWritten
			}
			h.Log.ErrorContext(c.Context(), "validate mcp server ids",
				slog.String("server_id", id), slog.String("error", err.Error()))
			apierror.InternalError(c, "failed to validate MCP server")
			return errMCPAccessResponseWritten
		}

		if s.OrgID != nil || s.TeamID != nil {
			apierror.BadRequest(c, "server is not global: "+id)
			return errMCPAccessResponseWritten
		}

		if !s.IsActive {
			apierror.BadRequest(c, "server is not active: "+id)
			return errMCPAccessResponseWritten
		}
	}
	return nil
}

// requireSubsetOfOrgMCPServers checks that all requested server IDs are permitted
// at the org level. If the org has no configured allowlist (empty), all non-empty
// requests are rejected because MCP access is closed by default at the org level.
// It writes a 400 response and returns a non-nil error on violation.
func (h *Handler) requireSubsetOfOrgMCPServers(c fiber.Ctx, orgID string, serverIDs []string) error {
	orgServers, err := h.DB.GetOrgMCPAccess(c.Context(), orgID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "set team mcp access: get org allowlist", slog.String("org_id", orgID), slog.String("error", err.Error()))
		apierror.InternalError(c, "failed to validate org mcp access")
		return errMCPAccessResponseWritten
	}

	if len(orgServers) == 0 {
		apierror.BadRequest(c, "organization has no MCP servers allowed")
		return errMCPAccessResponseWritten
	}

	for _, id := range serverIDs {
		if !slices.Contains(orgServers, id) {
			apierror.BadRequest(c, "server not allowed by org: "+id)
			return errMCPAccessResponseWritten
		}
	}
	return nil
}

// requireSubsetOfKeyEffectiveMCPServers validates that all server IDs requested
// for a key-level allowlist are permitted by the key's effective scope: the team
// allowlist (if the key belongs to a team) and the org allowlist. If the relevant
// parent allowlist is empty (unconfigured), the check is skipped for that level.
// It writes a 400 response and returns a non-nil error on violation.
func (h *Handler) requireSubsetOfKeyEffectiveMCPServers(c fiber.Ctx, orgID, keyID string, serverIDs []string) error {
	key, err := h.DB.GetAPIKey(c.Context(), keyID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			apierror.NotFound(c, "api key not found")
			return errMCPAccessResponseWritten
		}
		h.Log.ErrorContext(c.Context(), "set key mcp access: get key", slog.String("key_id", keyID), slog.String("error", err.Error()))
		apierror.InternalError(c, "failed to validate key mcp access")
		return errMCPAccessResponseWritten
	}

	if key.TeamID != nil && *key.TeamID != "" {
		teamServers, err := h.DB.GetTeamMCPAccess(c.Context(), *key.TeamID)
		if err != nil {
			h.Log.ErrorContext(c.Context(), "set key mcp access: get team allowlist", slog.String("team_id", *key.TeamID), slog.String("error", err.Error()))
			apierror.InternalError(c, "failed to validate team mcp access")
			return errMCPAccessResponseWritten
		}
		if len(teamServers) > 0 {
			for _, id := range serverIDs {
				if !slices.Contains(teamServers, id) {
					apierror.BadRequest(c, "server not allowed by team: "+id)
					return errMCPAccessResponseWritten
				}
			}
		}
	}

	return h.requireSubsetOfOrgMCPServers(c, orgID, serverIDs)
}
