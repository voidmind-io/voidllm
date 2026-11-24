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
	// Reason says why the request was rejected, "tokens" or "cost". It always
	// agrees with Err: "tokens" for ErrTokenBudgetExceeded (also when cost
	// budgets are exceeded as well), "cost" for ErrCostBudgetExceeded. Set for
	// budget errors only; for logs and metrics, never for clients.
	Reason string
	// RetryMetric names the metric ("tokens" or "cost") of the budget that
	// determines RetryAfter. It can differ from Reason: when token and cost
	// budgets are exceeded together, Reason is "tokens" while the cost budget
	// may reset last. Set for budget errors only.
	RetryMetric string
	// RetryScope names the kind of scope ("key", "user", "team" or "org") of
	// the budget that determines RetryAfter. Set for budget errors only.
	RetryScope string
	// RetryWindow names the window ("daily" or "monthly") of the budget that
	// determines RetryAfter. Set for budget errors only.
	RetryWindow string
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
