package storedetails

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"
	"time"

	"gamedeal-radar/daily-check/internal/steam"
)

// epicImageRoots are the hosts Epic serves store images and trailers from.
var epicImageRoots = []string{"unrealengine.com", "epicgames.com"}

// EpicDetails fetches the Epic product page of slug and maps it. ErrNotFound when Epic has no
// such page or the page has no product name; ErrInvalidID when the slug is not valid.
func (c *Client) EpicDetails(ctx context.Context, slug string) (*steam.Details, error) {
	if !ValidEpicSlug(slug) {
		return nil, ErrInvalidID
	}
	body, err := c.get(ctx, c.epicBase+slug, "epic product page")
	if err != nil {
		return nil, err
	}
	return ParseEpic(slug, body)
}

type epicImage struct {
	Src string `json:"src"`
}

type epicRatings struct {
	Ratings []struct {
		CountryCodes string `json:"countryCodes"`
		Title        string `json:"title"`
	} `json:"ratings"`
}

type epicVideo struct {
	Title   string          `json:"title"`
	Type    string          `json:"type"`
	Recipes json.RawMessage `json:"recipes"`
}

type epicPageData struct {
	About struct {
		Description          string      `json:"description"`
		ShortDescription     string      `json:"shortDescription"`
		DeveloperAttribution flexStrings `json:"developerAttribution"`
		PublisherAttribution flexStrings `json:"publisherAttribution"`
		Image                epicImage   `json:"image"`
	} `json:"about"`
	Hero struct {
		BackgroundImageURL string `json:"backgroundImageUrl"`
	} `json:"hero"`
	Gallery struct {
		GalleryImages []epicImage `json:"galleryImages"`
	} `json:"gallery"`
	Carousel struct {
		Items []struct {
			Image epicImage `json:"image"`
			Video epicVideo `json:"video"`
		} `json:"items"`
	} `json:"carousel"`
	Meta struct {
		ReleaseDate       string      `json:"releaseDate"`
		CustomReleaseDate string      `json:"customReleaseDate"`
		Developer         flexStrings `json:"developer"`
		Publisher         flexStrings `json:"publisher"`
		Platform          flexStrings `json:"platform"`
	} `json:"meta"`
	Requirements struct {
		Languages flexStrings `json:"languages"`
		Systems   []struct {
			SystemType string `json:"systemType"`
			Details    []struct {
				Title       string `json:"title"`
				Minimum     string `json:"minimum"`
				Recommended string `json:"recommended"`
			} `json:"details"`
		} `json:"systems"`
	} `json:"requirements"`
	SocialLinks struct {
		LinkHomepage string `json:"linkHomepage"`
	} `json:"socialLinks"`
}

type epicPage struct {
	Type           string       `json:"type"`
	ProductRatings *epicRatings `json:"productRatings"`
	Data           epicPageData `json:"data"`
}

type epicProduct struct {
	ProductName    string       `json:"productName"`
	ProductRatings *epicRatings `json:"productRatings"`
	Pages          []epicPage   `json:"pages"`
}

// EpicName returns the product name of an Epic page body, or "" (used by tests and logs).
func EpicName(body []byte) string {
	var p epicProduct
	if json.Unmarshal(body, &p) != nil {
		return ""
	}
	return strings.TrimSpace(p.ProductName)
}

// ParseEpic maps an Epic product page into Details. The main page is the one of type
// "productHome" (Fortnite-style pages list other pages first), else the first page.
func ParseEpic(slug string, body []byte) (*steam.Details, error) {
	var p epicProduct
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("decoding epic product page: %w", err)
	}
	if strings.TrimSpace(p.ProductName) == "" || len(p.Pages) == 0 {
		return nil, ErrNotFound
	}
	page := p.Pages[0]
	for _, pg := range p.Pages {
		if pg.Type == "productHome" {
			page = pg
			break
		}
	}
	data := page.Data
	d := emptyDetails(slug)

	// Description: Epic's own short description, else the first paragraph of the long one.
	d.ShortDescription = shortText(plainFromMarkdown(data.About.ShortDescription))
	if d.ShortDescription == "" {
		d.ShortDescription = shortText(plainFromMarkdown(data.About.Description))
	}

	d.Developers = cleanNames(data.About.DeveloperAttribution)
	if len(d.Developers) == 0 {
		d.Developers = cleanNames(data.Meta.Developer)
	}
	d.Publishers = cleanNames(data.About.PublisherAttribution)
	if len(d.Publishers) == 0 {
		d.Publishers = cleanNames(data.Meta.Publisher)
	}

	// Screenshots: carousel and gallery images first, then the hero and about artwork.
	var shots screenshotList
	addEpic := func(u string) {
		if safe, ok := safeImage(u, epicImageRoots); ok {
			shots.add(safe, safe)
		}
	}
	for _, it := range data.Carousel.Items {
		addEpic(it.Image.Src)
	}
	for _, g := range data.Gallery.GalleryImages {
		addEpic(g.Src)
	}
	addEpic(data.Hero.BackgroundImageURL)
	addEpic(data.About.Image.Src)
	d.Screenshots = shots.result()

	for _, it := range data.Carousel.Items {
		if len(d.Movies) >= maxMovies {
			break
		}
		if m, ok := epicMovie(it.Video); ok {
			d.Movies = append(d.Movies, m)
		}
	}

	// Requirements per platform, and the platforms themselves.
	for _, sys := range data.Requirements.Systems {
		var minLines, recLines []Requirement
		for _, det := range sys.Details {
			minLines = append(minLines, Requirement{Name: det.Title, Value: det.Minimum})
			recLines = append(recLines, Requirement{Name: det.Title, Value: det.Recommended})
		}
		minHTML := requirementsHTML("Minimum", minLines)
		recHTML := requirementsHTML("Recommended", recLines)
		switch platformOf(sys.SystemType) {
		case "windows":
			d.Platforms.Windows = true
			d.PCReqMinHTML, d.PCReqRecHTML = orKeep(d.PCReqMinHTML, minHTML), orKeep(d.PCReqRecHTML, recHTML)
		case "mac":
			d.Platforms.Mac = true
			d.MacReqMinHTML, d.MacReqRecHTML = orKeep(d.MacReqMinHTML, minHTML), orKeep(d.MacReqRecHTML, recHTML)
		case "linux":
			d.Platforms.Linux = true
			d.LinuxReqMinHTML, d.LinuxReqRecHTML = orKeep(d.LinuxReqMinHTML, minHTML), orKeep(d.LinuxReqRecHTML, recHTML)
		}
	}
	for _, pl := range data.Meta.Platform {
		switch platformOf(pl) {
		case "windows":
			d.Platforms.Windows = true
		case "mac":
			d.Platforms.Mac = true
		case "linux":
			d.Platforms.Linux = true
		}
	}

	d.SupportedLanguages = languagesHTML(epicLanguages(data.Requirements.Languages))

	// Release date: the ISO date when Epic has one. customReleaseDate is free text that the
	// publisher edits (Alan Wake 2 still says "Coming Soon" long after release), so it is used
	// only when there is no ISO date and it names a year.
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(data.Meta.ReleaseDate)); err == nil {
		d.ReleaseDate = dateOnly(t.UTC())
		d.ReleaseDateText = releaseText(*d.ReleaseDate)
	} else if txt := collapseSpace(html.UnescapeString(plainFromMarkdown(data.Meta.CustomReleaseDate))); yearPattern.MatchString(txt) {
		d.ReleaseDateText = clipRunes(txt, maxReleaseText)
	}

	ratings := page.ProductRatings
	if ratings == nil || len(ratings.Ratings) == 0 {
		ratings = p.ProductRatings
	}
	if ratings != nil {
		for _, r := range ratings.Ratings {
			board := strings.ToLower(collapseSpace(r.CountryCodes))
			rating := collapseSpace(r.Title)
			if board == "" || rating == "" || len(board) > 40 || len(rating) > 40 {
				continue
			}
			d.AgeRatings = append(d.AgeRatings, steam.AgeRating{Board: board, Rating: rating})
		}
		d.AgeRatings = sortRatings(d.AgeRatings)
	}

	d.Website = safeWebsite(data.SocialLinks.LinkHomepage)
	return d, nil
}

var yearPattern = regexp.MustCompile(`\b(19|20)[0-9]{2}\b`)

func orKeep(old, next string) string {
	if old != "" {
		return old
	}
	return next
}

// platformOf maps "Windows", "MacOS", "Mac", "Linux" to windows, mac or linux.
func platformOf(s string) string {
	s = strings.ToLower(s)
	switch {
	case strings.Contains(s, "windows"):
		return "windows"
	case strings.Contains(s, "mac"), strings.Contains(s, "osx"):
		return "mac"
	case strings.Contains(s, "linux"):
		return "linux"
	}
	return ""
}

var langPrefix = regexp.MustCompile(`^\s*[A-Za-z ]{2,20}:\s*`)

// epicLanguages splits Epic's language lines, for example
// "AUDIO: English, French | TEXT: English, Polish", into names. Commas inside parentheses
// ("Spanish (Latin America, Spain)") do not split.
func epicLanguages(lines []string) []string {
	var out []string
	for _, line := range lines {
		for _, seg := range strings.Split(line, "|") {
			seg = langPrefix.ReplaceAllString(seg, "")
			out = append(out, splitTopLevel(seg)...)
		}
	}
	return out
}

func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			if depth > 0 {
				depth--
			}
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

type epicRecipe struct {
	Recipe  string `json:"recipe"`
	Outputs []struct {
		ContentType string `json:"contentType"`
		Key         string `json:"key"`
		URL         string `json:"url"`
	} `json:"outputs"`
}

// epicMovie builds a trailer from an Epic-hosted carousel video. The "recipes" field is a
// JSON document (sent as a string) keyed by locale; the video-hls recipe's manifest is the
// direct HLS playlist and the video-fmp4 recipe's manifest the DASH one. A video without a
// direct manifest URL on an Epic host is skipped.
func epicMovie(v epicVideo) (steam.Movie, bool) {
	raw := bytes.TrimSpace(v.Recipes)
	if len(raw) == 0 || string(raw) == "null" {
		return steam.Movie{}, false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return steam.Movie{}, false
		}
		raw = []byte(s)
	}
	var byLocale map[string][]epicRecipe
	if json.Unmarshal(raw, &byLocale) != nil || len(byLocale) == 0 {
		return steam.Movie{}, false
	}
	recipes, ok := byLocale["en-US"]
	if !ok {
		keys := make([]string, 0, len(byLocale))
		for k := range byLocale {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		recipes = byLocale[keys[0]]
	}
	m := steam.Movie{Name: clipRunes(v.Title, 120)}
	for _, rc := range recipes {
		for _, o := range rc.Outputs {
			safe, ok := safeImage(o.URL, epicImageRoots)
			if !ok {
				continue
			}
			switch {
			case o.Key == "manifest" && rc.Recipe == "video-hls" && strings.HasSuffix(strings.ToLower(pathOf(safe)), ".m3u8"):
				m.HLS = safe
			case o.Key == "manifest" && rc.Recipe == "video-fmp4" && strings.HasSuffix(strings.ToLower(pathOf(safe)), ".mpd"):
				m.DASH = safe
			case o.Key == "thumbnail" && m.Thumb == "":
				m.Thumb = safe
			}
		}
	}
	if m.HLS == "" && m.DASH == "" {
		return steam.Movie{}, false
	}
	return m, true
}

func pathOf(u string) string {
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		return u[:i]
	}
	return u
}
