package storeprices

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// UserAgent identifies the jobs to the stores. Honest name, no browser spoofing.
const UserAgent = "GameDealRadar-jobs/1.0"

// maxBodyBytes caps how much of a response is read.
const maxBodyBytes = 5 << 20

// ErrBlocked means a store refused the job (HTTP 401, 403 or 429). The run must stop
// calling that store.
var ErrBlocked = errors.New("store blocked the request")

// transientError marks a network failure or HTTP 5xx, which is worth retrying later.
type transientError struct{ err error }

func (e *transientError) Error() string { return e.err.Error() }
func (e *transientError) Unwrap() error { return e.err }

// Transient wraps err so IsTransient reports true.
func Transient(err error) error { return &transientError{err: err} }

// IsTransient reports whether a store error is worth a retry pass.
func IsTransient(err error) bool {
	var te *transientError
	return errors.As(err, &te)
}

// IsBlocked reports whether a store error means the run must stop.
func IsBlocked(err error) bool { return errors.Is(err, ErrBlocked) }

// StatusError classifies an unexpected HTTP status. 401, 403 and 429 are blocked,
// 5xx is transient, and any other status is a permanent error for this game.
func StatusError(status int, what string) error {
	switch {
	case status == http.StatusTooManyRequests, status == http.StatusUnauthorized, status == http.StatusForbidden:
		return fmt.Errorf("%s: HTTP %d: %w", what, status, ErrBlocked)
	case status >= 500:
		return Transient(fmt.Errorf("%s: HTTP %d", what, status))
	default:
		return fmt.Errorf("%s: HTTP %d", what, status)
	}
}

// Limiter enforces a minimum gap between requests. A nil Limiter never waits.
type Limiter struct {
	Interval time.Duration
	mu       sync.Mutex
	last     time.Time
}

// Wait blocks until Interval has passed since the previous request started, or ctx ends.
func (l *Limiter) Wait(ctx context.Context) error {
	if l == nil || l.Interval <= 0 {
		return ctx.Err()
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() {
		if d := l.Interval - time.Since(l.last); d > 0 {
			if err := SleepCtx(ctx, d); err != nil {
				return err
			}
		}
	}
	l.last = time.Now()
	return nil
}

// Response is a fully read HTTP response.
type Response struct {
	Status int
	Header http.Header
	Body   []byte
}

// Fetcher performs one HTTP request at a time through the limiter. It never retries
// by itself: retries happen in the run's retry passes, so the waits are visible in logs.
type Fetcher struct {
	HTTP      *http.Client
	UserAgent string
	Limiter   *Limiter
}

// NewFetcher returns a Fetcher with a 15 second timeout and the given minimum request gap.
func NewFetcher(interval time.Duration) *Fetcher {
	return &Fetcher{
		HTTP:      &http.Client{Timeout: 15 * time.Second},
		UserAgent: UserAgent,
		Limiter:   &Limiter{Interval: interval},
	}
}

// Do sends req and reads the body. Network errors and context errors are returned
// as-is (transient for network errors). A non-2xx status is not an error here.
func (f *Fetcher) Do(ctx context.Context, req *http.Request) (Response, error) {
	if err := f.Limiter.Wait(ctx); err != nil {
		return Response{}, err
	}
	req.Header.Set("User-Agent", f.UserAgent)
	resp, err := f.HTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, Transient(fmt.Errorf("request failed: %w", err))
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, ctx.Err()
		}
		return Response{}, Transient(fmt.Errorf("reading response: %w", err))
	}
	return Response{Status: resp.StatusCode, Header: resp.Header, Body: body}, nil
}

// SleepCtx waits for d or until ctx is done. A non-positive d returns at once.
func SleepCtx(ctx context.Context, d time.Duration) error {
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
