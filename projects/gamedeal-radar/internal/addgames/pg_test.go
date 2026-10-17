package addgames

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gamedeal-radar/daily-check/internal/steam"
	"gamedeal-radar/daily-check/internal/storedetails"
	"gamedeal-radar/daily-check/internal/storeprices"
)

func TestOneGameViewsReturnOnlyTheRequestedGame(t *testing.T) {
	o := oneGame{id: "g1", name: "Half-Life 2", appID: "220"}
	games, _ := o.ListActiveSteamGames(context.Background())
	if len(games) != 1 || games[0].ID != "g1" || games[0].ExternalAppID != "220" || games[0].LastCheckedAt != nil {
		t.Fatalf("run view = %+v", games)
	}
	usd, _ := o.ListMissingUSD(context.Background(), time.Time{})
	if len(usd) != 1 || usd[0].ExternalAppID != "220" {
		t.Fatalf("usd view = %+v", usd)
	}
	s := oneStoreGame{id: "g1", name: "Half-Life 2"}
	sg, _ := s.ActiveGames(context.Background())
	if len(sg) != 1 || sg[0] != (storeprices.Game{ID: "g1", Name: "Half-Life 2"}) {
		t.Fatalf("store view = %+v", sg)
	}
}

func TestEpicWithoutKeyIsSkippedNotFailed(t *testing.T) {
	s := &PgSteps{}
	res := s.Epic(context.Background(), Request{})
	if res.Status != StepSkipped || res.Note == "" {
		t.Fatalf("epic without key = %+v", res)
	}
}

func TestFetchNoteNeverEchoesErrorText(t *testing.T) {
	err := &steam.StatusError{Code: 403}
	if got := fetchNote(context.Background(), err); got != "steam request failed" {
		t.Fatalf("note = %q", got)
	}
}

type fakePages struct {
	d        *steam.Details
	err      error
	gotSlug  string
	gotName  string
	gotStore string
}

func (f *fakePages) EpicDetails(_ context.Context, slug string) (*steam.Details, error) {
	f.gotStore, f.gotSlug = "epic", slug
	return f.d, f.err
}

func (f *fakePages) GOGDetails(_ context.Context, slug, name string) (*steam.Details, error) {
	f.gotStore, f.gotSlug, f.gotName = "gog", slug, name
	return f.d, f.err
}

func TestStorePageStepsDryRunFetchOnly(t *testing.T) {
	p := &fakePages{d: &steam.Details{}}
	s := &PgSteps{Pages: p, DryRun: true} // nil Repo: a write would panic
	r := Request{StoreCode: StoreEpic, ExternalID: "alan-wake-2", Name: "Alan Wake 2", GameID: "g"}
	if res := s.EpicDetails(context.Background(), r); res.Status != StepOK || res.Name != "details" || p.gotSlug != "alan-wake-2" {
		t.Fatalf("epic = %+v slug %q", res, p.gotSlug)
	}
	r = Request{StoreCode: StoreGOG, ExternalID: "alan_wake", Name: "Alan Wake", GameID: "g"}
	if res := s.GOGDetails(context.Background(), r); res.Status != StepOK || p.gotStore != "gog" || p.gotName != "Alan Wake" {
		t.Fatalf("gog = %+v %+v", res, p)
	}
}

func TestStorePageStepNotes(t *testing.T) {
	cases := []struct {
		err  error
		note string
	}{
		{fmt.Errorf("epic product page: HTTP 404: %w", storedetails.ErrNotFound), "epic has no such product"},
		{storedetails.ErrInvalidID, "invalid epic id"},
		{fmt.Errorf("x: %w", storedetails.ErrBlocked), "epic refused the request"},
		{errors.New("decoding epic product page: unexpected EOF at https://secret"), "epic page could not be read"},
	}
	for _, c := range cases {
		s := &PgSteps{Pages: &fakePages{err: c.err}}
		res := s.EpicDetails(context.Background(), Request{ExternalID: "x"})
		if res.Status != StepFailed || res.Note != c.note {
			t.Errorf("%v: %+v", c.err, res)
		}
		if c.note == "epic has no such product" && !errors.Is(res.Err, storedetails.ErrNotFound) {
			t.Error("not found must stay classifiable")
		}
	}
	if res := (&PgSteps{}).EpicDetails(context.Background(), Request{}); res.Status != StepFailed {
		t.Fatalf("missing client: %+v", res)
	}
	if res := (&PgSteps{Pages: &fakePages{}}).GOGDetails(context.Background(), Request{}); res.Status != StepFailed {
		t.Fatalf("nil details must fail: %+v", res)
	}
}
