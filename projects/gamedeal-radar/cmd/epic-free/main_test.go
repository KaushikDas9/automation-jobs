package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gamedeal-radar/daily-check/internal/epicfree"
)

// fakeSource answers from fixed data and never touches the network.
type fakeSource struct {
	games []epicfree.Game
	err   error
	calls int
}

func (f *fakeSource) FetchGames(context.Context) ([]epicfree.Game, error) {
	f.calls++
	return f.games, f.err
}

// fakeSave records what it was asked to write. It never touches a database.
type fakeSave struct {
	err   error
	calls int
	got   []epicfree.Game
}

func (f *fakeSave) save(_ context.Context, games []epicfree.Game) error {
	f.calls++
	f.got = games
	return f.err
}

func twoGames() []epicfree.Game {
	price := "1499.00"
	return []epicfree.Game{
		{EpicID: "a", Title: "Now", StoreURL: "https://store.epicgames.com/en-US/p/now", RegularPrice: &price, Currency: "INR"},
		{EpicID: "b", Title: "Next", StoreURL: "https://store.epicgames.com/en-US/p/next", Currency: "INR", Upcoming: true},
	}
}

func TestRunJobStoresFreeGames(t *testing.T) {
	src := &fakeSource{games: twoGames()}
	sv := &fakeSave{}
	sum := runJob(t.Context(), src, sv.save, config{})
	if sum.Err != nil || decide(sum) != 0 {
		t.Fatalf("summary = %+v code = %d", sum, decide(sum))
	}
	if sv.calls != 1 || len(sv.got) != 2 {
		t.Fatalf("save calls = %d, games = %d", sv.calls, len(sv.got))
	}
	if !sum.Stored || sum.Found != 2 || sum.FreeNow != 1 || sum.Upcoming != 1 {
		t.Fatalf("counts = %+v", sum)
	}
	if sum.detail() != "1 free now, 1 upcoming stored" {
		t.Fatalf("detail = %q", sum.detail())
	}
}

func TestRunJobDryRunWritesNothing(t *testing.T) {
	src := &fakeSource{games: twoGames()}
	sv := &fakeSave{}
	sum := runJob(t.Context(), src, sv.save, config{dryRun: true})
	if sv.calls != 0 {
		t.Fatalf("dry run called save %d times", sv.calls)
	}
	if sum.Stored || sum.Err != nil || decide(sum) != 0 {
		t.Fatalf("dry run summary = %+v", sum)
	}
	if !strings.HasPrefix(sum.detail(), "dry run: ") || !strings.Contains(sum.line(), "dry_run=true") {
		t.Fatalf("dry run text: %q / %q", sum.detail(), sum.line())
	}
}

func TestRunJobZeroFreeGamesIsFailureAndWritesNothing(t *testing.T) {
	src := &fakeSource{games: nil}
	sv := &fakeSave{}
	sum := runJob(t.Context(), src, sv.save, config{})
	if decide(sum) != 1 || sum.Err == nil {
		t.Fatalf("zero games must fail: %+v", sum)
	}
	if sv.calls != 0 {
		t.Fatal("a feed with no free games must write nothing")
	}
	if !strings.Contains(sum.detail(), "no free games") {
		t.Fatalf("detail = %q", sum.detail())
	}
}

func TestRunJobZeroFreeGamesIsFailureEvenOnDryRun(t *testing.T) {
	sum := runJob(t.Context(), &fakeSource{}, (&fakeSave{}).save, config{dryRun: true})
	if decide(sum) != 1 {
		t.Fatal("zero games must fail a dry run too")
	}
}

func TestRunJobFetchErrorWritesNothing(t *testing.T) {
	src := &fakeSource{err: errors.New("epic free games feed returned HTTP 503")}
	sv := &fakeSave{}
	sum := runJob(t.Context(), src, sv.save, config{})
	if decide(sum) != 1 || sv.calls != 0 {
		t.Fatalf("fetch failure: code=%d saves=%d", decide(sum), sv.calls)
	}
	if sum.detail() != "feed not read, nothing written" {
		t.Fatalf("detail = %q", sum.detail())
	}
}

func TestRunJobStoreErrorIsFailure(t *testing.T) {
	src := &fakeSource{games: twoGames()}
	sv := &fakeSave{err: errors.New("upserting epic free game: boom")}
	sum := runJob(t.Context(), src, sv.save, config{})
	if decide(sum) != 1 || sum.Stored {
		t.Fatalf("store failure: %+v", sum)
	}
	if sum.detail() != "storing the games failed, nothing written" {
		t.Fatalf("detail = %q", sum.detail())
	}
}

func TestDetailNeverCarriesErrorText(t *testing.T) {
	const secret = "postgres://user:hunter2@example.test/db"
	sum := summary{Err: errors.New("connecting to " + secret), Reason: "database unreachable, nothing written"}
	if strings.Contains(sum.detail(), "hunter2") || strings.Contains(sum.detail(), secret) {
		t.Fatalf("detail leaked the error: %q", sum.detail())
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("GDR_DATABASE_URL", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("a missing GDR_DATABASE_URL must be a configuration error")
	}

	t.Setenv("GDR_DATABASE_URL", "postgres://example.test/db")
	t.Setenv("DRY_RUN", "1")
	t.Setenv("FINAL_ATTEMPT", "true")
	cfg, err := loadConfig()
	if err != nil || !cfg.dryRun || !cfg.final {
		t.Fatalf("cfg = %+v err = %v", cfg, err)
	}
}

func TestRunOnceConfigErrorExitsTwo(t *testing.T) {
	t.Setenv("GDR_DATABASE_URL", "")
	if code := runOnce(); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}
