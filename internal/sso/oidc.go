// Package sso provides OIDC/OAuth2 single sign-on integration for VoidLLM.
// Provider performs OIDC Discovery at construction time and then manages
// the full authorization code flow: generating redirect URLs and exchanging
// authorization codes for verified identity claims.
package sso

import (
	"context"
	"fmt"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/voidmind-io/voidllm/internal/config"
)

// Provider wraps an OIDC provider with its OAuth2 configuration and ID token
// verifier. All methods are safe to call concurrently.
type Provider struct {
	oidcProvider *oidc.Provider
	oauth2Config oauth2.Config
	verifier     *oidc.IDTokenVerifier
	cfg          config.SSOConfig
}

// UserClaims holds the identity attributes extracted from a verified OIDC ID token.
// All fields are derived from standard or configurable claims; no raw token data
// is retained beyond this struct.
type UserClaims struct {
	// Subject is the stable unique identifier for the user at the identity provider.
	Subject string
	// Email is the user's email address from the "email" claim.
	Email string
	// Name is the user's display name from the "name" claim.
	Name string
	// Groups is the list of groups/roles from the configurable group claim.
	// Empty when the claim is absent or the value is not a string slice.
	Groups []string
	// EmailDomain is the domain portion of Email (everything after "@").
	EmailDomain string
}

// NewProvider performs OIDC Discovery against cfg.Issuer and returns a fully
// initialised Provider ready to use. It returns an error if Discovery fails or
// the configuration is invalid.
func NewProvider(ctx context.Context, cfg config.SSOConfig) (*Provider, error) {
	if cfg.Issuer == "" {
		return nil, fmt.Errorf("sso: issuer is required")
	}
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("sso: client_id is required")
	}
	if cfg.ClientSecret == "" {
		return nil, fmt.Errorf("sso: client_secret is required")
	}
	if cfg.RedirectURL == "" {
		return nil, fmt.Errorf("sso: redirect_url is required")
	}

	oidcProv, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("sso: oidc discovery for %q: %w", cfg.Issuer, err)
	}

	scopes := cfg.Scopes
	if len(scopes) == 0 {
		scopes = []string{oidc.ScopeOpenID, "email", "profile"}
	}

	oauth2Cfg := oauth2.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		RedirectURL:  cfg.RedirectURL,
		Endpoint:     oidcProv.Endpoint(),
		Scopes:       scopes,
	}

	verifier := oidcProv.Verifier(&oidc.Config{ClientID: cfg.ClientID})

	return &Provider{
		oidcProvider: oidcProv,
		oauth2Config: oauth2Cfg,
		verifier:     verifier,
		cfg:          cfg,
	}, nil
}

// AuthURL returns the OAuth2 authorization endpoint URL that the user's browser
// should be redirected to. state is an opaque CSRF token and nonce is a
// single-use value bound to the ID token to prevent replay attacks.
func (p *Provider) AuthURL(state, nonce string) string {
	return p.oauth2Config.AuthCodeURL(state, oidc.Nonce(nonce))
}

// Exchange completes the authorization code flow: it exchanges code for tokens,
// verifies the ID token signature and claims (including the nonce), and returns
// the extracted user identity. ctx must carry a deadline appropriate for an
// upstream HTTP call.
func (p *Provider) Exchange(ctx context.Context, code, expectedNonce string) (*UserClaims, error) {
	token, err := p.oauth2Config.Exchange(ctx, code)
	if err != nil {
		return nil, fmt.Errorf("sso: exchange code: %w", err)
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		return nil, fmt.Errorf("sso: id_token missing from token response")
	}

	idToken, err := p.verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, fmt.Errorf("sso: verify id token: %w", err)
	}

	if idToken.Nonce != expectedNonce {
		return nil, fmt.Errorf("sso: nonce mismatch")
	}

	var rawClaims map[string]interface{}
	if err := idToken.Claims(&rawClaims); err != nil {
		return nil, fmt.Errorf("sso: decode id token claims: %w", err)
	}

	email, _ := rawClaims["email"].(string)
	name, _ := rawClaims["name"].(string)

	emailVerified, _ := rawClaims["email_verified"].(bool)
	if email != "" && !emailVerified {
		return nil, fmt.Errorf("sso: email not verified by identity provider")
	}

	var emailDomain string
	if idx := strings.LastIndex(email, "@"); idx >= 0 {
		emailDomain = email[idx+1:]
	}

	groupClaim := p.cfg.GroupClaim
	if groupClaim == "" {
		groupClaim = "groups"
	}

	var groups []string
	if raw, exists := rawClaims[groupClaim]; exists {
		switch v := raw.(type) {
		case []interface{}:
			for _, elem := range v {
				if s, ok := elem.(string); ok {
					groups = append(groups, s)
				}
			}
		case []string:
			groups = v
		}
	}

	return &UserClaims{
		Subject:     idToken.Subject,
		Email:       email,
		Name:        name,
		Groups:      groups,
		EmailDomain: emailDomain,
	}, nil
}
