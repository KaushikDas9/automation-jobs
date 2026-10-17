package storedetails

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSlugValidation(t *testing.T) {
	good := []string{"alan-wake-2", "hades", "a", "fortnite", "cyberpunk-2077", strings.Repeat("a", 120)}
	for _, s := range good {
		if !ValidEpicSlug(s) {
			t.Errorf("epic slug %q should be valid", s)
		}
	}
	bad := []string{
		"", "../etc/passwd", "..", "alan-wake-2/../x", "Alan-Wake-2", "ALAN", "-leading",
		strings.Repeat("a", 121), "https://evil.example/x", "//evil.example", "a?b=1", "a#b",
		"a%2F..", "a b", "a\nb", "a_b", "a.b", "a/b", "@evil", "a\\b",
	}
	for _, s := range bad {
		if ValidEpicSlug(s) {
			t.Errorf("epic slug %q must be refused", s)
		}
	}
	for _, s := range []string{"alan_wake", "the_witcher_3_wild_hunt", "cyberpunk_2077", "a-b"} {
		if !ValidGOGSlug(s) {
			t.Errorf("gog slug %q should be valid", s)
		}
	}
	for _, s := range []string{"", "../x", "Alan_Wake", "_x", strings.Repeat("a", 121), "https://www.gog.com/game/x", "a/b", "a?b", "a.b"} {
		if ValidGOGSlug(s) {
			t.Errorf("gog slug %q must be refused", s)
		}
	}
}

func TestInvalidSlugSendsNoRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	c := testClient(srv.URL)
	for _, s := range []string{"../../admin", "http://169.254.169.254/latest", "UPPER"} {
		if _, err := c.EpicDetails(context.Background(), s); !errors.Is(err, ErrInvalidID) {
			t.Errorf("epic %q: err = %v", s, err)
		}
		if _, err := c.GOGDetails(context.Background(), s, "x"); !errors.Is(err, ErrInvalidID) {
			t.Errorf("gog %q: err = %v", s, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests were sent for invalid slugs", hits.Load())
	}
}

func TestProductionHostsAreFixed(t *testing.T) {
	c := NewClient()
	if c.epicBase != "https://store-content-ipv4.ak.epicgames.com/api/en-US/content/products/" ||
		c.gogCatalogBase != "https://catalog.gog.com/v1/catalog" || c.gogGamesBase != "https://api.gog.com/v2/games/" {
		t.Fatalf("unexpected bases %q %q %q", c.epicBase, c.gogCatalogBase, c.gogGamesBase)
	}
	if c.UserAgent == "" || strings.Contains(strings.ToLower(c.UserAgent), "mozilla") {
		t.Fatalf("user agent must be honest: %q", c.UserAgent)
	}
	if c.HTTP.Timeout <= 0 {
		t.Fatal("client needs a timeout")
	}
}

func TestRedirectOffHostRefused(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"productName":"x","pages":[{}]}`))
	}))
	defer other.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer srv.Close()
	c := testClient(srv.URL)
	c.HTTP = srv.Client()
	c.HTTP.CheckRedirect = sameHostRedirects
	if _, err := c.EpicDetails(context.Background(), "alan-wake-2"); err == nil {
		t.Fatal("a redirect to another host must fail")
	}
}

func testClient(base string) *Client {
	c := NewClient()
	c.RetryWait = time.Millisecond
	c.epicBase = base + "/epic/"
	c.gogCatalogBase = base + "/catalog"
	c.gogGamesBase = base + "/v2/games/"
	return c
}

func TestFetchStatusHandling(t *testing.T) {
	cases := []struct {
		name      string
		statuses  []int
		wantErr   func(error) bool
		wantCalls int32
	}{
		{"404 is not found, no retry", []int{404}, func(e error) bool { return errors.Is(e, ErrNotFound) }, 1},
		{"one 5xx is retried", []int{503, 200}, func(e error) bool { return e == nil }, 2},
		{"two 5xx is transient", []int{502, 500}, IsTransient, 2},
		{"403 blocked, no retry", []int{403}, func(e error) bool { return errors.Is(e, ErrBlocked) }, 1},
		{"429 blocked, no retry", []int{429}, func(e error) bool { return errors.Is(e, ErrBlocked) }, 1},
		{"400 permanent", []int{400}, func(e error) bool { return e != nil && !IsTransient(e) && !errors.Is(e, ErrNotFound) }, 1},
	}
	body := readFixture(t, "epic_alan-wake-2.json")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				if r.Header.Get("User-Agent") == "" {
					t.Error("missing user agent")
				}
				st := tc.statuses[min(int(n)-1, len(tc.statuses)-1)]
				w.WriteHeader(st)
				if st == 200 {
					w.Write(body)
				} else {
					w.Write([]byte(`{"secret-body":"do not log"}`))
				}
			}))
			defer srv.Close()
			_, err := testClient(srv.URL).EpicDetails(context.Background(), "alan-wake-2")
			if !tc.wantErr(err) {
				t.Fatalf("err = %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "secret-body") {
				t.Fatalf("error leaks the body: %v", err)
			}
			if calls.Load() != tc.wantCalls {
				t.Fatalf("calls = %d, want %d", calls.Load(), tc.wantCalls)
			}
		})
	}
}

func TestBodyOverLimitIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"productName":"`))
		w.Write([]byte(strings.Repeat("a", MaxBodyBytes)))
		w.Write([]byte(`"}`))
	}))
	defer srv.Close()
	_, err := testClient(srv.URL).EpicDetails(context.Background(), "big")
	if err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v", err)
	}
}

func TestEpicRequestPath(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write(readFixture(t, "epic_alan-wake-2.json"))
	}))
	defer srv.Close()
	if _, err := testClient(srv.URL).EpicDetails(context.Background(), "alan-wake-2"); err != nil {
		t.Fatal(err)
	}
	if path != "/epic/alan-wake-2" {
		t.Fatalf("path = %q", path)
	}
}

func TestParseEpicAlanWake2(t *testing.T) {
	d, err := ParseEpic("alan-wake-2", readFixture(t, "epic_alan-wake-2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.ShortDescription, "Saga Anderson arrives to investigate ritualistic murders") {
		t.Errorf("short description = %q", d.ShortDescription)
	}
	if len(d.Developers) != 1 || d.Developers[0] != "Remedy Entertainment" {
		t.Errorf("developers = %v", d.Developers)
	}
	if len(d.Publishers) != 1 || d.Publishers[0] != "Epic Games Publishing" {
		t.Errorf("publishers = %v", d.Publishers)
	}
	// 3 carousel images, then the hero background and the about artwork.
	if len(d.Screenshots) != 5 {
		t.Fatalf("screenshots = %d: %+v", len(d.Screenshots), d.Screenshots)
	}
	for _, s := range d.Screenshots {
		if !strings.HasPrefix(s.Full, "https://cdn2.unrealengine.com/") || s.Thumb == "" {
			t.Errorf("screenshot %+v", s)
		}
	}
	if len(d.Movies) != 2 {
		t.Fatalf("movies = %+v", d.Movies)
	}
	m := d.Movies[0]
	if !strings.HasSuffix(m.HLS, ".m3u8") || !strings.HasPrefix(m.HLS, "https://media-cdn.epicgames.com/") ||
		!strings.HasSuffix(m.DASH, ".mpd") || !strings.HasPrefix(m.Thumb, "https://cdn2.unrealengine.com/") {
		t.Errorf("movie = %+v", m)
	}
	if !d.Platforms.Windows || d.Platforms.Mac || d.Platforms.Linux {
		t.Errorf("platforms = %+v", d.Platforms)
	}
	wantMin := `<strong>Minimum:</strong><br><ul class="bb_ul"><li><strong>Windows OS:</strong> Windows 10/11 64-bit</li>`
	if !strings.HasPrefix(d.PCReqMinHTML, wantMin) || !strings.Contains(d.PCReqMinHTML, "GeForce GTX 1070 / Radeon RX 5600 XT") {
		t.Errorf("pc min = %q", d.PCReqMinHTML)
	}
	if !strings.HasPrefix(d.PCReqRecHTML, "<strong>Recommended:</strong>") || !strings.Contains(d.PCReqRecHTML, "Ryzen 7 3700X") {
		t.Errorf("pc rec = %q", d.PCReqRecHTML)
	}
	if d.MacReqMinHTML != "" || d.LinuxReqMinHTML != "" {
		t.Error("no mac or linux requirements on this page")
	}
	if !strings.HasPrefix(d.SupportedLanguages, "English, German, Spanish - Spain, Japanese, French") ||
		strings.Count(d.SupportedLanguages, "English") != 1 || strings.Contains(d.SupportedLanguages, "AUDIO") {
		t.Errorf("languages = %q", d.SupportedLanguages)
	}
	// The page says customReleaseDate "Coming Soon" and has no ISO date: nothing is stored.
	if d.ReleaseDateText != "" || d.ReleaseDate != nil || d.ComingSoon {
		t.Errorf("release = %q %v %t", d.ReleaseDateText, d.ReleaseDate, d.ComingSoon)
	}
	if len(d.AgeRatings) != 0 {
		t.Errorf("ratings = %+v", d.AgeRatings)
	}
	if d.Website != "http://alanwake.com/" {
		t.Errorf("website = %q", d.Website)
	}
	if d.Type != "game" || d.ExternalAppID != "alan-wake-2" || d.Genres == nil || d.Categories == nil {
		t.Errorf("basics = %+v", d)
	}
}

func TestParseEpicCyberpunkRatingsAndDate(t *testing.T) {
	d, err := ParseEpic("cyberpunk-2077", readFixture(t, "epic_cyberpunk-2077.json"))
	if err != nil {
		t.Fatal(err)
	}
	if d.ReleaseDate == nil || d.ReleaseDate.Format("2006-01-02") != "2020-12-10" || d.ReleaseDateText != "10 Dec, 2020" {
		t.Errorf("release = %v %q", d.ReleaseDate, d.ReleaseDateText)
	}
	if len(d.AgeRatings) != 3 || d.AgeRatings[0].Board != "br" || d.AgeRatings[0].Rating != "18 anos" {
		t.Errorf("ratings = %+v", d.AgeRatings)
	}
	if !d.Platforms.Windows || !d.Platforms.Mac || d.MacReqMinHTML == "" {
		t.Errorf("platforms %+v mac %q", d.Platforms, d.MacReqMinHTML)
	}
	if strings.Contains(d.SupportedLanguages, "TEXT:") || !strings.Contains(d.SupportedLanguages, "Thai") {
		t.Errorf("languages = %q", d.SupportedLanguages)
	}
}

func TestParseEpicNotFound(t *testing.T) {
	for _, body := range []string{
		`{"error":true,"message":"Page was not found"}`,
		`{"productName":"","pages":[{"type":"productHome"}]}`,
		`{"productName":"X","pages":[]}`,
	} {
		if _, err := ParseEpic("x", []byte(body)); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v", body, err)
		}
	}
	if _, err := ParseEpic("x", []byte(`not json`)); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("bad json must be a plain error: %v", err)
	}
}

func TestParseEpicPicksProductHomePage(t *testing.T) {
	body := `{"productName":"F","pages":[
	 {"type":"experience","data":{"about":{"shortDescription":"wrong page"}}},
	 {"type":"productHome","data":{"about":{"shortDescription":"right page"}}}]}`
	d, err := ParseEpic("f", []byte(body))
	if err != nil || d.ShortDescription != "right page" {
		t.Fatalf("%+v %v", d, err)
	}
}

// Everything Epic sends is publisher-edited text. None of it may reach the HTML columns raw.
func TestParseEpicEscapesHostileText(t *testing.T) {
	body := `{"productName":"Evil","pages":[{"type":"productHome","data":{
	  "about":{"shortDescription":"<img src=x onerror=alert(1)>Nice **game**",
	           "developerAttribution":"<b>Dev</b>","publisherAttribution":["Pub","Pub"],
	           "image":{"src":"javascript:alert(1)"}},
	  "hero":{"backgroundImageUrl":"https://evil.example/x.jpg"},
	  "gallery":{"galleryImages":[{"src":"https://cdn2.unrealengine.com.evil.example/a.jpg"},{"src":"http://cdn2.unrealengine.com/plain-http.jpg"},{"src":"https://user:pw@cdn2.unrealengine.com/a.jpg"},{"src":"https://cdn2.unrealengine.com/ok.jpg"}]},
	  "carousel":{"items":[{"video":{"type":"epicHosted","recipes":"{\"en-US\":[{\"recipe\":\"video-hls\",\"outputs\":[{\"key\":\"manifest\",\"url\":\"https://evil.example/a.m3u8\"}]}]}"}}]},
	  "meta":{"customReleaseDate":"<script>2024</script>","releaseDate":"not a date"},
	  "requirements":{"languages":["English<script>alert(1)</script>, Fr&nch"],
	    "systems":[{"systemType":"Windows","details":[
	      {"title":"OS<script>","minimum":"<script>alert('x')</script>","recommended":"\"quoted\" & <b>bold</b>"}]}]},
	  "socialLinks":{"linkHomepage":"javascript:alert(1)"}
	}}]}`
	d, err := ParseEpic("evil", []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	for name, v := range map[string]string{
		"pc min": d.PCReqMinHTML, "pc rec": d.PCReqRecHTML, "languages": d.SupportedLanguages,
	} {
		lower := strings.ToLower(v)
		if strings.Contains(lower, "<script") || strings.Contains(lower, "<b>") || strings.Contains(lower, "<img") {
			t.Errorf("%s keeps raw HTML: %q", name, v)
		}
	}
	if !strings.Contains(d.PCReqMinHTML, "&lt;script&gt;") {
		t.Errorf("pc min should hold the escaped text: %q", d.PCReqMinHTML)
	}
	if !strings.Contains(d.PCReqRecHTML, "&lt;b&gt;bold&lt;/b&gt;") || !strings.Contains(d.PCReqRecHTML, "&amp;") {
		t.Errorf("pc rec = %q", d.PCReqRecHTML)
	}
	if !strings.Contains(d.SupportedLanguages, "Fr&amp;nch") || strings.Contains(d.SupportedLanguages, "<") {
		t.Errorf("languages = %q", d.SupportedLanguages)
	}
	if strings.Contains(d.ShortDescription, "<") || d.ShortDescription != "Nice game" {
		t.Errorf("short description = %q", d.ShortDescription)
	}
	if len(d.Developers) != 1 || d.Developers[0] != "<b>Dev</b>" {
		// Developers are stored as text[] and rendered as text by the site, like Steam's.
		t.Errorf("developers = %v", d.Developers)
	}
	if len(d.Publishers) != 1 {
		t.Errorf("publishers not deduped: %v", d.Publishers)
	}
	if len(d.Screenshots) != 1 || d.Screenshots[0].Full != "https://cdn2.unrealengine.com/ok.jpg" {
		t.Errorf("only the Epic-hosted https image may pass: %+v", d.Screenshots)
	}
	if len(d.Movies) != 0 {
		t.Errorf("a trailer off the Epic hosts must be skipped: %+v", d.Movies)
	}
	if d.Website != "" {
		t.Errorf("website = %q", d.Website)
	}
	if d.ReleaseDateText != "2024" || d.ReleaseDate != nil {
		t.Errorf("release text must be plain: %q", d.ReleaseDateText)
	}
}

func TestShortTextClipsLongDescription(t *testing.T) {
	long := strings.Repeat("word ", 200)
	got := shortText(long)
	if len([]rune(got)) > maxShortDescription || !strings.HasSuffix(got, "...") {
		t.Fatalf("got %d runes: %q", len([]rune(got)), got)
	}
}

func TestFindGOGProductBySlugOnly(t *testing.T) {
	body := readFixture(t, "gog_catalog_alan_wake.json")
	p, ok, err := FindGOGProduct(body, "alan_wake")
	if err != nil || !ok || p.ID != "1207659037" || p.Title != "Alan Wake" {
		t.Fatalf("%+v %t %v", p, ok, err)
	}
	if _, ok, _ := FindGOGProduct(body, "alan"); ok {
		t.Fatal("a partial slug must not match")
	}
}

func TestParseGOGAlanWake(t *testing.T) {
	p, _, _ := FindGOGProduct(readFixture(t, "gog_catalog_alan_wake.json"), "alan_wake")
	d, err := ParseGOG(p, readFixture(t, "gog_v2_1207659037.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d.ShortDescription, "When the wife of the best-selling writer Alan Wake disappears") || strings.Contains(d.ShortDescription, "<") {
		t.Errorf("short description = %q", d.ShortDescription)
	}
	if len(d.Developers) != 1 || d.Developers[0] != "Remedy Entertainment" || len(d.Publishers) != 1 {
		t.Errorf("devs %v pubs %v", d.Developers, d.Publishers)
	}
	if len(d.Genres) != 3 || d.Genres[0].ID != "action" || d.Genres[0].Description != "Action" {
		t.Errorf("genres = %+v", d.Genres)
	}
	if len(d.Screenshots) != 3 {
		t.Fatalf("screenshots = %+v", d.Screenshots)
	}
	s := d.Screenshots[0]
	if !strings.HasPrefix(s.Full, "https://images.gog-statics.com/") || !strings.HasSuffix(s.Full, "_1600.jpg") ||
		!strings.HasSuffix(s.Thumb, "_product_card_screenshot_748.jpg") || strings.Contains(s.Full, "{formatter}") {
		t.Errorf("screenshot = %+v", s)
	}
	if len(d.Movies) != 0 {
		t.Errorf("YouTube trailers have no direct URL and must be skipped: %+v", d.Movies)
	}
	if !d.Platforms.Windows || d.Platforms.Mac {
		t.Errorf("platforms = %+v", d.Platforms)
	}
	if !strings.HasPrefix(d.PCReqMinHTML, `<strong>Minimum:</strong><br><ul class="bb_ul"><li><strong>System:</strong> Windows 7 or later</li>`) {
		t.Errorf("pc min = %q", d.PCReqMinHTML)
	}
	if !strings.HasPrefix(d.PCReqRecHTML, "<strong>Recommended:</strong>") {
		t.Errorf("pc rec = %q", d.PCReqRecHTML)
	}
	if d.ReleaseDate == nil || d.ReleaseDate.Format("2006-01-02") != "2012-01-16" || d.ReleaseDateText != "16 Jan, 2012" {
		t.Errorf("release = %v %q", d.ReleaseDate, d.ReleaseDateText)
	}
	if !strings.Contains(d.SupportedLanguages, "English") || strings.Count(d.SupportedLanguages, "English") != 1 {
		t.Errorf("languages = %q", d.SupportedLanguages)
	}
	boards := map[string]string{}
	for _, r := range d.AgeRatings {
		boards[r.Board] = r.Rating
	}
	if boards["esrb"] != "Teen" || boards["pegi"] != "16" || boards["usk"] != "16" {
		t.Errorf("ratings = %+v", d.AgeRatings)
	}
}

func TestParseGOGRejectsOtherProduct(t *testing.T) {
	p := GOGProduct{ID: "1", Slug: "alan_wake"}
	if _, err := ParseGOG(p, readFixture(t, "gog_v2_1207659037.json")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestParseGOGEscapesRequirements(t *testing.T) {
	p := GOGProduct{ID: "7", Slug: "x"}
	body := `{"description":"<script>x</script>Hi &amp; bye","_embedded":{"product":{"id":7},
	  "supportedOperatingSystems":[{"operatingSystem":{"name":"linux"},"systemRequirements":[
	    {"type":"minimum","requirements":[{"name":"<i>OS</i>:","description":"<a href=javascript:x>Ubuntu</a>"}]}]}],
	  "screenshots":[{"_links":{"self":{"href":"https://evil.example/a_{formatter}.jpg","formatters":["1600"]}}}]}}`
	d, err := ParseGOG(p, []byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(d.LinuxReqMinHTML, "<a") || strings.Contains(d.LinuxReqMinHTML, "<i>") || !strings.Contains(d.LinuxReqMinHTML, "&lt;a href=") {
		t.Errorf("linux min = %q", d.LinuxReqMinHTML)
	}
	if !d.Platforms.Linux || len(d.Screenshots) != 0 {
		t.Errorf("platforms %+v screenshots %+v", d.Platforms, d.Screenshots)
	}
	if d.ShortDescription != "xHi & bye" {
		t.Errorf("short = %q", d.ShortDescription)
	}
}

func TestGOGDetailsSearchesThenReadsProduct(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.Query().Get("query"))
		switch {
		case r.URL.Path == "/catalog" && r.URL.Query().Get("query") == "like:Alan Wake Remastered":
			w.Write([]byte(`{"products":[{"id":"9","slug":"other"}]}`))
		case r.URL.Path == "/catalog":
			w.Write(readFixture(t, "gog_catalog_alan_wake.json"))
		case r.URL.Path == "/v2/games/1207659037":
			w.Write(readFixture(t, "gog_v2_1207659037.json"))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	d, err := testClient(srv.URL).GOGDetails(context.Background(), "alan_wake", "Alan Wake Remastered")
	if err != nil {
		t.Fatal(err)
	}
	if d.ExternalAppID != "alan_wake" || len(paths) != 3 || paths[1] != "/catalog?like:alan wake" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestGOGDetailsNotListedIsNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"products":[]}`))
	}))
	defer srv.Close()
	if _, err := testClient(srv.URL).GOGDetails(context.Background(), "nope_game", "Nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// TestLiveEpicAndGOG reads the real pages. It runs only with GDR_LIVE_STORE_TESTS=1.
func TestLiveEpicAndGOG(t *testing.T) {
	if os.Getenv("GDR_LIVE_STORE_TESTS") != "1" {
		t.Skip("set GDR_LIVE_STORE_TESTS=1 to call Epic and GOG")
	}
	c := NewClient()
	for _, slug := range []string{"alan-wake-2", "hades"} {
		d, err := c.EpicDetails(context.Background(), slug)
		if err != nil {
			t.Fatalf("epic %s: %v", slug, err)
		}
		t.Logf("epic %s: desc=%d devs=%v pubs=%v shots=%d movies=%d win=%t mac=%t pcmin=%d langs=%d release=%q ratings=%d",
			slug, len(d.ShortDescription), d.Developers, d.Publishers, len(d.Screenshots), len(d.Movies),
			d.Platforms.Windows, d.Platforms.Mac, len(d.PCReqMinHTML), len(d.SupportedLanguages), d.ReleaseDateText, len(d.AgeRatings))
	}
	if _, err := c.EpicDetails(context.Background(), "no-such-product-gdr-test"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing epic page: %v", err)
	}
	d, err := c.GOGDetails(context.Background(), "alan_wake", "Alan Wake")
	if err != nil {
		t.Fatalf("gog: %v", err)
	}
	t.Logf("gog alan_wake: desc=%d devs=%v shots=%d pcmin=%d release=%q ratings=%d genres=%d",
		len(d.ShortDescription), d.Developers, len(d.Screenshots), len(d.PCReqMinHTML), d.ReleaseDateText, len(d.AgeRatings), len(d.Genres))
}
