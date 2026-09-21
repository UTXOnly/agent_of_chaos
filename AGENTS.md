# agent_of_chaos (aoc) — instructions for coding agents

`aoc` is a Go CLI that A/B tests two Datadog Agent images on the same log
workload against a fake intake and answers one question: did the logs
pipeline regress between agent A and agent B? The answer is an
**investigation brief** — a verdict, four signals against their
thresholds behind a delivery gate, and the evidence that says why.
Datadog is the system of record (metrics, events, profiles, notebooks);
files on disk are the working copy. Read this file, then the skill for the
task at hand.

## The verdict

- Gate **delivery**: records lost, duplicated, orphaned or truncated on b
  fail the run. Out of order is reported and does not fail.
- **throughput** — received logs per second. A paced workload holds the
  rate, so it only fails when b falls short of what was generated; under a
  flat-out workload (`profiles/deterministic-bytes.yaml`) it is the
  agent's maximum.
- **saturation** — `pipeline utilization (busiest component)`, the max of
  the agent's `logs_component_utilization` ratio over components.
- **cpu** — `core agent process CPU avg`, `CPU seconds per 1M logs`.
- **memory** — `core agent RSS max`, `agent container anon mem avg`,
  `core agent Go heap in use`.

Each signal has its own percent in `thresholds` (aoc.yaml). Latency, tags,
compression, wire bytes, goroutines and log warnings are not findings:
they stay in `compare.md` until `--watch METRIC` promotes one back to the
headline.

## The workflow

1. **Configure once.** `aoc.yaml` is settings, not a test: the control
   image, shared env, the default workload, `source`, `runs`, per-signal
   `thresholds`. A test does not edit it.
2. **Launch** one test from the command line —
   `./bin/aoc ab --b IMAGE --focus "…" [--workload NAME] [--code PKG]…`
   (skill: `ab-test`, which turns what the engineer changed into that
   line). Both sides run at once, each in its own compose project, so a
   round takes ~(warmup + duration + drain + 2 min); `parallel: false` /
   `--sequential` doubles it. Run it in the background with output to a
   file and wait — do not poll. Exit 3 means the gate failed.
3. **Read** `results/<name>/findings.md` (skill: `investigate`). It is the
   whole brief: the verdict line, the signals table (signal, metric, a, b,
   Δ, threshold), the gate line, what we tested, one section per regressed
   signal (a sentence, the tables that name the cause, a `Next` line with
   the MCP call to make), the profiles, and the code — each mover located
   in the agent's source, packages under test first, with whether its file
   changed between the two builds. ~5–10 KB.
4. **Dig** only where the brief points: the function at `file:line` in the
   agent checkout (`source:` in aoc.yaml; `../datadog-agent` is found on
   its own) for a cpu/memory mover that is new or changed, and the Datadog
   MCP with the exact filters the brief gives. Two calls per regressed
   signal is the budget.
5. **Conclude** with `./bin/aoc conclude --results results/<name> "…"`:
   what the difference is, where in the code, and what to change. It
   becomes an event next to the test's data and the first cell of the
   notebook.

## What to read, and what not to

| want | read | never |
|---|---|---|
| the verdict and why | `results/<name>/findings.md` | the whole `report.json` (100 KB+), `timeseries.csv`, `*.pprof` |
| the signals or the gate alone | `jq '.signals' results/<name>/findings.json`, `jq '.gate' …` | |
| one number | `jq '.resources' results/<name>/<side>/report.json` (paths in `.claude/rules/results.md`) | |
| the agent's log | `grep -c '| ERROR |' results/<name>/<side>/agent.log`; the brief already lists the top messages | `cat agent.log` (thousands of lines) |
| every metric, not just the signals | `results/<name>/compare.md` | |
| identities, windows, focus, notebook URL | `results/<name>/ab.json` | |
| a chart or flame graph | the notebook URL in `ab.json`, or the MCP | constructing profiler URLs by hand |

## Commands

```
make build                     # ./bin/aoc
make test                      # go test ./...
make vet                       # go vet + gofmt check
./bin/aoc ab --b IMAGE [--a IMAGE] [--workload NAME|PATH] [--focus TEXT] [--code PKG]...
              [--watch METRIC]... [--b-env K=V]... [--env K=V]...
              [--runs N] [--name NAME] [--duration D] [--sequential] [--plan]
./bin/aoc ab --only b | --compare-only        # re-run b and reuse a; re-render from disk
./bin/aoc conclude --results results/<name> "text" [--verdict pass|fail|inconclusive]
./bin/aoc profiles                                         # the workloads and what they stress
./bin/aoc run --profile profiles/<w>.yaml --name <run>     # one side, no comparison
./bin/aoc notebook --results results/<run>                 # per-run notebook
```

Docker is required for `ab`/`run`. `.env` holds `DD_API_KEY` (never print
or commit it) and optionally `DD_APP_KEY` (notebook creation) and
`DD_SUBDOMAIN`. Without `DD_APP_KEY`, `notebook.json` is written and can be
created with the MCP's `create_datadog_notebook`.

## Conventions

- Datadog is the record: results go there as `aoc.*` metrics, `source:aoc`
  events, the agent's profiles and notebooks. Never build a local UI or
  dashboard; explore with the Datadog MCP.
- Tags: `run:<name>-<side>[-<round>]` per run, `experiment:<name>` and
  `variant:<side>` across the test, on the intake's metrics/events and on
  the agent's own metrics and profiles (`service:datadog-agent`).
- Profiles: the agent's continuous-profiler uploads are tee'd by the intake
  — kept under `<side>/profiles/` for the local diff and forwarded to
  Datadog unchanged. Only the core agent is profiled by default. The
  profiler's tags carry each build's commit; the brief uses them for the
  code section and the agent checkout. Movers inside the packages under
  test (`--code`, default `pkg/logs`, `comp/logs`, `comp/logs-library`)
  come first. The notebook embeds the profiler's comparison view
  (`findings.CompareURL`: a beside b, one cell per profile type, then one
  per mover focused on that function) — extend that, never hand-build
  profiler URLs elsewhere.
- A test is one command line and a `--focus` sentence; the focus is the
  notebook's title and is carried in `findings.json` and `ab.json`.
- Keep experiments small (laptop or a t4g instance); the workload is the
  same for both sides, so absolute numbers matter less than the delta.

## Code map

| path | what |
|---|---|
| `internal/cli/ab.go`, `run.go`, `collect.go`, `conclude.go` | the commands; `executeRun` drives compose, measures, collects profiles/log |
| `internal/intake/` | the fake intake: ledgers, latency, `observe.go`/`procs.go` (docker stats, per-process top, telemetry scrape), `profiles.go` (profile tee), `emit.go` (`aoc.*` metrics) |
| `internal/report/` | `report.json` schema, `compare.go` (metric table, signal rows, per-process/profile tables) |
| `internal/prof/` | pprof merge/aggregate/diff (google/pprof) |
| `internal/findings/` | classify the gate and the signals, gather evidence per signal, render `findings.md` and the event |
| `internal/notebook/` | Datadog notebook cells (brief first, overlaid charts, table last) |
| `profiles/` | workloads; `aoc.yaml` names the default, `--workload` picks another |
| `docs/measurements.md`, `docs/datadog.md` | what every number means; what lands in Datadog |

Changing what is measured: see `.claude/rules/measurements.md` (loads when
you open those files).
