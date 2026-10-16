package storeprices

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"
)

// FXLatestURL is Frankfurter's public ECB-based rate endpoint for USD to INR.
const FXLatestURL = "https://api.frankfurter.dev/v1/latest?base=USD&symbols=INR"

// Accepted range for the USD to INR rate. Anything outside it is treated as a bad
// response and nothing is written.
const (
	minUSDINR = 40.0
	maxUSDINR = 200.0
)

type frankfurterResponse struct {
	Amount float64            `json:"amount"`
	Base   string             `json:"base"`
	Date   string             `json:"date"`
	Rates  map[string]float64 `json:"rates"`
}

// FetchUSDINR returns the latest USD to INR rate and its date (YYYY-MM-DD).
func FetchUSDINR(ctx context.Context, f *Fetcher, endpoint string) (float64, string, error) {
	if endpoint == "" {
		endpoint = FXLatestURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.Do(ctx, req)
	if err != nil {
		return 0, "", err
	}
	if resp.Status != http.StatusOK {
		return 0, "", StatusError(resp.Status, "fx rate")
	}
	return ParseUSDINR(resp.Body)
}

// ParseUSDINR reads a Frankfurter /latest response for USD to INR. The rate must be
// between 40 and 200 inclusive, and the base must be USD.
func ParseUSDINR(body []byte) (float64, string, error) {
	var resp frankfurterResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, "", fmt.Errorf("decoding fx rate: %w", err)
	}
	if resp.Base != "" && resp.Base != "USD" {
		return 0, "", fmt.Errorf("fx rate: unexpected base %q", resp.Base)
	}
	rate, ok := resp.Rates["INR"]
	if !ok {
		return 0, "", fmt.Errorf("fx rate: no INR rate in response")
	}
	if math.IsNaN(rate) || rate < minUSDINR || rate > maxUSDINR {
		return 0, "", fmt.Errorf("fx rate: INR rate %v outside %v..%v", rate, minUSDINR, maxUSDINR)
	}
	date, err := time.Parse("2006-01-02", resp.Date)
	if err != nil {
		return 0, "", fmt.Errorf("fx rate: bad date %q: %w", resp.Date, err)
	}
	return rate, date.Format("2006-01-02"), nil
}
