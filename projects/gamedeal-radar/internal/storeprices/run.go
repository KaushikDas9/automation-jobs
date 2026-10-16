package storeprices

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// ErrNoGames means there are no active games, so coverage is undefined.
var ErrNoGames = errors.New("no active games")

// MinCoverage is the share of games that must be handled for a run to succeed.
const MinCoverage = 0.90

// AlreadyCompleteWindow is how recent a row must be for its game to count as done.
const AlreadyCompleteWindow = 20 * time.Hour

// Game is an active catalog title.
type Game struct {
	ID   string
	Name string
}

// StoredKey identifies one stored row for a game and region.
type StoredKey struct {
	GameID string
	Region string
}

// SaveResult is what SaveGameRows did.
type SaveResult struct {
	Written    int // rows inserted or updated
	KeptManual int // rows left alone because a manual row exists
}

// Repo is the persistence port for the store jobs. The database implementation lives in
// internal/db. Every write is one transaction per game.
type Repo interface {
	// ActiveGames returns every active game in priority order.
	ActiveGames(ctx context.Context) ([]Game, error)
	// StoredCents returns the stored current price (in cents) of every row for the store
	// that has one. Used for the jump check.
	StoredCents(ctx context.Context, storeCode string) (map[StoredKey]int64, error)
	// ManualGameIDs returns the games with at least one source='manual' row for the store.
	ManualGameIDs(ctx context.Context, storeCode string) (map[string]bool, error)
	// CheckedCount returns how many active games have a row for the store checked at or
	// after since.
	CheckedCount(ctx context.Context, storeCode string, since time.Time) (int, error)
	// SaveGameRows writes the rows of one game in one transaction. Rows for source='manual'
	// are never touched.
	SaveGameRows(ctx context.Context, storeCode, gameID string, rows []Price) (SaveResult, error)
}

// Options controls a store run. Zero values are safe.
type Options struct {
	DryRun bool
	Manual bool // workflow_dispatch: skips the already-complete check
	Limit  int  // 0 means all active games
	PassOptions
}

// Summary reports what a run did. Counts about games are over the games in this run
// (after LIMIT).
type Summary struct {
	DryRun          bool
	AlreadyComplete bool
	Blocked         bool // the store refused the run (401, 403 or 429)
	Cancelled       bool // the run was cut short (signal or deadline)
	Games           int
	Failed          int
	Retried         int
	Written         int            // rows written (dry run: rows that would be written)
	KeptManual      int            // rows left alone because a manual row exists
	SkippedManual   int            // games skipped entirely because they have a manual row
	SkippedSanity   int            // rows skipped by a sanity check
	NoMatch         int            // Epic: games with no IsThereAnyDeal match
	TitleMismatch   int            // Epic: matches rejected by the strict title rule
	Matched         int            // Epic: games matched to IsThereAnyDeal
	WithPrice       map[string]int // region -> games with a usable price row
	NoPrice         map[string]int // region -> matched games with no Epic deal (Epic)
	NotListed       map[string]int // region -> games the store does not list (GOG)
	ListedNoPrice   map[string]int // region -> games listed without a price (GOG)
	SlugLinks       int            // Epic: store links built from a slug
	SearchLinks     int            // Epic: store links built as a search link
	Coverage        float64
}

// NewSummary returns a Summary with its maps ready.
func NewSummary(dryRun bool) Summary {
	return Summary{
		DryRun:        dryRun,
		WithPrice:     map[string]int{},
		NoPrice:       map[string]int{},
		NotListed:     map[string]int{},
		ListedNoPrice: map[string]int{},
	}
}

// CoverageOf returns the handled share of games: (total - failed) / total.
func CoverageOf(total, failed int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(total-failed) / float64(total)
}

// Succeeded reports whether a coverage ratio meets MinCoverage.
func Succeeded(coverage float64) bool { return coverage >= MinCoverage }

// LoadGames returns the working set: active games, cut to Limit. The full list is
// returned too, for the already-complete check.
func LoadGames(ctx context.Context, repo Repo, limit int) (working, all []Game, err error) {
	all, err = repo.ActiveGames(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("listing active games: %w", err)
	}
	if len(all) == 0 {
		return nil, nil, ErrNoGames
	}
	working = all
	if limit > 0 && len(working) > limit {
		working = working[:limit]
	}
	return working, all, nil
}

// AlreadyComplete reports whether at least 90% of the active games already have a row for
// the store checked within AlreadyCompleteWindow. It is never applied to dry runs or
// manual runs.
func AlreadyComplete(ctx context.Context, repo Repo, storeCode string, o Options, now time.Time, totalActive int) (bool, error) {
	if o.DryRun || o.Manual || totalActive == 0 {
		return false, nil
	}
	checked, err := repo.CheckedCount(ctx, storeCode, now.Add(-AlreadyCompleteWindow))
	if err != nil {
		return false, fmt.Errorf("counting checked games: %w", err)
	}
	return checked*10 >= totalActive*9, nil
}

// Clock returns the run clock, honouring the test override.
func (o Options) Clock() time.Time {
	if o.PassOptions.Now != nil {
		return o.PassOptions.Now().UTC()
	}
	return time.Now().UTC()
}

// GOGPricer is the GOG lookup used by RunGOG (GOGClient in production).
type GOGPricer interface {
	Price(ctx context.Context, name, region string) (Price, error)
}

// GOGRegions are the buyer countries GOG is asked about. GOG prices both in USD.
var GOGRegions = []string{RegionUS, RegionIN}

// RunGOG prices every working game on GOG, for US and IN, and saves both regions of a game
// in one transaction. A blocked response stops the run. Transient failures go through the
// retry passes. A game that still fails is counted, and nothing is written for it.
func RunGOG(ctx context.Context, repo Repo, api GOGPricer, o Options) (Summary, error) {
	sum := NewSummary(o.DryRun)
	games, all, err := LoadGames(ctx, repo, o.Limit)
	if err != nil {
		return sum, err
	}
	sum.Games = len(games)

	done, err := AlreadyComplete(ctx, repo, StoreGOG, o, o.Clock(), len(all))
	if err != nil {
		return sum, err
	}
	if done {
		sum.AlreadyComplete = true
		sum.Coverage = 1
		slog.Info("gog already complete, nothing to fetch", "active_games", len(all))
		return sum, nil
	}

	stored, err := repo.StoredCents(ctx, StoreGOG)
	if err != nil {
		return sum, fmt.Errorf("loading stored gog prices: %w", err)
	}

	failed := map[string]bool{}
	left, rep := RunPasses(ctx, games, o.PassOptions, func(g Game) Verdict {
		rows := make([]Price, 0, len(GOGRegions))
		for _, region := range GOGRegions {
			p, err := api.Price(ctx, g.Name, region)
			if err != nil {
				switch {
				case IsBlocked(err):
					sum.Blocked = true
					slog.Warn("gog blocked the request, stopping the run", "error", err)
					return Stop
				case IsTransient(err):
					slog.Warn("gog lookup failed (transient), will retry", "game", g.Name, "region", region, "error", err)
					return Retry
				default:
					failed[g.ID] = true
					slog.Warn("gog lookup failed, nothing written", "game", g.Name, "region", region, "error", err)
					return Done
				}
			}
			rows = append(rows, p)
		}
		writeGOGRows(ctx, repo, g, rows, stored, o, &sum, failed)
		return Done
	})
	sum.Retried = rep.Retried
	sum.Cancelled = rep.Stopped && !sum.Blocked
	for _, g := range left {
		failed[g.ID] = true
	}
	sum.Failed = len(failed)
	sum.Coverage = CoverageOf(len(games), sum.Failed)
	if sum.Blocked || sum.Cancelled {
		// An interrupted run is not counted as covered: games after the stop were never handled.
		sum.Coverage = 0
	}
	return sum, nil
}

// writeGOGRows applies the sanity checks and writes (or, for a dry run, prints) the rows of one game.
func writeGOGRows(ctx context.Context, repo Repo, g Game, rows []Price, stored map[StoredKey]int64, o Options, sum *Summary, failed map[string]bool) {
	var keep []Price
	for _, p := range rows {
		countStatus(sum, p)
		var prev *int64
		if v, ok := stored[StoredKey{GameID: g.ID, Region: p.Region}]; ok {
			prev = &v
		}
		if reason := CheckPrice(p, "USD", prev); reason != "" {
			sum.SkippedSanity++
			slog.Warn("gog price skipped by sanity check, stored row kept", "game", g.Name, "region", p.Region, "reason", reason)
			continue
		}
		keep = append(keep, p)
	}
	if len(keep) == 0 {
		return
	}
	if o.DryRun {
		for _, p := range keep {
			sum.Written++
			fmt.Printf("dry run: gog %-3s %-14s cur=%-8s base=%-8s disc=%3d%%  %s  game=%q\n",
				p.Region, p.Status, centsText(p.CurrentCents), centsText(p.BaseCents), p.DiscountPct, p.StoreURL, g.Name)
		}
		return
	}
	res, err := repo.SaveGameRows(ctx, StoreGOG, g.ID, keep)
	if err != nil {
		failed[g.ID] = true
		slog.Error("saving gog rows failed, nothing written for this game", "game", g.Name, "error", err)
		return
	}
	sum.Written += res.Written
	sum.KeptManual += res.KeptManual
}

// countStatus updates the per-region counters for one fetched row.
func countStatus(sum *Summary, p Price) {
	switch p.Status {
	case StatusOK, StatusFree:
		sum.WithPrice[p.Region]++
	case StatusNotListed:
		sum.NotListed[p.Region]++
	case StatusListedNoPrice:
		sum.ListedNoPrice[p.Region]++
	}
}

func centsText(c *int64) string {
	if c == nil {
		return "-"
	}
	return FormatCents(*c)
}
