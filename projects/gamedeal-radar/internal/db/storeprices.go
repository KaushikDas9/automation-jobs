package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"gamedeal-radar/daily-check/internal/storeprices"
)

// upsertStorePriceSQL writes one game_store_prices row. The DO UPDATE branch has a WHERE
// guard so a source='manual' row is never changed: the statement then affects 0 rows,
// which SaveGameRows counts as kept_manual. Do not remove the guard.
const upsertStorePriceSQL = `
INSERT INTO game_store_prices (
    game_id, store_code, region, currency, base_price, current_price, discount_pct,
    store_url, status, checked_at, source, source_updated_at
) VALUES ($1::uuid, $2, $3, $4, $5::numeric, $6::numeric, $7, $8, $9, now(), 'auto', $10)
ON CONFLICT (game_id, store_code, region) DO UPDATE SET
    currency           = EXCLUDED.currency,
    base_price         = EXCLUDED.base_price,
    current_price      = EXCLUDED.current_price,
    discount_pct       = EXCLUDED.discount_pct,
    store_url          = EXCLUDED.store_url,
    status             = EXCLUDED.status,
    checked_at         = now(),
    source             = 'auto',
    source_updated_at  = EXCLUDED.source_updated_at
WHERE game_store_prices.source <> 'manual'`

const activeGamesSQL = `
SELECT id::text, name FROM games WHERE is_active = true ORDER BY priority DESC, name ASC`

const storedCentsSQL = `
SELECT game_id::text, region, (COALESCE(base_price, current_price) * 100)::bigint
FROM game_store_prices
WHERE store_code = $1 AND COALESCE(base_price, current_price) IS NOT NULL`

const manualGameIDsSQL = `
SELECT DISTINCT game_id::text FROM game_store_prices
WHERE store_code = $1 AND source = 'manual'`

const checkedCountSQL = `
SELECT COUNT(*) FROM games g
WHERE g.is_active = true
  AND EXISTS (
      SELECT 1 FROM game_store_prices p
      WHERE p.game_id = g.id AND p.store_code = $1 AND p.checked_at >= $2
  )`

// StorePriceRepo implements storeprices.Repo on one pgx connection.
type StorePriceRepo struct {
	Conn *pgx.Conn
}

// ActiveGames returns the active games in priority order.
func (r StorePriceRepo) ActiveGames(ctx context.Context) ([]storeprices.Game, error) {
	rows, err := r.Conn.Query(ctx, activeGamesSQL)
	if err != nil {
		return nil, fmt.Errorf("listing active games: %w", err)
	}
	defer rows.Close()
	var out []storeprices.Game
	for rows.Next() {
		var g storeprices.Game
		if err := rows.Scan(&g.ID, &g.Name); err != nil {
			return nil, fmt.Errorf("scanning active game: %w", err)
		}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating active games: %w", err)
	}
	return out, nil
}

// StoredCents returns the stored regular price (base price, else current price), in cents,
// of every row for the store.
func (r StorePriceRepo) StoredCents(ctx context.Context, storeCode string) (map[storeprices.StoredKey]int64, error) {
	rows, err := r.Conn.Query(ctx, storedCentsSQL, storeCode)
	if err != nil {
		return nil, fmt.Errorf("loading stored prices: %w", err)
	}
	defer rows.Close()
	out := map[storeprices.StoredKey]int64{}
	for rows.Next() {
		var gameID, region string
		var cents int64
		if err := rows.Scan(&gameID, &region, &cents); err != nil {
			return nil, fmt.Errorf("scanning stored price: %w", err)
		}
		out[storeprices.StoredKey{GameID: gameID, Region: region}] = cents
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating stored prices: %w", err)
	}
	return out, nil
}

// ManualGameIDs returns the games with a manual row for the store.
func (r StorePriceRepo) ManualGameIDs(ctx context.Context, storeCode string) (map[string]bool, error) {
	rows, err := r.Conn.Query(ctx, manualGameIDsSQL, storeCode)
	if err != nil {
		return nil, fmt.Errorf("loading manual rows: %w", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scanning manual row: %w", err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating manual rows: %w", err)
	}
	return out, nil
}

// CheckedCount counts active games with a row for the store checked at or after since.
func (r StorePriceRepo) CheckedCount(ctx context.Context, storeCode string, since time.Time) (int, error) {
	var n int
	if err := r.Conn.QueryRow(ctx, checkedCountSQL, storeCode, since).Scan(&n); err != nil {
		return 0, fmt.Errorf("counting checked games: %w", err)
	}
	return n, nil
}

// SaveGameRows writes all rows of one game in one transaction. If any write fails, the
// whole game is rolled back.
func (r StorePriceRepo) SaveGameRows(ctx context.Context, storeCode, gameID string, rows []storeprices.Price) (storeprices.SaveResult, error) {
	var res storeprices.SaveResult
	tx, err := r.Conn.Begin(ctx)
	if err != nil {
		return res, fmt.Errorf("starting transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	for _, p := range rows {
		if p.StoreCode != storeCode {
			return res, fmt.Errorf("row store %q does not match %q", p.StoreCode, storeCode)
		}
		tag, err := tx.Exec(ctx, upsertStorePriceSQL,
			gameID, p.StoreCode, p.Region,
			nullable(p.Currency), centsParam(p.BaseCents), centsParam(p.CurrentCents),
			p.DiscountPct, nullable(p.StoreURL), p.Status, p.SourceUpdatedAt,
		)
		if err != nil {
			return storeprices.SaveResult{}, fmt.Errorf("upserting %s %s row: %w", p.StoreCode, p.Region, err)
		}
		if tag.RowsAffected() == 0 {
			res.KeptManual++
			continue
		}
		res.Written++
	}
	if err := tx.Commit(ctx); err != nil {
		return storeprices.SaveResult{}, fmt.Errorf("committing game rows: %w", err)
	}
	return res, nil
}

// upsertFXRateSQL stores the USD to INR rate. Only the rate row is touched.
const upsertFXRateSQL = `
INSERT INTO fx_rates (pair, rate, rate_date, fetched_at)
VALUES ($1, $2::numeric, $3::date, now())
ON CONFLICT (pair) DO UPDATE SET
    rate       = EXCLUDED.rate,
    rate_date  = EXCLUDED.rate_date,
    fetched_at = now()`

// USDINRPair is the fx_rates key for USD to INR.
const USDINRPair = "USD_INR"

// SaveUSDINR upserts the USD to INR rate. rate is written with six decimals.
func SaveUSDINR(ctx context.Context, conn *pgx.Conn, rate float64, rateDate string) error {
	if rate <= 0 {
		return errors.New("refusing to store a non-positive rate")
	}
	if _, err := conn.Exec(ctx, upsertFXRateSQL, USDINRPair, fmt.Sprintf("%.6f", rate), rateDate); err != nil {
		return fmt.Errorf("upserting fx rate: %w", err)
	}
	return nil
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func centsParam(c *int64) *string {
	if c == nil {
		return nil
	}
	s := storeprices.FormatCents(*c)
	return &s
}
