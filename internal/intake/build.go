package intake

import (
	"sort"
	"strconv"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

func q(h *hist) report.Quantiles {
	s := h.summary()
	return report.Quantiles{Count: s.Count, Mean: s.Mean, P50: s.P50, P90: s.P90, P99: s.P99, P999: s.P999, Max: s.Max, Min: s.Min}
}

func toNC(in []nameCount) []report.NameCount {
	out := make([]report.NameCount, len(in))
	for i, c := range in {
		out[i] = report.NameCount{Name: c.Name, Count: c.Count, Bytes: c.Bytes}
	}
	return out
}

func copyMap(m map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// buildReport assembles the report for the window since the last reset.
// withGaps walks each stream's bitset to list missing ranges (costly for
// very long streams, so the 1 Hz UI snapshot skips it).
func (s *Server) buildReport(withGaps bool) *report.Report {
	now := time.Now()
	st := s.stats
	st.mu.Lock()
	st.ts.roll(now, st)
	r := &report.Report{
		Schema: report.Schema, Name: s.cfg.Name, GeneratedAt: now,
		WindowStart: st.resetAt, WindowEnd: now, Seconds: now.Sub(st.resetAt).Seconds(),
	}
	r.Agent = report.Agent{
		Versions: copyMap(st.agentVersions), Origins: copyMap(st.origins), UserAgent: copyMap(st.userAgents),
		APIKeys: copyMap(st.apiKeys), Encodings: copyMap(st.byEncoding), Paths: copyMap(st.byPath), Image: s.cfg.AgentImage,
		Container: s.cfg.DockerContainer,
	}
	if hosts := topN(st.hosts, 1); len(hosts) > 0 && hosts[0].Name != "(none)" {
		r.Agent.Hostname = hosts[0].Name
	}

	// Generators + per-stream generated seqs.
	type genSeq struct {
		seq    int64
		active bool
	}
	genSeqs := map[string]map[string]genSeq{}
	allFinal := len(st.gens) > 0
	var genCPU float64
	for name, g := range st.gens {
		l := g.Last
		tot := l.Totals
		tot.Lines -= g.base.Lines
		tot.Records -= g.base.Records
		tot.Bytes -= g.base.Bytes
		tot.Rotations -= g.base.Rotations
		tot.Multiline -= g.base.Multiline
		tot.Wide -= g.base.Wide
		tot.Bursts -= g.base.Bursts
		tot.Errors -= g.base.Errors
		cpuAvg := 0.0
		if g.cpuN > 0 {
			cpuAvg = g.cpuSum / float64(g.cpuN)
		}
		genCPU += cpuAvg
		r.Generators = append(r.Generators, report.Generator{
			Name: name, Host: l.Host, Mode: l.Mode, Scenario: l.Scenario, Phase: l.Phase, Format: l.Format, Output: l.Output,
			ActiveStreams: g.MaxStreams, TargetRate: l.Knobs.Rate, Deterministic: l.Deterministic, Seed: l.Seed,
			RotateBytes: l.RotateBytes, RotateKeep: l.RotateKeep, RotateMode: l.RotateMode,
			Records: tot.Records, Lines: tot.Lines, Bytes: tot.Bytes, Rotations: tot.Rotations, Multiline: tot.Multiline,
			Wide: tot.Wide, Bursts: tot.Bursts, WriteErrors: tot.Errors, CPUAvgPercent: cpuAvg, RSSMaxBytes: g.RSSMax,
			FirstSeen: g.FirstSeen, LastSeen: g.LastSeen, Final: l.Final,
		})
		r.Delivery.GeneratedRecords += tot.Records
		r.Delivery.GeneratedLines += tot.Lines
		r.Delivery.GeneratedBytes += tot.Bytes
		r.Delivery.MultilineWritten += tot.Multiline
		if !l.Final {
			allFinal = false
		}
		m := map[string]genSeq{}
		for _, sr := range l.Streams {
			m[sr.Name] = genSeq{seq: sr.Seq - g.baseSeq[sr.Name], active: sr.Active}
		}
		genSeqs[name] = m
	}
	sort.Slice(r.Generators, func(i, j int) bool { return r.Generators[i].Name < r.Generators[j].Name })

	d := &r.Delivery
	d.ReceivedLogs = st.logs
	d.ReceivedMarked = st.marked
	d.Orphans = st.orphans
	d.Unmarked = st.unmarked
	d.Multiline = st.multiline
	d.ReceivedLines = st.lines
	d.Truncated = st.truncated
	d.AllFinal = allFinal

	r.Latency = report.Latency{EndToEnd: q(st.e2e), Sender: q(st.sender), Processing: q(st.procTime), NoTimestamp: st.e2eNoTS}

	h := &r.HTTP
	h.Requests = st.requests
	h.ByStatus = map[string]int64{}
	for code, n := range st.byStatus {
		h.ByStatus[strconv.Itoa(code)] = n
	}
	h.Probes, h.Malformed = st.probes, st.malformed
	h.Rejected = copyMap(st.rejected)
	h.WireBytes, h.RawBytes = st.wireBytes, st.rawBytes
	h.PayloadWire, h.PayloadRaw, h.LogsPerPayload = q(st.payloadWire), q(st.payloadRaw), q(st.logsPerPayload)
	h.FaultDropped, h.FaultErrored, h.FaultDelayed, h.FaultSlowed = st.faultDropped, st.faultErrored, st.faultDelayed, st.faultSlowed
	h.OtherRequests = copyMap(st.otherRequests)
	h.TCPFrames, h.TCPBytes = st.tcpFrames, st.tcpBytes

	t := &r.Tags
	t.TotalTags, t.TotalTagBytes = st.tagCount, st.tagBytes
	if st.logs > 0 {
		t.AvgTagsPerLog = float64(st.tagCount) / float64(st.logs)
		t.AvgTagBytesPerLog = float64(st.tagBytes) / float64(st.logs)
	}
	t.UniqueKeys = len(st.tagKeys)
	t.Keys = toNC(topN(st.tagKeys, 100))

	r.Breakdown = report.Breakdown{
		Services: toNC(topN(st.services, 50)), Sources: toNC(topN(st.sources, 50)),
		Hosts: toNC(topN(st.hosts, 50)), Statuses: toNC(topN(st.statuses, 20)),
	}

	for _, f := range st.faultLog {
		r.Faults = append(r.Faults, report.FaultEvent{At: f.At, Desc: f.Desc})
	}

	points := st.ts.since(st.resetAt.Unix())
	st.mu.Unlock()

	// Streams: union of what the intake saw and what generators reported.
	// "Generated" per stream is the generator's last reported seq or, if the
	// intake has already seen a higher seq (reports lag by up to a second),
	// that: every marker received proves the record was written.
	tracked := st.track.snapshot()
	seen := map[string]map[string]bool{}
	var effectiveGenerated int64
	for _, tr := range tracked {
		gs := genSeqs[tr.Gen][tr.Stream]
		upTo := tr.MaxSeq
		if gs.seq > upTo {
			upTo = gs.seq
		}
		effectiveGenerated += upTo
		missing := upTo - tr.Unique - tr.Untracked
		if missing < 0 {
			missing = 0
		}
		rs := report.Stream{
			Gen: tr.Gen, Stream: tr.Stream, Generated: upTo, Received: tr.Received, Unique: tr.Unique,
			Duplicates: tr.Dups, Missing: missing, OutOfOrder: tr.OutOfOrder, MaxSeq: tr.MaxSeq, Bytes: tr.Bytes,
			Multiline: tr.Multiline, FirstAt: tr.FirstAt, LastAt: tr.LastAt, Active: gs.active,
		}
		if withGaps && missing > 0 {
			st.track.withStream(tr.Gen, tr.Stream, func(t *streamTrack) {
				for _, g := range t.gaps(upTo, 20) {
					rs.Gaps = append(rs.Gaps, report.SeqRange{From: g.From, To: g.To})
				}
			})
		}
		r.Streams = append(r.Streams, rs)
		if seen[tr.Gen] == nil {
			seen[tr.Gen] = map[string]bool{}
		}
		seen[tr.Gen][tr.Stream] = true
		d.Unique += tr.Unique
		d.Duplicates += tr.Dups
		d.OutOfOrder += tr.OutOfOrder
		d.Missing += missing
	}
	for gen, streams := range genSeqs {
		for name, gs := range streams {
			if seen[gen][name] || gs.seq <= 0 {
				continue
			}
			r.Streams = append(r.Streams, report.Stream{Gen: gen, Stream: name, Generated: gs.seq, Missing: gs.seq, Active: gs.active})
			d.Missing += gs.seq
			effectiveGenerated += gs.seq
		}
	}
	if effectiveGenerated > d.GeneratedRecords {
		d.GeneratedRecords = effectiveGenerated
	}
	sort.Slice(r.Streams, func(i, j int) bool {
		if r.Streams[i].Gen != r.Streams[j].Gen {
			return r.Streams[i].Gen < r.Streams[j].Gen
		}
		return r.Streams[i].Stream < r.Streams[j].Stream
	})
	if d.GeneratedRecords > 0 {
		d.Ratio = float64(d.Unique) / float64(d.GeneratedRecords)
	}

	// Throughput over the window, peaks from the per-second points.
	tp := &r.Throughput
	if r.Seconds > 0 {
		tp.GenRecordsPerSec = float64(d.GeneratedRecords) / r.Seconds
		tp.GenLinesPerSec = float64(d.GeneratedLines) / r.Seconds
		tp.GenBytesPerSec = float64(d.GeneratedBytes) / r.Seconds
		tp.RecvLogsPerSec = float64(d.ReceivedLogs) / r.Seconds
		tp.RecvRawBytesPerSec = float64(h.RawBytes) / r.Seconds
		tp.RecvWireBytesPerSec = float64(h.WireBytes) / r.Seconds
		tp.RequestsPerSec = float64(h.Requests) / r.Seconds
	}
	if h.WireBytes > 0 {
		tp.CompressionRatio = float64(h.RawBytes) / float64(h.WireBytes)
	}

	// Resources + per-minute rollups.
	res := &r.Resources
	var cpuSum, procSum float64
	var memSum, anonSum, fileSum int64
	var nCPU, nMem, nProc, nAnon int
	minutes := map[int64]*minuteAcc{}
	for _, p := range points {
		if p.Logs > tp.PeakRecvLogsPerSec {
			tp.PeakRecvLogsPerSec = p.Logs
		}
		if p.GenRecords > tp.PeakGenRecordsPerSec {
			tp.PeakGenRecordsPerSec = p.GenRecords
		}
		if p.AgentCPU >= 0 {
			cpuSum += p.AgentCPU
			nCPU++
			if p.AgentCPU > res.ContainerCPUMax {
				res.ContainerCPUMax = p.AgentCPU
			}
		}
		if p.AgentMem >= 0 {
			memSum += p.AgentMem
			nMem++
			if p.AgentMem > res.ContainerMemMax {
				res.ContainerMemMax = p.AgentMem
			}
		}
		if p.ProcCPU >= 0 {
			procSum += p.ProcCPU
			nProc++
			if p.ProcCPU > res.ProcessCPUMax {
				res.ProcessCPUMax = p.ProcCPU
			}
		}
		if p.ProcRSS > res.ProcessRSSMax {
			res.ProcessRSSMax = p.ProcRSS
		}
		if p.AgentAnon >= 0 {
			anonSum += p.AgentAnon
			nAnon++
			if p.AgentAnon > res.ContainerAnonMax {
				res.ContainerAnonMax = p.AgentAnon
			}
		}
		if p.AgentFile >= 0 {
			fileSum += p.AgentFile
			if p.AgentFile > res.ContainerFileMax {
				res.ContainerFileMax = p.AgentFile
			}
		}
		m := p.T - p.T%60
		acc := minutes[m]
		if acc == nil {
			acc = &minuteAcc{t: m, mem: -1}
			minutes[m] = acc
		}
		acc.add(p)
	}
	res.Samples = len(points)
	if nCPU > 0 {
		res.ContainerCPUAvg = cpuSum / float64(nCPU)
	}
	if nMem > 0 {
		res.ContainerMemAvg = memSum / int64(nMem)
	}
	if nProc > 0 {
		res.ProcessCPUAvg = procSum / float64(nProc)
	}
	if nAnon > 0 {
		res.ContainerAnonAvg, res.ContainerFileAvg = anonSum/int64(nAnon), fileSum/int64(nAnon)
	}
	if s.agent != nil {
		res.Processes = s.agent.processes()
	}
	res.IntakeCPUAvg, res.IntakeRSSMax = s.proc.Averages()
	res.GenCPUAvg = genCPU

	if s.agent != nil {
		for _, m := range s.agent.series() {
			t := report.Telemetry{Name: m.Name, Labels: m.Labels, Type: m.Type, First: m.First, Last: m.Value, Delta: m.Value - m.First, Mean: m.Mean}
			if (m.Type == "counter") && r.Seconds > 0 {
				t.Rate = t.Delta / r.Seconds
			}
			if m.Name == "process_cpu_seconds_total" {
				res.ProcessCPUSecs = t.Delta
			}
			r.Telemetry = append(r.Telemetry, t)
		}
	}
	if res.ProcessCPUSecs > 0 && d.ReceivedLogs > 0 {
		res.CPUSecondsPerMLogs = res.ProcessCPUSecs / (float64(d.ReceivedLogs) / 1e6)
	}

	keys := make([]int64, 0, len(minutes))
	for k := range minutes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	for _, k := range keys {
		r.Minutes = append(r.Minutes, minutes[k].result())
	}
	return r
}

type minuteAcc struct {
	t                                 int64
	n                                 int
	genRecords, logs, raw, wire, reqs float64
	errors                            int64
	e2e50Sum, e2e99Max                float64
	nE2E                              int
	cpu, proc                         float64
	nCPU, nProc                       int
	mem                               int64
	nMem                              int
}

func (a *minuteAcc) add(p Point) {
	a.n++
	a.genRecords += p.GenRecords
	a.logs += float64(p.Logs)
	a.raw += float64(p.RawBytes)
	a.wire += float64(p.WireBytes)
	a.reqs += float64(p.Requests)
	a.errors += p.Err4xx + p.Err5xx + p.Dropped
	if p.E2EP50 > 0 {
		a.e2e50Sum += p.E2EP50
		a.nE2E++
	}
	if p.E2EP99 > a.e2e99Max {
		a.e2e99Max = p.E2EP99
	}
	if p.AgentCPU >= 0 {
		a.cpu += p.AgentCPU
		a.nCPU++
	}
	if p.ProcCPU >= 0 {
		a.proc += p.ProcCPU
		a.nProc++
	}
	if p.AgentMem >= 0 {
		if a.mem < 0 {
			a.mem = 0
		}
		a.mem += p.AgentMem
		a.nMem++
	}
}

func (a *minuteAcc) result() report.Minute {
	n := float64(a.n)
	m := report.Minute{T: a.t, GenRecords: a.genRecords / n, RecvLogs: a.logs / n, RawBytes: a.raw / n, WireBytes: a.wire / n,
		Requests: a.reqs / n, Errors: a.errors, E2EP99: a.e2e99Max, AgentCPU: -1, AgentMem: -1, ProcCPU: -1}
	if a.nE2E > 0 {
		m.E2EP50 = a.e2e50Sum / float64(a.nE2E)
	}
	if a.nCPU > 0 {
		m.AgentCPU = a.cpu / float64(a.nCPU)
	}
	if a.nProc > 0 {
		m.ProcCPU = a.proc / float64(a.nProc)
	}
	if a.nMem > 0 {
		m.AgentMem = a.mem / int64(a.nMem)
	}
	return m
}
