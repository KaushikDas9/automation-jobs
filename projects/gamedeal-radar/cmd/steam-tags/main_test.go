package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"gamedeal-radar/daily-check/internal/db"
	"gamedeal-radar/daily-check/internal/steamtags"
)

// fakeAPI answers from fixed data. items may fail; the names may fail.
type fakeAPI struct {
	names    map[int]string
	namesErr error
	items    func(ids []int) ([]steamtags.StoreItem, error)
}

func (f *fakeAPI) TagNames(context.Context) (map[int]string, error) {
	if f.namesErr != nil {
		return nil, f.namesErr
	}
	return f.names, nil
}

func (f *fakeAPI) GetItems(_ context.Context, ids []int) ([]steamtags.StoreItem, error) {
	if f.items == nil {
		return nil, nil
	}
	return f.items(ids)
}

// fakeStore records writes. It never touches a database.
type fakeStore struct {
	games     []db.GameRef
	listErr   error
	saveErr   error
	gotLimit  int
	saved     map[string][]steamtags.Tag
	saveCalls int
}

func (f *fakeStore) ListGamesForTags(_ context.Context, limit int) ([]db.GameRef, error) {
	f.gotLimit = limit
	return f.games, f.listErr
}

func (f *fakeStore) SaveTags(_ context.Context, gameID string, tags []steamtags.Tag) error {
	f.saveCalls++
	if f.saveErr != nil {
		return f.saveErr
	}
	if f.saved == nil {
		f.saved = map[string][]steamtags.Tag{}
	}
	f.saved[gameID] = tags
	return nil
}

var names = map[int]string{1695: "Open World", 122: "RPG"}

func tagged(appID int) steamtags.StoreItem {
	return steamtags.StoreItem{AppID: appID, Success: 1, Tags: []steamtags.ItemTag{{TagID: 1695, Weight: 9}, {TagID: 122, Weight: 3}}}
}

func TestRunJobWritesTagsAndSkipsEmpty(t *testing.T) {
	api := &fakeAPI{names: names, items: func(ids []int) ([]steamtags.StoreItem, error) {
		return []steamtags.StoreItem{
			tagged(10),
			{AppID: 20, Success: 1},
			{AppID: 30, Success: 0, Tags: []steamtags.ItemTag{{TagID: 122, Weight: 1}}},
		}, nil
	}}
	store := &fakeStore{games: []db.GameRef{
		{GameID: "g1", ExternalAppID: "10"},
		{GameID: "g2", ExternalAppID: "20"},
		{GameID: "g3", ExternalAppID: "30"},
		{GameID: "g4", ExternalAppID: "10"},
	}}
	sum := runJob(t.Context(), api, store, config{limit: 7})
	if sum.Err != nil {
		t.Fatal(sum.Err)
	}
	if store.gotLimit != 7 {
		t.Fatalf("limit passed = %d", store.gotLimit)
	}
	if len(store.saved) != 2 || store.saved["g1"][0].ID != "1695" || store.saved["g4"][0].Description != "Open World" {
		t.Fatalf("saved = %v", store.saved)
	}
	if sum.Tagged != 2 || sum.NoTags != 2 || sum.Failed != 0 || sum.Coverage != 1 {
		t.Fatalf("summary = %+v", sum)
	}
	if decide(sum) != 0 {
		t.Fatal("full coverage must exit 0")
	}
}

func TestRunJobDryRunWritesNothing(t *testing.T) {
	api := &fakeAPI{names: names, items: func(ids []int) ([]steamtags.StoreItem, error) {
		return []steamtags.StoreItem{tagged(10)}, nil
	}}
	store := &fakeStore{games: []db.GameRef{{GameID: "g1", ExternalAppID: "10"}}}
	sum := runJob(t.Context(), api, store, config{dryRun: true})
	if store.saveCalls != 0 {
		t.Fatalf("dry run wrote %d times", store.saveCalls)
	}
	if sum.Tagged != 1 || !sum.DryRun || decide(sum) != 0 {
		t.Fatalf("dry run summary = %+v", sum)
	}
	if !strings.HasPrefix(sum.detail(), "dry run: ") || !strings.Contains(sum.line(), "dry_run=true") {
		t.Fatalf("dry run text: %q / %q", sum.detail(), sum.line())
	}
}

func TestRunJobFailedBatchCountsAsFailed(t *testing.T) {
	api := &fakeAPI{names: names, items: func([]int) ([]steamtags.StoreItem, error) {
		return nil, errors.New("HTTP 500")
	}}
	store := &fakeStore{games: []db.GameRef{{GameID: "g1", ExternalAppID: "10"}, {GameID: "g2", ExternalAppID: "11"}}}
	sum := runJob(t.Context(), api, store, config{})
	if sum.Err != nil || sum.Failed != 2 || sum.Coverage != 0 || decide(sum) != 1 {
		t.Fatalf("all batches failed: %+v code=%d", sum, decide(sum))
	}
	if store.saveCalls != 0 {
		t.Fatal("a failed batch must write nothing")
	}
}

func TestRunJobStopsOnBlocked(t *testing.T) {
	api := &fakeAPI{names: names, items: func([]int) ([]steamtags.StoreItem, error) {
		return nil, steamtags.ErrBlocked
	}}
	store := &fakeStore{games: []db.GameRef{{GameID: "g1", ExternalAppID: "10"}}}
	sum := runJob(t.Context(), api, store, config{})
	if !sum.Blocked || sum.Err == nil || decide(sum) != 1 {
		t.Fatalf("blocked run: %+v", sum)
	}
}

func TestRunJobFailsOnTagNamesAndListErrors(t *testing.T) {
	sum := runJob(t.Context(), &fakeAPI{namesErr: steamtags.ErrBlocked}, &fakeStore{}, config{})
	if sum.Err == nil || !sum.Blocked || decide(sum) != 1 {
		t.Fatalf("tag list blocked: %+v", sum)
	}
	sum = runJob(t.Context(), &fakeAPI{namesErr: errors.New("boom")}, &fakeStore{}, config{})
	if sum.Err == nil || sum.Blocked || decide(sum) != 1 {
		t.Fatalf("tag list failed: %+v", sum)
	}
	sum = runJob(t.Context(), &fakeAPI{names: names}, &fakeStore{listErr: errors.New("db")}, config{})
	if sum.Err == nil || decide(sum) != 1 {
		t.Fatalf("list failed: %+v", sum)
	}
}

func TestRunJobSaveErrorAbortsRun(t *testing.T) {
	api := &fakeAPI{names: names, items: func([]int) ([]steamtags.StoreItem, error) {
		return []steamtags.StoreItem{tagged(10)}, nil
	}}
	store := &fakeStore{games: []db.GameRef{{GameID: "g1", ExternalAppID: "10"}}, saveErr: errors.New("write")}
	sum := runJob(t.Context(), api, store, config{})
	if sum.Err == nil || decide(sum) != 1 {
		t.Fatalf("save error must fail the run: %+v", sum)
	}
}

func TestRunJobNonNumericAppIDCountsAsFailed(t *testing.T) {
	api := &fakeAPI{names: names, items: func([]int) ([]steamtags.StoreItem, error) { return nil, nil }}
	store := &fakeStore{games: []db.GameRef{{GameID: "g1", ExternalAppID: "abc"}, {GameID: "g2", ExternalAppID: "0"}}}
	sum := runJob(t.Context(), api, store, config{})
	if sum.Failed != 2 || sum.Games != 2 {
		t.Fatalf("invalid app ids: %+v", sum)
	}
}

func TestRunJobNoGamesIsSuccess(t *testing.T) {
	sum := runJob(t.Context(), &fakeAPI{names: names}, &fakeStore{}, config{})
	if sum.Coverage != 1 || decide(sum) != 0 {
		t.Fatalf("no games: %+v", sum)
	}
}

func TestDecideCoverageThreshold(t *testing.T) {
	cases := []struct {
		games, failed, want int
	}{
		{10, 1, 0}, // 90% handled is enough
		{10, 2, 1}, // 80% is not
		{1, 0, 0},
		{1, 1, 1},
	}
	for _, tc := range cases {
		s := summary{Games: tc.games, Failed: tc.failed}
		if got := decide(s); got != tc.want {
			t.Errorf("games=%d failed=%d: exit %d, want %d", tc.games, tc.failed, got, tc.want)
		}
	}
	if decide(summary{Err: errors.New("x")}) != 1 {
		t.Fatal("an error must exit 1")
	}
}

func TestLoadConfig(t *testing.T) {
	t.Setenv("GDR_DATABASE_URL", "")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "GDR_DATABASE_URL") {
		t.Fatalf("missing DSN: %v", err)
	}
	t.Setenv("GDR_DATABASE_URL", "postgres://user:pw@host/db")
	t.Setenv("DRY_RUN", "1")
	t.Setenv("FINAL_ATTEMPT", "true")
	t.Setenv("LIMIT", "5")
	// A bad healthcheck URL must not matter: this job reads no healthcheck variables.
	t.Setenv("GDR_HEALTHCHECK_URL", "ftp://nope")
	c, err := loadConfig()
	if err != nil || !c.dryRun || !c.final || c.limit != 5 {
		t.Fatalf("config = %+v, %v", c, err)
	}
	t.Setenv("LIMIT", "-2")
	if _, err := loadConfig(); err == nil {
		t.Fatal("negative LIMIT must fail")
	}
}

func TestExecuteBadDSNWritesNothingAndHidesDSN(t *testing.T) {
	// The URL cannot be parsed, so no connection is attempted.
	cfg := config{dsn: "postgres://%zz-secret-password"}
	sum := execute(t.Context(), cfg)
	if sum.Err == nil || strings.Contains(sum.Err.Error(), "secret") {
		t.Fatalf("execute error must be generic: %v", sum.Err)
	}
}

func TestSummaryDetailAndLine(t *testing.T) {
	s := summary{Games: 4, Tagged: 2, NoTags: 1, Failed: 1, Coverage: 0.75}
	if d := s.detail(); !strings.Contains(d, "2 of 4 games tagged") || strings.HasPrefix(d, "dry run") {
		t.Fatalf("detail = %q", d)
	}
	if l := s.line(); !strings.Contains(l, "coverage_pct=75.0") || !strings.Contains(l, "dry_run=false") {
		t.Fatalf("line = %q", l)
	}
}
