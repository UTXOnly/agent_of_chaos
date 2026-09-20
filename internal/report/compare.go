package report

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/UTXOnly/agent_of_chaos/internal/fmtutil"
)

// sense says which direction is an improvement; neutral metrics (load
// controls, batch geometry) get no verdict.
type sense uint8

const (
	higher sense = iota
	lower
	neutral
)

type metric struct {
	name   string
	get    func(*Report) float64
	render func(float64) string
	sense  sense
}

var compareMetrics = []metric{
	{"generated records", func(r *Report) float64 { return float64(r.Delivery.GeneratedRecords) }, func(v float64) string { return fmtutil.Int(int64(v)) }, neutral},
	{"unique delivered", func(r *Report) float64 { return float64(r.Delivery.Unique) }, func(v float64) string { return fmtutil.Int(int64(v)) }, neutral},
	{"delivery ratio", func(r *Report) float64 { return r.Delivery.Ratio }, func(v float64) string { return fmtutil.Pct(v) }, higher},
	{"lost / missing", func(r *Report) float64 { return float64(r.Delivery.Missing) }, func(v float64) string { return fmtutil.Int(int64(v)) }, lower},
	{"duplicates", func(r *Report) float64 { return float64(r.Delivery.Duplicates) }, func(v float64) string { return fmtutil.Int(int64(v)) }, lower},
	{"out of order", func(r *Report) float64 { return float64(r.Delivery.OutOfOrder) }, func(v float64) string { return fmtutil.Int(int64(v)) }, lower},
	{"orphan continuation lines", func(r *Report) float64 { return float64(r.Delivery.Orphans) }, func(v float64) string { return fmtutil.Int(int64(v)) }, lower},
	{"truncated", func(r *Report) float64 { return float64(r.Delivery.Truncated) }, func(v float64) string { return fmtutil.Int(int64(v)) }, lower},
	{"generated /s", func(r *Report) float64 { return r.Throughput.GenRecordsPerSec }, fmtutil.Rate, neutral},
	{"received logs /s", func(r *Report) float64 { return r.Throughput.RecvLogsPerSec }, fmtutil.Rate, neutral},
	{"received wire B/s", func(r *Report) float64 { return r.Throughput.RecvWireBytesPerSec }, func(v float64) string { return fmtutil.BytesF(v) + "/s" }, lower},
	{"compression ratio", func(r *Report) float64 { return r.Throughput.CompressionRatio }, func(v float64) string { return fmt.Sprintf("%.2f×", v) }, higher},
	{"e2e latency p50", func(r *Report) float64 { return r.Latency.EndToEnd.P50 }, secs, lower},
	{"e2e latency p99", func(r *Report) float64 { return r.Latency.EndToEnd.P99 }, secs, lower},
	{"e2e latency max", func(r *Report) float64 { return r.Latency.EndToEnd.Max }, secs, lower},
	{"sender latency p99", func(r *Report) float64 { return r.Latency.Sender.P99 }, secs, lower},
	{"agent container CPU avg", func(r *Report) float64 { return r.Resources.ContainerCPUAvg }, func(v float64) string { return fmt.Sprintf("%.1f%%", v) }, lower},
	{"agent container CPU max", func(r *Report) float64 { return r.Resources.ContainerCPUMax }, func(v float64) string { return fmt.Sprintf("%.1f%%", v) }, lower},
	{"agent container mem avg", func(r *Report) float64 { return float64(r.Resources.ContainerMemAvg) }, func(v float64) string { return fmtutil.BytesF(v) }, lower},
	{"agent container mem max", func(r *Report) float64 { return float64(r.Resources.ContainerMemMax) }, func(v float64) string { return fmtutil.BytesF(v) }, lower},
	{"core agent process CPU avg", func(r *Report) float64 { return r.Resources.ProcessCPUAvg }, func(v float64) string { return fmt.Sprintf("%.1f%%", v) }, lower},
	{"core agent RSS max", func(r *Report) float64 { return float64(r.Resources.ProcessRSSMax) }, func(v float64) string { return fmtutil.BytesF(v) }, lower},
	{"CPU seconds per 1M logs", func(r *Report) float64 { return r.Resources.CPUSecondsPerMLogs }, func(v float64) string { return fmt.Sprintf("%.2f", v) }, lower},
	{"tags per log", func(r *Report) float64 { return r.Tags.AvgTagsPerLog }, func(v float64) string { return fmt.Sprintf("%.2f", v) }, lower},
	{"tag bytes per log", func(r *Report) float64 { return r.Tags.AvgTagBytesPerLog }, func(v float64) string { return fmt.Sprintf("%.1f B", v) }, lower},
	{"logs per payload p50", func(r *Report) float64 { return r.HTTP.LogsPerPayload.P50 }, func(v float64) string { return fmt.Sprintf("%.0f", v) }, neutral},
	{"payload wire p50", func(r *Report) float64 { return r.HTTP.PayloadWire.P50 }, func(v float64) string { return fmtutil.BytesF(v) }, neutral},
	{"requests /s", func(r *Report) float64 { return r.Throughput.RequestsPerSec }, func(v float64) string { return fmt.Sprintf("%.1f", v) }, neutral},
}

// headlineMetrics are the rows of the short verdict (aoc ab).
var headlineMetrics = []string{
	"delivery ratio", "lost / missing", "duplicates", "orphan continuation lines",
	"e2e latency p50", "e2e latency p99", "received wire B/s", "compression ratio",
	"agent container CPU avg", "agent container mem max", "core agent process CPU avg",
	"CPU seconds per 1M logs", "tags per log", "tag bytes per log",
}

// Column is one side of a comparison: a label and the runs behind it. A
// column with several runs (repeated A/B rounds) is represented by the
// median of each metric.
type Column struct {
	Name string
	Runs []*Report
}

// Columns wraps each report in a column of its own.
func Columns(reports []*Report) []Column {
	cols := make([]Column, len(reports))
	for i, r := range reports {
		cols[i] = Column{Name: r.Name, Runs: []*Report{r}}
	}
	return cols
}

// value is the column's (median) value of a metric.
func (c Column) value(get func(*Report) float64) float64 {
	vals := make([]float64, 0, len(c.Runs))
	for _, r := range c.Runs {
		vals = append(vals, get(r))
	}
	return median(vals)
}

func median(vals []float64) float64 {
	switch len(vals) {
	case 0:
		return 0
	case 1:
		return vals[0]
	}
	sort.Float64s(vals)
	n := len(vals)
	if n%2 == 1 {
		return vals[n/2]
	}
	return (vals[n/2-1] + vals[n/2]) / 2
}

// AgentLabel is "<version> (<image>@<short digest>)" for a run.
func AgentLabel(r *Report) string {
	s := keys(r.Agent.Versions)
	if r.Agent.Image != "" {
		s += " (" + r.Agent.Image
		if d := ShortDigest(r.Agent.Digest); d != "" {
			s += "@" + d
		}
		s += ")"
	}
	return s
}

// ShortDigest is the first 12 hex characters of repo@sha256:<hex>.
func ShortDigest(digest string) string {
	if i := strings.Index(digest, "sha256:"); i >= 0 {
		digest = digest[i+len("sha256:"):]
	}
	if len(digest) > 12 {
		digest = digest[:12]
	}
	return digest
}

// Compare renders a side-by-side Markdown table of two or more reports. The
// first report is the baseline; deltas are relative to it.
func Compare(reports []*Report) string {
	return CompareColumns(Columns(reports))
}

// CompareColumns is Compare over columns that may each hold several runs.
func CompareColumns(cols []Column) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	names := colNames(cols)
	sep := strings.Repeat("---|", len(cols))
	p("# aoc compare\n\n")
	p("| | %s |\n|---|%s\n", strings.Join(names, " | "), sep)
	agents := make([]string, len(cols))
	win := make([]string, len(cols))
	runs := make([]string, len(cols))
	multi := false
	for i, c := range cols {
		agents[i] = AgentLabel(c.Runs[0])
		win[i] = secs(c.value(func(r *Report) float64 { return r.Seconds }))
		runs[i] = fmt.Sprintf("%d", len(c.Runs))
		if len(c.Runs) > 1 {
			multi = true
		}
	}
	p("| agent | %s |\n", strings.Join(agents, " | "))
	p("| window | %s |\n", strings.Join(win, " | "))
	if multi {
		p("| runs (values are medians) | %s |\n", strings.Join(runs, " | "))
	}

	p("\n## Metrics (baseline = %s)\n\n| metric | %s | Δ vs baseline |\n|---|%s---|\n", cols[0].Name, strings.Join(names, " | "), sep)
	for _, m := range compareMetrics {
		vals := make([]float64, len(cols))
		cells := make([]string, len(cols))
		for i, c := range cols {
			vals[i] = c.value(m.get)
			cells[i] = m.render(vals[i])
		}
		var deltas []string
		for i := 1; i < len(cols); i++ {
			deltas = append(deltas, delta(vals[0], vals[i], m.sense))
		}
		p("| %s | %s | %s |\n", m.name, strings.Join(cells, " | "), strings.Join(deltas, " · "))
	}

	// Telemetry counters present in the baseline and at least one other.
	type tkey struct{ name, labels string }
	base := map[tkey]Telemetry{}
	first := cols[0].Runs[0]
	for _, t := range first.Telemetry {
		if t.Type == "counter" && !strings.HasSuffix(t.Name, "_bucket") {
			base[tkey{t.Name, t.Labels}] = t
		}
	}
	if len(base) > 0 && len(cols) > 1 {
		p("\n## Agent telemetry counters (Δ over window)\n\n| metric | labels | %s |\n|---|---|%s\n", strings.Join(names, " | "), sep)
		n := 0
		for _, t := range first.Telemetry {
			k := tkey{t.Name, t.Labels}
			if _, ok := base[k]; !ok || (!strings.HasPrefix(t.Name, "logs") && !strings.HasPrefix(t.Name, "process_")) {
				continue
			}
			cells := make([]string, len(cols))
			any := false
			for i, c := range cols {
				cells[i] = "–"
				var vals []float64
				for _, r := range c.Runs {
					for _, o := range r.Telemetry {
						if o.Name == k.name && o.Labels == k.labels {
							vals = append(vals, o.Delta)
						}
					}
				}
				if len(vals) > 0 {
					v := median(vals)
					cells[i] = fmtutil.Float(v, 0)
					if v != 0 {
						any = true
					}
				}
			}
			if !any {
				continue
			}
			n++
			if n > 60 {
				break
			}
			p("| `%s` | `%s` | %s |\n", t.Name, t.Labels, strings.Join(cells, " | "))
		}
	}
	return b.String()
}

// HeadlineRow is one line of the short verdict: a metric, one cell per
// column, and each non-baseline column's delta against the first.
type HeadlineRow struct {
	Metric string
	Cells  []string
	Deltas []string
}

// Headline is the handful of rows that answer "which agent is better".
func Headline(cols []Column) []HeadlineRow {
	var rows []HeadlineRow
	for _, name := range headlineMetrics {
		for _, m := range compareMetrics {
			if m.name != name {
				continue
			}
			row := HeadlineRow{Metric: m.name}
			vals := make([]float64, len(cols))
			for i, c := range cols {
				vals[i] = c.value(m.get)
				row.Cells = append(row.Cells, m.render(vals[i]))
			}
			for i := 1; i < len(cols); i++ {
				row.Deltas = append(row.Deltas, delta(vals[0], vals[i], m.sense))
			}
			rows = append(rows, row)
		}
	}
	return rows
}

func colNames(cols []Column) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = c.Name
	}
	return out
}

func delta(a, b float64, s sense) string {
	if a == 0 && b == 0 {
		return "="
	}
	if a == 0 || math.IsInf(b/a, 0) || math.IsNaN(b/a) {
		if s == lower && b > 0 {
			return fmt.Sprintf("+%s ⚠️", fmtutil.Float(b, 0))
		}
		return "n/a"
	}
	pct := (b - a) / math.Abs(a) * 100
	if math.Abs(pct) < 0.5 {
		return "≈"
	}
	out := fmt.Sprintf("%+.1f%%", pct)
	switch s {
	case neutral:
		return out
	case higher:
		if pct > 0 {
			return out + " ✅"
		}
		return out + " ⚠️"
	default:
		if pct < 0 {
			return out + " ✅"
		}
		return out + " ⚠️"
	}
}
