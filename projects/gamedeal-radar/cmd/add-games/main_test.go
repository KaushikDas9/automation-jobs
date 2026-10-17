package main

import (
	"errors"
	"strings"
	"testing"

	"gamedeal-radar/daily-check/internal/addgames"
)

func TestLoadConfigDefaultsAndBounds(t *testing.T) {
	t.Setenv("GDR_DATABASE_URL", "postgres://u:pw@h/db")
	t.Setenv("MAX_GAMES", "")
	t.Setenv("GAME_REQUEST_IDS", "")
	t.Setenv("GDR_HEALTHCHECK_URL", "")
	c, err := loadConfig()
	if err != nil || c.maxGames != 10 || len(c.requestID) != 0 {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	for _, bad := range []string{"0", "-1", "abc", "51"} {
		t.Setenv("MAX_GAMES", bad)
		if _, err := loadConfig(); err == nil {
			t.Errorf("MAX_GAMES=%q must fail", bad)
		}
	}
	t.Setenv("MAX_GAMES", "3")
	if c, err := loadConfig(); err != nil || c.maxGames != 3 {
		t.Fatalf("MAX_GAMES=3: %+v %v", c, err)
	}
	t.Setenv("GDR_DATABASE_URL", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("missing database url must fail")
	}
}

func TestParseIDs(t *testing.T) {
	a := "0b0e6b1e-9d57-4d4e-8a39-0f3a1a2b3c4d"
	b := "1b0e6b1e-9d57-4d4e-8a39-0f3a1a2b3c4d"
	ids, err := parseIDs(a + " , " + b + ",")
	if err != nil || len(ids) != 2 || ids[1] != b {
		t.Fatalf("ids = %v %v", ids, err)
	}
	if _, err := parseIDs("not-a-uuid"); err == nil || strings.Contains(err.Error(), "not-a-uuid") {
		t.Fatalf("bad id must fail without echoing it: %v", err)
	}
	if ids, err := parseIDs(""); err != nil || len(ids) != 0 {
		t.Fatalf("empty = %v %v", ids, err)
	}
}

func TestDecideExitCodes(t *testing.T) {
	cases := []struct {
		name string
		sum  addgames.Summary
		err  error
		code int
	}{
		{"empty queue", addgames.Summary{}, nil, 0},
		{"all done", addgames.Summary{Claimed: 2, Done: 2}, nil, 0},
		{"done and not found", addgames.Summary{Claimed: 2, Done: 1, NotFound: 1}, nil, 0},
		{"only not found", addgames.Summary{Claimed: 1, NotFound: 1}, nil, 0},
		{"some failed", addgames.Summary{Claimed: 3, Done: 2, Failed: 1}, nil, 0},
		{"all failed", addgames.Summary{Claimed: 2, Failed: 2}, nil, 1},
		{"db unreachable", addgames.Summary{}, errors.New("database unreachable"), 1},
	}
	for _, c := range cases {
		if code, _ := decide(c.sum, c.err); code != c.code {
			t.Errorf("%s: code %d, want %d", c.name, code, c.code)
		}
	}
}
