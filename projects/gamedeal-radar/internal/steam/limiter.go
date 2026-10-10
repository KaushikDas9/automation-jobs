package steam

import (
	"context"
	"sync"
	"time"
)

// TokenBucket is a thread-safe token bucket with an optional manual pause (used after HTTP 429).
// Port of the server's steam.TokenBucket.
type TokenBucket struct {
	rate       float64 // tokens added per second
	burst      int     // maximum token capacity
	tokens     float64
	lastRefill time.Time
	pauseUntil time.Time
	mu         sync.Mutex
}

// NewTokenBucket creates a limiter with the given tokens per second and burst size.
func NewTokenBucket(rate float64, burst int) *TokenBucket {
	if rate <= 0 {
		rate = 1.0
	}
	if burst <= 0 {
		burst = 1
	}
	return &TokenBucket{
		rate:       rate,
		burst:      burst,
		tokens:     float64(burst),
		lastRefill: time.Now(),
	}
}

// Wait blocks until a token is available or ctx is done.
func (tb *TokenBucket) Wait(ctx context.Context) error {
	for {
		tb.mu.Lock()
		now := time.Now()

		if now.Before(tb.pauseUntil) {
			remaining := tb.pauseUntil.Sub(now)
			tb.mu.Unlock()
			if err := sleepCtx(ctx, remaining); err != nil {
				return err
			}
			continue
		}

		elapsed := now.Sub(tb.lastRefill).Seconds()
		tb.tokens += elapsed * tb.rate
		if tb.tokens > float64(tb.burst) {
			tb.tokens = float64(tb.burst)
		}
		tb.lastRefill = now

		if tb.tokens >= 1.0 {
			tb.tokens--
			tb.mu.Unlock()
			return nil
		}

		needed := 1.0 - tb.tokens
		wait := time.Duration(needed / tb.rate * float64(time.Second))
		tb.mu.Unlock()

		if err := sleepCtx(ctx, wait); err != nil {
			return err
		}
	}
}

// Pause suspends all outgoing requests for d (keeps the longest pause).
func (tb *TokenBucket) Pause(d time.Duration) {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if target := time.Now().Add(d); target.After(tb.pauseUntil) {
		tb.pauseUntil = target
	}
}

// sleepCtx sleeps for d or returns ctx.Err() if the context ends first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
