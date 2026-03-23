package admin

import (
	"errors"
	"log/slog"
	"regexp"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
)

// slugRegex validates that a slug consists of lowercase alphanumeric characters
// and hyphens, starts and ends with an alphanumeric character, and is between
// 2 and 63 characters long.
var slugRegex = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,61}[a-z0-9]$`)

// createOrgRequest is the JSON body accepted by CreateOrg.
type createOrgRequest struct {
	Name              string  `json:"name"`
	Slug              string  `json:"slug"`
	Timezone          *string `json:"timezone"`
	DailyTokenLimit   int64   `json:"daily_token_limit"`
	MonthlyTokenLimit int64   `json:"monthly_token_limit"`
	RequestsPerMinute int     `json:"requests_per_minute"`
	RequestsPerDay    int     `json:"requests_per_day"`
}

// updateOrgRequest is the JSON body accepted by UpdateOrg.
// All fields are optional; a nil pointer means the field is left unchanged.
type updateOrgRequest struct {
	Name              *string `json:"name"`
	Slug              *string `json:"slug"`
	Timezone          *string `json:"timezone"`
	DailyTokenLimit   *int64  `json:"daily_token_limit"`
	MonthlyTokenLimit *int64  `json:"monthly_token_limit"`
	RequestsPerMinute *int    `json:"requests_per_minute"`
	RequestsPerDay    *int    `json:"requests_per_day"`
}

// orgResponse is the JSON representation of an organization returned by the API.
type orgResponse struct {
	ID                string  `json:"id"`
	Name              string  `json:"name"`
	Slug              string  `json:"slug"`
	Timezone          *string `json:"timezone,omitempty"`
	DailyTokenLimit   int64   `json:"daily_token_limit"`
	MonthlyTokenLimit int64   `json:"monthly_token_limit"`
	RequestsPerMinute int     `json:"requests_per_minute"`
	RequestsPerDay    int     `json:"requests_per_day"`
	MemberCount       int     `json:"member_count"`
	TeamCount         int     `json:"team_count"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
	DeletedAt         *string `json:"deleted_at,omitempty"`
}

// paginatedOrgsResponse wraps a page of organizations with pagination metadata.
type paginatedOrgsResponse struct {
	Data    []orgResponse `json:"data"`
	HasMore bool          `json:"has_more"`
	Cursor  string        `json:"next_cursor,omitempty"`
}

// orgToResponse converts a db.OrgWithCounts to its API wire representation,
// including the computed member_count and team_count fields.
func orgToResponse(o *db.OrgWithCounts) orgResponse {
	return orgResponse{
		ID:                o.ID,
		Name:              o.Name,
		Slug:              o.Slug,
		Timezone:          o.Timezone,
		DailyTokenLimit:   o.DailyTokenLimit,
		MonthlyTokenLimit: o.MonthlyTokenLimit,
		RequestsPerMinute: o.RequestsPerMinute,
		RequestsPerDay:    o.RequestsPerDay,
		MemberCount:       o.MemberCount,
		TeamCount:         o.TeamCount,
		CreatedAt:         o.CreatedAt,
		UpdatedAt:         o.UpdatedAt,
		DeletedAt:         o.DeletedAt,
	}
}

// CreateOrg handles POST /api/v1/orgs.
// Only system admins may create organizations.
//
// @Summary      Create an organization
// @Description  Creates a new organization. Only system admins may call this endpoint.
// @Tags         orgs
// @Accept       json
// @Produce      json
// @Param        body  body      createOrgRequest  true  "Organization parameters"
// @Success      201   {object}  orgResponse
// @Failure      400   {object}  swaggerErrorResponse
// @Failure      401   {object}  swaggerErrorResponse
// @Failure      403   {object}  swaggerErrorResponse
// @Failure      409   {object}  swaggerErrorResponse
// @Failure      500   {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs [post]
func (h *Handler) CreateOrg(c fiber.Ctx) error {
	var req createOrgRequest
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
	if lic.MaxOrgs() > 0 {
		count, err := h.DB.CountOrgs(c.Context())
		if err != nil {
			h.Log.ErrorContext(c.Context(), "create org: count orgs", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to check organization limit")
		}
		if count >= lic.MaxOrgs() {
			return apierror.Send(c, fiber.StatusForbidden, "limit_reached",
				"organization limit reached for your plan")
		}
	}

	created, err := h.DB.CreateOrg(c.Context(), db.CreateOrgParams{
		Name:              req.Name,
		Slug:              req.Slug,
		Timezone:          req.Timezone,
		DailyTokenLimit:   req.DailyTokenLimit,
		MonthlyTokenLimit: req.MonthlyTokenLimit,
		RequestsPerMinute: req.RequestsPerMinute,
		RequestsPerDay:    req.RequestsPerDay,
	})
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "organization slug already exists")
		}
		h.Log.ErrorContext(c.Context(), "create org", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to create organization")
	}

	org, err := h.DB.GetOrgWithCounts(c.Context(), created.ID)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "create org: get with counts", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to retrieve organization")
	}

	return c.Status(fiber.StatusCreated).JSON(orgToResponse(org))
}

// GetOrg handles GET /api/v1/orgs/:org_id.
// System admins may fetch any organization; org admins and members may only fetch their own.
//
// @Summary      Get an organization
// @Description  Returns a single organization. System admins may fetch any org; other roles may only fetch their own.
// @Tags         orgs
// @Produce      json
// @Param        org_id  path      string  true  "Organization ID"
// @Success      200     {object}  orgResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id} [get]
func (h *Handler) GetOrg(c fiber.Ctx) error {
	id := c.Params("org_id")

	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}
	if !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) && keyInfo.OrgID != id {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "insufficient permissions")
	}

	org, err := h.DB.GetOrgWithCounts(c.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "organization not found")
		}
		h.Log.ErrorContext(c.Context(), "get org", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get organization")
	}
	return c.JSON(orgToResponse(org))
}

// ListOrgs handles GET /api/v1/orgs.
// System admins receive a cursor-paginated list of all organizations.
// All other roles receive a single-item list containing only their own organization.
// Accepts query parameters: limit (int, default 20, max 100), cursor (UUIDv7 string),
// and include_deleted=true (system admin only).
//
// @Summary      List organizations
// @Description  System admins get a paginated list of all orgs; other roles receive only their own org.
// @Tags         orgs
// @Produce      json
// @Param        limit            query     int     false  "Page size (default 20, max 100)"
// @Param        cursor           query     string  false  "Pagination cursor (UUIDv7 of the last seen org)"
// @Param        include_deleted  query     bool    false  "Include soft-deleted orgs (system admin only)"
// @Success      200              {object}  paginatedOrgsResponse
// @Failure      400              {object}  swaggerErrorResponse
// @Failure      401              {object}  swaggerErrorResponse
// @Failure      403              {object}  swaggerErrorResponse
// @Failure      500              {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs [get]
func (h *Handler) ListOrgs(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}

	p, err := parsePagination(c)
	if err != nil {
		return apierror.BadRequest(c, err.Error())
	}
	includeDeleted := c.Query("include_deleted") == "true" && auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin)

	if !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		org, err := h.DB.GetOrgWithCounts(c.Context(), keyInfo.OrgID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return c.JSON(paginatedOrgsResponse{Data: []orgResponse{}})
			}
			h.Log.ErrorContext(c.Context(), "list orgs", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to list organizations")
		}
		return c.JSON(paginatedOrgsResponse{Data: []orgResponse{orgToResponse(org)}})
	}

	orgs, err := h.DB.ListOrgsWithCounts(c.Context(), p.Cursor, p.Limit+1, includeDeleted)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list orgs", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list organizations")
	}

	hasMore := len(orgs) > p.Limit
	if hasMore {
		orgs = orgs[:p.Limit]
	}

	resp := paginatedOrgsResponse{
		Data:    make([]orgResponse, len(orgs)),
		HasMore: hasMore,
	}
	for i := range orgs {
		resp.Data[i] = orgToResponse(&orgs[i])
	}
	if hasMore && len(orgs) > 0 {
		resp.Cursor = orgs[len(orgs)-1].ID
	}
	return c.JSON(resp)
}

// UpdateOrg handles PATCH /api/v1/orgs/:org_id.
// System admins may update any organization; org admins may update only their own.
//
// @Summary      Update an organization
// @Description  Updates the organization. System admins may update any org; org admins may only update their own.
// @Tags         orgs
// @Accept       json
// @Produce      json
// @Param        org_id  path      string            true  "Organization ID"
// @Param        body    body      updateOrgRequest  true  "Fields to update"
// @Success      200     {object}  orgResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      409     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id} [patch]
func (h *Handler) UpdateOrg(c fiber.Ctx) error {
	id := c.Params("org_id")

	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}
	isSystemAdmin := auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin)
	isOrgAdminOfTarget := auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) && keyInfo.OrgID == id
	if !isSystemAdmin && !isOrgAdminOfTarget {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "insufficient permissions")
	}

	var req updateOrgRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.Slug != nil && !slugRegex.MatchString(*req.Slug) {
		return apierror.BadRequest(c, "slug must be lowercase alphanumeric with hyphens, 2-63 characters")
	}

	_, err := h.DB.UpdateOrg(c.Context(), id, db.UpdateOrgParams{
		Name:              req.Name,
		Slug:              req.Slug,
		Timezone:          req.Timezone,
		DailyTokenLimit:   req.DailyTokenLimit,
		MonthlyTokenLimit: req.MonthlyTokenLimit,
		RequestsPerMinute: req.RequestsPerMinute,
		RequestsPerDay:    req.RequestsPerDay,
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "organization not found")
		}
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "organization slug already exists")
		}
		h.Log.ErrorContext(c.Context(), "update org", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to update organization")
	}

	// Invalidate key cache so updated limits take effect immediately.
	if h.Redis != nil {
		if pubErr := h.Redis.PublishInvalidation(c.Context(), voidredis.ChannelKeys, "org:"+id); pubErr != nil {
			h.Log.ErrorContext(c.Context(), "publish key cache invalidation", slog.String("error", pubErr.Error()))
		}
	}

	org, err := h.DB.GetOrgWithCounts(c.Context(), id)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "update org: get with counts", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to retrieve organization")
	}

	return c.JSON(orgToResponse(org))
}

// DeleteOrg handles DELETE /api/v1/orgs/:org_id.
// Only system admins may delete organizations. Deletion is a soft-delete.
//
// @Summary      Delete an organization
// @Description  Soft-deletes an organization. Only system admins may call this endpoint.
// @Tags         orgs
// @Produce      json
// @Param        org_id  path  string  true  "Organization ID"
// @Success      204     "No Content"
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id} [delete]
func (h *Handler) DeleteOrg(c fiber.Ctx) error {
	id := c.Params("org_id")

	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}
	if !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "insufficient permissions")
	}

	if err := h.DB.DeleteOrg(c.Context(), id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "organization not found")
		}
		h.Log.ErrorContext(c.Context(), "delete org", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to delete organization")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
