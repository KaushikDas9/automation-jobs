package db

import (
	"strings"
	"testing"
)

// The manual-row guard is the only thing that keeps the jobs away from owner-typed prices.
// These tests fail if the guard or the forbidden statements ever change.
func TestUpsertStorePriceSQLKeepsManualRows(t *testing.T) {
	if !strings.Contains(upsertStorePriceSQL, "WHERE game_store_prices.source <> 'manual'") {
		t.Fatal("the DO UPDATE branch must carry the manual guard")
	}
	if !strings.Contains(upsertStorePriceSQL, "ON CONFLICT (game_id, store_code, region) DO UPDATE") {
		t.Fatal("the upsert must conflict on the primary key")
	}
	if !strings.Contains(upsertStorePriceSQL, "'auto'") {
		t.Fatal("automated writes must set source to auto")
	}
	if strings.Contains(strings.ToUpper(upsertStorePriceSQL), "DELETE") {
		t.Fatal("the store price upsert must never delete")
	}
}

func TestStorePriceReadsOnlyTouchTheirStore(t *testing.T) {
	for name, q := range map[string]string{
		"storedCents":  storedCentsSQL,
		"manualGames":  manualGameIDsSQL,
		"checkedCount": checkedCountSQL,
	} {
		upper := strings.ToUpper(q)
		for _, bad := range []string{"INSERT", "UPDATE ", "DELETE", "TRUNCATE"} {
			if strings.Contains(upper, bad) {
				t.Errorf("%s must be read-only, contains %s", name, bad)
			}
		}
	}
	if !strings.Contains(manualGameIDsSQL, "source = 'manual'") {
		t.Fatal("manual lookup must filter on source = 'manual'")
	}
}

func TestFXUpsertTouchesOnlyFXRates(t *testing.T) {
	if !strings.Contains(upsertFXRateSQL, "INSERT INTO fx_rates") || strings.Contains(upsertFXRateSQL, "DELETE") {
		t.Fatal("fx upsert must write fx_rates only")
	}
	if USDINRPair != "USD_INR" {
		t.Fatalf("pair = %q", USDINRPair)
	}
}
