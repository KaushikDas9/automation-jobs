package storeprices

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// DefaultGOGCatalogURL is GOG's public catalog search endpoint.
const DefaultGOGCatalogURL = "https://catalog.gog.com/v1/catalog"

// GOGCatalogURL builds the catalog search URL for a game name and country code.
// The query is an exact-title search ("like:" prefix); the match is done by title later.
func GOGCatalogURL(base, name, region string) string {
	if base == "" {
		base = DefaultGOGCatalogURL
	}
	// QueryEscape encodes spaces as "+"; GOG's own links use %20, so swap them.
	q := strings.ReplaceAll(url.QueryEscape("like:"+name), "+", "%20")
	return base + "?limit=10&query=" + q +
		"&order=desc%3Ascore&productType=in%3Agame%2Cpack&countryCode=" + url.QueryEscape(region) +
		"&locale=en-US&currencyCode=USD"
}

// GOGClient looks games up in GOG's public catalog.
type GOGClient struct {
	Fetcher *Fetcher
	BaseURL string // empty means DefaultGOGCatalogURL
}

// Price returns the GOG row for one game in one region (US or IN). Both regions are
// priced in USD by GOG. No exact title match gives a not_listed row.
// Errors are classified with IsTransient (network, 5xx) and IsBlocked (401, 403, 429).
func (c *GOGClient) Price(ctx context.Context, name, region string) (Price, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, GOGCatalogURL(c.BaseURL, name, region), nil)
	if err != nil {
		return Price{}, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.Fetcher.Do(ctx, req)
	if err != nil {
		return Price{}, err
	}
	if resp.Status != http.StatusOK {
		return Price{}, StatusError(resp.Status, "GOG catalog")
	}
	return ParseGOGCatalog(resp.Body, name, region)
}

type gogCatalogResponse struct {
	Products []gogProduct `json:"products"`
}

type gogProduct struct {
	Title     string    `json:"title"`
	StoreLink string    `json:"storeLink"`
	Price     *gogPrice `json:"price"`
}

type gogPrice struct {
	BaseMoney  *gogMoney `json:"baseMoney"`
	FinalMoney *gogMoney `json:"finalMoney"`
}

type gogMoney struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

// ParseGOGCatalog finds the product whose normalized title equals the game name and
// converts it to a Price for the region. No match gives a not_listed row. A matched
// product with a malformed price is an error, so nothing is written for it.
func ParseGOGCatalog(body []byte, gameName, region string) (Price, error) {
	var resp gogCatalogResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return Price{}, fmt.Errorf("decoding GOG catalog: %w", err)
	}
	want := NormalizeTitle(gameName)
	for _, p := range resp.Products {
		if NormalizeTitle(p.Title) != want {
			continue
		}
		return gogPriceFromProduct(p, region)
	}
	return NotListed(StoreGOG, region), nil
}

func gogPriceFromProduct(p gogProduct, region string) (Price, error) {
	// GOG sometimes lists a product with no price. The game exists, so the row says
	// listed_no_price with no prices, not an error.
	if p.Price == nil || p.Price.FinalMoney == nil {
		return Price{StoreCode: StoreGOG, Region: region, Status: StatusListedNoPrice, StoreURL: p.StoreLink}, nil
	}
	finalCents, err := ParseCents(p.Price.FinalMoney.Amount)
	if err != nil {
		return Price{}, fmt.Errorf("GOG product %q final price: %w", p.Title, err)
	}

	out := Price{
		StoreCode:    StoreGOG,
		Region:       region,
		Currency:     p.Price.FinalMoney.Currency,
		CurrentCents: &finalCents,
		StoreURL:     p.StoreLink,
		Status:       StatusOK,
	}
	if out.Currency == "" && p.Price.BaseMoney != nil {
		out.Currency = p.Price.BaseMoney.Currency
	}
	if p.Price.BaseMoney != nil && p.Price.BaseMoney.Amount != "" {
		baseCents, err := ParseCents(p.Price.BaseMoney.Amount)
		if err != nil {
			return Price{}, fmt.Errorf("GOG product %q base price: %w", p.Title, err)
		}
		out.BaseCents = &baseCents
	}
	// The discount is computed here, never taken from GOG's label.
	if out.BaseCents != nil {
		out.DiscountPct = DiscountPercent(*out.BaseCents, finalCents)
	}
	if finalCents == 0 {
		out.Status = StatusFree
	}
	return out, nil
}
