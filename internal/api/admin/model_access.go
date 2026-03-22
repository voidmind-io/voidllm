package admin

import (
	"context"
	"errors"
	"log/slog"
	"slices"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
)

// modelAccessRequest is the JSON body accepted by the Set*ModelAccess handlers.
type modelAccessRequest struct {
	Models []string `json:"models"`
}

// modelAccessResponse is the JSON body returned by all model access handlers.
type modelAccessResponse struct {
	Models []string `json:"models"`
}

// GetOrgModelAccess handles GET /api/v1/orgs/:org_id/model-access.
// Returns the list of model names allowed for the org.
// An empty list means "not configured" — all models are allowed.
//
// @Summary      Get org model access
// @Description  Returns the list of model names in the org's allowlist. An empty list means all models are permitted.
// @Tags         model-access
// @Produce      json
// @Param        org_id  path      string  true  "Organization ID"
// @Success      200     {object}  modelAccessResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/model-access [get]
func (h *Handler) GetOrgModelAccess(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	models, err := h.DB.GetOrgModelAccess(c.Context(), orgID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "get org model access", slog.String("org_id", orgID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get org model access")
	}

	if models == nil {
		models = []string{}
	}
	return c.JSON(modelAccessResponse{Models: models})
}

// SetOrgModelAccess handles PUT /api/v1/orgs/:org_id/model-access.
// Replaces the org's model allowlist atomically.
// An empty models array clears the list (all models allowed).
//
// @Summary      Set org model access
// @Description  Atomically replaces the org's model allowlist. Pass an empty array to allow all models.
// @Tags         model-access
// @Accept       json
// @Produce      json
// @Param        org_id  path      string               true  "Organization ID"
// @Param        body    body      modelAccessRequest   true  "Model allowlist"
// @Success      200     {object}  modelAccessResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/model-access [put]
func (h *Handler) SetOrgModelAccess(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	var req modelAccessRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if err := h.validateModelNames(c, req.Models); err != nil {
		return err
	}

	if err := h.DB.SetOrgModelAccess(c.Context(), orgID, req.Models); err != nil {
		h.Log.ErrorContext(c.Context(), "set org model access", slog.String("org_id", orgID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to set org model access")
	}

	h.refreshAccessCache(c.Context())
	h.publishAccessInvalidation(c.Context())

	//nolint:gosimple // S1016: fields are intentionally mapped explicitly for clarity
	return c.JSON(modelAccessResponse{Models: req.Models})
}

// GetTeamModelAccess handles GET /api/v1/orgs/:org_id/teams/:team_id/model-access.
// Returns the list of model names allowed for the team.
// An empty list means "not configured" — all org-allowed models are allowed.
// Team admins may only access teams they are a member of.
//
// @Summary      Get team model access
// @Description  Returns the list of model names in the team's allowlist. An empty list means all org-allowed models are permitted.
// @Tags         model-access
// @Produce      json
// @Param        org_id   path      string  true  "Organization ID"
// @Param        team_id  path      string  true  "Team ID"
// @Success      200      {object}  modelAccessResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/model-access [get]
func (h *Handler) GetTeamModelAccess(c fiber.Ctx) error {
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
			h.Log.ErrorContext(c.Context(), "get team model access: check membership", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to verify team membership")
		}
		if !isMember {
			return apierror.NotFound(c, "team not found")
		}
	}

	models, err := h.DB.GetTeamModelAccess(c.Context(), teamID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "get team model access", slog.String("team_id", teamID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get team model access")
	}

	if models == nil {
		models = []string{}
	}
	return c.JSON(modelAccessResponse{Models: models})
}

// SetTeamModelAccess handles PUT /api/v1/orgs/:org_id/teams/:team_id/model-access.
// Replaces the team's model allowlist atomically.
// All requested models must be present in the org's allowlist (if the org has one configured).
// An empty models array clears the list (all org-allowed models are allowed).
// Team admins may only manage access for teams they are a member of.
//
// @Summary      Set team model access
// @Description  Atomically replaces the team's model allowlist. All models must be allowed at the org level. Pass an empty array to clear.
// @Tags         model-access
// @Accept       json
// @Produce      json
// @Param        org_id   path      string               true  "Organization ID"
// @Param        team_id  path      string               true  "Team ID"
// @Param        body     body      modelAccessRequest   true  "Model allowlist"
// @Success      200      {object}  modelAccessResponse
// @Failure      400      {object}  swaggerErrorResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/model-access [put]
func (h *Handler) SetTeamModelAccess(c fiber.Ctx) error {
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
			h.Log.ErrorContext(c.Context(), "set team model access: check membership", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to verify team membership")
		}
		if !isMember {
			return apierror.NotFound(c, "team not found")
		}
	}

	var req modelAccessRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if err := h.validateModelNames(c, req.Models); err != nil {
		return err
	}

	if len(req.Models) > 0 {
		if err := h.requireSubsetOfOrgModels(c, orgID, req.Models); err != nil {
			return err
		}
	}

	if err := h.DB.SetTeamModelAccess(c.Context(), teamID, req.Models); err != nil {
		h.Log.ErrorContext(c.Context(), "set team model access", slog.String("team_id", teamID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to set team model access")
	}

	h.refreshAccessCache(c.Context())
	h.publishAccessInvalidation(c.Context())

	//nolint:gosimple // S1016: fields are intentionally mapped explicitly for clarity
	return c.JSON(modelAccessResponse{Models: req.Models})
}

// GetKeyModelAccess handles GET /api/v1/orgs/:org_id/keys/:key_id/model-access.
// Returns the list of model names allowed for the API key.
// An empty list means "not configured" — all team-allowed models are allowed.
//
// @Summary      Get key model access
// @Description  Returns the list of model names in the API key's allowlist. An empty list means all team-allowed models are permitted.
// @Tags         model-access
// @Produce      json
// @Param        org_id  path      string  true  "Organization ID"
// @Param        key_id  path      string  true  "API key ID"
// @Success      200     {object}  modelAccessResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/keys/{key_id}/model-access [get]
func (h *Handler) GetKeyModelAccess(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	keyID := c.Params("key_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	if err := h.requireKeyBelongsToOrg(c, keyID, orgID); err != nil {
		return err
	}

	models, err := h.DB.GetKeyModelAccess(c.Context(), keyID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "get key model access", slog.String("key_id", keyID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get key model access")
	}

	if models == nil {
		models = []string{}
	}
	return c.JSON(modelAccessResponse{Models: models})
}

// SetKeyModelAccess handles PUT /api/v1/orgs/:org_id/keys/:key_id/model-access.
// Replaces the API key's model allowlist atomically.
// An empty models array clears the list (all team-allowed models are allowed).
//
// @Summary      Set key model access
// @Description  Atomically replaces the API key's model allowlist. All models must be allowed at the team and org level. Pass an empty array to clear.
// @Tags         model-access
// @Accept       json
// @Produce      json
// @Param        org_id  path      string               true  "Organization ID"
// @Param        key_id  path      string               true  "API key ID"
// @Param        body    body      modelAccessRequest   true  "Model allowlist"
// @Success      200     {object}  modelAccessResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/keys/{key_id}/model-access [put]
func (h *Handler) SetKeyModelAccess(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	keyID := c.Params("key_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	if err := h.requireKeyBelongsToOrg(c, keyID, orgID); err != nil {
		return err
	}

	var req modelAccessRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if err := h.validateModelNames(c, req.Models); err != nil {
		return err
	}

	if len(req.Models) > 0 {
		if err := h.requireSubsetOfKeyEffectiveModels(c, orgID, keyID, req.Models); err != nil {
			return err
		}
	}

	if err := h.DB.SetKeyModelAccess(c.Context(), keyID, req.Models); err != nil {
		h.Log.ErrorContext(c.Context(), "set key model access", slog.String("key_id", keyID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to set key model access")
	}

	h.refreshAccessCache(c.Context())
	h.publishAccessInvalidation(c.Context())

	//nolint:gosimple // S1016: fields are intentionally mapped explicitly for clarity
	return c.JSON(modelAccessResponse{Models: req.Models})
}

// validateModelNames checks that each name in models references a known model
// in the registry and that there are no duplicates. It writes a 400 response
// and returns a non-nil error on the first violation found.
func (h *Handler) validateModelNames(c fiber.Ctx, models []string) error {
	seen := make(map[string]struct{}, len(models))
	for _, name := range models {
		if _, dup := seen[name]; dup {
			return apierror.BadRequest(c, "duplicate model: "+name)
		}
		seen[name] = struct{}{}
		if _, err := h.Registry.Resolve(name); err != nil {
			return apierror.BadRequest(c, "unknown model: "+name)
		}
	}
	return nil
}

// requireSubsetOfOrgModels checks that all requested models are permitted at the org level.
// If the org has no configured allowlist the check is skipped (empty = all allowed).
// It writes a 400 response and returns a non-nil error on violation.
func (h *Handler) requireSubsetOfOrgModels(c fiber.Ctx, orgID string, models []string) error {
	orgModels, err := h.DB.GetOrgModelAccess(c.Context(), orgID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "set team model access: get org allowlist", slog.String("org_id", orgID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to validate org model access")
	}

	if len(orgModels) == 0 {
		return nil
	}

	for _, name := range models {
		if !slices.Contains(orgModels, name) {
			return apierror.BadRequest(c, "model not allowed by org: "+name)
		}
	}
	return nil
}

// requireSubsetOfKeyEffectiveModels validates that all models requested for a
// key-level allowlist are permitted by the key's effective scope: the team
// allowlist (if the key belongs to a team) and the org allowlist. If the
// relevant parent allowlist is empty (unconfigured), the check is skipped.
// It writes a 400 response and returns a non-nil error on violation.
func (h *Handler) requireSubsetOfKeyEffectiveModels(c fiber.Ctx, orgID, keyID string, models []string) error {
	key, err := h.DB.GetAPIKey(c.Context(), keyID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "api key not found")
		}
		h.Log.ErrorContext(c.Context(), "set key model access: get key", slog.String("key_id", keyID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to validate key model access")
	}

	if key.TeamID != nil && *key.TeamID != "" {
		teamModels, err := h.DB.GetTeamModelAccess(c.Context(), *key.TeamID)
		if err != nil {
			h.Log.ErrorContext(c.Context(), "set key model access: get team allowlist", slog.String("team_id", *key.TeamID), slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to validate team model access")
		}
		if len(teamModels) > 0 {
			for _, name := range models {
				if !slices.Contains(teamModels, name) {
					return apierror.BadRequest(c, "model not allowed by team: "+name)
				}
			}
		}
	}

	return h.requireSubsetOfOrgModels(c, orgID, models)
}

// requireTeamBelongsToOrg fetches the team and confirms it belongs to orgID.
// It writes a 404 response and returns a non-nil error if the team does not exist
// or belongs to a different org.
func (h *Handler) requireTeamBelongsToOrg(c fiber.Ctx, teamID, orgID string) error {
	team, err := h.DB.GetTeam(c.Context(), teamID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "team not found")
		}
		h.Log.ErrorContext(c.Context(), "validate team ownership", slog.String("team_id", teamID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to validate team")
	}
	if team.OrgID != orgID {
		return apierror.NotFound(c, "team not found")
	}
	return nil
}

// requireKeyBelongsToOrg fetches the API key and confirms it belongs to orgID.
// It writes a 404 response and returns a non-nil error if the key does not exist
// or belongs to a different org.
func (h *Handler) requireKeyBelongsToOrg(c fiber.Ctx, keyID, orgID string) error {
	key, err := h.DB.GetAPIKey(c.Context(), keyID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "api key not found")
		}
		h.Log.ErrorContext(c.Context(), "validate api key ownership", slog.String("key_id", keyID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to validate api key")
	}
	if key.OrgID != orgID {
		return apierror.NotFound(c, "api key not found")
	}
	return nil
}

// publishAccessInvalidation sends a cache invalidation message on the access
// channel so that other instances can reload their model access cache from the
// database. It is a no-op when Redis is not configured. Errors are logged as
// warnings and do not affect the response.
func (h *Handler) publishAccessInvalidation(ctx context.Context) {
	if h.Redis == nil {
		return
	}
	if err := h.Redis.PublishInvalidation(ctx, voidredis.ChannelAccess, "reload"); err != nil {
		h.Log.LogAttrs(ctx, slog.LevelWarn, "redis: publish access invalidation failed",
			slog.String("error", err.Error()),
		)
	}
}
