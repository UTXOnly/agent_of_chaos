---
name: ab-test
description: Run an A/B test of two Datadog Agent images with aoc — turn what the engineer changed into one `aoc ab` command line, run it in the background, and report the verdict from the brief. Use when asked to test, compare, benchmark or A/B an agent build against the release.
---

# A/B test an agent build

The question is always the same: did the logs pipeline regress between
agent A and agent B? The engineer says what they changed; you write one
command line, run it, and read the brief.

`aoc.yaml` is settings — control image, shared env, default workload,
`source`, `runs`, per-signal `thresholds`. A test never edits it.

## 1. The command line

```
./bin/aoc ab --b IMAGE --focus "<what they said, verbatim>" [--workload NAME] [--code PKG]...
```

> "test datadog/agent-dev:my-branch-py3, I changed the tailer's rotation handling"

```bash
./bin/aoc ab --b datadog/agent-dev:my-branch-py3 \
  --focus "I changed the tailer's rotation handling" \
  --workload rotation-churn --code pkg/logs/tailers --code pkg/logs/launchers
```

| flag | when |
|---|---|
| `--b IMAGE` | always: the build under test. A CI build of a branch is `datadog/agent-dev:<branch>-py3` |
| `--a IMAGE` | only to compare two named versions; the default control is `datadog/agent:7`, the latest release |
| `--focus TEXT` | whenever they said why: their sentence, verbatim. It becomes the notebook title |
| `--workload NAME` | from the table below — a name in `profiles/`, or a path |
| `--code PKG` | the package they touched, repeatable. Default: `pkg/logs`, `comp/logs`, `comp/logs-library` |
| `--b-env K=V` | the feature flag under test: b only |
| `--env K=V` | an agent setting both sides need for the test to be fair |
| `--watch METRIC` | a `compare.md` metric they asked about (`e2e latency p99`, `tag bytes per log`, `received wire B/s`) back on the headline |
| `--name NAME` | only when they name the run; the default is `<b tag>-<workload>` |

### What changed → workload, packages

| what they changed | `--workload` | `--code` |
|---|---|---|
| tailer, file discovery, rotation | `rotation-churn`, then `chaos-restarts` | `pkg/logs/tailers`, `pkg/logs/launchers` |
| decoder, multiline, framing, parsing | `multiline-heavy` | `pkg/logs/internal/decoder`, `pkg/logs/internal/framer`, `pkg/logs/internal/parsers` |
| processor, tags, message, encoding | `baseline` | `comp/logs-library/processor`, `pkg/logs/message` |
| compression kind or level | `high-compression` | `comp/logs-library/sender`, `pkg/logs/message` |
| sender, destination, batching, retry, backoff | `intake-outage`, then `flaky-network` | `comp/logs-library/sender`, `comp/logs-library/client` |
| scheduling, sources, integrations, container logs | `baseline` | `pkg/logs/schedulers`, `pkg/logs/sources`, `comp/logs/agent` |
| raw throughput, or "just a version bump" | `deterministic-bytes`, then `baseline` | the default |

Two workloads means two runs: do the first, report it, and offer the
second rather than queueing both. `--code` matches by path prefix;
builds from before the logs-library move keep the processor, sender and
client under `pkg/logs/`, so pass those paths too if the brief's code
table comes back empty. `./bin/aoc profiles` lists every workload with
what it stresses.

### Rounds and duration

`aoc.yaml` already says `runs: 2`, so the brief is the median of two
rounds — pass `--runs` only to go up (`--runs 3` when a Δ is likely to
land within a point of its threshold) or down (`--runs 1 --duration 2m`,
a smoke that proves the images pull and the stack comes up).

`--plan` validates and prints the plan without running anything. Use it
whenever you had to guess; it also warns when one image is `-full` or
`-jmx` and the other is not, which moves memory and CPU on its own.

## 2. Run it

```bash
make build                                    # if ./bin/aoc is missing or stale
mkdir -p results
./bin/aoc ab --b … --focus "…" > results/ab.log 2>&1    # the line from step 1
```

Start it in the background, output to a file, and wait for the process to
exit. Do not poll, and do not touch Docker meanwhile — the run owns the
`aoc-a` and `aoc-b` compose projects. Wall time is
`runs × (warmup + duration + drain + ~2 min)`, doubled by `--sequential`.

Exit codes: 0 ok; 3 the delivery gate failed (b lost, duplicated,
orphaned or truncated records); 130 interrupted; anything else means a run
failed — `tail -40 results/ab.log`.

## 3. Report

The run's last lines name the brief (`tail -5 results/ab.log`); the
experiment defaults to `<b tag>-<workload>`, so the example above writes
`results/my-branch-py3-rotation-churn/`. Read that `findings.md` and
report this, in this order, and nothing else:

1. the verdict line;
2. every signal that moved, from the signals table: signal, metric, a, b,
   Δ, and the threshold it was judged against;
3. the gate line — pass, or what b lost, duplicated, orphaned or truncated;
4. the notebook URL: `jq -r '.notebook' results/<name>/ab.json`;
5. one next step: the `investigate` skill when a signal regressed, another
   round when a Δ sits on its threshold, nothing more when it is clean.

If the brief says no profiler uploads were captured, the window was
shorter than a profiling period or the intake could not reach the
trace-agent — say so, and expect the profile and code sections to be empty.

## Variations

- `--only b` after rebuilding the dev image: re-runs b, reuses a.
- `--compare-only` re-renders the brief and the notebook from disk, after
  a change to `thresholds`, `--watch` or `--code`.
- `--sequential` when the host cannot carry both stacks at once.
