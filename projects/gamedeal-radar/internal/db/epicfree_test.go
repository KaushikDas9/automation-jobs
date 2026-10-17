package db

import (
	"context"
	"strings"
	"testing"
	"time"

	"gamedeal-radar/daily-check/internal/epicfree"
)

// These checks need no database. They pin the upsert to the one table and to the
// no-delete rule, and check the parameter mapping.
func TestEpicFreeUpsertShape(t *testing.T) {
	if !strings.Contains(upsertEpicFreeSQL, "INSERT INTO epic_free_games") {
		t.Fatal("the upsert must write epic_free_games")
	}
	if !strings.Contains(upsertEpicFreeSQL, "ON CONFLICT (epic_id) DO UPDATE SET") {
		t.Fatal("the upsert must conflict on epic_id")
	}
	for _, col := range []string{"title", "image_url", "store_url", "regular_price", "currency", "starts_at", "ends_at", "fetched_at"} {
		if !strings.Contains(upsertEpicFreeSQL, col+" ") && !strings.Contains(upsertEpicFreeSQL, col+"=") {
			t.Errorf("the conflict branch must refresh %s", col)
		}
	}
	if !strings.Contains(upsertEpicFreeSQL, "fetched_at    = NOW()") {
		t.Fatal("fetched_at must be set to NOW() on update")
	}
	upper := strings.ToUpper(upsertEpicFreeSQL)
	for _, bad := range []string{"DELETE", "TRUNCATE", "DROP ", "ALTER "} {
		if strings.Contains(upper, bad) {
			t.Errorf("the epic free upsert must not contain %s", bad)
		}
	}
}

func TestEpicFreeArgsOrderAndNulls(t *testing.T) {
	start := time.Date(2026, 10, 15, 15, 0, 0, 0, time.FixedZone("x", 3600))
	end := start.Add(7 * 24 * time.Hour)
	img := "https://cdn.example-store.test/a.jpg"
	price := "1499.00"
	args := epicFreeArgs(epicfree.Game{
		EpicID: "id-1", Title: "Title", ImageURL: &img, StoreURL: "https://store.epicgames.com/en-US/p/a",
		RegularPrice: &price, Currency: "INR", StartsAt: start, EndsAt: end,
	})
	if len(args) != 8 || args[0] != "id-1" || args[1] != "Title" || args[5] != "INR" {
		t.Fatalf("args = %v", args)
	}
	if args[2] != img || args[4] != price {
		t.Fatalf("present image and price must be passed through, got %v and %v", args[2], args[4])
	}
	if s, ok := args[6].(time.Time); !ok || s.Location() != time.UTC || !s.Equal(start) {
		t.Fatalf("starts_at must be UTC: %v", args[6])
	}

	nilArgs := epicFreeArgs(epicfree.Game{EpicID: "id-2", Title: "T", StoreURL: "u", Currency: "INR", StartsAt: start, EndsAt: end})
	if nilArgs[2] != nil || nilArgs[4] != nil {
		t.Fatalf("missing image and price must be nil, got %v and %v", nilArgs[2], nilArgs[4])
	}
}

func TestSaveEpicFreeGamesRefusesEmptyList(t *testing.T) {
	// A nil connection is safe here: the empty list is refused before any use of it.
	err := SaveEpicFreeGames(context.Background(), nil, nil)
	if err == nil || !strings.Contains(err.Error(), "refusing") {
		t.Fatalf("empty list must be refused, got %v", err)
	}
}
