# Project template

Copy this folder to `projects/<project-name>/` to start a new project. It is a
minimal Go module with a stub job, a workflow template, and a place for docs.

## Contents

- `go.mod`: its own module. Do not share code with other projects.
- `cmd/job/main.go`: stub entry point. Replace it.
- `workflow.yml.example`: template for `.github/workflows/<project-name>-<job>.yml`.
- `README.md`: this file. Replace it with the project's own docs.

## Steps

1. `cp -r projects/_template projects/<project-name>`
2. Rename the module in `go.mod` and change `cmd/job` to what the job needs.
3. Copy `workflow.yml.example` to `.github/workflows/<project-name>-<job>.yml`, then
   set the cron, the working directory and the secret names. Secrets are named
   `PROJECT_NAME_SECRET`, for example `MYPROJ_API_TOKEN`.
4. Write this README: what the job does, its exit codes, its env vars and its schedule.
5. Run `go vet ./... && go test ./...` inside the project folder, then open a PR.

See the root README for the full checklist and the rules about stacking.
