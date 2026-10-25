# Workload profiles

A profile is one experiment shape: the log sources, the intake's fault
timeline, the agent's settings and the measured window. `aoc run --profile`
and `aoc ab --workload` take a name here or a path anywhere. `aoc profiles`
lists them; `aoc profiles show NAME` prints one with its defaults filled in.

```yaml
name: my-experiment          # results/<name>/ unless --name says otherwise
description: what this stresses, in a sentence or two
duration: 3m                 # the measured window
warmup: 20s                  # after the generators start, before the window opens
drain: 45s                   # after they stop: wait for the pipeline to flush (default 45s)

agent:                       # optional
  image: datadog/agent:7     # --agent-image / --b wins
  env:                       # any DD_* setting
    DD_LOGS_CONFIG_AUTO_MULTI_LINE_DETECTION: "true"

generators:                  # one container per entry (per replica)
  plain: { streams: 8, rate: 5000 }
  json:  { format: json, streams: 8, rate: 5000, rotate-bytes: 256KiB }
  app:
    output: stdout           # collected through the docker log driver
    replicas: 15             # app-01 … app-15, same settings
    streams: 1
    rate: 500
    labels:                  # docker labels on the container; {n} is the replica number
      team: payments
      app: app-{n}

faults:                      # offsets from the start of the window
  - at: 60s
    set: { error_rate: 0.5, error_status: 503, note: "intake 503 storm" }
  - at: 90s
    clear: true

compose: [docker-compose.offline.yml]   # optional extra compose files
```

## Generators

Each entry is a container running `aoc generate`, named `gen-<key>` (or
`gen-<key>-NN` for replicas). Four keys belong to the harness:

| key | values | default |
|---|---|---|
| `format` | `plain`, `json`, `both` | `plain` |
| `output` | `file` (tailed from `/var/log/agent-of-chaos/<key>/`), `stdout` (the docker log driver) | `file` |
| `replicas` | containers with these settings | 1 |
| `labels` | docker labels; `{n}` expands to the replica number | none |

Every other key is an `aoc generate` flag, written as on the command line
without the dashes in front: `streams`, `rate` (lines/s across the streams,
`0` for flat out), `rotate-bytes`, `rotate-keep`, `rotate-mode`,
`multiline-rate`, `multiline-frames` (0 = 2–8 frames; 300 ≈ 25 KiB, 3000 ≈ 250 KiB),
`wide-line-rate`, `wide-line-bytes`, `pad-to`, `burst-size`, `burst-interval`,
`deterministic`, `seed`, `max-lines`, `mode` (`steady`, `ramp`, `chaos`,
`spike`, `pulse`, `scenario`) and that mode's flags. `aoc generate -h`
lists them all; an unknown key is an error before anything starts.

Generators on stdout wait 20 s before their first line (`start-delay`),
because the agent's docker launcher tails a new container from the moment
it notices it.

## Faults

Each step either sets the intake's faults (replacing the previous state)
or clears them. Fields: `latency_ms`, `jitter_ms`, `error_rate`,
`error_status`, `drop_rate`, `outage`, `read_bps`, `tcp_reset_rate`,
`tcp_refuse`, `note`, and `flap_on_ms` with `flap_off_ms` to apply the
step's faults in randomized episodes (about `flap_on_ms` long, about
`flap_off_ms` apart, each ± 50 %) instead of steadily. A step at `0s` runs
for the whole window. [docs/reference.md](../docs/reference.md) says what
each one does to the agent. A fault for the whole window needs no profile:
`--fault K=V` on `aoc run` and `aoc ab`.

The HTTP faults (`latency_ms`, `error_rate`, `drop_rate`, `outage`) shape
HTTP responses; the legacy TCP transport has none, so it gets
`tcp_reset_rate` (close the agent's connection after a batch of frames with
this probability) and `tcp_refuse` (close every TCP connection on accept).
`read_bps` slows both.

A step with `boot: true` instead of `at:` is in place before the agent
starts, so what the agent decides at startup is decided under the fault.
Its use is the transport choice: an agent pointed at the intake's TCP port
with `DD_LOGS_CONFIG_USE_HTTP: "false"` runs an HTTP connectivity check at
startup and falls back to TCP when it fails, then re-checks and upgrades
back to HTTP once it passes. `outage` at boot fails the check without
touching TCP; a later `clear` lets the upgrade happen
(`http-fallback.yaml`). One boot step per profile; it holds until a later
step changes it.

## Changing a profile without editing it

```bash
aoc run --profile baseline --set rate=20000 --set json.streams=16 --plan
aoc ab --b IMAGE --workload tag-heavy-fleet --set app.replicas=30 --fault latency_ms=300
```

`--set KEY=VALUE` changes a setting for every generator, or
`<generator>.KEY=VALUE` for one; `format`, `output` and `replicas` work the
same way. The profile as it ran, with every default and `--set` applied, is
written to `results/<name>/profile.yaml` (and the A/B's `aoc.yaml` keeps
the `--set` list), so `aoc run --profile results/<name>/profile.yaml`
repeats it exactly. When a shape is worth keeping, save that file here.
