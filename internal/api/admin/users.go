package admin

import (
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/audit"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"golang.org/x/crypto/bcrypt"
)

// createUserRequest is the JSON body accepted by CreateUser.
// OrgID is required for all users, including system admins. Every user belongs
// to exactly one organization; there are no org-less users. Role is the
// organization membership role (defaults to "member" when omitted).
type createUserRequest struct {
	Email         string `json:"email"`
	DisplayName   string `json:"display_name"`
	Password      string `json:"password"`
	IsSystemAdmin bool   `json:"is_system_admin"`
	OrgID         string `json:"org_id"`
	Role          string `json:"role"`
}

// updateUserRequest is the JSON body accepted by UpdateUser.
// All fields are optional; a nil pointer means the field is left unchanged.
type updateUserRequest struct {
	Email         *string `json:"email"`
	DisplayName   *string `json:"display_name"`
	Password      *string `json:"password"`
	IsSystemAdmin *bool   `json:"is_system_admin"`
}

// userResponse is the JSON representation of a user returned by the API.
// password_hash and external_id are never included.
type userResponse struct {
	ID            string  `json:"id"`
	Email         string  `json:"email"`
	DisplayName   string  `json:"display_name"`
	AuthProvider  string  `json:"auth_provider"`
	IsSystemAdmin bool    `json:"is_system_admin"`
	CreatedAt     string  `json:"created_at"`
	UpdatedAt     string  `json:"updated_at"`
	DeletedAt     *string `json:"deleted_at,omitempty"`
}

// paginatedUsersResponse wraps a page of users with pagination metadata.
type paginatedUsersResponse struct {
	Data    []userResponse `json:"data"`
	HasMore bool           `json:"has_more"`
	Cursor  string         `json:"next_cursor,omitempty"`
}

// userToResponse converts a db.User to its API wire representation. For
// soft-deleted users the email column is stripped of its tombstone suffix
// (see db.StripTombstone) so admins viewing include_deleted listings see the
// original email rather than the mangled one used to free it for reuse.
func userToResponse(u *db.User) userResponse {
	email := u.Email
	if u.DeletedAt != nil {
		email = db.StripTombstone(email, u.ID)
	}
	return userResponse{
		ID:            u.ID,
		Email:         email,
		DisplayName:   u.DisplayName,
		AuthProvider:  u.AuthProvider,
		IsSystemAdmin: u.IsSystemAdmin,
		CreatedAt:     u.CreatedAt,
		UpdatedAt:     u.UpdatedAt,
		DeletedAt:     u.DeletedAt,
	}
}

// CreateUser handles POST /api/v1/users.
// Requires org_admin or higher. Only system admins may create system admin users.
//
// @Summary      Create a user
// @Description  Creates a new user account. Only system admins may set is_system_admin=true.
// @Tags         users
// @Accept       json
// @Produce      json
// @Param        body  body      createUserRequest  true  "User parameters"
// @Success      201   {object}  userResponse
// @Failure      400   {object}  swaggerErrorResponse
// @Failure      401   {object}  swaggerErrorResponse
// @Failure      403   {object}  swaggerErrorResponse
// @Failure      409   {object}  swaggerErrorResponse
// @Failure      500   {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /users [post]
func (h *Handler) CreateUser(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}
	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "insufficient permissions")
	}

	var req createUserRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	req.Email = strings.TrimSpace(req.Email)
	req.DisplayName = strings.TrimSpace(req.DisplayName)

	if req.Email == "" {
		return apierror.BadRequest(c, "email is required")
	}
	if !strings.Contains(req.Email, "@") {
		return apierror.BadRequest(c, "invalid email format")
	}
	if db.ContainsTombstoneMarker(req.Email) {
		return apierror.BadRequest(c, `email must not contain ":deleted:"`)
	}
	if req.DisplayName == "" {
		return apierror.BadRequest(c, "display_name is required")
	}
	if len(req.Password) < 8 {
		return apierror.BadRequest(c, "password must be at least 8 characters")
	}
	if len(req.Password) > 72 {
		return apierror.BadRequest(c, "password must be at most 72 bytes")
	}
	if req.IsSystemAdmin && !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "only system admins may create system admin users")
	}

	// Every user must belong to an org — there are no org-less users.
	if req.OrgID == "" {
		return apierror.BadRequest(c, "org_id is required")
	}

	// Non-system-admin callers may only assign users to their own org.
	if !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		if req.OrgID != keyInfo.OrgID {
			return apierror.Send(c, fiber.StatusForbidden, "forbidden", "org_id must match your organization")
		}
	}

	// Validate and default the membership role.
	membershipRole := req.Role
	if membershipRole == "" {
		membershipRole = auth.RoleMember
	}
	switch membershipRole {
	case auth.RoleMember, auth.RoleTeamAdmin:
		// valid membership roles for all callers
	case auth.RoleOrgAdmin:
		// only system admins may grant the org_admin membership role
		if !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
			return apierror.Send(c, fiber.StatusForbidden, "forbidden", "only system admins may assign the org_admin role")
		}
	default:
		return apierror.BadRequest(c, "role must be one of: member, team_admin, org_admin")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		if errors.Is(err, bcrypt.ErrPasswordTooLong) {
			return apierror.BadRequest(c, "password must be at most 72 bytes")
		}
		h.Log.ErrorContext(c.Context(), "create user: bcrypt", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to hash password")
	}
	hashStr := string(hash)

	user, err := h.DB.CreateUserWithMembership(c.Context(), db.CreateUserParams{
		Email:         req.Email,
		DisplayName:   req.DisplayName,
		PasswordHash:  &hashStr,
		AuthProvider:  "local",
		IsSystemAdmin: req.IsSystemAdmin,
	}, req.OrgID, membershipRole)
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "email already in use")
		}
		if errors.Is(err, db.ErrForeignKey) {
			return apierror.BadRequest(c, "organization not found")
		}
		if errors.Is(err, db.ErrReservedValue) {
			return apierror.BadRequest(c, `email must not contain ":deleted:"`)
		}
		h.Log.ErrorContext(c.Context(), "create user", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to create user")
	}

	if h.AuditLogger != nil {
		h.AuditLogger.Log(audit.Event{
			Timestamp:    time.Now().UTC(),
			OrgID:        req.OrgID,
			ActorID:      keyInfo.UserID,
			ActorType:    "user",
			ActorKeyID:   keyInfo.ID,
			Action:       "user_created",
			ResourceType: "user",
			ResourceID:   user.ID,
			Description: marshalDescription(map[string]string{
				"email":           req.Email,
				"org_id":          req.OrgID,
				"role":            membershipRole,
				"is_system_admin": strconv.FormatBool(req.IsSystemAdmin),
			}),
			IPAddress:  c.IP(),
			StatusCode: fiber.StatusCreated,
			RequestID:  apierror.RequestIDFromCtx(c),
		})
	}

	return c.Status(fiber.StatusCreated).JSON(userToResponse(user))
}

// GetUser handles GET /api/v1/users/:user_id.
// Requires org_admin or higher. org_admin callers may only fetch users who are
// members of their own organization; system_admin may fetch any user.
//
// @Summary      Get a user
// @Description  Returns a single user. Org admins may only fetch users within their org; system admins may fetch any user.
// @Tags         users
// @Produce      json
// @Param        user_id  path      string  true  "User ID"
// @Success      200      {object}  userResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /users/{user_id} [get]
func (h *Handler) GetUser(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}
	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "insufficient permissions")
	}

	id := c.Params("user_id")

	user, err := h.DB.GetUser(c.Context(), id)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "user not found")
		}
		h.Log.ErrorContext(c.Context(), "get user", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get user")
	}

	if !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		if _, err := h.DB.GetUserOrgRole(c.Context(), id, keyInfo.OrgID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return apierror.NotFound(c, "user not found")
			}
			h.Log.ErrorContext(c.Context(), "get user: verify org membership", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to verify user access")
		}
	}

	return c.JSON(userToResponse(user))
}

// ListUsers handles GET /api/v1/users.
// Requires system_admin. Accepts query parameters: limit, cursor, include_deleted (system_admin only).
//
// @Summary      List users
// @Description  Returns a cursor-paginated list of all users. Requires system admin.
// @Tags         users
// @Produce      json
// @Param        limit            query     int     false  "Page size (default 20, max 100)"
// @Param        cursor           query     string  false  "Pagination cursor (UUIDv7 of the last seen user)"
// @Param        include_deleted  query     bool    false  "Include soft-deleted users"
// @Success      200              {object}  paginatedUsersResponse
// @Failure      400              {object}  swaggerErrorResponse
// @Failure      401              {object}  swaggerErrorResponse
// @Failure      403              {object}  swaggerErrorResponse
// @Failure      500              {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /users [get]
func (h *Handler) ListUsers(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}
	if !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "insufficient permissions")
	}

	p, err := parsePagination(c)
	if err != nil {
		return apierror.BadRequest(c, err.Error())
	}
	includeDeleted := c.Query("include_deleted") == "true" && auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin)

	users, err := h.DB.ListUsers(c.Context(), p.Cursor, p.Limit+1, includeDeleted)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list users", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list users")
	}

	hasMore := len(users) > p.Limit
	if hasMore {
		users = users[:p.Limit]
	}

	resp := paginatedUsersResponse{
		Data:    make([]userResponse, len(users)),
		HasMore: hasMore,
	}
	for i := range users {
		resp.Data[i] = userToResponse(&users[i])
	}
	if hasMore && len(users) > 0 {
		resp.Cursor = users[len(users)-1].ID
	}
	return c.JSON(resp)
}

// UpdateUser handles PATCH /api/v1/users/:user_id.
// Requires org_admin or higher. org_admin callers may only update users who are
// members of their own organization; system_admin may update any user.
// Only system admins may change is_system_admin.
//
// @Summary      Update a user
// @Description  Updates user profile fields. Only system admins may change is_system_admin.
// @Tags         users
// @Accept       json
// @Produce      json
// @Param        user_id  path      string             true  "User ID"
// @Param        body     body      updateUserRequest  true  "Fields to update"
// @Success      200      {object}  userResponse
// @Failure      400      {object}  swaggerErrorResponse
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      409      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /users/{user_id} [patch]
func (h *Handler) UpdateUser(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}
	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "insufficient permissions")
	}

	id := c.Params("user_id")

	if !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		if _, err := h.DB.GetUserOrgRole(c.Context(), id, keyInfo.OrgID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return apierror.NotFound(c, "user not found")
			}
			h.Log.ErrorContext(c.Context(), "update user: verify org membership", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to verify user access")
		}
	}

	var req updateUserRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if req.IsSystemAdmin != nil && !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "only system admins may change is_system_admin")
	}

	if req.Email != nil {
		trimmed := strings.TrimSpace(*req.Email)
		if trimmed == "" {
			return apierror.BadRequest(c, "email must not be empty")
		}
		if !strings.Contains(trimmed, "@") {
			return apierror.BadRequest(c, "invalid email format")
		}
		if db.ContainsTombstoneMarker(trimmed) {
			return apierror.BadRequest(c, `email must not contain ":deleted:"`)
		}
		req.Email = &trimmed
	}

	if req.DisplayName != nil {
		trimmed := strings.TrimSpace(*req.DisplayName)
		if trimmed == "" {
			return apierror.BadRequest(c, "display_name must not be empty")
		}
		req.DisplayName = &trimmed
	}

	params := db.UpdateUserParams{
		Email:         req.Email,
		DisplayName:   req.DisplayName,
		IsSystemAdmin: req.IsSystemAdmin,
	}

	if req.Password != nil {
		if len(*req.Password) < 8 {
			return apierror.BadRequest(c, "password must be at least 8 characters")
		}
		if len(*req.Password) > 72 {
			return apierror.BadRequest(c, "password must be at most 72 bytes")
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(*req.Password), bcrypt.DefaultCost)
		if err != nil {
			if errors.Is(err, bcrypt.ErrPasswordTooLong) {
				return apierror.BadRequest(c, "password must be at most 72 bytes")
			}
			h.Log.ErrorContext(c.Context(), "update user: bcrypt", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to hash password")
		}
		hashStr := string(hash)
		params.PasswordHash = &hashStr
	}

	user, err := h.DB.UpdateUser(c.Context(), id, params)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "user not found")
		}
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "email already in use")
		}
		if errors.Is(err, db.ErrReservedValue) {
			return apierror.BadRequest(c, `email must not contain ":deleted:"`)
		}
		h.Log.ErrorContext(c.Context(), "update user", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to update user")
	}
	return c.JSON(userToResponse(user))
}

// DeleteUser handles DELETE /api/v1/users/:user_id.
// Requires system_admin. Deletion is a soft-delete.
//
// @Summary      Delete a user
// @Description  Soft-deletes a user. Requires system admin.
// @Tags         users
// @Produce      json
// @Param        user_id  path  string  true  "User ID"
// @Success      204      "No Content"
// @Failure      401      {object}  swaggerErrorResponse
// @Failure      403      {object}  swaggerErrorResponse
// @Failure      404      {object}  swaggerErrorResponse
// @Failure      500      {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /users/{user_id} [delete]
func (h *Handler) DeleteUser(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}
	if !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "insufficient permissions")
	}

	id := c.Params("user_id")

	if err := h.DB.DeleteUser(c.Context(), id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "user not found")
		}
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "delete blocked: another row occupies the reserved deleted-name for this record; rename it and retry")
		}
		h.Log.ErrorContext(c.Context(), "delete user", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to delete user")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
