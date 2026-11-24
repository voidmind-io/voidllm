package ratelimit

import (
	"errors"
	"time"
)

// LimitError wraps ErrRateLimitExceeded, ErrTokenBudgetExceeded or
// ErrCostBudgetExceeded together with the time after which the rejected
// request may succeed again. It satisfies errors.Is for the wrapped sentinel,
// so existing sentinel checks keep working.
type LimitError struct {
	// Err is the wrapped sentinel.
	Err error
	// RetryAfter is how long until the exceeded window resets.
	RetryAfter time.Duration
	// Metric names the metric of the budget that determines RetryAfter
	// ("tokens" or "cost"). When token and cost budgets are exceeded together
	// it can be "cost" although Err is ErrTokenBudgetExceeded; which kind of
	// rejection it is follows from Err. Set for budget errors only; for logs
	// and metrics, never for clients.
	Metric string
	// Scope names the kind of scope whose budget determines RetryAfter
	// ("key", "user", "team" or "org"). Set for budget errors only; for logs
	// and metrics, never for clients.
	Scope string
	// Window names the window that determines RetryAfter ("daily" or
	// "monthly"). Set for budget errors only; for logs and metrics, never for
	// clients.
	Window string
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
