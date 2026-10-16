package storeprices

import (
	"context"
	"log/slog"
	"time"
)

// MinRemaining is the time that must be left before the run deadline after a retry
// wait. Waits that would leave less are skipped.
const MinRemaining = 5 * time.Minute

// Verdict is what a process callback reports for one item in a pass.
type Verdict int

const (
	// Done means the item is finished for this run (stored, skipped, or failed for good).
	Done Verdict = iota
	// Retry means a transient failure; the item goes on the retry list.
	Retry
	// Stop means the store blocked the run. Nothing else is attempted.
	Stop
)

// PassOptions controls the retry passes.
type PassOptions struct {
	RetryWait1 time.Duration // wait before the first retry pass
	RetryWait2 time.Duration // wait before the second retry pass
	Deadline   time.Time     // zero means no deadline
	Now        func() time.Time
	Sleep      func(ctx context.Context, d time.Duration) error
}

// PassReport summarises RunPasses.
type PassReport struct {
	Retried int  // distinct items that entered a retry pass
	Stopped bool // a callback returned Stop, or ctx ended
}

// RunPasses calls process for every item. Items that return Retry are retried after
// each wait in turn (RetryWait1, then RetryWait2). A wait is skipped when it would leave
// less than MinRemaining before Deadline. The items still at Retry after the last pass
// are returned: the caller counts them as failed for this run.
func RunPasses[T any](ctx context.Context, items []T, o PassOptions, process func(T) Verdict) (left []T, rep PassReport) {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = SleepCtx
	}

	var retry []T
	for _, it := range items {
		if ctx.Err() != nil {
			rep.Stopped = true
			return nil, rep
		}
		switch process(it) {
		case Retry:
			retry = append(retry, it)
		case Stop:
			rep.Stopped = true
			return nil, rep
		}
	}
	rep.Retried = len(retry)

	for i, w := range []time.Duration{o.RetryWait1, o.RetryWait2} {
		if len(retry) == 0 {
			break
		}
		if !canWait(now(), o.Deadline, w) {
			slog.Warn("skipping retry pass, not enough time left before the run deadline",
				"pass", i+2, "items_left", len(retry))
			break
		}
		slog.Info("waiting before retry pass", "pass", i+2, "wait", w.String(), "items", len(retry))
		if err := sleep(ctx, w); err != nil {
			rep.Stopped = true
			break
		}
		var next []T
		for _, it := range retry {
			if ctx.Err() != nil {
				rep.Stopped = true
				break
			}
			switch process(it) {
			case Retry:
				next = append(next, it)
			case Stop:
				rep.Stopped = true
			}
			if rep.Stopped {
				break
			}
		}
		retry = next
		if rep.Stopped {
			break
		}
	}
	if rep.Stopped {
		return nil, rep
	}
	return retry, rep
}

// canWait reports whether waiting d still leaves MinRemaining before the deadline.
func canWait(now, deadline time.Time, d time.Duration) bool {
	if deadline.IsZero() {
		return true
	}
	return deadline.Sub(now)-d >= MinRemaining
}
