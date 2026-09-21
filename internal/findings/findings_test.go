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

// sample is a run that delivered everything at the rate the generators
// offered: the shape every test starts from.
func sample(name string) *report.Report {
	r := &report.Report{Schema: report.Schema, Name: name, WindowStart: time.Unix(1_700_000_000, 0), WindowEnd: time.Unix(1_700_000_600, 0), Seconds: 600}
	r.Agent.Versions = map[string]int64{"7.83.2": 10}
	r.Agent.Image = "datadog/agent:7"
	r.Generators = []report.Generator{{Name: "plain", ActiveStreams: 8, TargetRate: 5000}, {Name: "json", ActiveStreams: 8, TargetRate: 5000}}
	r.Delivery = report.Delivery{GeneratedRecords: 6_000_000, ReceivedLogs: 6_000_000, Unique: 6_000_000, Ratio: 1, AllFinal: true}
	r.Throughput = report.Throughput{GenRecordsPerSec: 10000, RecvLogsPerSec: 10000, RecvWireBytesPerSec: 250e3, CompressionRatio: 20}
	r.Latency.EndToEnd = report.Quantiles{Count: 10, P50: 0.7, P99: 1.9, Max: 2}
	r.Resources = report.Resources{ContainerMemMax: 560e6, ContainerMemAvg: 520e6, ContainerAnonMax: 200e6, ContainerAnonAvg: 160e6, ContainerFileMax: 300e6,
		ContainerCPUAvg: 16, ProcessCPUAvg: 14, ProcessRSSMax: 250e6, CPUSecondsPerMLogs: 16,
		Processes: []report.ProcessStat{{Name: "agent", RSSMax: 244e6, CPUAvg: 14, Samples: 10}, {Name: "trace-agent", RSSMax: 40e6, CPUAvg: 1, Samples: 10}}}
	r.Tags = report.Tags{AvgTagsPerLog: 4, AvgTagBytesPerLog: 120, Keys: []report.NameCount{{Name: "env", Count: 1000}}}
	r.Telemetry = []report.Telemetry{
		{Name: "go_memstats_heap_inuse_bytes", Type: "gauge", Last: 86e6},
		{Name: "go_goroutines", Type: "gauge", Last: 600},
		{Name: "logs_component_utilization__ratio", Labels: `{name="processor"}`, Type: "gauge", Last: 0.2, Mean: 0.2},
		{Name: "logs_component_utilization__ratio", Labels: `{name="strategy"}`, Type: "gauge", Last: 0.3, Mean: 0.3},
	}
	r.HTTP.ByStatus = map[string]int64{"202": 500}
	r.AgentLog = &report.LogSummary{Lines: 100, Warnings: 2, Top: []report.NameCount{{Name: "WARN | CORE | something #", Count: 2}}}
	return r
}

func columns(a, b *report.Report) []report.Column {
	return []report.Column{{Name: "release", Runs: []*report.Report{a}}, {Name: "dev", Runs: []*report.Report{b}}}
}

func setHeap(r *report.Report, v float64) {
	for i := range r.Telemetry {
		if r.Telemetry[i].Name == "go_memstats_heap_inuse_bytes" {
			r.Telemetry[i].Last = v
		}
	}
}

func TestMemoryRegressionAndBrief(t *testing.T) {
	a, b := sample("x-release"), sample("x-dev")
	b.Resources.ProcessRSSMax = 320e6 // +28 %
	b.Resources.ContainerAnonAvg = 166e6
	b.Resources.ContainerMemMax, b.Resources.ContainerFileMax = 690e6, 410e6
	b.Resources.Processes = append(b.Resources.Processes, report.ProcessStat{Name: "python3", RSSMax: 70e6, CPUAvg: 3, Samples: 10})
	setHeap(b, 110e6) // +28 %
	cols := columns(a, b)
	in := Input{Experiment: "x", Cols: cols, Threshold: 10, AppURL: "https://x.datadoghq.com", ResultsDir: "results/x",
		Workload: "`baseline` (steady mix)", ConfigDiff: []string{"only `dev` has DD_X=1"}}
	res := Build(in)

	if !res.Gate.Pass || !strings.Contains(res.Gate.Line, "Delivery gate: pass") {
		t.Errorf("gate: %+v", res.Gate)
	}
	if res.Verdict[0] != "regression: memory" {
		t.Errorf("verdict: %v", res.Verdict)
	}
	if res.Summary != "regression in memory" {
		t.Errorf("summary: %s", res.Summary)
	}
	kinds := map[string]string{}
	for _, s := range res.Signals {
		kinds[s.Metric] = s.Kind
	}
	for metric, want := range map[string]string{
		"core agent RSS max":                       "regression",
		"core agent Go heap in use":                "regression",
		"agent container anon mem avg":             "flat",
		"core agent process CPU avg":               "flat",
		"received logs /s":                         "flat", // paced, and both sides kept up
		"pipeline utilization (busiest component)": "flat",
	} {
		if kinds[metric] != want {
			t.Errorf("%s: kind %q, want %q", metric, kinds[metric], want)
		}
	}
	var topics []string
	for _, s := range res.Sections {
		topics = append(topics, s.Topic)
	}
	if strings.Join(topics, ",") != "memory" {
		t.Errorf("sections = %v", topics)
	}
	mem := res.Sections[0]
	if mem.Reading != "The core agent's Go heap grew with its RSS: the heap-in-use table names the functions holding it." {
		t.Errorf("memory reading: %s", mem.Reading)
	}
	if len(mem.Evidence) != 2 { // the container's own accounting, the processes; no profiles were captured
		t.Errorf("memory evidence: %+v", mem.Evidence)
	}
	if !strings.Contains(mem.Evidence[1].Markdown, "`python3` | – | 70.0 MB | +70.0 MB (new)") || strings.Contains(mem.Evidence[1].Markdown, "`trace-agent`") {
		t.Errorf("process table should list the core agent and what moved only:\n%s", mem.Evidence[1].Markdown)
	}

	md := Markdown(res, in)
	for _, want := range []string{
		"# aoc A/B x\n\n**regression: memory**",
		"| memory | core agent RSS max | 250.0 MB | 320.0 MB | **+28.0%** | ±10% |",
		"| signal | metric | release | dev | Δ | threshold |",
		"Throughput: held by the generators at 10.0k/s and both sides kept up, so saturation carries the signal.",
		"Delivery gate: pass — nothing lost, duplicated, orphaned or truncated in dev. Delivery ratio 100.00% → 100.00%.",
		"## Tested\n\n| side | image | version | commit | only this side |",
		"| release | `datadog/agent:7` | 7.83.2 | – | – |",
		"| dev | `datadog/agent:7` | 7.83.2 | – | `DD_X=1` |",
		"Workload `baseline` (steady mix) — 16 streams at 10.0k/s, 10m00s window, 1 round per side.",
		"## Where\n\n### Memory\n\nThe core agent's Go heap grew",
		"Next: `explore_profiling_flame_graph`",
		"Every metric, the process table and the telemetry counters: `compare.md`.",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n%s", want, md)
		}
	}
	for _, unwanted := range []string{"⚠", "✅", "👀", "(s)", "**The core agent", "## Code", "If that is not enough"} {
		if strings.Contains(md, unwanted) {
			t.Errorf("markdown should not contain %q", unwanted)
		}
	}
	if n := len(md); n > 6000 {
		t.Errorf("brief is %d bytes; it should stay small", n)
	}
	if _, err := json.Marshal(res); err != nil {
		t.Errorf("findings.json: %v", err)
	}
	ev := EventText(res, in, "https://x/notebook/1")
	if !strings.HasPrefix(ev, "%%% ") || !strings.HasSuffix(ev, "%%%") || !strings.Contains(ev, "**regression: memory**") ||
		!strings.Contains(ev, "Delivery gate: pass") || !strings.Contains(ev, "notebook") || len(ev) > 4000 {
		t.Errorf("event text: %s", ev)
	}

	// With a conclusion, it follows the identity table.
	in.Conclusion = "The extra heap is the new python3 check runner."
	md = Markdown(Build(in), in)
	if i, j := strings.Index(md, "## Conclusion"), strings.Index(md, "## Where"); i < 0 || j < i {
		t.Error("the conclusion should come before the evidence")
	}
}

func TestGateFails(t *testing.T) {
	a, b := sample("x-release"), sample("x-dev")
	b.Delivery.Missing, b.Delivery.Unique, b.Delivery.Ratio = 1200, 5_998_800, 0.9998
	b.Delivery.Duplicates = 3
	a.Delivery.Duplicates = 2 // both sides duplicate: the harness
	cols := columns(a, b)
	in := Input{Experiment: "x", Cols: cols, Threshold: 10, ResultsDir: "results/x"}
	res := Build(in)

	if res.Gate.Pass {
		t.Fatal("the gate should fail when records are lost")
	}
	for _, want := range []string{"1,200 lost / missing in dev", "3 duplicates in dev (2 in release too — the workload or the harness, not the build)"} {
		if !strings.Contains(res.Gate.Line, want) {
			t.Errorf("gate line missing %q:\n%s", want, res.Gate.Line)
		}
	}
	if !strings.HasPrefix(res.Verdict[0], "fail: 1,200 lost / missing in dev") {
		t.Errorf("verdict: %v", res.Verdict)
	}
	if res.Sections[0].Topic != "delivery" {
		t.Errorf("sections: %+v", res.Sections)
	}
	if !strings.Contains(res.Sections[0].Reading, "did not arrive exactly once in dev") {
		t.Errorf("delivery reading: %s", res.Sections[0].Reading)
	}
	kinds := map[string]string{}
	for _, f := range res.Findings {
		kinds[f.Metric] = f.Kind
	}
	if kinds["lost / missing"] != "regression" || kinds["duplicates"] != "attention" {
		t.Errorf("findings: %+v", res.Findings)
	}
	md := Markdown(res, in)
	if !strings.Contains(md, "**fail: 1,200 lost / missing in dev") || !strings.Contains(md, "### Delivery") {
		t.Errorf("markdown: %s", md)
	}
	// Out of order is reported in the gate line without failing it.
	a2, b2 := sample("y-release"), sample("y-dev")
	a2.Delivery.OutOfOrder, b2.Delivery.OutOfOrder = 1086, 1147
	res2 := Build(Input{Experiment: "y", Cols: columns(a2, b2), Threshold: 10})
	if !res2.Gate.Pass || !strings.Contains(res2.Gate.Line, "out of order 1,086 → 1,147, which does not gate") {
		t.Errorf("out-of-order gate: %+v", res2.Gate)
	}
}

func TestPacedThroughput(t *testing.T) {
	// The generators hold the rate and both sides keep up: a row, not a
	// finding.
	a, b := sample("x-release"), sample("x-dev")
	b.Throughput.RecvLogsPerSec = 9950
	res := Build(Input{Experiment: "x", Cols: columns(a, b), Threshold: 10})
	var s Signal
	for _, x := range res.Signals {
		if x.Name == "throughput" {
			s = x
		}
	}
	if s.Kind != "flat" || !strings.Contains(s.Note, "held by the generators at 10.0k/s") {
		t.Errorf("paced throughput: %+v", s)
	}

	// b falls behind what its own generators wrote: a regression even
	// though the two sides' rates are within the threshold of each other.
	b.Throughput.RecvLogsPerSec = 8000
	res = Build(Input{Experiment: "x", Cols: columns(a, b), Threshold: 10})
	for _, x := range res.Signals {
		if x.Name == "throughput" {
			s = x
		}
	}
	if s.Kind != "regression" || !strings.Contains(s.Note, "dev delivered 20.0% fewer logs than its generators wrote") {
		t.Errorf("throughput shortfall: %+v", s)
	}
	if res.Verdict[0] != "regression: throughput" || res.Sections[0].Topic != "throughput" {
		t.Errorf("verdict %v, sections %+v", res.Verdict, res.Sections)
	}

	// Flat out (no target rate), the two sides are compared directly.
	a2, b2 := sample("y-release"), sample("y-dev")
	for i := range b2.Generators {
		a2.Generators[i].TargetRate, b2.Generators[i].TargetRate = 0, 0
	}
	b2.Throughput.RecvLogsPerSec, b2.Throughput.GenRecordsPerSec = 8000, 8000
	res = Build(Input{Experiment: "y", Cols: columns(a2, b2), Threshold: 10})
	for _, x := range res.Signals {
		if x.Name == "throughput" {
			s = x
		}
	}
	if s.Kind != "regression" || s.Note != "" {
		t.Errorf("unpaced throughput: %+v", s)
	}
}

func TestWatchAndPerSignalThresholds(t *testing.T) {
	a, b := sample("x-release"), sample("x-dev")
	b.Resources.ProcessCPUAvg = 20 // +43 %
	b.Throughput.CompressionRatio = 17
	in := Input{Experiment: "x", Cols: columns(a, b), Threshold: 10,
		Watch: []string{"compression ratio"}, Thresholds: map[string]float64{"cpu": 50}}
	res := Build(in)
	byMetric := map[string]Signal{}
	for _, s := range res.Signals {
		byMetric[s.Metric] = s
	}
	if s := byMetric["core agent process CPU avg"]; s.Kind != "flat" || s.Threshold != 50 {
		t.Errorf("cpu under its own threshold: %+v", s)
	}
	w := byMetric["compression ratio"]
	if w.Name != "watch" || w.Kind != "regression" {
		t.Errorf("watched metric: %+v", w)
	}
	if !strings.Contains(Markdown(res, in), "| watch | compression ratio | 20.00× | 17.00× |") {
		t.Error("the watch list belongs in the signals table")
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

func TestCodeSectionAndScoping(t *testing.T) {
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
		"runtime.memmove":  {int64(1e9), "/usr/local/go/src/runtime/memmove.go", 40},
	}, "git.commit.sha:"+shaA+",git.repository_url:https://github.com/DataDog/datadog-agent")
	writeCPU(t, dirB, t0.Add(5*time.Minute), map[string][3]any{
		"github.com/DataDog/datadog-agent/pkg/logs/sender.(*Sender).run": {int64(10e9), mod + "pkg/logs/sender/sender.go", 10}, // +6.7 % of a core
		"github.com/DataDog/datadog-agent/comp/tagfilter.(*Scoped).Keep": {int64(3e9), mod + "comp/tagfilter/tagfilter.go", 3}, // +5.0 %, new
		"runtime.mallocgc": {int64(9e9), "/usr/local/go/src/runtime/malloc.go", 900},   // +13.3 %: bigger than anything in scope
		"runtime.memmove":  {int64(1.1e9), "/usr/local/go/src/runtime/memmove.go", 40}, // +0.2 %: noise
	}, "git.commit.sha:"+shaB+",git.repository_url:https://github.com/DataDog/datadog-agent")
	capsA, _ := prof.LoadDir(dirA)
	capsB, _ := prof.LoadDir(dirB)

	a, b := sample("x-a"), sample("x-b")
	a.Agent.Commit, a.Agent.Repo = shaA, "https://github.com/DataDog/datadog-agent"
	b.Agent.Commit, b.Agent.Repo = shaB, "https://github.com/DataDog/datadog-agent"
	b.Resources.ProcessCPUAvg = 20 // +43 %
	cols := columns(a, b)
	in := Input{Experiment: "x", Cols: cols, Captures: [][]prof.Capture{capsA, capsB}, Threshold: 10, Source: src,
		Code: []string{"pkg/logs", "comp/tagfilter"}, AppURL: "https://x.datadoghq.com"}
	res := Build(in)

	if len(res.Sections) == 0 || res.Sections[0].Topic != "cpu" {
		t.Fatalf("sections: %+v", res.Sections)
	}
	if res.Sections[0].Reading != "The largest CPU mover is `sender.(*Sender).run` (`pkg/logs/sender/sender.go:10`, +2/−0)." {
		t.Errorf("cpu reading: %s", res.Sections[0].Reading)
	}
	byFn := map[string]CodeRow{}
	for _, r := range res.Code {
		byFn[r.Function] = r
	}
	keep, ok := byFn["tagfilter.(*Scoped).Keep"]
	if !ok || keep.File != "comp/tagfilter/tagfilter.go" || keep.Line != 3 || keep.Where != "repo" || !keep.Scope || keep.Change != "new in b" ||
		keep.URL != "https://github.com/DataDog/datadog-agent/blob/"+shaB+"/comp/tagfilter/tagfilter.go#L3" {
		t.Errorf("keep row: %+v", keep)
	}
	if snd := byFn["sender.(*Sender).run"]; snd.Change != "+2/−0" || snd.Delta != "+6.7%" {
		t.Errorf("sender row: %+v", snd)
	}
	if _, ok := byFn["runtime.mallocgc"]; ok {
		t.Error("functions outside the agent's module do not belong in the code section")
	}
	if len(res.Code) != 2 {
		t.Errorf("code rows: %+v", res.Code)
	}
	if !strings.Contains(res.CodeNote, "release at `"+shaA[:8]+"`, dev at `"+shaB[:8]+"`") {
		t.Errorf("code note: %s", res.CodeNote)
	}
	md := Markdown(res, in)
	for _, want := range []string{
		"## Code",
		"| `tagfilter.(*Scoped).Keep` | CPU | +5.0% | [comp/tagfilter/tagfilter.go:3](",
		"| new in b |",
		// the packages under test lead the table, then the bigger mover
		// elsewhere; the 0.2 % one is left out
		"| `sender.(*Sender).run` | `pkg/logs/sender/sender.go:10` | 10.0% | 16.7% | +6.7% |",
		"std `runtime/malloc.go:900`",
		"Side by side: [CPU](https://x.datadoghq.com/profiling/comparison?",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q\n%s", want, md)
		}
	}
	if strings.Contains(md, "memmove") {
		t.Error("movers below the noise floor should be left out")
	}
	if i, j := strings.Index(md, "sender.(*Sender).run"), strings.Index(md, "runtime.mallocgc"); i < 0 || j < i {
		t.Error("the code under test comes before the rest of the agent")
	}
	// The flame-graph filter follows the code under test, not the runtime.
	if next := strings.Join(res.Sections[0].Next, " "); !strings.Contains(next, `frameRegexFilter="sender\.\(\*Sender\)\.run"`) {
		t.Errorf("next: %s", next)
	}
	// Without a checkout the rows still locate the code, without change status.
	in.Source = ""
	res = Build(in)
	if r := res.Code[0]; r.Change != "" || !strings.Contains(res.CodeNote, "No checkout configured") {
		t.Errorf("no-source: %+v %s", r, res.CodeNote)
	}
}

func TestNoFindings(t *testing.T) {
	a, b := sample("a"), sample("b")
	b.Resources.ProcessRSSMax = 255e6 // +2 %
	res := Build(Input{Experiment: "x", Cols: columns(a, b)})
	if len(res.Findings) != 0 || len(res.Sections) != 0 || res.Verdict[0] != "pass" || res.Summary != "no change beyond the threshold" {
		t.Errorf("%+v %v %s", res.Findings, res.Verdict, res.Summary)
	}
	if len(res.Signals) != 7 { // throughput, saturation, cpu ×2, memory ×3
		t.Errorf("signals: %+v", res.Signals)
	}
}

func TestFlameFuncAndCompareURL(t *testing.T) {
	for in, want := range map[string]string{
		"github.com/DataDog/datadog-agent/comp/logs-library/tagfilter.(*Scoped).Keep": "(*Scoped).Keep",
		"gopkg.in/yaml.v3.(*parser).document":                                         "(*parser).document",
		"runtime.mallocgc":                                                            "mallocgc",
		"encoding/json.(*encodeState).marshal.func1":                                  "(*encodeState).marshal.func1",
	} {
		if got := FlameFunc(in); got != want {
			t.Errorf("FlameFunc(%q) = %q, want %q", in, got, want)
		}
	}
	a := &report.Report{Name: "x-a", WindowStart: time.Unix(1000, 0), WindowEnd: time.Unix(1600, 0)}
	b := &report.Report{Name: "x-b", WindowStart: time.Unix(2000, 0), WindowEnd: time.Unix(2600, 0)}
	u := CompareURL(a, b, "https://x.datadoghq.com", time.Minute, "cpu-time", "(*Scoped).Keep")
	for _, want := range []string{
		"https://x.datadoghq.com/profiling/comparison?query=service%3Adatadog-agent+run%3Ax-b&start=1940000&end=2660000",
		"&compare_query_A=service%3Adatadog-agent+run%3Ax-a&compare_start_A=940000&compare_end_A=1660000",
		"&profile_type=cpu-time&viz=flame_graph&paused=true&profiling-flame-graph__filter=focus_on%28function%3A%22%28%2AScoped%29.Keep%22%29",
	} {
		if !strings.Contains(u, want) {
			t.Errorf("CompareURL missing %q in\n%s", want, u)
		}
	}
}
