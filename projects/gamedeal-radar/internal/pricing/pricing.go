// Package pricing ports the server's pricing.ProcessPriceResult: it saves a price
// snapshot and derives at most one price event (NEW_DEAL, PRICE_DROP, DEAL_ENDED).
package pricing

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"gamedeal-radar/daily-check/internal/steam"
)

// ErrNoSnapshotFound is returned by a Store when a game has no prior snapshot.
var ErrNoSnapshotFound = errors.New("no price snapshot found")

// Snapshot is the stored state used for comparison and as the saved row.
type Snapshot struct {
	GameID       string
	StoreID      string
	Country      string
	Currency     string
	BasePrice    float64
	CurrentPrice float64
	DiscountPct  int
	IsDeal       bool
	CheckedAt    time.Time
	RawPayload   []byte
}

// Event is a price transition worth recording.
type Event struct {
	GameID      string
	StoreID     string
	EventType   string
	OldPrice    *float64
	NewPrice    float64
	DiscountPct int
	CreatedAt   time.Time
}

// Store is the persistence port. Implementations return ErrNoSnapshotFound from
// LatestSnapshot when no prior row exists.
type Store interface {
	LatestSnapshot(ctx context.Context, gameID, storeID string) (*Snapshot, error)
	SaveSnapshot(ctx context.Context, s *Snapshot) error
	RecordEvent(ctx context.Context, e *Event) error
}

// DetermineEvent is the pure state-transition rule from the server's service.go.
// prev is nil when the game has no previous snapshot.
func DetermineEvent(prev *Snapshot, res *steam.PriceResult, gameID, storeID string, now time.Time) *Event {
	if prev == nil {
		if res.IsDeal {
			base := res.BasePrice
			return &Event{
				GameID: gameID, StoreID: storeID, EventType: "NEW_DEAL",
				OldPrice: &base, NewPrice: res.CurrentPrice, DiscountPct: res.DiscountPct, CreatedAt: now,
			}
		}
		return nil
	}

	switch {
	case res.IsDeal && !prev.IsDeal:
		old := prev.CurrentPrice
		return &Event{
			GameID: gameID, StoreID: storeID, EventType: "NEW_DEAL",
			OldPrice: &old, NewPrice: res.CurrentPrice, DiscountPct: res.DiscountPct, CreatedAt: now,
		}
	case !res.IsDeal && prev.IsDeal:
		old := prev.CurrentPrice
		return &Event{
			GameID: gameID, StoreID: storeID, EventType: "DEAL_ENDED",
			OldPrice: &old, NewPrice: res.CurrentPrice, DiscountPct: 0, CreatedAt: now,
		}
	case res.IsDeal && prev.IsDeal && res.CurrentPrice < prev.CurrentPrice:
		old := prev.CurrentPrice
		return &Event{
			GameID: gameID, StoreID: storeID, EventType: "PRICE_DROP",
			OldPrice: &old, NewPrice: res.CurrentPrice, DiscountPct: res.DiscountPct, CreatedAt: now,
		}
	}
	return nil
}

// ProcessPriceResult saves the snapshot first (an error here is returned), then
// records any derived event. A failed event insert is logged, not returned,
// matching the server's behaviour.
func ProcessPriceResult(ctx context.Context, st Store, gameID, storeID string, res *steam.PriceResult) (*Snapshot, *Event, error) {
	prev, err := st.LatestSnapshot(ctx, gameID, storeID)
	if err != nil && !errors.Is(err, ErrNoSnapshotFound) {
		return nil, nil, fmt.Errorf("fetching previous snapshot: %w", err)
	}
	if errors.Is(err, ErrNoSnapshotFound) {
		prev = nil
	}

	snap := &Snapshot{
		GameID:       gameID,
		StoreID:      storeID,
		Country:      res.Country,
		Currency:     res.Currency,
		BasePrice:    res.BasePrice,
		CurrentPrice: res.CurrentPrice,
		DiscountPct:  res.DiscountPct,
		IsDeal:       res.IsDeal,
		CheckedAt:    res.CheckedAt,
		RawPayload:   res.RawPayload,
	}
	if snap.CheckedAt.IsZero() {
		snap.CheckedAt = time.Now().UTC()
	}
	if err := st.SaveSnapshot(ctx, snap); err != nil {
		return nil, nil, fmt.Errorf("saving price snapshot: %w", err)
	}

	event := DetermineEvent(prev, res, gameID, storeID, time.Now().UTC())
	if event != nil {
		if err := st.RecordEvent(ctx, event); err != nil {
			slog.Error("failed to record price event", "game_id", gameID, "event_type", event.EventType, "error", err)
			return snap, event, nil
		}
		slog.Info("price event detected", "game_id", gameID, "type", event.EventType,
			"new_price", event.NewPrice, "discount_pct", event.DiscountPct)
	}
	return snap, event, nil
}
