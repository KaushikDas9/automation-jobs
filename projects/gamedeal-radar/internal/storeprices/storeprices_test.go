package storeprices

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestNormalizeTitle(t *testing.T) {
	cases := map[string]string{
		"Cyberpunk 2077":            "cyberpunk 2077",
		"Cyberpunk 2077™":           "cyberpunk 2077",
		"The Witcher® 3: Wild Hunt": "the witcher 3 wild hunt",
		"Baldur’s Gate 3":           "baldurs gate 3",
		"Baldur's   Gate  3":        "baldurs gate 3",
		"Half-Life 2 ©":             "half life 2",
		"  Spaces\tand\nlines  ":    "spaces and lines",
		"Tom & Jerry: Chase!":       "tom jerry chase",
		"ALL CAPS":                  "all caps",
	}
	for in, want := range cases {
		if got := NormalizeTitle(in); got != want {
			t.Errorf("NormalizeTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseCents(t *testing.T) {
	ok := map[string]int64{"35.20": 3520, "35": 3500, "10.5": 1050, "0": 0, "0.05": 5}
	for in, want := range ok {
		got, err := ParseCents(in)
		if err != nil || got != want {
			t.Errorf("ParseCents(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", ".5", "1.234", "-3", "abc", "1."} {
		if _, err := ParseCents(bad); err == nil {
			t.Errorf("ParseCents(%q) accepted a bad amount", bad)
		}
	}
}

func TestFormatCentsAndDiscount(t *testing.T) {
	if got := FormatCents(3520); got != "35.20" {
		t.Fatalf("FormatCents = %q", got)
	}
	if got := DiscountPercent(5999, 1799); got != 70 {
		t.Fatalf("DiscountPercent(5999,1799) = %d, want 70", got)
	}
	if DiscountPercent(1000, 1000) != 0 || DiscountPercent(1000, 1200) != 0 || DiscountPercent(0, 0) != 0 {
		t.Fatal("no discount must give 0")
	}
}

func TestSleepCtxHonoursCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := SleepCtx(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("SleepCtx on cancelled ctx = %v", err)
	}
	if err := SleepCtx(context.Background(), 0); err != nil {
		t.Fatalf("zero wait must return at once: %v", err)
	}
}

func TestStatusErrorClassification(t *testing.T) {
	for _, s := range []int{401, 403, 429} {
		err := StatusError(s, "x")
		if !IsBlocked(err) || IsTransient(err) {
			t.Errorf("status %d must be blocked, not transient: %v", s, err)
		}
	}
	for _, s := range []int{500, 502, 503} {
		err := StatusError(s, "x")
		if !IsTransient(err) || IsBlocked(err) {
			t.Errorf("status %d must be transient: %v", s, err)
		}
	}
	err := StatusError(404, "x")
	if IsTransient(err) || IsBlocked(err) {
		t.Errorf("404 must be permanent: %v", err)
	}
}

func TestFetcherSendsUserAgentAndClassifiesNetworkErrors(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusTeapot)
	}))
	f := NewFetcher(0)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	resp, err := f.Do(context.Background(), req)
	if err != nil || resp.Status != http.StatusTeapot {
		t.Fatalf("Do = %+v, %v", resp, err)
	}
	if gotUA != "GameDealRadar-jobs/1.0" {
		t.Fatalf("User-Agent = %q", gotUA)
	}
	srv.Close()

	req, _ = http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	if _, err := f.Do(context.Background(), req); !IsTransient(err) {
		t.Fatalf("connection refused must be transient, got %v", err)
	}
}

func TestLimiterSpacesRequests(t *testing.T) {
	l := &Limiter{Interval: 60 * time.Millisecond}
	start := time.Now()
	for i := 0; i < 3; i++ {
		if err := l.Wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if el := time.Since(start); el < 110*time.Millisecond {
		t.Fatalf("3 requests at 60ms gap took %v", el)
	}
	var nilLimiter *Limiter
	if err := nilLimiter.Wait(context.Background()); err != nil {
		t.Fatalf("nil limiter must not wait: %v", err)
	}
}

func TestCheckPriceRules(t *testing.T) {
	c := func(v int64) *int64 { return &v }
	ok := Price{StoreCode: "gog", Region: "US", Currency: "USD", Status: StatusOK, CurrentCents: c(1799), BaseCents: c(5999)}
	if r := CheckPrice(ok, "USD", nil); r != "" {
		t.Fatalf("valid row rejected: %s", r)
	}
	cases := []struct {
		name   string
		p      Price
		stored *int64
		want   string
	}{
		{"missing price", Price{Status: StatusOK, Currency: "USD"}, nil, "price missing"},
		{"zero on ok", Price{Status: StatusOK, Currency: "USD", CurrentCents: c(0)}, nil, "zero price on a non-free row"},
		{"negative", Price{Status: StatusOK, Currency: "USD", CurrentCents: c(-5)}, nil, "negative price"},
		{"base below current", Price{Status: StatusOK, Currency: "USD", CurrentCents: c(2000), BaseCents: c(1000)}, nil, "base price below current price"},
		{"wrong currency", Price{Status: StatusOK, Currency: "EUR", CurrentCents: c(1000)}, nil, `unexpected currency "EUR" (want USD)`},
		{"jump over 80 percent", Price{Status: StatusOK, Currency: "USD", CurrentCents: c(5000)}, c(1000), "price jumped more than 80% from stored 10.00"},
	}
	for _, tc := range cases {
		got := CheckPrice(tc.p, "USD", tc.stored)
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s: got %q, want prefix %q", tc.name, got, tc.want)
		}
	}
	// A free row may be zero.
	free := Price{Status: StatusFree, Currency: "USD", CurrentCents: c(0)}
	if r := CheckPrice(free, "USD", nil); r != "" {
		t.Errorf("free zero price rejected: %s", r)
	}
	// A 50% move is accepted.
	if r := CheckPrice(Price{Status: StatusOK, Currency: "USD", CurrentCents: c(1500)}, "USD", c(1000)); r != "" {
		t.Errorf("50%% move rejected: %s", r)
	}
	// Rows without a price have nothing to check.
	if r := CheckPrice(NotListed(StoreGOG, RegionUS), "USD", c(9999)); r != "" {
		t.Errorf("not_listed row rejected: %s", r)
	}
}

func TestRunPassesRetriesThenSucceeds(t *testing.T) {
	var waits []time.Duration
	o := PassOptions{
		RetryWait1: time.Second,
		RetryWait2: 2 * time.Second,
		Sleep: func(_ context.Context, d time.Duration) error {
			waits = append(waits, d)
			return nil
		},
	}
	attempts := map[string]int{}
	items := []string{"a", "b", "c"}
	left, rep := RunPasses(context.Background(), items, o, func(s string) Verdict {
		attempts[s]++
		// "b" fails transiently twice, then succeeds on the second retry pass.
		if s == "b" && attempts[s] < 3 {
			return Retry
		}
		return Done
	})
	if len(left) != 0 || rep.Stopped {
		t.Fatalf("left=%v stopped=%v", left, rep.Stopped)
	}
	if rep.Retried != 1 {
		t.Fatalf("Retried = %d, want 1", rep.Retried)
	}
	if len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("waits = %v", waits)
	}
}

func TestRunPassesLeavesFailuresAndStopsOnBlock(t *testing.T) {
	o := PassOptions{Sleep: func(context.Context, time.Duration) error { return nil }}
	left, _ := RunPasses(context.Background(), []string{"a"}, o, func(string) Verdict { return Retry })
	if len(left) != 1 {
		t.Fatalf("persistent transient must be left for the caller, got %v", left)
	}

	calls := 0
	_, rep := RunPasses(context.Background(), []string{"a", "b", "c"}, o, func(string) Verdict {
		calls++
		return Stop
	})
	if !rep.Stopped || calls != 1 {
		t.Fatalf("Stop must end the pass at once: stopped=%v calls=%d", rep.Stopped, calls)
	}
}

func TestRunPassesSkipsWaitWithoutTimeLeft(t *testing.T) {
	now := time.Date(2026, 10, 16, 3, 0, 0, 0, time.UTC)
	slept := false
	o := PassOptions{
		RetryWait1: 2 * time.Minute,
		Deadline:   now.Add(4 * time.Minute), // only 4 minutes left, a 5 minute minimum is needed
		Now:        func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error {
			slept = true
			return nil
		},
	}
	left, _ := RunPasses(context.Background(), []string{"a"}, o, func(string) Verdict { return Retry })
	if slept || len(left) != 1 {
		t.Fatalf("wait must be skipped near the deadline: slept=%v left=%v", slept, left)
	}
}

func TestFileFixtureIsReadable(t *testing.T) {
	if _, err := os.Stat("testdata/gog_catalog.json"); err != nil {
		t.Fatal(err)
	}
}

func TestCheckPriceDoesNotBlockSales(t *testing.T) {
	c := func(v int64) *int64 { return &v }
	// Regular price 59.99 stored; a 90% sale keeps the regular price, so it is accepted.
	sale := Price{Status: StatusOK, Currency: "USD", CurrentCents: c(599), BaseCents: c(5999)}
	if r := CheckPrice(sale, "USD", c(5999)); r != "" {
		t.Fatalf("a 90%% sale must not be skipped, got %q", r)
	}
	// A regular price that suddenly changes by more than 80% is still skipped.
	wild := Price{Status: StatusOK, Currency: "USD", CurrentCents: c(30000), BaseCents: c(30000)}
	if r := CheckPrice(wild, "USD", c(5999)); r == "" {
		t.Fatal("a wild regular price jump must be skipped")
	}
}
