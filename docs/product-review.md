# Product review: a logs engineer's local feedback loop

**Verdict: useful for a focused development decision, with a credibility gap in
how it was presented.** I would use it to test a dirty sender/tailer change under
a failure I can name, then inspect delivery and resource cost. I would not treat
its short A/B or a persuasive notebook as a release gate. Its value is the time
saved assembling that experiment and deciding what to do next.

This review examines the README, CLI, profiles, result generation, notebook/API
path, and Claude skills. It is a code/documentation review, not a new performance
experiment. Historical numbers below come from the repository's session account.

## Findings and changes

| Priority | Friction or trust problem | Change in this branch |
|---|---|---|
| High | The README opens with plumbing and then becomes a manual. The local-change quick start is buried beneath release-versus-master examples. An engineer has to reconstruct the payoff. | Lead with development questions, evidence and decisions; give one complete checkout → plan → smoke → read path. Move the detailed workload catalog to `docs/workloads.md`. |
| High | “Local feedback versus CI” does not distinguish the tool from SMP. SMP explicitly supports `smp local-run`. | State the overlap; position `aoc` around shaping a logs scenario, uncommitted builds, delivery accounting and the edit/rerun conversation. Avoid claims that SMP cannot do those things. |
| High | Documentation overstates causal certainty: shared failures are “the host,” a profile mover is “the build,” a heap ramp “is a leak.” This encourages confident but unsupported summaries. | Require observation/hypothesis/confirmation to be distinguished; preserve failed gates; explain host contention, stale controls and missing profiles. |
| High | Notebook ownership is split: Go generates the narrative, app-key HTTP calls publish it, and Claude investigates afterward. Repeated conclusions prepend cells; a re-render can create another notebook. | Remove notebook HTTP publishing and app-key handling. Keep deterministic evidence cells; add a compact handoff, agent-authored MCP workflow and URL recording. Local evidence refreshes preserve the recorded notebook. |
| Medium | The original diagrams explain routing and a long scripted chat, not the decision an engineer is trying to make. | Put one decision loop in the README. Put the two-stack experiment boundary in the reference. Show measurements flowing into an interpretation, then a fix or repeat. |
| Medium | The first showcased session claims a pass with 99.97% delivery and about 3.7k missing on each side. That undermines the exact-delivery promise. | Preserve the historical numbers but label delivery failed and attribution inconclusive. Treat the warmup explanation as needing validation; propose a controlled rerun before another shape. |
| Medium | “Two minutes” hides wall time; `rate=20000` means per generator; a short outage test omits the later 429 phase; bare `--only b` can select the wrong saved test. | Make wall time, aggregate load, smoke coverage, saved-config reruns and stale-control limitations explicit. |
| Medium | Agents are told to pass notebook cells to MCP and also never read `notebook.json`. `aoc notebook --results` cannot reopen an A/B root. | Index visual cells in `notebook.md`, extract only selected cells, and support A/B roots in the preparation command. |

## What makes this worth keeping?

The distinctive deliverable is a bounded answer to a change-specific question,
with enough evidence to choose the next edit. The ledger and build provenance
make that answer inspectable; profiles shorten the jump from “CPU changed” to
“read this function.” Fault timelines and CLI overrides avoid writing bespoke
benchmarks for each hypothesis. These are stronger reasons to use the tool than
“chaos,” a large metric catalog, or automatic notebook creation.

There is useful historical evidence of this: the tag-filter session reports CPU
34.5% → 30.9%, heap 104.9 → 88.2 MB, and a 1.7 MB/s allocation site in
`tagfilter.(*Scoped).Keep`. That is an actionable resource tradeoff. The same
session's missing records prevent a clean delivery claim. Both facts should be
visible together; the favorable delta must not hide the unresolved gate.

[SMP's Agent documentation](https://github.com/DataDog/datadog-agent/blob/main/test/regression/README.md)
describes regression detection, maintained experiments, quality gates and local
runs. The recommendation here is a division of workflow, not a claim of exclusive
capabilities: use `aoc` for fast hypothesis testing; carry meaningful scenarios
and findings into maintained tests when stronger evidence is needed.

## Why MCP helps—and what it does not fix

MCP lets the agent publish the analysis it actually reached, using the connected
Datadog identity. Removing the second notebook-auth path reduces setup and avoids
presenting generated tables as an investigation. Keep query construction,
measurements and classifications in code; let the agent select evidence and
explain consequences. The writing contract is a short answer, deciding numbers,
confidence limits and one next experiment. Changing transport alone would not
improve the analysis.

Datadog documents [notebook create/edit tools](https://docs.datadoghq.com/mcp_server/tools/)
and [OAuth setup and write permissions](https://docs.datadoghq.com/mcp_server/setup/).
The implementation prepares evidence and records MCP-returned URLs; it does not
embed an MCP client in the CLI. Live publication still requires those tools in
the engineer's coding-agent session.

## Remaining gaps worth prioritizing

- **Capacity semantics:** `internal/intake/build.go` divides received records by
  elapsed window time. `deterministic-bytes` has a fixed record budget and may
  finish early. That average is not automatically the Agent's maximum. Corrected
  the language; a capacity claim needs verified sustained demand or a separate
  completion-time measurement. Do not silently redefine existing metrics.
- **Confidence:** medians and percentage thresholds do not estimate noise or
  significance. Parallel sides contend for one host; sequential sides introduce
  time drift; rerunning B reuses an older control. A/A noise characterization and
  a visible comparison-validity summary would add more value than more charts.
- **Time to first answer:** first builds/downloads and two-round defaults are
  expensive for exploratory edits. The README exposes smoke versus decision
  runs; the CLI could next report estimated wall time and which fault phases
  fit the chosen window before starting.
- **Acceptance evidence:** validate the product with one logs engineer's real
  dirty-branch change: can they identify the control, predict fault coverage,
  explain the gate, name one next action and reopen the notebook without help?
  Measure setup time, time to the first actionable answer and required corrections.
  Do not count notebook length or number of metrics as success.

**Next experiment:** an A/A baseline on the target laptop, followed by the same
known change under its relevant fault. It tests whether the default thresholds
and workflow produce a useful decision before expanding the feature set.

## Validation

`make test`, `make vet` and `make build` pass. Tests cover offline preparation
with keys present, A/B reopening, URL persistence, preservation of existing
metadata and the failed delivery gate during a local re-render. The revised
diagrams were rendered and visually inspected; local documentation links and
all four skill definitions were validated. The CLI smoke plan was checked.
No Docker performance experiment was run, and live MCP notebook publication
was not verified because Datadog MCP is not connected in the review session.
