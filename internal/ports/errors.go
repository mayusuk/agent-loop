package ports

import (
	"errors"
	"time"
)

// The error classes every port method returns (INTEGRATION-SPEC §8). The core
// decides on the class with errors.Is, never on message text. Adapters wrap
// them, for example fmt.Errorf("issue 42: %w", ports.ErrNotFound).
var (
	// ErrNotFound: the item, change or other object doesn't exist.
	ErrNotFound = errors.New("not found")
	// ErrConflict: a precondition failed, for example the item changed
	// since it was read (Claim).
	ErrConflict = errors.New("conflict")
	// ErrRateLimited: the service refused the call for its rate limit.
	// Errors of this class are *RateLimitError, which carries the retry time.
	ErrRateLimited = errors.New("rate limited")
	// ErrPermission: the identity isn't allowed to make the call.
	ErrPermission = errors.New("permission denied")
	// ErrTransient: a temporary failure (network, server error); retry.
	ErrTransient = errors.New("transient failure")
)

// RateLimitError is an ErrRateLimited error with the time from which the call
// may be retried.
type RateLimitError struct {
	RetryAt time.Time
	Err     error // the underlying cause, may be nil
}

// RateLimited returns a *RateLimitError for a call that may be retried at
// retryAt, wrapping cause.
func RateLimited(retryAt time.Time, cause error) error {
	return &RateLimitError{RetryAt: retryAt, Err: cause}
}

func (e *RateLimitError) Error() string {
	msg := ErrRateLimited.Error() + " until " + e.RetryAt.UTC().Format(time.RFC3339)
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Is makes errors.Is(err, ErrRateLimited) true.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }

// Unwrap returns the cause.
func (e *RateLimitError) Unwrap() error { return e.Err }

// RetryAt returns the retry time of a rate-limited error anywhere in err's
// chain, and false if err isn't one.
func RetryAt(err error) (time.Time, bool) {
	var rl *RateLimitError
	if errors.As(err, &rl) {
		return rl.RetryAt, true
	}
	return time.Time{}, false
}
