// Command steam-tags stores each game's top Steam user tags in game_details.tags. It runs
// weekly. The tag names come from Steam's public store API, one request per run, and the
// games are fetched in batches of 50 app ids. Only the tags column is written.
//
// Environment:
//
//	GDR_DATABASE_URL  Postgres connection string. Required. Never logged.
//	DRY_RUN           "1" or "true" fetches the tags and prints them, and writes nothing.
//	LIMIT             max games in this run (testing). Coverage is counted over them.
//	FINAL_ATTEMPT     "1" marks the scheduled run, so a failure opens an issue.
//	SLACK_WEBHOOK_URL Slack webhook (shared by all projects). Optional.
//	GDR_SLACK_WEBHOOK_URL legacy fallback, read only when SLACK_WEBHOOK_URL is empty.
//	GITHUB_OUTPUT     set by Actions. Receives flags for the workflow.
//
// No healthcheck is used by this job.
//
// Exit codes: 0 tags stored (or dry run fetched) with at least 90% of games handled,
// 1 the tag list or the games could not be read, Steam refused the run, the run was cut
// short, a write failed, or coverage was below 90%. 2 configuration error.
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
	"gamedeal-radar/daily-check/internal/jobrun"
	"gamedeal-radar/daily-check/internal/steamtags"
)

// runBudget leaves room inside the workflow's timeout-minutes.
const runBudget = 40 * time.Minute

// minCoveragePct is the share of games (in percent) that must be handled, as in the store jobs.
const minCoveragePct = 90

type config struct {
	dsn    string
	dryRun bool
	final  bool
	limit  int
}

// tagAPI is the Steam side: the tag names and the store items.
type tagAPI interface {
	TagNames(ctx context.Context) (map[int]string, error)
	GetItems(ctx context.Context, appIDs []int) ([]steamtags.StoreItem, error)
}

// tagStore is the database side. db.Repo implements it.
type tagStore interface {
	ListGamesForTags(ctx context.Context, limit int) ([]db.GameRef, error)
	SaveTags(ctx context.Context, gameID string, tags []steamtags.Tag) error
}

// summary is the outcome of one run. Games is the number of games listed.
type summary struct {
	DryRun   bool
	Games    int
	Tagged   int // games whose tags were written (or would be, in a dry run)
	NoTags   int // handled games with no usable tags, left as they are
	Failed   int // games whose batch failed after retries
	Blocked  bool
	Coverage float64
	Err      error
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	os.Exit(runOnce())
}

func loadConfig() (config, error) {
	var c config
	var err error
	if c.dsn, err = jobrun.RequiredEnv("GDR_DATABASE_URL"); err != nil {
		return c, err
	}
	c.dryRun = jobrun.BoolEnv("DRY_RUN")
	c.final = jobrun.BoolEnv("FINAL_ATTEMPT")
	if c.limit, err = jobrun.LimitEnv(); err != nil {
		return c, err
	}
	return c, nil
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

	sum := execute(ctx, cfg)
	code := decide(sum)
	detail := sum.detail()
	if sum.Err != nil {
		slog.Error("steam tags run stopped", "error", sum.Err)
		detail = "run stopped, nothing more written. The log has the reason."
	} else {
		fmt.Println(sum.line())
		slog.Info("steam tags run finished", "games", sum.Games, "tagged", sum.Tagged,
			"no_tags", sum.NoTags, "failed", sum.Failed, "coverage_pct", sum.Coverage*100)
	}

	jobrun.Finish(ctx, jobrun.Report{
		Job:      "Steam tags",
		Code:     code,
		DryRun:   cfg.dryRun,
		Final:    cfg.final,
		Blocked:  sum.Blocked,
		Coverage: sum.Coverage,
		Failed:   sum.Failed,
		Total:    sum.Games,
		Detail:   detail,
		Started:  started,
	}, "", "", os.Getenv("GITHUB_OUTPUT"))
	return code
}

// execute opens the database and runs the job. A database error is returned with no
// detail that could carry the DSN.
func execute(ctx context.Context, cfg config) summary {
	conn, err := db.Connect(ctx, cfg.dsn)
	if err != nil {
		slog.Error("database unreachable", "error", err)
		return summary{DryRun: cfg.dryRun, Err: errors.New("database unreachable")}
	}
	defer func() { _ = conn.Close(context.Background()) }()

	steamID, err := db.SteamStoreID(ctx, conn)
	if err != nil {
		slog.Error("database unreachable", "error", err)
		return summary{DryRun: cfg.dryRun, Err: errors.New("database unreachable")}
	}
	repo := db.NewRepo(conn, steamID)

	sum := runJob(ctx, steamtags.NewClient(), repo, cfg)
	if sum.Err != nil {
		sum.Err = errors.New(db.Redact(sum.Err, cfg.dsn))
	}
	return sum
}

// runJob loads the tag names and the games, fetches the tags in batches and writes each
// game that has tags. A game is never written with an empty list. Each write is
// independent, so a run that stops early keeps the games it already wrote.
func runJob(ctx context.Context, api tagAPI, store tagStore, cfg config) summary {
	sum := summary{DryRun: cfg.dryRun}

	names, err := api.TagNames(ctx)
	if err != nil {
		sum.Err = fmt.Errorf("reading steam tag names: %w", err)
		sum.Blocked = errors.Is(err, steamtags.ErrBlocked)
		return sum
	}

	games, err := store.ListGamesForTags(ctx, cfg.limit)
	if err != nil {
		sum.Err = fmt.Errorf("listing games for tags: %w", err)
		return sum
	}
	sum.Games = len(games)

	// Games with an app id that is not a number count as failed, though the query only
	// returns digit-only ids.
	byApp := make(map[int][]db.GameRef, len(games))
	var appIDs []int
	var invalid int
	for _, g := range games {
		id, err := strconv.Atoi(g.ExternalAppID)
		if err != nil || id <= 0 {
			invalid++
			continue
		}
		if _, seen := byApp[id]; !seen {
			appIDs = append(appIDs, id)
		}
		byApp[id] = append(byApp[id], g)
	}

	res, err := steamtags.Collect(ctx, api, names, appIDs)
	if err != nil {
		sum.Err = fmt.Errorf("fetching steam tags: %w", err)
		sum.Blocked = errors.Is(err, steamtags.ErrBlocked)
		return sum
	}

	failed := make(map[int]bool, len(res.Failed))
	for _, id := range res.Failed {
		failed[id] = true
	}
	sum.Failed = invalid
	for _, id := range appIDs {
		if failed[id] {
			sum.Failed += len(byApp[id])
		}
	}

	for _, id := range appIDs {
		if failed[id] {
			continue
		}
		tags := res.Tags[id]
		for _, g := range byApp[id] {
			if len(tags) == 0 {
				sum.NoTags++
				continue
			}
			if !cfg.dryRun {
				if err := store.SaveTags(ctx, g.GameID, tags); err != nil {
					sum.Err = fmt.Errorf("saving tags: %w", err)
					return sum
				}
			}
			sum.Tagged++
		}
	}

	sum.Coverage = coverage(sum)
	return sum
}

// coverage is the share of listed games that were handled. A game is handled when its
// tags were written or it had nothing to write. A failed game is not handled.
func coverage(s summary) float64 {
	if s.Games == 0 {
		return 1
	}
	handled := s.Games - s.Failed
	return float64(handled) / float64(s.Games)
}

// decide maps a summary to an exit code. An error or a Steam refusal is always exit 1,
// and coverage must reach minCoveragePct.
func decide(s summary) int {
	if s.Err != nil || s.Blocked {
		return 1
	}
	if s.Games == 0 {
		return 0
	}
	handled := s.Games - s.Failed
	if handled*100 < s.Games*minCoveragePct {
		return 1
	}
	return 0
}

func (s summary) line() string {
	return fmt.Sprintf("tags summary dry_run=%t games=%d tagged=%d no_tags=%d failed=%d coverage_pct=%.1f",
		s.DryRun, s.Games, s.Tagged, s.NoTags, s.Failed, s.Coverage*100)
}

// detail is the one short line for Slack. It has no secrets.
func (s summary) detail() string {
	var b strings.Builder
	if s.DryRun {
		b.WriteString("dry run: ")
	}
	fmt.Fprintf(&b, "%d of %d games tagged, %d without tags, %d failed (%.1f%% covered)",
		s.Tagged, s.Games, s.NoTags, s.Failed, s.Coverage*100)
	return b.String()
}
