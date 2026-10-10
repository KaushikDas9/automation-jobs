// Command daily-check runs the GameDeal Radar daily Steam India price check.
//
// Configuration (environment):
//
//	GDR_DATABASE_URL  Postgres connection string (required, never logged)
//	DRY_RUN           "1" or "true" to fetch prices without writing anything
//	LIMIT             optional max number of games to process (testing)
//
// Exit codes: 0 success, 1 run failed (DB unreachable, failure rate too high, cancelled),
// 2 configuration error.
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

	"github.com/jackc/pgx/v5"

	"gamedeal-radar/daily-check/internal/db"
	"gamedeal-radar/daily-check/internal/pricing"
	"gamedeal-radar/daily-check/internal/steam"
)

// maxFailureRate is the share of failed games above which the run exits non-zero.
const maxFailureRate = 0.20

// steamCountry is the storefront country code used by the server (India, INR).
const steamCountry = "in"

type config struct {
	dsn    string
	dryRun bool
	limit  int
}

// Summary is the per-run outcome printed at the end.
type Summary struct {
	Checked int
	Updated int
	Skipped int
	Failed  int
}

// FailureRate is failed/checked, or 0 when nothing was checked.
func (s Summary) FailureRate() float64 {
	if s.Checked == 0 {
		return 0
	}
	return float64(s.Failed) / float64(s.Checked)
}

func loadConfig() (config, error) {
	var c config
	c.dsn = strings.TrimSpace(os.Getenv("GDR_DATABASE_URL"))
	if c.dsn == "" {
		return c, errors.New("GDR_DATABASE_URL is not set")
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("DRY_RUN"))) {
	case "1", "true", "yes":
		c.dryRun = true
	}
	if raw := strings.TrimSpace(os.Getenv("LIMIT")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return c, fmt.Errorf("LIMIT must be a non-negative integer, got %q", raw)
		}
		c.limit = n
	}
	return c, nil
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	os.Exit(run())
}

func run() int {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("configuration error", "error", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := db.Connect(ctx, cfg.dsn)
	if err != nil {
		slog.Error("database unreachable", "error", err)
		return 1
	}
	defer func() { _ = conn.Close(ctx) }()

	sum, err := execute(ctx, conn, cfg)
	if err != nil {
		slog.Error("daily check aborted", "error", err)
		printSummary(sum, cfg.dryRun)
		return 1
	}
	printSummary(sum, cfg.dryRun)

	if sum.FailureRate() > maxFailureRate {
		slog.Error("failure rate above threshold", "rate_pct", int(sum.FailureRate()*100), "threshold_pct", int(maxFailureRate*100))
		return 1
	}
	return 0
}

// execute loads active Steam games and processes each one. Per-game failures are
// counted and logged, not returned, so one bad title never aborts the run.
func execute(ctx context.Context, conn *pgx.Conn, cfg config) (Summary, error) {
	var sum Summary

	steamStoreID, err := db.SteamStoreID(ctx, conn)
	if err != nil {
		return sum, err
	}
	games, err := db.ListActiveSteamGames(ctx, conn, steamStoreID, cfg.limit)
	if err != nil {
		return sum, err
	}
	slog.Info("loaded active games", "count", len(games), "dry_run", cfg.dryRun, "limit", cfg.limit)

	client := steam.NewClient(steam.DefaultConfig())
	store := db.PgStore{Conn: conn}

	for _, g := range games {
		if err := ctx.Err(); err != nil {
			return sum, fmt.Errorf("run cancelled: %w", err)
		}
		sum.Checked++

		if g.ExternalAppID == "" {
			sum.Failed++
			slog.Warn("no steam app mapping for game", "slug", g.Slug)
			continue
		}

		res, err := client.FetchPrice(ctx, g.ExternalAppID, steamCountry)
		if err != nil {
			if errors.Is(err, steam.ErrNotFound) || errors.Is(err, steam.ErrPriceUnavailable) {
				sum.Skipped++
				slog.Info("skipped: no price on steam for this country", "slug", g.Slug, "app_id", g.ExternalAppID, "reason", err.Error())
				continue
			}
			if ctx.Err() != nil {
				return sum, fmt.Errorf("run cancelled: %w", ctx.Err())
			}
			sum.Failed++
			slog.Warn("price fetch failed", "slug", g.Slug, "app_id", g.ExternalAppID, "error", err)
			continue
		}

		if cfg.dryRun {
			sum.Updated++
			slog.Info("dry run: would write snapshot", "slug", g.Slug, "app_id", g.ExternalAppID,
				"current_price", res.CurrentPrice, "discount_pct", res.DiscountPct, "is_deal", res.IsDeal)
			continue
		}

		if _, _, err := pricing.ProcessPriceResult(ctx, store, g.ID, steamStoreID, res); err != nil {
			if ctx.Err() != nil {
				return sum, fmt.Errorf("run cancelled: %w", ctx.Err())
			}
			sum.Failed++
			slog.Warn("processing price result failed", "slug", g.Slug, "error", err)
			continue
		}
		sum.Updated++
	}
	return sum, nil
}

func printSummary(s Summary, dryRun bool) {
	fmt.Printf("summary dry_run=%t checked=%d updated=%d skipped=%d failed=%d failure_rate=%.1f%%\n",
		dryRun, s.Checked, s.Updated, s.Skipped, s.Failed, s.FailureRate()*100)
}
