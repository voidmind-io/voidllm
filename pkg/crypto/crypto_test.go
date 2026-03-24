package crypto_test

import (
	"bytes"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/voidmind-io/voidllm/pkg/crypto"
)

// testKey returns a fixed 32-byte key suitable for deterministic tests.
func testKey() []byte {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	return key
}

// testKeyBase64 returns the standard base64 encoding of testKey().
func testKeyBase64() string {
	return base64.StdEncoding.EncodeToString(testKey())
}

// --- Encrypt + Decrypt ---

func TestEncryptDecryptRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		plaintext []byte
	}{
		{
			name:      "ascii string",
			plaintext: []byte("hello, world"),
		},
		{
			name:      "empty plaintext",
			plaintext: []byte{},
		},
		{
			name:      "binary data",
			plaintext: []byte{0x00, 0xff, 0xab, 0xcd, 0x01},
		},
		{
			name:      "long plaintext",
			plaintext: bytes.Repeat([]byte("a"), 4096),
		},
		{
			name:      "single byte",
			plaintext: []byte{0x42},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			key := testKey()

			ct, err := crypto.Encrypt(tc.plaintext, key, nil)
			if err != nil {
				t.Fatalf("Encrypt() unexpected error: %v", err)
			}

			got, err := crypto.Decrypt(ct, key, nil)
			if err != nil {
				t.Fatalf("Decrypt() unexpected error: %v", err)
			}

			if !bytes.Equal(got, tc.plaintext) {
				t.Errorf("Decrypt() = %q, want %q", got, tc.plaintext)
			}
		})
	}
}

func TestEncryptProducesUniqueOutput(t *testing.T) {
	t.Parallel()

	key := testKey()
	plaintext := []byte("same plaintext every time")

	ct1, err := crypto.Encrypt(plaintext, key, nil)
	if err != nil {
		t.Fatalf("Encrypt() first call error: %v", err)
	}

	ct2, err := crypto.Encrypt(plaintext, key, nil)
	if err != nil {
		t.Fatalf("Encrypt() second call error: %v", err)
	}

	if bytes.Equal(ct1, ct2) {
		t.Error("Encrypt() produced identical output for the same plaintext; nonce must differ")
	}
}

// --- AAD ---

func TestEncryptDecryptAADMismatchFails(t *testing.T) {
	t.Parallel()

	key := testKey()
	plaintext := []byte("secret value")

	ct, err := crypto.Encrypt(plaintext, key, []byte("model-123"))
	if err != nil {
		t.Fatalf("Encrypt() unexpected error: %v", err)
	}

	_, err = crypto.Decrypt(ct, key, []byte("model-456"))
	if err == nil {
		t.Fatal("Decrypt() expected an error when AAD differs but got nil")
	}
}

func TestEncryptDecryptAADMatchSucceeds(t *testing.T) {
	t.Parallel()

	key := testKey()
	plaintext := []byte("secret value")
	aad := []byte("model-123")

	ct, err := crypto.Encrypt(plaintext, key, aad)
	if err != nil {
		t.Fatalf("Encrypt() unexpected error: %v", err)
	}

	got, err := crypto.Decrypt(ct, key, aad)
	if err != nil {
		t.Fatalf("Decrypt() unexpected error: %v", err)
	}

	if !bytes.Equal(got, plaintext) {
		t.Errorf("Decrypt() = %q, want %q", got, plaintext)
	}
}

// --- Decrypt error paths ---

func TestDecryptErrors(t *testing.T) {
	t.Parallel()

	key := testKey()
	plaintext := []byte("secret value")

	validCT, err := crypto.Encrypt(plaintext, key, nil)
	if err != nil {
		t.Fatalf("setup: Encrypt() error: %v", err)
	}

	wrongKey := make([]byte, 32)
	for i := range wrongKey {
		wrongKey[i] = 0xff
	}

	// Build a tampered ciphertext: flip a byte in the ciphertext body (after
	// the 12-byte nonce) so the GCM authentication tag will not match.
	tampered := make([]byte, len(validCT))
	copy(tampered, validCT)
	tampered[12] ^= 0x01

	tests := []struct {
		name       string
		ciphertext []byte
		key        []byte
		wantErr    error // nil means any non-nil error is acceptable
	}{
		{
			name:       "wrong key causes authentication failure",
			ciphertext: validCT,
			key:        wrongKey,
			wantErr:    nil,
		},
		{
			name:       "tampered ciphertext causes authentication failure",
			ciphertext: tampered,
			key:        key,
			wantErr:    nil,
		},
		{
			// Minimum valid ciphertext is nonce (12 bytes) + GCM overhead (16 bytes) = 28 bytes.
			// Three bytes is too short to contain even the nonce.
			name:       "ciphertext shorter than nonce+overhead",
			ciphertext: []byte{0x01, 0x02, 0x03},
			key:        key,
			wantErr:    crypto.ErrCiphertextTooShort,
		},
		{
			name:       "empty ciphertext",
			ciphertext: []byte{},
			key:        key,
			wantErr:    crypto.ErrCiphertextTooShort,
		},
		{
			// Exactly 12 bytes: has a full nonce but is missing the 16-byte auth tag.
			name:       "nonce-only ciphertext (12 bytes) is too short",
			ciphertext: make([]byte, 12),
			key:        key,
			wantErr:    crypto.ErrCiphertextTooShort,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := crypto.Decrypt(tc.ciphertext, tc.key, nil)
			if err == nil {
				t.Fatal("Decrypt() expected an error but got nil")
			}

			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("Decrypt() error = %v, want errors.Is(%v)", err, tc.wantErr)
			}
		})
	}
}

// --- ParseKey ---

func TestParseKey(t *testing.T) {
	t.Parallel()

	// A 16-byte slice encoded to base64 — wrong length for AES-256.
	shortKey := base64.StdEncoding.EncodeToString(make([]byte, 16))

	tests := []struct {
		name         string
		input        string
		wantErr      error
		wantBytes    int  // expected length of returned key, 0 means don't check
		wantExactKey bool // true only for the base64 case that must round-trip to testKey()
	}{
		{
			name:         "valid 32-byte base64 key",
			input:        testKeyBase64(),
			wantErr:      nil,
			wantBytes:    32,
			wantExactKey: true,
		},
		{
			name:      "non-base64 string derives 32-byte key via SHA-256",
			input:     "not-valid-base64!!!",
			wantErr:   nil,
			wantBytes: 32,
		},
		{
			name:      "base64-encoded 16-byte value derives 32-byte key via SHA-256",
			input:     shortKey,
			wantErr:   nil,
			wantBytes: 32,
		},
		{
			name:    "empty string is rejected",
			input:   "",
			wantErr: nil, // non-nil error expected; no sentinel defined
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := crypto.ParseKey(tc.input)

			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("ParseKey() expected error %v but got nil", tc.wantErr)
				}
				if !errors.Is(err, tc.wantErr) {
					t.Errorf("ParseKey() error = %v, want errors.Is(%v)", err, tc.wantErr)
				}
				return
			}

			// wantErr == nil cases that still expect an error (e.g. invalid base64).
			if tc.wantBytes == 0 {
				if err == nil {
					t.Fatal("ParseKey() expected an error but got nil")
				}
				return
			}

			// Happy-path: no error expected.
			if err != nil {
				t.Fatalf("ParseKey() unexpected error: %v", err)
			}
			if len(got) != tc.wantBytes {
				t.Errorf("ParseKey() returned %d bytes, want %d", len(got), tc.wantBytes)
			}
			if tc.wantExactKey && !bytes.Equal(got, testKey()) {
				t.Errorf("ParseKey() returned wrong key bytes")
			}
		})
	}
}

// --- ZeroKey ---

func TestZeroKey(t *testing.T) {
	t.Parallel()

	key := testKey()
	// Confirm the key is non-zero before zeroing.
	allZero := true
	for _, b := range key {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("testKey() returned an all-zero key; test is invalid")
	}

	crypto.ZeroKey(key)

	for i, b := range key {
		if b != 0 {
			t.Errorf("ZeroKey() left non-zero byte at index %d: got %#x", i, b)
		}
	}
}

// --- EncryptString + DecryptString ---

func TestEncryptStringDecryptStringRoundTrip(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		plaintext string
	}{
		{
			name:      "regular string",
			plaintext: "sk-supersecretapikey-12345",
		},
		{
			name:      "empty string",
			plaintext: "",
		},
		{
			name:      "unicode string",
			plaintext: "cle-secrete-\u00e9\u00e0\u00fc",
		},
		{
			name:      "string with special characters",
			plaintext: "Bearer tok!@#$%^&*()_+-=[]{}|;':\",./<>?",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			key := testKey()

			encoded, err := crypto.EncryptString(tc.plaintext, key, nil)
			if err != nil {
				t.Fatalf("EncryptString() unexpected error: %v", err)
			}

			got, err := crypto.DecryptString(encoded, key, nil)
			if err != nil {
				t.Fatalf("DecryptString() unexpected error: %v", err)
			}

			if got != tc.plaintext {
				t.Errorf("DecryptString() = %q, want %q", got, tc.plaintext)
			}
		})
	}
}

func TestDecryptStringErrors(t *testing.T) {
	t.Parallel()

	key := testKey()

	wrongKey := make([]byte, 32)
	for i := range wrongKey {
		wrongKey[i] = 0xde
	}

	// Produce a valid encoded ciphertext to use in the wrong-key case.
	validEncoded, err := crypto.EncryptString("some secret", key, nil)
	if err != nil {
		t.Fatalf("setup: EncryptString() error: %v", err)
	}

	tests := []struct {
		name       string
		ciphertext string
		key        []byte
	}{
		{
			name:       "invalid base64 input",
			ciphertext: "this is not base64 !!!",
			key:        key,
		},
		{
			name:       "wrong key",
			ciphertext: validEncoded,
			key:        wrongKey,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := crypto.DecryptString(tc.ciphertext, tc.key, nil)
			if err == nil {
				t.Fatal("DecryptString() expected an error but got nil")
			}
		})
	}
}
