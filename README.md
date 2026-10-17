# agent_of_chaos

`aoc` puts a Datadog Agent in log situations that are hard to produce on
purpose (a slow or flaky intake, an outage, rotation storms, crashing
writers, stack-trace floods, tag-heavy container fleets) and tells you
exactly what the agent lost, duplicated or slowed down on the way. Pointed
at two agent builds, it answers one question: did the logs pipeline regress
between A and B, and if so, in which function.

It runs log generators, the agent under test and a fake Datadog logs intake
in Docker. Every log line carries a sequence number, so the intake counts
missing and duplicated records exactly instead of estimating from byte
totals. The intake is also where faults are injected, where the agent's
telemetry is scraped, and where its continuous-profiler uploads are copied
before being forwarded. Results land in `results/<name>/` and in Datadog as
metrics, events, the agent's own profiles and a notebook.

```
aoc generate ──files/stdout──▶ Datadog Agent ──HTTP──▶ aoc intake (fake: counts every record, injects faults)
                                     │                        │
                                     └──── metrics, profiles, events ────▶ Datadog
```

## Setup

Docker and Go 1.25 or later.

```bash
cp .env.example .env     # DD_API_KEY; DD_APP_KEY too if you want notebooks created
make build               # ./bin/aoc
```

## Run an experiment

One agent, one situation:

```bash
./bin/aoc run --profile baseline --fault latency_ms=500,jitter_ms=200 --name slow-intake
cat results/slow-intake/report.md
```

Two agents, same situation, with a verdict:

```bash
./bin/aoc ab --b datadog/agent-dev:my-branch-py3 --workload rotation-churn \
  --focus "I changed the tailer's rotation handling"
cat results/my-branch-py3-rotation-churn/findings.md
```

Your checkout instead of a CI image, built on the release so the Python
integrations keep working:

```bash
./bin/aoc ab --b-src . --a-src main --code pkg/logs/tailers --focus "…"
```

A run takes warmup + window + drain + about two minutes; `aoc ab` runs both
sides at once, two rounds each by default ([aoc.yaml](aoc.yaml)), so plan on
half an hour. `--runs 1 --duration 2m` is a smoke test. `--plan` prints what
would run and exits.

## Shape the workload

A workload is a profile in [profiles/](profiles/): the generators (how many
streams, how fast, which format, files or stdout, rotation, stack traces),
the intake's fault timeline, the agent's settings and the measured window.
`./bin/aoc profiles` lists them with their numbers.

| situation | workload |
|---|---|
| steady mixed load (the default) | `baseline` |
| as many logs as the tailer can take | `high-throughput` |
| byte-identical input, written flat out, for CPU per log | `deterministic-bytes` |
| fast rotation, rename vs copytruncate | `rotation-churn` |
| writers crashing and restarting | `chaos-restarts` |
| stack traces that must be joined into one log | `multiline-heavy` |
| oversized lines, poorly compressible content | `high-compression` |
| a sudden spike, then recovery | `incident-scenario` |
| the intake goes down, comes back, then throttles | `intake-outage` |
| dropped connections, latency and a slow uplink that recover | `flaky-network` |
| 15 containers on stdout, about 40 tags per log | `tag-heavy-fleet` |

Any setting changes from the command line, so a new experiment rarely needs
a new file:

```bash
--set rate=20000                 # every generator
--set json.streams=16            # one generator
--set plain.multiline-rate=0.3   # any `aoc generate` flag
--set app.replicas=30            # more containers
--fault drop_rate=0.05           # an intake fault for the whole window
--fault error_rate=0.2,error_status=429
--env DD_LOGS_CONFIG_AUTO_MULTI_LINE_DETECTION=true     # an agent setting
```

Faults that start, stop or change during the window are a `faults:`
timeline in a profile. The profile as it ran, with every default and every
`--set` applied, is written to the results, so a run is reproducible from
one file. [profiles/README.md](profiles/README.md) has the format.

## Read the results

`aoc run` writes `results/<name>/report.md`: delivery (lost, duplicated,
out of order, split stack traces, truncated), latency, throughput, the
agent's CPU and memory by process, and its own telemetry. Exit 3 means
records were lost or duplicated.

`aoc ab` writes `results/<name>/findings.md`, a one-page brief:

- **The delivery gate.** Did the build under test lose, duplicate, split
  or truncate records? If so the run fails with exit 3.
- **Four signals**, each against its own threshold in `aoc.yaml`:
  throughput, pipeline saturation, CPU and memory.
- **Under each regressed signal**, the evidence: memory by process and by
  cgroup, utilization by component, the agent's CPU and heap profiles
  diffed function by function, and each mover located in the agent's
  source with whether its file changed between the two builds.

`compare.md` next to it has every metric side by side. The same brief leads
the Datadog notebook, with the profiler's comparison view embedded and both
agents overlaid on each chart. When you know what happened:

```bash
./bin/aoc conclude --results results/<name> "what it turned out to be"
```

In Datadog everything carries `run:<name>`; A/B tests add
`experiment:<name>` and `variant:a|b`, so
`avg:aoc.intake.logs_per_sec{experiment:x} by {variant}` charts both
agents. [docs/datadog.md](docs/datadog.md) lists the metrics and
[docs/measurements.md](docs/measurements.md) says how each number is
computed and where it misleads.

## With a coding agent

The repository is written to be driven by Claude Code or another coding
agent. Describe the experiment in plain words; the skills turn it into a
command, run it and read the results back:

- "What happens to the agent when the intake times out half the time?"
  The `experiment` skill picks the workload, the `--set` and `--fault`
  flags, runs it and reports delivery, latency and cost.
- "Test my branch, I changed rotation handling." The `ab-test` skill picks
  the workload and the packages to watch, runs the A/B and reports the
  verdict.
- "Why did memory go up in `results/x`?" The `investigate` skill reads
  the brief, makes a bounded number of Datadog MCP calls, and records a
  conclusion.

[AGENTS.md](AGENTS.md) is the agent-facing guide. The brief is sized so
that an agent reads one 10 KB file, not a results directory.

## Commands

```
aoc run       --profile NAME|PATH [--set K=V] [--fault K=V] [--env K=V] [--agent-image I] [--name N] [--plan]
aoc ab        --b IMAGE | --b-src . [--a IMAGE | --a-src REF] [--workload NAME|PATH]
              [--set K=V] [--fault K=V] [--env K=V] [--b-env K=V] [--code PKG] [--focus TEXT] [--plan]
aoc build     [--src .|REF] [--base IMAGE]      an agent image from the checkout: aoc/agent-dev:<branch>
aoc conclude  --results results/<name> "text"   record a conclusion (event, notebook, findings.md)
aoc profiles  [show NAME]                       the workloads; one with its defaults filled in
aoc compare   results/x results/y               diff two single runs
aoc notebook  --results results/x               (re)create a Datadog notebook
aoc generate | intake | ship | report           the pieces, run by hand
```

Every command takes `-h`. Settings shared by every A/B test (control image,
thresholds, default workload, rounds, the agent checkout) live in
[aoc.yaml](aoc.yaml); [aoc.example.yaml](aoc.example.yaml) documents each
one. To run the generator or the intake on their own, change faults on a
live intake, or see what trips people up, read
[docs/reference.md](docs/reference.md).
