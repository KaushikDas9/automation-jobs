package itad

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"gamedeal-radar/daily-check/internal/storeprices"
)

const testKey = "test-key-do-not-use-0123456789"

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMatchTitleIsStrict(t *testing.T) {
	found := func(title string) LookupResult { return LookupResult{Found: true, ID: "id-1", Title: title} }

	accept := [][2]string{
		{"Cyberpunk 2077", "Cyberpunk 2077"},
		{"Cyberpunk 2077", "Cyberpunk 2077™"},
		{"Baldur's Gate 3", "Baldur’s Gate 3"},
		{"Half-Life 2", "Half Life 2"}, // punctuation becomes a space on both sides
		{"Portal 2", "  portal   2 "},
	}
	for _, c := range accept {
		if !MatchTitle(c[0], found(c[1])) {
			t.Errorf("should accept %q for %q", c[1], c[0])
		}
	}

	reject := [][2]string{
		{"Cyberpunk 2077", "Cyberpunk 2077: Phantom Liberty"},
		{"Cyberpunk 2077", "Cyberpunk 2"},
		{"Portal", "Portal 2"},
		{"Cyberpunk", "Cyberpunk 2077"},
	}
	for _, c := range reject {
		if MatchTitle(c[0], found(c[1])) {
			t.Errorf("should reject %q for %q", c[1], c[0])
		}
	}

	if MatchTitle("Anything", LookupResult{Found: false, ID: "x", Title: "Anything"}) {
		t.Error("not found must never match")
	}
	if MatchTitle("Anything", LookupResult{Found: true, ID: "", Title: "Anything"}) {
		t.Error("a match without an id must never match")
	}
	if MatchTitle("™®©", found("™®©")) {
		t.Error("a name that normalizes to nothing must never match")
	}
}

func TestParseLookupFixtures(t *testing.T) {
	r, err := ParseLookup(fixture(t, "lookup_found.json"))
	if err != nil || !r.Found || r.ID != "018d937e-3a5c-7b2a-9d4e-0f5a1c2b3d4e" || r.Title != "Cyberpunk 2077" {
		t.Fatalf("found = %+v, %v", r, err)
	}
	r, err = ParseLookup(fixture(t, "lookup_not_found.json"))
	if err != nil || r.Found {
		t.Fatalf("not found = %+v, %v", r, err)
	}
	if _, err := ParseLookup([]byte("[")); err == nil {
		t.Fatal("bad json must error")
	}
}

func TestParsePricesKeepsEpicOnly(t *testing.T) {
	in, err := ParsePrices(fixture(t, "prices_in.json"), "IN")
	if err != nil {
		t.Fatal(err)
	}
	if len(in) != 2 {
		t.Fatalf("games with an epic deal = %d, want 2 (the GOG-only game is absent)", len(in))
	}

	d := in["018d937e-3a5c-7b2a-9d4e-0f5a1c2b3d4e"]
	if d.Currency != "INR" || storeprices.FormatCents(*d.CurrentCents) != "1199.50" || storeprices.FormatCents(*d.BaseCents) != "2999.00" {
		t.Fatalf("epic deal = %+v", d)
	}
	if d.Timestamp == nil || !d.Timestamp.Equal(time.Date(2026, 10, 13, 18, 30, 0, 0, time.UTC)) {
		t.Fatalf("timestamp = %v", d.Timestamp)
	}

	free := in["018d937e-8888-7000-8000-000000000002"]
	if free.CurrentCents == nil || *free.CurrentCents != 0 {
		t.Fatalf("free epic deal must have a zero price: %+v", free)
	}

	if _, ok := in["018d937e-9999-7000-8000-000000000003"]; ok {
		t.Fatal("a game with no shop 16 deal must not appear")
	}

	us, err := ParsePrices(fixture(t, "prices_us.json"), "US")
	if err != nil {
		t.Fatal(err)
	}
	if u := us["018d937e-3a5c-7b2a-9d4e-0f5a1c2b3d4e"]; u.Currency != "USD" || storeprices.FormatCents(*u.CurrentCents) != "17.99" {
		t.Fatalf("us deal = %+v", u)
	}
}

func TestParsePricesLowestEpicDealWins(t *testing.T) {
	body := []byte(`[{"id":"a","deals":[
		{"shop":{"id":16},"price":{"amount":20,"currency":"USD"},"regular":{"amount":60,"currency":"USD"}},
		{"shop":{"id":16},"price":{"amount":15,"currency":"USD"},"regular":{"amount":60,"currency":"USD"}}
	]}]`)
	got, err := ParsePrices(body, "US")
	if err != nil {
		t.Fatal(err)
	}
	if storeprices.FormatCents(*got["a"].CurrentCents) != "15.00" {
		t.Fatalf("lowest deal not chosen: %+v", got["a"])
	}
}

func TestParsePricesMissingAmountIsNil(t *testing.T) {
	body := []byte(`[{"id":"a","deals":[{"shop":{"id":16},"price":{"currency":"INR"},"regular":{"amount":-5,"currency":"INR"}}]}]`)
	got, err := ParsePrices(body, "IN")
	if err != nil {
		t.Fatal(err)
	}
	if got["a"].CurrentCents != nil || got["a"].BaseCents != nil {
		t.Fatalf("missing or negative amounts must be nil: %+v", got["a"])
	}
}

func TestParseShopLinksHexAndEmptyValues(t *testing.T) {
	links, err := ParseShopLinks(fixture(t, "shop_links.json"))
	if err != nil {
		t.Fatal(err)
	}
	if links["018d937e-3a5c-7b2a-9d4e-0f5a1c2b3d4e"] != "cyberpunk-2077" {
		t.Fatalf("slug = %q", links["018d937e-3a5c-7b2a-9d4e-0f5a1c2b3d4e"])
	}
	if links["018d937e-8888-7000-8000-000000000002"] != "9f8e7d6c5b4a39281706f5e4d3c2b1a0" {
		t.Fatal("hex id must be returned raw, the link builder rejects it")
	}
	if v, ok := links["018d937e-9999-7000-8000-000000000003"]; !ok || v != "" {
		t.Fatalf("empty value must map to empty string, got %q %v", v, ok)
	}
	if _, err := ParseShopLinks([]byte(`[1]`)); err == nil {
		t.Fatal("wrong shape must error")
	}
}

func TestValidEpicSlugAndLinks(t *testing.T) {
	good := []string{"cyberpunk-2077", "ab", "a1-b2-c3", "x" + strings.Repeat("y", 120)}
	for _, s := range good {
		if !ValidEpicSlug(s) {
			t.Errorf("slug %q should be valid", s)
		}
	}
	bad := []string{
		"", "a", "-leading", "Upper-Case", "has space", "under_score", "dot.dot", "trailing/slash",
		"9f8e7d6c5b4a39281706f5e4d3c2b1a0", // 32-char hex id
		"x" + strings.Repeat("y", 121),
		"https://evil.example/x",
	}
	for _, s := range bad {
		if ValidEpicSlug(s) {
			t.Errorf("slug %q should be rejected", s)
		}
	}

	if u, ok := EpicPageURL("cyberpunk-2077"); !ok || u != "https://store.epicgames.com/en-US/p/cyberpunk-2077" {
		t.Fatalf("page url = %q %v", u, ok)
	}
	if _, ok := EpicPageURL("9f8e7d6c5b4a39281706f5e4d3c2b1a0"); ok {
		t.Fatal("hex id must not become a page link")
	}

	got := EpicSearchURL("Half-Life 2 & more")
	want := "https://store.epicgames.com/en-US/browse?q=Half-Life+2+%26+more&sortBy=relevancy&sortDir=DESC&count=40"
	if got != want {
		t.Fatalf("search url = %q", got)
	}
}

func TestEpicStoreURLFallsBackToSearch(t *testing.T) {
	u, slug := epicStoreURL("Portal 2", "9f8e7d6c5b4a39281706f5e4d3c2b1a0")
	if slug || !strings.HasPrefix(u, "https://store.epicgames.com/en-US/browse?q=Portal+2") {
		t.Fatalf("hex must fall back to search: %q %v", u, slug)
	}
	u, slug = epicStoreURL("Portal 2", "portal-2")
	if !slug || u != "https://store.epicgames.com/en-US/p/portal-2" {
		t.Fatalf("slug must be used: %q %v", u, slug)
	}
}

func TestPriceFromDeal(t *testing.T) {
	cur := int64(1199)
	base := int64(2999)
	ts := time.Date(2026, 10, 13, 18, 30, 0, 0, time.UTC)
	p, reason := PriceFromDeal(Deal{Currency: "INR", CurrentCents: &cur, BaseCents: &base, Timestamp: &ts}, storeprices.RegionIN, "https://x")
	if reason != "" {
		t.Fatalf("reason = %q", reason)
	}
	if p.Status != storeprices.StatusOK || p.StoreCode != storeprices.StoreEpic || p.Region != "IN" {
		t.Fatalf("row = %+v", p)
	}
	if p.DiscountPct != storeprices.DiscountPercent(base, cur) || p.DiscountPct != 60 {
		t.Fatalf("discount = %d, want 60 computed by us", p.DiscountPct)
	}
	if p.SourceUpdatedAt == nil || !p.SourceUpdatedAt.Equal(ts) {
		t.Fatal("source_updated_at must be the deal timestamp")
	}

	zero := int64(0)
	free, _ := PriceFromDeal(Deal{Currency: "INR", CurrentCents: &zero}, storeprices.RegionIN, "u")
	if free.Status != storeprices.StatusFree {
		t.Fatalf("zero price must be free: %+v", free)
	}

	_, reason = PriceFromDeal(Deal{Currency: "USD", RegularCurrency: "INR", CurrentCents: &cur}, storeprices.RegionUS, "u")
	if reason == "" {
		t.Fatal("mismatched regular currency must be rejected")
	}
}

// --- HTTP client -----------------------------------------------------------------

type recorded struct {
	method, path, query, key, ua, ct string
	body                             string
}

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{Fetcher: storeprices.NewFetcher(0), BaseURL: srv.URL, APIKey: testKey}, srv
}

func TestClientSendsKeyUserAgentAndBatchBody(t *testing.T) {
	var got recorded
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = recorded{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery,
			key: r.Header.Get("ITAD-API-Key"), ua: r.Header.Get("User-Agent"), ct: r.Header.Get("Content-Type"), body: string(b)}
		_, _ = w.Write(fixture(t, "prices_in.json"))
	})
	deals, err := c.Prices(context.Background(), []string{"id-a", "id-b"}, "IN")
	if err != nil {
		t.Fatal(err)
	}
	if len(deals) != 2 {
		t.Fatalf("deals = %d", len(deals))
	}
	if got.method != http.MethodPost || got.path != "/games/prices/v3" {
		t.Fatalf("request = %s %s", got.method, got.path)
	}
	if !strings.Contains(got.query, "country=IN") || !strings.Contains(got.query, "shops=16") {
		t.Fatalf("query = %q", got.query)
	}
	if got.key != testKey || got.ua != "GameDealRadar-jobs/1.0" || got.ct != "application/json" {
		t.Fatalf("headers: key_ok=%t ua=%q ct=%q", got.key == testKey, got.ua, got.ct)
	}
	var ids []string
	if err := json.Unmarshal([]byte(got.body), &ids); err != nil || len(ids) != 2 || ids[0] != "id-a" {
		t.Fatalf("body = %q", got.body)
	}
	if strings.Contains(got.query, testKey) || strings.Contains(got.body, testKey) {
		t.Fatal("the key must only travel in the header")
	}
}

func TestClientLookupUsesTitleQuery(t *testing.T) {
	var q string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query().Get("title")
		_, _ = w.Write(fixture(t, "lookup_found.json"))
	})
	r, err := c.Lookup(context.Background(), "Cyberpunk 2077")
	if err != nil || !r.Found || q != "Cyberpunk 2077" {
		t.Fatalf("lookup = %+v q=%q err=%v", r, q, err)
	}
}

func TestClientStatusErrors(t *testing.T) {
	c, srv := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	_, err := c.Lookup(context.Background(), "x")
	if !storeprices.IsBlocked(err) {
		t.Fatalf("403 must block: %v", err)
	}
	if strings.Contains(err.Error(), testKey) {
		t.Fatal("error text must never contain the key")
	}
	srv.Close()

	c2, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	if _, err := c2.Lookup(context.Background(), "x"); !storeprices.IsTransient(err) {
		t.Fatalf("502 must be transient: %v", err)
	}
	c3, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	if _, err := c3.Lookup(context.Background(), "x"); !storeprices.IsBlocked(err) {
		t.Fatalf("401 must block: %v", err)
	}
}

func TestClientRetriesOnceAfterRetryAfter(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write(fixture(t, "lookup_found.json"))
	})
	if _, err := c.Lookup(context.Background(), "x"); err != nil {
		t.Fatalf("one 429 then 200 must succeed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d", calls)
	}
}

func TestClientSecond429StopsTheRun(t *testing.T) {
	calls := 0
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	_, err := c.Lookup(context.Background(), "x")
	if !storeprices.IsBlocked(err) || calls != 2 {
		t.Fatalf("two 429s must block after 2 calls: err=%v calls=%d", err, calls)
	}
}

func TestRetryAfterParsing(t *testing.T) {
	h := http.Header{}
	if retryAfter(h) != 30*time.Second {
		t.Fatal("default retry-after must be 30s")
	}
	h.Set("Retry-After", "5")
	if retryAfter(h) != 5*time.Second {
		t.Fatal("seconds must parse")
	}
	h.Set("Retry-After", "100000")
	if retryAfter(h) != maxRetryAfter {
		t.Fatal("retry-after must be capped")
	}
	h.Set("Retry-After", "Wed, 21 Oct 2026 07:28:00 GMT")
	if retryAfter(h) != 30*time.Second {
		t.Fatal("unreadable retry-after falls back to the default")
	}
}

// --- Epic run with fakes ------------------------------------------------------------

type fakeAPI struct {
	lookups   map[string]LookupResult
	lookupErr map[string][]error
	prices    map[string]map[string]Deal // country -> id -> deal
	links     map[string]string
	priceErr  []error
	linkErr   []error
	lookupN   int
	priceN    int
	linkN     int
}

func (f *fakeAPI) Lookup(_ context.Context, title string) (LookupResult, error) {
	f.lookupN++
	if q := f.lookupErr[title]; len(q) > 0 {
		err := q[0]
		f.lookupErr[title] = q[1:]
		return LookupResult{}, err
	}
	return f.lookups[title], nil
}

func (f *fakeAPI) Prices(_ context.Context, ids []string, country string) (map[string]Deal, error) {
	f.priceN++
	if len(f.priceErr) > 0 {
		err := f.priceErr[0]
		f.priceErr = f.priceErr[1:]
		return nil, err
	}
	out := map[string]Deal{}
	for _, id := range ids {
		if d, ok := f.prices[country][id]; ok {
			out[id] = d
		}
	}
	return out, nil
}

func (f *fakeAPI) EpicLinks(_ context.Context, ids []string) (map[string]string, error) {
	f.linkN++
	if len(f.linkErr) > 0 {
		err := f.linkErr[0]
		f.linkErr = f.linkErr[1:]
		return nil, err
	}
	out := map[string]string{}
	for _, id := range ids {
		if v, ok := f.links[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

type fakeRepo struct {
	games  []storeprices.Game
	manual map[string]bool
	stored map[storeprices.StoredKey]int64
	saved  map[string][]storeprices.Price
}

func (r *fakeRepo) ActiveGames(context.Context) ([]storeprices.Game, error) { return r.games, nil }
func (r *fakeRepo) StoredCents(context.Context, string) (map[storeprices.StoredKey]int64, error) {
	if r.stored == nil {
		return map[storeprices.StoredKey]int64{}, nil
	}
	return r.stored, nil
}
func (r *fakeRepo) ManualGameIDs(context.Context, string) (map[string]bool, error) {
	if r.manual == nil {
		return map[string]bool{}, nil
	}
	return r.manual, nil
}
func (r *fakeRepo) CheckedCount(context.Context, string, time.Time) (int, error) { return 0, nil }
func (r *fakeRepo) SaveGameRows(_ context.Context, _, gameID string, rows []storeprices.Price) (storeprices.SaveResult, error) {
	if r.saved == nil {
		r.saved = map[string][]storeprices.Price{}
	}
	r.saved[gameID] = append(r.saved[gameID], rows...)
	return storeprices.SaveResult{Written: len(rows)}, nil
}

func ptr[T any](v T) *T { return &v }

const cpID = "018d937e-3a5c-7b2a-9d4e-0f5a1c2b3d4e"

func epicFake() *fakeAPI {
	return &fakeAPI{
		lookups: map[string]LookupResult{
			"Cyberpunk 2077": {Found: true, ID: cpID, Title: "Cyberpunk 2077"},
			"Portal":         {Found: true, ID: "other-id", Title: "Portal 2"}, // title mismatch
		},
		prices: map[string]map[string]Deal{
			"IN": {cpID: {Currency: "INR", CurrentCents: ptr(int64(119950)), BaseCents: ptr(int64(299900))}},
			"US": {cpID: {Currency: "USD", CurrentCents: ptr(int64(1799)), BaseCents: ptr(int64(5999))}},
		},
		links: map[string]string{cpID: "cyberpunk-2077"},
	}
}

func TestRunEpicWritesSlugLinksAndBothRegions(t *testing.T) {
	repo := &fakeRepo{games: []storeprices.Game{{ID: "g1", Name: "Cyberpunk 2077"}, {ID: "g2", Name: "Portal"}, {ID: "g3", Name: "Unknown Game"}}}
	api := epicFake()
	sum, err := RunEpic(context.Background(), repo, api, storeprices.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rows := repo.saved["g1"]
	if len(rows) != 2 {
		t.Fatalf("rows for g1 = %d, want 2", len(rows))
	}
	for _, p := range rows {
		if p.StoreURL != "https://store.epicgames.com/en-US/p/cyberpunk-2077" || p.StoreCode != "epic" {
			t.Fatalf("row = %+v", p)
		}
	}
	if _, ok := repo.saved["g2"]; ok {
		t.Fatal("title mismatch must not be written")
	}
	if _, ok := repo.saved["g3"]; ok {
		t.Fatal("no match must not be written")
	}
	if sum.NoMatch != 1 || sum.TitleMismatch != 1 || sum.Matched != 1 || sum.SlugLinks != 1 || sum.Failed != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.WithPrice[storeprices.RegionIN] != 1 || sum.WithPrice[storeprices.RegionUS] != 1 {
		t.Fatalf("with price = %+v", sum.WithPrice)
	}
	if sum.Coverage != 1 {
		t.Fatalf("coverage = %v (no-match games count as completed)", sum.Coverage)
	}
}

func TestRunEpicSearchLinkAndNoDealCounted(t *testing.T) {
	repo := &fakeRepo{games: []storeprices.Game{{ID: "g1", Name: "Cyberpunk 2077"}}}
	api := epicFake()
	delete(api.prices["US"], cpID) // no Epic deal in the US
	api.links = map[string]string{cpID: "9f8e7d6c5b4a39281706f5e4d3c2b1a0"}
	sum, err := RunEpic(context.Background(), repo, api, storeprices.Options{})
	if err != nil {
		t.Fatal(err)
	}
	rows := repo.saved["g1"]
	if len(rows) != 1 || rows[0].Region != storeprices.RegionIN {
		t.Fatalf("rows = %+v", rows)
	}
	if !strings.HasPrefix(rows[0].StoreURL, "https://store.epicgames.com/en-US/browse?q=Cyberpunk+2077") {
		t.Fatalf("search link = %q", rows[0].StoreURL)
	}
	if sum.NoPrice[storeprices.RegionUS] != 1 || sum.SearchLinks != 1 {
		t.Fatalf("summary = %+v", sum)
	}
}

func TestRunEpicManualGameIsUntouchedAndNotLookedUp(t *testing.T) {
	repo := &fakeRepo{
		games:  []storeprices.Game{{ID: "g1", Name: "Cyberpunk 2077"}},
		manual: map[string]bool{"g1": true},
	}
	api := epicFake()
	sum, err := RunEpic(context.Background(), repo, api, storeprices.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if api.lookupN != 0 || len(repo.saved) != 0 || sum.SkippedManual != 1 || sum.Failed != 0 {
		t.Fatalf("manual game: lookups=%d saved=%v summary=%+v", api.lookupN, repo.saved, sum)
	}
}

func TestRunEpicJumpIsSkipped(t *testing.T) {
	repo := &fakeRepo{
		games:  []storeprices.Game{{ID: "g1", Name: "Cyberpunk 2077"}},
		stored: map[storeprices.StoredKey]int64{{GameID: "g1", Region: storeprices.RegionUS}: 500},
	}
	sum, err := RunEpic(context.Background(), repo, epicFake(), storeprices.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sum.SkippedSanity != 1 {
		t.Fatalf("SkippedSanity = %d", sum.SkippedSanity)
	}
	if rows := repo.saved["g1"]; len(rows) != 1 || rows[0].Region != storeprices.RegionIN {
		t.Fatalf("only IN must be written: %+v", rows)
	}
}

func TestRunEpicPriceBatchFailureWritesNothingForItsGames(t *testing.T) {
	repo := &fakeRepo{games: []storeprices.Game{{ID: "g1", Name: "Cyberpunk 2077"}}}
	api := epicFake()
	api.priceErr = []error{errors.New("bad response shape")} // permanent: no retry
	sum, err := RunEpic(context.Background(), repo, api, storeprices.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.saved) != 0 || sum.Failed != 1 || sum.Coverage != 0 {
		t.Fatalf("failed batch: saved=%v summary=%+v", repo.saved, sum)
	}
}

func TestRunEpicTransientThenBlockedStops(t *testing.T) {
	repo := &fakeRepo{games: []storeprices.Game{{ID: "g1", Name: "Cyberpunk 2077"}, {ID: "g2", Name: "Portal"}}}
	api := epicFake()
	api.lookupErr = map[string][]error{"Cyberpunk 2077": {storeprices.ErrBlocked}}
	sum, err := RunEpic(context.Background(), repo, api, storeprices.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Blocked || len(repo.saved) != 0 {
		t.Fatalf("blocked run must stop and write nothing: %+v", sum)
	}
	if sum.Coverage != 0 || storeprices.Succeeded(sum.Coverage) {
		t.Fatalf("an interrupted run must not count as covered: %v", sum.Coverage)
	}
}

func TestRunEpicTransientLookupRetriedThenFailed(t *testing.T) {
	repo := &fakeRepo{games: []storeprices.Game{{ID: "g1", Name: "Cyberpunk 2077"}}}
	api := epicFake()
	tr := storeprices.Transient(errors.New("timeout"))
	api.lookupErr = map[string][]error{"Cyberpunk 2077": {tr, tr, tr}}
	o := storeprices.Options{PassOptions: storeprices.PassOptions{Sleep: func(context.Context, time.Duration) error { return nil }}}
	sum, err := RunEpic(context.Background(), repo, api, o)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Failed != 1 || sum.Retried != 1 || len(repo.saved) != 0 || api.lookupN != 3 {
		t.Fatalf("summary = %+v lookups=%d", sum, api.lookupN)
	}
}

func TestRunEpicDryRunWritesNothing(t *testing.T) {
	repo := &fakeRepo{games: []storeprices.Game{{ID: "g1", Name: "Cyberpunk 2077"}}}
	sum, err := RunEpic(context.Background(), repo, epicFake(), storeprices.Options{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(repo.saved) != 0 || sum.Written != 2 || !sum.DryRun {
		t.Fatalf("dry run: %+v saved=%v", sum, repo.saved)
	}
}

func TestRunEpicBatchesAtMostTwoHundredIDs(t *testing.T) {
	var games []storeprices.Game
	api := &fakeAPI{lookups: map[string]LookupResult{}, prices: map[string]map[string]Deal{"IN": {}, "US": {}}, links: map[string]string{}}
	for i := 0; i < 450; i++ {
		name := "Game " + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + strconv.Itoa(i)
		id := "id-" + strconv.Itoa(i)
		games = append(games, storeprices.Game{ID: "g" + strconv.Itoa(i), Name: name})
		api.lookups[name] = LookupResult{Found: true, ID: id, Title: name}
	}
	repo := &fakeRepo{games: games}
	if _, err := RunEpic(context.Background(), repo, api, storeprices.Options{}); err != nil {
		t.Fatal(err)
	}
	// 450 matches -> 3 chunks; two countries for prices, and 3 link calls.
	if api.priceN != 6 || api.linkN != 3 {
		t.Fatalf("priceN=%d linkN=%d, want 6 and 3", api.priceN, api.linkN)
	}
}
