package db

import (
	"strings"
	"testing"
)

func TestNormalizeRequest(t *testing.T) {
	cases := []struct {
		in, want GameRequest
	}{
		// A row from before store_id existed: Steam, external id from the app id.
		{GameRequest{SteamAppID: "620"}, GameRequest{StoreCode: "steam", ExternalID: "620", SteamAppID: "620"}},
		// A Steam row with external_id only.
		{GameRequest{StoreCode: "steam", ExternalID: "620"}, GameRequest{StoreCode: "steam", ExternalID: "620", SteamAppID: "620"}},
		// An Epic row keeps an empty Steam app id.
		{GameRequest{StoreCode: "epic", ExternalID: "alan-wake-2"}, GameRequest{StoreCode: "epic", ExternalID: "alan-wake-2"}},
	}
	for _, c := range cases {
		got := c.in
		normalizeRequest(&got)
		if got != c.want {
			t.Errorf("normalize(%+v) = %+v, want %+v", c.in, got, c.want)
		}
	}
}

func TestClaimSQLKeysSupersedeRuleOnStoreItem(t *testing.T) {
	for _, want := range []string{
		"newer.store_id IS NOT DISTINCT FROM r.store_id",
		"COALESCE(NULLIF(newer.external_id, ''), newer.steam_app_id) = COALESCE(NULLIF(r.external_id, ''), r.steam_app_id)",
		"LEFT JOIN stores s ON s.id = r.store_id",
	} {
		if !strings.Contains(claimSQL, want) {
			t.Errorf("claim query lacks %q", want)
		}
	}
}

// seedStore makes sure a stores row exists and returns its id.
func seedStore(t *testing.T, db execer, code, name, baseURL string) string {
	t.Helper()
	if _, err := db.Exec(t.Context(),
		`INSERT INTO stores (code, name, base_url, is_active) VALUES ($1, $2, $3, true) ON CONFLICT (code) DO NOTHING`,
		code, name, baseURL); err != nil {
		t.Fatal(err)
	}
	var id string
	if err := db.QueryRow(t.Context(), `SELECT id::text FROM stores WHERE code = $1`, code).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// requireStoreColumns skips unless game_requests has store_id and external_id (migration
// 000014) and a nullable steam_app_id (migration 000015).
func requireStoreColumns(t *testing.T, db execer) {
	t.Helper()
	var cols int
	var nullable string
	if err := db.QueryRow(t.Context(), `
		SELECT COUNT(*) FILTER (WHERE column_name IN ('store_id', 'external_id')),
		       COALESCE(MAX(is_nullable) FILTER (WHERE column_name = 'steam_app_id'), 'NO')
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'game_requests'`).Scan(&cols, &nullable); err != nil {
		t.Fatal(err)
	}
	if cols != 2 || nullable != "YES" {
		t.Skip("game_requests needs migrations 000014 and 000015 for this test")
	}
}

func TestClaimGameRequestsByStore(t *testing.T) {
	tx := testTx(t)
	requireStoreColumns(t, tx)
	ctx := t.Context()
	steamID := seedStore(t, tx, "steam", "Steam", "https://store.steampowered.com")
	epicID := seedStore(t, tx, "epic", "Epic Games", "https://store.epicgames.com")
	gogID := seedStore(t, tx, "gog", "GOG", "https://www.gog.com")

	steamGame, appID := seedGame(t, tx, false, 0, true)
	epicGame, _ := seedGame(t, tx, false, 0, false)
	gogGame, _ := seedGame(t, tx, false, 0, false)
	slug := "claim-test-" + appID

	insert := func(storeID, external string, steamApp any, game, status, age string) string {
		t.Helper()
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO game_requests (store_id, external_id, steam_app_id, name, game_id, status, attempts,
			                           requested_at, finished_at)
			VALUES ($1::uuid, $2, $3, 'Claim test', $4::uuid, $5, CASE WHEN $5 = 'failed' THEN 1 ELSE 0 END,
			        now() - $6::interval, CASE WHEN $5 = 'failed' THEN now() - $6::interval END)
			RETURNING id::text`,
			storeID, external, steamApp, game, status, age).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	steamReq := insert(steamID, appID, appID, steamGame, "pending", "3 hours")
	oldEpic := insert(epicID, slug, nil, epicGame, "failed", "5 hours") // superseded by the next row
	epicReq := insert(epicID, slug, nil, epicGame, "pending", "2 hours")
	gogReq := insert(gogID, "claim_test_"+appID, nil, gogGame, "pending", "1 hour")
	ids := []string{steamReq, oldEpic, epicReq, gogReq}

	// Dry run: read only.
	got, err := ClaimGameRequests(ctx, tx, 10, ids, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("claimed %d rows, want 3 (the superseded epic row is skipped): %+v", len(got), got)
	}
	want := []GameRequest{
		{ID: steamReq, StoreCode: "steam", ExternalID: appID, SteamAppID: appID, GameID: steamGame},
		{ID: epicReq, StoreCode: "epic", ExternalID: slug, SteamAppID: "", GameID: epicGame},
		{ID: gogReq, StoreCode: "gog", ExternalID: "claim_test_" + appID, SteamAppID: "", GameID: gogGame},
	}
	for i, w := range want {
		g := got[i]
		if g.ID != w.ID || g.StoreCode != w.StoreCode || g.ExternalID != w.ExternalID || g.SteamAppID != w.SteamAppID || g.GameID != w.GameID {
			t.Errorf("row %d = %+v, want %+v", i, g, w)
		}
	}
	if n := countRows(t, tx, `SELECT COUNT(*) FROM game_requests WHERE id = ANY($1::uuid[]) AND status = 'processing'`, ids); n != 0 {
		t.Fatalf("dry run changed %d rows", n)
	}

	// Real claim: the three rows go to processing with one more attempt.
	got, err = ClaimGameRequests(ctx, tx, 10, ids, false)
	if err != nil || len(got) != 3 {
		t.Fatalf("claim: %d rows, %v", len(got), err)
	}
	if n := countRows(t, tx, `SELECT COUNT(*) FROM game_requests WHERE id = ANY($1::uuid[]) AND status = 'processing' AND attempts >= 1`, ids); n != 3 {
		t.Fatalf("processing rows = %d", n)
	}
	if n := countRows(t, tx, `SELECT COUNT(*) FROM game_requests WHERE id = $1::uuid AND status = 'failed'`, oldEpic); n != 1 {
		t.Fatal("the superseded row must stay as it was")
	}

	if err := FinishGameRequest(ctx, tx, epicReq, RequestDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := ActivateGame(ctx, tx, epicGame); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, tx, `SELECT COUNT(*) FROM games WHERE id = $1::uuid AND is_active`, epicGame); n != 1 {
		t.Fatal("game not activated")
	}
}
