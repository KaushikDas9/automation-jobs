package storeprices

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func loadFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseGOGCatalogExactTitleOnly(t *testing.T) {
	body := loadFixture(t, "gog_catalog.json")

	// The exact title matches, and the "Ultimate Edition" product is not picked.
	p, err := ParseGOGCatalog(body, "Cyberpunk 2077", RegionUS)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusOK || p.Currency != "USD" || p.StoreURL != "https://www.gog.com/en/game/cyberpunk_2077" {
		t.Fatalf("row = %+v", p)
	}
	if FormatCents(*p.CurrentCents) != "10.55" || FormatCents(*p.BaseCents) != "35.20" {
		t.Fatalf("prices = %s / %s", FormatCents(*p.CurrentCents), FormatCents(*p.BaseCents))
	}
	if p.DiscountPct != 70 {
		t.Fatalf("discount = %d, want 70 (computed, not from a label)", p.DiscountPct)
	}

	// Nothing similar enough: not_listed, never a fuzzy match to the Ultimate Edition.
	p, err = ParseGOGCatalog(body, "Cyberpunk", RegionIN)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != StatusNotListed || p.Region != RegionIN || p.StoreCode != StoreGOG {
		t.Fatalf("no match must be not_listed: %+v", p)
	}
}

func TestParseGOGCatalogFreeAndListedNoPrice(t *testing.T) {
	body := loadFixture(t, "gog_catalog.json")

	free, err := ParseGOGCatalog(body, "Free Test Game", RegionUS)
	if err != nil {
		t.Fatal(err)
	}
	if free.Status != StatusFree || *free.CurrentCents != 0 || free.DiscountPct != 0 {
		t.Fatalf("free row = %+v", free)
	}

	noPrice, err := ParseGOGCatalog(body, "No Price Game", RegionUS)
	if err != nil {
		t.Fatal(err)
	}
	if noPrice.Status != StatusListedNoPrice || noPrice.CurrentCents != nil || noPrice.StoreURL == "" {
		t.Fatalf("listed_no_price row = %+v", noPrice)
	}
}

func TestParseGOGCatalogRejectsBadJSONAndBadAmount(t *testing.T) {
	if _, err := ParseGOGCatalog([]byte("{not json"), "x", RegionUS); err == nil {
		t.Fatal("bad json must be an error")
	}
	bad := []byte(`{"products":[{"title":"Bad Amount","price":{"finalMoney":{"amount":"1.234","currency":"USD"}}}]}`)
	if _, err := ParseGOGCatalog(bad, "Bad Amount", RegionUS); err == nil {
		t.Fatal("a matched product with a malformed amount must be an error")
	}
}

func TestGOGCatalogURL(t *testing.T) {
	got := GOGCatalogURL("", "Half-Life 2", "IN")
	if !strings.HasPrefix(got, "https://catalog.gog.com/v1/catalog?") {
		t.Fatalf("base = %q", got)
	}
	if !strings.Contains(got, "query=like%3AHalf-Life%202") {
		t.Fatalf("query not escaped as expected: %q", got)
	}
	if !strings.Contains(got, "countryCode=IN") || !strings.Contains(got, "currencyCode=USD") {
		t.Fatalf("missing country or currency: %q", got)
	}
}

func TestGOGClientStatuses(t *testing.T) {
	body := loadFixture(t, "gog_catalog.json")
	var gotUA, gotCountry string
	statuses := []int{http.StatusOK, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusNotFound, http.StatusForbidden}
	i := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		gotCountry = r.URL.Query().Get("countryCode")
		w.WriteHeader(statuses[i])
		if statuses[i] == http.StatusOK {
			_, _ = w.Write(body)
		}
		i++
	}))
	defer srv.Close()

	client := &GOGClient{Fetcher: NewFetcher(0), BaseURL: srv.URL}

	p, err := client.Price(context.Background(), "Cyberpunk 2077", RegionUS)
	if err != nil || p.Status != StatusOK {
		t.Fatalf("200: %+v, %v", p, err)
	}
	if gotUA != "GameDealRadar-jobs/1.0" || gotCountry != "US" {
		t.Fatalf("request: ua=%q country=%q", gotUA, gotCountry)
	}

	if _, err := client.Price(context.Background(), "x", RegionUS); !IsBlocked(err) {
		t.Fatalf("429 must block: %v", err)
	}
	if _, err := client.Price(context.Background(), "x", RegionUS); !IsTransient(err) {
		t.Fatalf("500 must be transient: %v", err)
	}
	if _, err := client.Price(context.Background(), "x", RegionUS); err == nil || IsTransient(err) || IsBlocked(err) {
		t.Fatalf("404 must be permanent: %v", err)
	}
	if _, err := client.Price(context.Background(), "x", RegionUS); !errors.Is(err, ErrBlocked) {
		t.Fatalf("403 must block: %v", err)
	}
}

// fakeGOG returns scripted results per (game, region) and counts calls.
type fakeGOG struct {
	results map[string][]error // key "name|region": errors to return in order, nil = use price
	prices  map[string]Price
	calls   int
}

func (f *fakeGOG) Price(_ context.Context, name, region string) (Price, error) {
	f.calls++
	key := name + "|" + region
	if q := f.results[key]; len(q) > 0 {
		err := q[0]
		f.results[key] = q[1:]
		if err != nil {
			return Price{}, err
		}
	}
	if p, ok := f.prices[key]; ok {
		return p, nil
	}
	return NotListed(StoreGOG, region), nil
}

// fakeRepo is an in-memory Repo that records every write.
type fakeRepo struct {
	games     []Game
	stored    map[StoredKey]int64
	manual    map[string]bool
	checked   int
	saved     map[string][]Price
	saveErr   error
	saveCalls int
}

func (r *fakeRepo) ActiveGames(context.Context) ([]Game, error) { return r.games, nil }
func (r *fakeRepo) StoredCents(context.Context, string) (map[StoredKey]int64, error) {
	if r.stored == nil {
		return map[StoredKey]int64{}, nil
	}
	return r.stored, nil
}
func (r *fakeRepo) ManualGameIDs(context.Context, string) (map[string]bool, error) {
	if r.manual == nil {
		return map[string]bool{}, nil
	}
	return r.manual, nil
}
func (r *fakeRepo) CheckedCount(context.Context, string, time.Time) (int, error) {
	return r.checked, nil
}
func (r *fakeRepo) SaveGameRows(_ context.Context, store, gameID string, rows []Price) (SaveResult, error) {
	r.saveCalls++
	if r.saveErr != nil {
		return SaveResult{}, r.saveErr
	}
	if r.saved == nil {
		r.saved = map[string][]Price{}
	}
	r.saved[gameID] = append(r.saved[gameID], rows...)
	return SaveResult{Written: len(rows)}, nil
}

func cents(v int64) *int64 { return &v }

func okRow(region string, cur, base int64) Price {
	return Price{StoreCode: StoreGOG, Region: region, Currency: "USD", Status: StatusOK,
		CurrentCents: cents(cur), BaseCents: cents(base), DiscountPct: DiscountPercent(base, cur)}
}

func TestRunGOGWritesBothRegionsOncePerGame(t *testing.T) {
	repo := &fakeRepo{games: []Game{{ID: "g1", Name: "Alpha"}, {ID: "g2", Name: "Beta"}}}
	api := &fakeGOG{prices: map[string]Price{
		"Alpha|US": okRow("US", 1000, 2000),
		"Alpha|IN": okRow("IN", 900, 2000),
	}}
	sum, err := RunGOG(context.Background(), repo, api, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(repo.saved["g1"]); got != 2 {
		t.Fatalf("game g1 rows = %d, want 2 (both regions)", got)
	}
	if repo.saveCalls != 2 {
		t.Fatalf("one transaction per game expected, got %d", repo.saveCalls)
	}
	// Beta has no match in either region: not_listed rows are written, so it is covered.
	if len(repo.saved["g2"]) != 2 || repo.saved["g2"][0].Status != StatusNotListed {
		t.Fatalf("g2 rows = %+v", repo.saved["g2"])
	}
	if sum.Failed != 0 || sum.Coverage != 1 || sum.NotListed[RegionUS] != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestRunGOGSanitySkipKeepsStoredRow(t *testing.T) {
	repo := &fakeRepo{
		games:  []Game{{ID: "g1", Name: "Alpha"}},
		stored: map[StoredKey]int64{{GameID: "g1", Region: RegionUS}: 1000},
	}
	api := &fakeGOG{prices: map[string]Price{
		"Alpha|US": okRow("US", 5000, 6000), // 400% jump: skipped
		"Alpha|IN": okRow("IN", 900, 2000),
	}}
	sum, err := RunGOG(context.Background(), repo, api, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.SkippedSanity != 1 {
		t.Fatalf("SkippedSanity = %d, want 1", sum.SkippedSanity)
	}
	rows := repo.saved["g1"]
	if len(rows) != 1 || rows[0].Region != RegionIN {
		t.Fatalf("only the IN row must be written, got %+v", rows)
	}
}

func TestRunGOGTransientRetriedThenWritten(t *testing.T) {
	repo := &fakeRepo{games: []Game{{ID: "g1", Name: "Alpha"}}}
	api := &fakeGOG{
		results: map[string][]error{"Alpha|US": {Transient(errors.New("503"))}},
		prices: map[string]Price{
			"Alpha|US": okRow("US", 1000, 2000),
			"Alpha|IN": okRow("IN", 900, 2000),
		},
	}
	o := Options{PassOptions: PassOptions{
		RetryWait1: time.Second, RetryWait2: time.Second,
		Sleep: func(context.Context, time.Duration) error { return nil },
	}}
	sum, err := RunGOG(context.Background(), repo, api, o)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Retried != 1 || sum.Failed != 0 || len(repo.saved["g1"]) != 2 {
		t.Fatalf("summary = %+v saved = %+v", sum, repo.saved)
	}
}

func TestRunGOGPersistentTransientWritesNothingForGame(t *testing.T) {
	repo := &fakeRepo{games: []Game{{ID: "g1", Name: "Alpha"}, {ID: "g2", Name: "Beta"}}}
	api := &fakeGOG{
		results: map[string][]error{"Alpha|IN": {Transient(errors.New("1")), Transient(errors.New("2")), Transient(errors.New("3"))}},
		prices:  map[string]Price{"Beta|US": okRow("US", 1000, 2000), "Beta|IN": okRow("IN", 900, 2000)},
	}
	o := Options{PassOptions: PassOptions{Sleep: func(context.Context, time.Duration) error { return nil }}}
	sum, err := RunGOG(context.Background(), repo, api, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, wrote := repo.saved["g1"]; wrote {
		t.Fatal("a game that still fails must have nothing written")
	}
	if sum.Failed != 1 || sum.Coverage != 0.5 || Succeeded(sum.Coverage) {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestRunGOGBlockedStopsTheRun(t *testing.T) {
	repo := &fakeRepo{games: []Game{{ID: "g1", Name: "Alpha"}, {ID: "g2", Name: "Beta"}}}
	api := &fakeGOG{results: map[string][]error{"Alpha|US": {ErrBlocked}}}
	sum, err := RunGOG(context.Background(), repo, api, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Blocked || api.calls != 1 || len(repo.saved) != 0 {
		t.Fatalf("blocked must stop at once: %+v calls=%d", sum, api.calls)
	}
	if sum.Coverage != 0 {
		t.Fatalf("an interrupted run must not count as covered: %v", sum.Coverage)
	}
}

func TestRunGOGAlreadyCompleteSkipsFetch(t *testing.T) {
	repo := &fakeRepo{games: []Game{{ID: "g1", Name: "Alpha"}, {ID: "g2", Name: "Beta"}}, checked: 2}
	api := &fakeGOG{}
	sum, err := RunGOG(context.Background(), repo, api, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.AlreadyComplete || api.calls != 0 {
		t.Fatalf("already complete must not fetch: %+v calls=%d", sum, api.calls)
	}
}

func TestAlreadyCompleteIsSkippedForDryAndManualRuns(t *testing.T) {
	repo := &fakeRepo{games: []Game{{ID: "g1", Name: "Alpha"}}, checked: 1}
	api := &fakeGOG{}
	for _, o := range []Options{{DryRun: true}, {Manual: true}} {
		sum, err := RunGOG(context.Background(), repo, api, o)
		if err != nil || sum.AlreadyComplete || api.calls == 0 {
			t.Fatalf("options %+v: %+v err=%v calls=%d", o, sum, err, api.calls)
		}
		api.calls = 0
	}
}

func TestRunGOGLimitCountsCoverageOverWorkingSet(t *testing.T) {
	repo := &fakeRepo{games: []Game{{ID: "g1", Name: "Alpha"}, {ID: "g2", Name: "Beta"}, {ID: "g3", Name: "Gamma"}}}
	api := &fakeGOG{}
	sum, err := RunGOG(context.Background(), repo, api, Options{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if sum.Games != 1 || api.calls != 2 || sum.Coverage != 1 {
		t.Fatalf("limit run: %+v calls=%d", sum, api.calls)
	}
}

func TestRunGOGDryRunWritesNothing(t *testing.T) {
	repo := &fakeRepo{games: []Game{{ID: "g1", Name: "Alpha"}}}
	api := &fakeGOG{prices: map[string]Price{"Alpha|US": okRow("US", 1000, 2000)}}
	sum, err := RunGOG(context.Background(), repo, api, Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if repo.saveCalls != 0 || !sum.DryRun || sum.Written != 2 {
		t.Fatalf("dry run: saveCalls=%d summary=%+v", repo.saveCalls, sum)
	}
}

func TestRunGOGNoGamesIsAnError(t *testing.T) {
	if _, err := RunGOG(context.Background(), &fakeRepo{}, &fakeGOG{}, Options{}); !errors.Is(err, ErrNoGames) {
		t.Fatalf("err = %v", err)
	}
}
