package notebook

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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
