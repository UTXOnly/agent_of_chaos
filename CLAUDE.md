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
- Long runs: start `./bin/aoc ab …` or `./bin/aoc run …` in the background
  with its output in a file and wait for the exit; a Monitor on the log is
  fine, a `sleep` loop is not. One run at a time.
- Token discipline: `findings.md` first, `compare.md` second, `jq` on
  `findings.json` (`.signals`, `.gate`) or `report.json` for single
  values, `grep` for logs. Do not read `report.json`, `timeseries.csv`,
  the whole `notebook.json` or pprof files into context.
- Notebook analysis: follow [docs/notebooks.md](docs/notebooks.md). Read the
  compact `notebook.md`, select cells from `notebook.json` with `jq`, write the
  conclusion yourself and publish/update through Datadog MCP. Record the returned
  URL with `aoc notebook --results <dir> --link URL`. Missing MCP never blocks the
  local answer. Preserve the measured gate and label causal hypotheses.
