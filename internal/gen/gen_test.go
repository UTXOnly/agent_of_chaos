package gen

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/UTXOnly/agent_of_chaos/internal/wire"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	c := Default()
	c.Name = "t"
	c.LogDir = t.TempDir()
	c.Quiet = true
	c.StatsInterval = 100 * time.Millisecond
	return c
}

func TestDeterministicBytes(t *testing.T) {
	run := func() map[string][]byte {
		c := testConfig(t)
		c.Deterministic, c.Seed = true, 7
		c.Streams, c.Rate, c.MaxLines = 3, 0, 3000
		c.Format = FormatBoth
		c.MultilineRate, c.WideLineRate = 0.2, 0.05
		c.RotateBytes, c.RotateKeep = 64<<10, 2
		cd, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := cd.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if rep.Totals.Records < 3000 || rep.Totals.Records > 3000+6 { // lifecycle lines + quota granularity
			t.Errorf("records = %d", rep.Totals.Records)
		}
		out := map[string][]byte{}
		entries, _ := os.ReadDir(c.LogDir)
		for _, e := range entries {
			b, _ := os.ReadFile(filepath.Join(c.LogDir, e.Name()))
			out[e.Name()] = b
		}
		return out
	}
	a, b := run(), run()
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("files: %d vs %d", len(a), len(b))
	}
	for name, ab := range a {
		if !bytes.Equal(ab, b[name]) {
			t.Errorf("%s differs between runs", name)
		}
	}
}

func TestLineFormatsAndMarkers(t *testing.T) {
	c := testConfig(t)
	c.Streams, c.Rate, c.MaxLines, c.Format = 2, 0, 400, FormatBoth
	c.MultilineRate, c.WideLineRate = 0.3, 0.1
	cd, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cd.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(c.LogDir, "*.log"))
	if len(files) != 4 {
		t.Fatalf("files: %v", files)
	}
	for _, f := range files {
		data, _ := os.ReadFile(f)
		lines := bytes.Split(bytes.TrimRight(data, "\n"), []byte("\n"))
		isJSON := strings.HasSuffix(f, ".json.log")
		var lastSeq int64
		markers := 0
		for _, ln := range lines {
			if isJSON {
				var obj map[string]any
				if err := json.Unmarshal(ln, &obj); err != nil {
					t.Fatalf("%s: bad json %q: %v", f, ln, err)
				}
				if _, ok := obj["aoc"]; !ok {
					t.Fatalf("%s: no aoc field: %q", f, ln)
				}
			}
			m, ok := wire.FindMarker(string(ln))
			if !ok {
				if isJSON {
					t.Fatalf("%s: unmarked json line %q", f, ln)
				}
				continue // continuation line
			}
			markers++
			if m.Seq != lastSeq+1 {
				t.Fatalf("%s: seq %d after %d", f, m.Seq, lastSeq)
			}
			lastSeq = m.Seq
			if !isJSON {
				if _, err := time.Parse("2006-01-02T15:04:05.000000Z", string(ln[:27])); err != nil {
					t.Fatalf("%s: bad timestamp prefix %q", f, ln[:27])
				}
			}
		}
		if markers < 200 {
			t.Errorf("%s: only %d markers", f, markers)
		}
	}
}

func TestRotation(t *testing.T) {
	for _, mode := range []RotateMode{RotateRename, RotateTruncate} {
		c := testConfig(t)
		c.Streams, c.Rate, c.MaxLines = 1, 0, 5000
		c.RotateBytes, c.RotateKeep, c.RotateMode = 32<<10, 2, mode
		c.MultilineRate = 0
		cd, err := New(c)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := cd.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		files, _ := filepath.Glob(filepath.Join(c.LogDir, "*.log*"))
		if len(files) != 3 { // live + 2 backups
			t.Errorf("%s: files = %v", mode, files)
		}
		if rep.Totals.Rotations < 5 {
			t.Errorf("%s: rotations = %d", mode, rep.Totals.Rotations)
		}
		for _, f := range files {
			st, _ := os.Stat(f)
			if st.Size() > 32<<10+1024 {
				t.Errorf("%s: %s is %d bytes", mode, f, st.Size())
			}
		}
	}
}

func TestPacing(t *testing.T) {
	c := testConfig(t)
	c.Streams, c.Rate, c.Duration = 2, 2000, 1500*time.Millisecond
	c.MultilineRate = 0
	cd, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	rep, err := cd.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// ~3000 records expected; allow slack for scheduling.
	if rep.Totals.Records < 2400 || rep.Totals.Records > 3400 {
		t.Errorf("records = %d, want ≈3000", rep.Totals.Records)
	}
}

func TestScenarioBuilders(t *testing.T) {
	c := Default()
	c.Streams, c.Rate, c.RampStart, c.RampStep, c.RampInterval = 5, 1000, 1, 2, time.Second
	sc := rampScenario(&c)
	if len(sc.Phases) != 3 || *sc.Phases[0].Streams != 1 || *sc.Phases[2].Streams != 5 || *sc.Phases[1].Rate != 600 {
		t.Errorf("ramp: %+v", sc.Phases)
	}
	c.PulseCycles = 2
	if p := pulseScenario(&c); len(p.Phases) != 1+4*2 || p.Loop {
		t.Errorf("pulse: %d phases loop=%v", len(p.Phases), p.Loop)
	}
	for _, name := range BuiltinNames() {
		if err := Builtin[name].Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := LoadScenario("does-not-exist"); err == nil {
		t.Error("expected error for unknown scenario")
	}
}

func TestDurationParsing(t *testing.T) {
	var d Duration
	for in, want := range map[string]time.Duration{`"90s"`: 90 * time.Second, `600`: 600 * time.Second, `"1h30m"`: 90 * time.Minute, `0.5`: 500 * time.Millisecond} {
		if err := json.Unmarshal([]byte(in), &d); err != nil || d.D() != want {
			t.Errorf("Duration(%s) = %v, %v; want %v", in, d.D(), err, want)
		}
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	c.Name = "bad name"
	if err := c.Validate(); err == nil {
		t.Error("name with space accepted")
	}
	c = Default()
	c.Mode = "scenario"
	if err := c.Validate(); err == nil {
		t.Error("scenario mode without --scenario accepted")
	}
	c = Default()
	c.Name = "x-{host}"
	if err := c.Validate(); err != nil || strings.Contains(c.Name, "{host}") {
		t.Errorf("host expansion: %q %v", c.Name, err)
	}
}
