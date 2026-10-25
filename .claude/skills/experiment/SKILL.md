---
name: experiment
description: Turn "what happens to the agent when …" into an aoc run. Pick the workload (steady, max throughput, rotation storms, crashing writers, multiline floods, spikes, tag-heavy fleets), tune it with --set, add an intake fault with --fault (latency, dropped connections, 429/5xx, outage, slow uplink), run it, and report what the agent lost, duplicated or slowed down; or sweep one knob ("where is the knee", "how fast can it go here") one run at a time until delivery drops or a component saturates. Use when someone wants to put one agent through a situation or find its limit; comparing two builds is the ab-test skill.
---

# Run a situation

The user describes a situation. Produce one command, run it, report back.
One image and "how does it behave when …" is `aoc run`. Two images, or
"did my change make it better or worse under …", is `aoc ab`: build the
same workload and fault flags, then follow `ab-test` from its step 2.

## 1. Pick the workload

`./bin/aoc profiles` lists each one with its numbers.

| they say | `--profile` |
|---|---|
| nothing in particular, "normal load" | `baseline` |
| max throughput, "how fast can it go", "push it" | `high-throughput` (`deterministic-bytes` for an A/B) |
| log rotation, copytruncate, file races | `rotation-churn` |
| apps crashing, files reopened | `chaos-restarts` |
| stack traces, multiline | `multiline-heavy` |
| huge multi-line records, truncation, aggregation memory | `multiline-giant` |
| random spurts, bursts at odd times | `bursty` |
| huge lines, truncation, compression | `high-compression` |
| traffic spike, burst, step change | `incident-scenario` |
| container logs, lots of tags, docker labels | `tag-heavy-fleet` |
| outage then recovery | `intake-outage` |
| flaky network that recovers | `flaky-network` |
| network blips at random times | `intermittent-network` |
| the TCP transport, "force TCP", legacy protocol | `tcp-forced` (`tcp-reconnect` for the agent's own periodic reconnect) |
| TCP connection resets, a bad link on TCP | `tcp-flaky` |
| TCP intake down, reconnect backoff | `tcp-outage` |
| HTTP unreachable at startup, fallback to TCP, switch back to HTTP | `http-fallback` |

## 2. Tune it

Numbers they name go on the command line, never into the profile file:

| they say | flag |
|---|---|
| a rate, "20k lines per second" | `--set rate=20000` (every generator) or `--set json.rate=20000` |
| more or fewer files, streams, services | `--set streams=32` |
| more containers | `--set app.replicas=30` (tag-heavy-fleet) |
| bigger or smaller files, rotation size | `--set rotate-bytes=512KiB` |
| more stack traces | `--set multiline-rate=0.3` |
| bigger stack traces, "a 200 KB log" | `--set multiline-frames=300` (20–30 KiB) or `3000` (200–300 KiB, past the single-message limit) |
| spurts, "random bursts" | `--set mode=spike,spike-jitter=15s,spike-rate=40000,spike-duration=5s` (`--set burst-size=2000` for per-stream bursts) |
| long lines | `--set wide-line-rate=0.1 --set wide-line-bytes=16384` |
| JSON instead of plain text | `--set format=json` |
| stdout instead of files | `--set output=stdout` |
| an agent setting | `--env DD_LOGS_CONFIG_AUTO_MULTI_LINE_DETECTION=true` |
| a longer or shorter window | `--duration 5m` |

Any `aoc generate` flag is a `--set` key (`aoc generate -h`); an unknown
key fails before anything starts. `--plan` prints the resolved workload
and the containers it would start: use it whenever you had to guess.

## 3. Add the fault

`--fault` applies for the whole window. Combine fields with commas.

| they say | `--fault` |
|---|---|
| slow intake, high latency | `latency_ms=500,jitter_ms=200` |
| timeouts | `latency_ms=15000` (above the agent's 10 s HTTP timeout) |
| packet loss, flaky network, dropped connections | `drop_rate=0.05` |
| rate limiting, 429s | `error_rate=0.2,error_status=429` |
| 5xx, intake errors | `error_rate=0.3,error_status=503` |
| intake down the whole time | `outage=true` |
| bad requests, rejected payloads (the agent drops these: expect loss) | `error_rate=0.1,error_status=400` |
| slow uplink, bandwidth cap | `read_bps=500000` |
| intermittent, "blips", "comes and goes" | add `flap_on_ms=3000,flap_off_ms=15000` to any of the above |

`drop_rate` drops whole requests, not packets; on the TCP transport the
equivalents are `tcp_reset_rate` (whole connections) and `tcp_refuse`
(every connection); `latency_ms` and `error_rate` do nothing to TCP, which
has no responses. Random blips are `flap_on_ms`/`flap_off_ms` on the fault,
no timeline needed. Faults that change at fixed offsets (on at 60 s, off at
90 s) need a `faults:` timeline: copy the closest profile to
`profiles/<name>.yaml`, edit `faults:`, tell them the path. `intake-outage`
and `flaky-network` already have one. A fault the agent must boot under
(the HTTP connectivity check that picks the transport) is a step with
`boot: true`; `http-fallback` has one.

## 4. Run it

```bash
make build
mkdir -p results
ui/bin/aoc-ui run -- run --profile <workload> [--set …] [--fault …] [--agent-image IMAGE] \
  --name <short-name> > results/<short-name>.log 2>&1
```

Through the UI, so the engineer watches the pipeline diagram and the
charts live at `http://localhost:4100/#live`; the wrapper prints each
phase change and exits with aoc's code. `--agent-image` defaults to
`datadog/agent:7`. Profiles run 3 to 5 minutes; `--duration 2m` for a
first look. Start it in the background with output to that file and wait
for it to exit; do not poll and do not touch Docker meanwhile. Exit 0 is clean. Exit 3 means records were lost or
duplicated, which is often the finding they were after, not an error.
Anything else is a failed run: `tail -40` the log.

## 5. Report

Read `results/<name>/report.md`, delivery section first. Report:

1. the workload, the settings changed and the fault, in one line;
2. delivery: lost, duplicated, out of order, orphan continuation lines,
   truncated;
3. what the fault did: end-to-end latency p99 and max, retries and
   responses by status, the agent's `sender_latency`;
4. the cost: core agent CPU avg and RSS max;
5. the notebook URL if published through MCP, otherwise the local results path;
6. the path of `results/<name>/profile.yaml`, which repeats the run.

Single values: `jq '.delivery' results/<name>/report.json`,
`jq '.latency' …`. Never read `report.json` or `timeseries.csv` whole.

If records were lost under a 429/5xx/drop/latency fault, check the notes
in `report.md` before calling it loss: the agent may still have been
retrying when the drain ended, and a longer `drain:` tells the two apart.

## Sweeps: raise a knob until something gives

"Where is the knee", "how fast can it go here", "how many containers
before it falls behind" is a sweep: one `aoc run` per value, read after
each, stop when something gives. Bounded: at most four runs, 2 min
windows, one knob.

| the knob | `--set` | the first value |
|---|---|---|
| lines per second | `rate=…` | the profile's rate |
| files per host | `streams=…` | the profile's streams |
| containers | `app.replicas=…` (tag-heavy-fleet) | 15 |
| stack traces | `multiline-rate=…` | 0.05 |
| stack trace size | `multiline-frames=…` | 300 |
| line size | `wide-line-rate=… wide-line-bytes=…` | 0.06 / 4096 |
| the intake's bandwidth | `--fault read_bps=…` | 2000000 |

```bash
ui/bin/aoc-ui run -- run --profile high-throughput --set rate=100000 --duration 2m --name knee-100k
ui/bin/aoc-ui run -- run --profile high-throughput --set rate=150000 --duration 2m --name knee-150k
./bin/aoc compare results/knee-100k results/knee-150k
```

Name each run `<base>-<value>`. After each one read two numbers:

```bash
jq '.delivery.ratio' results/<name>/report.json
jq '.telemetry[] | select(.name=="logs_component_utilization__ratio") | {labels, last}' results/<name>/report.json
```

Stop when delivery drops under 99.9 % while the generators held their
rate (the `aoc.gen.records_per_sec` line in the report is flat), or when
a component sits at 1.0. Step up by half the previous step from there if
they want the value narrowed; otherwise report the bracket. The finding
is the saturation point and the component that pinned first, with the
core agent's CPU there; the loss past it is the generator outrunning the
agent, not a bug, and says nothing on its own.

A sweep across two builds is one `aoc ab` per value with `--name
<base>-<value>`, each judged on saturation and throughput; propose that
only after the single-agent sweep has found the bracket.

`aoc conclude` is A/B-only; for a single run write
`results/<name>/conclusion.md` directly. To publish, follow
[docs/notebooks.md](../../../docs/notebooks.md).
