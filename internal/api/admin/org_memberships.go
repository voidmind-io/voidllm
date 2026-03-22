package admin

import (
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// validOrgMembershipRoles is the set of roles that may be assigned to an org membership.
var validOrgMembershipRoles = map[string]bool{
	auth.RoleOrgAdmin: true,
	auth.RoleMember:   true,
}

// createOrgMembershipRequest is the JSON body accepted by CreateOrgMembership.
type createOrgMembershipRequest struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

// updateOrgMembershipRequest is the JSON body accepted by UpdateOrgMembership.
// All fields are optional; a nil pointer means the field is left unchanged.
type updateOrgMembershipRequest struct {
	Role *string `json:"role"`
}

// orgMembershipResponse is the JSON representation of an org membership returned by the API.
type orgMembershipResponse struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	UserID    string `json:"user_id"`
	Role      string `json:"role"`
	CreatedAt string `json:"created_at"`
}

// paginatedOrgMembershipsResponse wraps a page of org memberships with pagination metadata.
type paginatedOrgMembershipsResponse struct {
	Data    []orgMembershipResponse `json:"data"`
	HasMore bool                    `json:"has_more"`
	Cursor  string                  `json:"next_cursor,omitempty"`
}

// orgMembershipToResponse converts a db.OrgMembership to its API wire representation.
func orgMembershipToResponse(m *db.OrgMembership) orgMembershipResponse {
	return orgMembershipResponse{
		ID:        m.ID,
		OrgID:     m.OrgID,
		UserID:    m.UserID,
		Role:      m.Role,
		CreatedAt: m.CreatedAt,
	}
}

// CreateOrgMembership handles POST /api/v1/orgs/:org_id/members.
// System admins may assign any role. Org admins may only assign the "member" role.
//
// @Summary      Add an org member
// @Description  Adds a user to the organization with the specified role. Only system admins may assign the org_admin role.
// @Tags         org-members
// @Accept       json
// @Produce      json
// @Param        org_id  path      string                       true  "Organization ID"
// @Param        body    body      createOrgMembershipRequest   true  "Membership parameters"
// @Success      201     {object}  orgMembershipResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      409     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/members [post]
func (h *Handler) CreateOrgMembership(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}
	isSystemAdmin := auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin)

	var req createOrgMembershipRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.UserID == "" {
		return apierror.BadRequest(c, "user_id is required")
	}
	if req.Role == "" {
		return apierror.BadRequest(c, "role is required")
	}
	if !validOrgMembershipRoles[req.Role] {
		return apierror.BadRequest(c, "role must be \"org_admin\" or \"member\"")
	}
	if !isSystemAdmin && req.Role == auth.RoleOrgAdmin {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "only system admins may assign the org_admin role")
	}

	m, err := h.DB.CreateOrgMembership(c.Context(), db.CreateOrgMembershipParams{
		OrgID:  orgID,
		UserID: req.UserID,
		Role:   req.Role,
	})
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "user is already a member of this organization")
		}
		h.Log.ErrorContext(c.Context(), "create org membership", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to create org membership")
	}

	return c.Status(fiber.StatusCreated).JSON(orgMembershipToResponse(m))
}

// ListOrgMemberships handles GET /api/v1/orgs/:org_id/members.
// System admins may list memberships for any organization; org admins may only list
// memberships for their own organization.
//
// @Summary      List org members
// @Description  Returns a cursor-paginated list of organization memberships.
// @Tags         org-members
// @Produce      json
// @Param        org_id  path      string  true   "Organization ID"
// @Param        limit   query     int     false  "Page size (default 20, max 100)"
// @Param        cursor  query     string  false  "Pagination cursor (UUIDv7 of the last seen membership)"
// @Success      200     {object}  paginatedOrgMembershipsResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/members [get]
func (h *Handler) ListOrgMemberships(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	p, err := parsePagination(c)
	if err != nil {
		return apierror.BadRequest(c, err.Error())
	}

	memberships, err := h.DB.ListOrgMemberships(c.Context(), orgID, p.Cursor, p.Limit+1)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list org memberships", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list org memberships")
	}

	hasMore := len(memberships) > p.Limit
	if hasMore {
		memberships = memberships[:p.Limit]
	}

	resp := paginatedOrgMembershipsResponse{
		Data:    make([]orgMembershipResponse, len(memberships)),
		HasMore: hasMore,
	}
	for i := range memberships {
		resp.Data[i] = orgMembershipToResponse(&memberships[i])
	}
	if hasMore && len(memberships) > 0 {
		resp.Cursor = memberships[len(memberships)-1].ID
	}
	return c.JSON(resp)
}

// UpdateOrgMembership handles PATCH /api/v1/orgs/:org_id/members/:membership_id.
// System admins may change a membership role to any valid value. Org admins may
// only change a role to "member".
//
// @Summary      Update an org membership
// @Description  Changes the role of an org membership. Only system admins may assign org_admin.
// @Tags         org-members
// @Accept       json
// @Produce      json
// @Param        org_id         path      string                        true  "Organization ID"
// @Param        membership_id  path      string                        true  "Membership ID"
// @Param        body           body      updateOrgMembershipRequest    true  "Fields to update"
// @Success      200            {object}  orgMembershipResponse
// @Failure      400            {object}  swaggerErrorResponse
// @Failure      401            {object}  swaggerErrorResponse
// @Failure      403            {object}  swaggerErrorResponse
// @Failure      404            {object}  swaggerErrorResponse
// @Failure      500            {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/members/{membership_id} [patch]
func (h *Handler) UpdateOrgMembership(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	membershipID := c.Params("membership_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}
	isSystemAdmin := auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin)

	existing, err := h.DB.GetOrgMembership(c.Context(), membershipID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "org membership not found")
		}
		h.Log.ErrorContext(c.Context(), "update org membership: get", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get org membership")
	}
	if existing.OrgID != orgID {
		return apierror.NotFound(c, "org membership not found")
	}

	var req updateOrgMembershipRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.Role != nil {
		if !validOrgMembershipRoles[*req.Role] {
			return apierror.BadRequest(c, "role must be \"org_admin\" or \"member\"")
		}
		if !isSystemAdmin && *req.Role == auth.RoleOrgAdmin {
			return apierror.Send(c, fiber.StatusForbidden, "forbidden", "only system admins may assign the org_admin role")
		}
	}

	m, err := h.DB.UpdateOrgMembership(c.Context(), membershipID, db.UpdateOrgMembershipParams{
		Role: req.Role,
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "org membership not found")
		}
		h.Log.ErrorContext(c.Context(), "update org membership", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to update org membership")
	}
	return c.JSON(orgMembershipToResponse(m))
}

// DeleteOrgMembership handles DELETE /api/v1/orgs/:org_id/members/:membership_id.
// System admins may delete any membership; org admins may only delete memberships
// within their own organization.
//
// @Summary      Remove an org member
// @Description  Removes a user from the organization by deleting their membership record.
// @Tags         org-members
// @Produce      json
// @Param        org_id         path  string  true  "Organization ID"
// @Param        membership_id  path  string  true  "Membership ID"
// @Success      204            "No Content"
// @Failure      401            {object}  swaggerErrorResponse
// @Failure      403            {object}  swaggerErrorResponse
// @Failure      404            {object}  swaggerErrorResponse
// @Failure      500            {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/members/{membership_id} [delete]
func (h *Handler) DeleteOrgMembership(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	membershipID := c.Params("membership_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	existing, err := h.DB.GetOrgMembership(c.Context(), membershipID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "org membership not found")
		}
		h.Log.ErrorContext(c.Context(), "delete org membership: get", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get org membership")
	}
	if existing.OrgID != orgID {
		return apierror.NotFound(c, "org membership not found")
	}

	if err := h.DB.DeleteOrgMembership(c.Context(), membershipID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "org membership not found")
		}
		h.Log.ErrorContext(c.Context(), "delete org membership", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to delete org membership")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
