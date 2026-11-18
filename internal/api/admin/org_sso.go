package admin

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gofiber/fiber/v3"

	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/internal/jsonx"
	"github.com/voidmind-io/voidllm/internal/mcp"
	"github.com/voidmind-io/voidllm/pkg/crypto"
)

// ssoDiscoveryTimeout bounds the OIDC discovery request made by
// TestSSOConnection.
const ssoDiscoveryTimeout = 10 * time.Second

// ssoDiscoveryMaxBody caps the size of an OIDC discovery response read by
// TestSSOConnection.
const ssoDiscoveryMaxBody = 1 << 20

// ssoDiscoveryIdleConnTimeout bounds how long an idle discovery connection is
// kept open.
const ssoDiscoveryIdleConnTimeout = 5 * time.Second

// errSSODiscoveryTooLarge is returned from the discovery response body once
// it exceeds ssoDiscoveryMaxBody.
var errSSODiscoveryTooLarge = errors.New("discovery response too large")

// newSSODiscoveryClient returns the HTTP client used for OIDC issuer
// discovery: internal destinations are refused at dial time, redirects are not
// followed, and the response body is size-limited.
func newSSODiscoveryClient() *http.Client {
	return newSSODiscoveryClientWithGuard(false)
}

// newSSODiscoveryClientWithGuard builds the discovery client. allowPrivate
// disables the dial guard and exists only so tests can reach local servers;
// production code must go through newSSODiscoveryClient.
func newSSODiscoveryClientWithGuard(allowPrivate bool) *http.Client {
	transport := mcp.NewSSRFSafeTransport(allowPrivate)
	transport.DisableKeepAlives = true
	transport.IdleConnTimeout = ssoDiscoveryIdleConnTimeout
	return &http.Client{
		Transport: &limitedBodyTransport{base: transport, limit: ssoDiscoveryMaxBody},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: ssoDiscoveryTimeout,
	}
}

// limitedBodyTransport wraps an http.Transport and replaces each response
// body with a reader that fails, rather than truncates, past limit bytes.
type limitedBodyTransport struct {
	base  *http.Transport
	limit int64
}

// RoundTrip performs the request on the base transport and limits the body.
func (t *limitedBodyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	resp.Body = &limitedBody{rc: resp.Body, remaining: t.limit}
	return resp, nil
}

// CloseIdleConnections closes idle connections on the base transport.
func (t *limitedBodyTransport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
}

// limitedBody is a response body that returns errSSODiscoveryTooLarge once more
// than the allowed number of bytes would be read.
type limitedBody struct {
	rc        io.ReadCloser
	remaining int64
}

// Read reads from the underlying body, failing once the limit is exceeded.
func (b *limitedBody) Read(p []byte) (int, error) {
	if b.remaining < 0 {
		return 0, errSSODiscoveryTooLarge
	}
	if int64(len(p)) > b.remaining+1 {
		p = p[:b.remaining+1]
	}
	n, err := b.rc.Read(p)
	b.remaining -= int64(n)
	if b.remaining < 0 {
		return 0, errSSODiscoveryTooLarge
	}
	return n, err
}

// Close closes the underlying body.
func (b *limitedBody) Close() error {
	return b.rc.Close()
}

// ssoConfigRequest is the JSON body accepted by UpsertOrgSSOConfig.
// If ClientSecret is empty, the stored encrypted secret is preserved unchanged.
type ssoConfigRequest struct {
	Enabled        bool     `json:"enabled"`
	Issuer         string   `json:"issuer"`
	ClientID       string   `json:"client_id"`
	ClientSecret   string   `json:"client_secret"`
	RedirectURL    string   `json:"redirect_url"`
	Scopes         []string `json:"scopes"`
	AllowedDomains []string `json:"allowed_domains"`
	AutoProvision  bool     `json:"auto_provision"`
	DefaultRole    string   `json:"default_role"`
	GroupSync      bool     `json:"group_sync"`
	GroupClaim     string   `json:"group_claim"`
}

// ssoConfigResponse is the JSON representation of an SSO configuration returned by the API.
// HasSecret is true when an encrypted client secret is stored; the secret itself is never returned.
type ssoConfigResponse struct {
	ID             string   `json:"id,omitempty"`
	OrgID          string   `json:"org_id,omitempty"`
	Enabled        bool     `json:"enabled"`
	Issuer         string   `json:"issuer"`
	ClientID       string   `json:"client_id"`
	HasSecret      bool     `json:"has_secret"`
	RedirectURL    string   `json:"redirect_url"`
	Scopes         []string `json:"scopes"`
	AllowedDomains []string `json:"allowed_domains"`
	AutoProvision  bool     `json:"auto_provision"`
	DefaultRole    string   `json:"default_role"`
	GroupSync      bool     `json:"group_sync"`
	GroupClaim     string   `json:"group_claim"`
	CreatedAt      string   `json:"created_at,omitempty"`
	UpdatedAt      string   `json:"updated_at,omitempty"`
}

// testSSORequest is the JSON body accepted by TestSSOConnection.
type testSSORequest struct {
	Issuer string `json:"issuer"`
}

// testSSOResponse is the JSON response returned by TestSSOConnection.
type testSSOResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// ssoConfigToResponse converts a db.OrgSSOConfig to its API wire representation.
func ssoConfigToResponse(cfg *db.OrgSSOConfig) ssoConfigResponse {
	return ssoConfigResponse{
		ID:             cfg.ID,
		OrgID:          cfg.OrgID,
		Enabled:        cfg.Enabled,
		Issuer:         cfg.Issuer,
		ClientID:       cfg.ClientID,
		HasSecret:      len(cfg.ClientSecretEnc) > 0,
		RedirectURL:    cfg.RedirectURL,
		Scopes:         cfg.Scopes,
		AllowedDomains: cfg.AllowedDomains,
		AutoProvision:  cfg.AutoProvision,
		DefaultRole:    cfg.DefaultRole,
		GroupSync:      cfg.GroupSync,
		GroupClaim:     cfg.GroupClaim,
		CreatedAt:      cfg.CreatedAt,
		UpdatedAt:      cfg.UpdatedAt,
	}
}

// orgSSOAAD returns the additional authenticated data used when encrypting the
// client secret for an org SSO config. Binding the AAD to the org ID ensures
// the ciphertext cannot be transplanted to a different org's record.
func orgSSOAAD(orgID string) []byte {
	return []byte("org_sso:" + orgID)
}

// GetOrgSSOConfig handles GET /api/v1/orgs/:org_id/sso.
// Callers must be org admins or system admins.
//
// @Summary      Get org SSO configuration
// @Description  Returns the OIDC/SSO configuration for an organization. The client secret is never returned; has_secret indicates whether one is stored.
// @Tags         sso
// @Produce      json
// @Param        org_id  path      string  true  "Organization ID"
// @Success      200     {object}  ssoConfigResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/sso [get]
func (h *Handler) GetOrgSSOConfig(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAdmin(c, orgID); !ok {
		return nil
	}

	cfg, err := h.DB.GetOrgSSOConfig(c.Context(), orgID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "sso configuration not found")
		}
		h.Log.ErrorContext(c.Context(), "get org sso config", slog.String("org_id", orgID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to get sso configuration")
	}

	return c.JSON(ssoConfigToResponse(cfg))
}

// UpsertOrgSSOConfig handles PUT /api/v1/orgs/:org_id/sso.
// Callers must be org admins or system admins. When client_secret is provided and
// non-empty it is encrypted with AES-256-GCM and stored. When client_secret is
// empty the existing encrypted secret is preserved.
//
// @Summary      Upsert org SSO configuration
// @Description  Creates or replaces the OIDC/SSO configuration for an organization. Provide client_secret only when rotating it; leave empty to preserve the stored secret.
// @Tags         sso
// @Accept       json
// @Produce      json
// @Param        org_id  path      string            true  "Organization ID"
// @Param        body    body      ssoConfigRequest  true  "SSO configuration"
// @Success      200     {object}  ssoConfigResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/sso [put]
func (h *Handler) UpsertOrgSSOConfig(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAdmin(c, orgID); !ok {
		return nil
	}

	var req ssoConfigRequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}

	if req.DefaultRole != "member" && req.DefaultRole != "team_admin" {
		return apierror.BadRequest(c, "default_role must be \"member\" or \"team_admin\"")
	}

	// Determine the encrypted secret to store.
	secretEnc, err := h.resolveSecretEnc(c.Context(), orgID, req.ClientSecret)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "upsert org sso config: resolve secret", slog.String("org_id", orgID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to process client secret")
	}

	// Marshal slice fields to JSON strings for storage.
	scopesJSON, err := marshalStringSlice(req.Scopes)
	if err != nil {
		return apierror.BadRequest(c, "invalid scopes value")
	}
	allowedDomainsJSON, err := marshalStringSlice(req.AllowedDomains)
	if err != nil {
		return apierror.BadRequest(c, "invalid allowed_domains value")
	}

	cfg, err := h.DB.UpsertOrgSSOConfig(c.Context(), orgID, db.UpsertOrgSSOParams{
		Enabled:         req.Enabled,
		Issuer:          req.Issuer,
		ClientID:        req.ClientID,
		ClientSecretEnc: secretEnc,
		RedirectURL:     req.RedirectURL,
		Scopes:          scopesJSON,
		AllowedDomains:  allowedDomainsJSON,
		AutoProvision:   req.AutoProvision,
		DefaultRole:     req.DefaultRole,
		GroupSync:       req.GroupSync,
		GroupClaim:      req.GroupClaim,
	})
	if err != nil {
		h.Log.ErrorContext(c.Context(), "upsert org sso config", slog.String("org_id", orgID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to save sso configuration")
	}

	return c.JSON(ssoConfigToResponse(cfg))
}

// DeleteOrgSSOConfig handles DELETE /api/v1/orgs/:org_id/sso.
// Callers must be org admins or system admins. The deletion is permanent (no soft-delete).
//
// @Summary      Delete org SSO configuration
// @Description  Permanently removes the OIDC/SSO configuration for an organization.
// @Tags         sso
// @Param        org_id  path  string  true  "Organization ID"
// @Success      204     "No Content"
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Failure      404     {object}  swaggerErrorResponse
// @Failure      500     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/sso [delete]
func (h *Handler) DeleteOrgSSOConfig(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAdmin(c, orgID); !ok {
		return nil
	}

	if err := h.DB.DeleteOrgSSOConfig(c.Context(), orgID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return apierror.NotFound(c, "sso configuration not found")
		}
		h.Log.ErrorContext(c.Context(), "delete org sso config", slog.String("org_id", orgID), slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to delete sso configuration")
	}

	return c.SendStatus(fiber.StatusNoContent)
}

// TestSSOConnection handles POST /api/v1/orgs/:org_id/sso/test.
// Callers must be org admins or system admins. It validates that the provided
// issuer URL is reachable and returns a valid OIDC discovery document.
// The issuer must use https and must not be, or resolve to, an internal
// address; redirects are not followed. Failure details are not returned.
//
// @Summary      Test SSO connection
// @Description  Validates an OIDC issuer URL by fetching its discovery document. Returns status "ok" on success or "error" with a message on failure.
// @Tags         sso
// @Accept       json
// @Produce      json
// @Param        org_id  path      string          true  "Organization ID"
// @Param        body    body      testSSORequest  true  "Issuer URL to test"
// @Success      200     {object}  testSSOResponse
// @Failure      400     {object}  swaggerErrorResponse
// @Failure      401     {object}  swaggerErrorResponse
// @Failure      403     {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /orgs/{org_id}/sso/test [post]
func (h *Handler) TestSSOConnection(c fiber.Ctx) error {
	orgID := c.Params("org_id")

	if _, ok := requireOrgAdmin(c, orgID); !ok {
		return nil
	}

	var req testSSORequest
	if err := c.Bind().JSON(&req); err != nil {
		return apierror.BadRequest(c, "invalid request body")
	}
	if req.Issuer == "" {
		return apierror.BadRequest(c, "issuer is required")
	}

	u, parseErr := url.Parse(req.Issuer)
	if parseErr != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return apierror.BadRequest(c, "issuer must be a valid https URL")
	}
	if u.Scheme != "https" {
		return apierror.BadRequest(c, "issuer must use https")
	}
	if isPrivateHost(u.Hostname()) {
		return apierror.BadRequest(c, "issuer URL must not point to a private address")
	}

	// Discovery runs through a dial-time guarded transport so that hostnames
	// resolving to internal addresses are refused, and redirects are not
	// followed. This is independent of MCPAllowPrivateURLs.
	client := newSSODiscoveryClient()
	defer client.CloseIdleConnections()
	if _, err := oidc.NewProvider(oidc.ClientContext(c.Context(), client), req.Issuer); err != nil {
		// The error text may embed the upstream response body, so it is
		// neither logged nor returned.
		h.Log.WarnContext(c.Context(), "sso issuer discovery failed",
			slog.String("org_id", orgID),
			slog.String("category", "discovery"),
		)
		return c.JSON(testSSOResponse{
			Status:  "error",
			Message: "issuer discovery failed",
		})
	}

	return c.JSON(testSSOResponse{Status: "ok"})
}

// GetGlobalSSOConfig handles GET /api/v1/settings/sso.
// Only system admins may call this endpoint. It returns the YAML-level SSO
// configuration (not per-org). The client secret is never returned; has_secret
// indicates whether one is configured.
//
// @Summary      Get global SSO configuration
// @Description  Returns the system-level OIDC/SSO configuration loaded from voidllm.yaml. Only system admins may call this endpoint.
// @Tags         sso
// @Produce      json
// @Success      200  {object}  ssoConfigResponse
// @Failure      401  {object}  swaggerErrorResponse
// @Failure      403  {object}  swaggerErrorResponse
// @Security     BearerAuth
// @Router       /settings/sso [get]
func (h *Handler) GetGlobalSSOConfig(c fiber.Ctx) error {
	sso := h.SSOConfig
	resp := ssoConfigResponse{
		Enabled:        sso.Enabled,
		Issuer:         sso.Issuer,
		ClientID:       sso.ClientID,
		HasSecret:      len(sso.ClientSecret) > 0,
		RedirectURL:    sso.RedirectURL,
		Scopes:         sso.Scopes,
		AllowedDomains: sso.AllowedDomains,
		AutoProvision:  sso.AutoProvision,
		DefaultRole:    sso.DefaultRole,
		GroupSync:      sso.GroupSync,
		GroupClaim:     sso.GroupClaim,
	}
	// Ensure slice fields are never null in JSON output.
	if resp.Scopes == nil {
		resp.Scopes = []string{}
	}
	if resp.AllowedDomains == nil {
		resp.AllowedDomains = []string{}
	}
	return c.JSON(resp)
}

// resolveSecretEnc returns the encrypted client secret to persist.
// When rawSecret is non-empty it is encrypted with AES-256-GCM using the org ID
// as additional authenticated data. When rawSecret is empty the existing stored
// ciphertext is returned unchanged (by loading the current config from the DB).
// If no config exists yet and rawSecret is empty, an empty string is returned.
func (h *Handler) resolveSecretEnc(ctx context.Context, orgID, rawSecret string) (string, error) {
	if rawSecret != "" {
		enc, err := crypto.EncryptString(rawSecret, h.EncryptionKey, orgSSOAAD(orgID))
		if err != nil {
			return "", fmt.Errorf("encrypt client secret: %w", err)
		}
		return enc, nil
	}

	// Preserve the existing encrypted secret.
	existing, err := h.DB.GetOrgSSOConfig(ctx, orgID)
	if err != nil {
		if errors.Is(err, db.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("load existing sso config: %w", err)
	}
	return existing.ClientSecretEnc, nil
}

// marshalStringSlice encodes a []string as a compact JSON array.
// A nil or empty slice is encoded as "[]".
func marshalStringSlice(s []string) (string, error) {
	if s == nil {
		s = []string{}
	}
	b, err := jsonx.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// isPrivateHost reports whether host is an IP literal rejected by
// mcp.IsBlockedAddr, ignoring any IPv6 zone; a host with a zone separator whose
// address part does not parse is rejected. The name "localhost" is also
// rejected. Other hostnames are allowed here and are checked at dial time by
// the guarded transport used for discovery.
func isPrivateHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	_, blocked := blockedHostLiteral(host)
	return blocked
}
