@AGENTS.md

## Claude Code specifics

- You lead the session: after every run, report what moved and propose
  the next run. The `aoc` skill is the procedure (goal → plan → run →
  read → decide → conclude); it delegates single runs to `experiment`
  (one agent in a situation, or a sweep) and `ab-test` (two builds, or a
  flag on one), and finished results to `investigate` (from `findings.md`
  to a conclusion with a bounded number of Datadog MCP calls). Use them
  for those tasks; `docs/sessions.md` shows what a good session looks
  like.
- Datadog MCP: load the `datadog/profiling` skill before profiling calls,
  and `datadog/metrics` before metric queries when those skills are available.
  Surface the `visualizationLink` the profiling tools return instead of building URLs.
- Every run goes through the UI so the engineer can watch:
  `ui/bin/aoc-ui run -- ab|run <flags> --name <name> > results/<name>.log 2>&1`
  in the background; wait for the exit (the task notification), never a
  `sleep` loop. The wrapper starts the server, prints the live URL and each
  phase, and exits with aoc's code. One run at a time. `--plan` runs
  directly. Hooks in `.claude/settings.json` enforce this, refuse a second
  concurrent run and whole-file reads of heavy artifacts, and ask before
  edits to `aoc.yaml` or an existing profile.
- The session starts with a briefing from `.claude/hooks/session-context.sh`
  (host, UI, active run, agent checkout, recent results); the `status` skill
  reruns it, `stop` ends a run cleanly.
- Delegate to keep the loop's context small: the `investigator` agent for a
  failed gate or a regressed signal (it returns the conclusion text), the
  `code-reader` agent for a mover's diff, the `notebook-author` agent to
  publish through Datadog MCP after `aoc conclude`.
- Token discipline: `findings.md` first, `compare.md` second, `jq` on
  `findings.json` (`.signals`, `.gate`) or `report.json` for single
  values, `grep` for logs. Do not read `report.json`, `timeseries.csv`,
  the whole `notebook.json` or pprof files into context.
- Notebook analysis: follow [docs/notebooks.md](docs/notebooks.md). Read the
  compact `notebook.md`, select cells from `notebook.json` with `jq`, write the
  conclusion yourself and publish/update through Datadog MCP. Record the returned
  URL with `aoc notebook --results <dir> --link URL`. Missing MCP never blocks the
  local answer. Preserve the measured gate and label causal hypotheses.
