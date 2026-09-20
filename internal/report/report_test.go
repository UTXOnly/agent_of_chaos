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
