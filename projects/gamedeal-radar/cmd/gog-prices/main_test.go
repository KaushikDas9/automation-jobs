package main

import (
	"strings"
	"testing"
	"time"
)

func TestLoadConfigNeedsDatabaseOnly(t *testing.T) {
	for _, k := range []string{"GDR_DATABASE_URL", "DRY_RUN", "LIMIT", "RETRY_WAIT_1", "RETRY_WAIT_2", "GITHUB_EVENT_NAME"} {
		t.Setenv(k, "")
	}
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "GDR_DATABASE_URL") {
		t.Fatalf("missing DSN must fail: %v", err)
	}
	t.Setenv("GDR_DATABASE_URL", "postgres://u:p@localhost/db")
	t.Setenv("RETRY_WAIT_1", "0")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.retryWait1 != 0 || cfg.retryWait2 != 2*time.Minute {
		t.Fatalf("cfg = %+v", cfg)
	}
}
