package intake

import (
	"fmt"
	"net/http"
	"sort"
)

// handleMetrics exposes the harness state in Prometheus text format, so the
// agent under test (or anything else) can scrape it.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	rep := s.buildReport(false)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	p := func(format string, args ...any) { fmt.Fprintf(w, format, args...) }

	p("# HELP aoc_intake_requests_total Logs requests received, by response status.\n# TYPE aoc_intake_requests_total counter\n")
	codes := make([]string, 0, len(rep.HTTP.ByStatus))
	for c := range rep.HTTP.ByStatus {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		p("aoc_intake_requests_total{status=%q} %d\n", c, rep.HTTP.ByStatus[c])
	}
	p("# TYPE aoc_intake_logs_total counter\naoc_intake_logs_total %d\n", rep.Delivery.ReceivedLogs)
	p("# TYPE aoc_intake_marked_total counter\naoc_intake_marked_total %d\n", rep.Delivery.ReceivedMarked)
	p("# TYPE aoc_intake_unique_total counter\naoc_intake_unique_total %d\n", rep.Delivery.Unique)
	p("# TYPE aoc_intake_duplicates_total counter\naoc_intake_duplicates_total %d\n", rep.Delivery.Duplicates)
	p("# TYPE aoc_intake_out_of_order_total counter\naoc_intake_out_of_order_total %d\n", rep.Delivery.OutOfOrder)
	p("# TYPE aoc_intake_orphans_total counter\naoc_intake_orphans_total %d\n", rep.Delivery.Orphans)
	p("# TYPE aoc_intake_multiline_total counter\naoc_intake_multiline_total %d\n", rep.Delivery.Multiline)
	p("# TYPE aoc_intake_truncated_total counter\naoc_intake_truncated_total %d\n", rep.Delivery.Truncated)
	p("# HELP aoc_intake_missing Generated records not (yet) received.\n# TYPE aoc_intake_missing gauge\naoc_intake_missing %d\n", rep.Delivery.Missing)
	p("# TYPE aoc_intake_bytes_total counter\n")
	p("aoc_intake_bytes_total{kind=\"wire\"} %d\naoc_intake_bytes_total{kind=\"raw\"} %d\n", rep.HTTP.WireBytes, rep.HTTP.RawBytes)
	p("# TYPE aoc_intake_tags_total counter\naoc_intake_tags_total %d\n# TYPE aoc_intake_tag_bytes_total counter\naoc_intake_tag_bytes_total %d\n", rep.Tags.TotalTags, rep.Tags.TotalTagBytes)
	p("# TYPE aoc_intake_faults_total counter\n")
	p("aoc_intake_faults_total{kind=\"dropped\"} %d\naoc_intake_faults_total{kind=\"errored\"} %d\naoc_intake_faults_total{kind=\"delayed\"} %d\n", rep.HTTP.FaultDropped, rep.HTTP.FaultErrored, rep.HTTP.FaultDelayed)
	f := s.faults.get()
	active := 0
	if f.Active() {
		active = 1
	}
	p("# TYPE aoc_intake_faults_active gauge\naoc_intake_faults_active %d\n", active)
	p("# TYPE aoc_intake_inflight gauge\naoc_intake_inflight %d\n", s.stats.inflight.Load())

	for _, name := range []struct {
		n string
		q func() (uint64, float64, float64, float64, float64, float64)
	}{
		{"aoc_intake_e2e_seconds", func() (uint64, float64, float64, float64, float64, float64) {
			l := rep.Latency.EndToEnd
			return l.Count, l.Mean * float64(l.Count), l.P50, l.P90, l.P99, l.Max
		}},
		{"aoc_intake_sender_seconds", func() (uint64, float64, float64, float64, float64, float64) {
			l := rep.Latency.Sender
			return l.Count, l.Mean * float64(l.Count), l.P50, l.P90, l.P99, l.Max
		}},
	} {
		c, sum, p50, p90, p99, mx := name.q()
		p("# TYPE %s summary\n", name.n)
		p("%s{quantile=\"0.5\"} %g\n%s{quantile=\"0.9\"} %g\n%s{quantile=\"0.99\"} %g\n%s{quantile=\"1\"} %g\n%s_sum %g\n%s_count %d\n",
			name.n, p50, name.n, p90, name.n, p99, name.n, mx, name.n, sum, name.n, c)
	}

	p("# TYPE aoc_gen_records_total counter\n# TYPE aoc_gen_bytes_total counter\n# TYPE aoc_gen_rotations_total counter\n")
	for _, g := range rep.Generators {
		p("aoc_gen_records_total{gen=%q} %d\naoc_gen_bytes_total{gen=%q} %d\naoc_gen_rotations_total{gen=%q} %d\n", g.Name, g.Records, g.Name, g.Bytes, g.Name, g.Rotations)
	}
	p("# TYPE aoc_stream_missing gauge\n# TYPE aoc_stream_duplicates_total counter\n")
	for _, st := range rep.Streams {
		p("aoc_stream_missing{gen=%q,stream=%q} %d\naoc_stream_duplicates_total{gen=%q,stream=%q} %d\n", st.Gen, st.Stream, st.Missing, st.Gen, st.Stream, st.Duplicates)
	}
	if s.agent != nil {
		st := s.agent.status()
		if st.DockerOK {
			p("# TYPE aoc_agent_container_cpu_percent gauge\naoc_agent_container_cpu_percent %.2f\n", st.CPUPercent)
			p("# TYPE aoc_agent_container_memory_bytes gauge\naoc_agent_container_memory_bytes %d\n", st.MemBytes)
		}
		if st.TelemetryOK {
			p("# TYPE aoc_agent_process_cpu_percent gauge\naoc_agent_process_cpu_percent %.2f\n", st.ProcCPUPercent)
			p("# TYPE aoc_agent_process_rss_bytes gauge\naoc_agent_process_rss_bytes %d\n", st.ProcRSS)
		}
	}
}
