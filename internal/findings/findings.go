// Package findings turns an A/B comparison into an investigation brief:
// which headline metrics moved beyond the threshold, and for each, the
// evidence that says why — per-process memory, cgroup breakdown, the
// agent's own profiles diffed function by function, its telemetry and its
// log. It is written for a reader with a small budget: the regressions
// first, the numbers that explain them next, the full metric table last.
package findings

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/fmtutil"
	"github.com/UTXOnly/agent_of_chaos/internal/prof"
	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

// Finding is one headline metric that moved.
type Finding struct {
	Metric string  `json:"metric"`
	Kind   string  `json:"kind"`  // regression | improvement | attention
	Topic  string  `json:"topic"` // memory | cpu | latency | delivery | tags | bytes | stability
	A      string  `json:"a"`
	B      string  `json:"b"`
	Delta  string  `json:"delta"`
	Pct    float64 `json:"pct"`
}

// Evidence is one table or note that supports a topic's findings.
type Evidence struct {
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
}

// Input is what the brief is built from.
type Input struct {
	Experiment string
	Cols       []report.Column // exactly two: a (baseline), b
	Captures   [][]prof.Capture
	Threshold  float64 // percent; below it a change is noise
	AppURL     string
	ResultsDir string
	Margin     time.Duration
}

// Result is the brief.
type Result struct {
	Findings  []Finding             `json:"findings"`
	Evidence  map[string][]Evidence `json:"evidence"` // by topic
	Next      map[string][]string   `json:"next"`     // by topic: suggested follow-ups
	Profiles  []prof.Diff           `json:"profiles"` // per-function diffs, every view found
	Headline  []report.HeadlineRow  `json:"-"`
	Threshold float64               `json:"threshold"`
	Summary   string                `json:"summary"`
	Verdict   []string              `json:"verdict"` // one line per point
}

// topics maps headline metrics to the evidence that explains them.
var topics = map[string]string{
	"agent container mem avg": "memory", "agent container mem max": "memory", "agent container anon mem max": "memory",
	"core agent RSS max": "memory", "core agent Go heap in use": "memory",
	"agent container CPU avg": "cpu", "agent container CPU max": "cpu", "core agent process CPU avg": "cpu", "CPU seconds per 1M logs": "cpu",
	"e2e latency p50": "latency", "e2e latency p99": "latency", "e2e latency max": "latency", "sender latency p99": "latency",
	"delivery ratio": "delivery", "lost / missing": "delivery", "duplicates": "delivery", "out of order": "delivery",
	"orphan continuation lines": "delivery", "truncated": "delivery",
	"tags per log": "tags", "tag bytes per log": "tags",
	"received wire B/s": "bytes", "compression ratio": "bytes",
	"agent log errors": "stability", "agent log warnings": "stability", "core agent goroutines": "stability",
}

var topicOrder = []string{"delivery", "memory", "cpu", "latency", "stability", "bytes", "tags"}

// Build classifies the headline and gathers evidence for every topic with
// a finding.
func Build(in Input) *Result {
	if in.Threshold <= 0 {
		in.Threshold = 10
	}
	if in.Margin == 0 {
		in.Margin = time.Minute
	}
	res := &Result{Evidence: map[string][]Evidence{}, Next: map[string][]string{}, Threshold: in.Threshold}
	res.Headline = report.Headline(in.Cols)
	if len(in.Cols) < 2 {
		return res
	}
	for _, row := range res.Headline {
		f := classify(row, in.Threshold)
		if f != nil {
			res.Findings = append(res.Findings, *f)
		}
	}
	sort.SliceStable(res.Findings, func(i, j int) bool {
		a, b := res.Findings[i], res.Findings[j]
		if a.Kind != b.Kind {
			return rank(a.Kind) < rank(b.Kind)
		}
		if ta, tb := topicRank(a.Topic), topicRank(b.Topic); ta != tb {
			return ta < tb
		}
		return math.Abs(a.Pct) > math.Abs(b.Pct)
	})

	// Profiles: every view both sides have.
	var an [2][]*prof.Analysis
	for i := 0; i < 2 && i < len(in.Captures); i++ {
		an[i] = prof.Analyze(in.Captures[i])
	}
	res.Profiles = prof.Compare(an[0], an[1], 15)

	seen := map[string]bool{}
	for _, f := range res.Findings {
		if seen[f.Topic] {
			continue
		}
		seen[f.Topic] = true
		res.Evidence[f.Topic] = evidenceFor(f.Topic, in, res)
		res.Next[f.Topic] = nextFor(f.Topic, in, res)
	}
	res.Verdict = verdict(res, in)
	res.Summary = summary(res)
	return res
}

func rank(kind string) int {
	switch kind {
	case "regression":
		return 0
	case "attention":
		return 1
	}
	return 2
}

func topicRank(t string) int {
	for i, x := range topicOrder {
		if x == t {
			return i
		}
	}
	return len(topicOrder)
}

// classify decides whether a row is worth a finding. Delivery counters
// that are non-zero on either side are always reported ("attention" when
// both sides have them, so the workload's own flakiness is visible).
func classify(row report.HeadlineRow, threshold float64) *Finding {
	if len(row.Values) < 2 || len(row.Pct) < 1 {
		return nil
	}
	a, b, pct := row.Values[0], row.Values[1], row.Pct[0]
	topic := topics[row.Metric]
	if topic == "" {
		topic = "other"
	}
	f := &Finding{Metric: row.Metric, Topic: topic, A: row.Cells[0], B: row.Cells[1], Delta: row.Deltas[0], Pct: pct}
	isCounter := topic == "delivery" && row.Metric != "delivery ratio" || topic == "stability" && row.Metric != "core agent goroutines"
	switch {
	case isCounter && a == 0 && b == 0:
		return nil
	case isCounter && a == 0 && b > 0:
		f.Kind = "regression"
		f.Pct = math.Inf(1)
		return f
	case isCounter && a > 0 && b == 0:
		f.Kind = "improvement"
		f.Pct = -100
		return f
	case isCounter && topic == "delivery" && math.Abs(pct) < threshold:
		// Lost/duplicated/reordered records on both sides, in similar
		// numbers: the workload or the harness, not the change — but not
		// something to hide either.
		f.Kind = "attention"
		return f
	case math.IsNaN(pct) || math.Abs(pct) < threshold || row.Sense == "neutral":
		return nil
	}
	worse := pct > 0
	if row.Sense == "higher" {
		worse = pct < 0
	}
	if worse {
		f.Kind = "regression"
	} else {
		f.Kind = "improvement"
	}
	return f
}

func verdict(res *Result, in Input) []string {
	var regs, imps, att []string
	byTopic := map[string][]string{}
	for _, f := range res.Findings {
		s := fmt.Sprintf("%s %s", f.Metric, f.Delta)
		switch f.Kind {
		case "regression":
			regs = append(regs, s)
			byTopic[f.Topic] = append(byTopic[f.Topic], f.Metric)
		case "improvement":
			imps = append(imps, s)
		default:
			att = append(att, s)
		}
	}
	var out []string
	if len(regs) == 0 {
		out = append(out, fmt.Sprintf("✅ no regression beyond ±%.0f%% on the headline metrics", in.Threshold))
	} else {
		out = append(out, fmt.Sprintf("⚠️ %d regression(s): %s", len(regs), strings.Join(regs, "; ")))
	}
	if len(imps) > 0 {
		out = append(out, fmt.Sprintf("✅ %d improvement(s): %s", len(imps), strings.Join(imps, "; ")))
	}
	if len(att) > 0 {
		out = append(out, fmt.Sprintf("👀 present on both sides (workload or harness, not the change): %s", strings.Join(att, "; ")))
	}
	a, b := in.Cols[0], in.Cols[1]
	da, db := a.Value(func(r *report.Report) float64 { return r.Delivery.Ratio }), b.Value(func(r *report.Report) float64 { return r.Delivery.Ratio })
	out = append(out, fmt.Sprintf("delivery %s vs %s; lost %s vs %s; duplicates %s vs %s",
		fmtutil.Pct(da), fmtutil.Pct(db),
		fmtutil.Int(int64(a.Value(func(r *report.Report) float64 { return float64(r.Delivery.Missing) }))), fmtutil.Int(int64(b.Value(func(r *report.Report) float64 { return float64(r.Delivery.Missing) }))),
		fmtutil.Int(int64(a.Value(func(r *report.Report) float64 { return float64(r.Delivery.Duplicates) }))), fmtutil.Int(int64(b.Value(func(r *report.Report) float64 { return float64(r.Delivery.Duplicates) })))))
	return out
}

func summary(res *Result) string {
	n := map[string]int{}
	topicsOf := map[string][]string{}
	for _, f := range res.Findings {
		n[f.Kind]++
		if f.Kind == "regression" && !contains(topicsOf["r"], f.Topic) {
			topicsOf["r"] = append(topicsOf["r"], f.Topic)
		}
	}
	if n["regression"] == 0 {
		if n["improvement"] == 0 {
			return "no change beyond the threshold"
		}
		return fmt.Sprintf("no regressions; %d improvement(s)", n["improvement"])
	}
	return fmt.Sprintf("%d regression(s) in %s; %d improvement(s)", n["regression"], strings.Join(topicsOf["r"], ", "), n["improvement"])
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// ── evidence ─────────────────────────────────────────────────────────────────

func evidenceFor(topic string, in Input, res *Result) []Evidence {
	a, b := in.Cols[0], in.Cols[1]
	var ev []Evidence
	add := func(title, md string) {
		if strings.TrimSpace(md) != "" {
			ev = append(ev, Evidence{Title: title, Markdown: md})
		}
	}
	switch topic {
	case "memory":
		add("Where the memory is", memoryTable(in.Cols))
		if t := report.ProcessTable(in.Cols); t != "" {
			add("Processes in the agent container (docker top)", t)
		}
		add("Core agent heap in use, by function (profiler, averaged over the window)", diffTable(res.Profiles, "heap in use", a.Name, b.Name))
		add("Core agent allocation rate, by function", diffTable(res.Profiles, "allocation rate", a.Name, b.Name))
	case "cpu":
		if t := report.ProcessTable(in.Cols); t != "" {
			add("Processes in the agent container (docker top)", t)
		}
		add("Core agent CPU, by function (profiler; % of one core)", diffTable(res.Profiles, "CPU", a.Name, b.Name))
		add("Runtime", rowsTable(in.Cols, []string{"core agent goroutines", "core agent Go heap in use", "CPU seconds per 1M logs", "received logs /s"}))
	case "latency":
		add("Latency and the pipeline", rowsTable(in.Cols, []string{"e2e latency p50", "e2e latency p99", "e2e latency max", "sender latency p99", "logs per payload p50", "payload wire p50", "requests /s"}))
		add("Pipeline utilization (agent telemetry, ratio of time busy)", teleGaugeTable(in.Cols, "logs_component_utilization__ratio", "name"))
		add("Retries, errors, responses", retriesTable(in.Cols))
		add("Core agent CPU, by function (profiler; % of one core)", diffTable(res.Profiles, "CPU", a.Name, b.Name))
	case "delivery":
		add("Delivery ledger", rowsTable(in.Cols, []string{"generated records", "unique delivered", "lost / missing", "duplicates", "out of order", "orphan continuation lines", "truncated"}))
		add("Retries, errors, responses", retriesTable(in.Cols))
		add("Faults injected", faultsNote(in.Cols))
		add("Agent log: most repeated warnings and errors", logTable(in.Cols))
	case "tags":
		add("Tag keys (share of logs carrying each key)", tagKeysTable(in.Cols))
		add("Payload geometry", rowsTable(in.Cols, []string{"tags per log", "tag bytes per log", "received wire B/s", "compression ratio", "logs per payload p50", "payload wire p50"}))
	case "bytes":
		add("Payload geometry", rowsTable(in.Cols, []string{"received wire B/s", "compression ratio", "logs per payload p50", "payload wire p50", "requests /s", "tags per log", "tag bytes per log"}))
		add("Tag keys (share of logs carrying each key)", tagKeysTable(in.Cols))
	case "stability":
		add("Agent log: most repeated warnings and errors", logTable(in.Cols))
		add("Runtime", rowsTable(in.Cols, []string{"core agent goroutines", "core agent Go heap in use", "core agent RSS max"}))
		add("Retries, errors, responses", retriesTable(in.Cols))
	}
	return ev
}

// rowsTable is a slice of the comparison table.
func rowsTable(cols []report.Column, names []string) string {
	rows := report.Rows(cols, names)
	if len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "| metric | %s | %s | Δ |\n|---|---|---|---|\n", cols[0].Name, cols[1].Name)
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", r.Metric, r.Cells[0], r.Cells[1], r.Deltas[0])
	}
	return b.String()
}

// memoryTable puts the container total next to its cgroup split and the
// core agent's own numbers, so "the container grew" becomes "anon grew, in
// process X" or "it is page cache".
func memoryTable(cols []report.Column) string {
	rows := report.Rows(cols, []string{"agent container mem max", "agent container anon mem max", "agent container file cache max", "core agent RSS max", "core agent Go heap in use"})
	var b strings.Builder
	fmt.Fprintf(&b, "| | %s | %s | Δ |\n|---|---|---|---|\n", cols[0].Name, cols[1].Name)
	for _, r := range rows {
		label := r.Metric
		switch r.Metric {
		case "agent container mem max":
			label = "container memory max (usage − inactive file cache)"
		case "agent container anon mem max":
			label = "├ anon: the processes' own memory (max)"
		case "agent container file cache max":
			label = "├ file: page cache charged to the container (max)"
		case "core agent RSS max":
			label = "core agent process RSS max"
		case "core agent Go heap in use":
			label = "core agent Go heap in use (end of window)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", label, r.Cells[0], r.Cells[1], r.Deltas[0])
	}
	heapSys := func(r *report.Report) float64 { return report.Tele(r, "go_memstats_sys_bytes", "") }
	ha, hb := cols[0].Value(heapSys), cols[1].Value(heapSys)
	if ha > 0 || hb > 0 {
		fmt.Fprintf(&b, "| core agent Go runtime total (go_memstats_sys_bytes) | %s | %s | |\n", fmtutil.BytesF(ha), fmtutil.BytesF(hb))
	}
	fmt.Fprintf(&b, "\n**Reading:** %s\n", memoryReading(rows))
	return b.String()
}

// memoryReading turns the memory rows into one sentence: which of the
// container's numbers actually moved, and what that usually means.
func memoryReading(rows []report.HeadlineRow) string {
	pct := map[string]float64{}
	val := map[string][2]float64{}
	for _, r := range rows {
		if len(r.Pct) > 0 && len(r.Values) > 1 {
			pct[r.Metric] = r.Pct[0]
			val[r.Metric] = [2]float64{r.Values[0], r.Values[1]}
		}
	}
	cont, anon, file, rss, heap := pct["agent container mem max"], pct["agent container anon mem max"], pct["agent container file cache max"], pct["core agent RSS max"], pct["core agent Go heap in use"]
	up := func(p float64) bool { return !math.IsNaN(p) && p >= 5 }
	flat := func(p float64) bool { return math.IsNaN(p) || math.Abs(p) < 5 }
	fv := func(m string) string {
		v := val[m]
		return fmt.Sprintf("%s → %s", fmtutil.BytesF(v[0]), fmtutil.BytesF(v[1]))
	}
	switch {
	case up(cont) && !up(anon) && up(file):
		return fmt.Sprintf("the container grew through page cache (file %s), not process memory (anon %s) — files read, not a leak; the kernel reclaims it under pressure.", fv("agent container file cache max"), fv("agent container anon mem max"))
	case up(anon) && flat(rss):
		return fmt.Sprintf("anon grew (%s) while the core agent's RSS did not (%s): the memory is in another process — see the process table.", fv("agent container anon mem max"), fv("core agent RSS max"))
	case up(anon) && up(rss) && flat(heap):
		return fmt.Sprintf("the core agent's RSS grew (%s) but its Go heap did not (%s): non-Go memory in the core agent (cgo, embedded Python, mmap) or a larger runtime reserve.", fv("core agent RSS max"), fv("core agent Go heap in use"))
	case up(anon) && up(rss) && up(heap):
		return fmt.Sprintf("the core agent's Go heap grew (%s) with its RSS (%s): the heap-in-use table below names the functions holding it.", fv("core agent Go heap in use"), fv("core agent RSS max"))
	case up(cont) && up(anon):
		return fmt.Sprintf("process memory grew (anon %s); the process table says where.", fv("agent container anon mem max"))
	case !up(cont) && !up(anon) && !up(rss):
		return "nothing here grew by more than 5 %: the flagged metric is within noise for a single run."
	}
	return "compare anon (process memory) against file (page cache) first, then the core agent's RSS against its Go heap; the process table attributes anon to a process."
}

// diffTable renders one profile view's per-function diff.
func diffTable(diffs []prof.Diff, label, aName, bName string) string {
	for _, d := range diffs {
		if d.Label != label {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "`%s` · total %s → %s (%s); %d vs %d profile period(s) merged\n\n", d.Service, prof.Format(d.ATotal, d.Unit), prof.Format(d.BTotal, d.Unit), prof.FormatDelta(d.BTotal-d.ATotal, d.Unit), d.ACaps, d.BCaps)
		fmt.Fprintf(&b, "| function (flat) | %s | %s | Δ |\n|---|---|---|---|\n", aName, bName)
		for _, r := range d.Rows {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", prof.ShortFunc(r.Function), prof.Format(r.A, d.Unit), prof.Format(r.B, d.Unit), prof.FormatDelta(r.Delta, d.Unit))
		}
		return b.String()
	}
	return ""
}

// teleGaugeTable lists a labelled telemetry gauge's last values by label.
func teleGaugeTable(cols []report.Column, name, labelKey string) string {
	labels := map[string]bool{}
	var order []string
	for _, c := range cols {
		for _, r := range c.Runs {
			for _, t := range r.Telemetry {
				if t.Name == name && !labels[t.Labels] {
					labels[t.Labels] = true
					order = append(order, t.Labels)
				}
			}
		}
	}
	if len(order) == 0 {
		return ""
	}
	sort.Strings(order)
	var b strings.Builder
	fmt.Fprintf(&b, "| %s | %s | %s |\n|---|---|---|\n", labelKey, cols[0].Name, cols[1].Name)
	for _, l := range order {
		get := func(r *report.Report) float64 {
			for _, t := range r.Telemetry {
				if t.Name == name && t.Labels == l {
					return t.Last
				}
			}
			return math.NaN()
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", labelValue(l, labelKey), fmtutil.Float(cols[0].Value(get), 3), fmtutil.Float(cols[1].Value(get), 3))
	}
	return b.String()
}

func labelValue(labels, key string) string {
	for _, kv := range strings.Split(strings.Trim(labels, "{}"), ",") {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return strings.Trim(v, `"`)
		}
	}
	return labels
}

// retriesTable is the agent-side counters that explain latency and loss.
func retriesTable(cols []report.Column) string {
	type row struct {
		label string
		get   func(*report.Report) float64
	}
	rows := []row{
		{"retries (logs__retry_count)", func(r *report.Report) float64 { return report.TeleDelta(r, "logs__retry_count", "") }},
		{"network errors (logs__network_errors)", func(r *report.Report) float64 { return report.TeleDelta(r, "logs__network_errors", "") }},
		{"dropped by agent (logs__dropped)", func(r *report.Report) float64 { return report.TeleDelta(r, "logs__dropped", "") }},
		{"intake responses 202", func(r *report.Report) float64 { return float64(r.HTTP.ByStatus["202"]) }},
		{"intake responses non-2xx", func(r *report.Report) float64 {
			n := int64(0)
			for k, v := range r.HTTP.ByStatus {
				if !strings.HasPrefix(k, "2") {
					n += v
				}
			}
			return float64(n)
		}},
		{"connections dropped / errors injected", func(r *report.Report) float64 { return float64(r.HTTP.FaultDropped + r.HTTP.FaultErrored) }},
	}
	var b strings.Builder
	fmt.Fprintf(&b, "| counter (Δ over window) | %s | %s |\n|---|---|---|\n", cols[0].Name, cols[1].Name)
	for _, r := range rows {
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.label, fmtutil.Float(cols[0].Value(r.get), 0), fmtutil.Float(cols[1].Value(r.get), 0))
	}
	return b.String()
}

func faultsNote(cols []report.Column) string {
	n := 0
	for _, c := range cols {
		for _, r := range c.Runs {
			n += len(r.Faults)
		}
	}
	if n == 0 {
		return "No faults were injected: the intake answered 202 to everything, so what is missing was lost between the file and the agent's sender, not on the network."
	}
	var b strings.Builder
	for _, c := range cols {
		for _, r := range c.Runs {
			for _, f := range r.Faults {
				fmt.Fprintf(&b, "- `%s` %s — %s\n", r.Name, f.At.UTC().Format("15:04:05"), f.Desc)
			}
		}
	}
	return b.String()
}

// logTable is each side's most repeated warnings/errors.
func logTable(cols []report.Column) string {
	var b strings.Builder
	for _, c := range cols {
		var counts []report.NameCount
		var errs, warns, lines int64
		trunc := false
		for _, r := range c.Runs {
			if r.AgentLog == nil {
				continue
			}
			errs += r.AgentLog.Errors
			warns += r.AgentLog.Warnings
			lines += r.AgentLog.Lines
			trunc = trunc || r.AgentLog.Truncated
			counts = append(counts, r.AgentLog.Top...)
		}
		if lines == 0 {
			continue
		}
		merged := map[string]int64{}
		for _, nc := range counts {
			merged[nc.Name] += nc.Count
		}
		var top []report.NameCount
		for k, v := range merged {
			top = append(top, report.NameCount{Name: k, Count: v})
		}
		sort.Slice(top, func(i, j int) bool {
			if top[i].Count != top[j].Count {
				return top[i].Count > top[j].Count
			}
			return top[i].Name < top[j].Name
		})
		if len(top) > 6 {
			top = top[:6]
		}
		note := ""
		if trunc {
			note = " (tail only)"
		}
		fmt.Fprintf(&b, "**%s** — %s lines%s, %s errors, %s warnings\n\n", c.Name, fmtutil.Int(lines), note, fmtutil.Int(errs), fmtutil.Int(warns))
		if len(top) > 0 {
			b.WriteString("| count | level · source · message |\n|---|---|\n")
			for _, t := range top {
				fmt.Fprintf(&b, "| %s | `%s` |\n", fmtutil.Int(t.Count), strings.ReplaceAll(t.Name, "|", "\\|"))
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// tagKeysTable shows which tag keys each side sends and how often.
func tagKeysTable(cols []report.Column) string {
	keys := map[string]bool{}
	var order []string
	share := func(c report.Column, key string) float64 {
		var vals []float64
		for _, r := range c.Runs {
			v := 0.0
			for _, k := range r.Tags.Keys {
				if k.Name == key && r.Delivery.ReceivedLogs > 0 {
					v = float64(k.Count) / float64(r.Delivery.ReceivedLogs)
				}
			}
			vals = append(vals, v)
		}
		return report.Median(vals)
	}
	for _, c := range cols {
		for _, r := range c.Runs {
			for _, k := range r.Tags.Keys {
				if !keys[k.Name] {
					keys[k.Name] = true
					order = append(order, k.Name)
				}
			}
		}
	}
	if len(order) == 0 {
		return ""
	}
	sort.Slice(order, func(i, j int) bool { return share(cols[0], order[i]) > share(cols[0], order[j]) })
	if len(order) > 20 {
		order = order[:20]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "| tag key | %s | %s |\n|---|---|---|\n", cols[0].Name, cols[1].Name)
	for _, k := range order {
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", k, fmtutil.Pct(math.Min(1, share(cols[0], k))), fmtutil.Pct(math.Min(1, share(cols[1], k))))
	}
	return b.String()
}

// ── next steps ───────────────────────────────────────────────────────────────

// nextFor suggests the few Datadog MCP calls that go one level deeper than
// the local evidence, with the exact filters filled in so the reader does
// not have to reconstruct them.
func nextFor(topic string, in Input, res *Result) []string {
	a, b := in.Cols[0].Runs[0], in.Cols[1].Runs[0]
	if len(in.Cols[1].Runs) == 0 || len(in.Cols[0].Runs) == 0 {
		return nil
	}
	win := func(r *report.Report) string {
		return fmt.Sprintf("fromString=%s toString=%s", r.WindowStart.Add(-in.Margin).UTC().Format(time.RFC3339), r.WindowEnd.Add(in.Margin).UTC().Format(time.RFC3339))
	}
	scope := func(r *report.Report) string { return "service:datadog-agent run:" + r.Name }
	topFn := func(label string) string {
		for _, d := range res.Profiles {
			if d.Label == label && len(d.Rows) > 0 {
				return regexpSafe(prof.ShortFunc(d.Rows[0].Function))
			}
		}
		return ""
	}
	var out []string
	switch topic {
	case "memory":
		around := ""
		if fn := topFn("heap in use"); fn != "" {
			around = fmt.Sprintf(" — compare the two stacks around `%s`", fn)
		}
		out = append(out, fmt.Sprintf("If the process table blames the core agent and the heap table is not enough: `explore_profiling_flame_graph` profileType=heap-live-size queryString=\"%s\" %s, then the same for `%s`%s.", scope(b), win(b), scope(a), around))
		out = append(out, "If anon grew but core RSS did not, the process table names the process; only the core agent is profiled by default (set DD_APM_INTERNAL_PROFILING_ENABLED / DD_PROCESS_CONFIG_INTERNAL_PROFILING_ENABLED for the others).")
		out = append(out, fmt.Sprintf("Trend over the window: `get_datadog_metric` `avg:aoc.agent.proc.rss_bytes{experiment:%s} by {variant,proc}` — a ramp is a leak, a step is a cache.", in.Experiment))
	case "cpu":
		out = append(out, fmt.Sprintf("`explore_profiling_flame_graph` profileType=cpu-time queryString=\"%s\" %s frameRegexFilter=\"%s\", then the same for `%s`.", scope(b), win(b), topFn("CPU"), scope(a)))
		out = append(out, fmt.Sprintf("`get_profiling_timeseries` profileType=cpu-time queryString=\"service:datadog-agent experiment:%s\" groupBy=@lastFrame.function %s.", in.Experiment, win(b)))
	case "latency":
		out = append(out, fmt.Sprintf("Where the time goes: `get_datadog_metric` `avg:aoc.agent.telemetry.logs_component_utilization.ratio{experiment:%s} by {variant,name}` — the component near 1.0 is the bottleneck.", in.Experiment))
		out = append(out, fmt.Sprintf("`explore_profiling_flame_graph` profileType=cpu-time queryString=\"%s\" %s frameRegexFilter=\"logs\".", scope(b), win(b)))
	case "delivery":
		out = append(out, fmt.Sprintf("Per-stream gaps are in `%s/report.md` (streams table); gaps right after a rotation point at the tailer.", in.Cols[1].Name))
		out = append(out, fmt.Sprintf("`search_datadog_events` query=\"source:aoc run:%s\" for the window/fault timeline; `search_datadog_logs` is not useful (the logs never reach Datadog).", b.Name))
	case "tags", "bytes":
		out = append(out, fmt.Sprintf("`get_datadog_metric` `avg:aoc.intake.tags.bytes_per_log{experiment:%s} by {variant}` and `sum:aoc.intake.bytes.wire_per_sec{experiment:%s} by {variant}` over the session.", in.Experiment, in.Experiment))
	case "stability":
		out = append(out, fmt.Sprintf("Read the repeated messages in `%s/agent.log` with grep (never the whole file); goroutine growth: `avg:aoc.agent.telemetry.go_goroutines{experiment:%s} by {variant}`.", in.ResultsDir, in.Experiment))
	}
	return out
}

func title(s string) string {
	if s == "cpu" {
		return "CPU"
	}
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func regexpSafe(s string) string {
	r := strings.NewReplacer("(", "\\(", ")", "\\)", "*", "\\*", ".", "\\.", "[", "\\[", "]", "\\]")
	return r.Replace(s)
}

// ── rendering ────────────────────────────────────────────────────────────────

// Markdown renders the brief (findings.md).
func Markdown(res *Result, in Input) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	a, bb := in.Cols[0], in.Cols[1]
	p("# aoc A/B %s — findings\n\n", in.Experiment)
	p("**%s** = %s · **%s** = %s · window %s · %d run(s) per side · threshold ±%.0f%%\n\n", a.Name, report.AgentLabel(a.Runs[0]), bb.Name, report.AgentLabel(bb.Runs[0]), fmtutil.Duration(time.Duration(a.Runs[0].Seconds*float64(time.Second))), len(a.Runs), in.Threshold)
	p("## Verdict\n\n")
	for _, v := range res.Verdict {
		p("- %s\n", v)
	}
	p("\n")
	byKind := map[string][]Finding{}
	for _, f := range res.Findings {
		byKind[f.Kind] = append(byKind[f.Kind], f)
	}
	for _, kind := range []string{"regression", "attention", "improvement"} {
		fs := byKind[kind]
		if len(fs) == 0 {
			continue
		}
		switch kind {
		case "regression":
			p("## Regressions\n\n")
		case "attention":
			p("## Present on both sides\n\n")
		default:
			p("## Improvements\n\n")
		}
		printed := map[string]bool{}
		for _, f := range fs {
			if printed[f.Topic] {
				continue
			}
			printed[f.Topic] = true
			var lines []string
			for _, g := range fs {
				if g.Topic == f.Topic {
					lines = append(lines, fmt.Sprintf("%s %s → %s (%s)", g.Metric, g.A, g.B, g.Delta))
				}
			}
			p("### %s — %s\n\n", title(f.Topic), strings.Join(lines, "; "))
			if kind == "regression" || kind == "attention" {
				for _, ev := range res.Evidence[f.Topic] {
					p("**%s**\n\n%s\n", ev.Title, ev.Markdown)
				}
				if next := res.Next[f.Topic]; len(next) > 0 {
					p("**Next, if the cause is not obvious from the above**\n\n")
					for _, n := range next {
						p("- %s\n", n)
					}
					p("\n")
				}
			} else if ev := res.Evidence[f.Topic]; len(ev) > 0 {
				// Improvements get their first table only: enough to confirm
				// the intended effect without doubling the brief.
				p("%s\n", ev[0].Markdown)
			}
		}
	}
	if len(res.Profiles) > 0 {
		p("## Profiles (core agent, merged over the window)\n\n")
		p("Rates are per wall-clock second (CPU as %% of one core); levels are averaged over the profile periods. Largest movers first.\n\n")
		for _, d := range res.Profiles {
			p("**%s** — `%s`: %s → %s (%s), %d vs %d period(s)\n\n", d.Label, d.Service, prof.Format(d.ATotal, d.Unit), prof.Format(d.BTotal, d.Unit), prof.FormatDelta(d.BTotal-d.ATotal, d.Unit), d.ACaps, d.BCaps)
			p("| function (flat) | %s | %s | Δ |\n|---|---|---|---|\n", a.Name, bb.Name)
			n := 0
			for _, r := range d.Rows {
				if n++; n > 10 {
					break
				}
				p("| `%s` | %s | %s | %s |\n", prof.ShortFunc(r.Function), prof.Format(r.A, d.Unit), prof.Format(r.B, d.Unit), prof.FormatDelta(r.Delta, d.Unit))
			}
			p("\n")
		}
	} else {
		p("## Profiles\n\nNo profiler uploads were captured on both sides (the window must span at least one full profiling period, 60 s by default, and DD_INTERNAL_PROFILING_ENABLED must be on).\n\n")
	}
	p("## Headline\n\n| metric | %s | %s | Δ |\n|---|---|---|---|\n", a.Name, bb.Name)
	for _, r := range res.Headline {
		p("| %s | %s | %s | %s |\n", r.Metric, r.Cells[0], r.Cells[1], r.Deltas[0])
	}
	p("\nEvery metric, the per-process table and the telemetry counters: `compare.md`. Per run: `<side>/report.md`, `<side>/profiles/` (pprof), `<side>/agent.log`.\n")
	return b.String()
}

// EventText is the compact version for the Datadog event (≤ 4000 chars).
func EventText(res *Result, in Input, nbURL string) string {
	var b strings.Builder
	a, bb := in.Cols[0], in.Cols[1]
	fmt.Fprintf(&b, "%%%%%% \n**%s** %s vs **%s** %s\n\n", a.Name, report.AgentLabel(a.Runs[0]), bb.Name, report.AgentLabel(bb.Runs[0]))
	for _, v := range res.Verdict {
		fmt.Fprintf(&b, "- %s\n", v)
	}
	regs := 0
	for _, f := range res.Findings {
		if f.Kind == "regression" {
			regs++
		}
	}
	if regs > 0 {
		fmt.Fprintf(&b, "\n| regression | %s | %s | Δ |\n|---|---|---|---|\n", a.Name, bb.Name)
		for _, f := range res.Findings {
			if f.Kind == "regression" {
				fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", f.Metric, f.A, f.B, f.Delta)
			}
		}
	}
	if nbURL != "" {
		fmt.Fprintf(&b, "\n[notebook](%s)\n", nbURL)
	}
	fmt.Fprintf(&b, "\nfindings.md in %s\n%%%%%%", in.ResultsDir)
	s := b.String()
	if len(s) > 3900 {
		s = s[:3850] + "\n…\n%%%"
	}
	return s
}

// ProfileLinks renders the Datadog profiler link for every run: the
// explorer scoped to the run's tag and window, where the flame graph's type
// (CPU, heap live size, allocations) is a click away.
func ProfileLinks(cols []report.Column, appURL string, margin time.Duration) string {
	var b strings.Builder
	for _, c := range cols {
		for _, r := range c.Runs {
			from, to := r.WindowStart.Add(-margin).UnixMilli(), r.WindowEnd.Add(margin).UnixMilli()
			q := url.QueryEscape("service:datadog-agent run:" + r.Name)
			fmt.Fprintf(&b, "- **%s** `run:%s` (%s): [profiles](%s/profiling/explorer?query=%s&from_ts=%d&to_ts=%d&paused=true)\n", c.Name, r.Name, report.AgentLabel(r), appURL, q, from, to)
		}
	}
	return b.String()
}
