---
paths:
  - "aoc.yaml"
  - "aoc.example.yaml"
  - "profiles/**"
---

# Settings are not tests

- `aoc.yaml` is what every test on this checkout shares: the control
  image, the window, the rounds, the thresholds, the agent checkout. A test
  is a command line: `--workload`, `--set`, `--fault`, `--env`, `--b-env`,
  `--duration`, `--runs`, `--threshold`, `--code`, `--watch`, `--focus`.
  Edit `aoc.yaml` only when the engineer asks to change what every test
  shares; the hook asks before any edit.
- A profile in `profiles/` is a workload worth keeping. Tune it from the
  command line (`--set rate=…`, `--fault latency_ms=…`). A new file only
  when a shape is worth keeping or faults must change over time (a
  `faults:` timeline): copy the closest profile, keep its format
  (`profiles/README.md`), add it to the table in `docs/workloads.md`, and
  tell the engineer the path. Never edit an existing profile for one run.
- `aoc run --plan` / `aoc ab --plan` show the resolved workload; a profile
  change is checked that way before it is used.
