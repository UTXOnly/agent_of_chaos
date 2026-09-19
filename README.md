# agent_of_chaos

Datadog Logs Agent stress tester. Spawns configurable service workers that write rotating plain-text and JSON log pairs for tailing by the Datadog Agent.

## Quick start (Docker)

```bash
# Build and run with docker compose (logs land in ./logs/)
docker compose up --build

# Or run the image directly
docker build -t agent-of-chaos .
docker run --rm -v "$(pwd)/logs:/var/log/agent-of-chaos" \
  -e AOCH_MODE=steady -e AOCH_SERVICES=10 \
  agent-of-chaos
```

Point the Datadog Agent `logs` integration at the mounted directory (host path `./logs`, container path `/var/log/agent-of-chaos`).

Stop with `Ctrl+C` or `docker compose down`.

## Quick start (local)

Requires Python 3.9+ (stdlib only — no pip install).

```bash
python3 agent_of_chaos.py --mode steady --services 10
```

Logs are written to `./logs/` by default (override with `--log-dir` or `AOCH_LOG_DIR`).

## Execution modes

| Mode | Description |
|------|-------------|
| `steady` | Constant load; all services start immediately |
| `ramp` | Start small, add services gradually until target |
| `chaos` | Services randomly crash and restart |
| `spike` | Steady baseline plus periodic coordinated volume spikes |
| `scenario` | Multi-phase scripted run (built-in or custom JSON) |
| `pulse` | Repeating ramp → peak → cooldown → rest cycle |

```bash
python3 agent_of_chaos.py --mode chaos --services 8 --chaos-crash-rate 3
python3 agent_of_chaos.py --mode scenario --scenario business-day --scenario-speed 3
python3 agent_of_chaos.py --mode pulse --pulse-ramp 300 --pulse-peak 120 --pulse-rest 600
```

## Benchmark suite

Deterministic, byte-identical logs for a fixed seed + config. See `benchmark_profiles.json`.

```bash
python3 agent_of_chaos.py \
  --benchmark-config benchmark_profiles.json \
  --benchmark profile-high-throughput
```

In Docker:

```bash
docker run --rm -v "$(pwd)/logs:/var/log/agent-of-chaos" \
  -e AOCH_BENCHMARK_CONFIG=/app/benchmark_profiles.json \
  -e AOCH_BENCHMARK=profile-high-throughput \
  agent-of-chaos
```

## Configuration

Every CLI flag can be set via an environment variable with the `AOCH_` prefix. CLI arguments take precedence over environment variables.

Boolean env vars accept `1`, `true`, `yes`, or `on` (case-insensitive).

### Environment variable reference

| Environment variable | CLI flag | Default |
|---------------------|----------|---------|
| `AOCH_MODE` | `--mode` | `steady` |
| `AOCH_SERVICES` | `--services` | `5` |
| `AOCH_LINES_PER_CYCLE` | `--lines-per-cycle` | `20` |
| `AOCH_CYCLE_SLEEP_MS` | `--cycle-sleep-ms` | `0` |
| `AOCH_BURST_INTERVAL` | `--burst-interval` | `5.0` |
| `AOCH_BURST_SIZE` | `--burst-size` | `1000` |
| `AOCH_MAX_BYTES` | `--max-bytes` | `524288` |
| `AOCH_BACKUP_COUNT` | `--backup-count` | `5` |
| `AOCH_RAMP_START` | `--ramp-start` | `1` |
| `AOCH_RAMP_STEP` | `--ramp-step` | `1` |
| `AOCH_RAMP_INTERVAL` | `--ramp-interval` | `30` |
| `AOCH_CHAOS_CRASH_RATE` | `--chaos-crash-rate` | `2.0` |
| `AOCH_CHAOS_RESTART_DELAY` | `--chaos-restart-delay` | `5` |
| `AOCH_SPIKE_INTERVAL` | `--spike-interval` | `30` |
| `AOCH_SPIKE_DURATION` | `--spike-duration` | `10` |
| `AOCH_SPIKE_SERVICES` | `--spike-services` | `5` |
| `AOCH_PULSE_RAMP` | `--pulse-ramp` | `600` |
| `AOCH_PULSE_PEAK` | `--pulse-peak` | `180` |
| `AOCH_PULSE_COOLDOWN` | `--pulse-cooldown` | `0` |
| `AOCH_PULSE_REST` | `--pulse-rest` | `2100` |
| `AOCH_PULSE_BASE_SERVICES` | `--pulse-base-services` | `2` |
| `AOCH_PULSE_PEAK_SERVICES` | `--pulse-peak-services` | `10` |
| `AOCH_PULSE_BASE_LINES` | `--pulse-base-lines` | `4` |
| `AOCH_PULSE_PEAK_LINES` | `--pulse-peak-lines` | `40` |
| `AOCH_PULSE_PEAK_BURST_SIZE` | `--pulse-peak-burst-size` | `2000` |
| `AOCH_PULSE_PEAK_BURST_INTERVAL` | `--pulse-peak-burst-interval` | `4.0` |
| `AOCH_PULSE_CYCLES` | `--pulse-cycles` | `0` (forever) |
| `AOCH_SCENARIO` | `--scenario` | — |
| `AOCH_SCENARIO_FILE` | `--scenario-file` | — |
| `AOCH_SCENARIO_SPEED` | `--scenario-speed` | `1.0` |
| `AOCH_MULTILINE_RATE` | `--multiline-rate` | `0.20` |
| `AOCH_WIDE_LINES` | `--wide-lines` | `false` |
| `AOCH_WIDE_LINE_RATE` | `--wide-line-rate` | `0.03` |
| `AOCH_DETERMINISTIC` | `--deterministic` | `false` |
| `AOCH_GLOBAL_SEED` | `--global-seed` | `0` |
| `AOCH_BENCHMARK_CONFIG` | `--benchmark-config` | — |
| `AOCH_BENCHMARK` | `--benchmark` | — |
| `AOCH_RUN_ALL_BENCHMARKS` | `--run-all-benchmarks` | `false` |
| `AOCH_BENCHMARK_LIST` | `--benchmark-list` | `false` |
| `AOCH_BENCHMARK_EMIT_CHUNK` | `--benchmark-emit-chunk` | `32` |
| `AOCH_BENCHMARK_PLAIN_ONLY` | `--benchmark-plain-only` | `false` |
| `AOCH_LOG_DIR` | `--log-dir` | repo dir (local) / `/var/log/agent-of-chaos` (image) |
| `AOCH_STATS_INTERVAL` | `--stats-interval` | `5` |

Copy `.env.example` to `.env` for docker compose overrides.

## Docker details

- **Image**: `python:3.12-slim`, no extra dependencies
- **User**: runs as non-root `appuser` (uid 10001)
- **Log volume**: `/var/log/agent-of-chaos` — mount this when running the container
- **Entrypoint**: `python3 /app/agent_of_chaos.py` — additional args are forwarded as CLI flags

### Datadog Agent sidecar example

Mount the same host directory in both containers so the Agent tails logs the generator writes:

```yaml
# docker-compose.override.yml (example)
services:
  agent-of-chaos:
    volumes:
      - ./logs:/var/log/agent-of-chaos

  datadog-agent:
    image: gcr.io/datadoghq/agent:latest
    volumes:
      - ./logs:/var/log/agent-of-chaos:ro
      - /var/run/docker.sock:/var/run/docker.sock:ro
    environment:
      DD_API_KEY: ${DD_API_KEY}
      DD_LOGS_ENABLED: "true"
      DD_LOGS_CONFIG_CONTAINER_COLLECT_ALL: "false"
```

Configure a file log collection rule for `/var/log/agent-of-chaos/*.log` in the Agent.

## Output

Each service worker writes:

- `{service}.log` — plain text
- `{service}.json.log` — JSON (unless `AOCH_BENCHMARK_PLAIN_ONLY=true`)

Files rotate at `AOCH_MAX_BYTES` with `AOCH_BACKUP_COUNT` backups retained.
