# GameDeal Radar daily price check

Checks the Steam India (`cc=in`, INR) price of every active GameDeal Radar game and
stores a price snapshot. When a deal starts, ends, or drops further, it records a price
event. It is a port of the server's `ProcessPriceResult`. The workflow is
`.github/workflows/gamedeal-radar-daily.yml`.

## Schedule

Steam refreshes deals around 10:00 AM Pacific. That is 18:00 UTC in winter and 17:00 UTC
in summer. This is an **unverified** assumption. GitHub cron is UTC only, so the times
below are fixed in UTC.

Each day has two slots. Each slot has up to six attempts, with gaps that grow:

| Attempt | Slot A offset | Slot A (UTC) | Slot B offset | Slot B (UTC) |
| --- | --- | --- | --- | --- |
| 1 | +0 min | 18:30 | +0 min | 06:30 |
| 2 | +30 min | 19:00 | +30 min | 07:00 |
| 3 | +75 min | 19:45 | +75 min | 07:45 |
| 4 | +135 min | 20:45 | +135 min | 08:45 |
| 5 | +210 min | 22:00 | +210 min | 10:00 |
| 6 (final) | +300 min | 23:30 | +300 min | 11:30 |

In IST, slot A runs from 00:00 to 05:00 and slot B from 12:00 to 17:00. The last attempt of each
slot ends about 7 hours before the next slot starts. GitHub can start scheduled runs 5 to
30 minutes late. Each run maps to its attempt with a 20 minute tolerance.

Manual runs (`workflow_dispatch`) skip the window check, default to dry run, and are not
counted as scheduled attempts.

## What happens in a slot

1. **Window check.** If the run starts more than 6 hours after the slot start, it prints
   `outside slot window, skipping` and exits 0 without fetching anything.
2. **Slot already complete?** It counts the active games with a Steam mapping that were
   checked since the slot start. If at least 90% are done, it prints `slot already
   complete` and exits 0 without fetching. Backup attempts are cheap for this reason.
3. **Resume.** It fetches only the games not yet checked in this slot. A game is never
   fetched twice in one slot.
4. **Main pass.** It fetches each missing game at no more than 1 request per second with
   timeouts and retries inside the HTTP client.
5. **Retry passes.** Games that failed with a transient error (network, timeout, HTTP 429
   or 5xx) go on a retry list. The job waits `RETRY_WAIT_1` (default 5 minutes) and
   retries only those games. If they still fail, it waits `RETRY_WAIT_2` (default 10
   minutes) and retries once more. A wait is skipped if it would leave less than 5
   minutes before the 45 minute run deadline.
6. **Permanent failures are not retried.** "Not found" and "price unavailable" mean Steam
   has no price for that country. The game is recorded as checked so later attempts in
   the same slot skip it. Other 4xx responses and parse errors are also not retried.
7. **Coverage check.** It recounts the database. Coverage is the share of mapped active
   games checked in this slot. At least 90% is success (exit 0). Below 90% is a failure
   (exit 1). Games that failed do not by themselves cause a failure.

### Example: 500 games, 20 transient failures

Attempt 1 fetches the 500 games. 480 succeed, and 20 fail with HTTP 503 errors. The job
puts those 20 on the retry list. After 5 minutes it retries them: 15 succeed and 5 fail
again. After 10 more minutes it retries the last 5: 3 succeed and 2 fail. The run ends
with 498 of 500 games checked, which is 99.6% coverage. That is above 90%, so the run
exits 0 and sends the success ping. Backup attempts see the slot as complete and do
nothing.

If the outage had been bigger, for example 60 games still missing after the retries,
coverage would be 88%. The run would exit 1 and send the failure ping. Attempt 2 then
starts 30 minutes after the slot start. It fetches only the games still missing, so the
first attempt's work is not repeated.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | Coverage is at least 90%, or the slot was already complete, or the run is outside the window |
| 1 | Coverage is below 90% after retries, or the database was unreachable, or no games have a Steam mapping |
| 2 | Configuration error (for example a bad `RETRY_WAIT_1` or a malformed `GDR_HEALTHCHECK_URL`) |

## Notifications

| Event | What you get |
| --- | --- |
| Any attempt ends below 90% coverage (exit 1) | GitHub's failed-run email, and the `/fail` ping on `GDR_HEALTHCHECK_URL` if it is set |
| Attempt 6 (final) ends below 90% | Everything above, plus a GitHub issue titled `GameDeal Radar: slot <YYYY-MM-DD HH:MMZ> failed after 6 attempts` (label `job-failure`), and a `/fail` ping on `GDR_HEALTHCHECK_FINAL_URL` if it is set |
| An attempt ends with 90% or more coverage | The success ping on `GDR_HEALTHCHECK_URL`, and any open `job-failure` issues are closed with a "recovered" comment |

The issue includes the slot id, the coverage percentage, the number of games failed in the
final attempt, and a link to the run. It has no secrets. If an open issue with the same
title already exists, no duplicate is created.

Slack messages are described in the next section. Secrets are never logged. Healthcheck URLs are scrubbed from any error text.

## Slack notifications

Optional. If the secret is not set, nothing is sent and nothing is logged about it.

### Set it up

1. Go to https://api.slack.com/apps, choose **Create New App**, then **From scratch**.
2. Open **Incoming Webhooks**, switch it on, and choose **Add New Webhook to Workspace**.
   Pick the channel for the messages.
3. Copy the webhook URL. In the GitHub repository open Settings, Secrets and variables,
   Actions, and add a secret named `SLACK_WEBHOOK_URL` with that URL. This secret is shared
   by every project in the repository, because they all post to one channel.

The workflow reads `SLACK_WEBHOOK_URL` first. If that is empty, it falls back to
`GDR_SLACK_WEBHOOK_URL`, a project-specific secret that still works for existing setups.

Every message starts with the tag `[GameDeal Radar]`, so messages from other projects in the
same channel are easy to tell apart.

The URL is a secret. Do not paste it into issues, commits, or chat. It is never logged,
and it is removed from any error text.

### When a message is sent

| Situation | Message |
| --- | --- |
| A real run finishes with 90% or more coverage and did work (updated or skipped games) | `success` |
| An attempt (1 to 5) ends below 90% coverage, or the database is unreachable | `attempt_failed` |
| Attempt 6 ends below 90% coverage (in addition to the GitHub issue) | `final_failed` |
| A backup attempt finds the slot already complete | nothing |
| Dry run | nothing |

Manual runs with dry run switched off can send messages. Slack is best effort: it has a
10 second timeout and one retry, and a Slack problem never changes the job's result.

### Customize the messages

The default text lives in `internal/notify/messages.tmpl`, which is compiled into the
program. To change the text without touching code, make a folder with any of
`success.tmpl`, `attempt_failed.tmpl` and `final_failed.tmpl`, each holding only the
message text, and point `GDR_NOTIFY_TEMPLATE_DIR` at it. A missing or broken file falls
back to the default. Messages use Slack mrkdwn and Go `text/template` syntax. Keep the
`[GameDeal Radar]` tag at the start of every message, because the channel is shared. Fields:

| Field | Meaning |
| --- | --- |
| `{{.SlotLabel}}` | Slot start in IST and UTC, for example `00:00 IST / 18:30 UTC, 11 Oct` |
| `{{.Attempt}}` | Attempt number, 1 to 6 |
| `{{.Total}}` | Active games with a Steam mapping |
| `{{.Updated}}` | Price snapshots written in this run |
| `{{.Skipped}}` | Games with no Steam price |
| `{{.Failed}}` | Games that failed for good in this run |
| `{{.Retried}}` | Games that went through a retry pass |
| `{{.CoveragePct}}` | Coverage in this slot, for example `99.6` |
| `{{.Duration}}` | How long this run took, for example `7m12s` |
| `{{.RunURL}}` | Link to the Actions run (built from `GITHUB_SERVER_URL`, `GITHUB_REPOSITORY`, `GITHUB_RUN_ID`) |

## Why jobs cannot stack up

See the root README. In short: the concurrency group allows one active run and one
pending run, so a backlog never builds up. Each slot works only on its own games, and a
run that starts more than 6 hours after its slot start exits without fetching.

## Environment variables

| Name | Required | Default | Meaning |
| --- | --- | --- | --- |
| `GDR_DATABASE_URL` | yes | none | Postgres connection string, from the `gdr_daily_job` role. Never logged. |
| `DRY_RUN` | no | `0` | `1` or `true` fetches prices and writes nothing. Workflow manual runs default to `1`. |
| `LIMIT` | no | none | Maximum games to fetch in this run. For testing. A limited run usually does not reach 90% coverage. |
| `SLOT_START` | no | clock | RFC3339 slot start override for testing. It disables the window check. |
| `ATTEMPT` | no | clock | `1` to `6` attempt override for testing or manual runs. It disables the window check. |
| `RETRY_WAIT_1` | no | `5m` | Wait before the first retry pass. Go duration, `0` for tests. |
| `RETRY_WAIT_2` | no | `10m` | Wait before the second retry pass. Go duration, `0` for tests. |
| `GDR_HEALTHCHECK_URL` | no | none | Dead man's switch URL (secret). |
| `GDR_HEALTHCHECK_FINAL_URL` | no | none | Second check URL (secret), pinged with `/fail` on the final attempt's failure. |
| `SLACK_WEBHOOK_URL` | no | none | Slack Incoming Webhook URL (secret), shared by all projects. Unset means no Slack messages. |
| `GDR_SLACK_WEBHOOK_URL` | no | none | Fallback Slack webhook URL (secret), read only when `SLACK_WEBHOOK_URL` is empty. |
| `GDR_NOTIFY_TEMPLATE_DIR` | no | none | Folder with `success.tmpl`, `attempt_failed.tmpl`, `final_failed.tmpl` to override message text. |

## Set up the healthchecks.io checks

Create two checks at healthchecks.io (free plan is enough):

1. **Main check.** Name it `gamedeal-radar daily`. Set the period to **12 hours** and the
   grace time to **2 hours**. Copy its ping URL into the repository secret
   `GDR_HEALTHCHECK_URL`.
2. **Final check (optional).** Name it `gamedeal-radar final failure`. Use the same period
   and grace. Set the secret `GDR_HEALTHCHECK_FINAL_URL`. The program only pings this one
   after a final failure, so it works as an alarm.

The period is 12 hours because there are two slots a day. The job sends a success ping
after each complete slot, so the check stays green while the schedule runs. If no slot
completes for 14 hours, healthchecks.io sends you an alert. Both URLs are secrets. Do not
paste them into issues, commits, or chat.

## Database role

See `db-role.sql`. It is documentation only and is not executed by CI. The resilience
changes need no new grants. The resume query reads `game_store_mappings` and `games`,
which the role already reads. Marking a game checked uses the existing `UPDATE` on
`last_checked_at` and `updated_at`.

## Local checks

```sh
cd projects/gamedeal-radar
go vet ./... && go test -race ./...
```

Tests use no network and no database.

## Store price jobs (GOG, Epic, USD to INR)

Three more jobs run next to the Steam jobs. They share the same database role and the same
Slack and healthcheck setup, but they are separate programs with their own workflows. The
Steam jobs are not changed by them.

| Job | Command | Workflow | Schedule (UTC) | Schedule (IST) |
| --- | --- | --- | --- | --- |
| USD to INR rate | `cmd/fx-rate` | `gamedeal-radar-fx-rate.yml` | 02:30 daily | 08:00 daily |
| GOG prices (US and IN) | `cmd/gog-prices` | `gamedeal-radar-gog-prices.yml` | 03:00 main, 07:00 backup | 08:30 main, 12:30 backup |
| Epic prices | `cmd/epic-prices` | `gamedeal-radar-epic-prices.yml` | 04:00 main, 08:00 backup | 09:30 main, 13:30 backup |

Each job is also available as a manual run (`workflow_dispatch`). Manual runs default to
dry run, so nothing is written unless you untick `dry_run`.

### USD to INR rate (`cmd/fx-rate`)

Fetches the rate from Frankfurter (`https://api.frankfurter.dev/v1/latest?base=USD&symbols=INR`)
and stores it in `fx_rates` as pair `USD_INR`. The rate must be between 40 and 200. Any
failure writes nothing and exits 1. There is one request per run and no retries.

### GOG prices (`cmd/gog-prices`)

For every active game, looks up the exact title on GOG's public catalog. GOG returns both
countries in USD, so the job asks for `US` and `IN` and stores both as `store_code gog`.
A title that does not match exactly is stored as `not_listed`. A listed game with no price
is stored as `listed_no_price`. The discount is computed by the job.

- One request per second to GOG, with the User-Agent `GameDealRadar-jobs/1.0`.
- A `429`, `401` or `403` stops the run at once (exit 1, flag `blocked=true`).
- A `5xx` or network error retries that game in a retry pass. The waits are `RETRY_WAIT_1`
  (default 1m) and `RETRY_WAIT_2` (default 2m). A game that still fails gets nothing written.
- Both regions of a game are written in one transaction.
- Every scheduled run checks the database first. If at least 90% of
  active games already have a `gog` row checked in the last 20 hours, it prints
  `already complete` and exits 0 without fetching. Manual runs and dry runs skip this check.

### Epic prices (`cmd/epic-prices`)

Epic is read through the price data provider's API, `https://api.isthereanydeal.com`. Epic itself
is never called by this job. Flow:

1. **Lookup.** Each active game is looked up by its name. A match is accepted only when the
   normalized provider title equals the normalized game name. Normalization lowercases the text,
   removes `™ ® ©` and apostrophes, turns other punctuation into spaces, and collapses spaces.
   No fuzzy matching. The provider's id is used for the next steps and is never stored.
2. **Prices.** The matched ids go in batches of at most 200, once for `country=IN` and once
   for `country=US`, with `shops=16` (Epic). Only Epic deals are kept. The price is stored
   in the currency the provider returns, which must be `INR` for IN and `USD` for US. The discount is
   computed by the job, not taken from the provider.
3. **Link.** One batch call to the provider's Epic lookup gives the Epic address part. It is used only
   if it is a readable slug (`^[a-z0-9][a-z0-9-]{1,120}$`) and not a 32-character hex id. Then
   the link is `https://store.epicgames.com/en-US/p/<slug>`. Otherwise the link is an Epic
   search link: `https://store.epicgames.com/en-US/browse?q=<name>&sortBy=relevancy&sortDir=DESC&count=40`.
4. **Write.** Rows are `store_code epic`, `source auto`, with `source_updated_at` set to the
   deal's timestamp. A game with no Epic deal in a region gets no row for that region, and
   an old row is left as it is. Games with no provider match get nothing.

Rules for this job:

- **Provider deal URLs are never read or stored.** They are the provider's tracked redirects, with its
  affiliate tag. The job decodes only the amount, currency, and timestamp.
- **Manual rows are never touched.** A game that has any `source = manual` Epic row is skipped
  entirely: no lookup, and neither its price nor its link is written. The write statement
  also guards every row with `WHERE game_store_prices.source <> 'manual'`.
- **Provider credit.** The provider's API terms ask for it to be credited wherever its data is
  shown. Check the current terms before showing this data on the site, and put the credit line
  the terms ask for next to Epic prices. This README does not restate the
  exact wording of the terms.
- **Rate.** At most 2 requests per second, which keeps the job far below the provider's limit of
  1000 requests per 5 minutes. A `429` is retried once after its `Retry-After` (capped at
  120 seconds). A second `429`, or a `401` or `403`, stops the run and sets `blocked=true`.
- **Retries and coverage.** Retry passes work as for GOG. A failed batch writes nothing for
  its games.
- **Secret.** The API key comes from the `ITAD_API_KEY` environment variable. It is sent only
  in the `ITAD-API-Key` header, and it is never logged or printed.

### Shared rules for the store jobs

- **Coverage.** A run succeeds (exit 0) when at least 90% of the games in the run were handled.
  A game is handled when its rows were written, when it was skipped on purpose (a manual row,
  or no provider match), or when it had no error. A game that still fails after the retries counts
  as failed. An interrupted run (blocked, or cut short by the deadline) counts as 0% covered.
- **Jump check.** A price more than 80% away from the stored price for the same game and
  region is not written. It is logged as `price jumped more than 80%`, and the stored row stays.
  This protects against a bad response. It also means a real very large change (for example
  a 90% sale) is held back until someone checks it. Review the logged skips.
- **Sanity checks.** A priced row must have a price above zero, unless its status is `free`.
  Base price must be at least the current price. The currency must be the expected one. A
  failing row is skipped and logged, and the rest of the game is still written.
- **Not stored.** Nothing from a store's API is stored except the fields listed above. No
  provider ids, no provider URLs, no raw payloads.

### Exit codes (store jobs)

| Code | Meaning |
| --- | --- |
| 0 | Coverage is at least 90%, or the run was already complete, or (fx) the rate was stored |
| 1 | Coverage below 90%, the store blocked the run, the run was cut short, the database was unreachable, or the rate could not be fetched or stored |
| 2 | Configuration error (for example a missing `GDR_DATABASE_URL` or `ITAD_API_KEY`, or a bad `LIMIT`) |

### Environment variables (store jobs)

| Name | Used by | Required | Default | Meaning |
| --- | --- | --- | --- | --- |
| `GDR_DATABASE_URL` | all | yes | none | Postgres connection string, from the `gdr_daily_job` role. Never logged. |
| `ITAD_API_KEY` | epic-prices | yes | none | API key for the price data provider (GitHub secret `ITAD_API_KEY`). Never logged. |
| `DRY_RUN` | all | no | `0` | `1` fetches and prints, and writes nothing. Manual runs default to `1`. |
| `LIMIT` | gog, epic | no | none | Maximum games in this run. Coverage is counted over these games. For testing. |
| `RETRY_WAIT_1` | gog, epic | no | `1m` | Wait before the first retry pass. `0` for tests. |
| `RETRY_WAIT_2` | gog, epic | no | `2m` | Wait before the second retry pass. `0` for tests. |
| `FINAL_ATTEMPT` | all | no | `0` | Set by the workflow on the last scheduled run of the day. A failure then sets `final_attempt=true` for the issue step. |
| `GDR_HEALTHCHECK_URL` | all | no | none | Dead man's switch URL (secret). Not set by the store workflows. |
| `GDR_HEALTHCHECK_FINAL_URL` | all | no | none | Pinged with `/fail` on a failing final run (secret). |
| `SLACK_WEBHOOK_URL` | all | no | none | Shared Slack webhook (secret). See the Slack section above. |
| `GDR_SLACK_WEBHOOK_URL` | all | no | none | Fallback Slack webhook, read only when `SLACK_WEBHOOK_URL` is empty. |

### GitHub secrets (store jobs)

The three workflows use the existing secrets `GDR_DATABASE_URL`, `SLACK_WEBHOOK_URL` and
`GDR_SLACK_WEBHOOK_URL`, plus one new one:

- **`ITAD_API_KEY`** (new). Create an API key with the price data provider, then add it
  as a repository secret. It is used only by the Epic workflow.

The store workflows do not set the health-check secrets on purpose. The Steam daily job's
health check is a dead man's switch, and a GOG or Epic ping on the same URL would hide a Steam
outage. To add a check per job later, create a separate secret for each job and add it to that
workflow's env.

### Failure issues (store jobs)

Only the last scheduled run of the day can open an issue: the 08:00 UTC run for Epic, the
07:00 UTC run for GOG, and the 02:30 UTC run for the rate. Each job has its own title, so
they never close each other's issues. An open issue with the same title is not duplicated.
A later successful run closes it.

### Database role (store jobs)

See `db-role.sql`, section 8 and 9. The job role needs `SELECT, INSERT, UPDATE` on
`game_store_prices` and on `fx_rates`. It has no `DELETE`. The store jobs also need
migrations `000007` and `000009` from the GameDealRader repo.

## Steam tags job (`cmd/steam-tags`)

A weekly job stores each game's top Steam user tags in `game_details.tags`. It is separate
from the other jobs and uses no key. The workflow is `gamedeal-radar-steam-tags.yml`, on
Sunday at 05:00 UTC (10:30 IST), with a manual run that defaults to dry run.

- **Source.** Steam's public store API. The tag names come from `IStoreService/GetTagList`
  once per run (English). The tags of each game come from `IStoreBrowseService/GetItems`,
  in batches of 50 app ids, for the US store, with at most 10 tags per game, most popular
  first. Each stored tag is `{"id": "<tagid>", "description": "<name>"}`.
- **Games.** Active games that have a `game_details` row and a numeric Steam app id.
- **Writes.** Only `game_details.tags`, one `UPDATE` per game, never a delete. A game
  that Steam returns no usable tags for is left as it is, so an empty list never replaces
  tags it already has. A dry run writes nothing.
- **Rate and retries.** One request per second with the User-Agent `GameDealRadar/1.0 (tags
  job)`. A `429` or `5xx` is retried with a doubling wait (5s, 10s, 20s), and `Retry-After` is
  honoured up to 60 seconds. A `401` or `403`, or a `429` that does not clear, stops the run.
  A batch that still fails is counted as failed, and the other batches go on.
- **Coverage.** The run succeeds (exit 0) when at least 90% of the games were handled. A
  game with no tags counts as handled. A failed game does not. A stopped run exits 1.
- **No healthcheck.** This job reads no `GDR_HEALTHCHECK_*` variables. Its only Slack
  message is the success or failure line, and the failure issue opens only on the scheduled
  run.
- **Limit.** `LIMIT=n` caps the games in a run, for testing.
- **Database role.** Needs only `UPDATE` on `game_details`, already granted in
  `db-role.sql` section 7.

Exit codes: 0 stored (or dry run fetched) with 90% coverage, 1 the tag list or games could
not be read, Steam refused, the run was cut short, a write failed, or coverage was below
90%, 2 configuration error.

## Epic free games job (`cmd/epic-free`)

A daily job stores Epic's current and upcoming free games in `epic_free_games`. The workflow
is `gamedeal-radar-epic-free.yml`. It runs at 05:30 UTC every day (11:00 IST) and at 15:45 UTC
on Thursdays (21:15 IST), 45 minutes after Epic's usual Thursday rotation. A manual run
defaults to dry run.

- **Source.** Epic's public free games list, one request per run:
  `https://store-site-backend-static.ak.epicgames.com/freeGamesPromotions?locale=en-US&country=IN&allowCountries=IN`.
  It needs no key and no cookies. The User-Agent is `PriceTrackerJob/1.0 (daily free games job)`,
  the timeout is 30 seconds, and a `5xx` response is retried once after 10 seconds. Any other
  failure is not retried.
- **Which offers.** Only offers with a 0 percent discount are kept. A game that is free now
  uses the current offer. A game that is free next uses the upcoming offer. An element with
  no free offer is skipped.
- **Store link.** `https://store.epicgames.com/en-US/p/<slug>` when a slug is readable
  (`^[a-z0-9][a-z0-9-]{1,120}$`, not a 32-character hex id). Otherwise an Epic search link for
  the title.
- **Image.** The first `https` image in the order `OfferImageWide`, `Thumbnail`,
  `DieselStoreFrontWide`. Other URLs are not stored.
- **Price.** The original price, converted from minor units. It is empty when the feed gives
  no usable price.
- **Writes.** One transaction, one upsert per `epic_id`. Every column is refreshed and
  `fetched_at` is set to now. Nothing is deleted, so a game that leaves the list keeps its row.
- **Safety.** A feed with zero free offers is a failure, so a changed feed never writes.
  A dry run writes nothing.
- **No healthcheck.** This job reads no `GDR_HEALTHCHECK_*` variables. Its only Slack
  message is the success or failure line, and the failure issue opens only on the scheduled
  run.
- **Database role.** Needs `SELECT, INSERT, UPDATE` on `epic_free_games`, no `DELETE`. The
  grant is applied by migration `000012` in the GameDealRader repo, and noted in
  `db-role.sql` section 10.

Exit codes: 0 stored (or dry run fetched), 1 the feed could not be read, had no free games,
or the write failed, 2 configuration error.
