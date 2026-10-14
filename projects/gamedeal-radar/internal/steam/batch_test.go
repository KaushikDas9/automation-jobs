package steam

import (
	"reflect"
	"testing"
)

func TestParseBatchPrices(t *testing.T) {
	cases := []struct {
		name string
		body string
		want map[string]BatchPrice
	}{
		{
			name: "discounted usd price converted from cents",
			body: `{"1091500":{"success":true,"data":{"price_overview":{"currency":"USD","initial":5999,"final":1799,"discount_percent":70}}},
				"730":{"success":true,"data":[]}}`,
			want: map[string]BatchPrice{
				"1091500": {Currency: "USD", Base: 59.99, Current: 17.99, DiscountPct: 70},
				"730":     {Unavailable: true},
			},
		},
		{
			name: "full price is not a deal but still priced",
			body: `{"10":{"success":true,"data":{"price_overview":{"currency":"USD","initial":999,"final":999,"discount_percent":0}}}}`,
			want: map[string]BatchPrice{
				"10": {Currency: "USD", Base: 9.99, Current: 9.99, DiscountPct: 0},
			},
		},
		{
			name: "success false is unavailable",
			body: `{"999999999":{"success":false}}`,
			want: map[string]BatchPrice{
				"999999999": {Unavailable: true},
			},
		},
		{
			name: "no price_overview is unavailable",
			body: `{"730":{"success":true,"data":{"is_free":true}}}`,
			want: map[string]BatchPrice{
				"730": {Unavailable: true},
			},
		},
		{
			name: "data null is unavailable",
			body: `{"5":{"success":true,"data":null}}`,
			want: map[string]BatchPrice{
				"5": {Unavailable: true},
			},
		},
		{
			name: "empty body is an empty map",
			body: `{}`,
			want: map[string]BatchPrice{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseBatchPrices([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseBatchPrices: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseBatchPrices_Fixtures(t *testing.T) {
	cases := []struct {
		name  string
		file  string
		appID string
		want  BatchPrice
	}{
		{
			name:  "cyberpunk KRW from real fixture",
			file:  "full_1091500.json",
			appID: "1091500",
			want:  BatchPrice{Currency: "KRW", Base: 66000, Current: 19800, DiscountPct: 70},
		},
		{
			name:  "elden ring INR with no discount",
			file:  "full_1245620.json",
			appID: "1245620",
			want:  BatchPrice{Currency: "INR", Base: 3599, Current: 3599, DiscountPct: 0},
		},
		{
			name:  "counter-strike has no price overview",
			file:  "full_730.json",
			appID: "730",
			want:  BatchPrice{Unavailable: true},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseBatchPrices(mustReadFixture(t, tc.file))
			if err != nil {
				t.Fatalf("ParseBatchPrices: %v", err)
			}
			if p, ok := got[tc.appID]; !ok || !reflect.DeepEqual(p, tc.want) {
				t.Errorf("got %+v (present=%t), want %+v", p, ok, tc.want)
			}
		})
	}
}

func TestParseBatchPrices_InvalidJSON(t *testing.T) {
	if _, err := ParseBatchPrices([]byte(`{"1091500":`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}
