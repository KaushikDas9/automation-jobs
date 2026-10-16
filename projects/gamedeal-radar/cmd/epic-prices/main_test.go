package main

import (
	"strings"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"GDR_DATABASE_URL", "ITAD_API_KEY", "DRY_RUN", "LIMIT", "RETRY_WAIT_1",
		"RETRY_WAIT_2", "GDR_HEALTHCHECK_URL", "GDR_HEALTHCHECK_FINAL_URL", "GITHUB_EVENT_NAME", "FINAL_ATTEMPT"} {
		t.Setenv(k, "")
	}
}

func TestLoadConfigRequiresITADKeyWithoutEchoingIt(t *testing.T) {
	clearEnv(t)
	t.Setenv("GDR_DATABASE_URL", "postgres://u:p@localhost/db")
	_, err := loadConfig()
	if err == nil || !strings.Contains(err.Error(), "ITAD_API_KEY") {
		t.Fatalf("missing key must name ITAD_API_KEY: %v", err)
	}
}

func TestLoadConfigDefaultsAndManual(t *testing.T) {
	clearEnv(t)
	t.Setenv("GDR_DATABASE_URL", "postgres://u:p@localhost/db")
	t.Setenv("ITAD_API_KEY", "not-a-real-key")
	t.Setenv("GITHUB_EVENT_NAME", "workflow_dispatch")
	t.Setenv("DRY_RUN", "1")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.retryWait1 != time.Minute || cfg.retryWait2 != 2*time.Minute || !cfg.manual || !cfg.dryRun {
		t.Fatalf("cfg = %+v", cfg)
	}
}
