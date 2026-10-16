package steamtags

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const tagList = `{"response":{"tags":[{"tagid":122,"name":"RPG"},{"tagid":1695,"name":"Open World"},{"tagid":4115,"name":"Naughty"}]}}`

func TestParseTagList(t *testing.T) {
	names, err := ParseTagList([]byte(`{"response":{"tags":[{"tagid":122,"name":" RPG "},{"tagid":0,"name":"zero"},{"tagid":5,"name":"  "}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(names, map[int]string{122: "RPG"}) {
		t.Fatalf("names = %v", names)
	}
	if _, err := ParseTagList([]byte(`{"response":{"tags":[]}}`)); err == nil {
		t.Fatal("an empty tag list must be an error")
	}
	if _, err := ParseTagList([]byte(`not json`)); err == nil {
		t.Fatal("bad json must be an error")
	}
}

func TestParseItems(t *testing.T) {
	items, err := ParseItems([]byte(`{"response":{"store_items":[{"appid":10,"success":1,"tags":[{"tagid":1,"weight":5}]},{"appid":11,"success":2}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].AppID != 10 || items[0].Tags[0].Weight != 5 || items[1].Success != 2 {
		t.Fatalf("items = %+v", items)
	}
	if _, err := ParseItems([]byte(`{`)); err == nil {
		t.Fatal("bad json must be an error")
	}
}

func TestMapTagsOrderFilterAndCap(t *testing.T) {
	names := map[int]string{1: "A", 2: "B", 3: "C"}
	got := MapTags([]ItemTag{
		{TagID: 2, Weight: 10},
		{TagID: 99, Weight: 50}, // unknown name, dropped
		{TagID: 1, Weight: 30},
		{TagID: 2, Weight: 5}, // duplicate, dropped
		{TagID: 3, Weight: 30},
	}, names)
	want := []Tag{{ID: "1", Description: "A"}, {ID: "3", Description: "C"}, {ID: "2", Description: "B"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("MapTags = %+v, want %+v", got, want)
	}

	var many []ItemTag
	bigNames := map[int]string{}
	for i := 1; i <= 15; i++ {
		many = append(many, ItemTag{TagID: i, Weight: 100 - i})
		bigNames[i] = fmt.Sprintf("T%d", i)
	}
	if n := len(MapTags(many, bigNames)); n != MaxTags {
		t.Fatalf("cap = %d, want %d", n, MaxTags)
	}
	if got := MapTags(nil, names); got == nil || len(got) != 0 {
		t.Fatalf("no tags must give an empty non-nil slice, got %#v", got)
	}
}

func TestItemsInputShape(t *testing.T) {
	raw, err := ItemsInput([]int{1091500, 730})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	want := `{"context":{"country_code":"US","language":"english"},"data_request":{"include_tag_count":10},"ids":[{"appid":1091500},{"appid":730}]}`
	var wantMap map[string]any
	_ = json.Unmarshal([]byte(want), &wantMap)
	if !reflect.DeepEqual(got, wantMap) {
		t.Fatalf("input = %s", raw)
	}
}

// fakeSource answers GetItems from a script. Each call records its batch size.
type fakeSource struct {
	calls []int
	fail  map[int]error // by call index
	items func(ids []int) []StoreItem
}

func (f *fakeSource) GetItems(_ context.Context, ids []int) ([]StoreItem, error) {
	i := len(f.calls)
	f.calls = append(f.calls, len(ids))
	if err := f.fail[i]; err != nil {
		return nil, err
	}
	if f.items == nil {
		return nil, nil
	}
	return f.items(ids), nil
}

func TestCollectBatchesSkipsAndFailures(t *testing.T) {
	ids := make([]int, 0, 125)
	for i := 1; i <= 125; i++ {
		ids = append(ids, i)
	}
	names := map[int]string{1: "RPG"}
	src := &fakeSource{
		fail: map[int]error{1: errors.New("HTTP 500")},
		items: func(batch []int) []StoreItem {
			out := []StoreItem{
				{AppID: batch[0], Success: 1, Tags: []ItemTag{{TagID: 1, Weight: 9}}},
				{AppID: batch[1], Success: 0, Tags: []ItemTag{{TagID: 1, Weight: 9}}},
				{AppID: batch[2], Success: 1},
				{AppID: batch[3], Success: 1, Tags: []ItemTag{{TagID: 777, Weight: 9}}},
			}
			return out
		},
	}
	res, err := Collect(t.Context(), src, names, ids)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(src.calls, []int{50, 50, 25}) {
		t.Fatalf("batch sizes = %v", src.calls)
	}
	if len(res.Failed) != 50 || res.Failed[0] != 51 {
		t.Fatalf("failed = %d ids starting %v", len(res.Failed), res.Failed[:1])
	}
	if len(res.Tags) != 2 || res.Tags[1][0].Description != "RPG" || res.Tags[101][0].ID != "1" {
		t.Fatalf("tags = %v", res.Tags)
	}
}

func TestCollectStopsOnBlockedAndCancel(t *testing.T) {
	src := &fakeSource{fail: map[int]error{0: fmt.Errorf("HTTP 429: %w", ErrBlocked)}}
	res, err := Collect(t.Context(), src, map[int]string{}, []int{1, 2, 3})
	if !errors.Is(err, ErrBlocked) || len(src.calls) != 1 || len(res.Failed) != 0 {
		t.Fatalf("blocked: err=%v calls=%v failed=%v", err, src.calls, res.Failed)
	}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	src2 := &fakeSource{fail: map[int]error{0: errors.New("boom")}}
	if _, err := Collect(ctx, src2, map[int]string{}, []int{1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled run: %v", err)
	}
}

func TestCollectEmptyInput(t *testing.T) {
	src := &fakeSource{}
	res, err := Collect(t.Context(), src, map[int]string{}, nil)
	if err != nil || len(src.calls) != 0 || res.Tags == nil {
		t.Fatalf("empty: err=%v calls=%v", err, src.calls)
	}
}

// testClient points a Client at srv with no pacing and no backoff.
func testClient(srv *httptest.Server) *Client {
	return &Client{
		HTTP:        srv.Client(),
		TagListURL:  srv.URL + "/taglist",
		ItemsURL:    srv.URL + "/items",
		UserAgent:   UserAgent,
		MaxAttempts: 3,
	}
}

func TestClientTagNamesAndItems(t *testing.T) {
	var sawUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawUA = r.Header.Get("User-Agent")
		switch r.URL.Path {
		case "/taglist":
			if r.URL.Query().Get("language") != "english" {
				http.Error(w, "language", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, tagList)
		case "/items":
			var in map[string]any
			if err := json.Unmarshal([]byte(r.URL.Query().Get("input_json")), &in); err != nil {
				http.Error(w, "input", http.StatusBadRequest)
				return
			}
			fmt.Fprint(w, `{"response":{"store_items":[{"appid":1091500,"success":1,"tags":[{"tagid":1695,"weight":873}]}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := testClient(srv)
	names, err := c.TagNames(t.Context())
	if err != nil || names[1695] != "Open World" {
		t.Fatalf("TagNames: %v %v", names, err)
	}
	if sawUA != UserAgent {
		t.Fatalf("user agent = %q", sawUA)
	}
	items, err := c.GetItems(t.Context(), []int{1091500})
	if err != nil || len(items) != 1 || items[0].Tags[0].TagID != 1695 {
		t.Fatalf("GetItems: %+v %v", items, err)
	}
}

func TestClientRetriesServerErrorsThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, tagList)
	}))
	defer srv.Close()

	if _, err := testClient(srv).TagNames(t.Context()); err != nil {
		t.Fatalf("expected success after retries: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
}

func TestClientRateLimitExhaustedIsBlocked(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	_, err := testClient(srv).GetItems(t.Context(), []int{1})
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("429 after retries must be ErrBlocked: %v", err)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d, want 3", calls.Load())
	}
}

func TestClientStatusClassification(t *testing.T) {
	for _, tc := range []struct {
		status      int
		wantBlocked bool
		wantCalls   int32
	}{
		{http.StatusUnauthorized, true, 1},
		{http.StatusForbidden, true, 1},
		{http.StatusNotFound, false, 1},
		{http.StatusBadRequest, false, 1},
	} {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(tc.status)
		}))
		_, err := testClient(srv).GetItems(t.Context(), []int{1})
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error", tc.status)
		}
		if errors.Is(err, ErrBlocked) != tc.wantBlocked {
			t.Errorf("status %d: blocked = %v, want %v", tc.status, errors.Is(err, ErrBlocked), tc.wantBlocked)
		}
		if calls.Load() != tc.wantCalls {
			t.Errorf("status %d: calls = %d, want %d", tc.status, calls.Load(), tc.wantCalls)
		}
	}
}

func TestClientBadBodyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>")
	}))
	defer srv.Close()
	if _, err := testClient(srv).GetItems(t.Context(), []int{1}); err == nil {
		t.Fatal("a body that is not JSON must be an error")
	}
	if _, err := testClient(srv).TagNames(t.Context()); err == nil {
		t.Fatal("a body that is not JSON must be an error for the tag list")
	}
}

func TestClientNetworkErrorIsRetriedThenFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // every request now fails to connect
	c := testClient(srv)
	c.MaxAttempts = 2
	_, err := c.GetItems(t.Context(), []int{1})
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrBlocked) {
		t.Fatalf("network failure must be a plain error: %v", err)
	}
}

func TestClientHonoursCancelDuringPacing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, tagList)
	}))
	defer srv.Close()
	c := testClient(srv)
	c.Gap = time.Hour
	if _, err := c.TagNames(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := c.TagNames(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("pacing must return on cancel: %v", err)
	}
}

func TestClientCancelDuringBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	c := testClient(srv)
	c.Backoff = time.Hour
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.GetItems(ctx, []int{1}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("backoff must stop on cancel: %v", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := map[string]time.Duration{"3": 3 * time.Second, "abc": 0, "-1": 0, "": 0, "9999": maxRetryWait}
	for in, want := range cases {
		if got := parseRetryAfter(in); got != want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSleepCtxZero(t *testing.T) {
	if err := sleepCtx(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
}

func TestNewClientDefaults(t *testing.T) {
	c := NewClient()
	if c.TagListURL != TagListURL || c.ItemsURL != ItemsURL || c.UserAgent != UserAgent {
		t.Fatalf("defaults: %+v", c)
	}
	if !strings.HasPrefix(c.TagListURL, "https://api.steampowered.com/") {
		t.Fatal("live endpoints must be on api.steampowered.com")
	}
	if u, err := url.Parse(ItemsURL); err != nil || u.Scheme != "https" {
		t.Fatal("items url must be https")
	}
}
