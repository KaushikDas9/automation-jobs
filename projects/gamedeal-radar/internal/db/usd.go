package db

import (
	"context"
	"fmt"
	"time"

	"gamedeal-radar/daily-check/internal/usd"
)

// ListMissingUSD returns active games with a Steam mapping that have no USD price checked
// at or after since, in priority order. It backs the resume behaviour of the USD pass.
func (r *Repo) ListMissingUSD(ctx context.Context, since time.Time) ([]usd.Game, error) {
	const q = `
		SELECT g.id::text, g.slug, m.external_app_id
		FROM games g
		JOIN LATERAL (
			SELECT external_app_id
			FROM game_store_mappings
			WHERE game_id = g.id AND store_id = $1 AND external_app_id <> ''
			ORDER BY last_checked_at DESC NULLS LAST
			LIMIT 1
		) m ON true
		LEFT JOIN game_current_price_usd u ON u.game_id = g.id
		WHERE g.is_active = true AND (u.game_id IS NULL OR u.checked_at < $2)
		ORDER BY g.priority DESC, g.name ASC;`

	rows, err := r.Conn.Query(ctx, q, r.SteamStoreID, since)
	if err != nil {
		return nil, fmt.Errorf("listing games missing a usd price: %w", err)
	}
	defer rows.Close()

	var out []usd.Game
	for rows.Next() {
		var g usd.Game
		if err := rows.Scan(&g.ID, &g.Slug, &g.ExternalAppID); err != nil {
			return nil, fmt.Errorf("scanning game missing a usd price: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating games missing a usd price: %w", err)
	}
	return out, nil
}
