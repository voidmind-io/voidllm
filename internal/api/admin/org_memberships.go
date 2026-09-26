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
// The limit fields cap the user's aggregate usage across every API key they
// own in this org and may only be set by org admins and system admins.
type updateOrgMembershipRequest struct {
	Role              *string `json:"role"`
	DailyTokenLimit   *int64  `json:"daily_token_limit"`
	MonthlyTokenLimit *int64  `json:"monthly_token_limit"`
	RequestsPerMinute *int    `json:"requests_per_minute"`
	RequestsPerDay    *int    `json:"requests_per_day"`
}

// orgMembershipResponse is the JSON representation of an org membership returned by the API.
type orgMembershipResponse struct {
	ID                string `json:"id"`
	OrgID             string `json:"org_id"`
	UserID            string `json:"user_id"`
	Role              string `json:"role"`
	CreatedAt         string `json:"created_at"`
	DailyTokenLimit   int64  `json:"daily_token_limit"`
	MonthlyTokenLimit int64  `json:"monthly_token_limit"`
	RequestsPerMinute int    `json:"requests_per_minute"`
	RequestsPerDay    int    `json:"requests_per_day"`
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
		ID:                m.ID,
		OrgID:             m.OrgID,
		UserID:            m.UserID,
		Role:              m.Role,
		CreatedAt:         m.CreatedAt,
		DailyTokenLimit:   m.DailyTokenLimit,
		MonthlyTokenLimit: m.MonthlyTokenLimit,
		RequestsPerMinute: m.RequestsPerMinute,
		RequestsPerDay:    m.RequestsPerDay,
	}
}

// CreateOrgMembership handles POST /api/v1/orgs/:org_id/members.
// System admins may assign any role. Org admins may only assign the "member" role.
// On success the key cache is fully reloaded from the DB, mirroring
// UpdateOrgMembership, so that a user_key or session_key belonging to this
// user that was previously uncacheable for lack of a membership row (see
// auth.Cacheable) becomes usable on this instance immediately instead of
// waiting for the next periodic reload.
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

	// Reload the key cache so a user_key or session_key belonging to req.UserID
	// that could not previously be cached for lack of a membership row (see
	// auth.Cacheable) becomes usable locally right away. The membership itself
	// already succeeded above; a reload failure here is logged only, since the
	// periodic reload will retry regardless.
	if err := auth.LoadKeysIntoCache(c.Context(), h.DB, h.KeyCache, h.Log); err != nil {
		h.Log.ErrorContext(c.Context(), "create org membership: reload key cache", slog.String("error", err.Error()))
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
// only change a role to "member". Both may also set the per-user token and rate
// limits, which apply across every API key the user owns in this org. On
// success the key cache is fully reloaded from the DB so the new role and
// limits take effect immediately instead of waiting for the next periodic
// key cache reload.
//
// @Summary      Update an org membership
// @Description  Changes the role and/or per-user limits of an org membership. Only system admins may assign org_admin. Limits apply across all of the user's keys in this org and are enforced immediately.
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
	if req.DailyTokenLimit != nil && *req.DailyTokenLimit < 0 {
		return apierror.BadRequest(c, "daily_token_limit must be >= 0")
	}
	if req.MonthlyTokenLimit != nil && *req.MonthlyTokenLimit < 0 {
		return apierror.BadRequest(c, "monthly_token_limit must be >= 0")
	}
	if req.RequestsPerMinute != nil && *req.RequestsPerMinute < 0 {
		return apierror.BadRequest(c, "requests_per_minute must be >= 0")
	}
	if req.RequestsPerDay != nil && *req.RequestsPerDay < 0 {
		return apierror.BadRequest(c, "requests_per_day must be >= 0")
	}

	m, err := h.DB.UpdateOrgMembership(c.Context(), membershipID, db.UpdateOrgMembershipParams{
		Role:              req.Role,
		DailyTokenLimit:   req.DailyTokenLimit,
		MonthlyTokenLimit: req.MonthlyTokenLimit,
		RequestsPerMinute: req.RequestsPerMinute,
		RequestsPerDay:    req.RequestsPerDay,
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "org membership not found")
		}
		h.Log.ErrorContext(c.Context(), "update org membership", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to update org membership")
	}

	// Refresh the key cache from the DB so the new limits are enforced
	// immediately, without waiting for the next periodic key cache reload.
	// A prior version patched matching cache entries in place, but that
	// Range-then-Set sequence is not atomic with respect to the periodic
	// full reload goroutine: a reload running concurrently could win the
	// race and overwrite this patch with the role/limit state it read just
	// before the DB write above, leaving the cache stale until the next
	// reload cycle. A full reload has no such race — it always reflects the
	// DB state as of when it ran. The membership update itself already
	// succeeded above; a reload failure here is logged only, since the
	// periodic reload will retry regardless.
	if err := auth.LoadKeysIntoCache(c.Context(), h.DB, h.KeyCache, h.Log); err != nil {
		h.Log.ErrorContext(c.Context(), "update org membership: reload key cache", slog.String("error", err.Error()))
	}

	return c.JSON(orgMembershipToResponse(m))
}

// DeleteOrgMembership handles DELETE /api/v1/orgs/:org_id/members/:membership_id.
// System admins may delete any membership; org admins may only delete memberships
// within their own organization. Every API key the removed user owns in this
// org is immediately evicted from the in-memory key cache (and, when Redis is
// configured, invalidated on other instances) so those keys stop
// authenticating without waiting for the next periodic cache reload. Keys the
// user owns in other organizations are unaffected.
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

	// Evict every cached key the removed user owns in this org so they stop
	// authenticating immediately instead of waiting for the next periodic
	// cache reload. Removing the membership row breaks the invariant
	// auth.Cacheable relies on (every user_key/session_key requires a
	// surviving membership in the key's own org), so leaving a stale cache
	// entry in place would let this user keep making requests under their old
	// role and limits until the next reload. The membership was already
	// deleted above; a failure here is logged only and never fails the
	// request.
	hashes, err := h.DB.ListActiveKeyHashesByUserInOrg(c.Context(), existing.UserID, orgID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "delete org membership: list key hashes for cache eviction", slog.String("error", err.Error()))
	}
	for _, keyHash := range hashes {
		h.KeyCache.Delete(keyHash)

		if h.Redis != nil {
			if err := h.Redis.PublishInvalidation(c.Context(), voidredis.ChannelKeys, keyHash); err != nil {
				h.Log.LogAttrs(c.Context(), slog.LevelWarn, "redis: publish key invalidation failed",
					slog.String("error", err.Error()),
				)
			}
		}
	}

	return c.SendStatus(fiber.StatusNoContent)
}
