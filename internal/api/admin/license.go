package admin

import (
	"errors"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/license"
)

// setLicenseRequest is the JSON body accepted by SetLicense.
type setLicenseRequest struct {
	// Key is the VoidLLM enterprise license JWT issued by voidllm.ai.
	Key string `json:"key"`
}

// setLicenseResponse is the JSON body returned by SetLicense on success.
type setLicenseResponse struct {
	// Status is always "saved" on a successful activation.
	Status  string        `json:"status"`
	// Message is a human-readable confirmation of the activation.
	Message string        `json:"message"`
	// License contains the parsed details of the accepted license.
	License licenseDetail `json:"license"`
}

// licenseDetail carries the plan, features, and expiry extracted from a
// successfully validated license JWT.
type licenseDetail struct {
	// Plan is the human-readable tier label embedded in the JWT (e.g. "enterprise").
	Plan      string     `json:"plan"`
	// Features lists the feature names enabled by the license.
	Features  []string   `json:"features"`
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
		Edition:  string(lic.Edition()),
		Valid:    lic.Valid(),
		Features: lic.Features(),
		MaxOrgs:  lic.MaxOrgs(),
		MaxTeams: lic.MaxTeams(),
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
