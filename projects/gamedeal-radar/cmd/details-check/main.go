// Command details-check refreshes the Steam store details (description, genres,
// screenshots, requirements and so on) of games whose details are missing or stale.
//
// Each game is saved on its own, so a run that stops early keeps its progress and the
// next run resumes with the games still stale. Prices are not touched here: the details
// response carries a region-dependent price that is deliberately ignored.
//
// Environment: GDR_DATABASE_URL (required), DRY_RUN=1 to fetch without writing,
// LIMIT=n to cap games per run, STALE_DAYS (default 6) for the refresh age,
// FORCE=1 to refresh every candidate game and ignore the staleness cutoff.
// Boolean flags accept 1, true or yes.
//
// Exit codes: 0 done (or at most 10% of attempted games failed), 1 too many failures
// or the database was unreachable, 2 configuration error.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gamedeal-radar/daily-check/internal/db"
	"gamedeal-radar/daily-check/internal/steam"
)

const (
	// runBudget matches the workflow's timeout-minutes.
	runBudget = 50 * time.Minute

	// maxFailureShare is the share of attempted games that may fail before the run fails.
	maxFailureShare = 0.10

	// requestsPerSecond keeps the details run gentler than the daily price check.
	requestsPerSecond = 0.6
)

type config struct {
	dsn       string
	dryRun    bool
	force     bool
	limit     int
	staleDays int
}

type result struct {
	candidates, saved, notFound, failed int
}

func main() { os.Exit(runOnce()) }

func loadConfig() (config, error) {
	c := config{staleDays: 6}
	c.dsn = strings.TrimSpace(os.Getenv("GDR_DATABASE_URL"))
	if c.dsn == "" {
		return c, errors.New("GDR_DATABASE_URL is not set")
	}
	c.dryRun = boolEnv("DRY_RUN")
	c.force = boolEnv("FORCE")
	if raw := strings.TrimSpace(os.Getenv("LIMIT")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return c, fmt.Errorf("LIMIT must be a non-negative integer, got %q", raw)
		}
		c.limit = n
	}
	if raw := strings.TrimSpace(os.Getenv("STALE_DAYS")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 90 {
			return c, fmt.Errorf("STALE_DAYS must be between 1 and 90, got %q", raw)
		}
		c.staleDays = n
	}
	return c, nil
}

// boolEnv reports whether the environment variable is set to 1, true or yes.
func boolEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func runOnce() int {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("configuration error", "error", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, runBudget)
	defer cancel()

	conn, err := db.Connect(ctx, cfg.dsn)
	if err != nil {
		slog.Error("database unreachable", "error", err)
		return 1
	}
	defer func() { _ = conn.Close(context.Background()) }()

	steamID, err := db.SteamStoreID(ctx, conn)
	if err != nil {
		slog.Error("database unreachable", "error", err)
		return 1
	}
	repo := db.NewRepo(conn, steamID)

	scfg := steam.DefaultConfig()
	scfg.RateLimitRPS = requestsPerSecond
	client := steam.NewClient(scfg)

	cutoff := time.Now().UTC().AddDate(0, 0, -cfg.staleDays)
	if cfg.force {
		// Every fetched game is older than an hour from now, so all candidates are listed.
		cutoff = time.Now().UTC().Add(time.Hour)
	}
	limit := cfg.limit
	if limit == 0 {
		limit = 100000
	}
	games, err := repo.ListGamesNeedingDetails(ctx, cutoff, limit)
	if err != nil {
		slog.Error("listing games needing details failed", "error", err)
		return 1
	}

	res := result{candidates: len(games)}
	slog.Info("starting details refresh", "games", res.candidates, "stale_days", cfg.staleDays, "dry_run", cfg.dryRun, "force", cfg.force)

	for _, g := range games {
		if ctx.Err() != nil {
			slog.Warn("stopping early, the next run will resume", "reason", ctx.Err())
			break
		}
		refresh(ctx, repo, client, g, cfg.dryRun, &res)
	}

	fmt.Printf("details summary dry_run=%t candidates=%d saved=%d not_found=%d failed=%d\n",
		cfg.dryRun, res.candidates, res.saved, res.notFound, res.failed)

	attempted := res.saved + res.notFound + res.failed
	if attempted > 0 && float64(res.failed)/float64(attempted) > maxFailureShare {
		slog.Error("too many games failed", "failed", res.failed, "attempted", attempted)
		return 1
	}
	return 0
}

// refresh fetches and stores the details of one game. Reviews are optional: when they
// cannot be fetched the details are still saved without a review summary.
func refresh(ctx context.Context, repo *db.Repo, client *steam.Client, g db.GameRef, dryRun bool, res *result) {
	d, err := client.FetchDetails(ctx, g.ExternalAppID)
	if err != nil {
		if errors.Is(err, steam.ErrNotFound) {
			res.notFound++
			slog.Info("no store entry on steam", "app_id", g.ExternalAppID)
			return
		}
		res.failed++
		slog.Warn("details fetch failed", "app_id", g.ExternalAppID, "error", err)
		return
	}

	rev, err := client.FetchReviews(ctx, g.ExternalAppID)
	if err != nil {
		slog.Warn("reviews fetch failed, saving details without them", "app_id", g.ExternalAppID, "error", err)
		rev = nil
	}

	if dryRun {
		res.saved++
		slog.Info("dry run: would save details", "app_id", g.ExternalAppID, "genres", len(d.Genres), "screenshots", len(d.Screenshots))
		return
	}
	if err := repo.UpsertGameDetails(ctx, g.GameID, d, rev); err != nil {
		res.failed++
		slog.Warn("saving details failed", "app_id", g.ExternalAppID, "error", err)
		return
	}
	res.saved++
}
