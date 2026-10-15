package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"gamedeal-radar/daily-check/internal/steam"
)

// execer is the part of *pgx.Conn, pgx.Tx and pgxpool.Pool that the details queries use.
// Begin on a pgx.Tx opens a savepoint, so the writers also work inside a test transaction.
type execer interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// GameRef identifies a game and the Steam app id to fetch its details with.
type GameRef struct {
	GameID        string
	ExternalAppID string
}

// UpsertGameDetails writes the Steam details row for a game and stamps fetched_at.
// rev may be nil, in which case the review columns are left NULL.
func (r *Repo) UpsertGameDetails(ctx context.Context, gameID string, d *steam.Details, rev *steam.Reviews) error {
	return upsertGameDetails(ctx, r.Conn, gameID, d, rev)
}

// SaveUSDPrice records one USD price in game_usd_snapshots and replaces the latest USD
// price in game_current_price_usd, in one transaction. Unavailable prices write nothing.
func (r *Repo) SaveUSDPrice(ctx context.Context, gameID string, p steam.BatchPrice, checkedAt time.Time) error {
	return saveUSDPrice(ctx, r.Conn, gameID, p, checkedAt)
}

// ListGamesNeedingDetails returns active games with a Steam mapping that have no
// game_details row, or whose fetched_at is before fetchedBefore. Never-fetched games
// come first, then the oldest fetched_at, then higher priority.
func (r *Repo) ListGamesNeedingDetails(ctx context.Context, fetchedBefore time.Time, limit int) ([]GameRef, error) {
	return listGamesNeedingDetails(ctx, r.Conn, fetchedBefore, limit)
}

func upsertGameDetails(ctx context.Context, db execer, gameID string, d *steam.Details, rev *steam.Reviews) error {
	if d == nil {
		return errors.New("upserting game details: nil details")
	}
	genres, err := jsonArray(d.Genres)
	if err != nil {
		return fmt.Errorf("encoding genres: %w", err)
	}
	categories, err := jsonArray(d.Categories)
	if err != nil {
		return fmt.Errorf("encoding categories: %w", err)
	}
	ageRatings, err := jsonArray(d.AgeRatings)
	if err != nil {
		return fmt.Errorf("encoding age ratings: %w", err)
	}
	platforms, err := json.Marshal(d.Platforms)
	if err != nil {
		return fmt.Errorf("encoding platforms: %w", err)
	}
	screenshots, err := jsonArray(d.Screenshots)
	if err != nil {
		return fmt.Errorf("encoding screenshots: %w", err)
	}
	movies, err := jsonArray(d.Movies)
	if err != nil {
		return fmt.Errorf("encoding movies: %w", err)
	}

	var releaseDate *string
	if d.ReleaseDate != nil {
		s := d.ReleaseDate.Format("2006-01-02")
		releaseDate = &s
	}

	var reviewSummary *string
	var reviewPositive, reviewTotal *int
	if rev != nil {
		reviewSummary = nullIfEmpty(rev.Summary)
		positive, total := rev.Positive, rev.Total
		reviewPositive, reviewTotal = &positive, &total
	}

	const q = `
		INSERT INTO game_details (
			game_id, short_description, release_date_text, release_date, coming_soon,
			developers, publishers, genres, categories, metacritic_score, metacritic_url,
			age_ratings, platforms, screenshots, movies,
			pc_req_min_html, pc_req_rec_html,
			mac_req_min_html, mac_req_rec_html, linux_req_min_html, linux_req_rec_html,
			supported_languages_html, website,
			review_summary, review_positive, review_total, fetched_at
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7, $8, $9, $10, $11,
			$12, $13, $14, $15,
			$16, $17, $18, $19, $20, $21,
			$22, $23,
			$24, $25, $26, NOW()
		)
		ON CONFLICT (game_id) DO UPDATE SET
			short_description = EXCLUDED.short_description,
			release_date_text = EXCLUDED.release_date_text,
			release_date = EXCLUDED.release_date,
			coming_soon = EXCLUDED.coming_soon,
			developers = EXCLUDED.developers,
			publishers = EXCLUDED.publishers,
			genres = EXCLUDED.genres,
			categories = EXCLUDED.categories,
			metacritic_score = EXCLUDED.metacritic_score,
			metacritic_url = EXCLUDED.metacritic_url,
			age_ratings = EXCLUDED.age_ratings,
			platforms = EXCLUDED.platforms,
			screenshots = EXCLUDED.screenshots,
			movies = EXCLUDED.movies,
			pc_req_min_html = EXCLUDED.pc_req_min_html,
			pc_req_rec_html = EXCLUDED.pc_req_rec_html,
			mac_req_min_html = EXCLUDED.mac_req_min_html,
			mac_req_rec_html = EXCLUDED.mac_req_rec_html,
			linux_req_min_html = EXCLUDED.linux_req_min_html,
			linux_req_rec_html = EXCLUDED.linux_req_rec_html,
			supported_languages_html = EXCLUDED.supported_languages_html,
			website = EXCLUDED.website,
			review_summary = EXCLUDED.review_summary,
			review_positive = EXCLUDED.review_positive,
			review_total = EXCLUDED.review_total,
			fetched_at = NOW();`
	_, err = db.Exec(ctx, q,
		gameID,
		d.ShortDescription,
		d.ReleaseDateText,
		releaseDate,
		d.ComingSoon,
		stringArray(d.Developers),
		stringArray(d.Publishers),
		string(genres),
		string(categories),
		d.MetacriticScore,
		nullIfEmpty(d.MetacriticURL),
		string(ageRatings),
		string(platforms),
		string(screenshots),
		string(movies),
		nullIfEmpty(d.PCReqMinHTML),
		nullIfEmpty(d.PCReqRecHTML),
		nullIfEmpty(d.MacReqMinHTML),
		nullIfEmpty(d.MacReqRecHTML),
		nullIfEmpty(d.LinuxReqMinHTML),
		nullIfEmpty(d.LinuxReqRecHTML),
		nullIfEmpty(d.SupportedLanguages),
		nullIfEmpty(d.Website),
		reviewSummary,
		reviewPositive,
		reviewTotal,
	)
	if err != nil {
		return fmt.Errorf("upserting game details: %w", err)
	}
	return nil
}

func saveUSDPrice(ctx context.Context, db execer, gameID string, p steam.BatchPrice, checkedAt time.Time) error {
	if p.Unavailable {
		return nil
	}
	base := strconv.FormatFloat(p.Base, 'f', 2, 64)
	current := strconv.FormatFloat(p.Current, 'f', 2, 64)
	isDeal := p.DiscountPct > 0

	tx, err := db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning usd price transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const insert = `
		INSERT INTO game_usd_snapshots (
			game_id, base_price, current_price, discount_pct, is_deal, checked_at
		) VALUES ($1, $2, $3, $4, $5, $6);`
	if _, err := tx.Exec(ctx, insert, gameID, base, current, p.DiscountPct, isDeal, checkedAt); err != nil {
		return fmt.Errorf("inserting usd price snapshot: %w", err)
	}

	const upsertCurrent = `
		INSERT INTO game_current_price_usd (
			game_id, base_price, current_price, discount_pct, is_deal, checked_at
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (game_id) DO UPDATE SET
			base_price = EXCLUDED.base_price,
			current_price = EXCLUDED.current_price,
			discount_pct = EXCLUDED.discount_pct,
			is_deal = EXCLUDED.is_deal,
			checked_at = EXCLUDED.checked_at;`
	if _, err := tx.Exec(ctx, upsertCurrent, gameID, base, current, p.DiscountPct, isDeal, checkedAt); err != nil {
		return fmt.Errorf("upserting usd current price: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing usd price transaction: %w", err)
	}
	return nil
}

func listGamesNeedingDetails(ctx context.Context, db execer, fetchedBefore time.Time, limit int) ([]GameRef, error) {
	const q = `
		SELECT g.id::text, m.external_app_id
		FROM games g
		JOIN LATERAL (
			SELECT gm.external_app_id
			FROM game_store_mappings gm
			JOIN stores s ON s.id = gm.store_id AND s.code = 'steam'
			WHERE gm.game_id = g.id AND gm.external_app_id <> ''
			ORDER BY gm.last_checked_at DESC NULLS LAST
			LIMIT 1
		) m ON true
		LEFT JOIN game_details d ON d.game_id = g.id
		WHERE g.is_active = true
		  AND (d.game_id IS NULL OR d.fetched_at < $1)
		ORDER BY d.fetched_at ASC NULLS FIRST, g.priority DESC
		LIMIT $2;`

	rows, err := db.Query(ctx, q, fetchedBefore, limit)
	if err != nil {
		return nil, fmt.Errorf("listing games needing details: %w", err)
	}
	defer rows.Close()

	var out []GameRef
	for rows.Next() {
		var g GameRef
		if err := rows.Scan(&g.GameID, &g.ExternalAppID); err != nil {
			return nil, fmt.Errorf("scanning game needing details: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating games needing details: %w", err)
	}
	return out, nil
}

// jsonArray encodes v as a JSON array, writing [] for a nil slice instead of null.
func jsonArray[T any](v []T) ([]byte, error) {
	if v == nil {
		v = []T{}
	}
	return json.Marshal(v)
}

// stringArray returns v, or an empty slice for nil, since a nil []string is sent as NULL.
func stringArray(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// nullIfEmpty maps "" to NULL.
func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
