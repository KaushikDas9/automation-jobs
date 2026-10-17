package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"gamedeal-radar/daily-check/internal/epicfree"
)

// upsertEpicFreeSQL writes one free game. Every column is refreshed on conflict, and
// fetched_at is set to now. There is no DELETE anywhere in this file.
const upsertEpicFreeSQL = `
INSERT INTO epic_free_games (
    epic_id, title, image_url, store_url, regular_price, currency, starts_at, ends_at, fetched_at
) VALUES ($1, $2, $3, $4, $5::numeric, $6, $7, $8, NOW())
ON CONFLICT (epic_id) DO UPDATE SET
    title         = EXCLUDED.title,
    image_url     = EXCLUDED.image_url,
    store_url     = EXCLUDED.store_url,
    regular_price = EXCLUDED.regular_price,
    currency      = EXCLUDED.currency,
    starts_at     = EXCLUDED.starts_at,
    ends_at       = EXCLUDED.ends_at,
    fetched_at    = NOW();`

// epicFreeArgs maps one game to the upsert parameters, in column order. A missing image or
// price is passed as an untyped nil, which Postgres stores as NULL.
func epicFreeArgs(g epicfree.Game) []any {
	var image, price any
	if g.ImageURL != nil {
		image = *g.ImageURL
	}
	if g.RegularPrice != nil {
		price = *g.RegularPrice
	}
	return []any{
		g.EpicID, g.Title, image, g.StoreURL, price, g.Currency,
		g.StartsAt.UTC(), g.EndsAt.UTC(),
	}
}

// SaveEpicFreeGames upserts every game in one transaction. Either all rows are written or
// none is. An empty list is refused, so a feed with no free games never reaches the table.
func SaveEpicFreeGames(ctx context.Context, conn *pgx.Conn, games []epicfree.Game) error {
	if len(games) == 0 {
		return errors.New("refusing to write an empty list of epic free games")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	for _, g := range games {
		if _, err := tx.Exec(ctx, upsertEpicFreeSQL, epicFreeArgs(g)...); err != nil {
			return fmt.Errorf("upserting epic free game: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing epic free games: %w", err)
	}
	return nil
}
