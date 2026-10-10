package steam

import (
	"errors"
	"testing"
	"time"
)

var fixedNow = time.Date(2026, 10, 10, 21, 30, 0, 0, time.UTC)

func TestParseAppDetails_Discounted(t *testing.T) {
	body := []byte(`{"620":{"success":true,"data":{"name":"Portal 2","price_overview":{
		"currency":"INR","initial":99900,"final":29900,"discount_percent":70}}}}`)

	res, err := ParseAppDetails("620", "in", body, fixedNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.BasePrice != 999.0 || res.CurrentPrice != 299.0 {
		t.Fatalf("prices: base=%v current=%v, want 999 and 299", res.BasePrice, res.CurrentPrice)
	}
	if res.DiscountPct != 70 || !res.IsDeal || res.IsFree {
		t.Fatalf("deal flags wrong: discount=%d deal=%t free=%t", res.DiscountPct, res.IsDeal, res.IsFree)
	}
	if res.Currency != "INR" || res.Country != "in" || res.ExternalAppID != "620" {
		t.Fatalf("metadata wrong: %+v", res)
	}
	if string(res.RawPayload) != string(body) {
		t.Fatalf("raw payload not preserved")
	}
}

func TestParseAppDetails_NotDiscounted(t *testing.T) {
	body := []byte(`{"10":{"success":true,"data":{"price_overview":{"currency":"INR","initial":49900,"final":49900,"discount_percent":0}}}}`)
	res, err := ParseAppDetails("10", "in", body, fixedNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsDeal {
		t.Fatal("0% discount must not be a deal")
	}
}

func TestParseAppDetails_Free(t *testing.T) {
	body := []byte(`{"730":{"success":true,"data":{"is_free":true}}}`)
	res, err := ParseAppDetails("730", "in", body, fixedNow)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsFree || res.CurrentPrice != 0 || res.Currency != "INR" || res.IsDeal {
		t.Fatalf("free game parsed wrong: %+v", res)
	}
}

func TestParseAppDetails_Errors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"missing app key", `{"999":{"success":true,"data":{}}}`, ErrNotFound},
		{"success false", `{"1":{"success":false}}`, ErrNotFound},
		{"empty data array", `{"1":{"success":true,"data":[]}}`, ErrPriceUnavailable},
		{"no price overview", `{"1":{"success":true,"data":{"name":"Soon"}}}`, ErrPriceUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseAppDetails("1", "in", []byte(tc.body), fixedNow)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestParseAppDetails_InvalidJSON(t *testing.T) {
	if _, err := ParseAppDetails("1", "in", []byte("not json"), fixedNow); err == nil {
		t.Fatal("expected a parse error for invalid JSON")
	}
}

func TestBackoffCapsAtMax(t *testing.T) {
	c := NewClient(DefaultConfig())
	for attempt := 0; attempt < 20; attempt++ {
		got := c.backoff(attempt)
		if got > c.cfg.MaxBackoff+c.cfg.MaxBackoff/4 {
			t.Fatalf("attempt %d backoff %v exceeds cap plus jitter", attempt, got)
		}
		if got < c.cfg.BaseBackoff && attempt == 0 {
			t.Fatalf("attempt 0 backoff %v below base", got)
		}
	}
}
