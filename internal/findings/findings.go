// Package findings turns an A/B comparison into an investigation brief:
// what was tested, what differed, where (per-process memory, the cgroup
// split, the agent's own profiles diffed function by function with the
// source line behind each mover, telemetry, the log), which files in the
// agent's source those movers live in and whether they changed between the
// two builds, and the conclusion once someone has looked. It is written
// for a reader with a small budget: nothing is printed twice, rows that did
// not move are left to compare.md.
package findings

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
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

// Evidence is one table or note that supports a section.
type Evidence struct {
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
}

// Section is one topic with regressions (or delivery problems on both
// sides): its findings, the one-sentence reading, the evidence, and what
// to do next if that is not enough.
type Section struct {
	Topic    string     `json:"topic"`
	Kind     string     `json:"kind"`
	Findings []Finding  `json:"findings"`
	Reading  string     `json:"reading,omitempty"`
	Evidence []Evidence `json:"evidence"`
	Next     []string   `json:"next,omitempty"`
}

// CodeRow is one profile mover located in the agent's source.
type CodeRow struct {
	Function string `json:"function"`
	View     string `json:"view"`
	Delta    string `json:"delta"`
	File     string `json:"file"`
	Line     int64  `json:"line"`
	Where    string `json:"where"`            // repo | stdlib | dep | ""
	Change   string `json:"change,omitempty"` // between the two commits: new in b, +N/−M, unchanged, removed in b
	URL      string `json:"url,omitempty"`
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
	Workload   string   // profile name and description
	ConfigDiff []string // what only one side had, e.g. "tagfilter only: DD_X=1"
	Source     string   // a checkout of the agent's repository, for the code section
	Conclusion string   // conclusion.md, once written
}

// Result is the brief.
type Result struct {
	Tested      string               `json:"tested"`
	Verdict     []string             `json:"verdict"`
	Summary     string               `json:"summary"`
	Findings    []Finding            `json:"findings"`
	Changed     []report.HeadlineRow `json:"-"`
	Sections    []Section            `json:"sections"`
	Profiles    []prof.Diff          `json:"profiles"`
	ProfileNote string               `json:"profile_note,omitempty"`
	Code        []CodeRow            `json:"code,omitempty"`
	CodeNote    string               `json:"code_note,omitempty"`
	Conclusion  string               `json:"conclusion,omitempty"`
	Threshold   float64              `json:"threshold"`
	Headline    []report.HeadlineRow `json:"-"`
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
// a regression.
func Build(in Input) *Result {
	if in.Threshold <= 0 {
		in.Threshold = 10
	}
	if in.Margin == 0 {
		in.Margin = time.Minute
	}
	res := &Result{Threshold: in.Threshold, Conclusion: strings.TrimSpace(in.Conclusion)}
	res.Headline = report.Headline(in.Cols)
	if len(in.Cols) < 2 {
		return res
	}
	for _, row := range res.Headline {
		if f := classify(row, in.Threshold); f != nil {
			res.Findings = append(res.Findings, *f)
		}
		if changed(row) {
			res.Changed = append(res.Changed, row)
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

	var an [2][]*prof.Analysis
	for i := 0; i < 2 && i < len(in.Captures); i++ {
		an[i] = prof.Analyze(in.Captures[i])
	}
	res.Profiles = prof.Compare(an[0], an[1], 12)
	res.ProfileNote = profileNote(res.Profiles, in.Cols)

	// The code section first: the readings refer to it.
	res.Code, res.CodeNote = codeRows(in, res)

	seen := map[string]bool{}
	for _, f := range res.Findings {
		if f.Kind == "improvement" || seen[f.Topic] {
			continue
		}
		seen[f.Topic] = true
		sec := Section{Topic: f.Topic, Kind: f.Kind}
		for _, g := range res.Findings {
			if g.Topic == f.Topic && g.Kind != "improvement" {
				sec.Findings = append(sec.Findings, g)
			}
		}
		sec.Reading, sec.Evidence = evidenceFor(f.Topic, f.Kind, in, res)
		sec.Next = nextFor(f.Topic, in, res)
		res.Sections = append(res.Sections, sec)
	}
	res.Tested = tested(in)
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

// changed says whether a headline row belongs in the "what differed" table:
// moved by 5 % or more, or a counter that appeared or vanished.
func changed(row report.HeadlineRow) bool {
	if len(row.Values) < 2 || len(row.Pct) < 1 {
		return false
	}
	a, b, pct := row.Values[0], row.Values[1], row.Pct[0]
	if a == 0 && b == 0 {
		return false
	}
	if a == 0 || b == 0 {
		return true
	}
	return !math.IsNaN(pct) && math.Abs(pct) >= 5
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

// tested is the "what we tested" paragraph.
func tested(in Input) string {
	a, b := in.Cols[0], in.Cols[1]
	side := func(c report.Column) string {
		r := c.Runs[0]
		s := fmt.Sprintf("**%s** = %s", c.Name, report.AgentLabel(r))
		if r.Agent.Commit != "" {
			s += fmt.Sprintf(", commit `%s`", short(r.Agent.Commit))
		}
		return s
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s · %s.", side(a), side(b))
	if len(in.ConfigDiff) > 0 {
		fmt.Fprintf(&sb, " %s.", strings.Join(in.ConfigDiff, "; "))
	}
	r := a.Runs[0]
	work := in.Workload
	if work == "" {
		work = "the profile"
	}
	streams := 0
	for _, g := range r.Generators {
		streams += g.ActiveStreams
	}
	fmt.Fprintf(&sb, " Workload %s — %d streams at %s, %s window, %d round(s) per side.", work, streams, fmtutil.Rate(r.Throughput.GenRecordsPerSec), fmtutil.Duration(time.Duration(r.Seconds*float64(time.Second))), len(a.Runs))
	get := func(c report.Column, f func(*report.Report) float64) float64 { return c.Value(f) }
	fmt.Fprintf(&sb, " Delivery %s vs %s, lost %s vs %s, duplicates %s vs %s.",
		fmtutil.Pct(get(a, func(r *report.Report) float64 { return r.Delivery.Ratio })), fmtutil.Pct(get(b, func(r *report.Report) float64 { return r.Delivery.Ratio })),
		fmtutil.Int(int64(get(a, func(r *report.Report) float64 { return float64(r.Delivery.Missing) }))), fmtutil.Int(int64(get(b, func(r *report.Report) float64 { return float64(r.Delivery.Missing) }))),
		fmtutil.Int(int64(get(a, func(r *report.Report) float64 { return float64(r.Delivery.Duplicates) }))), fmtutil.Int(int64(get(b, func(r *report.Report) float64 { return float64(r.Delivery.Duplicates) }))))
	return sb.String()
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func verdict(res *Result, in Input) []string {
	var regs, imps, att []string
	for _, f := range res.Findings {
		s := fmt.Sprintf("%s %s → %s (%s)", f.Metric, f.A, f.B, f.Delta)
		switch f.Kind {
		case "regression":
			regs = append(regs, s)
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
		out = append(out, fmt.Sprintf("👀 on both sides (workload or harness, not the change): %s", strings.Join(att, "; ")))
	}
	return out
}

func summary(res *Result) string {
	n := map[string]int{}
	var rt []string
	for _, f := range res.Findings {
		n[f.Kind]++
		if f.Kind == "regression" && !contains(rt, f.Topic) {
			rt = append(rt, f.Topic)
		}
	}
	if n["regression"] == 0 {
		if n["improvement"] == 0 {
			return "no change beyond the threshold"
		}
		return fmt.Sprintf("no regressions; %d improvement(s)", n["improvement"])
	}
	return fmt.Sprintf("%d regression(s) in %s; %d improvement(s)", n["regression"], strings.Join(rt, ", "), n["improvement"])
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

// evidenceFor returns the reading and the tables that explain a topic. Each
// table appears once in the brief: profile diffs printed here are skipped
// by the Profiles section.
func evidenceFor(topic, kind string, in Input, res *Result) (string, []Evidence) {
	a, b := in.Cols[0], in.Cols[1]
	var ev []Evidence
	add := func(title, md string) {
		if strings.TrimSpace(md) != "" {
			ev = append(ev, Evidence{Title: title, Markdown: md})
		}
	}
	rows := report.Rows(in.Cols, []string{"agent container mem max", "agent container anon mem max", "agent container file cache max", "core agent RSS max", "core agent Go heap in use"})
	reading := ""
	switch topic {
	case "memory":
		reading = memoryReading(rows)
		add("Where the memory is", memoryTable(in.Cols, rows))
		add("Processes that moved (docker top)", processTable(in.Cols, false))
		if coreMemoryRose(rows) {
			add("Core agent heap in use, by function (profiler, averaged over the window)", diffTable(res.Profiles, "heap in use", a.Name, b.Name, 8, in))
			add("Core agent allocation rate, by function", diffTable(res.Profiles, "allocation rate", a.Name, b.Name, 8, in))
		}
	case "cpu":
		reading = cpuReading(in, res)
		add("Core agent CPU, by function (profiler; % of one core)", diffTable(res.Profiles, "CPU", a.Name, b.Name, 8, in))
		add("Processes that moved (docker top)", processTable(in.Cols, false))
	case "latency":
		reading = latencyReading(in.Cols)
		add("Pipeline utilization (agent telemetry, share of time busy)", teleGaugeTable(in.Cols, "logs_component_utilization__ratio", "name"))
		add("Retries, errors, responses", retriesTable(in.Cols))
		add("Core agent CPU, by function (profiler; % of one core)", diffTable(res.Profiles, "CPU", a.Name, b.Name, 5, in))
	case "delivery":
		add("Delivery ledger", rowsTable(in.Cols, []string{"generated records", "unique delivered", "lost / missing", "duplicates", "out of order", "orphan continuation lines", "truncated"}, true))
		add("Retries, errors, responses", retriesTable(in.Cols))
		add("Faults injected", faultsNote(in.Cols))
		if kind == "regression" {
			add("Agent log: most repeated warnings and errors", logTable(in.Cols))
		}
	case "tags", "bytes":
		reading = bytesReading(in.Cols)
		add("Payload geometry", rowsTable(in.Cols, []string{"received wire B/s", "compression ratio", "logs per payload p50", "payload wire p50", "tags per log", "tag bytes per log"}, false))
		add("Tag keys that changed (share of logs carrying each key)", tagKeysTable(in.Cols))
	case "stability":
		add("Agent log: most repeated warnings and errors", logTable(in.Cols))
		add("Runtime", rowsTable(in.Cols, []string{"core agent goroutines", "core agent Go heap in use", "core agent RSS max"}, false))
	}
	return reading, ev
}

// rowsTable is a slice of the comparison table; onlyChanged drops rows
// that did not move.
func rowsTable(cols []report.Column, names []string, onlyChanged bool) string {
	rows := report.Rows(cols, names)
	var b strings.Builder
	n := 0
	fmt.Fprintf(&b, "| metric | %s | %s | Δ |\n|---|---|---|---|\n", cols[0].Name, cols[1].Name)
	for _, r := range rows {
		if onlyChanged && !changed(r) {
			continue
		}
		n++
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", r.Metric, r.Cells[0], r.Cells[1], r.Deltas[0])
	}
	if n == 0 {
		return ""
	}
	return b.String()
}

func memoryTable(cols []report.Column, rows []report.HeadlineRow) string {
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
	return b.String()
}

func pctOf(rows []report.HeadlineRow, metric string) (float64, [2]float64) {
	for _, r := range rows {
		if r.Metric == metric && len(r.Pct) > 0 && len(r.Values) > 1 {
			return r.Pct[0], [2]float64{r.Values[0], r.Values[1]}
		}
	}
	return math.NaN(), [2]float64{}
}

func coreMemoryRose(rows []report.HeadlineRow) bool {
	rss, _ := pctOf(rows, "core agent RSS max")
	heap, _ := pctOf(rows, "core agent Go heap in use")
	return rss >= 5 || heap >= 5
}

// memoryReading turns the memory rows into one sentence: which of the
// container's numbers actually moved, and what that usually means.
func memoryReading(rows []report.HeadlineRow) string {
	cont, _ := pctOf(rows, "agent container mem max")
	anon, av := pctOf(rows, "agent container anon mem max")
	file, fv := pctOf(rows, "agent container file cache max")
	rss, rv := pctOf(rows, "core agent RSS max")
	heap, hv := pctOf(rows, "core agent Go heap in use")
	up := func(p float64) bool { return !math.IsNaN(p) && p >= 5 }
	flat := func(p float64) bool { return math.IsNaN(p) || math.Abs(p) < 5 }
	f := func(v [2]float64) string { return fmtutil.BytesF(v[0]) + " → " + fmtutil.BytesF(v[1]) }
	switch {
	case up(cont) && !up(anon) && up(file):
		return fmt.Sprintf("The container grew through page cache (file %s), not process memory (anon %s): files read, not a leak; the kernel reclaims it under pressure. Not a regression.", f(fv), f(av))
	case up(anon) && flat(rss):
		return fmt.Sprintf("Process memory grew (anon %s) while the core agent's RSS did not (%s): the memory is in another process — see the process table.", f(av), f(rv))
	case up(anon) && up(rss) && flat(heap):
		return fmt.Sprintf("The core agent's RSS grew (%s) but its Go heap did not (%s): non-Go memory in the core agent (cgo, embedded Python, mmap) or a larger runtime reserve.", f(rv), f(hv))
	case up(anon) && up(rss) && up(heap):
		return fmt.Sprintf("The core agent's Go heap grew (%s) with its RSS (%s): the heap-in-use table names the functions holding it.", f(hv), f(rv))
	case up(cont) && up(anon):
		return fmt.Sprintf("Process memory grew (anon %s); the process table says where.", f(av))
	case !up(cont) && !up(anon) && !up(rss):
		return "Nothing here grew by more than 5 %: the flagged metric is within noise for a single run."
	}
	return "Compare anon (process memory) against file (page cache) first, then the core agent's RSS against its Go heap; the process table attributes anon to a process."
}

// cpuReading names the top CPU mover and whether it is new code.
func cpuReading(in Input, res *Result) string {
	for _, d := range res.Profiles {
		if d.Label != "CPU" || len(d.Rows) == 0 {
			continue
		}
		r := d.Rows[0]
		if r.Delta <= 0 {
			return fmt.Sprintf("The core agent's CPU by function did not shift toward any one place (largest mover %s %s); look at the process table for other processes.", prof.ShortFunc(r.Function), prof.FormatDelta(r.Delta, d.Unit))
		}
		where := ""
		if row := findCode(res, r.Function); row != nil && row.Change != "" {
			where = fmt.Sprintf(" (`%s:%d`, %s)", row.File, row.Line, row.Change)
		}
		return fmt.Sprintf("The largest CPU mover is `%s`%s: %s → %s of one core.", prof.ShortFunc(r.Function), where, prof.Format(r.A, d.Unit), prof.Format(r.B, d.Unit))
	}
	return ""
}

func findCode(res *Result, function string) *CodeRow {
	short := prof.ShortFunc(function)
	for i := range res.Code {
		if res.Code[i].Function == short {
			return &res.Code[i]
		}
	}
	return nil
}

// latencyReading names a saturated pipeline component, if any.
func latencyReading(cols []report.Column) string {
	best, bestV := "", 0.0
	for _, r := range cols[1].Runs {
		for _, t := range r.Telemetry {
			if t.Name == "logs_component_utilization__ratio" && t.Last > bestV {
				best, bestV = labelValue(t.Labels, "name"), t.Last
			}
		}
	}
	if bestV >= 0.9 {
		return fmt.Sprintf("Pipeline component `%s` is busy %.0f%% of the time in %s: the bottleneck.", best, bestV*100, cols[1].Name)
	}
	if best != "" {
		return fmt.Sprintf("No pipeline component is saturated (busiest: `%s` at %.0f%%); look at retries and the CPU diff.", best, bestV*100)
	}
	return ""
}

// bytesReading explains a compression-ratio drop that comes from removing
// repetitive bytes.
func bytesReading(cols []report.Column) string {
	rows := report.Rows(cols, []string{"compression ratio", "received wire B/s", "tag bytes per log"})
	comp, _ := pctOf(rows, "compression ratio")
	wire, wv := pctOf(rows, "received wire B/s")
	tagb, tv := pctOf(rows, "tag bytes per log")
	if comp <= -5 && tagb <= -5 && wire <= 1 {
		return fmt.Sprintf("The compression ratio fell because the bytes removed (tags, %s per log) were the most repetitive ones; bytes on the wire still went %s → %s (%.1f%%). Not a regression.", prof.FormatDelta(tv[1]-tv[0], "B"), fmtutil.BytesF(wv[0])+"/s", fmtutil.BytesF(wv[1])+"/s", wire)
	}
	if wire >= 5 {
		return fmt.Sprintf("Bytes on the wire rose %.1f%%: more logs, bigger logs, more tags or worse compression — the geometry table says which.", wire)
	}
	return ""
}

// processTable lists the container's processes; with all=false only the
// ones that moved by 5 % or appeared/vanished, plus the core agent.
func processTable(cols []report.Column, all bool) string {
	names := map[string]bool{}
	var order []string
	for _, c := range cols {
		for _, r := range c.Runs {
			for _, ps := range r.Resources.Processes {
				if !names[ps.Name] {
					names[ps.Name] = true
					order = append(order, ps.Name)
				}
			}
		}
	}
	if len(order) == 0 {
		return ""
	}
	val := func(c report.Column, name string, get func(report.ProcessStat) float64) (float64, bool) {
		var vals []float64
		for _, r := range c.Runs {
			for _, ps := range r.Resources.Processes {
				if ps.Name == name {
					vals = append(vals, get(ps))
				}
			}
		}
		if len(vals) == 0 {
			return 0, false
		}
		return report.Median(vals), true
	}
	rss := func(p report.ProcessStat) float64 { return float64(p.RSSMax) }
	cpu := func(p report.ProcessStat) float64 { return p.CPUAvg }
	sort.SliceStable(order, func(i, j int) bool {
		a, _ := val(cols[0], order[i], rss)
		b, _ := val(cols[0], order[j], rss)
		return a > b
	})
	var b strings.Builder
	fmt.Fprintf(&b, "| process | %s RSS max | %s RSS max | Δ RSS | CPU avg %s → %s |\n|---|---|---|---|---|\n", cols[0].Name, cols[1].Name, cols[0].Name, cols[1].Name)
	n := 0
	for i, name := range order {
		ra, oka := val(cols[0], name, rss)
		rb, okb := val(cols[1], name, rss)
		ca, _ := val(cols[0], name, cpu)
		cb, _ := val(cols[1], name, cpu)
		var delta string
		moved := true
		switch {
		case !oka && !okb:
			continue
		case !oka:
			delta = "+" + fmtutil.BytesF(rb) + " (new)"
		case !okb:
			delta = "gone"
		default:
			delta = report.DeltaLower(ra, rb)
			moved = ra == 0 || math.Abs((rb-ra)/ra*100) >= 5
		}
		if !all && !moved && i != 0 {
			continue
		}
		n++
		cell := func(v float64, ok bool) string {
			if !ok {
				return "–"
			}
			return fmtutil.BytesF(v)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %.1f%% → %.1f%% |\n", name, cell(ra, oka), cell(rb, okb), delta, ca, cb)
	}
	if n == 0 {
		return ""
	}
	if !all && n == 1 {
		b.WriteString("\nNo other process moved by 5 % or more.\n")
	}
	return b.String()
}

// diffTable renders one profile view's per-function diff with the source
// line of each mover.
func diffTable(diffs []prof.Diff, label, aName, bName string, n int, in Input) string {
	for _, d := range diffs {
		if d.Label != label {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "`%s` total %s → %s (%s), %d vs %d period(s) merged\n\n", d.Service, prof.Format(d.ATotal, d.Unit), prof.Format(d.BTotal, d.Unit), prof.FormatDelta(d.BTotal-d.ATotal, d.Unit), d.ACaps, d.BCaps)
		fmt.Fprintf(&b, "| function (flat) | source | %s | %s | Δ |\n|---|---|---|---|---|\n", aName, bName)
		for i, r := range d.Rows {
			if i >= n {
				break
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", prof.ShortFunc(r.Function), sourceCell(r.File, r.Line, in), prof.Format(r.A, d.Unit), prof.Format(r.B, d.Unit), prof.FormatDelta(r.Delta, d.Unit))
		}
		return b.String()
	}
	return ""
}

// sourceCell is "path:line" relative to the agent's module, marked when
// it is the Go runtime or a dependency.
func sourceCell(file string, line int64, in Input) string {
	if file == "" {
		return "–"
	}
	path, where := prof.RepoPath(file, module(in))
	s := fmt.Sprintf("`%s:%d`", path, line)
	switch where {
	case "stdlib":
		s = "std " + s
	case "dep":
		s = "dep " + s
	}
	return s
}

// module is the Go module path of the agent, from the profiler's
// repository tag (https://github.com/DataDog/datadog-agent →
// github.com/DataDog/datadog-agent).
func module(in Input) string {
	for _, c := range in.Cols {
		for _, r := range c.Runs {
			if r.Agent.Repo != "" {
				u := strings.TrimSuffix(r.Agent.Repo, ".git")
				u = strings.TrimPrefix(strings.TrimPrefix(u, "https://"), "http://")
				return u
			}
		}
	}
	return "github.com/DataDog/datadog-agent"
}

func profileNote(diffs []prof.Diff, cols []report.Column) string {
	if len(diffs) == 0 {
		return ""
	}
	var parts []string
	for _, d := range diffs {
		if d.Label == "CPU" || d.Label == "heap in use" || d.Label == "allocation rate" {
			parts = append(parts, fmt.Sprintf("%s %s → %s (%s)", d.Label, prof.Format(d.ATotal, d.Unit), prof.Format(d.BTotal, d.Unit), report.DeltaLower(d.ATotal, d.BTotal)))
		}
	}
	return fmt.Sprintf("`%s`: %s · %d vs %d profiling period(s) merged", diffs[0].Service, strings.Join(parts, " · "), diffs[0].ACaps, diffs[0].BCaps)
}

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

func retriesTable(cols []report.Column) string {
	type row struct {
		label string
		get   func(*report.Report) float64
	}
	rows := []row{
		{"retries (logs__retry_count)", func(r *report.Report) float64 { return report.TeleDelta(r, "logs__retry_count", "") }},
		{"network errors (logs__network_errors)", func(r *report.Report) float64 { return report.TeleDelta(r, "logs__network_errors", "") }},
		{"dropped by agent (logs__dropped)", func(r *report.Report) float64 { return report.TeleDelta(r, "logs__dropped", "") }},
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
	any := false
	for _, r := range rows {
		va, vb := cols[0].Value(r.get), cols[1].Value(r.get)
		if va != 0 || vb != 0 {
			any = true
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.label, fmtutil.Float(va, 0), fmtutil.Float(vb, 0))
	}
	if !any {
		return "No retries, network errors, agent-side drops, non-2xx responses or injected faults on either side.\n"
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
		return ""
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

// logTable is each side's most repeated warnings/errors, skipping messages
// both sides repeat equally (the harness's own noise).
func logTable(cols []report.Column) string {
	counts := make([]map[string]int64, len(cols))
	lines := make([]int64, len(cols))
	errs, warns := make([]int64, len(cols)), make([]int64, len(cols))
	for i, c := range cols {
		counts[i] = map[string]int64{}
		for _, r := range c.Runs {
			if r.AgentLog == nil {
				continue
			}
			lines[i] += r.AgentLog.Lines
			errs[i] += r.AgentLog.Errors
			warns[i] += r.AgentLog.Warnings
			for _, nc := range r.AgentLog.Top {
				counts[i][nc.Name] += nc.Count
			}
		}
	}
	if lines[0] == 0 && lines[1] == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s lines, %s errors, %s warnings · %s: %s lines, %s errors, %s warnings\n\n", cols[0].Name, fmtutil.Int(lines[0]), fmtutil.Int(errs[0]), fmtutil.Int(warns[0]), cols[1].Name, fmtutil.Int(lines[1]), fmtutil.Int(errs[1]), fmtutil.Int(warns[1]))
	type kv struct {
		k    string
		a, b int64
	}
	var rows []kv
	seen := map[string]bool{}
	for i := range cols {
		for k := range counts[i] {
			if seen[k] {
				continue
			}
			seen[k] = true
			va, vb := counts[0][k], counts[1][k]
			if va == vb {
				continue
			}
			rows = append(rows, kv{k, va, vb})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		di, dj := abs64(rows[i].b-rows[i].a), abs64(rows[j].b-rows[j].a)
		if di != dj {
			return di > dj
		}
		return rows[i].k < rows[j].k
	})
	if len(rows) == 0 {
		b.WriteString("The repeated messages are the same on both sides.\n")
		return b.String()
	}
	if len(rows) > 8 {
		rows = rows[:8]
	}
	fmt.Fprintf(&b, "| %s | %s | level · source · message |\n|---|---|---|\n", cols[0].Name, cols[1].Name)
	for _, r := range rows {
		fmt.Fprintf(&b, "| %d | %d | `%s` |\n", r.a, r.b, strings.ReplaceAll(r.k, "|", "\\|"))
	}
	return b.String()
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// tagKeysTable shows the tag keys whose share of logs changed.
func tagKeysTable(cols []report.Column) string {
	keys := map[string]bool{}
	var order []string
	share := func(c report.Column, key string) float64 {
		var vals []float64
		for _, r := range c.Runs {
			v := 0.0
			for _, k := range r.Tags.Keys {
				if k.Name == key && r.Delivery.ReceivedLogs > 0 {
					v = math.Min(1, float64(k.Count)/float64(r.Delivery.ReceivedLogs))
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
	var b strings.Builder
	fmt.Fprintf(&b, "| tag key | %s | %s |\n|---|---|---|\n", cols[0].Name, cols[1].Name)
	n := 0
	sort.Slice(order, func(i, j int) bool { return share(cols[0], order[i]) > share(cols[0], order[j]) })
	for _, k := range order {
		sa, sb := share(cols[0], k), share(cols[1], k)
		if math.Abs(sa-sb) < 0.01 {
			continue
		}
		n++
		fmt.Fprintf(&b, "| `%s` | %s | %s |\n", k, fmtutil.Pct(sa), fmtutil.Pct(sb))
	}
	if n == 0 {
		return ""
	}
	return b.String()
}

// ── code ─────────────────────────────────────────────────────────────────────

// codeRows locates the largest profile movers in the agent's source: the
// file and line from the profile, and — with a checkout that has both
// commits — whether that file changed between the two builds.
func codeRows(in Input, res *Result) ([]CodeRow, string) {
	if len(res.Profiles) == 0 {
		return nil, ""
	}
	mod := module(in)
	a, b := in.Cols[0].Runs[0], in.Cols[1].Runs[0]
	var rows []CodeRow
	seen := map[string]bool{}
	src := in.Source
	canDiff := src != "" && a.Agent.Commit != "" && b.Agent.Commit != "" && hasCommit(src, a.Agent.Commit) && hasCommit(src, b.Agent.Commit)
	for _, d := range res.Profiles {
		floor := 0.02 * math.Max(d.ATotal, d.BTotal) // a mover worth a row: 2 % of the view
		n := 0
		for _, r := range d.Rows {
			if n >= 4 {
				break
			}
			if r.File == "" || seen[r.Function] || !prof.InModule(r.Function, mod) {
				continue
			}
			path, where := prof.RepoPath(r.File, mod)
			row := CodeRow{Function: prof.ShortFunc(r.Function), View: d.Label, Delta: prof.FormatDelta(r.Delta, d.Unit), File: path, Line: r.Line, Where: where}
			if canDiff && where == "repo" {
				row.Change = fileChange(src, a.Agent.Commit, b.Agent.Commit, path)
			}
			if math.Abs(r.Delta) < floor && row.Change != "new in b" {
				continue
			}
			seen[r.Function] = true
			n++
			if b.Agent.Repo != "" && b.Agent.Commit != "" {
				row.URL = fmt.Sprintf("%s/blob/%s/%s#L%d", strings.TrimSuffix(b.Agent.Repo, ".git"), b.Agent.Commit, path, r.Line)
			}
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil, ""
	}
	note := fmt.Sprintf("Both builds come from `%s`", mod)
	switch {
	case a.Agent.Commit != "" && b.Agent.Commit != "":
		note += fmt.Sprintf(": %s at `%s`, %s at `%s`", in.Cols[0].Name, short(a.Agent.Commit), in.Cols[1].Name, short(b.Agent.Commit))
	default:
		note += "; the profiler did not report the commits (older agent), so file changes could not be checked"
	}
	switch {
	case src == "":
		note += ". No checkout configured (`source:` in aoc.yaml or DATADOG_AGENT_SRC), so whether these files changed is not shown."
	case a.Agent.Commit == "" || b.Agent.Commit == "":
		note += "."
	case !canDiff:
		note += fmt.Sprintf(". The checkout at `%s` lacks one of the commits (`git fetch` it), so file changes are not shown.", src)
	default:
		note += fmt.Sprintf(". Changes are `git diff %s..%s` in `%s`; open a function with `git -C %s show %s:<file>`. Movers under 2 %% of a view are left out unless their file is new.", short(a.Agent.Commit), short(b.Agent.Commit), src, src, short(b.Agent.Commit))
	}
	return rows, note
}

func git(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func hasCommit(dir, sha string) bool {
	_, err := git(dir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

// fileChange summarises one file's change between two commits.
func fileChange(dir, a, b, path string) string {
	_, errA := git(dir, "cat-file", "-e", a+":"+path)
	_, errB := git(dir, "cat-file", "-e", b+":"+path)
	switch {
	case errA != nil && errB != nil:
		return "not in repo"
	case errA != nil:
		return "new in b"
	case errB != nil:
		return "removed in b"
	}
	out, err := git(dir, "diff", "--numstat", a, b, "--", path)
	if err != nil {
		return ""
	}
	if out == "" {
		return "unchanged"
	}
	f := strings.Fields(out)
	if len(f) >= 2 {
		return fmt.Sprintf("+%s/−%s", f[0], f[1])
	}
	return "changed"
}

// DetectSource finds a datadog-agent checkout: the given path, then
// DATADOG_AGENT_SRC, then ../datadog-agent next to the working directory.
func DetectSource(configured string) string {
	for _, c := range []string{configured, os.Getenv("DATADOG_AGENT_SRC"), filepath.Join("..", "datadog-agent")} {
		if c == "" {
			continue
		}
		if strings.HasPrefix(c, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				c = filepath.Join(home, c[2:])
			}
		}
		if st, err := os.Stat(filepath.Join(c, ".git")); err == nil && (st.IsDir() || st.Mode().IsRegular()) {
			if abs, err := filepath.Abs(c); err == nil {
				return abs
			}
			return c
		}
	}
	return ""
}

// ── next steps ───────────────────────────────────────────────────────────────

// nextFor suggests the one or two Datadog MCP calls that go a level deeper
// than the local evidence, with the exact filters filled in.
func nextFor(topic string, in Input, res *Result) []string {
	if len(in.Cols[1].Runs) == 0 || len(in.Cols[0].Runs) == 0 {
		return nil
	}
	a, b := in.Cols[0].Runs[0], in.Cols[1].Runs[0]
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
	switch topic {
	case "memory":
		fn := topFn("heap in use")
		if fn != "" {
			fn = " frameRegexFilter=\"" + fn + "\""
		}
		return []string{fmt.Sprintf("`explore_profiling_flame_graph` profileType=heap-live-size queryString=\"%s\" %s%s, then the same for `%s`.", scope(b), win(b), fn, scope(a)),
			fmt.Sprintf("Trend: `get_datadog_metric` `avg:aoc.agent.proc.rss_bytes{experiment:%s} by {variant,proc}` — a ramp is a leak, a step is a cache.", in.Experiment)}
	case "cpu":
		return []string{fmt.Sprintf("`explore_profiling_flame_graph` profileType=cpu-time queryString=\"%s\" %s frameRegexFilter=\"%s\", then the same for `%s`.", scope(b), win(b), topFn("CPU"), scope(a))}
	case "latency":
		return []string{fmt.Sprintf("`get_datadog_metric` `avg:aoc.agent.telemetry.logs_component_utilization.ratio{experiment:%s} by {variant,name}` — the component near 1.0 is the bottleneck.", in.Experiment)}
	case "delivery":
		return []string{fmt.Sprintf("Per-stream gaps: `%s/report.md` (streams table); gaps right after a rotation point at the tailer. Events: `search_datadog_events` query=\"source:aoc run:%s\".", in.Cols[1].Name, b.Name)}
	case "stability":
		return []string{fmt.Sprintf("`grep '| ERROR |' %s/agent.log | sort | uniq -c | sort -rn | head` (never the whole file).", in.Cols[1].Name)}
	}
	return nil
}

func regexpSafe(s string) string {
	r := strings.NewReplacer("(", "\\(", ")", "\\)", "*", "\\*", ".", "\\.", "[", "\\[", "]", "\\]")
	return r.Replace(s)
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

// ── rendering ────────────────────────────────────────────────────────────────

// SectionMarkdown renders one topic section (used by the brief and the
// notebook alike).
func SectionMarkdown(sec Section) string {
	var b strings.Builder
	var lines []string
	for _, g := range sec.Findings {
		lines = append(lines, fmt.Sprintf("%s %s → %s (%s)", g.Metric, g.A, g.B, g.Delta))
	}
	kind := ""
	if sec.Kind == "attention" {
		kind = " (both sides)"
	}
	fmt.Fprintf(&b, "### %s%s — %s\n\n", title(sec.Topic), kind, strings.Join(lines, "; "))
	if sec.Reading != "" {
		fmt.Fprintf(&b, "**%s**\n\n", sec.Reading)
	}
	for _, ev := range sec.Evidence {
		fmt.Fprintf(&b, "*%s*\n\n%s\n", ev.Title, ev.Markdown)
	}
	if len(sec.Next) > 0 {
		b.WriteString("If that is not enough: ")
		b.WriteString(strings.Join(sec.Next, " "))
		b.WriteString("\n\n")
	}
	return b.String()
}

// ChangedTable is the "what differed" table: only rows that moved.
func ChangedTable(res *Result, cols []report.Column) string {
	if len(res.Changed) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "| metric | %s | %s | Δ |\n|---|---|---|---|\n", cols[0].Name, cols[1].Name)
	for _, r := range res.Changed {
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", r.Metric, r.Cells[0], r.Cells[1], r.Deltas[0])
	}
	return b.String()
}

// ProfilesMarkdown renders the profile totals, the movers of every view
// not already shown in a section, and where to click.
func ProfilesMarkdown(res *Result, in Input) string {
	var b strings.Builder
	if len(res.Profiles) == 0 {
		b.WriteString("No profiler uploads were captured on both sides (the window must span at least one full profiling period, 60 s by default, and DD_INTERNAL_PROFILING_ENABLED must be on).\n\n")
		b.WriteString(ProfileLinks(in.Cols, in.AppURL, in.Margin))
		return b.String()
	}
	shown := map[string]bool{}
	for _, sec := range res.Sections {
		for _, ev := range sec.Evidence {
			for _, d := range res.Profiles {
				if strings.Contains(ev.Title, d.Label) {
					shown[d.Label] = true
				}
			}
		}
	}
	fmt.Fprintf(&b, "%s. Rates are per wall-clock second (CPU as %% of one core), levels are averaged over the periods; largest movers first, with the source line of the sampled leaf.\n\n", res.ProfileNote)
	for _, d := range res.Profiles {
		if shown[d.Label] {
			continue
		}
		fmt.Fprintf(&b, "**%s** — %s → %s (%s)\n\n", d.Label, prof.Format(d.ATotal, d.Unit), prof.Format(d.BTotal, d.Unit), prof.FormatDelta(d.BTotal-d.ATotal, d.Unit))
		fmt.Fprintf(&b, "| function (flat) | source | %s | %s | Δ |\n|---|---|---|---|---|\n", in.Cols[0].Name, in.Cols[1].Name)
		for i, r := range d.Rows {
			if i >= 6 {
				break
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", prof.ShortFunc(r.Function), sourceCell(r.File, r.Line, in), prof.Format(r.A, d.Unit), prof.Format(r.B, d.Unit), prof.FormatDelta(r.Delta, d.Unit))
		}
		b.WriteString("\n")
	}
	b.WriteString("Flame graphs (each run's tag and window; pick CPU, heap live size or allocations there, or ⇄ Compare one against the other):\n\n")
	b.WriteString(ProfileLinks(in.Cols, in.AppURL, in.Margin))
	return b.String()
}

// CodeMarkdown renders the code section.
func CodeMarkdown(res *Result) string {
	if len(res.Code) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n| function | view | Δ | source | changed a → b |\n|---|---|---|---|---|\n", res.CodeNote)
	for _, r := range res.Code {
		src := fmt.Sprintf("`%s:%d`", r.File, r.Line)
		if r.URL != "" {
			src = fmt.Sprintf("[%s:%d](%s)", r.File, r.Line, r.URL)
		}
		ch := r.Change
		if ch == "" {
			ch = "–"
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", r.Function, r.View, r.Delta, src, ch)
	}
	return b.String()
}

// Markdown renders the brief (findings.md).
func Markdown(res *Result, in Input) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	p("# aoc A/B %s\n\n", in.Experiment)
	p("## What we tested\n\n%s\n\n", res.Tested)
	if res.Conclusion != "" {
		p("## Conclusion\n\n%s\n\n", res.Conclusion)
	}
	p("## What differed (threshold ±%.0f%%)\n\n", res.Threshold)
	for _, v := range res.Verdict {
		p("- %s\n", v)
	}
	if t := ChangedTable(res, in.Cols); t != "" {
		p("\n%s", t)
	}
	p("\n")
	if len(res.Sections) > 0 {
		p("## Where\n\n")
		for _, sec := range res.Sections {
			p("%s", SectionMarkdown(sec))
		}
	}
	p("## Profiles\n\n%s\n", ProfilesMarkdown(res, in))
	if c := CodeMarkdown(res); c != "" {
		p("## Code\n\n%s\n", c)
	}
	p("Every metric, the full process table and the telemetry counters: `compare.md`. Per run: `<side>/report.md`, `<side>/profiles/` (pprof), `<side>/agent.log`. Record what you conclude with `aoc conclude --results %s \"…\"`.\n", in.ResultsDir)
	return b.String()
}

// EventText is the compact version for the Datadog event (≤ 4000 chars).
func EventText(res *Result, in Input, nbURL string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%%%%%% \n%s\n\n", res.Tested)
	for _, v := range res.Verdict {
		fmt.Fprintf(&b, "- %s\n", v)
	}
	for _, sec := range res.Sections {
		if sec.Reading != "" {
			fmt.Fprintf(&b, "- %s: %s\n", title(sec.Topic), sec.Reading)
		}
	}
	if res.Conclusion != "" {
		fmt.Fprintf(&b, "\n**Conclusion** %s\n", res.Conclusion)
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
// explorer scoped to the run's tag and window.
func ProfileLinks(cols []report.Column, appURL string, margin time.Duration) string {
	var b strings.Builder
	for _, c := range cols {
		for _, r := range c.Runs {
			fmt.Fprintf(&b, "- **%s** `run:%s`: [profiles](%s)\n", c.Name, r.Name, ProfileURL(r, appURL, margin))
		}
	}
	return b.String()
}

// ProfileURL is the profiling explorer scoped to one run.
func ProfileURL(r *report.Report, appURL string, margin time.Duration) string {
	from, to := r.WindowStart.Add(-margin).UnixMilli(), r.WindowEnd.Add(margin).UnixMilli()
	return fmt.Sprintf("%s/profiling/explorer?query=%s&from_ts=%d&to_ts=%d&paused=true", appURL, url.QueryEscape("service:datadog-agent run:"+r.Name), from, to)
}
