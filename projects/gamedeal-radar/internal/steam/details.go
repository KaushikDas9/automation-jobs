package steam

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"gamedeal-radar/daily-check/internal/htmlclean"
)

// Tag is a Steam genre or category.
type Tag struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// AgeRating is one rating board entry, e.g. ESRB "m".
type AgeRating struct {
	Board       string `json:"board"`
	Rating      string `json:"rating"`
	Descriptors string `json:"descriptors"`
}

// Platforms lists the operating systems a game supports.
type Platforms struct {
	Windows bool `json:"windows"`
	Mac     bool `json:"mac"`
	Linux   bool `json:"linux"`
}

// Screenshot is one store screenshot with both URLs checked against Steam hosts.
type Screenshot struct {
	Thumb string `json:"thumb"`
	Full  string `json:"full"`
}

// Movie is one store trailer. URLs that fail the Steam host check are empty.
type Movie struct {
	Name  string `json:"name"`
	Thumb string `json:"thumb"`
	HLS   string `json:"hls"`
	DASH  string `json:"dash"`
}

// Details is the storefront metadata for one Steam app.
type Details struct {
	ExternalAppID      string
	ShortDescription   string
	ReleaseDateText    string
	ReleaseDate        *time.Time
	ComingSoon         bool
	Developers         []string
	Publishers         []string
	Genres             []Tag
	Categories         []Tag
	MetacriticScore    *int
	MetacriticURL      string
	AgeRatings         []AgeRating
	Platforms          Platforms
	Screenshots        []Screenshot
	Movies             []Movie
	PCReqMinHTML       string
	PCReqRecHTML       string
	MacReqMinHTML      string
	MacReqRecHTML      string
	LinuxReqMinHTML    string
	LinuxReqRecHTML    string
	SupportedLanguages string
	Website            string

	// The requirement and SupportedLanguages fields are HTML already passed through htmlclean.Clean.
	// A requirement that has no list items or visible text (Steam's empty shells) is "".
}

// Reviews is the review summary from the appreviews endpoint.
type Reviews struct {
	Summary  string
	Positive int
	Total    int
}

type rawDetailsEntry struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
}

type rawDetails struct {
	ShortDescription   string          `json:"short_description"`
	Website            string          `json:"website"`
	SupportedLanguages string          `json:"supported_languages"`
	Developers         []string        `json:"developers"`
	Publishers         []string        `json:"publishers"`
	Genres             []rawTag        `json:"genres"`
	Categories         []rawTag        `json:"categories"`
	Screenshots        []rawScreenshot `json:"screenshots"`
	Movies             []rawMovie      `json:"movies"`
	PCRequirements     json.RawMessage `json:"pc_requirements"`
	MacRequirements    json.RawMessage `json:"mac_requirements"`
	LinuxRequirements  json.RawMessage `json:"linux_requirements"`
	Metacritic         json.RawMessage `json:"metacritic"`
	Ratings            json.RawMessage `json:"ratings"`
	Platforms          json.RawMessage `json:"platforms"`
	ReleaseDate        json.RawMessage `json:"release_date"`
}

type rawTag struct {
	// Steam sends genre ids as strings ("3") and category ids as numbers (2).
	ID          json.RawMessage `json:"id"`
	Description string          `json:"description"`
}

type rawScreenshot struct {
	PathThumbnail string `json:"path_thumbnail"`
	PathFull      string `json:"path_full"`
}

type rawMovie struct {
	Name      string `json:"name"`
	Thumbnail string `json:"thumbnail"`
	HLSH264   string `json:"hls_h264"`
	DashH264  string `json:"dash_h264"`
}

type rawPCRequirements struct {
	Minimum     string `json:"minimum"`
	Recommended string `json:"recommended"`
}

type rawMetacritic struct {
	Score *int   `json:"score"`
	URL   string `json:"url"`
}

type rawRating struct {
	Rating      string `json:"rating"`
	Descriptors string `json:"descriptors"`
}

type rawPlatforms struct {
	Windows bool `json:"windows"`
	Mac     bool `json:"mac"`
	Linux   bool `json:"linux"`
}

type rawReleaseDate struct {
	ComingSoon bool   `json:"coming_soon"`
	Date       string `json:"date"`
}

// ParseDetails converts an appdetails?appids=... body into Details.
// It returns ErrNotFound when the entry is missing, success is false, or data is absent.
func ParseDetails(appID string, body []byte) (*Details, error) {
	var parsed map[string]rawDetailsEntry
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshaling steam details: %w", err)
	}

	entry, ok := parsed[appID]
	if !ok || !entry.Success || isEmptyJSON(entry.Data) {
		return nil, ErrNotFound
	}

	var raw rawDetails
	if err := json.Unmarshal(entry.Data, &raw); err != nil {
		return nil, fmt.Errorf("unmarshaling steam details data: %w", err)
	}

	d := &Details{
		ExternalAppID:      appID,
		ShortDescription:   plainText(raw.ShortDescription),
		Developers:         nonNilStrings(raw.Developers),
		Publishers:         nonNilStrings(raw.Publishers),
		Genres:             convertTags(raw.Genres),
		Categories:         convertTags(raw.Categories),
		Screenshots:        convertScreenshots(raw.Screenshots),
		Movies:             convertMovies(raw.Movies),
		Website:            safeWebsite(raw.Website),
		SupportedLanguages: htmlclean.Clean(raw.SupportedLanguages),
	}

	var req rawPCRequirements
	if err := decodeObject(raw.PCRequirements, &req); err != nil {
		return nil, fmt.Errorf("unmarshaling pc_requirements: %w", err)
	}
	d.PCReqMinHTML = requirementHTML(req.Minimum)
	d.PCReqRecHTML = requirementHTML(req.Recommended)

	// macOS and Linux requirements are a bonus: a malformed block means "no data for that platform",
	// it must not stop the rest of the game's details from being saved.
	var mac rawPCRequirements
	if err := decodeObject(raw.MacRequirements, &mac); err != nil {
		mac = rawPCRequirements{}
	}
	d.MacReqMinHTML = requirementHTML(mac.Minimum)
	d.MacReqRecHTML = requirementHTML(mac.Recommended)

	var linux rawPCRequirements
	if err := decodeObject(raw.LinuxRequirements, &linux); err != nil {
		linux = rawPCRequirements{}
	}
	d.LinuxReqMinHTML = requirementHTML(linux.Minimum)
	d.LinuxReqRecHTML = requirementHTML(linux.Recommended)

	var meta rawMetacritic
	if err := decodeObject(raw.Metacritic, &meta); err != nil {
		return nil, fmt.Errorf("unmarshaling metacritic: %w", err)
	}
	d.MetacriticScore = meta.Score
	d.MetacriticURL = strings.TrimSpace(meta.URL)

	var ratings map[string]rawRating
	if err := decodeObject(raw.Ratings, &ratings); err != nil {
		return nil, fmt.Errorf("unmarshaling ratings: %w", err)
	}
	d.AgeRatings = convertAgeRatings(ratings)

	var plats rawPlatforms
	if err := decodeObject(raw.Platforms, &plats); err != nil {
		return nil, fmt.Errorf("unmarshaling platforms: %w", err)
	}
	d.Platforms = Platforms{Windows: plats.Windows, Mac: plats.Mac, Linux: plats.Linux}

	var release rawReleaseDate
	if err := decodeObject(raw.ReleaseDate, &release); err != nil {
		return nil, fmt.Errorf("unmarshaling release_date: %w", err)
	}
	d.ReleaseDateText = strings.TrimSpace(release.Date)
	d.ReleaseDate = parseReleaseDate(d.ReleaseDateText)
	d.ComingSoon = release.ComingSoon

	return d, nil
}

// ParseReviews converts an appreviews?json=1 body into Reviews.
func ParseReviews(body []byte) (*Reviews, error) {
	var parsed struct {
		Success      int `json:"success"`
		QuerySummary struct {
			ReviewScoreDesc string `json:"review_score_desc"`
			TotalPositive   int    `json:"total_positive"`
			TotalReviews    int    `json:"total_reviews"`
		} `json:"query_summary"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshaling steam reviews: %w", err)
	}
	if parsed.Success != 1 {
		return nil, fmt.Errorf("steam reviews returned success=%d", parsed.Success)
	}

	return &Reviews{
		Summary:  strings.TrimSpace(parsed.QuerySummary.ReviewScoreDesc),
		Positive: parsed.QuerySummary.TotalPositive,
		Total:    parsed.QuerySummary.TotalReviews,
	}, nil
}

var htmlTagPattern = regexp.MustCompile(`<[^>]*>`)

// plainText turns Steam's entity-encoded text into plain text with no tags.
func plainText(s string) string {
	s = html.UnescapeString(s)
	s = htmlTagPattern.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

// requirementHTML cleans one requirements block and drops it when it has no content,
// so Steam's empty shells (a heading and an empty list) become "".
func requirementHTML(s string) string {
	clean := htmlclean.Clean(s)
	if !hasRequirementItems(clean) {
		return ""
	}
	return clean
}

var (
	requirementListItem = regexp.MustCompile(`(?i)<li[\s>/]`)
	requirementHeading  = regexp.MustCompile(`(?i)^\s*(minimum|recommended)\s*:\s*`)
)

// hasRequirementItems reports whether cleaned requirements HTML has at least one <li>
// element or visible text besides the "Minimum:" / "Recommended:" heading.
func hasRequirementItems(cleanHTML string) bool {
	if requirementListItem.MatchString(cleanHTML) {
		return true
	}
	text := html.UnescapeString(htmlTagPattern.ReplaceAllString(cleanHTML, ""))
	text = requirementHeading.ReplaceAllString(strings.TrimSpace(text), "")
	return strings.TrimSpace(text) != ""
}

// isEmptyJSON reports whether a raw value is absent, null, or an array.
// Steam sends [] where it has no object, so arrays count as empty for object fields.
func isEmptyJSON(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null" || t[0] == '['
}

// decodeObject fills v from an object-valued field, leaving it zero when Steam sent
// nothing or an empty array instead of an object.
func decodeObject(raw json.RawMessage, v any) error {
	if isEmptyJSON(raw) {
		return nil
	}
	return json.Unmarshal(raw, v)
}

// rawIDString returns a Steam id as a string whether it was sent as a number or a string.
func rawIDString(raw json.RawMessage) string {
	return strings.Trim(strings.TrimSpace(string(raw)), `"`)
}

func nonNilStrings(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, strings.TrimSpace(s))
	}
	return out
}

func convertTags(in []rawTag) []Tag {
	out := make([]Tag, 0, len(in))
	for _, t := range in {
		out = append(out, Tag{ID: rawIDString(t.ID), Description: strings.TrimSpace(t.Description)})
	}
	return out
}

func convertScreenshots(in []rawScreenshot) []Screenshot {
	out := make([]Screenshot, 0, len(in))
	for _, s := range in {
		thumb, okThumb := htmlclean.SafeSteamURL(s.PathThumbnail)
		full, okFull := htmlclean.SafeSteamURL(s.PathFull)
		if !okThumb || !okFull {
			continue
		}
		out = append(out, Screenshot{Thumb: thumb, Full: full})
	}
	return out
}

func convertMovies(in []rawMovie) []Movie {
	out := make([]Movie, 0, len(in))
	for _, m := range in {
		out = append(out, Movie{
			Name:  strings.TrimSpace(m.Name),
			Thumb: safeURLOrEmpty(m.Thumbnail),
			HLS:   safeURLOrEmpty(m.HLSH264),
			DASH:  safeURLOrEmpty(m.DashH264),
		})
	}
	return out
}

func safeURLOrEmpty(u string) string {
	safe, ok := htmlclean.SafeSteamURL(u)
	if !ok {
		return ""
	}
	return safe
}

// convertAgeRatings returns the ratings sorted by board name, so output is deterministic.
// Boards without a rating are dropped.
func convertAgeRatings(in map[string]rawRating) []AgeRating {
	out := make([]AgeRating, 0, len(in))
	for board, r := range in {
		if strings.TrimSpace(r.Rating) == "" {
			continue
		}
		out = append(out, AgeRating{
			Board:       board,
			Rating:      strings.TrimSpace(r.Rating),
			Descriptors: joinDescriptors(r.Descriptors),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Board < out[j].Board })
	return out
}

// joinDescriptors turns Steam's newline-separated descriptor text into a comma-separated list.
func joinDescriptors(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	parts := strings.Split(s, "\n")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, ", ")
}

// releaseDateLayouts are the formats Steam uses for release_date.date, e.g. "9 Dec, 2020".
var releaseDateLayouts = []string{
	"2 Jan, 2006",
	"Jan 2, 2006",
	"2 Jan 2006",
	"2 January, 2006",
	"January 2, 2006",
}

// parseReleaseDate returns the date in UTC, or nil for text such as "Coming soon".
func parseReleaseDate(s string) *time.Time {
	for _, layout := range releaseDateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			t = t.UTC()
			return &t
		}
	}
	return nil
}

// safeWebsite returns the game's own website only when it is an absolute http(s) URL with a
// host. Publishers control this field, so anything else (javascript:, data:, relative) is dropped.
func safeWebsite(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return raw
}
