package pricing

import (
	"context"
	"errors"
	"testing"
	"time"

	"gamedeal-radar/daily-check/internal/steam"
)

var now = time.Date(2026, 10, 10, 21, 30, 0, 0, time.UTC)

func res(current, base float64, pct int, deal bool) *steam.PriceResult {
	return &steam.PriceResult{Country: "in", Currency: "INR", CurrentPrice: current, BasePrice: base, DiscountPct: pct, IsDeal: deal, CheckedAt: now}
}

func TestDetermineEvent(t *testing.T) {
	prevDeal := &Snapshot{IsDeal: true, CurrentPrice: 300}
	prevFull := &Snapshot{IsDeal: false, CurrentPrice: 1000}

	tests := []struct {
		name     string
		prev     *Snapshot
		res      *steam.PriceResult
		wantType string
		wantOld  float64
		hasOld   bool
	}{
		{"first check, deal -> NEW_DEAL using base price", nil, res(300, 1000, 70, true), "NEW_DEAL", 1000, true},
		{"first check, no deal -> no event", nil, res(1000, 1000, 0, false), "", 0, false},
		{"enters sale -> NEW_DEAL using previous current price", prevFull, res(500, 1000, 50, true), "NEW_DEAL", 1000, true},
		{"sale ends -> DEAL_ENDED", prevDeal, res(1000, 1000, 0, false), "DEAL_ENDED", 300, true},
		{"deeper cut during sale -> PRICE_DROP", prevDeal, res(200, 1000, 80, true), "PRICE_DROP", 300, true},
		{"same price during sale -> no event", prevDeal, res(300, 1000, 70, true), "", 0, false},
		{"no deal before or after -> no event", prevFull, res(1000, 1000, 0, false), "", 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := DetermineEvent(tc.prev, tc.res, "g", "s", now)
			if tc.wantType == "" {
				if ev != nil {
					t.Fatalf("expected no event, got %s", ev.EventType)
				}
				return
			}
			if ev == nil || ev.EventType != tc.wantType {
				t.Fatalf("event = %+v, want %s", ev, tc.wantType)
			}
			if tc.hasOld && (ev.OldPrice == nil || *ev.OldPrice != tc.wantOld) {
				t.Fatalf("old price = %v, want %v", ev.OldPrice, tc.wantOld)
			}
		})
	}
}

// fakeStore is an in-memory Store for ProcessPriceResult.
type fakeStore struct {
	latest  *Snapshot
	saved   []*Snapshot
	events  []*Event
	saveErr error
}

func (f *fakeStore) LatestSnapshot(_ context.Context, _, _ string) (*Snapshot, error) {
	if f.latest == nil {
		return nil, ErrNoSnapshotFound
	}
	return f.latest, nil
}

func (f *fakeStore) SaveSnapshot(_ context.Context, s *Snapshot) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saved = append(f.saved, s)
	return nil
}

func (f *fakeStore) RecordEvent(_ context.Context, e *Event) error {
	f.events = append(f.events, e)
	return nil
}

func TestProcessPriceResultSavesThenRecordsEvent(t *testing.T) {
	st := &fakeStore{}
	snap, ev, err := ProcessPriceResult(context.Background(), st, "game-1", "steam-id", res(300, 1000, 70, true))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(st.saved) != 1 || snap == nil {
		t.Fatal("snapshot must be saved exactly once")
	}
	if ev == nil || ev.EventType != "NEW_DEAL" || len(st.events) != 1 {
		t.Fatalf("expected one NEW_DEAL event, got %+v (recorded %d)", ev, len(st.events))
	}
}

func TestProcessPriceResultSaveErrorIsReturned(t *testing.T) {
	st := &fakeStore{saveErr: errors.New("boom")}
	_, _, err := ProcessPriceResult(context.Background(), st, "g", "s", res(300, 1000, 70, true))
	if err == nil {
		t.Fatal("expected save error to be returned")
	}
	if len(st.events) != 0 {
		t.Fatal("no event may be recorded when the snapshot failed")
	}
}
