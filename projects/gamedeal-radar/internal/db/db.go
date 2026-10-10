// Package db holds the Postgres queries ported from the server's games and pricing
// repositories. Only the statements this job needs are present.
package db

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"gamedeal-radar/daily-check/internal/pricing"
)

// Game is an active catalog title plus its Steam app id (empty when unmapped).
type Game struct {
	ID            string
	Slug          string
	ExternalAppID string
}

// Connect opens a single connection. Any error text is scrubbed of the DSN and its
// password so the secret can never reach logs or the GitHub Actions log.
func Connect(ctx context.Context, dsn string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to database: %s", Redact(err, dsn))
	}
	return conn, nil
}

// Redact returns err's message with the DSN and any password it contains removed.
func Redact(err error, dsn string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	if dsn != "" {
		msg = strings.ReplaceAll(msg, dsn, "[redacted-dsn]")
	}
	if u, perr := url.Parse(dsn); perr == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			msg = strings.ReplaceAll(msg, pw, "[redacted]")
		}
	}
	return msg
}

// SteamStoreID returns the id of the stores row with code 'steam'.
func SteamStoreID(ctx context.Context, conn *pgx.Conn) (string, error) {
	var id string
	err := conn.QueryRow(ctx, `SELECT id FROM stores WHERE code = 'steam' LIMIT 1;`).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("fetching steam store id: %w", err)
	}
	return id, nil
}

// ListActiveSteamGames returns active games, highest priority first, with one Steam
// app id per game (any mapping, as the server's resolver does). limit <= 0 means no limit.
func ListActiveSteamGames(ctx context.Context, conn *pgx.Conn, steamStoreID string, limit int) ([]Game, error) {
	const q = `
		SELECT g.id::text, g.slug, COALESCE(m.external_app_id, '')
		FROM games g
		LEFT JOIN LATERAL (
			SELECT external_app_id
			FROM game_store_mappings
			WHERE game_id = g.id AND store_id = $1
			LIMIT 1
		) m ON true
		WHERE g.is_active = true
		ORDER BY g.priority DESC, g.name ASC
		LIMIT NULLIF($2::int, 0);`

	rows, err := conn.Query(ctx, q, steamStoreID, limit)
	if err != nil {
		return nil, fmt.Errorf("listing active games: %w", err)
	}
	defer rows.Close()

	var out []Game
	for rows.Next() {
		var g Game
		if err := rows.Scan(&g.ID, &g.Slug, &g.ExternalAppID); err != nil {
			return nil, fmt.Errorf("scanning active game: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating active games: %w", err)
	}
	return out, nil
}

// PgStore implements pricing.Store on a pgx connection.
type PgStore struct {
	Conn *pgx.Conn
}

// LatestSnapshot returns the newest snapshot for the game/store, or ErrNoSnapshotFound.
func (s PgStore) LatestSnapshot(ctx context.Context, gameID, storeID string) (*pricing.Snapshot, error) {
	const q = `
		SELECT game_id::text, store_id::text, country, currency, base_price, current_price,
		       discount_pct, is_deal, checked_at
		FROM price_snapshots
		WHERE game_id = $1 AND store_id = $2
		ORDER BY checked_at DESC
		LIMIT 1;`
	var snap pricing.Snapshot
	err := s.Conn.QueryRow(ctx, q, gameID, storeID).Scan(
		&snap.GameID, &snap.StoreID, &snap.Country, &snap.Currency,
		&snap.BasePrice, &snap.CurrentPrice, &snap.DiscountPct, &snap.IsDeal, &snap.CheckedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, pricing.ErrNoSnapshotFound
		}
		return nil, fmt.Errorf("querying latest price snapshot: %w", err)
	}
	return &snap, nil
}

// SaveSnapshot inserts the snapshot, then best-effort updates game_store_mappings and
// upserts game_current_price. Those two follow-up writes ignore errors, as the server does.
func (s PgStore) SaveSnapshot(ctx context.Context, snap *pricing.Snapshot) error {
	const insert = `
		INSERT INTO price_snapshots (
			game_id, store_id, country, currency,
			base_price, current_price, discount_pct, is_deal,
			checked_at, raw_payload
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);`
	if _, err := s.Conn.Exec(ctx, insert,
		snap.GameID, snap.StoreID, snap.Country, snap.Currency,
		snap.BasePrice, snap.CurrentPrice, snap.DiscountPct, snap.IsDeal,
		snap.CheckedAt, snap.RawPayload,
	); err != nil {
		return fmt.Errorf("inserting price snapshot: %w", err)
	}

	const mapping = `
		UPDATE game_store_mappings
		SET last_checked_at = $1, updated_at = NOW()
		WHERE game_id = $2 AND store_id = $3;`
	if _, err := s.Conn.Exec(ctx, mapping, snap.CheckedAt, snap.GameID, snap.StoreID); err != nil {
		slog.Warn("could not update game_store_mappings.last_checked_at", "game_id", snap.GameID, "error", err)
	}

	const current = `
		INSERT INTO game_current_price (
			game_id, base_price, current_price, discount_pct, is_deal, checked_at
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (game_id) DO UPDATE SET
			base_price = EXCLUDED.base_price,
			current_price = EXCLUDED.current_price,
			discount_pct = EXCLUDED.discount_pct,
			is_deal = EXCLUDED.is_deal,
			checked_at = EXCLUDED.checked_at;`
	if _, err := s.Conn.Exec(ctx, current,
		snap.GameID, snap.BasePrice, snap.CurrentPrice, snap.DiscountPct, snap.IsDeal, snap.CheckedAt,
	); err != nil {
		slog.Warn("could not upsert game_current_price", "game_id", snap.GameID, "error", err)
	}
	return nil
}

// RecordEvent inserts a price_events row.
func (s PgStore) RecordEvent(ctx context.Context, e *pricing.Event) error {
	const q = `
		INSERT INTO price_events (game_id, store_id, event_type, old_price, new_price, discount_pct, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7);`
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if _, err := s.Conn.Exec(ctx, q, e.GameID, e.StoreID, e.EventType, e.OldPrice, e.NewPrice, e.DiscountPct, e.CreatedAt); err != nil {
		return fmt.Errorf("inserting price event (%s): %w", e.EventType, err)
	}
	return nil
}
