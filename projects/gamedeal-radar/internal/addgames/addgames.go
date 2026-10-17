// Package addgames processes the queue of admin-requested games. For each claimed request
// it runs the same steps as the full automation, for that one game, and activates the game
// when the core steps of the request's store succeed: Steam details and the Steam INR price
// for a Steam request, the store page details for an Epic or GOG request. The package
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
	"gamedeal-radar/daily-check/internal/storedetails"
)

// Request statuses written back to the queue.
const (
	StatusDone     = "done"
	StatusFailed   = "failed"
	StatusNotFound = "not_found"
)

// Store codes a request can come from (stores.code).
const (
	StoreSteam = "steam"
	StoreEpic  = "epic"
	StoreGOG   = "gog"
)

// Request is one claimed queue row.
type Request struct {
	ID         string
	StoreCode  string // steam, epic or gog
	ExternalID string // the store's own id: Steam app id, Epic slug, GOG slug
	SteamAppID string // empty for a non-Steam request
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
	// Details is the Steam details step, EpicDetails and GOGDetails the store page details
	// of a non-Steam request. All three report under the step name "details".
	Details(ctx context.Context, r Request) StepResult
	EpicDetails(ctx context.Context, r Request) StepResult
	GOGDetails(ctx context.Context, r Request) StepResult
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
		slog.Info("game request processed", "game", r.Name, "request_id", r.ID, "store", r.StoreCode,
			"status", out.Status, "dry_run", o.DryRun, "steps", StepsLine(out.Steps), "note", out.Note)
		fmt.Printf("request game=%q store=%s status=%s steps=[%s]\n", r.Name, r.StoreCode, out.Status, StepsLine(out.Steps))

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
// (not in a dry run) when the core steps of the request's store succeeded. It does not write
// the queue status.
func Process(ctx context.Context, q Queue, st Steps, r Request, dryRun bool) Outcome {
	switch r.StoreCode {
	case StoreSteam, "":
		return processSteam(ctx, q, st, r, dryRun)
	case StoreEpic:
		if !storedetails.ValidEpicSlug(r.ExternalID) {
			return Outcome{Request: r, Status: StatusNotFound, Note: "invalid epic slug"}
		}
		// Price checks never block activation: Epic prices also come from the daily
		// epic-prices job or the admin's manual price.
		return processStorePage(ctx, q, r, dryRun, st.EpicDetails, "epic has no such product", st.Epic, st.GOG)
	case StoreGOG:
		if !storedetails.ValidGOGSlug(r.ExternalID) {
			return Outcome{Request: r, Status: StatusNotFound, Note: "invalid gog slug"}
		}
		return processStorePage(ctx, q, r, dryRun, st.GOGDetails, "gog has no such product", st.GOG, st.Epic)
	}
	return Outcome{Request: r, Status: StatusFailed, Note: "store not supported by the job"}
}

type stepFunc func(context.Context, Request) StepResult

// processSteam is the Steam request: core steps details and INR, optional USD, tags, GOG, Epic.
func processSteam(ctx context.Context, q Queue, st Steps, r Request, dryRun bool) Outcome {
	out := Outcome{Request: r}
	if !appIDPattern.MatchString(r.SteamAppID) {
		out.Status = StatusNotFound
		out.Note = "invalid steam app id"
		return out
	}

	// Core step a: Steam details. A missing app, or an app that is not a game, ends the request.
	details := st.Details(ctx, r)
	out.Steps = append(out.Steps, details)
	if details.Status == StepFailed && isNotFound(details.Err) {
		out.Status = StatusNotFound
		out.Note = clip(orElse(details.Note, "steam has no such app"))
		return out
	}

	// Core step b: Steam INR price. Independent of the details step, so it runs either way.
	inr := st.INR(ctx, r)
	out.Steps = append(out.Steps, inr)

	if failed := coreFailures(details, inr); failed != "" {
		out.Status = StatusFailed
		out.Note = failed
		return out
	}

	// Optional steps never block activation.
	for _, step := range []stepFunc{st.USD, st.Tags, st.GOG, st.Epic} {
		out.Steps = append(out.Steps, step(ctx, r))
	}
	return activate(ctx, q, out, dryRun)
}

// processStorePage is an Epic or GOG request: the one core step reads the store page into
// game_details; the price checks are optional. The Steam INR, USD and tags steps do not run,
// since the request has no Steam app id (a Steam link of the same game is picked up by the
// daily Steam jobs once the game is active).
func processStorePage(ctx context.Context, q Queue, r Request, dryRun bool, details stepFunc, notFound string, optional ...stepFunc) Outcome {
	out := Outcome{Request: r}
	d := details(ctx, r)
	out.Steps = append(out.Steps, d)
	if d.Status == StepFailed && isNotFound(d.Err) {
		out.Status = StatusNotFound
		out.Note = clip(orElse(d.Note, notFound))
		return out
	}
	if failed := coreFailures(d); failed != "" {
		out.Status = StatusFailed
		out.Note = failed
		return out
	}
	for _, step := range optional {
		out.Steps = append(out.Steps, step(ctx, r))
	}
	return activate(ctx, q, out, dryRun)
}

func coreFailures(steps ...StepResult) string {
	var failed []string
	for _, s := range steps {
		if s.Status == StepFailed {
			failed = append(failed, s.Name+": "+orElse(s.Note, "failed"))
		}
	}
	return clip(strings.Join(failed, "; "))
}

// activate makes the game active (not in a dry run) and marks the outcome done.
func activate(ctx context.Context, q Queue, out Outcome, dryRun bool) Outcome {
	if !dryRun {
		if err := q.Activate(ctx, out.Request.GameID); err != nil {
			slog.Warn("activating game failed", "game", out.Request.Name, "error", err)
			out.Status = StatusFailed
			out.Note = "activate: database update failed"
			return out
		}
	}
	out.Status = StatusDone
	out.Note = clip(optionalNote(out.Steps))
	return out
}

// isNotFound reports a store's "no such product" answer (Steam, Epic or GOG).
func isNotFound(err error) bool {
	return errors.Is(err, steam.ErrNotFound) || errors.Is(err, storedetails.ErrNotFound)
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
