package steam

import (
	"context"
	"fmt"
	"net/url"
	"strings"
)

// maxBatchIDs caps how many app ids go into one price request.
const maxBatchIDs = 50

// storeRoot is the storefront root without the /api suffix (the reviews endpoint lives there).
func (c *Client) storeRoot() string {
	return strings.TrimSuffix(strings.TrimRight(c.cfg.BaseURL, "/"), "/api")
}

// FetchDetails retrieves the full store entry for appID in English. The price inside it
// depends on the caller's region, so callers must ignore it and use FetchPrice and
// FetchBatchPrices for prices.
func (c *Client) FetchDetails(ctx context.Context, appID string) (*Details, error) {
	endpoint := fmt.Sprintf("%s/appdetails?appids=%s&l=english", c.cfg.BaseURL, url.QueryEscape(appID))
	body, err := c.getBody(ctx, endpoint, "details "+appID)
	if err != nil {
		return nil, err
	}
	return ParseDetails(appID, body)
}

// FetchReviews retrieves the review summary ("Very Positive", counts) for appID.
func (c *Client) FetchReviews(ctx context.Context, appID string) (*Reviews, error) {
	endpoint := fmt.Sprintf("%s/appreviews/%s?json=1&language=all&num_per_page=0", c.storeRoot(), url.PathEscape(appID))
	body, err := c.getBody(ctx, endpoint, "reviews "+appID)
	if err != nil {
		return nil, err
	}
	return ParseReviews(body)
}

// FetchBatchPrices retrieves prices for many apps in countryCode ("us" for USD) using as
// few requests as possible. Apps Steam has no price for come back with Unavailable set.
// A failed chunk returns the prices gathered so far together with the error.
func (c *Client) FetchBatchPrices(ctx context.Context, appIDs []string, countryCode string) (map[string]BatchPrice, error) {
	out := make(map[string]BatchPrice, len(appIDs))
	for start := 0; start < len(appIDs); start += maxBatchIDs {
		end := start + maxBatchIDs
		if end > len(appIDs) {
			end = len(appIDs)
		}
		ids := make([]string, 0, end-start)
		for _, id := range appIDs[start:end] {
			ids = append(ids, url.QueryEscape(id))
		}
		endpoint := fmt.Sprintf("%s/appdetails?appids=%s&cc=%s&filters=price_overview",
			c.cfg.BaseURL, strings.Join(ids, ","), url.QueryEscape(countryCode))
		body, err := c.getBody(ctx, endpoint, fmt.Sprintf("batch prices %s (%d apps)", countryCode, len(ids)))
		if err != nil {
			return out, err
		}
		got, err := ParseBatchPrices(body)
		if err != nil {
			return out, err
		}
		for id, p := range got {
			out[id] = p
		}
	}
	return out, nil
}
