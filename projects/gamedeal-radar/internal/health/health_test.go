package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// failOnUse is a transport that fails the test if any request is made.
type failOnUse struct{ t *testing.T }

func (f failOnUse) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Fatal("no request should be sent when the healthcheck URL is unset")
	return nil, nil
}

func TestPingSkippedWhenUnset(t *testing.T) {
	p := New("")
	p.Client = &http.Client{Transport: failOnUse{t}}
	if p.Enabled() {
		t.Fatal("empty URL must be disabled")
	}
	if err := p.Success(context.Background()); err != nil {
		t.Fatalf("Success with unset URL: %v", err)
	}
	if err := p.Fail(context.Background()); err != nil {
		t.Fatalf("Fail with unset URL: %v", err)
	}
}

func TestPingSuccessAndFailPaths(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(srv.URL + "/abc-uuid")
	p.Backoff = 0
	if err := p.Success(context.Background()); err != nil {
		t.Fatalf("success ping: %v", err)
	}
	if err := p.Fail(context.Background()); err != nil {
		t.Fatalf("fail ping: %v", err)
	}
	if len(paths) != 2 || paths[0] != "/abc-uuid" || paths[1] != "/abc-uuid/fail" {
		t.Fatalf("paths = %v", paths)
	}
}

func TestPingRetriesOnceThenSucceeds(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	p := New(srv.URL)
	p.Backoff = 0
	if err := p.Success(context.Background()); err != nil {
		t.Fatalf("expected success after one retry, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestPingErrorNeverContainsURL(t *testing.T) {
	// A closed loopback port: the request fails fast and the error includes the URL unless scrubbed.
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL + "/secret-check-id"
	srv.Close()

	p := New(url)
	p.Backoff = 0
	p.Timeout = 2 * time.Second
	err := p.Success(context.Background())
	if err == nil {
		t.Fatal("expected an error from a closed server")
	}
	if strings.Contains(err.Error(), "secret-check-id") || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("healthcheck URL leaked into error: %q", err.Error())
	}
}
