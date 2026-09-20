package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/fmtutil"
)

func secs(s float64) string { return fmtutil.Duration(time.Duration(s * float64(time.Second))) }

func keys(m map[string]int64) string {
	if len(m) == 0 {
		return "–"
	}
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return strings.Join(ks, ", ")
}

func pctOf(a, b int64) string {
	if b == 0 {
		return "–"
	}
	return fmtutil.Pct(float64(a) / float64(b))
}

// Markdown renders the report for humans (and for diffing in a PR).
func Markdown(r *Report) string {
	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	row := func(k string, v any) { p("| %s | %v |\n", k, v) }

	p("# aoc run: %s\n\n", r.Name)
	p("%s · window %s → %s (%s)\n\n", r.GeneratedAt.UTC().Format("2006-01-02 15:04 UTC"), r.WindowStart.UTC().Format("15:04:05"), r.WindowEnd.UTC().Format("15:04:05"), fmtutil.Duration(time.Duration(r.Seconds*float64(time.Second))))
	agent := keys(r.Agent.Versions)
	if r.Agent.Image != "" {
		agent += " (" + r.Agent.Image + ")"
	}
	p("**Agent** %s · encodings %s · paths %s  \n", agent, keys(r.Agent.Encodings), keys(r.Agent.Paths))
	var gens []string
	for _, g := range r.Generators {
		rate := "flat out"
		if g.TargetRate > 0 {
			rate = fmtutil.Rate(g.TargetRate)
		}
		gens = append(gens, fmt.Sprintf("%s (%s, %s→%s, %d streams, %s, rotate %s×%d %s)", g.Name, g.Mode, g.Format, g.Output, g.ActiveStreams, rate, fmtutil.Bytes(g.RotateBytes), g.RotateKeep, g.RotateMode))
	}
	if len(gens) == 0 {
		gens = []string{"none reported"}
	}
	p("**Generators** %s\n", strings.Join(gens, "; "))
	for _, n := range r.Notes {
		p("\n> %s\n", n)
	}

	d := r.Delivery
	p("\n## Delivery\n\n| metric | value |\n|---|---|\n")
	row("records written by generators", fmtutil.Int(d.GeneratedRecords))
	row("logs received by intake", fmtutil.Int(d.ReceivedLogs))
	row("unique records delivered", fmt.Sprintf("%s (%s)", fmtutil.Int(d.Unique), pctOf(d.Unique, d.GeneratedRecords)))
	lost := "lost"
	if !d.AllFinal {
		lost = "missing (generators still running)"
	}
	row(lost, fmtutil.Int(d.Missing))
	row("duplicates", fmtutil.Int(d.Duplicates))
	row("out of order", fmtutil.Int(d.OutOfOrder))
	row("orphan continuation lines (multiline split)", fmtutil.Int(d.Orphans))
	row("unmarked logs (other sources)", fmtutil.Int(d.Unmarked))
	row("truncated by the agent", fmtutil.Int(d.Truncated))
	row("multiline logs received / traces written", fmt.Sprintf("%s / %s", fmtutil.Int(d.Multiline), fmtutil.Int(d.MultilineWritten)))
	row("physical lines written / inside received logs", fmt.Sprintf("%s / %s", fmtutil.Int(d.GeneratedLines), fmtutil.Int(d.ReceivedLines)))

	t := r.Throughput
	p("\n## Throughput (window averages)\n\n| metric | value |\n|---|---|\n")
	row("generated", fmt.Sprintf("%s records, %s", fmtutil.Rate(t.GenRecordsPerSec), fmtutil.BytesF(t.GenBytesPerSec)+"/s"))
	row("received", fmt.Sprintf("%s logs, %s raw, %s on the wire", fmtutil.Rate(t.RecvLogsPerSec), fmtutil.BytesF(t.RecvRawBytesPerSec)+"/s", fmtutil.BytesF(t.RecvWireBytesPerSec)+"/s"))
	row("compression (decompressed ÷ wire)", fmt.Sprintf("%.2f×", t.CompressionRatio))
	row("peak second", fmt.Sprintf("%s generated, %s received", fmtutil.Rate(t.PeakGenRecordsPerSec), fmtutil.Rate(float64(t.PeakRecvLogsPerSec))))
	row("HTTP requests", fmt.Sprintf("%.1f/s", t.RequestsPerSec))

	l := r.Latency
	p("\n## Latency\n\n| path | p50 | p90 | p99 | p99.9 | max | samples |\n|---|---|---|---|---|---|---|\n")
	p("| written → intake (end to end) | %s | %s | %s | %s | %s | %s |\n", secs(l.EndToEnd.P50), secs(l.EndToEnd.P90), secs(l.EndToEnd.P99), secs(l.EndToEnd.P999), secs(l.EndToEnd.Max), fmtutil.Int(int64(l.EndToEnd.Count)))
	p("| agent encode → intake (sender) | %s | %s | %s | %s | %s | %s |\n", secs(l.Sender.P50), secs(l.Sender.P90), secs(l.Sender.P99), secs(l.Sender.P999), secs(l.Sender.Max), fmtutil.Int(int64(l.Sender.Count)))
	p("| intake processing | %s | %s | %s | %s | %s | %s |\n", secs(l.Processing.P50), secs(l.Processing.P90), secs(l.Processing.P99), secs(l.Processing.P999), secs(l.Processing.Max), fmtutil.Int(int64(l.Processing.Count)))
	if l.NoTimestamp > 0 {
		p("\n%s marked logs had no parseable write timestamp (deterministic runs stamp year 2000, so e2e is n/a there).\n", fmtutil.Int(l.NoTimestamp))
	}

	res := r.Resources
	p("\n## Agent resources\n\n| metric | value |\n|---|---|\n")
	if res.ContainerCPUMax > 0 || res.ContainerMemMax > 0 {
		row("container CPU avg / max", fmt.Sprintf("%.1f%% / %.1f%%", res.ContainerCPUAvg, res.ContainerCPUMax))
		row("container memory avg / max", fmt.Sprintf("%s / %s", fmtutil.Bytes(res.ContainerMemAvg), fmtutil.Bytes(res.ContainerMemMax)))
	} else {
		row("container CPU / memory", "n/a (run the intake with --docker-container)")
	}
	if res.ProcessCPUMax > 0 || res.ProcessRSSMax > 0 {
		row("core agent process CPU avg / max", fmt.Sprintf("%.1f%% / %.1f%%", res.ProcessCPUAvg, res.ProcessCPUMax))
		row("core agent process RSS max", fmtutil.Bytes(res.ProcessRSSMax))
		row("core agent CPU seconds over window", fmt.Sprintf("%.1f", res.ProcessCPUSecs))
		row("CPU seconds per 1M logs delivered", fmt.Sprintf("%.2f", res.CPUSecondsPerMLogs))
	} else {
		row("core agent process", "n/a (run the intake with --agent-telemetry and DD_TELEMETRY_ENABLED=true)")
	}
	row("generators CPU (sum of averages)", fmt.Sprintf("%.0f%%", res.GenCPUAvg))
	row("intake CPU avg / RSS max", fmt.Sprintf("%.0f%% / %s", res.IntakeCPUAvg, fmtutil.Bytes(res.IntakeRSSMax)))

	h := r.HTTP
	p("\n## Intake HTTP\n\n| metric | value |\n|---|---|\n")
	var st []string
	for _, k := range sortedKeys(h.ByStatus) {
		st = append(st, fmt.Sprintf("%s: %s", k, fmtutil.Int(h.ByStatus[k])))
	}
	row("logs requests by status", strings.Join(st, ", "))
	row("logs per payload p50 / p99 / max", fmt.Sprintf("%.0f / %.0f / %.0f", h.LogsPerPayload.P50, h.LogsPerPayload.P99, h.LogsPerPayload.Max))
	row("payload on wire p50 / p99 / max", fmt.Sprintf("%s / %s / %s", fmtutil.BytesF(h.PayloadWire.P50), fmtutil.BytesF(h.PayloadWire.P99), fmtutil.BytesF(h.PayloadWire.Max)))
	row("payload decompressed p50 / p99 / max", fmt.Sprintf("%s / %s / %s", fmtutil.BytesF(h.PayloadRaw.P50), fmtutil.BytesF(h.PayloadRaw.P99), fmtutil.BytesF(h.PayloadRaw.Max)))
	row("total wire / decompressed", fmt.Sprintf("%s / %s", fmtutil.Bytes(h.WireBytes), fmtutil.Bytes(h.RawBytes)))
	row("connectivity probes / malformed", fmt.Sprintf("%d / %d", h.Probes, h.Malformed))
	var rej []string
	for _, k := range sortedKeys(h.Rejected) {
		rej = append(rej, fmt.Sprintf("%s: %d", k, h.Rejected[k]))
	}
	if len(rej) > 0 {
		row("rejected", strings.Join(rej, ", "))
	}
	if h.FaultDropped+h.FaultErrored+h.FaultDelayed+h.FaultSlowed > 0 {
		row("injected faults dropped / errored / delayed / slowed", fmt.Sprintf("%d / %d / %d / %d", h.FaultDropped, h.FaultErrored, h.FaultDelayed, h.FaultSlowed))
	}
	if h.TCPFrames > 0 {
		row("TCP frames / bytes", fmt.Sprintf("%s / %s", fmtutil.Int(h.TCPFrames), fmtutil.Bytes(h.TCPBytes)))
	}
	var other []string
	for _, k := range sortedKeys(h.OtherRequests) {
		other = append(other, fmt.Sprintf("%s ×%d", k, h.OtherRequests[k]))
	}
	if len(other) > 0 {
		row("other agent traffic (sink)", strings.Join(other, ", "))
	}

	tg := r.Tags
	p("\n## Tags\n\n| metric | value |\n|---|---|\n")
	row("tags per log (avg)", fmt.Sprintf("%.2f", tg.AvgTagsPerLog))
	row("tag bytes per log (avg)", fmt.Sprintf("%.1f B", tg.AvgTagBytesPerLog))
	row("total tag bytes", fmtutil.Bytes(tg.TotalTagBytes))
	row("unique tag keys", tg.UniqueKeys)
	if len(tg.Keys) > 0 {
		p("\n| tag key | on logs | share |\n|---|---|---|\n")
		for i, k := range tg.Keys {
			if i >= 30 {
				p("| … | | |\n")
				break
			}
			p("| `%s` | %s | %s |\n", k.Name, fmtutil.Int(k.Count), pctOf(k.Count, d.ReceivedLogs))
		}
	}

	bd := r.Breakdown
	p("\n## Breakdown\n\n| kind | value | logs | bytes |\n|---|---|---|---|\n")
	for _, kind := range []struct {
		n    string
		rows []NameCount
	}{{"service", bd.Services}, {"source", bd.Sources}, {"host", bd.Hosts}, {"status", bd.Statuses}} {
		for i, c := range kind.rows {
			if i >= 10 {
				p("| %s | … (%d more) | | |\n", kind.n, len(kind.rows)-10)
				break
			}
			p("| %s | %s | %s | %s |\n", kind.n, c.Name, fmtutil.Int(c.Count), fmtutil.Bytes(c.Bytes))
		}
	}

	var bad []Stream
	for _, s := range r.Streams {
		if s.Missing > 0 || s.Duplicates > 0 || s.OutOfOrder > 0 {
			bad = append(bad, s)
		}
	}
	p("\n## Streams\n\n%d streams; %d with missing, duplicated or reordered records.\n", len(r.Streams), len(bad))
	if len(bad) > 0 {
		sort.Slice(bad, func(i, j int) bool { return bad[i].Missing+bad[i].Duplicates > bad[j].Missing+bad[j].Duplicates })
		p("\n| gen | stream | generated | unique | missing | dups | out of order | first missing ranges |\n|---|---|---|---|---|---|---|---|\n")
		for i, s := range bad {
			if i >= 25 {
				p("| … | | | | | | | |\n")
				break
			}
			var gaps []string
			for j, g := range s.Gaps {
				if j >= 5 {
					gaps = append(gaps, "…")
					break
				}
				if g.From == g.To {
					gaps = append(gaps, fmt.Sprintf("%d", g.From))
				} else {
					gaps = append(gaps, fmt.Sprintf("%d–%d", g.From, g.To))
				}
			}
			p("| %s | %s | %s | %s | %s | %s | %s | %s |\n", s.Gen, s.Stream, fmtutil.Int(s.Generated), fmtutil.Int(s.Unique), fmtutil.Int(s.Missing), fmtutil.Int(s.Duplicates), fmtutil.Int(s.OutOfOrder), strings.Join(gaps, ", "))
		}
	}

	if len(r.Telemetry) > 0 {
		p("\n## Agent telemetry\n\nCounters show the change over the window; gauges show the last value.\n\n| metric | labels | type | Δ / last | rate /s |\n|---|---|---|---|---|\n")
		n := 0
		for _, m := range r.Telemetry {
			if !strings.HasPrefix(m.Name, "logs") && !strings.HasPrefix(m.Name, "process_") {
				continue
			}
			if m.Type == "counter" && m.Delta == 0 {
				continue
			}
			if strings.HasSuffix(m.Name, "_bucket") {
				continue
			}
			n++
			if n > 80 {
				p("| … | | | | |\n")
				break
			}
			val, rate := fmtutil.Float(m.Last, 2), ""
			if m.Type == "counter" {
				val, rate = fmtutil.Float(m.Delta, 0), fmtutil.Float(m.Rate, 2)
			}
			p("| `%s` | `%s` | %s | %s | %s |\n", m.Name, m.Labels, m.Type, val, rate)
		}
	}

	if len(r.Faults) > 0 {
		p("\n## Faults\n\n")
		for _, f := range r.Faults {
			p("- %s — %s\n", f.At.UTC().Format("15:04:05"), f.Desc)
		}
	}

	if len(r.Minutes) > 1 {
		p("\n## Per minute\n\n| minute (UTC) | generated /s | received /s | wire /s | e2e p50 | e2e p99 | agent cpu | agent mem | errors |\n|---|---|---|---|---|---|---|---|---|\n")
		for _, m := range r.Minutes {
			cpu, mem := "–", "–"
			if m.AgentCPU >= 0 {
				cpu = fmt.Sprintf("%.0f%%", m.AgentCPU)
			} else if m.ProcCPU >= 0 {
				cpu = fmt.Sprintf("%.0f%% (proc)", m.ProcCPU)
			}
			if m.AgentMem >= 0 {
				mem = fmtutil.Bytes(m.AgentMem)
			}
			p("| %s | %s | %s | %s | %s | %s | %s | %s | %d |\n", time.Unix(m.T, 0).UTC().Format("15:04"), fmtutil.Rate(m.GenRecords), fmtutil.Rate(m.RecvLogs), fmtutil.BytesF(m.WireBytes)+"/s", secs(m.E2EP50), secs(m.E2EP99), cpu, mem, m.Errors)
		}
	}
	return b.String()
}

func sortedKeys(m map[string]int64) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
