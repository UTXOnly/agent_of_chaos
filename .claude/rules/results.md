---
paths:
  - "results/**"
---

# Reading results without reading everything

A results directory is large; the brief is small. A single `aoc run` has
no brief: read its `report.md` (delivery first) and use the `jq` lines in
5. For an `aoc ab`, in order of preference:

1. `results/<name>/findings.md` — the whole story (the verdict, the
   signals table, the gate, what was tested, a section per regressed
   signal, profiles, code, conclusion). Read it fully, once.
2. `results/<name>/findings.json` for a signal on its own, never the
   whole file:
   - `jq '.signals' results/<name>/findings.json` — throughput,
     saturation, cpu, memory: metric, a, b, Δ, the threshold each was
     judged against, and whether it regressed
   - `jq '.gate' results/<name>/findings.json` — delivery: pass or fail
     and the counts that decided it
   - `jq -r '.focus' results/<name>/findings.json` — the question the test
     was launched with
3. `results/<name>/compare.md` — every metric, including the ones that are
   no longer signals (latency, tags, compression, wire bytes, goroutines),
   the per-process table, the telemetry counters.
4. `results/<name>/ab.json` — identities, windows, focus, notebook URL
   (`jq -r '.notebook'`), file paths.
5. Single values from a run's `report.json` with `jq`, never `cat`:
   - `jq '.resources' <side>/report.json` — CPU/memory incl. `processes[]`,
     `container_anon_max_bytes`, `container_file_max_bytes`
   - `jq '.delivery' <side>/report.json`
   - `jq '.profiles[] | {service,label,total,captures}' <side>/report.json`
   - `jq '.profiles[] | select(.label=="heap in use") | .top[:10]' <side>/report.json`
   - `jq '.telemetry[] | select(.name|startswith("logs__retry"))' <side>/report.json`
   - `jq '.telemetry[] | select(.name=="logs_component_utilization__ratio")' <side>/report.json`
   - `jq '.agent_log' <side>/report.json`
   - `jq '.streams[] | select(.missing>0) | {gen,stream,missing,gaps}' <side>/report.json`
6. The agent log with `grep` (`grep -c '| ERROR |'`, `grep '| ERROR |' | sort | uniq -c | sort -rn | head`), never whole.

Never open `timeseries.csv`, `notebook.json`, `*.pprof`, `intake.log` or
`gen-*.log` into context; ask the Datadog notebook or the MCP for charts.
`results/` is gitignored — nothing here is ever committed.
