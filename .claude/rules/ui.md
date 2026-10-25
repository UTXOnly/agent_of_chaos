---
paths:
  - "ui/**"
---

# The web UI is the window, not the record

- The UI composes a run and watches it; analysis lives in Datadog and in
  `results/`. Never add a results dashboard, a comparison view or a
  conclusion editor here. A chart belongs in the live view only if it is a
  metric the intake already serves and helps someone watching a run.
- Agents launch runs through it so the engineer can watch: `ui/bin/aoc-ui
  run -- <aoc args>` posts `{argv}` to `POST /api/runs`, long-polls
  `GET /api/runs/:id/wait`, and exits with aoc's code. Keep that contract
  stable: `argv` is run as given, the record's `request` is only what the
  Live view renders (`parseArgv` in `ui/server/repo.ts`), and the name must
  be known up front because `results/<name>` is where the brief lands.
- One aoc process at a time (`Runner`); a second launch queues. Stop is
  SIGINT first so aoc takes its compose stack down.
- Phases come from the lines `internal/cli/run.go` and `ab.go` print
  (`classify` in `runner.ts`). Changing a CLI log line means checking those
  regexes.
- The server binds to 127.0.0.1 and never reads `DD_API_KEY`'s value, only
  whether it is set.
- After a server change: `pnpm typecheck`, `pnpm build`, restart with
  `ui/bin/aoc-ui restart`, and `ui/bin/aoc-ui run -- run --profile baseline
  --duration 1m --name ui-smoke --plan` as the no-Docker smoke.
