# agent_of_chaos

`aoc` puts a Datadog Agent in log situations that are hard to produce on
purpose — a slow or flaky intake, an outage, rotation storms, crashing
writers, stack-trace floods — and tells you exactly what the agent lost,
duplicated or slowed down on the way.

It runs a log generator, the agent you want to test, and a fake Datadog
logs intake in Docker. Every log line carries a sequence number, so the
intake counts missing and duplicated records exactly. The intake is also
where faults are injected. Results are written to `results/<name>/` and
sent to Datadog as metrics, events, the agent's profiles and a notebook.

```
aoc generate ──files/stdout──▶ Datadog Agent ──HTTP──▶ aoc intake (fake, injects faults, counts every record)
                                     │                        │
                                     └──── metrics, profiles, events ────▶ Datadog
```

## Setup

You need Docker and Go 1.25+.

```bash
cp .env.example .env     # set DD_API_KEY; DD_APP_KEY too if you want notebooks created
make build               # ./bin/aoc
```

## Run a situation

One agent, one situation:

```bash
./bin/aoc run --profile profiles/baseline.yaml --fault latency_ms=500,jitter_ms=200 --name slow-intake
cat results/slow-intake/report.md
```

The same situation, with your build compared against the latest release:

```bash
./bin/aoc ab --b datadog/agent-dev:my-branch-py3 --workload baseline \
  --fault latency_ms=500,jitter_ms=200 --focus "does my sender change cope with a slow intake?"
cat results/my-branch-py3-baseline/findings.md
```

A run takes warmup + duration + drain + about 2 minutes. `aoc ab` runs both
agents side by side, two rounds each by default (`aoc.yaml`), so plan on
~30 minutes; use `--runs 1 --duration 2m` for a quick check.

## Situations

### Intake and network faults

`--fault` works on `aoc run` and `aoc ab`. It applies for the whole measured
window, and both sides of an A/B get it. Combine fields with commas.

| situation | flag |
|---|---|
| slow intake | `--fault latency_ms=500,jitter_ms=200` |
| connections dropped with no response | `--fault drop_rate=0.05` (5% of requests) |
| rate limited | `--fault error_rate=0.2,error_status=429` |
| server errors | `--fault error_rate=0.3,error_status=503` |
| permanent rejections (agent drops the batch) | `--fault error_rate=0.1,error_status=400` |
| intake down | `--fault outage=true` |
| saturated uplink | `--fault read_bps=500000` (bytes/s) |

For faults that start, stop or change during the run (an outage followed by
recovery, for example), use a profile with a fault timeline. Two profiles
ship with one:

- `intake-outage`: 503s for 45 s, recovery, then 30 s of 20% 429s.
- `flaky-network`: dropped connections, 300±200 ms latency and a 500 kB/s
  uplink, then recovery.

Run either with `--workload` (`aoc ab`) or `--profile` (`aoc run`).

`drop_rate` drops whole HTTP requests, not packets. To see TCP
retransmission behavior, run `scripts/spotty-intake.sh` (iptables) on a
Linux host that sends to the real intake.

### Hard workloads

| situation | workload |
|---|---|
| fast rotation, rename vs copytruncate | `rotation-churn` |
| writers crashing and restarting, files closing and reopening | `chaos-restarts` |
| stack traces that have to be joined into one log | `multiline-heavy` |
| oversized lines, poorly compressible content | `high-compression` |
| a sudden spike, then recovery | `incident-scenario` |
| as many logs as the tailer can take | `high-throughput` |
| byte-identical input, written as fast as possible | `deterministic-bytes` |
| 15 containers on stdout, about 40 tags per log | `tag-heavy-fleet` |
| steady mixed load (the default) | `baseline` |

`./bin/aoc profiles` lists them with their numbers. To make your own, copy
one in `profiles/` and change it. [profiles/README.md](profiles/README.md)
documents the format, including generator settings and fault timelines.

## Reading the results

`aoc run` writes `results/<name>/report.md`, covering delivery (lost,
duplicated, out of order, split stack traces, truncated), latency,
throughput, agent CPU and memory, and the agent's own telemetry. It exits
with 3 if records were lost or duplicated.

`aoc ab` writes `results/<name>/findings.md`, a one-page verdict:

- **Delivery gate.** Did the build under test lose, duplicate, split or
  truncate records? If so, the run fails with exit 3.
- **Four signals**, each with its own threshold in `aoc.yaml`:
  throughput, pipeline saturation, CPU and memory.
- **Under each regression:** the evidence (per-process memory, per-component
  utilization, CPU and heap profiles diffed by function) and the agent
  source lines that moved.

`compare.md` next to it holds every metric side by side. When you know what
happened, record it:

```bash
./bin/aoc conclude --results results/<name> "what it turned out to be"
```

In Datadog everything is tagged `run:<name>`, and A/B tests also get
`experiment:<name>` and `variant:a|b`. That means
`avg:aoc.intake.logs_per_sec{experiment:x} by {variant}` charts both agents.
[docs/datadog.md](docs/datadog.md) lists the metrics, and
[docs/measurements.md](docs/measurements.md) explains how each number is
computed.

## With Claude Code (or another coding agent)

Describe the experiment in plain words. The repo's skills turn that into a
command, run it and read the results back:

- "What happens to `datadog/agent:7` when the intake times out half the
  time?" → `experiment` picks the situation and writes the command.
- "Test `datadog/agent-dev:my-branch-py3`, I changed rotation handling" →
  `ab-test` picks the workload and the packages to watch.
- "Why did memory go up in `results/x`?" → `investigate` reads the brief,
  makes a few Datadog MCP calls, and records a conclusion.

[AGENTS.md](AGENTS.md) is the agent-facing guide.

## Commands

```
aoc run       --profile P [--fault K=V] [--agent-image I] [--name N]   one agent, one situation
aoc ab        --b IMAGE [--workload W] [--fault K=V] [--focus TEXT]     two agents, same situation
aoc conclude  --results results/<name> "text"                           record a conclusion
aoc profiles                                                            list workloads
aoc compare   results/x results/y                                       diff two single runs
aoc notebook  --results results/x                                       (re)create a Datadog notebook
aoc generate | intake | ship | report                                   the pieces, run by hand
```

Every command takes `-h`. Settings shared by every A/B test (control image,
thresholds, default workload, rounds) live in [aoc.yaml](aoc.yaml);
[aoc.example.yaml](aoc.example.yaml) documents each one. To run the
generator, intake or shipper on their own, change faults on a live intake,
or use the compose files directly, see [docs/reference.md](docs/reference.md).
