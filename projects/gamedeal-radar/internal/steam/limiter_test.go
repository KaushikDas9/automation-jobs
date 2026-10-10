package steam

import (
	"context"
	"testing"
	"time"
)

func TestTokenBucketEnforcesRate(t *testing.T) {
	// 10 req/s, burst 1: the first call is immediate, then each call waits ~100ms.
	tb := NewTokenBucket(10, 1)
	ctx := context.Background()

	start := time.Now()
	for i := 0; i < 4; i++ {
		if err := tb.Wait(ctx); err != nil {
			t.Fatalf("wait %d: %v", i, err)
		}
	}
	elapsed := time.Since(start)
	// 3 refills at 100ms each = ~300ms minimum. Allow a little scheduler slack downward.
	if elapsed < 250*time.Millisecond {
		t.Fatalf("4 requests at 10 rps finished in %v, too fast", elapsed)
	}
}

func TestTokenBucketPauseBlocksAndRespectsContext(t *testing.T) {
	tb := NewTokenBucket(100, 1)
	tb.Pause(5 * time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	if err := tb.Wait(ctx); err == nil {
		t.Fatal("expected context error while paused")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("Wait did not return promptly on context cancellation")
	}
}
