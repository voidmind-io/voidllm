package admin

import (
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
	"golang.org/x/crypto/bcrypt"

	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/audit"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// marshalDescription serializes a map to compact JSON for audit event descriptions.
func marshalDescription(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// dummyHash is used to burn CPU time on failed login paths, preventing
// timing-based user enumeration (valid email ~100ms vs invalid email ~0ms).
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("void-dummy-timing-pad"), bcrypt.DefaultCost)

// loginRequest is the JSON body for POST /api/v1/auth/login.
type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// meResponse is the JSON body returned for the authenticated user's profile.
type meResponse struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	DisplayName   string `json:"display_name"`
	Role          string `json:"role"`
	OrgID         string `json:"org_id,omitempty"`
	IsSystemAdmin bool   `json:"is_system_admin"`
}

// loginResponse is the JSON body returned on successful authentication.
type loginResponse struct {
	Token     string     `json:"token"`
	ExpiresAt string     `json:"expires_at"`
	User      meResponse `json:"user"`
}

// Login handles POST /api/v1/auth/login. It verifies email and password and
// returns a short-lived session token valid for 24 hours. The session token is
// a real api_keys row and works with the existing auth middleware.
// This endpoint does not require prior authentication.
//
// @Summary      Authenticate with email and password
// @Description  Verifies credentials and returns a 24-hour session token. Revokes any existing session for the user.
// @Tags         auth
// @Accept       json
// @Produce      json
// @Param        body  body      loginRequest  true  "Login credentials"
// @Success      200   {object}  loginResponse
// @Failure      400   {object}  swaggerErrorResponse
// @Failure      401   {object}  swaggerErrorResponse
// @Failure      500   {object}  swaggerErrorResponse
// @Router       /auth/login [post]
func (h *Handler) Login(c fiber.Ctx) error {
	var req loginRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.Email == "" {
		return apierror.BadRequest(c, "email is required")
	}
	if req.Password == "" {
		return apierror.BadRequest(c, "password is required")
	}

	ctx := c.Context()

	userID, hash, err := h.DB.GetUserPasswordHash(ctx, req.Email)
	if err != nil {
		// Burn bcrypt time to prevent timing-based email enumeration.
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(req.Password))
		if errors.Is(err, db.ErrNotFound) || errors.Is(err, db.ErrNoPassword) {
			return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "invalid email or password")
		}
		h.Log.ErrorContext(ctx, "login: get user password hash", slog.String("error", err.Error()))
		return apierror.InternalError(c, "authentication failed")
	}

	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(req.Password)); err != nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "invalid email or password")
	}

	role, orgID, err := h.DB.ResolveUserRole(ctx, userID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "user has no organization membership")
		}
		h.Log.ErrorContext(ctx, "login: resolve user role", slog.String("error", err.Error()))
		return apierror.InternalError(c, "authentication failed")
	}

	// Revoke previous session keys for this user so only one session exists.
	if err := h.DB.RevokeUserSessions(ctx, userID); err != nil {
		h.Log.ErrorContext(ctx, "login: revoke old sessions", slog.String("error", err.Error()))
		// Non-fatal: proceed with login even if cleanup fails.
	}

	key, err := keygen.Generate(keygen.KeyTypeSession)
	if err != nil {
		h.Log.ErrorContext(ctx, "login: generate session key", slog.String("error", err.Error()))
		return apierror.InternalError(c, "authentication failed")
	}

	keyHash := keygen.Hash(key, h.HMACSecret)
	keyHint := keygen.Hint(key)
	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	expiresAtStr := expiresAt.Format(time.RFC3339)

	apiKey, err := h.DB.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:   keyHash,
		KeyHint:   keyHint,
		KeyType:   keygen.KeyTypeSession,
		Name:      "Login session",
		OrgID:     orgID,
		UserID:    &userID,
		ExpiresAt: &expiresAtStr,
		CreatedBy: userID,
	})
	if err != nil {
		h.Log.ErrorContext(ctx, "login: create api key", slog.String("error", err.Error()))
		return apierror.InternalError(c, "authentication failed")
	}

	h.KeyCache.Set(keyHash, auth.KeyInfo{
		ID:        apiKey.ID,
		KeyType:   keygen.KeyTypeSession,
		Role:      role,
		OrgID:     orgID,
		UserID:    userID,
		Name:      "Login session",
		ExpiresAt: &expiresAt,
	})

	user, err := h.DB.GetUser(ctx, userID)
	if err != nil {
		h.Log.ErrorContext(ctx, "login: get user", slog.String("error", err.Error()))
		return apierror.InternalError(c, "authentication failed")
	}

	if h.AuditLogger != nil {
		h.AuditLogger.Log(audit.Event{
			Timestamp:    time.Now().UTC(),
			OrgID:        orgID,
			ActorID:      user.ID,
			ActorType:    "user",
			ActorKeyID:   apiKey.ID,
			Action:       "login",
			ResourceType: "session",
			ResourceID:   apiKey.ID,
			Description:  marshalDescription(map[string]string{"email": req.Email}),
			IPAddress:    c.IP(),
			StatusCode:   fiber.StatusOK,
		})
	}

	return c.Status(fiber.StatusOK).JSON(loginResponse{
		Token:     key,
		ExpiresAt: expiresAtStr,
		User: meResponse{
			ID:            user.ID,
			Email:         user.Email,
			DisplayName:   user.DisplayName,
			Role:          role,
			OrgID:         orgID,
			IsSystemAdmin: user.IsSystemAdmin,
		},
	})
}

// availableModelsResponse is the JSON body returned by AvailableModels.
type availableModelsResponse struct {
	Models []string `json:"models"`
}

// AvailableModels handles GET /api/v1/me/available-models.
// It returns the list of model names accessible to the current key's scope,
// respecting the org → team → key access hierarchy enforced by the access cache.
// Any authenticated key may call this endpoint — no additional role is required.
//
// @Summary      List models available to the authenticated key
// @Description  Returns model names accessible to the caller's org, team, and key scope.
// @Tags         auth
// @Produce      json
// @Success      200  {object}  availableModelsResponse
// @Failure      401  {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /me/available-models [get]
func (h *Handler) AvailableModels(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}

	allModels := h.Registry.ListInfo()

	names := make([]string, 0, len(allModels))
	for _, m := range allModels {
		if h.AccessCache == nil || h.AccessCache.Check(keyInfo.OrgID, keyInfo.TeamID, keyInfo.ID, m.Name) {
			names = append(names, m.Name)
		}
	}

	return c.JSON(availableModelsResponse{Models: names})
}

// Me returns the authenticated user's profile.
//
// @Summary      Get authenticated user profile
// @Description  Returns the profile of the user associated with the current session key.
// @Tags         auth
// @Produce      json
// @Success      200  {object}  meResponse
// @Failure      400  {object}  swaggerErrorResponse
// @Failure      401  {object}  swaggerErrorResponse
// @Failure      404  {object}  swaggerErrorResponse
// @Failure      500  {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /me [get]
func (h *Handler) Me(c fiber.Ctx) error {
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo == nil {
		return apierror.Send(c, fiber.StatusUnauthorized, "unauthorized", "missing authentication")
	}
	if keyInfo.UserID == "" {
		return apierror.Send(c, fiber.StatusBadRequest, "bad_request", "this endpoint requires a user-scoped key")
	}
	user, err := h.DB.GetUser(c.Context(), keyInfo.UserID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "user not found")
		}
		h.Log.ErrorContext(c.Context(), "me: get user", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to retrieve user profile")
	}
	return c.JSON(meResponse{
		ID:            user.ID,
		Email:         user.Email,
		DisplayName:   user.DisplayName,
		Role:          keyInfo.Role,
		OrgID:         keyInfo.OrgID,
		IsSystemAdmin: user.IsSystemAdmin,
	})
}
