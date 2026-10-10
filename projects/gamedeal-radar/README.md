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

Secrets are never logged. Healthcheck URLs are scrubbed from any error text.

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
