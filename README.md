# agent_of_chaos (`aoc`)

A local test bench for changes to the Datadog Agent's logs pipeline. It
puts your working tree and a control build through the same workload and
the same intake faults in Docker, then reports what was lost, duplicated,
slowed down or made more expensive, down to the function.

Workloads cover load (rate, file count, container count) and edge cases
(huge multi-line records, random spurts, rotation races, crashing writers,
intake outages, throttling, network blips). Every shape is one command
line. Claude Code can lead the loop, and every step is a command you can
run yourself.

## Start here

You need Docker with Compose, Go 1.25+, the Agent checkout at
`../datadog-agent` (or `DATADOG_AGENT_SRC` in `.env`), and `DD_API_KEY` in
`.env` so metrics and profiles land in Datadog. The web UI also needs Node
and pnpm.

```bash
cp .env.example .env && make build
```

A first answer about an uncommitted change, in about six minutes once images
exist. `.` is the Agent checkout's working tree, `main` is its main branch;
only the branch differs between the sides.

```bash
./bin/aoc ab --b-src . --a-src main --workload intake-outage \
  --runs 1 --duration 3m --name retry-smoke \
  --focus "does my retry change lose or duplicate anything through a 503 outage and 429s?" --plan
```

`--plan` prints the checkout, the dirty files, the resolved workload and
the wall time, and warns when the window ends before a fault step. Remove
it to run. Then:

```bash
cat results/retry-smoke/findings.md
```

Exit 0 is clean, 3 means records were lost or duplicated, anything else is a
failed run (`tail -40` the log).

`make ui` starts the web UI at http://localhost:4100. It builds the same
command line from a form, shows the plan, launches the run and watches the
intake live (received rate, missing records, faults, agent CPU and RSS, per
side). `make ui-stop` stops it; see [ui/README.md](ui/README.md).

In Claude Code, say what you changed and what worries you. The `aoc` skill
plans the runs, launches each one through the UI (`ui/bin/aoc-ui run -- …`)
so you can watch while it waits, reads the brief and proposes the next run.

## Put the agent in a situation

Each row is one `--workload` (or `aoc run --profile`) and the knobs that shape
it. `./bin/aoc profiles` prints the catalog with its numbers.

| situation | workload | shape it with |
|---|---|---|
| steady mixed load, the reference | `baseline` | `--set rate=20000` (per generator) |
| as fast as the tailer goes | `high-throughput` | `--set rate=150000`, `--set streams=48` |
| byte-identical input for CPU per log and wire bytes | `deterministic-bytes` | `--set max-lines=4000000` |
| huge multi-line records, truncation, aggregation memory | `multiline-giant` | `--set multiline-frames=3000`, `--set multiline-rate=0.5` |
| stack traces at volume, auto multiline on or off | `multiline-heavy` | `--env DD_LOGS_CONFIG_AUTO_MULTI_LINE_DETECTION=true` |
| random spurts of logs on a quiet background | `bursty` | `--set plain.spike-rate=80000`, `--set plain.spike-jitter=20s` |
| a sudden incident and a slow recovery | `incident-scenario` | `--set scenario-speed=20` |
| rotation races, copytruncate versus rename | `rotation-churn` | `--set rotate-bytes=64KiB`, `--set plain.rotate-mode=truncate` |
| writers crashing and reopening files | `chaos-restarts` | `--set chaos-crash-rate=6` |
| oversized lines, compression | `high-compression` | `--set wide-line-bytes=65536`, `--env DD_LOGS_CONFIG_COMPRESSION_KIND=zstd` |
| 15 containers, 40 tags per line | `tag-heavy-fleet` | `--set app.replicas=30`, `--b-env DD_LOGS_CONFIG_TAG_FILTERS=…` |
| intake down for 45 s, then throttled | `intake-outage` | `--duration 4m` to reach the 429 phase |
| a bad uplink that recovers | `flaky-network` | `--set rate=10000` |
| network blips at random times | `intermittent-network` | `--fault drop_rate=0.5,latency_ms=800,flap_on_ms=3000,flap_off_ms=15000` |
| the legacy TCP transport | `tcp-forced` | `--set rate=20000`, `--env DD_LOGS_CONFIG_CONNECTION_RESET_INTERVAL=20` |
| TCP connection resets the agent cannot see | `tcp-flaky` | `--fault tcp_reset_rate=0.1,flap_on_ms=5000,flap_off_ms=20000` |
| TCP intake down, the connection manager's backoff | `tcp-outage` | `--duration 4m` to see the recovery |
| HTTP down at startup: fallback to TCP, upgrade back | `http-fallback` | `--env DD_LOGS_CONFIG_HTTP_CONNECTIVITY_RETRY_INTERVAL_MAX=60s` |

Any intake fault goes on any workload for the whole window with `--fault`:
`latency_ms`, `jitter_ms`, `drop_rate` (no response), `error_rate` with
`error_status` (429/5xx retry, other 4xx drop), `outage`, `read_bps` (slow
uplink), `tcp_reset_rate` and `tcp_refuse` for the TCP transport, and
`flap_on_ms`/`flap_off_ms` to make any of them come and go in randomized
episodes. Faults that change at fixed offsets live in a profile's `faults:`
timeline; a step with `boot: true` is in place before the agent starts. Any `aoc generate` flag is a `--set` key, for every
generator or one (`json.streams=16`); `--plan` validates before anything
starts. [Workloads and every knob](docs/workloads.md).

## What comes back

`results/<name>/findings.md` is one screen, written to be read once:

1. **The verdict** and a table of the four signals, A, B, delta, threshold:
   throughput (received logs/s), saturation (busiest component's utilization),
   CPU (process average, seconds per 1M logs) and memory (RSS max, container
   anon, Go heap).
2. **The delivery gate**: records missing, duplicated, orphaned (continuation
   lines that lost their header) or truncated on B fail the run. Out of order
   is reported and does not gate. Every record carries a sequence marker, so
   the counts are exact, not sampled.
3. **What the fault did**: end-to-end latency p99 and max, retries and
   responses by status, recovery after the fault cleared.
4. **Where to look**: the profile movers inside the packages under test
   (`--code`, default `pkg/logs`, `comp/logs`, `comp/logs-library`), each with
   its source line, whether it changed between the builds, and a link to the
   profiler's A/B comparison focused on that function.

`compare.md` has every metric; `jq` on `findings.json` or `report.json` gives
one number. `--watch "e2e latency p99"` promotes any metric to the headline.
[What every number means and where it misleads](docs/measurements.md).

Limits to keep in mind: both stacks share one host, so a delta on its
threshold deserves `--runs 3` before a conclusion. A paced workload cannot
show capacity. A 2 minute window skips later fault phases (the plan says so)
and may capture no profile. If records are still missing after a fault, the
report says whether a longer `drain` would tell buffered from lost. None of
this is release readiness; carry a real finding into SMP or the Agent's CI.

## Decide, fix, rerun

`aoc conclude` records the verdict with the numbers next to the results and
posts a Datadog event; the Datadog MCP publishes the notebook from the
evidence cells `notebook.md` indexes ([how](docs/notebooks.md)). After a fix:

```bash
./bin/aoc ab --config results/retry-smoke/aoc.yaml --only b    # rebuild b, reuse a
```

Refresh both sides when the host, image or workload changed.

## Go deeper

- [Sessions](docs/sessions.md): what a good loop looks like, turn by turn.
- [Workloads](docs/workloads.md) and the [profile format](profiles/README.md).
- [Measurements](docs/measurements.md), [Datadog data](docs/datadog.md), [reference](docs/reference.md): commands by hand, faults, builds, gotchas.
- [AGENTS.md](AGENTS.md): what Claude is told; the skills are in `.claude/skills/`.
