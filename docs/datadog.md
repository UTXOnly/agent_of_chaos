# What lands in Datadog

The intake submits gauges every `--dd-interval` (10 s) to `/api/v2/series`
and lifecycle events to `/api/v1/events`. Everything carries `run:<name>`
(the intake's `--name`; `aoc run --name`), `agent_image:<image>` when known,
`agent_version:<v>` once the agent has sent a payload, and whatever
`--dd-tags` adds (the compose files add `harness:aoc`; the fleet adds
`experiment:log-tag-filter,variant:<AOC_VARIANT>`).

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
| `aoc.agent.container.cpu_percent`, `.memory_bytes`, `.pids` | Docker stats stream for `--docker-container` |
| `aoc.agent.process.cpu_percent`, `.rss_bytes` | `process_cpu_seconds_total` / `process_resident_memory_bytes` from the agent's `/telemetry` |
| `aoc.agent.telemetry.<name>` | gauges from `/telemetry`, e.g. `aoc.agent.telemetry.logs_component_utilization.ratio{name:…}` |
| `aoc.agent.telemetry.<name>.rate` | counters from `/telemetry` as per-second rates, e.g. `aoc.agent.telemetry.logs.bytes_sent.rate`, `…logs.destination_http_resp.rate{status_code:…}`, `…logs.retry_count.rate`, `…logs.rotations_nix.rate`, `…logs.sender_latency_sum.rate` / `_count.rate` |

Names map `subsystem__name` → `subsystem.name`; labels become tags.
Histogram buckets are not forwarded. Which series are kept is
`--telemetry-filter` (default: everything under `logs`, the `process_*`
series and a few `go_*`).

Besides these, the agent ships its usual metrics with the `run:` host tag
from `DD_TAGS`: `docker.cpu.usage{container_name:aoc-agent}`,
`docker.mem.rss`, `system.*`, `datadog.agent.*` — and, with
`DD_INTERNAL_PROFILING_ENABLED=true`, CPU/heap profiles under
`service:datadog-agent`.

## Events (`source:aoc`)

`aoc: intake started`, `aoc: generator <gen> started|finished`,
`aoc: measurement window opened`, `aoc: faults set|clear`, and whatever
`POST /harness/mark?text=…` posts (`aoc run` marks window closed, generators
stopped, drain finished). Query them with `source:aoc run:<name>`.

## Notebooks

`aoc notebook --results <dir>` builds a *report* notebook whose global time
is the run window ±60 s. Cells: findings summary; generated vs received;
delivery ledger; latency; bytes; responses by status; agent CPU; agent
memory; agent bytes sent; destination responses by status; pipeline
utilization; retries/network errors; tags per log; faults; per generator;
dig-deeper links. With several `--results`, the comparison table comes first
and each run's key cells are pinned to that run's window.

Creating the notebook needs `DD_APP_KEY`; `notebook.json` is always written
and can be fed to the Datadog MCP's `create_datadog_notebook`.

## Turning it off

`--dd-metrics=false` (env `AOC_DD_METRICS=false`), or the
`docker-compose.offline.yml` overlay, which also points the agent's metrics
at the intake's 202 sink so nothing leaves the host.
