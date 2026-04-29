// Package licensetest provides test helpers for packages that need to generate
// valid VoidLLM enterprise license JWTs in tests. It uses go:linkname to swap
// the license package's embedded Ed25519 public key so that ValidateKey
// accepts JWTs signed with a test-generated keypair.
//
// This package must only be imported from _test.go files. It is not suitable
// for use in production code.
package licensetest

import (
	"crypto/ed25519"
	"testing"
	"time"
	_ "unsafe" // required for go:linkname

	"github.com/golang-jwt/jwt/v5"
	"github.com/voidmind-io/voidllm/internal/license"
)

//go:linkname licenseEmbeddedPublicKey github.com/voidmind-io/voidllm/internal/license.embeddedPublicKey
var licenseEmbeddedPublicKey ed25519.PublicKey

// WithTestPublicKey replaces the license package's embedded Ed25519 public
// key for the duration of the test, then restores the original value via
// t.Cleanup. Tests that call this MUST NOT run in parallel with other tests
// that also call WithTestPublicKey, because they mutate shared package state.
func WithTestPublicKey(t *testing.T, pub ed25519.PublicKey) {
	t.Helper()
	orig := make(ed25519.PublicKey, len(licenseEmbeddedPublicKey))
	copy(orig, licenseEmbeddedPublicKey)
	licenseEmbeddedPublicKey = pub
	t.Cleanup(func() {
		licenseEmbeddedPublicKey = orig
	})
}

// NewTestKeypair generates a fresh Ed25519 keypair and fatals the test on
// error.
func NewTestKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := license.GenerateKeyPair()
	if err != nil {
		t.Fatalf("licensetest.NewTestKeypair: GenerateKeyPair() error = %v", err)
	}
	return pub, priv
}

// SignTestJWT signs a LicenseClaims value with the provided Ed25519 private
// key and returns the compact JWT string.
func SignTestJWT(t *testing.T, priv ed25519.PrivateKey, claims license.LicenseClaims) string {
	t.Helper()
	key, err := license.GenerateLicenseJWT(priv, claims)
	if err != nil {
		t.Fatalf("licensetest.SignTestJWT: GenerateLicenseJWT() error = %v", err)
	}
	return key
}

// NewEnterpriseClaims returns a minimal valid LicenseClaims with the given
// features, a 24-hour expiry, and the "voidllm.ai" issuer.
func NewEnterpriseClaims(features []string) license.LicenseClaims {
	return license.LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			Issuer:    "voidllm.ai",
		},
		Plan:       "enterprise",
		Features:   features,
		MaxOrgs:    -1,
		MaxTeams:   -1,
		CustomerID: "cust_test",
	}
}
