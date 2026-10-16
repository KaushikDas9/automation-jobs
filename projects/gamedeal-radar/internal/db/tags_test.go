package db

import (
	"strings"
	"testing"

	"gamedeal-radar/daily-check/internal/steamtags"
)

// These checks need no database. They pin the statements to the one column the tags job
// may write, and to the read-only shape of the game list.
func TestUpdateTagsSQLWritesOnlyTheTagsColumn(t *testing.T) {
	const want = "UPDATE game_details SET tags = $2::jsonb WHERE game_id = $1;"
	if updateTagsSQL != want {
		t.Fatalf("update statement = %q", updateTagsSQL)
	}
	upper := strings.ToUpper(updateTagsSQL)
	for _, bad := range []string{"DELETE", "INSERT", "TRUNCATE"} {
		if strings.Contains(upper, bad) {
			t.Errorf("tags update must not contain %s", bad)
		}
	}
}

func TestListGamesForTagsSQLIsReadOnly(t *testing.T) {
	upper := strings.ToUpper(listGamesForTagsSQL)
	for _, bad := range []string{"INSERT", "UPDATE ", "DELETE", "TRUNCATE"} {
		if strings.Contains(upper, bad) {
			t.Errorf("game list must be read-only, contains %s", bad)
		}
	}
	for _, want := range []string{"g.is_active = true", "JOIN game_details d", "'^[0-9]+$'", "LIMIT NULLIF($2::int, 0)"} {
		if !strings.Contains(listGamesForTagsSQL, want) {
			t.Errorf("game list is missing %q", want)
		}
	}
}

func TestEncodeTagsShape(t *testing.T) {
	got, err := encodeTags([]steamtags.Tag{{ID: "1695", Description: "Open World"}})
	if err != nil {
		t.Fatal(err)
	}
	if got != `[{"id":"1695","description":"Open World"}]` {
		t.Fatalf("encoded = %s", got)
	}
	if got, _ := encodeTags(nil); got != "[]" {
		t.Fatalf("nil list = %s, want []", got)
	}
}

func TestSaveTagsEmptyListWritesNothing(t *testing.T) {
	// A nil execer would panic if the function reached the database.
	if err := saveTags(t.Context(), nil, "game", nil); err != nil {
		t.Fatalf("empty list must be a no-op: %v", err)
	}
}
