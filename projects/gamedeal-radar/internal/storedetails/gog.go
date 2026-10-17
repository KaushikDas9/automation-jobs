package storedetails

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"gamedeal-radar/daily-check/internal/steam"
)

// gogImageRoots are the hosts GOG serves product images from.
var gogImageRoots = []string{"gog-statics.com"}

// GOGProduct is the catalog entry of a GOG product, found by its slug.
type GOGProduct struct {
	ID               string   `json:"id"`
	Slug             string   `json:"slug"`
	Title            string   `json:"title"`
	ProductType      string   `json:"productType"`
	ReleaseDate      string   `json:"releaseDate"` // "2012.01.16"
	Developers       []string `json:"developers"`
	Publishers       []string `json:"publishers"`
	OperatingSystems []string `json:"operatingSystems"`
	Genres           []struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	} `json:"genres"`
}

// GOGDetails finds the GOG product with this slug and maps its data. GOG has no key-free
// lookup by slug, so the public catalog search is asked for the game name and then for the
// slug's words, and only a product whose slug is exactly slug is accepted. Its numeric id
// then reads https://api.gog.com/v2/games/<id>. ErrNotFound when neither search lists the slug
// or the product page is 404.
func (c *Client) GOGDetails(ctx context.Context, slug, name string) (*steam.Details, error) {
	if !ValidGOGSlug(slug) {
		return nil, ErrInvalidID
	}
	prod, err := c.findGOGProduct(ctx, slug, name)
	if err != nil {
		return nil, err
	}
	if !gogIDPattern.MatchString(prod.ID) {
		return nil, fmt.Errorf("gog catalog: product id is not numeric: %w", ErrInvalidID)
	}
	body, err := c.get(ctx, c.gogGamesBase+prod.ID+"?locale=en-US", "gog product data")
	if err != nil {
		return nil, err
	}
	return ParseGOG(prod, body)
}

// gogQueries returns the catalog searches tried for a slug: the stored game name, then the
// slug with underscores as spaces. Empty and repeated queries are dropped.
func gogQueries(slug, name string) []string {
	var out []string
	for _, q := range []string{collapseSpace(name), collapseSpace(strings.NewReplacer("_", " ", "-", " ").Replace(slug))} {
		if q == "" || len(q) > 200 || slices.Contains(out, q) {
			continue
		}
		out = append(out, q)
	}
	return out
}

func (c *Client) gogCatalogURL(query string) string {
	q := strings.ReplaceAll(url.QueryEscape("like:"+query), "+", "%20")
	return c.gogCatalogBase + "?limit=48&query=" + q +
		"&productType=in%3Agame%2Cpack&countryCode=US&locale=en-US&currencyCode=USD"
}

func (c *Client) findGOGProduct(ctx context.Context, slug, name string) (GOGProduct, error) {
	for _, q := range gogQueries(slug, name) {
		body, err := c.get(ctx, c.gogCatalogURL(q), "gog catalog")
		if err != nil {
			return GOGProduct{}, err
		}
		p, ok, err := FindGOGProduct(body, slug)
		if err != nil {
			return GOGProduct{}, err
		}
		if ok {
			return p, nil
		}
	}
	return GOGProduct{}, fmt.Errorf("gog catalog: slug not listed: %w", ErrNotFound)
}

// FindGOGProduct returns the catalog product whose slug is exactly slug.
func FindGOGProduct(body []byte, slug string) (GOGProduct, bool, error) {
	var resp struct {
		Products []GOGProduct `json:"products"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return GOGProduct{}, false, fmt.Errorf("decoding gog catalog: %w", err)
	}
	for _, p := range resp.Products {
		if p.Slug == slug {
			return p, true, nil
		}
	}
	return GOGProduct{}, false, nil
}

type gogLink struct {
	Href       string   `json:"href"`
	Formatters []string `json:"formatters"`
}

type gogRating struct {
	AgeRating json.Number `json:"ageRating"`
	Category  struct {
		Name string `json:"name"`
	} `json:"category"`
	ContentDescriptors []struct {
		Descriptor string `json:"descriptor"`
	} `json:"contentDescriptors"`
}

type gogGame struct {
	Description string `json:"description"`
	Embedded    struct {
		Product struct {
			ID                json.Number `json:"id"`
			Title             string      `json:"title"`
			GlobalReleaseDate string      `json:"globalReleaseDate"`
		} `json:"product"`
		Localizations []struct {
			Embedded struct {
				Language struct {
					Name string `json:"name"`
				} `json:"language"`
				Scope struct {
					Type string `json:"type"`
				} `json:"localizationScope"`
			} `json:"_embedded"`
		} `json:"localizations"`
		Screenshots []struct {
			Links struct {
				Self gogLink `json:"self"`
			} `json:"_links"`
		} `json:"screenshots"`
		Developers []struct {
			Name string `json:"name"`
		} `json:"developers"`
		Publishers []struct {
			Name string `json:"name"`
		} `json:"publishers"`
		SupportedOperatingSystems []struct {
			OperatingSystem struct {
				Name string `json:"name"`
			} `json:"operatingSystem"`
			SystemRequirements []struct {
				Type         string `json:"type"`
				Description  string `json:"description"`
				Requirements []struct {
					Name        string `json:"name"`
					Description string `json:"description"`
				} `json:"requirements"`
			} `json:"systemRequirements"`
		} `json:"supportedOperatingSystems"`
		ESRB *gogRating `json:"esrbRating"`
		PEGI *gogRating `json:"pegiRating"`
		USK  *gogRating `json:"uskRating"`
		BR   *gogRating `json:"brRating"`
	} `json:"_embedded"`
}

// ParseGOG maps the catalog entry and the v2 product data into Details. GOG's trailers are
// YouTube embeds with no direct video URL, so no movies are stored.
func ParseGOG(prod GOGProduct, body []byte) (*steam.Details, error) {
	var g gogGame
	if err := json.Unmarshal(body, &g); err != nil {
		return nil, fmt.Errorf("decoding gog product data: %w", err)
	}
	if g.Embedded.Product.ID.String() != prod.ID {
		// The page must be the product the catalog named; anything else is not this game.
		return nil, fmt.Errorf("gog product data: id does not match the catalog: %w", ErrNotFound)
	}
	d := emptyDetails(prod.Slug)
	d.ShortDescription = shortText(plainFromHTML(strings.ReplaceAll(g.Description, "<br><br>", "\n\n")))

	d.Developers = cleanNames(prod.Developers)
	if len(d.Developers) == 0 {
		for _, x := range g.Embedded.Developers {
			d.Developers = append(d.Developers, x.Name)
		}
		d.Developers = cleanNames(d.Developers)
	}
	d.Publishers = cleanNames(prod.Publishers)
	if len(d.Publishers) == 0 {
		for _, x := range g.Embedded.Publishers {
			d.Publishers = append(d.Publishers, x.Name)
		}
		d.Publishers = cleanNames(d.Publishers)
	}

	for _, gn := range prod.Genres {
		name := collapseSpace(gn.Name)
		if name == "" || len(d.Genres) >= 10 {
			continue
		}
		d.Genres = append(d.Genres, steam.Tag{ID: collapseSpace(gn.Slug), Description: name})
	}

	// Screenshots: GOG gives templated URLs ("..._{formatter}.jpg") with the formatters it serves.
	var shots screenshotList
	for _, s := range g.Embedded.Screenshots {
		l := s.Links.Self
		if !strings.Contains(l.Href, "{formatter}") {
			continue
		}
		thumbF := pickFormatter(l.Formatters, "product_card_screenshot_748", "product_card_screenshot_748_2x")
		fullF := pickFormatter(l.Formatters, "1600", "product_card_screenshot_748_2x")
		if fullF == "" {
			continue
		}
		full, ok := safeImage(strings.Replace(l.Href, "{formatter}", fullF, 1), gogImageRoots)
		if !ok {
			continue
		}
		thumb := full
		if thumbF != "" {
			if t, ok := safeImage(strings.Replace(l.Href, "{formatter}", thumbF, 1), gogImageRoots); ok {
				thumb = t
			}
		}
		shots.add(thumb, full)
	}
	d.Screenshots = shots.result()

	for _, osys := range g.Embedded.SupportedOperatingSystems {
		var minHTML, recHTML string
		for _, req := range osys.SystemRequirements {
			var lines []Requirement
			for _, r := range req.Requirements {
				lines = append(lines, Requirement{Name: r.Name, Value: r.Description})
			}
			if len(lines) == 0 && collapseSpace(req.Description) != "" {
				lines = []Requirement{{Value: req.Description}}
			}
			switch strings.ToLower(req.Type) {
			case "minimum":
				minHTML = requirementsHTML("Minimum", lines)
			case "recommended":
				recHTML = requirementsHTML("Recommended", lines)
			}
		}
		switch platformOf(osys.OperatingSystem.Name) {
		case "windows":
			d.Platforms.Windows = true
			d.PCReqMinHTML, d.PCReqRecHTML = minHTML, recHTML
		case "mac":
			d.Platforms.Mac = true
			d.MacReqMinHTML, d.MacReqRecHTML = minHTML, recHTML
		case "linux":
			d.Platforms.Linux = true
			d.LinuxReqMinHTML, d.LinuxReqRecHTML = minHTML, recHTML
		}
	}
	for _, osName := range prod.OperatingSystems {
		switch platformOf(osName) {
		case "windows":
			d.Platforms.Windows = true
		case "mac":
			d.Platforms.Mac = true
		case "linux":
			d.Platforms.Linux = true
		}
	}

	var langs []string
	for _, l := range g.Embedded.Localizations {
		if strings.EqualFold(l.Embedded.Scope.Type, "text") {
			langs = append(langs, l.Embedded.Language.Name)
		}
	}
	d.SupportedLanguages = languagesHTML(langs)

	// Release date: GOG's global release date, as the calendar date it is written in.
	if t, ok := gogDate(g.Embedded.Product.GlobalReleaseDate, prod.ReleaseDate); ok {
		d.ReleaseDate = dateOnly(t)
		d.ReleaseDateText = releaseText(*d.ReleaseDate)
	}

	for _, br := range []struct {
		board string
		r     *gogRating
	}{{"brazil", g.Embedded.BR}, {"esrb", g.Embedded.ESRB}, {"pegi", g.Embedded.PEGI}, {"usk", g.Embedded.USK}} {
		if br.r == nil {
			continue
		}
		rating := br.r.AgeRating.String()
		if br.board == "esrb" && collapseSpace(br.r.Category.Name) != "" {
			rating = collapseSpace(br.r.Category.Name)
		}
		if rating == "" || len(rating) > 40 {
			continue
		}
		var desc []string
		for _, cd := range br.r.ContentDescriptors {
			if s := collapseSpace(cd.Descriptor); s != "" {
				desc = append(desc, s)
			}
		}
		d.AgeRatings = append(d.AgeRatings, steam.AgeRating{Board: br.board, Rating: rating, Descriptors: clipRunes(strings.Join(desc, ", "), 300)})
	}
	d.AgeRatings = sortRatings(d.AgeRatings)
	return d, nil
}

func pickFormatter(have []string, prefer ...string) string {
	for _, p := range prefer {
		if slices.Contains(have, p) {
			return p
		}
	}
	return ""
}

// gogDate reads "2012-01-16T00:00:00+01:00" (keeping the written date) or the catalog's
// "2012.01.16".
func gogDate(iso, dotted string) (time.Time, bool) {
	if len(iso) >= 10 {
		if t, err := time.Parse("2006-01-02", iso[:10]); err == nil {
			return t, true
		}
	}
	if t, err := time.Parse("2006.01.02", strings.TrimSpace(dotted)); err == nil {
		return t, true
	}
	return time.Time{}, false
}
