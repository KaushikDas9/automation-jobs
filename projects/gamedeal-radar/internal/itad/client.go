// Package itad reads Epic Games Store prices through IsThereAnyDeal (ITAD). Epic itself is
// never called. ITAD's deal URLs are ITAD's tracked redirects with ITAD's affiliate tag,
// so the job never reads, stores or links them. Only the price amounts, currency and
// timestamp are kept.
//
// Docs: https://docs.isthereanydeal.com/
package itad

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"gamedeal-radar/daily-check/internal/storeprices"
)

// DefaultBaseURL is the ITAD API root.
const DefaultBaseURL = "https://api.isthereanydeal.com"

// EpicShopID is ITAD's shop id for the Epic Games Store.
const EpicShopID = 16

// MaxIDsPerRequest is ITAD's batch size limit used by this job.
const MaxIDsPerRequest = 200

// maxRetryAfter caps how long a Retry-After header may make the job wait.
const maxRetryAfter = 120 * time.Second

// keyHeader is the auth header. Its value is never logged or put in an error.
const keyHeader = "ITAD-API-Key"

// Client calls ITAD. It makes at most one request per Interval (the production value keeps
// the job far below ITAD's 1000 requests per 5 minutes).
type Client struct {
	Fetcher *storeprices.Fetcher
	BaseURL string
	APIKey  string
}

// UserAgent names this job to ITAD. It says what the program is, a daily store price job,
// and carries no project name, so a project rename never touches it. It is not a browser name.
const UserAgent = "PriceTrackerJob/1.0 (daily store price job)"

// newFetcher is the shared HTTP fetcher with this job's own User-Agent.
func newFetcher(gap time.Duration) *storeprices.Fetcher {
	f := storeprices.NewFetcher(gap)
	f.UserAgent = UserAgent
	return f
}

// NewClient returns a client with the production base URL and request gap.
// The gap of 500 ms caps the job at 2 requests per second.
func NewClient(apiKey string) *Client {
	return &Client{
		Fetcher: newFetcher(500 * time.Millisecond),
		BaseURL: DefaultBaseURL,
		APIKey:  apiKey,
	}
}

// do sends one API call. A 429 is retried once after its Retry-After; a second 429 is
// reported as blocked so the run stops.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) ([]byte, error) {
	base := c.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	target := strings.TrimRight(base, "/") + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}

	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding itad request: %w", err)
		}
		payload = b
	}

	for attempt := 0; attempt < 2; attempt++ {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, target, reader)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set(keyHeader, c.APIKey)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.Fetcher.Do(ctx, req)
		if err != nil {
			return nil, err
		}
		if resp.Status == http.StatusTooManyRequests {
			if attempt == 0 {
				wait := retryAfter(resp.Header)
				if err := storeprices.SleepCtx(ctx, wait); err != nil {
					return nil, err
				}
				continue
			}
			return nil, fmt.Errorf("itad %s: rate limited twice: %w", path, storeprices.ErrBlocked)
		}
		if resp.Status != http.StatusOK {
			return nil, storeprices.StatusError(resp.Status, "itad "+path)
		}
		return resp.Body, nil
	}
	return nil, errors.New("unreachable")
}

// retryAfter reads a Retry-After header given in seconds. The default is 30 seconds.
func retryAfter(h http.Header) time.Duration {
	if n, err := strconv.Atoi(strings.TrimSpace(h.Get("Retry-After"))); err == nil && n >= 0 {
		d := time.Duration(n) * time.Second
		if d > maxRetryAfter {
			d = maxRetryAfter
		}
		return d
	}
	return 30 * time.Second
}

// Lookup asks ITAD for the game with exactly this title. The caller applies the strict
// title rule with MatchTitle.
func (c *Client) Lookup(ctx context.Context, title string) (LookupResult, error) {
	q := url.Values{}
	q.Set("title", title)
	body, err := c.do(ctx, http.MethodGet, "/games/lookup/v1", q, nil)
	if err != nil {
		return LookupResult{}, err
	}
	return ParseLookup(body)
}

// Prices returns the Epic deal (shop 16) of each id for one country, from one batch request.
// ids must hold at most MaxIDsPerRequest entries.
func (c *Client) Prices(ctx context.Context, ids []string, country string) (map[string]Deal, error) {
	q := url.Values{}
	q.Set("country", country)
	q.Set("shops", strconv.Itoa(EpicShopID))
	body, err := c.do(ctx, http.MethodPost, "/games/prices/v3", q, ids)
	if err != nil {
		return nil, err
	}
	return ParsePrices(body, country)
}

// EpicLinks returns, per ITAD id, the raw first element of ITAD's Epic lookup (the
// address part). Use EpicPageURL to decide whether it can be used.
func (c *Client) EpicLinks(ctx context.Context, ids []string) (map[string]string, error) {
	q := url.Values{}
	body, err := c.do(ctx, http.MethodPost, fmt.Sprintf("/lookup/shop/%d/id/v1", EpicShopID), q, ids)
	if err != nil {
		return nil, err
	}
	return ParseShopLinks(body)
}
