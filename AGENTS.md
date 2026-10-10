# agent_of_chaos (aoc): instructions for coding agents

`aoc` puts Datadog Agents in hard log situations (intake faults like
latency, dropped connections, 429/5xx and outages, and hard workloads like
rotation storms, crashing writers, multiline floods and spikes) against a
fake intake that counts every record. Then it reports what was lost,
duplicated or slowed down. Datadog is the system of record (metrics,
events, profiles, notebooks); `results/` is the working copy.

## Which skill

| the user wants | skill | command |
|---|---|---|
| to see how an agent behaves in a situation ("what happens when the intake is slow") | `experiment` | `aoc run --profile … [--fault K=V]` |
| to know whether their build regressed against the release, optionally under a situation | `ab-test` | `aoc ab --b IMAGE --workload … [--fault K=V] --focus "…"` |
| to know why an A/B result came out the way it did | `investigate` | reads `findings.md`, then `aoc conclude` |

Faults are `--fault` flags for the whole window, or a `faults:` timeline in
a profile when they change over time. Workloads are profiles in
`profiles/`. A new situation is a new profile file, never an edit to
`aoc.yaml`, which holds settings shared by every test (control image,
shared env, default workload, `runs`, per-signal `thresholds`).

Run `aoc run` and `aoc ab` in the background with output to a file and wait
for the exit; don't poll. Exit 3 means records were lost or duplicated.

## The A/B verdict

- Gate **delivery**: records lost, duplicated, orphaned or truncated on b
  fail the run. Out of order is reported and does not fail.
- **throughput**: received logs per second. A paced workload holds the
  rate, so it only fails when b falls short of what was generated. Under
  `deterministic-bytes` it is the agent's maximum.
- **saturation**: the max of the agent's `logs_component_utilization`
  ratio over components.
- **cpu**: `core agent process CPU avg`, `CPU seconds per 1M logs`.
- **memory**: `core agent RSS max`, `agent container anon mem avg`,
  `core agent Go heap in use`.

Latency, tags, compression, wire bytes, goroutines and log warnings stay in
`compare.md` unless `--watch METRIC` promotes one to the headline.
`findings.md` gives each regressed signal a `Next` line with the MCP call
to make. Two calls per regressed signal is the budget.

## What to read, and what not to

| want | read | never |
|---|---|---|
| a single run's outcome | `results/<name>/report.md`, delivery section first | |
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
              [--watch METRIC]... [--b-env K=V]... [--env K=V]... [--fault K=V]...
              [--runs N] [--name NAME] [--duration D] [--sequential] [--plan]
./bin/aoc ab --only b | --compare-only        # re-run b and reuse a; re-render from disk
./bin/aoc conclude --results results/<name> "text" [--verdict pass|fail|inconclusive]
./bin/aoc profiles                                         # the workloads and what they stress
./bin/aoc run --profile profiles/<w>.yaml [--fault K=V]... --name <run>   # one agent, no comparison
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
| `internal/intake/` | the fake intake: `faults.go` (fault fields and validation), ledgers, latency, `observe.go`/`procs.go` (docker stats, per-process top, telemetry scrape), `profiles.go` (profile tee), `emit.go` (`aoc.*` metrics) |
| `internal/report/` | `report.json` schema, `compare.go` (metric table, signal rows, per-process/profile tables) |
| `internal/prof/` | pprof merge/aggregate/diff (google/pprof) |
| `internal/findings/` | classify the gate and the signals, gather evidence per signal, render `findings.md` and the event |
| `internal/notebook/` | Datadog notebook cells (brief first, overlaid charts, table last) |
| `profiles/` | workloads; `aoc.yaml` names the default, `--workload` picks another |
| `docs/measurements.md`, `docs/datadog.md`, `docs/reference.md` | what every number means; what lands in Datadog; the pieces by hand, faults, gotchas |

Changing what is measured: see `.claude/rules/measurements.md` (loads when
you open those files).
