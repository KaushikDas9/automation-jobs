package run

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gamedeal-radar/daily-check/internal/pricing"
	"gamedeal-radar/daily-check/internal/steam"
)

var slotStart = time.Date(2026, 10, 10, 18, 30, 0, 0, time.UTC)

// fakeRepo is an in-memory Repo that mirrors the DB's last_checked_at behaviour.
type fakeRepo struct {
	games     []Game
	snapshots int
	events    int
	marked    []string
	saveErr   error
	countErr  error
}

func (f *fakeRepo) ListActiveSteamGames(context.Context) ([]Game, error) {
	out := make([]Game, len(f.games))
	copy(out, f.games)
	return out, nil
}

func (f *fakeRepo) CountCheckedSince(_ context.Context, since time.Time) (int, error) {
	if f.countErr != nil {
		return 0, f.countErr
	}
	n := 0
	for _, g := range f.games {
		if g.ExternalAppID != "" && g.LastCheckedAt != nil && !g.LastCheckedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

func (f *fakeRepo) MarkChecked(_ context.Context, gameID string, at time.Time) error {
	f.marked = append(f.marked, gameID)
	f.setChecked(gameID, at)
	return nil
}

func (f *fakeRepo) setChecked(gameID string, at time.Time) {
	for i := range f.games {
		if f.games[i].ID == gameID {
			t := at
			f.games[i].LastCheckedAt = &t
		}
	}
}

func (f *fakeRepo) LatestSnapshot(context.Context, string, string) (*pricing.Snapshot, error) {
	return nil, pricing.ErrNoSnapshotFound
}

func (f *fakeRepo) SaveSnapshot(_ context.Context, s *pricing.Snapshot) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.snapshots++
	f.setChecked(s.GameID, s.CheckedAt)
	return nil
}

func (f *fakeRepo) RecordEvent(context.Context, *pricing.Event) error {
	f.events++
	return nil
}

// fakeFetcher returns scripted results per app id. Each call pops the next outcome;
// when the script runs out, it returns a successful price.
type fakeFetcher struct {
	script map[string][]error
	calls  map[string]int
	total  int
}

func newFetcher(script map[string][]error) *fakeFetcher {
	return &fakeFetcher{script: script, calls: map[string]int{}}
}

func (f *fakeFetcher) FetchPrice(_ context.Context, appID, _ string) (*steam.PriceResult, error) {
	f.calls[appID]++
	f.total++
	if s := f.script[appID]; len(s) > 0 {
		err := s[0]
		f.script[appID] = s[1:]
		if err != nil {
			return nil, err
		}
	}
	return &steam.PriceResult{ExternalAppID: appID, Country: "in", Currency: "INR", CurrentPrice: 100, BasePrice: 200, DiscountPct: 50, IsDeal: true, CheckedAt: testNow}, nil
}

func instantSleep(context.Context, time.Duration) error { return nil }

func mkGames(n int) []Game {
	out := make([]Game, n)
	for i := range out {
		out[i] = Game{ID: fmt.Sprintf("g%03d", i), Slug: fmt.Sprintf("game-%03d", i), ExternalAppID: fmt.Sprintf("%d", 1000+i)}
	}
	return out
}

func tAt(s string) *time.Time {
	t, _ := time.Parse(time.RFC3339, s)
	return &t
}

// testNow is a fixed clock inside the test slot, so timestamps written by the run
// are always after SlotStart regardless of the real clock.
var testNow = slotStart.Add(time.Hour)

func opts(dry bool) Options {
	return Options{
		SteamStoreID: "steam-id",
		SlotStart:    slotStart,
		DryRun:       dry,
		RetryWait1:   0,
		RetryWait2:   0,
		Sleep:        instantSleep,
		Now:          func() time.Time { return testNow },
	}
}

func TestResumeFetchesOnlyMissingGames(t *testing.T) {
	games := mkGames(10)
	// Games 0-2 were checked in this slot. Game 3 was checked in the previous slot.
	for i := 0; i < 3; i++ {
		games[i].LastCheckedAt = tAt("2026-10-10T19:00:00Z")
	}
	games[3].LastCheckedAt = tAt("2026-10-10T07:00:00Z")
	repo := &fakeRepo{games: games}
	f := newFetcher(map[string][]error{})

	res, err := Execute(context.Background(), repo, f, opts(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.DoneBefore != 3 || res.Missing != 7 {
		t.Fatalf("done=%d missing=%d, want 3 and 7", res.DoneBefore, res.Missing)
	}
	if f.total != 7 {
		t.Fatalf("fetched %d games, want 7", f.total)
	}
	for i := 0; i < 3; i++ {
		if f.calls[games[i].ExternalAppID] != 0 {
			t.Fatalf("already-checked game %s was fetched again", games[i].Slug)
		}
	}
	if res.CoverageAfter != 1.0 {
		t.Fatalf("coverage after = %v, want 1.0", res.CoverageAfter)
	}
}

func TestSlotAlreadyCompleteSkipsFetching(t *testing.T) {
	games := mkGames(10)
	for i := 0; i < 9; i++ { // 90% done
		games[i].LastCheckedAt = tAt("2026-10-10T19:00:00Z")
	}
	repo := &fakeRepo{games: games}
	f := newFetcher(map[string][]error{})

	res, err := Execute(context.Background(), repo, f, opts(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.AlreadyComplete {
		t.Fatal("expected AlreadyComplete at exactly 90% coverage")
	}
	if f.total != 0 {
		t.Fatalf("no fetches expected, got %d", f.total)
	}
}

func TestTransientFailuresRetriedThenSucceed(t *testing.T) {
	games := mkGames(5)
	repo := &fakeRepo{games: games}
	transient := fmt.Errorf("network: %w", &steam.StatusError{Code: 503})
	f := newFetcher(map[string][]error{
		"1001": {transient}, // fails once, then succeeds on retry
		"1003": {steam.ErrRateLimited, steam.ErrRateLimited},
	})

	res, err := Execute(context.Background(), repo, f, opts(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Retried != 2 {
		t.Fatalf("retried = %d, want 2", res.Retried)
	}
	if f.calls["1001"] != 2 {
		t.Fatalf("game 1001 fetched %d times, want 2", f.calls["1001"])
	}
	if f.calls["1003"] != 3 {
		t.Fatalf("game 1003 fetched %d times, want 3 (main + two retry passes)", f.calls["1003"])
	}
	if res.Failed != 0 || res.Updated != 5 {
		t.Fatalf("updated=%d failed=%d, want 5 and 0", res.Updated, res.Failed)
	}
}

func TestPermanentFailuresNotRetried(t *testing.T) {
	games := mkGames(3)
	repo := &fakeRepo{games: games}
	f := newFetcher(map[string][]error{
		"1000": {steam.ErrNotFound},
		"1001": {steam.ErrPriceUnavailable},
		"1002": {&steam.StatusError{Code: 404}},
	})

	res, err := Execute(context.Background(), repo, f, opts(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Retried != 0 {
		t.Fatalf("permanent failures must not be retried, retried=%d", res.Retried)
	}
	if res.Skipped != 2 || res.Failed != 1 {
		t.Fatalf("skipped=%d failed=%d, want 2 and 1", res.Skipped, res.Failed)
	}
	if f.total != 3 {
		t.Fatalf("each permanent failure fetched once, total=%d", f.total)
	}
	// Skipped (not found / unavailable) games are recorded as checked.
	if len(repo.marked) != 2 {
		t.Fatalf("marked = %d, want 2", len(repo.marked))
	}
}

func TestRetryStillFailingCountsAsFailed(t *testing.T) {
	games := mkGames(2)
	repo := &fakeRepo{games: games}
	f := newFetcher(map[string][]error{
		"1000": {steam.ErrRateLimited, steam.ErrRateLimited, steam.ErrRateLimited},
	})

	res, err := Execute(context.Background(), repo, f, opts(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.calls["1000"] != 3 {
		t.Fatalf("calls = %d, want main + 2 retry passes", f.calls["1000"])
	}
	if res.Failed != 1 || res.Updated != 1 {
		t.Fatalf("failed=%d updated=%d", res.Failed, res.Updated)
	}
}

func TestRetryWaitsSkippedWhenDeadlineTooClose(t *testing.T) {
	games := mkGames(1)
	repo := &fakeRepo{games: games}
	f := newFetcher(map[string][]error{"1000": {steam.ErrRateLimited}})

	o := opts(false)
	o.Now = func() time.Time { return time.Date(2026, 10, 10, 19, 0, 0, 0, time.UTC) }
	o.Deadline = o.Now().Add(6 * time.Minute) // 6 min left: a 5 min wait leaves 1 min, below the 5 min floor
	o.RetryWait1 = 5 * time.Minute
	slept := 0
	o.Sleep = func(context.Context, time.Duration) error { slept++; return nil }

	res, err := Execute(context.Background(), repo, f, o)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if slept != 0 {
		t.Fatalf("waited %d times despite deadline, want 0", slept)
	}
	if res.Failed != 1 {
		t.Fatalf("failed = %d, want 1", res.Failed)
	}
}

func TestRetryWaitAllowedWhenPlentyOfTime(t *testing.T) {
	games := mkGames(1)
	repo := &fakeRepo{games: games}
	f := newFetcher(map[string][]error{"1000": {steam.ErrRateLimited}})

	o := opts(false)
	now := time.Date(2026, 10, 10, 19, 0, 0, 0, time.UTC)
	o.Now = func() time.Time { return now }
	o.Deadline = now.Add(45 * time.Minute)
	o.RetryWait1 = 5 * time.Minute
	slept := 0
	o.Sleep = func(context.Context, time.Duration) error { slept++; return nil }

	if _, err := Execute(context.Background(), repo, f, o); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// One wait before the first retry pass; the second pass is not needed because
	// the game succeeds on the retry, so only one wait is expected.
	if slept != 1 {
		t.Fatalf("slept %d times, want 1", slept)
	}
}

func TestDryRunWritesNothing(t *testing.T) {
	games := mkGames(3)
	repo := &fakeRepo{games: games}
	f := newFetcher(map[string][]error{"1001": {steam.ErrNotFound}})

	res, err := Execute(context.Background(), repo, f, opts(true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if repo.snapshots != 0 || repo.events != 0 || len(repo.marked) != 0 {
		t.Fatalf("dry run wrote data: snapshots=%d events=%d marked=%d", repo.snapshots, repo.events, len(repo.marked))
	}
	if res.Updated != 2 || res.Skipped != 1 {
		t.Fatalf("updated=%d skipped=%d", res.Updated, res.Skipped)
	}
	if res.CoverageAfter != 1.0 {
		t.Fatalf("projected coverage = %v, want 1.0", res.CoverageAfter)
	}
}

func TestLimitCapsFetchedGames(t *testing.T) {
	repo := &fakeRepo{games: mkGames(10)}
	f := newFetcher(map[string][]error{})
	o := opts(true)
	o.Limit = 4
	if _, err := Execute(context.Background(), repo, f, o); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.total != 4 {
		t.Fatalf("fetched %d, want 4", f.total)
	}
}

func TestUnmappedGamesExcludedFromCoverage(t *testing.T) {
	games := mkGames(2)
	games = append(games, Game{ID: "unmapped", Slug: "no-steam"})
	repo := &fakeRepo{games: games}
	res, err := Execute(context.Background(), repo, newFetcher(map[string][]error{}), opts(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Total != 2 || res.Unmapped != 1 {
		t.Fatalf("total=%d unmapped=%d", res.Total, res.Unmapped)
	}
}

func TestNoMappedGamesIsAnError(t *testing.T) {
	repo := &fakeRepo{games: []Game{{ID: "x", Slug: "x"}}}
	if _, err := Execute(context.Background(), repo, newFetcher(nil), opts(false)); !errors.Is(err, ErrNoGames) {
		t.Fatalf("err = %v, want ErrNoGames", err)
	}
}

func TestRecountErrorIsReturned(t *testing.T) {
	repo := &fakeRepo{games: mkGames(2), countErr: errors.New("db down")}
	if _, err := Execute(context.Background(), repo, newFetcher(map[string][]error{}), opts(false)); err == nil {
		t.Fatal("expected the recount error to be returned")
	}
}

func TestCancelledRunStillRecountsAndReportsCoverage(t *testing.T) {
	games := mkGames(4)
	repo := &fakeRepo{games: games}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := Execute(ctx, repo, newFetcher(map[string][]error{}), opts(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Cancelled {
		t.Fatal("expected Cancelled")
	}
	if res.CoverageAfter != 0 {
		t.Fatalf("coverage after = %v, want 0 because nothing was processed", res.CoverageAfter)
	}
}

func TestSucceededThreshold(t *testing.T) {
	cases := map[float64]bool{1.0: true, 0.9: true, 0.95: true, 0.89: false, 0.5: false, 0: false}
	for cov, want := range cases {
		if got := Succeeded(cov); got != want {
			t.Errorf("Succeeded(%v) = %v, want %v", cov, got, want)
		}
	}
}

func TestFailuresAboveThresholdStillSucceedIfCoverageHolds(t *testing.T) {
	// 20 of 200 games fail permanently (coverage 90%): the run still succeeds.
	games := mkGames(200)
	repo := &fakeRepo{games: games}
	script := map[string][]error{}
	for i := 0; i < 20; i++ {
		script[games[i].ExternalAppID] = []error{&steam.StatusError{Code: 400}}
	}
	res, err := Execute(context.Background(), repo, newFetcher(script), opts(false))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Failed != 20 {
		t.Fatalf("failed = %d, want 20", res.Failed)
	}
	if !Succeeded(res.CoverageAfter) {
		t.Fatalf("coverage %v should satisfy the threshold", res.CoverageAfter)
	}
}
