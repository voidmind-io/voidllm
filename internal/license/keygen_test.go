package license_test

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/voidmind-io/voidllm/internal/license"
)

func TestGenerateKeyPair(t *testing.T) {
	t.Parallel()

	pub, priv, err := license.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}

	if len(pub) != ed25519.PublicKeySize {
		t.Errorf("public key len = %d, want %d", len(pub), ed25519.PublicKeySize)
	}
	if len(priv) != ed25519.PrivateKeySize {
		t.Errorf("private key len = %d, want %d", len(priv), ed25519.PrivateKeySize)
	}

	// Verify the keypair is functional: sign a message and verify it.
	msg := []byte("voidllm license test message")
	sig := ed25519.Sign(priv, msg)
	if !ed25519.Verify(pub, msg, sig) {
		t.Error("ed25519.Verify() = false, generated keypair does not produce verifiable signatures")
	}
}

func TestGenerateKeyPair_UniquenessAcrossCalls(t *testing.T) {
	t.Parallel()

	pub1, _, err := license.GenerateKeyPair()
	if err != nil {
		t.Fatalf("first GenerateKeyPair() error = %v", err)
	}

	pub2, _, err := license.GenerateKeyPair()
	if err != nil {
		t.Fatalf("second GenerateKeyPair() error = %v", err)
	}

	// Two independent calls must produce different keys.
	if string(pub1) == string(pub2) {
		t.Error("GenerateKeyPair() returned identical public keys on successive calls")
	}
}

func TestGenerateLicenseJWT(t *testing.T) {
	t.Parallel()

	_, priv, err := license.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}

	claims := license.LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
		},
		Plan:       "enterprise",
		Features:   []string{license.FeatureAuditLogs},
		MaxOrgs:    5,
		MaxTeams:   20,
		CustomerID: "cust_jwt_test",
	}

	token, err := license.GenerateLicenseJWT(priv, claims)
	if err != nil {
		t.Fatalf("GenerateLicenseJWT() error = %v", err)
	}
	if token == "" {
		t.Fatal("GenerateLicenseJWT() returned empty string")
	}

	// A valid JWT has exactly three dot-separated parts.
	parts := splitJWT(token)
	if len(parts) != 3 {
		t.Errorf("JWT has %d parts (dot-separated), want 3; token = %q", len(parts), token)
	}
}

func TestRoundTrip(t *testing.T) {
	t.Parallel()

	pub, priv, err := license.GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}

	wantCustomerID := "cust_roundtrip"
	wantFeatures := []string{license.FeatureAuditLogs, license.FeatureOTelTracing, license.FeatureSSOOIDC}
	wantMaxOrgs := 7
	wantMaxTeams := 30
	wantExpiry := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)

	claims := license.LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(wantExpiry),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
		},
		Plan:       "enterprise",
		Features:   wantFeatures,
		MaxOrgs:    wantMaxOrgs,
		MaxTeams:   wantMaxTeams,
		CustomerID: wantCustomerID,
	}

	tokenStr, err := license.GenerateLicenseJWT(priv, claims)
	if err != nil {
		t.Fatalf("GenerateLicenseJWT() error = %v", err)
	}

	// Parse the JWT with the public key to confirm claims survive the round-trip.
	parsed := &license.LicenseClaims{}
	token, err := jwt.ParseWithClaims(
		tokenStr,
		parsed,
		func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return pub, nil
		},
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		t.Fatalf("jwt.ParseWithClaims() error = %v", err)
	}
	if !token.Valid {
		t.Fatal("parsed token is not valid")
	}

	if parsed.CustomerID != wantCustomerID {
		t.Errorf("CustomerID = %q, want %q", parsed.CustomerID, wantCustomerID)
	}
	if parsed.MaxOrgs != wantMaxOrgs {
		t.Errorf("MaxOrgs = %d, want %d", parsed.MaxOrgs, wantMaxOrgs)
	}
	if parsed.MaxTeams != wantMaxTeams {
		t.Errorf("MaxTeams = %d, want %d", parsed.MaxTeams, wantMaxTeams)
	}
	if len(parsed.Features) != len(wantFeatures) {
		t.Errorf("Features len = %d, want %d; got %v", len(parsed.Features), len(wantFeatures), parsed.Features)
	}
	featureSet := make(map[string]struct{}, len(parsed.Features))
	for _, f := range parsed.Features {
		featureSet[f] = struct{}{}
	}
	for _, f := range wantFeatures {
		if _, ok := featureSet[f]; !ok {
			t.Errorf("Features missing %q", f)
		}
	}
	if parsed.ExpiresAt == nil {
		t.Fatal("ExpiresAt is nil")
	}
	if got := parsed.ExpiresAt.Time.Truncate(time.Second); !got.Equal(wantExpiry) {
		t.Errorf("ExpiresAt = %v, want %v", got, wantExpiry)
	}
}

// splitJWT splits a JWT string into its dot-separated components without
// importing a full JWT library. Used only to validate token structure.
func splitJWT(token string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(token); i++ {
		if token[i] == '.' {
			parts = append(parts, token[start:i])
			start = i + 1
		}
	}
	parts = append(parts, token[start:])
	return parts
}
