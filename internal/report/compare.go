package report

import (
	"fmt"
	"math"
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

// Compare renders a side-by-side Markdown table of two or more reports. The
// first report is the baseline; deltas are relative to it.
func Compare(reports []*Report) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	p("# aoc compare\n\n")
	p("| | %s |\n|---|", strings.Join(names(reports), " | "))
	for range reports {
		p("---|")
	}
	p("\n")
	agents := make([]string, len(reports))
	for i, r := range reports {
		agents[i] = keys(r.Agent.Versions)
		if r.Agent.Image != "" {
			agents[i] += " (" + r.Agent.Image + ")"
		}
	}
	p("| agent | %s |\n", strings.Join(agents, " | "))
	win := make([]string, len(reports))
	for i, r := range reports {
		win[i] = secs(r.Seconds)
	}
	p("| window | %s |\n", strings.Join(win, " | "))

	p("\n## Metrics (baseline = %s)\n\n| metric | %s | Δ vs baseline |\n|---|", reports[0].Name, strings.Join(names(reports), " | "))
	for range reports {
		p("---|")
	}
	p("---|\n")
	for _, m := range compareMetrics {
		vals := make([]float64, len(reports))
		cells := make([]string, len(reports))
		for i, r := range reports {
			vals[i] = m.get(r)
			cells[i] = m.render(vals[i])
		}
		var deltas []string
		for i := 1; i < len(reports); i++ {
			deltas = append(deltas, delta(vals[0], vals[i], m.sense))
		}
		p("| %s | %s | %s |\n", m.name, strings.Join(cells, " | "), strings.Join(deltas, " · "))
	}

	// Telemetry counters present in the baseline and at least one other.
	type tkey struct{ name, labels string }
	base := map[tkey]Telemetry{}
	for _, t := range reports[0].Telemetry {
		if t.Type == "counter" && !strings.HasSuffix(t.Name, "_bucket") {
			base[tkey{t.Name, t.Labels}] = t
		}
	}
	if len(base) > 0 && len(reports) > 1 {
		p("\n## Agent telemetry counters (Δ over window)\n\n| metric | labels | %s |\n|---|---|", strings.Join(names(reports), " | "))
		for range reports {
			p("---|")
		}
		p("\n")
		n := 0
		for _, t := range reports[0].Telemetry {
			k := tkey{t.Name, t.Labels}
			if _, ok := base[k]; !ok || (!strings.HasPrefix(t.Name, "logs") && !strings.HasPrefix(t.Name, "process_")) {
				continue
			}
			cells := make([]string, len(reports))
			any := false
			for i, r := range reports {
				cells[i] = "–"
				for _, o := range r.Telemetry {
					if o.Name == k.name && o.Labels == k.labels {
						cells[i] = fmtutil.Float(o.Delta, 0)
						if o.Delta != 0 {
							any = true
						}
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

func names(rs []*Report) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = r.Name
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
