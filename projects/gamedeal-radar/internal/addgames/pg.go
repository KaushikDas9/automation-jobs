package addgames

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"gamedeal-radar/daily-check/internal/db"
	"gamedeal-radar/daily-check/internal/itad"
	"gamedeal-radar/daily-check/internal/run"
	"gamedeal-radar/daily-check/internal/steam"
	"gamedeal-radar/daily-check/internal/steamtags"
	"gamedeal-radar/daily-check/internal/storedetails"
	"gamedeal-radar/daily-check/internal/storeprices"
	"gamedeal-radar/daily-check/internal/usd"
)

// Retry waits inside one game's GOG and Epic steps and its INR step. They are short on
// purpose: a game that still fails is retried by the queue an hour later.
const (
	retryWait1 = 5 * time.Second
	retryWait2 = 15 * time.Second
)

// PgQueue implements Queue on one connection.
type PgQueue struct{ Conn *pgx.Conn }

// Claim implements Queue.
func (q PgQueue) Claim(ctx context.Context, max int, ids []string, dryRun bool) ([]Request, error) {
	rows, err := db.ClaimGameRequests(ctx, q.Conn, max, ids, dryRun)
	if err != nil {
		return nil, err
	}
	out := make([]Request, 0, len(rows))
	for _, r := range rows {
		out = append(out, Request{
			ID: r.ID, StoreCode: r.StoreCode, ExternalID: r.ExternalID, SteamAppID: r.SteamAppID,
			Name: r.Name, GameID: r.GameID, Attempts: r.Attempts,
		})
	}
	return out, nil
}

// Finish implements Queue.
func (q PgQueue) Finish(ctx context.Context, id, status, note string) error {
	return db.FinishGameRequest(ctx, q.Conn, id, status, note)
}

// Activate implements Queue.
func (q PgQueue) Activate(ctx context.Context, gameID string) error {
	return db.ActivateGame(ctx, q.Conn, gameID)
}

// TagAPI is the Steam tags client (steamtags.Client in production).
type TagAPI interface {
	TagNames(ctx context.Context) (map[int]string, error)
	GetItems(ctx context.Context, appIDs []int) ([]steamtags.StoreItem, error)
}

// StorePages reads Epic and GOG store pages (storedetails.Client in production).
type StorePages interface {
	EpicDetails(ctx context.Context, slug string) (*steam.Details, error)
	GOGDetails(ctx context.Context, slug, name string) (*steam.Details, error)
}

// PgSteps runs the real steps with the existing internal packages. The game is not active
// yet, so the "active games" lists those packages read are replaced by one-game views
// (oneGame and oneStoreGame) that return only the requested game.
type PgSteps struct {
	Repo    *db.Repo
	Store   db.StorePriceRepo
	Steam   *steam.Client
	TagSrc  TagAPI
	GOGAPI  storeprices.GOGPricer
	EpicAPI itad.API // nil when ITAD_API_KEY is not set: the Epic step is skipped
	Pages   StorePages
	DryRun  bool

	tagNames    map[int]string
	tagNamesErr error
}

// oneGame limits run.Repo and usd.Repo to a single game, whether active or not.
type oneGame struct {
	*db.Repo
	id, name, appID string
}

func (o oneGame) ListActiveSteamGames(context.Context) ([]run.Game, error) {
	return []run.Game{{ID: o.id, Slug: o.name, ExternalAppID: o.appID}}, nil
}

func (o oneGame) CountCheckedSince(ctx context.Context, since time.Time) (int, error) {
	return o.Repo.CountGameCheckedSince(ctx, o.id, since)
}

func (o oneGame) ListMissingUSD(context.Context, time.Time) ([]usd.Game, error) {
	return []usd.Game{{ID: o.id, Slug: o.name, ExternalAppID: o.appID}}, nil
}

// oneStoreGame limits storeprices.Repo to a single game, whether active or not.
type oneStoreGame struct {
	db.StorePriceRepo
	id, name string
}

func (o oneStoreGame) ActiveGames(context.Context) ([]storeprices.Game, error) {
	return []storeprices.Game{{ID: o.id, Name: o.name}}, nil
}

func (o oneStoreGame) StoredCents(ctx context.Context, store string) (map[storeprices.StoredKey]int64, error) {
	all, err := o.StorePriceRepo.StoredCents(ctx, store)
	if err != nil {
		return nil, err
	}
	for k := range all {
		if k.GameID != o.id {
			delete(all, k)
		}
	}
	return all, nil
}

func (o oneStoreGame) ManualGameIDs(ctx context.Context, store string) (map[string]bool, error) {
	all, err := o.StorePriceRepo.ManualGameIDs(ctx, store)
	if err != nil {
		return nil, err
	}
	return map[string]bool{o.id: all[o.id]}, nil
}

func (s *PgSteps) one(r Request) oneGame {
	return oneGame{Repo: s.Repo, id: r.GameID, name: r.Name, appID: r.SteamAppID}
}

func (s *PgSteps) storeOpts(ctx context.Context) storeprices.Options {
	var dl time.Time
	if d, ok := ctx.Deadline(); ok {
		dl = d
	}
	return storeprices.Options{
		DryRun: s.DryRun,
		Manual: true, // never use the already-complete shortcut
		PassOptions: storeprices.PassOptions{
			RetryWait1: retryWait1, RetryWait2: retryWait2, Deadline: dl,
		},
	}
}

// nonGameTypes are Steam app types that are not something the site lists.
var nonGameTypes = map[string]bool{
	"music": true, "video": true, "series": true, "episode": true, "advertising": true, "hardware": true,
}

// Details fetches the Steam store details and reviews and saves them (details-check logic).
func (s *PgSteps) Details(ctx context.Context, r Request) StepResult {
	d, err := s.Steam.FetchDetails(ctx, r.SteamAppID)
	if err != nil {
		if errors.Is(err, steam.ErrNotFound) {
			return Failed("details", "steam has no such app", err)
		}
		return Failed("details", fetchNote(ctx, err), err)
	}
	if nonGameTypes[d.Type] {
		return Failed("details", "steam app is not a game ("+d.Type+")", steam.ErrNotFound)
	}
	rev, err := s.Steam.FetchReviews(ctx, r.SteamAppID)
	if err != nil {
		rev = nil // reviews are optional, as in details-check
	}
	if s.DryRun {
		return OK("details")
	}
	if err := s.Repo.UpsertGameDetails(ctx, r.GameID, d, rev); err != nil {
		return Failed("details", "database write failed", err)
	}
	return OK("details")
}

// EpicDetails reads the Epic product page of the request's slug and saves it as the game's
// details. Review columns stay empty (Epic has no review summary).
func (s *PgSteps) EpicDetails(ctx context.Context, r Request) StepResult {
	if s.Pages == nil {
		return Failed("details", "store pages not configured", nil)
	}
	d, err := s.Pages.EpicDetails(ctx, r.ExternalID)
	return s.saveStoreDetails(ctx, r, "epic", d, err)
}

// GOGDetails reads the GOG product data of the request's slug and saves it as the game's details.
func (s *PgSteps) GOGDetails(ctx context.Context, r Request) StepResult {
	if s.Pages == nil {
		return Failed("details", "store pages not configured", nil)
	}
	d, err := s.Pages.GOGDetails(ctx, r.ExternalID, r.Name)
	return s.saveStoreDetails(ctx, r, "gog", d, err)
}

func (s *PgSteps) saveStoreDetails(ctx context.Context, r Request, store string, d *steam.Details, err error) StepResult {
	if err == nil && d == nil {
		err = errors.New("no details returned")
	}
	if err != nil {
		return Failed("details", storeNote(ctx, store, err), err)
	}
	if s.DryRun {
		return OK("details")
	}
	if err := s.Repo.UpsertGameDetails(ctx, r.GameID, d, nil); err != nil {
		return Failed("details", "database write failed", err)
	}
	return OK("details")
}

// storeNote is a fixed, non-sensitive reason for a failed Epic or GOG page read.
func storeNote(ctx context.Context, store string, err error) string {
	switch {
	case errors.Is(err, storedetails.ErrNotFound):
		return store + " has no such product"
	case errors.Is(err, storedetails.ErrInvalidID):
		return "invalid " + store + " id"
	case ctx.Err() != nil:
		return "run cut short"
	case errors.Is(err, storedetails.ErrBlocked):
		return store + " refused the request"
	case storedetails.IsTransient(err):
		return store + " temporarily unavailable"
	}
	return store + " page could not be read"
}

// INR runs the daily-check path (fetch, snapshot, current price, events, MarkChecked) for
// this one game through run.Execute.
func (s *PgSteps) INR(ctx context.Context, r Request) StepResult {
	var dl time.Time
	if d, ok := ctx.Deadline(); ok {
		dl = d
	}
	res, err := run.Execute(ctx, s.one(r), s.Steam, run.Options{
		SteamStoreID: s.Repo.SteamStoreID,
		SlotStart:    time.Now().UTC().Add(-time.Minute),
		DryRun:       s.DryRun,
		RetryWait1:   retryWait1,
		RetryWait2:   retryWait2,
		Deadline:     dl,
	})
	switch {
	case err != nil:
		return Failed("inr", "price check failed", err)
	case res.Cancelled:
		return Failed("inr", "run cut short", ctx.Err())
	case res.Failed > 0:
		return Failed("inr", "steam price fetch failed", nil)
	case res.Skipped > 0:
		return Skipped("inr", "no steam price in India")
	}
	return OK("inr")
}

// USD runs the USD pass for this one game.
func (s *PgSteps) USD(ctx context.Context, r Request) StepResult {
	res, err := usd.Execute(ctx, s.one(r), s.Steam, usd.Options{DryRun: s.DryRun})
	switch {
	case err != nil || res.Failed > 0:
		return Failed("usd", "fetch or save failed", err)
	case res.Unavailable > 0:
		return Skipped("usd", "no usd price")
	}
	return OK("usd")
}

// Tags collects the game's Steam tags and saves them (steam-tags logic).
func (s *PgSteps) Tags(ctx context.Context, r Request) StepResult {
	id, err := strconv.Atoi(r.SteamAppID)
	if err != nil || id <= 0 {
		return Failed("tags", "invalid app id", err)
	}
	if s.tagNames == nil && s.tagNamesErr == nil {
		s.tagNames, s.tagNamesErr = s.TagSrc.TagNames(ctx)
	}
	if s.tagNamesErr != nil {
		return Failed("tags", "tag list unavailable", s.tagNamesErr)
	}
	res, err := steamtags.Collect(ctx, s.TagSrc, s.tagNames, []int{id})
	if err != nil || len(res.Failed) > 0 {
		return Failed("tags", "fetch failed", err)
	}
	tags := res.Tags[id]
	if len(tags) == 0 {
		return Skipped("tags", "no tags")
	}
	if !s.DryRun {
		if err := s.Repo.SaveTags(ctx, r.GameID, tags); err != nil {
			return Failed("tags", "database write failed", err)
		}
	}
	return OK("tags")
}

// GOG runs the GOG price pass for this one game.
func (s *PgSteps) GOG(ctx context.Context, r Request) StepResult {
	repo := oneStoreGame{StorePriceRepo: s.Store, id: r.GameID, name: r.Name}
	sum, err := storeprices.RunGOG(ctx, repo, s.GOGAPI, s.storeOpts(ctx))
	if res, bad := storeFailure("gog", sum, err); bad {
		return res
	}
	total := 0
	for _, n := range sum.WithPrice {
		total += n
	}
	switch {
	case total > 0:
		return OK("gog")
	case len(sum.ListedNoPrice) > 0:
		return Skipped("gog", "listed without a price")
	}
	return Skipped("gog", "no match")
}

// Epic runs the Epic price pass (through the price data provider) for this one game. With
// no API key the step is skipped.
func (s *PgSteps) Epic(ctx context.Context, r Request) StepResult {
	if s.EpicAPI == nil {
		return Skipped("epic", "skipped, no api key")
	}
	repo := oneStoreGame{StorePriceRepo: s.Store, id: r.GameID, name: r.Name}
	sum, err := itad.RunEpic(ctx, repo, s.EpicAPI, s.storeOpts(ctx))
	if res, bad := storeFailure("epic", sum, err); bad {
		return res
	}
	total := 0
	for _, n := range sum.WithPrice {
		total += n
	}
	switch {
	case sum.SkippedManual > 0:
		return Skipped("epic", "manual row kept")
	case total > 0:
		return OK("epic")
	case sum.NoMatch+sum.TitleMismatch > 0:
		return Skipped("epic", "no match")
	}
	return Skipped("epic", "no epic deal")
}

// storeFailure maps a store summary to a failed step result when the run did not complete.
func storeFailure(name string, sum storeprices.Summary, err error) (StepResult, bool) {
	switch {
	case err != nil:
		return Failed(name, "lookup failed", err), true
	case sum.Blocked:
		return Failed(name, "blocked by the store", nil), true
	case sum.Cancelled:
		return Failed(name, "run cut short", nil), true
	case sum.Failed > 0:
		return Failed(name, "lookup failed", nil), true
	}
	return StepResult{}, false
}

// fetchNote is a short, non-sensitive reason for a failed Steam fetch. It never includes
// the error text, which may carry URLs.
func fetchNote(ctx context.Context, err error) string {
	switch {
	case ctx.Err() != nil:
		return "run cut short"
	case steam.IsTransient(err):
		return "steam temporarily unavailable"
	}
	return "steam request failed"
}
