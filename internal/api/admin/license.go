package admin

import (
	"errors"
	"log/slog"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/license"
	voidredis "github.com/voidmind-io/voidllm/internal/redis"
)

// setLicenseRequest is the JSON body accepted by SetLicense.
type setLicenseRequest struct {
	// Key is the VoidLLM enterprise license JWT issued by voidllm.ai.
	Key string `json:"key"`
}

// setLicenseResponse is the JSON body returned by SetLicense on success.
type setLicenseResponse struct {
	// Status is always "saved" on a successful activation.
	Status string `json:"status"`
	// Message is a human-readable confirmation of the activation.
	Message string `json:"message"`
	// License contains the parsed details of the accepted license.
	License licenseDetail `json:"license"`
}

// licenseDetail carries the plan, features, and expiry extracted from a
// successfully validated license JWT.
type licenseDetail struct {
	// Plan is the human-readable tier label embedded in the JWT (e.g. "enterprise").
	Plan string `json:"plan"`
	// Features lists the feature names enabled by the license.
	Features []string `json:"features"`
	// ExpiresAt is the RFC 3339 expiry timestamp, or null for perpetual licenses.
	ExpiresAt *time.Time `json:"expires_at"`
}

// licenseResponse is the JSON body returned by GetLicense.
type licenseResponse struct {
	// Edition is the product tier: "community", "enterprise", or "dev".
	Edition string `json:"edition"`
	// Valid reports whether the license is currently active.
	Valid bool `json:"valid"`
	// Features lists the enterprise feature names enabled by this license.
	Features []string `json:"features"`
	// ExpiresAt is the RFC 3339 expiry time, or null for perpetual licenses.
	ExpiresAt *time.Time `json:"expires_at"`
	// MaxOrgs is the maximum permitted organization count. -1 means unlimited.
	MaxOrgs int `json:"max_orgs"`
	// MaxTeams is the maximum permitted team count across all organizations.
	// -1 means unlimited.
	MaxTeams int `json:"max_teams"`
	// CustomerID is the opaque customer identifier embedded in an enterprise
	// license. Empty for community and dev licenses.
	CustomerID string `json:"customer_id"`
	// FallbackMaxDepth is the configured maximum fallback chain depth.
	// 0 means fallback chains are disabled even if models have fallback configured.
	FallbackMaxDepth int `json:"fallback_max_depth"`
}

// SetLicense handles PUT /api/v1/settings/license.
//
// It validates the provided license key and hot-swaps the in-memory license.
// The heartbeat goroutine is not restarted — a full restart is required for
// the heartbeat to pick up the new key.
//
//	@Summary		Set license key
//	@Description	Validates and activates an enterprise license JWT in memory. Restart VoidLLM to enable heartbeat with the new key.
//	@Tags			license
//	@Accept			json
//	@Produce		json
//	@Param			body	body		setLicenseRequest	true	"License key payload"
//	@Success		200		{object}	setLicenseResponse
//	@Failure		400		{object}	swaggerErrorResponse
//	@Failure		401		{object}	swaggerErrorResponse
//	@Failure		403		{object}	swaggerErrorResponse
//	@Router			/settings/license [put]
//	@Security		BearerAuth
func (h *Handler) SetLicense(c fiber.Ctx) error {
	var req setLicenseRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.Key == "" {
		return apierror.BadRequest(c, "key is required")
	}

	lic, err := license.ValidateKey(req.Key)
	if err != nil {
		if errors.Is(err, license.ErrInvalidKey) {
			return apierror.BadRequest(c, "invalid license key")
		}
		return apierror.InternalError(c, "license validation failed")
	}

	h.License.Store(lic)

	// Apply the license change locally by re-gating the registry.
	// This path is the ONLY re-gate trigger on deployments without Redis;
	// on multi-instance deployments it also covers the local instance
	// before the Redis broadcast reaches peers.
	if h.ReloadModels != nil {
		if err := h.ReloadModels(c.Context()); err != nil {
			h.Log.LogAttrs(c.Context(), slog.LevelError,
				"set license: in-process registry reload failed",
				slog.String("error", err.Error()))
			// Do not return an error to the client - the license is already
			// stored, only the registry is stale. Operators can trigger a
			// manual reload or restart.
		}
	}

	// On multi-instance deployments, broadcast a model invalidation so
	// all peers rebuild their registry with the updated license state.
	if h.Redis != nil {
		if pubErr := h.Redis.PublishInvalidation(c.Context(), voidredis.ChannelModels, "reload"); pubErr != nil {
			h.Log.LogAttrs(c.Context(), slog.LevelWarn, "set license: publish model reload failed",
				slog.String("error", pubErr.Error()),
			)
			// Non-fatal: the in-memory license is already updated above and the
			// local registry has been reloaded. Peers will reconcile on the next
			// admin-triggered model change or process restart.
		}
	}

	// Persist to DB so the license survives restarts.
	if err := h.DB.SetSetting(c.Context(), "license_jwt", req.Key); err != nil {
		return apierror.InternalError(c, "failed to persist license")
	}

	keyInfo := auth.KeyInfoFromCtx(c)
	actorID := ""
	orgID := ""
	if keyInfo != nil {
		actorID = keyInfo.UserID
		orgID = keyInfo.OrgID
	}
	h.Log.LogAttrs(c.Context(), slog.LevelInfo, "license activated via API",
		slog.String("edition", string(lic.Edition())),
		slog.String("actor_id", actorID),
		slog.String("org_id", orgID),
	)

	detail := licenseDetail{
		Plan:     string(lic.Edition()),
		Features: lic.Features(),
	}
	if !lic.ExpiresAt().IsZero() {
		t := lic.ExpiresAt()
		detail.ExpiresAt = &t
	}

	return c.JSON(setLicenseResponse{
		Status:  "saved",
		Message: "License activated. Restart VoidLLM to enable heartbeat with the new key.",
		License: detail,
	})
}

// GetLicense godoc
//
//	@Summary		Get license information
//	@Description	Returns the current license edition, status, enabled features, and resource limits.
//	@Tags			license
//	@Produce		json
//	@Success		200	{object}	licenseResponse
//	@Failure		401	{object}	swaggerErrorResponse
//	@Router			/license [get]
//	@Security		BearerAuth
func (h *Handler) GetLicense(c fiber.Ctx) error {
	lic := h.License.Load()

	resp := licenseResponse{
		Edition:          string(lic.Edition()),
		Valid:            lic.Valid(),
		Features:         lic.Features(),
		MaxOrgs:          lic.MaxOrgs(),
		MaxTeams:         lic.MaxTeams(),
		FallbackMaxDepth: h.FallbackMaxDepth,
	}

	// CustomerID is sensitive — only org_admin and above may see it.
	keyInfo := auth.KeyInfoFromCtx(c)
	if keyInfo != nil && auth.HasRole(keyInfo.Role, auth.RoleOrgAdmin) {
		resp.CustomerID = lic.CustomerID()
	}

	if !lic.ExpiresAt().IsZero() {
		t := lic.ExpiresAt()
		resp.ExpiresAt = &t
	}

	return c.JSON(resp)
}
