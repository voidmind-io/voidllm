// Package crypto provides AES-256-GCM encryption utilities for storing
// sensitive values (such as upstream API keys) in the database.
//
// Nonce safety: AES-256-GCM with random 96-bit nonces is safe for up to
// approximately 2^48 encryptions under the same key. For typical VoidLLM
// deployments (hundreds to thousands of stored API keys), this provides
// an enormous safety margin. Key rotation is recommended well before
// reaching this threshold.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// ErrKeyLength is kept for backward compatibility. It is no longer returned
// by ParseKey but may be used by callers that construct keys manually.
var ErrKeyLength = errors.New("key must be exactly 32 bytes (AES-256)")

// ErrCiphertextTooShort is returned when the ciphertext is shorter than the
// required nonce size plus GCM overhead and therefore cannot contain a valid
// GCM message.
var ErrCiphertextTooShort = errors.New("ciphertext too short")

// ParseKey returns a 32-byte AES-256 key from raw. If raw is a valid standard
// base64 string that decodes to exactly 32 bytes it is used as-is (backward
// compatible). Otherwise the key is derived from raw via SHA-256, which
// accepts any string of at least 16 characters (e.g. Railway secret values).
func ParseKey(raw string) ([]byte, error) {
	// Try base64 first — backward compatible with existing keys.
	if key, err := base64.StdEncoding.DecodeString(raw); err == nil && len(key) == 32 {
		return key, nil
	}
	// Fall back to SHA-256 key derivation for arbitrary strings.
	if len(raw) < 16 {
		return nil, fmt.Errorf("ParseKey: key must be at least 16 characters")
	}
	h := sha256.Sum256([]byte(raw))
	return h[:], nil
}

// ZeroKey overwrites key material in memory with zeros. Callers should
// defer this after obtaining a key from ParseKey to minimize the window
// during which key material is present in process memory.
func ZeroKey(key []byte) {
	for i := range key {
		key[i] = 0
	}
}

// Encrypt encrypts plaintext using AES-256-GCM with a randomly generated
// 12-byte nonce. The returned byte slice has the format:
//
//	[ nonce (12 bytes) | ciphertext+tag ]
//
// key must be exactly 32 bytes; use ParseKey to obtain one from a base64
// string.
//
// aad is optional additional authenticated data bound to the ciphertext.
// Pass nil if not needed. When provided, the same aad must be passed to
// Decrypt.
func Encrypt(plaintext []byte, key []byte, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("Encrypt: new cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("Encrypt: new GCM: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("Encrypt: generate nonce: %w", err)
	}

	return gcm.Seal(nonce, nonce, plaintext, aad), nil
}

// Decrypt decrypts a ciphertext produced by Encrypt. The ciphertext must
// begin with the 12-byte nonce followed by the GCM ciphertext and
// authentication tag. It returns ErrCiphertextTooShort if the input is
// smaller than the nonce size plus GCM overhead, or an error if
// authentication fails.
//
// aad is optional additional authenticated data bound to the ciphertext.
// Pass nil if not needed. When provided, aad must match the value passed
// to Encrypt exactly, or decryption will fail.
func Decrypt(ciphertext []byte, key []byte, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: new cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: new GCM: %w", err)
	}

	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize+gcm.Overhead() {
		return nil, fmt.Errorf("Decrypt: %w", ErrCiphertextTooShort)
	}

	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("Decrypt: authentication failed: %w", err)
	}

	return plaintext, nil
}

// EncryptString encrypts a plaintext string using AES-256-GCM and returns the
// result as a standard base64-encoded string suitable for database storage.
//
// aad is optional additional authenticated data bound to the ciphertext.
// Pass nil if not needed. When provided, the same aad must be passed to
// DecryptString.
func EncryptString(plaintext string, key []byte, aad []byte) (string, error) {
	ct, err := Encrypt([]byte(plaintext), key, aad)
	if err != nil {
		return "", fmt.Errorf("EncryptString: %w", err)
	}
	return base64.StdEncoding.EncodeToString(ct), nil
}

// DecryptString decodes a standard base64-encoded ciphertext (as produced by
// EncryptString) and decrypts it, returning the original plaintext string.
//
// aad is optional additional authenticated data bound to the ciphertext.
// Pass nil if not needed. When provided, aad must match the value passed
// to EncryptString exactly, or decryption will fail.
func DecryptString(ciphertext string, key []byte, aad []byte) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", fmt.Errorf("DecryptString: decode base64: %w", err)
	}
	plaintext, err := Decrypt(decoded, key, aad)
	if err != nil {
		return "", fmt.Errorf("DecryptString: %w", err)
	}
	return string(plaintext), nil
}
