// Package storedetails reads the store page data of a game that is not on Steam, from the
// Epic Games Store or GOG, and maps it into the same steam.Details shape the Steam details job
// stores in game_details. Only fixed, key-free public hosts are called:
//
//	Epic: https://store-content-ipv4.ak.epicgames.com/api/en-US/content/products/<slug>
//	GOG:  https://catalog.gog.com/v1/catalog (search, to find the product id of a slug)
//	      https://api.gog.com/v2/games/<id> (description, requirements, ratings, screenshots)
//
// The slug or id is validated before any URL is built, redirects off the host are refused,
// bodies are capped at 5 MB and never logged.
package storedetails

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"time"

	"gamedeal-radar/daily-check/internal/storeprices"
)

// Fixed production endpoints. Tests point the unexported base fields of Client at httptest.
const (
	EpicContentBase = "https://store-content-ipv4.ak.epicgames.com/api/en-US/content/products/"
	GOGCatalogBase  = "https://catalog.gog.com/v1/catalog"
	GOGGamesBase    = "https://api.gog.com/v2/games/"
)

// MaxBodyBytes caps every response. A larger body is an error, never silently truncated.
const MaxBodyBytes = 5 << 20

// ErrNotFound means the store has no such product (HTTP 404, or a page without a product).
var ErrNotFound = errors.New("store has no such product")

// ErrBlocked means the store refused the job (HTTP 401, 403 or 429).
var ErrBlocked = errors.New("store refused the request")

// ErrInvalidID means a slug or id failed validation, so no request was sent.
var ErrInvalidID = errors.New("invalid store id")

var (
	epicSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,119}$`)
	gogSlugPattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,119}$`)
	gogIDPattern    = regexp.MustCompile(`^[0-9]{1,20}$`)
)

// ValidEpicSlug reports whether s is an Epic product slug such as "alan-wake-2". It is the
// only shape ever put into a URL path, so "../", "/", "?", "%", uppercase and URLs are refused.
func ValidEpicSlug(s string) bool { return epicSlugPattern.MatchString(s) }

// ValidGOGSlug reports whether s is a GOG product slug such as "alan_wake".
func ValidGOGSlug(s string) bool { return gogSlugPattern.MatchString(s) }

// Client fetches store pages. The zero value is not usable; use NewClient.
type Client struct {
	HTTP      *http.Client
	UserAgent string
	RetryWait time.Duration // wait before the single retry of a 5xx or network error

	epicBase       string
	gogCatalogBase string
	gogGamesBase   string
}

// NewClient returns a client for the fixed production hosts with a 20 second timeout, the
// jobs' honest User-Agent, and a redirect policy that never leaves the requested host.
func NewClient() *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout:       20 * time.Second,
			CheckRedirect: sameHostRedirects,
		},
		UserAgent:      storeprices.UserAgent,
		RetryWait:      2 * time.Second,
		epicBase:       EpicContentBase,
		gogCatalogBase: GOGCatalogBase,
		gogGamesBase:   GOGGamesBase,
	}
}

// sameHostRedirects allows at most 3 redirects, each https and on the original host.
func sameHostRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return errors.New("too many redirects")
	}
	if req.URL.Scheme != "https" || req.URL.Host != via[0].URL.Host {
		return errors.New("redirect off the store host refused")
	}
	return nil
}

// transientError marks a network failure or HTTP 5xx that survived the retry.
type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

// IsTransient reports whether err is a network failure or 5xx (worth a later retry).
func IsTransient(err error) bool {
	var te *transientError
	return errors.As(err, &te)
}

// get fetches target and returns the body of a 200 response. what names the endpoint in
// errors ("epic product page"); errors never carry the body. A 5xx or network error is
// retried once after RetryWait.
func (c *Client) get(ctx context.Context, target, what string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			if err := storeprices.SleepCtx(ctx, c.RetryWait); err != nil {
				return nil, err
			}
		}
		body, retry, err := c.once(ctx, target, what)
		if err == nil {
			return body, nil
		}
		if !retry {
			return nil, err
		}
		lastErr = err
	}
	return nil, &transientError{err: lastErr}
}

func (c *Client) once(ctx context.Context, target, what string) (body []byte, retry bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, false, fmt.Errorf("%s: building request: %w", what, err)
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		// The error text of a failed request names the URL, which only holds a validated
		// public slug or id. No body is involved.
		return nil, true, fmt.Errorf("%s: request failed: %w", what, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, false, fmt.Errorf("%s: HTTP 404: %w", what, ErrNotFound)
	case resp.StatusCode == http.StatusTooManyRequests, resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, false, fmt.Errorf("%s: HTTP %d: %w", what, resp.StatusCode, ErrBlocked)
	case resp.StatusCode >= 500:
		return nil, true, fmt.Errorf("%s: HTTP %d", what, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, false, fmt.Errorf("%s: HTTP %d", what, resp.StatusCode)
	}

	body, err = io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		return nil, true, fmt.Errorf("%s: reading response: %w", what, err)
	}
	if len(body) > MaxBodyBytes {
		return nil, false, fmt.Errorf("%s: response larger than %d bytes", what, MaxBodyBytes)
	}
	return body, false, nil
}
