package storeprices

import (
	"context"

	"net/http"
	"net/http/httptest"

	"testing"
)

func TestParseUSDINRAcceptsRangeAndRejectsOutside(t *testing.T) {
	good := `{"amount":1.0,"base":"USD","date":"2026-10-15","rates":{"INR":85.4321}}`
	rate, date, err := ParseUSDINR([]byte(good))
	if err != nil || rate != 85.4321 || date != "2026-10-15" {
		t.Fatalf("good = %v %q %v", rate, date, err)
	}

	edges := map[string]bool{
		`{"base":"USD","date":"2026-10-15","rates":{"INR":40}}`:     true,
		`{"base":"USD","date":"2026-10-15","rates":{"INR":200}}`:    true,
		`{"base":"USD","date":"2026-10-15","rates":{"INR":39.99}}`:  false,
		`{"base":"USD","date":"2026-10-15","rates":{"INR":200.01}}`: false,
		`{"base":"USD","date":"2026-10-15","rates":{"INR":0}}`:      false,
		`{"base":"USD","date":"2026-10-15","rates":{"EUR":0.9}}`:    false,
		`{"base":"EUR","date":"2026-10-15","rates":{"INR":85}}`:     false,
		`{"base":"USD","date":"15/10/2026","rates":{"INR":85}}`:     false,
		`{not json`: false,
	}
	for body, want := range edges {
		_, _, err := ParseUSDINR([]byte(body))
		if (err == nil) != want {
			t.Errorf("ParseUSDINR(%s) err=%v, want ok=%v", body, err, want)
		}
	}
}

func TestFetchUSDINRStatusHandling(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		if r.URL.Query().Get("symbols") != "INR" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	f := NewFetcher(0)
	_, _, err := FetchUSDINR(context.Background(), f, srv.URL+"/v1/latest?base=USD&symbols=INR")
	if !IsTransient(err) {
		t.Fatalf("503 must be transient, got %v", err)
	}
	if gotUA != "GameDealRadar-jobs/1.0" {
		t.Fatalf("User-Agent = %q", gotUA)
	}
	if FXLatestURL != "https://api.frankfurter.dev/v1/latest?base=USD&symbols=INR" {
		t.Fatalf("FXLatestURL changed: %s", FXLatestURL)
	}
}

func TestFetchUSDINRSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"amount":1,"base":"USD","date":"2026-10-15","rates":{"INR":84.5}}`))
	}))
	defer srv.Close()
	rate, date, err := FetchUSDINR(context.Background(), NewFetcher(0), srv.URL)
	if err != nil || rate != 84.5 || date != "2026-10-15" {
		t.Fatalf("FetchUSDINR = %v %q %v", rate, date, err)
	}
	if _, _, err := FetchUSDINR(context.Background(), NewFetcher(0), "http://127.0.0.1:1/x"); !IsTransient(err) {
		t.Fatalf("unreachable host must be a transient error: %v", err)
	}
}
