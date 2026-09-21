// Package findings turns an A/B comparison into an investigation brief for
// the logs pipeline: did it regress between two agent builds? The delivery
// gate answers whether every record still arrives exactly once; four
// signals — throughput, saturation, cpu, memory — answer what it costs.
// Everything else stays in compare.md. Each number is printed once: the
// signals table carries the verdict, the sections carry the evidence that
// names a cause, and the code section locates the profile movers inside the
// packages under test.
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

// Finding is one signal metric that moved, or a gate counter that fired.
type Finding struct {
	Metric string  `json:"metric"`
	Kind   string  `json:"kind"`  // regression | improvement | attention
	Topic  string  `json:"topic"` // delivery | throughput | saturation | cpu | memory | watch
	A      string  `json:"a"`
	B      string  `json:"b"`
	Delta  string  `json:"delta"`
	Pct    float64 `json:"pct"`
}

// Signal is one row of the signals table: a question the brief answers
// whether or not the answer moved.
type Signal struct {
	Name      string  `json:"name"`
	Metric    string  `json:"metric"`
	A         string  `json:"a"`
	B         string  `json:"b"`
	Delta     string  `json:"delta"`
	Pct       float64 `json:"pct"`
	Threshold float64 `json:"threshold"`
	Kind      string  `json:"kind"`           // regression | improvement | flat
	Note      string  `json:"note,omitempty"` // why the number reads the way it does
}

// Gate is the delivery gate: b must not lose, duplicate, orphan or truncate
// records. Out-of-order delivery is reported but does not fail it.
type Gate struct {
	Pass   bool   `json:"pass"`
	Reason string `json:"reason,omitempty"` // what failed, for the verdict line
	Line   string `json:"line"`             // the rendered gate line
}

// Evidence is one table or note that supports a section.
type Evidence struct {
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
}

// Section is one signal that regressed (or the gate, when it failed): the
// findings behind it, a one-sentence reading, the evidence, and what to do
// next if that is not enough.
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
	Scope    bool   `json:"scope,omitempty"` // inside the packages under test
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
	Workload   string             // profile name and description
	ConfigDiff []string           // what only one side had, e.g. "tagfilter only: DD_X=1"
	Source     string             // a checkout of the agent's repository, for the code section
	Conclusion string             // conclusion.md, once written
	Focus      string             // the question this test answers (--focus), verbatim
	Code       []string           // packages under test, e.g. pkg/logs/sender: profile movers here are shown first
	Watch      []string           // extra compare.md metric names promoted to the headline
	Thresholds map[string]float64 // percent per signal (throughput, saturation, cpu, memory); Threshold is the fallback
}

// Result is the brief.
type Result struct {
	Focus       string               `json:"focus,omitempty"`
	Tested      string               `json:"tested"`
	Gate        Gate                 `json:"gate"`
	Signals     []Signal             `json:"signals"`
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

// signals are the four questions about the logs pipeline and the metrics
// that answer them, in the order the brief asks them. The delivery gate is
// separate: it fails the run rather than scoring it.
var signals = []struct {
	name    string
	metrics []string
}{
	{"throughput", []string{"received logs /s"}},
	{"saturation", []string{"pipeline utilization (busiest component)"}},
	{"cpu", []string{"core agent process CPU avg", "CPU seconds per 1M logs"}},
	{"memory", []string{"core agent RSS max", "agent container anon mem avg", "core agent Go heap in use"}},
}

// gateMetrics fail the gate when b has any of them.
var gateMetrics = []string{"lost / missing", "duplicates", "orphan continuation lines", "truncated"}

// defaultScope is the logs pipeline, used when the test names no packages.
var defaultScope = []string{"pkg/logs", "comp/logs", "comp/logs-library"}

// sectionOrder is the order the brief explains what moved. A watched
// metric has no evidence of its own: it stays a row in the signals table.
var sectionOrder = []string{"delivery", "throughput", "saturation", "cpu", "memory"}

// Build runs the gate, scores the signals and gathers the evidence for
// every signal that regressed.
func Build(in Input) *Result {
	if in.Threshold <= 0 {
		in.Threshold = 10
	}
	if in.Margin == 0 {
		in.Margin = time.Minute
	}
	res := &Result{Threshold: in.Threshold, Focus: strings.TrimSpace(in.Focus), Conclusion: strings.TrimSpace(in.Conclusion)}
	res.Headline = report.Rows(in.Cols, append(report.HeadlineMetrics(), in.Watch...))
	if len(in.Cols) < 2 {
		return res
	}
	for _, row := range res.Headline {
		if changed(row) {
			res.Changed = append(res.Changed, row)
		}
	}
	res.Gate = gate(in, res)
	res.Signals = signalRows(in, res)
	res.Findings = findingsOf(res)

	var an [2][]*prof.Analysis
	for i := 0; i < 2 && i < len(in.Captures); i++ {
		an[i] = prof.Analyze(in.Captures[i])
	}
	res.Profiles = prof.Compare(an[0], an[1], 12)
	res.ProfileNote = profileNote(res.Profiles)

	// The code section first: the readings refer to it.
	res.Code, res.CodeNote = codeRows(in, res)

	for _, topic := range sectionOrder {
		if topic == "delivery" && res.Gate.Pass || topic != "delivery" && !regressed(res, topic) {
			continue
		}
		sec := Section{Topic: topic, Kind: "regression"}
		for _, f := range res.Findings {
			if f.Topic == topic && f.Kind != "improvement" {
				sec.Findings = append(sec.Findings, f)
			}
		}
		sec.Reading, sec.Evidence = evidenceFor(topic, in, res)
		sec.Next = nextFor(topic, in, res)
		res.Sections = append(res.Sections, sec)
	}
	res.Tested = tested(in)
	res.Verdict = verdict(res)
	res.Summary = summary(res)
	return res
}

func regressed(res *Result, topic string) bool {
	for _, s := range res.Signals {
		if s.Name == topic && s.Kind == "regression" {
			return true
		}
	}
	return false
}

// thresholdFor is the signal's own threshold, else the run's.
func thresholdFor(in Input, signal string) float64 {
	if t := in.Thresholds[signal]; t != 0 {
		return t
	}
	return in.Threshold
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

func rowFor(rows []report.HeadlineRow, metric string) (report.HeadlineRow, bool) {
	for _, r := range rows {
		if r.Metric == metric && len(r.Values) > 1 && len(r.Cells) > 1 && len(r.Deltas) > 0 {
			return r, true
		}
	}
	return report.HeadlineRow{}, false
}

// ── the gate ─────────────────────────────────────────────────────────────────

// gate decides whether b delivered the workload. A counter that is non-zero
// on both sides still fails it, but says so: that is the harness or the
// workload rather than the build.
func gate(in Input, res *Result) Gate {
	g := Gate{Pass: true}
	var fails, notes []string
	for _, m := range gateMetrics {
		row, ok := rowFor(res.Headline, m)
		if !ok {
			continue
		}
		a, b := row.Values[0], row.Values[1]
		switch {
		case b > 0:
			g.Pass = false
			s := fmt.Sprintf("%s %s in %s", row.Cells[1], m, in.Cols[1].Name)
			if a > 0 {
				s += fmt.Sprintf(" (%s in %s too — the workload or the harness, not the build)", row.Cells[0], in.Cols[0].Name)
			}
			fails = append(fails, s)
		case a > 0:
			notes = append(notes, fmt.Sprintf("%s %s only in %s", row.Cells[0], m, in.Cols[0].Name))
		}
	}
	var tail []string
	if row, ok := rowFor(res.Headline, "delivery ratio"); ok {
		tail = append(tail, fmt.Sprintf("delivery ratio %s → %s", row.Cells[0], row.Cells[1]))
	}
	if row, ok := rowFor(res.Headline, "out of order"); ok && (row.Values[0] > 0 || row.Values[1] > 0) {
		tail = append(tail, fmt.Sprintf("out of order %s → %s, which does not gate", row.Cells[0], row.Cells[1]))
	}
	tail = append(tail, notes...)
	if g.Pass {
		g.Line = "Delivery gate: pass — nothing lost, duplicated, orphaned or truncated in " + in.Cols[1].Name
	} else {
		g.Reason = strings.Join(fails, ", ")
		g.Line = "Delivery gate: fail — " + g.Reason
	}
	if len(tail) > 0 {
		g.Line += ". " + upperFirst(strings.Join(tail, "; "))
	}
	g.Line += "."
	return g
}

// ── the signals ──────────────────────────────────────────────────────────────

func signalRows(in Input, res *Result) []Signal {
	var out []Signal
	for _, s := range signals {
		t := thresholdFor(in, s.name)
		for _, m := range s.metrics {
			if row, ok := rowFor(res.Headline, m); ok {
				out = append(out, signalOf(s.name, row, t))
			}
		}
	}
	for _, m := range in.Watch {
		if row, ok := rowFor(res.Headline, m); ok {
			out = append(out, signalOf("watch", row, in.Threshold))
		}
	}
	for i := range out {
		if out[i].Name == "throughput" {
			pace(&out[i], in)
		}
	}
	return out
}

func signalOf(name string, row report.HeadlineRow, t float64) Signal {
	s := Signal{Name: name, Metric: row.Metric, A: row.Cells[0], B: row.Cells[1], Delta: row.Deltas[0], Pct: row.Pct[0], Threshold: t,
		Kind: report.Kind(row.Sense, row.Pct[0], t)}
	if math.IsNaN(s.Pct) || math.IsInf(s.Pct, 0) {
		s.Pct = 0
	}
	return s
}

// pace rescores throughput for a workload the generators hold at a target
// rate: received then tracks generated by construction, and only b falling
// short of what it generated — while a keeps up — is a regression.
func pace(s *Signal, in Input) {
	rate := targetRate(in)
	if rate <= 0 {
		return
	}
	a, b := shortfall(in.Cols[0]), shortfall(in.Cols[1])
	if b > s.Threshold && a <= s.Threshold {
		s.Kind = "regression"
		s.Note = fmt.Sprintf("%s delivered %.1f%% fewer logs than its generators wrote, %s %.1f%%", in.Cols[1].Name, b, in.Cols[0].Name, a)
		return
	}
	s.Kind = "flat"
	s.Note = fmt.Sprintf("held by the generators at %s and both sides kept up, so saturation carries the signal", fmtutil.Rate(rate))
}

// targetRate is the rate the generators were asked for, 0 when they ran
// flat out.
func targetRate(in Input) float64 {
	total := 0.0
	for _, r := range in.Cols[0].Runs {
		sum := 0.0
		for _, g := range r.Generators {
			sum += g.TargetRate
		}
		if sum > total {
			total = sum
		}
	}
	return total
}

// shortfall is how far received fell behind generated, in percent.
func shortfall(c report.Column) float64 {
	gen := c.Value(func(r *report.Report) float64 { return r.Throughput.GenRecordsPerSec })
	recv := c.Value(func(r *report.Report) float64 { return r.Throughput.RecvLogsPerSec })
	if gen <= 0 {
		return 0
	}
	return math.Max(0, (gen-recv)/gen*100)
}

// findingsOf are the gate failures and the signals that moved, worst first.
func findingsOf(res *Result) []Finding {
	var out []Finding
	for _, m := range gateMetrics {
		row, ok := rowFor(res.Headline, m)
		if !ok || row.Values[1] == 0 {
			continue
		}
		kind := "regression"
		if row.Values[0] > 0 {
			kind = "attention" // both sides: the workload or the harness
		}
		out = append(out, Finding{Metric: m, Kind: kind, Topic: "delivery", A: row.Cells[0], B: row.Cells[1], Delta: row.Deltas[0]})
	}
	for _, s := range res.Signals {
		if s.Kind == "flat" {
			continue
		}
		out = append(out, Finding{Metric: s.Metric, Kind: s.Kind, Topic: s.Name, A: s.A, B: s.B, Delta: s.Delta, Pct: s.Pct})
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return rank(a.Kind) < rank(b.Kind)
		}
		if ta, tb := topicRank(a.Topic), topicRank(b.Topic); ta != tb {
			return ta < tb
		}
		return math.Abs(a.Pct) > math.Abs(b.Pct)
	})
	return out
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
	for i, x := range sectionOrder {
		if x == t {
			return i
		}
	}
	return len(sectionOrder)
}

// verdict is the one-line answer, then the signals that moved.
func verdict(res *Result) []string {
	var regs, imps, topics []string
	for _, s := range res.Signals {
		switch s.Kind {
		case "regression":
			regs = append(regs, fmt.Sprintf("%s regressed: %s %s → %s (%s, threshold ±%.0f%%)", s.Name, s.Metric, s.A, s.B, s.Delta, s.Threshold))
			if !contains(topics, s.Name) {
				topics = append(topics, s.Name)
			}
		case "improvement":
			imps = append(imps, fmt.Sprintf("%s improved: %s %s → %s (%s)", s.Name, s.Metric, s.A, s.B, s.Delta))
		}
	}
	head := "pass"
	switch {
	case !res.Gate.Pass:
		head = "fail: " + res.Gate.Reason
	case len(topics) > 0:
		head = "regression: " + strings.Join(topics, ", ")
	}
	return append(append([]string{head}, regs...), imps...)
}

func summary(res *Result) string {
	var topics []string
	imps := 0
	for _, s := range res.Signals {
		switch s.Kind {
		case "regression":
			if !contains(topics, s.Name) {
				topics = append(topics, s.Name)
			}
		case "improvement":
			imps++
		}
	}
	switch {
	case !res.Gate.Pass:
		return "delivery gate failed: " + res.Gate.Reason
	case len(topics) > 0:
		return "regression in " + strings.Join(topics, ", ")
	case imps > 0:
		return "no regression; " + plural(imps, "improvement")
	}
	return "no change beyond the threshold"
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// tested identifies the two builds, then the workload both ran.
func tested(in Input) string {
	var b strings.Builder
	b.WriteString("| side | image | version | commit | only this side |\n|---|---|---|---|---|\n")
	var shared []string
	for _, c := range in.Cols {
		r := c.Runs[0]
		img := r.Agent.Image
		if d := report.ShortDigest(r.Agent.Digest); d != "" {
			img += "@" + d
		}
		only, rest := configFor(c.Name, in.ConfigDiff)
		shared = append(shared, rest...)
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", c.Name, code(img), versions(r), code(short(r.Agent.Commit)), only)
	}
	r := in.Cols[0].Runs[0]
	work := in.Workload
	if work == "" {
		work = "the profile"
	}
	streams := 0
	for _, g := range r.Generators {
		streams += g.ActiveStreams
	}
	fmt.Fprintf(&b, "\nWorkload %s — %d streams at %s, %s window, %s per side.\n", work, streams, fmtutil.Rate(r.Throughput.GenRecordsPerSec),
		fmtutil.Duration(time.Duration(r.Seconds*float64(time.Second))), plural(len(in.Cols[0].Runs), "round"))
	if s := dedupe(shared); len(s) > 0 {
		fmt.Fprintf(&b, "Both sides: %s.\n", strings.Join(s, "; "))
	}
	return b.String()
}

// configFor splits the configuration differences into what only this side
// had and what the entry does not attribute to a side.
func configFor(side string, diff []string) (string, []string) {
	var mine, rest []string
	for _, d := range diff {
		if strings.Contains(d, "`"+side+"`") {
			if _, after, ok := strings.Cut(d, "has "); ok {
				d = after
			}
			mine = append(mine, d)
			continue
		}
		if !strings.Contains(d, "`") || strings.HasPrefix(d, "both") {
			rest = append(rest, strings.TrimPrefix(d, "both have "))
		}
	}
	if len(mine) == 0 {
		return "–", rest
	}
	return code(strings.Join(mine, "; ")), rest
}

func dedupe(xs []string) []string {
	var out []string
	for _, x := range xs {
		if !contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}

func code(s string) string {
	if s == "" {
		return "–"
	}
	return "`" + s + "`"
}

func versions(r *report.Report) string {
	ks := make([]string, 0, len(r.Agent.Versions))
	for k := range r.Agent.Versions {
		ks = append(ks, k)
	}
	if len(ks) == 0 {
		return "–"
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// ── evidence ─────────────────────────────────────────────────────────────────

// evidenceFor returns the reading and the tables that explain a signal.
// Each table appears once in the brief: profile diffs printed here are
// skipped by the Profiles section.
func evidenceFor(topic string, in Input, res *Result) (string, []Evidence) {
	a, b := in.Cols[0], in.Cols[1]
	var ev []Evidence
	add := func(title, md string) {
		if strings.TrimSpace(md) != "" {
			ev = append(ev, Evidence{Title: title, Markdown: md})
		}
	}
	reading := ""
	switch topic {
	case "delivery":
		reading = deliveryReading(in, res)
		add("Delivery ledger", rowsTable(in.Cols, []string{"generated records", "unique delivered", "lost / missing", "duplicates", "out of order", "orphan continuation lines", "truncated"}, true))
		add("Retries, errors, responses", retriesTable(in.Cols))
		add("Faults injected", faultsNote(in.Cols))
		add("Agent log: most repeated warnings and errors", logTable(in.Cols))
	case "throughput":
		reading = throughputReading(in)
		add("Offered against delivered", rowsTable(in.Cols, []string{"generated /s", "received logs /s", "requests /s"}, false))
		add("Pipeline utilization by component", teleGaugeTable(in.Cols, "logs_component_utilization__ratio", "name"))
	case "saturation":
		reading = saturationReading(in.Cols)
		add("Pipeline utilization by component (share of a second spent busy)", teleGaugeTable(in.Cols, "logs_component_utilization__ratio", "name"))
	case "cpu":
		reading = cpuReading(in, res)
		add("Core agent CPU, by function (profiler; % of one core)", diffTable(res.Profiles, "CPU", a.Name, b.Name, 6, in))
	case "memory":
		rows := report.Rows(in.Cols, []string{"agent container mem max", "agent container anon mem max", "agent container file cache max", "agent container anon mem avg", "core agent RSS max", "core agent Go heap in use"})
		reading = memoryReading(rows)
		// The memory is either the core agent's, and the profiler names the
		// functions holding it, or it is elsewhere in the container.
		if coreMemoryRose(rows) {
			add("Core agent heap in use, by function (profiler, averaged over the window)", diffTable(res.Profiles, "heap in use", a.Name, b.Name, 6, in))
			add("Core agent allocation rate, by function", diffTable(res.Profiles, "allocation rate", a.Name, b.Name, 6, in))
		}
		if len(ev) == 0 {
			add("The container around the core agent", memoryTable(in.Cols, rows))
			add("Processes that moved (docker top)", processTable(in.Cols))
		}
	}
	return reading, ev
}

func deliveryReading(in Input, res *Result) string {
	reg, both := false, false
	for _, f := range res.Findings {
		if f.Topic != "delivery" {
			continue
		}
		if f.Kind == "attention" {
			both = true
		} else {
			reg = true
		}
	}
	if both && !reg {
		return fmt.Sprintf("Records go missing on both sides in similar numbers, so the workload or the harness is losing them rather than %s.", in.Cols[1].Name)
	}
	s := fmt.Sprintf("Records the generators wrote did not arrive exactly once in %s: the ledger says how many and the counters whether the agent retried, dropped them or was refused.", in.Cols[1].Name)
	if both {
		s += fmt.Sprintf(" Part of it happens in %s too, which points at the workload rather than the build.", in.Cols[0].Name)
	}
	return s
}

func throughputReading(in Input) string {
	if targetRate(in) > 0 {
		return fmt.Sprintf("%s could not hold the rate the generators offered while %s did: the pipeline, not the workload, is the limit.", in.Cols[1].Name, in.Cols[0].Name)
	}
	return fmt.Sprintf("%s delivered fewer logs per second than %s with the generators running flat out.", in.Cols[1].Name, in.Cols[0].Name)
}

// saturationReading names the busiest component and how close to busy it is.
func saturationReading(cols []report.Column) string {
	name, v := busiest(cols[1])
	if name == "" {
		return ""
	}
	aName, _ := busiest(cols[0])
	switch {
	case v >= 0.9:
		return fmt.Sprintf("Component `%s` is busy almost all of the time in %s: the pipeline's bottleneck, and the stage to profile.", name, cols[1].Name)
	case v >= 0.5:
		return fmt.Sprintf("`%s` is the busiest component in %s and now spends most of each second working: headroom is going.", name, cols[1].Name)
	case name != aName:
		return fmt.Sprintf("The busiest component moved from `%s` to `%s`, both far from saturated: the extra work is real but nothing queues behind it yet.", aName, name)
	}
	return fmt.Sprintf("`%s` is the busiest component on both sides and is far from saturated: the pipeline spends the extra time working, not waiting.", name)
}

// busiest is the component with the highest utilization ratio in a column.
func busiest(c report.Column) (string, float64) {
	name, best := "", 0.0
	for _, r := range c.Runs {
		for _, t := range r.Telemetry {
			if t.Name != "logs_component_utilization__ratio" {
				continue
			}
			if v := report.TeleAvg(r, t.Name, t.Labels); v > best {
				name, best = labelValue(t.Labels, "name"), v
			}
		}
	}
	return name, best
}

// cpuReading names the largest CPU mover, preferring the code under test.
func cpuReading(in Input, res *Result) string {
	for _, d := range res.Profiles {
		if d.Label != "CPU" {
			continue
		}
		rows := scopedRows(d, in, 6)
		if len(rows) == 0 || rows[0].Delta <= 0 {
			return "The core agent's CPU did not shift toward any one function: the cost is spread across the pipeline."
		}
		r := rows[0]
		s := fmt.Sprintf("The largest CPU mover is `%s`", prof.ShortFunc(r.Function))
		if row := findCode(res, r.Function); row != nil {
			s += fmt.Sprintf(" (`%s:%d`", row.File, row.Line)
			if row.Change != "" {
				s += ", " + row.Change
			}
			s += ")"
		}
		return s + "."
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

// memoryReading says which part of the memory moved and what that usually
// means; the numbers are in the tables.
func memoryReading(rows []report.HeadlineRow) string {
	cont := pctOf(rows, "agent container mem max")
	anon := pctOf(rows, "agent container anon mem max")
	anonAvg := pctOf(rows, "agent container anon mem avg")
	file := pctOf(rows, "agent container file cache max")
	rss := pctOf(rows, "core agent RSS max")
	heap := pctOf(rows, "core agent Go heap in use")
	up := func(p float64) bool { return !math.IsNaN(p) && p >= 5 }
	flat := func(p float64) bool { return math.IsNaN(p) || math.Abs(p) < 5 }
	// The core agent first: that is the process the signal is about.
	switch {
	case up(rss) && up(heap):
		return "The core agent's Go heap grew with its RSS: the heap-in-use table names the functions holding it."
	case up(rss) && flat(heap):
		return "The core agent's RSS grew but its Go heap did not: non-Go memory (cgo, embedded Python, mmap) or a larger runtime reserve."
	case up(heap):
		return "The Go heap the core agent holds grew without its RSS following: the allocator has the pages already."
	case up(anonAvg) && flat(rss):
		return "Process memory grew while the core agent's RSS did not: the memory belongs to another process in the container."
	case up(cont) && !up(anon) && up(file):
		return "The container grew through page cache rather than process memory: files the image reads, reclaimed under pressure, not a leak."
	case up(anon) && flat(anonAvg) && flat(rss):
		return "Process memory peaked higher but averaged the same over the window while the core agent's RSS stayed flat: a spike between samples, not a steady increase."
	case up(anon):
		return "Process memory grew; the process table says which process holds it."
	}
	return "Nothing in the container's own accounting grew with it: compare process memory against page cache, then the core agent's RSS against its Go heap."
}

func pctOf(rows []report.HeadlineRow, metric string) float64 {
	for _, r := range rows {
		if r.Metric == metric && len(r.Pct) > 0 {
			return r.Pct[0]
		}
	}
	return math.NaN()
}

func coreMemoryRose(rows []report.HeadlineRow) bool {
	return pctOf(rows, "core agent RSS max") >= 5 || pctOf(rows, "core agent Go heap in use") >= 5
}

// rowsTable is a slice of the comparison table; onlyChanged drops rows that
// did not move.
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

// memoryTable is the container's own accounting around the core agent: the
// numbers the signals table does not carry.
func memoryTable(cols []report.Column, rows []report.HeadlineRow) string {
	labels := map[string]string{
		"agent container mem max":        "container memory max (usage − inactive file cache)",
		"agent container anon mem max":   "├ anon: the processes' own memory (max)",
		"agent container file cache max": "└ file: page cache charged to the container (max)",
	}
	var b strings.Builder
	fmt.Fprintf(&b, "| | %s | %s | Δ |\n|---|---|---|---|\n", cols[0].Name, cols[1].Name)
	n := 0
	for _, r := range rows {
		label, ok := labels[r.Metric]
		if !ok {
			continue
		}
		n++
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", label, r.Cells[0], r.Cells[1], r.Deltas[0])
	}
	if n == 0 {
		return ""
	}
	return b.String()
}

// processTable lists the container's processes that moved by 5 % or
// appeared, plus the largest one.
func processTable(cols []report.Column) string {
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
			// 5 % of a process that holds half a megabyte is not news.
			moved = math.Abs(rb-ra) >= 2e6 && (ra == 0 || math.Abs((rb-ra)/ra*100) >= 5)
		}
		if !moved && i != 0 {
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
	if n == 1 {
		b.WriteString("\nNo other process moved by 5 % or more.\n")
	}
	return b.String()
}

// diffTable renders one profile view's movers with the source line of each.
func diffTable(diffs []prof.Diff, label, aName, bName string, n int, in Input) string {
	for _, d := range diffs {
		if d.Label != label {
			continue
		}
		rows := scopedRows(d, in, n)
		if len(rows) == 0 {
			return ""
		}
		var b strings.Builder
		fmt.Fprintf(&b, "`%s` total %s → %s (%s), %s merged\n\n", d.Service, prof.Format(d.ATotal, d.Unit), prof.Format(d.BTotal, d.Unit), prof.FormatDelta(d.BTotal-d.ATotal, d.Unit), periods(d))
		fmt.Fprintf(&b, "| function (flat) | source | %s | %s | Δ |\n|---|---|---|---|---|\n", aName, bName)
		for _, r := range rows {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", prof.ShortFunc(r.Function), sourceCell(r.File, r.Line, in), prof.Format(r.A, d.Unit), prof.Format(r.B, d.Unit), prof.FormatDelta(r.Delta, d.Unit))
		}
		return b.String()
	}
	return ""
}

func periods(d prof.Diff) string {
	if d.ACaps == d.BCaps {
		return plural(d.ACaps, "profiling period")
	}
	return fmt.Sprintf("%d vs %d profiling periods", d.ACaps, d.BCaps)
}

// ── profile scoping ──────────────────────────────────────────────────────────

// codeScope is the module-relative prefixes of the packages under test.
func codeScope(in Input) []string {
	if len(in.Code) > 0 {
		return in.Code
	}
	return defaultScope
}

// inScope says whether a profile row's source file is one of the packages
// under test.
func inScope(file string, in Input) bool {
	path, where := prof.RepoPath(file, module(in))
	if where != "repo" {
		return false
	}
	for _, p := range codeScope(in) {
		if strings.HasPrefix(path, strings.TrimSuffix(p, "/")) {
			return true
		}
	}
	return false
}

// scopedRows are the movers a table shows: the packages under test first (at
// most n), then at most three functions from elsewhere in the agent, and
// only when they moved more than anything in scope. A mover is noise unless
// it clears the view's absolute floor; out of scope it must also be worth
// 2 % of the view, so the tables do not fill up with the runtime.
func scopedRows(d prof.Diff, in Input, n int) []prof.DiffRow {
	abs := absFloor(d.Unit)
	rel := 0.02 * math.Max(math.Abs(d.ATotal), math.Abs(d.BTotal))
	var scoped, other []prof.DiffRow
	for _, r := range d.Rows {
		v := math.Abs(r.Delta)
		if inScope(r.File, in) {
			if v >= abs {
				scoped = append(scoped, r)
			}
			continue
		}
		if v >= abs && v >= rel {
			other = append(other, r)
		}
	}
	if len(scoped) > n {
		scoped = scoped[:n]
	}
	biggest := 0.0
	if len(scoped) > 0 {
		biggest = math.Abs(scoped[0].Delta)
	}
	rows := scoped
	for _, r := range other {
		if len(rows)-len(scoped) >= 3 || math.Abs(r.Delta) <= biggest {
			continue
		}
		rows = append(rows, r)
	}
	return rows
}

// absFloor is the smallest move worth a row in a view: a fifth of a percent
// of a core, 1 MB live, 50 kB/s allocated.
func absFloor(unit string) float64 {
	switch unit {
	case "%":
		return 0.2
	case "B":
		return 1e6
	case "B/s":
		return 50e3
	}
	return 1
}

// sourceCell is "path:line" relative to the agent's module, marked when it
// is the Go runtime or a dependency.
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

// module is the Go module path of the agent, from the profiler's repository
// tag (https://github.com/DataDog/datadog-agent →
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

func profileNote(diffs []prof.Diff) string {
	if len(diffs) == 0 {
		return ""
	}
	var parts []string
	for _, d := range diffs {
		if d.Label == "CPU" || d.Label == "heap in use" || d.Label == "allocation rate" {
			parts = append(parts, fmt.Sprintf("%s %s → %s (%s)", d.Label, prof.Format(d.ATotal, d.Unit), prof.Format(d.BTotal, d.Unit), report.DeltaLower(d.ATotal, d.BTotal)))
		}
	}
	return fmt.Sprintf("`%s`: %s · %s", diffs[0].Service, strings.Join(parts, " · "), periods(diffs[0]))
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
	fmt.Fprintf(&b, "| %s | %s | %s | Δ |\n|---|---|---|---|\n", labelKey, cols[0].Name, cols[1].Name)
	for _, l := range order {
		get := func(r *report.Report) float64 {
			for _, t := range r.Telemetry {
				if t.Name == name && t.Labels == l {
					return report.TeleAvg(r, t.Name, t.Labels)
				}
			}
			return math.NaN()
		}
		va, vb := cols[0].Value(get), cols[1].Value(get)
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", labelValue(l, labelKey), fmtutil.Float(va, 3), fmtutil.Float(vb, 3), report.DeltaLower(va, vb))
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

// retriesTable is the agent's own delivery counters, empty when neither
// side retried, dropped or was refused.
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
		if va == 0 && vb == 0 {
			continue
		}
		any = true
		fmt.Fprintf(&b, "| %s | %s | %s |\n", r.label, fmtutil.Float(va, 0), fmtutil.Float(vb, 0))
	}
	if !any {
		return ""
	}
	return b.String()
}

func faultsNote(cols []report.Column) string {
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

// logTable is each side's most repeated warnings and errors, skipping the
// messages both sides repeat equally (the harness's own noise). It is
// rendered only when a side logged an error.
func logTable(cols []report.Column) string {
	counts := make([]map[string]int64, len(cols))
	errs, warns := make([]int64, len(cols)), make([]int64, len(cols))
	for i, c := range cols {
		counts[i] = map[string]int64{}
		for _, r := range c.Runs {
			if r.AgentLog == nil {
				continue
			}
			errs[i] += r.AgentLog.Errors
			warns[i] += r.AgentLog.Warnings
			for _, nc := range r.AgentLog.Top {
				counts[i][nc.Name] += nc.Count
			}
		}
	}
	if errs[0] == 0 && errs[1] == 0 {
		return ""
	}
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
			if va, vb := counts[0][k], counts[1][k]; va != vb {
				rows = append(rows, kv{k, va, vb})
			}
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
		return ""
	}
	if len(rows) > 6 {
		rows = rows[:6]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s errors, %s warnings · %s: %s errors, %s warnings\n\n", cols[0].Name, fmtutil.Int(errs[0]), fmtutil.Int(warns[0]), cols[1].Name, fmtutil.Int(errs[1]), fmtutil.Int(warns[1]))
	fmt.Fprintf(&b, "| %s | %s | level · source · message |\n|---|---|---|\n", cols[0].Name, cols[1].Name)
	for _, r := range rows {
		fmt.Fprintf(&b, "| %d | %d | `%s` |\n", r.a, r.b, strings.ReplaceAll(cut(r.k, 80), "|", "\\|"))
	}
	return b.String()
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ── code ─────────────────────────────────────────────────────────────────────

// codeRows locates the profile movers in the agent's source: the file and
// line from the profile, and — with a checkout that has both commits —
// whether that file changed between the two builds. The packages under test
// come first and are never dropped for being small.
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
		for _, r := range scopedRows(d, in, 4) {
			path, where := prof.RepoPath(r.File, mod)
			if r.File == "" || seen[r.Function] || where != "repo" || !prof.InModule(r.Function, mod) {
				continue
			}
			seen[r.Function] = true
			row := CodeRow{Function: prof.ShortFunc(r.Function), View: d.Label, Delta: prof.FormatDelta(r.Delta, d.Unit), File: path, Line: r.Line, Where: where, Scope: inScope(r.File, in)}
			if canDiff {
				row.Change = fileChange(src, a.Agent.Commit, b.Agent.Commit, path)
			}
			if b.Agent.Repo != "" && b.Agent.Commit != "" {
				row.URL = fmt.Sprintf("%s/blob/%s/%s#L%d", strings.TrimSuffix(b.Agent.Repo, ".git"), b.Agent.Commit, path, r.Line)
			}
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		return nil, ""
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Scope && !rows[j].Scope })
	note := fmt.Sprintf("Both builds come from `%s`", mod)
	switch {
	case a.Agent.Commit != "" && b.Agent.Commit != "":
		note += fmt.Sprintf(": %s at `%s`, %s at `%s`.", in.Cols[0].Name, short(a.Agent.Commit), in.Cols[1].Name, short(b.Agent.Commit))
	default:
		note += "; the profiler did not report the commits (older agent), so file changes could not be checked."
	}
	switch {
	case src == "":
		note += " No checkout configured (`source:` in aoc.yaml or DATADOG_AGENT_SRC), so whether these files changed is not shown."
	case a.Agent.Commit == "" || b.Agent.Commit == "":
	case !canDiff:
		note += " The checkout lacks one of the commits (`git fetch` it), so file changes are not shown."
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
	// Focus the flame graph on the code under test, not on the largest
	// mover in the binary.
	topFn := func(label string) string {
		for _, d := range res.Profiles {
			if d.Label != label {
				continue
			}
			for _, r := range scopedRows(d, in, 6) {
				if r.Delta > 0 {
					return regexpSafe(prof.ShortFunc(r.Function))
				}
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
		fn := topFn("CPU")
		if fn != "" {
			fn = " frameRegexFilter=\"" + fn + "\""
		}
		return []string{fmt.Sprintf("`explore_profiling_flame_graph` profileType=cpu-time queryString=\"%s\" %s%s, then the same for `%s`.", scope(b), win(b), fn, scope(a))}
	case "saturation":
		return []string{fmt.Sprintf("`get_datadog_metric` `avg:aoc.agent.telemetry.logs_component_utilization.ratio{experiment:%s} by {variant,name}` — the component near 1.0 is the bottleneck.", in.Experiment)}
	case "throughput":
		return []string{fmt.Sprintf("`get_datadog_metric` `avg:aoc.intake.logs_per_sec{experiment:%s} by {variant}` against `avg:aoc.gen.records_per_sec{experiment:%s} by {variant}` — where the lines part is where the pipeline fell behind.", in.Experiment, in.Experiment)}
	case "delivery":
		return []string{fmt.Sprintf("Per-stream gaps: `%s/report.md` (streams table); gaps right after a rotation point at the tailer. Events: `search_datadog_events` query=\"source:aoc run:%s\".", in.Cols[1].Name, b.Name)}
	}
	return nil
}

func regexpSafe(s string) string {
	r := strings.NewReplacer("(", "\\(", ")", "\\)", "*", "\\*", ".", "\\.", "[", "\\[", "]", "\\]")
	return r.Replace(s)
}

// ── rendering ────────────────────────────────────────────────────────────────

// SectionMarkdown renders one signal's section (used by the brief and the
// notebook alike; the next steps are the brief's and stay out of it).
func SectionMarkdown(sec Section) string {
	var b strings.Builder
	fmt.Fprintf(&b, "### %s\n\n", sec.Topic)
	if sec.Reading != "" {
		fmt.Fprintf(&b, "%s\n\n", sec.Reading)
	}
	for _, ev := range sec.Evidence {
		fmt.Fprintf(&b, "%s\n\n%s\n", ev.Title, ev.Markdown)
	}
	return b.String()
}

// ChangedTable is every headline row that moved, signal or not.
func ChangedTable(res *Result, cols []report.Column) string {
	if len(res.Changed) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "| metric | %s | %s | Δ | |\n|---|---|---|---|---|\n", cols[0].Name, cols[1].Name)
	for _, r := range res.Changed {
		kind := report.Kind(r.Sense, r.Pct[0], res.Threshold)
		if kind == "flat" {
			kind = ""
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", r.Metric, r.Cells[0], r.Cells[1], r.Deltas[0], kind)
	}
	return b.String()
}

// SignalsTable is the brief's first table: every signal, moved or not.
func SignalsTable(res *Result, cols []report.Column) string {
	if len(res.Signals) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "| signal | metric | %s | %s | Δ | threshold |\n|---|---|---|---|---|---|\n", cols[0].Name, cols[1].Name)
	last := ""
	for _, s := range res.Signals {
		name := s.Name
		if name == last {
			name = ""
		}
		last = s.Name
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | ±%.0f%% |\n", name, s.Metric, s.A, s.B, s.Delta, s.Threshold)
	}
	return b.String()
}

// ProfilesMarkdown renders the profile totals and the movers of every view
// not already shown in a section.
func ProfilesMarkdown(res *Result, in Input) string {
	if len(res.Profiles) == 0 {
		return "No profiler uploads were captured on both sides (the window must span at least one full profiling period, 60 s by default, and DD_INTERNAL_PROFILING_ENABLED must be on).\n"
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
	var b strings.Builder
	fmt.Fprintf(&b, "%s. Rates are per wall-clock second (CPU as %% of one core), levels are averaged over the periods; %s first, then anything bigger elsewhere in the agent.\n\n", res.ProfileNote, strings.Join(codeScope(in), ", "))
	for _, d := range res.Profiles {
		rows := scopedRows(d, in, 6)
		if shown[d.Label] || len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "**%s** — %s → %s (%s)\n\n", d.Label, prof.Format(d.ATotal, d.Unit), prof.Format(d.BTotal, d.Unit), prof.FormatDelta(d.BTotal-d.ATotal, d.Unit))
		fmt.Fprintf(&b, "| function (flat) | source | %s | %s | Δ |\n|---|---|---|---|---|\n", in.Cols[0].Name, in.Cols[1].Name)
		for _, r := range rows {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | %s |\n", prof.ShortFunc(r.Function), sourceCell(r.File, r.Line, in), prof.Format(r.A, d.Unit), prof.Format(r.B, d.Unit), prof.FormatDelta(r.Delta, d.Unit))
		}
		b.WriteString("\n")
	}
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

// Markdown renders the brief (findings.md): the verdict, the signals and
// the gate first, then evidence only for what moved.
func Markdown(res *Result, in Input) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	if res.Focus != "" {
		p("# %s\n\n", res.Focus)
	} else {
		p("# aoc A/B %s\n\n", in.Experiment)
	}
	for i, v := range res.Verdict {
		if i == 0 {
			p("**%s**\n\n", v)
			continue
		}
		p("- %s\n", v)
	}
	if len(res.Verdict) > 1 {
		p("\n")
	}
	if t := SignalsTable(res, in.Cols); t != "" {
		p("%s\n", t)
	}
	for _, s := range res.Signals {
		if s.Note != "" {
			p("%s: %s.\n\n", upperFirst(s.Name), s.Note)
		}
	}
	p("%s\n\n", res.Gate.Line)
	p("## Tested\n\n%s\n", res.Tested)
	if res.Conclusion != "" {
		p("## Conclusion\n\n%s\n\n", res.Conclusion)
	}
	if len(res.Sections) > 0 {
		p("## Where\n\n")
		for _, sec := range res.Sections {
			p("%s", SectionMarkdown(sec))
			if len(sec.Next) > 0 {
				p("Next: %s\n\n", strings.Join(sec.Next, " "))
			}
		}
	}
	p("## Profiles\n\n%s", ProfilesMarkdown(res, in))
	if l := compareLine(res, in); l != "" {
		p("%s", l)
	}
	if c := CodeMarkdown(res); c != "" {
		p("\n## Code\n\n%s", c)
		if in.Source != "" {
			p("\nLocal checkout `%s`; read a function with `git -C %s show <commit>:<file>`.\n", in.Source, in.Source)
		}
	}
	p("\nEvery metric, the process table and the telemetry counters: `compare.md`.\n")
	return b.String()
}

// compareLine is the profiler's comparison view in one line: the three
// whole-agent views, then the mover worth opening focused.
func compareLine(res *Result, in Input) string {
	if in.AppURL == "" || len(in.Cols) < 2 || len(in.Cols[0].Runs) == 0 || len(in.Cols[1].Runs) == 0 || len(res.Profiles) == 0 {
		return ""
	}
	a, b := in.Cols[0].Runs[0], in.Cols[1].Runs[0]
	s := fmt.Sprintf("Side by side: [CPU](%s) · [heap live size](%s) · [allocations](%s)",
		CompareURL(a, b, in.AppURL, in.Margin, "cpu-time", ""), CompareURL(a, b, in.AppURL, in.Margin, "heap-live-size", ""), CompareURL(a, b, in.AppURL, in.Margin, "alloc-size", ""))
	if m := Movers(res); len(m) > 0 {
		s += fmt.Sprintf(" · [`%s` focused](%s)", m[0].Function, CompareURL(a, b, in.AppURL, in.Margin, m[0].ProfileType, m[0].Focus))
	}
	return s + "\n"
}

// EventText is the compact version for the Datadog event (≤ 4000 chars).
func EventText(res *Result, in Input, nbURL string) string {
	var b strings.Builder
	b.WriteString("%%% \n")
	for i, v := range res.Verdict {
		if i == 0 {
			fmt.Fprintf(&b, "**%s**\n\n", v)
			continue
		}
		fmt.Fprintf(&b, "- %s\n", v)
	}
	fmt.Fprintf(&b, "\n%s\n", res.Gate.Line)
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

// ProfileLinks renders, for every run, the profiler's flame graph for CPU,
// live heap and allocations, scoped to the run's tag and window.
func ProfileLinks(cols []report.Column, appURL string, margin time.Duration) string {
	var b strings.Builder
	for _, c := range cols {
		for _, r := range c.Runs {
			fmt.Fprintf(&b, "- **%s** `run:%s`: [CPU](%s) · [heap live size](%s) · [allocations](%s)\n", c.Name, r.Name,
				ProfileURL(r, appURL, margin, "cpu-time"), ProfileURL(r, appURL, margin, "heap-live-size"), ProfileURL(r, appURL, margin, "alloc-size"))
		}
	}
	return b.String()
}

// profileTypes maps a profile view's label to the profiler's profile type.
var profileTypes = map[string]string{"CPU": "cpu-time", "heap in use": "heap-live-size", "allocation rate": "alloc-size"}

// ProfileType is the profiler's profile type for a view label ("" when the
// profiler has no matching view).
func ProfileType(label string) string { return profileTypes[label] }

// CompareURL is the profiler's comparison view — b's flame graph on the
// right, a's on the left, each scoped to its run's tag and window — for
// one profile type, optionally focused on one function (the profiler's
// `function` field, i.e. `(*T).Method` without the package).
func CompareURL(a, b *report.Report, appURL string, margin time.Duration, profileType, focus string) string {
	af, at := a.WindowStart.Add(-margin).UnixMilli(), a.WindowEnd.Add(margin).UnixMilli()
	bf, bt := b.WindowStart.Add(-margin).UnixMilli(), b.WindowEnd.Add(margin).UnixMilli()
	u := fmt.Sprintf("%s/profiling/comparison?query=%s&start=%d&end=%d&compare_query_A=%s&compare_start_A=%d&compare_end_A=%d&compareValuesMode=absolute&profile_type=%s&viz=flame_graph&paused=true",
		appURL, url.QueryEscape("service:datadog-agent run:"+b.Name), bf, bt, url.QueryEscape("service:datadog-agent run:"+a.Name), af, at, profileType)
	if focus != "" {
		u += "&profiling-flame-graph__filter=" + url.QueryEscape(fmt.Sprintf("focus_on(function:%q)", focus))
	}
	return u
}

// FlameFunc is the name the profiler's flame-graph filter matches on: the
// symbol without its package path, e.g. `(*Scoped).Keep` for
// `github.com/DataDog/datadog-agent/comp/logs-library/tagfilter.(*Scoped).Keep`.
func FlameFunc(name string) string {
	s := prof.ShortFunc(name)
	if i := strings.Index(s, ".("); i >= 0 {
		return s[i+1:]
	}
	if i := strings.Index(s, "."); i >= 0 {
		return s[i+1:]
	}
	return s
}

// CompareLinks renders the comparison view for CPU, live heap and
// allocations, then one focused on each of the located movers.
func CompareLinks(res *Result, cols []report.Column, appURL string, margin time.Duration) string {
	if len(cols) < 2 || len(cols[0].Runs) == 0 || len(cols[1].Runs) == 0 {
		return ""
	}
	a, b := cols[0].Runs[0], cols[1].Runs[0]
	var out strings.Builder
	fmt.Fprintf(&out, "- whole agent: [CPU](%s) · [heap live size](%s) · [allocations](%s)\n",
		CompareURL(a, b, appURL, margin, "cpu-time", ""), CompareURL(a, b, appURL, margin, "heap-live-size", ""), CompareURL(a, b, appURL, margin, "alloc-size", ""))
	for _, m := range Movers(res) {
		fmt.Fprintf(&out, "- `%s` (%s, %s): [compare](%s)\n", m.Function, m.View, m.Delta, CompareURL(a, b, appURL, margin, m.ProfileType, m.Focus))
	}
	return out.String()
}

// Mover is a profile mover the comparison view can be focused on.
type Mover struct {
	Function    string // as reported (short)
	View        string
	Delta       string
	ProfileType string
	Focus       string // FlameFunc(Function)
}

// Movers are the functions worth a focused comparison: the located code
// rows first, else the largest increase of every view the profiler has when
// it is at least 2 % of that view, at most four, one per function.
func Movers(res *Result) []Mover {
	var out []Mover
	seen := map[string]bool{}
	add := func(fn, view, delta string) {
		pt := ProfileType(view)
		f := FlameFunc(fn)
		if pt == "" || f == "" || seen[f] || len(out) >= 4 {
			return
		}
		seen[f] = true
		out = append(out, Mover{Function: prof.ShortFunc(fn), View: view, Delta: delta, ProfileType: pt, Focus: f})
	}
	for _, r := range res.Code {
		add(r.Function, r.View, r.Delta)
	}
	if len(out) == 0 {
		for _, d := range res.Profiles {
			floor := 0.02 * math.Max(d.ATotal, d.BTotal)
			for _, r := range d.Rows {
				if r.Delta > 0 {
					if r.Delta >= floor {
						add(r.Function, d.Label, prof.FormatDelta(r.Delta, d.Unit))
					}
					break
				}
			}
		}
	}
	return out
}

// ProfileURL is the profiling explorer's flame graph of one profile type
// (cpu-time, heap-live-size, alloc-size, …) scoped to one run, in the form
// the profiler's own links use.
func ProfileURL(r *report.Report, appURL string, margin time.Duration, profileType string) string {
	from, to := r.WindowStart.Add(-margin).UnixMilli(), r.WindowEnd.Add(margin).UnixMilli()
	return fmt.Sprintf("%s/profiling/explorer?query=%s&profile_type=%s&start=%d&end=%d&viz=flame_graph&paused=true", appURL, url.QueryEscape("service:datadog-agent run:"+r.Name), profileType, from, to)
}
