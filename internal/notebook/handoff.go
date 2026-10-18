package notebook

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Handoff indexes reusable evidence without copying the generated report into
// the agent's context. The agent owns the narrative, selection and MCP call.
func (n *Notebook) Handoff(jsonFile string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Notebook evidence\n\nQuestion/title: %s\n\n", n.Name)
	fmt.Fprintf(&b, "Display window: %s → %s (UTC; includes chart margins).\n\n", n.Start.UTC().Format(time.RFC3339), n.End.UTC().Format(time.RFC3339))
	b.WriteString("Read `conclusion.md` if present, then `findings.md` and `ab.json` for an A/B, or `report.md` for a single run. Actual measurement windows and build identities are in those results. Compare runs use each source directory's report.\n\n")
	b.WriteString("Write the answer to the question, delivery counts, the few deltas that decide it, evidence versus hypotheses, limitations, and one next experiment. Preserve the measured gate even when explaining a shared failure. Follow `docs/notebooks.md`.\n\n")
	b.WriteString("## Reusable visual evidence\n\nThese are optional evidence cells, not a finished analysis. Select only cells that support the conclusion. Keep their queries, absolute times and time shifts intact.\n\n| Cell index | Type | Title |\n|---|---|---|\n")
	var heading string
	for i, cell := range n.Cells {
		attrs, _ := cell["attributes"].(map[string]any)
		def, _ := attrs["definition"].(map[string]any)
		kind, _ := def["type"].(string)
		if kind == "markdown" {
			text, _ := def["text"].(string)
			heading = strings.TrimSpace(strings.TrimLeft(strings.SplitN(text, "\n", 2)[0], "#"))
			continue
		}
		if kind == "" {
			continue
		}
		title, _ := def["title"].(string)
		if kind == "iframe" && title == "" {
			title = heading
		}
		title = strings.NewReplacer("|", "\\|", "\n", " ").Replace(title)
		fmt.Fprintf(&b, "| %d | %s | %s |\n", i, kind, title)
	}
	fmt.Fprintf(&b, "\nIn this directory, read metadata with `jq '{name,start,end}' %s`; extract a selected cell with `jq '.cells[N]' %s` (replace N with an index above). Do not read the entire JSON into context.\n\n", strconv.Quote(jsonFile), strconv.Quote(jsonFile))
	b.WriteString("Publish with Datadog MCP `create_datadog_notebook`, or read and update the recorded notebook with `get_datadog_notebook` / `edit_datadog_notebook`. Discover the connected tool schema; this JSON is an evidence format, not an MCP request. Save the returned URL with `aoc notebook --results <results-directory> --link <URL>`. If MCP is unavailable, keep the local conclusion and report the notebook as unpublished.\n")
	return b.String()
}
