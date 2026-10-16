// Package storeprices holds the pieces shared by the store price jobs (GOG and Epic
// through IsThereAnyDeal, plus the USD to INR rate): the row type, title matching,
// money helpers, sanity checks, HTTP plumbing, timed retry passes and the run
// orchestration. Parsing functions are pure so they can be tested without network
// or database access.
//
// Ported from the server's internal/storeprices package in the GameDealRader repo.
// That package is read-only reference; this module cannot import it.
package storeprices

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Store codes written to game_store_prices.store_code.
const (
	StoreGOG  = "gog"
	StoreEpic = "epic"
)

// Regions written to game_store_prices.region.
const (
	RegionUS = "US"
	RegionIN = "IN"
)

// Status values written to game_store_prices.status.
const (
	StatusOK            = "ok"
	StatusFree          = "free"
	StatusListedNoPrice = "listed_no_price"
	StatusNotListed     = "not_listed"
)

// Source values written to game_store_prices.source. Manual rows are typed by the owner
// and are never overwritten or deleted by a job.
const (
	SourceAuto   = "auto"
	SourceManual = "manual"
)

// Price is one row of game_store_prices before it is written.
// Nil pointers and empty strings become SQL NULL.
type Price struct {
	StoreCode       string
	Region          string
	Currency        string
	BaseCents       *int64
	CurrentCents    *int64
	DiscountPct     int
	StoreURL        string
	Status          string
	SourceUpdatedAt *time.Time
}

// NotListed is the row written when a store has no matching product.
func NotListed(storeCode, region string) Price {
	return Price{StoreCode: storeCode, Region: region, Status: StatusNotListed}
}

// NormalizeTitle is used to compare a game name with a store's product title.
// It lowercases, drops trademark signs, drops apostrophes, turns any other
// punctuation or symbol into a space, and collapses whitespace.
// Matching is exact equality on the result; there is no fuzzy matching.
func NormalizeTitle(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '™' || r == '®' || r == '©':
			continue
		case r == '\'' || r == '’' || r == '‘' || r == '`':
			continue
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			b.WriteRune(' ')
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// ParseCents converts a decimal amount such as "35.20", "35" or "10.5" to cents.
// More than two fractional digits is an error, since the column keeps two.
func ParseCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	var f int64
	if hasFrac {
		if len(frac) == 0 || len(frac) > 2 {
			return 0, fmt.Errorf("invalid amount %q", s)
		}
		f, err = strconv.ParseInt(frac, 10, 64)
		if err != nil || f < 0 {
			return 0, fmt.Errorf("invalid amount %q", s)
		}
		if len(frac) == 1 {
			f *= 10
		}
	}
	return w*100 + f, nil
}

// FormatCents renders cents as a NUMERIC(10,2) literal, for example "35.20".
func FormatCents(c int64) string {
	return fmt.Sprintf("%d.%02d", c/100, c%100)
}

// DiscountPercent returns the rounded percentage saved, or 0 when there is no discount.
// The job computes this itself from base and current price; it never uses a store label.
func DiscountPercent(baseCents, currentCents int64) int {
	if baseCents <= 0 || currentCents >= baseCents {
		return 0
	}
	return int(((baseCents-currentCents)*100 + baseCents/2) / baseCents)
}
