package admin

import (
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
)

// createTeamRequest is the JSON body accepted by CreateTeam.
type createTeamRequest struct {
	Name              string `json:"name"`
	Slug              string `json:"slug"`
	DailyTokenLimit   int64  `json:"daily_token_limit"`
	MonthlyTokenLimit int64  `json:"monthly_token_limit"`
	RequestsPerMinute int    `json:"requests_per_minute"`
	RequestsPerDay    int    `json:"requests_per_day"`
}

// updateTeamRequest is the JSON body accepted by UpdateTeam.
// All fields are optional; a nil pointer means the field is left unchanged.
type updateTeamRequest struct {
	Name              *string `json:"name"`
	Slug              *string `json:"slug"`
	DailyTokenLimit   *int64  `json:"daily_token_limit"`
	MonthlyTokenLimit *int64  `json:"monthly_token_limit"`
	RequestsPerMinute *int    `json:"requests_per_minute"`
	RequestsPerDay    *int    `json:"requests_per_day"`
}

// teamResponse is the JSON representation of a team returned by the API.
type teamResponse struct {
	ID                string  `json:"id"`
	OrgID             string  `json:"org_id"`
	Name              string  `json:"name"`
	Slug              string  `json:"slug"`
	DailyTokenLimit   int64   `json:"daily_token_limit"`
	MonthlyTokenLimit int64   `json:"monthly_token_limit"`
	RequestsPerMinute int     `json:"requests_per_minute"`
	RequestsPerDay    int     `json:"requests_per_day"`
	MemberCount       int     `json:"member_count"`
	KeyCount          int     `json:"key_count"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
	DeletedAt         *string `json:"deleted_at,omitempty"`
}

// paginatedTeamsResponse wraps a page of teams with pagination metadata.
type paginatedTeamsResponse struct {
	Data    []teamResponse `json:"data"`
	HasMore bool           `json:"has_more"`
	Cursor  string         `json:"next_cursor,omitempty"`
}

// teamToResponse converts a db.Team to its API wire representation.
// memberCount and keyCount are provided by the caller and included as-is.
func teamToResponse(t *db.Team, memberCount, keyCount int) teamResponse {
	return teamResponse{
		ID:                t.ID,
		OrgID:             t.OrgID,
		Name:              t.Name,
		Slug:              t.Slug,
		DailyTokenLimit:   t.DailyTokenLimit,
		MonthlyTokenLimit: t.MonthlyTokenLimit,
		RequestsPerMinute: t.RequestsPerMinute,
		RequestsPerDay:    t.RequestsPerDay,
		MemberCount:       memberCount,
		KeyCount:          keyCount,
		CreatedAt:         t.CreatedAt,
		UpdatedAt:         t.UpdatedAt,
		DeletedAt:         t.DeletedAt,
	}
}

// teamWithCountsToResponse converts a db.TeamWithCounts to its API wire representation.
func teamWithCountsToResponse(t *db.TeamWithCounts) teamResponse {
	return teamToResponse(&t.Team, t.MemberCount, t.KeyCount)
}

// CreateTeam handles POST /api/v1/orgs/:org_id/teams.
// System admins and org admins of the target org may create teams.
//
// @Summary      Create a team
// @Description  Creates a new team within the organization. Requires org admin or higher.
// @Tags         teams
// @Accept       json
// @Produce      json
// @Param        org_id  path      string             true  "Organization ID"
// @Param        body    body      createTeamRequest  true  "Team parameters"
// @Success      201     {object}  teamResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      409     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams [post]
func (h *Handler) CreateTeam(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAdmin(c, orgID); !ok {
		return nil
	}

	var req createTeamRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.Name == "" {
		return apierror.BadRequest(c, "name is required")
	}
	if req.Slug == "" {
		return apierror.BadRequest(c, "slug is required")
	}
	if !slugRegex.MatchString(req.Slug) {
		return apierror.BadRequest(c, "slug must be lowercase alphanumeric with hyphens, 2-63 characters")
	}

	lic := h.License.Load()
	if lic.MaxTeams() > 0 {
		count, err := h.DB.CountTeams(c.Context(), orgID)
		if err != nil {
			h.Log.ErrorContext(c.Context(), "create team: count teams", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to check team limit")
		}
		if count >= lic.MaxTeams() {
			return apierror.Send(c, fiber.StatusForbidden, "limit_reached",
				"team limit reached for your plan")
		}
	}

	team, err := h.DB.CreateTeam(c.Context(), db.CreateTeamParams{
		OrgID:             orgID,
		Name:              req.Name,
		Slug:              req.Slug,
		DailyTokenLimit:   req.DailyTokenLimit,
		MonthlyTokenLimit: req.MonthlyTokenLimit,
		RequestsPerMinute: req.RequestsPerMinute,
		RequestsPerDay:    req.RequestsPerDay,
	})
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "team slug already exists in this organization")
		}
		h.Log.ErrorContext(c.Context(), "create team", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to create team")
	}

	return c.Status(fiber.StatusCreated).JSON(teamToResponse(team, 0, 0))
}

// GetTeam handles GET /api/v1/orgs/:org_id/teams/:team_id.
// System admins and org admins may fetch any team in the org.
// Team admins may only fetch teams they are a member of.
// Returns 404 if the team belongs to a different org or the caller lacks access.
//
// @Summary      Get a team
// @Description  Returns a single team with member and key counts. Team admins only see teams they belong to.
// @Tags         teams
// @Produce      json
// @Param        org_id   path      string  true  "Organization ID"
// @Param        team_id  path      string  true  "Team ID"
// @Success      200      {object}  teamResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id} [get]
func (h *Handler) GetTeam(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	teamID := c.Params("team_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	team, err := h.DB.GetTeamWithCounts(c.Context(), teamID)
	if err != nil {
		if !errors.Is(err, db.ErrNotFound) {
			h.Log.ErrorContext(c.Context(), "get team with counts", slog.String("error", err.Error()))
		}
		return apierror.NotFound(c, "team not found")
	}
	if team.OrgID != orgID {
		return apierror.NotFound(c, "team not found")
	}

	// Team admins may only view teams they belong to.
	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		isMember, err := h.DB.IsTeamMember(c.Context(), keyInfo.UserID, teamID)
		if err != nil {
			h.Log.ErrorContext(c.Context(), "get team: check membership", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to verify team membership")
		}
		if !isMember {
			return apierror.NotFound(c, "team not found")
		}
	}

	return c.JSON(teamWithCountsToResponse(team))
}

// ListTeams handles GET /api/v1/orgs/:org_id/teams.
// System admins and org admins see all teams in the org.
// Team admins see only teams they are a member of (no pagination cursor support for
// the membership-filtered path, as membership sets are expected to be small).
// Accepts query parameters: limit (int, default 20, max 100), cursor (UUIDv7 string),
// and include_deleted=true (system admin only).
//
// @Summary      List teams
// @Description  Returns a cursor-paginated list of teams. Org admins see all teams; others see only teams they belong to.
// @Tags         teams
// @Produce      json
// @Param        org_id           path      string  true   "Organization ID"
// @Param        limit            query     int     false  "Page size (default 20, max 100)"
// @Param        cursor           query     string  false  "Pagination cursor (UUIDv7 of the last seen team)"
// @Param        include_deleted  query     bool    false  "Include soft-deleted teams (system admin only)"
// @Success      200              {object}  paginatedTeamsResponse
// @Failure      400              {object}  swaggerErrorResponse
// @Failure      401              {object}  swaggerErrorResponse
// @Failure      403              {object}  swaggerErrorResponse
// @Failure      500              {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams [get]
func (h *Handler) ListTeams(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	// Team admins see only their own teams.
	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		teams, err := h.DB.ListUserTeams(c.Context(), orgID, keyInfo.UserID)
		if err != nil {
			h.Log.ErrorContext(c.Context(), "list teams for user", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to list teams")
		}
		resp := paginatedTeamsResponse{
			Data:    make([]teamResponse, len(teams)),
			HasMore: false,
		}
		for i := range teams {
			resp.Data[i] = teamWithCountsToResponse(&teams[i])
		}
		return c.JSON(resp)
	}

	p, err := parsePagination(c)
	if err != nil {
		return apierror.BadRequest(c, err.Error())
	}
	includeDeleted := c.Query("include_deleted") == "true" && auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin)

	teams, err := h.DB.ListTeamsWithCounts(c.Context(), orgID, p.Cursor, p.Limit+1, includeDeleted)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list teams", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list teams")
	}

	hasMore := len(teams) > p.Limit
	if hasMore {
		teams = teams[:p.Limit]
	}

	resp := paginatedTeamsResponse{
		Data:    make([]teamResponse, len(teams)),
		HasMore: hasMore,
	}
	for i := range teams {
		resp.Data[i] = teamWithCountsToResponse(&teams[i])
	}
	if hasMore && len(teams) > 0 {
		resp.Cursor = teams[len(teams)-1].ID
	}
	return c.JSON(resp)
}

// UpdateTeam handles PATCH /api/v1/orgs/:org_id/teams/:team_id.
// System admins and org admins of the target org may update teams.
// Returns 404 if the team belongs to a different org (cross-org protection).
//
// @Summary      Update a team
// @Description  Updates team name, slug, or limits. Only provided fields are changed.
// @Tags         teams
// @Accept       json
// @Produce      json
// @Param        org_id   path      string             true  "Organization ID"
// @Param        team_id  path      string             true  "Team ID"
// @Param        body     body      updateTeamRequest  true  "Fields to update"
// @Success      200      {object}  teamResponse
// @Failure      400      {object}  swaggerErrorResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      409      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id} [patch]
func (h *Handler) UpdateTeam(c fiber.Ctx) error {
	_, existing, ok := h.requireTeamAccess(c)
	if !ok {
		return nil
	}

	var req updateTeamRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.Slug != nil && !slugRegex.MatchString(*req.Slug) {
		return apierror.BadRequest(c, "slug must be lowercase alphanumeric with hyphens, 2-63 characters")
	}

	if _, err := h.DB.UpdateTeam(c.Context(), existing.ID, db.UpdateTeamParams{
		Name:              req.Name,
		Slug:              req.Slug,
		DailyTokenLimit:   req.DailyTokenLimit,
		MonthlyTokenLimit: req.MonthlyTokenLimit,
		RequestsPerMinute: req.RequestsPerMinute,
		RequestsPerDay:    req.RequestsPerDay,
	}); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "team not found")
		}
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "team slug already exists in this organization")
		}
		h.Log.ErrorContext(c.Context(), "update team", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to update team")
	}

	// Invalidate key cache so updated limits take effect immediately.
	if h.Redis != nil {
		if pubErr := h.Redis.PublishInvalidation(c.Context(), voidredis.ChannelKeys, "team:"+existing.ID); pubErr != nil {
			h.Log.ErrorContext(c.Context(), "publish key cache invalidation", slog.String("error", pubErr.Error()))
		}
	}

	team, err := h.DB.GetTeamWithCounts(c.Context(), existing.ID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "get team with counts after update", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to retrieve updated team")
	}
	return c.JSON(teamWithCountsToResponse(team))
}

// DeleteTeam handles DELETE /api/v1/orgs/:org_id/teams/:team_id.
// System admins and org admins of the target org may delete teams. Deletion is a soft-delete.
// Returns 404 if the team belongs to a different org (cross-org protection).
//
// @Summary      Delete a team
// @Description  Soft-deletes the team. Requires org admin or higher.
// @Tags         teams
// @Produce      json
// @Param        org_id   path  string  true  "Organization ID"
// @Param        team_id  path  string  true  "Team ID"
// @Success      204      "No Content"
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id} [delete]
func (h *Handler) DeleteTeam(c fiber.Ctx) error {
	_, team, ok := h.requireTeamAccess(c)
	if !ok {
		return nil
	}

	if err := h.DB.DeleteTeam(c.Context(), team.ID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "team not found")
		}
		h.Log.ErrorContext(c.Context(), "delete team", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to delete team")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
