# automation-jobs

Scheduled jobs for my projects, run on GitHub Actions instead of always-on servers.

Each project is self-contained. It lives in its own folder under `projects/<name>/`,
with its own `go.mod`, README, and docs. Its workflow lives in `.github/workflows/`
with a `<name>-` prefix, because GitHub only reads workflows from that folder. Projects
share no code. Per-project secrets are prefixed with the project's name. The one
exception is `SLACK_WEBHOOK_URL`, which every project shares because all of them post to
one Slack channel.

## Projects

| Project | What it does | Schedule (UTC) | Docs |
| --- | --- | --- | --- |
| [gamedeal-radar](projects/gamedeal-radar/) | Daily Steam India price check. Two slots a day, six attempts each. | 06:30 and 18:30, with retries until 11:30 and 23:30 | [README](projects/gamedeal-radar/README.md) |
| [_template](projects/_template/) | Starter for a new project. Not a real job. | none | [README](projects/_template/README.md) |

## Add a new project in 5 steps

1. Copy the template: `cp -r projects/_template projects/<project-name>`. Use lowercase
   letters and hyphens for the name.
2. Edit `projects/<project-name>/go.mod` and `cmd/job/main.go`. Keep everything inside
   the folder. Do not import code from another project.
3. Copy `projects/<project-name>/workflow.yml.example` to
   `.github/workflows/<project-name>-<job>.yml`. Set the cron (UTC), the working
   directory, and the secret names. Keep `concurrency` with `cancel-in-progress: false`
   and `timeout-minutes` at or below what the job needs.
4. Write `projects/<project-name>/README.md`, covering what it does, its exit codes,
   its env vars, and its schedule. Add a `db-role.sql` if it uses Postgres. Add a row to
   the table above.
5. Verify locally with `go vet ./... && go test ./...`, then open a PR. Add the secrets
   under the repository's Settings, then Secrets and variables, then Actions. Trigger a
   `workflow_dispatch` run first, in dry-run mode if the job supports it.

## Secrets

Per-project secrets are named `PROJECT_NAME_SECRET` in upper case. Shared secrets have
no prefix. For example:

| Secret | Scope | Purpose |
| --- | --- | --- |
| `SLACK_WEBHOOK_URL` | shared by all projects, one channel | Optional Slack Incoming Webhook. Every project tags its messages with its own name, for example `[GameDeal Radar]`, so the channel stays readable. |
| `GDR_DATABASE_URL` | GameDeal Radar | The GameDeal Radar database connection |
| `GDR_HEALTHCHECK_URL` and `GDR_HEALTHCHECK_FINAL_URL` | GameDeal Radar | Healthcheck URLs |
| `GDR_SLACK_WEBHOOK_URL` | GameDeal Radar | Optional fallback for the Slack webhook. Read only when `SLACK_WEBHOOK_URL` is empty. Can be removed once `SLACK_WEBHOOK_URL` is set. |
| `MYPROJ_API_TOKEN` | placeholder for another project | Example of a project-prefixed secret |

Give each project its own database role and credentials. Never share a super-user
connection string.

## Why jobs cannot stack up

GitHub's `concurrency` group `gamedeal-radar-daily` is set with
`cancel-in-progress: false`. That gives two guarantees:

- At most one run is active at a time. Its own attempts never overlap.
- At most one run waits behind it. GitHub replaces any further pending run, so a
  backlog never builds up.

The schedule leaves a gap of about 7 hours after the last attempt of each slot. The
program also ignores any run that starts more than 6 hours after its slot start, and it
never reuses a past slot's leftovers. Every slot starts fresh: only the games not yet
checked in that slot are fetched. A late or stuck run therefore cannot touch the next
slot's data.

## Cron and UTC

GitHub cron is always UTC with five fields: `minute hour day month weekday`.

- IST is UTC+5:30. Subtract 5h30m from IST to get UTC. For example, 12:00 IST is 06:30
  UTC.
- GitHub can start scheduled runs 5 to 30 minutes late under load. Do not depend on an
  exact start time. The programs allow a 20 minute tolerance when mapping a run to an
  attempt.
- In a public repository, GitHub disables scheduled workflows after 60 days with no
  repository activity.
- Scheduled runs use the default branch.

## Manual runs

1. Open the repository's Actions tab, choose the workflow, then choose Run workflow.
2. Set the inputs. `dry_run` defaults to true. Leave `limit` empty for a full run.

Manual runs skip the slot window check and are not counted as scheduled attempts, so
they never open a failure issue. Start with a dry run after any change.

## Failure notifications

- Any run that ends below the coverage threshold fails. GitHub's default failed-run email
  covers this. The healthcheck is also pinged with `/fail`, if one is set up.
- The sixth and final attempt of a slot also opens a GitHub issue labelled `job-failure`
  (see the project README), and pings the optional final healthcheck.
- When a later slot completes, the open `job-failure` issues are closed with a comment.

## GitHub activity notifications

`.github/workflows/slack-notify.yml` posts to the shared Slack channel when something
happens on GitHub. It covers pushes to `main`, pull requests (opened, reopened, ready for
review, merged, closed), issues (opened, closed, reopened), new comments on issues and
pull requests (bots are skipped), published releases, and failed runs of the GameDeal
Radar job. Every message starts with `[Automation Jobs]`. The workflow needs the
`SLACK_WEBHOOK_URL` secret in each repository that uses it, including this one. Without
the secret, the job logs "Slack webhook not configured, skipping" and passes. Set it with
`gh secret set SLACK_WEBHOOK_URL`.

## Security rules

- No secrets, passwords, tokens, or real connection strings in the repository, commit
  messages, workflow files, or logs. Use Actions secrets only.
- Do not print environment variables. Code that handles connection strings or healthcheck
  URLs must scrub them from error text.
- Do not commit `.env` files. `.gitignore` covers them, but check `git status` before each
  commit.
- No internal-only URLs, hostnames, or IP addresses in the repository. It may become
  public.
- Use the least database privilege the job needs. See
  `projects/gamedeal-radar/db-role.sql` for the pattern.
- Respect third-party rate limits. Do not bypass them.
