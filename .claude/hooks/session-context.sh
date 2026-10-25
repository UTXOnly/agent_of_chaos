#!/usr/bin/env bash
# SessionStart hook: where things stand, in a screen, so the session can
# lead from the first message. Also the body of the `status` skill.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 0
PORT="${AOC_UI_PORT:-4100}"
URL="http://localhost:$PORT"

echo "## aoc: where things stand"
branch="$(git rev-parse --abbrev-ref HEAD 2>/dev/null)"
dirty="$(git status --porcelain 2>/dev/null | wc -l | tr -d ' ')"
echo "- repo: branch $branch, $dirty uncommitted files"

if [ -x bin/aoc ]; then
  stale="$(find cmd internal -name '*.go' -newer bin/aoc 2>/dev/null | head -1)"
  [ -n "$stale" ] && echo "- bin/aoc: stale (Go sources newer); run \`make build\`" || echo "- bin/aoc: built"
else
  echo "- bin/aoc: missing; run \`make build\`"
fi

if docker info --format '{{.ServerVersion}}' >/dev/null 2>&1; then
  left="$(docker ps -a --filter 'label=com.docker.compose.project=aoc' --filter 'label=com.docker.compose.project=aoc-a' --filter 'label=com.docker.compose.project=aoc-b' -q 2>/dev/null | wc -l | tr -d ' ')"
  echo "- docker: up; $left leftover aoc containers$( [ "$left" != 0 ] && echo ' (clean up before a run: POST /api/docker/cleanup or docker compose -p aoc|aoc-a|aoc-b down -v)')"
else
  echo "- docker: not reachable; aoc run/ab cannot start"
fi

if h="$(curl -fsS -m 2 "$URL/api/health" 2>/dev/null)"; then
  if [ "$(echo "$h" | jq -r .busy)" = "true" ]; then
    echo "- ui: $URL, a run is active: $(echo "$h" | jq -r '.active | "\(.name) (\(.kind)) phase \(.phase) since \(.phaseSince), \(.resultsDir)"'); one run at a time"
  else
    echo "- ui: $URL, no run active (launch with \`ui/bin/aoc-ui run -- …\` so the engineer can watch)"
  fi
else
  echo "- ui: not running; \`ui/bin/aoc-ui run\` starts it on first use, or \`make ui\`"
fi

src="$(sed -n 's/^source: *//p' aoc.yaml | tr -d '"' | head -1)"
src="${src:-${DATADOG_AGENT_SRC:-../datadog-agent}}"
src="${src/#\~/$HOME}"
if [ -d "$src/.git" ]; then
  echo "- agent checkout: $src, branch $(git -C "$src" rev-parse --abbrev-ref HEAD 2>/dev/null), $(git -C "$src" status --porcelain 2>/dev/null | wc -l | tr -d ' ') uncommitted files (what --b-src . builds)"
else
  echo "- agent checkout: none at $src (set source: in aoc.yaml or DATADOG_AGENT_SRC for --b-src and the code section)"
fi

[ -f .env ] && grep -q '^ *\(export \)\?DD_API_KEY=.' .env && echo "- .env: DD_API_KEY set" || echo "- .env: no DD_API_KEY; metrics, events and profiles will not reach Datadog"

echo "- recent results (newest first):"
ls -1dt results/*/ 2>/dev/null | head -6 | while read -r d; do
  n="$(basename "$d")"
  if [ -f "$d/findings.json" ]; then
    v="$(jq -r '[(.gate.pass // empty | if . then "gate pass" else "GATE FAIL" end), ([.signals[]? | select(.kind=="regression") | .name] | unique | if length>0 then "regressed: " + join(",") else "signals clean" end)] | join(" · ")' "$d/findings.json" 2>/dev/null)"
    c="$( [ -f "$d/conclusion.md" ] && echo "concluded" || echo "NO CONCLUSION")"
    echo "  - $n: A/B · $v · $c · $(jq -r '.focus // "" | .[0:70]' "$d/findings.json" 2>/dev/null)"
  elif [ -f "$d/report.json" ]; then
    echo "  - $n: run · $(jq -r '.delivery | "delivery \(.ratio*100|.*100|round/100)% missing \(.missing) dup \(.duplicates)"' "$d/report.json" 2>/dev/null) · $( [ -f "$d/conclusion.md" ] && echo concluded || echo 'no conclusion')"
  else
    echo "  - $n: incomplete (no findings.json or report.json)"
  fi
done
echo "Lead the loop: goal → plan → run (one at a time, through the UI) → read findings.md → decide → conclude. After every run, say what moved and propose the next run."
