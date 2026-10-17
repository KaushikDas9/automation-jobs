package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"gamedeal-radar/daily-check/internal/steam"
)

// The tests need a real database. They run only when GDR_TEST_DATABASE_URL is set, and
// only against localhost. Each test works inside a transaction that is rolled back, except
// the wrapper test, which deletes the game it created.

var testSeq atomic.Int64

func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("GDR_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("GDR_TEST_DATABASE_URL not set; skipping database tests")
	}
	u, err := url.Parse(dsn)
	if err != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		t.Fatal("GDR_TEST_DATABASE_URL must be a postgres:// URL")
	}
	for _, host := range []string{u.Hostname(), u.Query().Get("host")} {
		if host != "" && host != "localhost" && host != "127.0.0.1" {
			t.Fatalf("refusing to run database tests against non-local host %q", host)
		}
	}
	if u.Hostname() == "" {
		t.Fatal("refusing to run database tests without an explicit localhost host")
	}
	return dsn
}

func testConn(t *testing.T) *pgx.Conn {
	t.Helper()
	conn, err := Connect(t.Context(), testDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// testTx returns a transaction that is rolled back when the test ends.
func testTx(t *testing.T) pgx.Tx {
	t.Helper()
	tx, err := testConn(t).Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

// seedGame inserts a game, and when withSteam is true a Steam mapping to it. The
// steam stores row is created only when it is missing. Returns the game id and app id.
func seedGame(t *testing.T, db execer, active bool, priority int, withSteam bool) (string, string) {
	t.Helper()
	ctx := t.Context()
	n := testSeq.Add(1)
	appID := strconv.FormatInt(time.Now().UnixNano()+n, 10)
	slug := fmt.Sprintf("details-test-%s", appID)

	if _, err := db.Exec(ctx, `
		INSERT INTO stores (code, name, base_url, is_active)
		VALUES ('steam', 'Steam', 'https://store.steampowered.com', true)
		ON CONFLICT (code) DO NOTHING;`); err != nil {
		t.Fatal(err)
	}
	var storeID, gameID string
	if err := db.QueryRow(ctx, `SELECT id::text FROM stores WHERE code = 'steam';`).Scan(&storeID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(ctx,
		`INSERT INTO games (name, slug, is_active, priority) VALUES ($1, $2, $3, $4) RETURNING id::text;`,
		"Details Test "+appID, slug, active, priority,
	).Scan(&gameID); err != nil {
		t.Fatal(err)
	}
	if withSteam {
		if _, err := db.Exec(ctx, `
			INSERT INTO game_store_mappings (game_id, store_id, external_app_id, store_url)
			VALUES ($1, $2, $3, $4);`,
			gameID, storeID, appID, "https://store.steampowered.com/app/"+appID,
		); err != nil {
			t.Fatal(err)
		}
	}
	return gameID, appID
}

func countRows(t *testing.T, db execer, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func fullDetails() *steam.Details {
	release := time.Date(2020, 12, 9, 0, 0, 0, 0, time.UTC)
	score := 86
	return &steam.Details{
		ExternalAppID:      "1091500",
		ShortDescription:   "Cyberpunk short",
		ReleaseDateText:    "9 Dec, 2020",
		ReleaseDate:        &release,
		Developers:         []string{"CD PROJEKT RED"},
		Publishers:         []string{"CD PROJEKT RED", "Other"},
		Genres:             []steam.Tag{{ID: "3", Description: "RPG"}},
		Categories:         []steam.Tag{{ID: "2", Description: "Single-player"}},
		MetacriticScore:    &score,
		MetacriticURL:      "https://www.metacritic.com/game/cyberpunk-2077",
		AgeRatings:         []steam.AgeRating{{Board: "esrb", Rating: "m", Descriptors: "Blood and Gore, Intense Violence"}},
		Platforms:          steam.Platforms{Windows: true, Mac: false, Linux: false},
		Screenshots:        []steam.Screenshot{{Thumb: "https://cdn.steamstatic.com/a.600x338.jpg", Full: "https://cdn.steamstatic.com/a.1920x1080.jpg"}},
		Movies:             []steam.Movie{{Name: "Trailer", Thumb: "https://cdn.steamstatic.com/t.jpg", HLS: "https://cdn.steamstatic.com/t.m3u8", DASH: ""}},
		PCReqMinHTML:       "<ul><li>OS: Windows 10</li></ul>",
		PCReqRecHTML:       "<ul><li>OS: Windows 11</li></ul>",
		SupportedLanguages: "English",
		Website:            "https://www.cyberpunk.net",
	}
}

func fullReviews() *steam.Reviews {
	return &steam.Reviews{Summary: "Very Positive", Positive: 778181, Total: 889000}
}

// jsonColumn reads a jsonb column as raw bytes so the stored text can be compared.
func jsonColumn(t *testing.T, db execer, gameID, column string) string {
	t.Helper()
	var raw string
	q := fmt.Sprintf(`SELECT %s::text FROM game_details WHERE game_id = $1;`, column)
	if err := db.QueryRow(t.Context(), q, gameID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestUpsertGameDetailsInsertThenUpdate(t *testing.T) {
	tx := testTx(t)
	ctx := t.Context()
	gameID, _ := seedGame(t, tx, true, 0, true)

	if err := upsertGameDetails(ctx, tx, gameID, fullDetails(), fullReviews()); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	var (
		shortDesc, releaseText, metaURL, website, pcMin, pcRec, langs, summary string
		releaseDate                                                            string
		metaScore, positive, total                                             int
		coming                                                                 bool
		devs                                                                   []string
		fetched1                                                               time.Time
	)
	if err := tx.QueryRow(ctx, `
		SELECT short_description, release_date_text, release_date::text, metacritic_score,
		       metacritic_url, website, pc_req_min_html, pc_req_rec_html, supported_languages_html,
		       review_summary, review_positive, review_total, coming_soon, developers, fetched_at
		FROM game_details WHERE game_id = $1;`, gameID,
	).Scan(&shortDesc, &releaseText, &releaseDate, &metaScore, &metaURL, &website, &pcMin, &pcRec,
		&langs, &summary, &positive, &total, &coming, &devs, &fetched1); err != nil {
		t.Fatal(err)
	}
	if shortDesc != "Cyberpunk short" || releaseText != "9 Dec, 2020" || releaseDate != "2020-12-09" {
		t.Fatalf("unexpected text/date fields: %q %q %q", shortDesc, releaseText, releaseDate)
	}
	if metaScore != 86 || metaURL == "" || website == "" || pcMin == "" || pcRec == "" || langs == "" {
		t.Fatalf("unexpected metacritic/html fields")
	}
	if summary != "Very Positive" || positive != 778181 || total != 889000 {
		t.Fatalf("unexpected review fields: %q %d %d", summary, positive, total)
	}
	if coming || !reflect.DeepEqual(devs, []string{"CD PROJEKT RED"}) {
		t.Fatalf("unexpected coming_soon/developers: %v %v", coming, devs)
	}
	if got := jsonColumn(t, tx, gameID, "genres"); got != `[{"id": "3", "description": "RPG"}]` {
		t.Fatalf("genres = %s", got)
	}
	if got := jsonColumn(t, tx, gameID, "platforms"); got != `{"mac": false, "linux": false, "windows": true}` {
		t.Fatalf("platforms = %s", got)
	}

	// Force fetched_at into the past so the second upsert has a visible change to make.
	if _, err := tx.Exec(ctx, `UPDATE game_details SET fetched_at = NOW() - INTERVAL '10 days' WHERE game_id = $1;`, gameID); err != nil {
		t.Fatal(err)
	}
	var past time.Time
	if err := tx.QueryRow(ctx, `SELECT fetched_at FROM game_details WHERE game_id = $1;`, gameID).Scan(&past); err != nil {
		t.Fatal(err)
	}

	updated := fullDetails()
	updated.ShortDescription = "Changed"
	updated.Developers = []string{"New Dev"}
	updated.Platforms = steam.Platforms{Windows: true, Mac: true, Linux: true}
	if err := upsertGameDetails(ctx, tx, gameID, updated, &steam.Reviews{Summary: "Mixed", Positive: 1, Total: 2}); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	var count int
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM game_details WHERE game_id = $1;`, gameID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one game_details row, got %d", count)
	}
	var fetched2 time.Time
	var short, sum string
	if err := tx.QueryRow(ctx, `SELECT fetched_at, short_description, review_summary FROM game_details WHERE game_id = $1;`, gameID).
		Scan(&fetched2, &short, &sum); err != nil {
		t.Fatal(err)
	}
	if !fetched2.After(past) {
		t.Fatalf("fetched_at not refreshed: before=%v after=%v", past, fetched2)
	}
	if short != "Changed" || sum != "Mixed" {
		t.Fatalf("fields not updated: short=%q summary=%q", short, sum)
	}
	if got := jsonColumn(t, tx, gameID, "platforms"); got != `{"mac": true, "linux": true, "windows": true}` {
		t.Fatalf("platforms not updated: %s", got)
	}
}

func TestUpsertGameDetailsEmptyCollectionsAndNulls(t *testing.T) {
	tx := testTx(t)
	ctx := t.Context()
	gameID, _ := seedGame(t, tx, true, 0, true)

	// Nil slices, empty strings, nil metacritic and nil reviews.
	d := &steam.Details{ShortDescription: "Free game", Platforms: steam.Platforms{Windows: true}}
	if err := upsertGameDetails(ctx, tx, gameID, d, nil); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	for _, col := range []string{"genres", "categories", "age_ratings", "screenshots", "movies"} {
		if got := jsonColumn(t, tx, gameID, col); got != "[]" {
			t.Errorf("%s = %s, want []", col, got)
		}
	}
	var devs, pubs int
	if err := tx.QueryRow(ctx, `SELECT cardinality(developers), cardinality(publishers) FROM game_details WHERE game_id = $1;`, gameID).
		Scan(&devs, &pubs); err != nil {
		t.Fatal(err)
	}
	if devs != 0 || pubs != 0 {
		t.Fatalf("developers/publishers should be empty arrays, got %d %d", devs, pubs)
	}

	var nulls struct {
		releaseDate, metaScore, metaURL, pcMin, pcRec, langs, website, summary, positive, total *string
	}
	if err := tx.QueryRow(ctx, `
		SELECT release_date::text, metacritic_score::text, metacritic_url, pc_req_min_html, pc_req_rec_html,
		       supported_languages_html, website, review_summary, review_positive::text, review_total::text
		FROM game_details WHERE game_id = $1;`, gameID,
	).Scan(&nulls.releaseDate, &nulls.metaScore, &nulls.metaURL, &nulls.pcMin, &nulls.pcRec,
		&nulls.langs, &nulls.website, &nulls.summary, &nulls.positive, &nulls.total); err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]*string{
		"release_date": nulls.releaseDate, "metacritic_score": nulls.metaScore, "metacritic_url": nulls.metaURL,
		"pc_req_min_html": nulls.pcMin, "pc_req_rec_html": nulls.pcRec, "supported_languages_html": nulls.langs,
		"website": nulls.website, "review_summary": nulls.summary, "review_positive": nulls.positive,
		"review_total": nulls.total,
	} {
		if v != nil {
			t.Errorf("%s = %q, want NULL", name, *v)
		}
	}
}

func TestUpsertGameDetailsRejectsNilDetails(t *testing.T) {
	tx := testTx(t)
	gameID, _ := seedGame(t, tx, true, 0, true)
	if err := upsertGameDetails(t.Context(), tx, gameID, nil, nil); err == nil {
		t.Fatal("expected an error for nil details")
	}
}

func TestSaveUSDPriceSnapshotAndCurrentUpsert(t *testing.T) {
	tx := testTx(t)
	ctx := t.Context()
	gameID, _ := seedGame(t, tx, true, 0, true)
	at1 := time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)
	at2 := at1.Add(24 * time.Hour)

	first := steam.BatchPrice{Currency: "USD", Base: 59.99, Current: 17.99, DiscountPct: 70}
	second := steam.BatchPrice{Currency: "USD", Base: 59.99, Current: 59.99, DiscountPct: 0}
	if err := saveUSDPrice(ctx, tx, gameID, first, at1); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if err := saveUSDPrice(ctx, tx, gameID, second, at2); err != nil {
		t.Fatalf("second save: %v", err)
	}

	if n := countRows(t, tx, `SELECT COUNT(*) FROM game_usd_snapshots WHERE game_id = $1;`, gameID); n != 2 {
		t.Fatalf("snapshots = %d, want 2", n)
	}
	if n := countRows(t, tx, `SELECT COUNT(*) FROM game_usd_snapshots WHERE game_id = $1 AND is_deal;`, gameID); n != 1 {
		t.Fatalf("deal snapshots = %d, want 1", n)
	}
	if n := countRows(t, tx, `SELECT COUNT(*) FROM game_current_price_usd WHERE game_id = $1;`, gameID); n != 1 {
		t.Fatalf("current rows = %d, want 1", n)
	}

	var base, current string
	var discount int
	var isDeal bool
	var checked time.Time
	if err := tx.QueryRow(ctx, `
		SELECT base_price::text, current_price::text, discount_pct, is_deal, checked_at
		FROM game_current_price_usd WHERE game_id = $1;`, gameID,
	).Scan(&base, &current, &discount, &isDeal, &checked); err != nil {
		t.Fatal(err)
	}
	if base != "59.99" || current != "59.99" || discount != 0 || isDeal || !checked.Equal(at2) {
		t.Fatalf("current price = %s %s %d %v %v, want second save", base, current, discount, isDeal, checked)
	}
}

func TestSaveUSDPriceUnavailableWritesNothing(t *testing.T) {
	tx := testTx(t)
	gameID, _ := seedGame(t, tx, true, 0, true)
	p := steam.BatchPrice{Currency: "USD", Unavailable: true}
	if err := saveUSDPrice(t.Context(), tx, gameID, p, time.Now()); err != nil {
		t.Fatalf("unavailable price should not error: %v", err)
	}
	if n := countRows(t, tx, `SELECT COUNT(*) FROM game_usd_snapshots WHERE game_id = $1;`, gameID); n != 0 {
		t.Fatalf("snapshots = %d, want 0", n)
	}
	if n := countRows(t, tx, `SELECT COUNT(*) FROM game_current_price_usd WHERE game_id = $1;`, gameID); n != 0 {
		t.Fatalf("current rows = %d, want 0", n)
	}
}

func TestListGamesNeedingDetailsOrderAndStaleFilter(t *testing.T) {
	tx := testTx(t)
	ctx := t.Context()

	// Candidates and exclusions. Only the games seeded here are asserted on, since the
	// scratch database may hold other rows.
	noDetailsLow, _ := seedGame(t, tx, true, 1, true)  // A: never fetched, priority 1
	noDetailsHigh, _ := seedGame(t, tx, true, 5, true) // B: never fetched, priority 5
	fresh, _ := seedGame(t, tx, true, 9, true)         // C: fetched now, excluded
	stale, _ := seedGame(t, tx, true, 0, true)         // D: fetched 10 days ago
	inactive, _ := seedGame(t, tx, false, 9, true)     // E: inactive, excluded
	noSteam, _ := seedGame(t, tx, true, 9, false)      // F: no Steam mapping, excluded

	for _, id := range []string{fresh, stale} {
		if err := upsertGameDetails(ctx, tx, id, fullDetails(), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE game_details SET fetched_at = NOW() - INTERVAL '10 days' WHERE game_id = $1;`, stale); err != nil {
		t.Fatal(err)
	}

	cutoff := time.Now().Add(-24 * time.Hour)
	got, err := listGamesNeedingDetails(ctx, tx, cutoff, 1000)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	mine := map[string]bool{noDetailsLow: true, noDetailsHigh: true, fresh: true, stale: true, inactive: true, noSteam: true}
	var ours []string
	for _, g := range got {
		if mine[g.GameID] {
			ours = append(ours, g.GameID)
		}
	}
	want := []string{noDetailsHigh, noDetailsLow, stale}
	if !reflect.DeepEqual(ours, want) {
		t.Fatalf("ordering/filter mismatch\n got: %v\nwant: %v", ours, want)
	}
	for _, g := range got {
		if g.GameID == noDetailsHigh && g.ExternalAppID == "" {
			t.Fatal("ExternalAppID not returned")
		}
	}

	limited, err := listGamesNeedingDetails(ctx, tx, cutoff, 1)
	if err != nil {
		t.Fatalf("list with limit: %v", err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit 1 returned %d rows", len(limited))
	}
}

// TestRepoWrappersUseConnection runs the exported Repo methods on a real connection
// (no test transaction). The game row is deleted at the end, which cascades to the
// details and price rows.
func TestRepoWrappersUseConnection(t *testing.T) {
	conn := testConn(t)
	ctx := t.Context()
	gameID, _ := seedGame(t, conn, true, 0, true)
	t.Cleanup(func() {
		if _, err := conn.Exec(context.Background(), `DELETE FROM games WHERE id = $1;`, gameID); err != nil {
			t.Errorf("cleaning up game %s: %v", gameID, err)
		}
	})

	repo := NewRepo(conn, "")
	if err := repo.UpsertGameDetails(ctx, gameID, fullDetails(), fullReviews()); err != nil {
		t.Fatalf("UpsertGameDetails: %v", err)
	}
	if err := repo.SaveUSDPrice(ctx, gameID, steam.BatchPrice{Currency: "USD", Base: 10, Current: 5, DiscountPct: 50}, time.Now()); err != nil {
		t.Fatalf("SaveUSDPrice: %v", err)
	}
	if n := countRows(t, conn, `SELECT COUNT(*) FROM game_current_price_usd WHERE game_id = $1;`, gameID); n != 1 {
		t.Fatalf("current rows = %d, want 1", n)
	}

	refs, err := repo.ListGamesNeedingDetails(ctx, time.Now().Add(-time.Hour), 1000)
	if err != nil {
		t.Fatalf("ListGamesNeedingDetails: %v", err)
	}
	for _, g := range refs {
		if g.GameID == gameID {
			t.Fatal("game with fresh details should not need details")
		}
	}
}

func TestListGamesNeedingDetailsOncePerGameWithTwoMappings(t *testing.T) {
	tx := testTx(t)
	ctx := t.Context()

	// Migration 000014 allows one mapping per (game, store), so the case cannot happen there.
	if countRows(t, tx, `SELECT COUNT(*) FROM pg_constraint WHERE conname = 'uq_game_store_mappings_game_store'`) > 0 {
		t.Skip("one mapping per game and store is enforced by the schema (migration 000014)")
	}
	gameID, _ := seedGame(t, tx, true, 1, true)
	// A second Steam mapping for the same game must not make the game appear twice.
	if _, err := tx.Exec(ctx, `
		INSERT INTO game_store_mappings (game_id, store_id, external_app_id, store_url)
		SELECT $1, s.id, 'second-mapping-test', 'https://store.steampowered.com/app/2/'
		FROM stores s WHERE s.code = 'steam';`, gameID); err != nil {
		t.Fatal(err)
	}

	got, err := listGamesNeedingDetails(ctx, tx, time.Now().Add(-time.Hour), 1000)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, g := range got {
		if g.GameID == gameID {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("game listed %d times, want exactly once", n)
	}
}

// platformReqColumns reads the four Mac and Linux requirement columns, NULL as nil.
func platformReqColumns(t *testing.T, db execer, gameID string) [4]*string {
	t.Helper()
	var cols [4]*string
	if err := db.QueryRow(t.Context(), `
		SELECT mac_req_min_html, mac_req_rec_html, linux_req_min_html, linux_req_rec_html
		FROM game_details WHERE game_id = $1;`, gameID,
	).Scan(&cols[0], &cols[1], &cols[2], &cols[3]); err != nil {
		t.Fatal(err)
	}
	return cols
}

func TestUpsertGameDetailsPlatformRequirements(t *testing.T) {
	tx := testTx(t)
	ctx := t.Context()
	gameID, _ := seedGame(t, tx, true, 0, true)

	first := fullDetails()
	first.MacReqMinHTML = "<ul class=\"bb_ul\"><li>Apple M1</li></ul>"
	first.MacReqRecHTML = "<ul class=\"bb_ul\"><li>16 GB RAM</li></ul>"
	first.LinuxReqMinHTML = "<ul class=\"bb_ul\"><li>Ubuntu 20.04</li></ul>"
	first.LinuxReqRecHTML = "<ul class=\"bb_ul\"><li>Ubuntu 22.04</li></ul>"
	if err := upsertGameDetails(ctx, tx, gameID, first, nil); err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	want := [4]string{first.MacReqMinHTML, first.MacReqRecHTML, first.LinuxReqMinHTML, first.LinuxReqRecHTML}
	got := platformReqColumns(t, tx, gameID)
	for i := range got {
		if got[i] == nil || *got[i] != want[i] {
			t.Fatalf("column %d = %v, want %q", i, got[i], want[i])
		}
	}

	// A second upsert replaces the values, and empty strings become NULL.
	second := fullDetails()
	second.MacReqMinHTML = "<ul class=\"bb_ul\"><li>Apple M2</li></ul>"
	second.MacReqRecHTML = ""
	second.LinuxReqMinHTML = ""
	second.LinuxReqRecHTML = ""
	if err := upsertGameDetails(ctx, tx, gameID, second, nil); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got = platformReqColumns(t, tx, gameID)
	if got[0] == nil || *got[0] != second.MacReqMinHTML {
		t.Errorf("mac_req_min_html = %v, want updated value", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i] != nil {
			t.Errorf("column %d = %q, want NULL after empty upsert", i, *got[i])
		}
	}
}
