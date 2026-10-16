package storeprices

import "fmt"

// MaxJumpShare is the largest relative change in the regular price (the base price, or the
// current price when there is no base) from the stored one that is accepted without a human
// look. A bigger jump is skipped and logged, and the stored row stays. Sales are not held
// back: a 90% discount changes the current price but not the regular price.
const MaxJumpShare = 0.80

// CheckPrice returns an empty string when p may be written, or the reason it must be
// skipped. expectCurrency is the currency the job expects for this store and region.
// storedCents is the regular price already stored for the same game, store and region
// (its base price, else its current price), or nil when no such row exists. Rows without a price (not_listed, listed_no_price)
// have nothing to check.
func CheckPrice(p Price, expectCurrency string, storedCents *int64) string {
	if p.Status != StatusOK && p.Status != StatusFree {
		return ""
	}
	if p.CurrentCents == nil {
		return "price missing"
	}
	cur := *p.CurrentCents
	if cur < 0 {
		return "negative price"
	}
	if cur == 0 && p.Status != StatusFree {
		return "zero price on a non-free row"
	}
	if p.BaseCents != nil && *p.BaseCents < cur {
		return "base price below current price"
	}
	if p.Currency != expectCurrency {
		return fmt.Sprintf("unexpected currency %q (want %s)", p.Currency, expectCurrency)
	}
	ref := cur
	if p.BaseCents != nil && *p.BaseCents > 0 {
		ref = *p.BaseCents
	}
	if storedCents != nil && *storedCents > 0 && ref > 0 {
		old := float64(*storedCents)
		diff := float64(ref) - old
		if diff < 0 {
			diff = -diff
		}
		if diff/old > MaxJumpShare {
			return fmt.Sprintf("price jumped more than %d%% from stored %s",
				int(MaxJumpShare*100), FormatCents(*storedCents))
		}
	}
	return ""
}
