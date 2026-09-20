---
name: investigate
description: Investigate the regressions an aoc A/B test found (memory, CPU, latency, delivery, log errors) from results/<name>/findings.md, using the local evidence first and a bounded number of Datadog MCP profiling/metric calls, then record the conclusion with aoc conclude. Use when asked why an agent build regressed or to explain an A/B result.
---

# From findings.md to a conclusion

Input: an experiment's results directory (`results/<name>/`, with
`ab.json` and `findings.md`). Read `findings.md` once, fully. It already
contains: the verdict; per regression topic the tables that usually name
the cause; per-function CPU/heap/allocation diffs from the agent's own
profiles; "Next" lines with the exact MCP filters (run tags, time windows).

## Budget

- Local evidence first. Most regressions are explained by one table:
  - **memory**: the "Where the memory is" table. anon ↑ + core RSS flat →
    another process (the process table names it). anon flat + file ↑ →
    page cache, not a leak. core RSS ↑ + Go heap flat → non-Go memory in
    the core agent. core RSS ↑ + heap ↑ → the heap-in-use diff names the
    function.
  - **cpu**: the CPU diff by function; the process table if the core agent
    is not the one that grew.
  - **latency**: pipeline utilization (the component near 1.0), retries,
    then the CPU diff.
  - **delivery**: retries/responses, faults, the agent log's top messages,
    the streams table in `<side>/report.md` (gaps after rotations → tailer).
  - **stability**: the log table; goroutines.
- Datadog MCP: at most 2–3 calls per regression topic, only the ones the
  brief's "Next" lines suggest, with their filters. Load the
  `datadog/profiling` skill first. Prefer `explore_profiling_flame_graph`
  with `frameRegexFilter` set to the function the diff points at over
  unfiltered graphs; `get_profiling_timeseries` grouped by
  `@lastFrame.function` to confirm a trend. Always keep the
  `visualizationLink` the tool returns for the conclusion.
- Single values: `jq` on `report.json` (recipes in
  `.claude/rules/results.md`). Never read `report.json`, `timeseries.csv`
  or pprof files whole.
- Stop when the cause is named at the level of "process X" or "function Y
  in package Z" and the direction of the number is explained. A second run
  (`./bin/aoc ab --runs 2`) beats a fourth flame graph when the numbers are
  within a few percent of the threshold.

## Write it down

```bash
./bin/aoc conclude --results results/<name> --verdict fail "container memory +160 %: a new python3 check runner (350 MB RSS) in the -full image; the logs pipeline itself is unchanged (core RSS, heap in use and CPU by function flat). Tag filters removed 67 % of tag bytes as intended."
```

That writes `conclusion.md` and posts a `source:aoc` event tagged
`experiment:<name>`. If the user wants it in the notebook, add one markdown
cell at the top with the MCP's `edit_datadog_notebook` (URL in `ab.json`).
Report the conclusion to the user in two or three sentences, with the
numbers from the brief, and the links (notebook, any flame graph).
