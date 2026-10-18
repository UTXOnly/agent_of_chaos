---
name: ab-test
description: Run an A/B test of two Datadog Agent images with aoc. Turn what the engineer changed into one `aoc ab` command line (image or checkout, workload, --set and --fault tuning, packages to watch), run it in the background, and report the verdict from the brief. Use when asked to test, compare, benchmark or A/B an agent build against the release or against main.
---

# A/B test an agent build

The question is always the same: did the logs pipeline regress between
agent A and agent B? The engineer says what they changed; you write one
command line, run it, and read the brief.

`aoc.yaml` is settings: control image, shared env, default workload,
`source`, `runs`, per-signal `thresholds`. A test never edits it.

## 1. The command line

```
./bin/aoc ab --b IMAGE | --b-src . --focus "<what they said, verbatim>" [--workload NAME] [--set K=V] [--fault K=V] [--code PKG]...
```

> "test datadog/agent-dev:my-branch-py3, I changed the tailer's rotation handling"

```bash
./bin/aoc ab --b datadog/agent-dev:my-branch-py3 \
  --focus "I changed the tailer's rotation handling" \
  --workload rotation-churn --code pkg/logs/tailers --code pkg/logs/launchers
```

> "I changed the sender's batching in my checkout, is it slower behind a slow intake?"

```bash
./bin/aoc ab --b-src . --a-src main \
  --focus "I changed the sender's batching in my checkout, is it slower behind a slow intake?" \
  --fault latency_ms=500,jitter_ms=200 --code comp/logs-library/sender
```

| flag | when |
|---|---|
| `--b IMAGE` | the build under test is an image. A CI build of a branch is `datadog/agent-dev:<branch>-py3` |
| `--b-src .` | the build under test is their checkout: "my changes", "my branch", "what I have locally", no CI image named. `.` is the working tree, uncommitted changes included; a ref (`my-branch`, a commit) builds that instead. One of `--b` / `--b-src`, never both |
| `--a-src main` | with `--b-src`, whenever the question is about their change rather than about main: both sides are then built the same way and only the branch differs. Without it the control is the release image the build sits on |
| `--a IMAGE` | only to compare two named versions; the default control is `datadog/agent:7`, the latest release |
| `--focus TEXT` | whenever they said why: their sentence, verbatim. It becomes the notebook title |
| `--workload NAME` | from the table below; a name in `profiles/`, or a path. `./bin/aoc profiles` lists them with their numbers |
| `--set K=V` | a number they named: `rate=20000` for every generator, `json.streams=16` for one, `app.replicas=30`, any `aoc generate` flag. Both sides get it |
| `--fault K=V` | an intake fault for the whole window, both sides: `latency_ms=500`, `drop_rate=0.05`, `error_rate=0.2,error_status=429`, `outage=true`, `read_bps=500000`. The `experiment` skill has the full table |
| `--code PKG` | the package they touched, repeatable. Default: `pkg/logs`, `comp/logs`, `comp/logs-library` |
| `--b-env K=V` | the feature flag under test: b only |
| `--env K=V` | an agent setting both sides need for the test to be fair |
| `--watch METRIC` | a `compare.md` metric they asked about (`e2e latency p99`, `tag bytes per log`, `received wire B/s`) back on the headline |
| `--name NAME` | only when they name the run; the default is `<b tag>-<workload>` |

### What changed, which workload, which packages

| what they changed | `--workload` | `--code` |
|---|---|---|
| tailer, file discovery, rotation | `rotation-churn`, then `chaos-restarts` | `pkg/logs/tailers`, `pkg/logs/launchers` |
| decoder, multiline, framing, parsing | `multiline-heavy` | `pkg/logs/internal/decoder`, `pkg/logs/internal/framer`, `pkg/logs/internal/parsers` |
| processor, tags, message, encoding | `baseline`, `tag-heavy-fleet` for tag work | `comp/logs-library/processor`, `pkg/logs/message` |
| compression kind or level | `high-compression` | `comp/logs-library/sender`, `pkg/logs/message` |
| sender, destination, batching, retry, backoff | `intake-outage`, then `flaky-network` | `comp/logs-library/sender`, `comp/logs-library/client` |
| container logs, docker launcher, autodiscovery | `tag-heavy-fleet` | `pkg/logs/launchers/container`, `comp/logs/agent` |
| scheduling, sources, integrations | `baseline` | `pkg/logs/schedulers`, `pkg/logs/sources`, `comp/logs/agent` |
| raw throughput, or "just a version bump" | `deterministic-bytes`, then `baseline` | the default |

Two workloads means two runs: do the first, report it, and offer the
second rather than queueing both. `--code` matches by path prefix; builds
from before the logs-library move keep the processor, sender and client
under `pkg/logs/`, so pass those paths too if the brief's code table comes
back empty.

### Rounds and duration

`aoc.yaml` already says `runs: 2`, so the brief is the median of two
rounds. Pass `--runs` only to go up (`--runs 3` when a delta is likely to
land within a point of its threshold) or down (`--runs 1 --duration 2m`,
a smoke that proves the images pull and the stack comes up).

`--plan` validates and prints the plan without running anything: the
resolved generators and faults, the sides, the rounds. Use it whenever you
had to guess. It warns when one image is `-full` or `-jmx` and the other
is not, which moves memory and CPU on its own. With `--b-src` it shows the
checkout, the branch, the commit and how many uncommitted files the build
will include; check that against what they said they changed.

### Building from the checkout

`--b-src` / `--a-src` compile the agent from the checkout (`source:` in
`aoc.yaml`, `DATADOG_AGENT_SRC`, else `../datadog-agent`) inside the
agent's Linux dev-env image and layer it on the release image, whose
embedded Python and integrations it keeps, so the Python checks work. The
image is `aoc/agent-dev:<branch>[-dirty]`, so the experiment defaults to
`<branch>[-dirty]-<workload>`. The first build pulls a 3 GB builder image
and takes several minutes; later ones take about a minute. The build's
output is `results/<name>/build-<side>.log`: `tail -30` it when a build
fails, never `cat` it. `./bin/aoc build --src .` builds the image on its
own and prints its tag.

## 2. Run it

```bash
make build                                    # if ./bin/aoc is missing or stale
mkdir -p results
./bin/aoc ab --b … --focus "…" > results/ab.log 2>&1    # the line from step 1
```

Start it in the background, output to a file, and wait for the process to
exit. Do not poll, and do not touch Docker meanwhile: the run owns the
`aoc-a` and `aoc-b` compose projects. Wall time is
`runs × (warmup + duration + drain + ~2 min)`, doubled by `--sequential`.

Exit codes: 0 delivery gate passed (performance signals may still regress); 3 the delivery gate failed (b lost, duplicated,
orphaned or truncated records); 130 interrupted; anything else means a run
failed: `tail -40 results/ab.log`.

## 3. Report

The run's last lines name the brief (`tail -5 results/ab.log`); the
experiment defaults to `<b tag>-<workload>`, so the first example above
writes `results/my-branch-py3-rotation-churn/`. Read that `findings.md`
and give a short answer to the original question using:

1. the verdict line;
2. every signal that moved, from the signals table: signal, metric, a, b,
   delta, and the threshold it was judged against;
3. the gate line: pass, or what b lost, duplicated, orphaned or truncated;
4. the notebook URL if published, otherwise the local results path;
5. one next step: the `investigate` skill when a signal regressed, another
   round when a delta sits on its threshold, the next relevant shape when the goal requires it, or a scoped conclusion.

If the brief says no profiler uploads were captured, the window was
shorter than a profiling period or the intake could not reach the
trace-agent. Say so, and expect the profile and code sections to be empty.

## Variations

- `--config results/<name>/aoc.yaml --only b` after rebuilding the dev image: re-runs b, reuses a. With
  `--b-src .` it rebuilds b from the tree as it now is first.
- `--config results/<name>/aoc.yaml --compare-only` re-renders the brief and local notebook evidence from disk, after
  a change to `thresholds`, `--watch` or `--code`.
- `--sequential` when the host cannot carry both stacks at once.

A profile mover suggests a cause; it does not establish one. Shared failures do
not cancel the gate. A reused control can be stale: rerun both sides when host,
image or workload conditions changed. If publishing the result, follow
[docs/notebooks.md](../../../docs/notebooks.md); author the analysis through MCP
from the evidence, not by uploading all generated Markdown.
