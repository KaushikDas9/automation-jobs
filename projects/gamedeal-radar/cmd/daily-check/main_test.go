package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gamedeal-radar/daily-check/internal/slot"
)

func readOutputs(t *testing.T, path string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("malformed output line %q", line)
		}
		out[k] = v
	}
	return out
}

func TestWriteOutputsFinalFlagAndSlotID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "github_output")
	st := &status{slotID: slot.ID(time.Date(2026, 10, 10, 18, 30, 0, 0, time.UTC)), attempt: 6, final: true, coverage: 0.85, failed: 40, total: 500}
	if err := writeOutputs(path, st); err != nil {
		t.Fatal(err)
	}
	out := readOutputs(t, path)
	if out["final_attempt"] != "true" {
		t.Fatalf("final_attempt = %q", out["final_attempt"])
	}
	if out["slot_id"] != "2026-10-10 18:30Z" {
		t.Fatalf("slot_id = %q", out["slot_id"])
	}
	if out["slot_complete"] != "false" || out["coverage_pct"] != "85.0" || out["failed_games"] != "40" {
		t.Fatalf("outputs = %v", out)
	}
}

func TestWriteOutputsDryRunNeverMarksComplete(t *testing.T) {
	path := filepath.Join(t.TempDir(), "github_output")
	st := &status{slotID: "2026-10-10 06:30Z", attempt: 1, dryRun: true, complete: true}
	if err := writeOutputs(path, st); err != nil {
		t.Fatal(err)
	}
	out := readOutputs(t, path)
	if out["slot_complete"] != "false" || out["final_attempt"] != "false" {
		t.Fatalf("dry run must not set slot_complete or final_attempt: %v", out)
	}
}

func TestWriteOutputsNoPathIsNoop(t *testing.T) {
	if err := writeOutputs("", &status{}); err != nil {
		t.Fatalf("empty path must be a no-op, got %v", err)
	}
}

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"GDR_DATABASE_URL", "DRY_RUN", "LIMIT", "SLOT_START", "ATTEMPT",
		"RETRY_WAIT_1", "RETRY_WAIT_2", "GDR_HEALTHCHECK_URL", "GDR_HEALTHCHECK_FINAL_URL", "GITHUB_EVENT_NAME"} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	setEnv(t, map[string]string{"GDR_DATABASE_URL": "postgres://u:p@h/db"})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.retryWait1 != 5*time.Minute || cfg.retryWait2 != 10*time.Minute {
		t.Fatalf("default waits = %v, %v", cfg.retryWait1, cfg.retryWait2)
	}
	if cfg.manual || cfg.dryRun || cfg.healthURL != "" || cfg.finalHealthURL != "" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadConfigZeroWaitsAndManualEvent(t *testing.T) {
	setEnv(t, map[string]string{
		"GDR_DATABASE_URL":  "postgres://u:p@h/db",
		"RETRY_WAIT_1":      "0",
		"RETRY_WAIT_2":      "0",
		"GITHUB_EVENT_NAME": "workflow_dispatch",
		"ATTEMPT":           "3",
		"SLOT_START":        "2026-10-10T18:30:00Z",
	})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.retryWait1 != 0 || cfg.retryWait2 != 0 || !cfg.manual || cfg.attempt != 3 || cfg.slotOverride == nil {
		t.Fatalf("config = %+v", cfg)
	}
}

func TestLoadConfigRejectsBadValuesWithoutEchoingURLs(t *testing.T) {
	secretURL := "https://hc-ping.example.invalid/SECRET-UUID"
	cases := []map[string]string{
		{"GDR_DATABASE_URL": "postgres://u:p@h/db", "ATTEMPT": "7"},
		{"GDR_DATABASE_URL": "postgres://u:p@h/db", "SLOT_START": "yesterday"},
		{"GDR_DATABASE_URL": "postgres://u:p@h/db", "RETRY_WAIT_1": "5 minutes"},
		{"GDR_DATABASE_URL": "postgres://u:p@h/db", "GDR_HEALTHCHECK_URL": "not a url " + secretURL},
	}
	for _, kv := range cases {
		setEnv(t, kv)
		_, err := loadConfig()
		if err == nil {
			t.Fatalf("expected an error for %v", kv)
		}
		if strings.Contains(err.Error(), "SECRET-UUID") {
			t.Fatalf("error leaked a healthcheck URL: %q", err.Error())
		}
	}
}

func TestLoadConfigMissingDatabaseURL(t *testing.T) {
	setEnv(t, map[string]string{})
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected an error when GDR_DATABASE_URL is unset")
	}
}
