package steam

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"gamedeal-radar/daily-check/internal/htmlclean"
)

// fixtureData returns the "data" object of a real appdetails fixture, keyed by app id.
func fixtureData(t *testing.T, file, appID string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", file, err)
	}
	var doc map[string]struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("decoding fixture %s: %v", file, err)
	}
	return doc[appID].Data
}

// fixtureScreenshots returns the raw path_thumbnail and path_full pairs from a fixture.
func fixtureScreenshots(t *testing.T, file, appID string) [][2]string {
	t.Helper()
	var out [][2]string
	for _, s := range fixtureData(t, file, appID)["screenshots"].([]any) {
		m := s.(map[string]any)
		out = append(out, [2]string{m["path_thumbnail"].(string), m["path_full"].(string)})
	}
	return out
}

func mustReadFixture(t *testing.T, file string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + file)
	if err != nil {
		t.Fatalf("reading fixture %s: %v", file, err)
	}
	return raw
}

func TestParseDetails_Fixtures(t *testing.T) {
	cyberpunk := fixtureData(t, "full_1091500.json", "1091500")
	cyberpunkMin, _ := cyberpunk["pc_requirements"].(map[string]any)["minimum"].(string)
	cyberpunkRec, _ := cyberpunk["pc_requirements"].(map[string]any)["recommended"].(string)
	cyberpunkLangs, _ := cyberpunk["supported_languages"].(string)

	elden := fixtureData(t, "full_1245620.json", "1245620")
	eldenMin, _ := elden["pc_requirements"].(map[string]any)["minimum"].(string)
	eldenRec, _ := elden["pc_requirements"].(map[string]any)["recommended"].(string)

	cs2 := fixtureData(t, "full_730.json", "730")
	cs2Min, _ := cs2["pc_requirements"].(map[string]any)["minimum"].(string)

	cases := []struct {
		name  string
		file  string
		appID string
		check func(t *testing.T, d *Details)
	}{
		{
			name:  "cyberpunk with metacritic and ratings",
			file:  "full_1091500.json",
			appID: "1091500",
			check: func(t *testing.T, d *Details) {
				if d.ExternalAppID != "1091500" {
					t.Errorf("ExternalAppID = %q", d.ExternalAppID)
				}
				if d.ShortDescription == "" {
					t.Error("ShortDescription is empty")
				}
				if d.ReleaseDateText != "9 Dec, 2020" {
					t.Errorf("ReleaseDateText = %q", d.ReleaseDateText)
				}
				want := time.Date(2020, 12, 9, 0, 0, 0, 0, time.UTC)
				if d.ReleaseDate == nil || !d.ReleaseDate.Equal(want) {
					t.Errorf("ReleaseDate = %v, want %v", d.ReleaseDate, want)
				}
				if d.ComingSoon {
					t.Error("ComingSoon = true, want false")
				}
				if d.MetacriticScore == nil || *d.MetacriticScore != 86 {
					t.Errorf("MetacriticScore = %v, want 86", d.MetacriticScore)
				}
				if d.MetacriticURL != "https://www.metacritic.com/game/pc/cyberpunk-2077?ftag=MCD-06-10aaa1f" {
					t.Errorf("MetacriticURL = %q", d.MetacriticURL)
				}
				if !reflect.DeepEqual(d.Genres, []Tag{{ID: "3", Description: "RPG"}}) {
					t.Errorf("Genres = %+v", d.Genres)
				}
				if len(d.Categories) == 0 || d.Categories[0] != (Tag{ID: "2", Description: "Single-player"}) {
					t.Errorf("Categories[0] = %+v, want numeric id 2 as string", d.Categories)
				}
				if !reflect.DeepEqual(d.Developers, []string{"CD PROJEKT RED"}) {
					t.Errorf("Developers = %v", d.Developers)
				}
				if d.Platforms != (Platforms{Windows: true, Mac: true, Linux: false}) {
					t.Errorf("Platforms = %+v", d.Platforms)
				}
				if d.Website != "https://www.cyberpunk.net" {
					t.Errorf("Website = %q", d.Website)
				}
				wantRatings := []AgeRating{
					{Board: "esrb", Rating: "m", Descriptors: "Blood and Gore, Intense Violence, Nudity, Strong Language, Strong Sexual Content, Use of Drugs, Use of Alcohol"},
				}
				for _, r := range d.AgeRatings {
					if r.Board == "esrb" && r != wantRatings[0] {
						t.Errorf("esrb rating = %+v, want %+v", r, wantRatings[0])
					}
				}
				for i := 1; i < len(d.AgeRatings); i++ {
					if d.AgeRatings[i-1].Board >= d.AgeRatings[i].Board {
						t.Errorf("AgeRatings not sorted by board: %+v", d.AgeRatings)
					}
				}
				if len(d.AgeRatings) < 2 {
					t.Errorf("AgeRatings = %+v, want several boards", d.AgeRatings)
				}
				if d.PCReqMinHTML != htmlclean.Clean(cyberpunkMin) {
					t.Error("PCReqMinHTML is not htmlclean.Clean(minimum)")
				}
				if d.PCReqRecHTML != htmlclean.Clean(cyberpunkRec) {
					t.Error("PCReqRecHTML is not htmlclean.Clean(recommended)")
				}
				if d.SupportedLanguages != htmlclean.Clean(cyberpunkLangs) {
					t.Error("SupportedLanguages is not htmlclean.Clean(supported_languages)")
				}
				if len(d.Movies) != len(cyberpunk["movies"].([]any)) {
					t.Errorf("Movies: got %d, want %d (movies are kept even when URLs fail)", len(d.Movies), len(cyberpunk["movies"].([]any)))
				}
				checkMoviesAgainstFixture(t, d.Movies, cyberpunk["movies"].([]any))
				checkScreenshots(t, d.Screenshots, fixtureScreenshots(t, "full_1091500.json", "1091500"))
			},
		},
		{
			name:  "counter-strike free to play without metacritic",
			file:  "full_730.json",
			appID: "730",
			check: func(t *testing.T, d *Details) {
				if d.MetacriticScore != nil || d.MetacriticURL != "" {
					t.Errorf("metacritic = %v %q, want none", d.MetacriticScore, d.MetacriticURL)
				}
				if d.Platforms != (Platforms{Windows: true, Mac: false, Linux: true}) {
					t.Errorf("Platforms = %+v", d.Platforms)
				}
				if d.PCReqMinHTML != htmlclean.Clean(cs2Min) {
					t.Error("PCReqMinHTML is not htmlclean.Clean(minimum)")
				}
				if d.ReleaseDate == nil || !d.ReleaseDate.Equal(time.Date(2012, 8, 21, 0, 0, 0, 0, time.UTC)) {
					t.Errorf("ReleaseDate = %v", d.ReleaseDate)
				}
				if len(d.Genres) != 2 || d.Genres[1] != (Tag{ID: "37", Description: "Free To Play"}) {
					t.Errorf("Genres = %+v", d.Genres)
				}
				checkScreenshots(t, d.Screenshots, fixtureScreenshots(t, "full_730.json", "730"))
			},
		},
		{
			name:  "elden ring with null website and two publishers",
			file:  "full_1245620.json",
			appID: "1245620",
			check: func(t *testing.T, d *Details) {
				if d.Website != "" {
					t.Errorf("Website = %q, want empty for null", d.Website)
				}
				if d.MetacriticScore == nil || *d.MetacriticScore != 94 {
					t.Errorf("MetacriticScore = %v, want 94", d.MetacriticScore)
				}
				if !reflect.DeepEqual(d.Publishers, []string{"FromSoftware, Inc.", "Bandai Namco Entertainment"}) {
					t.Errorf("Publishers = %v", d.Publishers)
				}
				if d.PCReqMinHTML != htmlclean.Clean(eldenMin) {
					t.Error("PCReqMinHTML is not htmlclean.Clean(minimum)")
				}
				if d.PCReqRecHTML != htmlclean.Clean(eldenRec) {
					t.Error("PCReqRecHTML is not htmlclean.Clean(recommended)")
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseDetails(tc.appID, mustReadFixture(t, tc.file))
			if err != nil {
				t.Fatalf("ParseDetails: %v", err)
			}
			tc.check(t, d)
		})
	}
}

// checkMoviesAgainstFixture verifies each movie URL equals htmlclean.SafeSteamURL's answer,
// or "" when that check fails.
func checkMoviesAgainstFixture(t *testing.T, got []Movie, raw []any) {
	t.Helper()
	for i, r := range raw {
		m := r.(map[string]any)
		if i >= len(got) {
			t.Fatalf("missing movie %d", i)
		}
		want := Movie{
			Name:  m["name"].(string),
			Thumb: safeOrEmpty(m["thumbnail"].(string)),
			HLS:   safeOrEmpty(m["hls_h264"].(string)),
			DASH:  safeOrEmpty(m["dash_h264"].(string)),
		}
		if got[i] != want {
			t.Errorf("movie %d = %+v, want %+v", i, got[i], want)
		}
	}
}

// checkScreenshots verifies that exactly the screenshots whose two URLs pass
// htmlclean.SafeSteamURL are kept, with the URLs as the checker returned them.
func checkScreenshots(t *testing.T, got []Screenshot, raw [][2]string) {
	t.Helper()
	var want []Screenshot
	for _, pair := range raw {
		thumb, okThumb := htmlclean.SafeSteamURL(pair[0])
		full, okFull := htmlclean.SafeSteamURL(pair[1])
		if okThumb && okFull {
			want = append(want, Screenshot{Thumb: thumb, Full: full})
		}
	}
	if len(want) == 0 {
		want = []Screenshot{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Screenshots = %+v, want %+v", got, want)
	}
}

func safeOrEmpty(u string) string {
	if s, ok := htmlclean.SafeSteamURL(u); ok {
		return s
	}
	return ""
}

func TestParseDetails_NotFound(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"delisted success false", `{"999":{"success":false}}`},
		{"missing app id", `{"12":{"success":true,"data":{"name":"x"}}}`},
		{"data missing", `{"999":{"success":true}}`},
		{"data empty array", `{"999":{"success":true,"data":[]}}`},
		{"data null", `{"999":{"success":true,"data":null}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseDetails("999", []byte(tc.body))
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("err = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestParseDetails_InvalidJSON(t *testing.T) {
	_, err := ParseDetails("1", []byte(`{"1":`))
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want a decode error", err)
	}
}

func TestParseDetails_EdgeCases(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, d *Details)
	}{
		{
			name: "empty arrays and objects are zero values",
			body: `{"5":{"success":true,"data":{
				"short_description":"x",
				"pc_requirements":[],"metacritic":[],"ratings":[],"platforms":[],"release_date":[],
				"developers":[],"genres":[],"categories":[],"screenshots":[],"movies":[]}}}`,
			check: func(t *testing.T, d *Details) {
				if d.PCReqMinHTML != htmlclean.Clean("") || d.PCReqRecHTML != htmlclean.Clean("") {
					t.Error("pc requirements should be Clean(\"\")")
				}
				if d.MetacriticScore != nil || d.MetacriticURL != "" {
					t.Error("metacritic should be empty")
				}
				if d.ReleaseDate != nil || d.ReleaseDateText != "" {
					t.Error("release date should be empty")
				}
				if d.Platforms != (Platforms{}) {
					t.Error("platforms should be zero")
				}
			},
		},
		{
			name: "plain text short description unescaped and tags stripped",
			// Entities are decoded first, then tags are stripped, so &lt;3&gt; never survives as markup.
			body: `{"5":{"success":true,"data":{"short_description":"  <b>Fast</b> &amp; furious &lt;3&gt;  "}}}`,
			check: func(t *testing.T, d *Details) {
				if d.ShortDescription != "Fast & furious" {
					t.Errorf("ShortDescription = %q", d.ShortDescription)
				}
			},
		},
		{
			name: "release date formats",
			body: `{"5":{"success":true,"data":{"release_date":{"coming_soon":false,"date":"Dec 9, 2020"}}}}`,
			check: func(t *testing.T, d *Details) {
				want := time.Date(2020, 12, 9, 0, 0, 0, 0, time.UTC)
				if d.ReleaseDate == nil || !d.ReleaseDate.Equal(want) {
					t.Errorf("ReleaseDate = %v, want %v", d.ReleaseDate, want)
				}
				if d.ReleaseDateText != "Dec 9, 2020" {
					t.Errorf("ReleaseDateText = %q", d.ReleaseDateText)
				}
			},
		},
		{
			name: "coming soon has no parsed date but keeps text",
			body: `{"5":{"success":true,"data":{"release_date":{"coming_soon":true,"date":"Coming soon"}}}}`,
			check: func(t *testing.T, d *Details) {
				if d.ReleaseDate != nil {
					t.Errorf("ReleaseDate = %v, want nil", d.ReleaseDate)
				}
				if d.ReleaseDateText != "Coming soon" || !d.ComingSoon {
					t.Errorf("text=%q coming=%t", d.ReleaseDateText, d.ComingSoon)
				}
			},
		},
		{
			name: "to be announced has no parsed date",
			body: `{"5":{"success":true,"data":{"release_date":{"coming_soon":false,"date":"To be announced"}}}}`,
			check: func(t *testing.T, d *Details) {
				if d.ReleaseDate != nil || d.ReleaseDateText != "To be announced" {
					t.Errorf("ReleaseDate=%v text=%q", d.ReleaseDate, d.ReleaseDateText)
				}
			},
		},
		{
			name: "descriptors split on crlf and lf, trimmed, empties dropped",
			body: `{"5":{"success":true,"data":{"ratings":{"esrb":{"rating":"m","descriptors":"Blood\r\n\r\n  Violence \n\nLanguage\n"}}}}}`,
			check: func(t *testing.T, d *Details) {
				want := []AgeRating{{Board: "esrb", Rating: "m", Descriptors: "Blood, Violence, Language"}}
				if !reflect.DeepEqual(d.AgeRatings, want) {
					t.Errorf("AgeRatings = %+v, want %+v", d.AgeRatings, want)
				}
			},
		},
		{
			name: "age ratings sorted by board",
			body: `{"5":{"success":true,"data":{"ratings":{"pegi":{"rating":"18"},"esrb":{"rating":"m"},"usk":{"rating":"16"}}}}}`,
			check: func(t *testing.T, d *Details) {
				var boards []string
				for _, r := range d.AgeRatings {
					boards = append(boards, r.Board)
				}
				if !reflect.DeepEqual(boards, []string{"esrb", "pegi", "usk"}) {
					t.Errorf("boards = %v", boards)
				}
			},
		},
		{
			name: "metacritic score of zero is still a score",
			body: `{"5":{"success":true,"data":{"metacritic":{"score":0,"url":""}}}}`,
			check: func(t *testing.T, d *Details) {
				if d.MetacriticScore == nil || *d.MetacriticScore != 0 {
					t.Errorf("MetacriticScore = %v, want pointer to 0", d.MetacriticScore)
				}
			},
		},
		{
			name: "metacritic score absent is nil",
			body: `{"5":{"success":true,"data":{"metacritic":{"url":"https://www.metacritic.com/x"}}}}`,
			check: func(t *testing.T, d *Details) {
				if d.MetacriticScore != nil {
					t.Errorf("MetacriticScore = %v, want nil", *d.MetacriticScore)
				}
			},
		},
		{
			name: "unsafe screenshot urls drop the whole screenshot",
			body: `{"5":{"success":true,"data":{"screenshots":[
				{"path_thumbnail":"javascript:alert(1)","path_full":"https://cdn.steamstatic.com/a.jpg"},
				{"path_thumbnail":"http://evil.example/t.jpg","path_full":"http://evil.example/f.jpg"},
				{"path_thumbnail":"https://cdn.steamstatic.com/t.jpg","path_full":"https://evil.example/f.jpg"}
			]}}}`,
			check: func(t *testing.T, d *Details) {
				if len(d.Screenshots) != 0 {
					t.Errorf("Screenshots = %+v, want none", d.Screenshots)
				}
				if d.Screenshots == nil {
					t.Error("Screenshots should be a non-nil empty slice")
				}
			},
		},
		{
			name: "unsafe movie urls become empty strings",
			body: `{"5":{"success":true,"data":{"movies":[
				{"name":"Trailer","thumbnail":"javascript:alert(1)","hls_h264":"http://evil.example/a.m3u8","dash_h264":"javascript:x"}
			]}}}`,
			check: func(t *testing.T, d *Details) {
				want := []Movie{{Name: "Trailer"}}
				if !reflect.DeepEqual(d.Movies, want) {
					t.Errorf("Movies = %+v, want %+v", d.Movies, want)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := ParseDetails("5", []byte(tc.body))
			if err != nil {
				t.Fatalf("ParseDetails: %v", err)
			}
			tc.check(t, d)
		})
	}
}

func TestParseDetails_SlicesMarshalAsArrays(t *testing.T) {
	d, err := ParseDetails("5", []byte(`{"5":{"success":true,"data":{"name":"bare"}}}`))
	if err != nil {
		t.Fatalf("ParseDetails: %v", err)
	}
	out, err := json.Marshal(d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(out, &fields); err != nil {
		t.Fatalf("decode marshaled: %v", err)
	}
	// Go field names are used by Details itself; the API layer maps them.
	for _, name := range []string{"Developers", "Publishers", "Genres", "Categories", "AgeRatings", "Screenshots", "Movies"} {
		raw, ok := fields[name]
		if !ok {
			t.Errorf("%s missing from JSON", name)
			continue
		}
		if string(raw) != "[]" {
			t.Errorf("%s = %s, want []", name, raw)
		}
	}
	if string(fields["MetacriticScore"]) != "null" {
		t.Errorf("MetacriticScore = %s, want null", fields["MetacriticScore"])
	}
}

func TestParseReviews(t *testing.T) {
	t.Run("fixture", func(t *testing.T) {
		r, err := ParseReviews(mustReadFixture(t, "reviews_1091500.json"))
		if err != nil {
			t.Fatalf("ParseReviews: %v", err)
		}
		want := Reviews{Summary: "Very Positive", Positive: 778190, Total: 889010}
		if *r != want {
			t.Errorf("got %+v, want %+v", *r, want)
		}
	})

	errCases := []struct {
		name string
		body string
	}{
		{"success zero", `{"success":0,"query_summary":{}}`},
		{"invalid json", `{"success":1,`},
	}
	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseReviews([]byte(tc.body)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// fixtureRequirement returns the minimum and recommended strings of one requirements field.
func fixtureRequirement(t *testing.T, file, appID, field string) (string, string, bool) {
	t.Helper()
	obj, ok := fixtureData(t, file, appID)[field].(map[string]any)
	if !ok {
		return "", "", false
	}
	min, _ := obj["minimum"].(string)
	rec, _ := obj["recommended"].(string)
	return min, rec, true
}

func TestParseDetails_MacRequirementsWithItems(t *testing.T) {
	d, err := ParseDetails("1091500", mustReadFixture(t, "full_1091500.json"))
	if err != nil {
		t.Fatal(err)
	}
	min, rec, ok := fixtureRequirement(t, "full_1091500.json", "1091500", "mac_requirements")
	if !ok {
		t.Fatal("fixture has no mac_requirements object")
	}
	if d.MacReqMinHTML != htmlclean.Clean(min) || d.MacReqMinHTML == "" {
		t.Errorf("MacReqMinHTML = %q, want Clean(minimum)", d.MacReqMinHTML)
	}
	if d.MacReqRecHTML != htmlclean.Clean(rec) || d.MacReqRecHTML == "" {
		t.Errorf("MacReqRecHTML = %q, want Clean(recommended)", d.MacReqRecHTML)
	}
	if !strings.Contains(d.MacReqMinHTML, "<li>") {
		t.Errorf("MacReqMinHTML lost its list items: %q", d.MacReqMinHTML)
	}
}

func TestParseDetails_EmptyShellRequirementsBecomeEmpty(t *testing.T) {
	// Cyberpunk: Linux is an empty shell on both levels.
	cyber, err := ParseDetails("1091500", mustReadFixture(t, "full_1091500.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cyber.LinuxReqMinHTML != "" || cyber.LinuxReqRecHTML != "" {
		t.Errorf("Cyberpunk linux = %q / %q, want empty", cyber.LinuxReqMinHTML, cyber.LinuxReqRecHTML)
	}

	// Elden Ring: Mac and Linux are empty shells, PC is real.
	elden, err := ParseDetails("1245620", mustReadFixture(t, "full_1245620.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"MacReqMinHTML":   elden.MacReqMinHTML,
		"MacReqRecHTML":   elden.MacReqRecHTML,
		"LinuxReqMinHTML": elden.LinuxReqMinHTML,
		"LinuxReqRecHTML": elden.LinuxReqRecHTML,
	} {
		if got != "" {
			t.Errorf("Elden Ring %s = %q, want empty", name, got)
		}
	}
	if elden.PCReqMinHTML == "" {
		t.Error("Elden Ring PCReqMinHTML should still be set")
	}
}

func TestParseDetails_MinimumOnlyOnAllPlatforms(t *testing.T) {
	d, err := ParseDetails("730", mustReadFixture(t, "full_730.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"pc_requirements", "mac_requirements", "linux_requirements"} {
		min, _, ok := fixtureRequirement(t, "full_730.json", "730", field)
		if !ok {
			t.Fatalf("fixture has no %s object", field)
		}
		var got string
		switch field {
		case "pc_requirements":
			got = d.PCReqMinHTML
		case "mac_requirements":
			got = d.MacReqMinHTML
		case "linux_requirements":
			got = d.LinuxReqMinHTML
		}
		if got == "" || got != htmlclean.Clean(min) {
			t.Errorf("%s minimum = %q, want Clean(minimum)", field, got)
		}
	}
}

func TestParseDetails_EmptyArrayRequirementsAreEmpty(t *testing.T) {
	body := []byte(`{"9":{"success":true,"data":{
		"pc_requirements":{"minimum":"<ul class=\"bb_ul\"><li>OS: Windows 10</li></ul>"},
		"mac_requirements":[],
		"linux_requirements":[]}}}`)
	d, err := ParseDetails("9", body)
	if err != nil {
		t.Fatalf("ParseDetails: %v", err)
	}
	if d.MacReqMinHTML != "" || d.MacReqRecHTML != "" || d.LinuxReqMinHTML != "" || d.LinuxReqRecHTML != "" {
		t.Errorf("array requirements should be empty: %+v", d)
	}
	if d.PCReqMinHTML == "" {
		t.Error("PC minimum should still be parsed next to [] fields")
	}
}

func TestParseDetails_MacRequirementsAreCleaned(t *testing.T) {
	body := []byte(`{"9":{"success":true,"data":{
		"mac_requirements":{"minimum":"<script>alert(1)</script><ul class=\"bb_ul\"><li><strong>OS:</strong> macOS 14<br></li></ul><img src=x onerror=alert(2)>"}}}}`)
	d, err := ParseDetails("9", body)
	if err != nil {
		t.Fatalf("ParseDetails: %v", err)
	}
	if strings.Contains(strings.ToLower(d.MacReqMinHTML), "<script") || strings.Contains(d.MacReqMinHTML, "alert") {
		t.Errorf("hostile markup survived: %q", d.MacReqMinHTML)
	}
	if strings.Contains(d.MacReqMinHTML, "<img") || strings.Contains(d.MacReqMinHTML, "onerror") {
		t.Errorf("img/handler survived: %q", d.MacReqMinHTML)
	}
	if !strings.Contains(d.MacReqMinHTML, "macOS 14") {
		t.Errorf("real list item was dropped: %q", d.MacReqMinHTML)
	}
}

func TestHasRequirementItems(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty string", "", false},
		{"heading and empty list", `<strong>Minimum:</strong><br><ul class="bb_ul"></ul>`, false},
		{"heading only", "<strong>Recommended:</strong>", false},
		{"whitespace only", "  <br>  ", false},
		{"list item", `<ul class="bb_ul"><li>OS: Windows 10</li></ul>`, true},
		{"empty list item still counts as an element", `<ul><li></li></ul>`, true},
		{"visible text without a list", "<strong>Minimum:</strong><br>Requires a PC", true},
		{"plain text", "Windows 10", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hasRequirementItems(tc.in); got != tc.want {
				t.Errorf("hasRequirementItems(%q) = %t, want %t", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseDetailsMalformedPlatformRequirementsDoNotFailTheGame(t *testing.T) {
	body := []byte(`{"1":{"success":true,"data":{"name":"X","short_description":"d",` +
		`"pc_requirements":{"minimum":"<strong>Minimum:</strong><br><ul class=\"bb_ul\"><li><strong>OS:</strong> Windows 10</li></ul>"},` +
		`"mac_requirements":"not an object","linux_requirements":12}}}`)
	d, err := ParseDetails("1", body)
	if err != nil {
		t.Fatalf("a malformed mac/linux block must not fail the game: %v", err)
	}
	if d.PCReqMinHTML == "" {
		t.Fatal("the PC requirements should still be kept")
	}
	if d.MacReqMinHTML != "" || d.LinuxReqMinHTML != "" {
		t.Fatalf("malformed platform blocks should be empty, got mac=%q linux=%q", d.MacReqMinHTML, d.LinuxReqMinHTML)
	}
}

func TestParseDetailsKeepsAppType(t *testing.T) {
	body := []byte(`{"620":{"success":true,"data":{"type":" Game ","short_description":"x"}}}`)
	d, err := ParseDetails("620", body)
	if err != nil || d.Type != "game" {
		t.Fatalf("type = %q, err = %v", d.Type, err)
	}
}
