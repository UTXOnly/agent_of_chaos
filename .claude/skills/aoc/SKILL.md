---
name: aoc
description: Lead an aoc session from a development goal to a conclusion. Turn "I'm working on X, help me test it", "is my branch ready", "what should I run for this change", "keep going" or "what next" into an ordered plan of experiments, run them one at a time with the ab-test and experiment skills, read each result, decide the next run from it (another shape, another round, an investigation, or done), and close with aoc conclude. Use when the engineer states a goal rather than a single test; a single test is ab-test or experiment, and explaining a finished result is investigate.
---

# Lead the loop

The engineer brings a goal. You own the plan, the environment and the
order of runs; they own the code and the decisions about it. After every
run you report what moved and say what you would run next, and why, so
the session always has a next step until it has a conclusion.

```
goal → plan → run → read → decide → (next run | investigate → conclude | done)
```

One run at a time. The machine carries both sides of one A/B, not two
A/Bs; queueing runs costs the engineer the chance to redirect. The
engineer watches in the UI and steers between runs; you do everything
else, including the reading and the conclusion. `status` says where
things stand when a session resumes; `stop` ends a run cleanly when the
goal changes.

## 1. Understand the goal

Get these from what they said; infer before you ask, and ask at most one
question, only when two readings would lead to different runs:

| need | from | if unknown |
|---|---|---|
| **what is under test** | "my checkout", "my branch", a CI image, a flag, a setting, a version | `--b-src .` when they talk about their own change and name no image |
| **what they are worried about** | loss or duplicates, CPU, memory, latency, recovery, throughput, bytes | the area of the pipeline they touched decides (table below) |
| **how sure the answer has to be** | "quick look", "before I push", "for the design doc", "a default" | a quick look is `--runs 1 --duration 2m`; a decision is the config's window and rounds |
| **what the host can carry** | laptop or instance; the default workloads run twice over on a laptop | keep `--set rate` within what both sides can take at once |

The area under change picks the first two workloads and the packages:

| they touched | run first | then | `--code` |
|---|---|---|---|
| tailer, rotation, file discovery, handles | `rotation-churn` | `chaos-restarts` | `pkg/logs/tailers`, `pkg/logs/launchers` |
| decoder, multiline, framing, parsers | `multiline-heavy` | `multiline-giant` | `pkg/logs/internal/decoder`, `…/framer`, `…/parsers` |
| processor, tags, message, encoding | `baseline` | `tag-heavy-fleet` | `comp/logs-library/processor`, `pkg/logs/message` |
| sender, destination, batching, retry, backoff | `intake-outage` | `flaky-network`, `intermittent-network` | `comp/logs-library/sender`, `comp/logs-library/client` |
| buffering, backpressure, pipeline sizing | `bursty` | `incident-scenario` | `comp/logs-library/sender` |
| compression | `high-compression` | `deterministic-bytes` | `comp/logs-library/sender`, `pkg/logs/message` |
| container launcher, autodiscovery | `tag-heavy-fleet` | `--set app.replicas=30` | `pkg/logs/launchers/container`, `comp/logs/agent` |
| schedulers, sources, integrations | `baseline` | `chaos-restarts` | `pkg/logs/schedulers`, `pkg/logs/sources` |
| a version bump, "did master regress" | `deterministic-bytes` | `baseline` | the default |
| a setting or feature flag | the workload the flag is for | `baseline` for the common case | the package that reads it |

## 2. Propose the plan

Two to four runs, in order, each one line with its purpose and its wall
time, and the decision each one is a gate for. Say it in a few lines,
then start the first run without waiting unless something material is
unknown. A plan for "is it safe before I push":

```
1. smoke, baseline, 1 round, 2 min: the build compiles and both stacks come up   ~6 min
2. intake-outage, 2 rounds, 10 min: the gate under 503s and 429s                 ~35 min
3. flaky-network if 2 is clean: duplicates under dropped connections             ~35 min
```

Shape the environment to the goal, on the command line, never in
`aoc.yaml` or a profile file:

| the goal needs | flag |
|---|---|
| the change isolated from main | `--b-src . --a-src main` |
| a flag's cost and benefit | same image both sides, `--b-env K=V` |
| a setting both sides need to be fair | `--env K=V` |
| a heavier or different shape | `--set rate=…`, `--set json.streams=…`, `--set multiline-rate=…`, `--set multiline-frames=…`, `--set app.replicas=…` |
| spurts at random times | `--set mode=spike,spike-jitter=…,spike-rate=…` or the `bursty` workload |
| the intake misbehaving the whole window | `--fault latency_ms=…`, `drop_rate=…`, `error_rate=…,error_status=…`, `outage=true`, `read_bps=…` |
| the intake misbehaving in random episodes | the same, plus `flap_on_ms=…,flap_off_ms=…` |
| faults that change mid-window | the profile that has the timeline (`intake-outage`, `flaky-network`) or a copied profile |
| a metric the test is about on the headline | `--watch "tag bytes per log"`, `--watch "received wire B/s"`, `--watch "e2e latency p99"` |
| a tighter or looser bar for every signal | `--threshold N` |
| the profiler's functions to lead with | `--code PKG` |
| a quick look | `--runs 1 --duration 2m` |
| a delta that sits on its threshold | `--runs 3` |

`--plan` before the first run whenever you had to guess, and always with
`--b-src`: it shows the checkout, the branch, the commit and the
uncommitted files, which you check against what they said they changed,
the wall time, and a warning when the window ends before a fault step
(a 2 min look at `intake-outage` never reaches its 429s).

## 3. Run

Hand each run to `ab-test` (two sides) or `experiment` (one agent, a
situation, or a sweep) from their step 2. Every run goes through the UI so
the engineer can watch it, always with `--name`:

```bash
ui/bin/aoc-ui run -- ab --b-src . --a-src main --workload intake-outage --name backoff-outage \
  --code comp/logs-library/sender --focus "…" > results/backoff-outage.log 2>&1
```

Background, output to a file, wait for the exit, do not poll, do not touch
Docker meanwhile. The wrapper starts the server if needed, prints the live
URL (`http://localhost:4100/#live`) and each phase change, and exits with
aoc's code. Tell the engineer what is running, the URL and how long it
takes, once. A hook refuses a bare `./bin/aoc ab|run` and a second
concurrent run; `--plan` runs anywhere.

## 4. Read and decide

Read `findings.md` (or `report.md` for a single run) whole, once. Report
the verdict line, every signal that moved with its threshold, the gate
line, and the notebook URL if published (otherwise the local evidence path). Then decide, and say which of these it is:

| the result | the next step |
|---|---|
| gate failed on b | investigate now: the `investigator` agent on `results/<name>`; it returns the gate counts, the evidence versus the hypotheses and the conclusion text; `inconclusive` when the cause stays unknown |
| a signal regressed clearly | the `investigator` agent, then `aoc conclude` with its text; the `code-reader` agent when the mover is changed in b and the engineer wants the diff explained; offer the fix-and-rerun loop (`aoc ab --config results/<name>/aoc.yaml --only b` rebuilds b from the tree) |
| a delta within a point of its threshold | another round: the same line with `--runs 3`; do not investigate noise |
| clean | the next shape in the plan; after the last one, `aoc conclude --verdict pass` with the numbers and stop |
| improved | say so with the numbers, and whether the improvement is in the packages under test or elsewhere (a `-full` image, another process) |
| no profiles captured | say so; the window was shorter than a profiling period or the intake could not reach the trace-agent; lengthen `--duration` before trusting the cpu and memory sections |
| the run failed (exit other than 0 or 3) | `tail -40` the log, fix the cause, rerun the same line |

When both sides fail similarly, check shared workload/host effects before
attributing the result to the build. This is a hypothesis, not an exemption
from the delivery gate. Lower `--set rate` or try `--sequential` to isolate
contention; sequential runs introduce time drift. Do not call the change safe
just because both sides lost the same number of records.

## 5. Close

Every session ends with a conclusion on the run that decided it:

```bash
./bin/aoc conclude --results results/<name> --verdict pass|fail|inconclusive "what it turned out to be, with the numbers"
```

Use the saved config with `--compare-only` to refresh the local brief. For a
shared notebook, hand `results/<name>` and the conclusion to the
`notebook-author` agent; it follows [docs/notebooks.md](../../../docs/notebooks.md)
and records the returned URL. If MCP is unavailable it says the notebook is
unpublished; finish locally. Publication calls are separate from investigation.

Then two or three sentences to the engineer: the answer to the goal, the
runs that showed it (their `results/` paths and the notebook), and the
one thing you would run next if they want more certainty. A session
that stops short because of the host or time says what was not run.

## Rules

- `aoc.yaml` and `profiles/*.yaml` are settings, never a test. A new
  profile only when faults must change over time and no existing one
  fits; tell them the path.
- Token discipline: `findings.md` first, `compare.md` second, `jq` on
  `findings.json` or `report.json` for single values, `grep` for the
  agent log. Never `report.json`, `timeseries.csv`, `notebook.json` or
  pprof whole.
- Two Datadog MCP calls per regressed signal; the brief's **Next** line
  names the call. Load `datadog/profiling` before profiling calls,
  `datadog/metrics` before metric queries when available; surface the
  `visualizationLink` the tool returns, never a hand-built URL.
- Numbers from the brief, not from memory; a delta you did not read is
  not a finding. Separate what was measured from what is hypothesized.
- `--only b` takes the saved `--config results/<name>/aoc.yaml`; say that
  the control was reused, and rerun both sides if conditions changed.
