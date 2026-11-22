package ratelimit

import (
	"errors"
	"time"
)

// LimitError wraps ErrRateLimitExceeded or ErrTokenBudgetExceeded together
// with the time after which the rejected request may succeed again. It
// satisfies errors.Is for the wrapped sentinel, so existing sentinel checks
// keep working.
type LimitError struct {
	// Err is the wrapped sentinel.
	Err error
	// RetryAfter is how long until the exceeded window resets.
	RetryAfter time.Duration
}

// Error returns the wrapped sentinel's message.
func (e *LimitError) Error() string { return e.Err.Error() }

// Unwrap returns the wrapped sentinel.
func (e *LimitError) Unwrap() error { return e.Err }

// RetryAfterFromError returns the reset delay carried by a LimitError in err's
// chain. ok is false when err carries none.
func RetryAfterFromError(err error) (d time.Duration, ok bool) {
	var le *LimitError
	if errors.As(err, &le) {
		return le.RetryAfter, true
	}
	return 0, false
}

// nextUTCMidnight returns the first instant after now at 00:00 UTC.
func nextUTCMidnight(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month(), n.Day()+1, 0, 0, 0, 0, time.UTC)
}

// nextUTCMonth returns the first instant after now at the 1st of the next
// month, 00:00 UTC.
func nextUTCMonth(now time.Time) time.Time {
	n := now.UTC()
	return time.Date(n.Year(), n.Month()+1, 1, 0, 0, 0, 0, time.UTC)
}
