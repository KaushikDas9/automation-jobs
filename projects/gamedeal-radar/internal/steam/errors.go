package steam

import "errors"

var (
	// ErrNotFound means Steam did not return a successful entry for the app id.
	ErrNotFound = errors.New("steam app not found")
	// ErrPriceUnavailable means Steam returned no usable price (unreleased, region-locked, delisted).
	ErrPriceUnavailable = errors.New("steam price unavailable")
	// ErrRateLimited means Steam answered HTTP 429 on the last attempt.
	ErrRateLimited = errors.New("steam rate limited")
)
