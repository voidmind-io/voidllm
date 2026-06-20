package pii

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

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
//   - Fixed length per type: every EMAIL token is exactly 27 characters
//     (PII_EM_xxxxxxxxxxxxxxxxxxxxxxxxxxxx). This property is preserved for
//     Stage 0b's rolling-buffer streaming restore, where the buffer must
//     know the exact byte length of each token to detect chunk-split
//     boundaries.
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
// abbreviation used in the token format. Unknown types fall back to "XX"
// so that new detector types do not break the token format.
func typeAbbrev(typ string) string {
	switch typ {
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
	default:
		return "XX"
	}
}
