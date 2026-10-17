-- =============================================================================
-- Least-privilege Postgres role for the GameDeal Radar daily price check.
--
-- UPDATE: the table grants (sections 3 to 9) are now applied automatically by migration
-- 000011_grant_jobs_role in the GameDealRader repo, which runs on every API deploy. Only
-- sections 1 and 2 (create the role, CONNECT and schema USAGE) are still run by hand, once.
-- The rest of this file stays as the written record of what the role may do.
--
-- DOCUMENTATION ONLY. This file is NOT executed by CI or by any script in this
-- repository. A DBA or the project owner runs it once, by hand, against the
-- target database, after replacing the placeholders below. Never commit the
-- real password.
--
-- Placeholders:
--   <DB_NAME>       target database name (Neon: the database in the connection string)
--   <STRONG_PASSWORD> generate with a password manager; store it only as the
--                     GDR_DATABASE_URL GitHub Actions secret
--
-- The role gets exactly what daily-check uses:
--   stores                   SELECT   (find the 'steam' store id)
--   games                    SELECT   (active catalog list)
--   game_store_mappings      SELECT   (Steam app id lookup)
--                            UPDATE   (last_checked_at, updated_at only)
--   price_snapshots          SELECT   (previous snapshot for comparison)
--                            INSERT   (new snapshot)
--   game_current_price       SELECT   (ON CONFLICT target lookup)
--                            INSERT, UPDATE (upsert of the current price)
--   price_events             INSERT   (NEW_DEAL / PRICE_DROP / DEAL_ENDED)
--
-- Resilience revision: the resume and coverage queries read games and
-- game_store_mappings (both already granted above). Marking a game as checked uses
-- the existing UPDATE on last_checked_at and updated_at. No new grants are needed.
--
-- Deliberately NOT granted: DELETE, TRUNCATE, DDL, access to job_records,
-- content_records, publishing_records, source_references, or any other table,
-- and no superuser or CREATEDB/CREATEROLE rights.
-- =============================================================================

-- 1. Role. LOGIN with a password, no inherited privileges.
CREATE ROLE gdr_daily_job LOGIN PASSWORD '<STRONG_PASSWORD>'
    NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOINHERIT;

-- 2. Connect and schema usage. Nothing else on the database.
GRANT CONNECT ON DATABASE <DB_NAME> TO gdr_daily_job;
GRANT USAGE ON SCHEMA public TO gdr_daily_job;

-- 3. Reference data (read only).
GRANT SELECT ON stores TO gdr_daily_job;
GRANT SELECT ON games TO gdr_daily_job;
GRANT SELECT ON game_store_mappings TO gdr_daily_job;

-- 4. Mapping timestamps only. The job never changes an app id or store URL.
GRANT UPDATE (last_checked_at, updated_at) ON game_store_mappings TO gdr_daily_job;

-- 5. Price history: append-only snapshots and events.
GRANT SELECT, INSERT ON price_snapshots TO gdr_daily_job;
GRANT INSERT ON price_events TO gdr_daily_job;

-- 6. Current-price cache: upserted once per game per run.
--    SELECT is required for ON CONFLICT ... DO UPDATE on the game_id conflict target.
GRANT SELECT, INSERT, UPDATE ON game_current_price TO gdr_daily_job;

-- 7. Steam store details (weekly details-check) and USD prices (daily USD pass).
--    Requires migration 000005 (game_details_and_usd_prices) in the GameDealRader repo. game_details and
--    game_current_price_usd are upserted, so they need SELECT, INSERT and UPDATE.
--    game_usd_snapshots is append-only history.
--    cmd/steam-tags (weekly) needs UPDATE on game_details, which the grant below already
--    gives. It writes only the tags column, and it never deletes. No new grant is needed.
GRANT SELECT, INSERT, UPDATE ON game_details TO gdr_daily_job;
GRANT SELECT, INSERT, UPDATE ON game_current_price_usd TO gdr_daily_job;
GRANT SELECT, INSERT ON game_usd_snapshots TO gdr_daily_job;

-- 8. Store prices from other stores (cmd/gog-prices, cmd/epic-prices). Requires migrations
--    000007 (game_store_prices) and 000009 (source, source_updated_at) in the GameDealRader repo.
--    Rows are upserted per game and region. There is NO DELETE here, on purpose.
--    The upsert's DO UPDATE has WHERE game_store_prices.source <> 'manual', so the job
--    cannot change a row the owner typed. A manual row is never overwritten.
GRANT SELECT, INSERT, UPDATE ON game_store_prices TO gdr_daily_job;

-- 9. USD to INR rate (cmd/fx-rate). One row, pair 'USD_INR', upserted once a day.
--    No DELETE, same as above.
GRANT SELECT, INSERT, UPDATE ON fx_rates TO gdr_daily_job;

-- 10. Epic free games (cmd/epic-free). The grant is applied by migration 000012 in the
--     GameDealRader repo, not by this file. Rows are upserted per epic_id, with SELECT for
--     the ON CONFLICT lookup. There is NO DELETE here, on purpose.
-- GRANT SELECT, INSERT, UPDATE ON epic_free_games TO gdr_daily_job;

-- 11. Requested games (cmd/add-games). Applied by migration 000013 in the GameDealRader
--     repo, which also creates the game_requests table. The job claims queue rows
--     (SELECT ... FOR UPDATE SKIP LOCKED, which needs UPDATE) and records their end state.
--     It activates a game by updating only is_active and updated_at on games. Everything else
--     it writes (game_details, prices, snapshots, events, game_store_prices, tags) is already
--     granted in sections 3 to 8. There is NO INSERT or DELETE on game_requests or games:
--     the API creates the rows, the job cannot create or remove a game.
--       GRANT SELECT, UPDATE ON game_requests TO gdr_daily_job;
--       GRANT UPDATE (is_active, updated_at) ON games TO gdr_daily_job;
GRANT SELECT, UPDATE ON game_requests TO gdr_daily_job;
GRANT UPDATE (is_active, updated_at) ON games TO gdr_daily_job;

-- Rows use gen_random_uuid() defaults, so no sequence grants are needed.

-- Optional, after reviewing: keep the password out of logs and shell history.
-- ALTER ROLE gdr_daily_job SET statement_timeout = '60s';

-- To rotate the password later:
--   ALTER ROLE gdr_daily_job PASSWORD '<NEW_STRONG_PASSWORD>';
-- then update the GDR_DATABASE_URL secret in GitHub. Revoke with:
--   REVOKE ALL ON ALL TABLES IN SCHEMA public FROM gdr_daily_job;
--   DROP ROLE gdr_daily_job;
