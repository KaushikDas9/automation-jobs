package epicfree

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

// Game is one free offer ready to store. A game is free now (the current offer) or
// upcoming (the next offer). RegularPrice is the price a player would otherwise pay, as
// an exact decimal string, or nil when the feed does not give a usable price.
type Game struct {
	EpicID       string
	Title        string
	ImageURL     *string
	StoreURL     string
	RegularPrice *string
	Currency     string
	StartsAt     time.Time
	EndsAt       time.Time
	Upcoming     bool
}

// feed mirrors only the parts of the response this job reads.
type feed struct {
	Data struct {
		Catalog struct {
			SearchStore struct {
				Elements []element `json:"elements"`
			} `json:"searchStore"`
		} `json:"Catalog"`
	} `json:"data"`
}

type element struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	ProductSlug string `json:"productSlug"`
	URLSlug     string `json:"urlSlug"`
	CatalogNs   struct {
		Mappings []struct {
			PageSlug string `json:"pageSlug"`
		} `json:"mappings"`
	} `json:"catalogNs"`
	KeyImages []keyImage `json:"keyImages"`
	Price     struct {
		TotalPrice struct {
			OriginalPrice int64  `json:"originalPrice"`
			CurrencyCode  string `json:"currencyCode"`
			CurrencyInfo  struct {
				Decimals *int `json:"decimals"`
			} `json:"currencyInfo"`
		} `json:"totalPrice"`
	} `json:"price"`
	Promotions *promotions `json:"promotions"`
}

type keyImage struct {
	Type string `json:"type"`
	URL  string `json:"url"`
}

type promotions struct {
	PromotionalOffers         []offerGroup `json:"promotionalOffers"`
	UpcomingPromotionalOffers []offerGroup `json:"upcomingPromotionalOffers"`
}

type offerGroup struct {
	PromotionalOffers []offer `json:"promotionalOffers"`
}

type offer struct {
	StartDate       string `json:"startDate"`
	EndDate         string `json:"endDate"`
	DiscountSetting struct {
		DiscountPercentage *int `json:"discountPercentage"`
	} `json:"discountSetting"`
}

// slugPattern is the only shape of slug that becomes a store page link.
var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,120}$`)

// hexIDPattern matches a 32-character hex id, which is not a usable page slug.
var hexIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// currencyPattern matches an ISO-style three letter currency code.
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

// imagePreference is the order in which key image types are tried.
var imagePreference = []string{"OfferImageWide", "Thumbnail", "DieselStoreFrontWide"}

// Parse reads the feed body and returns one Game per listed element that has a free
// offer, the current offer first and then the upcoming one. Elements without a free
// offer are skipped. An empty result is not an error here; the caller decides what an
// empty feed means. A body that is not JSON is an error.
func Parse(body []byte) ([]Game, error) {
	var f feed
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("parsing epic free games feed: %w", err)
	}

	seen := make(map[string]bool)
	var out []Game
	for _, el := range f.Data.Catalog.SearchStore.Elements {
		g, ok := gameFrom(el)
		if !ok || seen[g.EpicID] {
			continue
		}
		seen[g.EpicID] = true
		out = append(out, g)
	}
	return out, nil
}

// gameFrom builds a Game from one element. It returns false when the element has no id
// or title, or no free offer, now or upcoming.
func gameFrom(el element) (Game, bool) {
	if el.ID == "" || el.Title == "" || el.Promotions == nil {
		return Game{}, false
	}

	g := Game{
		EpicID:   el.ID,
		Title:    el.Title,
		ImageURL: pickImage(el.KeyImages),
		StoreURL: storeURL(el),
		Currency: currencyOf(el),
	}
	g.RegularPrice = regularPrice(el)

	if starts, ends, ok := firstFree(el.Promotions.PromotionalOffers); ok {
		g.StartsAt, g.EndsAt = starts, ends
		return g, true
	}
	if starts, ends, ok := firstFree(el.Promotions.UpcomingPromotionalOffers); ok {
		g.StartsAt, g.EndsAt = starts, ends
		g.Upcoming = true
		return g, true
	}
	return Game{}, false
}

// firstFree returns the dates of the first offer with a 0 percent discount and valid dates.
func firstFree(groups []offerGroup) (time.Time, time.Time, bool) {
	for _, grp := range groups {
		for _, o := range grp.PromotionalOffers {
			if o.DiscountSetting.DiscountPercentage == nil || *o.DiscountSetting.DiscountPercentage != 0 {
				continue
			}
			starts, err1 := time.Parse(time.RFC3339Nano, o.StartDate)
			ends, err2 := time.Parse(time.RFC3339Nano, o.EndDate)
			if err1 != nil || err2 != nil || !ends.After(starts) {
				continue
			}
			return starts.UTC(), ends.UTC(), true
		}
	}
	return time.Time{}, time.Time{}, false
}

// storeURL returns the store page when a slug is usable, and the store search otherwise.
// The candidates are tried in order: product slug, url slug, then the catalog page slug.
func storeURL(el element) string {
	candidates := []string{el.ProductSlug, el.URLSlug}
	for _, m := range el.CatalogNs.Mappings {
		candidates = append(candidates, m.PageSlug)
	}
	for _, s := range candidates {
		if slugPattern.MatchString(s) && !hexIDPattern.MatchString(s) {
			return "https://store.epicgames.com/en-US/p/" + s
		}
	}
	return "https://store.epicgames.com/en-US/browse?q=" + url.QueryEscape(el.Title) +
		"&sortBy=relevancy&sortDir=DESC&count=40"
}

// pickImage returns the first https image in the preferred order, or nil.
func pickImage(images []keyImage) *string {
	for _, want := range imagePreference {
		for _, img := range images {
			if img.Type != want {
				continue
			}
			u, err := url.Parse(img.URL)
			if err == nil && u.Scheme == "https" && u.Host != "" {
				s := img.URL
				return &s
			}
		}
	}
	return nil
}

// currencyOf returns the feed's currency when it is a three letter code, or INR.
func currencyOf(el element) string {
	if c := el.Price.TotalPrice.CurrencyCode; currencyPattern.MatchString(c) {
		return c
	}
	return "INR"
}

// regularPrice converts the original price in minor units to a decimal string. It is nil
// when the price is missing, zero, negative, or the decimals are unknown or out of range.
func regularPrice(el element) *string {
	tp := el.Price.TotalPrice
	if tp.OriginalPrice <= 0 || tp.CurrencyInfo.Decimals == nil {
		return nil
	}
	dec := *tp.CurrencyInfo.Decimals
	if dec < 0 || dec > 4 {
		return nil
	}
	s := formatMinor(tp.OriginalPrice, dec)
	return &s
}

// formatMinor writes v / 10^dec exactly, with dec digits after the point.
func formatMinor(v int64, dec int) string {
	if dec == 0 {
		return strconv.FormatInt(v, 10)
	}
	p := int64(1)
	for i := 0; i < dec; i++ {
		p *= 10
	}
	frac := strconv.FormatInt(v%p, 10)
	for len(frac) < dec {
		frac = "0" + frac
	}
	return strconv.FormatInt(v/p, 10) + "." + frac
}
