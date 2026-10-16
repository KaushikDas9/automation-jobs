package itad

import (
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strings"
	"time"

	"gamedeal-radar/daily-check/internal/storeprices"
)

// LookupResult is the answer to /games/lookup/v1.
type LookupResult struct {
	Found bool
	ID    string
	Title string
}

type lookupResponse struct {
	Found bool `json:"found"`
	Game  *struct {
		ID    string `json:"id"`
		Title string `json:"title"`
	} `json:"game"`
}

// ParseLookup reads a /games/lookup/v1 response.
func ParseLookup(body []byte) (LookupResult, error) {
	var resp lookupResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return LookupResult{}, fmt.Errorf("decoding price API lookup: %w", err)
	}
	if !resp.Found || resp.Game == nil || resp.Game.ID == "" {
		return LookupResult{Found: false}, nil
	}
	return LookupResult{Found: true, ID: resp.Game.ID, Title: resp.Game.Title}, nil
}

// MatchTitle is the strict title rule. A lookup is accepted only when both titles are
// equal after storeprices.NormalizeTitle. There is no fuzzy matching. A name that
// normalizes to nothing is never accepted.
func MatchTitle(ourName string, r LookupResult) bool {
	if !r.Found || r.ID == "" {
		return false
	}
	want := storeprices.NormalizeTitle(ourName)
	if want == "" {
		return false
	}
	return storeprices.NormalizeTitle(r.Title) == want
}

// Deal is the Epic (shop 16) deal of one provider game in one country.
// Amounts are in cents. Nil means the amount was missing or unreadable.
type Deal struct {
	ID              string
	Currency        string
	CurrentCents    *int64
	BaseCents       *int64
	RegularCurrency string
	Timestamp       *time.Time
}

type pricesEntry struct {
	ID    string       `json:"id"`
	Deals []pricesDeal `json:"deals"`
}

type pricesDeal struct {
	Shop struct {
		ID int `json:"id"`
	} `json:"shop"`
	Price   *money `json:"price"`
	Regular *money `json:"regular"`
	// Timestamp is the time the provider last saw this deal.
	Timestamp string `json:"timestamp"`
	// The deal URL is deliberately not decoded: it is the provider's tracked redirect.
}

type money struct {
	Amount   *float64 `json:"amount"`
	Currency string   `json:"currency"`
}

// ParsePrices reads a /games/prices/v3 response and keeps, per game id, the Epic deal
// only. When a game has several Epic deals the lowest current price wins. A malformed
// amount leaves that price nil, so the row is skipped later by the sanity check.
func ParsePrices(body []byte, country string) (map[string]Deal, error) {
	var entries []pricesEntry
	if err := json.Unmarshal(body, &entries); err != nil {
		return nil, fmt.Errorf("decoding price API prices (%s): %w", country, err)
	}
	out := make(map[string]Deal, len(entries))
	for _, e := range entries {
		if e.ID == "" {
			continue
		}
		var best *Deal
		for _, d := range e.Deals {
			if d.Shop.ID != EpicShopID {
				continue
			}
			deal := dealFrom(e.ID, d)
			if best == nil || lowerPrice(deal, *best) {
				cp := deal
				best = &cp
			}
		}
		if best != nil {
			out[e.ID] = *best
		}
	}
	return out, nil
}

func dealFrom(id string, d pricesDeal) Deal {
	deal := Deal{ID: id}
	if d.Price != nil {
		deal.Currency = d.Price.Currency
		deal.CurrentCents = centsOf(d.Price.Amount)
	}
	if d.Regular != nil {
		deal.RegularCurrency = d.Regular.Currency
		deal.BaseCents = centsOf(d.Regular.Amount)
	}
	if ts, err := time.Parse(time.RFC3339, d.Timestamp); err == nil {
		t := ts.UTC()
		deal.Timestamp = &t
	}
	return deal
}

// lowerPrice reports whether a is a lower current price than b. A deal without a price
// never beats one with a price.
func lowerPrice(a, b Deal) bool {
	if a.CurrentCents == nil {
		return false
	}
	if b.CurrentCents == nil {
		return true
	}
	return *a.CurrentCents < *b.CurrentCents
}

// centsOf converts a decimal amount from JSON to cents, rounding to the nearest cent.
// A missing, negative or non-finite amount gives nil.
func centsOf(amount *float64) *int64 {
	if amount == nil || math.IsNaN(*amount) || math.IsInf(*amount, 0) || *amount < 0 {
		return nil
	}
	c := int64(math.Round(*amount * 100))
	return &c
}

// ParseShopLinks reads a /lookup/shop/16/id/v1 response: {"<id>": ["<slug>", "<offer id>"]}.
// The value may be missing, null or empty. Each id maps to the first element, or to ""
// when there is none.
func ParseShopLinks(body []byte) (map[string]string, error) {
	var raw map[string][]string
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decoding price API shop lookup: %w", err)
	}
	out := make(map[string]string, len(raw))
	for id, vals := range raw {
		first := ""
		if len(vals) > 0 {
			first = strings.TrimSpace(vals[0])
		}
		out[id] = first
	}
	return out, nil
}

var (
	slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,120}$`)
	hexID       = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

// ValidEpicSlug reports whether s looks like a readable Epic product slug: lowercase
// letters, digits and dashes, and not a 32-character hex id.
func ValidEpicSlug(s string) bool {
	return slugPattern.MatchString(s) && !hexID.MatchString(s)
}

// EpicPageURL returns the Epic product page for a slug, or false when the slug is not usable.
func EpicPageURL(slug string) (string, bool) {
	if !ValidEpicSlug(slug) {
		return "", false
	}
	return "https://store.epicgames.com/en-US/p/" + slug, true
}

// EpicSearchURL returns the Epic store search link for a game name.
func EpicSearchURL(name string) string {
	return "https://store.epicgames.com/en-US/browse?q=" + url.QueryEscape(name) +
		"&sortBy=relevancy&sortDir=DESC&count=40"
}

// PriceFromDeal turns an Epic deal into a game_store_prices row. Status is 'ok', or
// 'free' when the current price is zero. The discount is computed here, not taken from
// the provider. The reason is non-empty when the deal cannot be used at all.
func PriceFromDeal(d Deal, region, storeURL string) (storeprices.Price, string) {
	if d.RegularCurrency != "" && d.Currency != "" && d.RegularCurrency != d.Currency {
		return storeprices.Price{}, "regular and current currencies differ"
	}
	p := storeprices.Price{
		StoreCode:       storeprices.StoreEpic,
		Region:          region,
		Currency:        d.Currency,
		BaseCents:       d.BaseCents,
		CurrentCents:    d.CurrentCents,
		StoreURL:        storeURL,
		Status:          storeprices.StatusOK,
		SourceUpdatedAt: d.Timestamp,
	}
	if d.CurrentCents != nil && *d.CurrentCents == 0 {
		p.Status = storeprices.StatusFree
	}
	if d.CurrentCents != nil && d.BaseCents != nil {
		p.DiscountPct = storeprices.DiscountPercent(*d.BaseCents, *d.CurrentCents)
	}
	return p, ""
}
