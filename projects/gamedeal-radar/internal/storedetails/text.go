package storedetails

import (
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"gamedeal-radar/daily-check/internal/htmlclean"
	"gamedeal-radar/daily-check/internal/steam"
)

// Limits on what is stored from a store page.
const (
	maxShortDescription = 300 // runes, about the length of a Steam short description
	maxReleaseText      = 64  // game_details.release_date_text is VARCHAR(64)
	maxScreenshots      = 20
	maxMovies           = 6
	maxAgeRatings       = 20
	maxNameLen          = 200
)

// Requirement is one line of a requirements list, for example {"OS", "Windows 10"}.
type Requirement struct {
	Name  string
	Value string
}

// requirementsHTML renders requirement lines in the same HTML shape the Steam details store:
//
//	<strong>Minimum:</strong><br><ul class="bb_ul"><li><strong>OS:</strong> Windows 10</li>...</ul>
//
// Every name and value is plain text from the store and is HTML-escaped; no store HTML is
// ever passed through. The result also goes through htmlclean.Clean. No lines gives "".
func requirementsHTML(heading string, lines []Requirement) string {
	var b strings.Builder
	n := 0
	for _, l := range lines {
		name := strings.TrimSuffix(collapseSpace(l.Name), ":")
		value := collapseSpace(l.Value)
		if value == "" {
			continue
		}
		if n == 0 {
			b.WriteString("<strong>" + html.EscapeString(heading) + ":</strong><br><ul class=\"bb_ul\">")
		}
		n++
		b.WriteString("<li>")
		if name != "" {
			b.WriteString("<strong>" + html.EscapeString(name) + ":</strong> ")
		}
		b.WriteString(html.EscapeString(value))
		b.WriteString("</li>")
	}
	if n == 0 {
		return ""
	}
	b.WriteString("</ul>")
	return htmlclean.Clean(b.String())
}

// languagesHTML renders language names as the escaped comma list the site reads from
// supported_languages_html. Duplicates are dropped, first one wins.
func languagesHTML(names []string) string {
	seen := map[string]bool{}
	var kept []string
	for _, n := range names {
		n = collapseSpace(n)
		key := strings.ToLower(n)
		if n == "" || seen[key] || utf8.RuneCountInString(n) > 60 {
			continue
		}
		seen[key] = true
		kept = append(kept, html.EscapeString(n))
	}
	return strings.Join(kept, ", ")
}

var spaceRun = regexp.MustCompile(`\s+`)

func collapseSpace(s string) string {
	return strings.TrimSpace(spaceRun.ReplaceAllString(s, " "))
}

var (
	tagPattern       = regexp.MustCompile(`<[^>]*>`)
	breakPattern     = regexp.MustCompile(`(?i)<br\s*/?>|</p>|</li>`)
	markdownHeading  = regexp.MustCompile(`(?m)^\s{0,3}#{1,6}\s*`)
	markdownEmphasis = regexp.MustCompile(`\*\*|__`)
	markdownLink     = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	markdownImage    = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
)

// plainFromHTML turns store HTML into plain text, keeping line breaks as "\n".
func plainFromHTML(s string) string {
	s = breakPattern.ReplaceAllString(s, "\n")
	s = tagPattern.ReplaceAllString(s, "")
	return html.UnescapeString(s)
}

// plainFromMarkdown removes the simple Markdown Epic uses (headings, bold, images, links).
func plainFromMarkdown(s string) string {
	s = markdownImage.ReplaceAllString(s, "")
	s = markdownLink.ReplaceAllString(s, "$1")
	s = markdownHeading.ReplaceAllString(s, "")
	s = markdownEmphasis.ReplaceAllString(s, "")
	return tagPattern.ReplaceAllString(s, "")
}

// shortText returns the first non-empty paragraph of plain text, cut to maxShortDescription
// runes at a word boundary. The site renders short_description as text.
func shortText(plain string) string {
	plain = strings.ReplaceAll(plain, "\r\n", "\n")
	var para string
	for _, p := range strings.Split(plain, "\n") {
		if p = collapseSpace(p); p != "" {
			para = p
			break
		}
	}
	return clipRunes(para, maxShortDescription)
}

// clipRunes cuts s to at most max runes, at the last space when there is one, adding "...".
func clipRunes(s string, max int) string {
	s = collapseSpace(s)
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)[:max-3]
	cut := string(r)
	if i := strings.LastIndex(cut, " "); i > max/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " ,.;:") + "..."
}

// releaseText formats a date the way Steam's release_date.date does, e.g. "9 Dec, 2020".
func releaseText(t time.Time) string { return t.Format("2 Jan, 2006") }

// dateOnly keeps the calendar date as written (no time zone shift) and returns it as UTC midnight.
func dateOnly(t time.Time) *time.Time {
	d := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	return &d
}

// safeImage returns u when it is an https URL on one of the allowed host roots, with no user
// info and no port other than 443. Store pages are publisher-edited, so any other URL is dropped.
func safeImage(u string, roots []string) (string, bool) {
	u = strings.TrimSpace(u)
	parsed, err := url.Parse(u)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" {
		return "", false
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return "", false
	}
	host := strings.ToLower(parsed.Hostname())
	for _, root := range roots {
		if host == root || strings.HasSuffix(host, "."+root) {
			return u, true
		}
	}
	return "", false
}

// safeWebsite keeps an absolute http(s) link with a host, as the Steam parser does.
func safeWebsite(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return ""
	}
	return raw
}

// screenshotList dedupes and caps screenshots.
type screenshotList struct {
	seen map[string]bool
	list []steam.Screenshot
}

func (s *screenshotList) add(thumb, full string) {
	if full == "" || len(s.list) >= maxScreenshots {
		return
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[full] {
		return
	}
	s.seen[full] = true
	if thumb == "" {
		thumb = full
	}
	s.list = append(s.list, steam.Screenshot{Thumb: thumb, Full: full})
}

func (s *screenshotList) result() []steam.Screenshot {
	if s.list == nil {
		return []steam.Screenshot{}
	}
	return s.list
}

// cleanNames trims, dedupes and caps a list of developer or publisher names.
func cleanNames(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, n := range in {
		n = collapseSpace(n)
		if n == "" || seen[n] || utf8.RuneCountInString(n) > maxNameLen {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

func sortRatings(r []steam.AgeRating) []steam.AgeRating {
	sort.SliceStable(r, func(i, j int) bool { return r[i].Board < r[j].Board })
	if len(r) > maxAgeRatings {
		r = r[:maxAgeRatings]
	}
	return r
}

// flexStrings decodes either a JSON string or an array of strings.
type flexStrings []string

func (f *flexStrings) UnmarshalJSON(b []byte) error {
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		if one != "" {
			*f = flexStrings{one}
		}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		// An unexpected shape is "no data", not a failed page.
		*f = nil
		return nil
	}
	*f = many
	return nil
}

// emptyDetails returns a Details with every list non-nil, so jsonb columns get [] not null.
func emptyDetails(externalID string) *steam.Details {
	return &steam.Details{
		ExternalAppID: externalID,
		Type:          "game",
		Developers:    []string{},
		Publishers:    []string{},
		Genres:        []steam.Tag{},
		Categories:    []steam.Tag{},
		AgeRatings:    []steam.AgeRating{},
		Screenshots:   []steam.Screenshot{},
		Movies:        []steam.Movie{},
	}
}
