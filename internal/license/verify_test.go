package license

// White-box tests for Verify(). We are inside the package so we can swap
// embeddedPublicKey with a test keypair, keeping the production embedded key
// untouched for all other tests.

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// withTestPublicKey replaces embeddedPublicKey for the duration of the test
// and restores the original value via t.Cleanup. Tests that call this must NOT
// run in parallel with each other, because they mutate package-level state.
func withTestPublicKey(t *testing.T, pub ed25519.PublicKey) {
	t.Helper()
	orig := embeddedPublicKey
	embeddedPublicKey = pub
	t.Cleanup(func() { embeddedPublicKey = orig })
}

// newTestKeypair generates a fresh Ed25519 keypair and fails the test on error.
func newTestKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}
	return pub, priv
}

// signTestJWT builds and signs a LicenseClaims JWT with the given private key.
func signTestJWT(t *testing.T, priv ed25519.PrivateKey, claims LicenseClaims) string {
	t.Helper()
	key, err := GenerateLicenseJWT(priv, claims)
	if err != nil {
		t.Fatalf("GenerateLicenseJWT() error = %v", err)
	}
	return key
}

func TestVerify_EmptyKey(t *testing.T) {
	t.Parallel()

	lic := Verify("", false)

	if got := lic.Edition(); got != EditionCommunity {
		t.Errorf("Edition() = %q, want %q", got, EditionCommunity)
	}
	if !lic.Valid() {
		t.Error("Valid() = false, want true")
	}
}

func TestVerify_EnterpriseDev(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
	}{
		{"empty key with dev flag", ""},
		{"non-empty key ignored with dev flag", "some-license-key"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lic := Verify(tc.key, true)

			if got := lic.Edition(); got != EditionDev {
				t.Errorf("Edition() = %q, want %q", got, EditionDev)
			}
			if !lic.Valid() {
				t.Error("Valid() = false, want true")
			}
			if !lic.HasFeature(FeatureAuditLogs) {
				t.Error("HasFeature(FeatureAuditLogs) = false, want true")
			}
			if got := lic.MaxOrgs(); got != -1 {
				t.Errorf("MaxOrgs() = %d, want -1", got)
			}
		})
	}
}

func TestVerify_InvalidJWT(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		key  string
	}{
		{"random garbage", "not-a-jwt"},
		{"malformed header", "abc.def.ghi"},
		{"empty parts", ".."},
		{"truncated jwt", "eyJhbGciOiJFZERTQSJ9."},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			lic := Verify(tc.key, false)

			if got := lic.Edition(); got != EditionCommunity {
				t.Errorf("Edition() = %q, want %q", got, EditionCommunity)
			}
			if !lic.Valid() {
				t.Error("Valid() = false, want true for community fallback")
			}
		})
	}
}

func TestVerify_ExpiredJWT(t *testing.T) {
	// Mutates embeddedPublicKey — must not run in parallel with other key-swapping tests.
	pub, priv := newTestKeypair(t)
	withTestPublicKey(t, pub)

	claims := LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(-1 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC().Add(-2 * time.Hour)),
		},
		Plan:       "enterprise",
		Features:   []string{FeatureAuditLogs},
		MaxOrgs:    -1,
		MaxTeams:   -1,
		CustomerID: "cust_expired",
	}
	key := signTestJWT(t, priv, claims)

	lic := Verify(key, false)

	// Verify falls back to community on expiry.
	if got := lic.Edition(); got != EditionCommunity {
		t.Errorf("Edition() = %q, want %q (expired license must fall back to community)", got, EditionCommunity)
	}
	if !lic.Valid() {
		t.Error("Valid() = false, want true (community fallback is always valid)")
	}
}

func TestVerify_ValidJWT(t *testing.T) {
	// Mutates embeddedPublicKey — must not run in parallel with other key-swapping tests.
	pub, priv := newTestKeypair(t)
	withTestPublicKey(t, pub)

	wantCustomerID := "cust_test_42"
	wantFeatures := []string{FeatureAuditLogs, FeatureSSOOIDC, FeatureMultiOrg}
	wantMaxOrgs := 10
	wantMaxTeams := 50
	wantExpiry := time.Now().UTC().Add(365 * 24 * time.Hour).Truncate(time.Second)

	claims := LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(wantExpiry),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			Issuer:    "voidllm.ai",
		},
		Plan:       "enterprise",
		Features:   wantFeatures,
		MaxOrgs:    wantMaxOrgs,
		MaxTeams:   wantMaxTeams,
		CustomerID: wantCustomerID,
	}
	key := signTestJWT(t, priv, claims)

	lic := Verify(key, false)

	if got := lic.Edition(); got != EditionEnterprise {
		t.Errorf("Edition() = %q, want %q", got, EditionEnterprise)
	}
	if !lic.Valid() {
		t.Error("Valid() = false, want true")
	}
	if got := lic.ExpiresAt().Truncate(time.Second); !got.Equal(wantExpiry) {
		t.Errorf("ExpiresAt() = %v, want %v", got, wantExpiry)
	}
	if got := lic.MaxOrgs(); got != wantMaxOrgs {
		t.Errorf("MaxOrgs() = %d, want %d", got, wantMaxOrgs)
	}
	if got := lic.MaxTeams(); got != wantMaxTeams {
		t.Errorf("MaxTeams() = %d, want %d", got, wantMaxTeams)
	}
	if got := lic.CustomerID(); got != wantCustomerID {
		t.Errorf("CustomerID() = %q, want %q", got, wantCustomerID)
	}
	for _, f := range wantFeatures {
		if !lic.HasFeature(f) {
			t.Errorf("HasFeature(%q) = false, want true", f)
		}
	}
	if lic.HasFeature(FeatureOTelTracing) {
		t.Errorf("HasFeature(%q) = true, want false (not in license)", FeatureOTelTracing)
	}
	if lic.HasFeature(FeatureCustomRoles) {
		t.Errorf("HasFeature(%q) = true, want false (not in license)", FeatureCustomRoles)
	}

	gotFeatures := lic.Features()
	if len(gotFeatures) != len(wantFeatures) {
		t.Errorf("Features() len = %d, want %d; got %v", len(gotFeatures), len(wantFeatures), gotFeatures)
	}
}

func TestVerify_ValidJWT_DefaultLimits(t *testing.T) {
	// Mutates embeddedPublicKey — must not run in parallel with other key-swapping tests.
	// When MaxOrgs and MaxTeams are zero in the JWT claims, Verify must substitute
	// CommunityMaxOrgs and CommunityMaxTeams rather than returning zero.
	pub, priv := newTestKeypair(t)
	withTestPublicKey(t, pub)

	claims := LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
			Issuer:    "voidllm.ai",
		},
		Plan:       "enterprise",
		Features:   []string{FeatureAuditLogs},
		MaxOrgs:    0, // zero → should default to CommunityMaxOrgs
		MaxTeams:   0, // zero → should default to CommunityMaxTeams
		CustomerID: "cust_defaults",
	}
	key := signTestJWT(t, priv, claims)

	lic := Verify(key, false)

	if got := lic.MaxOrgs(); got != CommunityMaxOrgs {
		t.Errorf("MaxOrgs() = %d, want %d (zero in JWT should fall back to community default)", got, CommunityMaxOrgs)
	}
	if got := lic.MaxTeams(); got != CommunityMaxTeams {
		t.Errorf("MaxTeams() = %d, want %d (zero in JWT should fall back to community default)", got, CommunityMaxTeams)
	}
}

func TestVerify_WrongSigningKey(t *testing.T) {
	// Mutates embeddedPublicKey — must not run in parallel with other key-swapping tests.
	// Sign with one key but verify with a different public key: should fall back to community.
	_, signingPriv := newTestKeypair(t)
	verifyingPub, _ := newTestKeypair(t)
	withTestPublicKey(t, verifyingPub)

	claims := LicenseClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().UTC().Add(24 * time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now().UTC()),
		},
		Plan:       "enterprise",
		Features:   []string{FeatureAuditLogs},
		MaxOrgs:    -1,
		MaxTeams:   -1,
		CustomerID: "cust_wrong_key",
	}
	key := signTestJWT(t, signingPriv, claims)

	lic := Verify(key, false)

	if got := lic.Edition(); got != EditionCommunity {
		t.Errorf("Edition() = %q, want %q (wrong key must fall back to community)", got, EditionCommunity)
	}
}
