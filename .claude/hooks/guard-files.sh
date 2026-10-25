#!/usr/bin/env bash
# PreToolUse hook for Read/Edit/Write/MultiEdit: settings are not tests, and
# the heavy artifacts are never read whole.
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/../.." || exit 0
in="$(cat)"
tool="$(echo "$in" | jq -r '.tool_name // ""')"
f="$(echo "$in" | jq -r '.tool_input.file_path // .tool_input.notebook_path // ""')"
[ -n "$f" ] || exit 0
rel="${f#"$PWD"/}"

answer() {
  jq -cn --arg d "$1" --arg r "$2" '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:$d,permissionDecisionReason:$r}}'
  exit 0
}

case "$rel" in
  results/*report.json|results/*timeseries.csv|results/*notebook.json|*.pprof|results/*agent.log|results/*intake.log|results/*/gen-*.log|results/*/profiles/*)
    [ "$tool" = "Read" ] && answer deny "Never read $rel whole: jq for a value (.signals/.gate in findings.json; .delivery/.resources/.streams/.telemetry in report.json), grep for logs, jq '.cells[N]' for one notebook cell. Recipes: .claude/rules/results.md." ;;
esac

[ "$tool" = "Read" ] && exit 0

case "$rel" in
  aoc.yaml)
    answer ask "aoc.yaml holds the settings every test shares (control image, window, rounds, thresholds, checkout). A test's shape goes on the command line: --workload, --set, --fault, --env/--b-env, --duration, --runs, --threshold. Approve only if the engineer asked to change the shared settings." ;;
  profiles/*.yaml)
    if [ -f "$rel" ]; then
      answer ask "$rel is a shared workload, not a test. Tuning goes on the command line (--set, --fault); a new shape or fault timeline is a new file copied from the closest profile. Approve only if the engineer asked to change this workload for everyone." 
    fi ;;
  .env|.env.*)
    answer deny ".env holds DD_API_KEY; the engineer edits it. Never print or write it." ;;
esac
exit 0
