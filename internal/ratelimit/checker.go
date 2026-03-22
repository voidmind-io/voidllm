package ratelimit

// Checker evaluates request rate limits. Implementations must be safe for
// concurrent use. A nil Checker disables rate limiting entirely.
type Checker interface {
	CheckRate(keyID, teamID, orgID string, keyLimits, teamLimits, orgLimits Limits) error
}
