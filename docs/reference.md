# Reference

`aoc run` and `aoc ab` drive everything below through docker compose. This
page is for running the pieces by hand, changing faults on a live intake,
and the things that trip people up.

## Faults

| field | effect | what the agent does |
|---|---|---|
| `latency_ms`, `jitter_ms` | delay every response by latency ± up to jitter | senders stall, in-flight grows, `sender_latency` climbs |
| `drop_rate` | read the request, close the connection with no response | retries; the intake never counted it, so a retry counts once |
| `error_rate`, `error_status` | answer that status instead of 202 (default 500) | 429/5xx: retry with backoff; other 4xx: drop the batch |
| `outage` | drop every request | pipeline backs up, retries until it clears |
| `read_bps` | read request bodies at this many bytes/s | a saturated uplink |
| `note` | free text shown in events and the report | |

There are three ways to set faults:

- `--fault K=V,…` on `aoc run` / `aoc ab`: on for the whole window.
- A `faults:` timeline in a profile: changes at given offsets
  ([profiles/README.md](../profiles/README.md)).
- On a running intake, by hand:

```bash
curl -XPOST localhost:8282/harness/faults -d '{"latency_ms":800,"note":"slow region"}'
curl localhost:8282/harness/faults
curl -XDELETE localhost:8282/harness/faults
```

A POST replaces the whole fault state. Every change becomes an event in
Datadog and a line in the report. After a run with faults, `aoc run` waits
for the full drain, because the agent's retry backoff can run for minutes.
If records are still missing when the drain ends, the report says they may
still be buffered rather than lost.

## `aoc generate`

```bash
aoc generate --streams 8 --rate 5000 --log-dir ./logs                  # 8 files, 5k lines/s total
aoc generate --format json --rate 20000 --intake http://localhost:8282 # push counters to an intake
aoc generate --output stdout --rate 500                                # for docker log collection
aoc generate --mode chaos --chaos-crash-rate 3                         # streams crash and restart
aoc generate --mode scenario --scenario incident --scenario-speed 5    # scripted phases
aoc generate --rotate-mode truncate --rotate-bytes 1MiB                # copytruncate rotation
aoc generate --deterministic --seed 42 --max-lines 1000000 --rate 0    # identical bytes, flat out
```

- `--rate` is total lines/s across streams; `0` means as fast as possible.
- Content: `--multiline-rate` (stack traces), `--wide-line-rate` /
  `--wide-line-bytes` (oversized lines), `--pad-to`, `--burst-size` /
  `--burst-interval`.
- Modes: `steady`, `ramp`, `chaos`, `spike`, `pulse`, `scenario` (built-in
  `wave`, `business-day`, `incident`, `longhaul`, or your own YAML:
  `aoc scenarios show wave > my.yaml`).

`aoc generate -h` lists every flag. Each flag is also an env var
(`--rotate-bytes` → `AOC_ROTATE_BYTES`), which is how profiles and compose
pass them.

## `aoc intake`

```bash
aoc intake                                  # :8282 HTTP, :10516 legacy TCP
aoc intake --fault-latency-ms 300           # start with a fault on
aoc intake --dd-metrics=false               # keep everything local
```

It accepts `POST /api/v2/logs` and `/v1/input` (gzip, zstd, deflate or
uncompressed) plus the legacy TCP framing, and answers `202 {}` the way the
real intake does. Requests over 5 MiB decompressed get a `413`. Other
Datadog endpoints are accepted and discarded, so you can point an agent at
it entirely ([docker-compose.offline.yml](../docker-compose.offline.yml)).

To point an agent at it:

```yaml
DD_LOGS_CONFIG_LOGS_DD_URL: http://<intake-host>:8282
DD_LOGS_CONFIG_LOGS_NO_SSL: "true"
DD_LOGS_CONFIG_USE_HTTP: "true"
DD_TELEMETRY_ENABLED: "true"   # the intake scrapes /telemetry, which needs a shared network namespace
```

| endpoint | |
|---|---|
| `GET /harness/report?gaps=1` | the full JSON report, with missing-seq ranges |
| `GET /harness/status` | generators, receive rate, missing, faults, Datadog submission health |
| `GET /harness/timeseries?since=<unix>&format=csv` | per-second history |
| `GET`/`POST`/`DELETE /harness/faults` | read, set, clear faults |
| `POST /harness/reset?name=<run>` | zero the counters and open a new window |
| `POST /harness/mark?text=…` | an annotation event |
| `GET /metrics` | Prometheus text |

## `aoc ship`

A minimal shipper (tails files, POSTs batches, retries on 429/5xx). Use it
to check the harness without an agent:

```bash
aoc intake --dd-metrics=false &
aoc ship --log-dir ./logs --intake http://localhost:8282 --from-start &
aoc generate --streams 6 --rate 8000 --duration 30s --log-dir ./logs --intake http://localhost:8282
aoc report --intake http://localhost:8282 --stdout
```

## Compose files

| file | |
|---|---|
| `docker-compose.yml` | intake + agent + generators; what `aoc run` uses |
| `docker-compose.offline.yml` | point every agent endpoint at the intake (`DD_API_KEY=offline`) |
| `docker-compose.tag-heavy-fleet.yml` | the 15 stdout containers behind the `tag-heavy-fleet` workload |
| `docker-compose.tag-filter.yml` (+ `.filters.yml`) | the standalone tag-filter fleet: base file is the baseline, add the filters file for the comparison; `AOC_VARIANT` tags the run |

## Gotchas

- **Custom metrics.** Each run submits about 100 `aoc.*` series tagged with
  its name. Reuse run names when you don't need a new record.
- **The control image moves.** `aoc ab` re-pulls `datadog/agent:7` every
  run and records the digest and version. For a locally built image, set
  `pull: never`.
- **Profiles.** The compose sets the agent's profiling period to 60 s. A
  window shorter than that has no profiles, and the last partial period is
  never uploaded. Profiles go through the trace-agent, so keep
  `DD_APM_ENABLED` on.
- **Recreating the intake by hand.** The agent shares the intake's network
  namespace, so recreate the agent too
  (`docker compose up -d --force-recreate datadog-agent`).
- **Other containers' logs.** `DD_LOGS_CONFIG_CONTAINER_COLLECT_ALL=true` is
  the default, so other containers on the host show up as unmarked logs.
- **~10 s latency spikes** come from the agent's `file_scan_period`: after a
  rotation, the new file is only found on the next scan.
- **"Missing" during a run** includes logs still in flight. It becomes
  "lost" only after the generators stop and the drain ends.
- **Deterministic runs** use synthetic timestamps, so they have no
  end-to-end latency.
- **Notebook links** go to `app.datadoghq.com`. To use your own subdomain,
  set `DD_SUBDOMAIN` in `.env`.
