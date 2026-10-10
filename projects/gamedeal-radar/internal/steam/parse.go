package steam

import (
	"encoding/json"
	"fmt"
	"time"
)

// PriceResult is the normalized Steam price for one app in one country.
type PriceResult struct {
	ExternalAppID string
	Country       string
	Currency      string
	BasePrice     float64
	CurrentPrice  float64
	DiscountPct   int
	IsDeal        bool
	IsFree        bool
	CheckedAt     time.Time
	RawPayload    []byte
}

type rawAppData struct {
	IsFree        bool   `json:"is_free"`
	Name          string `json:"name"`
	PriceOverview *struct {
		Currency         string `json:"currency"`
		Initial          int64  `json:"initial"`
		Final            int64  `json:"final"`
		DiscountPercent  int    `json:"discount_percent"`
		InitialFormatted string `json:"initial_formatted"`
		FinalFormatted   string `json:"final_formatted"`
	} `json:"price_overview"`
}

type appDetailsResponse map[string]struct {
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data"`
}

// ParseAppDetails converts an appdetails response body into a PriceResult.
// It mirrors the server's steam client: ErrNotFound / ErrPriceUnavailable for
// games that have no usable price, a zero-price result for free-to-play games,
// and paisa-to-rupee conversion (÷100) otherwise.
func ParseAppDetails(appID, countryCode string, body []byte, now time.Time) (*PriceResult, error) {
	var parsed appDetailsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("unmarshaling steam response: %w", err)
	}

	entry, exists := parsed[appID]
	if !exists || !entry.Success {
		return nil, ErrNotFound
	}
	if len(entry.Data) == 0 || string(entry.Data) == "[]" {
		return nil, ErrPriceUnavailable
	}

	var data rawAppData
	if err := json.Unmarshal(entry.Data, &data); err != nil {
		return nil, ErrPriceUnavailable
	}

	if data.IsFree {
		return &PriceResult{
			ExternalAppID: appID,
			Country:       countryCode,
			Currency:      "INR",
			IsFree:        true,
			CheckedAt:     now.UTC(),
			RawPayload:    body,
		}, nil
	}

	if data.PriceOverview == nil {
		return nil, ErrPriceUnavailable
	}

	po := data.PriceOverview
	return &PriceResult{
		ExternalAppID: appID,
		Country:       countryCode,
		Currency:      po.Currency,
		BasePrice:     float64(po.Initial) / 100.0,
		CurrentPrice:  float64(po.Final) / 100.0,
		DiscountPct:   po.DiscountPercent,
		IsDeal:        po.DiscountPercent > 0,
		CheckedAt:     now.UTC(),
		RawPayload:    body,
	}, nil
}
