# Notebook analysis through Datadog MCP

The harness owns measurements; the coding agent owns the explanation.
`aoc run` and `aoc ab` write local evidence. They never publish a notebook.
Claude (or another connected coding agent) reads the results, writes the
analysis, and creates or updates a Datadog notebook through MCP.

## Connect once

Follow [Datadog's MCP setup](https://docs.datadoghq.com/mcp_server/setup/)
for your client and Datadog site. OAuth is the recommended interactive setup.
Notebook authoring needs MCP write access plus Notebooks Read/Write permissions;
verify that `get_datadog_notebook`, `create_datadog_notebook` and
`edit_datadog_notebook` are available in the connected tools. Tool schemas can
change: inspect the connected schema instead of guessing arguments.
[Datadog's tool reference](https://docs.datadoghq.com/mcp_server/tools/).

`DD_API_KEY` in `.env` ships telemetry and events; MCP authenticates
separately through the client. If MCP is absent or cannot write, finish the
local analysis and state "notebook not published" with the evidence path.
Do not delay the experiment or claim a notebook exists.

## Write for the engineer making a decision

Read `findings.md`, the build identities and windows in `ab.json`, and
`conclusion.md` if present. For a single run, start with `report.md`.
Use `compare.md` for supporting numbers. Aim for a 150–250-word opening:

1. **Answer and scope.** What does this result say about the original question,
   on this host, workload and set of rounds?
2. **Deciding evidence.** Delivery counts first; then two or three useful A/B
   values, deltas and thresholds. Include improvements and their tradeoffs.
3. **Explanation and confidence.** Distinguish what was measured, a plausible
   cause, and what a follow-up actually confirmed. Name a function only when
   profiles or code support it. Missing evidence is a limitation, not zero.
4. **Next experiment.** One concrete command or change, and the uncertainty it
   resolves. Stop when the requested decision has enough evidence.

Do not paste the whole brief, narrate tool calls, repeat every metric, or declare
release readiness from a short local test. Shared failures still fail the gate.
A code diff and a profile delta suggest where to look; neither proves a cause.

Illustrative opening (invented numbers, **not a measured result**):

> In two 10-minute outage runs, B delivered all 1.2M generated records with
> zero duplicates, orphans or truncations. CPU rose from 30% to 34% (+13%, above
> the 5% threshold); heap was flat. The retry function accounts for most of the
> CPU-profile increase, but the extra timer creation is still a hypothesis.
> Delivery looks sound for this scenario; the CPU cost needs investigation.
> Next, remove the per-retry timer allocation and repeat both sides under
> intake-outage. This does not yet cover dropped connections or rotation.

## Assemble and publish

1. Save the A/B analysis with `aoc conclude --results <dir> --verdict
   pass|fail|inconclusive --file <analysis.md>`. This persists `conclusion.md` and
   posts the conclusion event when `DD_API_KEY` is configured. The event verdict
   is the author's interpretation; it does not override the measured gate.
   To refresh `findings.md`, use `aoc ab --config <dir>/aoc.yaml --compare-only`.
   For a single run, save the analysis as `<dir>/conclusion.md` directly.
2. Read `<dir>/notebook.md`. It contains the title/window and an index of reusable
   visual evidence in `notebook.json`. For older results, prepare it with
   `aoc notebook --results <dir>`. For an older A/B without `notebook.json`, use
   the saved config with `--compare-only` first.
3. Extract only selected cells with `jq '.cells[N]' <dir>/notebook.json`.
   Usually two to four charts or profile comparisons are enough. Preserve query
   filters, absolute time ranges and time shifts: earlier A/B rounds are aligned
   to the latest window. Reuse generated profiler comparisons or MCP-returned
   links; do not invent URLs. The JSON is an evidence library, **not an MCP
   request body**, and its generated Markdown is not the final narrative.
4. Check `<dir>/notebook.url` and, for A/B, `ab.json`'s `notebook` field. If a link
   exists, retrieve that notebook and update its analysis/evidence cells using
   the connected edit tool's schema. Preserve unrelated cells and their IDs;
   do not prepend another conclusion on every rerun. Otherwise create a notebook
   with the authored opening, selected evidence, and reproducibility details.
5. Verify the returned notebook (title, summary, windows, A/B filters), then save
   the **returned** URL:

   ```bash
   ./bin/aoc notebook --results results/my-change --link https://app.datadoghq.com/notebook/123
   ```

   This updates `notebook.url` and `ab.json` when present. Re-rendering local
   evidence preserves that link; it does not update the remote notebook.
   If a create call times out, search for the experiment's notebook before
   retrying so an uncertain response does not create duplicates.

Publishing and verifying the notebook are separate from the two investigative
MCP calls per regressed signal. If that investigation budget cannot settle the
cause, say so and propose the next experiment.

Older saved commands and configs may still carry `--notebook`,
`--dry-run` or a `notebook:` key; they are accepted and ignored.
