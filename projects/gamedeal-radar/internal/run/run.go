// Package run orchestrates one daily check: resume within the slot, a main pass,
// timed retry passes for transient failures, and the coverage check. It depends
// only on interfaces so it can be tested without a database or network.
package run

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gamedeal-radar/daily-check/internal/pricing"
	"gamedeal-radar/daily-check/internal/steam"
)

const (
	// MinCoverage is the share of active mapped games that must be checked in the
	// slot for the run to count as successful.
	MinCoverage = 0.90

	// MinRemaining is the time that must be left before the run deadline after a
	// retry wait. Waits that would leave less are skipped.
	MinRemaining = 5 * time.Minute

	// Country is the Steam storefront country code (India, INR).
	Country = "in"
)

// ErrNoGames means no active game has a Steam mapping, so coverage is undefined.
var ErrNoGames = errors.New("no active games with a steam app mapping")

// Game is an active catalog title with its Steam mapping state.
type Game struct {
	ID            string
	Slug          string
	ExternalAppID string // empty when the game has no Steam mapping
	LastCheckedAt *time.Time
}

// Repo is the persistence port used by a run.
type Repo interface {
	pricing.Store
	// ListActiveSteamGames returns every active game, mapped or not, in priority order.
	ListActiveSteamGames(ctx context.Context) ([]Game, error)
	// CountCheckedSince counts active mapped games with last_checked_at >= since.
	CountCheckedSince(ctx context.Context, since time.Time) (int, error)
	// MarkChecked sets last_checked_at for the game's Steam mappings.
	MarkChecked(ctx context.Context, gameID string, at time.Time) error
}

// Fetcher is the price source (the Steam client in production).
type Fetcher interface {
	FetchPrice(ctx context.Context, appID, country string) (*steam.PriceResult, error)
}

// Options controls a run. Zero values are safe: no waits, no deadline, no limit.
type Options struct {
	SteamStoreID string
	SlotStart    time.Time
	DryRun       bool
	Limit        int           // 0 means no limit on games fetched
	RetryWait1   time.Duration // wait before the first retry pass
	RetryWait2   time.Duration // wait before the second retry pass
	Deadline     time.Time     // zero means no deadline
	Now          func() time.Time
	Sleep        func(ctx context.Context, d time.Duration) error
}

// Result summarises a run.
type Result struct {
	AlreadyComplete bool
	Cancelled       bool
	Total           int // active games with a Steam mapping
	Unmapped        int // active games without a Steam mapping (not counted in coverage)
	DoneBefore      int // checked in this slot before the run started
	Missing         int // not yet checked in this slot at start
	Retried         int // distinct games that entered a retry pass
	Updated         int // snapshots written (dry run: would be written)
	Skipped         int // no price on Steam (not found or unavailable)
	Failed          int // failed for good in this run
	CoverageBefore  float64
	CoverageAfter   float64
}

// Succeeded reports whether a coverage ratio meets the threshold.
func Succeeded(coverage float64) bool { return coverage >= MinCoverage }

type outcome int

const (
	outUpdated outcome = iota
	outSkipped
	outFailed
	outTransient
	outCancelled
)

// Execute runs one check for the slot. It returns an error only when the database
// cannot be read or written in a way that prevents an authoritative result.
func Execute(ctx context.Context, repo Repo, fetch Fetcher, o Options) (Result, error) {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = sleepCtx
	}

	var res Result
	games, err := repo.ListActiveSteamGames(ctx)
	if err != nil {
		return res, fmt.Errorf("listing active games: %w", err)
	}

	mapped := make([]Game, 0, len(games))
	for _, g := range games {
		if g.ExternalAppID == "" {
			res.Unmapped++
			continue
		}
		mapped = append(mapped, g)
	}
	res.Total = len(mapped)
	if res.Total == 0 {
		return res, ErrNoGames
	}

	var missing []Game
	for _, g := range mapped {
		if g.LastCheckedAt != nil && !g.LastCheckedAt.Before(o.SlotStart) {
			res.DoneBefore++
			continue
		}
		missing = append(missing, g)
	}
	res.Missing = len(missing)
	res.CoverageBefore = ratio(res.DoneBefore, res.Total)

	slog.Info("slot status",
		"slot_start", o.SlotStart.Format(time.RFC3339),
		"active_mapped_games", res.Total,
		"unmapped_games", res.Unmapped,
		"already_done", res.DoneBefore,
		"missing", res.Missing,
		"coverage_pct", pct(res.CoverageBefore),
	)

	if Succeeded(res.CoverageBefore) {
		res.AlreadyComplete = true
		res.CoverageAfter = res.CoverageBefore
		slog.Info("slot already complete, nothing to fetch")
		return res, nil
	}

	todo := missing
	if o.Limit > 0 && len(todo) > o.Limit {
		todo = todo[:o.Limit]
	}

	e := &engine{repo: repo, fetch: fetch, o: o, res: &res}

	// Main pass.
	var retry []Game
	for _, g := range todo {
		if ctx.Err() != nil {
			res.Cancelled = true
			break
		}
		switch e.process(ctx, g) {
		case outTransient:
			retry = append(retry, g)
		case outCancelled:
			res.Cancelled = true
		}
		if res.Cancelled {
			break
		}
	}
	res.Retried = len(retry)

	// Retry passes: wait, retry only transient failures, repeat once more if needed.
	waits := []time.Duration{o.RetryWait1, o.RetryWait2}
	for i, w := range waits {
		if len(retry) == 0 || res.Cancelled {
			break
		}
		if !canWait(now(), o.Deadline, w) {
			slog.Warn("skipping retry pass, not enough time left before the run deadline",
				"pass", i+2, "games_left", len(retry))
			break
		}
		slog.Info("waiting before retry pass", "pass", i+2, "wait", w.String(), "games", len(retry))
		if err := sleep(ctx, w); err != nil {
			res.Cancelled = true
			break
		}
		var next []Game
		for _, g := range retry {
			if ctx.Err() != nil {
				res.Cancelled = true
				break
			}
			switch e.process(ctx, g) {
			case outTransient:
				next = append(next, g)
			case outCancelled:
				res.Cancelled = true
			}
			if res.Cancelled {
				break
			}
		}
		retry = next
	}
	// Anything still on the retry list has failed for this run.
	res.Failed += len(retry)

	// Coverage. A real run recounts the database; a dry run projects the result
	// from what would have been written.
	if o.DryRun {
		res.CoverageAfter = ratio(res.DoneBefore+res.Updated+res.Skipped, res.Total)
	} else {
		countCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		checked, err := repo.CountCheckedSince(countCtx, o.SlotStart)
		cancel()
		if err != nil {
			return res, fmt.Errorf("recounting checked games: %w", err)
		}
		res.CoverageAfter = ratio(checked, res.Total)
	}

	slog.Info("run finished",
		"updated", res.Updated, "skipped", res.Skipped, "failed", res.Failed,
		"retried", res.Retried, "cancelled", res.Cancelled,
		"coverage_before_pct", pct(res.CoverageBefore), "coverage_after_pct", pct(res.CoverageAfter),
	)
	return res, nil
}

type engine struct {
	repo  Repo
	fetch Fetcher
	o     Options
	res   *Result
}

// process fetches and stores one game, updates the counters, and reports the outcome.
func (e *engine) now() time.Time {
	if e.o.Now != nil {
		return e.o.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *engine) process(ctx context.Context, g Game) outcome {
	price, err := e.fetch.FetchPrice(ctx, g.ExternalAppID, Country)
	if err != nil {
		if ctx.Err() != nil {
			return outCancelled
		}
		if errors.Is(err, steam.ErrNotFound) || errors.Is(err, steam.ErrPriceUnavailable) {
			// Record the attempt so backup runs in this slot do not fetch it again.
			// A dry run writes nothing.
			if !e.o.DryRun {
				if merr := e.repo.MarkChecked(ctx, g.ID, e.now()); merr != nil {
					e.res.Failed++
					slog.Warn("could not record skipped game as checked", "slug", g.Slug, "error", merr)
					return outFailed
				}
			}
			e.res.Skipped++
			slog.Info("skipped: no price on steam for this country", "slug", g.Slug, "app_id", g.ExternalAppID, "reason", err.Error())
			return outSkipped
		}
		if steam.IsTransient(err) {
			slog.Warn("price fetch failed (transient), will retry", "slug", g.Slug, "app_id", g.ExternalAppID, "error", err)
			return outTransient
		}
		e.res.Failed++
		slog.Warn("price fetch failed", "slug", g.Slug, "app_id", g.ExternalAppID, "error", err)
		return outFailed
	}

	if e.o.DryRun {
		e.res.Updated++
		slog.Info("dry run: would write snapshot", "slug", g.Slug, "app_id", g.ExternalAppID,
			"current_price", price.CurrentPrice, "discount_pct", price.DiscountPct, "is_deal", price.IsDeal)
		return outUpdated
	}

	if _, _, err := pricing.ProcessPriceResult(ctx, e.repo, g.ID, e.o.SteamStoreID, price); err != nil {
		if ctx.Err() != nil {
			return outCancelled
		}
		e.res.Failed++
		slog.Warn("processing price result failed", "slug", g.Slug, "error", err)
		return outFailed
	}
	e.res.Updated++
	return outUpdated
}

// canWait reports whether waiting d still leaves MinRemaining before the deadline.
func canWait(now, deadline time.Time, d time.Duration) bool {
	if deadline.IsZero() {
		return true
	}
	return deadline.Sub(now)-d >= MinRemaining
}

func ratio(n, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(n) / float64(total)
}

func pct(r float64) float64 { return float64(int(r*1000+0.5)) / 10 }

// sleepCtx waits for d or until ctx is done. A non-positive d returns immediately.
func sleepCtx(ctx context.Context, d time.Duration) error {
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
