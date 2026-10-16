// Command fx-rate stores the USD to INR rate in fx_rates (pair USD_INR). It is a single
// request per run. Any failure writes nothing and exits 1.
//
// Environment:
//
//	GDR_DATABASE_URL          Postgres connection string. Required. Never logged.
//	DRY_RUN                   "1" or "true" fetches the rate and prints it, and writes nothing.
//	FINAL_ATTEMPT             "1" marks the scheduled run, so a failure opens an issue.
//	GDR_HEALTHCHECK_URL       dead man's switch URL. Optional. Never logged.
//	GDR_HEALTHCHECK_FINAL_URL pinged with /fail when FINAL_ATTEMPT is set and the run fails.
//	SLACK_WEBHOOK_URL         Slack webhook (shared by all projects). Optional.
//	GDR_SLACK_WEBHOOK_URL     legacy fallback, read only when SLACK_WEBHOOK_URL is empty.
//	GITHUB_OUTPUT             set by Actions. Receives flags for the workflow.
//
// Exit codes: 0 rate stored (or dry run fetched), 1 fetch, validation or database failure,
// 2 configuration error.
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

const runBudget = 10 * time.Minute

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	os.Exit(runOnce())
}

func runOnce() int {
	started := time.Now()
	dsn, err := jobrun.RequiredEnv("GDR_DATABASE_URL")
	if err != nil {
		slog.Error("configuration error", "error", err)
		return 2
	}
	healthURL, err := jobrun.HTTPURLEnv("GDR_HEALTHCHECK_URL")
	if err != nil {
		slog.Error("configuration error", "error", err)
		return 2
	}
	finalURL, err := jobrun.HTTPURLEnv("GDR_HEALTHCHECK_FINAL_URL")
	if err != nil {
		slog.Error("configuration error", "error", err)
		return 2
	}
	dryRun := jobrun.BoolEnv("DRY_RUN")
	final := jobrun.BoolEnv("FINAL_ATTEMPT")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, runBudget)
	defer cancel()

	code, detail := run(ctx, dsn, dryRun)
	jobrun.Finish(ctx, jobrun.Report{
		Job:     "USD to INR rate",
		Code:    code,
		DryRun:  dryRun,
		Final:   final,
		Detail:  detail,
		Started: started,
	}, healthURL, finalURL, os.Getenv("GITHUB_OUTPUT"))
	return code
}

// run fetches the rate, validates it and stores it. It returns the exit code and a short
// detail line for Slack. The DSN is never printed; database errors are redacted.
func run(ctx context.Context, dsn string, dryRun bool) (int, string) {
	f := storeprices.NewFetcher(0)
	rate, date, err := storeprices.FetchUSDINR(ctx, f, "")
	if err != nil {
		slog.Error("usd to inr rate not fetched, nothing written", "error", err)
		return 1, "rate not fetched, nothing written"
	}
	slog.Info("usd to inr rate fetched", "rate", fmt.Sprintf("%.6f", rate), "rate_date", date)

	if dryRun {
		fmt.Printf("dry run: would store %s rate %.6f (%s)\n", db.USDINRPair, rate, date)
		return 0, fmt.Sprintf("dry run: USD to INR %.4f (%s)", rate, date)
	}

	conn, err := db.Connect(ctx, dsn)
	if err != nil {
		slog.Error("database unreachable", "error", err)
		return 1, "database unreachable, nothing written"
	}
	defer func() { _ = conn.Close(context.Background()) }()

	if err := db.SaveUSDINR(ctx, conn, rate, date); err != nil {
		slog.Error("storing usd to inr rate failed", "error", db.Redact(err, dsn))
		return 1, "storing the rate failed"
	}
	fmt.Printf("fx summary dry_run=false pair=%s rate=%.6f rate_date=%s\n", db.USDINRPair, rate, date)
	return 0, fmt.Sprintf("USD to INR %.4f (%s) stored", rate, date)
}
