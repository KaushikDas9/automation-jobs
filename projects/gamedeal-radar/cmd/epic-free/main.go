// Command epic-free stores Epic's current and upcoming free games in epic_free_games. It
// is a single request per run to Epic's public free games list. Rows are upserted by
// epic_id and never deleted. Any failure writes nothing and exits 1.
//
// Environment:
//
//	GDR_DATABASE_URL  Postgres connection string. Required. Never logged.
//	DRY_RUN           "1" or "true" fetches the list and prints it, and writes nothing.
//	FINAL_ATTEMPT     "1" marks the scheduled run, so a failure opens an issue.
//	SLACK_WEBHOOK_URL Slack webhook (shared by all projects). Optional.
//	GDR_SLACK_WEBHOOK_URL legacy fallback, read only when SLACK_WEBHOOK_URL is empty.
//	GITHUB_OUTPUT     set by Actions. Receives flags for the workflow.
//
// No healthcheck is used by this job.
//
// Exit codes: 0 games stored (or dry run fetched), 1 the feed could not be read, the feed
// had no free games, or the database write failed, 2 configuration error.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"gamedeal-radar/daily-check/internal/db"
	"gamedeal-radar/daily-check/internal/epicfree"
	"gamedeal-radar/daily-check/internal/jobrun"
)

// runBudget is far inside the workflow's timeout-minutes. One request and one transaction.
const runBudget = 5 * time.Minute

type config struct {
	dsn    string
	dryRun bool
	final  bool
}

// freeSource is the Epic side. epicfree.Client implements it.
type freeSource interface {
	FetchGames(ctx context.Context) ([]epicfree.Game, error)
}

// saveFunc writes the games in one transaction. It is never called on a dry run.
type saveFunc func(ctx context.Context, games []epicfree.Game) error

// summary is the outcome of one run.
type summary struct {
	DryRun   bool
	Found    int    // games parsed from the feed
	FreeNow  int    // of those, free now
	Upcoming int    // of those, free next
	Stored   bool   // rows written in this run
	Reason   string // short cause for Slack when Err is set, with no secrets
	Err      error  // the cause, for the log only
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

	sum := execute(ctx, cfg, epicfree.NewClient())
	code := decide(sum)
	if sum.Err != nil {
		slog.Error("epic free games run stopped", "error", sum.Err)
	} else {
		fmt.Println(sum.line())
		slog.Info("epic free games run finished", "found", sum.Found, "free_now", sum.FreeNow,
			"upcoming", sum.Upcoming, "stored", sum.Stored)
	}

	jobrun.Finish(ctx, jobrun.Report{
		Job:     "Epic free games",
		Code:    code,
		DryRun:  cfg.dryRun,
		Final:   cfg.final,
		Total:   sum.Found,
		Detail:  sum.detail(),
		Started: started,
	}, "", "", os.Getenv("GITHUB_OUTPUT"))
	return code
}

// execute runs the job against the real feed and database. The database is opened only
// after the feed has been read and checked, and never on a dry run. Database errors are
// redacted of the DSN before they leave this function.
func execute(ctx context.Context, cfg config, src freeSource) summary {
	save := func(ctx context.Context, games []epicfree.Game) error {
		conn, err := db.Connect(ctx, cfg.dsn)
		if err != nil {
			return errors.New(db.Redact(err, cfg.dsn))
		}
		defer func() { _ = conn.Close(context.Background()) }()
		if err := db.SaveEpicFreeGames(ctx, conn, games); err != nil {
			return errors.New(db.Redact(err, cfg.dsn))
		}
		return nil
	}
	return runJob(ctx, src, save, cfg)
}

// runJob reads the feed, checks it and stores it. A feed with no free games is a failure,
// so a changed feed never leads to a write. It is separate from execute so tests can use
// fakes.
func runJob(ctx context.Context, src freeSource, save saveFunc, cfg config) summary {
	sum := summary{DryRun: cfg.dryRun}

	games, err := src.FetchGames(ctx)
	if err != nil {
		sum.fail("feed not read, nothing written", err)
		return sum
	}
	sum.Found = len(games)
	if len(games) == 0 {
		sum.fail("feed had no free games, nothing written", errors.New("no free offers in a 200 response"))
		return sum
	}
	for _, g := range games {
		if g.Upcoming {
			sum.Upcoming++
		} else {
			sum.FreeNow++
		}
	}

	if cfg.dryRun {
		return sum
	}
	if err := save(ctx, games); err != nil {
		sum.fail("storing the games failed, nothing written", err)
		return sum
	}
	sum.Stored = true
	return sum
}

func (s *summary) fail(reason string, err error) {
	s.Reason = reason
	s.Err = err
}

// decide maps a summary to an exit code: 0 when nothing failed, 1 otherwise.
func decide(s summary) int {
	if s.Err != nil {
		return 1
	}
	return 0
}

func (s summary) line() string {
	return fmt.Sprintf("epic free games summary dry_run=%t found=%d free_now=%d upcoming=%d stored=%t",
		s.DryRun, s.Found, s.FreeNow, s.Upcoming, s.Stored)
}

// detail is the one short line for Slack. It has no secrets and no error text.
func (s summary) detail() string {
	if s.Err != nil {
		return s.Reason
	}
	var b strings.Builder
	if s.DryRun {
		b.WriteString("dry run: ")
	}
	fmt.Fprintf(&b, "%d free now, %d upcoming", s.FreeNow, s.Upcoming)
	if s.Stored {
		b.WriteString(" stored")
	}
	return b.String()
}
