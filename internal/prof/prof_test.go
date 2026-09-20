package prof

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/pprof/profile"
	"github.com/klauspost/compress/zstd"
)

// synth writes a capture dir with a cpu profile (samples/count + cpu/ns)
// and a delta-heap profile (inuse_space) whose leaf frames are fns with the
// given values.
var zstdOut bool

func synth(t *testing.T, root, service string, start time.Time, period time.Duration, cpuNS map[string]int64, heap map[string]int64) {
	t.Helper()
	dir := filepath.Join(root, "profiles", service, start.UTC().Format("20060102T150405Z"))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ev := `{"start":"` + start.UTC().Format(time.RFC3339Nano) + `","end":"` + start.Add(period).UTC().Format(time.RFC3339Nano) + `","family":"go","tags_profiler":"service:` + service + `,run:x","attachments":["cpu.pprof","delta-heap.pprof"]}`
	os.WriteFile(filepath.Join(dir, "event.json"), []byte(ev), 0o644)
	write := func(name string, types [][2]string, vals map[string]int64, valueIndex int, nvals int, dur time.Duration) {
		p := &profile.Profile{DurationNanos: int64(dur)}
		for _, tp := range types {
			p.SampleType = append(p.SampleType, &profile.ValueType{Type: tp[0], Unit: tp[1]})
		}
		id := uint64(1)
		caller := &profile.Function{ID: id, Name: "main.run"}
		id++
		p.Function = append(p.Function, caller)
		callerLoc := &profile.Location{ID: id, Line: []profile.Line{{Function: caller}}}
		id++
		p.Location = append(p.Location, callerLoc)
		for fn, v := range vals {
			f := &profile.Function{ID: id, Name: fn}
			id++
			loc := &profile.Location{ID: id, Line: []profile.Line{{Function: f}}}
			id++
			p.Function = append(p.Function, f)
			p.Location = append(p.Location, loc)
			values := make([]int64, nvals)
			values[valueIndex] = v
			if nvals > 1 && valueIndex == 1 {
				values[0] = v / 10_000_000 // samples at 100 Hz
			}
			p.Sample = append(p.Sample, &profile.Sample{Location: []*profile.Location{loc, callerLoc}, Value: values})
		}
		var buf bytes.Buffer
		if err := p.Write(&buf); err != nil {
			t.Fatal(err)
		}
		if zstdOut { // as dd-trace-go v2 uploads them
			var raw bytes.Buffer
			p.WriteUncompressed(&raw)
			enc, _ := zstd.NewWriter(&buf)
			buf.Reset()
			enc.Write(raw.Bytes())
			enc.Close()
		}
		os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o644)
	}
	write("cpu.pprof", [][2]string{{"samples", "count"}, {"cpu", "nanoseconds"}}, cpuNS, 1, 2, period)
	write("delta-heap.pprof", [][2]string{{"alloc_objects", "count"}, {"alloc_space", "bytes"}, {"inuse_objects", "count"}, {"inuse_space", "bytes"}}, heap, 3, 4, period)
}

func TestAnalyzeAndCompare(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	t0 := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	// a: two periods, 6 s of CPU in sender per 60 s (10%), 3 s in tailer (5%); heap 100 MB sender
	for i := 0; i < 2; i++ {
		synth(t, a, "datadog-agent", t0.Add(time.Duration(i)*time.Minute), time.Minute,
			map[string]int64{"pkg/logs/sender.(*Sender).run": 6e9, "pkg/logs/tailer.read": 3e9},
			map[string]int64{"pkg/logs/sender.(*Sender).run": 100e6})
	}
	// b: one period, sender 12 s (20%), tailer 3 s, new tagfilter 1.2 s; heap sender 100 MB + tagfilter 50 MB
	zstdOut = true
	defer func() { zstdOut = false }()
	synth(t, b, "datadog-agent", t0.Add(5*time.Minute), time.Minute,
		map[string]int64{"pkg/logs/sender.(*Sender).run": 12e9, "pkg/logs/tailer.read": 3e9, "pkg/logs/tagfilter.compile": 1.2e9},
		map[string]int64{"pkg/logs/sender.(*Sender).run": 100e6, "pkg/logs/tagfilter.compile": 50e6})

	capsA, err := LoadDir(a)
	if err != nil || len(capsA) != 2 {
		t.Fatalf("LoadDir a: %v %d", err, len(capsA))
	}
	if capsA[0].Service != "datadog-agent" || len(capsA[0].Files) != 2 {
		t.Fatalf("capture: %+v", capsA[0])
	}
	if got := Overlapping(capsA, t0.Add(90*time.Second), t0.Add(10*time.Minute)); len(got) != 1 {
		t.Errorf("Overlapping = %d, want 1", len(got))
	}
	capsB, _ := LoadDir(b)
	an, bn := Analyze(capsA), Analyze(capsB)
	if len(an) != 3 { // cpu, inuse_space, alloc_space
		t.Fatalf("analyses = %d", len(an))
	}
	cpu := an[0]
	if cpu.Kind.Label != "CPU" || cpu.Captures != 2 || cpu.Wall != 120 {
		t.Fatalf("cpu analysis: %+v", cpu.Kind)
	}
	// 12 s CPU over 120 s wall = 10 % of a core for the sender.
	if r := cpu.Rows["pkg/logs/sender.(*Sender).run"]; r == nil || r.Flat < 9.99 || r.Flat > 10.01 {
		t.Errorf("sender flat = %+v", r)
	}
	if r := cpu.Rows["main.run"]; r == nil || r.Flat != 0 || r.Cum < 14.99 || r.Cum > 15.01 {
		t.Errorf("caller cum = %+v", r)
	}
	heap := an[1]
	if heap.Kind.SampleType != "inuse_space" || heap.Total != 100e6 { // averaged over the two captures
		t.Errorf("heap: %s total %v", heap.Kind.SampleType, heap.Total)
	}
	diffs := Compare(an, bn, 5)
	if len(diffs) != 3 {
		t.Fatalf("diffs = %d", len(diffs))
	}
	d := diffs[0]
	if d.Label != "CPU" || d.Rows[0].Function != "pkg/logs/sender.(*Sender).run" || d.Rows[0].Delta < 9.99 || d.Rows[0].Delta > 10.01 {
		t.Errorf("cpu diff: %+v", d.Rows[0])
	}
	if d.Rows[1].Function != "pkg/logs/tagfilter.compile" || d.Rows[1].A != 0 {
		t.Errorf("new function should be the second mover: %+v", d.Rows[1])
	}
	h := diffs[1]
	if h.Rows[0].Function != "pkg/logs/tagfilter.compile" || h.Rows[0].Delta != 50e6 {
		t.Errorf("heap diff: %+v", h.Rows[0])
	}
	s := an[0].Summary(1)
	if s.Total < 14.99 || len(s.Top) != 1 || s.Top[0].Function != "pkg/logs/sender.(*Sender).run" {
		t.Errorf("summary: %+v", s)
	}
	for _, c := range []struct {
		v          float64
		unit, want string
	}{{12.345, "%", "12.3%"}, {1.5e6, "B", "1.5 MB"}, {2e3, "B/s", "2.0 kB/s"}, {3, "", "3"}} {
		if got := Format(c.v, c.unit); got != c.want {
			t.Errorf("Format(%v,%q) = %q", c.v, c.unit, got)
		}
	}
	if FormatDelta(-5e6, "B") != "−5.0 MB" || FormatDelta(0, "%") != "=" || ShortFunc("github.com/x/y/pkg/logs/sender.(*Sender).run") != "sender.(*Sender).run" {
		t.Error("formatting")
	}
}
