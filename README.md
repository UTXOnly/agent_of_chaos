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
cp .env.example .env          # DD_API_KEY (required), DD_APP_KEY (for notebooks), DD_SUBDOMAIN; the CLI reads it too
make build                    # ./bin/aoc

./bin/aoc ab --b datadog/agent-dev:my-branch-py3 \
  --workload baseline \
  --focus "does the new sender batching cost CPU?" \
  --code pkg/logs/sender \
  --b-env DD_LOGS_CONFIG_TAG_FILTERS='{"exclude":["dirname:*"]}'

cat results/my-branch-py3-baseline/findings.md            # the brief
./bin/aoc conclude --results results/my-branch-py3-baseline "what it turned out to be"
```

One command line is the test: the build under test (`--b`), the workload
(`--workload`, a name in [profiles/](profiles/) or a path) and the question
it answers (`--focus`, which opens the brief and names the notebook). The
control is `datadog/agent:7`, the latest release. The experiment is named
after the image tag and the workload unless `--name` says otherwise.

[aoc.yaml](aoc.yaml) holds only what every test here shares — the control
image, the agent settings both sides get, the thresholds, the default
workload, where results go. [aoc.example.yaml](aoc.example.yaml) documents
every setting with its default, in the spirit of `datadog.yaml`.

```yaml
a:
  image: datadog/agent:7             # the control: re-pulled every run, so it is the latest 7.x
profile: profiles/baseline.yaml      # the workload when --workload names none
runs: 2                              # rounds per side; tables take the median
thresholds:                          # percent change that counts as a finding
  throughput: 5
  saturation: 10
  cpu: 5
  memory: 5
```

`aoc ab` runs the profile against `a` and `b` at the same time, each in its
own compose project (`parallel: false` or `--sequential` runs them one after
the other; `runs: 2` repeats the round and takes medians), and writes an
**investigation brief** rather than a wall of metrics:

| file | what |
|---|---|
| `findings.md` | **the brief.** The verdict line; the signals table — throughput, saturation, CPU, memory, each against its threshold; the delivery gate; what we tested; one section per regressed signal with a one-sentence reading and the tables that name the cause (memory by process and cgroup, utilization by component, the agent's CPU/heap/allocation profiles diffed **function by function**, packages under test first); the profiles; and **the code** — each mover located in the agent's source at the tested commit, with whether its file changed between the two builds (given a checkout, `source:`). Your conclusion goes on top once you have one |
| `compare.md` | every metric side by side, the per-process table, the agent's telemetry counters |
| `ab.json`, `findings.json` | the same for tooling: images, digests, versions, windows, findings, file paths, notebook URL |
| `notebook.json` (+ the notebook itself when `DD_APP_KEY` is set) | the same brief, then the profiler's **comparison view embedded** (same-origin iframes: `a`'s CPU flame graph beside `b`'s, then one per mover focused on that function) and four charts, one per signal, **both agents overlaid** (the earlier run `timeshift`ed onto the later one) |
| `a/`, `b/` (`a-2/`, `b-2/`, …) | a full [run directory](#what-a-run-writes) per side and round, including `profiles/` — the agent's pprof uploads over the window |
| `aoc.yaml` | the test as it ran, settings and flags together: `aoc ab --config results/<name>/aoc.yaml` repeats it |

The profiles are the point: the agent's continuous profiler is on, the
trace-agent's upload proxy is pointed at the intake, and the intake keeps a
copy of every upload while forwarding it to Datadog unchanged. So "memory
went up 160 %" comes with "anon, not page cache; in `python3`, not the core
agent; core heap in use by function unchanged" — or with the function that
grew.

In Datadog, every `aoc.*` metric and event, and the agent's own metrics and
profiles, carry `experiment:<name>` and `variant:<a|b>` on top of
`run:<name>-<side>`, so `… {experiment:my-feature} by {variant}` puts the
two agents on one chart. The finished test posts a `source:aoc` event with
the verdict, and `aoc conclude --results results/<name> "…"` records what
the investigation found next to it.

```bash
aoc ab --b IMAGE --plan                     # validate and print what would run
aoc ab --b IMAGE --workload high-throughput # a workload by name, or a path
aoc ab --b IMAGE --duration 2m              # a quick smoke of the setup
aoc ab --b IMAGE --watch "tag bytes per log" --threshold 5
aoc ab --only b                             # rebuilt the dev image: re-run b, reuse a's results
aoc ab --config results/<name>/aoc.yaml --compare-only   # re-render the brief / the notebook from disk
aoc conclude --results results/<name> --verdict pass "…"  # → conclusion.md, an event, the notebook's first cell
```

| flag | what |
|---|---|
| `--b IMAGE` | the build under test (required, unless the config pins `b.image`) |
| `--a IMAGE` | the control (default `datadog/agent:7`, or the config's `a.image`) |
| `--workload NAME\|PATH` | the workload both sides run: a name in `profiles/`, or a path |
| `--focus TEXT` | the question this test answers: it opens the brief and names the notebook |
| `--code PKG` | a package under test; the brief shows movers there first (repeatable, commas allowed) |
| `--watch METRIC` | a `compare.md` metric to promote to the headline (repeatable) |
| `--b-env K=V` | a setting only the build under test gets — the feature flag (repeatable) |
| `--env K=V` | a setting both sides get (repeatable) |
| `--name`, `--runs`, `--duration`, `--threshold` | the experiment's name, rounds per side, the measured window, the percent that counts as a finding |
| `--sequential`, `--only a\|b`, `--plan`, `--compare-only`, `--config` | run the sides one after the other, one side, validate only, re-render from disk, another settings file |

The terminal ends with what differed and the one-sentence reading per
regression; the brief has the rest. From a 2½-minute smoke of the log
tag-filter build against the release:

```
  ⚠️ 2 regression(s): agent container mem max 553.3 MB → 643.2 MB (+16.3% ⚠️); compression ratio 20.82× → 18.07× (-13.2% ⚠️)
  ✅ 5 improvement(s): core agent Go heap in use 87.7 MB → 67.8 MB (-22.8% ✅); core agent RSS max … ; tag bytes per log 119.3 B → 39.3 B (-67.1% ✅); tags per log 4.05 → 2.04 (-49.7% ✅)
  memory: The container grew through page cache (file 308.4 MB → 410.7 MB), not process memory (anon 199.0 MB → 179.0 MB): files read, not a leak. Not a regression.
  bytes: The compression ratio fell because the bytes removed (tags, −80 B per log) were the most repetitive ones; bytes on the wire still went 248.2 kB/s → 242.1 kB/s (-2.4%). Not a regression.
```

and `findings.md` goes from the numbers to the code:

```
## What we tested
release = 7.83.2 (datadog/agent:7@29baa94e0a1a), commit 1183252e · tagfilter = 7.85.0-devel+git.404.9e35a50
(datadog/agent-dev:log-tag-filtering-9e35a50b-full@e6f1b0aac73d), commit 9e35a50b. Only tagfilter has
DD_LOGS_CONFIG_TAG_FILTERS={"exclude":["env:*","dirname:*","filename:*"]}. Workload baseline — 16 streams
at 9,862/s, 2m32s window, 1 round per side. Delivery 100.00% vs 100.00%, lost 0 vs 0, duplicates 0 vs 0.

## Where
### Memory — agent container mem max 553.3 MB → 643.2 MB (+16.3% ⚠️)
The container grew through page cache (file 308 → 411 MB), not process memory (anon 199 → 179 MB) …
| container memory max | 553.3 MB | 643.2 MB | +16.3% ⚠️ |
| ├ anon: the processes' own memory | 199.0 MB | 179.0 MB | -10.0% ✅ |
| ├ file: page cache charged to the container | 308.4 MB | 410.7 MB | +33.2% |

## Profiles
allocation rate — 22.6 MB/s → 23.1 MB/s (+448.5 kB/s)
| tagfilter.(*Scoped).Keep | comp/logs-library/tagfilter/tagfilter.go:364 | 0 B/s | 395.2 kB/s | +395.2 kB/s |

## Code
| function                 | view            | Δ           | source                                       | changed a → b |
| tagfilter.(*Scoped).Keep | allocation rate | +395.2 kB/s | comp/logs-library/tagfilter/tagfilter.go:364 | new in b      |
```

From there the investigation is `git show 9e35a50b:comp/logs-library/tagfilter/tagfilter.go`
around line 364, and the conclusion — what the difference is, where, and what
to change — goes on top of the brief and the notebook with `aoc conclude`.
(The earlier 40-second smoke had reported "+162 % container memory" for the
same build; it was this page cache.)

### For coding agents

The repository ships its own workflow: [AGENTS.md](AGENTS.md) (what to
read, what never to read, the commands), [`CLAUDE.md`](CLAUDE.md), two
skills — `ab-test` (configure, run, read the brief) and `investigate` (from
`findings.md` to a conclusion with a bounded number of Datadog MCP calls) —
and path-scoped rules for `results/` and the measurement code. The brief is
written so that an agent reads one ~10 KB file, not a results directory.

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
| `report.md` / `report.json` | the full report (delivery ledger, throughput, latency, resources incl. every process and the anon/file memory split, HTTP, tags, streams, agent telemetry deltas, profile tables, agent log digest, per-minute table); the agent's version and the image digest that actually ran |
| `profiles/<service>/<start>/` | the agent's continuous-profiler uploads over the window (`event.json`, `cpu.pprof`, `delta-heap.pprof`, …), tee'd by the intake |
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

Everything is a normal metric, event or profile, so the MCP can answer
questions about a run without any of this tooling — and `findings.md`
tells you which questions are worth asking, with the filters filled in:

- "show the CPU flame graph for `service:datadog-agent run:x-b` over its
  window, filtered to `tagfilter`" — the function the local diff named
- "`avg:aoc.agent.proc.rss_bytes{experiment:x} by {variant,proc}`" — a
  ramp is a leak, a step is a cache
- "compare `aoc.agent.process.cpu_percent` between variant a and b of `x`"
- "what happened around the `source:aoc run:x-b` events?"

The metric catalog is in [docs/datadog.md](docs/datadog.md).

## What gets measured

| question | how |
|---|---|
| Did everything arrive? | Per-stream ledger from the `aoc=<gen>/<stream>/<seq>` marker: unique, **missing**, **duplicates**, out of order, first missing seq ranges (gaps line up with rotations). Generators push their own counters, so "generated" is known independently of what arrived. |
| Was multiline aggregation right? | Traces written vs multiline logs received; **orphan continuation lines** = stack-trace lines the agent shipped as separate logs. |
| How long did it take? | **written → received** per log (timestamp inside the line), and **agent encode → received** (the agent's `timestamp` field), as p50/p90/p99/p99.9/max. |
| What did it cost? | Agent **container CPU/memory** (Docker API) split into **anon vs page cache**, **every process's RSS/CPU** (`docker top`), **core agent process CPU/RSS** (from the agent's `process_*` telemetry), **CPU seconds per 1M logs**, wire bytes vs decompressed bytes (compression ratio), bytes/tags per log. |
| Where did it go? | The agent's own **continuous-profiler uploads** — CPU, heap in use, allocation rate — merged over the window and reduced by function; `aoc ab` diffs the two builds function by function. |
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
