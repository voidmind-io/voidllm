package admin

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/db"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
)

// createModelAliasRequest is the JSON body accepted by the Create*Alias handlers.
type createModelAliasRequest struct {
	Alias     string `json:"alias"`
	ModelName string `json:"model_name"`
}

// modelAliasResponse is the JSON body returned by all model alias handlers.
type modelAliasResponse struct {
	ID        string  `json:"id"`
	Alias     string  `json:"alias"`
	ModelName string  `json:"model_name"`
	ScopeType string  `json:"scope_type"`
	OrgID     string  `json:"org_id"`
	TeamID    *string `json:"team_id,omitempty"`
	CreatedBy string  `json:"created_by"`
	CreatedAt string  `json:"created_at"`
}

// modelAliasToResponse converts a db.ModelAlias to its API wire representation.
func modelAliasToResponse(a *db.ModelAlias) modelAliasResponse {
	return modelAliasResponse{
		ID:        a.ID,
		Alias:     a.Alias,
		ModelName: a.ModelName,
		ScopeType: a.ScopeType,
		OrgID:     a.OrgID,
		TeamID:    a.TeamID,
		CreatedBy: a.CreatedBy,
		CreatedAt: a.CreatedAt,
	}
}

// validateAliasName checks that name is non-empty, at most 128 characters, and
// contains only letters, digits, hyphens, and underscores. It returns an error
// message suitable for a 400 response, or an empty string when the name is valid.
func validateAliasName(name string) string {
	if name == "" {
		return "alias is required"
	}
	if len(name) > 128 {
		return "alias must be at most 128 characters"
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_') {
			return "alias may only contain letters, numbers, hyphens, and underscores"
		}
	}
	return ""
}

// CreateOrgAlias handles POST /api/v1/orgs/:org_id/model-aliases.
// Creates a model alias scoped to the org. The alias must not be empty, the
// target model_name must exist in the registry, and the alias must not collide
// with a canonical model name.
//
// @Summary      Create an org model alias
// @Description  Creates a model alias scoped to the org. The alias must not conflict with any canonical model name.
// @Tags         model-aliases
// @Accept       json
// @Produce      json
// @Param        org_id  path      string                    true  "Organization ID"
// @Param        body    body      createModelAliasRequest   true  "Alias parameters"
// @Success      201     {object}  modelAliasResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      409     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/model-aliases [post]
func (h *Handler) CreateOrgAlias(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	var req createModelAliasRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	aliasName := strings.TrimSpace(req.Alias)
	if msg := validateAliasName(aliasName); msg != "" {
		return apierror.BadRequest(c, msg)
	}
	if req.ModelName == "" {
		return apierror.BadRequest(c, "model_name is required")
	}

	if _, err := h.Registry.Resolve(req.ModelName); err != nil {
		return apierror.BadRequest(c, "unknown model: "+req.ModelName)
	}

	if _, err := h.Registry.Resolve(aliasName); err == nil {
		return apierror.BadRequest(c, "alias conflicts with model name")
	}

	params := db.CreateModelAliasParams{
		Alias:     aliasName,
		ModelName: req.ModelName,
		ScopeType: "org",
		OrgID:     orgID,
		CreatedBy: keyInfo.ID,
	}
	alias, err := h.DB.CreateModelAlias(c.Context(), params)
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "alias already exists in this scope")
		}
		h.Log.ErrorContext(c.Context(), "create org model alias",
			slog.String("org_id", orgID),
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to create model alias")
	}

	h.refreshAliasCache(c.Context())
	h.publishAliasInvalidation(c.Context())

	return c.Status(fiber.StatusCreated).JSON(modelAliasToResponse(alias))
}

// ListOrgAliases handles GET /api/v1/orgs/:org_id/model-aliases.
// Returns all model aliases scoped to the org.
//
// @Summary      List org model aliases
// @Description  Returns all model aliases defined at the org scope.
// @Tags         model-aliases
// @Produce      json
// @Param        org_id  path      string  true  "Organization ID"
// @Success      200     {array}   modelAliasResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/model-aliases [get]
func (h *Handler) ListOrgAliases(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	aliases, err := h.DB.ListModelAliases(c.Context(), "org", orgID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list org model aliases",
			slog.String("org_id", orgID),
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to list model aliases")
	}

	resp := make([]modelAliasResponse, len(aliases))
	for i := range aliases {
		resp[i] = modelAliasToResponse(&aliases[i])
	}
	return c.JSON(resp)
}

// DeleteOrgAlias handles DELETE /api/v1/orgs/:org_id/model-aliases/:alias_id.
// Hard-deletes the model alias from the org scope.
//
// @Summary      Delete an org model alias
// @Description  Permanently removes the model alias from the org scope.
// @Tags         model-aliases
// @Produce      json
// @Param        org_id    path  string  true  "Organization ID"
// @Param        alias_id  path  string  true  "Alias ID"
// @Success      204       "No Content"
// @Failure      401       {object}  swaggerErrorResponse
// @Failure      403       {object}  swaggerErrorResponse
// @Failure      404       {object}  swaggerErrorResponse
// @Failure      500       {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/model-aliases/{alias_id} [delete]
func (h *Handler) DeleteOrgAlias(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	aliasID := c.Params("alias_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	if err := h.DB.DeleteModelAlias(c.Context(), aliasID, "org", orgID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model alias not found")
		}
		h.Log.ErrorContext(c.Context(), "delete org model alias",
			slog.String("org_id", orgID),
			slog.String("alias_id", aliasID),
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to delete model alias")
	}

	h.refreshAliasCache(c.Context())
	h.publishAliasInvalidation(c.Context())

	return c.SendStatus(fiber.StatusNoContent)
}

// CreateTeamAlias handles POST /api/v1/orgs/:org_id/teams/:team_id/model-aliases.
// Creates a model alias scoped to the team. The alias must not be empty, the
// target model_name must exist in the registry, and the alias must not collide
// with a canonical model name. The team must belong to the org.
//
// @Summary      Create a team model alias
// @Description  Creates a model alias scoped to the team. The alias must not conflict with any canonical model name.
// @Tags         model-aliases
// @Accept       json
// @Produce      json
// @Param        org_id   path      string                    true  "Organization ID"
// @Param        team_id  path      string                    true  "Team ID"
// @Param        body     body      createModelAliasRequest   true  "Alias parameters"
// @Success      201      {object}  modelAliasResponse
// @Failure      400      {object}  swaggerErrorResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      409      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/model-aliases [post]
func (h *Handler) CreateTeamAlias(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	teamID := c.Params("team_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	if err := h.requireTeamBelongsToOrg(c, teamID, orgID); err != nil {
		return err
	}

	var req createModelAliasRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	aliasName := strings.TrimSpace(req.Alias)
	if msg := validateAliasName(aliasName); msg != "" {
		return apierror.BadRequest(c, msg)
	}
	if req.ModelName == "" {
		return apierror.BadRequest(c, "model_name is required")
	}

	if _, err := h.Registry.Resolve(req.ModelName); err != nil {
		return apierror.BadRequest(c, "unknown model: "+req.ModelName)
	}

	if _, err := h.Registry.Resolve(aliasName); err == nil {
		return apierror.BadRequest(c, "alias conflicts with model name")
	}

	params := db.CreateModelAliasParams{
		Alias:     aliasName,
		ModelName: req.ModelName,
		ScopeType: "team",
		OrgID:     orgID,
		TeamID:    &teamID,
		CreatedBy: keyInfo.ID,
	}
	alias, err := h.DB.CreateModelAlias(c.Context(), params)
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "alias already exists in this scope")
		}
		h.Log.ErrorContext(c.Context(), "create team model alias",
			slog.String("org_id", orgID),
			slog.String("team_id", teamID),
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to create model alias")
	}

	h.refreshAliasCache(c.Context())
	h.publishAliasInvalidation(c.Context())

	return c.Status(fiber.StatusCreated).JSON(modelAliasToResponse(alias))
}

// ListTeamAliases handles GET /api/v1/orgs/:org_id/teams/:team_id/model-aliases.
// Returns all model aliases scoped to the team.
//
// @Summary      List team model aliases
// @Description  Returns all model aliases defined at the team scope.
// @Tags         model-aliases
// @Produce      json
// @Param        org_id   path      string  true  "Organization ID"
// @Param        team_id  path      string  true  "Team ID"
// @Success      200      {array}   modelAliasResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/model-aliases [get]
func (h *Handler) ListTeamAliases(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	teamID := c.Params("team_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	if err := h.requireTeamBelongsToOrg(c, teamID, orgID); err != nil {
		return err
	}

	aliases, err := h.DB.ListModelAliases(c.Context(), "team", teamID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list team model aliases",
			slog.String("org_id", orgID),
			slog.String("team_id", teamID),
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to list model aliases")
	}

	resp := make([]modelAliasResponse, len(aliases))
	for i := range aliases {
		resp[i] = modelAliasToResponse(&aliases[i])
	}
	return c.JSON(resp)
}

// DeleteTeamAlias handles DELETE /api/v1/orgs/:org_id/teams/:team_id/model-aliases/:alias_id.
// Hard-deletes the model alias from the team scope.
//
// @Summary      Delete a team model alias
// @Description  Permanently removes the model alias from the team scope.
// @Tags         model-aliases
// @Produce      json
// @Param        org_id   path  string  true  "Organization ID"
// @Param        team_id  path  string  true  "Team ID"
// @Param        alias_id  path  string  true  "Alias ID"
// @Success      204       "No Content"
// @Failure      401       {object}  swaggerErrorResponse
// @Failure      403       {object}  swaggerErrorResponse
// @Failure      404       {object}  swaggerErrorResponse
// @Failure      500       {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/model-aliases/{alias_id} [delete]
func (h *Handler) DeleteTeamAlias(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	teamID := c.Params("team_id")
	aliasID := c.Params("alias_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	if err := h.requireTeamBelongsToOrg(c, teamID, orgID); err != nil {
		return err
	}

	if err := h.DB.DeleteModelAlias(c.Context(), aliasID, "team", teamID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "model alias not found")
		}
		h.Log.ErrorContext(c.Context(), "delete team model alias",
			slog.String("org_id", orgID),
			slog.String("team_id", teamID),
			slog.String("alias_id", aliasID),
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to delete model alias")
	}

	h.refreshAliasCache(c.Context())
	h.publishAliasInvalidation(c.Context())

	return c.SendStatus(fiber.StatusNoContent)
}

// refreshAliasCache reloads all model aliases from the database into the
// in-memory alias cache. It is called after any Create or Delete alias
// mutation so that the hot path immediately reflects the updated configuration.
// If AliasCache is nil the call is a no-op.
func (h *Handler) refreshAliasCache(ctx context.Context) {
	if h.AliasCache == nil {
		return
	}
	orgA, teamA, err := h.DB.LoadAllModelAliases(ctx)
	if err != nil {
		h.Log.ErrorContext(ctx, "refresh alias cache", slog.String("error", err.Error()))
		return
	}
	h.AliasCache.Load(orgA, teamA)
}

// publishAliasInvalidation sends a cache invalidation message on the aliases
// channel so that other instances can reload their alias cache from the
// database. It is a no-op when Redis is not configured. Errors are logged as
// warnings and do not affect the response.
func (h *Handler) publishAliasInvalidation(ctx context.Context) {
	if h.Redis == nil {
		return
	}
	if err := h.Redis.PublishInvalidation(ctx, voidredis.ChannelAliases, "reload"); err != nil {
		h.Log.LogAttrs(ctx, slog.LevelWarn, "redis: publish alias invalidation failed",
			slog.String("error", err.Error()),
		)
	}
}
