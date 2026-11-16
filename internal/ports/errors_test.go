package ports

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestErrorClassesMatchWithErrorsIs(t *testing.T) {
	classes := []error{ErrNotFound, ErrConflict, ErrRateLimited, ErrPermission, ErrTransient}
	for _, c := range classes {
		wrapped := fmt.Errorf("issue 42: %w", c)
		for _, other := range classes {
			if got, want := errors.Is(wrapped, other), c == other; got != want {
				t.Errorf("errors.Is(wrap(%v), %v) = %v, want %v", c, other, got, want)
			}
		}
	}
}

func TestRateLimitedCarriesRetryTime(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	cause := errors.New("403 secondary rate limit")
	err := fmt.Errorf("list events: %w", RateLimited(at, cause))

	if !errors.Is(err, ErrRateLimited) {
		t.Fatal("errors.Is(err, ErrRateLimited) = false")
	}
	for _, other := range []error{ErrNotFound, ErrConflict, ErrPermission, ErrTransient} {
		if errors.Is(err, other) {
			t.Errorf("rate-limited error also matches %v", other)
		}
	}
	if !errors.Is(err, cause) {
		t.Error("the cause isn't in the chain")
	}
	got, ok := RetryAt(err)
	if !ok || !got.Equal(at) {
		t.Fatalf("RetryAt = %v, %v; want %v, true", got, ok, at)
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) || !rl.RetryAt.Equal(at) {
		t.Fatalf("errors.As gave %+v", rl)
	}
	want := "list events: rate limited until 2026-11-15T12:00:00Z: 403 secondary rate limit"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
}

func TestRateLimitedWithoutCause(t *testing.T) {
	at := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	err := RateLimited(at, nil)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatal("errors.Is(err, ErrRateLimited) = false")
	}
	if errors.Unwrap(err) != nil {
		t.Error("Unwrap of a cause-less error isn't nil")
	}
	if err.Error() != "rate limited until 2026-11-15T12:00:00Z" {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestRetryAtOnOtherErrors(t *testing.T) {
	for _, err := range []error{nil, ErrRateLimited, ErrTransient, fmt.Errorf("x: %w", ErrNotFound)} {
		if at, ok := RetryAt(err); ok || !at.IsZero() {
			t.Errorf("RetryAt(%v) = %v, %v; want zero, false", err, at, ok)
		}
	}
}
