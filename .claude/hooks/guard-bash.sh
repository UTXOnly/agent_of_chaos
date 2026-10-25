#!/usr/bin/env bash
# PreToolUse hook for Bash: the token and one-run-at-a-time rules, enforced.
# Reads the tool call on stdin, answers with a permission decision, exit 0.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 0
cmd="$(jq -r '.tool_input.command // ""')"
[ -n "$cmd" ] || exit 0
PORT="${AOC_UI_PORT:-4100}"

deny() {
  jq -cn --arg r "$1" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
  exit 0
}

heavy='(report\.json|timeseries\.csv|notebook\.json|[^ ]*\.pprof|agent\.log|intake\.log|gen-[^ ]*\.log|build-[ab]\.log)'
if echo "$cmd" | grep -Eq "(^|[|;&(] *)(cat|less|more|bat|head|cat -n)( +-[^ ]+)* +[^|;&]*$heavy"; then
  deny "That file is too large to read whole. Single values: jq on findings.json (.signals, .gate) or report.json (.delivery, .resources, .streams); logs: grep -c '| ERROR |' or grep | sort | uniq -c | sort -rn | head; build logs: tail -30. Recipes: .claude/rules/results.md."
fi
if echo "$cmd" | grep -Eq "tail( +-[^ ]+)* +[^|;&]*(report\.json|timeseries\.csv|notebook\.json|[^ ]*\.pprof)"; then
  deny "tail on a JSON/CSV/pprof artifact is not a reading strategy; use jq on the field you need (.claude/rules/results.md)."
fi

if echo "$cmd" | grep -Eq '\bwhile\b.*\bsleep\b|\bsleep +[0-9]+.*\bwhile\b|for .*\bsleep\b'; then
  deny "No sleep loops. A run launched with ui/bin/aoc-ui run in the background ends with the task's exit notification; wait for that. A condition to watch goes in a Monitor."
fi

launch=0
echo "$cmd" | grep -Eq '(^|[^-A-Za-z])(\./)?bin/aoc +(ab|run)\b|aoc-ui +run\b|make +(ab|run-baseline)\b' && launch=1
if [ "$launch" = 1 ] && ! echo "$cmd" | grep -Eq -- '--plan|--compare-only'; then
  active="$(curl -fsS -m 2 "http://localhost:$PORT/api/health" 2>/dev/null | jq -r 'select(.busy) | .active | "\(.name) (\(.kind)), phase \(.phase)"' 2>/dev/null)"
  [ -z "$active" ] && pgrep -f 'bin/aoc (ab|run)( |$)' >/dev/null 2>&1 && active="a bin/aoc process outside the UI (pgrep -fl 'bin/aoc')"
  [ -n "$active" ] && deny "One run at a time: $active is still running. Wait for its exit, or stop it with ui/bin/aoc-ui stop-run."
  if ! echo "$cmd" | grep -Eq 'aoc-ui +run\b|AOC_NO_UI=1'; then
    deny "Launch through the UI so the engineer can watch: replace './bin/aoc <args>' with 'ui/bin/aoc-ui run -- <args>' (same flags, add --name; it starts the server if needed, waits, and exits with aoc's code). Run it in the background with output to a file. AOC_NO_UI=1 on the command skips this when the UI cannot be used."
  fi
fi

if echo "$cmd" | grep -Eq 'docker +compose\b.*\b(down|rm|stop|kill)\b|docker +(rm|stop|kill|system +prune)\b'; then
  active="$(curl -fsS -m 2 "http://localhost:$PORT/api/health" 2>/dev/null | jq -r 'select(.busy) | .active.name' 2>/dev/null)"
  [ -n "$active" ] && deny "A run ($active) owns the aoc compose stacks right now. Stop it first: ui/bin/aoc-ui stop-run."
fi
exit 0
