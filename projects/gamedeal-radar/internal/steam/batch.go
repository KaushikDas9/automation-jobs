package steam

import (
	"encoding/json"
	"fmt"
)

// BatchPrice is the USD price for one app from a batched appdetails call.
// Unavailable is set when Steam has no usable price for the app.
// IsFree cannot be known from price_overview alone, so it is left false here.
type BatchPrice struct {
	Currency    string
	Base        float64
	Current     float64
	DiscountPct int
	IsFree      bool
	Unavailable bool
}

type batchPriceEntry struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
}

// ParseBatchPrices converts an appdetails?appids=a,b&filters=price_overview body into
// one BatchPrice per app id. Prices are in cents and are converted to whole units.
func ParseBatchPrices(body []byte) (map[string]BatchPrice, error) {
	var parsed map[string]batchPriceEntry
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshaling steam batch prices: %w", err)
	}

	out := make(map[string]BatchPrice, len(parsed))
	for appID, entry := range parsed {
		out[appID] = convertBatchPrice(entry)
	}
	return out, nil
}

func convertBatchPrice(entry batchPriceEntry) BatchPrice {
	unavailable := BatchPrice{Unavailable: true}
	if !entry.Success || isEmptyJSON(entry.Data) {
		return unavailable
	}

	var data struct {
		PriceOverview *struct {
			Currency        string `json:"currency"`
			Initial         int64  `json:"initial"`
			Final           int64  `json:"final"`
			DiscountPercent int    `json:"discount_percent"`
		} `json:"price_overview"`
	}
	if err := json.Unmarshal(entry.Data, &data); err != nil || data.PriceOverview == nil {
		return unavailable
	}

	po := data.PriceOverview
	return BatchPrice{
		Currency:    po.Currency,
		Base:        float64(po.Initial) / 100.0,
		Current:     float64(po.Final) / 100.0,
		DiscountPct: po.DiscountPercent,
	}
}
