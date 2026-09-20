# agent_of_chaos — a testing harness for the Datadog Logs Agent

Generate log volume with exact accounting, point any Datadog Agent at a fake
intake instead of Datadog, and get numbers you can diff: **did every line
arrive exactly once, how long did it take, and what did it cost the agent?**

```
 ┌──────────────────┐   files / stdout    ┌──────────────────┐   HTTP (gzip/zstd)   ┌──────────────────┐
 │  aoc generate    │ ──────────────────▶ │  Datadog Agent   │ ───────────────────▶ │  aoc intake      │
 │  N streams,      │  aoc=gen/stream/seq │  under test      │  /api/v2/logs        │  fake intake +   │
 │  rate-paced,     │  in every record    │  (any image)     │                      │  ledger + faults │
 │  rotation, …     │ ──── stats push ───────────────────────────────────────────▶ │  + dashboard     │
 └──────────────────┘                     └──────────────────┘ ◀── telemetry scrape ─┴──────────────────┘
                                                                docker stats (CPU/mem)
```

Every generated record carries a sequence marker. The intake keeps a per-stream
ledger of what arrived, so **missing, duplicated and reordered records are
counted exactly**, not estimated from byte totals. It also measures
written→received latency per log, scrapes the agent's own `/telemetry`
counters, samples the agent container's CPU/memory, and can inject faults
(429/5xx, dropped connections, latency, throttled uplink) while you watch.

One binary, `aoc`, plays every role. Everything runs offline: no API key, no
traffic to Datadog unless you ask for it.

## Quick start (Docker)

```bash
docker compose up -d --build        # fake intake + datadog/agent:7 + two generators
open http://localhost:8282          # live dashboard
```

Within ~30 s the tiles show generated vs received rates, the delivery ratio,
duplicates, e2e latency, agent CPU/memory and bytes on the wire. When you have
seen enough:

```bash
make build                                                   # ./bin/aoc
./bin/aoc report --intake http://localhost:8282 --out results/first-look
docker compose down -v                                       # -v drops the log volume: clean slate next time
```

`results/first-look/` now holds `report.md` (human), `report.json` (for `aoc
compare`) and `timeseries.csv` (per second).

Swap the agent with `DD_AGENT_IMAGE=datadog/agent-dev:my-branch docker compose
up -d`, change the workload with `AOC_RATE=50000 AOC_STREAMS=24`, or add a
generator that logs to stdout (docker log-driver path) with
`docker compose --profile stdout up -d`. All knobs: [.env.example](.env.example).

## Run an experiment you can diff

`aoc run` turns a profile into a measured, reproducible run: clean stack →
agent healthy → generators → warm-up → measured window (with a fault timeline,
if any) → stop generators → drain → report + agent status/logs → down.

```bash
./bin/aoc profiles                                         # what's available
./bin/aoc run --profile profiles/baseline.yaml --name baseline --agent-image datadog/agent:7
./bin/aoc run --profile profiles/baseline.yaml --name candidate --agent-image datadog/agent-dev:my-build
./bin/aoc compare results/baseline results/candidate
```

`compare` prints a Markdown table with deltas and a verdict per metric (lower
latency ✅, more memory ⚠️, load controls are neutral), plus the agent's own
telemetry counters side by side. An excerpt from two real 1-minute runs on the
same laptop:

| metric | baseline | candidate | Δ vs baseline |
|---|---|---|---|
| agent | 7.79.0 (gcr.io/datadoghq/agent:7) | 7.85.0-devel (agent-dev:log-tag-filtering) | |
| unique delivered | 601,697 | 602,087 | ≈ |
| lost / missing | 0 | 0 | = |
| duplicates | 0 | 0 | = |
| e2e latency p99 | 8.8s | 1.9s | -78.3% ✅ |
| agent container mem avg | 123.3 MB | 520.3 MB | +321.8% ⚠️ |
| CPU seconds per 1M logs | 14.69 | 15.05 | +2.5% ⚠️ |
| tag bytes per log | 118.6 B | 119.3 B | +0.6% ⚠️ |

Profiles are small YAML files ([profiles/README.md](profiles/README.md)). The
ones shipped: `baseline`, `high-throughput` (24 streams, 100k lines/s),
`high-compression`, `rotation-churn` (128 KiB files, copytruncate vs rename),
`multiline-heavy`, `intake-outage` (503 storm then 429s), `flaky-network`
(dropped connections, latency, slow uplink), `chaos-restarts`,
`incident-scenario`, `deterministic-bytes` (byte-identical input for A/B).

## What gets measured

| question | how |
|---|---|
| Did everything arrive? | Per-stream ledger from the `aoc=<gen>/<stream>/<seq>` marker: unique, **missing**, **duplicates**, out of order, first missing seq ranges (gaps line up with rotations). Generators push their own counters, so "generated" is known independently of what arrived. |
| Was multiline aggregation right? | Traces written vs multiline logs received; **orphan continuation lines** = stack-trace lines the agent shipped as separate logs. |
| How long did it take? | **written → received** per log (timestamp inside the line), and **agent encode → received** (the agent's `timestamp` field), as p50/p90/p99/p99.9/max. |
| What did it cost? | Agent **container CPU/memory** (Docker API), **core agent process CPU/RSS** (from the agent's `process_*` telemetry), **CPU seconds per 1M logs**, wire bytes vs decompressed bytes (compression ratio), bytes/tags per log. |
| What did the agent do? | Every `logs*` series from the agent's `/telemetry` endpoint — `logs__bytes_sent`, `logs__sender_latency`, `logs__rotations_nix`, `logs_component_utilization__ratio`, … — as deltas over the window. Requests by status, payload sizes, logs per payload, encodings, agent version. |
| What did the agent send? | Live samples with service/source/host/status/tags, tag-key cardinality and bytes (the number the tag-filter feature moves). |

The full definitions and the caveats that matter are in
[docs/measurements.md](docs/measurements.md).

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

* **Rate-based**: `--rate` is aggregate lines/s across all streams (0 = as fast
  as the disk allows). Pacing counts every record including the extra
  stack-trace and oversized ones. Bursts (`--burst-size/--burst-interval`)
  ride on top.
* **Realistic content**: weighted DEBUG…CRITICAL messages, Python / Java / Go
  stack traces (`--multiline-rate`), oversized lines (`--wide-line-rate`,
  `--wide-line-bytes`), padding to a target line size (`--pad-to`).
* **Formats**: `plain` (`<svc>.log`, multi-line traces), `json` (`<svc>.json.log`,
  one object per line, trace in `error.stack`), `both`; `--output stdout` for
  container log collection.
* **Rotation** by rename (new inode) or truncate (copytruncate, same inode),
  size and backup count configurable.
* **Modes**: `steady`, `ramp`, `chaos` (streams crash/restart, sequence
  resumes), `spike`, `pulse`, `scenario` (built-in `wave`, `business-day`,
  `incident`, `longhaul`, or your YAML — `aoc scenarios show wave > my.yaml`).
* **Deterministic**: `--deterministic --seed N` gives identical bytes for
  identical config (seeded content, synthetic timestamps), `--max-lines`
  fixes the budget.
* Fast: a single process comfortably writes hundreds of thousands of lines per
  second; use `--streams`/`--rate` rather than many containers.

`aoc generate -h` lists every flag, grouped. Each flag is also an env var
(`AOC_ROTATE_BYTES=4MiB`), which is how the compose files configure it.

### `aoc intake` — the fake Datadog logs intake

```bash
aoc intake                                                # :8282 HTTP, :10516 legacy TCP, dashboard at /
aoc intake --agent-telemetry http://localhost:5000/telemetry --docker-container aoc-agent
aoc intake --api-key "$DD_API_KEY" --strict               # 403 on a wrong key, 400 on malformed payloads
```

Accepts what the agent sends: `POST /api/v2/logs` (and `/v1/input`), gzip /
zstd / deflate / identity, the empty `{}` connectivity probe, and the legacy
TCP framing. Answers `202 {}` like the real intake, `413` above 5 MiB
decompressed, and — so an agent can be *entirely* offline — swallows
`/api/v1/validate`, `/api/v2/series`, `/intake/`, `/api/v1/check_run` and
anything else with a 202 (counted under "other agent traffic").

Point an agent at it:

```yaml
DD_LOGS_CONFIG_LOGS_DD_URL: http://<intake-host>:8282
DD_LOGS_CONFIG_LOGS_NO_SSL: "true"
DD_LOGS_CONFIG_USE_HTTP: "true"
DD_DD_URL: http://<intake-host>:8282      # optional: metrics/metadata too → fully offline
DD_TELEMETRY_ENABLED: "true"              # optional: lets the intake scrape /telemetry
```

HTTP API (what the dashboard and the CLI use):

| endpoint | purpose |
|---|---|
| `GET /` | dashboard |
| `GET /harness/stats` | report + live state (1 Hz poll) |
| `GET /harness/report?gaps=1` | full JSON report, with missing-seq ranges |
| `GET /harness/timeseries?since=<unix>&format=csv` | per-second history |
| `GET /harness/faults` · `POST` · `DELETE` | read / set / clear fault injection |
| `POST /harness/reset?name=<run>` | zero everything, open a new measurement window |
| `GET /harness/samples` | last 50 received logs |
| `GET /metrics` | Prometheus text (scrape it with the agent's OpenMetrics check if you like) |
| `POST /harness/gen/report` | generators push their counters here |

### Faults

Set from the dashboard, the CLI (`--fault-*` for the initial state), or
`POST /harness/faults` with a JSON body. All fields are independent.

| field | effect | agent behaviour |
|---|---|---|
| `error_rate`, `error_status` | answer that status instead of 202 (payload not ingested) | 429 / 5xx → retry with backoff; other 4xx → the batch is dropped |
| `drop_rate` | read the request, then close the connection with no response | retries (the payload was never counted, so retries count once) |
| `outage` | drop everything | pipeline backs up, retries until it clears |
| `latency_ms` (+ `jitter_ms`) | delay every response | senders stall, in-flight grows, `sender_latency` climbs |
| `read_bps` | throttle how fast bodies are read | models a saturated uplink |

Profiles schedule faults on a timeline (`intake-outage.yaml`,
`flaky-network.yaml`). Every change is logged into the report.

### `aoc ship` — a reference shipper

Tails a directory and POSTs batches exactly the way the agent does (JSON
arrays, gzip/zstd, agent headers, retry on 429/5xx). Use it to validate the
harness without an agent, or as the naive baseline the agent should beat:

```bash
aoc intake &
aoc ship --log-dir ./logs --intake http://localhost:8282 --from-start &
aoc generate --streams 6 --rate 8000 --duration 30s --log-dir ./logs --intake http://localhost:8282
aoc report --intake http://localhost:8282 --stdout
```

## The tag-filter fleet

[docker-compose.tag-filter.yml](docker-compose.tag-filter.yml) is the 15-generator
topology (8 plain-text, 7 JSON) behind `datadog/agent-dev:log-tag-filtering`,
now with logs going to the fake intake. The base file is the **baseline**;
stacking [docker-compose.tag-filter.filters.yml](docker-compose.tag-filter.filters.yml)
adds `DD_LOGS_CONFIG_TAG_FILTERS` for the **comparison**. Metrics still go to
Datadog when `DD_API_KEY` is real (so existing dashboards keep working); set
`DD_DD_URL=http://localhost:8282` for a fully offline run.

```bash
cp .env.example .env          # DD_API_KEY, AOC_VARIANT=baseline|comparison
docker compose -f docker-compose.tag-filter.yml up --build -d
docker compose -f docker-compose.tag-filter.yml -f docker-compose.tag-filter.filters.yml up --build -d
ssh -L 8282:localhost:8282 <host>   # then open http://localhost:8282
aoc report --intake http://localhost:8282 --out results/$AOC_VARIANT
aoc compare results/baseline results/comparison    # tag bytes per log, wire B/s, CPU …
```

`scripts/spotty-intake.sh` (iptables packet loss) still works against the real
intake; against the fake one, use the `drop_rate` / `read_bps` faults instead.

## Building

```bash
make build            # ./bin/aoc (Go 1.25+)
make test             # unit tests (generator, intake, ledger, report)
make image            # docker image used by the compose files
go install github.com/UTXOnly/agent_of_chaos/cmd/aoc@latest
```

Dependencies: `klauspost/compress` (gzip/zstd) and `yaml.v3`. The dashboard is
plain HTML/JS embedded in the binary — no build step, works offline.

## Things worth knowing

* **Rebuilding the image while the stack runs**: the agent shares the intake's
  network namespace (so it can reach `localhost:8282` and expose its
  localhost-only telemetry to the intake). If you recreate the intake by hand,
  recreate the agent too (`docker compose up -d --force-recreate datadog-agent`).
  `aoc run` and `docker compose up -d --build` do the right thing.
* **Container logs**: `DD_LOGS_CONFIG_CONTAINER_COLLECT_ALL=true` by default,
  so other containers on the machine show up as "unmarked" logs. Set it to
  `false` for a quieter measurement.
* **The 10 s latency spikes** you will see in most runs are the agent's
  `file_scan_period`: after a rotation the new file is discovered on the next
  scan. Bigger `--rotate-bytes`, or a shorter scan period, flattens them.
* **"Missing" while generators run** includes logs still in flight; after the
  generators stop and the pipeline drains it becomes "lost". `aoc run` drains
  before reporting.
* **Deterministic runs** stamp synthetic timestamps, so e2e latency is n/a
  there; everything else is measured.
* Port 8282 for the intake, 10516 for TCP; override with `AOC_INTAKE_PORT` /
  `AOC_TCP_PORT`.

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
