# automation-jobs

Scheduled jobs for my projects, run on GitHub Actions instead of on always-on servers.
Each project lives in its own folder under `projects/` as a self-contained Go module
(its own `go.mod`), and each gets one workflow under `.github/workflows/`.

## Layout

```
.github/workflows/
  gamedeal-radar-daily.yml     schedule + manual trigger for the GameDeal Radar job
projects/
  gamedeal-radar/              Go module: daily Steam India price check
    cmd/daily-check/main.go    entry point (config from env, summary, exit codes)
    internal/steam/            Steam appdetails client, rate limiter, response parser
    internal/pricing/          snapshot + price-event rules (NEW_DEAL, PRICE_DROP, DEAL_ENDED)
    internal/db/               Postgres queries (pgx)
    db-role.sql                least-privilege DB role (documentation only, not executed)
```

## GameDeal Radar daily check

Runs twice a day, timed to follow Steam's daily deal refresh (unverified: around
10:00 AM Pacific, which is 18:00 UTC in winter or 17:00 UTC in summer):

| Run | Cron (UTC) | IST | Notes |
| --- | --- | --- | --- |
| Morning | `30 18 * * *` | 00:00 | About 30 min after refresh in winter, about 90 min in summer |
| Noon | `30 6 * * *` | 12:00 | Twelve hours after the first run |

Each run takes roughly 11 minutes, so about 22 minutes a day in total (estimate, not
measured yet). The `concurrency` group `gamedeal-radar-daily` with
`cancel-in-progress: false` means a run never overlaps another: if one is still
going when the next starts, the next waits in the queue.

Scheduled runs are real writes. Only manual `workflow_dispatch` runs default to dry
run. GitHub can also start scheduled runs 5 to 30 minutes late.

To shift the times, edit both `cron` lines in
`.github/workflows/gamedeal-radar-daily.yml`. Convert IST to UTC by subtracting 5h30m,
and remember that Steam's refresh moves between 17:00 and 18:00 UTC with US daylight
saving time, while GitHub cron does not. For each active game it fetches the Steam
India (`cc=in`, INR) price at no more than 1 request per second, writes a price
snapshot, updates the current-price row, and records a price event when the deal
state changes. It is a port of the server's `ProcessPriceResult`. The differences
from the server are listed in the commit message that added this job.

Exit codes: `0` success, `1` the run failed (database unreachable, more than 20% of
games failed, or the run was cancelled), `2` configuration error.

Environment variables the job reads:

| Name | Required | Meaning |
| --- | --- | --- |
| `GDR_DATABASE_URL` | yes | Postgres connection string. Never logged. |
| `DRY_RUN` | no | `1` or `true` fetches prices but writes nothing. |
| `LIMIT` | no | Max number of games to process, for testing. |

Run the tests locally with no network or database needed:

```sh
cd projects/gamedeal-radar
go vet ./... && go test ./...
```

## Adding a job for a new project

1. Create `projects/<project-name>/` with its own `go.mod` (or other tooling). Keep it
   self-contained so it can build without the rest of the repo.
2. Add `.github/workflows/<project-name>-<job>.yml`, modelled on
   `gamedeal-radar-daily.yml`:
   - `on: schedule` with a cron expression (or several), plus `workflow_dispatch` so you can run it
     by hand.
   - `permissions: contents: read` (the default for any new workflow).
   - A `concurrency` group unique to this job, with `cancel-in-progress: false`, so
     runs never overlap.
   - `timeout-minutes` set to a value that fits the job.
   - Pin third-party actions to a major version, for example `actions/checkout@v4`.
3. Pass secrets to the step that needs them as `env:` entries, not as command-line
   arguments.

## Secrets naming

Repository secrets are named `PROJECT_NAME_SECRET`, with the project name in upper
case and underscores:

- `GDR_DATABASE_URL` for GameDeal Radar's database connection
- `<OTHER>_API_KEY` and so on for other projects

Add secrets under Settings, then Secrets and variables, then Actions. Give each project
its own database role and credentials. Do not share one super-user connection string
across projects.

## Schedules and UTC

GitHub cron expressions are always UTC, with five fields: `minute hour day month weekday`.

- IST is UTC+5:30, so subtract 5h30m from IST to get UTC. 00:00 IST is 18:30 UTC the
  previous day, which gives `30 18 * * *`.
- Scheduled runs can start late, sometimes by 10 to 30 minutes, when GitHub is busy.
  Do not schedule anything that must start at an exact time.
- Scheduled runs use the default branch.
- In a public repository, GitHub disables scheduled workflows after 60 days with no
  repository activity. Push a commit or re-enable the workflow if this happens.

## Running a job manually

1. Open the repository on GitHub, then the Actions tab.
2. Choose the workflow, then Run workflow.
3. Set the inputs. `dry_run` is on by default. Leave `limit` empty for a full run.

Always start with a dry run after a change. A dry run fetches prices from Steam and
logs what it would write, without writing to the database.

## Failure notifications

GitHub's default behaviour is enough for now. When a scheduled run fails, GitHub emails
the account that last changed the workflow file. Failed manual runs show a red status
in the Actions tab. If you want a richer alert later, add a notification step to the
workflow.

## Security rules

- No secrets, passwords, tokens, or real connection strings in the repository, in
  commit messages, in workflow files, or in logs. Use Actions secrets only.
- Do not print environment variables. Code that handles a connection string must
  scrub it from any error text before logging it.
- Do not commit `.env` files. `.gitignore` covers them, but check `git status` before
  each commit.
- Internal-only URLs, hostnames, and IP addresses stay out of the repository too. The
  repository may become public.
- Database access uses the least privilege the job needs. See
  `projects/gamedeal-radar/db-role.sql` for the pattern.
- Respect third-party rate limits. The Steam client never sends more than 1 request
  per second, and it backs off on HTTP 429. Do not bypass these limits.
