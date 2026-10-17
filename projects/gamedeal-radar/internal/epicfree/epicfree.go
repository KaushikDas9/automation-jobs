// Package epicfree reads Epic's public list of free games (the current and the upcoming
// offers) and turns it into rows for epic_free_games. It makes one request per run.
package epicfree

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Endpoint is Epic's public, unauthenticated free games feed, as used by Epic's own site.
const Endpoint = "https://store-site-backend-static.ak.epicgames.com/freeGamesPromotions?locale=en-US&country=IN&allowCountries=IN"

// UserAgent identifies the job to Epic. Honest name, no browser spoofing, no cookies.
const UserAgent = "PriceTrackerJob/1.0 (daily free games job)"

// Timeout is the whole-request timeout.
const Timeout = 30 * time.Second

// DefaultRetryWait is the pause before the single retry after a 5xx response.
const DefaultRetryWait = 10 * time.Second

// maxBodyBytes caps how much of the feed is read. The real feed is far smaller.
const maxBodyBytes = 5 << 20

// Client fetches the feed. The zero value is not usable; use NewClient.
type Client struct {
	HTTP      *http.Client
	URL       string
	UserAgent string
	RetryWait time.Duration
}

// NewClient returns a Client for the real endpoint.
func NewClient() *Client {
	return &Client{
		HTTP:      &http.Client{Timeout: Timeout},
		URL:       Endpoint,
		UserAgent: UserAgent,
		RetryWait: DefaultRetryWait,
	}
}

// FetchGames requests the feed once, retries once after a 5xx response, and parses the
// body. Any failure returns an error and no games. The body is never logged.
func (c *Client) FetchGames(ctx context.Context) ([]Game, error) {
	body, err := c.get(ctx)
	if err != nil {
		return nil, err
	}
	return Parse(body)
}

// get performs the request. A 5xx response is retried once after RetryWait. Network
// errors and other non-2xx statuses are returned without a retry.
func (c *Client) get(ctx context.Context) ([]byte, error) {
	status, body, err := c.once(ctx)
	if err == nil && status >= 500 {
		if err := sleep(ctx, c.RetryWait); err != nil {
			return nil, err
		}
		status, body, err = c.once(ctx)
	}
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("epic free games feed returned HTTP %d", status)
	}
	return body, nil
}

func (c *Client) once(ctx context.Context) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("building epic free games request: %w", err)
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("epic free games request failed: %w", errorKind(err))
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return 0, nil, fmt.Errorf("reading epic free games response: %w", errorKind(err))
	}
	if len(body) > maxBodyBytes {
		return 0, nil, errors.New("epic free games response is larger than the limit")
	}
	return resp.StatusCode, body, nil
}

// errorKind keeps the error text short and free of the request URL. Client errors embed
// the URL, which carries no secret, but the message stays readable without it.
func errorKind(err error) error {
	var ue interface{ Timeout() bool }
	if errors.As(err, &ue) && ue.Timeout() {
		return errors.New("timed out")
	}
	return errors.New("network error")
}

// sleep waits for d or until ctx is done. A non-positive d returns at once.
func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
