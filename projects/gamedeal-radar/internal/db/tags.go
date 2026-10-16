package db

import (
	"context"
	"encoding/json"
	"fmt"

	"gamedeal-radar/daily-check/internal/steamtags"
)

// listGamesForTagsSQL picks active games with a game_details row and a numeric Steam app id.
// $1 is the Steam store id and $2 the limit. A limit of 0 becomes NULL, which means no limit.
const listGamesForTagsSQL = `
	SELECT g.id::text, m.external_app_id
	FROM games g
	JOIN game_details d ON d.game_id = g.id
	JOIN LATERAL (
		SELECT gm.external_app_id
		FROM game_store_mappings gm
		WHERE gm.game_id = g.id AND gm.store_id = $1
		  AND gm.external_app_id ~ '^[0-9]+$'
		ORDER BY gm.last_checked_at DESC NULLS LAST
		LIMIT 1
	) m ON true
	WHERE g.is_active = true
	ORDER BY g.priority DESC, g.name ASC
	LIMIT NULLIF($2::int, 0);`

// updateTagsSQL writes the tags column and nothing else.
const updateTagsSQL = `UPDATE game_details SET tags = $2::jsonb WHERE game_id = $1;`

// ListGamesForTags returns active games that have a game_details row and a numeric Steam
// app id, in priority order. limit 0 means no limit. Nothing is written.
func (r *Repo) ListGamesForTags(ctx context.Context, limit int) ([]GameRef, error) {
	return listGamesForTags(ctx, r.Conn, r.SteamStoreID, limit)
}

// SaveTags replaces the tags of one game. An empty list is a no-op, so a game that Steam
// returned no tags for keeps the tags it already has. Only game_details.tags is written.
func (r *Repo) SaveTags(ctx context.Context, gameID string, tags []steamtags.Tag) error {
	return saveTags(ctx, r.Conn, gameID, tags)
}

func listGamesForTags(ctx context.Context, db execer, steamStoreID string, limit int) ([]GameRef, error) {
	rows, err := db.Query(ctx, listGamesForTagsSQL, steamStoreID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing games for tags: %w", err)
	}
	defer rows.Close()

	var out []GameRef
	for rows.Next() {
		var g GameRef
		if err := rows.Scan(&g.GameID, &g.ExternalAppID); err != nil {
			return nil, fmt.Errorf("scanning game for tags: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating games for tags: %w", err)
	}
	return out, nil
}

func saveTags(ctx context.Context, db execer, gameID string, tags []steamtags.Tag) error {
	if len(tags) == 0 {
		return nil
	}
	raw, err := encodeTags(tags)
	if err != nil {
		return err
	}
	if _, err := db.Exec(ctx, updateTagsSQL, gameID, raw); err != nil {
		return fmt.Errorf("updating game tags: %w", err)
	}
	return nil
}

// encodeTags returns the JSON array stored in game_details.tags, for example
// [{"id":"1695","description":"Open World"}].
func encodeTags(tags []steamtags.Tag) (string, error) {
	if tags == nil {
		tags = []steamtags.Tag{}
	}
	raw, err := json.Marshal(tags)
	if err != nil {
		return "", fmt.Errorf("encoding tags: %w", err)
	}
	return string(raw), nil
}
