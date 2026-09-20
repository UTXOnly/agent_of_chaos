---
paths:
  - "results/**"
---

# Reading results without reading everything

A results directory is large; the brief is small. Order of preference:

1. `results/<name>/findings.md` — the whole story (what was tested, what
   differed, where, profiles, code, conclusion). Read it fully, once.
2. `results/<name>/compare.md` — every metric, the per-process table, the
   telemetry counters.
3. `results/<name>/ab.json` — identities, windows, notebook URL, file paths.
4. Single values from a run's `report.json` with `jq`, never `cat`:
   - `jq '.resources' <side>/report.json` — CPU/memory incl. `processes[]`,
     `container_anon_max_bytes`, `container_file_max_bytes`
   - `jq '.delivery' <side>/report.json`
   - `jq '.profiles[] | {service,label,total,captures}' <side>/report.json`
   - `jq '.profiles[] | select(.label=="heap in use") | .top[:10]' <side>/report.json`
   - `jq '.telemetry[] | select(.name|startswith("logs__retry"))' <side>/report.json`
   - `jq '.agent_log' <side>/report.json`
   - `jq '.streams[] | select(.missing>0) | {gen,stream,missing,gaps}' <side>/report.json`
5. The agent log with `grep` (`grep -c '| ERROR |'`, `grep '| ERROR |' | sort | uniq -c | sort -rn | head`), never whole.

Never open `timeseries.csv`, `notebook.json`, `*.pprof`, `intake.log` or
`gen-*.log` into context; ask the Datadog notebook or the MCP for charts.
`results/` is gitignored — nothing here is ever committed.
