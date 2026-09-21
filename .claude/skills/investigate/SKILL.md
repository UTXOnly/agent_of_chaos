---
name: investigate
description: Investigate the regressions an aoc A/B test found (the delivery gate, throughput, saturation, CPU, memory) from results/<name>/findings.md, using the local evidence first and a bounded number of Datadog MCP profiling/metric calls, then record the conclusion with aoc conclude. Use when asked why an agent build regressed or to explain an A/B result.
---

# From findings.md to a conclusion

Input: an experiment's results directory, `results/<name>/`. Read
`findings.md` once, fully, in its own order: the verdict line → the
signals table (signal, metric, a, b, Δ, threshold) → the gate line → what
we tested → one section per signal that regressed (a sentence, one or two
tables, a **Next** line) → the profiles → the code. Everything else is in
`compare.md`; open it only for a number the brief does not carry.

## Budget

Local evidence first — one table usually names the cause. Then at most two
Datadog MCP calls per regressed signal, and only the call that signal's
**Next** line gives, with its filters: load the `datadog/profiling` skill
before profiling calls and `datadog/metrics` before metric queries, and
keep the `visualizationLink` the tool returns for the conclusion. Single
values with `jq` (recipes in `.claude/rules/results.md`); never read
`report.json`, `timeseries.csv` or a pprof file whole.

A metric on the headline because of `--watch` is context, not a signal:
read it off `compare.md` and the section it sits under, and spend no MCP
call on it.

Stop when the cause is named at the level of "process X" or "function Y in
package Z" and the direction of the number is explained. Another round
(`./bin/aoc ab … --runs 3`) beats a fourth flame graph when a Δ is within
a point of its threshold.

## The gate

**delivery** — records lost, duplicated, orphaned or truncated on b fail
the run (exit 3); out of order is reported and does not fail. Read the
delivery ledger, then retries/errors/responses and the faults note. Gaps
right after a rotation are the tailer: the streams table in
`<side>/report.md`, or
`jq '.streams[] | select(.missing>0) | {gen,stream,missing,gaps}' <side>/report.json`.
The agent log's top messages usually name the destination error. One MCP
call: `search_datadog_events` query="source:aoc run:<b run>" to place the
injected faults against the loss.

## The four signals

**throughput** — `received logs /s`. Under a paced workload the generator
holds the rate, so this fails only when b fell short of what was
generated: check the gate and saturation first, they normally explain it.
Under `deterministic-bytes` (flat out, fixed record budget) it is the
agent's maximum, and cpu or saturation says why. MCP:
`get_datadog_metric` `avg:aoc.intake.logs_per_sec{experiment:<name>} by
{variant}` beside `avg:aoc.gen.records_per_sec{experiment:<name>} by
{variant}` — a flat generator line under a lower intake line is real.

**saturation** — `pipeline utilization (busiest component)`, the max over
the agent's `logs_component_utilization` ratio per component. The
per-component table is the evidence: the component near 1.0 is the
bottleneck, and its package is where to read the CPU diff. MCP:
`get_datadog_metric`
`avg:aoc.agent.telemetry.logs_component_utilization.ratio{experiment:<name>} by {variant,name}`.

**cpu** — `core agent process CPU avg`, `CPU seconds per 1M logs`. The CPU
diff by function names the mover; the process table takes over when the
core agent is not the one that grew (a `-full` image's python3 check
runner, the trace-agent). Seconds per 1M logs up while CPU avg is flat
means the same work cost more per log, not that there was more of it. MCP:
`explore_profiling_flame_graph` profileType=cpu-time with the brief's
queryString and window and `frameRegexFilter` on the mover, then the same
for a.

**memory** — `core agent RSS max`, `agent container anon mem avg`,
`core agent Go heap in use`. The "Where the memory is" table settles it:

- anon ↑, core RSS flat → another process; the process table names it.
- anon flat, file ↑ → page cache, not a leak.
- core RSS ↑, heap flat → non-Go memory in the core agent.
- core RSS ↑, heap ↑ → the heap-in-use diff names the function.

MCP: `explore_profiling_flame_graph` profileType=heap-live-size with that
function as `frameRegexFilter`, then the same for a; or
`get_datadog_metric` `avg:aoc.agent.proc.rss_bytes{experiment:<name>} by
{variant,proc}` — a ramp is a leak, a step is a cache.

## Into the code

`findings.md` ends with a **Code** table: for every profile mover inside
the agent's module — the packages under test (`--code`) first — its
`file:line` from the profile, a link to that line at the tested commit,
and, when a checkout with both commits is available (`source:` in
`aoc.yaml`, `DATADOG_AGENT_SRC`, or `../datadog-agent`), whether the file
is new, changed (`+N/−M`) or unchanged between the builds. Both commits
come from the profiler's tags (`ab.json` sides → `report.json`
`agent.commit`).

When a cpu or memory regression's mover is `new in b` or changed:

```bash
git -C <source> show <commit-b>:<file> | sed -n '<line-20>,<line+40>p'   # the function
git -C <source> diff <commit-a> <commit-b> -- <file> | head -150         # what changed
```

Read only those windows, not whole files or the whole diff (a dev branch
is often hundreds of commits past the release). Look for the usual causes:
an allocation per message where a slice could be reused or filtered in
place, a regexp or map built per call instead of at setup, a lock or
channel on the hot path, work done for every tag or line that could be
done once per source. Write the suggestion as a reviewer would: the
function, the line, what it does per message, what to do instead, and the
number from the profile that it should move.

## Write it down

```bash
./bin/aoc conclude --results results/<name> --verdict fail "memory +160 %: a new python3 check runner (350 MB RSS) in the -full image; the logs pipeline itself is unchanged (core RSS, heap in use and CPU by function flat)."
```

That writes `conclusion.md`, posts a `source:aoc` event tagged
`experiment:<name>` and, with `DD_APP_KEY`, puts the conclusion on top of
the notebook (without the key: `edit_datadog_notebook` with the notebook
URL from `ab.json`, full-replace with the existing cell ids and a new
markdown cell first). `aoc ab --compare-only` then re-renders
`findings.md` with the conclusion under "What we tested". Report the
conclusion to the user in two or three sentences, with the numbers from
the brief and the links (notebook, any flame graph the MCP returned).
