package main

import "testing"

func TestLoadConfigForceAndDryRunFlags(t *testing.T) {
	t.Setenv("GDR_DATABASE_URL", "postgres://example.invalid/db")
	cases := []struct {
		value     string
		wantForce bool
	}{
		{"", false},
		{"0", false},
		{"no", false},
		{"1", true},
		{"true", true},
		{" YES ", true},
	}
	for _, tc := range cases {
		t.Run("FORCE="+tc.value, func(t *testing.T) {
			t.Setenv("FORCE", tc.value)
			t.Setenv("DRY_RUN", "")
			cfg, err := loadConfig()
			if err != nil {
				t.Fatal(err)
			}
			if cfg.force != tc.wantForce {
				t.Fatalf("force = %t, want %t", cfg.force, tc.wantForce)
			}
			if cfg.dryRun {
				t.Fatal("dryRun should be false when DRY_RUN is empty")
			}
		})
	}
}
