package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"gamedeal-radar/daily-check/internal/run"
)

// Repo implements run.Repo on a pgx connection for one Steam store id.
type Repo struct {
	PgStore
	Conn         *pgx.Conn
	SteamStoreID string
}

// NewRepo wires a Repo around an open connection.
func NewRepo(conn *pgx.Conn, steamStoreID string) *Repo {
	return &Repo{PgStore: PgStore{Conn: conn}, Conn: conn, SteamStoreID: steamStoreID}
}

// ListActiveSteamGames returns every active game in priority order. Games without a
// Steam mapping come back with an empty ExternalAppID. When a game has several Steam
// mappings, the one checked most recently is used (all of them are updated together).
func (r *Repo) ListActiveSteamGames(ctx context.Context) ([]run.Game, error) {
	const q = `
		SELECT g.id::text, g.slug, COALESCE(m.external_app_id, ''), m.last_checked_at
		FROM games g
		LEFT JOIN LATERAL (
			SELECT external_app_id, last_checked_at
			FROM game_store_mappings
			WHERE game_id = g.id AND store_id = $1
			ORDER BY last_checked_at DESC NULLS LAST
			LIMIT 1
		) m ON true
		WHERE g.is_active = true
		ORDER BY g.priority DESC, g.name ASC;`

	rows, err := r.Conn.Query(ctx, q, r.SteamStoreID)
	if err != nil {
		return nil, fmt.Errorf("listing active games: %w", err)
	}
	defer rows.Close()

	var out []run.Game
	for rows.Next() {
		var g run.Game
		var lastChecked *time.Time
		if err := rows.Scan(&g.ID, &g.Slug, &g.ExternalAppID, &lastChecked); err != nil {
			return nil, fmt.Errorf("scanning active game: %w", err)
		}
		g.LastCheckedAt = lastChecked
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating active games: %w", err)
	}
	return out, nil
}

// CountCheckedSince counts active games with a Steam mapping whose last_checked_at is
// at or after since.
func (r *Repo) CountCheckedSince(ctx context.Context, since time.Time) (int, error) {
	const q = `
		SELECT COUNT(*)
		FROM games g
		JOIN LATERAL (
			SELECT last_checked_at
			FROM game_store_mappings
			WHERE game_id = g.id AND store_id = $1
			ORDER BY last_checked_at DESC NULLS LAST
			LIMIT 1
		) m ON true
		WHERE g.is_active = true AND m.last_checked_at >= $2;`
	var n int
	if err := r.Conn.QueryRow(ctx, q, r.SteamStoreID, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting checked games: %w", err)
	}
	return n, nil
}

// MarkChecked sets last_checked_at on the game's Steam mappings. It uses only the
// UPDATE grant on last_checked_at and updated_at.
func (r *Repo) MarkChecked(ctx context.Context, gameID string, at time.Time) error {
	const q = `
		UPDATE game_store_mappings
		SET last_checked_at = $1, updated_at = NOW()
		WHERE game_id = $2 AND store_id = $3;`
	if _, err := r.Conn.Exec(ctx, q, at, gameID, r.SteamStoreID); err != nil {
		return fmt.Errorf("marking game checked: %w", err)
	}
	return nil
}
