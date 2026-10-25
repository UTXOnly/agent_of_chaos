@AGENTS.md

## Claude Code specifics

- You lead the session: after every run, report what moved and propose
  the next run. The `aoc` skill is the procedure (goal → plan → run →
  read → decide → conclude); it delegates single runs to `experiment`
  (one agent in a situation, or a sweep) and `ab-test` (two builds, or a
  flag on one), and finished results to `investigate` (from `findings.md`
  to a conclusion with a bounded number of Datadog MCP calls). Use them
  for those tasks; `docs/sessions.md` shows what a good session looks
  like.
- Datadog MCP: load the `datadog/profiling` skill before profiling calls,
  and `datadog/metrics` before metric queries when those skills are available.
  Surface the `visualizationLink` the profiling tools return instead of building URLs.
- Launch runs as `ui/bin/aoc-ui run -- ab|run <flags> --name <name> >
  results/<name>.log 2>&1` in the background and wait for the task
  notification, never a `sleep` loop. Hooks in `.claude/settings.json`
  refuse a bare `./bin/aoc ab|run`, a second concurrent run and whole-file
  reads of heavy artifacts, and ask before edits to `aoc.yaml` or an
  existing profile.
- The session starts with a briefing from `.claude/hooks/session-context.sh`
  (host, UI, active run, agent checkout, recent results); the `status` skill
  reruns it, `stop` ends a run cleanly.
- Delegate to keep the loop's context small: the `investigator` agent for a
  failed gate or a regressed signal (it returns the conclusion text), the
  `code-reader` agent for a mover's diff, the `notebook-author` agent to
  publish through Datadog MCP after `aoc conclude`.
