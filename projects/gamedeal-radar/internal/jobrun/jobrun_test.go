package jobrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLimitAndBoolEnv(t *testing.T) {
	t.Setenv("LIMIT", "")
	if n, err := LimitEnv(); err != nil || n != 0 {
		t.Fatalf("unset LIMIT = %d, %v", n, err)
	}
	t.Setenv("LIMIT", "10")
	if n, err := LimitEnv(); err != nil || n != 10 {
		t.Fatalf("LIMIT=10 = %d, %v", n, err)
	}
	t.Setenv("LIMIT", "-1")
	if _, err := LimitEnv(); err == nil {
		t.Fatal("negative LIMIT must fail")
	}
	for v, want := range map[string]bool{"1": true, "true": true, "YES": true, "0": false, "": false} {
		t.Setenv("DRY_RUN", v)
		if BoolEnv("DRY_RUN") != want {
			t.Errorf("BoolEnv(%q) != %v", v, want)
		}
	}
}

func TestDurationEnvBounds(t *testing.T) {
	t.Setenv("RETRY_WAIT_1", "")
	if d, err := DurationEnv("RETRY_WAIT_1", time.Minute); err != nil || d != time.Minute {
		t.Fatalf("default = %v %v", d, err)
	}
	t.Setenv("RETRY_WAIT_1", "0")
	if d, err := DurationEnv("RETRY_WAIT_1", time.Minute); err != nil || d != 0 {
		t.Fatalf("zero = %v %v", d, err)
	}
	t.Setenv("RETRY_WAIT_1", "2h")
	if _, err := DurationEnv("RETRY_WAIT_1", time.Minute); err == nil || !strings.Contains(err.Error(), "RETRY_WAIT_1") {
		t.Fatalf("2h must fail naming the variable: %v", err)
	}
}

func TestHTTPURLEnvNamesVariableNotValue(t *testing.T) {
	t.Setenv("GDR_HEALTHCHECK_URL", "ftp://secret-host/abc")
	_, err := HTTPURLEnv("GDR_HEALTHCHECK_URL")
	if err == nil || strings.Contains(err.Error(), "secret-host") {
		t.Fatalf("error must name the variable and not echo the value: %v", err)
	}
	t.Setenv("GDR_HEALTHCHECK_URL", "https://hc-ping.example/uuid")
	if u, err := HTTPURLEnv("GDR_HEALTHCHECK_URL"); err != nil || u == "" {
		t.Fatalf("valid url rejected: %v", err)
	}
}

func TestWriteOutputsFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out")
	r := Report{Code: 1, Final: true, Blocked: true, Coverage: 0.85, Failed: 3, Total: 100}
	if err := writeOutputs(path, r); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{"complete=false", "final_attempt=true", "blocked=true", "coverage_pct=85.0", "failed_games=3", "total_games=100"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("outputs missing %q in %q", want, raw)
		}
	}
	if err := writeOutputs("", r); err != nil {
		t.Fatal("empty path must be a no-op")
	}
}

func TestRequiredEnv(t *testing.T) {
	t.Setenv("ITAD_API_KEY", "   ")
	if _, err := RequiredEnv("ITAD_API_KEY"); err == nil || !strings.Contains(err.Error(), "ITAD_API_KEY") {
		t.Fatalf("blank required value must fail: %v", err)
	}
}
