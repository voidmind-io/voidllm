package admin

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"golang.org/x/crypto/bcrypt"

	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// createInviteRequest is the JSON body accepted by CreateInvite.
type createInviteRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// createInviteResponse is the JSON body returned by CreateInvite.
// It includes the plaintext token, which is shown exactly once at creation time.
type createInviteResponse struct {
	ID        string `json:"id"`
	Token     string `json:"token"`
	TokenHint string `json:"token_hint"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	OrgID     string `json:"org_id"`
	ExpiresAt string `json:"expires_at"`
	CreatedAt string `json:"created_at"`
}

// inviteResponse is the JSON representation of an invite token returned by
// list operations. It never includes the plaintext token or the token_hash.
type inviteResponse struct {
	ID        string `json:"id"`
	TokenHint string `json:"token_hint"`
	Email     string `json:"email"`
	Role      string `json:"role"`
	OrgID     string `json:"org_id"`
	Status    string `json:"status"`
	ExpiresAt string `json:"expires_at"`
	CreatedBy string `json:"created_by"`
	CreatedAt string `json:"created_at"`
}

// paginatedInvitesResponse wraps a page of invite tokens with pagination metadata.
type paginatedInvitesResponse struct {
	Data    []inviteResponse `json:"data"`
	HasMore bool             `json:"has_more"`
	Cursor  string           `json:"next_cursor,omitempty"`
}

// peekInviteResponse is returned by PeekInvite for unauthenticated consumers.
type peekInviteResponse struct {
	Email     string `json:"email"`
	OrgName   string `json:"org_name"`
	Role      string `json:"role"`
	ExpiresAt string `json:"expires_at"`
}

// redeemInviteRequest is the JSON body accepted by RedeemInvite.
type redeemInviteRequest struct {
	Token       string `json:"token"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

// redeemInviteResponse is returned on successful invite redemption.
type redeemInviteResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	OrgID       string `json:"org_id"`
	Role        string `json:"role"`
}

// inviteStatus computes the human-readable status for an invite token.
func inviteStatus(t *db.InviteToken) string {
	if t.RedeemedAt != nil {
		return "redeemed"
	}
	exp, err := time.Parse(time.RFC3339, t.ExpiresAt)
	if err == nil && exp.Before(time.Now().UTC()) {
		return "expired"
	}
	return "pending"
}

// inviteToResponse converts a db.InviteToken to its list wire representation.
// The token_hash field is intentionally excluded.
func inviteToResponse(t *db.InviteToken) inviteResponse {
	return inviteResponse{
		ID:        t.ID,
		TokenHint: t.TokenHint,
		Email:     t.Email,
		Role:      t.Role,
		OrgID:     t.OrgID,
		Status:    inviteStatus(t),
		ExpiresAt: t.ExpiresAt,
		CreatedBy: t.CreatedBy,
		CreatedAt: t.CreatedAt,
	}
}

// CreateInvite handles POST /api/v1/orgs/:org_id/invites.
// System admins and org admins may invite members. Only system admins may
// invite with role org_admin. The plaintext token is returned exactly once.
//
// @Summary      Create an invite
// @Description  Generates an invite token for the given email. Only system admins may invite with role org_admin. The plaintext token is returned exactly once.
// @Tags         invites
// @Accept       json
// @Produce      json
// @Param        org_id  path      string                true  "Organization ID"
// @Param        body    body      createInviteRequest   true  "Invite parameters"
// @Success      201     {object}  createInviteResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/invites [post]
func (h *Handler) CreateInvite(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	var req createInviteRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if req.Email == "" || !strings.Contains(req.Email, "@") {
		return apierror.BadRequest(c, "email is required and must be a valid email address")
	}
	if req.Role == "" {
		req.Role = "member"
	}
	if req.Role != "member" && req.Role != "org_admin" {
		return apierror.BadRequest(c, "role must be member or org_admin")
	}
	if req.Role == "org_admin" && !auth.HasRole(keyInfo.Role, auth.RoleSystemAdmin) {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "only system admins may invite org admins")
	}

	ctx := c.Context()

	// Revoke any existing unredeemed invites for this email before creating
	// the new one. If creation subsequently fails, the old invite is already
	// gone — the admin can simply re-invite. This is an acceptable trade-off
	// because the alternative (create-then-revoke) would fail against the
	// UNIQUE index on (org_id, email) for unredeemed tokens.
	if err := h.DB.RevokeInviteTokensByEmail(ctx, orgID, req.Email); err != nil {
		h.Log.ErrorContext(ctx, "create invite: revoke existing invites",
			slog.String("org_id", orgID),
			slog.String("error", err.Error()),
		)
		return apierror.InternalError(c, "failed to create invite")
	}

	plaintextToken, err := keygen.Generate(keygen.KeyTypeInvite)
	if err != nil {
		h.Log.ErrorContext(ctx, "create invite: generate token", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to create invite")
	}

	tokenHash := keygen.Hash(plaintextToken, h.HMACSecret)
	tokenHint := keygen.Hint(plaintextToken)
	expiresAt := time.Now().UTC().Add(7 * 24 * time.Hour).Format(time.RFC3339)

	invite, err := h.DB.CreateInviteToken(ctx, db.CreateInviteTokenParams{
		TokenHash: tokenHash,
		TokenHint: tokenHint,
		OrgID:     orgID,
		Email:     req.Email,
		Role:      req.Role,
		ExpiresAt: expiresAt,
		CreatedBy: keyInfo.UserID,
	})
	if err != nil {
		h.Log.ErrorContext(ctx, "create invite: db insert", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to create invite")
	}

	return c.Status(fiber.StatusCreated).JSON(createInviteResponse{
		ID:        invite.ID,
		Token:     plaintextToken,
		TokenHint: invite.TokenHint,
		Email:     invite.Email,
		Role:      invite.Role,
		OrgID:     invite.OrgID,
		ExpiresAt: invite.ExpiresAt,
		CreatedAt: invite.CreatedAt,
	})
}

// ListInvites handles GET /api/v1/orgs/:org_id/invites.
// Returns a paginated list of invite tokens for the organization, with a
// computed status field for each entry.
//
// @Summary      List invites
// @Description  Returns a cursor-paginated list of invite tokens for the organization, with computed status (pending, expired, redeemed).
// @Tags         invites
// @Produce      json
// @Param        org_id  path      string  true   "Organization ID"
// @Param        limit   query     int     false  "Page size (default 20, max 100)"
// @Param        cursor  query     string  false  "Pagination cursor (UUIDv7 of the last seen invite)"
// @Success      200     {object}  paginatedInvitesResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/invites [get]
func (h *Handler) ListInvites(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	p, err := parsePagination(c)
	if err != nil {
		return apierror.BadRequest(c, err.Error())
	}

	tokens, err := h.DB.ListInviteTokens(c.Context(), orgID, p.Cursor, p.Limit+1)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list invites", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list invites")
	}

	hasMore := len(tokens) > p.Limit
	if hasMore {
		tokens = tokens[:p.Limit]
	}

	resp := paginatedInvitesResponse{
		Data:    make([]inviteResponse, len(tokens)),
		HasMore: hasMore,
	}
	for i := range tokens {
		resp.Data[i] = inviteToResponse(&tokens[i])
	}
	if hasMore && len(tokens) > 0 {
		resp.Cursor = tokens[len(tokens)-1].ID
	}

	return c.JSON(resp)
}

// RevokeInvite handles DELETE /api/v1/orgs/:org_id/invites/:invite_id.
// Hard-deletes the invite token. Returns 204 on success.
//
// @Summary      Revoke an invite
// @Description  Permanently deletes the invite token, preventing redemption.
// @Tags         invites
// @Produce      json
// @Param        org_id     path  string  true  "Organization ID"
// @Param        invite_id  path  string  true  "Invite ID"
// @Success      204        "No Content"
// @Failure      401        {object}  swaggerErrorResponse
// @Failure      403        {object}  swaggerErrorResponse
// @Failure      404        {object}  swaggerErrorResponse
// @Failure      500        {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/invites/{invite_id} [delete]
func (h *Handler) RevokeInvite(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	inviteID := c.Params("invite_id")

	if _, ok := requireOrgAccess(c, orgID); !ok {
		return nil
	}

	if err := h.DB.RevokeInviteToken(c.Context(), inviteID, orgID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "invite not found")
		}
		h.Log.ErrorContext(c.Context(), "revoke invite", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to revoke invite")
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// inviteInvalidMsg is the single error message returned by public invite
// endpoints for all invalid-token conditions. A uniform message prevents
// callers from distinguishing not-found, already-redeemed, and expired states.
const inviteInvalidMsg = "invite is no longer valid"

// PeekInvite handles GET /api/v1/invites/peek.
// This is a public endpoint — no authentication required.
// Accepts token query parameter and returns enough metadata for the
// registration UI to display the invite details without exposing sensitive data.
//
// @Summary      Peek at an invite
// @Description  Returns invite metadata (org name, role, expiry) for display in the registration UI. No authentication required.
// @Tags         invites
// @Produce      json
// @Param        token  query     string  true  "Invite token"
// @Success      200    {object}  peekInviteResponse
// @Failure      400    {object}  swaggerErrorResponse
// @Failure      410    {object}  swaggerErrorResponse
// @Failure      500    {object}  swaggerErrorResponse
// @Router       /invites/peek [get]
func (h *Handler) PeekInvite(c fiber.Ctx) error {
	rawToken := c.Query("token")
	if rawToken == "" {
		return apierror.BadRequest(c, "token query parameter is required")
	}

	ctx := c.Context()
	tokenHash := keygen.Hash(rawToken, h.HMACSecret)

	invite, err := h.DB.GetInviteTokenByHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.Send(c, fiber.StatusGone, "gone", inviteInvalidMsg)
		}
		h.Log.ErrorContext(ctx, "peek invite: get by hash", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to look up invite")
	}

	if invite.RedeemedAt != nil {
		return apierror.Send(c, fiber.StatusGone, "gone", inviteInvalidMsg)
	}

	exp, err := time.Parse(time.RFC3339, invite.ExpiresAt)
	if err != nil || exp.Before(time.Now().UTC()) {
		return apierror.Send(c, fiber.StatusGone, "gone", inviteInvalidMsg)
	}

	org, err := h.DB.GetOrg(ctx, invite.OrgID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.Send(c, fiber.StatusGone, "gone", inviteInvalidMsg)
		}
		h.Log.ErrorContext(ctx, "peek invite: get org", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to look up invite")
	}

	return c.JSON(peekInviteResponse{
		Email:     invite.Email,
		OrgName:   org.Name,
		Role:      invite.Role,
		ExpiresAt: invite.ExpiresAt,
	})
}

// RedeemInvite handles POST /api/v1/invites/redeem.
// This is a public endpoint — no authentication required.
// Creates a new user account and org membership from a valid invite token,
// then marks the invite as redeemed.
//
// @Summary      Redeem an invite
// @Description  Creates a new user account and org membership from a valid invite token. No authentication required.
// @Tags         invites
// @Accept       json
// @Produce      json
// @Param        body  body      redeemInviteRequest   true  "Redemption parameters"
// @Success      201   {object}  redeemInviteResponse
// @Failure      400   {object}  swaggerErrorResponse
// @Failure      409   {object}  swaggerErrorResponse
// @Failure      500   {object}  swaggerErrorResponse
// @Router       /invites/redeem [post]
func (h *Handler) RedeemInvite(c fiber.Ctx) error {
	var req redeemInviteRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if req.Token == "" {
		return apierror.BadRequest(c, "token is required")
	}
	if req.Password == "" {
		return apierror.BadRequest(c, "password is required")
	}
	if len(req.Password) < 8 {
		return apierror.BadRequest(c, "password must be at least 8 characters")
	}
	if req.DisplayName == "" {
		return apierror.BadRequest(c, "display_name is required")
	}

	ctx := c.Context()
	tokenHash := keygen.Hash(req.Token, h.HMACSecret)

	invite, err := h.DB.GetInviteTokenByHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.BadRequest(c, inviteInvalidMsg)
		}
		h.Log.ErrorContext(ctx, "redeem invite: get by hash", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to redeem invite")
	}

	if invite.RedeemedAt != nil {
		return apierror.BadRequest(c, inviteInvalidMsg)
	}

	exp, parseErr := time.Parse(time.RFC3339, invite.ExpiresAt)
	if parseErr != nil || exp.Before(time.Now().UTC()) {
		return apierror.BadRequest(c, inviteInvalidMsg)
	}

	passwordHashBytes, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		h.Log.ErrorContext(ctx, "redeem invite: hash password", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to redeem invite")
	}
	passwordHash := string(passwordHashBytes)

	// Mark the token as redeemed before creating the user. This prevents a
	// concurrent request from redeeming the same token simultaneously: the DB
	// constraint (redeemed_at IS NULL) ensures only one request can win. If
	// user or membership creation subsequently fails, the token is burned and
	// the admin must re-invite — this is preferable to a race where two
	// accounts are created for the same invite.
	if err := h.DB.RedeemInviteToken(ctx, invite.ID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			// Another concurrent request redeemed the token just ahead of us.
			return apierror.BadRequest(c, inviteInvalidMsg)
		}
		h.Log.ErrorContext(ctx, "redeem invite: mark redeemed", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to redeem invite")
	}

	user, err := h.DB.CreateUser(ctx, db.CreateUserParams{
		Email:        invite.Email,
		DisplayName:  req.DisplayName,
		PasswordHash: &passwordHash,
		AuthProvider: "local",
	})
	if err != nil {
		if errors.Is(err, db.ErrConflict) {
			return apierror.Conflict(c, "email already registered")
		}
		h.Log.ErrorContext(ctx, "redeem invite: create user", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to redeem invite")
	}

	_, err = h.DB.CreateOrgMembership(ctx, db.CreateOrgMembershipParams{
		OrgID:  invite.OrgID,
		UserID: user.ID,
		Role:   invite.Role,
	})
	if err != nil {
		h.Log.ErrorContext(ctx, "redeem invite: create org membership", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to redeem invite")
	}

	return c.Status(fiber.StatusCreated).JSON(redeemInviteResponse{
		ID:          user.ID,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		OrgID:       invite.OrgID,
		Role:        invite.Role,
	})
}
