// Package keygen provides API key generation, HMAC-SHA256 hashing, and
// prefix validation for VoidLLM's authentication system.
package keygen

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

// Key type constants identify the category of an API key.
const (
	KeyTypeUser    = "user_key"
	KeyTypeTeam    = "team_key"
	KeyTypeSA      = "sa_key"
	KeyTypeSession = "session_key"
	KeyTypeInvite  = "invite_token"
)

// Key prefix constants are the human-readable prefixes embedded in every key.
const (
	PrefixUser    = "vl_uk_"
	PrefixTeam    = "vl_tk_"
	PrefixSA      = "vl_sa_"
	PrefixSession = "vl_sk_"
	PrefixInvite  = "vl_iv_"
)

// Generate creates a new random API key for the given keyType. The returned
// key is prefix + hex(24 random bytes), producing a 54-character string.
// Returns an error if keyType is unrecognized or the random source fails.
func Generate(keyType string) (string, error) {
	prefix, err := prefixFor(keyType)
	if err != nil {
		return "", err
	}

	raw := make([]byte, 24)
	if _, err := io.ReadFull(rand.Reader, raw); err != nil {
		return "", fmt.Errorf("generate key: read random bytes: %w", err)
	}

	return prefix + hex.EncodeToString(raw), nil
}

// Hash returns the hex-encoded HMAC-SHA256 of plaintextKey using hmacSecret.
// The result is used as the stored representation of an API key — the
// plaintext is never persisted. HMAC-SHA256 never returns an error for a
// valid (non-nil) key, so no error is returned.
func Hash(plaintextKey string, hmacSecret []byte) string {
	mac := hmac.New(sha256.New, hmacSecret)
	// Write never returns an error for hash.Hash implementations.
	_, _ = io.WriteString(mac, plaintextKey)
	return hex.EncodeToString(mac.Sum(nil))
}

// Hint returns a short, non-secret representation of the key suitable for
// display in logs and UIs. If the key is 10 characters or shorter it is
// returned as-is. Otherwise the format is "<first6>...<last4>", e.g.
// "vl_uk_...e8b1".
func Hint(plaintextKey string) string {
	if len(plaintextKey) <= 10 {
		return plaintextKey
	}
	return plaintextKey[:6] + "..." + plaintextKey[len(plaintextKey)-4:]
}

// Verify reports whether plaintextKey, when HMAC'd with hmacSecret, matches
// storedHash. The comparison is constant-time to prevent timing attacks.
// storedHash must be the hex-encoded output of Hash. Returns false if either
// hex string is malformed.
func Verify(plaintextKey string, hmacSecret []byte, storedHash string) bool {
	computed := Hash(plaintextKey, hmacSecret)
	computedBytes, err := hex.DecodeString(computed)
	if err != nil {
		return false
	}
	expectedBytes, err := hex.DecodeString(storedHash)
	if err != nil {
		return false
	}
	return hmac.Equal(computedBytes, expectedBytes)
}

// ValidatePrefix inspects the key's prefix and returns the corresponding
// KeyType constant. Returns an error if the key does not begin with any
// recognized VoidLLM prefix.
func ValidatePrefix(key string) (string, error) {
	switch {
	case strings.HasPrefix(key, PrefixUser):
		return KeyTypeUser, nil
	case strings.HasPrefix(key, PrefixTeam):
		return KeyTypeTeam, nil
	case strings.HasPrefix(key, PrefixSA):
		return KeyTypeSA, nil
	case strings.HasPrefix(key, PrefixSession):
		return KeyTypeSession, nil
	case strings.HasPrefix(key, PrefixInvite):
		return KeyTypeInvite, nil
	default:
		return "", fmt.Errorf("validate prefix: unrecognized key prefix")
	}
}

// prefixFor returns the key prefix string for the given keyType constant.
func prefixFor(keyType string) (string, error) {
	switch keyType {
	case KeyTypeUser:
		return PrefixUser, nil
	case KeyTypeTeam:
		return PrefixTeam, nil
	case KeyTypeSA:
		return PrefixSA, nil
	case KeyTypeSession:
		return PrefixSession, nil
	case KeyTypeInvite:
		return PrefixInvite, nil
	default:
		return "", fmt.Errorf("prefix for: unknown key type %q", keyType)
	}
}
