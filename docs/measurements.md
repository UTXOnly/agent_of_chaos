# Measurements

What every number in `report.md` / `report.json` (and its `aoc.*` metric in
Datadog, see [datadog.md](datadog.md)) means, how it is computed, and where it
can mislead you.

## The marker

Every record the generator writes carries `aoc=<gen>/<stream>/<seq>` (plain
text, right after the logger name) or `"aoc":"<gen>/<stream>/<seq>"` (JSON,
near the start of the object). `gen` is the generator's `--name` (the container
id under `--scale`), `stream` the service name / file, `seq` a per-stream
counter starting at 1 that survives chaos restarts. Continuation lines of a
stack trace have no marker: they belong to the record above them.

## Delivery ledger

| field | definition |
|---|---|
| **records written** | Σ generators' `records` (each pushes its counters every second; ≤ 1 s stale). If the intake has already seen a higher seq for a stream, that seq is used — a marker received proves the record exists. |
| **logs received** | every log the intake accepted with 202, including logs from other sources. |
| **with a marker / unique** | marked logs, and distinct `(gen, stream, seq)` triples. |
| **duplicates** | a seq seen again. The usual cause: the agent did not get the intake's response (timeouts, `drop_rate`) and re-sent. |
| **out of order** | seq lower than the previous seq from the same stream. Concurrent senders reorder batches; the agent does this too. |
| **missing / lost** | `generated − unique` per stream. While generators run this includes in-flight logs, so the report labels it *missing (still running)*; once every generator has sent its final report it is *lost*. `aoc run` waits for the drain before reporting. |
| **gaps** | the first missing seq ranges per stream (`report?gaps=1`, `report.md`). Ranges that start right after a rotation boundary point at a tailer rotation race. |
| **orphan continuation lines** | unmarked logs that look like stack-trace lines (indented, `at …`, `Caused by:`, `Traceback`, `panic:`, `FooError:`…): multiline aggregation split a trace. |
| **unmarked** | everything else without a marker: the agent's own container logs, other containers, the generators' stderr status lines. |
| **multiline logs received / traces written** | marked logs whose message contains line breaks (the agent joins aggregated lines with a literal `\n`) vs plain-text records the generator wrote with a trace. JSON records keep the trace inside one line and are not counted. |
| **physical lines written / inside received logs** | the generator's newline count vs 1 + line breaks per marked log. Equal when every trace was aggregated and nothing was lost. |
| **truncated** | messages ending in `...TRUNCATED...` (the agent's `max_message_size_bytes`). |

Records with a seq at or below the generator's seq at the last **reset** are
"pre-window" and excluded from every counter, on both sides, so a reset mid-run
is exact.

## Latency

* **written → intake (end to end)**: intake receive time minus the timestamp
  inside the line (the generator's write time). Requires wall-clock timestamps,
  so deterministic runs report n/a. Clocks: generator and intake are usually the
  same host; across hosts, keep NTP in mind.
* **agent encode → intake (sender)**: receive time minus the payload's
  `timestamp` field, which the agent sets when it encodes the batch. Roughly
  batching wait + send + retries.
* **intake processing**: the handler's own time, a sanity check that the intake
  is not the bottleneck.

Quantiles come from a log-scale histogram (±2 %). The per-second timeseries
keeps p50/p99/max per second; the window quantiles are exact over all samples.

## Throughput

Window averages: totals ÷ window length. Peaks are the busiest single second.
**Compression ratio** is decompressed payload bytes ÷ bytes on the wire (the
whole JSON payload, tags included). "raw" bytes are payload bytes after
decompression; message bytes (just the log text) are in the streams table.

## Agent resources

* **container CPU / memory**: Docker stats stream for `--docker-container`,
  once per second (CPU = Δcontainer / Δsystem × online CPUs; memory = usage −
  inactive file cache). Covers every process in the container (core agent,
  trace-agent, process-agent, security-agent…).
* **container anon / file**: the cgroup's split of that memory (`anon` and
  `file` on cgroup v2, `rss`/`cache` on v1). *anon* is the processes' own
  pages — the number that grows when something leaks; *file* is page cache
  charged to the container (files the agent read first, e.g. Python
  integrations), which the kernel reclaims under pressure. A container
  memory increase that is all *file* is not a leak.
* **processes** (`docker top` every 5 s, summed by command name): RSS
  avg/max and CPU avg/max per process (`agent`, `trace-agent`,
  `process-agent`, `system-probe`, `python3`…). This is what attributes a
  container-level change to a process. ps reports CPU time in whole
  seconds, so the per-interval percentage (the `aoc.agent.proc.cpu_percent`
  gauge, and *max*) is coarse; the window *avg* is Δcputime over the whole
  window and is exact to a second. RSS double-counts shared pages between
  processes, so the sum can exceed *anon*.
* **core agent process**: `process_cpu_seconds_total` (rate → %) and
  `process_resident_memory_bytes` from the agent's `/telemetry` endpoint. The
  logs pipeline lives in this process.
* **CPU seconds per 1M logs**: Δ`process_cpu_seconds_total` over the window ÷
  (logs received / 1e6). The single best cost number to compare builds with.
* generators' and the intake's own CPU are reported so you can tell when the
  machine, not the agent, is the limit.

## Agent profiles

The agent's continuous-profiler uploads (one per `DD_INTERNAL_PROFILING_PERIOD`,
60 s in compose) are kept by the intake and, after the run, the ones that
overlap the window are merged and reduced by function (`report.json`
`profiles[]`, `<results>/profiles/` for the pprof files):

| view | file · sample type | value |
|---|---|---|
| **CPU** | `cpu.pprof` · `cpu` | nanoseconds of CPU per second of profile, as % of one core; flat = leaf frame, cum = anywhere on the stack |
| **heap in use** | `delta-heap.pprof` · `inuse_space` | bytes live at the end of each period, averaged over the periods |
| **allocation rate** | `delta-heap.pprof` · `alloc_space` | bytes allocated per wall-clock second |
| goroutines, blocking, mutex wait | when the agent is configured to upload them | count (avg); goroutine-seconds per second |

Where it misleads: the profiler samples the core agent only (the other
agents need their own `*_INTERNAL_PROFILING_ENABLED`); a window shorter
than a period yields nothing; the period in progress when the agent stops
is lost; inlined frames are attributed to the innermost function; merging
across periods averages out short spikes (the notebook's charts show them).
`aoc ab` diffs the two sides' tables function by function (largest movers
first); a function present on one side only shows `0` on the other.

## Agent log

`agent.log` (the container's last 20 000 lines) is counted by level and the
most repeated `WARN`/`ERROR` messages are kept with their numbers replaced
by `#` (`report.json` `agent_log`). `truncated` means the capture hit the
line limit, so counts are a lower bound.

## Findings (`aoc ab`)

The question `findings.md` answers is "did the logs pipeline regress
between these two agent builds?", and it answers it with a gate and four
signals. Everything else is in `compare.md`.

**The delivery gate** fails when the candidate lost, duplicated, orphaned
or truncated a record. When the baseline has the same counter, the gate
line says so — that is the workload or the harness, not the build, but it
still fails. Out-of-order delivery is reported in the gate line and does
not fail it; the delivery ratio is shown beside it.

| signal | metrics | sense |
|---|---|---|
| **throughput** | received logs /s | higher |
| **saturation** | pipeline utilization (busiest component) | lower |
| **cpu** | core agent process CPU avg, CPU seconds per 1M logs | lower |
| **memory** | core agent RSS max, agent container anon mem avg, core agent Go heap in use | lower |

A signal moved when it crossed its threshold: `thresholds.<signal>` in
aoc.yaml, else `threshold` (default 10 %), in percent. The threshold is not
a statistical test: a single run on a laptop moves resource numbers by a
few percent on its own; `runs: 2` or more takes medians. Metrics named in
`watch:` become extra rows under the signal `watch`, with the fallback
threshold. Container memory *max* is deliberately not a signal: page cache
from image packaging moved it by a fifth in runs where the pipeline held
exactly the same memory.

**Throughput when the workload is paced.** If any generator has a target
rate, received tracks generated by construction and comparing the two
sides' rates says nothing. The row is still shown, and it is a regression
only when the candidate delivered less than *its own* generators wrote by
more than the threshold while the baseline kept up. Otherwise the brief
says so in one line: saturation carries the signal instead.

The brief is then: the verdict line (`pass`, `fail: <gate reason>`,
`regression: <signals>`), the signals table, the gate line, the two builds
and the workload, the conclusion once `aoc conclude` has written one, one
section per signal that regressed (a one-sentence reading and one or two
tables that name the cause), the profile views not already shown, and the
code behind the movers — each located at `file:line`, linked at the tested
commit, and, with a checkout that has both commits, marked new, changed or
unchanged between the builds.

**Profile scoping.** `code:` in aoc.yaml lists the packages under test
(default `pkg/logs`, `comp/logs`, `comp/logs-library`). Every profile diff
table shows those first, at most six, then at most three functions from
elsewhere in the agent and only when they moved more than anything in
scope. A mover is noise unless it clears the view's absolute floor (0.2 %
of a core, 1 MB live, 50 kB/s allocated); outside the packages under test
it must also be worth 2 % of the view. The flame-graph link the brief
suggests is focused on the top in-scope mover.

## Agent telemetry

Every series matching `--telemetry-filter` (default: `logs*`, `process_*`, a
few `go_*`) is tracked. Counters report Δ over the window and a rate; gauges
report the last value and `mean`, their average over the window's scrapes
(one every `--scrape-interval`). Comparisons prefer the mean — a gauge's
last value is one sample and a busy pipeline's ratios bounce — and fall back
to the last value for runs recorded before `mean` existed.
**pipeline utilization (busiest component)** is the maximum of
`logs_component_utilization__ratio{name}` over the components: the share of
a second the busiest stage of the pipeline spends working. Near 1.0 it is
the bottleneck; it misleads when a stage is idle because an earlier one
starves it, so read it together with throughput. Names follow the agent's
`subsystem__name` convention:
`logs__bytes_sent`, `logs__encoded_bytes_sent`, `logs__sent`, `logs__decoded`,
`logs__destination_http_resp{status_code}`, `logs__rotations_nix`,
`logs__sender_latency_*`, `logs_component_utilization__ratio{name}`,
`logs_sender__send_wait`, … Needs `DD_TELEMETRY_ENABLED=true` on the agent and
network reach to its `expvar_port` (5000, bound to localhost — hence the shared
network namespace in the compose files).

## HTTP

Requests by status (what *we* answered), payload sizes on the wire and
decompressed, logs per payload (the agent's batching), encodings, agent
version/origin headers, connectivity probes (`{}`), malformed payloads,
rejections (403 bad key, 413 too large, 400 in `--strict`), injected faults,
and every non-logs path the agent hit.

## Tags

`ddtags` is comma-separated `key:value`. Tags per log and tag bytes per log are
averages over all received logs; unique keys and their frequency are listed.
This is the direct measurement of what a tag filter removes.

## Timeseries

`timeseries.csv` has one row per second: requests, logs, lines, raw/wire bytes,
4xx/5xx/dropped, generator records/lines/bytes per second, e2e p50/p99/max,
sender p50/p99, container CPU/mem, process CPU/RSS, in-flight requests. `-1`
means unknown (observer not configured or stale).
