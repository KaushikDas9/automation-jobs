package steam

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsTransientClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rate limited", ErrRateLimited, true},
		{"network error", transient(errors.New("dial tcp: timeout")), true},
		{"wrapped network error", fmt.Errorf("outer: %w", transient(errors.New("reset"))), true},
		{"HTTP 429", &StatusError{Code: 429}, true},
		{"HTTP 500", &StatusError{Code: 500}, true},
		{"HTTP 503", &StatusError{Code: 503}, true},
		{"HTTP 404 is permanent", &StatusError{Code: 404}, false},
		{"HTTP 400 is permanent", &StatusError{Code: 400}, false},
		{"not found is permanent", ErrNotFound, false},
		{"price unavailable is permanent", ErrPriceUnavailable, false},
		{"context cancelled is not retried", context.Canceled, false},
		{"parse error is permanent", errors.New("unmarshaling steam response: invalid character"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsTransient(tc.err); got != tc.want {
				t.Fatalf("IsTransient(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
