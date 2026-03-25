package keygen_test

import (
	"strings"
	"testing"

	"github.com/voidmind-io/voidllm/pkg/keygen"
)

func TestGenerate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		keyType    string
		wantPrefix string
		wantLen    int
		wantErr    bool
	}{
		{
			name:       "user key has correct prefix and length",
			keyType:    keygen.KeyTypeUser,
			wantPrefix: keygen.PrefixUser,
			wantLen:    54,
			wantErr:    false,
		},
		{
			name:       "team key has correct prefix and length",
			keyType:    keygen.KeyTypeTeam,
			wantPrefix: keygen.PrefixTeam,
			wantLen:    54,
			wantErr:    false,
		},
		{
			name:       "service account key has correct prefix and length",
			keyType:    keygen.KeyTypeSA,
			wantPrefix: keygen.PrefixSA,
			wantLen:    54,
			wantErr:    false,
		},
		{
			name:    "invalid key type returns error",
			keyType: "invalid",
			wantErr: true,
		},
		{
			name:    "empty key type returns error",
			keyType: "",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := keygen.Generate(tc.keyType)

			if tc.wantErr {
				if err == nil {
					t.Errorf("Generate(%q) expected error, got nil", tc.keyType)
				}
				if got != "" {
					t.Errorf("Generate(%q) error case returned non-empty key %q", tc.keyType, got)
				}
				return
			}

			if err != nil {
				t.Fatalf("Generate(%q) unexpected error: %v", tc.keyType, err)
			}
			if !strings.HasPrefix(got, tc.wantPrefix) {
				t.Errorf("Generate(%q) = %q, want prefix %q", tc.keyType, got, tc.wantPrefix)
			}
			if len(got) != tc.wantLen {
				t.Errorf("Generate(%q) len = %d, want %d", tc.keyType, len(got), tc.wantLen)
			}
		})
	}
}

func TestGenerateUniqueness(t *testing.T) {
	t.Parallel()

	a, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("Generate first call: %v", err)
	}
	b, err := keygen.Generate(keygen.KeyTypeUser)
	if err != nil {
		t.Fatalf("Generate second call: %v", err)
	}
	if a == b {
		t.Errorf("Generate produced duplicate keys: %q == %q", a, b)
	}
}

func TestHash(t *testing.T) {
	t.Parallel()

	fixedSecret := []byte("aaaabbbbccccddddeeeeffffgggghhhh") // 32 bytes

	tests := []struct {
		name          string
		key           string
		secret        []byte
		compareKey    string
		compareSecret []byte
		wantMatch     bool
		wantHexLen    int
	}{
		{
			name:          "same key and secret produce same hash",
			key:           "vl_uk_abc123",
			secret:        fixedSecret,
			compareKey:    "vl_uk_abc123",
			compareSecret: fixedSecret,
			wantMatch:     true,
			wantHexLen:    64,
		},
		{
			name:          "same key different secret produces different hash",
			key:           "vl_uk_abc123",
			secret:        fixedSecret,
			compareKey:    "vl_uk_abc123",
			compareSecret: []byte("zzzzyyyyxxxxwwwwvvvvuuuuttttssss"),
			wantMatch:     false,
			wantHexLen:    64,
		},
		{
			name:          "different key same secret produces different hash",
			key:           "vl_uk_abc123",
			secret:        fixedSecret,
			compareKey:    "vl_uk_xyz789",
			compareSecret: fixedSecret,
			wantMatch:     false,
			wantHexLen:    64,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h1 := keygen.Hash(tc.key, tc.secret)
			h2 := keygen.Hash(tc.compareKey, tc.compareSecret)

			if len(h1) != tc.wantHexLen {
				t.Errorf("Hash() len = %d, want %d", len(h1), tc.wantHexLen)
			}
			if tc.wantMatch && h1 != h2 {
				t.Errorf("Hash() mismatch: %q != %q, expected equal", h1, h2)
			}
			if !tc.wantMatch && h1 == h2 {
				t.Errorf("Hash() collision: %q == %q, expected different", h1, h2)
			}
		})
	}
}

func TestHashDeterministic(t *testing.T) {
	t.Parallel()

	secret := []byte("aaaabbbbccccddddeeeeffffgggghhhh")
	key := "vl_uk_deterministic"

	h1 := keygen.Hash(key, secret)
	h2 := keygen.Hash(key, secret)

	if h1 != h2 {
		t.Errorf("Hash() not deterministic: %q != %q", h1, h2)
	}
}

func TestHint(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "long key produces truncated hint",
			input: "vl_uk_a3f2e8b1c9d4f6e7a3f2e8b1c9d4f6e7a3f2e8b1c9d4f6e7",
			want:  "vl_uk_...f6e7",
		},
		{
			name:  "exactly 10 chars returned as-is",
			input: "1234567890",
			want:  "1234567890",
		},
		{
			name:  "shorter than 10 chars returned as-is",
			input: "short",
			want:  "short",
		},
		{
			name:  "empty string returned as-is",
			input: "",
			want:  "",
		},
		{
			name:  "11 chars produces hint",
			input: "12345678901",
			want:  "123456...8901",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := keygen.Hint(tc.input)

			if got != tc.want {
				t.Errorf("Hint(%q) = %q, want %q", tc.input, got, tc.want)
			}
			if len(tc.input) > 10 && strings.Contains(got, tc.input) {
				t.Errorf("Hint(%q) = %q contains the full key", tc.input, got)
			}
		})
	}
}

func TestValidatePrefix(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		key         string
		wantKeyType string
		wantErr     bool
	}{
		{
			name:        "user key prefix recognized",
			key:         "vl_uk_anything",
			wantKeyType: keygen.KeyTypeUser,
			wantErr:     false,
		},
		{
			name:        "team key prefix recognized",
			key:         "vl_tk_anything",
			wantKeyType: keygen.KeyTypeTeam,
			wantErr:     false,
		},
		{
			name:        "service account prefix recognized",
			key:         "vl_sa_anything",
			wantKeyType: keygen.KeyTypeSA,
			wantErr:     false,
		},
		{
			name:    "openai-style key rejected",
			key:     "sk-anything",
			wantErr: true,
		},
		{
			name:    "empty key rejected",
			key:     "",
			wantErr: true,
		},
		{
			name:    "partial prefix rejected",
			key:     "vl_uk",
			wantErr: true,
		},
		{
			name:    "wrong separator rejected",
			key:     "vl-uk-anything",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			gotKeyType, err := keygen.ValidatePrefix(tc.key)

			if tc.wantErr {
				if err == nil {
					t.Errorf("ValidatePrefix(%q) expected error, got nil", tc.key)
				}
				if gotKeyType != "" {
					t.Errorf("ValidatePrefix(%q) error case returned non-empty key type %q", tc.key, gotKeyType)
				}
				return
			}

			if err != nil {
				t.Fatalf("ValidatePrefix(%q) unexpected error: %v", tc.key, err)
			}
			if gotKeyType != tc.wantKeyType {
				t.Errorf("ValidatePrefix(%q) = %q, want %q", tc.key, gotKeyType, tc.wantKeyType)
			}
		})
	}
}

func TestVerify(t *testing.T) {
	t.Parallel()

	secret := []byte("aaaabbbbccccddddeeeeffffgggghhhh") // 32 bytes
	wrongSecret := []byte("zzzzyyyyxxxxwwwwvvvvuuuuttttssss")
	key := "vl_uk_a3f2e8b1c9d4f6e7a3f2e8b1c9d4f6e7"
	wrongKey := "vl_uk_deadbeef00000000deadbeef00000000"

	correctHash := keygen.Hash(key, secret)

	tests := []struct {
		name       string
		key        string
		secret     []byte
		storedHash string
		want       bool
	}{
		{
			name:       "correct key and secret match stored hash",
			key:        key,
			secret:     secret,
			storedHash: correctHash,
			want:       true,
		},
		{
			name:       "correct key with wrong secret does not match",
			key:        key,
			secret:     wrongSecret,
			storedHash: correctHash,
			want:       false,
		},
		{
			name:       "wrong key with correct secret does not match",
			key:        wrongKey,
			secret:     secret,
			storedHash: correctHash,
			want:       false,
		},
		{
			name:       "invalid hex in stored hash returns false",
			key:        key,
			secret:     secret,
			storedHash: "not-valid-hex!!",
			want:       false,
		},
		{
			name:       "empty stored hash returns false",
			key:        key,
			secret:     secret,
			storedHash: "",
			want:       false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := keygen.Verify(tc.key, tc.secret, tc.storedHash)
			if got != tc.want {
				t.Errorf("Verify(%q, secret, %q) = %v, want %v", tc.key, tc.storedHash, got, tc.want)
			}
		})
	}
}

func BenchmarkGenerate(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := keygen.Generate(keygen.KeyTypeUser)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkHash(b *testing.B) {
	secret := []byte("aaaabbbbccccddddeeeeffffgggghhhh")
	key := "vl_uk_a3f2e8b1c9d4f6e7a3f2e8b1c9d4f6e7a3f2e8b1c9d4f6e7"

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			keygen.Hash(key, secret)
		}
	})
}
