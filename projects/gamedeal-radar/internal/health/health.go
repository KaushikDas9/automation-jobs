// Package health sends dead man's switch pings (healthchecks.io style).
//
// The URL is a secret: it is never logged. Error text is scrubbed of it before being
// returned, and callers must not fail the job because a ping failed.
package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Pinger sends GET requests to a check URL. An empty URL disables pings entirely.
type Pinger struct {
	URL     string
	Client  *http.Client
	Timeout time.Duration // per attempt
	Retries int           // extra attempts after the first
	Backoff time.Duration // pause between attempts
}

// New returns a Pinger with production defaults: 10s timeout per attempt, one retry after 2s.
func New(url string) Pinger {
	return Pinger{
		URL:     strings.TrimSpace(url),
		Client:  &http.Client{Timeout: 15 * time.Second},
		Timeout: 10 * time.Second,
		Retries: 1,
		Backoff: 2 * time.Second,
	}
}

// Enabled reports whether a URL is configured.
func (p Pinger) Enabled() bool { return p.URL != "" }

// Success pings the check URL.
func (p Pinger) Success(ctx context.Context) error { return p.ping(ctx, "") }

// Fail pings the check URL's /fail endpoint.
func (p Pinger) Fail(ctx context.Context) error { return p.ping(ctx, "/fail") }

func (p Pinger) ping(ctx context.Context, suffix string) error {
	if !p.Enabled() {
		return nil
	}
	target := strings.TrimRight(p.URL, "/") + suffix

	var lastErr error
	for attempt := 0; attempt <= p.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return scrub(ctx.Err(), target)
			case <-time.After(p.Backoff):
			}
		}
		reqCtx, cancel := context.WithTimeout(ctx, p.Timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
		if err != nil {
			cancel()
			return scrub(err, target)
		}
		resp, err := p.Client.Do(req)
		cancel()
		if err != nil {
			lastErr = scrub(err, target)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("healthcheck responded with status %d", resp.StatusCode)
	}
	return lastErr
}

// scrub returns an error whose text no longer contains the check URL.
func scrub(err error, target string) error {
	if err == nil {
		return nil
	}
	msg := strings.ReplaceAll(err.Error(), target, "[healthcheck-url]")
	return errors.New(msg)
}
