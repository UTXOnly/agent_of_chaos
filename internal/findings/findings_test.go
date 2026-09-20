package findings

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/pprof/profile"

	"github.com/UTXOnly/agent_of_chaos/internal/prof"
	"github.com/UTXOnly/agent_of_chaos/internal/report"
)

func sample(name string, memMax, anonMax int64, cpu float64) *report.Report {
	r := &report.Report{Schema: report.Schema, Name: name, WindowStart: time.Unix(1_700_000_000, 0), WindowEnd: time.Unix(1_700_000_600, 0), Seconds: 600}
	r.Agent.Versions = map[string]int64{"7.83.2": 10}
	r.Agent.Image = "datadog/agent:7"
	r.Generators = []report.Generator{{Name: "plain", ActiveStreams: 8}, {Name: "json", ActiveStreams: 8}}
	r.Delivery = report.Delivery{GeneratedRecords: 1000, ReceivedLogs: 1000, Unique: 1000, Ratio: 1, AllFinal: true}
	r.Throughput = report.Throughput{GenRecordsPerSec: 9900, RecvLogsPerSec: 1000, RecvWireBytesPerSec: 200e3, CompressionRatio: 20}
	r.Latency.EndToEnd = report.Quantiles{Count: 10, P50: 0.7, P99: 1.9, Max: 2}
	r.Resources = report.Resources{ContainerMemMax: memMax, ContainerMemAvg: memMax - 10e6, ContainerAnonMax: anonMax, ContainerAnonAvg: anonMax - 5e6, ContainerFileMax: memMax - anonMax, ContainerCPUAvg: cpu, ProcessCPUAvg: cpu - 2, ProcessRSSMax: 220e6, CPUSecondsPerMLogs: 16,
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
	b.Throughput.CompressionRatio = 17
	b.AgentLog = &report.LogSummary{Lines: 100, Errors: 3, Warnings: 2, Top: []report.NameCount{{Name: "ERROR | CORE | boom #", Count: 3}, {Name: "WARN | CORE | something #", Count: 2}}}
	cols := []report.Column{{Name: "release", Runs: []*report.Report{a}}, {Name: "dev", Runs: []*report.Report{b}}}
	in := Input{Experiment: "x", Cols: cols, Threshold: 10, AppURL: "https://x.datadoghq.com", ResultsDir: "results/x",
		Workload: "baseline — steady mix", ConfigDiff: []string{"only `dev` has DD_X=1"}}
	res := Build(in)

	kinds := map[string]string{}
	for _, f := range res.Findings {
		kinds[f.Metric] = f.Kind
	}
	for metric, want := range map[string]string{
		"agent container mem max":      "regression",
		"agent container anon mem max": "regression",
		"agent log errors":             "regression", // 0 → 3
		"compression ratio":            "regression",
		"tags per log":                 "improvement",
		"tag bytes per log":            "improvement",
	} {
		if kinds[metric] != want {
			t.Errorf("%s: kind %q, want %q", metric, kinds[metric], want)
		}
	}
	if _, ok := kinds["agent container CPU avg"]; ok { // +0.6 %, under the threshold
		t.Error("CPU should not be a finding")
	}
	if _, ok := kinds["agent log warnings"]; ok {
		t.Error("equal warnings should not be a finding")
	}
	// Sections: one per topic with a regression, memory first; no section
	// for improvements.
	var topics []string
	for _, s := range res.Sections {
		topics = append(topics, s.Topic)
	}
	if strings.Join(topics, ",") != "memory,stability,bytes" {
		t.Errorf("sections = %v", topics)
	}
	mem := res.Sections[0]
	if !strings.HasPrefix(mem.Reading, "Process memory grew (anon 200.0 MB → 590.0 MB) while the core agent's RSS did not") {
		t.Errorf("memory reading: %s", mem.Reading)
	}
	if len(mem.Evidence) != 2 { // memory table + processes; the heap diff only when core memory rose
		t.Errorf("memory evidence: %d", len(mem.Evidence))
	}
	if !strings.Contains(mem.Evidence[1].Markdown, "`python3` | – | 350.0 MB | +350.0 MB (new)") || strings.Contains(mem.Evidence[1].Markdown, "`trace-agent`") {
		t.Errorf("process table should list the core agent and what moved only:\n%s", mem.Evidence[1].Markdown)
	}
	if !strings.Contains(res.Sections[2].Reading, "The compression ratio fell because the bytes removed") {
		t.Errorf("bytes reading: %s", res.Sections[2].Reading)
	}
	// Changed rows: moved ≥ 5 % or appeared; equal rows are left out.
	var changed []string
	for _, r := range res.Changed {
		changed = append(changed, r.Metric)
	}
	for _, want := range []string{"agent container mem max", "compression ratio", "agent log errors", "tags per log"} {
		if !contains(changed, want) {
			t.Errorf("changed rows missing %q: %v", want, changed)
		}
	}
	if contains(changed, "delivery ratio") || contains(changed, "e2e latency p50") {
		t.Errorf("unchanged rows should not be listed: %v", changed)
	}
	if !strings.Contains(res.Tested, "**release** = 7.83.2 (datadog/agent:7)") || !strings.Contains(res.Tested, "Only `dev` has DD_X=1") || !strings.Contains(res.Tested, "Workload baseline — steady mix — 16 streams at 9,900/s, 10m00s window, 1 round(s) per side") {
		t.Errorf("tested: %s", res.Tested)
	}

	md := Markdown(res, in)
	for _, want := range []string{
		"## What we tested", "## What differed (threshold ±10%)", "⚠️ 4 regression(s)", "| agent container mem max | 240.0 MB | 626.0 MB | +160.8% ⚠️ |",
		"## Where", "### Memory — agent container anon mem max 200.0 MB → 590.0 MB (+195.0% ⚠️); agent container mem max",
		"**Process memory grew", "*Processes that moved (docker top)*", "### Stability — agent log errors 0 → 3 (+3 ⚠️)", "ERROR \\| CORE \\| boom #",
		"### Bytes — compression ratio", "## Profiles", "No profiler uploads were captured", "aoc conclude --results results/x",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n%s", want, md)
		}
	}
	for _, unwanted := range []string{"## Code", "## Headline", "## Improvements", "WARN \\| CORE \\| something"} {
		if strings.Contains(md, unwanted) {
			t.Errorf("markdown should not contain %q", unwanted)
		}
	}
	// A counter that appeared (0 → 3) must still serialise: no ±Inf in the JSON.
	if _, err := json.Marshal(res); err != nil {
		t.Errorf("findings.json: %v", err)
	}
	ev := EventText(res, in, "https://x/notebook/1")
	if !strings.HasPrefix(ev, "%%% ") || !strings.HasSuffix(ev, "%%%") || !strings.Contains(ev, "Memory: Process memory grew") || len(ev) > 4000 {
		t.Errorf("event text: %s", ev)
	}
	if res.Summary != "4 regression(s) in memory, stability, bytes; 2 improvement(s)" {
		t.Errorf("summary: %s", res.Summary)
	}

	// With a conclusion, it leads.
	in.Conclusion = "The extra memory is a new python3 check runner."
	md = Markdown(Build(in), in)
	if i, j := strings.Index(md, "## Conclusion\n\nThe extra memory"), strings.Index(md, "## What differed"); i < 0 || j < i {
		t.Errorf("conclusion should come before the differences")
	}
}

func TestNoFindings(t *testing.T) {
	a, b := sample("a", 240e6, 200e6, 16), sample("b", 245e6, 204e6, 16.5)
	cols := []report.Column{{Name: "a", Runs: []*report.Report{a}}, {Name: "b", Runs: []*report.Report{b}}}
	res := Build(Input{Experiment: "x", Cols: cols})
	if len(res.Findings) != 0 || len(res.Sections) != 0 || !strings.HasPrefix(res.Verdict[0], "✅ no regression") || res.Summary != "no change beyond the threshold" {
		t.Errorf("%+v %v %s", res.Findings, res.Verdict, res.Summary)
	}
}

// writeCPU writes a capture with one cpu.pprof whose leaf functions have the
// given nanoseconds and source positions.
func writeCPU(t *testing.T, root string, start time.Time, fns map[string][3]any, tags string) {
	t.Helper()
	dir := filepath.Join(root, "profiles", "datadog-agent", start.UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "event.json"), []byte(`{"start":"`+start.UTC().Format(time.RFC3339Nano)+`","end":"`+start.Add(time.Minute).UTC().Format(time.RFC3339Nano)+`","family":"go","tags_profiler":"service:datadog-agent,`+tags+`","attachments":["cpu.pprof"]}`), 0o644)
	p := &profile.Profile{DurationNanos: int64(time.Minute), SampleType: []*profile.ValueType{{Type: "samples", Unit: "count"}, {Type: "cpu", Unit: "nanoseconds"}}}
	id := uint64(1)
	for name, v := range fns {
		f := &profile.Function{ID: id, Name: name, Filename: v[1].(string)}
		id++
		loc := &profile.Location{ID: id, Line: []profile.Line{{Function: f, Line: int64(v[2].(int))}}}
		id++
		p.Function = append(p.Function, f)
		p.Location = append(p.Location, loc)
		ns := v[0].(int64)
		p.Sample = append(p.Sample, &profile.Sample{Location: []*profile.Location{loc}, Value: []int64{ns / 10_000_000, ns}})
	}
	var buf bytes.Buffer
	if err := p.Write(&buf); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "cpu.pprof"), buf.Bytes(), 0o644)
}

func TestCodeSection(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	// A repository with two commits: the file behind the mover only exists
	// in the second one.
	src := t.TempDir()
	run := func(args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", src}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	os.MkdirAll(filepath.Join(src, "pkg", "logs", "sender"), 0o755)
	os.WriteFile(filepath.Join(src, "pkg", "logs", "sender", "sender.go"), []byte("package sender\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "a")
	shaA := run("rev-parse", "HEAD")
	os.MkdirAll(filepath.Join(src, "comp", "tagfilter"), 0o755)
	os.WriteFile(filepath.Join(src, "comp", "tagfilter", "tagfilter.go"), []byte("package tagfilter\n\nfunc Keep() {}\n"), 0o644)
	os.WriteFile(filepath.Join(src, "pkg", "logs", "sender", "sender.go"), []byte("package sender\n\n// changed\n"), 0o644)
	run("add", ".")
	run("commit", "-q", "-m", "b")
	shaB := run("rev-parse", "HEAD")

	t0 := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	dirA, dirB := t.TempDir(), t.TempDir()
	const mod = "/go/src/github.com/DataDog/datadog-agent/"
	writeCPU(t, dirA, t0, map[string][3]any{
		"github.com/DataDog/datadog-agent/pkg/logs/sender.(*Sender).run": {int64(6e9), mod + "pkg/logs/sender/sender.go", 10},
		"runtime.mallocgc": {int64(1e9), "/usr/local/go/src/runtime/malloc.go", 900},
	}, "git.commit.sha:"+shaA+",git.repository_url:https://github.com/DataDog/datadog-agent")
	writeCPU(t, dirB, t0.Add(5*time.Minute), map[string][3]any{
		"github.com/DataDog/datadog-agent/pkg/logs/sender.(*Sender).run": {int64(10e9), mod + "pkg/logs/sender/sender.go", 10}, // +6.7 % of a core: the largest mover
		"github.com/DataDog/datadog-agent/comp/tagfilter.(*Scoped).Keep": {int64(3e9), mod + "comp/tagfilter/tagfilter.go", 3},
		"runtime.mallocgc": {int64(1e9), "/usr/local/go/src/runtime/malloc.go", 900},
	}, "git.commit.sha:"+shaB+",git.repository_url:https://github.com/DataDog/datadog-agent")
	capsA, _ := prof.LoadDir(dirA)
	capsB, _ := prof.LoadDir(dirB)

	a, b := sample("x-a", 240e6, 200e6, 16), sample("x-b", 240e6, 200e6, 22) // CPU +37 %
	a.Agent.Commit, a.Agent.Repo = shaA, "https://github.com/DataDog/datadog-agent"
	b.Agent.Commit, b.Agent.Repo = shaB, "https://github.com/DataDog/datadog-agent"
	b.Resources.ProcessCPUAvg = 20
	cols := []report.Column{{Name: "a", Runs: []*report.Report{a}}, {Name: "b", Runs: []*report.Report{b}}}
	in := Input{Experiment: "x", Cols: cols, Captures: [][]prof.Capture{capsA, capsB}, Threshold: 10, Source: src, AppURL: "https://x.datadoghq.com"}
	res := Build(in)

	if len(res.Sections) == 0 || res.Sections[0].Topic != "cpu" {
		t.Fatalf("sections: %+v", res.Sections)
	}
	if !strings.HasPrefix(res.Sections[0].Reading, "The largest CPU mover is `sender.(*Sender).run` (`pkg/logs/sender/sender.go:10`, +2/−0): 10.0% → 16.7% of one core.") {
		t.Errorf("cpu reading: %s", res.Sections[0].Reading)
	}
	var byFn = map[string]CodeRow{}
	for _, r := range res.Code {
		byFn[r.Function] = r
	}
	keep, ok := byFn["tagfilter.(*Scoped).Keep"]
	if !ok || keep.File != "comp/tagfilter/tagfilter.go" || keep.Line != 3 || keep.Where != "repo" || keep.Change != "new in b" || keep.URL != "https://github.com/DataDog/datadog-agent/blob/"+shaB+"/comp/tagfilter/tagfilter.go#L3" {
		t.Errorf("keep row: %+v", keep)
	}
	if snd := byFn["sender.(*Sender).run"]; snd.Change != "+2/−0" || snd.Delta != "+6.7%" {
		t.Errorf("sender row: %+v", snd)
	}
	if _, ok := byFn["runtime.mallocgc"]; ok {
		t.Error("stdlib functions do not belong in the code section")
	}
	if len(res.Code) != 2 {
		t.Errorf("code rows: %+v", res.Code)
	}
	if !strings.Contains(res.CodeNote, "a at `"+shaA[:8]+"`, b at `"+shaB[:8]+"`") {
		t.Errorf("code note: %s", res.CodeNote)
	}
	md := Markdown(res, in)
	for _, want := range []string{"## Code", "| `tagfilter.(*Scoped).Keep` | CPU | +5.0% | [comp/tagfilter/tagfilter.go:3](", "| new in b |", "std `runtime/malloc.go:900`", "*Core agent CPU, by function (profiler; % of one core)*"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n%s", want, md)
		}
	}
	// Without a checkout the rows still locate the code, without change status.
	in.Source = ""
	res = Build(in)
	if r := res.Code[0]; r.Change != "" || !strings.Contains(res.CodeNote, "No checkout configured") {
		t.Errorf("no-source: %+v %s", r, res.CodeNote)
	}
}
