// Package addgames processes the queue of admin-requested games. For each claimed request
// it runs the same steps as the full automation, for that one game, and activates the game
// when the two core steps (Steam details and the Steam INR price) succeed. The package
// depends only on small interfaces, so the orchestration is tested without a database.
package addgames

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"gamedeal-radar/daily-check/internal/steam"
)

// Request statuses written back to the queue.
const (
	StatusDone     = "done"
	StatusFailed   = "failed"
	StatusNotFound = "not_found"
)

// Request is one claimed queue row.
type Request struct {
	ID         string
	SteamAppID string
	Name       string
	GameID     string
	Attempts   int
}

// StepStatus is the outcome of one step.
type StepStatus string

const (
	StepOK      StepStatus = "ok"
	StepSkipped StepStatus = "skipped"
	StepFailed  StepStatus = "failed"
)

// StepResult is what a step reports. Note is a short, non-sensitive reason for a skipped
// or failed step. Err is for logs and classification only and is never written to the queue.
type StepResult struct {
	Name   string
	Status StepStatus
	Note   string
	Err    error
}

// OK, Skipped and Failed build results.
func OK(name string) StepResult { return StepResult{Name: name, Status: StepOK} }
func Skipped(name, note string) StepResult {
	return StepResult{Name: name, Status: StepSkipped, Note: note}
}
func Failed(name, note string, err error) StepResult {
	return StepResult{Name: name, Status: StepFailed, Note: note, Err: err}
}

// Steps performs the work for one request. In a dry run the implementation fetches and
// writes nothing.
type Steps interface {
	Details(ctx context.Context, r Request) StepResult
	INR(ctx context.Context, r Request) StepResult
	USD(ctx context.Context, r Request) StepResult
	Tags(ctx context.Context, r Request) StepResult
	GOG(ctx context.Context, r Request) StepResult
	Epic(ctx context.Context, r Request) StepResult
}

// Queue is the persistence port.
type Queue interface {
	// Claim returns up to max requests and marks them processing (a dry run only reads).
	Claim(ctx context.Context, max int, ids []string, dryRun bool) ([]Request, error)
	// Finish records the end state of a claimed request.
	Finish(ctx context.Context, id, status, note string) error
	// Activate makes the game visible (games.is_active = true).
	Activate(ctx context.Context, gameID string) error
}

// Options controls a run.
type Options struct {
	Max    int
	IDs    []string
	DryRun bool
}

// Outcome is the result for one request.
type Outcome struct {
	Request Request
	Status  string // done, failed or not_found
	Note    string // what is written to game_requests.error
	Steps   []StepResult
}

// Summary is the result of a whole run.
type Summary struct {
	DryRun   bool
	Claimed  int
	Done     int
	NotFound int
	Failed   int
	Outcomes []Outcome
}

// Handled counts requests that reached a final answer (done or not_found).
func (s Summary) Handled() int { return s.Done + s.NotFound }

// AllFailed reports whether every claimed request failed.
func (s Summary) AllFailed() bool { return s.Claimed > 0 && s.Failed == s.Claimed }

var appIDPattern = regexp.MustCompile(`^[0-9]{1,12}$`)

// maxNote caps the text written to game_requests.error.
const maxNote = 200

// Run claims the queue and processes each request in turn. It returns an error only when
// the claim fails. Requests left unprocessed because the context ended are put back to
// failed so they are retried later.
func Run(ctx context.Context, q Queue, st Steps, o Options) (Summary, error) {
	sum := Summary{DryRun: o.DryRun}
	reqs, err := q.Claim(ctx, o.Max, o.IDs, o.DryRun)
	if err != nil {
		return sum, fmt.Errorf("claiming game requests: %w", err)
	}
	sum.Claimed = len(reqs)

	for _, r := range reqs {
		var out Outcome
		if ctx.Err() != nil {
			out = Outcome{Request: r, Status: StatusFailed, Note: "run cut short before this game, will retry"}
		} else {
			out = Process(ctx, q, st, r, o.DryRun)
		}
		sum.Outcomes = append(sum.Outcomes, out)
		switch out.Status {
		case StatusDone:
			sum.Done++
		case StatusNotFound:
			sum.NotFound++
		default:
			sum.Failed++
		}
		slog.Info("game request processed", "game", r.Name, "request_id", r.ID, "status", out.Status,
			"dry_run", o.DryRun, "steps", StepsLine(out.Steps), "note", out.Note)
		fmt.Printf("request game=%q status=%s steps=[%s]\n", r.Name, out.Status, StepsLine(out.Steps))

		if o.DryRun {
			continue
		}
		// Finish even when the run context has ended, so no row is left in processing.
		fctx := context.WithoutCancel(ctx)
		if err := q.Finish(fctx, r.ID, out.Status, out.Note); err != nil {
			slog.Error("could not record the request result, it will be reclaimed later",
				"request_id", r.ID, "status", out.Status, "error", err)
		}
	}
	return sum, nil
}

// Process runs the steps of one request and decides its status. It activates the game
// (not in a dry run) when both core steps succeeded. It does not write the queue status.
func Process(ctx context.Context, q Queue, st Steps, r Request, dryRun bool) Outcome {
	out := Outcome{Request: r}
	if !appIDPattern.MatchString(r.SteamAppID) {
		out.Status = StatusNotFound
		out.Note = "invalid steam app id"
		return out
	}

	// Core step a: Steam details. A missing app, or an app that is not a game, ends the request.
	details := st.Details(ctx, r)
	out.Steps = append(out.Steps, details)
	if details.Status == StepFailed && errors.Is(details.Err, steam.ErrNotFound) {
		out.Status = StatusNotFound
		out.Note = clip(orElse(details.Note, "steam has no such app"))
		return out
	}

	// Core step b: Steam INR price. Independent of the details step, so it runs either way.
	inr := st.INR(ctx, r)
	out.Steps = append(out.Steps, inr)

	var coreFailed []string
	for _, s := range []StepResult{details, inr} {
		if s.Status == StepFailed {
			coreFailed = append(coreFailed, s.Name+": "+orElse(s.Note, "failed"))
		}
	}
	if len(coreFailed) > 0 {
		out.Status = StatusFailed
		out.Note = clip(strings.Join(coreFailed, "; "))
		return out
	}

	// Optional steps never block activation.
	for _, step := range []func(context.Context, Request) StepResult{st.USD, st.Tags, st.GOG, st.Epic} {
		out.Steps = append(out.Steps, step(ctx, r))
	}

	if !dryRun {
		if err := q.Activate(ctx, r.GameID); err != nil {
			slog.Warn("activating game failed", "game", r.Name, "error", err)
			out.Status = StatusFailed
			out.Note = "activate: database update failed"
			return out
		}
	}
	out.Status = StatusDone
	out.Note = clip(optionalNote(out.Steps))
	return out
}

// optionalNote lists the optional steps that were skipped or failed, for example
// "gog: no match; epic: skipped (no api key)". Empty when all went well.
func optionalNote(steps []StepResult) string {
	var parts []string
	for _, s := range steps {
		switch s.Status {
		case StepSkipped:
			parts = append(parts, s.Name+": "+orElse(s.Note, "skipped"))
		case StepFailed:
			parts = append(parts, s.Name+": failed ("+orElse(s.Note, "error")+")")
		}
	}
	return strings.Join(parts, "; ")
}

// StepsLine renders "details=ok inr=ok usd=skipped gog=failed" for one log line.
func StepsLine(steps []StepResult) string {
	parts := make([]string, 0, len(steps))
	for _, s := range steps {
		parts = append(parts, s.Name+"="+string(s.Status))
	}
	return strings.Join(parts, " ")
}

func orElse(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func clip(s string) string {
	if len(s) <= maxNote {
		return s
	}
	return s[:maxNote-3] + "..."
}
