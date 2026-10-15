// Package usd runs the daily USD price pass. It is separate from the INR check on purpose:
// a USD failure never changes the INR result or the job's exit code.
package usd

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gamedeal-radar/daily-check/internal/steam"
)

// Country is the Steam storefront country code for USD prices.
const Country = "us"

// Game is an active, Steam-mapped catalog title that has no USD price in the slot yet.
type Game struct {
	ID            string
	Slug          string
	ExternalAppID string
}

// Repo is the persistence port for the USD pass.
type Repo interface {
	// ListMissingUSD returns active mapped games with no USD price checked at or after since.
	ListMissingUSD(ctx context.Context, since time.Time) ([]Game, error)
	// SaveUSDPrice writes one USD snapshot and updates the latest USD price.
	SaveUSDPrice(ctx context.Context, gameID string, p steam.BatchPrice, checkedAt time.Time) error
}

// Fetcher is the price source (the Steam client in production).
type Fetcher interface {
	FetchBatchPrices(ctx context.Context, appIDs []string, countryCode string) (map[string]steam.BatchPrice, error)
}

// Options controls a pass.
type Options struct {
	SlotStart time.Time
	DryRun    bool
	Limit     int // 0 means no limit on games fetched
	Now       func() time.Time
}

// Result summarises a pass.
type Result struct {
	Missing     int // games without a USD price in this slot at start
	Saved       int // prices written (dry run: would be written)
	Unavailable int // Steam has no USD price (free, unreleased, delisted)
	Failed      int // fetch or write failed
}

// Execute fetches the USD price of every game still missing in the slot and stores it.
// Games Steam has no price for are counted, not stored. A fetch error leaves the games it
// did not cover for the next attempt in the slot.
func Execute(ctx context.Context, repo Repo, fetch Fetcher, o Options) (Result, error) {
	var res Result
	now := o.Now
	if now == nil {
		now = time.Now
	}

	missing, err := repo.ListMissingUSD(ctx, o.SlotStart)
	if err != nil {
		return res, fmt.Errorf("listing games missing a usd price: %w", err)
	}
	if o.Limit > 0 && len(missing) > o.Limit {
		missing = missing[:o.Limit]
	}
	res.Missing = len(missing)
	if len(missing) == 0 {
		slog.Info("usd pass: nothing to fetch")
		return res, nil
	}

	ids := make([]string, 0, len(missing))
	for _, g := range missing {
		ids = append(ids, g.ExternalAppID)
	}
	prices, fetchErr := fetch.FetchBatchPrices(ctx, ids, Country)
	if fetchErr != nil {
		slog.Warn("usd pass: price fetch failed, keeping what was fetched", "error", fetchErr)
	}

	for _, g := range missing {
		p, ok := prices[g.ExternalAppID]
		if !ok {
			res.Failed++
			continue
		}
		if p.Unavailable {
			res.Unavailable++
			continue
		}
		if p.Currency != "USD" {
			res.Failed++
			slog.Warn("usd pass: steam returned a different currency, not saving", "slug", g.Slug, "currency", p.Currency)
			continue
		}
		if o.DryRun {
			res.Saved++
			slog.Info("dry run: would write usd price", "slug", g.Slug, "current", p.Current, "discount_pct", p.DiscountPct)
			continue
		}
		if err := repo.SaveUSDPrice(ctx, g.ID, p, now().UTC()); err != nil {
			res.Failed++
			slog.Warn("usd pass: saving price failed", "slug", g.Slug, "error", err)
			continue
		}
		res.Saved++
	}

	slog.Info("usd pass finished", "missing", res.Missing, "saved", res.Saved, "unavailable", res.Unavailable, "failed", res.Failed)
	return res, nil
}
