package license

import (
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/golang-jwt/jwt/v5"
)

// ErrInvalidKey is returned by ValidateKey when the provided license JWT
// cannot be parsed, has an invalid signature, or fails any claim check.
var ErrInvalidKey = errors.New("invalid license key")

// embeddedPublicKeyHex is the hex-encoded Ed25519 public key used to verify
// license JWTs issued by voidllm.ai. The corresponding private key is held
// exclusively by the VoidMind platform and is never embedded in this binary.
const embeddedPublicKeyHex = "94893c54639c3290e38c4ecb33293e86e381b29addcf4372a5a98df7fa81a51f"

// embeddedPublicKey is decoded once at package init and reused for every Verify call.
var embeddedPublicKey = mustDecodeHexKey(embeddedPublicKeyHex)

// mustDecodeHexKey decodes a hex-encoded Ed25519 public key. It panics if the
// input is malformed, which can only happen if embeddedPublicKeyHex was edited
// incorrectly — a programming error caught at startup.
func mustDecodeHexKey(s string) ed25519.PublicKey {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic("license: invalid embedded public key hex: " + err.Error())
	}
	if len(b) != ed25519.PublicKeySize {
		panic(fmt.Sprintf("license: embedded public key is %d bytes, want %d", len(b), ed25519.PublicKeySize))
	}
	return ed25519.PublicKey(b)
}

// LicenseClaims contains the enterprise license payload embedded in the JWT.
// Exported so the CLI's generate subcommand can populate it directly.
type LicenseClaims struct {
	jwt.RegisteredClaims

	// Plan is a human-readable label for the license tier (e.g. "enterprise").
	Plan string `json:"plan"`

	// Features lists the feature name strings enabled by this license.
	// Each string must match one of the Feature* constants in features.go.
	Features []string `json:"features"`

	// MaxOrgs is the maximum number of organizations permitted. Use -1 for
	// unlimited. Defaults to CommunityMaxOrgs when omitted (zero value).
	MaxOrgs int `json:"max_orgs"`

	// MaxTeams is the maximum number of teams permitted across all
	// organizations. Use -1 for unlimited. Defaults to CommunityMaxTeams
	// when omitted (zero value).
	MaxTeams int `json:"max_teams"`

	// CustomerID is an opaque identifier for the licensed customer, used for
	// support and audit purposes.
	CustomerID string `json:"customer_id"`
}

// Verify parses and cryptographically verifies a VoidLLM enterprise license key.
//
//   - If enterpriseDev is true, a devLicense is returned regardless of key.
//   - If key is empty, a communityLicense is returned.
//   - If key is a valid Ed25519-signed JWT, an enterpriseLicense is returned.
//   - On any error (parse failure, bad signature, expired), a warning is logged
//     and a communityLicense is returned so the proxy always starts.
func Verify(key string, enterpriseDev bool) License {
	if enterpriseDev {
		slog.Warn("enterprise dev mode active — all features enabled, not for production use")
		return devLicense{}
	}

	if key == "" {
		return communityLicense{}
	}

	claims := &LicenseClaims{}
	token, err := jwt.ParseWithClaims(key, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return embeddedPublicKey, nil
	}, jwt.WithExpirationRequired(), jwt.WithIssuer("voidllm.ai"))
	if err != nil {
		slog.Warn("license verification failed, falling back to community edition",
			slog.String("error", err.Error()),
		)
		return communityLicense{}
	}

	if !token.Valid {
		slog.Warn("license token is not valid, falling back to community edition")
		return communityLicense{}
	}

	featureSet := make(map[string]struct{}, len(claims.Features))
	for _, f := range claims.Features {
		featureSet[f] = struct{}{}
	}

	if claims.ExpiresAt == nil {
		slog.Warn("license JWT missing expiry claim, falling back to community edition")
		return communityLicense{}
	}
	expiresAt := claims.ExpiresAt.Time

	maxOrgs := claims.MaxOrgs
	if maxOrgs == 0 {
		maxOrgs = CommunityMaxOrgs
	}

	maxTeams := claims.MaxTeams
	if maxTeams == 0 {
		maxTeams = CommunityMaxTeams
	}

	lic := &enterpriseLicense{
		features:    featureSet,
		featureList: append([]string(nil), claims.Features...),
		expiresAt:   expiresAt,
		maxOrgs:     maxOrgs,
		maxTeams:    maxTeams,
		customerID:  claims.CustomerID,
	}

	if !lic.Valid() {
		slog.Warn("license has expired, falling back to community edition",
			slog.Time("expired_at", expiresAt),
		)
		return communityLicense{}
	}

	return lic
}

// ValidateKey parses and verifies a license key, returning the resulting License
// on success or an error describing why validation failed.
//
// Unlike Verify, ValidateKey does not fall back to communityLicense on failure —
// it returns ErrInvalidKey (wrapped) so callers can distinguish a valid key from
// a rejected one. It is intended for use by the SetLicense API handler, where a
// hard error is the correct response to a bad key.
func ValidateKey(key string) (License, error) {
	if key == "" {
		return nil, fmt.Errorf("%w: key is empty", ErrInvalidKey)
	}

	claims := &LicenseClaims{}
	token, err := jwt.ParseWithClaims(key, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, jwt.ErrSignatureInvalid
		}
		return embeddedPublicKey, nil
	}, jwt.WithExpirationRequired(), jwt.WithIssuer("voidllm.ai"))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidKey, err)
	}

	if !token.Valid {
		return nil, fmt.Errorf("%w: token validation failed", ErrInvalidKey)
	}

	if claims.ExpiresAt == nil {
		return nil, fmt.Errorf("%w: missing expiry claim", ErrInvalidKey)
	}
	expiresAt := claims.ExpiresAt.Time

	maxOrgs := claims.MaxOrgs
	if maxOrgs == 0 {
		maxOrgs = CommunityMaxOrgs
	}

	maxTeams := claims.MaxTeams
	if maxTeams == 0 {
		maxTeams = CommunityMaxTeams
	}

	featureSet := make(map[string]struct{}, len(claims.Features))
	for _, f := range claims.Features {
		featureSet[f] = struct{}{}
	}

	lic := &enterpriseLicense{
		features:    featureSet,
		featureList: append([]string(nil), claims.Features...),
		expiresAt:   expiresAt,
		maxOrgs:     maxOrgs,
		maxTeams:    maxTeams,
		customerID:  claims.CustomerID,
	}

	if !lic.Valid() {
		return nil, fmt.Errorf("%w: license has expired", ErrInvalidKey)
	}

	return lic, nil
}
