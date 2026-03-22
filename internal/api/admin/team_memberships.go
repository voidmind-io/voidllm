package admin

import (
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// validTeamMembershipRoles is the set of roles that may be assigned to a team membership.
var validTeamMembershipRoles = map[string]bool{
	auth.RoleTeamAdmin: true,
	auth.RoleMember:    true,
}

// createTeamMembershipRequest is the JSON body accepted by CreateTeamMembership.
type createTeamMembershipRequest struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

// updateTeamMembershipRequest is the JSON body accepted by UpdateTeamMembership.
// All fields are optional; a nil pointer means the field is left unchanged.
type updateTeamMembershipRequest struct {
	Role *string `json:"role"`
}

// teamMembershipResponse is the JSON representation of a team membership returned by the API.
type teamMembershipResponse struct {
	ID        string `json:"id"`
	TeamID    string `json:"team_id"`
	UserID    string `json:"user_id"`
	Role      string `json:"role"`
	CreatedAt string `json:"created_at"`
}

// paginatedTeamMembershipsResponse wraps a page of team memberships with pagination metadata.
type paginatedTeamMembershipsResponse struct {
	Data    []teamMembershipResponse `json:"data"`
	HasMore bool                     `json:"has_more"`
	Cursor  string                   `json:"next_cursor,omitempty"`
}

// teamMembershipToResponse converts a db.TeamMembership to its API wire representation.
func teamMembershipToResponse(m *db.TeamMembership) teamMembershipResponse {
	return teamMembershipResponse{
		ID:        m.ID,
		TeamID:    m.TeamID,
		UserID:    m.UserID,
		Role:      m.Role,
		CreatedAt: m.CreatedAt,
	}
}

// CreateTeamMembership handles POST /api/v1/orgs/:org_id/teams/:team_id/members.
// System admins and org admins of the target org may add members to the team.
//
// @Summary      Add a team member
// @Description  Adds a user to the team with the specified role (team_admin or member).
// @Tags         team-members
// @Accept       json
// @Produce      json
// @Param        org_id   path      string                        true  "Organization ID"
// @Param        team_id  path      string                        true  "Team ID"
// @Param        body     body      createTeamMembershipRequest   true  "Membership parameters"
// @Success      201      {object}  teamMembershipResponse
// @Failure      400      {object}  swaggerErrorResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      409      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/members [post]
func (h *Handler) CreateTeamMembership(c fiber.Ctx) error {
	_, team, ok := h.requireTeamAccess(c)
	if !ok {
		return nil
	}

	var req createTeamMembershipRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.UserID == "" {
		return apierror.BadRequest(c, "user_id is required")
	}
	if req.Role == "" {
		return apierror.BadRequest(c, "role is required")
	}
	if !validTeamMembershipRoles[req.Role] {
		return apierror.BadRequest(c, "role must be \"team_admin\" or \"member\"")
	}

	m, err := h.DB.CreateTeamMembership(c.Context(), db.CreateTeamMembershipParams{
		TeamID: team.ID,
		UserID: req.UserID,
		Role:   req.Role,
	})
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "user is already a member of this team")
		}
		h.Log.ErrorContext(c.Context(), "create team membership", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to create team membership")
	}

	return c.Status(fiber.StatusCreated).JSON(teamMembershipToResponse(m))
}

// ListTeamMemberships handles GET /api/v1/orgs/:org_id/teams/:team_id/members.
// System admins and org admins of the target org may list team memberships.
//
// @Summary      List team members
// @Description  Returns a cursor-paginated list of team memberships.
// @Tags         team-members
// @Produce      json
// @Param        org_id   path      string  true   "Organization ID"
// @Param        team_id  path      string  true   "Team ID"
// @Param        limit    query     int     false  "Page size (default 20, max 100)"
// @Param        cursor   query     string  false  "Pagination cursor (UUIDv7 of the last seen membership)"
// @Success      200      {object}  paginatedTeamMembershipsResponse
// @Failure      400      {object}  swaggerErrorResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/members [get]
func (h *Handler) ListTeamMemberships(c fiber.Ctx) error {
	_, team, ok := h.requireTeamAccess(c)
	if !ok {
		return nil
	}

	p, err := parsePagination(c)
	if err != nil {
		return apierror.BadRequest(c, err.Error())
	}

	memberships, err := h.DB.ListTeamMemberships(c.Context(), team.ID, p.Cursor, p.Limit+1)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list team memberships", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list team memberships")
	}

	hasMore := len(memberships) > p.Limit
	if hasMore {
		memberships = memberships[:p.Limit]
	}

	resp := paginatedTeamMembershipsResponse{
		Data:    make([]teamMembershipResponse, len(memberships)),
		HasMore: hasMore,
	}
	for i := range memberships {
		resp.Data[i] = teamMembershipToResponse(&memberships[i])
	}
	if hasMore && len(memberships) > 0 {
		resp.Cursor = memberships[len(memberships)-1].ID
	}
	return c.JSON(resp)
}

// UpdateTeamMembership handles PATCH /api/v1/orgs/:org_id/teams/:team_id/members/:membership_id.
// System admins and org admins of the target org may update team membership roles.
//
// @Summary      Update a team membership
// @Description  Changes the role of a team membership (team_admin or member).
// @Tags         team-members
// @Accept       json
// @Produce      json
// @Param        org_id         path      string                        true  "Organization ID"
// @Param        team_id        path      string                        true  "Team ID"
// @Param        membership_id  path      string                        true  "Membership ID"
// @Param        body           body      updateTeamMembershipRequest   true  "Fields to update"
// @Success      200            {object}  teamMembershipResponse
// @Failure      400            {object}  swaggerErrorResponse
// @Failure      401            {object}  swaggerErrorResponse
// @Failure      403            {object}  swaggerErrorResponse
// @Failure      404            {object}  swaggerErrorResponse
// @Failure      500            {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/members/{membership_id} [patch]
func (h *Handler) UpdateTeamMembership(c fiber.Ctx) error {
	membershipID := c.Params("membership_id")

	_, team, ok := h.requireTeamAccess(c)
	if !ok {
		return nil
	}

	existing, err := h.DB.GetTeamMembership(c.Context(), membershipID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "team membership not found")
		}
		h.Log.ErrorContext(c.Context(), "update team membership: get", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get team membership")
	}
	if existing.TeamID != team.ID {
		return apierror.NotFound(c, "team membership not found")
	}

	var req updateTeamMembershipRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.Role != nil && !validTeamMembershipRoles[*req.Role] {
		return apierror.BadRequest(c, "role must be \"team_admin\" or \"member\"")
	}

	m, err := h.DB.UpdateTeamMembership(c.Context(), membershipID, db.UpdateTeamMembershipParams{
		Role: req.Role,
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "team membership not found")
		}
		h.Log.ErrorContext(c.Context(), "update team membership", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to update team membership")
	}
	return c.JSON(teamMembershipToResponse(m))
}

// DeleteTeamMembership handles DELETE /api/v1/orgs/:org_id/teams/:team_id/members/:membership_id.
// System admins and org admins of the target org may remove members from the team.
//
// @Summary      Remove a team member
// @Description  Removes a user from the team by deleting their membership record.
// @Tags         team-members
// @Produce      json
// @Param        org_id         path  string  true  "Organization ID"
// @Param        team_id        path  string  true  "Team ID"
// @Param        membership_id  path  string  true  "Membership ID"
// @Success      204            "No Content"
// @Failure      401            {object}  swaggerErrorResponse
// @Failure      403            {object}  swaggerErrorResponse
// @Failure      404            {object}  swaggerErrorResponse
// @Failure      500            {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/teams/{team_id}/members/{membership_id} [delete]
func (h *Handler) DeleteTeamMembership(c fiber.Ctx) error {
	membershipID := c.Params("membership_id")

	_, team, ok := h.requireTeamAccess(c)
	if !ok {
		return nil
	}

	existing, err := h.DB.GetTeamMembership(c.Context(), membershipID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "team membership not found")
		}
		h.Log.ErrorContext(c.Context(), "delete team membership: get", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get team membership")
	}
	if existing.TeamID != team.ID {
		return apierror.NotFound(c, "team membership not found")
	}

	if err := h.DB.DeleteTeamMembership(c.Context(), membershipID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "team membership not found")
		}
		h.Log.ErrorContext(c.Context(), "delete team membership", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to delete team membership")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
