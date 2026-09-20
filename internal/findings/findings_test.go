package findings

import (
	"strings"
	"testing"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

func sample(name string, memMax, anonMax int64, cpu float64) *report.Report {
	r := &report.Report{Schema: report.Schema, Name: name, WindowStart: time.Unix(1_700_000_000, 0), WindowEnd: time.Unix(1_700_000_600, 0), Seconds: 600}
	r.Agent.Versions = map[string]int64{"7.83.2": 10}
	r.Agent.Image = "datadog/agent:7"
	r.Delivery = report.Delivery{GeneratedRecords: 1000, ReceivedLogs: 1000, Unique: 1000, Ratio: 1, AllFinal: true}
	r.Throughput = report.Throughput{RecvLogsPerSec: 1000, RecvWireBytesPerSec: 200e3, CompressionRatio: 20}
	r.Latency.EndToEnd = report.Quantiles{Count: 10, P50: 0.7, P99: 1.9, Max: 2}
	r.Resources = report.Resources{ContainerMemMax: memMax, ContainerMemAvg: memMax - 10e6, ContainerAnonMax: anonMax, ContainerFileMax: memMax - anonMax, ContainerCPUAvg: cpu, ProcessCPUAvg: cpu - 2, ProcessRSSMax: 220e6, CPUSecondsPerMLogs: 16,
		Processes: []report.ProcessStat{{Name: "agent", RSSMax: 220e6, CPUAvg: cpu - 2, Samples: 10}, {Name: "trace-agent", RSSMax: 40e6, CPUAvg: 1, Samples: 10}}}
	r.Tags = report.Tags{AvgTagsPerLog: 4, AvgTagBytesPerLog: 120, Keys: []report.NameCount{{Name: "env", Count: 1000}, {Name: "service", Count: 1000}}}
	r.Telemetry = []report.Telemetry{{Name: "go_goroutines", Type: "gauge", Last: 300}, {Name: "go_memstats_heap_inuse_bytes", Type: "gauge", Last: 150e6}, {Name: "logs_component_utilization__ratio", Labels: `{name="sender"}`, Type: "gauge", Last: 0.4}}
	r.HTTP.ByStatus = map[string]int64{"202": 500}
	r.AgentLog = &report.LogSummary{Lines: 100, Warnings: 2, Top: []report.NameCount{{Name: "WARN | CORE | something #", Count: 2}}}
	return r
}

func TestBuildAndMarkdown(t *testing.T) {
	a := sample("x-release", 240e6, 200e6, 16)
	b := sample("x-dev", 626e6, 590e6, 16.1)
	b.Resources.Processes = append(b.Resources.Processes, report.ProcessStat{Name: "python3", RSSMax: 350e6, CPUAvg: 3, Samples: 10})
	b.Tags = report.Tags{AvgTagsPerLog: 2, AvgTagBytesPerLog: 40, Keys: []report.NameCount{{Name: "service", Count: 1000}}}
	b.AgentLog = &report.LogSummary{Lines: 100, Errors: 3, Warnings: 2, Top: []report.NameCount{{Name: "ERROR | CORE | boom #", Count: 3}}}
	cols := []report.Column{{Name: "release", Runs: []*report.Report{a}}, {Name: "dev", Runs: []*report.Report{b}}}
	in := Input{Experiment: "x", Cols: cols, Threshold: 10, AppURL: "https://x.datadoghq.com", ResultsDir: "results/x"}
	res := Build(in)

	kinds := map[string]string{}
	topics := map[string]string{}
	for _, f := range res.Findings {
		kinds[f.Metric] = f.Kind
		topics[f.Metric] = f.Topic
	}
	for metric, want := range map[string]string{
		"agent container mem max":      "regression",
		"agent container anon mem max": "regression",
		"agent log errors":             "regression", // 0 → 3
		"tags per log":                 "improvement",
		"tag bytes per log":            "improvement",
	} {
		if kinds[metric] != want {
			t.Errorf("%s: kind %q, want %q (findings %+v)", metric, kinds[metric], want, res.Findings)
		}
	}
	if _, ok := kinds["agent container CPU avg"]; ok { // +0.6 %, under the threshold
		t.Error("CPU should not be a finding")
	}
	if _, ok := kinds["agent log warnings"]; ok { // 2 on both sides, equal: no finding
		t.Error("equal warnings should not be a finding")
	}
	if res.Findings[0].Kind != "regression" || topics[res.Findings[0].Metric] != "memory" {
		t.Errorf("regressions first, memory before stability: %+v", res.Findings[0])
	}
	if len(res.Evidence["memory"]) < 2 || len(res.Evidence["tags"]) == 0 || len(res.Evidence["cpu"]) != 0 { // evidence for every topic with a finding
		t.Errorf("evidence topics: %v", keysOf(res.Evidence))
	}
	md := Markdown(res, in)
	for _, want := range []string{
		"## Verdict", "⚠️ 3 regression(s)", "## Regressions", "### Memory — agent container anon mem max 200.0 MB → 590.0 MB (+195.0% ⚠️); agent container mem max 240.0 MB → 626.0 MB (+160.8% ⚠️)",
		"**Where the memory is**", "├ anon: the processes' own memory (max)", "`python3` | – | – | 350.0 MB | 3.0% | +350.0 MB (new)",
		"**Reading:** anon grew (200.0 MB → 590.0 MB) while the core agent's RSS did not (220.0 MB → 220.0 MB): the memory is in another process",
		"### Stability", "ERROR \\| CORE \\| boom #", "## Improvements", "### Tags", "## Headline", "explore_profiling_flame_graph",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n%s", want, md)
		}
	}
	if strings.Contains(md, "## Profiles (core agent") {
		t.Error("no captures: profiles section should say so")
	}
	ev := EventText(res, in, "https://x/notebook/1")
	if !strings.HasPrefix(ev, "%%% ") || !strings.HasSuffix(ev, "%%%") || !strings.Contains(ev, "| agent container mem max |") || len(ev) > 4000 {
		t.Errorf("event text: %s", ev)
	}
	if !strings.Contains(res.Summary, "2 regression(s) in memory, stability") && !strings.Contains(res.Summary, "3 regression(s) in memory, stability") {
		t.Errorf("summary: %s", res.Summary)
	}
}

func keysOf(m map[string][]Evidence) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestNoFindings(t *testing.T) {
	a, b := sample("a", 240e6, 200e6, 16), sample("b", 245e6, 204e6, 16.5)
	cols := []report.Column{{Name: "a", Runs: []*report.Report{a}}, {Name: "b", Runs: []*report.Report{b}}}
	res := Build(Input{Experiment: "x", Cols: cols})
	if len(res.Findings) != 0 || !strings.HasPrefix(res.Verdict[0], "✅ no regression") || res.Summary != "no change beyond the threshold" {
		t.Errorf("%+v %v %s", res.Findings, res.Verdict, res.Summary)
	}
}
