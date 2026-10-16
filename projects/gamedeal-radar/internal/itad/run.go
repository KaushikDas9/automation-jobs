package itad

import (
	"context"
	"fmt"
	"log/slog"

	"gamedeal-radar/daily-check/internal/storeprices"
)

// API is the price API surface the Epic run uses. Client implements it; tests use fakes.
type API interface {
	Lookup(ctx context.Context, title string) (LookupResult, error)
	Prices(ctx context.Context, ids []string, country string) (map[string]Deal, error)
	EpicLinks(ctx context.Context, ids []string) (map[string]string, error)
}

// epicRegions maps each stored region to the provider's country code and the currency that
// the provider must return for it.
var epicRegions = []struct {
	Region   string
	Country  string
	Currency string
}{
	{storeprices.RegionIN, "IN", "INR"},
	{storeprices.RegionUS, "US", "USD"},
}

type match struct {
	game   storeprices.Game
	itadID string
}

type priceJob struct {
	country string
	region  string
	matches []match
}

// RunEpic looks every working game up on the provider, reads the Epic (shop 16) price for India
// and the US, and saves the rows. The stages are: lookup (strict title rule), batched price
// requests per country, then batched Epic link lookups. A game is failed only when one of
// its requests still fails after the retry passes; nothing is written for a failed game.
// Games with a manual Epic row are left completely alone, price and link.
func RunEpic(ctx context.Context, repo storeprices.Repo, api API, o storeprices.Options) (storeprices.Summary, error) {
	sum := storeprices.NewSummary(o.DryRun)
	games, all, err := storeprices.LoadGames(ctx, repo, o.Limit)
	if err != nil {
		return sum, err
	}
	sum.Games = len(games)

	done, err := storeprices.AlreadyComplete(ctx, repo, storeprices.StoreEpic, o, o.Clock(), len(all))
	if err != nil {
		return sum, err
	}
	if done {
		sum.AlreadyComplete = true
		sum.Coverage = 1
		slog.Info("epic already complete, nothing to fetch", "active_games", len(all))
		return sum, nil
	}

	manual, err := repo.ManualGameIDs(ctx, storeprices.StoreEpic)
	if err != nil {
		return sum, fmt.Errorf("loading manual epic rows: %w", err)
	}
	stored, err := repo.StoredCents(ctx, storeprices.StoreEpic)
	if err != nil {
		return sum, fmt.Errorf("loading stored epic prices: %w", err)
	}

	failed := map[string]bool{}
	var todo []storeprices.Game
	for _, g := range games {
		if manual[g.ID] {
			sum.SkippedManual++
			continue
		}
		todo = append(todo, g)
	}

	// Stage 1: lookup. Only an exact normalized title is accepted.
	var matches []match
	left, rep := storeprices.RunPasses(ctx, todo, o.PassOptions, func(g storeprices.Game) storeprices.Verdict {
		res, err := api.Lookup(ctx, g.Name)
		if err != nil {
			switch {
			case storeprices.IsBlocked(err):
				sum.Blocked = true
				slog.Warn("price API blocked the request, stopping the run", "error", err)
				return storeprices.Stop
			case storeprices.IsTransient(err):
				slog.Warn("price API lookup failed (transient), will retry", "game", g.Name, "error", err)
				return storeprices.Retry
			default:
				failed[g.ID] = true
				slog.Warn("price API lookup failed, nothing written", "game", g.Name, "error", err)
				return storeprices.Done
			}
		}
		if !res.Found {
			sum.NoMatch++
			slog.Info("no provider match, skipped", "game", g.Name)
			return storeprices.Done
		}
		if !MatchTitle(g.Name, res) {
			sum.TitleMismatch++
			slog.Info("provider title differs after normalization, skipped", "game", g.Name, "itad_title", res.Title)
			return storeprices.Done
		}
		matches = append(matches, match{game: g, itadID: res.ID})
		return storeprices.Done
	})
	sum.Retried += rep.Retried
	markFailed(failed, left)
	if sum.Blocked || rep.Stopped {
		return finishEpic(sum, games, failed, rep.Stopped), nil
	}
	sum.Matched = len(matches)

	// Stage 2: prices, one batch per country and chunk.
	deals := map[string]map[string]Deal{}
	var jobs []priceJob
	for _, r := range epicRegions {
		deals[r.Region] = map[string]Deal{}
		for _, chunk := range chunkMatches(matches) {
			jobs = append(jobs, priceJob{country: r.Country, region: r.Region, matches: chunk})
		}
	}
	leftJobs, rep2 := storeprices.RunPasses(ctx, jobs, o.PassOptions, func(j priceJob) storeprices.Verdict {
		got, err := api.Prices(ctx, matchIDs(j.matches), j.country)
		if err != nil {
			switch {
			case storeprices.IsBlocked(err):
				sum.Blocked = true
				slog.Warn("price API blocked the price request, stopping the run", "error", err)
				return storeprices.Stop
			case storeprices.IsTransient(err):
				slog.Warn("price API price request failed (transient), will retry", "country", j.country, "games", len(j.matches), "error", err)
				return storeprices.Retry
			default:
				markMatchesFailed(failed, j.matches)
				slog.Warn("price API price request failed, nothing written for its games", "country", j.country, "games", len(j.matches), "error", err)
				return storeprices.Done
			}
		}
		for id, d := range got {
			deals[j.region][id] = d
		}
		return storeprices.Done
	})
	sum.Retried += rep2.Retried
	for _, j := range leftJobs {
		markMatchesFailed(failed, j.matches)
	}
	if sum.Blocked || rep2.Stopped {
		return finishEpic(sum, games, failed, rep2.Stopped), nil
	}

	// Stage 3: Epic address links, one batch per chunk.
	links := map[string]string{}
	var linkJobs [][]match
	linkJobs = append(linkJobs, chunkMatches(matches)...)
	leftLinks, rep3 := storeprices.RunPasses(ctx, linkJobs, o.PassOptions, func(ms []match) storeprices.Verdict {
		got, err := api.EpicLinks(ctx, matchIDs(ms))
		if err != nil {
			switch {
			case storeprices.IsBlocked(err):
				sum.Blocked = true
				slog.Warn("price API blocked the link request, stopping the run", "error", err)
				return storeprices.Stop
			case storeprices.IsTransient(err):
				slog.Warn("price API link request failed (transient), will retry", "games", len(ms), "error", err)
				return storeprices.Retry
			default:
				markMatchesFailed(failed, ms)
				slog.Warn("price API link request failed, nothing written for its games", "games", len(ms), "error", err)
				return storeprices.Done
			}
		}
		for id, v := range got {
			links[id] = v
		}
		return storeprices.Done
	})
	sum.Retried += rep3.Retried
	for _, ms := range leftLinks {
		markMatchesFailed(failed, ms)
	}
	if sum.Blocked || rep3.Stopped {
		return finishEpic(sum, games, failed, rep3.Stopped), nil
	}

	// Build and write the rows, one transaction per game.
	for _, m := range matches {
		if failed[m.game.ID] {
			continue
		}
		storeURL, usedSlug := epicStoreURL(m.game.Name, links[m.itadID])
		var prevs = func(region string) *int64 {
			if v, ok := stored[storeprices.StoredKey{GameID: m.game.ID, Region: region}]; ok {
				return &v
			}
			return nil
		}

		var rows []storeprices.Price
		for _, r := range epicRegions {
			deal, ok := deals[r.Region][m.itadID]
			if !ok {
				sum.NoPrice[r.Region]++
				continue
			}
			p, reason := PriceFromDeal(deal, r.Region, storeURL)
			if reason == "" {
				reason = storeprices.CheckPrice(p, r.Currency, prevs(r.Region))
			}
			if reason != "" {
				sum.SkippedSanity++
				slog.Warn("epic price skipped by sanity check, stored row kept", "game", m.game.Name, "region", r.Region, "reason", reason)
				continue
			}
			rows = append(rows, p)
			sum.WithPrice[r.Region]++
		}
		if len(rows) == 0 {
			continue
		}
		if usedSlug {
			sum.SlugLinks++
		} else {
			sum.SearchLinks++
		}
		if o.DryRun {
			for _, p := range rows {
				sum.Written++
				fmt.Printf("dry run: epic %-3s %-14s cur=%-8s base=%-8s disc=%3d%%  %s  game=%q\n",
					p.Region, p.Status, centsText(p.CurrentCents), centsText(p.BaseCents), p.DiscountPct, p.StoreURL, m.game.Name)
			}
			continue
		}
		res, err := repo.SaveGameRows(ctx, storeprices.StoreEpic, m.game.ID, rows)
		if err != nil {
			failed[m.game.ID] = true
			slog.Error("saving epic rows failed, nothing written for this game", "game", m.game.Name, "error", err)
			continue
		}
		sum.Written += res.Written
		sum.KeptManual += res.KeptManual
	}

	return finishEpic(sum, games, failed, false), nil
}

// finishEpic fills the failure and coverage fields.
func finishEpic(sum storeprices.Summary, games []storeprices.Game, failed map[string]bool, cancelled bool) storeprices.Summary {
	sum.Failed = len(failed)
	sum.Cancelled = cancelled && !sum.Blocked
	sum.Coverage = storeprices.CoverageOf(len(games), sum.Failed)
	if sum.Blocked || sum.Cancelled {
		// An interrupted run is not counted as covered: games after the stop were never handled.
		sum.Coverage = 0
	}
	return sum
}

// epicStoreURL returns the Epic page when the provider gave a readable slug, otherwise the search link.
func epicStoreURL(name, rawLink string) (string, bool) {
	if u, ok := EpicPageURL(rawLink); ok {
		return u, true
	}
	return EpicSearchURL(name), false
}

// chunkMatches splits matches into batches of at most MaxIDsPerRequest.
func chunkMatches(ms []match) [][]match {
	var out [][]match
	for i := 0; i < len(ms); i += MaxIDsPerRequest {
		end := i + MaxIDsPerRequest
		if end > len(ms) {
			end = len(ms)
		}
		out = append(out, ms[i:end])
	}
	return out
}

func matchIDs(ms []match) []string {
	ids := make([]string, 0, len(ms))
	for _, m := range ms {
		ids = append(ids, m.itadID)
	}
	return ids
}

func markFailed(failed map[string]bool, games []storeprices.Game) {
	for _, g := range games {
		failed[g.ID] = true
	}
}

func markMatchesFailed(failed map[string]bool, ms []match) {
	for _, m := range ms {
		failed[m.game.ID] = true
	}
}

func centsText(c *int64) string {
	if c == nil {
		return "-"
	}
	return storeprices.FormatCents(*c)
}
