package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// Statuses of a game_requests row.
const (
	RequestPending    = "pending"
	RequestProcessing = "processing"
	RequestDone       = "done"
	RequestFailed     = "failed"
	RequestNotFound   = "not_found"
)

const (
	// MaxRequestAttempts is how many times a request is claimed before it stays failed.
	MaxRequestAttempts = 3
	// RequestRetryAfter is how long a failed (or abandoned processing) request waits
	// before it is claimed again.
	RequestRetryAfter = time.Hour
)

// GameRequest is one claimed row of the game_requests queue.
type GameRequest struct {
	ID         string
	SteamAppID string
	Name       string
	GameID     string
	Attempts   int // including this claim
}

// claimSQL picks claimable rows. $1 is the max rows, $2 the retry age as seconds, $3 the
// attempt cap and $4 an optional list of request ids (NULL means any). A row is claimable
// when pending, when failed with attempts left and finished long enough ago, or when stuck
// in processing for that long (a run that died). SKIP LOCKED keeps two runs apart.
const claimSQL = `
SELECT id::text, steam_app_id, name, game_id::text, attempts
FROM game_requests
WHERE game_id IS NOT NULL
  AND (
        status = 'pending'
     OR (status = 'failed'     AND attempts < $3::int AND finished_at < now() - make_interval(secs => $2::float8))
     OR (status = 'processing' AND attempts < $3::int AND started_at  < now() - make_interval(secs => $2::float8))
  )
  AND ($4::uuid[] IS NULL OR id = ANY($4::uuid[]))
  -- A request that was re-queued is superseded by the newer row for the same game. Claiming both
  -- would break the one-open-request-per-game rule.
  AND NOT EXISTS (
        SELECT 1 FROM game_requests newer
        WHERE newer.steam_app_id = game_requests.steam_app_id
          AND newer.requested_at > game_requests.requested_at
  )
ORDER BY requested_at ASC
LIMIT $1::int`

const markProcessingSQL = `
UPDATE game_requests
SET status = 'processing', started_at = now(), finished_at = NULL, error = NULL,
    attempts = attempts + 1, updated_at = now()
WHERE id = ANY($1::uuid[])`

// ClaimGameRequests claims up to max requests in one short transaction. With dryRun it
// only reads them: no lock, no status change.
func ClaimGameRequests(ctx context.Context, conn *pgx.Conn, max int, ids []string, dryRun bool) ([]GameRequest, error) {
	var idArg any
	if len(ids) > 0 {
		idArg = ids
	}
	secs := int(RequestRetryAfter / time.Second)
	query := claimSQL
	if !dryRun {
		query += "\nFOR UPDATE SKIP LOCKED"
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("starting claim transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	rows, err := tx.Query(ctx, query, max, secs, MaxRequestAttempts, idArg)
	if err != nil {
		return nil, fmt.Errorf("selecting game requests: %w", err)
	}
	var out []GameRequest
	for rows.Next() {
		var r GameRequest
		if err := rows.Scan(&r.ID, &r.SteamAppID, &r.Name, &r.GameID, &r.Attempts); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scanning game request: %w", err)
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating game requests: %w", err)
	}

	if !dryRun && len(out) > 0 {
		claimed := make([]string, 0, len(out))
		for i := range out {
			claimed = append(claimed, out[i].ID)
			out[i].Attempts++
		}
		if _, err := tx.Exec(ctx, markProcessingSQL, claimed); err != nil {
			return nil, fmt.Errorf("marking game requests processing: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("committing claim: %w", err)
	}
	return out, nil
}

// FinishGameRequest records the end state of a claimed request. note may be empty (NULL).
// It only touches rows still in processing, so a row another run took over is left alone.
func FinishGameRequest(ctx context.Context, conn *pgx.Conn, id, status, note string) error {
	const q = `
		UPDATE game_requests
		SET status = $2, error = NULLIF($3, ''), finished_at = now(), updated_at = now()
		WHERE id = $1::uuid AND status = 'processing'`
	if _, err := conn.Exec(ctx, q, id, status, note); err != nil {
		return fmt.Errorf("finishing game request: %w", err)
	}
	return nil
}

// ActivateGame sets games.is_active. It uses only the UPDATE (is_active, updated_at) grant.
func ActivateGame(ctx context.Context, conn *pgx.Conn, gameID string) error {
	const q = `UPDATE games SET is_active = true, updated_at = now() WHERE id = $1::uuid`
	if _, err := conn.Exec(ctx, q, gameID); err != nil {
		return fmt.Errorf("activating game: %w", err)
	}
	return nil
}

// CountGameCheckedSince reports 1 when the game's Steam mapping was checked at or after
// since, else 0. It is the one-game form of CountCheckedSince and ignores is_active, so it
// works for a game that is not active yet.
func (r *Repo) CountGameCheckedSince(ctx context.Context, gameID string, since time.Time) (int, error) {
	const q = `
		SELECT COUNT(*) FROM game_store_mappings
		WHERE game_id = $1::uuid AND store_id = $2 AND last_checked_at >= $3`
	var n int
	if err := r.Conn.QueryRow(ctx, q, gameID, r.SteamStoreID, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting checked game: %w", err)
	}
	if n > 1 {
		n = 1
	}
	return n, nil
}
