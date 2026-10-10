package db

import (
	"errors"
	"strings"
	"testing"
)

func TestRedactRemovesDSNAndPassword(t *testing.T) {
	dsn := "postgres://gdr_daily_job:example-password-value@db.example.invalid:5432/app?sslmode=require"
	err := errors.New("failed to parse " + dsn + " near example-password-value")
	got := Redact(err, dsn)
	if strings.Contains(got, "example-password-value") || strings.Contains(got, "postgres://") {
		t.Fatalf("secret leaked: %q", got)
	}
}

func TestRedactNilIsEmpty(t *testing.T) {
	if Redact(nil, "x") != "" {
		t.Fatal("nil error should redact to empty string")
	}
}
