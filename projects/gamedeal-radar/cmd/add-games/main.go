// Command add-games processes the queue of admin-requested games (table game_requests).
// For each claimed request it runs the same steps as the full automation, for that one
// game: Steam details and reviews, the Steam INR price, the USD price, Steam tags, and the
// GOG and Epic prices. When the two core steps (details, INR price) succeed it sets
// games.is_active = true and marks the request done. See projects/gamedeal-radar/README.md.
//
// Environment:
//
//	GDR_DATABASE_URL          Postgres connection string. Required. Never logged.
//	MAX_GAMES                 max requests claimed per run (default 10, 1 to 50).
//	GAME_REQUEST_IDS          optional comma list of request ids, to process only those.
//	DRY_RUN                   "1" or "true" fetches and prints, claims nothing, writes nothing.
//	ITAD_API_KEY              optional. Without it the Epic step is skipped, not failed.
//	GDR_HEALTHCHECK_URL       optional dead man's switch URL. Not set by the workflow.
//	SLACK_WEBHOOK_URL         Slack webhook (shared by all projects). Optional.
//	GDR_SLACK_WEBHOOK_URL     legacy fallback, read only when SLACK_WEBHOOK_URL is empty.
//	GITHUB_OUTPUT             set by Actions. Receives flags for the workflow.
//
// Exit codes: 0 queue empty, or at least one request ended done or not_found; 1 the
// database was unreachable or every claimed request failed; 2 configuration error.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gamedeal-radar/daily-check/internal/addgames"
	"gamedeal-radar/daily-check/internal/db"
	"gamedeal-radar/daily-check/internal/itad"
	"gamedeal-radar/daily-check/internal/jobrun"
	"gamedeal-radar/daily-check/internal/steam"
	"gamedeal-radar/daily-check/internal/steamtags"
	"gamedeal-radar/daily-check/internal/storeprices"
)

// runBudget is below the workflow's timeout-minutes.
const runBudget = 40 * time.Minute

const (
	defaultMaxGames = 10
	maxMaxGames     = 50
)

type config struct {
	dsn       string
	apiKey    string
	dryRun    bool
	maxGames  int
	requestID []string
	healthURL string
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	os.Exit(runOnce())
}

func loadConfig() (config, error) {
	c := config{maxGames: defaultMaxGames}
	var err error
	if c.dsn, err = jobrun.RequiredEnv("GDR_DATABASE_URL"); err != nil {
		return c, err
	}
	c.apiKey = strings.TrimSpace(os.Getenv("ITAD_API_KEY"))
	c.dryRun = jobrun.BoolEnv("DRY_RUN")
	if raw := strings.TrimSpace(os.Getenv("MAX_GAMES")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > maxMaxGames {
			return c, fmt.Errorf("MAX_GAMES must be a number from 1 to %d", maxMaxGames)
		}
		c.maxGames = n
	}
	if c.requestID, err = parseIDs(os.Getenv("GAME_REQUEST_IDS")); err != nil {
		return c, err
	}
	if c.healthURL, err = jobrun.HTTPURLEnv("GDR_HEALTHCHECK_URL"); err != nil {
		return c, err
	}
	return c, nil
}

// parseIDs reads a comma list of request ids. Each must be a UUID.
func parseIDs(raw string) ([]string, error) {
	var ids []string
	for _, part := range strings.Split(raw, ",") {
		id := strings.TrimSpace(part)
		if id == "" {
			continue
		}
		if !uuidPattern.MatchString(id) {
			return nil, errors.New("GAME_REQUEST_IDS must be a comma list of request ids (UUIDs)")
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func runOnce() int {
	started := time.Now()
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("configuration error", "error", err)
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, runBudget)
	defer cancel()

	sum, err := execute(ctx, cfg)
	code, detail := decide(sum, err)
	if err != nil {
		slog.Error("add-games run failed", "error", err)
	} else if sum.Claimed == 0 {
		fmt.Println("no requested games")
	} else {
		fmt.Println(summaryLine(sum))
	}

	cov := 1.0
	if sum.Claimed > 0 {
		cov = float64(sum.Handled()) / float64(sum.Claimed)
	}
	jobrun.Finish(ctx, jobrun.Report{
		Job:             "Add requested games",
		Code:            code,
		DryRun:          cfg.dryRun,
		AlreadyComplete: err == nil && sum.Claimed == 0, // nothing to do: no Slack message
		Coverage:        cov,
		Failed:          sum.Failed,
		Total:           sum.Claimed,
		Detail:          detail,
		Started:         started,
	}, cfg.healthURL, "", os.Getenv("GITHUB_OUTPUT"))
	return code
}

// decide maps a run to an exit code and the short Slack line.
func decide(sum addgames.Summary, err error) (int, string) {
	switch {
	case err != nil:
		return 1, "run aborted, the log has the reason"
	case sum.AllFailed():
		return 1, "every requested game failed: " + counts(sum)
	}
	return 0, counts(sum)
}

func counts(s addgames.Summary) string {
	prefix := ""
	if s.DryRun {
		prefix = "dry run, "
	}
	return fmt.Sprintf("%s%d requested: %d done, %d not found, %d failed", prefix, s.Claimed, s.Done, s.NotFound, s.Failed)
}

func summaryLine(s addgames.Summary) string {
	return fmt.Sprintf("add-games summary dry_run=%t claimed=%d done=%d not_found=%d failed=%d",
		s.DryRun, s.Claimed, s.Done, s.NotFound, s.Failed)
}

// execute opens the database, wires the real steps and runs the queue. Errors are scrubbed
// of the DSN and the API key.
func execute(ctx context.Context, cfg config) (addgames.Summary, error) {
	conn, err := db.Connect(ctx, cfg.dsn)
	if err != nil {
		return addgames.Summary{DryRun: cfg.dryRun}, errors.New("database unreachable")
	}
	defer func() { _ = conn.Close(context.Background()) }()

	steamID, err := db.SteamStoreID(ctx, conn)
	if err != nil {
		return addgames.Summary{DryRun: cfg.dryRun}, errors.New("database unreachable")
	}
	repo := db.NewRepo(conn, steamID)

	steps := &addgames.PgSteps{
		Repo:   repo,
		Store:  db.StorePriceRepo{Conn: conn},
		Steam:  steam.NewClient(steam.DefaultConfig()), // 1 request/second, shared by all Steam calls
		TagSrc: steamtags.NewClient(),
		GOGAPI: &storeprices.GOGClient{Fetcher: storeprices.NewFetcher(time.Second)},
		DryRun: cfg.dryRun,
	}
	if cfg.apiKey != "" {
		steps.EpicAPI = itad.NewClient(cfg.apiKey)
	}

	sum, err := addgames.Run(ctx, addgames.PgQueue{Conn: conn}, steps, addgames.Options{
		Max: cfg.maxGames, IDs: cfg.requestID, DryRun: cfg.dryRun,
	})
	if err != nil {
		msg := db.Redact(err, cfg.dsn)
		if cfg.apiKey != "" {
			msg = strings.ReplaceAll(msg, cfg.apiKey, "[redacted]")
		}
		return sum, errors.New(msg)
	}
	return sum, nil
}
