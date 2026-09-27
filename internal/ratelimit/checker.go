package ratelimit

// Checker evaluates request rate limits. Implementations must be safe for
// concurrent use. A nil Checker disables rate limiting entirely.
type Checker interface {
	// CheckRate verifies rate limits for every non-empty scope in s against
	// the corresponding limits in l. Returns ErrRateLimitExceeded if any
	// scope is over its limit.
	CheckRate(s Scopes, l ScopeLimits) error
}
