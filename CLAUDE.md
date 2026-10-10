@AGENTS.md

## Claude Code specifics

- Skills: `experiment` (a situation in plain words → `aoc run` with the
  right fault and workload, then what the agent lost or slowed down),
  `ab-test` (what the engineer changed → one `aoc ab` line, then the
  verdict) and `investigate` (from `findings.md` to a conclusion, with a
  bounded number of Datadog MCP calls).
- Datadog MCP: load the `datadog/profiling` skill before profiling calls,
  and `datadog/metrics` before metric queries. Surface the
  `visualizationLink` the profiling tools return instead of building URLs.
- Long runs: start `./bin/aoc run|ab …` in the background with its output
  in a file and wait for the exit; a Monitor on the log is fine, a `sleep`
  loop is not.
- Token discipline: `findings.md` first, `compare.md` second, `jq` on
  `findings.json` (`.signals`, `.gate`) or `report.json` for single
  values, `grep` for logs. Do not read `report.json`, `timeseries.csv`,
  `notebook.json` or pprof files into context.
