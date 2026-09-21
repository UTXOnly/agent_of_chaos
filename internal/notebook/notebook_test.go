package notebook

import (
	"encoding/json"
	"fmt"
	"reflect"
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

// cellKinds lists every cell as "<type>: <title or first markdown line>",
// which is what an A/B notebook's structure comes down to.
func cellKinds(nb *Notebook) []string {
	var out []string
	for _, c := range nb.Cells {
		def := c["attributes"].(map[string]any)["definition"].(map[string]any)
		switch def["type"] {
		case "markdown":
			out = append(out, "markdown: "+strings.SplitN(def["text"].(string), "\n", 2)[0])
		case "iframe":
			out = append(out, "iframe")
		default:
			out = append(out, fmt.Sprintf("%v: %v", def["type"], def["title"]))
		}
	}
	return out
}

func TestForAB(t *testing.T) {
	a := sample("exp-a") // 22:13:20 → 22:16:20
	b := sample("exp-b")
	b.WindowStart, b.WindowEnd = a.WindowStart.Add(5*time.Minute), a.WindowEnd.Add(5*time.Minute)
	cols := []report.Column{{Name: "release", Runs: []*report.Report{a}}, {Name: "candidate", Runs: []*report.Report{b}}}
	nb := ForAB("aoc A/B exp", cols, Options{Experiment: "exp", AppURL: "https://x.datadoghq.com"})
	// Without findings: the header, the flame-graph intro and the whole
	// agent's CPU, then the four signals.
	want := []string{
		"markdown: - **release** — agent 7.79.0",
		"markdown: ## Flame graphs",
		"iframe",
		"timeseries: Received vs generated (records/s)",
		"timeseries: Pipeline utilization by component",
		"timeseries: Core agent CPU (%)",
		"timeseries: Core agent memory (RSS, anon)",
	}
	if got := cellKinds(nb); !reflect.DeepEqual(got, want) {
		t.Fatalf("cells:\n got %q\nwant %q", got, want)
	}
	if nb.Name != "aoc A/B exp" {
		t.Errorf("name = %q", nb.Name)
	}
	// The window is the later run's, ±60 s.
	if !nb.Start.Equal(b.WindowStart.Add(-time.Minute)) || !nb.End.Equal(b.WindowEnd.Add(time.Minute)) {
		t.Errorf("window %s → %s", nb.Start, nb.End)
	}
	s := string(nb.Body())
	for _, want := range []string{
		`"query": "timeshift(avg:aoc.agent.process.cpu_percent{run:exp-a}, -300)"`, // a shifted onto b
		`"alias": "release"`, // the side name, nothing else
		`"query": "avg:aoc.agent.process.cpu_percent{run:exp-b}"`,                           // the anchor is not shifted
		`avg:aoc.agent.telemetry.logs_component_utilization.ratio{run:exp-b} by {run,name}`, // grouped: run joins the group-by
		`"type": "iframe"`, // json escapes & in the body: b is the main query, a the compare_query_A
		"/profiling/comparison?query=service%3Adatadog-agent+run%3Aexp-b\\u0026start=",
		"compare_query_A=service%3Adatadog-agent+run%3Aexp-a\\u0026compare_start_A=",
		"profile_type=cpu-time\\u0026viz=flame_graph",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("body missing %q", want)
		}
	}
	for _, unwanted := range []string{"## All metrics", "# aoc A/B exp", "### Notes", "Timeline:", "heap-live-size", "docker stats via intake"} {
		if strings.Contains(s, unwanted) {
			t.Errorf("body should not contain %q", unwanted)
		}
	}
	var parsed map[string]any
	if err := json.Unmarshal(nb.Body(), &parsed); err != nil {
		t.Fatal(err)
	}

	// With findings: the brief leads (tested/differed, the conclusion, one
	// cell per regressed topic, profiles and code), then the flame graphs
	// (no movers without profiles) and the same four signals.
	b.Resources.ContainerMemMax, a.Resources.ContainerMemMax = 600e6, 200e6
	b.Resources.ContainerAnonMax, a.Resources.ContainerAnonMax = 580e6, 180e6
	res := findings.Build(findings.Input{Experiment: "exp", Cols: cols, Threshold: 10, Conclusion: "It is the python runner."})
	nb = ForAB("aoc A/B exp", cols, Options{Experiment: "exp", Findings: res})
	want = []string{
		"markdown: ## What we tested",
		"markdown: ## Conclusion",
		"markdown: ## Where",
		"markdown: ## Profiles",
		"markdown: ## Flame graphs",
		"iframe",
		"timeseries: Received vs generated (records/s)",
		"timeseries: Pipeline utilization by component",
		"timeseries: Core agent CPU (%)",
		"timeseries: Core agent memory (RSS, anon)",
	}
	if got := cellKinds(nb); !reflect.DeepEqual(got, want) {
		t.Fatalf("cells with one regression topic:\n got %q\nwant %q", got, want)
	}
	s = string(nb.Body())
	for _, want := range []string{"## What we tested", "## What differed (threshold ±10%)", "It is the python runner.", "### Memory", "Where the memory is",
		"agent container mem max 200.0 MB → 600.0 MB", `timeshift(avg:aoc.agent.container.memory_anon_bytes{run:exp-a}, -300)`} {
		if !strings.Contains(s, want) {
			t.Errorf("findings notebook missing %q", want)
		}
	}
	if strings.Contains(s, "Latency, written") {
		t.Error("charts unrelated to the four signals should be left out")
	}

	// The faults chart is only there when a run injected one.
	b.Faults = []report.FaultEvent{{At: b.WindowStart, Desc: "drop 10% of connections"}}
	nb = ForAB("aoc A/B exp", cols, Options{Experiment: "exp", Findings: res})
	if got := cellKinds(nb); !reflect.DeepEqual(got, append(want, "timeseries: Injected faults")) {
		t.Errorf("faults chart: got %q", got)
	}
}
