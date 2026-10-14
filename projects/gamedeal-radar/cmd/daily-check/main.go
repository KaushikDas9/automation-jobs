// Command daily-check runs one attempt of the GameDeal Radar Steam India price check.
//
// Environment (all optional except GDR_DATABASE_URL):
//
//	GDR_DATABASE_URL          Postgres connection string. Required. Never logged.
//	DRY_RUN                   "1" or "true" fetches prices and writes nothing.
//	LIMIT                     max games to fetch in this run (testing).
//	SLOT_START                RFC3339 slot start override (testing). Disables the window check.
//	ATTEMPT                   1..6 attempt override (testing and manual runs). Disables the window check.
//	RETRY_WAIT_1, RETRY_WAIT_2  waits before retry passes, Go durations (default 5m, 10m; "0" for tests).
//	GDR_HEALTHCHECK_URL       dead man's switch URL. Optional. Never logged.
//	GDR_HEALTHCHECK_FINAL_URL pinged with /fail on the final attempt's failure. Optional.
//	SLACK_WEBHOOK_URL         Slack Incoming Webhook URL, shared by all projects. Optional. Never logged.
//	GDR_SLACK_WEBHOOK_URL     legacy fallback, read only when SLACK_WEBHOOK_URL is empty. Optional.
//	GDR_NOTIFY_TEMPLATE_DIR   folder with success.tmpl, attempt_failed.tmpl, final_failed.tmpl
//	                          to override the Slack message text. Optional.
//	GITHUB_EVENT_NAME         set by Actions. "workflow_dispatch" marks a manual run.
//	GITHUB_OUTPUT             set by Actions. Receives flags for the notification steps.
//
// Exit codes: 0 slot complete (coverage >= 90%), slot already complete, or outside the
// slot window. 1 coverage below 90% after retries, or the database was unreachable.
// 2 configuration error.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gamedeal-radar/daily-check/internal/db"
	"gamedeal-radar/daily-check/internal/health"
	"gamedeal-radar/daily-check/internal/notify"
	"gamedeal-radar/daily-check/internal/run"
	"gamedeal-radar/daily-check/internal/slot"
	"gamedeal-radar/daily-check/internal/steam"
	"gamedeal-radar/daily-check/internal/usd"
)

// runBudget matches the workflow's timeout-minutes. Retry waits stop early enough to leave
// run.MinRemaining before this deadline.
const runBudget = 45 * time.Minute

type config struct {
	dsn            string
	dryRun         bool
	manual         bool
	limit          int
	attempt        int
	slotOverride   *time.Time
	retryWait1     time.Duration
	retryWait2     time.Duration
	healthURL      string
	finalHealthURL string
	outputPath     string
}

// status is what the run reports back to the workflow through GITHUB_OUTPUT.
type status struct {
	slotID          string
	attempt         int
	dryRun          bool
	complete        bool
	final           bool
	alreadyComplete bool
	coverage        float64
	failed          int
	total           int
	updated         int
	skipped         int
	retried         int
}

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))
	os.Exit(runOnce())
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
	c.manual = os.Getenv("GITHUB_EVENT_NAME") == "workflow_dispatch"
	c.outputPath = os.Getenv("GITHUB_OUTPUT")

	if raw := strings.TrimSpace(os.Getenv("LIMIT")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return c, fmt.Errorf("LIMIT must be a non-negative integer, got %q", raw)
		}
		c.limit = n
	}
	if raw := strings.TrimSpace(os.Getenv("SLOT_START")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return c, fmt.Errorf("SLOT_START must be RFC3339, got %q", raw)
		}
		t = t.UTC()
		c.slotOverride = &t
	}
	if raw := strings.TrimSpace(os.Getenv("ATTEMPT")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > slot.MaxAttempts {
			return c, fmt.Errorf("ATTEMPT must be between 1 and %d, got %q", slot.MaxAttempts, raw)
		}
		c.attempt = n
	}

	var err error
	if c.retryWait1, err = durationEnv("RETRY_WAIT_1", 5*time.Minute); err != nil {
		return c, err
	}
	if c.retryWait2, err = durationEnv("RETRY_WAIT_2", 10*time.Minute); err != nil {
		return c, err
	}
	if c.healthURL, err = httpURLEnv("GDR_HEALTHCHECK_URL"); err != nil {
		return c, err
	}
	if c.finalHealthURL, err = httpURLEnv("GDR_HEALTHCHECK_FINAL_URL"); err != nil {
		return c, err
	}
	return c, nil
}

func durationEnv(name string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 || d > time.Hour {
		return 0, fmt.Errorf("%s must be a duration from 0 to 1h, such as 5m or 0", name)
	}
	return d, nil
}

// httpURLEnv validates an optional URL. Error text names the variable, never its value.
func httpURLEnv(name string) (string, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return "", nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("%s is set but is not a valid http(s) URL", name)
	}
	return raw, nil
}

func runOnce() int {
	cfg, err := loadConfig()
	if err != nil {
		slog.Error("configuration error", "error", err)
		return 2
	}

	start := time.Now()
	plan := slot.PlanFor(start, cfg.slotOverride, cfg.attempt, cfg.manual)
	if plan.Skip {
		slog.Info("outside slot window, skipping",
			"slot", slot.ID(plan.Slot),
			"elapsed_minutes", int(start.UTC().Sub(plan.Slot).Minutes()),
			"window_hours", int(slot.Window.Hours()))
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	deadline := start.Add(runBudget)
	runCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	st := &status{slotID: slot.ID(plan.Slot), attempt: plan.Attempt, dryRun: cfg.dryRun}
	slog.Info("starting daily check",
		"slot", st.slotID, "attempt", plan.Attempt, "final_attempt", plan.Final(),
		"dry_run", cfg.dryRun, "manual", cfg.manual)

	code := execute(runCtx, cfg, plan, deadline, st)

	if !cfg.dryRun {
		primary := health.New(cfg.healthURL)
		if st.complete {
			if err := primary.Success(ctx); err != nil {
				slog.Warn("healthcheck success ping failed", "error", err)
			}
		} else {
			if err := primary.Fail(ctx); err != nil {
				slog.Warn("healthcheck fail ping failed", "error", err)
			}
			if plan.Final() {
				st.final = true
				final := health.New(cfg.finalHealthURL)
				if err := final.Fail(ctx); err != nil {
					slog.Warn("final healthcheck fail ping failed", "error", err)
				}
			}
		}
	}

	sendSlack(ctx, notify.New(os.Getenv), st, plan, time.Since(start))

	if err := writeOutputs(cfg.outputPath, st); err != nil {
		slog.Warn("could not write workflow outputs", "error", err)
	}
	return code
}

// notifyKind decides which Slack message, if any, a finished run sends. Dry runs and
// backup runs that found the slot already complete stay silent.
func notifyKind(st *status, final bool) (string, bool) {
	if st.dryRun {
		return "", false
	}
	if st.complete {
		if st.alreadyComplete || st.updated+st.skipped == 0 {
			return "", false
		}
		return notify.Success, true
	}
	if final {
		return notify.FinalFailed, true
	}
	return notify.AttemptFailed, true
}

// sendSlack posts the run summary to Slack. It is best effort: a failure is logged
// (without the URL) and never changes the exit code.
func sendSlack(ctx context.Context, n notify.Notifier, st *status, plan slot.Plan, took time.Duration) {
	if !n.Enabled() {
		return
	}
	kind, ok := notifyKind(st, plan.Final())
	if !ok {
		return
	}
	n.Logf = func(msg string, err error) { slog.Warn(msg, "error", err) }
	err := n.Send(ctx, kind, notify.Data{
		SlotLabel:   notify.SlotLabel(plan.Slot),
		Attempt:     st.attempt,
		Total:       st.total,
		Updated:     st.updated,
		Skipped:     st.skipped,
		Failed:      st.failed,
		Retried:     st.retried,
		CoveragePct: notify.FormatCoverage(st.coverage),
		Duration:    took.Round(time.Second).String(),
		RunURL:      notify.RunURL(os.Getenv),
	})
	if err != nil {
		slog.Warn("slack notification failed", "kind", kind, "error", err)
	}
}

// execute runs the check for the slot and fills st. It returns the exit code.
func execute(ctx context.Context, cfg config, plan slot.Plan, deadline time.Time, st *status) int {
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
	client := steam.NewClient(steam.DefaultConfig())

	res, err := run.Execute(ctx, repo, client, run.Options{
		SteamStoreID: steamID,
		SlotStart:    plan.Slot,
		DryRun:       cfg.dryRun,
		Limit:        cfg.limit,
		RetryWait1:   cfg.retryWait1,
		RetryWait2:   cfg.retryWait2,
		Deadline:     deadline,
	})
	if err != nil {
		slog.Error("daily check aborted", "error", err)
		return 1
	}

	st.total = res.Total
	st.failed = res.Failed
	st.coverage = res.CoverageAfter
	st.alreadyComplete = res.AlreadyComplete
	st.updated = res.Updated
	st.skipped = res.Skipped
	st.retried = res.Retried
	printSummary(res, cfg.dryRun)

	// The USD pass is best effort: its outcome never changes the INR result or the exit code.
	usdRes, usdErr := usd.Execute(ctx, repo, client, usd.Options{
		SlotStart: plan.Slot,
		DryRun:    cfg.dryRun,
		Limit:     cfg.limit,
	})
	if usdErr != nil {
		slog.Warn("usd pass aborted", "error", usdErr)
	}
	fmt.Printf("usd summary dry_run=%t missing=%d saved=%d unavailable=%d failed=%d\n",
		cfg.dryRun, usdRes.Missing, usdRes.Saved, usdRes.Unavailable, usdRes.Failed)

	if !run.Succeeded(res.CoverageAfter) {
		slog.Error("coverage below threshold for this attempt",
			"coverage_pct", res.CoverageAfter*100, "threshold_pct", run.MinCoverage*100)
		return 1
	}
	st.complete = true
	return 0
}

// writeOutputs appends flags to the GITHUB_OUTPUT file. A missing path is a no-op
// (local runs). Values never contain secrets.
func writeOutputs(path string, st *status) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	lines := []string{
		"slot_id=" + st.slotID,
		fmt.Sprintf("attempt=%d", st.attempt),
		fmt.Sprintf("final_attempt=%t", st.final),
		fmt.Sprintf("slot_complete=%t", st.complete && !st.dryRun),
		fmt.Sprintf("coverage_pct=%.1f", st.coverage*100),
		fmt.Sprintf("failed_games=%d", st.failed),
		fmt.Sprintf("total_games=%d", st.total),
	}
	_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}

func printSummary(r run.Result, dryRun bool) {
	fmt.Printf("summary dry_run=%t already_complete=%t total=%d unmapped=%d already_done=%d missing=%d "+
		"updated=%d skipped=%d failed=%d retried=%d coverage_before=%.1f%% coverage_after=%.1f%%\n",
		dryRun, r.AlreadyComplete, r.Total, r.Unmapped, r.DoneBefore, r.Missing,
		r.Updated, r.Skipped, r.Failed, r.Retried, r.CoverageBefore*100, r.CoverageAfter*100)
}
