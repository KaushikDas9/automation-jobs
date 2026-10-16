// Package notify posts short run summaries to a Slack Incoming Webhook.
//
// The webhook URL is a secret: it is never logged, and error text is scrubbed of it.
// Slack is best effort. Callers must never fail the job because a message was not sent.
package notify

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"
)

//go:embed messages.tmpl
var defaultTemplates string

// Message kinds, which are also the template names.
const (
	Success       = "success"
	AttemptFailed = "attempt_failed"
	FinalFailed   = "final_failed"
	// JobSuccess and JobFailed are used by the store price jobs (GOG, Epic, USD to INR).
	JobSuccess = "job_success"
	JobFailed  = "job_failed"
)

var kinds = []string{Success, AttemptFailed, FinalFailed, JobSuccess, JobFailed}

// Data is what the message templates can use.
type Data struct {
	SlotLabel   string // slot start in IST and UTC
	Attempt     int
	Total       int
	Updated     int
	Skipped     int
	Failed      int
	Retried     int
	CoveragePct string // one decimal, e.g. "99.6"
	Duration    string
	RunURL      string
	// Job and Detail are used by the job templates only (job_success, job_failed).
	Job    string // e.g. "GOG prices"
	Detail string // one short line with the run's numbers
}

// SlotLabel formats a slot start in IST and UTC.
func SlotLabel(slotStart time.Time) string {
	ist := time.FixedZone("IST", 5*3600+1800)
	u := slotStart.UTC()
	return fmt.Sprintf("%s IST / %s UTC, %s", slotStart.In(ist).Format("15:04"), u.Format("15:04"), u.Format("2 Jan"))
}

// FormatCoverage renders a 0..1 ratio as a percentage with one decimal.
func FormatCoverage(ratio float64) string { return fmt.Sprintf("%.1f", ratio*100) }

// RunURL builds the Actions run link from the standard GitHub variables.
func RunURL(getenv func(string) string) string {
	server, repo, id := getenv("GITHUB_SERVER_URL"), getenv("GITHUB_REPOSITORY"), getenv("GITHUB_RUN_ID")
	if server == "" || repo == "" || id == "" {
		return ""
	}
	return strings.TrimRight(server, "/") + "/" + repo + "/actions/runs/" + id
}

// Notifier sends messages. An empty URL disables it.
type Notifier struct {
	URL         string
	TemplateDir string
	Client      *http.Client
	Timeout     time.Duration // per attempt
	Retries     int           // extra attempts after the first
	Backoff     time.Duration
	Logf        func(msg string, err error) // optional warning sink; never receives the URL
}

// WebhookEnv is the shared Slack webhook variable. One channel serves every project in
// the repository, so this is the name to use.
const WebhookEnv = "SLACK_WEBHOOK_URL"

// LegacyWebhookEnv is the project-specific name. It is read only when WebhookEnv is empty.
const LegacyWebhookEnv = "GDR_SLACK_WEBHOOK_URL"

// webhookURL returns SLACK_WEBHOOK_URL, falling back to GDR_SLACK_WEBHOOK_URL when the
// first is empty or whitespace. The value is never logged.
func webhookURL(getenv func(string) string) string {
	if u := strings.TrimSpace(getenv(WebhookEnv)); u != "" {
		return u
	}
	return strings.TrimSpace(getenv(LegacyWebhookEnv))
}

// New returns a Notifier with production defaults (10s timeout, one retry after 2s)
// configured from SLACK_WEBHOOK_URL (or the legacy GDR_SLACK_WEBHOOK_URL) and
// GDR_NOTIFY_TEMPLATE_DIR.
func New(getenv func(string) string) Notifier {
	return Notifier{
		URL:         webhookURL(getenv),
		TemplateDir: strings.TrimSpace(getenv("GDR_NOTIFY_TEMPLATE_DIR")),
		Client:      &http.Client{Timeout: 15 * time.Second},
		Timeout:     10 * time.Second,
		Retries:     1,
		Backoff:     2 * time.Second,
	}
}

// Enabled reports whether a webhook URL is configured.
func (n Notifier) Enabled() bool { return n.URL != "" }

// Send renders the template for kind and posts it. It returns nil when disabled.
// Errors never contain the webhook URL.
func (n Notifier) Send(ctx context.Context, kind string, d Data) error {
	if !n.Enabled() {
		return nil
	}
	text, err := n.render(kind, d)
	if err != nil {
		return err
	}
	return n.post(ctx, text)
}

func (n Notifier) render(kind string, d Data) (string, error) {
	if n.TemplateDir != "" {
		if raw, err := os.ReadFile(filepath.Join(n.TemplateDir, kind+".tmpl")); err == nil {
			if out, err := execText(kind, string(raw), d, false); err == nil {
				return out, nil
			} else if n.Logf != nil {
				n.Logf("custom notification template ignored, using default", err)
			}
		}
	}
	return execText(kind, defaultTemplates, d, true)
}

func execText(name, src string, d Data, named bool) (string, error) {
	t, err := template.New("root").Option("missingkey=error").Parse(src)
	if err != nil {
		return "", fmt.Errorf("parse %s template: %w", name, err)
	}
	if named {
		t = t.Lookup(name)
		if t == nil {
			return "", fmt.Errorf("unknown message kind %q", name)
		}
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return "", fmt.Errorf("render %s template: %w", name, err)
	}
	out := strings.TrimSpace(buf.String())
	if out == "" {
		return "", fmt.Errorf("%s template rendered empty text", name)
	}
	return out, nil
}

func (n Notifier) post(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt <= n.Retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return n.scrub(ctx.Err())
			case <-time.After(n.Backoff):
			}
		}
		reqCtx, cancel := context.WithTimeout(ctx, n.Timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, n.URL, bytes.NewReader(body))
		if err != nil {
			cancel()
			return n.scrub(err)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := n.Client.Do(req)
		if err != nil {
			cancel()
			lastErr = n.scrub(err)
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		_ = resp.Body.Close()
		cancel()
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("slack responded with status %d", resp.StatusCode)
	}
	return lastErr
}

// scrub returns an error whose text no longer contains the webhook URL (or its parts).
func (n Notifier) scrub(err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ReplaceAll(err.Error(), n.URL, "[slack-webhook-url]")
	return errors.New(msg)
}
