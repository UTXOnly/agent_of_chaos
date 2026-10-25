---
name: stop
description: Stop the aoc run that is in progress and leave the host clean: interrupt it through the UI so aoc takes its compose stacks down, confirm nothing is left running, and say what partial results exist. Use for "stop", "cancel the run", "kill it", "abort", "that's enough, stop it", or when the engineer changes the goal mid-run.
---

# Stop the run

```bash
ui/bin/aoc-ui stop-run        # SIGINT to aoc; it runs compose down itself
ui/bin/aoc-ui status          # "no run active" when it is done
docker ps --filter label=com.docker.compose.project=aoc --filter label=com.docker.compose.project=aoc-a --filter label=com.docker.compose.project=aoc-b --format '{{.Names}} {{.Status}}'
```

A run launched outside the UI (`AOC_NO_UI=1`) is a `bin/aoc` process:
`pgrep -fl 'bin/aoc (ab|run)'`, then `kill -INT <pid>` and wait for it to
exit; it takes its stack down on SIGINT. Only if containers remain after
the process is gone:

```bash
curl -s -X POST localhost:4100/api/docker/cleanup
```

Interrupting an A/B mid-round leaves `results/<name>/` without
`findings.md`; a finished earlier round is still on disk under
`<side>/`. Say which it is. Then say what you would run instead, or that
the session is at its conclusion if the engineer stopped it because the
answer is in.

Never `docker compose down` or `docker rm` while the run is still alive;
the run owns its stacks and the hook refuses it.
