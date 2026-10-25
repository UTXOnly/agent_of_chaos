# aoc web UI

A local front end for `./bin/aoc`: compose a run or an A/B test as a form,
see the exact command line it becomes, check the plan, launch it, and watch
it live. One Node process, started and stopped with one command. Datadog
stays the record for analysis; this is for setting a run up and watching it.

```bash
make ui            # build if needed, start in the background, print the URL
make ui-status     # pid, port, the active run and its phase
make ui-stop       # stop; an active run is interrupted so aoc takes its stack down
ui/bin/aoc-ui run -- ab --b-src . --workload rotation-churn --name tailer-rotation   # launch through the UI, wait, exit with aoc's code
ui/bin/aoc-ui stop-run   # interrupt the active run; aoc takes its stack down
ui/bin/aoc-ui clean   # stop and remove ui/.state (run history and logs)
make ui-dev        # hot reload: vite on :5173, server on :4100, foreground
```

`aoc-ui run -- <aoc args>` is how Claude launches every run, so the person
who stated the goal can watch it at `/#live` while the agent waits for the
exit: it starts the server if needed, posts the command line, prints the
live URL and each phase change, and exits with aoc's own code (0 clean,
3 delivery gate failed, 130 interrupted). The command line is run as given;
`--name` is required with `--b-src`, since the server cannot know the
checkout's default name and `results/<name>` is where the brief lands.

`AOC_UI_PORT` changes the port (default 4100). The server binds to
127.0.0.1 only.

## What it does

- **Compose**: A/B or single experiment, the sides (checkout, image or the
  release), the workload from `profiles/`, `--set` and `--fault` chips with
  presets, the window presets from the `aoc` skill (quick look, decision,
  delta), `--env`/`--b-env`/`--code`/`--watch`, the focus sentence and the
  name. The command card is what the server runs; paste it into a shell to
  run it by hand. **Plan** runs the same line with `--plan`.
- **Live**: the phase track with aoc's own wall-time estimate, then
  **Inside the logs agent**: an interactive diagram of the agent's logs
  pipeline built only from metrics the agent exposes with telemetry on.
  One node per component that reports `logs_component_utilization`
  (processor and strategy per pipeline, sender, worker, destination), plus
  the tailers and the intake. Node fill is the component's utilization
  ratio; each node shows its buffered items and bytes. Edges are animated
  at their measured rate (written, `logs__decoded` per pipeline,
  `logs__processed`, payload flushes, HTTP sends, `logs__sent`); the
  retry loop from the intake back to the destination is `logs__retry_count`;
  a red sink under the tailer is `logs__bytes_missed`, the bytes lost to
  rotation before the tailer read them, which is where the intake's
  missing count comes from. A processor at 0% with a full channel while
  its strategy is saturated is marked **blocked**: the agent stops a
  component's busy clock while it waits to hand off, so that reading is
  correct, not idle. Hover a node for its live numbers; click a node: what it does, what to watch,
  sparklines of its ratio / items / bytes over the run, and the raw series
  behind it. Under the diagram, per side: eight gauges, a **why** list of
  rule-based sentences that name the number they came from, the counters,
  the generators. The values in those panels are 5-second means and the
  why lines hold for 15 seconds, so they can be read while the run moves.
  Then twelve one-axis charts with a and b overlaid
  (received vs written, e2e p99 with p50, rejected/s, backlog, CPU, RSS with
  Go heap, wire vs raw bytes, busiest utilization, bytes missed on
  rotation, retries with network errors, missing change/s, bytes held in
  the destination vs in the pipelines); fault spans shaded, hover for
  values. A **time cursor** sits under the phase track: live follows the
  newest sample; drag it, click a chart, or press *peak stress* to move to
  the most stressed second, and every panel shows that moment. The server
  keeps a finished run's samples (`ui/.state/samples/<id>.json`), so a run
  stays reviewable after it ends, opening at its peak. Stop sends SIGINT
  (SIGTERM after 90 s, then SIGKILL); a second launch queues.
- **Results**: what is on disk under `results/`, with the verdict, the gate
  line and the signal deltas; opens `findings.md`, `compare.md`,
  `report.md` and the Datadog notebook link.
- The header shows leftover aoc containers and a **clean up** button
  (`docker compose -p aoc|aoc-a|aoc-b down -v`), and a power button that
  stops the server.

## For agents

Everything the UI does is plain HTTP on the same port:

| call | effect |
|---|---|
| `GET /api/health` | `{ok, pid, busy, active: {id, name, kind, phase, phaseSince, resultsDir} \| null}` |
| `GET /api/profiles`, `GET /api/config` | the workloads, `aoc.yaml` and host checks |
| `POST /api/plan` | body: a `RunRequest` (`ui/shared/types.ts`); returns aoc's `--plan` output |
| `POST /api/runs` | enqueue a `RunRequest`, or `{argv: ["ab", "--b-src", ".", …]}` run as given; `GET /api/runs` lists; `GET /api/runs/:id` one record; `GET /api/runs/:id/log` the full log |
| `GET /api/runs/:id/wait?phase=P&timeout=60` | long poll: the record (plus `done`) once the phase leaves `P`, the run ends, or the timeout (max 120 s) passes |
| `POST /api/runs/:id/stop[?force=1]` | SIGINT (or SIGKILL) |
| `GET /api/events` | SSE: `hello`, `run`, `log`, `intake`, `docker` events |
| `GET /api/results`, `GET /api/results/:name/findings.md` | the working copy on disk |
| `POST /api/docker/cleanup` | compose down for every aoc stack (refused while a run is active) |
| `POST /api/shutdown` | interrupt the active run, exit |

State lives in `ui/.state/` (pid, server log, `runs.json`, one log per run);
it is gitignored and `aoc-ui clean` removes it.

## Layout

```
ui/server/   Hono on Node: index.ts (routes, SSE, shutdown), runner.ts (one aoc
             process at a time, phase from aoc's log lines), intake.ts (polls
             :8282/:8283 /harness/status, /harness/timeseries and
             /harness/telemetry once a second and digests the agent's series
             into stage rates, utilization, status codes, backlog), repo.ts
             (profiles, aoc.yaml, results, argv, docker)
ui/shared/   types shared with the browser
ui/src/      Vite + React 19 + Tailwind 4 + shadcn-style components
ui/bin/      aoc-ui lifecycle script
```
