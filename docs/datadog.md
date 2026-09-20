# What lands in Datadog

The intake submits gauges every `--dd-interval` (10 s) to `/api/v2/series`
and lifecycle events to `/api/v1/events`. Everything carries `run:<name>`
(the intake's `--name`; `aoc run --name`), `agent_image:<image>` when known,
`agent_version:<v>` once the agent has sent a payload, and whatever
`--dd-tags` adds (the compose files add `harness:aoc`; the fleet adds
`experiment:log-tag-filter,variant:<AOC_VARIANT>`).

`aoc ab` adds `experiment:<name>` and `variant:<side>` to every run of a
test — on the intake's `aoc.*` metrics and events, and (through `DD_TAGS`
and `DD_INTERNAL_PROFILING_EXTRA_TAGS`) on the agent's own metrics and
profiles — with `run:<name>-<side>[-<round>]` per run. So
`avg:aoc.agent.process.cpu_percent{experiment:my-feature} by {variant}` is
the two agents on one chart, and the profiler can be filtered to
`service:datadog-agent experiment:my-feature variant:b`.

Rates (`*_per_sec`, `*.rate`) are averages over the submission interval.
Ledger values are cumulative since the last window reset.

## `aoc.gen.*` — what the generators wrote (tag `gen:<name>`)

| metric | meaning |
|---|---|
| `aoc.gen.records_per_sec`, `.lines_per_sec`, `.bytes_per_sec` | write rates |
| `aoc.gen.records`, `.rotations`, `.write_errors` | window totals |
| `aoc.gen.streams`, `.target_rate` | active streams, configured aggregate rate (0 = flat out) |
| `aoc.gen.cpu_percent`, `.rss_bytes` | the generator process |

## `aoc.intake.*` — what the intake received

| metric | meaning |
|---|---|
| `aoc.intake.logs_per_sec`, `.lines_per_sec` | logs accepted; physical lines inside them |
| `aoc.intake.bytes.wire_per_sec`, `.raw_per_sec`, `.message_per_sec` | on the wire, decompressed payload, log text only |
| `aoc.intake.requests_per_sec`, `aoc.intake.responses_per_sec` (`status:202|503|dropped|…`) | HTTP traffic |
| `aoc.intake.compression_ratio`, `.logs_per_payload`, `.payload.wire_bytes` | batch geometry |
| `aoc.intake.latency.e2e.{p50,p90,p99,max,avg}` | written → received, seconds |
| `aoc.intake.latency.sender.{p50,p99,max}` | agent encode → received |
| `aoc.intake.delivery.{generated,received,unique,missing,duplicates,out_of_order,orphans,unmarked,truncated,multiline,multiline_written,ratio}` | the ledger |
| `aoc.intake.tags.per_log`, `.bytes_per_log`, `.unique_keys` | tag cost |
| `aoc.intake.faults.active` (0/1), `.dropped_per_sec`, `.errored_per_sec`, `.delayed_per_sec` | injected faults |
| `aoc.intake.orphans_per_sec`, `.unmarked_per_sec`, `.truncated_per_sec`, `.inflight`, `.tcp_frames_per_sec` | the rest |
| `aoc.stream.missing`, `aoc.stream.duplicates` (`gen:`, `stream:`) | only for streams with a problem, at most 300 |

## `aoc.agent.*` — the agent under test

| metric | source |
|---|---|
| `aoc.agent.container.cpu_percent`, `.memory_bytes`, `.pids` | Docker stats stream for `--docker-container` (memory = usage − inactive file cache) |
| `aoc.agent.container.memory_anon_bytes`, `.memory_file_bytes` | the cgroup split of that memory: the processes' own pages vs page cache charged to the container |
| `aoc.agent.proc.rss_bytes`, `.cpu_percent` (`proc:<command>`) | every process in the container, from `docker top` every 5 s, summed by command name (`agent`, `trace-agent`, `process-agent`, `python3`…) |
| `aoc.agent.process.cpu_percent`, `.rss_bytes` | `process_cpu_seconds_total` / `process_resident_memory_bytes` from the agent's `/telemetry` |
| `aoc.agent.telemetry.<name>` | gauges from `/telemetry`, e.g. `aoc.agent.telemetry.logs_component_utilization.ratio{name:…}` |
| `aoc.agent.telemetry.<name>.rate` | counters from `/telemetry` as per-second rates, e.g. `aoc.agent.telemetry.logs.bytes_sent.rate`, `…logs.destination_http_resp.rate{status_code:…}`, `…logs.retry_count.rate`, `…logs.rotations_nix.rate`, `…logs.sender_latency_sum.rate` / `_count.rate` |

Names map `subsystem__name` → `subsystem.name`; labels become tags.
Histogram buckets are not forwarded. Which series are kept is
`--telemetry-filter` (default: everything under `logs`, the `process_*`
series and a few `go_*`).

Besides these, the agent ships its usual metrics with the `run:` host tag
from `DD_TAGS`: `docker.cpu.usage{container_name:aoc-agent}`,
`docker.mem.rss`, `system.*`, `datadog.agent.*`.

## Profiles

With `DD_INTERNAL_PROFILING_ENABLED=true` (the compose default) the core
agent profiles itself and uploads CPU and delta-heap profiles (plus
goroutine/block/mutex when enabled) under `service:datadog-agent`, tagged
with `DD_INTERNAL_PROFILING_EXTRA_TAGS` (`run:<name>`, and `experiment:` /
`variant:` under `aoc ab`). Cadence is `DD_INTERNAL_PROFILING_PERIOD`
(agent default 5 m; compose default 60 s) with
`DD_INTERNAL_PROFILING_CPU_DURATION` of CPU sampling per period. Uploads go
through the trace-agent on `localhost:8126`, so `DD_APM_ENABLED` must be
true; the period in progress when the agent stops is lost.

The trace-agent's profiling proxy is pointed at the intake
(`DD_APM_PROFILING_DD_URL=http://localhost:8282/api/v2/profile`): the intake
keeps a copy of every upload and forwards the request to
`intake.profile.<site>` unchanged (`--profile-forward auto`; `off` keeps
them local only), so Datadog has exactly what it would have had. After the
drain `aoc run` downloads the uploads that overlap the window into
`<results>/profiles/<service>/<start>/` (`event.json`, `cpu.pprof`,
`delta-heap.pprof`, …), reduces them to per-function tables in
`report.json` (`profiles[]`), and `aoc ab` diffs the two sides function by
function in `findings.md`. `/harness/status` reports the tee
(`profiles.received`, `forwarded`, `forward_errors`).

Query with the MCP's `explore_profiling_flame_graph`
(`service:datadog-agent run:<name>`, `frameRegexFilter` set to the function
the local diff points at) or `get_profiling_timeseries` grouped by
`@lastFrame.function`, over the run's window (`ab.json` has it).

## Events (`source:aoc`)

`aoc: intake started`, `aoc: generator <gen> started|finished`,
`aoc: measurement window opened`, `aoc: faults set|clear`, and whatever
`POST /harness/mark?text=…` posts (`aoc run` marks window closed, generators
stopped, drain finished). Query them with `source:aoc run:<name>`. A finished
A/B test posts `aoc: A/B <name> finished — a vs b` with the verdict and the
regressions as Markdown (`experiment:<name>`), and `aoc conclude` posts
`aoc: A/B <name> conclusion` next to it.

## Notebooks

`aoc notebook --results <dir>` builds a *report* notebook whose global time
is the run window ±60 s. Cells: findings summary; generated vs received;
delivery ledger; latency; bytes; responses by status; agent CPU; agent
memory; agent bytes sent; destination responses by status; pipeline
utilization; retries/network errors; tags per log; faults; per generator;
dig-deeper links. With several `--results`, the comparison table comes first
and each run's key cells are pinned to that run's window.

`aoc ab` builds an *A/B* notebook instead, in the order an investigation
reads it: what was tested and what differed (only the rows that moved);
the conclusion once `aoc conclude` has written one; one cell per topic
that regressed with its reading and evidence (memory by process and
cgroup, the profile movers with their source lines, telemetry, log); the
profiles and the code behind the movers; each side's profiler embedded as
a same-origin iframe of the flame graph explorer scoped to the run
(notebooks have no native profiling widget); a real-time timeline of the
session (`{experiment:<name>} by {variant}`); then only the charts that
bear on what moved, all runs overlaid — the cells are pinned to the latest
run's window and each earlier run's queries are wrapped in
`timeshift(…, -<seconds between the runs>)` so the same second of the
measured window lines up; grouped queries get `run` added to their
group-by so the legend still tells the sides apart. Every metric stays in
`compare.md`.

Creating the notebook needs `DD_APP_KEY`; `notebook.json` is always written
and can be fed to the Datadog MCP's `create_datadog_notebook`.

## Turning it off

`--dd-metrics=false` (env `AOC_DD_METRICS=false`), or the
`docker-compose.offline.yml` overlay, which also points the agent's metrics
at the intake's 202 sink so nothing leaves the host.
