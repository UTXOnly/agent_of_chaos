---
name: investigate
description: Investigate the regressions an aoc A/B test found (memory, CPU, latency, delivery, log errors) from results/<name>/findings.md, using the local evidence first and a bounded number of Datadog MCP profiling/metric calls, then record the conclusion with aoc conclude. Use when asked why an agent build regressed or to explain an A/B result.
---

# From findings.md to a conclusion

Input: an experiment's results directory (`results/<name>/`, with
`ab.json` and `findings.md`). Read `findings.md` once, fully. It is built
as: what we tested → what differed (only the rows that moved) → where (one
section per regressed topic: a one-sentence reading, the tables that
usually name the cause, the profile diffs with the source line of each
mover) → profiles → code (the movers located in the agent's source, with
whether their files changed between the builds). Everything else is in
`compare.md`; do not open it unless a number you need is missing.

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

## Into the code

`findings.md` ends with a **Code** section: for every profile mover inside
the agent's module, its `file:line` (from the profile), a link to that line
at the tested commit, and — when a checkout with both commits is available
(`source:` in `aoc.yaml`, `DATADOG_AGENT_SRC`, or `../datadog-agent`) —
whether the file is new, changed (`+N/−M`) or unchanged between the two
builds. Both commits come from the profiler's tags (`ab.json` sides →
`report.json` `agent.commit`).

When a regression is CPU or memory in the core agent and the mover is
`new in b` or changed:

```bash
git -C <source> show <commit-b>:<file> | sed -n '<line-20>,<line+40>p'   # the function
git -C <source> diff <commit-a> <commit-b> -- <file> | head -150           # what changed
```

Read only those windows, not whole files or the whole diff (a dev branch
is often hundreds of commits past the release). Look for the usual
causes: an allocation per message where a slice could be reused or
filtered in place, a regexp or map built per call instead of at setup, a
lock or channel on the hot path, work done for every tag/line that could
be done once per source. Write the suggestion as a reviewer would: the
function, the line, what it does per message, what to do instead, and the
number from the profile that it should move.

## Write it down

```bash
./bin/aoc conclude --results results/<name> --verdict fail "container memory +160 %: a new python3 check runner (350 MB RSS) in the -full image; the logs pipeline itself is unchanged (core RSS, heap in use and CPU by function flat). Tag filters removed 67 % of tag bytes as intended."
```

That writes `conclusion.md`, posts a `source:aoc` event tagged
`experiment:<name>` and, with `DD_APP_KEY`, puts the conclusion on top of
the notebook (without the key: `edit_datadog_notebook` with the notebook
URL from `ab.json`, full-replace with the existing cell ids and a new
markdown cell first). `aoc ab --compare-only` then re-renders
`findings.md` with the conclusion under "What we tested". Report the
conclusion to the user in two or three sentences, with the numbers from
the brief and the links (notebook, any flame graph the MCP returned).
