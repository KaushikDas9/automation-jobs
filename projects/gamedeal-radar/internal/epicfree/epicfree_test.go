package epicfree

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/freegames.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestParseFixtureKeepsOnlyFreeOffers(t *testing.T) {
	games, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(games) != 5 {
		t.Fatalf("want 5 free games, got %d: %+v", len(games), games)
	}
	byID := map[string]Game{}
	for _, g := range games {
		byID[g.EpicID] = g
	}
	if _, ok := byID["d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9"]; ok {
		t.Error("a discounted offer (33 percent) must not be kept")
	}
	if _, ok := byID["c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3f4"]; ok {
		t.Error("an element with null promotions must be skipped")
	}
	if _, ok := byID["b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2e3"]; ok {
		t.Error("an element with an empty title must be skipped")
	}
}

func TestParseFixtureFieldsNowAndUpcoming(t *testing.T) {
	games, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Game{}
	for _, g := range games {
		byID[g.EpicID] = g
	}

	ember := byID["a1f0c3e27b9d4e8f9c2a51d7e0b4c6a1"]
	if ember.Upcoming || ember.Title != "Ember Ridge" {
		t.Fatalf("ember: %+v", ember)
	}
	if ember.StoreURL != "https://store.epicgames.com/en-US/p/ember-ridge" {
		t.Errorf("ember store url = %q", ember.StoreURL)
	}
	if ember.ImageURL == nil || *ember.ImageURL != "https://cdn.example-store.test/ember-ridge/offer-wide.jpg" {
		t.Errorf("ember image = %v, want the OfferImageWide", ember.ImageURL)
	}
	if ember.RegularPrice == nil || *ember.RegularPrice != "1499.00" || ember.Currency != "INR" {
		t.Errorf("ember price = %v %q", ember.RegularPrice, ember.Currency)
	}
	if !ember.StartsAt.Equal(time.Date(2026, 10, 15, 15, 0, 0, 0, time.UTC)) ||
		!ember.EndsAt.Equal(time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("ember dates = %v to %v", ember.StartsAt, ember.EndsAt)
	}

	starfall := byID["c3d4e5f6a7b8c9d0e1f2a3b4c5d6e7f8"]
	if !starfall.Upcoming {
		t.Fatalf("starfall must be upcoming: %+v", starfall)
	}
	if starfall.StoreURL != "https://store.epicgames.com/en-US/p/starfall-tactics" {
		t.Errorf("hex productSlug must fall back to urlSlug, got %q", starfall.StoreURL)
	}
	if starfall.ImageURL == nil || *starfall.ImageURL != "https://cdn.example-store.test/starfall/thumbnail.jpg" {
		t.Errorf("an http OfferImageWide must be skipped for the https Thumbnail, got %v", starfall.ImageURL)
	}
	if starfall.RegularPrice == nil || *starfall.RegularPrice != "19.99" {
		t.Errorf("starfall price = %v", starfall.RegularPrice)
	}
	if !starfall.StartsAt.Equal(time.Date(2026, 10, 22, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("starfall starts = %v", starfall.StartsAt)
	}

	quiet := byID["e5f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0"]
	if quiet.RegularPrice != nil {
		t.Errorf("a zero original price must give no regular price, got %v", *quiet.RegularPrice)
	}
	if quiet.StoreURL != "https://store.epicgames.com/en-US/p/quiet-orbit" {
		t.Errorf("productSlug with a path must fall back to urlSlug, got %q", quiet.StoreURL)
	}
	if quiet.ImageURL == nil || !strings.HasSuffix(*quiet.ImageURL, "storefront-wide.jpg") {
		t.Errorf("quiet image = %v", quiet.ImageURL)
	}

	glass := byID["f6a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1"]
	if glass.ImageURL != nil {
		t.Errorf("no key images must give no image, got %v", *glass.ImageURL)
	}
	if glass.StoreURL != "https://store.epicgames.com/en-US/p/glass-circuit" {
		t.Errorf("empty slugs must fall back to the mapping page slug, got %q", glass.StoreURL)
	}
	if glass.RegularPrice == nil || *glass.RegularPrice != "899.00" {
		t.Errorf("glass price = %v", glass.RegularPrice)
	}

	iron := byID["a7b8c9d0e1f2a3b4c5d6e7f8a9b0c1d2"]
	if !iron.Upcoming || iron.RegularPrice == nil || *iron.RegularPrice != "999.00" {
		t.Errorf("iron: %+v", iron)
	}
}

func TestParseEmptyFeedIsNotAnError(t *testing.T) {
	games, err := Parse([]byte(`{"data":{"Catalog":{"searchStore":{"elements":[]}}}}`))
	if err != nil || len(games) != 0 {
		t.Fatalf("games=%d err=%v", len(games), err)
	}
}

func TestParseRejectsNonJSON(t *testing.T) {
	if _, err := Parse([]byte("<html>maintenance</html>")); err == nil {
		t.Fatal("a body that is not JSON must be an error")
	}
}

func TestCurrentFreeOfferWinsOverUpcoming(t *testing.T) {
	body := `{"data":{"Catalog":{"searchStore":{"elements":[{
		"id":"0123456789abcdef0123456789abcdef","title":"Both",
		"productSlug":"both","keyImages":[],
		"price":{"totalPrice":{"originalPrice":1000,"currencyCode":"INR","currencyInfo":{"decimals":2}}},
		"promotions":{
			"promotionalOffers":[{"promotionalOffers":[{"startDate":"2026-10-15T15:00:00.000Z","endDate":"2026-10-22T15:00:00.000Z","discountSetting":{"discountPercentage":0}}]}],
			"upcomingPromotionalOffers":[{"promotionalOffers":[{"startDate":"2026-10-22T15:00:00.000Z","endDate":"2026-10-29T15:00:00.000Z","discountSetting":{"discountPercentage":0}}]}]
		}}]}}}}`
	games, err := Parse([]byte(body))
	if err != nil || len(games) != 1 {
		t.Fatalf("games=%v err=%v", games, err)
	}
	if games[0].Upcoming {
		t.Fatal("the current offer must be used when both exist")
	}
}

func TestDuplicateIDsKeepTheFirst(t *testing.T) {
	el := `{"id":"dup-1","title":"Twice","productSlug":"twice","keyImages":[],
		"price":{"totalPrice":{"originalPrice":0,"currencyCode":"INR","currencyInfo":{"decimals":2}}},
		"promotions":{"promotionalOffers":[{"promotionalOffers":[{"startDate":"2026-10-15T15:00:00.000Z","endDate":"2026-10-22T15:00:00.000Z","discountSetting":{"discountPercentage":0}}]}]}}`
	body := `{"data":{"Catalog":{"searchStore":{"elements":[` + el + `,` + el + `]}}}}`
	games, err := Parse([]byte(body))
	if err != nil || len(games) != 1 {
		t.Fatalf("games=%d err=%v", len(games), err)
	}
}

func TestSearchLinkWhenNoUsableSlug(t *testing.T) {
	el := element{Title: "Ember Ridge & Co"}
	el.ProductSlug = "0123456789abcdef0123456789abcdef"
	el.URLSlug = "A Bad Slug"
	el.CatalogNs.Mappings = []struct {
		PageSlug string `json:"pageSlug"`
	}{{PageSlug: "0123456789abcdef0123456789abcdef"}}

	want := "https://store.epicgames.com/en-US/browse?q=" + url.QueryEscape("Ember Ridge & Co") +
		"&sortBy=relevancy&sortDir=DESC&count=40"
	if got := storeURL(el); got != want {
		t.Fatalf("store url = %q, want %q", got, want)
	}
	if !strings.Contains(want, "Ember+Ridge+%26+Co") {
		t.Fatalf("title must be escaped: %q", want)
	}
}

func TestSlugRulesAcceptOnlyPlainSlugs(t *testing.T) {
	for _, s := range []string{"ab", "ember-ridge", "a1-b2"} {
		if !slugPattern.MatchString(s) {
			t.Errorf("%q should be a usable slug", s)
		}
	}
	for _, s := range []string{"", "a", "Ember", "ember/home", "-ember", "ember_ridge"} {
		if slugPattern.MatchString(s) {
			t.Errorf("%q should not be a usable slug", s)
		}
	}
	if !hexIDPattern.MatchString("0123456789abcdef0123456789abcdef") || hexIDPattern.MatchString("0123456789abcdef") {
		t.Error("hex id pattern is wrong")
	}
}

func TestFormatMinor(t *testing.T) {
	cases := []struct {
		v    int64
		dec  int
		want string
	}{
		{149900, 2, "1499.00"},
		{5, 2, "0.05"},
		{7, 0, "7"},
		{12345, 3, "12.345"},
	}
	for _, c := range cases {
		if got := formatMinor(c.v, c.dec); got != c.want {
			t.Errorf("formatMinor(%d, %d) = %q, want %q", c.v, c.dec, got, c.want)
		}
	}
}

func TestRegularPriceNilWhenDecimalsUnknown(t *testing.T) {
	el := element{}
	el.Price.TotalPrice.OriginalPrice = 500
	if regularPrice(el) != nil {
		t.Fatal("missing decimals must give no price")
	}
}

func TestCurrencyFallsBackToINR(t *testing.T) {
	el := element{}
	el.Price.TotalPrice.CurrencyCode = "inr"
	if currencyOf(el) != "INR" {
		t.Fatal("a lowercase or missing code must fall back to INR")
	}
	el.Price.TotalPrice.CurrencyCode = "USD"
	if currencyOf(el) != "USD" {
		t.Fatal("a valid code must be kept")
	}
}

func TestEndpointRequestsIndia(t *testing.T) {
	u, err := url.Parse(Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Scheme != "https" || q.Get("country") != "IN" || q.Get("allowCountries") != "IN" || q.Get("locale") != "en-US" {
		t.Fatalf("endpoint = %q", Endpoint)
	}
}

// testClient points the client at a test server with no retry wait.
func testClient(srv *httptest.Server) *Client {
	return &Client{HTTP: srv.Client(), URL: srv.URL + "/freeGamesPromotions?locale=en-US&country=IN&allowCountries=IN", UserAgent: UserAgent}
}

func TestFetchGamesSendsHonestRequest(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s", r.Method)
		}
		if r.URL.Path != "/freeGamesPromotions" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("User-Agent"); got != "PriceTrackerJob/1.0 (daily free games job)" {
			t.Errorf("user agent = %q", got)
		}
		if r.Header.Get("Cookie") != "" {
			t.Error("no cookies may be sent")
		}
		q := r.URL.Query()
		if q.Get("country") != "IN" || q.Get("allowCountries") != "IN" || q.Get("locale") != "en-US" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture(t))
	}))
	defer srv.Close()

	games, err := testClient(srv).FetchGames(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(games) != 5 {
		t.Fatalf("games = %d", len(games))
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestFetchRetriesOnceAfter5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write(fixture(t))
	}))
	defer srv.Close()

	c := testClient(srv)
	c.RetryWait = 0
	games, err := c.FetchGames(context.Background())
	if err != nil || len(games) != 5 {
		t.Fatalf("games=%d err=%v", len(games), err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestFetchGivesUpAfterSecond5xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	c := testClient(srv)
	c.RetryWait = 0
	games, err := c.FetchGames(context.Background())
	if err == nil || games != nil {
		t.Fatalf("want an error and no games, got %d games, err=%v", len(games), err)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want exactly 2", calls.Load())
	}
}

func TestFetchDoesNotRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	c := testClient(srv)
	c.RetryWait = 0
	if _, err := c.FetchGames(context.Background()); err == nil {
		t.Fatal("a 404 must be an error")
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1", calls.Load())
	}
}

func TestFetchRejectsOversizedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat(" ", maxBodyBytes+1)))
	}))
	defer srv.Close()

	if _, err := testClient(srv).FetchGames(context.Background()); err == nil {
		t.Fatal("a body over the limit must be an error")
	}
}

func TestNewClientUsesPublicSettings(t *testing.T) {
	c := NewClient()
	if c.URL != Endpoint || c.UserAgent != UserAgent || c.HTTP.Timeout != 30*time.Second {
		t.Fatalf("client = %+v", c)
	}
}
