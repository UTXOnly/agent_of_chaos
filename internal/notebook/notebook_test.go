package notebook

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/findings"
	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

func sample(name string) *report.Report {
	r := &report.Report{Schema: report.Schema, Name: name, WindowStart: time.Unix(1_700_000_000, 0), WindowEnd: time.Unix(1_700_000_180, 0), Seconds: 180}
	r.Agent.Versions = map[string]int64{"7.79.0": 10}
	r.Agent.Container = "aoc-agent"
	r.Agent.Hostname = "aoc-harness"
	r.Delivery = report.Delivery{GeneratedRecords: 100, Unique: 100, Ratio: 1, AllFinal: true}
	return r
}

func TestForRun(t *testing.T) {
	nb := ForRun(sample("baseline"), Options{Site: "datadoghq.com", AppURL: "https://bhartford.datadoghq.com"})
	if len(nb.Cells) != len(runCharts)+2 {
		t.Fatalf("cells = %d", len(nb.Cells))
	}
	body := nb.Body()
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatal(err)
	}
	attrs := parsed["data"].(map[string]any)["attributes"].(map[string]any)
	if attrs["name"] != "aoc run: baseline · agent 7.79.0" || attrs["status"] != "published" {
		t.Errorf("attrs: %v %v", attrs["name"], attrs["status"])
	}
	s := string(body)
	for _, want := range []string{"run:baseline", "aoc.intake.logs_per_sec{run:baseline}", "docker.cpu.usage{run:baseline,container_name:aoc-agent}", `"time": {`, "https://bhartford.datadoghq.com/profiling/explorer?query=service%3Adatadog-agent+run%3Abaseline"} {
		if !strings.Contains(s, want) {
			t.Errorf("body missing %q", want)
		}
	}
	if !strings.Contains(s, `"start": "2023-11-14T22:12:20Z"`) { // window start - 60s
		t.Errorf("window start not applied: %s", s[:400])
	}
}

func TestForCompare(t *testing.T) {
	nb := ForCompare([]*report.Report{sample("a"), sample("b")}, Options{})
	if len(nb.Cells) != 1+2*(1+len(keyCharts))+1 {
		t.Fatalf("cells = %d", len(nb.Cells))
	}
	s := string(nb.Body())
	if !strings.Contains(s, "aoc compare: a vs b") || !strings.Contains(s, "{run:b}") {
		t.Errorf("compare body: %s", s[:300])
	}
	// per-run cells carry their own absolute time
	if strings.Count(s, `"live": false`) < 1+2*len(keyCharts) {
		t.Errorf("per-cell absolute time missing")
	}
}

func TestForAB(t *testing.T) {
	a := sample("exp-a") // 22:13:20 → 22:16:20
	b := sample("exp-b")
	b.WindowStart, b.WindowEnd = a.WindowStart.Add(5*time.Minute), a.WindowEnd.Add(5*time.Minute)
	cols := []report.Column{{Name: "release", Runs: []*report.Report{a}}, {Name: "candidate", Runs: []*report.Report{b}}}
	nb := ForAB("aoc A/B exp", cols, Options{Experiment: "exp", AppURL: "https://x.datadoghq.com"})
	// Without findings: header, two embedded profilers, timeline, the two
	// default charts (CPU, memory), notes.
	if nb.Name != "aoc A/B exp" || len(nb.Cells) != 1+2+1+2+1 {
		t.Fatalf("name=%q cells=%d", nb.Name, len(nb.Cells))
	}
	// The window is the later run's, ±60 s.
	if !nb.Start.Equal(b.WindowStart.Add(-time.Minute)) || !nb.End.Equal(b.WindowEnd.Add(time.Minute)) {
		t.Errorf("window %s → %s", nb.Start, nb.End)
	}
	s := string(nb.Body())
	for _, want := range []string{
		`"query": "timeshift(avg:aoc.agent.container.cpu_percent{run:exp-a}, -300)"`, // a shifted onto b
		`"alias": "release: container (docker stats via intake)"`,
		`"query": "avg:aoc.agent.container.cpu_percent{run:exp-b}"`, // the anchor is not shifted
		`sum:aoc.intake.logs_per_sec{experiment:exp} by {variant}`,  // the real-time timeline
		`"type": "iframe"`, "https://x.datadoghq.com/profiling/explorer?query=service%3Adatadog-agent+run%3Aexp-b",
		"source%3Aaoc+run%3Aexp-b",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("body missing %q", want)
		}
	}
	for _, unwanted := range []string{"## All metrics", "# aoc A/B exp", "logs_component_utilization"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("body should not contain %q", unwanted)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal(nb.Body(), &parsed); err != nil {
		t.Fatal(err)
	}

	// With findings: tested/differed header, the conclusion, one cell per
	// regressed topic, profiles+code, the iframes, the timeline, the
	// topic's charts, notes.
	b.Resources.ContainerMemMax, a.Resources.ContainerMemMax = 600e6, 200e6
	b.Resources.ContainerAnonMax, a.Resources.ContainerAnonMax = 580e6, 180e6
	res := findings.Build(findings.Input{Experiment: "exp", Cols: cols, Threshold: 10, Conclusion: "It is the python runner."})
	nb = ForAB("aoc A/B exp", cols, Options{Experiment: "exp", Findings: res})
	if len(nb.Cells) != 1+1+1+1+2+1+3+1 {
		t.Fatalf("cells with one regression topic = %d", len(nb.Cells))
	}
	s = string(nb.Body())
	for _, want := range []string{"## What we tested", "## What differed (threshold ±10%)", "## Conclusion", "It is the python runner.", "## Where", "### Memory", "Where the memory is",
		"agent container mem max 200.0 MB → 600.0 MB", "## Profiles", `timeshift(avg:aoc.agent.proc.rss_bytes{run:exp-a} by {run,proc}, -300)`, "Agent container memory: anon (processes) vs file cache"} {
		if !strings.Contains(s, want) {
			t.Errorf("findings notebook missing %q", want)
		}
	}
	second, _ := json.Marshal(nb.Cells[1])
	if !strings.Contains(string(second), "## Conclusion") {
		t.Error("the conclusion should come right after the header")
	}
	if strings.Contains(s, "Latency, written") {
		t.Error("charts unrelated to the regression should be left out")
	}
}
