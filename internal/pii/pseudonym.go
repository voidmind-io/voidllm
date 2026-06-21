package pii

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// pseudonymLen is the fixed byte length of every pseudonym produced by
// pseudonym(). Format: "PII_" (4) + 2-char type abbrev + "_" (1) + 24 hex
// chars = 31 bytes. All pseudonym values share this exact length regardless
// of the PII type, which allows the rolling-buffer stream restorer to
// pre-size its carry window precisely.
const pseudonymLen = 31

// pseudonymMarker is the fixed prefix shared by every pseudonym. The rolling-
// buffer stream restorer uses this to determine which bytes in the carry buffer
// could be the start of a pseudonym that must not be emitted prematurely.
const pseudonymMarker = "PII_"

// pseudonym derives a deterministic, fixed-length replacement token for a
// PII value scoped to a specific organisation. The token format is:
//
//	PII_<TY>_<24 hex chars>
//
// where <TY> is a two-character uppercase abbreviation for the PII type
// and <24 hex chars> is the first 24 hex digits (12 bytes) of
// HMAC-SHA256(secret, orgID || 0x00 || type || 0x00 || normalize(value)).
//
// Design rationale:
//
//   - Per-org scope: including orgID in the HMAC input ensures that the same
//     PII value maps to a different pseudonym in each organisation. This
//     prevents cross-tenant correlation when the same real value (e.g. a
//     shared email address) appears in multiple orgs' requests.
//
//   - Type-binding: including the type with a NUL separator prevents
//     cross-type collisions where a value that happens to match two
//     different detector patterns (e.g. a 16-digit credit card that also
//     matches the TAX_ID pattern) would otherwise produce the same pseudonym
//     under different type labels.
//
//   - Collision probability: 12 bytes (96 bits) → ~1 in 2^96 per pair.
//     With up to 10,000 mappings per request (maxPIIMappings) the birthday
//     probability is negligible. A collision would cause rev[p] to be
//     overwritten, mapping the pseudonym to the wrong original value. The
//     96-bit tail makes this astronomically unlikely in practice.
//
//   - Fixed length: every pseudonym is exactly 31 characters regardless of
//     PII type (PII_<2-char abbrev>_<24 hex chars> = 4+2+1+24 = 31). This
//     property is used by Stage 0b's rolling-buffer streaming restorer, which
//     pre-sizes its per-choice carry window to pseudonymLen bytes so that it
//     can detect chunk-split boundaries precisely without generic look-ahead.
//
//   - [A-Za-z0-9_] alphabet only: no special characters that JSON
//     serializers, HTML escapers, or LLM tokenizers might alter. The token
//     passes through round-trips (JSON encode → LLM → JSON decode)
//     unchanged.
//
//   - Virtually zero natural collision: the PII_ prefix and type suffix
//     make accidental occurrence in real text extremely unlikely.
//
//   - Deterministic within (orgID, type, value): the same triple always
//     produces the same token. This guarantees cross-message consistency
//     within a request and, for the same installation, stable pseudonyms
//     across requests for the same org (though the current single-request
//     scope only requires within-request consistency).
//
// normalize applies type-specific normalization before hashing:
//   - EMAIL: trim whitespace, lowercase (case-insensitive identifier)
//   - all other types: trim whitespace only
//
// This means "User@Example.com" and "user@example.com" produce the same
// pseudonym, which is correct for identifiers whose canonical form is
// case-insensitive.
func pseudonym(secret []byte, orgID, typ, value string) string {
	norm := normalizeValue(typ, value)
	mac := hmac.New(sha256.New, secret)
	// HMAC input: orgID || NUL || type || NUL || normalized-value.
	// NUL separators prevent concatenation ambiguity between fields
	// (e.g. orgID="ab", type="cd" vs orgID="a", type="bcd").
	mac.Write([]byte(orgID))
	mac.Write([]byte{0x00})
	mac.Write([]byte(typ))
	mac.Write([]byte{0x00})
	mac.Write([]byte(norm))
	sum := mac.Sum(nil)
	abbr := typeAbbrev(typ)
	// 12 bytes → 24 hex characters → 96 bits of collision resistance.
	return "PII_" + abbr + "_" + hex.EncodeToString(sum[:12])
}

// normalizeValue produces a canonical form of value for the given PII type.
// Normalization is applied before hashing so that semantically equivalent
// values map to the same pseudonym.
func normalizeValue(typ, value string) string {
	v := strings.TrimSpace(value)
	if typ == "EMAIL" {
		v = strings.ToLower(v)
	}
	return v
}

// typeAbbrev maps a PII type name to a stable two-character uppercase
// abbreviation used in the token format. The returned value is always exactly
// two characters in [A-Z0-9], which keeps the canonical pseudonym shape
// (PII_<2 alnum>_<24 hex>) valid regardless of detector type.
//
// Known built-in and gazetteer types have fixed codes. For operator-supplied
// custom types, the first two [A-Za-z0-9] characters of the uppercased type
// are used; if fewer than two such characters exist, the shortfall is padded
// using a byte from the FNV-1a hash of the type so distinct types get
// distinct codes where possible.
func typeAbbrev(typ string) string {
	switch typ {
	// Stage 0 regex types (unchanged).
	case "EMAIL":
		return "EM"
	case "IBAN":
		return "IB"
	case "PHONE":
		return "PH"
	case "CREDIT_CARD":
		return "CC"
	case "TAX_ID":
		return "TX"
	// Gazetteer types.
	case "NAME":
		return "NM"
	case "PERSON":
		return "PN"
	case "CITY":
		return "CT"
	case "LOCATION":
		return "LO"
	case "ORG":
		return "OR"
	case "COMPANY":
		return "CO"
	default:
		return stableAbbrev(typ)
	}
}

// stableAbbrev derives a stable two-character [A-Z0-9] abbreviation for an
// arbitrary PII type string. It takes the first two alphanumeric characters
// from the uppercased type; if fewer than two are available, it pads using a
// character derived from the FNV-1a hash of the full type name so that
// distinct types produce distinct codes where possible.
func stableAbbrev(typ string) string {
	upper := strings.ToUpper(typ)
	var chars [2]byte

	idx := 0
	for _, r := range upper {
		if idx >= 2 {
			break
		}
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			chars[idx] = byte(r)
			idx++
		}
	}

	if idx < 2 {
		// Pad with a character derived from FNV-1a so distinct types differ.
		h := fnv1a(typ)
		const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
		for ; idx < 2; idx++ {
			chars[idx] = alphabet[h%uint32(len(alphabet))]
			h = (h >> 5) | (h << 27) // cheap rotation to get a second character
		}
	}

	return string(chars[:])
}

// fnv1a returns a 32-bit FNV-1a hash of s.
func fnv1a(s string) uint32 {
	const (
		offset uint32 = 2166136261
		prime  uint32 = 16777619
	)
	h := offset
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= prime
	}
	return h
}
