# agent_of_chaos (aoc) — instructions for coding agents

`aoc` is a Go CLI that A/B tests two Datadog Agent images on the same log
workload against a fake intake and writes an **investigation brief** —
which metrics regressed and the evidence that says why. Datadog is the
system of record (metrics, events, profiles, notebooks); files on disk are
the working copy. Read this file, then the skill for the task at hand.

## The workflow

1. **Configure** `aoc.yaml` (the control is `datadog/agent:7` = latest
   release; `b.image` is the build under test; `b.env` its feature flags).
   `./bin/aoc ab --plan` validates it.
2. **Run** `./bin/aoc ab` (skill: `ab-test`). A 10-minute window is the
   default and both sides run at once, each in its own compose project, so
   a run takes ~(warmup + duration + drain + 2 min) per round (`parallel:
   false` / `--sequential` doubles it). Run it in the background with
   output to a file and wait — do not poll.
3. **Read** `results/<name>/findings.md` (skill: `investigate`). It is the
   whole brief: what we tested, what differed (only rows that moved),
   where (per regressed topic: a one-sentence reading, the tables that
   name the cause, the profile movers with their source lines), the
   profiles, and the code — each mover located in the agent's source with
   whether its file changed between the two builds. ~5–10 KB.
4. **Dig** only where the brief points: the function at `file:line` in the
   agent checkout (`source:` in aoc.yaml; `../datadog-agent` is found on
   its own) for a CPU/memory mover that is new or changed, and the Datadog
   MCP with the exact filters the brief gives. Two or three calls per
   regression is the budget.
5. **Conclude** with `./bin/aoc conclude --results results/<name> "…"`:
   what the difference is, where in the code, and what to change. It
   becomes an event next to the test's data and the first cell of the
   notebook.

## What to read, and what not to

| want | read | never |
|---|---|---|
| the verdict and why | `results/<name>/findings.md` | the whole `report.json` (100 KB+), `timeseries.csv`, `*.pprof` |
| one number | `jq '.resources' results/<name>/<side>/report.json` (paths in `.claude/rules/results.md`) | |
| the agent's log | `grep -c '| ERROR |' results/<name>/<side>/agent.log`; the brief already lists the top messages | `cat agent.log` (thousands of lines) |
| the metrics table | `results/<name>/compare.md` | |
| identities, windows, notebook URL | `results/<name>/ab.json` | |
| a chart or flame graph | the notebook URL in `ab.json`, or the MCP | constructing profiler URLs by hand |

## Commands

```
make build                     # ./bin/aoc
make test                      # go test ./...
make vet                       # go vet + gofmt check
./bin/aoc ab [--plan|--only b|--compare-only|--duration 2m|--runs 3]
./bin/aoc conclude --results results/<name> "text" [--verdict pass|fail|inconclusive]
./bin/aoc run --profile profiles/<p>.yaml --name <run>     # one side, no comparison
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
  code section and the agent checkout. The notebook embeds the profiler's
  comparison view (`findings.CompareURL`: a beside b, one cell per profile
  type, then one per mover focused on that function) — extend that, never
  hand-build profiler URLs elsewhere.
- `threshold` (aoc.yaml, default 10 %) decides what is a finding. Lost,
  duplicated or reordered records always are.
- Keep experiments small (laptop or a t4g instance); the workload is the
  same for both sides, so absolute numbers matter less than the delta.

## Code map

| path | what |
|---|---|
| `internal/cli/ab.go`, `run.go`, `collect.go`, `conclude.go` | the commands; `executeRun` drives compose, measures, collects profiles/log |
| `internal/intake/` | the fake intake: ledgers, latency, `observe.go`/`procs.go` (docker stats, per-process top, telemetry scrape), `profiles.go` (profile tee), `emit.go` (`aoc.*` metrics) |
| `internal/report/` | `report.json` schema, `compare.go` (metric table, headline rows, per-process/profile tables) |
| `internal/prof/` | pprof merge/aggregate/diff (google/pprof) |
| `internal/findings/` | classify the headline, gather evidence per topic, render `findings.md` and the event |
| `internal/notebook/` | Datadog notebook cells (brief first, overlaid charts, table last) |
| `profiles/` | workloads; `aoc.yaml` points at one or inlines it |
| `docs/measurements.md`, `docs/datadog.md` | what every number means; what lands in Datadog |

Changing what is measured: see `.claude/rules/measurements.md` (loads when
you open those files).
