package addgames

import (
	"context"
	"testing"
	"time"

	"gamedeal-radar/daily-check/internal/steam"
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
