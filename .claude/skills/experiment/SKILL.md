---
name: experiment
description: Turn "what happens to the agent when …" into an aoc run — pick the situation (intake latency, dropped connections, outages, 429s, slow uplink, rotation storms, crashing writers, multiline floods, spikes, max throughput), write the command or a new profile, run it, and report what the agent lost, duplicated or slowed down. Use when someone wants to put an agent through a fault or a hard workload, with or without comparing two images.
---

# Run a situation

The user describes a situation. Your job is to produce one command, run it,
and report back. Comparing a build against the release is the `ab-test`
skill. Use this skill when the question is about a situation, not about a
build.

## 1. One agent or two?

- One image, "how does it behave when …" → `aoc run`.
- Two images, or "did my change make it better or worse under …" →
  `aoc ab`, then follow `ab-test` from its step 2.

## 2. Pick the situation

Faults are flags; workloads are profiles. Combine one of each.

| they say | fault (`--fault`) | workload |
|---|---|---|
| slow intake, high latency, timeouts | `latency_ms=500,jitter_ms=200` (timeouts: `latency_ms` above the agent's HTTP timeout, 10 s by default) | `baseline` |
| packet loss, flaky network, dropped connections | `drop_rate=0.05` | `baseline`; `flaky-network` for drops + latency + slow uplink that recover |
| rate limiting, 429s | `error_rate=0.2,error_status=429` | `baseline` |
| 5xx, intake errors | `error_rate=0.3,error_status=503` | `baseline` |
| outage, intake down | `outage=true` (stays down the whole window) | `intake-outage` for down-then-recover |
| bad requests, rejected payloads | `error_rate=0.1,error_status=400` (the agent drops these, so the gate will show loss) | `baseline` |
| slow uplink, bandwidth cap | `read_bps=500000` | `baseline` |
| log rotation, copytruncate, file races | — | `rotation-churn` |
| apps crashing, files reopened | — | `chaos-restarts` |
| stack traces, multiline | — | `multiline-heavy` |
| huge lines, truncation, compression | — | `high-compression` |
| traffic spike, burst | — | `incident-scenario` |
| max throughput, "how fast can it go" | — | `high-throughput`, or `deterministic-bytes` for an A/B |
| container logs, lots of tags | — | `tag-heavy-fleet` |

`drop_rate` drops whole requests. If they mean TCP-level packet loss
specifically, say that the fake intake does not model it, and offer
`scripts/spotty-intake.sh` against the real intake on a Linux host.

Faults that change over time (on at 60 s, off at 90 s, a different fault
after that) need a profile. Copy the closest one in `profiles/` to
`profiles/<short-name>.yaml` and edit its `faults:` timeline and
`generators:`. The format is in `profiles/README.md`, and generator keys are
`aoc generate -h` flag names. Do the same when they ask for a workload
number the shipped profiles don't have (rate, streams, rotation size,
multiline fraction). Tell them the file you wrote. Never put a test in
`aoc.yaml`.

## 3. The command

```bash
make build
mkdir -p results
./bin/aoc run --profile profiles/<workload>.yaml [--fault K=V,…] \
  [--agent-image IMAGE] --name <short-name> > results/<short-name>.log 2>&1
```

- `--agent-image` defaults to the compose default (the latest release).
- `--duration 2m` for a first look. Profiles default to 3–5 minutes.
- For the A/B form, use `./bin/aoc ab --b IMAGE --workload <workload>
  [--fault …] --focus "<their sentence>"`. Both sides get the fault.

Run it in the background with output to that file and wait for it to exit.
Don't poll and don't touch Docker meanwhile. Exit 0 is clean. Exit 3 means
records were lost or duplicated, which is often the finding the user was
after, not an error. Any other exit code means the run failed: `tail -40`
the log.

## 4. Report

For `aoc run`, read `results/<name>/report.md`, starting with the delivery
section. Report:

1. the fault and workload that ran, in one line;
2. delivery: lost, duplicated, out of order, orphan continuation lines,
   truncated;
3. what the fault did: e2e latency p99 and max, retries and responses by
   status, the agent's `sender_latency`;
4. the cost: core agent CPU avg and RSS max;
5. the notebook URL if one was created (printed at the end of the log).

Single values: `jq '.delivery' results/<name>/report.json`,
`jq '.latency' …`. Don't read `report.json` or `timeseries.csv` whole.

If records were lost under a 429/5xx/drop/latency fault, check
`report.md`'s notes before calling it loss. The agent may still have been
retrying when the drain ended, and a longer `drain:` tells the two apart.

For `aoc ab`, report as in `ab-test` step 3.
