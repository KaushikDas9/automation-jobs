package steam

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/url"
	"time"
)

// DefaultBaseURL is the public Steam storefront API root.
const DefaultBaseURL = "https://store.steampowered.com/api"

// Config holds HTTP and retry settings. Defaults match the server's steam.DefaultConfig.
type Config struct {
	BaseURL      string
	RateLimitRPS float64
	BurstSize    int
	HTTPTimeout  time.Duration
	BaseBackoff  time.Duration
	MaxBackoff   time.Duration
	MaxRetries   int
	UserAgent    string
}

// DefaultConfig returns the production defaults. Burst is 1 (stricter than the
// server's 3) so the job never exceeds 1 request in any one-second window.
func DefaultConfig() Config {
	return Config{
		BaseURL:      DefaultBaseURL,
		RateLimitRPS: 1.0,
		BurstSize:    1,
		HTTPTimeout:  10 * time.Second,
		BaseBackoff:  2 * time.Second,
		MaxBackoff:   60 * time.Second,
		MaxRetries:   3,
		UserAgent:    "GameDealRadar-DailyCheck/1.0 (+https://gamedealradar.in)",
	}
}

// Client fetches Steam storefront prices.
type Client struct {
	httpClient *http.Client
	limiter    *TokenBucket
	cfg        Config
}

// NewClient builds a client, filling zero-valued config fields with defaults.
func NewClient(cfg Config) *Client {
	d := DefaultConfig()
	if cfg.BaseURL == "" {
		cfg.BaseURL = d.BaseURL
	}
	if cfg.RateLimitRPS <= 0 {
		cfg.RateLimitRPS = d.RateLimitRPS
	}
	if cfg.BurstSize <= 0 {
		cfg.BurstSize = d.BurstSize
	}
	if cfg.HTTPTimeout <= 0 {
		cfg.HTTPTimeout = d.HTTPTimeout
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = d.BaseBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = d.MaxBackoff
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = d.MaxRetries
	}
	if cfg.UserAgent == "" {
		cfg.UserAgent = d.UserAgent
	}
	return &Client{
		httpClient: &http.Client{Timeout: cfg.HTTPTimeout},
		limiter:    NewTokenBucket(cfg.RateLimitRPS, cfg.BurstSize),
		cfg:        cfg,
	}
}

// FetchPrice retrieves the price for appID in countryCode ("in" for India/INR).
// It waits on the rate limiter before every attempt and retries transient failures
// (network, 429, 5xx) with exponential backoff plus jitter. Permanent failures
// (other 4xx, not found, price unavailable) return immediately. Errors are
// classified by IsTransient.
func (c *Client) FetchPrice(ctx context.Context, appID, countryCode string) (*PriceResult, error) {
	if countryCode == "" {
		countryCode = "in"
	}

	endpoint := fmt.Sprintf("%s/appdetails?appids=%s&cc=%s&filters=basic,price_overview",
		c.cfg.BaseURL, url.QueryEscape(appID), url.QueryEscape(countryCode))

	body, err := c.getBody(ctx, endpoint, appID)
	if err != nil {
		return nil, err
	}
	return ParseAppDetails(appID, countryCode, body, time.Now())
}

// getBody GETs endpoint through the rate limiter and returns the response body. label
// only names the request in logs and errors. It retries transient failures (network,
// 429, 5xx) with backoff; permanent failures return immediately.
func (c *Client) getBody(ctx context.Context, endpoint, label string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt <= c.cfg.MaxRetries; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return nil, fmt.Errorf("rate limiter wait cancelled: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("creating http request: %w", err)
		}
		req.Header.Set("User-Agent", c.cfg.UserAgent)
		req.Header.Set("Accept", "application/json")

		resp, err := c.httpClient.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = transient(fmt.Errorf("executing http request for %s: %w", label, err))
			slog.Warn("steam request network error, retrying", "request", label, "attempt", attempt)
			if err := sleepCtx(ctx, c.backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()
			wait := c.backoff(attempt)
			slog.Warn("steam rate limit hit (HTTP 429), pausing", "request", label, "pause", wait.String())
			c.limiter.Pause(wait)
			lastErr = ErrRateLimited
			continue
		}

		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			statusErr := &StatusError{Code: resp.StatusCode}
			slog.Warn("steam api non-200 status", "request", label, "status", resp.StatusCode)
			if !IsTransient(statusErr) {
				return nil, statusErr
			}
			lastErr = statusErr
			if err := sleepCtx(ctx, c.backoff(attempt)); err != nil {
				return nil, err
			}
			continue
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, transient(fmt.Errorf("reading steam response body: %w", err))
		}
		return body, nil
	}

	return nil, fmt.Errorf("steam request %s failed after %d retries: %w", label, c.cfg.MaxRetries, lastErr)
}

// backoff returns exponential backoff for attempt with 0-25% jitter, capped at MaxBackoff.
func (c *Client) backoff(attempt int) time.Duration {
	b := c.cfg.BaseBackoff * time.Duration(1<<attempt)
	if b > c.cfg.MaxBackoff || b <= 0 {
		b = c.cfg.MaxBackoff
	}
	jitter := time.Duration(0)
	if q := int64(b / 4); q > 0 {
		jitter = time.Duration(rand.Int63n(q))
	}
	return b + jitter
}
