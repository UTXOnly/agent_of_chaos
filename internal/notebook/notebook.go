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
	"github.com/UTXOnly/agent_of_chaos/internal/prof"
	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

func profFormat(v float64, unit string) string { return prof.Format(v, unit) }
func profDelta(v float64, unit string) string  { return prof.FormatDelta(v, unit) }
func shortFunc(name string) string             { return prof.ShortFunc(name) }

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
	// test (ForAB); it drives the real-time timeline cell.
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

// abCharts are the charts an A/B notebook overlays: delivery, latency and
// the agent's resources. The rest are one `aoc notebook --results` away.
var abCharts = []int{0, 1, 2, 5, 6, 14, 15, 9, 7}

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

// ForAB builds the notebook of an A/B test. It leads with the findings —
// what regressed and the evidence behind it, cell by cell — then the
// profiler links, then the charts with all runs overlaid. The runs
// happened one after the other, so each earlier run is time-shifted onto
// the window of the latest one (the anchor) and the cells are pinned to
// that window. The full metric table closes the notebook.
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
		return &Notebook{Name: truncate(title, 80), Type: "report"}
	}
	anchor, first := runs[0].r, runs[0].r
	var longest time.Duration
	for _, x := range runs {
		if x.r.WindowStart.After(anchor.WindowStart) {
			anchor = x.r
		}
		if x.r.WindowStart.Before(first.WindowStart) {
			first = x.r
		}
		if d := x.r.WindowEnd.Sub(x.r.WindowStart); d > longest {
			longest = d
		}
	}
	start, end := anchor.WindowStart.Add(-o.Margin), anchor.WindowStart.Add(longest).Add(o.Margin)
	nb := &Notebook{Name: truncate(title, 80), Type: "investigation", Start: start, End: end}

	// 1. Who was compared, and the verdict.
	var head strings.Builder
	for _, c := range cols {
		r := c.Runs[0]
		fmt.Fprintf(&head, "- **%s** — agent %s", c.Name, report.AgentLabel(r))
		if r.Agent.Digest != "" {
			fmt.Fprintf(&head, ", digest `%s`", r.Agent.Digest)
		}
		var windows []string
		for _, x := range c.Runs {
			windows = append(windows, fmt.Sprintf("`run:%s` %s → %s UTC", x.Name, x.WindowStart.UTC().Format("15:04:05"), x.WindowEnd.UTC().Format("15:04:05")))
		}
		fmt.Fprintf(&head, "; %s\n", strings.Join(windows, ", "))
	}
	if f := o.Findings; f != nil {
		fmt.Fprintf(&head, "\n## Verdict (threshold ±%.0f%%)\n\n", f.Threshold)
		for _, v := range f.Verdict {
			fmt.Fprintf(&head, "- %s\n", v)
		}
	}
	nb.Cells = append(nb.Cells, markdown(head.String()))

	// 2. One cell per topic with a regression: the evidence.
	if f := o.Findings; f != nil {
		printed := map[string]bool{}
		for _, fd := range f.Findings {
			if fd.Kind == "improvement" || printed[fd.Topic] {
				continue
			}
			printed[fd.Topic] = true
			var cell strings.Builder
			var lines []string
			for _, g := range f.Findings {
				if g.Topic == fd.Topic && g.Kind == fd.Kind {
					lines = append(lines, fmt.Sprintf("%s %s → %s (%s)", g.Metric, g.A, g.B, g.Delta))
				}
			}
			kind := "Regression"
			if fd.Kind == "attention" {
				kind = "Present on both sides"
			}
			fmt.Fprintf(&cell, "## %s — %s\n\n%s\n\n", kind, fd.Topic, strings.Join(lines, "; "))
			for _, ev := range f.Evidence[fd.Topic] {
				fmt.Fprintf(&cell, "**%s**\n\n%s\n", ev.Title, ev.Markdown)
			}
			if next := f.Next[fd.Topic]; len(next) > 0 {
				cell.WriteString("**Next**\n\n")
				for _, n := range next {
					fmt.Fprintf(&cell, "- %s\n", n)
				}
			}
			nb.Cells = append(nb.Cells, markdown(cell.String()))
		}
		var imps []string
		for _, g := range f.Findings {
			if g.Kind == "improvement" {
				imps = append(imps, fmt.Sprintf("%s %s → %s (%s)", g.Metric, g.A, g.B, g.Delta))
			}
		}
		if len(imps) > 0 {
			nb.Cells = append(nb.Cells, markdown("## Improvements\n\n- "+strings.Join(imps, "\n- ")+"\n"))
		}
	}

	// 3. The profiles: per-function diffs and where to click.
	var prof strings.Builder
	prof.WriteString("## Profiles\n\n")
	if f := o.Findings; f != nil && len(f.Profiles) > 0 {
		prof.WriteString("The core agent's continuous-profiler uploads, merged over each window and diffed by function (rates per wall-clock second, CPU as % of one core; levels averaged over the periods). Largest movers first.\n\n")
		for _, d := range f.Profiles {
			fmt.Fprintf(&prof, "**%s** — `%s`: %s → %s\n\n| function (flat) | %s | %s | Δ |\n|---|---|---|---|\n", d.Label, d.Service, profFormat(d.ATotal, d.Unit), profFormat(d.BTotal, d.Unit), cols[0].Name, cols[1].Name)
			n := 0
			for _, r := range d.Rows {
				if n++; n > 8 {
					break
				}
				fmt.Fprintf(&prof, "| `%s` | %s | %s | %s |\n", shortFunc(r.Function), profFormat(r.A, d.Unit), profFormat(r.B, d.Unit), profDelta(r.Delta, d.Unit))
			}
			prof.WriteString("\n")
		}
	} else {
		prof.WriteString("No profiler uploads were captured on both sides; the links below still open whatever the agent uploaded.\n\n")
	}
	prof.WriteString("Flame graphs in Datadog (scoped to each run's tag and window; pick CPU, heap live size or allocations there, or press ⇄ Compare on one and choose the other run as A):\n\n")
	prof.WriteString(findings.ProfileLinks(cols, o.AppURL, o.Margin))
	nb.Cells = append(nb.Cells, markdown(prof.String()))

	// 4. The session in real time, then the overlaid charts.
	if o.Experiment != "" {
		sc := "experiment:" + o.Experiment
		span := timeseries("Timeline: the runs as they happened (records/s by variant)",
			[]query{{q: fmt.Sprintf("sum:%s.intake.logs_per_sec{%s} by {variant}", o.Prefix, sc)}, {q: fmt.Sprintf("sum:%s.gen.records_per_sec{%s} by {variant}", o.Prefix, sc)}},
			first.WindowStart.Add(-o.Margin), latestEnd(cols).Add(o.Margin), "records/s")
		nb.Cells = append(nb.Cells, span)
	}
	nb.Cells = append(nb.Cells, markdown(fmt.Sprintf("## Charts\n\nEvery chart overlays the runs on one time axis: `%s` is drawn at its real time and each earlier run is shifted forward onto it with `timeshift`, so the same second of the measured window lines up. Legends carry the side (and `run` for grouped series).", anchor.Name)))
	for _, i := range abCharts {
		c := runCharts[i]
		var qs []query
		for _, x := range runs {
			delta := anchor.WindowStart.Sub(x.r.WindowStart).Round(time.Second)
			for _, q := range c.build(scopeFor(x.r), o.Prefix, container(x.r, &o)) {
				qs = append(qs, shifted(q, x.label, delta))
			}
		}
		nb.Cells = append(nb.Cells, timeseries(c.title, qs, start, end, c.unit))
	}

	// 5. Everything else.
	var tail strings.Builder
	tail.WriteString("## All metrics\n\nMedians when a side ran more than once.\n\n")
	tail.WriteString(strings.TrimPrefix(report.CompareColumns(cols), "# aoc compare\n\n"))
	nb.Cells = append(nb.Cells, markdown(tail.String()))
	nb.Cells = append(nb.Cells, markdown(digDeeperAB(cols, &o)))
	return nb
}

// latestEnd is the latest window end across all columns.
func latestEnd(cols []report.Column) time.Time {
	var end time.Time
	for _, c := range cols {
		for _, x := range c.Runs {
			if x.WindowEnd.After(end) {
				end = x.WindowEnd
			}
		}
	}
	return end
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

func digDeeperAB(cols []report.Column, o *Options) string {
	app := o.AppURL
	var b strings.Builder
	fmt.Fprintf(&b, "### Dig deeper\n\n")
	if o.Experiment != "" {
		fmt.Fprintf(&b, "Every run of this test carries `experiment:%s` and `variant:<side>` on its `%s.*` metrics, events and on the agent's own metrics and profiles, so `… {experiment:%s} by {variant}` puts the sides on one chart at their real times. Per run:\n\n", o.Experiment, o.Prefix, o.Experiment)
	}
	for _, c := range cols {
		for _, r := range c.Runs {
			from, to := r.WindowStart.Add(-o.Margin).UnixMilli(), r.WindowEnd.Add(o.Margin).UnixMilli()
			evQ := url.QueryEscape("source:aoc run:" + r.Name)
			profQ := url.QueryEscape("service:datadog-agent run:" + r.Name)
			fmt.Fprintf(&b, "- **%s** `run:%s` (%s): [events](%s/event/explorer?query=%s&from_ts=%d&to_ts=%d) · [agent profiles](%s/profiling/explorer?query=%s&from_ts=%d&to_ts=%d&paused=true)\n",
				c.Name, r.Name, report.AgentLabel(r), app, evQ, from, to, app, profQ, from, to)
		}
	}
	if len(cols) > 1 && len(cols[0].Runs) > 0 && len(cols[1].Runs) > 0 {
		a, bb := cols[0].Runs[0].Name, cols[1].Runs[0].Name
		fmt.Fprintf(&b, "\nAsk the Datadog MCP: \"compare `%s.agent.process.cpu_percent` and `%s.intake.latency.e2e.p99` between `run:%s` and `run:%s`\", or \"show the CPU flame graph for `service:datadog-agent run:%s` filtered to `logs` frames, then the same for `run:%s`\".\n", o.Prefix, o.Prefix, a, bb, a, bb)
	}
	fmt.Fprintf(&b, "\n`findings.md` (this brief), `compare.md`, each run's `report.md` / `profiles/` / `agent.log`, and the `aoc.yaml` that ran are in the results directory of the test.\n")
	return b.String()
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
