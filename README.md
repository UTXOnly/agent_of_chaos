# agent_of_chaos — a testing harness for the Datadog Logs Agent

Generate log volume with exact accounting, point any Datadog Agent at a fake
logs intake instead of Datadog, and record the outcome **in Datadog** — as
`aoc.*` metrics and events, a notebook per run, and the agent's own metrics
and profiles — so every experiment is shareable, comparable and queryable
(by hand or through the Datadog MCP).

```
 ┌──────────────────┐  files / stdout   ┌──────────────────┐  HTTP (gzip/zstd)  ┌──────────────────┐
 │  aoc generate    │ ────────────────▶ │  Datadog Agent   │ ─────────────────▶ │  aoc intake      │
 │  N streams,      │  aoc=gen/stream/  │  under test      │  /api/v2/logs      │  fake intake +   │
 │  rate-paced,     │  seq per record   │  (any image)     │                    │  delivery ledger │
 │  rotation, …     │ ── stats push ───────────────────────────────────────────▶ │  + faults        │
 └──────────────────┘                   └────────┬─────────┘ ◀── /telemetry ────┴────────┬─────────┘
                                                 │ metrics, profiles (run:<name>)         │ aoc.* metrics, events (run:<name>)
                                                 ▼                                        ▼
                                          ┌────────────────────────────────────────────────────┐
                                          │  Datadog: metrics · events · notebook per run · MCP │
                                          └────────────────────────────────────────────────────┘
```

Every generated record carries a sequence marker. The intake keeps a
per-stream ledger of what arrived, so **missing, duplicated and reordered
records are counted exactly**, not estimated from byte totals. It measures
written→received latency per log, scrapes the agent's own `/telemetry`
counters, samples the agent container's CPU/memory, injects faults (429/5xx,
dropped connections, latency, throttled uplink) and submits all of it to
Datadog every 10 s, tagged `run:<name>`.

One binary, `aoc`, plays every role. Logs never leave the host.

## Quick start: A/B the latest release against a dev build

```bash
cp .env.example .env          # DD_API_KEY (required), DD_APP_KEY (for notebooks), DD_SITE
make build                    # ./bin/aoc

$EDITOR aoc.yaml              # b.image: the development image to test
./bin/aoc ab                  # a = datadog/agent:7 (latest), b = your image, same workload
```

[aoc.yaml](aoc.yaml) is the test run's config, in the spirit of
`datadog.yaml`: every setting is listed with its default, uncomment what you
change. The minimum is the workload and the two images:

```yaml
profile: profiles/baseline.yaml      # or the profile written inline
a:
  image: datadog/agent:7             # the control: re-pulled every run, so it is the latest 7.x
b:
  image: datadog/agent-dev:my-branch-py3
  env:                               # settings only the candidate gets, e.g. the feature under test
    DD_LOGS_CONFIG_TAG_FILTERS: '{"exclude":["env:*","dirname:*"]}'
```

`aoc ab` runs the profile against `a`, tears everything down, runs it
against `b` (`runs: 3` alternates a, b, a, b, a, b and takes medians), then
writes `results/<name>/`:

| file | what |
|---|---|
| `compare.md` | the side-by-side table, b's delta against a, ✅/⚠️ per metric |
| `ab.json` | the verdict for tooling: images, digests, agent versions, run windows, headline metrics |
| `notebook.json` (+ the notebook itself when `DD_APP_KEY` is set) | the A/B notebook: **both agents overlaid on every chart** (the earlier run is `timeshift`ed onto the later one), a real-time timeline of the session, the table, links per run |
| `aoc.yaml` | the config that ran |
| `a/`, `b/` (`a-2/`, `b-2/`, …) | a full [run directory](#what-a-run-writes) per side and round |

In Datadog, every `aoc.*` metric and event, and the agent's own metrics and
profiles, carry `experiment:<name>` and `variant:<a|b>` on top of
`run:<name>-<side>`, so `… {experiment:my-feature} by {variant}` puts the
two agents on one chart and the MCP can answer "compare
`aoc.agent.process.cpu_percent` between variant a and b of my-feature". The
finished test also posts a `source:aoc` event with the headline table.

```bash
aoc ab --plan                 # validate aoc.yaml, print what would run
aoc ab --duration 45s         # a quick smoke of the config
aoc ab --only b               # rebuilt the dev image: re-run b, reuse a's results
aoc ab --compare-only         # re-render compare.md / the notebook from what is on disk
aoc ab --config tests/rotation.yaml --runs 3
```

The terminal ends with the headline (here `a` named `release`, `b` named
`tagfilter`, from a 40 s smoke of the log tag-filter build):

```
aoc ab smoke-ab — 7.83.2 (datadog/agent:7@29baa94e0a1a) vs 7.85.0-devel+git.404.9e35a50 (datadog/agent-dev:log-tag-filtering-9e35a50b-full@e6f1b0aac73d)

                               release            tagfilter          Δ tagfilter vs release
  delivery ratio               100.00%            100.00%            ≈
  lost / missing               0                  0                  =
  duplicates                   0                  0                  =
  e2e latency p99              1.9s               1.9s               ≈
  received wire B/s            247.4 kB/s         237.5 kB/s         -4.0% ✅
  agent container CPU avg      16.1%              16.0%              ≈
  agent container mem max      239.2 MB           626.2 MB           +161.9% ⚠️
  tags per log                 4.12               2.07               -49.8% ✅
  tag bytes per log            121.4 B            39.9 B             -67.2% ✅
  …
  results: results/smoke-ab/ (compare.md, ab.json, notebook.json, <side>/report.md …)
  notebook: https://<org>.datadoghq.com/notebook/15601304
```

## One run at a time

```bash
./bin/aoc run --profile profiles/baseline.yaml --name baseline --agent-image datadog/agent:7
```

That one command: builds the image, starts the fake intake and the agent
(sharing a network namespace so the agent's localhost-only telemetry is
scrapeable), waits for `agent health`, starts the generators, warms up, opens
a measured window, stops the generators, drains, and writes
`results/baseline/`:

<a name="what-a-run-writes"></a>

| file | what |
|---|---|
| `report.md` / `report.json` | the full report (delivery ledger, throughput, latency, resources, HTTP, tags, streams, agent telemetry deltas, per-minute table); the agent's version and the image digest that actually ran |
| `notebook.json` | the Datadog notebook for the run (created for you when `DD_APP_KEY` is set) |
| `timeseries.csv` | one row per second |
| `agent-status.txt`, `agent.log`, `intake.log`, `gen-*.log` | what the containers said |
| `profile.yaml`, `compose.override.yml` | exactly what ran |

Meanwhile in Datadog: `aoc.*` metrics and `source:aoc` events tagged
`run:baseline`, the agent's own `docker.*`/`system.*`/`datadog.agent.*`
metrics tagged `run:baseline` (host tag), and its CPU/heap profiles in the
Continuous Profiler (`service:datadog-agent run:baseline`, one profile per
minute).

Then:

```bash
./bin/aoc run --profile profiles/baseline.yaml --name candidate --agent-image datadog/agent-dev:my-build
./bin/aoc compare results/baseline results/candidate            # Markdown delta table
./bin/aoc notebook --results results/baseline --results results/candidate   # one notebook, both runs
```

(`aoc ab` is exactly this, driven by `aoc.yaml`, with the overlay notebook
and the `experiment:`/`variant:` tags on top.)

## Notebooks

`aoc notebook` builds a **report** notebook from `report.json`: a findings
summary (delivery verdict, throughput, latency, resources, faults), then
timeseries cells over the run's absolute window — generated vs received,
delivery ledger, latency, wire vs decompressed bytes, responses by status,
agent CPU and memory (intake-observed *and* from the docker integration),
the agent's own logs-pipeline telemetry (bytes sent, destination responses
by status, pipeline utilization, retries), tags per log, faults, per
generator — and a "dig deeper" cell with the metric names, event and
profiler links, and MCP prompts.

With several `--results` it writes the `aoc compare` table first and then
each run's key charts pinned to that run's own time window, so two agent
builds sit in one notebook.

It needs `DD_APP_KEY` to create the notebook. Without one it still writes
`notebook.json` (name, absolute window, cells) — hand that to the Datadog
MCP's `create_datadog_notebook`, or set the key and run it again. `aoc run`
creates the notebook automatically when the key is present (`--notebook` to
force, or write only).

## Investigating with the Datadog MCP

Everything is a normal metric or event, so the MCP can answer questions
about a run without any of this tooling:

- "query `aoc.intake.latency.e2e.p99` and `aoc.gen.records_per_sec` for
  `run:candidate` and explain the spikes"
- "compare `aoc.agent.process.cpu_percent` between `run:baseline` and
  `run:candidate`"
- "what happened around the `source:aoc run:candidate` events?"
- "show the CPU flame graph for `service:datadog-agent run:candidate`
  filtered to `logs` frames" — the agent's internal profiler is on by default
  and uploads one profile per minute

The metric catalog is in [docs/datadog.md](docs/datadog.md).

## What gets measured

| question | how |
|---|---|
| Did everything arrive? | Per-stream ledger from the `aoc=<gen>/<stream>/<seq>` marker: unique, **missing**, **duplicates**, out of order, first missing seq ranges (gaps line up with rotations). Generators push their own counters, so "generated" is known independently of what arrived. |
| Was multiline aggregation right? | Traces written vs multiline logs received; **orphan continuation lines** = stack-trace lines the agent shipped as separate logs. |
| How long did it take? | **written → received** per log (timestamp inside the line), and **agent encode → received** (the agent's `timestamp` field), as p50/p90/p99/p99.9/max. |
| What did it cost? | Agent **container CPU/memory** (Docker API), **core agent process CPU/RSS** (from the agent's `process_*` telemetry), **CPU seconds per 1M logs**, wire bytes vs decompressed bytes (compression ratio), bytes/tags per log. |
| What did the agent do? | Every `logs*` series from the agent's `/telemetry` endpoint — `logs__bytes_sent`, `logs__sender_latency`, `logs__rotations_nix`, `logs_component_utilization__ratio`, … — as deltas over the window and as `aoc.agent.telemetry.*` rates in Datadog. Requests by status, payload sizes, logs per payload, encodings, agent version. |
| What did the agent send? | Tag-key cardinality and tag bytes per log (the number the tag-filter feature moves), service/source/host/status breakdown. |

Definitions and caveats: [docs/measurements.md](docs/measurements.md).

## The pieces

### `aoc generate` — the workload

```bash
aoc generate --streams 8 --rate 5000 --log-dir ./logs                   # 8 files, 5k lines/s total
aoc generate --format json --rate 20000 --intake http://localhost:8282  # JSON, push stats to the intake
aoc generate --output stdout --rate 500                                 # one interleaved stream for docker log drivers
aoc generate --mode scenario --scenario incident --scenario-speed 5     # scripted multi-phase run
aoc generate --rotate-mode truncate --rotate-bytes 1MiB                 # copytruncate-style rotation
aoc generate --deterministic --seed 42 --max-lines 1000000 --rate 0     # byte-identical, flat out
```

* **Rate-based**: `--rate` is aggregate lines/s across all streams (0 = as
  fast as the disk allows). Pacing counts every record including the extra
  stack-trace and oversized ones. Bursts (`--burst-size/--burst-interval`)
  ride on top.
* **Realistic content**: weighted DEBUG…CRITICAL messages, Python / Java /
  Go stack traces (`--multiline-rate`), oversized lines (`--wide-line-rate`,
  `--wide-line-bytes`), padding to a target line size (`--pad-to`).
* **Formats**: `plain` (`<svc>.log`, multi-line traces), `json`
  (`<svc>.json.log`, one object per line, trace in `error.stack`), `both`;
  `--output stdout` for container log collection.
* **Rotation** by rename (new inode) or truncate (copytruncate, same inode),
  size and backup count configurable.
* **Modes**: `steady`, `ramp`, `chaos` (streams crash/restart, sequence
  resumes), `spike`, `pulse`, `scenario` (built-in `wave`, `business-day`,
  `incident`, `longhaul`, or your YAML — `aoc scenarios show wave > my.yaml`).
* **Deterministic**: `--deterministic --seed N` gives identical bytes for
  identical config; `--max-lines` fixes the budget.
* Fast: one process writes millions of lines per second flat out; paced
  rates land within a fraction of a percent of target.

`aoc generate -h` lists every flag, grouped. Each flag is also an env var
(`AOC_ROTATE_BYTES=4MiB`), which is how the compose files configure it.

### `aoc intake` — the fake Datadog logs intake

```bash
aoc intake                                                # :8282 HTTP, :10516 legacy TCP; aoc.* → Datadog when DD_API_KEY is set
aoc intake --agent-telemetry http://localhost:5000/telemetry --docker-container aoc-agent --name baseline
aoc intake --api-key "$DD_API_KEY" --strict               # 403 on a wrong key, 400 on malformed payloads
aoc intake --dd-metrics=false                             # keep everything local
```

Accepts what the agent sends: `POST /api/v2/logs` (and `/v1/input`), gzip /
zstd / deflate / identity, the empty `{}` connectivity probe, and the legacy
TCP framing. Answers `202 {}` like the real intake, `413` above 5 MiB
decompressed, and swallows `/api/v1/validate`, `/api/v2/series`, `/intake/`
etc. with a 202 so an agent can be pointed at it entirely
([docker-compose.offline.yml](docker-compose.offline.yml)).

Point an agent at it:

```yaml
DD_LOGS_CONFIG_LOGS_DD_URL: http://<intake-host>:8282
DD_LOGS_CONFIG_LOGS_NO_SSL: "true"
DD_LOGS_CONFIG_USE_HTTP: "true"
DD_TELEMETRY_ENABLED: "true"              # lets the intake scrape /telemetry (localhost-only → shared netns)
DD_TAGS: "run:<name>"                     # so the agent's own metrics line up with aoc.* in notebooks
```

HTTP API:

| endpoint | purpose |
|---|---|
| `GET /harness/report?gaps=1` | full JSON report (with missing-seq ranges) |
| `GET /harness/status` | liveness: generators, recent receive rate, missing, faults, Datadog submission health |
| `GET /harness/timeseries?since=<unix>&format=csv` | per-second history |
| `GET /harness/faults` · `POST` · `DELETE` | read / set / clear fault injection |
| `POST /harness/reset?name=<run>` | zero everything, open a new measurement window (posts an event) |
| `POST /harness/mark?text=…` | free-text annotation event (`aoc run` marks window close, generator stop, drain) |
| `GET /metrics` | Prometheus text, if you'd rather scrape than push |
| `POST /harness/gen/report` | generators push their counters here |

### Faults

Set with `POST /harness/faults` (JSON), `--fault-*` for the initial state, or
a profile timeline. All fields are independent.

| field | effect | agent behaviour |
|---|---|---|
| `error_rate`, `error_status` | answer that status instead of 202 (payload not ingested) | 429 / 5xx → retry with backoff; other 4xx → the batch is dropped |
| `drop_rate` | read the request, then close the connection with no response | retries (the payload was never counted, so retries count once) |
| `outage` | drop everything | pipeline backs up, retries until it clears |
| `latency_ms` (+ `jitter_ms`) | delay every response | senders stall, in-flight grows, `sender_latency` climbs |
| `read_bps` | throttle how fast bodies are read | models a saturated uplink |

Every change is an event in Datadog and a line in the report.

### `aoc ship` — a reference shipper

Tails a directory and POSTs batches the way the agent does (JSON arrays,
gzip/zstd, agent headers, retry on 429/5xx). Validate the harness without an
agent, or use it as the naive baseline the agent should beat:

```bash
aoc intake --dd-metrics=false &
aoc ship --log-dir ./logs --intake http://localhost:8282 --from-start &
aoc generate --streams 6 --rate 8000 --duration 30s --log-dir ./logs --intake http://localhost:8282
aoc report --intake http://localhost:8282 --stdout
```

## Profiles

A profile is one reproducible experiment: workload per generator, agent
image/env, fault timeline, warm-up/window/drain
([profiles/README.md](profiles/README.md)); `aoc.yaml` points at one (or
inlines it) and `aoc run` takes one directly. Shipped: `baseline`,
`high-throughput` (24 streams, 100k lines/s), `high-compression`,
`rotation-churn` (128 KiB files, copytruncate vs rename), `multiline-heavy`,
`intake-outage` (503 storm then 429s), `flaky-network` (dropped connections,
latency, slow uplink), `chaos-restarts`, `incident-scenario`,
`deterministic-bytes` (byte-identical input for A/B).

## The tag-filter fleet

[docker-compose.tag-filter.yml](docker-compose.tag-filter.yml) is the
15-generator topology (8 plain-text, 7 JSON) behind
`datadog/agent-dev:log-tag-filtering`, with logs going to the fake intake.
The base file is the **baseline**; stacking
[docker-compose.tag-filter.filters.yml](docker-compose.tag-filter.filters.yml)
adds `DD_LOGS_CONFIG_TAG_FILTERS` for the **comparison**. `AOC_VARIANT` is the
`run:` tag on the agent's metrics and the intake's `aoc.*` metrics.

```bash
cp .env.example .env          # DD_API_KEY, AOC_VARIANT=baseline|comparison
docker compose -f docker-compose.tag-filter.yml up --build -d
docker compose -f docker-compose.tag-filter.yml -f docker-compose.tag-filter.filters.yml up --build -d
aoc report --intake http://localhost:8282 --out results/$AOC_VARIANT       # ssh -L 8282:localhost:8282 on EC2
aoc notebook --results results/baseline --results results/comparison    # tag bytes per log, wire B/s, CPU …
```

`scripts/spotty-intake.sh` (iptables packet loss) still works against the
real intake; against the fake one, use the `drop_rate` / `read_bps` faults.

## Building

```bash
make build            # ./bin/aoc (Go 1.25+)
make test
make image            # docker image used by the compose files
make ab               # build, then run the A/B test in aoc.yaml
go install github.com/UTXOnly/agent_of_chaos/cmd/aoc@latest
```

Dependencies: `klauspost/compress` (gzip/zstd) and `yaml.v3`.

## Things worth knowing

* **Custom metrics**: a run submits ~100 `aoc.*` series (plus one
  `aoc.stream.*` pair per stream with problems), each tagged with the run
  name, so every run creates a new set of custom-metric timeseries. Reuse
  names when you don't need a new record.
* **"Latest" is pinned down**: `aoc ab` re-pulls `a`'s image before every
  run (`pull: always`) and the report records the digest and the version
  the agent announced, so a moving tag like `datadog/agent:7` is still a
  reproducible statement. For an image you built locally, set
  `pull: never`.
* **Links to your org**: set `DD_SUBDOMAIN=<yours>` (or `DD_APP_URL`) in
  `.env` so notebook links open your subdomain rather than
  `app.datadoghq.com`.
* **Profiling cadence**: the agent's internal profiler defaults to one
  profile every 5 minutes; the compose sets `DD_INTERNAL_PROFILING_PERIOD`
  and `DD_INTERNAL_PROFILING_CPU_DURATION` to 60 s (30 s works for very short
  runs). Profiles upload through the trace-agent, so `DD_APM_ENABLED` must
  stay on; the last partial period of a run is not uploaded.
* **Rebuilding the image while the stack runs**: the agent shares the
  intake's network namespace. If you recreate the intake by hand, recreate
  the agent too (`docker compose up -d --force-recreate datadog-agent`).
  `aoc run` and `docker compose up -d --build` do the right thing.
* **Container logs**: `DD_LOGS_CONFIG_CONTAINER_COLLECT_ALL=true` by default,
  so other containers on the machine show up as "unmarked" logs. Set it to
  `false` for a quieter measurement.
* **The ~10 s latency spikes** in most runs are the agent's
  `file_scan_period`: after a rotation the new file is discovered on the
  next scan. Bigger `--rotate-bytes`, or a shorter scan period, flattens them.
* **"Missing" while generators run** includes logs still in flight; after the
  generators stop and the pipeline drains it becomes "lost". After injected
  faults the agent may still be in retry backoff when the drain ends — the
  report says so; use a longer `drain`.
* **Deterministic runs** stamp synthetic timestamps, so e2e latency is n/a
  there; everything else is measured.

## Migrating from the Python version

The Go generator replaces `agent_of_chaos.py`; `benchmark_profiles.json`
became `profiles/*.yaml` for `aoc run`.

| before | now |
|---|---|
| `--services N` | `--streams N` |
| `--lines-per-cycle` / `--cycle-sleep-ms` | `--rate` (aggregate lines/s, 0 = flat out) |
| `--max-bytes` / `--backup-count` | `--rotate-bytes` / `--rotate-keep` (+ `--rotate-mode`) |
| `--benchmark-plain-only` | `--format plain` (default; `json` / `both`) |
| `--global-seed` / `--deterministic` | `--seed` / `--deterministic` |
| `--benchmark …` / `duration_sec × total_lines_per_sec` | `--max-lines`, `--duration`, or an `aoc run` profile |
| `AOCH_*` env vars | `AOC_*` (every flag) |
