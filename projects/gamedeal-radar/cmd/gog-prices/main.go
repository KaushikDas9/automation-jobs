// Command gog-prices looks every active game up in GOG's public catalog, for the US and
// India buyer countries, and stores the prices (store_code 'gog') in game_store_prices.
//
// Environment:
//
//	GDR_DATABASE_URL          Postgres connection string. Required. Never logged.
//	DRY_RUN                   "1" or "true" fetches and prints, and writes nothing.
//	LIMIT                     max games in this run (testing). Coverage is counted over them.
//	RETRY_WAIT_1, RETRY_WAIT_2  waits before retry passes, Go durations (default 1m, 2m; "0" for tests).
//	FINAL_ATTEMPT             "1" marks the last scheduled run of the day.
//	GITHUB_EVENT_NAME         set by Actions. "workflow_dispatch" skips the already-complete check.
//	GDR_HEALTHCHECK_URL       dead man's switch URL. Optional. Never logged.
//	GDR_HEALTHCHECK_FINAL_URL pinged with /fail on a failing final run. Optional.
//	SLACK_WEBHOOK_URL         Slack webhook (shared by all projects). Optional.
//	GDR_SLACK_WEBHOOK_URL     legacy fallback, read only when SLACK_WEBHOOK_URL is empty.
//	GITHUB_OUTPUT             set by Actions. Receives flags for the workflow.
//
// Exit codes: 0 done (coverage at least 90%, or already complete), 1 coverage below 90%,
// GOG blocked the run (401, 403 or 429), the run was cut short, or the database was
// unreachable. 2 configuration error.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gamedeal-radar/daily-check/internal/db"
	"gamedeal-radar/daily-check/internal/jobrun"
	"gamedeal-radar/daily-check/internal/storeprices"
)

// runBudget matches the workflow's timeout-minutes.
const runBudget = 45 * time.Minute

type config struct {
	dsn        string
	dryRun     bool
	manual     bool
	final      bool
	limit      int
	retryWait1 time.Duration
	retryWait2 time.Duration
	healthURL  string
	finalURL   string
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
	c.manual = jobrun.IsManual()
	c.final = jobrun.BoolEnv("FINAL_ATTEMPT")
	if c.limit, err = jobrun.LimitEnv(); err != nil {
		return c, err
	}
	if c.retryWait1, err = jobrun.DurationEnv("RETRY_WAIT_1", time.Minute); err != nil {
		return c, err
	}
	if c.retryWait2, err = jobrun.DurationEnv("RETRY_WAIT_2", 2*time.Minute); err != nil {
		return c, err
	}
	if c.healthURL, err = jobrun.HTTPURLEnv("GDR_HEALTHCHECK_URL"); err != nil {
		return c, err
	}
	if c.finalURL, err = jobrun.HTTPURLEnv("GDR_HEALTHCHECK_FINAL_URL"); err != nil {
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
	ctx, cancel := context.WithDeadline(ctx, started.Add(runBudget))
	defer cancel()

	sum, err := execute(ctx, cfg)
	code := 0
	detail := sum.Detail()
	switch {
	case err != nil:
		code = 1
		detail = "run aborted, nothing more written: " + err.Error()
		slog.Error("gog run aborted", "error", err)
	case sum.AlreadyComplete:
		code = 0
	case sum.Blocked, sum.Cancelled, !storeprices.Succeeded(sum.Coverage):
		code = 1
		slog.Error("gog run below the success threshold",
			"coverage_pct", sum.Coverage*100, "threshold_pct", storeprices.MinCoverage*100,
			"blocked", sum.Blocked, "cancelled", sum.Cancelled)
	}
	if err == nil {
		fmt.Println(sum.Line(storeprices.StoreGOG))
	}

	jobrun.Finish(ctx, jobrun.Report{
		Job:             "GOG prices",
		Code:            code,
		DryRun:          cfg.dryRun,
		AlreadyComplete: sum.AlreadyComplete,
		Blocked:         sum.Blocked,
		Final:           cfg.final,
		Coverage:        sum.Coverage,
		Failed:          sum.Failed,
		Total:           sum.Games,
		Detail:          detail,
		Started:         started,
	}, cfg.healthURL, cfg.finalURL, os.Getenv("GITHUB_OUTPUT"))
	return code
}

// execute opens the database and runs the GOG pass. A database error returns an error
// with no detail that could carry the DSN.
func execute(ctx context.Context, cfg config) (storeprices.Summary, error) {
	conn, err := db.Connect(ctx, cfg.dsn)
	if err != nil {
		slog.Error("database unreachable", "error", err)
		return storeprices.NewSummary(cfg.dryRun), err
	}
	defer func() { _ = conn.Close(context.Background()) }()

	api := &storeprices.GOGClient{Fetcher: storeprices.NewFetcher(time.Second)}
	sum, err := storeprices.RunGOG(ctx, db.StorePriceRepo{Conn: conn}, api, storeprices.Options{
		DryRun: cfg.dryRun,
		Manual: cfg.manual,
		Limit:  cfg.limit,
		PassOptions: storeprices.PassOptions{
			RetryWait1: cfg.retryWait1,
			RetryWait2: cfg.retryWait2,
			Deadline:   deadline(ctx),
		},
	})
	if err != nil {
		slog.Error("gog run failed", "error", db.Redact(err, cfg.dsn))
		return sum, err
	}
	return sum, nil
}

func deadline(ctx context.Context) time.Time {
	if d, ok := ctx.Deadline(); ok {
		return d
	}
	return time.Time{}
}
