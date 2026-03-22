package admin

import (
	"errors"
	"log/slog"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
)

// createServiceAccountRequest is the JSON body accepted by CreateServiceAccount.
type createServiceAccountRequest struct {
	Name   string  `json:"name"`
	TeamID *string `json:"team_id"`
}

// updateServiceAccountRequest is the JSON body accepted by UpdateServiceAccount.
// All fields are optional; a nil pointer means the field is left unchanged.
type updateServiceAccountRequest struct {
	Name *string `json:"name"`
}

// serviceAccountResponse is the JSON representation of a service account returned by the API.
type serviceAccountResponse struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	OrgID     string  `json:"org_id"`
	TeamID    *string `json:"team_id,omitempty"`
	CreatedBy string  `json:"created_by"`
	KeyCount  int     `json:"key_count"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
	DeletedAt *string `json:"deleted_at,omitempty"`
}

// paginatedServiceAccountsResponse wraps a page of service accounts with pagination metadata.
type paginatedServiceAccountsResponse struct {
	Data    []serviceAccountResponse `json:"data"`
	HasMore bool                     `json:"has_more"`
	Cursor  string                   `json:"next_cursor,omitempty"`
}

// serviceAccountToResponse converts a db.ServiceAccountWithCounts to its API wire representation.
func serviceAccountToResponse(sa *db.ServiceAccountWithCounts) serviceAccountResponse {
	return serviceAccountResponse{
		ID:        sa.ID,
		Name:      sa.Name,
		OrgID:     sa.OrgID,
		TeamID:    sa.TeamID,
		CreatedBy: sa.CreatedBy,
		KeyCount:  sa.KeyCount,
		CreatedAt: sa.CreatedAt,
		UpdatedAt: sa.UpdatedAt,
		DeletedAt: sa.DeletedAt,
	}
}

// CreateServiceAccount handles POST /api/v1/orgs/:org_id/service-accounts.
// System admins and org admins of the target org may create service accounts.
// If team_id is provided, the team must belong to the same org.
// The caller must be authenticated with a user key; service account keys may not create service accounts.
//
// @Summary      Create a service account
// @Description  Creates a new service account. Must be called with a user key (not a service account key).
// @Tags         service-accounts
// @Accept       json
// @Produce      json
// @Param        org_id  path      string                        true  "Organization ID"
// @Param        body    body      createServiceAccountRequest   true  "Service account parameters"
// @Success      201     {object}  serviceAccountResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      409     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/service-accounts [post]
func (h *Handler) CreateServiceAccount(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	if keyInfo.UserID == "" {
		return apierror.BadRequest(c, "service accounts can only be created by user keys")
	}

	var req createServiceAccountRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.Name == "" {
		return apierror.BadRequest(c, "name is required")
	}

	if req.TeamID != nil {
		team, err := h.DB.GetTeam(c.Context(), *req.TeamID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return apierror.NotFound(c, "team not found")
			}
			h.Log.ErrorContext(c.Context(), "create service account: get team", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to verify team")
		}
		if team.OrgID != orgID {
			return apierror.NotFound(c, "team not found")
		}
	}

	sa, err := h.DB.CreateServiceAccount(c.Context(), db.CreateServiceAccountParams{
		Name:      req.Name,
		OrgID:     orgID,
		TeamID:    req.TeamID,
		CreatedBy: keyInfo.UserID,
	})
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "service account already exists")
		}
		h.Log.ErrorContext(c.Context(), "create service account", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to create service account")
	}

	return c.Status(fiber.StatusCreated).JSON(serviceAccountToResponse(&db.ServiceAccountWithCounts{ServiceAccount: *sa}))
}

// GetServiceAccount handles GET /api/v1/orgs/:org_id/service-accounts/:sa_id.
// Org admins may fetch any service account. Members may only fetch service accounts
// they created.
// Returns 404 if the service account belongs to a different org or the caller lacks access.
//
// @Summary      Get a service account
// @Description  Returns a single service account. Members only see service accounts they created.
// @Tags         service-accounts
// @Produce      json
// @Param        org_id  path      string  true  "Organization ID"
// @Param        sa_id   path      string  true  "Service account ID"
// @Success      200     {object}  serviceAccountResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/service-accounts/{sa_id} [get]
func (h *Handler) GetServiceAccount(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	saID := c.Params("sa_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	sa, err := h.DB.GetServiceAccountWithCounts(c.Context(), saID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "service account not found")
		}
		h.Log.ErrorContext(c.Context(), "get service account", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get service account")
	}
	if sa.OrgID != orgID {
		return apierror.NotFound(c, "service account not found")
	}

	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) && sa.CreatedBy != keyInfo.UserID {
		return apierror.NotFound(c, "service account not found")
	}

	return c.JSON(serviceAccountToResponse(sa))
}

// ListServiceAccounts handles GET /api/v1/orgs/:org_id/service-accounts.
// Org admins see all service accounts. Members see only service accounts they created.
// Accepts query parameters: limit (int, default 20, max 100), cursor (UUIDv7 string),
// and include_deleted=true (system admin only).
//
// @Summary      List service accounts
// @Description  Returns a cursor-paginated list of service accounts. Members only see accounts they created.
// @Tags         service-accounts
// @Produce      json
// @Param        org_id           path      string  true   "Organization ID"
// @Param        limit            query     int     false  "Page size (default 20, max 100)"
// @Param        cursor           query     string  false  "Pagination cursor (UUIDv7 of the last seen service account)"
// @Param        include_deleted  query     bool    false  "Include soft-deleted service accounts (system admin only)"
// @Success      200              {object}  paginatedServiceAccountsResponse
// @Failure      400              {object}  swaggerErrorResponse
// @Failure      401              {object}  swaggerErrorResponse
// @Failure      403              {object}  swaggerErrorResponse
// @Failure      500              {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/service-accounts [get]
func (h *Handler) ListServiceAccounts(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	p, err := parsePagination(c)
	if err != nil {
		return apierror.BadRequest(c, err.Error())
	}
	includeDeleted := c.Query("include_deleted") == "true" && auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin)

	// Org admins see all service accounts; members see only those they created.
	var filterCreatedBy string
	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		filterCreatedBy = keyInfo.UserID
	}

	accounts, err := h.DB.ListServiceAccountsWithCounts(c.Context(), orgID, filterCreatedBy, p.Cursor, p.Limit+1, includeDeleted)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list service accounts", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list service accounts")
	}

	hasMore := len(accounts) > p.Limit
	if hasMore {
		accounts = accounts[:p.Limit]
	}

	resp := paginatedServiceAccountsResponse{
		Data:    make([]serviceAccountResponse, len(accounts)),
		HasMore: hasMore,
	}
	for i := range accounts {
		resp.Data[i] = serviceAccountToResponse(&accounts[i])
	}
	if hasMore && len(accounts) > 0 {
		resp.Cursor = accounts[len(accounts)-1].ID
	}
	return c.JSON(resp)
}

// UpdateServiceAccount handles PATCH /api/v1/orgs/:org_id/service-accounts/:sa_id.
// Org admins may update any service account. Members may only update service accounts
// they created.
// Returns 404 if the service account belongs to a different org or the caller lacks access.
// Only the name field may be updated.
//
// @Summary      Update a service account
// @Description  Updates the service account name. Members may only update service accounts they created.
// @Tags         service-accounts
// @Accept       json
// @Produce      json
// @Param        org_id  path      string                        true  "Organization ID"
// @Param        sa_id   path      string                        true  "Service account ID"
// @Param        body    body      updateServiceAccountRequest   true  "Fields to update"
// @Success      200     {object}  serviceAccountResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/service-accounts/{sa_id} [patch]
func (h *Handler) UpdateServiceAccount(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	saID := c.Params("sa_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	existing, err := h.DB.GetServiceAccount(c.Context(), saID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "service account not found")
		}
		h.Log.ErrorContext(c.Context(), "update service account: get", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get service account")
	}
	if existing.OrgID != orgID {
		return apierror.NotFound(c, "service account not found")
	}

	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) && existing.CreatedBy != keyInfo.UserID {
		return apierror.NotFound(c, "service account not found")
	}

	var req updateServiceAccountRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	sa, err := h.DB.UpdateServiceAccountWithCounts(c.Context(), existing.ID, db.UpdateServiceAccountParams{
		Name: req.Name,
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "service account not found")
		}
		h.Log.ErrorContext(c.Context(), "update service account", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to update service account")
	}

	return c.JSON(serviceAccountToResponse(sa))
}

// DeleteServiceAccount handles DELETE /api/v1/orgs/:org_id/service-accounts/:sa_id.
// Org admins may delete any service account. Members may only delete service accounts
// they created. Deletion is a soft-delete.
// Returns 404 if the service account belongs to a different org or the caller lacks access.
//
// @Summary      Delete a service account
// @Description  Soft-deletes the service account. Members may only delete service accounts they created.
// @Tags         service-accounts
// @Produce      json
// @Param        org_id  path  string  true  "Organization ID"
// @Param        sa_id   path  string  true  "Service account ID"
// @Success      204     "No Content"
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/service-accounts/{sa_id} [delete]
func (h *Handler) DeleteServiceAccount(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	saID := c.Params("sa_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	sa, err := h.DB.GetServiceAccount(c.Context(), saID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "service account not found")
		}
		h.Log.ErrorContext(c.Context(), "delete service account: get", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get service account")
	}
	if sa.OrgID != orgID {
		return apierror.NotFound(c, "service account not found")
	}

	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) && sa.CreatedBy != keyInfo.UserID {
		return apierror.NotFound(c, "service account not found")
	}

	if err := h.DB.DeleteServiceAccount(c.Context(), sa.ID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "service account not found")
		}
		h.Log.ErrorContext(c.Context(), "delete service account", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to delete service account")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
