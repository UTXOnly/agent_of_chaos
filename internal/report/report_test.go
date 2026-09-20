package report

import (
	"strings"
	"testing"
	"time"
)

func sample(name string, unique int64) *Report {
	r := &Report{Schema: Schema, Name: name, GeneratedAt: time.Now(), Seconds: 60}
	r.Delivery = Delivery{GeneratedRecords: 1000, ReceivedLogs: unique, Unique: unique, Missing: 1000 - unique, Ratio: float64(unique) / 1000, AllFinal: true}
	r.Throughput = Throughput{GenRecordsPerSec: 16.6, RecvLogsPerSec: float64(unique) / 60, CompressionRatio: 10}
	r.Latency.EndToEnd = Quantiles{Count: 10, P50: 0.2, P99: 0.5, Max: 1}
	r.HTTP.ByStatus = map[string]int64{"202": 3}
	r.Agent.Versions = map[string]int64{"7.99": 3}
	r.Telemetry = []Telemetry{{Name: "logs__bytes_sent", Type: "counter", First: 0, Last: 100, Delta: 100, Rate: 1.6}}
	r.Streams = []Stream{{Gen: "g", Stream: "s", Generated: 1000, Unique: unique, Missing: 1000 - unique, Gaps: []SeqRange{{5, 9}}}}
	r.Minutes = []Minute{{T: 0, RecvLogs: 10, AgentCPU: -1, AgentMem: -1, ProcCPU: -1}, {T: 60, RecvLogs: 12, AgentCPU: 12, AgentMem: 1e8, ProcCPU: 5}}
	return r
}

func TestMarkdownAndCompare(t *testing.T) {
	a, b := sample("baseline", 1000), sample("candidate", 990)
	md := Markdown(a)
	for _, want := range []string{"# aoc run: baseline", "| unique records delivered | 1,000 (100.00%) |", "## Latency", "logs__bytes_sent", "## Per minute"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q", want)
		}
	}
	mdb := Markdown(b)
	if !strings.Contains(mdb, "| lost | 10 |") || !strings.Contains(mdb, "5–9") {
		t.Errorf("candidate markdown: %s", mdb)
	}
	cmp := Compare([]*Report{a, b})
	if !strings.Contains(cmp, "| lost / missing | 0 | 10 | n/a |") && !strings.Contains(cmp, "| lost / missing | 0 | 10 |") {
		t.Errorf("compare lost row missing:\n%s", cmp)
	}
	if !strings.Contains(cmp, "delivery ratio | 100.00% | 99.00% | -1.0% ⚠️") {
		t.Errorf("compare ratio row wrong:\n%s", cmp)
	}
}

func TestCompareColumnsMedianAndHeadline(t *testing.T) {
	a1, a2, a3 := sample("x-a", 1000), sample("x-a-2", 1000), sample("x-a-3", 1000)
	a1.Resources.ContainerCPUAvg, a2.Resources.ContainerCPUAvg, a3.Resources.ContainerCPUAvg = 30, 50, 40
	b1, b2 := sample("x-b", 990), sample("x-b-2", 1000)
	b1.Resources.ContainerCPUAvg, b2.Resources.ContainerCPUAvg = 60, 70
	b1.Agent.Image, b1.Agent.Digest = "datadog/agent-dev:x", "datadog/agent-dev@sha256:0123456789abcdef0123"
	cols := []Column{{Name: "release", Runs: []*Report{a1, a2, a3}}, {Name: "candidate", Runs: []*Report{b1, b2}}}
	md := CompareColumns(cols)
	for _, want := range []string{
		"| runs (values are medians) | 3 | 2 |",
		"| agent container CPU avg | 40.0% | 65.0% | +62.5% ⚠️ |", // medians: 40 and (60+70)/2
		"| lost / missing | 0 | 5 |",                              // median of 10 and 0
		"7.99 (datadog/agent-dev:x@0123456789ab)",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("compare missing %q:\n%s", want, md)
		}
	}
	rows := Headline(cols)
	if len(rows) != len(headlineMetrics) {
		t.Fatalf("headline rows = %d", len(rows))
	}
	if rows[0].Metric != "delivery ratio" || rows[0].Cells[0] != "100.00%" || rows[0].Cells[1] != "99.50%" || rows[0].Deltas[0] != "-0.5% ⚠️" {
		t.Errorf("headline row 0: %+v", rows[0])
	}
}

func TestMedian(t *testing.T) {
	for _, c := range []struct {
		in   []float64
		want float64
	}{{nil, 0}, {[]float64{3}, 3}, {[]float64{3, 1}, 2}, {[]float64{5, 1, 3}, 3}, {[]float64{4, 1, 3, 2}, 2.5}} {
		if got := Median(append([]float64(nil), c.in...)); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
