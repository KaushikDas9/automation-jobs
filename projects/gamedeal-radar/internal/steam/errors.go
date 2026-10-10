package steam

import (
	"errors"
	"fmt"
)

var (
	// ErrNotFound means Steam did not return a successful entry for the app id.
	ErrNotFound = errors.New("steam app not found")
	// ErrPriceUnavailable means Steam returned no usable price (unreleased, region-locked, delisted).
	ErrPriceUnavailable = errors.New("steam price unavailable")
	// ErrRateLimited means Steam answered HTTP 429 on the last attempt.
	ErrRateLimited = errors.New("steam rate limited")
)

// StatusError is an unexpected HTTP status from the storefront API.
type StatusError struct {
	Code int
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("steam api returned unexpected status %d", e.Code)
}

// transientError marks a network or timeout failure that is worth retrying.
type transientError struct {
	err error
}

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

func transient(err error) error { return &transientError{err: err} }

// IsTransient reports whether a FetchPrice error is worth retrying later:
// network errors, timeouts, HTTP 429 and HTTP 5xx. Not found, price unavailable,
// other 4xx, parse errors, and context cancellation are permanent for this run.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrPriceUnavailable) {
		return false
	}
	if errors.Is(err, ErrRateLimited) {
		return true
	}
	var te *transientError
	if errors.As(err, &te) {
		return true
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se.Code == 429 || se.Code >= 500
	}
	return false
}
