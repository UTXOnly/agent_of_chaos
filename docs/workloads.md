# Workloads

A workload is a profile in [profiles/](../profiles/): the generators, the
intake's fault timeline, any agent settings, and the measured window.
Each one is built to stress a different part of the pipeline, and every
number in it can be changed on the command line (next section).
`./bin/aoc profiles` prints this list with the resolved numbers.

| workload | stresses | shape | intake faults | what to watch |
|---|---|---|---|---|
| `baseline` | everything, lightly; the reference point | 8 plain + 8 JSON file streams, 10k lines/s total, 5 % stack traces, 2 MiB rotation, 3 min | none | the four signals; a reference for this workload |
| `high-throughput` | file-tailer pressure | 24 plain streams, 100k lines/s, 4 MiB rotation keeping 3, 8 % stack traces, 6 % oversized 4 KiB lines, 3 min | none | delivery ratio, saturation, CPU, end-to-end latency; raise `rate` until something gives |
| `deterministic-bytes` | repeatable fixed-budget input | 8 streams flat out, seeded content, a fixed 2,000,000 records, 4 MiB rotation | none | throughput is the observed window average; verify sustained load before interpreting capacity; CPU seconds per 1M logs and wire bytes are directly comparable. No latency: timestamps are synthetic |
| `rotation-churn` | rotation races in the tailer | 16 streams, 20k lines/s, 128 KiB files kept 2 deep; copytruncate on the plain half, rename on the JSON half, 3 min | none | lost records per stream; the streams table says which rotation mode lost |
| `chaos-restarts` | the tailer's file-handle lifecycle | 24 streams, 16k lines/s, each stream crashes 3 times a minute and restarts after 5 s, 4 min | none | lost and duplicated records around restarts, open file handles |
| `multiline-heavy` | the decoder's multiline aggregation | 12 plain streams, 8k lines/s, 35 % of records followed by a Python, Java or Go stack trace; auto multiline detection off, 3 min | none | orphan continuation lines, "multiline received / written", CPU; flip `DD_LOGS_CONFIG_AUTO_MULTI_LINE_DETECTION` |
| `high-compression` | the encoder and compression | 12 plain + 12 JSON streams, 30k lines/s, 20 % stack traces, 12 % oversized lines, 256 KiB rotation, 3 min | none | wire bytes per second, compression ratio, CPU; compare `DD_LOGS_CONFIG_COMPRESSION_KIND=gzip` and `zstd` |
| `incident-scenario` | how the pipeline absorbs a step change | the built-in incident scenario at 10× speed: calm, a sudden 14-stream spike with heavy traces and bursts, a slow recovery, 5 min | none | latency and saturation through the spike, buffering, recovery |
| `intake-outage` | the sender's buffering, retry and backoff | the `baseline` streams, 4 min | 503 to every request from 45 s to 90 s, then 20 % 429s from 150 s to 180 s | recovery time, retries and responses by status, lost or duplicated on the way back |
| `flaky-network` | retry and delivery behavior on a bad uplink | the `baseline` streams, 4 min | from 30 s to 3 min: 10 % of requests get no response, every response delayed 300 ± 200 ms, bodies read at 500 kB/s | duplicates first, then latency and in-flight payloads |
| `tag-heavy-fleet` | tag volume in the processor and the docker launcher | 15 containers logging to stdout at 500 lines/s each, collected by the docker log driver; about 40 tags per line from 20 docker labels plus 15 host tags, 3 min | none | tag bytes per log, CPU and allocations in the processor; compare with and without `DD_LOGS_CONFIG_TAG_FILTERS` |

Which one to reach for, by the code you touched:

| you touched | start with | then |
|---|---|---|
| the tailer, rotation, file handles | `rotation-churn` | `chaos-restarts` |
| the decoder, multiline, framing | `multiline-heavy` | `high-throughput` |
| the processor, tags, encoding | `baseline` | `tag-heavy-fleet` |
| the sender, batching, retry, backoff | `intake-outage` | `flaky-network` |
| compression | `high-compression` with `--watch "received wire B/s"` | `deterministic-bytes` |
| container logs, autodiscovery | `tag-heavy-fleet --set app.replicas=30` | |
| raw throughput, a version bump | `deterministic-bytes` | `baseline` |

### Tuning a workload

Nothing in a profile is fixed. The command line changes any of it, and
the profile as it actually ran (every default and override applied) is
written to `results/<name>/profile.yaml`, so a run can be repeated from
one file.

| to change | flag | examples |
|---|---|---|
| any generator setting, for all generators or one | `--set KEY=V`, `--set <gen>.KEY=V` | `--set rate=20000`, `--set json.streams=16`, `--set multiline-rate=0.3`, `--set plain.rotate-mode=truncate`, `--set app.replicas=30` |
| the intake, for the whole window | `--fault K=V,…` | `--fault latency_ms=500,jitter_ms=200`, `--fault drop_rate=0.05`, `--fault error_rate=0.2,error_status=429`, `--fault outage=true`, `--fault read_bps=500000` |
| an agent setting on both sides | `--env DD_…=…` | `--env DD_LOGS_CONFIG_COMPRESSION_KIND=gzip` |
| an agent setting on the candidate only | `--b-env DD_…=…` | `--b-env DD_LOGS_CONFIG_PIPELINES=16` |
| the window and the rounds | `--duration`, `--runs` | `--runs 1 --duration 2m` for a look, `--runs 3` when a delta sits on its threshold |
| which packages the profile diff puts first | `--code PKG` | `--code comp/logs-library/sender` |
| a metric promoted to the headline | `--watch METRIC` | `--watch "tag bytes per log"` |

The `--set` keys are the `aoc generate` flags without the dashes
(`streams`, `rate`, `rotate-bytes`, `rotate-keep`, `rotate-mode`,
`multiline-rate`, `wide-line-rate`, `wide-line-bytes`, `mode` and that
mode's flags, and so on), plus `format`, `output` and `replicas`. An
unknown key is an error before anything starts. `--plan` prints the
resolved workload and exits, which is the way to check an override.

A new profile file is only needed when a shape is worth keeping or when
faults have to change during the window (a `faults:` timeline). Copy the
closest one and edit it; [profiles/README.md](../profiles/README.md) has the
format. [aoc.yaml](../aoc.yaml) holds what every test shares: the control
image, the default workload, the window, the rounds and the per-signal
thresholds. It is settings, never a test.


Return to the [development loop](../README.md).
