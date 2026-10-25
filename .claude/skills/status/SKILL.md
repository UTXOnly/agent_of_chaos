---
name: status
description: Where things stand in an aoc session: what is running and in which phase, whether the host can carry a run (Docker, bin/aoc, the UI, leftover stacks, the agent checkout's branch and dirty files), and the recent results with their verdicts and whether each has a conclusion. Use for "status", "what's running", "where were we", "is anything running", "what did we find so far", or at the start of a resumed session before proposing a run.
---

# Where things stand

One command, then a short reading:

```bash
.claude/hooks/session-context.sh
```

It prints the repo branch, whether `bin/aoc` is stale, Docker and leftover
`aoc*` containers, the UI and its active run with phase, the agent
checkout `--b-src .` would build, whether `DD_API_KEY` is set, and the six
newest results: A/B (gate, regressed signals, concluded or not, focus) or
single run (delivery). For the active run's own log:

```bash
ui/bin/aoc-ui status
curl -s localhost:4100/api/runs | jq -r '.[-1] | .tail[-15:][]'
```

Report in this order, only what is not clean:

1. **Running**: the run's name, kind, phase and how long it has been in it
   (a `window` phase lasts the `--duration`; `building` can take a minute
   per side, several the first time). Never start another run meanwhile.
2. **Blockers**: Docker down, `bin/aoc` stale (`make build`), leftover
   containers (`curl -X POST localhost:4100/api/docker/cleanup`, only when
   no run is active), no `DD_API_KEY` (results stay local), no agent
   checkout (`--b-src` impossible).
3. **Unfinished**: results with a gate failure or regression and no
   `conclusion.md`. Offer the `investigator` agent for one of them.
4. **Then propose the next run**, in one line with its wall time, from the
   goal the engineer last stated. If no goal is on record, ask for it in
   one sentence.
