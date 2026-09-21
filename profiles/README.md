# Experiment profiles

A profile is everything `aoc run` needs to turn the compose stack into one
measured, reproducible experiment: the workload for each generator, the agent
image/config, a fault timeline for the intake, and the timing of the measured
window. Copy one, change a few numbers, commit it next to your results.

```yaml
name: my-experiment          # results/<name>/ unless --name overrides
description: what you are trying to learn
duration: 3m                 # measured window
warmup: 20s                  # after generators start, before the window opens (agent cold start)
drain: 45s                   # after generators stop: wait for the pipeline to flush before the final report

agent:
  image: datadog/agent:7     # optional; --agent-image / DD_AGENT_IMAGE win
  env:                       # any DD_* setting for the agent container
    DD_LOGS_CONFIG_AUTO_MULTI_LINE_DETECTION: "true"

generators:                  # compose service → generator settings (flag names, no AOC_ prefix needed)
  gen-plain: { streams: 16, rate: 20000, multiline-rate: 0.1 }
  gen-json:  { streams: 8,  rate: 5000 }
  gen-stdout: { enabled: false }   # gen-stdout is off unless enabled

faults:                      # offsets from the start of the measured window
  - at: 60s
    set: { error_rate: 0.5, error_status: 503, note: "intake 503 storm" }
  - at: 90s
    clear: true
```

Run one:

```bash
aoc run --profile profiles/baseline.yaml --agent-image datadog/agent:7 --name baseline
aoc run --profile profiles/baseline.yaml --agent-image datadog/agent-dev:my-build --name candidate
aoc compare results/baseline results/candidate
```

Or A/B two agent images on it — `aoc ab --workload baseline` takes the name
of a profile here, or a path to one elsewhere. Without `--workload`, `aoc ab`
runs the one `aoc.yaml` points at (`profile:`), which can also be written
inline there.
