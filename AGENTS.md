# agent_of_chaos (aoc): instructions for coding agents

`aoc` is a local development loop for the Datadog Agent's logs pipeline,
driven by a coding agent. The engineer states a goal (a change in their
checkout, a flag, a version, a workload shape they are worried about); you
shape the environment, run the experiment in Docker, read the brief, say
what moved and in which function, and propose the next run. It
provides a faster answer about the change in progress, on a laptop or a t4g
instance. SMP also supports local runs: the distinction is the logs-specific
hypothesis → tailored scenario → evidence → next edit workflow. Short local
runs do not establish release readiness or statistical significance.

The harness puts Datadog Agents in hard log situations (intake faults
such as latency, dropped connections, 429/5xx and outages; hard workloads
such as rotation storms, crashing writers, multiline floods, spikes and
tag-heavy container fleets) against a fake intake that counts every
record, then reports what was lost, duplicated or slowed down. Datadog is
the system of record (metrics, events, profiles, notebooks); `results/`
is the working copy.

## Leading the loop

```
goal → plan → run → read → decide → (next run | investigate → conclude | done)
```

You lead. After every run, report the verdict and say what you would run
next and why. The `aoc` skill is the full procedure; its substance:

- **Plan before running.** Two to four runs in order, each one line with
  its purpose and wall time. A quick look is `--runs 1 --duration 2m`; a
  decision is the config's window and rounds; a delta on its threshold is
  `--runs 3`. `--plan` whenever anything was guessed, always with
  `--b-src`.
- **Shape the environment from the goal**, on the command line:
  `--workload` for the area touched, `--set` for the shape, `--fault` for
  the intake, `--env` / `--b-env` for the agent's settings, `--code` for
  the packages under test, `--watch` for the metric the test is about.
- **One run at a time**, in the background, output to a file, wait for
  the exit. The host carries one A/B, not two.
- **Decide from the result.** Gate failed: investigate now. A signal
  regressed: `investigate`, then `aoc conclude`, then offer
  `aoc ab --only b` after their fix. Within a point of the threshold:
  another round, not an investigation. Clean: the next shape, then
  `conclude --verdict pass`. Both sides moved the same way: attribution is uncertain; check the host,
  workload and shared behavior. Do not erase a failed delivery gate.
- **Close with a conclusion** on the run that decided the goal, with the
  numbers, and tell the engineer the one thing you would run next.

## Which skill

| the engineer wants | skill | command |
|---|---|---|
| a goal taken to a conclusion ("I'm working on …, help me test it", "is it ready", "keep going") | `aoc` | a plan, then the rows below in order |
| to see how one agent behaves in a situation, or to find a knee ("what happens when the intake is slow", "how fast can it go here") | `experiment` | `aoc run --profile … [--set K=V] [--fault K=V]`, one per value for a sweep |
| to know whether a build regressed against the release or main, optionally under a situation or with a flag | `ab-test` | `aoc ab --b IMAGE \| --b-src . [--a-src main] [--workload …] [--set K=V] [--fault K=V] [--b-env K=V] --focus "…"` |
| to know why an A/B result came out the way it did | `investigate` | reads `findings.md`, then `aoc conclude` |

## Shaping an experiment

- A workload is a profile in `profiles/` (`aoc profiles` lists them with
  their numbers; `profiles/README.md` has the format). Pick the closest.
- Any generator setting changes on the command line: `--set rate=20000`
  for every generator, `--set json.streams=16` for one, `--set
  app.replicas=30`; the keys are `aoc generate` flags, plus `format`,
  `output`, `replicas`. A fault for the whole window is `--fault K=V`.
  Neither needs a file.
- A new file only when a shape is worth keeping or faults must change over
  time (a `faults:` timeline): copy the closest profile, edit, tell the
  user the path. `aoc run --plan` / `aoc ab --plan` print the resolved
  workload without starting anything.
- Never put a test in `aoc.yaml`: it holds what every test shares (control
  image, shared env, default workload, `runs`, per-signal `thresholds`,
  the agent checkout).
- `--b-src .` builds the image under test from the agent checkout
  (`source:` in `aoc.yaml`, `DATADOG_AGENT_SRC`, else `../datadog-agent`),
  uncommitted changes included; `--a-src main` builds the control the same
  way so only the branch differs. About a minute per build after the first.

Run `aoc run` and `aoc ab` in the background with output to a file and wait
for the exit; do not poll. Both sides of an A/B run at once, so a round
takes warmup + window + drain + about 2 minutes. Exit 3 means records were
lost or duplicated.

## The A/B verdict

- Gate **delivery**: records lost, duplicated, orphaned or truncated on b
  fail the run. Out of order is reported and does not fail.
- **throughput**: received logs per second. A paced workload holds the
  rate, so it only fails when b falls short of what was generated. With
  `deterministic-bytes` it is a window average over a fixed record budget;
  confirm sustained demand before claiming capacity.
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
| what a built side came from (commit, dirty files, base) | `jq '.sides[].build' results/<name>/ab.json`; the build's output is `results/<name>/build-<side>.log` (`tail`, never `cat`) | |
| the workload as it ran | `results/<name>/<side>/profile.yaml` | |
| a chart or flame graph | the notebook URL in `ab.json`, or the MCP | constructing profiler URLs by hand |
| how a session goes, turn by turn | `docs/sessions.md` | |

## Commands

```
make build                     # ./bin/aoc
make test                      # go test ./...
make vet                       # go vet + gofmt check
./bin/aoc ab --b IMAGE | --b-src .|REF [--a IMAGE | --a-src REF] [--workload NAME|PATH]
              [--set K=V]... [--fault K=V]... [--env K=V]... [--b-env K=V]...
              [--focus TEXT] [--code PKG]... [--watch METRIC]...
              [--runs N] [--name NAME] [--duration D] [--sequential] [--plan]
./bin/aoc ab --config results/<name>/aoc.yaml --only b | --compare-only
./bin/aoc build [--src .|REF] [--base IMAGE]  # just the image: aoc/agent-dev:<branch>[-dirty]
./bin/aoc conclude --results results/<name> "text" [--verdict pass|fail|inconclusive]
./bin/aoc profiles [show NAME]                # the workloads; one resolved
./bin/aoc run --profile NAME|PATH [--set K=V]... [--fault K=V]... [--env K=V]... --name <run> [--plan]
./bin/aoc compare results/x results/y         # diff single runs (a sweep's steps)
./bin/aoc notebook --results results/<run>    # prepare evidence for MCP authoring
./bin/aoc notebook --results results/<run> --link URL  # save MCP-returned URL
```

Docker is required for `ab`/`run`. `.env` holds `DD_API_KEY` (never print
or commit it) and optionally `DD_SUBDOMAIN`. `DD_APP_KEY` is no longer used.
The coding agent writes the analysis and publishes through Datadog MCP;
`notebook.md` indexes reusable evidence cells in `notebook.json`.
Follow [docs/notebooks.md](docs/notebooks.md) when publishing or updating.
Keep the local conclusion usable when MCP is unavailable; state that the
notebook is unpublished. `aoc conclude` saves the conclusion and posts an
event; use the saved config with `--compare-only` to refresh the local brief.

## Analysis quality

Answer the engineer's question in one screen: scope and verdict, delivery
counts, deciding deltas with A/B values, evidence versus hypothesis, limitations,
and one next experiment. A profile mover is a lead, not proof of cause. Shared
losses still fail the gate. An unknown cause can end in `inconclusive`; do not
invent one to satisfy the loop. Use the saved `results/<name>/aoc.yaml` for
`--only b` / `--compare-only`. A reused control is useful for iteration but
needs refreshing when conditions change. Notebook authoring calls are separate
from the investigation budget.

## Conventions

- Datadog is the record: results go there as `aoc.*` metrics, `source:aoc`
  events, the agent's profiles and notebooks. Never build a local UI or
  dashboard; explore with the Datadog MCP.
- Tags: `run:<name>-<side>[-<round>]` per run, `experiment:<name>` and
  `variant:<side>` across the test, on the intake's metrics/events and on
  the agent's own metrics and profiles (`service:datadog-agent`).
- Profiles: the agent's continuous-profiler uploads are tee'd by the
  intake, kept under `<side>/profiles/` for the local diff and forwarded to
  Datadog unchanged. Only the core agent is profiled by default. The
  profiler's tags carry each build's commit; the brief uses them for the
  code section and the agent checkout. Movers inside the packages under
  test (`--code`, default `pkg/logs`, `comp/logs`, `comp/logs-library`)
  come first. The notebook evidence includes the profiler's comparison view
  (`findings.CompareURL`: a beside b, one cell per profile type, then one
  per mover focused on that function). Extend that; never hand-build
  profiler URLs elsewhere.
- A test is one command line and a `--focus` sentence; the focus is the
  notebook's title and is carried in `findings.json` and `ab.json`.
- Keep experiments small (laptop or a t4g instance); the workload is the
  same for both sides, so absolute numbers matter less than the delta.

## Code map

| path | what |
|---|---|
| `.claude/skills/` | `aoc` (lead the loop), `ab-test`, `experiment`, `investigate` |
| `internal/cli/profile.go` | the workload model: profile schema, `--set`, `--fault`, the compose services rendered per generator |
| `internal/cli/ab.go`, `run.go`, `collect.go`, `conclude.go`, `build.go` | the commands; `executeRun` drives compose, measures, collects profiles and the log |
| `internal/agentbuild/` | `aoc build` / `--b-src`: a scratch worktree of the checkout, the release's embedded Python extracted, the agent compiled in `datadog/agent-dev-env-linux` against it, the result layered on the release |
| `internal/gen/` | the log generator: pacing, rotation, content, modes |
| `internal/intake/` | the fake intake: `faults.go`, ledgers, latency, `observe.go`/`procs.go` (docker stats, per-process top, telemetry scrape), `profiles.go` (profile tee), `emit.go` (`aoc.*` metrics) |
| `internal/report/` | `report.json` schema, `compare.go` (metric table, signal rows, per-process/profile tables) |
| `internal/prof/` | pprof merge/aggregate/diff (google/pprof) |
| `internal/findings/` | classify the gate and the signals, gather evidence per signal, render `findings.md` and the event |
| `internal/notebook/` | local evidence cells and compact MCP handoff; the coding agent authors the analysis |
| `profiles/` | workloads; `aoc.yaml` names the default, `--workload` picks another |
| `docs/sessions.md`, `docs/measurements.md`, `docs/datadog.md`, `docs/reference.md` | sessions turn by turn; what every number means; what lands in Datadog; the pieces by hand, faults, gotchas |

Changing what is measured: see `.claude/rules/measurements.md` (loads when
you open those files).
