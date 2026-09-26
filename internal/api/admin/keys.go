package admin

import (
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

// createAPIKeyRequest is the JSON body accepted by CreateAPIKey.
type createAPIKeyRequest struct {
	Name              string  `json:"name"`
	KeyType           string  `json:"key_type"`
	TeamID            *string `json:"team_id"`
	UserID            *string `json:"user_id"`
	ServiceAccountID  *string `json:"service_account_id"`
	DailyTokenLimit   int64   `json:"daily_token_limit"`
	MonthlyTokenLimit int64   `json:"monthly_token_limit"`
	RequestsPerMinute int     `json:"requests_per_minute"`
	RequestsPerDay    int     `json:"requests_per_day"`
	ExpiresAt         *string `json:"expires_at"`
}

// updateAPIKeyRequest is the JSON body accepted by UpdateAPIKey.
// All fields are optional; a nil pointer means the field is left unchanged.
type updateAPIKeyRequest struct {
	Name              *string `json:"name"`
	DailyTokenLimit   *int64  `json:"daily_token_limit"`
	MonthlyTokenLimit *int64  `json:"monthly_token_limit"`
	RequestsPerMinute *int    `json:"requests_per_minute"`
	RequestsPerDay    *int    `json:"requests_per_day"`
	ExpiresAt         *string `json:"expires_at"`
}

// createAPIKeyResponse is the JSON representation returned by CreateAPIKey.
// It includes the plaintext key, which is shown exactly once at creation time.
type createAPIKeyResponse struct {
	ID                string  `json:"id"`
	Key               string  `json:"key"`
	KeyHint           string  `json:"key_hint"`
	KeyType           string  `json:"key_type"`
	Name              string  `json:"name"`
	OrgID             string  `json:"org_id"`
	TeamID            *string `json:"team_id,omitempty"`
	UserID            *string `json:"user_id,omitempty"`
	ServiceAccountID  *string `json:"service_account_id,omitempty"`
	DailyTokenLimit   int64   `json:"daily_token_limit"`
	MonthlyTokenLimit int64   `json:"monthly_token_limit"`
	RequestsPerMinute int     `json:"requests_per_minute"`
	RequestsPerDay    int     `json:"requests_per_day"`
	ExpiresAt         *string `json:"expires_at,omitempty"`
	CreatedBy         string  `json:"created_by"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

// apiKeyResponse is the JSON representation of an API key returned by GET and LIST operations.
// It never includes the plaintext key or the key_hash.
type apiKeyResponse struct {
	ID                string  `json:"id"`
	KeyHint           string  `json:"key_hint"`
	KeyType           string  `json:"key_type"`
	Name              string  `json:"name"`
	OrgID             string  `json:"org_id"`
	TeamID            *string `json:"team_id,omitempty"`
	UserID            *string `json:"user_id,omitempty"`
	ServiceAccountID  *string `json:"service_account_id,omitempty"`
	DailyTokenLimit   int64   `json:"daily_token_limit"`
	MonthlyTokenLimit int64   `json:"monthly_token_limit"`
	RequestsPerMinute int     `json:"requests_per_minute"`
	RequestsPerDay    int     `json:"requests_per_day"`
	ExpiresAt         *string `json:"expires_at,omitempty"`
	LastUsedAt        *string `json:"last_used_at,omitempty"`
	CreatedBy         string  `json:"created_by"`
	CreatedAt         string  `json:"created_at"`
	UpdatedAt         string  `json:"updated_at"`
}

// paginatedAPIKeysResponse wraps a page of API keys with pagination metadata.
type paginatedAPIKeysResponse struct {
	Data    []apiKeyResponse `json:"data"`
	HasMore bool             `json:"has_more"`
	Cursor  string           `json:"next_cursor,omitempty"`
}

// validKeyTypes is the set of accepted key_type values.
var validKeyTypes = map[string]bool{
	keygen.KeyTypeUser: true,
	keygen.KeyTypeTeam: true,
	keygen.KeyTypeSA:   true,
}

// apiKeyToResponse converts a db.APIKey to its API wire representation.
// The key_hash field is intentionally excluded.
func apiKeyToResponse(k *db.APIKey) apiKeyResponse {
	return apiKeyResponse{
		ID:                k.ID,
		KeyHint:           k.KeyHint,
		KeyType:           k.KeyType,
		Name:              k.Name,
		OrgID:             k.OrgID,
		TeamID:            k.TeamID,
		UserID:            k.UserID,
		ServiceAccountID:  k.ServiceAccountID,
		DailyTokenLimit:   k.DailyTokenLimit,
		MonthlyTokenLimit: k.MonthlyTokenLimit,
		RequestsPerMinute: k.RequestsPerMinute,
		RequestsPerDay:    k.RequestsPerDay,
		ExpiresAt:         k.ExpiresAt,
		LastUsedAt:        k.LastUsedAt,
		CreatedBy:         k.CreatedBy,
		CreatedAt:         k.CreatedAt,
		UpdatedAt:         k.UpdatedAt,
	}
}

// derefStr returns the string value pointed to by s, or "" if s is nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// errInvalidExpiresAt is returned by normalizeExpiresAt when the supplied
// value cannot be parsed as RFC3339. Its message is sent verbatim as the 400
// response body.
var errInvalidExpiresAt = errors.New("expires_at must be a valid RFC3339 timestamp")

// normalizeExpiresAt validates and canonicalizes a client-supplied expires_at
// value. A nil raw (the field was omitted from the request) returns nil,
// nil — the column is left unchanged. Any non-nil value, including an empty
// string, must parse as RFC3339(Nano); on success it is reformatted into the
// canonical UTC storage format (db.FormatTimestamp) so every stored expiry
// compares correctly as plain TEXT against other canonical timestamps. Past
// dates are accepted — setting expires_at to an already-past time is a valid
// way to revoke a key immediately.
func normalizeExpiresAt(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, *raw)
	if err != nil {
		return nil, errInvalidExpiresAt
	}
	formatted := db.FormatTimestamp(t)
	return &formatted, nil
}

// canSetKeyLimits reports whether the caller is permitted to set rate and
// token limits on the given key. Only org admins and system admins may set
// limits — team admins are never permitted, even on keys scoped to their own
// team, because a team-scoped key (team_key or team-bound sa_key) has no
// distinct owner and a team admin could otherwise raise or remove the limits
// on a key they themselves use.
//
// Human callers (a user key or session key, identified by a non-empty
// caller.UserID) with org_admin or system_admin role may set limits on any
// key in the org.
//
// Machine callers (caller.UserID == "") can only ever be an org-level
// service-account key. The role check above already excludes team-scoped
// sa_keys and team_keys, since auth.KeyInfoFromRecord resolves those to
// team_admin (derived from the owning service account's own team_id, not
// from caller.TeamID — the api_keys.team_id column is never populated on
// sa_key rows). So once the org_admin check has passed, a machine caller can
// only be an org-scoped sa_key, and no further role/team check is needed
// here. Such a caller may set limits on any other key in the org, but never
// on a key belonging to its own service account and never on itself, so a
// machine caller can never grant itself a higher or unlimited budget.
func canSetKeyLimits(caller *auth.KeyInfo, key *db.APIKey) bool {
	if !auth.HasRole(caller.Role, auth.RoleOrgAdmin) {
		return false
	}
	if caller.UserID != "" {
		return true
	}
	if caller.KeyType != keygen.KeyTypeSA {
		return false
	}
	if key.ServiceAccountID != nil && *key.ServiceAccountID == caller.ServiceAccountID {
		return false
	}
	if key.ID == caller.ID {
		return false
	}
	return true
}

// apiKeyVisibleToCallerKey reports whether the given API key is within the visible
// scope of the caller. Team admins may see keys scoped to their team or owned by
// themselves. Members may only see keys they own.
func apiKeyVisibleToCallerKey(key *db.APIKey, caller *auth.KeyInfo) bool {
	if auth.HasRole(caller.Role, auth.RoleTeamAdmin) {
		// Team admin with a team context sees team-scoped keys for their team.
		// A session key (no TeamID) falls through to the own-key check below.
		if caller.TeamID != "" && key.TeamID != nil && *key.TeamID == caller.TeamID {
			return true
		}
		if key.UserID != nil && *key.UserID == caller.UserID {
			return true
		}
		return false
	}
	// Member sees only own keys.
	return key.UserID != nil && *key.UserID == caller.UserID
}

// CreateAPIKey handles POST /api/v1/orgs/:org_id/keys.
// Org admins and system admins may create any key type.
// Members may only create user_key for themselves (user_id is forced to their own ID).
// The plaintext key is returned exactly once in the response body.
//
// @Summary      Create an API key
// @Description  Creates a new API key for the organization. Members may only create user keys for themselves. Rate and token limit fields may only be set by org admins or system admins; non-admin callers must omit or zero these fields. expires_at, when set, must be a valid RFC3339 timestamp; it is stored normalized to UTC. The plaintext key is returned exactly once.
// @Tags         keys
// @Accept       json
// @Produce      json
// @Param        org_id  path      string               true  "Organization ID"
// @Param        body    body      createAPIKeyRequest  true  "API key parameters"
// @Success      201     {object}  createAPIKeyResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/keys [post]
func (h *Handler) CreateAPIKey(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	if keyInfo.UserID == "" {
		return apierror.BadRequest(c, "keys can only be created by user keys")
	}

	var req createAPIKeyRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if req.Name == "" {
		return apierror.BadRequest(c, "name is required")
	}
	if !validKeyTypes[req.KeyType] {
		return apierror.BadRequest(c, "key_type must be one of: user_key, team_key, sa_key")
	}
	if req.DailyTokenLimit < 0 || req.MonthlyTokenLimit < 0 || req.RequestsPerMinute < 0 || req.RequestsPerDay < 0 {
		return apierror.BadRequest(c, "limit fields must be >= 0")
	}
	normalizedExpiresAt, err := normalizeExpiresAt(req.ExpiresAt)
	if err != nil {
		return apierror.BadRequest(c, err.Error())
	}
	req.ExpiresAt = normalizedExpiresAt

	// Members may only create user_key for themselves.
	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		if req.KeyType != keygen.KeyTypeUser {
			return apierror.Send(c, fiber.StatusForbidden, "forbidden", "you can only create user keys")
		}
		req.UserID = &keyInfo.UserID

		// Only org admins and system admins may set key limits at creation.
		// A non-admin who mints their own key must not be able to grant
		// themselves a higher (or unlimited) budget than an admin assigned.
		if req.DailyTokenLimit != 0 || req.MonthlyTokenLimit != 0 || req.RequestsPerMinute != 0 || req.RequestsPerDay != 0 {
			return apierror.Send(c, fiber.StatusForbidden, "forbidden", "only administrators can set key limits")
		}
	}

	switch req.KeyType {
	case keygen.KeyTypeUser:
		if req.UserID == nil || *req.UserID == "" {
			return apierror.BadRequest(c, "user_id is required for user_key")
		}
	case keygen.KeyTypeTeam:
		if req.TeamID == nil || *req.TeamID == "" {
			return apierror.BadRequest(c, "team_id is required for team_key")
		}
		if req.UserID != nil && *req.UserID != "" {
			return apierror.BadRequest(c, "user_id must not be set for team_key")
		}
		if req.ServiceAccountID != nil && *req.ServiceAccountID != "" {
			return apierror.BadRequest(c, "service_account_id must not be set for team_key")
		}
	case keygen.KeyTypeSA:
		if req.ServiceAccountID == nil || *req.ServiceAccountID == "" {
			return apierror.BadRequest(c, "service_account_id is required for sa_key")
		}
		if req.UserID != nil && *req.UserID != "" {
			return apierror.BadRequest(c, "user_id must not be set for sa_key")
		}
		if req.TeamID != nil && *req.TeamID != "" {
			return apierror.BadRequest(c, "team_id must not be set for sa_key")
		}
	}

	ctx := c.Context()

	// Verify referenced team belongs to this org.
	if req.TeamID != nil && *req.TeamID != "" {
		team, err := h.DB.GetTeam(ctx, *req.TeamID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return apierror.BadRequest(c, "team not found")
			}
			h.Log.ErrorContext(ctx, "create api key: get team", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to validate team")
		}
		if team.OrgID != orgID {
			return apierror.BadRequest(c, "team not found")
		}
	}

	// Verify referenced user exists and belongs to this org.
	if req.UserID != nil && *req.UserID != "" {
		if _, err := h.DB.GetUser(ctx, *req.UserID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return apierror.BadRequest(c, "user not found")
			}
			h.Log.ErrorContext(ctx, "create api key: get user", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to validate user")
		}
		if _, err := h.DB.GetUserOrgRole(ctx, *req.UserID, orgID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return apierror.BadRequest(c, "user is not a member of this organization")
			}
			h.Log.ErrorContext(ctx, "create api key: verify user org membership", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to validate user")
		}
	}

	// Verify team membership when a team_id is specified.
	// org_admin and system_admin can create keys for any team in the org.
	// member and team_admin must be a member of the specified team.
	if req.TeamID != nil && *req.TeamID != "" && !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		callerID := keyInfo.UserID
		if callerID == "" {
			callerID = keyInfo.ServiceAccountID
		}
		if callerID != "" {
			isMember, err := h.DB.IsTeamMember(ctx, callerID, *req.TeamID)
			if err != nil {
				h.Log.ErrorContext(ctx, "create api key: check team membership", slog.String("error", err.Error()))
				return apierror.InternalError(c, "failed to validate team membership")
			}
			if !isMember {
				return apierror.Send(c, fiber.StatusForbidden, "forbidden", "you are not a member of the specified team")
			}
		}
	}

	// Verify referenced service account belongs to this org.
	if req.ServiceAccountID != nil && *req.ServiceAccountID != "" {
		sa, err := h.DB.GetServiceAccount(ctx, *req.ServiceAccountID)
		if err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return apierror.BadRequest(c, "service account not found")
			}
			h.Log.ErrorContext(ctx, "create api key: get service account", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to validate service account")
		}
		if sa.OrgID != orgID {
			return apierror.BadRequest(c, "service account not found")
		}
	}

	plaintextKey, err := keygen.Generate(req.KeyType)
	if err != nil {
		h.Log.ErrorContext(ctx, "create api key: generate", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to generate key")
	}

	keyHash := keygen.Hash(plaintextKey, h.HMACSecret)
	keyHint := keygen.Hint(plaintextKey)

	apiKey, err := h.DB.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:           keyHash,
		KeyHint:           keyHint,
		KeyType:           req.KeyType,
		Name:              req.Name,
		OrgID:             orgID,
		TeamID:            req.TeamID,
		UserID:            req.UserID,
		ServiceAccountID:  req.ServiceAccountID,
		DailyTokenLimit:   req.DailyTokenLimit,
		MonthlyTokenLimit: req.MonthlyTokenLimit,
		RequestsPerMinute: req.RequestsPerMinute,
		RequestsPerDay:    req.RequestsPerDay,
		ExpiresAt:         req.ExpiresAt,
		CreatedBy:         keyInfo.UserID,
	})
	if err != nil {
		h.Log.ErrorContext(ctx, "create api key: db insert", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to create api key")
	}

	// Populate the cache from a fresh read of the key with its full org, team,
	// and per-user membership limits resolved via JOIN, so the key is subject
	// to its complete limit hierarchy from the very first request instead of
	// waiting for the next periodic cache reload. A lookup or mapping failure
	// is logged and the cache write is skipped rather than failing the
	// request — the key was already created, and the next reload (at most
	// 30s later) will pick it up.
	rec, err := h.DB.LoadActiveKey(ctx, apiKey.ID)
	if err != nil {
		h.Log.ErrorContext(ctx, "create api key: load for cache", slog.String("error", err.Error()))
	} else if !auth.Cacheable(*rec) {
		h.Log.LogAttrs(ctx, slog.LevelWarn, "create api key: skipping non-cacheable key",
			slog.String("key_id", rec.ID),
			slog.String("key_type", rec.KeyType),
		)
	} else {
		ki, ok := auth.KeyInfoFromRecord(*rec)
		if !ok {
			h.Log.LogAttrs(ctx, slog.LevelWarn, "create api key: could not resolve a definite role, defaulting to member",
				slog.String("key_type", rec.KeyType),
				slog.String("key_id", rec.ID),
			)
		}
		h.KeyCache.Set(rec.KeyHash, ki)
	}

	resp := createAPIKeyResponse{
		ID:                apiKey.ID,
		Key:               plaintextKey,
		KeyHint:           apiKey.KeyHint,
		KeyType:           apiKey.KeyType,
		Name:              apiKey.Name,
		OrgID:             apiKey.OrgID,
		TeamID:            apiKey.TeamID,
		UserID:            apiKey.UserID,
		ServiceAccountID:  apiKey.ServiceAccountID,
		DailyTokenLimit:   apiKey.DailyTokenLimit,
		MonthlyTokenLimit: apiKey.MonthlyTokenLimit,
		RequestsPerMinute: apiKey.RequestsPerMinute,
		RequestsPerDay:    apiKey.RequestsPerDay,
		ExpiresAt:         apiKey.ExpiresAt,
		CreatedBy:         apiKey.CreatedBy,
		CreatedAt:         apiKey.CreatedAt,
		UpdatedAt:         apiKey.UpdatedAt,
	}
	return c.Status(fiber.StatusCreated).JSON(resp)
}

// GetAPIKey handles GET /api/v1/orgs/:org_id/keys/:key_id.
// Org admins see any key in the org. Team admins see keys belonging to their team
// or owned by themselves. Members see only their own keys.
// Returns 404 if the key belongs to a different org or the caller lacks access.
//
// @Summary      Get an API key
// @Description  Returns a single API key. Visibility is scoped by role: org admins see all keys, team admins see their team keys, members see only their own.
// @Tags         keys
// @Produce      json
// @Param        org_id  path      string  true  "Organization ID"
// @Param        key_id  path      string  true  "API key ID"
// @Success      200     {object}  apiKeyResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/keys/{key_id} [get]
func (h *Handler) GetAPIKey(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	keyID := c.Params("key_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	apiKey, err := h.DB.GetAPIKey(c.Context(), keyID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "api key not found")
		}
		h.Log.ErrorContext(c.Context(), "get api key", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get api key")
	}
	if apiKey.OrgID != orgID {
		return apierror.NotFound(c, "api key not found")
	}

	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		if !apiKeyVisibleToCallerKey(apiKey, keyInfo) {
			return apierror.NotFound(c, "api key not found")
		}
	}

	return c.JSON(apiKeyToResponse(apiKey))
}

// ListAPIKeys handles GET /api/v1/orgs/:org_id/keys.
// Org admins see all keys in the org. Team admins see keys scoped to their team
// plus their own user keys. Members see only their own keys.
// Accepts query parameters: limit, cursor, and include_deleted=true (system admin only).
//
// @Summary      List API keys
// @Description  Returns a cursor-paginated list of API keys. Scope is filtered by role.
// @Tags         keys
// @Produce      json
// @Param        org_id           path      string  true   "Organization ID"
// @Param        limit            query     int     false  "Page size (default 20, max 100)"
// @Param        cursor           query     string  false  "Pagination cursor (UUIDv7 of the last seen key)"
// @Param        include_deleted  query     bool    false  "Include soft-deleted keys (system admin only)"
// @Success      200              {object}  paginatedAPIKeysResponse
// @Failure      400              {object}  swaggerErrorResponse
// @Failure      401              {object}  swaggerErrorResponse
// @Failure      403              {object}  swaggerErrorResponse
// @Failure      500              {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/keys [get]
func (h *Handler) ListAPIKeys(c fiber.Ctx) error {
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

	// Determine scope filters based on role.
	var filterUserID, filterTeamID string
	switch {
	case auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin):
		// org_admin sees all keys — no additional filter.
	case auth.HasRole(keyInfo.Role, auth.RoleTeamAdmin):
		// team_admin sees keys for their team. Scoping by team_id covers both team
		// keys and user keys that belong to team members within that team.
		// A session key (no TeamID) falls back to showing only the caller's own keys.
		if keyInfo.TeamID != "" {
			filterTeamID = keyInfo.TeamID
		} else {
			filterUserID = keyInfo.UserID
		}
	default:
		// member sees only own keys.
		filterUserID = keyInfo.UserID
	}

	// Fail closed: below org_admin, the caller must always end up scoped by
	// some identifier of their own. A caller with neither filter — e.g. a
	// team-bound sa_key, whose KeyInfo.TeamID is never populated (see
	// auth.KeyInfoFromRecord) and which has no UserID either — would
	// otherwise hit ListAPIKeys with both filters empty, and an empty filter
	// means "no filter" at the DB layer, returning every key in the org.
	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) && filterUserID == "" && filterTeamID == "" {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "insufficient scope to list keys")
	}

	keys, err := h.DB.ListAPIKeys(c.Context(), orgID, filterUserID, filterTeamID, p.Cursor, p.Limit+1, includeDeleted)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "list api keys", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to list api keys")
	}

	hasMore := len(keys) > p.Limit
	if hasMore {
		keys = keys[:p.Limit]
	}

	resp := paginatedAPIKeysResponse{
		Data:    make([]apiKeyResponse, len(keys)),
		HasMore: hasMore,
	}
	for i := range keys {
		resp.Data[i] = apiKeyToResponse(&keys[i])
	}
	if hasMore && len(keys) > 0 {
		resp.Cursor = keys[len(keys)-1].ID
	}
	return c.JSON(resp)
}

// UpdateAPIKey handles PATCH /api/v1/orgs/:org_id/keys/:key_id.
// Org admins may update any key in the org. Team admins may update keys scoped
// to their team or owned by themselves. Members may only update their own keys.
// Only name, limits, and expires_at are updatable. Rate and token limit fields
// may only be changed by org admins and system admins — see canSetKeyLimits
// for the exact rule, including the restriction that an org-level
// service-account caller can never change its own limits or those of another
// key belonging to the same service account.
// Returns 404 if the key belongs to a different org or the caller lacks access.
//
// @Summary      Update an API key
// @Description  Updates name, rate limits, token limits, or expiry of an API key. Only provided fields are changed. Rate and token limit fields may only be changed by org admins or system admins — team admins can never change them, even on keys scoped to their own team. A machine caller (org-level service-account key) may set limits on other keys in the org but never on its own key or another key belonging to the same service account. Submitting a limit value unchanged from the current stored value is always allowed. expires_at, when set, must be a valid RFC3339 timestamp (including in the past, to revoke a key immediately) and is stored normalized to UTC.
// @Tags         keys
// @Accept       json
// @Produce      json
// @Param        org_id  path      string               true  "Organization ID"
// @Param        key_id  path      string               true  "API key ID"
// @Param        body    body      updateAPIKeyRequest  true  "Fields to update"
// @Success      200     {object}  apiKeyResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/keys/{key_id} [patch]
func (h *Handler) UpdateAPIKey(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	keyID := c.Params("key_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	existing, err := h.DB.GetAPIKey(c.Context(), keyID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "api key not found")
		}
		h.Log.ErrorContext(c.Context(), "update api key: get", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get api key")
	}
	if existing.OrgID != orgID {
		return apierror.NotFound(c, "api key not found")
	}

	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		if !apiKeyVisibleToCallerKey(existing, keyInfo) {
			return apierror.NotFound(c, "api key not found")
		}
	}

	var req updateAPIKeyRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
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
	normalizedExpiresAt, err := normalizeExpiresAt(req.ExpiresAt)
	if err != nil {
		return apierror.BadRequest(c, err.Error())
	}
	req.ExpiresAt = normalizedExpiresAt

	// Only admins (see canSetKeyLimits) may change a limit field's stored
	// value. Submitting the unchanged value back is always allowed so
	// idempotent clients that round-trip the full resource do not break.
	limitChanged := (req.DailyTokenLimit != nil && *req.DailyTokenLimit != existing.DailyTokenLimit) ||
		(req.MonthlyTokenLimit != nil && *req.MonthlyTokenLimit != existing.MonthlyTokenLimit) ||
		(req.RequestsPerMinute != nil && *req.RequestsPerMinute != existing.RequestsPerMinute) ||
		(req.RequestsPerDay != nil && *req.RequestsPerDay != existing.RequestsPerDay)
	if limitChanged && !canSetKeyLimits(keyInfo, existing) {
		return apierror.Send(c, fiber.StatusForbidden, "forbidden", "only administrators can set key limits")
	}
	if !canSetKeyLimits(keyInfo, existing) {
		// The caller may not set limits at all. Even though limitChanged is
		// false here (the 403 above would otherwise have fired), the request
		// may still carry the current, unchanged limit values. Drop them
		// before building the DB params so this write can never clobber a
		// concurrent admin's limit change with a stale value the caller
		// merely echoed back — a nil pointer means "leave this column alone".
		req.DailyTokenLimit = nil
		req.MonthlyTokenLimit = nil
		req.RequestsPerMinute = nil
		req.RequestsPerDay = nil
	}

	apiKey, err := h.DB.UpdateAPIKey(c.Context(), existing.ID, db.UpdateAPIKeyParams{
		Name:              req.Name,
		DailyTokenLimit:   req.DailyTokenLimit,
		MonthlyTokenLimit: req.MonthlyTokenLimit,
		RequestsPerMinute: req.RequestsPerMinute,
		RequestsPerDay:    req.RequestsPerDay,
		ExpiresAt:         req.ExpiresAt,
	})
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "api key not found")
		}
		h.Log.ErrorContext(c.Context(), "update api key", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to update api key")
	}

	// Refresh the cache entry from a fresh DB read (with org, team, and
	// per-user membership limits resolved via JOIN), the same approach used
	// by CreateAPIKey, rather than patching the previously cached KeyInfo in
	// place. A patch-in-place cannot re-evaluate Cacheable: if the key's
	// owning user or membership state changed since it was last cached, a
	// Get/modify/Set sequence would keep a key cached that should have been
	// evicted. LoadActiveKey excludes expired keys, so a request that just
	// patched expires_at into the past gets db.ErrNotFound here instead of a
	// record — that case evicts the existing cache entry (and publishes a
	// Redis invalidation, same as DeleteAPIKey) directly rather than falling
	// into the generic error-log branch below, so the key dies immediately
	// instead of staying usable until the next periodic reload. Any other
	// lookup or mapping failure is logged and the cache is left as-is rather
	// than failing the request — the update itself already succeeded, and
	// the next periodic reload (at most 30s later) will reconcile the cache
	// regardless.
	rec, err := h.DB.LoadActiveKey(c.Context(), existing.ID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			h.KeyCache.Delete(existing.KeyHash)
			if h.Redis != nil {
				if pubErr := h.Redis.PublishInvalidation(c.Context(), voidredis.ChannelKeys, existing.KeyHash); pubErr != nil {
					h.Log.LogAttrs(c.Context(), slog.LevelWarn, "redis: publish key invalidation failed",
						slog.String("error", pubErr.Error()),
					)
				}
			}
		} else {
			h.Log.ErrorContext(c.Context(), "update api key: load for cache", slog.String("error", err.Error()))
		}
	} else if !auth.Cacheable(*rec) {
		h.Log.LogAttrs(c.Context(), slog.LevelWarn, "update api key: evicting non-cacheable key",
			slog.String("key_id", rec.ID),
			slog.String("key_type", rec.KeyType),
		)
		h.KeyCache.Delete(rec.KeyHash)
		if h.Redis != nil {
			if pubErr := h.Redis.PublishInvalidation(c.Context(), voidredis.ChannelKeys, rec.KeyHash); pubErr != nil {
				h.Log.LogAttrs(c.Context(), slog.LevelWarn, "redis: publish key invalidation failed",
					slog.String("error", pubErr.Error()),
				)
			}
		}
	} else {
		ki, ok := auth.KeyInfoFromRecord(*rec)
		if !ok {
			h.Log.LogAttrs(c.Context(), slog.LevelWarn, "update api key: could not resolve a definite role, defaulting to member",
				slog.String("key_type", rec.KeyType),
				slog.String("key_id", rec.ID),
			)
		}
		h.KeyCache.Set(rec.KeyHash, ki)
	}

	return c.JSON(apiKeyToResponse(apiKey))
}

// rotateKeyGracePeriod is the duration an old key remains valid after rotation.
// During this window callers have time to propagate the new key before the old one stops working.
const rotateKeyGracePeriod = 24 * time.Hour

// rotateKeyResponse is returned by RotateAPIKey. It contains metadata for both
// the newly issued key and the old key that is now in its grace period.
type rotateKeyResponse struct {
	NewKey rotatedKeyInfo `json:"new_key"`
	OldKey rotatedKeyInfo `json:"old_key"`
}

// rotatedKeyInfo is a compact key descriptor used inside rotateKeyResponse.
// Key (plaintext) is only set on the new key; it is empty for the old key.
type rotatedKeyInfo struct {
	ID        string  `json:"id"`
	Key       string  `json:"key,omitempty"`
	Hint      string  `json:"hint"`
	ExpiresAt *string `json:"expires_at,omitempty"`
}

// RotateAPIKey handles POST /api/v1/orgs/:org_id/keys/:key_id/rotate.
// It generates a new API key with the same metadata as the existing key and
// sets the old key to expire after a 24-hour grace period. Org admins and
// system admins may rotate any key; members may only rotate their own keys.
//
// @Summary      Rotate an API key
// @Description  Generates a new key with the same metadata as the existing one and shortens the old key's lifetime to a 24-hour grace period. Members may only rotate their own keys.
// @Tags         keys
// @Produce      json
// @Param        org_id  path      string  true  "Organization ID"
// @Param        key_id  path      string  true  "Key ID to rotate"
// @Success      200     {object}  rotateKeyResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/keys/{key_id}/rotate [post]
func (h *Handler) RotateAPIKey(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	keyID := c.Params("key_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	ctx := c.Context()

	existing, err := h.DB.GetAPIKey(ctx, keyID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "api key not found")
		}
		h.Log.ErrorContext(ctx, "rotate api key: get", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get api key")
	}

	if existing.OrgID != orgID {
		return apierror.NotFound(c, "api key not found")
	}

	// Only user, team, and service account keys can be rotated.
	// Session and invite keys are ephemeral and must not be rotated.
	switch existing.KeyType {
	case keygen.KeyTypeUser, keygen.KeyTypeTeam, keygen.KeyTypeSA:
		// allowed
	default:
		return apierror.BadRequest(c, "only user, team, and service account keys can be rotated")
	}

	// A sa_key can only be rotated while its owning service account still
	// exists and is not soft-deleted — GetServiceAccount filters both cases
	// to ErrNotFound. Without this check, rotating a key whose service
	// account was deleted after the key was minted would mint a brand-new,
	// fully active key for a service account that no longer exists.
	if existing.KeyType == keygen.KeyTypeSA {
		if existing.ServiceAccountID == nil || *existing.ServiceAccountID == "" {
			h.Log.ErrorContext(ctx, "rotate api key: sa_key has no service_account_id", slog.String("key_id", existing.ID))
			return apierror.InternalError(c, "failed to rotate key")
		}
		if _, err := h.DB.GetServiceAccount(ctx, *existing.ServiceAccountID); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				return apierror.BadRequest(c, "service account not found")
			}
			h.Log.ErrorContext(ctx, "rotate api key: get service account", slog.String("error", err.Error()))
			return apierror.InternalError(c, "failed to validate service account")
		}
	}

	// Members and team admins may only rotate keys within their scope; org_admin+ may rotate any key.
	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		if !apiKeyVisibleToCallerKey(existing, keyInfo) {
			return apierror.Send(c, fiber.StatusForbidden, "forbidden", "you can only rotate keys within your scope")
		}
	}

	plaintextKey, err := keygen.Generate(existing.KeyType)
	if err != nil {
		h.Log.ErrorContext(ctx, "rotate api key: generate", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to generate key")
	}

	keyHash := keygen.Hash(plaintextKey, h.HMACSecret)
	keyHint := keygen.Hint(plaintextKey)
	rotatedName := strings.TrimSuffix(existing.Name, " (rotated)") + " (rotated)"

	// Set the old key to expire after the grace period. If it already has an
	// expiry that is sooner than the grace period deadline, keep that shorter expiry.
	graceDeadline := time.Now().UTC().Add(rotateKeyGracePeriod)
	oldExpiresAt := graceDeadline.Format(time.RFC3339)
	if existing.ExpiresAt != nil {
		if t, parseErr := time.Parse(time.RFC3339, *existing.ExpiresAt); parseErr == nil && t.Before(graceDeadline) {
			oldExpiresAt = *existing.ExpiresAt
		}
	}

	// INSERT the new key and UPDATE the old key's expiry atomically so that a
	// crash between the two writes cannot leave both keys permanently valid.
	rotated, err := h.DB.RotateKeyTx(ctx, existing.ID, oldExpiresAt, db.CreateAPIKeyParams{
		KeyHash:           keyHash,
		KeyHint:           keyHint,
		KeyType:           existing.KeyType,
		Name:              rotatedName,
		OrgID:             existing.OrgID,
		TeamID:            existing.TeamID,
		UserID:            existing.UserID,
		ServiceAccountID:  existing.ServiceAccountID,
		DailyTokenLimit:   existing.DailyTokenLimit,
		MonthlyTokenLimit: existing.MonthlyTokenLimit,
		RequestsPerMinute: existing.RequestsPerMinute,
		RequestsPerDay:    existing.RequestsPerDay,
		ExpiresAt:         existing.ExpiresAt,
		CreatedBy:         keyInfo.UserID,
	})
	if err != nil {
		h.Log.ErrorContext(ctx, "rotate api key: rotate tx", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to rotate key")
	}
	newKey := rotated.NewKey
	updatedOld := rotated.OldKey

	// Populate the cache from a fresh read of the new key with its full org,
	// team, and per-user membership limits resolved via JOIN — the same
	// approach used by CreateAPIKey — so the rotated key is subject to its
	// complete limit hierarchy immediately. A lookup or mapping failure is
	// logged and the cache write is skipped rather than failing the request;
	// the next periodic reload (at most 30s later) will pick it up.
	newRec, err := h.DB.LoadActiveKey(ctx, newKey.ID)
	if err != nil {
		h.Log.ErrorContext(ctx, "rotate api key: load for cache", slog.String("error", err.Error()))
	} else if !auth.Cacheable(*newRec) {
		h.Log.LogAttrs(ctx, slog.LevelWarn, "rotate api key: skipping non-cacheable key",
			slog.String("key_id", newRec.ID),
			slog.String("key_type", newRec.KeyType),
		)
	} else {
		ki, ok := auth.KeyInfoFromRecord(*newRec)
		if !ok {
			h.Log.LogAttrs(ctx, slog.LevelWarn, "rotate api key: could not resolve a definite role, defaulting to member",
				slog.String("key_type", newRec.KeyType),
				slog.String("key_id", newRec.ID),
			)
		}
		h.KeyCache.Set(newRec.KeyHash, ki)
	}

	// Update old key's expiry in cache so the proxy enforces the grace period.
	// Use oldExpiresAt (which is the min of the grace deadline and any existing
	// expiry) rather than graceDeadline directly, so the cache stays consistent
	// with what was written to the database.
	if cached, hit := h.KeyCache.Get(existing.KeyHash); hit {
		if t, parseErr := time.Parse(time.RFC3339, oldExpiresAt); parseErr == nil {
			cached.ExpiresAt = &t
			h.KeyCache.Set(existing.KeyHash, cached)
		}
	}

	// Publish invalidation for the old key so other nodes update its expiry.
	// NOTE: The new key is NOT published because the Redis invalidation channel
	// triggers cache eviction, not cache priming. Other nodes will discover the
	// new key on the next full cache refresh cycle. This is a known limitation
	// of the current architecture — acceptable for single-instance deployments.
	if h.Redis != nil {
		if err := h.Redis.PublishInvalidation(ctx, voidredis.ChannelKeys, existing.KeyHash); err != nil {
			h.Log.LogAttrs(ctx, slog.LevelWarn, "redis: publish key invalidation failed",
				slog.String("error", err.Error()),
			)
		}
	}

	resp := rotateKeyResponse{
		NewKey: rotatedKeyInfo{
			ID:        newKey.ID,
			Key:       plaintextKey,
			Hint:      newKey.KeyHint,
			ExpiresAt: newKey.ExpiresAt,
		},
		OldKey: rotatedKeyInfo{
			ID:        updatedOld.ID,
			Hint:      updatedOld.KeyHint,
			ExpiresAt: updatedOld.ExpiresAt,
		},
	}
	return c.JSON(resp)
}

// DeleteAPIKey handles DELETE /api/v1/orgs/:org_id/keys/:key_id.
// Org admins may delete any key in the org. Members may only delete their own keys.
// Deletion is a soft-delete. The key is also removed from the in-memory cache so it
// is immediately rejected by the proxy.
// Returns 404 if the key belongs to a different org or the caller lacks access.
//
// @Summary      Delete an API key
// @Description  Soft-deletes the API key and immediately evicts it from the auth cache. The key will be rejected by the proxy without delay.
// @Tags         keys
// @Produce      json
// @Param        org_id  path  string  true  "Organization ID"
// @Param        key_id  path  string  true  "API key ID"
// @Success      204     "No Content"
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/keys/{key_id} [delete]
func (h *Handler) DeleteAPIKey(c fiber.Ctx) error {
	orgID := c.Params("org_id")
	keyID := c.Params("key_id")

	keyInfo, ok := requireOrgAccess(c, orgID)
	if !ok {
		return nil
	}

	apiKey, err := h.DB.GetAPIKey(c.Context(), keyID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "api key not found")
		}
		h.Log.ErrorContext(c.Context(), "delete api key: get", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get api key")
	}
	if apiKey.OrgID != orgID {
		return apierror.NotFound(c, "api key not found")
	}

	if !auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		if apiKey.UserID == nil || *apiKey.UserID != keyInfo.UserID {
			return apierror.Send(c, fiber.StatusForbidden, "forbidden", "you can only delete your own keys")
		}
	}

	if err := h.DB.DeleteAPIKey(c.Context(), apiKey.ID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "api key not found")
		}
		h.Log.ErrorContext(c.Context(), "delete api key", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to delete api key")
	}

	h.KeyCache.Delete(apiKey.KeyHash)

	if h.Redis != nil {
		if err := h.Redis.PublishInvalidation(c.Context(), voidredis.ChannelKeys, apiKey.KeyHash); err != nil {
			h.Log.LogAttrs(c.Context(), slog.LevelWarn, "redis: publish key invalidation failed",
				slog.String("error", err.Error()),
			)
		}
	}

	return c.SendStatus(fiber.StatusNoContent)
}
