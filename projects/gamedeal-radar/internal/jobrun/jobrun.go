// Package jobrun holds what the store price jobs share with each other: environment
// parsing, the GITHUB_OUTPUT flags, healthcheck pings and the Slack message at the end.
// The Steam jobs keep their own copies, so their behaviour is unchanged.
package jobrun

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"gamedeal-radar/daily-check/internal/health"
	"gamedeal-radar/daily-check/internal/notify"
)

// BoolEnv reports whether the variable is 1, true or yes.
func BoolEnv(name string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(name))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// IsManual reports whether the run was started by workflow_dispatch.
func IsManual() bool { return os.Getenv("GITHUB_EVENT_NAME") == "workflow_dispatch" }

// LimitEnv reads LIMIT, a non-negative integer. Unset means 0 (no limit).
func LimitEnv() (int, error) {
	raw := strings.TrimSpace(os.Getenv("LIMIT"))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("LIMIT must be a non-negative integer, got %q", raw)
	}
	return n, nil
}

// DurationEnv reads a Go duration from 0 to 1h. Unset gives def.
func DurationEnv(name string, def time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 || d > time.Hour {
		return 0, fmt.Errorf("%s must be a duration from 0 to 1h, such as 2m or 0", name)
	}
	return d, nil
}

// HTTPURLEnv validates an optional http(s) URL. The error names the variable, never its value.
func HTTPURLEnv(name string) (string, error) {
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

// RequiredEnv returns a trimmed value or an error naming the variable.
func RequiredEnv(name string) (string, error) {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return "", fmt.Errorf("%s is not set", name)
	}
	return v, nil
}

// Report is the result of one job run, handed to Finish.
type Report struct {
	Job             string  // label used in Slack, for example "GOG prices"
	Code            int     // exit code: 0 ok, 1 failure
	DryRun          bool    // dry runs send nothing and ping nothing
	AlreadyComplete bool    // backup run that found the work done: no Slack message
	Blocked         bool    // the store refused the run
	Final           bool    // the last scheduled run of the day (FINAL_ATTEMPT=1)
	Coverage        float64 // share of games handled, when the job has coverage
	Failed          int
	Total           int
	Detail          string // one short line for Slack, without secrets
	Started         time.Time
}

// Finish sends the healthcheck pings, the Slack message and the GITHUB_OUTPUT flags.
// Every step is best effort: a failure is logged and never changes the exit code.
// URLs and webhooks are never logged.
func Finish(ctx context.Context, r Report, healthURL, finalHealthURL, outputPath string) {
	if !r.DryRun {
		primary := health.New(healthURL)
		if r.Code == 0 {
			if err := primary.Success(ctx); err != nil {
				slog.Warn("healthcheck success ping failed", "error", err)
			}
		} else {
			if err := primary.Fail(ctx); err != nil {
				slog.Warn("healthcheck fail ping failed", "error", err)
			}
			if r.Final {
				final := health.New(finalHealthURL)
				if err := final.Fail(ctx); err != nil {
					slog.Warn("final healthcheck fail ping failed", "error", err)
				}
			}
		}
	}

	sendSlack(ctx, r)

	if err := writeOutputs(outputPath, r); err != nil {
		slog.Warn("could not write workflow outputs", "error", err)
	}
}

func sendSlack(ctx context.Context, r Report) {
	n := notify.New(os.Getenv)
	if !n.Enabled() || r.DryRun {
		return
	}
	kind := ""
	switch {
	case r.Code == 0 && r.AlreadyComplete:
		return
	case r.Code == 0:
		kind = notify.JobSuccess
	default:
		kind = notify.JobFailed
	}
	n.Logf = func(msg string, err error) { slog.Warn(msg, "error", err) }
	err := n.Send(ctx, kind, notify.Data{
		Job:      r.Job,
		Detail:   r.Detail,
		Duration: time.Since(r.Started).Round(time.Second).String(),
		RunURL:   notify.RunURL(os.Getenv),
	})
	if err != nil {
		slog.Warn("slack notification failed", "kind", kind, "error", err)
	}
}

// writeOutputs appends flags to the GITHUB_OUTPUT file. A missing path is a no-op.
// Values never contain secrets. final_attempt is set on the last scheduled run, so the
// workflow can open a failure issue only then.
func writeOutputs(path string, r Report) error {
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	lines := []string{
		fmt.Sprintf("complete=%t", r.Code == 0 && !r.DryRun),
		fmt.Sprintf("final_attempt=%t", r.Final),
		fmt.Sprintf("blocked=%t", r.Blocked),
		fmt.Sprintf("coverage_pct=%.1f", r.Coverage*100),
		fmt.Sprintf("failed_games=%d", r.Failed),
		fmt.Sprintf("total_games=%d", r.Total),
	}
	_, err = f.WriteString(strings.Join(lines, "\n") + "\n")
	return err
}

// ErrConfig marks a configuration error. Mains exit with code 2 for it.
var ErrConfig = errors.New("configuration error")
