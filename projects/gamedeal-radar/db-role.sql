-- =============================================================================
-- Least-privilege Postgres role for the GameDeal Radar daily price check.
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
GRANT SELECT, INSERT, UPDATE ON game_details TO gdr_daily_job;
GRANT SELECT, INSERT, UPDATE ON game_current_price_usd TO gdr_daily_job;
GRANT SELECT, INSERT ON game_usd_snapshots TO gdr_daily_job;

-- Rows use gen_random_uuid() defaults, so no sequence grants are needed.

-- Optional, after reviewing: keep the password out of logs and shell history.
-- ALTER ROLE gdr_daily_job SET statement_timeout = '60s';

-- To rotate the password later:
--   ALTER ROLE gdr_daily_job PASSWORD '<NEW_STRONG_PASSWORD>';
-- then update the GDR_DATABASE_URL secret in GitHub. Revoke with:
--   REVOKE ALL ON ALL TABLES IN SCHEMA public FROM gdr_daily_job;
--   DROP ROLE gdr_daily_job;
