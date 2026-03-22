package admin

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/voidmind-io/voidllm/internal/apierror"
	"github.com/voidmind-io/voidllm/internal/audit"
	"github.com/voidmind-io/voidllm/internal/auth"
	"github.com/voidmind-io/voidllm/internal/db"
	"github.com/voidmind-io/voidllm/pkg/keygen"
)

const (
	oidcStateCookieName = "voidllm_oidc_state"
	oidcTokenCookieName = "voidllm_oidc_token"
	oidcStateBytes      = 32
	oidcNonceBytes      = 32
)

// generateRandomHex returns a cryptographically random hex-encoded string of
// byteLen random bytes. It returns an error if the system entropy source fails.
func generateRandomHex(byteLen int) (string, error) {
	raw := make([]byte, byteLen)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// AuthProviders handles GET /api/v1/auth/providers. It returns which
// authentication methods are available on this instance. This is a public
// endpoint used by the frontend to decide whether to show an SSO button.
//
// @Summary      List available authentication providers
// @Description  Returns which login methods (local password, OIDC SSO) are enabled.
// @Tags         auth
// @Produce      json
// @Success      200  {object}  map[string]bool
// @Router       /auth/providers [get]
func (h *Handler) AuthProviders(c fiber.Ctx) error {
	return c.JSON(fiber.Map{
		"local": true,
		"oidc":  h.SSOProvider != nil,
	})
}

// OIDCLogin handles GET /api/v1/auth/oidc/login. It generates a random CSRF
// state token, stores it in a short-lived HTTP-only cookie, and redirects the
// browser to the identity provider's authorization endpoint.
//
// @Summary      Initiate OIDC SSO login
// @Description  Redirects the browser to the configured OIDC provider's authorization endpoint.
// @Tags         auth
// @Success      302
// @Router       /auth/oidc/login [get]
func (h *Handler) OIDCLogin(c fiber.Ctx) error {
	state, err := generateRandomHex(oidcStateBytes)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "oidc login: generate state", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to initiate login")
	}

	nonce, err := generateRandomHex(oidcNonceBytes)
	if err != nil {
		h.Log.ErrorContext(c.Context(), "oidc login: generate nonce", slog.String("error", err.Error()))
		return apierror.InternalError(c, "failed to initiate login")
	}

	// Encode both state and nonce in a single cookie separated by "|".
	// The nonce is bound to the ID token; the state provides CSRF protection.
	cookieValue := state + "|" + nonce
	secure := strings.HasPrefix(h.SSOConfig.RedirectURL, "https://")

	c.Cookie(&fiber.Cookie{
		Name:     oidcStateCookieName,
		Value:    cookieValue,
		Path:     "/",
		MaxAge:   300, // 5 minutes — enough time to complete the IdP redirect
		HTTPOnly: true,
		SameSite: "Lax",
		Secure:   secure,
	})

	return c.Redirect().To(h.SSOProvider.AuthURL(state, nonce))
}

// OIDCCallback handles GET /api/v1/auth/oidc/callback. It verifies the CSRF
// state, exchanges the authorization code for an ID token, provisions or
// looks up the user, syncs group memberships if configured, creates a session
// key, and redirects the browser to the frontend with the token in the query string.
//
// @Summary      OIDC SSO callback
// @Description  Receives the authorization code from the identity provider, completes the login flow, and redirects to the frontend.
// @Tags         auth
// @Param        code   query  string  true  "Authorization code"
// @Param        state  query  string  true  "CSRF state token"
// @Success      302
// @Router       /auth/oidc/callback [get]
func (h *Handler) OIDCCallback(c fiber.Ctx) error {
	ctx := c.Context()

	secure := strings.HasPrefix(h.SSOConfig.RedirectURL, "https://")

	// Step 1: CSRF check — verify state cookie matches query param using
	// constant-time comparison to prevent timing attacks.
	rawCookie := c.Cookies(oidcStateCookieName)
	queryState := c.Query("state")

	parts := strings.SplitN(rawCookie, "|", 2)
	cookieState := parts[0]
	cookieNonce := ""
	if len(parts) == 2 {
		cookieNonce = parts[1]
	}

	if cookieState == "" || subtle.ConstantTimeCompare([]byte(cookieState), []byte(queryState)) != 1 {
		return c.Redirect().To("/login?error=invalid_state")
	}

	// Step 2: Clear the state cookie immediately so it cannot be replayed.
	c.Cookie(&fiber.Cookie{
		Name:     oidcStateCookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HTTPOnly: true,
		SameSite: "Lax",
		Secure:   secure,
	})

	// Step 3: Exchange the code for a verified set of claims.
	code := c.Query("code")
	if code == "" {
		return c.Redirect().To("/login?error=missing_code")
	}

	claims, err := h.SSOProvider.Exchange(ctx, code, cookieNonce)
	if err != nil {
		h.Log.ErrorContext(ctx, "oidc callback: exchange code", slog.String("error", err.Error()))
		return c.Redirect().To("/login?error=exchange_failed")
	}

	// Step 4: Enforce allowed_domains if configured.
	if len(h.SSOConfig.AllowedDomains) > 0 {
		allowed := false
		for _, d := range h.SSOConfig.AllowedDomains {
			if strings.EqualFold(claims.EmailDomain, d) {
				allowed = true
				break
			}
		}
		if !allowed {
			h.Log.LogAttrs(ctx, slog.LevelWarn, "oidc callback: domain not allowed",
				slog.String("domain", claims.EmailDomain),
			)
			return c.Redirect().To("/login?error=domain_not_allowed")
		}
	}

	// Step 5: Look up the user by provider + subject.
	user, err := h.DB.GetUserByExternalID(ctx, "oidc", claims.Subject)
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		h.Log.ErrorContext(ctx, "oidc callback: lookup user", slog.String("error", err.Error()))
		return c.Redirect().To("/login?error=lookup_failed")
	}

	// Step 6: Provision or reject unknown users.
	if user == nil {
		if !h.SSOConfig.AutoProvision {
			return c.Redirect().To("/login?error=not_provisioned")
		}

		// Resolve the default org: by slug if configured, else the first active org.
		var orgID string
		if h.SSOConfig.DefaultOrgSlug != "" {
			org, orgErr := h.DB.GetOrgBySlug(ctx, h.SSOConfig.DefaultOrgSlug)
			if orgErr != nil {
				h.Log.ErrorContext(ctx, "oidc callback: resolve default org by slug",
					slog.String("slug", h.SSOConfig.DefaultOrgSlug),
					slog.String("error", orgErr.Error()),
				)
				return c.Redirect().To("/login?error=provision_failed")
			}
			orgID = org.ID
		} else {
			orgs, listErr := h.DB.ListOrgs(ctx, "", 1, false)
			if listErr != nil || len(orgs) == 0 {
				h.Log.ErrorContext(ctx, "oidc callback: resolve default org", slog.String("error", func() string {
					if listErr != nil {
						return listErr.Error()
					}
					return "no organizations exist"
				}()))
				return c.Redirect().To("/login?error=provision_failed")
			}
			orgID = orgs[0].ID
		}

		externalID := claims.Subject
		newUser, createErr := h.DB.CreateUser(ctx, db.CreateUserParams{
			Email:        claims.Email,
			DisplayName:  claims.Name,
			AuthProvider: "oidc",
			ExternalID:   &externalID,
		})
		if createErr != nil {
			h.Log.ErrorContext(ctx, "oidc callback: create user", slog.String("error", createErr.Error()))
			return c.Redirect().To("/login?error=provision_failed")
		}

		role := h.SSOConfig.DefaultRole
		if role == "" {
			role = "member"
		}

		_, memberErr := h.DB.CreateOrgMembership(ctx, db.CreateOrgMembershipParams{
			OrgID:  orgID,
			UserID: newUser.ID,
			Role:   role,
		})
		if memberErr != nil {
			// Compensate: delete the orphan user so the DB stays consistent.
			if delErr := h.DB.DeleteUser(ctx, newUser.ID); delErr != nil {
				h.Log.ErrorContext(ctx, "oidc callback: delete orphan user after membership failure",
					slog.String("user_id", newUser.ID),
					slog.String("error", delErr.Error()),
				)
			}
			h.Log.ErrorContext(ctx, "oidc callback: create org membership, rolled back user creation",
				slog.String("error", memberErr.Error()),
			)
			return c.Redirect().To("/login?error=provision_failed")
		}

		user = newUser
	} else if claims.Name != "" && claims.Name != user.DisplayName {
		// Step 7 (found path): optionally update display_name if it changed at IdP.
		if _, err := h.DB.UpdateUser(ctx, user.ID, db.UpdateUserParams{
			DisplayName: &claims.Name,
		}); err != nil {
			h.Log.LogAttrs(ctx, slog.LevelWarn, "oidc: update display name failed",
				slog.String("error", err.Error()),
			)
		}
	}

	// Step 8: Group sync — create teams matching IdP group names and ensure membership.
	if h.SSOConfig.GroupSync && len(claims.Groups) > 0 {
		role, orgID, roleErr := h.DB.ResolveUserRole(ctx, user.ID)
		if roleErr == nil {
			_ = role // role resolution is for orgID only here
			for _, groupName := range claims.Groups {
				team, teamErr := h.DB.GetTeamByName(ctx, orgID, groupName)
				if teamErr != nil && errors.Is(teamErr, db.ErrNotFound) {
					// Create the team on first encounter.
					slug := deriveTeamSlug(groupName)
					team, teamErr = h.DB.CreateTeam(ctx, db.CreateTeamParams{
						OrgID: orgID,
						Name:  groupName,
						Slug:  slug,
					})
				}
				if teamErr != nil {
					h.Log.LogAttrs(ctx, slog.LevelWarn, "oidc callback: group sync team",
						slog.String("group", groupName),
						slog.String("error", teamErr.Error()),
					)
					continue
				}

				isMember, memberCheckErr := h.DB.IsTeamMember(ctx, user.ID, team.ID)
				if memberCheckErr != nil {
					h.Log.LogAttrs(ctx, slog.LevelWarn, "oidc callback: group sync check member",
						slog.String("team_id", team.ID),
						slog.String("error", memberCheckErr.Error()),
					)
					continue
				}
				if !isMember {
					if _, err := h.DB.CreateTeamMembership(ctx, db.CreateTeamMembershipParams{
						TeamID: team.ID,
						UserID: user.ID,
						Role:   "member",
					}); err != nil {
						h.Log.LogAttrs(ctx, slog.LevelWarn, "oidc: group sync membership failed",
							slog.String("team", team.Name),
							slog.String("error", err.Error()),
						)
					}
				}
			}
		}
	}

	// Step 9: Resolve the user's effective role and org for the session key.
	sessionRole, sessionOrgID, resolveErr := h.DB.ResolveUserRole(ctx, user.ID)
	if resolveErr != nil {
		h.Log.ErrorContext(ctx, "oidc callback: resolve user role", slog.String("error", resolveErr.Error()))
		return c.Redirect().To("/login?error=auth_failed")
	}

	// Step 10: Revoke existing sessions.
	if err := h.DB.RevokeUserSessions(ctx, user.ID); err != nil {
		h.Log.ErrorContext(ctx, "oidc callback: revoke old sessions", slog.String("error", err.Error()))
		// Non-fatal: proceed even if cleanup fails.
	}

	// Step 11: Generate a new session key valid for 24 hours.
	key, err := keygen.Generate(keygen.KeyTypeSession)
	if err != nil {
		h.Log.ErrorContext(ctx, "oidc callback: generate session key", slog.String("error", err.Error()))
		return c.Redirect().To("/login?error=auth_failed")
	}

	keyHash := keygen.Hash(key, h.HMACSecret)
	keyHint := keygen.Hint(key)
	expiresAt := time.Now().UTC().Add(24 * time.Hour)
	expiresAtStr := expiresAt.Format(time.RFC3339)

	apiKey, err := h.DB.CreateAPIKey(ctx, db.CreateAPIKeyParams{
		KeyHash:   keyHash,
		KeyHint:   keyHint,
		KeyType:   keygen.KeyTypeSession,
		Name:      "SSO session",
		OrgID:     sessionOrgID,
		UserID:    &user.ID,
		ExpiresAt: &expiresAtStr,
		CreatedBy: user.ID,
	})
	if err != nil {
		h.Log.ErrorContext(ctx, "oidc callback: create api key", slog.String("error", err.Error()))
		return c.Redirect().To("/login?error=auth_failed")
	}

	h.KeyCache.Set(keyHash, auth.KeyInfo{
		ID:        apiKey.ID,
		KeyType:   keygen.KeyTypeSession,
		Role:      sessionRole,
		OrgID:     sessionOrgID,
		UserID:    user.ID,
		Name:      "SSO session",
		ExpiresAt: &expiresAt,
	})

	// Step 12: Emit audit event.
	if h.AuditLogger != nil {
		h.AuditLogger.Log(audit.Event{
			Timestamp:    time.Now().UTC(),
			OrgID:        sessionOrgID,
			ActorID:      user.ID,
			ActorType:    "user",
			ActorKeyID:   apiKey.ID,
			Action:       "login",
			ResourceType: "session",
			ResourceID:   apiKey.ID,
			Description:  marshalDescription(map[string]string{"email": claims.Email, "provider": "oidc"}),
			IPAddress:    c.IP(),
			StatusCode:   fiber.StatusFound,
		})
	}

	// Step 13: Deliver the session token via a short-lived, path-restricted
	// cookie instead of a URL query parameter. This keeps the token out of
	// browser history and Referer headers. The frontend callback page reads
	// the cookie, moves the value into localStorage, clears the cookie, and
	// navigates to "/". The 10-second MaxAge is sufficient and limits exposure.
	c.Cookie(&fiber.Cookie{
		Name:     oidcTokenCookieName,
		Value:    key,
		Path:     "/auth/callback",
		MaxAge:   10,
		HTTPOnly: false, // frontend JS must read it to store in localStorage
		SameSite: "Strict",
		Secure:   secure,
	})
	return c.Redirect().To("/auth/callback")
}

// deriveTeamSlug converts a group name to a URL-safe team slug using the same
// logic as config.deriveSlug: lowercase, spaces to hyphens, strip non-alnum.
func deriveTeamSlug(name string) string {
	s := strings.ToLower(name)
	s = strings.ReplaceAll(s, " ", "-")
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "group"
	}
	return slug
}
