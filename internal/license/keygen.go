package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"

	"github.com/golang-jwt/jwt/v5"
)

// GenerateKeyPair generates a new Ed25519 keypair suitable for signing license JWTs.
// The returned public key should be embedded in the VoidLLM binary as embeddedPublicKeyHex.
// The private key must be stored securely and used only by the voidllm.ai platform.
func GenerateKeyPair() (ed25519.PublicKey, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate ed25519 keypair: %w", err)
	}
	return pub, priv, nil
}

// GenerateLicenseJWT signs the given LicenseClaims with the provided Ed25519
// private key and returns the compact JWT string. The resulting token is
// suitable for use as a VoidLLM enterprise license key.
func GenerateLicenseJWT(privateKey ed25519.PrivateKey, claims LicenseClaims) (string, error) {
	token := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	signed, err := token.SignedString(privateKey)
	if err != nil {
		return "", fmt.Errorf("sign license jwt: %w", err)
	}
	return signed, nil
}
