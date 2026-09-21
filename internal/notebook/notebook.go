// Package notebook turns run reports into Datadog notebooks: a findings
// summary, timeseries cells over the aoc.* metrics the intake submitted, the
// agent's own integration metrics, and pointers for digging further.
package notebook

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/ddapi"
	"github.com/UTXOnly/agent_of_chaos/internal/findings"
	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

// Cell is a raw notebook cell ({"type":"notebook_cells","attributes":{...}}).
type Cell map[string]any

// Notebook is what gets created.
type Notebook struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"` // report | investigation | ...
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Cells []Cell    `json:"cells"`
}

// Options tune the generated notebook.
type Options struct {
	Site   string // for links
	AppURL string // browser base URL (custom subdomain); default derived from Site
	Name   string // notebook title override
	Prefix string // metric prefix, default aoc
	// Margin widens the time window on both sides (default 60s).
	Margin time.Duration
	// AgentContainer / AgentHost are used for the docker integration and
	// profiler queries when the report does not carry them.
	AgentContainer string
	AgentHost      string
	// Experiment is the experiment:<name> tag shared by the runs of an A/B
	// test (ForAB); the brief's cells use it to scope their links.
	Experiment string
	// Findings is the A/B brief (ForAB): the regressions and their evidence
	// lead the notebook, the charts follow.
	Findings *findings.Result
}

func (o *Options) defaults() {
	if o.Prefix == "" {
		o.Prefix = "aoc"
	}
	if o.Margin == 0 {
		o.Margin = 60 * time.Second
	}
	if o.Site == "" {
		o.Site = "datadoghq.com"
	}
	if o.AppURL == "" {
		o.AppURL = ddapi.AppURL(o.Site)
	}
}

// Body renders the create-notebook request body.
func (n *Notebook) Body() []byte {
	body := map[string]any{"data": map[string]any{
		"type": "notebooks",
		"attributes": map[string]any{
			"name":     n.Name,
			"status":   "published",
			"metadata": map[string]any{"type": n.Type, "is_template": false, "take_snapshots": false},
			"time":     absTime(n.Start, n.End),
			"cells":    n.Cells,
		},
	}}
	b, _ := json.MarshalIndent(body, "", "  ")
	return b
}

// File is the sidecar written next to report.json so the notebook can be
// created later (aoc notebook, or the Datadog MCP's create_datadog_notebook).
func (n *Notebook) File() []byte {
	b, _ := json.MarshalIndent(n, "", "  ")
	return b
}

func absTime(start, end time.Time) map[string]any {
	return map[string]any{"start": start.UTC().Format(time.RFC3339), "end": end.UTC().Format(time.RFC3339), "live": false}
}

func markdown(text string) Cell {
	return Cell{"type": "notebook_cells", "attributes": map[string]any{
		"definition": map[string]any{"type": "markdown", "text": text},
	}}
}

type query struct {
	q     string
	alias string
}

// timeseries builds a line chart cell; when start/end are set the cell is
// pinned to that absolute range (used by comparison notebooks).
func timeseries(title string, qs []query, start, end time.Time, yLabel string) Cell {
	queries := make([]map[string]any, len(qs))
	formulas := make([]map[string]any, len(qs))
	for i, q := range qs {
		name := fmt.Sprintf("query%d", i+1)
		queries[i] = map[string]any{"data_source": "metrics", "name": name, "query": q.q}
		f := map[string]any{"formula": name}
		if q.alias != "" {
			f["alias"] = q.alias
		}
		formulas[i] = f
	}
	def := map[string]any{
		"type":  "timeseries",
		"title": title,
		"requests": []map[string]any{{
			"queries":         queries,
			"formulas":        formulas,
			"response_format": "timeseries",
			"display_type":    "line",
			"style":           map[string]any{"palette": "dog_classic", "line_type": "solid", "line_width": "normal"},
		}},
		"yaxis":          map[string]any{"scale": "linear", "min": "auto", "max": "auto", "include_zero": true},
		"show_legend":    true,
		"legend_layout":  "auto",
		"legend_columns": []string{"avg", "max", "value"},
	}
	if yLabel != "" {
		def["description"] = yLabel
	}
	attrs := map[string]any{"definition": def, "graph_size": "m", "split_by": map[string]any{"keys": []string{}, "tags": []string{}}}
	if !start.IsZero() {
		attrs["time"] = absTime(start, end)
	} else {
		attrs["time"] = nil
	}
	return Cell{"type": "notebook_cells", "attributes": attrs}
}

// chart is one standard chart of a run, parameterised by the run's tag filter.
type chart struct {
	title string
	unit  string
	build func(scope, prefix, container string) []query
}

func q(agg, metric, scope string, alias string) query {
	return query{q: fmt.Sprintf("%s:%s{%s}", agg, metric, scope), alias: alias}
}

var runCharts = []chart{
	{"Generated vs received (records/s)", "records/s", func(sc, p, _ string) []query {
		return []query{q("sum", p+".gen.records_per_sec", sc, "generated"), q("sum", p+".intake.logs_per_sec", sc, "received by intake")}
	}},
	{"Delivery ledger (cumulative over the window)", "records", func(sc, p, _ string) []query {
		return []query{q("max", p+".intake.delivery.missing", sc, "missing"), q("max", p+".intake.delivery.duplicates", sc, "duplicates"),
			q("max", p+".intake.delivery.out_of_order", sc, "out of order"), q("max", p+".intake.delivery.orphans", sc, "orphan continuation lines")}
	}},
	{"Latency, written → intake (seconds)", "s", func(sc, p, _ string) []query {
		return []query{q("max", p+".intake.latency.e2e.p50", sc, "p50"), q("max", p+".intake.latency.e2e.p99", sc, "p99"), q("max", p+".intake.latency.e2e.max", sc, "max"),
			q("max", p+".intake.latency.sender.p99", sc, "sender p99 (agent encode → intake)")}
	}},
	{"Bytes received: decompressed vs on the wire (B/s)", "B/s", func(sc, p, _ string) []query {
		return []query{q("sum", p+".intake.bytes.raw_per_sec", sc, "decompressed"), q("sum", p+".intake.bytes.wire_per_sec", sc, "wire")}
	}},
	{"Intake responses by status (/s)", "/s", func(sc, p, _ string) []query {
		return []query{{q: fmt.Sprintf("sum:%s.intake.responses_per_sec{%s} by {status}", p, sc)}}
	}},
	{"Agent CPU (%)", "%", func(sc, p, c string) []query {
		out := []query{q("avg", p+".agent.container.cpu_percent", sc, "container (docker stats via intake)"), q("avg", p+".agent.process.cpu_percent", sc, "core agent process (agent telemetry)")}
		if c != "" {
			out = append(out, q("avg", "docker.cpu.usage", sc+",container_name:"+c, "container (docker integration)"))
		}
		return out
	}},
	{"Agent memory (bytes)", "B", func(sc, p, c string) []query {
		out := []query{q("avg", p+".agent.container.memory_bytes", sc, "container (docker stats via intake)"), q("avg", p+".agent.process.rss_bytes", sc, "core agent RSS (agent telemetry)")}
		if c != "" {
			out = append(out, q("avg", "docker.mem.rss", sc+",container_name:"+c, "container RSS (docker integration)"))
		}
		return out
	}},
	{"Agent logs pipeline: bytes sent (B/s, from agent telemetry)", "B/s", func(sc, p, _ string) []query {
		return []query{q("sum", p+".agent.telemetry.logs.bytes_sent.rate", sc, "bytes sent (uncompressed)"), q("sum", p+".agent.telemetry.logs.encoded_bytes_sent.rate", sc, "encoded bytes sent")}
	}},
	{"Agent destination responses (/s, from agent telemetry)", "/s", func(sc, p, _ string) []query {
		return []query{{q: fmt.Sprintf("sum:%s.agent.telemetry.logs.destination_http_resp.rate{%s} by {status_code}", p, sc)}}
	}},
	{"Agent pipeline utilization (ratio, from agent telemetry)", "ratio", func(sc, p, _ string) []query {
		return []query{{q: fmt.Sprintf("avg:%s.agent.telemetry.logs_component_utilization.ratio{%s} by {name}", p, sc)}}
	}},
	{"Agent retries and network errors (/s)", "/s", func(sc, p, _ string) []query {
		return []query{q("sum", p+".agent.telemetry.logs.retry_count.rate", sc, "retries"), q("sum", p+".agent.telemetry.logs.network_errors.rate", sc, "network errors"), q("sum", p+".agent.telemetry.logs.dropped.rate", sc, "dropped by agent")}
	}},
	{"Tags per log and tag bytes per log", "", func(sc, p, _ string) []query {
		return []query{q("avg", p+".intake.tags.per_log", sc, "tags per log"), q("avg", p+".intake.tags.bytes_per_log", sc, "tag bytes per log")}
	}},
	{"Injected faults", "", func(sc, p, _ string) []query {
		return []query{q("max", p+".intake.faults.active", sc, "fault active (1 = yes)"), q("sum", p+".intake.faults.dropped_per_sec", sc, "dropped connections /s"), q("sum", p+".intake.faults.errored_per_sec", sc, "injected errors /s")}
	}},
	{"Generators (records/s by generator)", "records/s", func(sc, p, _ string) []query {
		return []query{{q: fmt.Sprintf("sum:%s.gen.records_per_sec{%s} by {gen}", p, sc)}}
	}},
	{"Agent memory by process (RSS, docker top via intake)", "B", func(sc, p, _ string) []query {
		return []query{{q: fmt.Sprintf("avg:%s.agent.proc.rss_bytes{%s} by {proc}", p, sc)}}
	}},
	{"Agent container memory: anon (processes) vs file cache", "B", func(sc, p, _ string) []query {
		return []query{q("avg", p+".agent.container.memory_anon_bytes", sc, "anon"), q("avg", p+".agent.container.memory_file_bytes", sc, "file cache")}
	}},
}

// keyCharts are the subset used per run in comparison notebooks.
var keyCharts = []int{0, 1, 2, 3, 5, 6, 7}

// abCharts are the four signals a logs-pipeline A/B test is judged on
// — throughput, pipeline saturation, CPU, memory — one chart each, both
// sides overlaid. Every other metric is in compare.md or one
// `aoc notebook --results` away.
var abCharts = []chart{
	{"Received vs generated (records/s)", "records/s", func(sc, p, _ string) []query {
		return []query{q("sum", p+".intake.logs_per_sec", sc, "received"), q("sum", p+".gen.records_per_sec", sc, "generated")}
	}},
	{"Pipeline utilization by component", "ratio", func(sc, p, _ string) []query {
		return []query{{q: fmt.Sprintf("avg:%s.agent.telemetry.logs_component_utilization.ratio{%s} by {name}", p, sc)}}
	}},
	{"Core agent CPU (%)", "%", func(sc, p, _ string) []query {
		return []query{q("avg", p+".agent.process.cpu_percent", sc, "")}
	}},
	{"Core agent memory (RSS, anon)", "bytes", func(sc, p, _ string) []query {
		return []query{q("avg", p+".agent.process.rss_bytes", sc, "RSS"), q("avg", p+".agent.container.memory_anon_bytes", sc, "anon")}
	}},
}

// faultsChart is appended only when a run injected faults; with none it
// is flat lines at zero.
var faultsChart = chart{"Injected faults", "", func(sc, p, _ string) []query {
	return []query{q("max", p+".intake.faults.active", sc, "active"), q("sum", p+".intake.faults.dropped_per_sec", sc, "dropped"),
		q("sum", p+".intake.faults.errored_per_sec", sc, "errored")}
}}

func anyFaults(cols []report.Column) bool {
	for _, c := range cols {
		for _, r := range c.Runs {
			if len(r.Faults) > 0 {
				return true
			}
		}
	}
	return false
}

func scopeFor(r *report.Report) string { return "run:" + r.Name }

func container(r *report.Report, o *Options) string {
	if o.AgentContainer != "" {
		return o.AgentContainer
	}
	return r.Agent.Container
}

// ForRun builds a single-run report notebook.
func ForRun(r *report.Report, o Options) *Notebook {
	o.defaults()
	name := o.Name
	if name == "" {
		name = "aoc run: " + r.Name
		if img := keysOf(r.Agent.Versions); img != "" {
			name += " · agent " + img
		}
	}
	nb := &Notebook{Name: truncate(name, 80), Type: "report", Start: r.WindowStart.Add(-o.Margin), End: r.WindowEnd.Add(o.Margin)}
	nb.Cells = append(nb.Cells, markdown(report.Summary(r)))
	sc := scopeFor(r)
	for _, c := range runCharts {
		nb.Cells = append(nb.Cells, timeseries(c.title, c.build(sc, o.Prefix, container(r, &o)), time.Time{}, time.Time{}, c.unit))
	}
	nb.Cells = append(nb.Cells, markdown(digDeeper(r, &o)))
	return nb
}

// ForCompare builds a notebook that puts several runs side by side: the
// comparison table first, then each run's key charts pinned to its window.
func ForCompare(reports []*report.Report, o Options) *Notebook {
	o.defaults()
	name := o.Name
	if name == "" {
		var names []string
		for _, r := range reports {
			names = append(names, r.Name)
		}
		name = "aoc compare: " + strings.Join(names, " vs ")
	}
	start, end := reports[0].WindowStart, reports[0].WindowEnd
	for _, r := range reports {
		if r.WindowStart.Before(start) {
			start = r.WindowStart
		}
		if r.WindowEnd.After(end) {
			end = r.WindowEnd
		}
	}
	nb := &Notebook{Name: truncate(name, 80), Type: "report", Start: start.Add(-o.Margin), End: end.Add(o.Margin)}
	nb.Cells = append(nb.Cells, markdown(strings.TrimPrefix(report.Compare(reports), "# aoc compare\n\n")))
	for _, r := range reports {
		head := fmt.Sprintf("## %s\n\n%s", r.Name, report.Summary(r))
		nb.Cells = append(nb.Cells, markdown(head))
		sc := scopeFor(r)
		for _, i := range keyCharts {
			c := runCharts[i]
			nb.Cells = append(nb.Cells, timeseries(fmt.Sprintf("%s — %s", r.Name, c.title), c.build(sc, o.Prefix, container(r, &o)), r.WindowStart.Add(-o.Margin), r.WindowEnd.Add(o.Margin), c.unit))
		}
	}
	nb.Cells = append(nb.Cells, markdown(digDeeper(reports[0], &o)))
	return nb
}

// ForAB builds the notebook of an A/B test, in the order an investigation
// reads it: what was tested and what differed; the conclusion once there
// is one; one cell per topic that regressed, with its evidence; the
// profiles diffed by function and located in the code, then the flame
// graphs — the whole agent, then each mover; then the signals the test
// is judged on, both sides overlaid (each earlier run time-shifted onto
// the latest one's window, to which the cells are pinned).
func ForAB(title string, cols []report.Column, o Options) *Notebook {
	o.defaults()
	if o.Name != "" {
		title = o.Name
	}
	type overlay struct {
		label string
		r     *report.Report
	}
	var runs []overlay
	for _, c := range cols {
		for i, r := range c.Runs {
			label := c.Name
			if len(c.Runs) > 1 {
				label = fmt.Sprintf("%s #%d", c.Name, i+1)
			}
			runs = append(runs, overlay{label, r})
		}
	}
	if len(runs) == 0 {
		return &Notebook{Name: truncate(title, 80), Type: "investigation"}
	}
	anchor := runs[0].r
	var longest time.Duration
	var timeshifted bool
	for _, x := range runs {
		if x.r.WindowStart.After(anchor.WindowStart) {
			anchor = x.r
		}
		if d := x.r.WindowEnd.Sub(x.r.WindowStart); d > longest {
			longest = d
		}
	}
	for _, x := range runs {
		if anchor.WindowStart.Sub(x.r.WindowStart).Round(time.Second) > 0 {
			timeshifted = true
		}
	}
	start, end := anchor.WindowStart.Add(-o.Margin), anchor.WindowStart.Add(longest).Add(o.Margin)
	nb := &Notebook{Name: truncate(title, 80), Type: "investigation", Start: start, End: end}
	f := o.Findings
	in := findings.Input{Experiment: o.Experiment, Cols: cols, AppURL: o.AppURL, Margin: o.Margin}

	// 1. The verdict, the signals against their thresholds, the gate, and
	// what was tested: the same first screen as findings.md.
	var head strings.Builder
	if f != nil {
		if len(f.Verdict) > 0 {
			fmt.Fprintf(&head, "**%s**\n\n", f.Verdict[0])
		}
		if t := findings.SignalsTable(f, cols); t != "" {
			head.WriteString(t + "\n")
		}
		for _, sg := range f.Signals {
			if sg.Note != "" {
				fmt.Fprintf(&head, "%s: %s.\n\n", findings.Title(sg.Name), sg.Note)
			}
		}
		if f.Gate.Line != "" {
			head.WriteString(f.Gate.Line + "\n\n")
		}
		fmt.Fprintf(&head, "## Tested\n\n%s\n", f.Tested)
	} else {
		for _, c := range cols {
			fmt.Fprintf(&head, "- **%s** — agent %s\n", c.Name, report.AgentLabel(c.Runs[0]))
		}
	}
	nb.Cells = append(nb.Cells, markdown(head.String()))

	// 2. The conclusion, when the investigation has been done.
	if f != nil && f.Conclusion != "" {
		nb.Cells = append(nb.Cells, markdown("## Conclusion\n\n"+f.Conclusion+"\n"))
	}

	// 3. One cell per topic that moved.
	if f != nil {
		for i, sec := range f.Sections {
			text := findings.SectionMarkdown(sec)
			if i == 0 {
				text = "## Where\n\n" + text
			}
			nb.Cells = append(nb.Cells, markdown(text))
		}
	}

	// 4. The profiles and the code behind the movers, then the profiler's
	// comparison view (same-origin iframes): the whole agent's CPU, then
	// one per mover, focused on it.
	if f != nil {
		var pc strings.Builder
		pc.WriteString("## Profiles\n\n" + findings.ProfilesMarkdown(f, in))
		if c := findings.CodeMarkdown(f); c != "" {
			pc.WriteString("\n## Code\n\n" + c)
		}
		nb.Cells = append(nb.Cells, markdown(pc.String()))
	}
	nb.Cells = append(nb.Cells, compareCells(f, cols, &o, timeshifted)...)

	// 5. The four signals the verdict rests on, both sides overlaid.
	charts := abCharts
	if anyFaults(cols) {
		charts = append(append([]chart{}, charts...), faultsChart)
	}
	for _, c := range charts {
		var qs []query
		for _, x := range runs {
			delta := anchor.WindowStart.Sub(x.r.WindowStart).Round(time.Second)
			for _, q := range c.build(scopeFor(x.r), o.Prefix, container(x.r, &o)) {
				qs = append(qs, shifted(q, x.label, delta))
			}
		}
		nb.Cells = append(nb.Cells, timeseries(c.title, qs, start, end, c.unit))
	}
	return nb
}

// compareCells are the side-by-side flame graphs: the whole agent's CPU,
// then one comparison focused on each mover the brief located. The other
// profile types are in the Profiles section's tables and a click away in
// the profiler, which is worth more than two more xl iframes.
func compareCells(f *findings.Result, cols []report.Column, o *Options, timeshifted bool) []Cell {
	if len(cols) < 2 || len(cols[0].Runs) == 0 || len(cols[1].Runs) == 0 {
		return nil
	}
	a, b := cols[0].Runs[0], cols[1].Runs[0]
	intro := fmt.Sprintf("## Flame graphs\n\nA = **%s** (`run:%s`) beside B = **%s** (`run:%s`), each over its own window: frames only one side has are coloured and the bar under B is the difference — the whole agent's CPU first, then one comparison per profile mover, focused on that function.",
		cols[0].Name, a.Name, cols[1].Name, b.Name)
	if timeshifted {
		intro += " The charts below overlay the runs on one axis, each earlier run shifted onto the latest one's window with `timeshift`, so the same second of the measured window lines up."
	}
	cells := []Cell{markdown(intro + "\n"), iframe(findings.CompareURL(a, b, o.AppURL, o.Margin, "cpu-time", ""))}
	if f == nil {
		return cells
	}
	for _, m := range findings.Movers(f) {
		cells = append(cells, markdown(fmt.Sprintf("### `%s` — %s, %s\n", m.Function, m.View, m.Delta)),
			iframe(findings.CompareURL(a, b, o.AppURL, o.Margin, m.ProfileType, m.Focus)))
	}
	return cells
}

// iframe embeds a same-origin Datadog page (the profiler allows framing
// from its own origin) as a tall cell.
func iframe(url string) Cell {
	return Cell{"type": "notebook_cells", "attributes": map[string]any{
		"definition": map[string]any{"type": "iframe", "url": url},
		"graph_size": "xl",
	}}
}

// shifted moves a run's query forward by delta onto the anchor window and
// labels it. Grouped queries get `run` added to the group-by instead of an
// alias, so the legend still tells the runs apart.
func shifted(q query, label string, delta time.Duration) query {
	out := q
	if strings.Contains(out.q, " by {") {
		out.q = strings.Replace(out.q, " by {", " by {run,", 1)
		out.alias = ""
	} else if out.alias != "" {
		out.alias = label + ": " + out.alias
	} else {
		out.alias = label
	}
	if delta > 0 {
		out.q = fmt.Sprintf("timeshift(%s, -%d)", out.q, int64(delta.Seconds()))
	}
	return out
}

func digDeeper(r *report.Report, o *Options) string {
	app := o.AppURL
	from, to := r.WindowStart.Add(-o.Margin).UnixMilli(), r.WindowEnd.Add(o.Margin).UnixMilli()
	host := o.AgentHost
	if host == "" {
		host = r.Agent.Hostname
	}
	var b strings.Builder
	fmt.Fprintf(&b, "### Dig deeper\n\n")
	fmt.Fprintf(&b, "Every series above is filtered on `run:%s`. The harness submits `%s.intake.*` (what the intake saw), `%s.gen.*` (what the generators wrote), `%s.agent.container.*` / `%s.agent.process.*` (the agent's CPU and memory) and `%s.agent.telemetry.*` (the agent's own `logs*` telemetry, counters as `.rate`). Change the tag to another run to compare, or ask the Datadog MCP, e.g. \"query aoc.intake.latency.e2e.p99 for run %s and explain the spikes\".\n\n",
		r.Name, o.Prefix, o.Prefix, o.Prefix, o.Prefix, o.Prefix, r.Name)
	fmt.Fprintf(&b, "- Events for this run (window, faults, generators): [%s/event/explorer?query=%s&from_ts=%d&to_ts=%d](%s/event/explorer?query=%s&from_ts=%d&to_ts=%d)\n",
		app, url.QueryEscape("source:aoc run:"+r.Name), from, to, app, url.QueryEscape("source:aoc run:"+r.Name), from, to)
	profQ := url.QueryEscape("service:datadog-agent run:" + r.Name)
	fmt.Fprintf(&b, "- Agent CPU/heap profiles for this run (`DD_INTERNAL_PROFILING_ENABLED=true`, uploaded through the trace-agent): [%s/profiling/explorer?query=%s&from_ts=%d&to_ts=%d&paused=true](%s/profiling/explorer?query=%s&from_ts=%d&to_ts=%d&paused=true) — or ask the MCP for the flame graph of `service:datadog-agent run:%s` filtered to `logs` frames\n",
		app, profQ, from, to, app, profQ, from, to, r.Name)
	if host != "" {
		fmt.Fprintf(&b, "- Agent host in Infrastructure: [%s/infrastructure?host=%s](%s/infrastructure?host=%s)\n", app, host, app, host)
	}
	fmt.Fprintf(&b, "- Metrics Explorer: [%s/metric/explorer](%s/metric/explorer) → search `%s.` and filter `run:%s`\n", app, app, o.Prefix, r.Name)
	fmt.Fprintf(&b, "\nThe full report (`report.md` / `report.json`) and the per-second `timeseries.csv` live in the results directory of this run; `aoc compare` diffs two of them.\n")
	return b.String()
}

func keysOf(m map[string]int64) string {
	best, bestN := "", int64(0)
	for k, v := range m {
		if v > bestN {
			best, bestN = k, v
		}
	}
	return best
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
