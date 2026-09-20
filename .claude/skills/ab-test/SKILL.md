---
name: ab-test
description: Run an A/B test of two Datadog Agent images with aoc (configure aoc.yaml, run it in the background, read the findings brief). Use when asked to test, compare, benchmark or A/B a dev agent build against the release.
---

# A/B test two agent builds

## 1. Configure

Edit `aoc.yaml` (every key is documented in the file). The minimum:

```yaml
profile: profiles/baseline.yaml     # the workload; see `./bin/aoc profiles`
duration: 10m                       # ≥ 5 profiling periods per side; 2m for a smoke
a: { image: datadog/agent:7 }       # the control: latest release, re-pulled
b:
  image: datadog/agent-dev:<branch>-py3   # or a local image with `pull: never`
  env: { DD_SOME_FLAG: "true" }           # the feature under test, b only
```

Validate without running: `./bin/aoc ab --plan`. Fix errors it reports
(unknown keys, missing `b.image`, same side names).

Pick `runs: 2` (or `--runs 2`) when the result will decide something:
rounds alternate a, b, a, b and the brief uses medians.

## 2. Run

Expected wall time: `2 × runs × (warmup + duration + drain + ~2 min)`.
Start it in the background, output to a file, and wait for the process to
exit — do not poll every minute, and do not run anything else against
Docker meanwhile (the run owns the compose stack).

```bash
mkdir -p results && ./bin/aoc ab > results/ab.log 2>&1
```

Exit codes: 0 ok, 3 = records were lost or duplicated on some side (the
brief says which), 130 interrupted, anything else = a run failed
(`tail -40 results/ab.log`).

## 3. Read the brief

```bash
cat results/<name>/findings.md
```

Report to the user, in this order: the **Verdict** lines; each
**Regression** with the one or two evidence rows that explain it (e.g. "anon
memory +190 %, all of it in a new `python3` process; core agent RSS and heap
flat"); the improvements in one line; the notebook URL from `ab.json`.

If there are regressions, continue with the `investigate` skill. If the
brief says "No profiler uploads were captured", the window was shorter than
a profiling period or the intake could not reach the trace-agent — say so.

## Variations

- `./bin/aoc ab --only b` after rebuilding the dev image: re-runs b, reuses a.
- `./bin/aoc ab --compare-only` re-renders the brief/notebook from disk (after
  editing `threshold`, or the findings code).
- `./bin/aoc ab --duration 2m --name smoke` to check a config end to end.
- Other workloads: `profiles/*.yaml` (`deterministic-bytes` for
  byte-identical input, `rotation-churn`, `flaky-network` with faults…).
