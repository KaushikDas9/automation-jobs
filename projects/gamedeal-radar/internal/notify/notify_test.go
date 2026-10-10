package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func sample() Data {
	return Data{
		SlotLabel: SlotLabel(time.Date(2026, 10, 10, 18, 30, 0, 0, time.UTC)),
		Attempt:   2, Total: 500, Updated: 480, Skipped: 18, Failed: 2, Retried: 20,
		CoveragePct: "99.6", Duration: "7m12s", RunURL: "https://github.com/o/r/actions/runs/1",
	}
}

func fast(url string) Notifier {
	return Notifier{URL: url, Client: http.DefaultClient, Timeout: time.Second, Retries: 1, Backoff: time.Millisecond}
}

func TestSlotLabel(t *testing.T) {
	got := SlotLabel(time.Date(2026, 10, 10, 18, 30, 0, 0, time.UTC))
	if got != "00:00 IST / 18:30 UTC, 10 Oct" {
		t.Fatalf("got %q", got)
	}
}

func TestRunURL(t *testing.T) {
	env := map[string]string{"GITHUB_SERVER_URL": "https://github.com", "GITHUB_REPOSITORY": "o/r", "GITHUB_RUN_ID": "9"}
	if got := RunURL(func(k string) string { return env[k] }); got != "https://github.com/o/r/actions/runs/9" {
		t.Fatalf("got %q", got)
	}
	if got := RunURL(func(string) string { return "" }); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestDisabledSendsNothing(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer srv.Close()
	n := New(func(string) string { return "" })
	if n.Enabled() {
		t.Fatal("should be disabled")
	}
	if err := n.Send(context.Background(), Success, sample()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("unexpected request")
	}
}

func TestSuccessPostsExpectedText(t *testing.T) {
	var got map[string]string
	var ct string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ct = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&got)
	}))
	defer srv.Close()
	if err := fast(srv.URL).Send(context.Background(), Success, sample()); err != nil {
		t.Fatal(err)
	}
	want := "[GameDeal Radar] :white_check_mark: GameDeal Radar 00:00 IST / 18:30 UTC, 10 Oct complete. 500 games, 480 updated, 18 skipped, 20 retried, 99.6% coverage (7m12s). https://github.com/o/r/actions/runs/1"
	if got["text"] != want {
		t.Fatalf("got %q\nwant %q", got["text"], want)
	}
	if ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
}

func TestAllDefaultTemplatesRender(t *testing.T) {
	for _, k := range kinds {
		out, err := fast("x").render(k, sample())
		if err != nil || out == "" || strings.Contains(out, "{{") {
			t.Fatalf("%s: %q %v", k, out, err)
		}
		if !strings.HasPrefix(out, "[GameDeal Radar] ") {
			t.Fatalf("%s: missing project tag: %q", k, out)
		}
	}
}

func envOf(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestWebhookEnvOrder(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"shared only", map[string]string{"SLACK_WEBHOOK_URL": "https://hooks/shared"}, "https://hooks/shared"},
		{"legacy only", map[string]string{"GDR_SLACK_WEBHOOK_URL": "https://hooks/legacy"}, "https://hooks/legacy"},
		{"shared wins", map[string]string{"SLACK_WEBHOOK_URL": "https://hooks/shared", "GDR_SLACK_WEBHOOK_URL": "https://hooks/legacy"}, "https://hooks/shared"},
		{"empty shared falls back", map[string]string{"SLACK_WEBHOOK_URL": "", "GDR_SLACK_WEBHOOK_URL": "https://hooks/legacy"}, "https://hooks/legacy"},
		{"blank shared falls back", map[string]string{"SLACK_WEBHOOK_URL": "   ", "GDR_SLACK_WEBHOOK_URL": "https://hooks/legacy"}, "https://hooks/legacy"},
		{"both empty", map[string]string{"SLACK_WEBHOOK_URL": "", "GDR_SLACK_WEBHOOK_URL": ""}, ""},
		{"neither set", map[string]string{}, ""},
		{"shared is trimmed", map[string]string{"SLACK_WEBHOOK_URL": "  https://hooks/shared \n"}, "https://hooks/shared"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := New(envOf(c.env))
			if n.URL != c.want {
				t.Fatalf("URL %q, want %q", n.URL, c.want)
			}
			if n.Enabled() != (c.want != "") {
				t.Fatalf("Enabled %v for URL %q", n.Enabled(), n.URL)
			}
		})
	}
}

func TestSharedWebhookPostsMessage(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer srv.Close()
	n := New(envOf(map[string]string{"SLACK_WEBHOOK_URL": srv.URL}))
	n.Backoff = time.Millisecond
	if err := n.Send(context.Background(), AttemptFailed, sample()); err != nil {
		t.Fatal(err)
	}
	if atomic.LoadInt32(&hits) != 1 {
		t.Fatalf("hits %d", hits)
	}
}

func TestTemplateOverrideFromDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "success.tmpl"), []byte("custom {{.Total}} games\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "final_failed.tmpl"), []byte("{{.Nope}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	n := fast("x")
	n.TemplateDir = dir
	if out, _ := n.render(Success, sample()); out != "custom 500 games" {
		t.Fatalf("got %q", out)
	}
	// Broken override falls back to the default.
	out, err := n.render(FinalFailed, sample())
	if err != nil || !strings.Contains(out, "FAILED") {
		t.Fatalf("got %q %v", out, err)
	}
	// Missing override falls back to the default.
	if out, _ := n.render(AttemptFailed, sample()); !strings.Contains(out, "attempt 2 of 6") {
		t.Fatalf("got %q", out)
	}
}

func TestRetryThenSuccess(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&hits, 1) == 1 {
			w.WriteHeader(500)
		}
	}))
	defer srv.Close()
	if err := fast(srv.URL).Send(context.Background(), Success, sample()); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Fatalf("hits %d", hits)
	}
}

func TestFailureNeverLeaksURL(t *testing.T) {
	var logs bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(old)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) }))
	url := srv.URL + "/services/T000/B000/SECRETTOKEN"
	err := fast(url).Send(context.Background(), Success, sample())
	if err == nil || strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Fatalf("err %v", err)
	}
	srv.Close() // connection refused: Go's error text embeds the URL
	err = fast(url).Send(context.Background(), Success, sample())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "SECRETTOKEN") || strings.Contains(err.Error(), url) {
		t.Fatalf("leaked: %v", err)
	}
	if strings.Contains(logs.String(), "SECRETTOKEN") {
		t.Fatal("leaked in logs")
	}
	// Invalid URL error path.
	bad := fast("http://exa mple.com/SECRETTOKEN")
	if err := bad.Send(context.Background(), Success, sample()); err != nil && strings.Contains(err.Error(), "SECRETTOKEN") {
		t.Fatalf("leaked: %v", err)
	}
}
